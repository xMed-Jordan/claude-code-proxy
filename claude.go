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

// claudeWorkDir is where the child runs. A neutral dir (temp by default) keeps a
// stray project CLAUDE.md out of the prompt; --safe-mode also suppresses that.
func claudeWorkDir(cfg config) string {
	if d := strings.TrimSpace(cfg.ClaudeWorkDir); d != "" {
		return d
	}
	return os.TempDir()
}

// claudeChildEnv injects the subscription OAuth token (so the CLI authenticates
// without keychain access) on top of the proxy's own environment.
func claudeChildEnv(cfg config) []string {
	env := os.Environ()
	if tok := strings.TrimSpace(cfg.ClaudeOAuthToken); tok != "" {
		env = append(env, "CLAUDE_CODE_OAUTH_TOKEN="+tok)
	}
	return env
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

// runClaude execs `claude` for a single prompt (non-streaming) and returns the
// parsed result. It acquires a concurrency slot (ctx-aware) and bounds the run
// with cfg.ClaudeTimeout; the prompt is piped on stdin.
func runClaude(ctx context.Context, cfg config, prompt, model string) (claudeResult, error) {
	if err := claudeAcquire(ctx); err != nil {
		return claudeResult{}, fmt.Errorf("queue wait cancelled: %w", err)
	}
	defer claudeRelease()

	cctx, cancel := context.WithTimeout(ctx, claudeTimeout(cfg))
	defer cancel()

	cmd := exec.CommandContext(cctx, claudeBinPath(cfg), claudeArgs(cfg, model, false)...)
	cmd.Stdin = strings.NewReader(prompt)
	cmd.Env = claudeChildEnv(cfg)
	cmd.Dir = claudeWorkDir(cfg)

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

// runClaudeStream execs `claude` in stream-json mode and invokes onText for each
// incremental text_delta. It returns the distilled result (Response = full text)
// once the process exits. onText may be nil.
func runClaudeStream(ctx context.Context, cfg config, prompt, model string, onText func(string)) (claudeResult, error) {
	if err := claudeAcquire(ctx); err != nil {
		return claudeResult{}, fmt.Errorf("queue wait cancelled: %w", err)
	}
	defer claudeRelease()

	cctx, cancel := context.WithTimeout(ctx, claudeTimeout(cfg))
	defer cancel()

	cmd := exec.CommandContext(cctx, claudeBinPath(cfg), claudeArgs(cfg, model, true)...)
	cmd.Stdin = strings.NewReader(prompt)
	cmd.Env = claudeChildEnv(cfg)
	cmd.Dir = claudeWorkDir(cfg)

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

// serveClaudeAnthropic handles /anthropic/v1/messages for a claude-backed alias.
func serveClaudeAnthropic(ctx context.Context, cfg config, in anthropicRequest, w http.ResponseWriter, r *http.Request) {
	inputTokens := estimateAnthropicRequestTokens(in)
	model := firstNonEmpty(in.Model, "claude")
	setRequestStat(r, requestStat{Model: in.Model, Upstream: "claude", Stream: in.Stream, InputTokens: inputTokens})
	setRequestNote(r, claudeNote(in.Model, in.Stream))

	prompt := flattenAnthropicToPrompt(in)
	claudeModel := claudeModelFor(cfg, in.Model)

	if in.Stream {
		serveClaudeAnthropicStream(ctx, cfg, model, prompt, claudeModel, inputTokens, w, r)
		return
	}
	res, err := runClaude(ctx, cfg, prompt, claudeModel)
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

func serveClaudeAnthropicStream(ctx context.Context, cfg config, model, prompt, claudeModel string, inputTokens int, w http.ResponseWriter, r *http.Request) {
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
	res, err := runClaudeStream(ctx, cfg, prompt, claudeModel, func(text string) {
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

	prompt := flattenOpenAIChatToPrompt(in)
	claudeModel := claudeModelFor(cfg, in.Model)

	if in.Stream {
		serveClaudeOpenAIChatStream(ctx, cfg, model, prompt, claudeModel, w, r)
		return
	}
	res, err := runClaude(ctx, cfg, prompt, claudeModel)
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

func serveClaudeOpenAIChatStream(ctx context.Context, cfg config, model, prompt, claudeModel string, w http.ResponseWriter, r *http.Request) {
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
	res, err := runClaudeStream(ctx, cfg, prompt, claudeModel, func(text string) {
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

	prompt := flattenResponsesToPrompt(in)
	claudeModel := claudeModelFor(cfg, in.Model)

	if in.Stream {
		serveClaudeResponsesStream(ctx, cfg, model, prompt, claudeModel, inputTokens, w, r)
		return
	}
	res, err := runClaude(ctx, cfg, prompt, claudeModel)
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

func serveClaudeResponsesStream(ctx context.Context, cfg config, model, prompt, claudeModel string, inputTokens int, w http.ResponseWriter, r *http.Request) {
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
	res, err := runClaudeStream(ctx, cfg, prompt, claudeModel, func(text string) {
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
