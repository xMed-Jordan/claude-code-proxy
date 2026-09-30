package main

// muse.go — wiring for the "muse" upstream (Forwarded-to = Muse).
//
// muse is the Meta Muse CLI (`muse exec`), backed by a Meta API key. Unlike
// agy, no wrapper binary is needed: `muse exec --json` prints machine-readable
// JSONL directly on stdout. When a model alias's forward_to == "muse", the
// three request handlers short-circuit here: we flatten the request to one
// prompt, write it to a temp file, exec `muse` (bounded by a concurrency
// semaphore + timeout), then translate the reply back into the proper Anthropic
// / OpenAI response shape — reusing the existing converters and SSE emit helpers
// so no new response formats are invented.
//
// Scope (v1): chat-only. The CLI runs with `--max-model-steps 1`,
// `--disable-write`, `--disable-shell` and `--disable-web-tools`, so it
// answers as a plain text model with no server-side file/bash execution. It
// cannot accept caller-defined tool schemas, so tool definitions are dropped —
// same scope as agy. Streaming is REAL (token-by-token) via run.output.delta
// events, not synthesized. Multi-turn is stateless: the full incoming history
// is flattened every call (no --session-id resume, --no-session-log).
//
// Event shapes (see testdata/muse/*.jsonl):
//   - text delta: payload_type "run.output.delta", text at payload.text
//   - terminal:   payload_type "run.terminal.completed", full text at
//     payload.text, payload.terminal "completed", payload.reason null
//   - task error: payload_type "task.lifecycle.failed", reason at
//     payload.event.reason. A failed *task* does not fail the run — the
//     terminal record is authoritative (the echo fixture carries a failed
//     reminder task yet exits 0 with a completed terminal).

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

// museSem bounds how many `muse` subprocesses run at once. Sized at startup
// by initMuse.
var museSem chan struct{}

func initMuse(cfg config) {
	n := cfg.MuseConcurrency
	if n < 1 {
		n = 1
	}
	museSem = make(chan struct{}, n)
	fmt.Printf("[muse] concurrency %d (max simultaneous `muse` subprocesses)\n", n)
}

