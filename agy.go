package main

// agy.go — wiring for the "agy" upstream (Forwarded-to = Agy).
//
// agy is the Google Antigravity CLI. It is NOT an HTTP backend; it is wrapped
// by `agyj` (a sibling Go binary) which runs agy in print mode, reads the
// conversation SQLite db, and prints a single JSON blob on stdout. When a model
// alias's forward_to == "agy", the three request handlers short-circuit here:
// we flatten the request to one prompt, exec agyj (bounded by a concurrency
// semaphore + timeout), then translate the final text back into the proper
// Anthropic / OpenAI response shape — reusing the existing converters and SSE
// emit helpers so no new response formats are invented.
//
// Scope (v1): text-only. agy has no tool-calling and no native streaming, so an
// agy-backed model serves plain-text completions (Test page + simple chat /
// messages). Streaming is synthesized: run to completion, then replay as SSE.
// Multi-turn is stateless — the full incoming history is flattened every call
// (no agyj --conversation, no session store).

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
)

// agySem bounds how many agyj subprocesses run at once (heavy ~168MB process +
// shared Gemini quota). Sized at startup by initAgy from cfg.AgyConcurrency.
var agySem chan struct{}

func initAgy(cfg config) {
	n := cfg.AgyConcurrency
	if n < 1 {
		n = 1
	}
	agySem = make(chan struct{}, n)
}

func parseAgyConcurrency(s string) int {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil || n < 1 {
		return 2
	}
	if n > 16 {
		n = 16
	}
	return n
}

func parseAgyTimeout(s string) time.Duration {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil || n <= 0 {
		return 180 * time.Second
	}
	return time.Duration(n) * time.Second
}

func parseAgyWarmWorkers(s string) int {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil || n < 0 {
		return 0
	}
	if n > 64 {
		n = 64
	}
	return n
}

func parseAgyWorkerMaxTurns(s string) int {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil || n < 1 {
		return 50
	}
	return n
}

// parseByteSize accepts plain bytes or a K/M/G(B) suffix (e.g. "500MB", "1GB").
func parseByteSize(s string, def int64) int64 {
	s = strings.ToUpper(strings.TrimSpace(s))
	if s == "" {
		return def
	}
	mult := int64(1)
	switch {
	case strings.HasSuffix(s, "GB"):
		mult, s = 1<<30, strings.TrimSuffix(s, "GB")
	case strings.HasSuffix(s, "G"):
		mult, s = 1<<30, strings.TrimSuffix(s, "G")
	case strings.HasSuffix(s, "MB"):
		mult, s = 1<<20, strings.TrimSuffix(s, "MB")
	case strings.HasSuffix(s, "M"):
		mult, s = 1<<20, strings.TrimSuffix(s, "M")
	case strings.HasSuffix(s, "KB"):
		mult, s = 1<<10, strings.TrimSuffix(s, "KB")
	case strings.HasSuffix(s, "K"):
		mult, s = 1<<10, strings.TrimSuffix(s, "K")
	case strings.HasSuffix(s, "B"):
		s = strings.TrimSuffix(s, "B")
	}
	n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil || n <= 0 {
		return def
	}
	return n * mult
}

// agyMediaPrep materializes any attached media into content-addressed dirs
// (retained for reuse) and augments the prompt with the file paths. Returns the
// (possibly augmented) prompt, the content dirs to hand agy via --add-dir (nil
// when no media), and an error. Files are NOT deleted here — they persist for
// follow-up questions and are reaped after the retention window.
func agyMediaPrep(ctx context.Context, cfg config, basePrompt string, parts []mediaPart) (string, []string, error) {
	if !cfg.AgyMedia || len(parts) == 0 {
		return basePrompt, nil, nil
	}
	root := agyMediaRoot(cfg)
	if err := os.MkdirAll(root, 0o700); err != nil {
		return basePrompt, nil, err
	}
	items, addDirs, err := materializeMedia(ctx, cfg, root, parts)
	if err != nil {
		return basePrompt, nil, err
	}
	if len(items) == 0 {
		return basePrompt, nil, nil
	}
	return buildMediaPrompt(items, basePrompt), addDirs, nil
}

// agyModelForRequest resolves the model, defaulting media requests with no
// explicit Antigravity model to cfg.AgyMediaModel ("Gemini 3.6 Flash (Low)").
func agyModelForRequest(cfg config, alias string, hasMedia bool) string {
	m := agyModelFor(cfg, alias)
	if m == "" && hasMedia {
		return normalizeAgyModelName(cfg.AgyMediaModel)
	}
	return m
}

