package main

// muse_api.go — the "api" transport for the muse upstream (PROXY_MUSE_TRANSPORT,
// default "api"; "cli" keeps the `muse exec` path in muse.go as a rollback).
//
// Meta's Model API (api.meta.ai) speaks the Anthropic Messages API on
// /v1/messages and the OpenAI APIs on /v1/chat/completions and /v1/responses,
// so a muse alias is served by forwarding the caller's request over HTTPS — no
// subprocess, no prompt flattening, native tools, real streaming and prompt
// caching. Verified against the live API on 2026-10-13 (see the plan in the
// commit message): unknown parameters are rejected with 400 (so the body is
// allow-listed), the "[1m]" alias suffix is not a model id (404), and thinking
// tokens count against max_tokens (so a floor is applied).
//
// Credentials come from the proxy only — never from the caller:
// PROXY_MUSE_API_KEY from the proxy .env, else providers.meta.api_key from the
// service user's `muse login` file (auth.json), re-read whenever it changes.
// The key guard opens Muse Code now and then (and whenever Meta rejects the key)
// to keep that login fresh, and adopts the new key if Muse Code changed it.
//
// Connect's delegated tool loop (connect_tools + X-Connect-Callback-* headers)
// runs here in the proxy with NATIVE tools: each tool_use is executed through
// Connect's callback (mcpExecuteTool) and fed back as a tool_result. Only the
// Meta HTTP call is ever retried — a tool is never executed twice.

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const museAPIDefaultBase = "https://api.meta.ai/v1"

// museTransport reports which muse transport serves requests: "api" (default)
// or "cli" (the legacy `muse exec` subprocess path).
func museTransport(cfg config) string {
	return parseMuseTransport(cfg.MuseTransport)
}

func parseMuseTransport(s string) string {
	if strings.EqualFold(strings.TrimSpace(s), "cli") {
		return "cli"
	}
	return "api"
}

// parseMuseMinMaxTokens parses PROXY_MUSE_MIN_MAX_TOKENS: the floor applied to
// a request's max_tokens (Meta counts thinking against it). Default 16384;
// 0 disables the floor; capped at 128000.
func parseMuseMinMaxTokens(s string) int {
	s = strings.TrimSpace(s)
	if s == "" {
		return 16384
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 {
		return 16384
	}
	if n > 128000 {
		n = 128000
	}
	return n
}

// parseMuseKeyCheckInterval parses PROXY_MUSE_KEY_CHECK_INTERVAL: how often the
// key guard opens Muse Code. Accepts a Go duration ("6h", "90m") or plain
// seconds. Default 6h; 0 disables the periodic run (the on-401 run stays).
func parseMuseKeyCheckInterval(s string) time.Duration {
	s = strings.TrimSpace(s)
	if s == "" {
		return 6 * time.Hour
	}
	if n, err := strconv.Atoi(s); err == nil {
		if n <= 0 {
			return 0
		}
		return time.Duration(n) * time.Second
	}
	d, err := time.ParseDuration(s)
	if err != nil || d < 0 {
		return 6 * time.Hour
	}
	return d
}

// ----------------------------------------------------------------------------
// Credentials — from the proxy only.
// ----------------------------------------------------------------------------

type museCred struct {
	Key    string
	Base   string
	Source string // "env" (PROXY_MUSE_API_KEY) or "login" (auth.json)
}

type museAuthFile struct {
	Providers struct {
		Meta struct {
			APIKey     string `json:"api_key"`
			APIBaseURL string `json:"api_base_url"`
		} `json:"meta"`
	} `json:"providers"`
}

var (
	museKeyMu    sync.Mutex
	museKeyCache struct {
		path  string
		mtime time.Time
		size  int64
		key   string
		base  string
	}
)

// museKeyFP is a short, non-reversible fingerprint of a key for logs. The key
// itself is never logged.
func museKeyFP(key string) string {
	if strings.TrimSpace(key) == "" {
		return "none"
	}
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])[:12]
}

// museAuthPath is the service user's `muse login` file: PROXY_MUSE_AUTH_PATH,
// else the same place the muse launcher uses ($XDG_CONFIG_HOME/muse/auth.json,
// else ~/.config/muse/auth.json).
func museAuthPath(cfg config) string {
	if p := strings.TrimSpace(cfg.MuseAuthPath); p != "" {
		return p
	}
	if x := strings.TrimSpace(os.Getenv("XDG_CONFIG_HOME")); x != "" {
		return filepath.Join(x, "muse", "auth.json")
	}
	home, err := os.UserHomeDir()
	if err != nil || strings.TrimSpace(home) == "" {
		return ""
	}
	return filepath.Join(home, ".config", "muse", "auth.json")
}

// museLoginCred returns the key + base URL from the login file. The file is
// re-read only when its mtime/size change (or force); a missing or unreadable
// file keeps serving the last good key.
func museLoginCred(cfg config, force bool) (string, string, error) {
	path := museAuthPath(cfg)
	if path == "" {
		return "", "", errors.New("no home directory to find the muse login in")
	}
	museKeyMu.Lock()
	defer museKeyMu.Unlock()
	cached := museKeyCache.path == path && museKeyCache.key != ""
	st, err := os.Stat(path)
	if err != nil {
		if cached {
			return museKeyCache.key, museKeyCache.base, nil
		}
		return "", "", fmt.Errorf("no muse login at %s (run `muse login` as the service user or set PROXY_MUSE_API_KEY)", path)
	}
	if !force && cached && st.ModTime().Equal(museKeyCache.mtime) && st.Size() == museKeyCache.size {
		return museKeyCache.key, museKeyCache.base, nil
	}
	raw, err := os.ReadFile(path)
	var af museAuthFile
	if err == nil {
		err = json.Unmarshal(raw, &af)
	}
	key := strings.TrimSpace(af.Providers.Meta.APIKey)
	if err != nil || key == "" {
		if cached {
			return museKeyCache.key, museKeyCache.base, nil
		}
		return "", "", fmt.Errorf("no providers.meta.api_key in %s (run `muse login` as the service user)", path)
	}
	if cached && key != museKeyCache.key {
		fmt.Printf("[muse-key] login key changed on disk before=%s after=%s\n", museKeyFP(museKeyCache.key), museKeyFP(key))
	}
	museKeyCache.path = path
	museKeyCache.mtime = st.ModTime()
	museKeyCache.size = st.Size()
	museKeyCache.key = key
	museKeyCache.base = strings.TrimSpace(af.Providers.Meta.APIBaseURL)
	return museKeyCache.key, museKeyCache.base, nil
}