func museAcquire(ctx context.Context) error {
	if museSem == nil {
		return nil // not initialized (e.g. unit tests) → run unbounded
	}
	select {
	case museSem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func museRelease() {
	if museSem == nil {
		return
	}
	select {
	case <-museSem:
	default:
	}
}

// museBinPath resolves the `muse` CLI: explicit config first, else bare
// "muse" on PATH.
func museBinPath(cfg config) string {
	if b := strings.TrimSpace(cfg.MuseBin); b != "" {
		return b
	}
	return "muse"
}

// museHome is the directory used as the child's working dir (always) and as
// its HOME when isolating. A neutral dir keeps a stray project CLAUDE.md out
// of the prompt. Created on demand.
func museHome(cfg config) string {
	d := strings.TrimSpace(cfg.MuseWorkDir)
	if d == "" {
		d = filepath.Join(os.TempDir(), "connect-ai-proxy-muse")
	}
	_ = os.MkdirAll(d, 0o700)
	return d
}

// museChildEnv builds the environment for the child `muse`. It always:
//   - strips ANTHROPIC_BASE_URL / ANTHROPIC_AUTH_TOKEN / ANTHROPIC_API_KEY and
//     any META_* var that could point the backend CLI back at this proxy (an
//     infinite loop), and
//   - sets DISABLE_AUTOUPDATER=1 so a backend run never silently self-updates
//     the CLI to an unvetted version.
//
// When an API key is configured it ISOLATES the child's HOME to a clean dir
// (so ambient user config — which the proxy may rewrite to point tooling at
// itself — is not read) and authenticates with that key. With no key, the
// inherited HOME (and its login) is used.
func museChildEnv(cfg config) []string {
	key := strings.TrimSpace(cfg.MuseAPIKey)
	isolate := key != ""
	base := os.Environ()
	out := make([]string, 0, len(base)+4)
	for _, kv := range base {
		if strings.HasPrefix(kv, "ANTHROPIC_BASE_URL=") ||
			strings.HasPrefix(kv, "ANTHROPIC_AUTH_TOKEN=") ||
			strings.HasPrefix(kv, "ANTHROPIC_API_KEY=") ||
			strings.HasPrefix(kv, "META_API_KEY=") ||
			strings.HasPrefix(kv, "META_BASE_URL=") ||
			strings.HasPrefix(kv, "META_API_URL=") ||
			strings.HasPrefix(kv, "META_API_BASE_URL=") ||
			strings.HasPrefix(kv, "META_ENDPOINT=") {
			continue
		}
		if isolate && (strings.HasPrefix(kv, "HOME=") || strings.HasPrefix(kv, "USERPROFILE=")) {
			continue
		}
		out = append(out, kv)
	}
	out = append(out, "DISABLE_AUTOUPDATER=1")
	if isolate {
		home := museHome(cfg)
		out = append(out, "HOME="+home, "USERPROFILE="+home, "META_API_KEY="+key)
	}
	return out
}

// museModelFor decides which model to pass to `muse --model`. A global
// override (PROXY_MUSE_MODEL) wins; otherwise the alias's configured upstream
// model is forwarded verbatim. A trailing context marker (e.g. "[1m]") is
// stripped since it is the proxy's alias suffix, not a real model id.
func museModelFor(cfg config, alias string) string {
	if m := strings.TrimSpace(cfg.MuseModel); m != "" {
		return m
	}
	real := strings.TrimSpace(resolveModel(cfg, alias))
	if i := strings.Index(real, "["); i > 0 {
		real = strings.TrimSpace(real[:i])
	}
	return real
}

// museArgs builds the `muse exec` argument list for a chat-only, single-step,
// tool-free run. Only discovery-verified flags are used (see the discovery
// report): --provider/--model select the backend, the prompt is delivered via
// --prompt-file (avoids ARG_MAX), and --max-model-steps 1 plus the --disable-*
// flags keep the run a plain text turn. --reasoning-effort is deliberately NOT
// passed (rejected by the echo provider; see discovery).
func museArgs(cfg config, model, promptFile string) []string {
	args := []string{"exec"}
	if p := strings.TrimSpace(cfg.MuseProvider); p != "" {
		args = append(args, "--provider", p)
	}
	if m := strings.TrimSpace(model); m != "" {
		args = append(args, "--model", m)
	}
	args = append(args,
		"--json",
		"--prompt-file", promptFile,
		"--no-session-log",       // don't persist session event logs to disk
		"--max-model-steps", "1", // single model step, no agentic loop
		"--disable-write",               // no workspace filesystem writes
		"--disable-shell",               // no workspace shell execution
		"--disable-web-tools",           // no web tools
		"--no-foreign-personal-context", // exclude foreign personal rules/skills
	)
	return args
}

// museResult is the distilled outcome of one `muse` invocation.
type museResult struct {
	Ok       bool
	Response string
	Error    string
}

// museLine is the subset of one `--json` JSONL line we act on: run.output.delta
// (incremental output), run.terminal.* (authoritative outcome) and
// task.lifecycle.failed (task-level errors).
type museLine struct {
	PayloadType string      `json:"payload_type"`
	Payload     musePayload `json:"payload"`
}

type musePayload struct {
	Kind     string     `json:"kind"`
	Text     string     `json:"text"`
	Terminal string     `json:"terminal"`
	Reason   *string    `json:"reason"`
	Event    *museEvent `json:"event"`
}

type museEvent struct {
	Kind   string `json:"kind"`
	Reason string `json:"reason"`
}

// museTextDelta returns the incremental text carried by a JSONL line, or ""
// if the line is not a run.output.delta event.
func museTextDelta(line []byte) string {
	var ev museLine
	if json.Unmarshal(bytes.TrimSpace(line), &ev) != nil {
		return ""
	}
	if ev.PayloadType == "run.output.delta" {
		return ev.Payload.Text
	}
	return ""
}

// parseMuseJSONL decodes the `--json` JSONL stream from one `muse exec` run.
// It returns an error only when the bytes are missing or carry no valid JSONL;
// a failed run is a valid parse surfaced via res.Ok/res.Error. The terminal
// record is authoritative: a completed terminal with text is success even when
// a task.lifecycle.failed record is present (the echo fixture proves this
// combination exits 0).
func parseMuseJSONL(raw []byte) (museResult, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return museResult{}, fmt.Errorf("muse produced no output")
	}
	var deltas strings.Builder
	var failedReasons []string
	gotTerminal, terminalOK := false, false
	terminalText := ""
	lines := 0
	scanner := bufio.NewScanner(bytes.NewReader(trimmed))
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var ev museLine
		if json.Unmarshal(line, &ev) != nil {
			continue
		}
		lines++
		switch {
		case ev.PayloadType == "run.output.delta":
			if ev.Payload.Text != "" {
				deltas.WriteString(ev.Payload.Text)
			}
		case ev.PayloadType == "run.terminal.completed":
			gotTerminal = true
			terminalOK = ev.Payload.Terminal == "" || ev.Payload.Terminal == "completed"
			if ev.Payload.Text != "" {
				terminalText = ev.Payload.Text
			}
		case strings.HasPrefix(ev.PayloadType, "run.terminal."):
			gotTerminal = true
			terminalOK = false
			if ev.Payload.Text != "" {
				terminalText = ev.Payload.Text
			}
			if ev.Payload.Reason != nil && strings.TrimSpace(*ev.Payload.Reason) != "" {
				failedReasons = append(failedReasons, strings.TrimSpace(*ev.Payload.Reason))
			}
		case ev.PayloadType == "task.lifecycle.failed" && ev.Payload.Event != nil && ev.Payload.Event.Kind == "failed":
			if r := strings.TrimSpace(ev.Payload.Event.Reason); r != "" {
				failedReasons = append(failedReasons, r)
			}
		}
	}
	if lines == 0 {
		return museResult{}, fmt.Errorf("muse output not JSONL: %s", truncateString(string(trimmed), 300))
	}
	if gotTerminal && terminalOK {
		res := museResult{Ok: true, Response: deltas.String()}
		if strings.TrimSpace(res.Response) == "" {
			// Fallback: no deltas observed but the terminal record carries
			// the full text.
			res.Response = terminalText
		}
		return res, nil
	}
	res := museResult{}
	if len(failedReasons) > 0 {
		res.Error = firstNonEmpty(failedReasons[0], "muse run failed")
	} else if !gotTerminal {
		res.Error = "muse produced no terminal record"
	} else {
		res.Error = "muse run failed"
	}
	return res, nil
}

