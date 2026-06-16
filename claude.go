package main

// claude.go — wiring for the "claude" upstream (Forwarded-to = Claude).
//
// claude is the Anthropic Claude Code CLI, backed by a Claude Max/Pro
// subscription (a long-lived OAuth token from `claude setup-token`). Unlike agy,
// no wrapper binary is needed: `claude -p --output-format json|stream-json`
// prints machine-readable output directly on stdout. When a model alias's
// forward_to == "claude", the three request handlers short-circuit here: we
// flatten the request to one prompt, exec `claude` (bounded by a concurrency
// semaphore + timeout), then translate the reply back into the proper Anthropic
// / OpenAI response shape — reusing the existing converters and SSE emit helpers
// so no new response formats are invented.
//
// Scope (v1): chat-only. The CLI is run with `--tools ""` (no built-in tools),
// `--max-turns 1` and `--strict-mcp-config`, so it answers as a plain text model
// with no server-side file/bash execution. It cannot accept caller-defined tool
// schemas (the CLI uses its own tools), so tool definitions are dropped — same
// scope as agy. Streaming is REAL (token-by-token) via stream-json text_delta
// events, not synthesized. Multi-turn is stateless: the full incoming history is
// flattened every call (no --resume, no session store).

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// claudeSem bounds how many `claude` subprocesses run at once (each one is a
// full Node process plus an API round-trip). Sized at startup by initClaude.
var claudeSem chan struct{}

func initClaude(cfg config) {
	n := cfg.ClaudeConcurrency
	if n < 1 {
		n = 1
	}
	claudeSem = make(chan struct{}, n)
}