// agyAcquire blocks for a concurrency slot, honoring ctx cancellation/timeout so
// a queued request can give up cleanly instead of piling up. Returns nil (and
// "acquired") on success, or ctx.Err() if the caller was cancelled while waiting.
func agyAcquire(ctx context.Context) error {
	if agySem == nil {
		return nil // not initialized (e.g. unit tests) → run unbounded
	}
	select {
	case agySem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func agyRelease() {
	if agySem == nil {
		return
	}
	select {
	case <-agySem:
	default:
	}
}

// agyResult is the distilled outcome of one agyj invocation.
type agyResult struct {
	Ok         bool
	Response   string
	Model      string
	Error      string
	ExitCode   int
	DurationMs int64
}

// agyjOutput mirrors the relevant fields of agyj's stdout JSON (both the success
// shape and the {ok:false,error} error shape). Nullable fields are pointers.
type agyjOutput struct {
	Ok         bool    `json:"ok"`
	Response   string  `json:"response"`
	Model      *string `json:"model"`
	Error      string  `json:"error"`
	ExitCode   *int    `json:"exit_code"`
	DurationMs int64   `json:"duration_ms"`
	// agyj already captures the CLI's own streams on the failure path; without
	// carrying them through, an agy-side failure reaches the caller as a bare
	// wrapper message ("could not find a conversation db …") that says nothing
	// about WHY the CLI produced nothing — which is exactly the information
	// needed to tell a quota rejection from a crash from a slow disk.
	AgyStderr string `json:"agy_stderr"`
	AgyStdout string `json:"agy_stdout"`
}

// parseAgyjOutput decodes agyj's stdout. It returns an error only when the bytes
// are missing or not JSON; an ok=false payload is a valid parse (surfaced via
// res.Ok / res.Error) so the caller can report the agy-side error verbatim.
func parseAgyjOutput(raw []byte) (agyResult, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return agyResult{}, fmt.Errorf("agyj produced no output")
	}
	var o agyjOutput
	if err := json.Unmarshal(trimmed, &o); err != nil {
		return agyResult{}, fmt.Errorf("agyj output not JSON (%v): %s", err, truncateString(string(trimmed), 300))
	}
	res := agyResult{Ok: o.Ok, Response: o.Response, Error: strings.TrimSpace(o.Error), DurationMs: o.DurationMs}
	if o.Model != nil {
		res.Model = strings.TrimSpace(*o.Model)
	}
	if o.ExitCode != nil {
		res.ExitCode = *o.ExitCode
	}
	if !res.Ok && res.Error == "" {
		res.Error = "agy returned ok=false"
	}
	if !res.Ok {
		if detail := agyFailureDetail(o); detail != "" {
			res.Error += " [agy: " + detail + "]"
		}
	}
	return res, nil
}

// agyFailureDetail summarises whatever the CLI itself said on a failed run, so
// the proxy's 502 body names the real cause instead of only the wrapper's
// symptom. Prefers stderr (where crashes and auth/quota rejections land) and
// falls back to stdout.
func agyFailureDetail(o agyjOutput) string {
	for _, s := range []string{o.AgyStderr, o.AgyStdout} {
		if s = strings.TrimSpace(s); s != "" {
			return truncateString(strings.Join(strings.Fields(s), " "), 400)
		}
	}
	return ""
}

// agyBinPath resolves the agyj wrapper: explicit config first, then a sibling of
// the running proxy executable, then bare "agyj" on PATH.
func agyBinPath(cfg config) string {
	if b := strings.TrimSpace(cfg.AgyBin); b != "" {
		return b
	}
	name := "agyj"
	if runtime.GOOS == "windows" {
		name = "agyj.exe"
	}
	if exe, err := os.Executable(); err == nil {
		cand := filepath.Join(filepath.Dir(exe), name)
		if _, err := os.Stat(cand); err == nil {
			return cand
		}
	}
	return name
}

// runAgyj execs the agyj wrapper for a single prompt and returns the parsed
// result. It acquires a concurrency slot (ctx-aware), bounds the run with
// cfg.AgyTimeout, points agyj at the configured agy CLI, and parses stdout even
// on a non-zero exit (agyj still prints structured JSON on agy errors).
func runAgyj(ctx context.Context, cfg config, prompt, model string, addDirs []string) (agyResult, error) {
	if err := agyAcquire(ctx); err != nil {
		return agyResult{}, fmt.Errorf("agy queue wait cancelled: %w", err)
	}
	defer agyRelease()

	media := len(addDirs) > 0
	timeout := cfg.AgyTimeout
	if media {
		timeout = cfg.AgyMediaTimeout // media analysis (transcode/parse) is heavier
	}
	if timeout <= 0 {
		timeout = 180 * time.Second
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// If prompt is large (> 64KB) or stream-json CLI is available, execute via stream-json over stdin
	// to avoid Linux kernel E2BIG ("argument list too long") CLI argument limits.
	if len(prompt) > 64*1024 || resolveAgyCLIPath(cfg) != "" {
		res, err := runAgyStreamJSON(cctx, cfg, prompt, model, addDirs)
		if err == nil {
			return res, nil
		}
		if len(prompt) > 64*1024 {
			return agyResult{}, fmt.Errorf("stream-json execution failed for large prompt (%d bytes): %w", len(prompt), err)
		}
	}

	args := make([]string, 0, 8+2*len(addDirs))
	if m := strings.TrimSpace(model); m != "" {
		args = append(args, "--model", m)
	}
	// Always allow headless print mode to execute web/network tools and actions
	args = append(args, "--dangerously-skip-permissions")
	if media {
		// Scope agy to exactly this request's files and let it read them without
		// an interactive permission prompt (which would hang print mode).
		for _, d := range addDirs {
			args = append(args, "--add-dir", d)
		}
	}
	args = append(args, "-p", prompt)

	cmd := exec.CommandContext(cctx, agyBinPath(cfg), args...)
	env := os.Environ()
	if cli := strings.TrimSpace(cfg.AgyCLI); cli != "" {
		env = append(env, "AGYJ_AGY_BIN="+cli)
	}
	env = append(env, "AGYJ_TIMEOUT="+strconv.Itoa(int(timeout/time.Second)))
	cmd.Env = env

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	runErr := cmd.Run()

	if cctx.Err() == context.DeadlineExceeded {
		return agyResult{}, fmt.Errorf("agy timed out after %ds", int(timeout/time.Second))
	}
	if ctx.Err() != nil {
		return agyResult{}, ctx.Err()
	}

	out := stdout.Bytes()
	if len(bytes.TrimSpace(out)) == 0 {
		detail := strings.TrimSpace(stderr.String())
		if detail == "" && runErr != nil {
			detail = runErr.Error()
		}
		if detail == "" {
			detail = "no output"
		}
		return agyResult{}, fmt.Errorf("agyj produced no output: %s", truncateString(detail, 300))
	}
	return parseAgyjOutput(out)
}

// agyModelFor decides which model name to forward to agy. A global override
// (PROXY_AGY_MODEL) always wins; otherwise the alias's configured upstream model
// is used only when it looks like an Antigravity/Gemini model — codex/gpt-style
// names are dropped so agy falls back to its own configured default instead of
// rejecting an unknown model.
func agyModelFor(cfg config, alias string) string {
	if m := strings.TrimSpace(cfg.AgyModel); m != "" {
		return normalizeAgyModelName(m)
	}
	real := strings.TrimSpace(resolveModel(cfg, alias))
	if real == "" {
		return ""
	}
	real = normalizeAgyModelName(real)
	// Antigravity model names (from `agy models`) always contain a space, e.g.
	// "Gemini 3.6 Pro (High)", "Claude Opus 4.6 (Thinking)", "GPT-OSS 120B (Medium)"
	// — forward those verbatim (agy accepts the display name). A bare, space-free
	// gpt-*/o*/codex token is a Codex id left on an agy alias by mistake; drop it
	// so agy falls back to its own configured default instead of erroring.
	if !strings.Contains(real, " ") {
		low := strings.ToLower(real)
		if strings.HasPrefix(low, "gpt") || strings.HasPrefix(low, "o1") ||
			strings.HasPrefix(low, "o3") || strings.HasPrefix(low, "o4") ||
			strings.HasPrefix(low, "codex") {
			return ""
		}
	}
	return real
}

// normalizeAgyModelName remaps legacy 3.5 model references to 3.6 because agy
// deprecated and removed 3.5 in favor of 3.6.
func normalizeAgyModelName(m string) string {
	m = strings.TrimSpace(m)
	if m == "" {
		return ""
	}
	if strings.Contains(m, "3.5") {
		m = strings.ReplaceAll(m, "3.5", "3.6")
	}
	return m
}

// agyRoleLabel maps a chat role to a transcript label for the flattened prompt.
func agyRoleLabel(role string) string {
	switch strings.ToLower(strings.TrimSpace(role)) {
	case "assistant":
		return "Assistant"
	case "system", "developer":
		return "System"
	case "tool":
		return "Tool"
	default:
		return "User"
	}
}

// contentToTextNoMedia renders content as text for the agy flattening path but
// DROPS media blocks (image/document/audio/video/file) instead of
// json-marshalling them. Media is handed to agy out-of-band via --add-dir, so
// inlining a block's base64 here both wastes agy's context and — because the
// flattened prompt is passed as the agyj `-p` CLI argument — overflows the OS
// ARG_MAX limit on real-sized media, surfacing as
// "fork/exec agyj: argument list too long". (The shared contentToText, used by
// the upstream-request converters, is intentionally left unchanged.)
func contentToTextNoMedia(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case []any:
		parts := make([]string, 0, len(x))
		for _, item := range x {
			if m, ok := item.(map[string]any); ok {
				if m["type"] == "text" {
					parts = append(parts, fmt.Sprint(m["text"]))
					continue
				}
				if m["type"] == "tool_use" {
					name, _ := m["name"].(string)
					input := m["input"]
					if input == nil {
						input = map[string]any{}
					}
					toolCallPayload := map[string]any{
						"tool":  name,
						"input": input,
					}
					b, _ := json.Marshal(toolCallPayload)
					parts = append(parts, "<tool_call>\n"+string(b)+"\n</tool_call>")
					continue
				}
				if m["type"] == "tool_result" {
					contentStr := ""
					if c, ok := m["content"]; ok {
						contentStr = contentToTextNoMedia(c)
					}
					toolUseID, _ := m["tool_use_id"].(string)
					prefix := "Tool Result"
					if toolUseID != "" {
						prefix += " (" + toolUseID + ")"
					}
					parts = append(parts, prefix+":\n"+contentStr)
					continue
				}
				if _, isMedia := mediaPartFromBlock(m); isMedia {
					continue // handled via --add-dir; never inline base64 as text
				}
			}
			if s := contentToTextNoMedia(item); s != "" {
				parts = append(parts, s)
			}
		}
		return strings.Join(parts, "\n")
	case map[string]any:
		if text, ok := x["text"]; ok {
			return fmt.Sprint(text)
		}
		if x["type"] == "tool_use" {
			name, _ := x["name"].(string)
			input := x["input"]
			if input == nil {
				input = map[string]any{}
			}
			toolCallPayload := map[string]any{
				"tool":  name,
				"input": input,
			}
			b, _ := json.Marshal(toolCallPayload)
			return "<tool_call>\n" + string(b) + "\n</tool_call>"
		}
		if x["type"] == "tool_result" {
			contentStr := ""
			if c, ok := x["content"]; ok {
				contentStr = contentToTextNoMedia(c)
			}
			toolUseID, _ := x["tool_use_id"].(string)
			prefix := "Tool Result"
			if toolUseID != "" {
				prefix += " (" + toolUseID + ")"
			}
			return prefix + ":\n" + contentStr
		}
		if _, isMedia := mediaPartFromBlock(x); isMedia {
			return ""
		}
		b, _ := json.Marshal(x)
		return string(b)
	default:
		return fmt.Sprint(x)
	}
}

// buildAgyToolsPrompt renders tool definitions and call instructions for agy.
func buildAgyToolsPrompt(tools []responsesTool) string {
	if len(tools) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("### AVAILABLE TOOLS & INVOCATION RULES\n\n")
	b.WriteString("CRITICAL TOOL INVOCATION RULES:\n")
	b.WriteString("- NEVER execute shell commands, bash, python, or run_command. Those tools are strictly forbidden.\n")
	b.WriteString("- When you need to retrieve or check data (e.g. available appointment slots, customer packages, pricing) or take an action (e.g. create reservation, save memory), you MUST call the relevant tool using this exact format:\n\n")
	b.WriteString("<tool_call>\n{\n  \"tool\": \"tool_name\",\n  \"input\": {\n    \"param\": \"value\"\n  }\n}\n</tool_call>\n\n")
	b.WriteString("- You can invoke multiple tools in a single response by emitting multiple <tool_call> blocks or a JSON array of tool calls.\n")
	b.WriteString("- Never fabricate or guess tool results. Wait for the real tool result before responding.\n")
	b.WriteString("- When all necessary tools have been executed and you are ready to answer the user, provide your final response in natural conversation with no tool calls.\n\n")
	b.WriteString("Available Tools:\n\n")
	for _, t := range tools {
		b.WriteString("#### ")
		b.WriteString(t.Name)
		b.WriteString("\n")
		if desc := strings.TrimSpace(t.Description); desc != "" {
			b.WriteString(desc)
			b.WriteString("\n")
		}
		if t.Parameters != nil {
			var schema map[string]any
			rawBytes, _ := json.Marshal(t.Parameters)
			if err := json.Unmarshal(rawBytes, &schema); err == nil {
				if props, ok := schema["properties"].(map[string]any); ok && len(props) > 0 {
					b.WriteString("Parameters:\n")
					for pName, pDef := range props {
						pType := ""
						pDesc := ""
						if pm, ok := pDef.(map[string]any); ok {
							if ty, ok := pm["type"].(string); ok {
								pType = ty
							}
							if d, ok := pm["description"].(string); ok {
								pDesc = d
							}
						}
						b.WriteString("- `")
						b.WriteString(pName)
						b.WriteString("`")
						if pType != "" {
							b.WriteString(" (" + pType + ")")
						}
						if pDesc != "" {
							b.WriteString(": " + pDesc)
						}
						b.WriteString("\n")
					}
				}
			}
		}
		b.WriteString("\n")
	}
	return b.String()
}

func buildAgyAnthropicToolsPrompt(tools []anthropicTool) string {
	if len(tools) == 0 {
		return ""
	}
	rTools := make([]responsesTool, 0, len(tools))
	for _, t := range tools {
		rTools = append(rTools, responsesTool{
			Type:        "function",
			Name:        t.Name,
			Description: t.Description,
			Parameters:  t.InputSchema,
		})
	}
	return buildAgyToolsPrompt(rTools)
}

func buildAgyOpenAIToolsPrompt(tools []openAITool) string {
	if len(tools) == 0 {
		return ""
	}
	rTools := make([]responsesTool, 0, len(tools))
	for _, t := range tools {
		rTools = append(rTools, responsesTool{
			Type:        "function",
			Name:        t.Function.Name,
			Description: t.Function.Description,
			Parameters:  t.Function.Parameters,
		})
	}
	return buildAgyToolsPrompt(rTools)
}

func buildAgyToolsSystemPrompt(tools []anthropicTool) string {
	return buildAgyAnthropicToolsPrompt(tools)
}

func buildAgyTempDirective(temp *float64) string {
	if temp == nil {
		return ""
	}
	t := *temp
	if t <= 0.3 {
		return fmt.Sprintf("Generation strictness (temperature=%.1f): STRICT DETERMINISTIC FACTUALITY. Rely strictly on retrieved data and tool outputs. Never guess, assume, or fabricate any clinic details, prices, packages, or policies.", t)
	} else if t <= 0.6 {
		return fmt.Sprintf("Generation style (temperature=%.1f): BALANCED CONVERSATIONAL (temperature=%.1f). Maintain a natural, helpful flow while remaining grounded in retrieved tool data.", t, t)
	}
	return fmt.Sprintf("Generation style (temperature=%.1f): CREATIVE CONVERSATIONAL (temperature=%.1f). Use diverse phrasing and expressive responses.", t, t)
}

func isAnthropicToolResultMessage(msg anthropicMessage) bool {
	if strings.EqualFold(strings.TrimSpace(msg.Role), "tool") {
		return true
	}
	blocks, ok := msg.Content.([]any)
	if !ok || len(blocks) == 0 {
		return false
	}
	for _, b := range blocks {
		m, ok := b.(map[string]any)
		if !ok {
			return false
		}
		if fmt.Sprint(m["type"]) != "tool_result" {
			return false
		}
	}
	return true
}

func formatAnthropicMessageContent(msg anthropicMessage) string {
	var parts []string

	if blocks, ok := msg.Content.([]any); ok {
		for _, b := range blocks {
			m, ok := b.(map[string]any)
			if !ok {
				if s := strings.TrimSpace(contentToTextNoMedia(b)); s != "" {
					parts = append(parts, s)
				}
				continue
			}
			switch fmt.Sprint(m["type"]) {
			case "thinking", "redacted_thinking":
				continue
			case "text":
				if text := strings.TrimSpace(fmt.Sprint(m["text"])); text != "" {
					parts = append(parts, text)
				}
			case "tool_use":
				name, _ := m["name"].(string)
				input := m["input"]
				if input == nil {
					input = map[string]any{}
				}
				payload := map[string]any{"tool": name, "input": input}
				raw, _ := json.Marshal(payload)
				parts = append(parts, "<tool_call>\n"+string(raw)+"\n</tool_call>")
			case "tool_result":
				callID, _ := m["tool_use_id"].(string)
				contentStr := strings.TrimSpace(contentToTextNoMedia(m["content"]))
				label := "[Tool Result"
				if callID != "" {
					label += " (" + callID + ")"
				}
				label += "]"
				parts = append(parts, label+":\n"+contentStr)
			case "image", "document", "file", "audio", "video":
				continue
			default:
				if s := strings.TrimSpace(contentToTextNoMedia(m)); s != "" {
					parts = append(parts, s)
				}
			}
		}
	} else if msg.Content != nil {
		if s := strings.TrimSpace(contentToTextNoMedia(msg.Content)); s != "" {
			parts = append(parts, s)
		}
	}

	for _, tc := range msg.ToolCalls {
		var args any = tc.Function.Arguments
		if argStr := strings.TrimSpace(tc.Function.Arguments); argStr != "" {
			var parsed any
			if err := json.Unmarshal([]byte(argStr), &parsed); err == nil {
				args = parsed
			}
		}
		name := tc.Function.Name
		if name == "" {
			name = tc.Type
		}
		payload := map[string]any{
			"tool":  name,
			"input": args,
		}
		raw, _ := json.Marshal(payload)
		parts = append(parts, "<tool_call>\n"+string(raw)+"\n</tool_call>")
	}

	res := strings.TrimSpace(strings.Join(parts, "\n"))
	if strings.EqualFold(strings.TrimSpace(msg.Role), "tool") && !strings.HasPrefix(res, "[Tool Result") {
		label := "[Tool Result"
		if msg.ToolCallID != "" {
			label += " (" + msg.ToolCallID + ")"
		}
		label += "]"
		return label + ":\n" + res
	}

	return res
}

// extractPreflightToolCode finds the tool_code argument from a get_tool_instructions call or result.
func extractPreflightToolCode(text string) string {
	if idx := strings.Index(text, `"tool_code"`); idx != -1 {
		rest := text[idx+11:]
		rest = strings.TrimLeft(rest, ` :"'`)
		if end := strings.IndexAny(rest, `"',}`); end != -1 {
			return strings.TrimSpace(rest[:end])
		}
	}
	if idx := strings.Index(text, `"code"`); idx != -1 {
		rest := text[idx+6:]
		rest = strings.TrimLeft(rest, ` :"'`)
		if end := strings.IndexAny(rest, `"',}`); end != -1 {
			return strings.TrimSpace(rest[:end])
		}
	}
	return ""
}

// extractToolCallNames extracts the tool names from formatted <tool_call> blocks.
func extractToolCallNames(text string) []string {
	var names []string
	matches := agyToolTagRe.FindAllStringSubmatch(text, -1)
	for _, m := range matches {
		if len(m) > 1 {
			var payload struct {
				Tool string `json:"tool"`
				Name string `json:"name"`
			}
			if err := json.Unmarshal([]byte(strings.TrimSpace(m[1])), &payload); err == nil {
				if payload.Tool != "" {
					names = append(names, payload.Tool)
				} else if payload.Name != "" {
					names = append(names, payload.Name)
				}
			}
		}
	}
	return names
}

// buildPreflightDirective constructs prompt guidance reminding the model that instructions
// for certain tools have already been retrieved, preventing preflight loops.
func buildPreflightDirective(preflightedTools map[string]bool, executedTools map[string]bool, lastPreflightedTool string) string {
	if len(preflightedTools) == 0 && len(executedTools) == 0 {
		return ""
	}
	var toolList []string
	for tc := range preflightedTools {
		toolList = append(toolList, tc)
	}
	sort.Strings(toolList)

	var pb strings.Builder
	pb.WriteString("CRITICAL TOOL PREFLIGHT & PROTOCOL RULES:\n")
	if len(toolList) > 0 {
		pb.WriteString(fmt.Sprintf("- Instructions for the following tools have ALREADY been retrieved in this conversation: [%s].\n", strings.Join(toolList, ", ")))
		pb.WriteString("- NEVER call `get_tool_instructions` for any tool more than once in this conversation!\n")
		pb.WriteString("- NEVER call `get_tool_instructions` for any of these tools again! The preflight cache lasts for the entire conversation.\n")
	}
	if executedTools["membership_protocol"] {
		pb.WriteString("- The `membership_protocol` has ALREADY been executed and its instructions are returned above in the tool result. Do NOT call `membership_protocol` or `get_tool_instructions` again! Follow the protocol instructions directly to answer the customer in natural Arabic (e.g. asking which body area and branch she wants to book) or invoke the next booking tool.\n")
	} else if preflightedTools["membership_protocol"] {
		pb.WriteString("- Do NOT call `get_tool_instructions` for 'membership_protocol' again. If you need to use the membership protocol, call `membership_protocol` directly via <tool_call>, or proceed with booking.\n")
	}
	if executedTools["get_available_slots"] || executedTools["get_multi_service_slots"] {
		pb.WriteString("- Available appointment slots have ALREADY been retrieved in this conversation. Do NOT call `get_available_slots` or `get_multi_service_slots` again for the same date/branch! Quote the available options to the customer directly in natural Arabic.\n")
	}
	if executedTools["get_customer_packages"] {
		pb.WriteString("- Customer packages have ALREADY been retrieved in this conversation. Do NOT call `get_customer_packages` or `get_tool_instructions` again.\n")
	}
	if lastPreflightedTool != "" && lastPreflightedTool != "membership_protocol" && lastPreflightedTool != "get_available_slots" && lastPreflightedTool != "get_multi_service_slots" && lastPreflightedTool != "get_customer_packages" {
		if executedTools[lastPreflightedTool] {
			pb.WriteString(fmt.Sprintf("- The '%s' tool has ALREADY been executed and returned above. Do NOT call '%s' or `get_tool_instructions` again.\n", lastPreflightedTool, lastPreflightedTool))
		} else {
			pb.WriteString(fmt.Sprintf("- Do NOT call `get_tool_instructions` for '%s' again. If you were preparing to call '%s', call '%s' directly now via <tool_call>.\n", lastPreflightedTool, lastPreflightedTool, lastPreflightedTool))
		}
	}
	return pb.String()
}

// buildToolResultDirective constructs specific prompt instructions when the conversation's
// last turn is a tool result, guiding the model to formulate its text response or invoke the
// strictly necessary next tool, breaking infinite tool repetition loops.
func buildToolResultDirective(lastExecutedToolName string, lastToolResultContent string, activeCustomerRequest string, preflightDirective string) string {
	var b strings.Builder
	b.WriteString("### CURRENT STATE & ACTIVE CUSTOMER REQUEST\n\n")
	b.WriteString("The tool has executed and returned data above.\n\n")
	if activeCustomerRequest != "" {
		b.WriteString("ACTIVE CUSTOMER REQUEST BEING PROCESSED:\nCustomer: ")
		b.WriteString(activeCustomerRequest)
		b.WriteString("\n\n")
	}
	b.WriteString("CRITICAL DIRECTIVE FOR ASSISTANT:\n")
	b.WriteString("- You are in the MIDDLE of processing the customer's request. DO NOT reset the conversation.\n")
	b.WriteString("- The assistant has ALREADY introduced itself and greeted the customer. NEVER repeat the greeting, introduction, or persona opening.\n")
	b.WriteString("- The customer has ALREADY specified their required details (e.g. body area, clinic branch, appointment date/time). DO NOT re-ask questions the customer has already answered.\n")

	isSlotResult := lastExecutedToolName == "get_available_slots" ||
		lastExecutedToolName == "get_multi_service_slots" ||
		strings.Contains(lastToolResultContent, `"slots"`) ||
		strings.Contains(lastToolResultContent, `"merged_slots"`) ||
		strings.Contains(lastToolResultContent, `"available_slots"`)

	if isSlotResult {
		b.WriteString("- AVAILABLE APPOINTMENT SLOTS RETRIEVED: The available time slots for the customer's request have ALREADY been retrieved above in the tool result!\n")
		b.WriteString("- DO NOT call `get_available_slots` or `get_multi_service_slots` again! DO NOT call any more tools!\n")
		b.WriteString("- Provide the available slots directly to the customer in natural friendly Arabic according to clinic policies (quote the date and time ranges from merged_slots) and ask which time she prefers.\n")
	} else if lastExecutedToolName == "membership_protocol" || strings.Contains(lastToolResultContent, "MEMBERSHIP PROTOCOL") {
		b.WriteString("- The `membership_protocol` instructions have been retrieved above. Follow the instructions directly to ask the customer which area and branch she wants to book, or invoke the next booking tool via <tool_call>.\n")
	} else if lastExecutedToolName != "" {
		b.WriteString(fmt.Sprintf("- The '%s' tool has executed and returned data above. NEVER call '%s' again with the same parameters!\n", lastExecutedToolName, lastExecutedToolName))
		b.WriteString("- If the customer's question can now be answered from the returned data (e.g. packages, prices, clinic info), provide your final response to the customer in natural conversation with NO tool calls.\n")
		b.WriteString("- Only invoke another tool via <tool_call> if a completely different action is strictly required to fulfill the request.\n")
	} else {
		b.WriteString("- If additional tools are required to fulfill the request, invoke the next tool via <tool_call>.\n")
		b.WriteString("- Otherwise, provide your final response to the customer in natural conversation based on the retrieved data with no tool calls. Address their specific request directly.\n")
	}

	b.WriteString("- NEVER execute shell commands, bash, python, or run_command.\n")
	if preflightDirective != "" {
		b.WriteString("\n")
		b.WriteString(preflightDirective)
	}
	return b.String()
}


// flattenAnthropicToPrompt renders an Anthropic request into a single prompt.
func flattenAnthropicToPrompt(in anthropicRequest) string {
	sys := strings.TrimSpace(contentToTextNoMedia(in.System))
	toolsPrompt := strings.TrimSpace(buildAgyAnthropicToolsPrompt(in.Tools))
	tempDirective := buildAgyTempDirective(in.Temperature)

	if sys == "" && toolsPrompt == "" && tempDirective == "" && len(in.Messages) == 1 && strings.EqualFold(in.Messages[0].Role, "user") {
		return strings.TrimSpace(contentToTextNoMedia(in.Messages[0].Content))
	}

	var history []string
	var lastCustomerMessage string
	var activeCustomerRequest string
	var customerTurns []string
	lastTurnIsToolResult := false
	seenAssistantGreeting := false

	preflightedTools := make(map[string]bool)
	executedTools := make(map[string]bool)
	var lastPreflightedTool string
	var lastToolResultContent string
	var lastExecutedToolName string
	var lastAssistantToolCallText string
	suppressNextToolResult := false

	for i, msg := range in.Messages {
		content := formatAnthropicMessageContent(msg)
		if content == "" {
			continue
		}
		role := strings.ToLower(strings.TrimSpace(msg.Role))
		isLast := (i == len(in.Messages)-1)
		isToolResult := isAnthropicToolResultMessage(msg) || strings.HasPrefix(content, "[Tool Result")

		if isToolResult {
			lastToolResultContent = content
			if tc := extractPreflightToolCode(content); tc != "" {
				preflightedTools[tc] = true
				lastPreflightedTool = tc
			}
			if isLast {
				lastTurnIsToolResult = true
				history = append(history, content)
			} else if suppressNextToolResult {
				suppressNextToolResult = false
			} else {
				history = append(history, content)
			}
			continue
		}

		if role == "user" {
			suppressNextToolResult = false
			if strings.HasPrefix(content, "[SYSTEM ERROR:") || strings.Contains(content, "You injected internal reasoning") {
				continue
			}
			if !strings.HasPrefix(content, "[AUTO-CONTEXT") {
				activeCustomerRequest = content
				customerTurns = append(customerTurns, content)
			}
			if isLast {
				lastCustomerMessage = content
			} else {
				history = append(history, "Customer: "+content)
			}
			continue
		}

		if role == "assistant" {
			// Suppress repetitive identical greetings/questions to break autoregressive repetition loops
			if strings.Contains(content, "معك زينة مساعدتك الرقمية") &&
				(strings.Contains(content, "لأي منطقة") || strings.Contains(content, "بأي فرع")) {
				if seenAssistantGreeting {
					continue
				}
				seenAssistantGreeting = true
			}

			// Track preflights, executed tools, and collapse consecutive identical tool calls
			if strings.Contains(content, "<tool_call>") {
				if tc := extractPreflightToolCode(content); tc != "" {
					preflightedTools[tc] = true
					lastPreflightedTool = tc
				}
				names := extractToolCallNames(content)
				for _, name := range names {
					if name != "get_tool_instructions" && name != "" {
						executedTools[name] = true
					}
					if name != "" {
						lastExecutedToolName = name
					}
				}
				lastNames := extractToolCallNames(lastAssistantToolCallText)
				isConsecutiveSameTool := len(names) == 1 && len(lastNames) == 1 && names[0] == lastNames[0]
				if isConsecutiveSameTool {
					if len(history) >= 2 && strings.HasPrefix(history[len(history)-1], "[Tool Result") && strings.HasPrefix(history[len(history)-2], "Assistant: <tool_call>") {
						history = history[:len(history)-2]
					} else if len(history) >= 1 && strings.HasPrefix(history[len(history)-1], "Assistant: <tool_call>") {
						history = history[:len(history)-1]
					}
				}
				lastAssistantToolCallText = content
				suppressNextToolResult = false
			} else {
				lastAssistantToolCallText = ""
				suppressNextToolResult = false
			}

			history = append(history, "Assistant: "+content)
			continue
		}

		suppressNextToolResult = false
		if role == "system" || role == "developer" {
			history = append(history, "System: "+content)
			continue
		}

		history = append(history, agyRoleLabel(role)+": "+content)
	}

	preflightDirective := buildPreflightDirective(preflightedTools, executedTools, lastPreflightedTool)

	var b strings.Builder

	if sys != "" || tempDirective != "" || preflightDirective != "" {
		b.WriteString("### SYSTEM INSTRUCTIONS & POLICIES\n\n")
		if tempDirective != "" {
			b.WriteString(tempDirective)
			b.WriteString("\n\n")
		}
		if sys != "" {
			b.WriteString(sys)
			b.WriteString("\n\n")
		}
		if preflightDirective != "" {
			b.WriteString(preflightDirective)
			b.WriteString("\n\n")
		}
	}

	if toolsPrompt != "" {
		b.WriteString(toolsPrompt)
		b.WriteString("\n\n")
	}

	if len(customerTurns) > 1 {
		b.WriteString("### CHRONOLOGICAL CUSTOMER STATEMENTS (Review Carefully):\n")
		for idx, ct := range customerTurns {
			b.WriteString(fmt.Sprintf("- [Message %d]: %s\n", idx+1, ct))
		}
		b.WriteString("\n")
	}

	if len(history) > 0 {
		b.WriteString("### CONVERSATION HISTORY\n\n")
		b.WriteString(strings.Join(history, "\n\n"))
		b.WriteString("\n\n")
	}

	if lastCustomerMessage != "" {
		b.WriteString("### CURRENT CUSTOMER MESSAGE & REQUIRED ACTION\n\n")
		b.WriteString("Customer: ")
		b.WriteString(lastCustomerMessage)
		b.WriteString("\n\n")
		b.WriteString("CRITICAL DIRECTIVE FOR ASSISTANT:\n")
		b.WriteString("- The assistant has ALREADY introduced itself and greeted the customer. NEVER repeat the greeting, introduction, or persona opening.\n")
		b.WriteString("- The customer has ALREADY specified their required details (e.g. body area, clinic branch, appointment date/time) in earlier turns or customer statements above. DO NOT re-ask for details already provided!\n")
		b.WriteString("- If all required information to proceed is available, invoke the relevant tool immediately via <tool_call>.\n")
		b.WriteString("- NEVER execute shell commands, bash, python, or run_command.\n")
		if preflightDirective != "" {
			b.WriteString("\n")
			b.WriteString(preflightDirective)
		}
	} else if lastTurnIsToolResult {
		b.WriteString(buildToolResultDirective(lastExecutedToolName, lastToolResultContent, activeCustomerRequest, preflightDirective))
	} else if preflightDirective != "" {
		b.WriteString(preflightDirective)
	}


	return strings.TrimSpace(b.String())
}

// flattenOpenAIChatToPrompt renders an OpenAI chat request into a single prompt.
func flattenOpenAIChatToPrompt(in openAIRequest) string {
	toolsPrompt := strings.TrimSpace(buildAgyOpenAIToolsPrompt(in.Tools))
	tempDirective := buildAgyTempDirective(in.Temperature)

	if toolsPrompt == "" && tempDirective == "" && len(in.Messages) == 1 && strings.EqualFold(in.Messages[0].Role, "user") {
		return strings.TrimSpace(contentToTextNoMedia(in.Messages[0].Content))
	}

	var sysParts []string
	if tempDirective != "" {
		sysParts = append(sysParts, tempDirective)
	}

	var history []string
	var lastCustomerMessage string
	var activeCustomerRequest string
	var customerTurns []string
	lastTurnIsToolResult := false
	seenAssistantGreeting := false

	preflightedTools := make(map[string]bool)
	executedTools := make(map[string]bool)
	var lastPreflightedTool string
	var lastToolResultContent string
	var lastExecutedToolName string
	var lastAssistantToolCallText string
	suppressNextToolResult := false

	for i, msg := range in.Messages {
		role := strings.ToLower(strings.TrimSpace(msg.Role))
		if role == "system" || role == "developer" {
			if s := strings.TrimSpace(contentToTextNoMedia(msg.Content)); s != "" {
				sysParts = append(sysParts, s)
			}
			continue
		}

		isLast := (i == len(in.Messages)-1)
		content := strings.TrimSpace(contentToTextNoMedia(msg.Content))

		if role == "tool" {
			lastToolResultContent = content
			if tc := extractPreflightToolCode(content); tc != "" {
				preflightedTools[tc] = true
				lastPreflightedTool = tc
			}
			callID := msg.ToolCallID
			label := "[Tool Result"
			if callID != "" {
				label += " (" + callID + ")"
			}
			label += "]"
			formatted := label + ":\n" + content
			if isLast {
				lastTurnIsToolResult = true
				history = append(history, formatted)
			} else if suppressNextToolResult {
				suppressNextToolResult = false
			} else {
				history = append(history, formatted)
			}
			continue
		}

		if role == "user" {
			suppressNextToolResult = false
			if strings.HasPrefix(content, "[SYSTEM ERROR:") || strings.Contains(content, "You injected internal reasoning") {
				continue
			}
			if !strings.HasPrefix(content, "[AUTO-CONTEXT") {
				activeCustomerRequest = content
				customerTurns = append(customerTurns, content)
			}
			if isLast {
				lastCustomerMessage = content
			} else {
				history = append(history, "Customer: "+content)
			}
			continue
		}

		if role == "assistant" {
			var parts []string
			if content != "" {
				// Suppress repetitive identical greetings/questions to break autoregressive repetition loops
				if strings.Contains(content, "معك زينة مساعدتك الرقمية") &&
					(strings.Contains(content, "لأي منطقة") || strings.Contains(content, "بأي فرع")) {
					if seenAssistantGreeting {
						continue
					}
					seenAssistantGreeting = true
				}
				parts = append(parts, content)
			}
			var callNames []string
			for _, tc := range msg.ToolCalls {
				name := tc.Function.Name
				if name != "" {
					callNames = append(callNames, name)
					if name != "get_tool_instructions" {
						executedTools[name] = true
					}
					lastExecutedToolName = name
				}
				var args any = tc.Function.Arguments
				if argStr := strings.TrimSpace(tc.Function.Arguments); argStr != "" {
					var parsed any
					if err := json.Unmarshal([]byte(argStr), &parsed); err == nil {
						args = parsed
					}
				}
				callPayload := map[string]any{
					"tool":  name,
					"input": args,
				}
				raw, _ := json.Marshal(callPayload)
				parts = append(parts, "<tool_call>\n"+string(raw)+"\n</tool_call>")
			}
			if len(parts) > 0 {
				assistantText := strings.Join(parts, "\n")
				if strings.Contains(assistantText, "<tool_call>") {
					if tc := extractPreflightToolCode(assistantText); tc != "" {
						preflightedTools[tc] = true
						lastPreflightedTool = tc
					}
					lastNames := extractToolCallNames(lastAssistantToolCallText)
					isConsecutiveSameTool := len(callNames) == 1 && len(lastNames) == 1 && callNames[0] == lastNames[0]
					if isConsecutiveSameTool {
						if len(history) >= 2 && strings.HasPrefix(history[len(history)-1], "[Tool Result") && strings.HasPrefix(history[len(history)-2], "Assistant: <tool_call>") {
							history = history[:len(history)-2]
						} else if len(history) >= 1 && strings.HasPrefix(history[len(history)-1], "Assistant: <tool_call>") {
							history = history[:len(history)-1]
						}
					}
					lastAssistantToolCallText = assistantText
					suppressNextToolResult = false
				} else {
					lastAssistantToolCallText = ""
					suppressNextToolResult = false
				}
				history = append(history, "Assistant: "+assistantText)
			}
			continue
		}

		suppressNextToolResult = false
		history = append(history, agyRoleLabel(role)+": "+content)
	}

	preflightDirective := buildPreflightDirective(preflightedTools, executedTools, lastPreflightedTool)
	if preflightDirective != "" {
		sysParts = append(sysParts, preflightDirective)
	}

	var b strings.Builder
	if len(sysParts) > 0 {
		b.WriteString("### SYSTEM INSTRUCTIONS & POLICIES\n\n")
		b.WriteString(strings.Join(sysParts, "\n\n"))
		b.WriteString("\n\n")
	}

	if toolsPrompt != "" {
		b.WriteString(toolsPrompt)
		b.WriteString("\n\n")
	}

	if len(customerTurns) > 1 {
		b.WriteString("### CHRONOLOGICAL CUSTOMER STATEMENTS (Review Carefully):\n")
		for idx, ct := range customerTurns {
			b.WriteString(fmt.Sprintf("- [Message %d]: %s\n", idx+1, ct))
		}
		b.WriteString("\n")
	}

	if len(history) > 0 {
		b.WriteString("### CONVERSATION HISTORY\n\n")
		b.WriteString(strings.Join(history, "\n\n"))
		b.WriteString("\n\n")
	}

	if lastCustomerMessage != "" {
		b.WriteString("### CURRENT CUSTOMER MESSAGE & REQUIRED ACTION\n\n")
		b.WriteString("Customer: ")
		b.WriteString(lastCustomerMessage)
		b.WriteString("\n\n")
		b.WriteString("CRITICAL DIRECTIVE FOR ASSISTANT:\n")
		b.WriteString("- The assistant has ALREADY introduced itself and greeted the customer. NEVER repeat the greeting, introduction, or persona opening.\n")
		b.WriteString("- The customer has ALREADY specified their required details (e.g. body area, clinic branch, appointment date/time) in earlier turns or customer statements above. DO NOT re-ask for details already provided!\n")
		b.WriteString("- If all required information to proceed is available, invoke the relevant tool immediately via <tool_call>.\n")
		b.WriteString("- NEVER execute shell commands, bash, python, or run_command.\n")
		if preflightDirective != "" {
			b.WriteString("\n")
			b.WriteString(preflightDirective)
		}
	} else if lastTurnIsToolResult {
		b.WriteString(buildToolResultDirective(lastExecutedToolName, lastToolResultContent, activeCustomerRequest, preflightDirective))
	} else if preflightDirective != "" {
		b.WriteString(preflightDirective)
	}


	return strings.TrimSpace(b.String())
}

// flattenResponsesToPrompt renders an OpenAI Responses request into a single prompt.
func flattenResponsesToPrompt(in responsesRequest) string {
	toolsPrompt := strings.TrimSpace(buildAgyToolsPrompt(in.Tools))
	tempDirective := buildAgyTempDirective(in.Temperature)

	var sysParts []string
	if tempDirective != "" {
		sysParts = append(sysParts, tempDirective)
	}
	if instr := strings.TrimSpace(in.Instructions); instr != "" {
		sysParts = append(sysParts, instr)
	}

	var history []string
	var lastCustomerMessage string
	var activeCustomerRequest string
	var customerTurns []string
	lastTurnIsToolResult := false
	seenAssistantGreeting := false

	preflightedTools := make(map[string]bool)
	executedTools := make(map[string]bool)
	var lastPreflightedTool string
	var lastToolResultContent string
	var lastExecutedToolName string
	var lastAssistantToolCallText string
	suppressNextToolResult := false

	for i, raw := range in.Input {
		isLast := (i == len(in.Input)-1)
		m, ok := raw.(map[string]any)
		if !ok {
			suppressNextToolResult = false
			if s := strings.TrimSpace(contentToTextNoMedia(raw)); s != "" {
				history = append(history, s)
			}
			continue
		}

		itemType, _ := m["type"].(string)
		role, _ := m["role"].(string)

		if itemType == "function_call" {
			name, _ := m["name"].(string)
			if name != "" {
				if name != "get_tool_instructions" {
					executedTools[name] = true
				}
				lastExecutedToolName = name
			}
			arguments := m["arguments"]
			var input any = arguments
			if argStr, ok := arguments.(string); ok && strings.TrimSpace(argStr) != "" {
				var parsed any
				if err := json.Unmarshal([]byte(argStr), &parsed); err == nil {
					input = parsed
				}
			}
			callPayload := map[string]any{
				"tool":  name,
				"input": input,
			}
			rawB, _ := json.Marshal(callPayload)
			callText := "<tool_call>\n" + string(rawB) + "\n</tool_call>"
			if tc := extractPreflightToolCode(callText); tc != "" {
				preflightedTools[tc] = true
				lastPreflightedTool = tc
			}
			lastNames := extractToolCallNames(lastAssistantToolCallText)
			isConsecutiveSameTool := name != "" && len(lastNames) == 1 && name == lastNames[0]
			if isConsecutiveSameTool {
				if len(history) >= 2 && strings.HasPrefix(history[len(history)-1], "[Tool Result") && strings.HasPrefix(history[len(history)-2], "Assistant: <tool_call>") {
					history = history[:len(history)-2]
				} else if len(history) >= 1 && strings.HasPrefix(history[len(history)-1], "Assistant: <tool_call>") {
					history = history[:len(history)-1]
				}
			}
			lastAssistantToolCallText = callText
			suppressNextToolResult = false
			history = append(history, "Assistant: "+callText)
			continue
		}

		if itemType == "function_call_output" {
			callID, _ := m["call_id"].(string)
			output, _ := m["output"].(string)
			lastToolResultContent = output
			if tc := extractPreflightToolCode(output); tc != "" {
				preflightedTools[tc] = true
				lastPreflightedTool = tc
			}
			label := "[Tool Result"
			if callID != "" {
				label += " (" + callID + ")"
			}
			label += "]"
			formatted := label + ":\n" + strings.TrimSpace(output)
			if isLast {
				lastTurnIsToolResult = true
				history = append(history, formatted)
			} else if suppressNextToolResult {
				suppressNextToolResult = false
			} else {
				history = append(history, formatted)
			}
			continue
		}

		text := strings.TrimSpace(contentToTextNoMedia(m["content"]))
		if text == "" {
			if o, ok := m["output"].(string); ok {
				text = strings.TrimSpace(o)
			}
		}
		if text == "" {
			continue
		}

		roleLower := strings.ToLower(strings.TrimSpace(role))
		if roleLower == "assistant" {
			// Suppress repetitive identical greetings/questions to break autoregressive repetition loops
			if strings.Contains(text, "معك زينة مساعدتك الرقمية") &&
				(strings.Contains(text, "لأي منطقة") || strings.Contains(text, "بأي فرع")) {
				if seenAssistantGreeting {
					continue
				}
				seenAssistantGreeting = true
			}
			if strings.Contains(text, "<tool_call>") {
				if tc := extractPreflightToolCode(text); tc != "" {
					preflightedTools[tc] = true
					lastPreflightedTool = tc
				}
				names := extractToolCallNames(text)
				for _, name := range names {
					if name != "get_tool_instructions" && name != "" {
						executedTools[name] = true
					}
					if name != "" {
						lastExecutedToolName = name
					}
				}
				lastNames := extractToolCallNames(lastAssistantToolCallText)
				isConsecutiveSameTool := len(names) == 1 && len(lastNames) == 1 && names[0] == lastNames[0]
				if !isLast && lastAssistantToolCallText != "" && (text == lastAssistantToolCallText || isConsecutiveSameTool) {
					suppressNextToolResult = true
					continue
				}
				lastAssistantToolCallText = text
				suppressNextToolResult = false
			} else {
				lastAssistantToolCallText = ""
				suppressNextToolResult = false
			}
			history = append(history, "Assistant: "+text)
		} else if roleLower == "system" || roleLower == "developer" {
			suppressNextToolResult = false
			history = append(history, "System: "+text)
		} else {
			suppressNextToolResult = false
			if strings.HasPrefix(text, "[SYSTEM ERROR:") || strings.Contains(text, "You injected internal reasoning") {
				continue
			}
			if !strings.HasPrefix(text, "[AUTO-CONTEXT") {
				activeCustomerRequest = text
				customerTurns = append(customerTurns, text)
			}
			if isLast {
				lastCustomerMessage = text
			} else {
				history = append(history, "Customer: "+text)
			}
		}
	}

	preflightDirective := buildPreflightDirective(preflightedTools, executedTools, lastPreflightedTool)
	if preflightDirective != "" {
		sysParts = append(sysParts, preflightDirective)
	}

	var b strings.Builder
	if len(sysParts) > 0 {
		b.WriteString("### SYSTEM INSTRUCTIONS & POLICIES\n\n")
		b.WriteString(strings.Join(sysParts, "\n\n"))
		b.WriteString("\n\n")
	}

	if toolsPrompt != "" {
		b.WriteString(toolsPrompt)
		b.WriteString("\n\n")
	}

	if len(customerTurns) > 1 {
		b.WriteString("### CHRONOLOGICAL CUSTOMER STATEMENTS (Review Carefully):\n")
		for idx, ct := range customerTurns {
			b.WriteString(fmt.Sprintf("- [Message %d]: %s\n", idx+1, ct))
		}
		b.WriteString("\n")
	}

	if len(history) > 0 {
		b.WriteString("### CONVERSATION HISTORY\n\n")
		b.WriteString(strings.Join(history, "\n\n"))
		b.WriteString("\n\n")
	}

	if lastCustomerMessage != "" {
		b.WriteString("### CURRENT CUSTOMER MESSAGE & REQUIRED ACTION\n\n")
		b.WriteString("Customer: ")
		b.WriteString(lastCustomerMessage)
		b.WriteString("\n\n")
		b.WriteString("CRITICAL DIRECTIVE FOR ASSISTANT:\n")
		b.WriteString("- The assistant has ALREADY introduced itself and greeted the customer. NEVER repeat the greeting, introduction, or persona opening.\n")
		b.WriteString("- The customer has ALREADY specified their required details (e.g. body area, clinic branch, appointment date/time) in earlier turns or customer statements above. DO NOT re-ask for details already provided!\n")
		b.WriteString("- If all required information to proceed is available, invoke the relevant tool immediately via <tool_call>.\n")
		b.WriteString("- NEVER execute shell commands, bash, python, or run_command.\n")
		if preflightDirective != "" {
			b.WriteString("\n")
			b.WriteString(preflightDirective)
		}
	} else if lastTurnIsToolResult {
		b.WriteString(buildToolResultDirective(lastExecutedToolName, lastToolResultContent, activeCustomerRequest, preflightDirective))
	} else if preflightDirective != "" {
		b.WriteString(preflightDirective)
	}


	return strings.TrimSpace(b.String())
}

var (
	agyToolTagRe             = regexp.MustCompile(`(?s)<tool_call>\s*([\s\S]*?)\s*<\/tool_call>`)
	agyToolBracketRe         = regexp.MustCompile(`(?is)\[TOOL_CALL\]\s*([\s\S]*?)\s*\[\/TOOL_CALL\]`)
	agyToolFencedRe          = regexp.MustCompile("(?s)```(?:tool_call|json)?\\s*(\\{\\s*\"(?:tool|name)\"\\s*:[\\s\\S]*?\\})\\s*```")
	agyRawJsonToolRe         = regexp.MustCompile(`(?s)\{\s*"(?:tool|name|tool_call|function)"\s*:\s*"[^"]+"\s*,\s*"(?:input|arguments|parameters|params)"\s*:\s*\{[\s\S]*?\}\s*\}`)
	agyToolResultHeaderRe    = regexp.MustCompile(`(?is)\[Tool Result(?:\s*\([^)]*\))?\]:?`)
)

func stripSimulatedToolResults(s string) string {
	for {
		loc := agyToolResultHeaderRe.FindStringIndex(s)
		if loc == nil {
			break
		}
		start := loc[0]
		idx := loc[1]

		// Skip whitespace after header
		for idx < len(s) && (s[idx] == ' ' || s[idx] == '\t' || s[idx] == '\r' || s[idx] == '\n') {
			idx++
		}

		end := idx
		if idx < len(s) && (s[idx] == '{' || s[idx] == '[') {
			openChar := s[idx]
			closeChar := byte('}')
			if openChar == '[' {
				closeChar = ']'
			}
			depth := 0
			inString := false
			escaped := false
			for i := idx; i < len(s); i++ {
				c := s[i]
				if inString {
					if escaped {
						escaped = false
					} else if c == '\\' {
						escaped = true
					} else if c == '"' {
						inString = false
					}
					continue
				}
				if c == '"' {
					inString = true
				} else if c == openChar {
					depth++
				} else if c == closeChar {
					depth--
					if depth == 0 {
						end = i + 1
						break
					}
				}
			}
			if depth > 0 {
				end = len(s)
			}
		} else {
			nl := strings.Index(s[idx:], "\n\n")
			if nl != -1 {
				end = idx + nl
			} else {
				end = len(s)
			}
		}

		for end < len(s) && (s[end] == '\r' || s[end] == '\n') {
			end++
		}

		s = s[:start] + s[end:]
	}
	return s
}

// parseAgyToolCalls extracts tool calls (<tool_call>, [TOOL_CALL], fenced json, or raw json)
// from agy output and returns the remaining text and function_call items.
func parseAgyToolCalls(text string) (string, []responsesOutputItem) {
	var items []responsesOutputItem
	var seq int

	addToolItem := func(payload map[string]any) {
		toolName := ""
		for _, k := range []string{"tool", "name", "function", "action"} {
			if s, ok := payload[k].(string); ok && strings.TrimSpace(s) != "" {
				toolName = strings.TrimSpace(s)
				break
			}
		}
		if toolName == "" {
			return
		}

		// Discard internal Antigravity / developer tools if mistakenly emitted
		switch toolName {
		case "run_command", "write_to_file", "replace_file_content", "view_file",
			"list_dir", "find_by_name", "grep_search", "read_url_content",
			"search_web", "manage_task", "schedule", "send_message",
			"invoke_subagent", "define_subagent", "manage_subagents",
			"generate_image", "ask_question", "call_mcp_tool", "list_resources", "read_resource":
			return
		}

		var input any
		for _, k := range []string{"input", "arguments", "parameters", "params"} {
			if v, ok := payload[k]; ok && v != nil {
				input = v
				break
			}
		}
		if str, ok := input.(string); ok {
			var parsed any
			if err := json.Unmarshal([]byte(str), &parsed); err == nil {
				input = parsed
			}
		}
		if input == nil {
			extra := make(map[string]any)
			for k, v := range payload {
				if k != "tool" && k != "name" && k != "function" && k != "action" && k != "type" {
					extra[k] = v
				}
			}
			if len(extra) > 0 {
				input = extra
			} else {
				input = map[string]any{}
			}
		}
		argsBytes, _ := json.Marshal(input)
		seq++
		items = append(items, responsesOutputItem{
			Type:      "function_call",
			Name:      toolName,
			Arguments: string(argsBytes),
			CallID:    fmt.Sprintf("toolu_%d_%d", time.Now().UnixNano(), seq),
		})
	}

	cleanRawJSON := func(raw string) string {
		trimmed := strings.TrimSpace(raw)
		if strings.HasPrefix(trimmed, "```") {
			lines := strings.Split(trimmed, "\n")
			if len(lines) >= 2 {
				if strings.HasPrefix(strings.TrimSpace(lines[0]), "```") {
					lines = lines[1:]
				}
				if len(lines) > 0 && strings.HasPrefix(strings.TrimSpace(lines[len(lines)-1]), "```") {
					lines = lines[:len(lines)-1]
				}
				trimmed = strings.TrimSpace(strings.Join(lines, "\n"))
			}
		}
		return trimmed
	}

	processJSON := func(raw string) {
		clean := cleanRawJSON(raw)
		if clean == "" {
			return
		}
		if strings.HasPrefix(clean, "[") && strings.HasSuffix(clean, "]") {
			var arr []map[string]any
			if err := json.Unmarshal([]byte(clean), &arr); err == nil && len(arr) > 0 {
				for _, p := range arr {
					addToolItem(p)
				}
				return
			}
		}
		var payload map[string]any
		if err := json.Unmarshal([]byte(clean), &payload); err == nil {
			addToolItem(payload)
		}
	}

	extractFromRegex := func(re *regexp.Regexp, s string) string {
		matches := re.FindAllStringSubmatchIndex(s, -1)
		if len(matches) == 0 {
			return s
		}
		var clean strings.Builder
		last := 0
		for _, m := range matches {
			clean.WriteString(s[last:m[0]])
			last = m[1]
			var rawJSON string
			if len(m) >= 4 && m[2] >= 0 && m[3] >= 0 {
				rawJSON = s[m[2]:m[3]]
			} else {
				rawJSON = s[m[0]:m[1]]
			}
			processJSON(rawJSON)
		}
		clean.WriteString(s[last:])
		return clean.String()
	}

	cleaned := text
	cleaned = extractFromRegex(agyToolTagRe, cleaned)
	cleaned = extractFromRegex(agyToolBracketRe, cleaned)
	cleaned = extractFromRegex(agyToolFencedRe, cleaned)
	cleaned = extractFromRegex(agyRawJsonToolRe, cleaned)

	// Strip any simulated tool results that the model hallucinates into text
	cleaned = stripSimulatedToolResults(cleaned)

	if len(items) == 0 {
		trimmed := cleanRawJSON(text)
		if (strings.HasPrefix(trimmed, "{") && strings.HasSuffix(trimmed, "}")) ||
			(strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]")) {
			beforeCount := len(items)
			processJSON(trimmed)
			if len(items) > beforeCount {
				cleaned = ""
			}
		}
	}

	// If tool calls were extracted, ensure cleanText does not leak stray JSON blocks
	// that would trigger Connect's looksLikeRawToolCall validation guard.
	if len(items) > 0 {
		cleaned = regexp.MustCompile(`(?s)\{[\s\S]*?"(?:name|tool|result|parameters|status)"[\s\S]*?\}`).ReplaceAllString(cleaned, "")
	}

	return strings.TrimSpace(cleaned), items
}