// parseMuseConcurrency parses PROXY_MUSE_CONCURRENCY: how many `muse`
// subprocesses run in parallel (a global semaphore; the rest queue). Default 2;
// capped at 64.
func parseMuseConcurrency(s string) int {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil || n < 1 {
		return 2
	}
	if n > 64 {
		n = 64
	}
	return n
}

func museRetries(cfg config) int {
	if cfg.MuseRetries < 0 {
		return 0
	}
	return cfg.MuseRetries
}

// parseMuseRetries parses PROXY_MUSE_RETRIES: default 20, floor 0 (set 0 to
// disable outer retries). Capped at 50 as a runaway guard.
func parseMuseRetries(s string) int {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil || n < 0 {
		return 20
	}
	if n > 50 {
		n = 50
	}
	return n
}

// museTransient reports whether a failed run looks like a transient,
// retry-worthy condition (5xx / overloaded / rate limit / connection reset /
// empty output) rather than a permanent one (auth, bad request, disabled
// upstream — re-running those just hits the same wall).
func museTransient(res museResult, err error) bool {
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
	// NOTE: deliberately NOT matching the proxy's own deadline ("timed out
	// after Ns") — re-running a turn that already exhausted the timeout just
	// burns another full timeout. Only fast, server-side transients are retried.
	for _, s := range []string{
		"internal server error", "api error: 5", "server-side issue",
		"overloaded", "529", "503", "502", "service unavailable", "bad gateway",
		"rate limit", "429", "too many requests",
		"connection reset", "connection refused", "econnreset",
		"produced no output", "no output", "no terminal record",
	} {
		if strings.Contains(msg, s) {
			return true
		}
	}
	return false
}

