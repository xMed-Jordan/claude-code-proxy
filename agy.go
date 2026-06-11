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
	"runtime"
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
// explicit Antigravity model to cfg.AgyMediaModel ("Gemini 3.5 Flash (Low)").
func agyModelForRequest(cfg config, alias string, hasMedia bool) string {
	m := agyModelFor(cfg, alias)
	if m == "" && hasMedia {
		return strings.TrimSpace(cfg.AgyMediaModel)
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
	return res, nil
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

	args := make([]string, 0, 6+2*len(addDirs))
	if m := strings.TrimSpace(model); m != "" {
		args = append(args, "--model", m)
	}
	if media {
		// Scope agy to exactly this request's files and let it read them without
		// an interactive permission prompt (which would hang print mode).
		for _, d := range addDirs {
			args = append(args, "--add-dir", d)
		}
		args = append(args, "--dangerously-skip-permissions")
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
		return m
	}
	real := strings.TrimSpace(resolveModel(cfg, alias))
	if real == "" {
		return ""
	}
	// Antigravity model names (from `agy models`) always contain a space, e.g.
	// "Gemini 3.1 Pro (High)", "Claude Opus 4.6 (Thinking)", "GPT-OSS 120B (Medium)"
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

// flattenAnthropicToPrompt renders an Anthropic request into a single prompt.
// A lone user turn with no system prompt is sent raw; otherwise a role-tagged
// transcript is built. Tool/image blocks are dropped (agy is text-only).
func flattenAnthropicToPrompt(in anthropicRequest) string {
	sys := strings.TrimSpace(contentToText(in.System))
	if sys == "" && len(in.Messages) == 1 && strings.EqualFold(in.Messages[0].Role, "user") {
		return strings.TrimSpace(contentToText(in.Messages[0].Content))
	}
	var b strings.Builder
	if sys != "" {
		b.WriteString(sys)
		b.WriteString("\n\n")
	}
	for _, msg := range in.Messages {
		text := strings.TrimSpace(contentToText(msg.Content))
		if text == "" {
			continue
		}
		b.WriteString(agyRoleLabel(msg.Role))
		b.WriteString(": ")
		b.WriteString(text)
		b.WriteString("\n\n")
	}
	return strings.TrimSpace(b.String())
}

// flattenOpenAIChatToPrompt renders an OpenAI chat request into a single prompt.
func flattenOpenAIChatToPrompt(in openAIRequest) string {
	// Fast path: a single user message with no system context.
	if len(in.Messages) == 1 && strings.EqualFold(in.Messages[0].Role, "user") {
		return strings.TrimSpace(contentToText(in.Messages[0].Content))
	}
	var b strings.Builder
	for _, msg := range in.Messages {
		text := strings.TrimSpace(contentToText(msg.Content))
		if text == "" {
			continue
		}
		b.WriteString(agyRoleLabel(msg.Role))
		b.WriteString(": ")
		b.WriteString(text)
		b.WriteString("\n\n")
	}
	return strings.TrimSpace(b.String())
}

// flattenResponsesToPrompt renders an OpenAI Responses request into a single
// prompt. Instructions become a leading system block; each input item's text is
// role-tagged (function_call_output items contribute their "output" text).
func flattenResponsesToPrompt(in responsesRequest) string {
	var b strings.Builder
	if instr := strings.TrimSpace(in.Instructions); instr != "" {
		b.WriteString(instr)
		b.WriteString("\n\n")
	}
	for _, raw := range in.Input {
		m, ok := raw.(map[string]any)
		if !ok {
			if s := strings.TrimSpace(contentToText(raw)); s != "" {
				b.WriteString(s)
				b.WriteString("\n\n")
			}
			continue
		}
		role, _ := m["role"].(string)
		text := strings.TrimSpace(contentToText(m["content"]))
		if text == "" {
			if o, ok := m["output"].(string); ok {
				text = strings.TrimSpace(o)
			}
		}
		if text == "" {
			continue
		}
		b.WriteString(agyRoleLabel(role))
		b.WriteString(": ")
		b.WriteString(text)
		b.WriteString("\n\n")
	}
	return strings.TrimSpace(b.String())
}

// agyToResponsesResponse wraps agy's final text in a synthetic responsesResponse
// (one message/text output item + estimated usage) so it can flow through the
// existing converters and stream emitters.
func agyToResponsesResponse(text, model string, inputTokens int) responsesResponse {
	resp := responsesResponse{
		ID:    "msg_" + strconv.FormatInt(time.Now().UnixNano(), 36),
		Model: model,
	}
	resp.Output = []responsesOutputItem{{
		Type:    "message",
		Role:    "assistant",
		Content: []responsesOutputContent{{Type: "output_text", Text: text}},
	}}
	resp.Usage.InputTokens = inputTokens
	resp.Usage.OutputTokens = estimateTextTokens(text)
	return resp
}

func agyNote(alias string, stream bool) string {
	return "alias=" + alias + " upstream=agy stream=" + strconv.FormatBool(stream)
}

// serveAgyAnthropic handles /anthropic/v1/messages for an agy-backed alias.
func serveAgyAnthropic(ctx context.Context, cfg config, in anthropicRequest, w http.ResponseWriter, r *http.Request) {
	inputTokens := estimateAnthropicRequestTokens(in)
	model := firstNonEmpty(in.Model, "agy")
	setRequestStat(r, requestStat{Model: in.Model, Upstream: "agy", Stream: in.Stream, InputTokens: inputTokens})
	setRequestNote(r, agyNote(in.Model, in.Stream))

	prompt, addDirs, err := agyMediaPrep(ctx, cfg, flattenAnthropicToPrompt(in), collectAnthropicMedia(in))
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "agy media error: "+err.Error())
		return
	}
	res, err := runAgyj(ctx, cfg, prompt, agyModelForRequest(cfg, in.Model, len(addDirs) > 0), addDirs)
	if err != nil {
		writeAnthropicError(w, http.StatusBadGateway, "agy backend error: "+err.Error())
		return
	}
	if !res.Ok {
		writeAnthropicError(w, http.StatusBadGateway, "agy backend error: "+res.Error)
		return
	}
	resp := agyToResponsesResponse(res.Response, model, inputTokens)
	updateRequestStat(r, func(stat *requestStat) {
		stat.OutputTokens = resp.Usage.OutputTokens
		stat.StopReason = "end_turn"
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

	prompt, addDirs, err := agyMediaPrep(ctx, cfg, flattenOpenAIChatToPrompt(in), collectOpenAIChatMedia(in))
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "agy media error: "+err.Error())
		return
	}
	res, err := runAgyj(ctx, cfg, prompt, agyModelForRequest(cfg, in.Model, len(addDirs) > 0), addDirs)
	if err != nil {
		writeOpenAIError(w, http.StatusBadGateway, "agy backend error: "+err.Error())
		return
	}
	if !res.Ok {
		writeOpenAIError(w, http.StatusBadGateway, "agy backend error: "+res.Error)
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

	prompt, addDirs, err := agyMediaPrep(ctx, cfg, flattenResponsesToPrompt(in), collectResponsesMedia(in))
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "agy media error: "+err.Error())
		return
	}
	res, err := runAgyj(ctx, cfg, prompt, agyModelForRequest(cfg, in.Model, len(addDirs) > 0), addDirs)
	if err != nil {
		writeOpenAIError(w, http.StatusBadGateway, "agy backend error: "+err.Error())
		return
	}
	if !res.Ok {
		writeOpenAIError(w, http.StatusBadGateway, "agy backend error: "+res.Error)
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