// museAPICredential resolves the Meta key: PROXY_MUSE_API_KEY wins, else the
// service user's muse login. Caller-supplied credentials are never used.
func museAPICredential(cfg config, force bool) (museCred, error) {
	if k := strings.TrimSpace(cfg.MuseAPIKey); k != "" {
		return museCred{Key: k, Base: museAPIBaseFor(cfg, ""), Source: "env"}, nil
	}
	key, fileBase, err := museLoginCred(cfg, force)
	if err != nil {
		return museCred{}, err
	}
	return museCred{Key: key, Base: museAPIBaseFor(cfg, fileBase), Source: "login"}, nil
}

func museAPIBaseFor(cfg config, fileBase string) string {
	return strings.TrimRight(firstNonEmpty(strings.TrimSpace(cfg.MuseAPIBase), strings.TrimSpace(fileBase), museAPIDefaultBase), "/")
}

// ----------------------------------------------------------------------------
// Key guard — open Muse Code now and then; adopt a new key if it changed.
// ----------------------------------------------------------------------------

type museRefreshResult struct {
	Reason  string
	Before  string // key fingerprint before the run
	After   string // key fingerprint after the run
	Changed bool
	CLI     string // "ok" or a short error
	Models  int    // GET /models status with the after-key (0 = not reached)
	Skipped string
}

type museRefreshCall struct {
	done chan struct{}
	res  museRefreshResult
}

var (
	museRefreshMu  sync.Mutex
	museRefreshCur *museRefreshCall
	// museKeyRefreshTimeout bounds one Muse Code run of the key guard.
	museKeyRefreshTimeout = 90 * time.Second
	museGuardOnce         sync.Once
)

// museKeyRefresh opens Muse Code once (single-flight: concurrent callers share
// one run) and reports whether the login key changed. The run itself is not
// tied to the caller's context, so a cancelled request never kills a refresh
// other requests are waiting on.
func museKeyRefresh(ctx context.Context, cfg config, reason string) museRefreshResult {
	museRefreshMu.Lock()
	call := museRefreshCur
	if call == nil {
		call = &museRefreshCall{done: make(chan struct{})}
		museRefreshCur = call
		go func() {
			call.res = museRunKeyRefresh(cfg, reason)
			museRefreshMu.Lock()
			museRefreshCur = nil
			museRefreshMu.Unlock()
			close(call.done)
		}()
	}
	museRefreshMu.Unlock()
	select {
	case <-call.done:
		return call.res
	case <-ctx.Done():
		return museRefreshResult{Reason: reason, Skipped: "caller cancelled"}
	}
}

func museKeyCheckModel(cfg config) string {
	if alias := validationMuseModel(cfg); alias != "" {
		if m := museModelFor(cfg, alias); m != "" {
			return m
		}
	}
	return firstNonEmpty(strings.TrimSpace(cfg.MuseModel), "muse-spark-1.3")
}

// museKeyCheckArgs is the smallest real Muse Code run: one model step, every
// workspace tool and reminder agent off, nothing persisted.
func museKeyCheckArgs(cfg config) []string {
	args := []string{"exec"}
	if p := strings.TrimSpace(cfg.MuseProvider); p != "" {
		args = append(args, "--provider", p)
	}
	if m := museKeyCheckModel(cfg); m != "" {
		args = append(args, "--model", m)
	}
	return append(args,
		"--json",
		"--max-model-steps", "1",
		"--disable-write",
		"--disable-shell",
		"--disable-web-tools",
		"--disable-reminders",
		"--no-session-log",
		"--no-foreign-personal-context",
		"Reply with exactly: ok",
	)
}

func museRunKeyRefresh(cfg config, reason string) museRefreshResult {
	res := museRefreshResult{Reason: reason}
	if strings.TrimSpace(cfg.MuseAPIKey) != "" {
		res.Skipped = "PROXY_MUSE_API_KEY is set (the proxy key is used as-is)"
		fmt.Printf("[muse-key] reason=%s skipped: %s\n", reason, res.Skipped)
		return res
	}
	bin := museBinPath(cfg)
	if _, err := exec.LookPath(bin); err != nil {
		res.Skipped = "muse CLI not found (" + bin + ")"
		fmt.Printf("[muse-key] reason=%s skipped: %s\n", reason, res.Skipped)
		return res
	}
	before, _ := museAPICredential(cfg, true)
	res.Before = museKeyFP(before.Key)

	ctx, cancel := context.WithTimeout(context.Background(), museKeyRefreshTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, museKeyCheckArgs(cfg)...)
	cmd.Env = museChildEnv(cfg)
	cmd.Dir = museHome(cfg)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	switch {
	case ctx.Err() == context.DeadlineExceeded:
		res.CLI = fmt.Sprintf("timed out after %ds", int(museKeyRefreshTimeout/time.Second))
	case err != nil:
		res.CLI = truncateString(firstNonEmpty(strings.TrimSpace(stderr.String()), err.Error()), 200)
	default:
		if parsed, perr := parseMuseJSONL(out); perr != nil {
			res.CLI = truncateString(perr.Error(), 200)
		} else if !parsed.Ok {
			res.CLI = truncateString(firstNonEmpty(parsed.Error, "run failed"), 200)
		} else {
			res.CLI = "ok"
		}
	}

	after, aerr := museAPICredential(cfg, true)
	res.After = museKeyFP(after.Key)
	res.Changed = aerr == nil && after.Key != "" && after.Key != before.Key
	if aerr == nil && after.Key != "" {
		res.Models = museModelsStatus(cfg, after)
	}
	fmt.Printf("[muse-key] reason=%s cli=%s before=%s after=%s changed=%t models=%d\n",
		reason, res.CLI, res.Before, res.After, res.Changed, res.Models)
	if res.Changed {
		fmt.Printf("[muse-key] adopted the new login key (%s)\n", res.After)
	}
	if (res.Models == http.StatusUnauthorized || res.Models == http.StatusForbidden) && !res.Changed {
		fmt.Printf("[muse-key] KEY INVALID — run `muse login` as the proxy's service user (%s)\n", museAuthPath(cfg))
	}
	return res
}