// museBackoffWait sleeps with exponential backoff (1,2,4,…s, capped 30s)
// before a retry, returning false if the context is cancelled meanwhile.
func museBackoffWait(ctx context.Context, attempt int) bool {
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

// museTotalTimeout caps the WHOLE retry sequence so the proxy always answers
// within the caller's HTTP wait. Keep this comfortably below that timeout.
func museTotalTimeout(cfg config) time.Duration {
	if cfg.MuseTotalTimeout > 0 {
		return cfg.MuseTotalTimeout
	}
	return 480 * time.Second
}

// museWithRetry runs `run` and retries it on transient failures (and on an
// empty-but-successful result) up to museRetries(cfg) times with backoff —
// but bounds the ENTIRE sequence by museTotalTimeout. Permanent errors and
// clean successes return immediately.
func museWithRetry(ctx context.Context, cfg config, run func(context.Context) (museResult, error)) (museResult, error) {
	totalCtx, cancel := context.WithTimeout(ctx, museTotalTimeout(cfg))
	defer cancel()

	retries := museRetries(cfg)
	var res museResult
	var err error
	for attempt := 0; attempt <= retries; attempt++ {
		if attempt > 0 {
			if !museBackoffWait(totalCtx, attempt) {
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
		if !emptyOK && !museTransient(res, err) {
			return res, err // permanent failure — don't waste a retry
		}
		// transient or empty → loop and retry (until retries or budget exhausted)
	}
	return res, err
}

// writeMusePromptFile stages the prompt in a temp file (the CLI reads it via
// --prompt-file). Returns the path + a cleanup func.
func writeMusePromptFile(cfg config, prompt string) (string, func(), error) {
	f, err := os.CreateTemp(museHome(cfg), "prompt-*.txt")
	if err != nil {
		return "", func() {}, fmt.Errorf("muse prompt file: %w", err)
	}
	path := f.Name()
	if _, err := f.WriteString(prompt); err != nil {
		f.Close()
		os.Remove(path)
		return "", func() {}, err
	}
	if err := f.Close(); err != nil {
		os.Remove(path)
		return "", func() {}, err
	}
	return path, func() { os.Remove(path) }, nil
}

// runMuse execs `muse` for a single prompt (non-streaming) and returns the
// parsed result. It acquires a concurrency slot (ctx-aware) and bounds the run
// with cfg.MuseTimeout; the prompt is delivered via a --prompt-file temp file.
func runMuse(ctx context.Context, cfg config, prompt, model string) (museResult, error) {
	if err := museAcquire(ctx); err != nil {
		return museResult{}, fmt.Errorf("queue wait cancelled: %w", err)
	}
	defer museRelease()

	promptFile, cleanup, err := writeMusePromptFile(cfg, prompt)
	if err != nil {
		return museResult{}, err
	}
	defer cleanup()

	return museWithRetry(ctx, cfg, func(c context.Context) (museResult, error) {
		return runMuseAttempt(c, cfg, promptFile, model, nil)
	})
}

// runMuseStream execs `muse` and invokes onText for each incremental
// run.output.delta. `muse exec --json` always emits JSONL, so streaming and
// non-streaming share the same attempt runner; onText may be nil.
func runMuseStream(ctx context.Context, cfg config, prompt, model string, onText func(string)) (museResult, error) {
	if err := museAcquire(ctx); err != nil {
		return museResult{}, fmt.Errorf("queue wait cancelled: %w", err)
	}
	defer museRelease()

	promptFile, cleanup, err := writeMusePromptFile(cfg, prompt)
	if err != nil {
		return museResult{}, err
	}
	defer cleanup()

	return runMuseAttempt(ctx, cfg, promptFile, model, onText)
}

func runMuseAttempt(ctx context.Context, cfg config, promptFile, model string, onText func(string)) (museResult, error) {
	cctx, cancel := context.WithTimeout(ctx, museTimeout(cfg))
	defer cancel()

	cmd := exec.CommandContext(cctx, museBinPath(cfg), museArgs(cfg, model, promptFile)...)
	cmd.Env = museChildEnv(cfg)
	cmd.Dir = museHome(cfg)

	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return museResult{}, fmt.Errorf("stdout pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return museResult{}, fmt.Errorf("start: %w", err)
	}

	var raw bytes.Buffer
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		raw.Write(line)
		raw.WriteByte('\n')
		if d := museTextDelta(line); d != "" && onText != nil {
			onText(d)
		}
	}
	waitErr := cmd.Wait()

	if cctx.Err() == context.DeadlineExceeded {
		return museResult{}, fmt.Errorf("timed out after %ds", int(museTimeout(cfg)/time.Second))
	}
	if ctx.Err() != nil {
		return museResult{}, ctx.Err()
	}
	out := bytes.TrimSpace(raw.Bytes())
	if len(out) == 0 {
		detail := strings.TrimSpace(stderr.String())
		if detail == "" && waitErr != nil {
			detail = waitErr.Error()
		}
		if detail == "" {
			detail = "no output"
		}
		return museResult{}, fmt.Errorf("produced no output: %s", truncateString(detail, 300))
	}
	return parseMuseJSONL(out)
}

func museTimeout(cfg config) time.Duration {
	if cfg.MuseTimeout > 0 {
		return cfg.MuseTimeout
	}
	return 180 * time.Second
}

func museNote(alias string, stream bool) string {
	return "alias=" + alias + " upstream=muse stream=" + strconv.FormatBool(stream)
}

// serveMuseAnthropic handles /anthropic/v1/messages for a muse-backed alias.
func serveMuseAnthropic(ctx context.Context, cfg config, in anthropicRequest, w http.ResponseWriter, r *http.Request) {
	inputTokens := estimateAnthropicRequestTokens(in)
	model := firstNonEmpty(in.Model, "muse")
	setRequestStat(r, requestStat{Model: in.Model, Upstream: "muse", Stream: in.Stream, InputTokens: inputTokens})
	setRequestNote(r, museNote(in.Model, in.Stream))

	museModel := museModelFor(cfg, in.Model)
	prompt := flattenAnthropicToPrompt(in)

	if in.Stream {
		serveMuseAnthropicStream(ctx, cfg, model, prompt, museModel, inputTokens, w, r)
		return
	}
	res, err := runMuse(ctx, cfg, prompt, museModel)
	if err != nil {
		writeAnthropicError(w, http.StatusBadGateway, "muse "+err.Error())
		return
	}
	if !res.Ok {
		writeAnthropicError(w, http.StatusBadGateway, "muse "+res.Error)
		return
	}
	markModelAliasVerified(in.Model)
	resp := agyToResponsesResponse(res.Response, model, inputTokens)
	updateRequestStat(r, func(stat *requestStat) {
		stat.OutputTokens = resp.Usage.OutputTokens
		stat.StopReason = "end_turn"
	})
	writeJSON(w, http.StatusOK, toAnthropicResponsesResponse(resp, model))
}

func serveMuseAnthropicStream(ctx context.Context, cfg config, model, prompt, museModel string, inputTokens int, w http.ResponseWriter, r *http.Request) {
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
	res, err := runMuseStream(ctx, cfg, prompt, museModel, func(text string) {
		start()
		sendEvent(w, "content_block_delta", map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "text_delta", "text": text}})
		if flusher != nil {
			flusher.Flush()
		}
	})
	if !started && (err != nil || !res.Ok) {
		msg := "muse stream failed"
		if err != nil {
			msg = "muse " + err.Error()
		} else if res.Error != "" {
			msg = "muse " + res.Error
		}
		writeAnthropicError(w, http.StatusBadGateway, msg)
		return
	}
	start() // ensure a valid (possibly empty) text block even with no output
	if err == nil && res.Ok {
		markModelAliasVerified(model)
	}
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

// serveMuseOpenAIChat handles /openai/v1/chat/completions for a muse alias.
func serveMuseOpenAIChat(ctx context.Context, cfg config, in openAIRequest, w http.ResponseWriter, r *http.Request) {
	inputTokens := estimateOpenAIChatRequestTokens(in)
	model := in.Model
	setRequestStat(r, requestStat{Model: in.Model, Upstream: "muse", Stream: in.Stream, InputTokens: inputTokens})
	setRequestNote(r, museNote(in.Model, in.Stream))

	museModel := museModelFor(cfg, in.Model)
	prompt := flattenOpenAIChatToPrompt(in)

	if in.Stream {
		serveMuseOpenAIChatStream(ctx, cfg, model, prompt, museModel, w, r)
		return
	}
	res, err := runMuse(ctx, cfg, prompt, museModel)
	if err != nil {
		writeOpenAIError(w, http.StatusBadGateway, "muse "+err.Error())
		return
	}
	if !res.Ok {
		writeOpenAIError(w, http.StatusBadGateway, "muse "+res.Error)
		return
	}
	markModelAliasVerified(in.Model)
	resp := agyToResponsesResponse(res.Response, model, inputTokens)
	updateRequestStat(r, func(stat *requestStat) {
		stat.OutputTokens = resp.Usage.OutputTokens
		stat.StopReason = "stop"
	})
	writeJSON(w, http.StatusOK, responsesToOpenAIChat(resp, model))
}

func serveMuseOpenAIChatStream(ctx context.Context, cfg config, model, prompt, museModel string, w http.ResponseWriter, r *http.Request) {
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
	res, err := runMuseStream(ctx, cfg, prompt, museModel, func(text string) {
		start()
		sendOpenAIChatChunk(w, id, created, model, map[string]any{"content": text}, nil)
		if flusher != nil {
			flusher.Flush()
		}
	})
	if !started && (err != nil || !res.Ok) {
		msg := "muse stream failed"
		if err != nil {
			msg = "muse " + err.Error()
		} else if res.Error != "" {
			msg = "muse " + res.Error
		}
		writeOpenAIError(w, http.StatusBadGateway, msg)
		return
	}
	start()
	if err == nil && res.Ok {
		markModelAliasVerified(model)
	}
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

// serveMuseResponses handles /openai/v1/responses for a muse alias.
// in.Model is still the public alias here (routed before resolveModel).
func serveMuseResponses(ctx context.Context, cfg config, in responsesRequest, w http.ResponseWriter, r *http.Request) {
	inputTokens := estimateCodexRequestTokens(in)
	model := in.Model
	setRequestStat(r, requestStat{Model: in.Model, Upstream: "muse", Stream: in.Stream, InputTokens: inputTokens})
	setRequestNote(r, museNote(in.Model, in.Stream))

	museModel := museModelFor(cfg, in.Model)
	prompt := flattenResponsesToPrompt(in)

	if in.Stream {
		serveMuseResponsesStream(ctx, cfg, model, prompt, museModel, inputTokens, w, r)
		return
	}
	res, err := runMuse(ctx, cfg, prompt, museModel)
	if err != nil {
		writeOpenAIError(w, http.StatusBadGateway, "muse "+err.Error())
		return
	}
	if !res.Ok {
		writeOpenAIError(w, http.StatusBadGateway, "muse "+res.Error)
		return
	}
	markModelAliasVerified(in.Model)
	resp := agyToResponsesResponse(res.Response, model, inputTokens)
	updateRequestStat(r, func(stat *requestStat) {
		stat.OutputTokens = resp.Usage.OutputTokens
		stat.StopReason = "stop"
	})
	writeJSON(w, http.StatusOK, responsesToOpenAIResponse(resp))
}

func serveMuseResponsesStream(ctx context.Context, cfg config, model, prompt, museModel string, inputTokens int, w http.ResponseWriter, r *http.Request) {
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
	res, err := runMuseStream(ctx, cfg, prompt, museModel, func(text string) {
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
		msg := "muse stream failed"
		if err != nil {
			msg = "muse " + err.Error()
		} else if res.Error != "" {
			msg = "muse " + res.Error
		}
		writeOpenAIError(w, http.StatusBadGateway, msg)
		return
	}
	start()
	if err == nil && res.Ok {
		markModelAliasVerified(model)
	}
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