// agyToResponsesResponse wraps agy's final text in a synthetic responsesResponse
// (including function_call items if tool calls were emitted) so it can flow through the
// existing converters and stream emitters.
func agyToResponsesResponse(text, model string, inputTokens int) responsesResponse {
	resp := responsesResponse{
		ID:    "msg_" + strconv.FormatInt(time.Now().UnixNano(), 36),
		Model: model,
	}

	cleanText, toolItems := parseAgyToolCalls(text)
	if cleanText != "" {
		resp.Output = append(resp.Output, responsesOutputItem{
			Type:    "message",
			Role:    "assistant",
			Content: []responsesOutputContent{{Type: "output_text", Text: cleanText}},
		})
	}
	resp.Output = append(resp.Output, toolItems...)
	if len(resp.Output) == 0 {
		resp.Output = []responsesOutputItem{{
			Type:    "message",
			Role:    "assistant",
			Content: []responsesOutputContent{{Type: "output_text", Text: ""}},
		}}
	}

	resp.Usage.InputTokens = inputTokens
	resp.Usage.OutputTokens = estimateTextTokens(text)
	return resp
}

func agyNote(alias string, stream bool) string {
	return "alias=" + alias + " upstream=agy stream=" + strconv.FormatBool(stream)
}

// agyResolve produces the agy response for a request. If the attachments are
// audio/video only and Groq STT is configured, it transcribes them directly
// (fast, ~1s) and returns the transcript — agy itself can't hear audio. Otherwise
// it materializes media and runs agy on the flattened prompt as usual. A non-nil
// error means the caller should surface it so the client's fallback (e.g. Vertex)
// can take over.
func agyResolve(ctx context.Context, cfg config, parts []mediaPart, basePrompt, modelAlias string) (agyResult, error) {
	if transcript, ok, err := agyAudioTranscript(ctx, cfg, parts); err != nil {
		return agyResult{}, fmt.Errorf("transcription error: %w", err)
	} else if ok {
		return agyResult{Ok: true, Response: transcript}, nil
	}
	prompt, addDirs, err := agyMediaPrep(ctx, cfg, basePrompt, parts)
	if err != nil {
		return agyResult{}, fmt.Errorf("media error: %w", err)
	}
	requestedModel := agyModelForRequest(cfg, modelAlias, len(addDirs) > 0)
	pool := getAgyWorkerPool()
	if pool != nil && pool.IsEnabled() {
		res, poolErr := pool.Execute(ctx, prompt, requestedModel)
		if poolErr == nil && res.Ok {
			return res, nil
		}
	}
	res, err := runAgyj(ctx, cfg, prompt, requestedModel, addDirs)
	if err != nil {
		return agyResult{}, fmt.Errorf("backend error: %w", err)
	}
	if !res.Ok {
		return agyResult{}, fmt.Errorf("backend error: %s", res.Error)
	}
	return res, nil
}