// museModelsStatus is the cheap key check: GET {base}/models with this key.
func museModelsStatus(cfg config, cred museCred) int {
	resp, err := museAPISend(context.Background(), http.MethodGet, cred.Base+"/models", cred.Key, nil, nil, 30*time.Second)
	if err != nil {
		return 0
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode
}

// museRecoverKey runs after Meta rejected usedKey (401/403). It returns true when
// a DIFFERENT key is now available: first by re-reading the login file (free),
// then by opening Muse Code through the key guard.
func museRecoverKey(ctx context.Context, cfg config, usedKey, reason string) bool {
	if strings.TrimSpace(cfg.MuseAPIKey) != "" {
		return false
	}
	if cred, err := museAPICredential(cfg, true); err == nil && cred.Key != usedKey {
		fmt.Printf("[muse-key] reason=%s picked up a newer login key (%s)\n", reason, museKeyFP(cred.Key))
		return true
	}
	museKeyRefresh(ctx, cfg, reason)
	cred, err := museAPICredential(cfg, false)
	return err == nil && cred.Key != "" && cred.Key != usedKey
}

// startMuseKeyGuard logs the credential in use and starts the background guard:
// the first Muse Code run 2 minutes after startup, then every
// PROXY_MUSE_KEY_CHECK_INTERVAL, with an hourly GET /models in between (a 401
// there triggers a run at once).
func startMuseKeyGuard(cfg config) {
	if museTransport(cfg) != "api" {
		return
	}
	museGuardOnce.Do(func() {
		if cred, err := museAPICredential(cfg, true); err != nil {
			fmt.Printf("[muse-key] no Meta key yet: %v\n", err)
		} else {
			fmt.Printf("[muse-key] source=%s key=%s base=%s check_interval=%s\n", cred.Source, museKeyFP(cred.Key), cred.Base, cfg.MuseKeyCheckInterval)
		}
		go museKeyGuardLoop(cfg)
	})
}

func museKeyGuardLoop(cfg config) {
	interval := cfg.MuseKeyCheckInterval
	time.Sleep(2 * time.Minute)
	if interval > 0 {
		museKeyRefresh(context.Background(), cfg, "startup")
	}
	last := time.Now()
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for range ticker.C {
		if interval > 0 && time.Since(last) >= interval {
			museKeyRefresh(context.Background(), cfg, "periodic")
			last = time.Now()
			continue
		}
		cred, err := museAPICredential(cfg, false)
		if err != nil {
			fmt.Printf("[muse-key] hourly check: %v\n", err)
			continue
		}
		if st := museModelsStatus(cfg, cred); st == http.StatusUnauthorized || st == http.StatusForbidden {
			fmt.Printf("[muse-key] hourly check: models=%d for key %s\n", st, museKeyFP(cred.Key))
			museKeyRefresh(context.Background(), cfg, "models-"+strconv.Itoa(st))
			last = time.Now()
		}
	}
}

// ----------------------------------------------------------------------------
// Outbound HTTP with retry.
// ----------------------------------------------------------------------------

// museAPIClient has no overall timeout: streams can run for minutes. Each
// attempt bounds its wait for response headers instead (museAPISend).
var museAPIClient = &http.Client{}

// museAPIBackoffUnit is the base of the exponential backoff between retries
// (1,2,4,… units, capped at 30 units). A var so tests can shrink it.
var museAPIBackoffUnit = time.Second

type museTimeoutError struct{ d time.Duration }

func (e museTimeoutError) Error() string {
	return fmt.Sprintf("timed out after %ds waiting for Meta", int(e.d/time.Second))
}

// museCancelBody cancels the attempt's context when the body is closed, so a
// streamed body stays readable after the header wait ends.
type museCancelBody struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (b museCancelBody) Close() error {
	err := b.ReadCloser.Close()
	b.cancel()
	return err
}

func museAPITransientStatus(code int) bool {
	switch code {
	case http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusBadGateway,
		http.StatusServiceUnavailable, http.StatusGatewayTimeout, 529:
		return true
	}
	return false
}

// museAPISend performs one HTTP attempt against Meta with the given key.
// headerTimeout bounds the wait for the response headers only.
func museAPISend(ctx context.Context, method, url, key string, body []byte, hdr http.Header, headerTimeout time.Duration) (*http.Response, error) {
	actx, cancel := context.WithCancel(ctx)
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(actx, method, url, rdr)
	if err != nil {
		cancel()
		return nil, err
	}
	for k, vs := range hdr {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Authorization", "Bearer "+key)
	timer := time.AfterFunc(headerTimeout, cancel)
	resp, err := museAPIClient.Do(req)
	if !timer.Stop() {
		// The header deadline fired: treat as a timeout even if Do just returned.
		if err == nil {
			resp.Body.Close()
		}
		cancel()
		return nil, museTimeoutError{headerTimeout}
	}
	if err != nil {
		cancel()
		return nil, err
	}
	resp.Body = museCancelBody{ReadCloser: resp.Body, cancel: cancel}
	return resp, nil
}

func museAPIBackoff(ctx context.Context, attempt int, deadline time.Time) bool {
	d := time.Duration(1<<uint(min(attempt-1, 5))) * museAPIBackoffUnit
	if d > 30*museAPIBackoffUnit {
		d = 30 * museAPIBackoffUnit
	}
	if remaining := time.Until(deadline); d >= remaining {
		return false
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

// museAPIDo sends one request to Meta and handles everything before the first
// byte reaches the client: transient retries (network errors, 429/5xx/529) with
// backoff, all inside deadline; and one key-recovery retry after a 401/403. The
// proxy's own per-attempt timeout is not retried (it would only burn budget).
// A non-retryable or final response is returned unread for the caller to relay.
func museAPIDo(ctx context.Context, cfg config, method, path string, body []byte, hdr http.Header, deadline time.Time) (*http.Response, error) {
	retries := museRetries(cfg)
	authTried := false
	var lastErr error
	for attempt := 0; attempt <= retries; attempt++ {
		if attempt > 0 && !museAPIBackoff(ctx, attempt, deadline) {
			break
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			break
		}
		cred, err := museAPICredential(cfg, false)
		if err != nil {
			return nil, err
		}
		timeout := museTimeout(cfg)
		if remaining < timeout {
			timeout = remaining
		}
		resp, err := museAPISend(ctx, method, cred.Base+path, cred.Key, body, hdr, timeout)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			var te museTimeoutError
			if errors.As(err, &te) {
				return nil, err
			}
			lastErr = err
			continue
		}
		if (resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden) && !authTried {
			authTried = true
			if museRecoverKey(ctx, cfg, cred.Key, "auth-"+strconv.Itoa(resp.StatusCode)) {
				museDrain(resp)
				attempt-- // a key-recovery retry is not a transient retry
				continue
			}
			return resp, nil
		}
		if museAPITransientStatus(resp.StatusCode) && attempt < retries && time.Until(deadline) > 0 {
			lastErr = fmt.Errorf("Meta HTTP %d", resp.StatusCode)
			museDrain(resp)
			continue
		}
		return resp, nil
	}
	if lastErr == nil {
		lastErr = errors.New("retry budget exhausted")
	}
	return nil, lastErr
}

func museDrain(resp *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
}

// ----------------------------------------------------------------------------
// Request bodies.
// ----------------------------------------------------------------------------

// museAnthropicAllowedKeys are the top-level Messages API fields Meta accepts.
// Everything else (connect_tools, speed, stop_sequences, …) is dropped: Meta
// rejects unknown parameters with a 400.
var museAnthropicAllowedKeys = map[string]bool{
	"model": true, "messages": true, "system": true, "max_tokens": true,
	"metadata": true, "stream": true, "temperature": true, "top_p": true,
	"top_k": true, "tools": true, "tool_choice": true, "thinking": true,
	"service_tier": true, "output_config": true,
}

func museJSON(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

func museMinMaxTokens(cfg config) int {
	if cfg.MuseMinMaxTokens > 0 {
		return cfg.MuseMinMaxTokens
	}
	return 0
}

// museAPIAnthropicBody turns the caller's raw Messages body into what Meta
// accepts, keeping every allowed field byte-for-byte (system blocks,
// cache_control, images, thinking blocks). extraTools (Connect's catalog) are
// appended as native tools. Returns the body map and the dropped field names.
func museAPIAnthropicBody(cfg config, raw []byte, alias string, extraTools []connectToolDef) (map[string]json.RawMessage, []string, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, nil, err
	}
	if m == nil {
		return nil, nil, errors.New("empty body")
	}
	var dropped []string
	for k := range m {
		if !museAnthropicAllowedKeys[k] {
			delete(m, k)
			dropped = append(dropped, k)
		}
	}
	m["model"] = museJSON(museModelFor(cfg, alias))

	if oc, ok := m["output_config"]; ok {
		var o map[string]json.RawMessage
		if json.Unmarshal(oc, &o) != nil {
			delete(m, "output_config")
			dropped = append(dropped, "output_config")
		} else {
			for k := range o {
				if k != "effort" {
					dropped = append(dropped, "output_config."+k)
				}
			}
			if eff, has := o["effort"]; has {
				m["output_config"] = museJSON(map[string]json.RawMessage{"effort": eff})
			} else {
				delete(m, "output_config")
			}
		}
	}

	if th, ok := m["thinking"]; ok {
		var t map[string]json.RawMessage
		if json.Unmarshal(th, &t) == nil {
			if _, has := t["display"]; has {
				delete(t, "display")
				m["thinking"] = museJSON(t)
				dropped = append(dropped, "thinking.display")
			}
		}
	}

	if tools, changed := museAPITools(m["tools"], extraTools); changed {
		m["tools"] = tools
	}

	if msgs, orphans, unanswered, changed := museRepairToolPairs(m["messages"]); changed {
		m["messages"] = msgs
		fmt.Printf("[muse-api] model=%s history repaired: orphan_tool_results=%d unanswered_tool_uses=%d\n", alias, orphans, unanswered)
	}

	if floor := museMinMaxTokens(cfg); floor > 0 {
		var mt int
		_ = json.Unmarshal(m["max_tokens"], &mt)
		if mt < floor {
			m["max_tokens"] = museJSON(floor)
		}
	}
	sort.Strings(dropped)
	return m, dropped, nil
}

// museAPITools normalizes server web_search tools to the version Meta accepts
// (web_search_20250305, no max_uses) and appends Connect's catalog as native
// tools (skipping names the caller already defined).
func museAPITools(raw json.RawMessage, extra []connectToolDef) (json.RawMessage, bool) {
	var tools []map[string]json.RawMessage
	if len(bytes.TrimSpace(raw)) > 0 {
		if json.Unmarshal(raw, &tools) != nil {
			return raw, false
		}
	}
	changed := false
	have := map[string]bool{}
	for _, t := range tools {
		var typ, name string
		_ = json.Unmarshal(t["type"], &typ)
		_ = json.Unmarshal(t["name"], &name)
		have[name] = true
		if strings.HasPrefix(typ, "web_search") {
			if _, ok := t["max_uses"]; ok {
				delete(t, "max_uses")
				changed = true
			}
			if typ != "web_search_20250305" {
				t["type"] = museJSON("web_search_20250305")
				changed = true
			}
		}
	}
	for _, ct := range extra {
		name := strings.TrimSpace(ct.Name)
		if name == "" || have[name] {
			continue
		}
		have[name] = true
		t := map[string]json.RawMessage{"name": museJSON(name)}
		if d := strings.TrimSpace(ct.Description); d != "" {
			t["description"] = museJSON(d)
		}
		if len(bytes.TrimSpace(ct.InputSchema)) > 0 && json.Valid(ct.InputSchema) {
			t["input_schema"] = ct.InputSchema
		} else {
			t["input_schema"] = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		tools = append(tools, t)
		changed = true
	}
	if !changed {
		return raw, false
	}
	return museJSON(tools), true
}

// museRepairToolPairs makes the caller's history acceptable to Meta, which
// rejects the whole request when a tool_result does not answer a tool_use of
// the assistant message right before it ("No function call found for function
// call output with call_id …") or when a tool_use is left unanswered. Connect
// sends exactly that in its proxy-managed mode: it replays old tool rows that
// carry an id but leaves out the assistant tool_use they answered, and its
// history window can also start between a call and its result. The old CLI
// transport flattened history to text, so this never mattered there.
//
// An orphan tool_result becomes a text block, so the model still sees what the
// tool returned; an unanswered tool_use is removed; a message left with no
// content is removed. Messages that are already well formed are kept byte for
// byte. Returns the new messages, the two counts, and whether anything changed.
func museRepairToolPairs(raw json.RawMessage) (json.RawMessage, int, int, bool) {
	var msgs []json.RawMessage
	if len(bytes.TrimSpace(raw)) == 0 || json.Unmarshal(raw, &msgs) != nil {
		return raw, 0, 0, false
	}
	type parsed struct {
		fields  map[string]json.RawMessage
		role    string
		blocks  []json.RawMessage // nil when content is a plain string
		types   []string
		ids     []string // tool_use id / tool_result tool_use_id per block
		changed bool
	}
	ps := make([]parsed, len(msgs))
	for i, msg := range msgs {
		p := &ps[i]
		if json.Unmarshal(msg, &p.fields) != nil {
			continue
		}
		_ = json.Unmarshal(p.fields["role"], &p.role)
		if json.Unmarshal(p.fields["content"], &p.blocks) != nil {
			p.blocks = nil
			continue
		}
		p.types = make([]string, len(p.blocks))
		p.ids = make([]string, len(p.blocks))
		for j, b := range p.blocks {
			var blk struct {
				Type      string `json:"type"`
				ID        string `json:"id"`
				ToolUseID string `json:"tool_use_id"`
			}
			_ = json.Unmarshal(b, &blk)
			p.types[j] = blk.Type
			p.ids[j] = firstNonEmpty(blk.ID, blk.ToolUseID)
		}
	}
	answers := func(i int) map[string]bool { // tool_use ids answered by message i
		out := map[string]bool{}
		if i < len(ps) && ps[i].role == "user" {
			for j, t := range ps[i].types {
				if t == "tool_result" && ps[i].ids[j] != "" {
					out[ps[i].ids[j]] = true
				}
			}
		}
		return out
	}

	orphans, unanswered := 0, 0
	open := map[string]bool{} // tool_use ids of the previous assistant message still awaiting a result
	for i := range ps {
		p := &ps[i]
		switch {
		case p.role == "assistant" && p.blocks != nil:
			next := answers(i + 1)
			open = map[string]bool{}
			kept := p.blocks[:0:0]
			for j, b := range p.blocks {
				if p.types[j] == "tool_use" {
					if !next[p.ids[j]] || open[p.ids[j]] {
						unanswered++
						p.changed = true
						continue
					}
					open[p.ids[j]] = true
				}
				kept = append(kept, b)
			}
			p.blocks = kept
		case p.role == "user" && p.blocks != nil:
			kept := p.blocks[:0:0]
			for j, b := range p.blocks {
				if p.types[j] == "tool_result" {
					if open[p.ids[j]] {
						delete(open, p.ids[j]) // a second result for the same call is an orphan too
						kept = append(kept, b)
						continue
					}
					orphans++
					p.changed = true
					if text := museToolResultText(b); text != "" {
						kept = append(kept, museJSON(map[string]string{"type": "text", "text": "[result of an earlier tool call]\n" + text}))
					}
					continue
				}
				kept = append(kept, b)
			}
			p.blocks = kept
			open = map[string]bool{}
		default:
			open = map[string]bool{}
		}
	}
	if orphans == 0 && unanswered == 0 {
		return raw, 0, 0, false
	}
	out := make([]json.RawMessage, 0, len(msgs))
	for i, p := range ps {
		if !p.changed {
			out = append(out, msgs[i])
			continue
		}
		if len(p.blocks) == 0 {
			continue
		}
		p.fields["content"] = museJSON(p.blocks)
		out = append(out, museJSON(p.fields))
	}
	return museJSON(out), orphans, unanswered, true
}

// museToolResultText is the text of a tool_result block's content (a string or
// a list of blocks, of which only the text ones are kept).
func museToolResultText(block json.RawMessage) string {
	var b struct {
		Content json.RawMessage `json:"content"`
	}
	if json.Unmarshal(block, &b) != nil || len(bytes.TrimSpace(b.Content)) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(b.Content, &s) == nil {
		return strings.TrimSpace(s)
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(b.Content, &parts) != nil {
		return ""
	}
	var texts []string
	for _, p := range parts {
		if p.Type == "text" && strings.TrimSpace(p.Text) != "" {
			texts = append(texts, strings.TrimSpace(p.Text))
		}
	}
	return strings.Join(texts, "\n")
}

// museAPIOpenAIBody prepares an OpenAI chat / Responses body: the model is
// rewritten to the real Meta id, connect_tools is dropped, and the max-tokens
// floor is applied to whichever limit field the caller sent.
func museAPIOpenAIBody(cfg config, raw []byte, alias string) ([]byte, []string, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, nil, err
	}
	if m == nil {
		return nil, nil, errors.New("empty body")
	}
	var dropped []string
	if _, ok := m["connect_tools"]; ok {
		delete(m, "connect_tools")
		dropped = append(dropped, "connect_tools")
	}
	m["model"] = museJSON(museModelFor(cfg, alias))
	if floor := museMinMaxTokens(cfg); floor > 0 {
		for _, k := range []string{"max_tokens", "max_completion_tokens", "max_output_tokens"} {
			v, ok := m[k]
			if !ok {
				continue
			}
			var n int
			_ = json.Unmarshal(v, &n)
			if n < floor {
				m[k] = museJSON(floor)
			}
		}
	}
	b, err := json.Marshal(m)
	return b, dropped, err
}

// museMediaDownloadFailed reports Meta's 400 for an attachment it could not
// fetch from its URL ("failed to download media at input[0].content[0]: …").
func museMediaDownloadFailed(status int, body []byte) bool {
	return status == http.StatusBadRequest && bytes.Contains(bytes.ToLower(body), []byte("download media"))
}

// museStripURLMedia replaces every URL-sourced attachment (image/document) in
// the messages with a short text note, so a dead or private media link costs
// the customer the attachment, not the whole reply. Inline (base64) media is
// kept. Returns true when something was replaced.
func museStripURLMedia(m map[string]json.RawMessage) bool {
	var messages []map[string]json.RawMessage
	if json.Unmarshal(m["messages"], &messages) != nil {
		return false
	}
	changed := false
	for _, msg := range messages {
		var blocks []map[string]json.RawMessage
		if json.Unmarshal(msg["content"], &blocks) != nil {
			continue
		}
		touched := false
		for i, b := range blocks {
			var src struct {
				Type string `json:"type"`
			}
			if json.Unmarshal(b["source"], &src) == nil && src.Type == "url" {
				blocks[i] = map[string]json.RawMessage{
					"type": museJSON("text"),
					"text": museJSON("[The customer sent an attachment here that could not be loaded.]"),
				}
				touched = true
			}
		}
		if touched {
			msg["content"] = museJSON(blocks)
			changed = true
		}
	}
	if changed {
		m["messages"] = museJSON(messages)
	}
	return changed
}

// museAPIPostMessages sends a /messages body; when Meta could not download a
// URL attachment it retries once with those attachments replaced by a note.
// The response is returned for the caller to read or relay.
func museAPIPostMessages(ctx context.Context, cfg config, m map[string]json.RawMessage, hdr http.Header, deadline time.Time) (*http.Response, error) {
	body, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	resp, err := museAPIDo(ctx, cfg, http.MethodPost, "/messages", body, hdr, deadline)
	if err != nil || resp.StatusCode != http.StatusBadRequest {
		return resp, err
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
	if museMediaDownloadFailed(resp.StatusCode, raw) && museStripURLMedia(m) {
		fmt.Printf("[muse-api] attachment URL not downloadable by Meta; retrying without it (%s)\n", truncateString(strings.TrimSpace(string(raw)), 200))
		body, _ = json.Marshal(m)
		return museAPIDo(ctx, cfg, http.MethodPost, "/messages", body, hdr, deadline)
	}
	resp.Body = io.NopCloser(bytes.NewReader(raw))
	return resp, nil
}

func museAnthropicHeaders(r *http.Request) http.Header {
	h := http.Header{}
	version := ""
	if r != nil {
		version = r.Header.Get("anthropic-version")
		if b := strings.TrimSpace(r.Header.Get("anthropic-beta")); b != "" {
			h.Set("anthropic-beta", b)
		}
	}
	h.Set("anthropic-version", firstNonEmpty(strings.TrimSpace(version), "2023-06-01"))
	return h
}

func museTraceDropped(ctx context.Context, dropped []string) {
	if len(dropped) == 0 {
		return
	}
	traceLogID(traceIDFromContext(ctx), "muse_api.dropped", map[string]any{"fields": dropped})
}

// ----------------------------------------------------------------------------
// Relaying responses and usage.
// ----------------------------------------------------------------------------

type museUsageJSON struct {
	InputTokens      int `json:"input_tokens"`
	OutputTokens     int `json:"output_tokens"`
	CacheRead        int `json:"cache_read_input_tokens"`
	CacheCreate      int `json:"cache_creation_input_tokens"`
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
}

func (j *museUsageJSON) in() int {
	return j.InputTokens + j.CacheRead + j.CacheCreate + j.PromptTokens
}

func (j *museUsageJSON) out() int {
	return j.OutputTokens + j.CompletionTokens
}

type museUsage struct {
	In, Out int
	Stop    string
}

func (u *museUsage) take(j *museUsageJSON) {
	if j == nil {
		return
	}
	if v := j.in(); v > u.In {
		u.In = v
	}
	if v := j.out(); v > u.Out {
		u.Out = v
	}
}

// absorb picks usage and the stop reason out of one JSON payload: a full
// Anthropic/OpenAI response, or one SSE event's data.
func (u *museUsage) absorb(b []byte) {
	var ev struct {
		Usage      *museUsageJSON `json:"usage"`
		StopReason string         `json:"stop_reason"`
		Message    *struct {
			Usage      *museUsageJSON `json:"usage"`
			StopReason string         `json:"stop_reason"`
		} `json:"message"`
		Delta    json.RawMessage `json:"delta"`
		Response *struct {
			Usage  *museUsageJSON `json:"usage"`
			Status string         `json:"status"`
		} `json:"response"`
		Choices []struct {
			FinishReason *string `json:"finish_reason"`
		} `json:"choices"`
	}
	if json.Unmarshal(b, &ev) != nil {
		return
	}
	u.take(ev.Usage)
	if ev.StopReason != "" {
		u.Stop = ev.StopReason
	}
	if ev.Message != nil {
		u.take(ev.Message.Usage)
		if ev.Message.StopReason != "" {
			u.Stop = ev.Message.StopReason
		}
	}
	if len(ev.Delta) > 0 && ev.Delta[0] == '{' {
		var d struct {
			StopReason string `json:"stop_reason"`
		}
		if json.Unmarshal(ev.Delta, &d) == nil && d.StopReason != "" {
			u.Stop = d.StopReason
		}
	}
	if ev.Response != nil {
		u.take(ev.Response.Usage)
		if ev.Response.Status != "" && ev.Response.Status != "in_progress" {
			u.Stop = ev.Response.Status
		}
	}
	for _, c := range ev.Choices {
		if c.FinishReason != nil && *c.FinishReason != "" {
			u.Stop = *c.FinishReason
		}
	}
}

type museRelayResult struct {
	Usage   museUsage
	ErrBody string // short upstream error body (non-2xx, non-stream)
}

// museRelay copies Meta's response to the client: status, content type and
// body. Event streams are written and flushed line by line (real streaming);
// usage is captured from the JSON payloads on the way through.
func museRelay(w http.ResponseWriter, resp *http.Response) museRelayResult {
	var out museRelayResult
	ct := resp.Header.Get("Content-Type")
	if ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	if strings.Contains(ct, "text/event-stream") {
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(resp.StatusCode)
		flusher, _ := w.(http.Flusher)
		br := bufio.NewReaderSize(resp.Body, 64*1024)
		for {
			line, err := br.ReadBytes('\n')
			if len(line) > 0 {
				_, _ = w.Write(line)
				if t := bytes.TrimSpace(line); bytes.HasPrefix(t, []byte("data:")) {
					out.Usage.absorb(bytes.TrimSpace(t[len("data:"):]))
				}
				if flusher != nil {
					flusher.Flush()
				}
			}
			if err != nil {
				break
			}
		}
		return out
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		out.Usage.absorb(body)
	} else {
		out.ErrBody = truncateString(strings.TrimSpace(string(body)), 300)
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(body)
	return out
}

// museAfterRelay records stats for a relayed response and logs failures.
func museAfterRelay(r *http.Request, alias string, status int, rr museRelayResult) {
	if status >= 200 && status < 300 {
		markModelAliasVerified(alias)
	} else {
		fmt.Printf("[muse-api] model=%s status=%d err=%s\n", alias, status, firstNonEmpty(rr.ErrBody, "(stream)"))
	}
	updateRequestStat(r, func(stat *requestStat) {
		if rr.Usage.In > 0 {
			stat.InputTokens = rr.Usage.In
		}
		if rr.Usage.Out > 0 {
			stat.OutputTokens = rr.Usage.Out
		}
		if rr.Usage.Stop != "" {
			stat.StopReason = rr.Usage.Stop
		}
	})
}

// ----------------------------------------------------------------------------
// Handlers.
// ----------------------------------------------------------------------------

// serveMuseAPIAnthropic handles /anthropic/v1/messages for a muse alias over
// the Meta API: Connect's delegated tool loop runs in the proxy; everything
// else (including caller-defined native tools) is a straight passthrough.
func serveMuseAPIAnthropic(ctx context.Context, cfg config, in anthropicRequest, rawBody []byte, w http.ResponseWriter, r *http.Request) {
	inputTokens := estimateAnthropicRequestTokens(in)
	model := firstNonEmpty(in.Model, "muse")
	setRequestStat(r, requestStat{Model: in.Model, Upstream: "muse", Stream: in.Stream, InputTokens: inputTokens})
	setRequestNote(r, museNote(in.Model, in.Stream)+" transport=api")
	deadline := time.Now().Add(museTotalTimeout(cfg))

	if spec := museToolSpecFromRequest(cfg, in.ConnectTools, r); spec != nil {
		serveMuseAPIAnthropicTools(ctx, cfg, in, rawBody, spec, model, inputTokens, deadline, w, r)
		return
	}

	m, dropped, err := museAPIAnthropicBody(cfg, rawBody, in.Model, nil)
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	museTraceDropped(ctx, dropped)
	resp, err := museAPIPostMessages(ctx, cfg, m, museAnthropicHeaders(r), deadline)
	if err != nil {
		fmt.Printf("[muse-api] model=%s err=%s\n", in.Model, truncateString(err.Error(), 300))
		writeAnthropicError(w, http.StatusBadGateway, "muse "+err.Error())
		return
	}
	defer resp.Body.Close()
	museAfterRelay(r, in.Model, resp.StatusCode, museRelay(w, resp))
}

// serveMuseAPIOpenAI handles /openai/v1/chat/completions (path "/chat/completions")
// and /openai/v1/responses (path "/responses") as passthroughs to Meta.
func serveMuseAPIOpenAI(ctx context.Context, cfg config, alias string, stream bool, inputTokens int, rawBody []byte, path string, w http.ResponseWriter, r *http.Request) {
	setRequestStat(r, requestStat{Model: alias, Upstream: "muse", Stream: stream, InputTokens: inputTokens})
	setRequestNote(r, museNote(alias, stream)+" transport=api")
	body, dropped, err := museAPIOpenAIBody(cfg, rawBody, alias)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	museTraceDropped(ctx, dropped)
	resp, err := museAPIDo(ctx, cfg, http.MethodPost, path, body, nil, time.Now().Add(museTotalTimeout(cfg)))
	if err != nil {
		fmt.Printf("[muse-api] model=%s err=%s\n", alias, truncateString(err.Error(), 300))
		writeOpenAIError(w, http.StatusBadGateway, "muse "+err.Error())
		return
	}
	defer resp.Body.Close()
	museAfterRelay(r, alias, resp.StatusCode, museRelay(w, resp))
}

// ----------------------------------------------------------------------------
// Connect tool loop with native tools.
// ----------------------------------------------------------------------------

type museToolLoopResult struct {
	Text       string
	Stop       string
	Turns      int
	ToolCalls  int
	Duplicates int // repeated identical calls answered without running the tool
	In, Out    int
}

// museDuplicateCallResult answers a tool call the model already made, with the
// same arguments and a successful result, earlier in this turn. Re-running it
// would repeat its side effects (a second booking, a second summary…).
const museDuplicateCallResult = `{"success":false,"error":"duplicate_call: this exact call (same tool, same arguments) already ran successfully earlier in this turn; its result is above. It was NOT run again. Do not repeat it — continue with the next step, or finish."}`

// museCallKey identifies a tool call by name + canonical arguments (Go
// marshals map keys sorted, so key order does not matter).
func museCallKey(name string, input json.RawMessage) string {
	var v any
	if json.Unmarshal(input, &v) != nil {
		return name + "\x00" + string(bytes.TrimSpace(input))
	}
	if m, ok := v.(map[string]any); ok && len(m) == 0 {
		v = nil
	}
	b, _ := json.Marshal(v)
	return name + "\x00" + string(b)
}

// museToolSucceeded reads Connect's {"success": …} flag; a result without it
// counts as success.
func museToolSucceeded(result string) bool {
	var r struct {
		Success *bool `json:"success"`
	}
	if json.Unmarshal([]byte(result), &r) != nil || r.Success == nil {
		return true
	}
	return *r.Success
}

type museContentBlock struct {
	Type  string          `json:"type"`
	Text  string          `json:"text"`
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
}

type museMessageResponse struct {
	Content    []json.RawMessage `json:"content"`
	StopReason string            `json:"stop_reason"`
	Usage      *museUsageJSON    `json:"usage"`
}

// museAPIPostMessage makes one non-stream /messages call and decodes it. A
// non-2xx answer is returned as an error carrying Meta's message.
func museAPIPostMessage(ctx context.Context, cfg config, m map[string]json.RawMessage, hdr http.Header, deadline time.Time) (museMessageResponse, error) {
	var out museMessageResponse
	resp, err := museAPIPostMessages(ctx, cfg, m, hdr, deadline)
	if err != nil {
		return out, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return out, fmt.Errorf("reading Meta response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return out, fmt.Errorf("Meta HTTP %d: %s", resp.StatusCode, truncateString(strings.TrimSpace(string(raw)), 300))
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return out, fmt.Errorf("Meta response not JSON: %s", truncateString(string(raw), 200))
	}
	return out, nil
}

// museAPIToolLoop runs the agentic loop: call Meta, execute each tool_use in
// order through Connect's callback, send the results back, until the model
// answers in text. After the turn limit one last call forces a text answer
// (tool_choice none); an empty final text is re-asked at most twice. Tools are
// never executed twice — only the Meta call itself is retried.
func museAPIToolLoop(ctx context.Context, cfg config, m map[string]json.RawMessage, messages []json.RawMessage, spec *museToolSpec, hdr http.Header, deadline time.Time) (museToolLoopResult, error) {
	var res museToolLoopResult
	allowed := map[string]bool{}
	for _, t := range spec.Tools {
		if n := strings.TrimSpace(t.Name); n != "" {
			allowed[n] = true
		}
	}
	httpc := &http.Client{Timeout: 120 * time.Second}
	maxTurns := museToolMaxTurns(cfg)
	emptyRetries := 0
	succeeded := map[string]bool{} // museCallKey → that call already ran successfully
	forceText := false
	for turn := 0; ; turn++ {
		if ctx.Err() != nil {
			return res, ctx.Err()
		}
		if time.Until(deadline) <= 0 {
			return res, fmt.Errorf("tool loop ran out of time after %d turns", res.Turns)
		}
		final := turn >= maxTurns || forceText
		m["messages"] = museJSON(messages)
		if final {
			m["tool_choice"] = json.RawMessage(`{"type":"none"}`)
		}
		msg, err := museAPIPostMessage(ctx, cfg, m, hdr, deadline)
		if err != nil {
			return res, err
		}
		// Keep any attachment fallback for the following turns.
		_ = json.Unmarshal(m["messages"], &messages)
		res.Turns++
		if msg.Usage != nil {
			res.In += msg.Usage.in()
			res.Out += msg.Usage.out()
		}
		var text strings.Builder
		var toolUses []museContentBlock
		for _, raw := range msg.Content {
			var b museContentBlock
			if json.Unmarshal(raw, &b) != nil {
				continue
			}
			switch b.Type {
			case "text":
				text.WriteString(b.Text)
			case "tool_use":
				toolUses = append(toolUses, b)
			}
		}
		if msg.StopReason == "tool_use" && len(toolUses) > 0 && !final {
			messages = append(messages, museJSON(map[string]any{"role": "assistant", "content": msg.Content}))
			results := make([]map[string]any, 0, len(toolUses))
			onlyRepeats := true
			for _, tu := range toolUses {
				key := museCallKey(tu.Name, tu.Input)
				var out string
				if succeeded[key] {
					out = museDuplicateCallResult
					res.Duplicates++
				} else {
					out = mcpExecuteTool(httpc, spec.CallbackURL, spec.CallbackToken, allowed, tu.Name, tu.Input)
					res.ToolCalls++
					succeeded[key] = museToolSucceeded(out)
					onlyRepeats = false
				}
				results = append(results, map[string]any{"type": "tool_result", "tool_use_id": tu.ID, "content": out})
			}
			messages = append(messages, museJSON(map[string]any{"role": "user", "content": results}))
			if onlyRepeats {
				// The model is looping on calls that already ran: ask for the answer.
				forceText = true
			}
			continue
		}
		if strings.TrimSpace(text.String()) == "" && emptyRetries < 2 {
			emptyRetries++
			turn-- // re-ask the same turn; no tool runs again
			continue
		}
		res.Text = text.String()
		res.Stop = msg.StopReason
		return res, nil
	}
}

func serveMuseAPIAnthropicTools(ctx context.Context, cfg config, in anthropicRequest, rawBody []byte, spec *museToolSpec, model string, inputTokens int, deadline time.Time, w http.ResponseWriter, r *http.Request) {
	m, dropped, err := museAPIAnthropicBody(cfg, rawBody, in.Model, spec.Tools)
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	museTraceDropped(ctx, dropped)
	delete(m, "stream") // the loop is non-streamed; a stream:true caller gets a synthesized SSE
	var messages []json.RawMessage
	if err := json.Unmarshal(m["messages"], &messages); err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid messages")
		return
	}
	res, err := museAPIToolLoop(ctx, cfg, m, messages, spec, museAnthropicHeaders(r), deadline)
	note := fmt.Sprintf("%s transport=api tools=%d turns=%d tool_calls=%d repeats_skipped=%d", museNote(in.Model, in.Stream), len(spec.Tools), res.Turns, res.ToolCalls, res.Duplicates)
	setRequestNote(r, note)
	fmt.Printf("[muse-api] model=%s tools=%d turns=%d tool_calls=%d repeats_skipped=%d ok=%t\n", in.Model, len(spec.Tools), res.Turns, res.ToolCalls, res.Duplicates, err == nil)
	if err != nil {
		fmt.Printf("[muse-api] model=%s err=%s\n", in.Model, truncateString(err.Error(), 300))
		writeAnthropicError(w, http.StatusBadGateway, "muse "+err.Error())
		return
	}
	markModelAliasVerified(in.Model)
	writeMuseFinalText(w, r, model, res.Text, inputTokens, in.Stream)
	updateRequestStat(r, func(stat *requestStat) {
		if res.In > 0 {
			stat.InputTokens = res.In
		}
		if res.Out > 0 {
			stat.OutputTokens = res.Out
		}
	})
}