func claudeAcquire(ctx context.Context) error {
	if claudeSem == nil {
		return nil // not initialized (e.g. unit tests) → run unbounded
	}
	select {
	case claudeSem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func claudeRelease() {
	if claudeSem == nil {
		return
	}
	select {
	case <-claudeSem:
	default:
	}
}

// claudeBinPath resolves the `claude` CLI: explicit config first, else bare
// "claude" on PATH (exec.LookPath handles the .cmd/.exe shim on Windows).
func claudeBinPath(cfg config) string {
	if b := strings.TrimSpace(cfg.ClaudeBin); b != "" {
		return b
	}
	return "claude"
}

// claudeHome is the directory used as the child's working dir (always) and as
// its HOME when isolating. A neutral dir keeps a stray project CLAUDE.md out of
// the prompt. Created on demand.
func claudeHome(cfg config) string {
	d := strings.TrimSpace(cfg.ClaudeWorkDir)
	if d == "" {
		d = filepath.Join(os.TempDir(), "connect-ai-proxy-claude")
	}
	_ = os.MkdirAll(d, 0o700)
	return d
}

// claudeChildEnv builds the environment for the child `claude`. It always:
//   - strips ANTHROPIC_BASE_URL / ANTHROPIC_AUTH_TOKEN / ANTHROPIC_API_KEY so the
//     backend CLI can never be pointed back at this proxy (an infinite loop), and
//   - sets DISABLE_AUTOUPDATER=1 so a backend run never silently self-updates the
//     CLI to an unvetted version.
//
// When an OAuth token is configured it ISOLATES the child's HOME to a clean dir
// (so the operator's ~/.claude/settings.json — which the proxy may rewrite to
// point Claude Code at itself — is not read) and authenticates with that token.
// With no token, the inherited HOME (and its subscription login) is used; the
// operator must ensure that HOME's settings do not point back at the proxy
// (see PROXY_CLAUDE_SETTINGS_SYNC).
func claudeChildEnv(cfg config) []string {
	tok := strings.TrimSpace(cfg.ClaudeOAuthToken)
	isolate := tok != ""
	base := os.Environ()
	out := make([]string, 0, len(base)+4)
	for _, kv := range base {
		if strings.HasPrefix(kv, "ANTHROPIC_BASE_URL=") ||
			strings.HasPrefix(kv, "ANTHROPIC_AUTH_TOKEN=") ||
			strings.HasPrefix(kv, "ANTHROPIC_API_KEY=") {
			continue
		}
		if isolate && (strings.HasPrefix(kv, "HOME=") || strings.HasPrefix(kv, "USERPROFILE=")) {
			continue
		}
		out = append(out, kv)
	}
	out = append(out, "DISABLE_AUTOUPDATER=1")
	if isolate {
		home := claudeHome(cfg)
		out = append(out, "HOME="+home, "USERPROFILE="+home, "CLAUDE_CODE_OAUTH_TOKEN="+tok)
	}
	return out
}

// claudeModelFor decides which model to pass to `claude --model`. A global
// override (PROXY_CLAUDE_MODEL) wins; otherwise the alias's configured upstream
// model is forwarded only when it is a Claude name (a short alias like
// sonnet/opus/haiku/fable, or a claude-* id). A gpt/gemini/codex id left on a
// claude alias by mistake is dropped so the CLI falls back to the subscription's
// own default instead of erroring. A trailing context marker (e.g. "[1m]") is
// stripped since it is the proxy's alias suffix, not a real model id.
func claudeModelFor(cfg config, alias string) string {
	if m := strings.TrimSpace(cfg.ClaudeModel); m != "" {
		return m
	}
	real := strings.TrimSpace(resolveModel(cfg, alias))
	if i := strings.Index(real, "["); i > 0 {
		real = strings.TrimSpace(real[:i])
	}
	if real == "" {
		return ""
	}
	switch strings.ToLower(real) {
	case "sonnet", "opus", "haiku", "fable":
		return real
	}
	if strings.HasPrefix(strings.ToLower(real), "claude") {
		return real
	}
	return ""
}

// claudeArgs builds the `claude` argument list for a chat-only, single-turn,
// tool-free run. The prompt itself is delivered on stdin (avoids ARG_MAX).
func claudeArgs(cfg config, model string, stream bool) []string {
	args := []string{"-p"}
	if stream {
		// stream-json output requires --verbose in print mode; partial messages
		// give us token-level text_delta events.
		args = append(args, "--output-format", "stream-json", "--verbose", "--include-partial-messages")
	} else {
		args = append(args, "--output-format", "json")
	}
	if m := strings.TrimSpace(model); m != "" {
		args = append(args, "--model", m)
	}
	args = append(args,
		"--tools", "", // disable ALL built-in tools → pure chat, no server file/bash access
		"--strict-mcp-config", // load no MCP servers
		"--max-turns", "1",    // single assistant turn, no agentic loop
		"--no-session-persistence", // don't write resumable session files
	)
	if cfg.ClaudeSafeMode {
		// Ignore ambient CLAUDE.md / skills / plugins / hooks while keeping auth
		// and model selection (differs from --bare, which would require an API key).
		args = append(args, "--safe-mode")
	}
	if sp := strings.TrimSpace(cfg.ClaudeSystemPrompt); sp != "" {
		args = append(args, "--system-prompt", sp)
	}
	if extra := strings.Fields(cfg.ClaudeExtraArgs); len(extra) > 0 {
		args = append(args, extra...)
	}
	return args
}

// claudeResult is the distilled outcome of one `claude` invocation.
type claudeResult struct {
	Ok       bool
	Response string
	Error    string
}

// claudeJSONResult mirrors the fields we read from `--output-format json` (a
// single result object). The assistant text is in `result`.
type claudeJSONResult struct {
	Type    string `json:"type"`
	Subtype string `json:"subtype"`
	IsError bool   `json:"is_error"`
	Result  string `json:"result"`
	Error   string `json:"error"`
}

// parseClaudeJSON decodes the single JSON object from `--output-format json`.
// It returns an error only when the bytes are missing or not JSON; a result with
// is_error / a non-success subtype is a valid parse surfaced via res.Ok/res.Error.
func parseClaudeJSON(raw []byte) (claudeResult, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return claudeResult{}, fmt.Errorf("claude produced no output")
	}
	var o claudeJSONResult
	if err := json.Unmarshal(trimmed, &o); err != nil {
		return claudeResult{}, fmt.Errorf("claude output not JSON (%v): %s", err, truncateString(string(trimmed), 300))
	}
	res := claudeResult{Response: o.Result}
	res.Ok = !o.IsError && (o.Subtype == "" || o.Subtype == "success")
	if !res.Ok {
		res.Error = firstNonEmpty(strings.TrimSpace(o.Error), strings.TrimSpace(o.Result), o.Subtype, "claude returned an error")
	}
	return res, nil
}

// claudeStreamLine is the subset of one `--output-format stream-json` NDJSON line
// we act on: text_delta events (incremental output) and the final result line.
type claudeStreamLine struct {
	Type    string `json:"type"`    // "system" | "assistant" | "stream_event" | "result" | ...
	Subtype string `json:"subtype"` // for result/system
	IsError bool   `json:"is_error"`
	Result  string `json:"result"`
	Error   string `json:"error"`
	Event   *struct {
		Type  string `json:"type"`
		Delta *struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"delta"`
	} `json:"event"`
}

// claudeStreamTextDelta returns the incremental text carried by a stream-json
// line, or "" if the line is not a text_delta event.
func claudeStreamTextDelta(line []byte) string {
	var ev claudeStreamLine
	if json.Unmarshal(bytes.TrimSpace(line), &ev) != nil {
		return ""
	}
	if ev.Type == "stream_event" && ev.Event != nil && ev.Event.Delta != nil && ev.Event.Delta.Type == "text_delta" {
		return ev.Event.Delta.Text
	}
	return ""
}

// claudeRetries is the max number of OUTER retries (re-running the CLI) on a
// transient failure. The CLI already retries individual API calls internally
// (Claude-Code style); this is a coarser layer for when it exhausts those and
// surfaces a transient error. With Connect's fallback disabled the proxy is the
// last line, so this is set high — the real limit is the wall-clock budget
// (claudeTotalTimeout), which keeps the proxy answering within Connect's window.
func claudeRetries(cfg config) int {
	if cfg.ClaudeRetries < 0 {
		return 0
	}
	return cfg.ClaudeRetries
}

// parseClaudeRetries parses PROXY_CLAUDE_RETRIES: default 20, floor 0 (set 0 to
// disable outer retries). With Connect's fallback turned off the proxy is the
// last line, so it should keep retrying claude until the wall-clock budget
// (claudeTotalTimeout) runs out — the count is set high enough that the time
// budget, not the count, is the real limit. Capped at 50 as a runaway guard.
func parseClaudeRetries(s string) int {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil || n < 0 {
		return 20
	}
	if n > 50 {
		n = 50
	}
	return n
}

// claudeTransient reports whether a failed run looks like a transient,
// retry-worthy condition (Anthropic 5xx / overloaded / rate limit / timeout /
// connection reset / empty output) rather than a permanent one (auth, bad
// request, disabled upstream, error_max_turns — re-running those just wastes
// tokens and hits the same wall).
func claudeTransient(res claudeResult, err error) bool {
	msg := ""
	if err != nil {
		msg = err.Error()
	}
	if !res.Ok && res.Error != "" {
		msg += " " + res.Error
	}
	msg = strings.ToLower(strings.TrimSpace(msg))
	if msg == "" {
		return false
	}
	if strings.Contains(msg, "error_max_turns") || strings.Contains(msg, "max_turns") {
		return false // not transient: needs more turns / a different prompt, not a retry
	}
	// NOTE: deliberately NOT matching the proxy's own deadline ("timed out
	// after Ns") — re-running a turn that already exhausted the timeout just
	// burns another full timeout and Connect would give up first. Only fast,
	// server-side transients are retried.
	for _, s := range []string{
		"internal server error", "api error: 5", "server-side issue",
		"overloaded", "529", "503", "502", "service unavailable", "bad gateway",
		"rate limit", "429", "too many requests",
		"connection reset", "connection refused", "econnreset",
		"produced no output", "no output",
	} {
		if strings.Contains(msg, s) {
			return true
		}
	}
	return false
}

// claudeBackoffWait sleeps with exponential backoff (1,2,4,…s, capped 30s)
// before a retry, returning false if the context is cancelled meanwhile.
func claudeBackoffWait(ctx context.Context, attempt int) bool {
	d := time.Duration(1<<uint(attempt-1)) * time.Second
	if d > 30*time.Second {
		d = 30 * time.Second
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// claudeTotalTimeout caps the WHOLE retry sequence so the proxy always answers
// within Connect's HTTP wait (config ai.api.request_timeout, ~500s). If the
// proxy ran longer, Connect would time out first and fall back — wasting the
// retry. Keep this comfortably below the caller's timeout.
func claudeTotalTimeout(cfg config) time.Duration {
	if cfg.ClaudeTotalTimeout > 0 {
		return cfg.ClaudeTotalTimeout
	}
	return 480 * time.Second
}

// claudeWithRetry runs `run` and retries it on transient failures (and on an
// empty-but-successful result) up to claudeRetries(cfg) times with backoff —
// but bounds the ENTIRE sequence by claudeTotalTimeout so it never overruns
// Connect's timeout. Each attempt is therefore min(per-attempt, remaining
// budget). Permanent errors and clean successes return immediately.
func claudeWithRetry(ctx context.Context, cfg config, run func(context.Context) (claudeResult, error)) (claudeResult, error) {
	totalCtx, cancel := context.WithTimeout(ctx, claudeTotalTimeout(cfg))
	defer cancel()

	retries := claudeRetries(cfg)
	var res claudeResult
	var err error
	for attempt := 0; attempt <= retries; attempt++ {
		if attempt > 0 {
			if !claudeBackoffWait(totalCtx, attempt) {
				return res, err // out of budget / cancelled — return the last result
			}
		}
		res, err = run(totalCtx)
		// Clean success with non-empty text → done.
		if err == nil && res.Ok && strings.TrimSpace(res.Response) != "" {
			return res, nil
		}
		// Out of total budget, or the caller cancelled → stop now.
		if totalCtx.Err() != nil || ctx.Err() != nil {
			return res, err
		}
		emptyOK := err == nil && res.Ok && strings.TrimSpace(res.Response) == ""
		if !emptyOK && !claudeTransient(res, err) {
			return res, err // permanent failure — don't waste a retry
		}
		// transient or empty → loop and retry (until retries or budget exhausted)
	}
	return res, err
}

// runClaude execs `claude` for a single prompt (non-streaming) and returns the
// parsed result. It acquires a concurrency slot (ctx-aware) and bounds the run
// with cfg.ClaudeTimeout; the prompt is piped on stdin.
func runClaude(ctx context.Context, cfg config, prompt, model string) (claudeResult, error) {
	if err := claudeAcquire(ctx); err != nil {
		return claudeResult{}, fmt.Errorf("queue wait cancelled: %w", err)
	}
	defer claudeRelease()

	return claudeWithRetry(ctx, cfg, func(c context.Context) (claudeResult, error) {
		return runClaudeAttempt(c, cfg, prompt, model)
	})
}

func runClaudeAttempt(ctx context.Context, cfg config, prompt, model string) (claudeResult, error) {
	cctx, cancel := context.WithTimeout(ctx, claudeTimeout(cfg))
	defer cancel()

	cmd := exec.CommandContext(cctx, claudeBinPath(cfg), claudeArgs(cfg, model, false)...)
	cmd.Stdin = strings.NewReader(prompt)
	cmd.Env = claudeChildEnv(cfg)
	cmd.Dir = claudeHome(cfg)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	runErr := cmd.Run()

	if cctx.Err() == context.DeadlineExceeded {
		return claudeResult{}, fmt.Errorf("timed out after %ds", int(claudeTimeout(cfg)/time.Second))
	}
	if ctx.Err() != nil {
		return claudeResult{}, ctx.Err()
	}
	out := bytes.TrimSpace(stdout.Bytes())
	if len(out) == 0 {
		detail := strings.TrimSpace(stderr.String())
		if detail == "" && runErr != nil {
			detail = runErr.Error()
		}
		if detail == "" {
			detail = "no output"
		}
		return claudeResult{}, fmt.Errorf("produced no output: %s", truncateString(detail, 300))
	}
	return parseClaudeJSON(out)
}

// runClaudeStream execs `claude` with stream-json output and invokes onText for
// each incremental text_delta. stdin is a plain prompt, or — when mediaIn — a
// stream-json user message (so images/PDFs reach the model); mediaIn adds
// --input-format stream-json. It returns the distilled result (Response = full
// text) once the process exits. onText may be nil (used for non-stream media).
func runClaudeStream(ctx context.Context, cfg config, stdin string, mediaIn bool, model string, onText func(string)) (claudeResult, error) {
	if err := claudeAcquire(ctx); err != nil {
		return claudeResult{}, fmt.Errorf("queue wait cancelled: %w", err)
	}
	defer claudeRelease()

	cctx, cancel := context.WithTimeout(ctx, claudeTimeout(cfg))
	defer cancel()

	args := claudeArgs(cfg, model, true)
	if mediaIn {
		args = append(args, "--input-format", "stream-json")
	}
	cmd := exec.CommandContext(cctx, claudeBinPath(cfg), args...)
	cmd.Stdin = strings.NewReader(stdin)
	cmd.Env = claudeChildEnv(cfg)
	cmd.Dir = claudeHome(cfg)

	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return claudeResult{}, fmt.Errorf("stdout pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return claudeResult{}, fmt.Errorf("start: %w", err)
	}

	var full strings.Builder
	result := claudeResult{Ok: true}
	gotResult := false

	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024) // full-message lines can be large
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var ev claudeStreamLine
		if json.Unmarshal(line, &ev) != nil {
			continue
		}
		switch ev.Type {
		case "stream_event":
			if ev.Event != nil && ev.Event.Delta != nil && ev.Event.Delta.Type == "text_delta" && ev.Event.Delta.Text != "" {
				full.WriteString(ev.Event.Delta.Text)
				if onText != nil {
					onText(ev.Event.Delta.Text)
				}
			}
		case "result":
			gotResult = true
			if ev.IsError || (ev.Subtype != "" && ev.Subtype != "success") {
				result.Ok = false
				result.Error = firstNonEmpty(strings.TrimSpace(ev.Error), strings.TrimSpace(ev.Result), ev.Subtype, "claude returned an error")
			} else if full.Len() == 0 && strings.TrimSpace(ev.Result) != "" {
				// Fallback: partial messages were unavailable but the result line
				// carries the full text — emit it as one delta.
				full.WriteString(ev.Result)
				if onText != nil {
					onText(ev.Result)
				}
			}
		}
	}
	scanErr := scanner.Err()
	waitErr := cmd.Wait()

	if cctx.Err() == context.DeadlineExceeded {
		return claudeResult{}, fmt.Errorf("timed out after %ds", int(claudeTimeout(cfg)/time.Second))
	}
	if ctx.Err() != nil {
		return claudeResult{}, ctx.Err()
	}
	result.Response = full.String()
	if result.Ok && !gotResult && full.Len() == 0 {
		detail := strings.TrimSpace(stderr.String())
		if detail == "" && waitErr != nil {
			detail = waitErr.Error()
		}
		if detail == "" && scanErr != nil {
			detail = scanErr.Error()
		}
		if detail == "" {
			detail = "no output"
		}
		return claudeResult{}, fmt.Errorf("produced no output: %s", truncateString(detail, 300))
	}
	if !result.Ok && result.Error == "" {
		result.Error = "claude returned an error"
	}
	return result, nil
}

func claudeTimeout(cfg config) time.Duration {
	if cfg.ClaudeTimeout > 0 {
		return cfg.ClaudeTimeout
	}
	return 180 * time.Second
}

func claudeNote(alias string, stream bool) string {
	return "alias=" + alias + " upstream=claude stream=" + strconv.FormatBool(stream)
}

// claudeMaxInlineMediaBytes caps total base64 media inlined into one stream-json
// message. `claude -p` reads stdin (capped ~10MB), so keep headroom for the JSON
// envelope + text.
const claudeMaxInlineMediaBytes = 7 << 20

// claudeMediaBlocks converts collected media parts into Anthropic image/document
// content blocks (base64 or url source), keeping only images and PDFs within the
// byte budget. Audio/video/other and oversized parts are dropped (count returned)
// — Claude can't ingest those natively (use codex/agy, or agy's Groq transcription).
func claudeMediaBlocks(parts []mediaPart, budget int) ([]any, int) {
	var blocks []any
	dropped, used := 0, 0
	for _, p := range parts {
		mt := strings.ToLower(strings.TrimSpace(p.MediaType))
		if i := strings.Index(mt, ";"); i >= 0 {
			mt = strings.TrimSpace(mt[:i])
		}
		ext := strings.ToLower(filepath.Ext(p.Filename))
		isImage := strings.HasPrefix(mt, "image/") || (mt == "" && (ext == ".png" || ext == ".jpg" || ext == ".jpeg" || ext == ".gif" || ext == ".webp" || ext == ".bmp"))
		isPDF := mt == "application/pdf" || (mt == "" && ext == ".pdf")
		if !isImage && !isPDF {
			dropped++
			continue
		}
		kind := "image"
		if isPDF {
			kind = "document"
		}
		switch {
		case p.B64 != "":
			if used+len(p.B64) > budget {
				dropped++
				continue
			}
			used += len(p.B64)
			srcMT := mt
			if srcMT == "" {
				srcMT = "image/png"
				if isPDF {
					srcMT = "application/pdf"
				}
			}
			blocks = append(blocks, map[string]any{"type": kind, "source": map[string]any{"type": "base64", "media_type": srcMT, "data": p.B64}})
		case p.URL != "":
			blocks = append(blocks, map[string]any{"type": kind, "source": map[string]any{"type": "url", "url": p.URL}})
		default:
			dropped++
		}
	}
	return blocks, dropped
}

// claudeBuildInput returns the stdin payload for `claude`. With usable image/PDF
// media it returns a stream-json user message (text + media blocks) and
// mediaIn=true; otherwise the plain flattened prompt and false. dropped counts
// media that could not be forwarded.
func claudeBuildInput(prompt string, parts []mediaPart) (string, bool, int) {
	blocks, dropped := claudeMediaBlocks(parts, claudeMaxInlineMediaBytes)
	if len(blocks) == 0 {
		return prompt, false, dropped
	}
	content := make([]any, 0, len(blocks)+1)
	if strings.TrimSpace(prompt) != "" {
		content = append(content, map[string]any{"type": "text", "text": prompt})
	}
	content = append(content, blocks...)
	msg := map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": content}}
	b, _ := json.Marshal(msg)
	return string(b) + "\n", true, dropped
}

// runClaudeUnified runs a non-streaming request: the stream-json media path when
// mediaIn (media requires stream-json output), else the plain JSON path.
func runClaudeUnified(ctx context.Context, cfg config, stdin string, mediaIn bool, model string) (claudeResult, error) {
	if mediaIn {
		return runClaudeStream(ctx, cfg, stdin, true, model, nil)
	}
	return runClaude(ctx, cfg, stdin, model)
}

// serveClaudeAnthropic handles /anthropic/v1/messages for a claude-backed alias.
func serveClaudeAnthropic(ctx context.Context, cfg config, in anthropicRequest, w http.ResponseWriter, r *http.Request) {
	inputTokens := estimateAnthropicRequestTokens(in)
	model := firstNonEmpty(in.Model, "claude")
	setRequestStat(r, requestStat{Model: in.Model, Upstream: "claude", Stream: in.Stream, InputTokens: inputTokens})
	setRequestNote(r, claudeNote(in.Model, in.Stream))

	claudeModel := claudeModelFor(cfg, in.Model)

	// Tool loop: when the request carries a Connect tool catalog + callback
	// headers and PROXY_CLAUDE_TOOLS_ENABLED is on, run the agentic loop inside
	// the CLI via the MCP gateway (which bridges tool calls back to Connect).
	// Otherwise fall through to the chat-only path below (unchanged).
	if spec := claudeToolSpecFromRequest(cfg, in.ConnectTools, r); spec != nil {
		serveClaudeAnthropicTools(ctx, cfg, in, model, claudeModel, spec, inputTokens, w, r)
		return
	}

	stdin, mediaIn, _ := claudeBuildInput(flattenAnthropicToPrompt(in), collectAnthropicMedia(in))

	if in.Stream {
		serveClaudeAnthropicStream(ctx, cfg, model, stdin, mediaIn, claudeModel, inputTokens, w, r)
		return
	}
	res, err := runClaudeUnified(ctx, cfg, stdin, mediaIn, claudeModel)
	if err != nil {
		writeAnthropicError(w, http.StatusBadGateway, "claude "+err.Error())
		return
	}
	if !res.Ok {
		writeAnthropicError(w, http.StatusBadGateway, "claude "+res.Error)
		return
	}
	resp := agyToResponsesResponse(res.Response, model, inputTokens)
	updateRequestStat(r, func(stat *requestStat) {
		stat.OutputTokens = resp.Usage.OutputTokens
		stat.StopReason = "end_turn"
	})
	writeJSON(w, http.StatusOK, toAnthropicResponsesResponse(resp, model))
}

func serveClaudeAnthropicStream(ctx context.Context, cfg config, model, stdin string, mediaIn bool, claudeModel string, inputTokens int, w http.ResponseWriter, r *http.Request) {
	flusher, _ := w.(http.Flusher)
	messageID := "msg_" + strconv.FormatInt(time.Now().UnixNano(), 36)
	started := false
	start := func() {
		if started {
			return
		}
		started = true
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		sendAnthropicMessageStart(w, messageID, model, inputTokens, 0)
		sendEvent(w, "content_block_start", map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "text", "text": ""}})
		if flusher != nil {
			flusher.Flush()
		}
	}
	res, err := runClaudeStream(ctx, cfg, stdin, mediaIn, claudeModel, func(text string) {
		start()
		sendEvent(w, "content_block_delta", map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "text_delta", "text": text}})
		if flusher != nil {
			flusher.Flush()
		}
	})
	if !started && (err != nil || !res.Ok) {
		msg := "claude stream failed"
		if err != nil {
			msg = "claude " + err.Error()
		} else if res.Error != "" {
			msg = "claude " + res.Error
		}
		writeAnthropicError(w, http.StatusBadGateway, msg)
		return
	}
	start() // ensure a valid (possibly empty) text block even with no output
	outputTokens := estimateTextTokens(res.Response)
	updateRequestStat(r, func(stat *requestStat) {
		stat.OutputTokens = outputTokens
		stat.StopReason = "end_turn"
	})
	sendEvent(w, "content_block_stop", map[string]any{"type": "content_block_stop", "index": 0})
	sendAnthropicMessageDeltaStop(w, "end_turn", outputTokens)
	sendEvent(w, "message_stop", map[string]any{"type": "message_stop"})
	fmt.Fprint(w, "data: [DONE]\n\n")
	if flusher != nil {
		flusher.Flush()
	}
}

// serveClaudeOpenAIChat handles /openai/v1/chat/completions for a claude alias.
func serveClaudeOpenAIChat(ctx context.Context, cfg config, in openAIRequest, w http.ResponseWriter, r *http.Request) {
	inputTokens := estimateOpenAIChatRequestTokens(in)
	model := in.Model
	setRequestStat(r, requestStat{Model: in.Model, Upstream: "claude", Stream: in.Stream, InputTokens: inputTokens})
	setRequestNote(r, claudeNote(in.Model, in.Stream))

	claudeModel := claudeModelFor(cfg, in.Model)
	stdin, mediaIn, _ := claudeBuildInput(flattenOpenAIChatToPrompt(in), collectOpenAIChatMedia(in))

	if in.Stream {
		serveClaudeOpenAIChatStream(ctx, cfg, model, stdin, mediaIn, claudeModel, w, r)
		return
	}
	res, err := runClaudeUnified(ctx, cfg, stdin, mediaIn, claudeModel)
	if err != nil {
		writeOpenAIError(w, http.StatusBadGateway, "claude "+err.Error())
		return
	}
	if !res.Ok {
		writeOpenAIError(w, http.StatusBadGateway, "claude "+res.Error)
		return
	}
	resp := agyToResponsesResponse(res.Response, model, inputTokens)
	updateRequestStat(r, func(stat *requestStat) {
		stat.OutputTokens = resp.Usage.OutputTokens
		stat.StopReason = "stop"
	})
	writeJSON(w, http.StatusOK, responsesToOpenAIChat(resp, model))
}

func serveClaudeOpenAIChatStream(ctx context.Context, cfg config, model, stdin string, mediaIn bool, claudeModel string, w http.ResponseWriter, r *http.Request) {
	flusher, _ := w.(http.Flusher)
	id := "chatcmpl_" + strconv.FormatInt(time.Now().UnixNano(), 36)
	created := time.Now().Unix()
	started := false
	start := func() {
		if started {
			return
		}
		started = true
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		sendOpenAIChatChunk(w, id, created, model, map[string]any{"role": "assistant"}, nil)
		if flusher != nil {
			flusher.Flush()
		}
	}
	res, err := runClaudeStream(ctx, cfg, stdin, mediaIn, claudeModel, func(text string) {
		start()
		sendOpenAIChatChunk(w, id, created, model, map[string]any{"content": text}, nil)
		if flusher != nil {
			flusher.Flush()
		}
	})
	if !started && (err != nil || !res.Ok) {
		msg := "claude stream failed"
		if err != nil {
			msg = "claude " + err.Error()
		} else if res.Error != "" {
			msg = "claude " + res.Error
		}
		writeOpenAIError(w, http.StatusBadGateway, msg)
		return
	}
	start()
	updateRequestStat(r, func(stat *requestStat) {
		stat.OutputTokens = estimateTextTokens(res.Response)
		stat.StopReason = "stop"
	})
	stop := "stop"
	sendOpenAIChatChunk(w, id, created, model, map[string]any{}, &stop)
	fmt.Fprint(w, "data: [DONE]\n\n")
	if flusher != nil {
		flusher.Flush()
	}
}

// serveClaudeResponses handles /openai/v1/responses for a claude alias.
// in.Model is still the public alias here (routed before resolveModel).
func serveClaudeResponses(ctx context.Context, cfg config, in responsesRequest, w http.ResponseWriter, r *http.Request) {
	inputTokens := estimateCodexRequestTokens(in)
	model := in.Model
	setRequestStat(r, requestStat{Model: in.Model, Upstream: "claude", Stream: in.Stream, InputTokens: inputTokens})
	setRequestNote(r, claudeNote(in.Model, in.Stream))

	claudeModel := claudeModelFor(cfg, in.Model)
	stdin, mediaIn, _ := claudeBuildInput(flattenResponsesToPrompt(in), collectResponsesMedia(in))

	if in.Stream {
		serveClaudeResponsesStream(ctx, cfg, model, stdin, mediaIn, claudeModel, inputTokens, w, r)
		return
	}
	res, err := runClaudeUnified(ctx, cfg, stdin, mediaIn, claudeModel)
	if err != nil {
		writeOpenAIError(w, http.StatusBadGateway, "claude "+err.Error())
		return
	}
	if !res.Ok {
		writeOpenAIError(w, http.StatusBadGateway, "claude "+res.Error)
		return
	}
	resp := agyToResponsesResponse(res.Response, model, inputTokens)
	updateRequestStat(r, func(stat *requestStat) {
		stat.OutputTokens = resp.Usage.OutputTokens
		stat.StopReason = "stop"
	})
	writeJSON(w, http.StatusOK, responsesToOpenAIResponse(resp))
}

func serveClaudeResponsesStream(ctx context.Context, cfg config, model, stdin string, mediaIn bool, claudeModel string, inputTokens int, w http.ResponseWriter, r *http.Request) {
	flusher, _ := w.(http.Flusher)
	respID := "msg_" + strconv.FormatInt(time.Now().UnixNano(), 36)
	started := false
	start := func() {
		if started {
			return
		}
		started = true
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		sendEvent(w, "response.created", map[string]any{
			"type":     "response.created",
			"response": map[string]any{"id": respID, "object": "response", "status": "in_progress", "model": model, "output": []any{}},
		})
		if flusher != nil {
			flusher.Flush()
		}
	}
	res, err := runClaudeStream(ctx, cfg, stdin, mediaIn, claudeModel, func(text string) {
		start()
		sendEvent(w, "response.output_text.delta", map[string]any{
			"type":          "response.output_text.delta",
			"item_id":       respID,
			"output_index":  0,
			"content_index": 0,
			"delta":         text,
		})
		if flusher != nil {
			flusher.Flush()
		}
	})
	if !started && (err != nil || !res.Ok) {
		msg := "claude stream failed"
		if err != nil {
			msg = "claude " + err.Error()
		} else if res.Error != "" {
			msg = "claude " + res.Error
		}
		writeOpenAIError(w, http.StatusBadGateway, msg)
		return
	}
	start()
	resp := agyToResponsesResponse(res.Response, model, inputTokens)
	resp.ID = respID
	updateRequestStat(r, func(stat *requestStat) {
		stat.OutputTokens = resp.Usage.OutputTokens
		stat.StopReason = "stop"
	})
	sendEvent(w, "response.completed", map[string]any{
		"type":     "response.completed",
		"response": responsesToOpenAIResponse(resp),
	})
	fmt.Fprint(w, "data: [DONE]\n\n")
	if flusher != nil {
		flusher.Flush()
	}
}

// ----------------------------------------------------------------------------
// Tool loop (caller-defined tools via the MCP gateway).
//
// When Connect's autonomous agent delegates its tool loop to the proxy, the
// claude request carries a `connect_tools` catalog + X-Connect-Callback-*
// headers. We run `claude -p` with a per-request MCP gateway (this binary in
// claude-mcp-gateway mode); the CLI runs the agentic loop and calls our single
// umbrella tool, which bridges each call back to Connect. The proxy returns
// only the model's final text. Gated by PROXY_CLAUDE_TOOLS_ENABLED. Codex never
// sets connect_tools, so none of this affects it.
// ----------------------------------------------------------------------------

// connectToolDef is one caller tool in the inbound `connect_tools` catalog.
type connectToolDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

// claudeToolSpec is the per-request tool context extracted from the inbound body + headers.
type claudeToolSpec struct {
	CallbackURL   string
	CallbackToken string
	Conversation  string
	Agent         string
	Tools         []connectToolDef
}

func parseClaudeToolMaxTurns(s string) int {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil || n < 1 {
		return 20
	}
	if n > 60 {
		n = 60
	}
	return n
}

func claudeToolMaxTurns(cfg config) int {
	if cfg.ClaudeToolMaxTurns > 0 {
		return cfg.ClaudeToolMaxTurns
	}
	return 20
}

func claudeToolTimeout(cfg config) time.Duration {
	if cfg.ClaudeToolTimeout > 0 {
		return cfg.ClaudeToolTimeout
	}
	return 600 * time.Second
}

// claudeToolSpecFromRequest returns a tool spec when tool mode is active for
// this request (master switch on + a non-empty catalog + callback url/token),
// else nil (→ chat-only path).
func claudeToolSpecFromRequest(cfg config, connectTools json.RawMessage, r *http.Request) *claudeToolSpec {
	if !cfg.ClaudeToolsEnabled || r == nil {
		return nil
	}
	if len(bytes.TrimSpace(connectTools)) == 0 {
		return nil
	}
	url := strings.TrimSpace(r.Header.Get("X-Connect-Callback-Url"))
	token := strings.TrimSpace(r.Header.Get("X-Connect-Callback-Token"))
	if url == "" || token == "" {
		return nil
	}
	var tools []connectToolDef
	if json.Unmarshal(connectTools, &tools) != nil || len(tools) == 0 {
		return nil
	}
	return &claudeToolSpec{
		CallbackURL:   url,
		CallbackToken: token,
		Conversation:  strings.TrimSpace(r.Header.Get("X-Connect-Conversation")),
		Agent:         strings.TrimSpace(r.Header.Get("X-Connect-Agent")),
		Tools:         tools,
	}
}

// claudeToolArgs builds the `claude` args for a tool-capable run. Unlike the
// chat-only claudeArgs it does NOT pass --tools "" (that would disable the MCP
// tool too); instead it allow-lists exactly the umbrella tool and raises
// --max-turns so the loop can run. --strict-mcp-config ensures ONLY our gateway
// is loaded (no ambient project/user MCP servers).
func claudeToolArgs(cfg config, model, mcpConfigPath string) []string {
	args := []string{"-p", "--output-format", "json"}
	if m := strings.TrimSpace(model); m != "" {
		args = append(args, "--model", m)
	}
	args = append(args,
		"--strict-mcp-config",
		"--mcp-config", mcpConfigPath,
		"--allowedTools", "mcp__connect__"+mcpGatewayToolName,
		"--max-turns", strconv.Itoa(claudeToolMaxTurns(cfg)),
		"--no-session-persistence",
	)
	if cfg.ClaudeSafeMode {
		args = append(args, "--safe-mode")
	}
	return args
}

// claudeToolSystemPrompt renders the standing instructions + the tool catalog
// (as data) injected via --append-system-prompt.
func claudeToolSystemPrompt(spec *claudeToolSpec) string {
	var b strings.Builder
	b.WriteString("You have access to a set of Connect business tools. Run them ONLY through the single tool `connect_execute_tool`, by passing {\"tool_name\":\"<name>\",\"arguments\":{...}}. Use only the tools listed below; do not invent names. Call one tool at a time and read its result before the next call. \"arguments\" must match the named tool's input_schema. A result of {\"success\":false,\"error\":...} means the tool failed — adapt or tell the user, do not blindly retry. These tools act on real customer data and bookings; only call a tool when the request requires it. When you have gathered enough information, reply to the user in plain text with NO further tool calls — that plain-text reply is your final answer.\n\nAvailable tools:\n")
	for _, t := range spec.Tools {
		if strings.TrimSpace(t.Name) == "" {
			continue
		}
		b.WriteString("- ")
		b.WriteString(t.Name)
		if d := strings.TrimSpace(t.Description); d != "" {
			b.WriteString(": ")
			b.WriteString(d)
		}
		b.WriteString("\n")
		if len(bytes.TrimSpace(t.InputSchema)) > 0 {
			var compact bytes.Buffer
			if json.Compact(&compact, t.InputSchema) == nil {
				b.WriteString("  input_schema: ")
				b.Write(compact.Bytes())
				b.WriteString("\n")
			}
		}
	}
	return b.String()
}

// writeClaudeMCPConfig writes a per-request mcp-config that points the CLI at
// this binary in gateway mode, passing the callback context via the server's
// `env` block (verified to propagate to the spawned MCP server). Returns the
// path + a cleanup func.
func writeClaudeMCPConfig(cfg config, spec *claudeToolSpec) (string, func(), error) {
	self, err := os.Executable()
	if err != nil || strings.TrimSpace(self) == "" {
		self = currentProxyBinaryPath()
	}
	names := make([]string, 0, len(spec.Tools))
	for _, t := range spec.Tools {
		if n := strings.TrimSpace(t.Name); n != "" {
			names = append(names, n)
		}
	}
	allowedJSON, _ := json.Marshal(names)
	conf := map[string]any{
		"mcpServers": map[string]any{
			"connect": map[string]any{
				"command": self,
				"args":    []string{"claude-mcp-gateway"},
				"env": map[string]string{
					"CONNECT_CALLBACK_URL":   spec.CallbackURL,
					"CONNECT_CALLBACK_TOKEN": spec.CallbackToken,
					"CONNECT_CONVERSATION":   spec.Conversation,
					"CONNECT_AGENT":          spec.Agent,
					"CONNECT_ALLOWED_TOOLS":  string(allowedJSON),
					"DISABLE_AUTOUPDATER":    "1",
				},
			},
		},
	}
	b, _ := json.MarshalIndent(conf, "", "  ")
	f, err := os.CreateTemp(claudeHome(cfg), "mcp-*.json")
	if err != nil {
		return "", func() {}, fmt.Errorf("mcp config: %w", err)
	}
	path := f.Name()
	if _, err := f.Write(b); err != nil {
		f.Close()
		os.Remove(path)
		return "", func() {}, err
	}
	f.Close()
	return path, func() { os.Remove(path) }, nil
}

// runClaudeTools runs one tool-capable, non-streaming claude invocation: the CLI
// runs the agentic loop (calling the MCP gateway), and we parse the final result.
func runClaudeTools(ctx context.Context, cfg config, prompt string, spec *claudeToolSpec, model string) (claudeResult, error) {
	if err := claudeAcquire(ctx); err != nil {
		return claudeResult{}, fmt.Errorf("queue wait cancelled: %w", err)
	}
	defer claudeRelease()

	mcpPath, cleanup, err := writeClaudeMCPConfig(cfg, spec)
	if err != nil {
		return claudeResult{}, err
	}
	defer cleanup()

	return claudeWithRetry(ctx, cfg, func(c context.Context) (claudeResult, error) {
		return runClaudeToolsAttempt(c, cfg, prompt, spec, model, mcpPath)
	})
}

func runClaudeToolsAttempt(ctx context.Context, cfg config, prompt string, spec *claudeToolSpec, model, mcpPath string) (claudeResult, error) {
	cctx, cancel := context.WithTimeout(ctx, claudeToolTimeout(cfg))
	defer cancel()

	args := claudeToolArgs(cfg, model, mcpPath)
	if catalog := claudeToolSystemPrompt(spec); strings.TrimSpace(catalog) != "" {
		args = append(args, "--append-system-prompt", catalog)
	}
	if extra := strings.Fields(cfg.ClaudeExtraArgs); len(extra) > 0 {
		args = append(args, extra...)
	}

	cmd := exec.CommandContext(cctx, claudeBinPath(cfg), args...)
	cmd.Stdin = strings.NewReader(prompt)
	cmd.Env = claudeChildEnv(cfg)
	cmd.Dir = claudeHome(cfg)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	runErr := cmd.Run()

	if cctx.Err() == context.DeadlineExceeded {
		return claudeResult{}, fmt.Errorf("timed out after %ds", int(claudeToolTimeout(cfg)/time.Second))
	}
	if ctx.Err() != nil {
		return claudeResult{}, ctx.Err()
	}
	out := bytes.TrimSpace(stdout.Bytes())
	if len(out) == 0 {
		detail := strings.TrimSpace(stderr.String())
		if detail == "" && runErr != nil {
			detail = runErr.Error()
		}
		if detail == "" {
			detail = "no output"
		}
		return claudeResult{}, fmt.Errorf("produced no output: %s", truncateString(detail, 300))
	}
	return parseClaudeJSON(out)
}

// serveClaudeAnthropicTools handles a tool-capable /anthropic/v1/messages turn.
// The loop runs non-streamed (so only the final answer is returned — no leaked
// intermediate "I'll call X" chatter); a stream:true request gets the final text
// synthesized as a single-block SSE.
func serveClaudeAnthropicTools(ctx context.Context, cfg config, in anthropicRequest, model, claudeModel string, spec *claudeToolSpec, inputTokens int, w http.ResponseWriter, r *http.Request) {
	setRequestNote(r, claudeNote(in.Model, in.Stream)+" tools="+strconv.Itoa(len(spec.Tools)))

	res, err := runClaudeTools(ctx, cfg, flattenAnthropicToPrompt(in), spec, claudeModel)
	if err != nil {
		writeAnthropicError(w, http.StatusBadGateway, "claude "+err.Error())
		return
	}
	if !res.Ok {
		writeAnthropicError(w, http.StatusBadGateway, "claude "+res.Error)
		return
	}
	resp := agyToResponsesResponse(res.Response, model, inputTokens)
	updateRequestStat(r, func(stat *requestStat) {
		stat.OutputTokens = resp.Usage.OutputTokens
		stat.StopReason = "end_turn"
	})

	if !in.Stream {
		writeJSON(w, http.StatusOK, toAnthropicResponsesResponse(resp, model))
		return
	}

	// stream:true → emit the (already complete) final text as one SSE text block.
	flusher, _ := w.(http.Flusher)
	messageID := "msg_" + strconv.FormatInt(time.Now().UnixNano(), 36)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	sendAnthropicMessageStart(w, messageID, model, inputTokens, 0)
	sendEvent(w, "content_block_start", map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "text", "text": ""}})
	if res.Response != "" {
		sendEvent(w, "content_block_delta", map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "text_delta", "text": res.Response}})
	}
	sendEvent(w, "content_block_stop", map[string]any{"type": "content_block_stop", "index": 0})
	sendAnthropicMessageDeltaStop(w, "end_turn", resp.Usage.OutputTokens)
	sendEvent(w, "message_stop", map[string]any{"type": "message_stop"})
	fmt.Fprint(w, "data: [DONE]\n\n")
	if flusher != nil {
		flusher.Flush()
	}
}