// serveAgyAnthropic handles /anthropic/v1/messages for an agy-backed alias.
func serveAgyAnthropic(ctx context.Context, cfg config, in anthropicRequest, w http.ResponseWriter, r *http.Request) {
	inputTokens := estimateAnthropicRequestTokens(in)
	model := firstNonEmpty(in.Model, "agy")
	setRequestStat(r, requestStat{Model: in.Model, Upstream: "agy", Stream: in.Stream, InputTokens: inputTokens})
	setRequestNote(r, agyNote(in.Model, in.Stream))

	res, err := agyResolve(ctx, cfg, collectAnthropicMedia(in), flattenAnthropicToPrompt(in), in.Model)
	if err != nil {
		writeAnthropicError(w, http.StatusBadGateway, "agy "+err.Error())
		return
	}
	resp := agyToResponsesResponse(res.Response, model, inputTokens)
	stopReason := "end_turn"
	for _, item := range resp.Output {
		if item.Type == "function_call" {
			stopReason = "tool_use"
			break
		}
	}
	updateRequestStat(r, func(stat *requestStat) {
		stat.OutputTokens = resp.Usage.OutputTokens
		stat.StopReason = stopReason
	})
	if in.Stream {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		flusher, _ := w.(http.Flusher)
		sendAnthropicMessageStart(w, resp.ID, model, inputTokens, 0)
		writeAnthropicBufferedStreamFrom(ctx, w, resp, 0)
		fmt.Fprint(w, "data: [DONE]\n\n")
		if flusher != nil {
			flusher.Flush()
		}
		return
	}
	writeJSON(w, http.StatusOK, toAnthropicResponsesResponse(resp, model))
}

// serveAgyOpenAIChat handles /openai/v1/chat/completions for an agy-backed alias.
func serveAgyOpenAIChat(ctx context.Context, cfg config, in openAIRequest, w http.ResponseWriter, r *http.Request) {
	inputTokens := estimateOpenAIChatRequestTokens(in)
	model := in.Model
	setRequestStat(r, requestStat{Model: in.Model, Upstream: "agy", Stream: in.Stream, InputTokens: inputTokens})
	setRequestNote(r, agyNote(in.Model, in.Stream))

	res, err := agyResolve(ctx, cfg, collectOpenAIChatMedia(in), flattenOpenAIChatToPrompt(in), in.Model)
	if err != nil {
		writeOpenAIError(w, http.StatusBadGateway, "agy "+err.Error())
		return
	}
	resp := agyToResponsesResponse(res.Response, model, inputTokens)
	updateRequestStat(r, func(stat *requestStat) {
		stat.OutputTokens = resp.Usage.OutputTokens
		stat.StopReason = "stop"
	})
	if in.Stream {
		id := "chatcmpl_" + strconv.FormatInt(time.Now().UnixNano(), 36)
		created := time.Now().Unix()
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		flusher, _ := w.(http.Flusher)
		sendOpenAIChatChunk(w, id, created, model, map[string]any{"role": "assistant"}, nil)
		if res.Response != "" {
			sendOpenAIChatChunk(w, id, created, model, map[string]any{"content": res.Response}, nil)
		}
		stop := "stop"
		sendOpenAIChatChunk(w, id, created, model, map[string]any{}, &stop)
		fmt.Fprint(w, "data: [DONE]\n\n")
		if flusher != nil {
			flusher.Flush()
		}
		return
	}
	writeJSON(w, http.StatusOK, responsesToOpenAIChat(resp, model))
}

// serveAgyResponses handles /openai/v1/responses for an agy-backed alias.
// in.Model is still the public alias here (routed before resolveModel).
func serveAgyResponses(ctx context.Context, cfg config, in responsesRequest, w http.ResponseWriter, r *http.Request) {
	inputTokens := estimateCodexRequestTokens(in)
	model := in.Model
	setRequestStat(r, requestStat{Model: in.Model, Upstream: "agy", Stream: in.Stream, InputTokens: inputTokens})
	setRequestNote(r, agyNote(in.Model, in.Stream))

	res, err := agyResolve(ctx, cfg, collectResponsesMedia(in), flattenResponsesToPrompt(in), in.Model)
	if err != nil {
		writeOpenAIError(w, http.StatusBadGateway, "agy "+err.Error())
		return
	}
	resp := agyToResponsesResponse(res.Response, model, inputTokens)
	updateRequestStat(r, func(stat *requestStat) {
		stat.OutputTokens = resp.Usage.OutputTokens
		stat.StopReason = "stop"
	})
	if in.Stream {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		flusher, _ := w.(http.Flusher)
		sendEvent(w, "response.created", map[string]any{
			"type":     "response.created",
			"response": map[string]any{"id": resp.ID, "object": "response", "status": "in_progress", "model": model, "output": []any{}},
		})
		if res.Response != "" {
			sendEvent(w, "response.output_text.delta", map[string]any{
				"type":          "response.output_text.delta",
				"item_id":       resp.ID,
				"output_index":  0,
				"content_index": 0,
				"delta":         res.Response,
			})
		}
		sendEvent(w, "response.completed", map[string]any{
			"type":     "response.completed",
			"response": responsesToOpenAIResponse(resp),
		})
		fmt.Fprint(w, "data: [DONE]\n\n")
		if flusher != nil {
			flusher.Flush()
		}
		return
	}
	writeJSON(w, http.StatusOK, responsesToOpenAIResponse(resp))
}
