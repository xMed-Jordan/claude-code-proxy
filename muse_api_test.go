package main

// muse_api_test.go — tests for the muse "api" transport (muse_api.go): a fake
// Meta API and a fake Connect callback run as httptest servers; the key guard
// runs the stub `muse` from muse_test.go (modes "ok" and "rotate").

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func resetMuseKeyCache(t *testing.T) {
	t.Helper()
	museKeyMu.Lock()
	museKeyCache.path, museKeyCache.key, museKeyCache.base = "", "", ""
	museKeyCache.mtime, museKeyCache.size = time.Time{}, 0
	museKeyMu.Unlock()
	t.Cleanup(func() {
		museKeyMu.Lock()
		museKeyCache.path, museKeyCache.key, museKeyCache.base = "", "", ""
		museKeyMu.Unlock()
	})
	prev := museAPIBackoffUnit
	museAPIBackoffUnit = time.Millisecond
	t.Cleanup(func() { museAPIBackoffUnit = prev })
}

func writeMuseAuth(t *testing.T, path, key string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(`{"schema_version":1,"providers":{"meta":{"api_key":"`+key+`"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
}

// fakeMeta records every request it serves and answers with handle.
type fakeMeta struct {
	mu   sync.Mutex
	reqs []fakeMetaReq
	srv  *httptest.Server
}

type fakeMetaReq struct {
	Path   string
	Auth   string
	Header http.Header
	Body   map[string]any
	Raw    string
}

func newFakeMeta(t *testing.T, handle func(n int, req fakeMetaReq, w http.ResponseWriter)) *fakeMeta {
	t.Helper()
	fm := &fakeMeta{}
	fm.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		req := fakeMetaReq{Path: r.URL.Path, Auth: r.Header.Get("Authorization"), Header: r.Header.Clone(), Raw: string(raw)}
		_ = json.Unmarshal(raw, &req.Body)
		fm.mu.Lock()
		fm.reqs = append(fm.reqs, req)
		n := len(fm.reqs)
		fm.mu.Unlock()
		handle(n, req, w)
	}))
	t.Cleanup(fm.srv.Close)
	return fm
}

func (fm *fakeMeta) requests() []fakeMetaReq {
	fm.mu.Lock()
	defer fm.mu.Unlock()
	return append([]fakeMetaReq(nil), fm.reqs...)
}

func metaJSON(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	io.WriteString(w, body)
}

func museAPITestConfig(base string) config {
	return config{
		Models:           map[string]string{"museapitest": "muse-spark-1.3[1m]"},
		ModelForward:     map[string]string{"museapitest": "muse"},
		MuseTransport:    "api",
		MuseAPIBase:      base,
		MuseAPIKey:       "k-env",
		MuseTimeout:      10 * time.Second,
		MuseTotalTimeout: 30 * time.Second,
		MuseRetries:      2,
		MuseMinMaxTokens: 16384,
		MuseToolsEnabled: true,
		MuseToolMaxTurns: 20,
	}
}

func postMessages(t *testing.T, cfg config, body string, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", strings.NewReader(body))
	r.Header.Set("x-api-key", "proxy-client-key")
	r.Header.Set("anthropic-version", "2023-06-01")
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	handleMessages(cfg)(w, r)
	return w
}

const okMessage = `{"id":"msg_1","type":"message","role":"assistant","model":"muse-spark-1.3","content":[{"type":"redacted_thinking","data":"xyz"},{"type":"text","text":"hello"}],"stop_reason":"end_turn","usage":{"input_tokens":10,"cache_read_input_tokens":5,"output_tokens":7}}`

// ---------------------------------------------------------------------------
// Parsing / config
// ---------------------------------------------------------------------------

func TestParseMuseAPISettings(t *testing.T) {
	if parseMuseTransport("") != "api" || parseMuseTransport("CLI") != "cli" || parseMuseTransport("x") != "api" {
		t.Error("parseMuseTransport")
	}
	for in, want := range map[string]int{"": 16384, "0": 0, "-1": 16384, "x": 16384, "8000": 8000, "999999": 128000} {
		if got := parseMuseMinMaxTokens(in); got != want {
			t.Errorf("parseMuseMinMaxTokens(%q)=%d want %d", in, got, want)
		}
	}
	for in, want := range map[string]time.Duration{"": 6 * time.Hour, "0": 0, "90m": 90 * time.Minute, "3600": time.Hour, "bad": 6 * time.Hour} {
		if got := parseMuseKeyCheckInterval(in); got != want {
			t.Errorf("parseMuseKeyCheckInterval(%q)=%v want %v", in, got, want)
		}
	}
}

func TestMuseAPICredential(t *testing.T) {
	resetMuseKeyCache(t)
	dir := t.TempDir()
	auth := filepath.Join(dir, "auth.json")

	// Env key wins over the login file.
	writeMuseAuth(t, auth, "k-file-1")
	cred, err := museAPICredential(config{MuseAPIKey: "k-env", MuseAuthPath: auth}, false)
	if err != nil || cred.Key != "k-env" || cred.Source != "env" {
		t.Fatalf("env: cred=%+v err=%v", cred, err)
	}
	// Login file, with the default base.
	cfg := config{MuseAuthPath: auth}
	cred, err = museAPICredential(cfg, false)
	if err != nil || cred.Key != "k-file-1" || cred.Source != "login" || cred.Base != museAPIDefaultBase {
		t.Fatalf("file: cred=%+v err=%v", cred, err)
	}
	// A rewritten file is picked up (mtime moved).
	writeMuseAuth(t, auth, "k-file-22")
	later := time.Now().Add(2 * time.Second)
	_ = os.Chtimes(auth, later, later)
	cred, _ = museAPICredential(cfg, false)
	if cred.Key != "k-file-22" {
		t.Fatalf("rewrite not picked up: %q", cred.Key)
	}
	// api_base_url in the file is honoured; PROXY_MUSE_API_BASE beats it.
	os.WriteFile(auth, []byte(`{"providers":{"meta":{"api_key":"k3","api_base_url":"https://example.test/v1/"}}}`), 0o600)
	_ = os.Chtimes(auth, later.Add(time.Second), later.Add(time.Second))
	if cred, _ = museAPICredential(cfg, false); cred.Base != "https://example.test/v1" {
		t.Fatalf("file base = %q", cred.Base)
	}
	if cred, _ = museAPICredential(config{MuseAuthPath: auth, MuseAPIBase: "http://override/v1"}, true); cred.Base != "http://override/v1" {
		t.Fatalf("override base = %q", cred.Base)
	}
	// A deleted file keeps serving the last good key.
	os.Remove(auth)
	if cred, err = museAPICredential(cfg, true); err != nil || cred.Key != "k3" {
		t.Fatalf("deleted file: cred=%+v err=%v", cred, err)
	}
	// No file and nothing cached → a clear error.
	resetMuseKeyCache(t)
	if _, err := museAPICredential(config{MuseAuthPath: filepath.Join(dir, "missing.json")}, false); err == nil || !strings.Contains(err.Error(), "muse login") {
		t.Fatalf("missing: err=%v", err)
	}
}

func TestMuseAPIAnthropicBody(t *testing.T) {
	cfg := museAPITestConfig("")
	raw := `{"model":"museapitest","max_tokens":4096,"stream":false,"speed":"fast","stop_sequences":["x"],
		"connect_tools":[{"name":"c1"}],"output_config":{"effort":"xhigh","reasoning_summary":"none"},
		"thinking":{"type":"adaptive","display":"summarized"},
		"system":[{"type":"text","text":"sys","cache_control":{"type":"ephemeral"}}],
		"messages":[{"role":"user","content":"hi"}],
		"tools":[{"type":"web_search_20260222","name":"web_search","max_uses":5},{"name":"get_x","input_schema":{"type":"object"}}]}`
	extra := []connectToolDef{
		{Name: "get_bookings", Description: "List bookings.", InputSchema: json.RawMessage(`{"type":"object","properties":{"date":{"type":"string"}}}`)},
		{Name: "get_x"}, // already defined by the caller → skipped
		{Name: "no_schema"},
		{Name: " "},
	}
	m, dropped, err := museAPIAnthropicBody(cfg, []byte(raw), "museapitest", extra)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(m)
	var got map[string]any
	json.Unmarshal(b, &got)

	if got["model"] != "muse-spark-1.3" {
		t.Errorf("model=%v", got["model"])
	}
	for _, k := range []string{"speed", "stop_sequences", "connect_tools"} {
		if _, ok := got[k]; ok {
			t.Errorf("%s not dropped", k)
		}
	}
	wantDropped := []string{"connect_tools", "output_config.reasoning_summary", "speed", "stop_sequences", "thinking.display"}
	if strings.Join(dropped, ",") != strings.Join(wantDropped, ",") {
		t.Errorf("dropped=%v want %v", dropped, wantDropped)
	}
	if oc := got["output_config"].(map[string]any); len(oc) != 1 || oc["effort"] != "xhigh" {
		t.Errorf("output_config=%v", oc)
	}
	if th := got["thinking"].(map[string]any); th["display"] != nil || th["type"] != "adaptive" {
		t.Errorf("thinking=%v", th)
	}
	if got["max_tokens"].(float64) != 16384 {
		t.Errorf("max_tokens=%v (floor)", got["max_tokens"])
	}
	if !strings.Contains(string(m["system"]), `"cache_control"`) {
		t.Errorf("system block not kept verbatim: %s", m["system"])
	}
	tools := got["tools"].([]any)
	if len(tools) != 4 {
		t.Fatalf("tools=%v", tools)
	}
	ws := tools[0].(map[string]any)
	if ws["type"] != "web_search_20250305" || ws["max_uses"] != nil {
		t.Errorf("web_search not normalized: %v", ws)
	}
	if tools[2].(map[string]any)["name"] != "get_bookings" || tools[3].(map[string]any)["input_schema"] == nil {
		t.Errorf("connect tools not merged: %v", tools)
	}

	// A large max_tokens is left alone; the floor can be disabled.
	m, _, _ = museAPIAnthropicBody(cfg, []byte(`{"model":"museapitest","max_tokens":64000,"messages":[]}`), "museapitest", nil)
	if string(m["max_tokens"]) != "64000" {
		t.Errorf("max_tokens=%s", m["max_tokens"])
	}
	cfg.MuseMinMaxTokens = 0
	m, _, _ = museAPIAnthropicBody(cfg, []byte(`{"model":"museapitest","max_tokens":5,"messages":[]}`), "museapitest", nil)
	if string(m["max_tokens"]) != "5" {
		t.Errorf("floor off: max_tokens=%s", m["max_tokens"])
	}
}

func TestMuseRepairToolPairs(t *testing.T) {
	type msg = map[string]any
	decode := func(t *testing.T, raw json.RawMessage) []msg {
		t.Helper()
		var out []msg
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatal(err)
		}
		return out
	}

	// Well formed: kept byte for byte, nothing reported.
	good := `[{"role":"user","content":"hi"},` +
		`{"role":"assistant","content":[{"type":"text","text":"checking"},{"type":"tool_use","id":"t1","name":"get_slots","input":{}}]},` +
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"ok","cache_control":{"type":"ephemeral"}}]},` +
		`{"role":"assistant","content":"done"}]`
	out, o, u, changed := museRepairToolPairs(json.RawMessage(good))
	if changed || o != 0 || u != 0 || string(out) != good {
		t.Fatalf("well-formed history changed: o=%d u=%d out=%s", o, u, out)
	}

	// Connect's proxy-managed history: the assistant tool_use was dropped but
	// the tool row with its id is still sent — Meta 400s on it.
	orphan := `[{"role":"assistant","content":"Let me check the slots."},` +
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1789906577530881884_1","content":"{\"slots\":[\"10:00\"]}"}]},` +
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_x","content":[{"type":"text","text":"booked"},{"type":"image","source":{}}]},{"type":"tool_result","tool_use_id":"call_y","content":""}]},` +
		`{"role":"user","content":"Any slots tomorrow?"}]`
	out, o, u, changed = museRepairToolPairs(json.RawMessage(orphan))
	if !changed || o != 3 || u != 0 {
		t.Fatalf("orphans: changed=%v o=%d u=%d", changed, o, u)
	}
	got := decode(t, out)
	if len(got) != 4 {
		t.Fatalf("messages=%d: %s", len(got), out)
	}
	if strings.Contains(string(out), `"tool_result"`) {
		t.Errorf("orphan tool_result left in: %s", out)
	}
	b1 := got[1]["content"].([]any)
	if len(b1) != 1 || b1[0].(map[string]any)["text"] != "[result of an earlier tool call]\n{\"slots\":[\"10:00\"]}" {
		t.Errorf("orphan not kept as text: %v", b1)
	}
	b2 := got[2]["content"].([]any)
	if len(b2) != 1 || b2[0].(map[string]any)["text"] != "[result of an earlier tool call]\nbooked" {
		t.Errorf("block-list orphan / empty orphan: %v", b2)
	}
	if got[3]["content"] != "Any slots tomorrow?" {
		t.Errorf("untouched message changed: %v", got[3])
	}

	// A window that starts between a call and its result, a tool_use left
	// unanswered, a result for the wrong call and a duplicate result.
	mixed := `[{"role":"user","content":[{"type":"tool_result","tool_use_id":"gone","content":""}]},` +
		`{"role":"assistant","content":[{"type":"tool_use","id":"a","name":"x","input":{}},{"type":"tool_use","id":"b","name":"y","input":{}}]},` +
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":"a","content":"A"},{"type":"tool_result","tool_use_id":"a","content":"A again"},{"type":"tool_result","tool_use_id":"z","content":"Z"}]},` +
		`{"role":"assistant","content":[{"type":"tool_use","id":"c","name":"x","input":{}}]},` +
		`{"role":"user","content":"next question"}]`
	out, o, u, changed = museRepairToolPairs(json.RawMessage(mixed))
	if !changed || o != 3 || u != 2 {
		t.Fatalf("mixed: changed=%v o=%d u=%d out=%s", changed, o, u, out)
	}
	got = decode(t, out)
	// msg0 (empty orphan only) and msg3 (only an unanswered tool_use) are gone.
	if len(got) != 3 {
		t.Fatalf("messages=%d: %s", len(got), out)
	}
	asst := got[0]["content"].([]any)
	if got[0]["role"] != "assistant" || len(asst) != 1 || asst[0].(map[string]any)["id"] != "a" {
		t.Errorf("assistant should keep only the answered tool_use a: %v", got[0])
	}
	res := got[1]["content"].([]any)
	if len(res) != 3 || res[0].(map[string]any)["tool_use_id"] != "a" ||
		res[1].(map[string]any)["text"] != "[result of an earlier tool call]\nA again" ||
		res[2].(map[string]any)["text"] != "[result of an earlier tool call]\nZ" {
		t.Errorf("results: %v", res)
	}
	if got[2]["content"] != "next question" {
		t.Errorf("last message: %v", got[2])
	}

	// Not a list / empty: left alone.
	for _, in := range []string{``, `null`, `"x"`, `{}`} {
		if _, _, _, changed := museRepairToolPairs(json.RawMessage(in)); changed {
			t.Errorf("%q reported changed", in)
		}
	}
}

func TestMuseWithCacheBreakpoints(t *testing.T) {
	body := func(raw string) map[string]json.RawMessage {
		t.Helper()
		var m map[string]json.RawMessage
		if err := json.Unmarshal([]byte(raw), &m); err != nil {
			t.Fatal(err)
		}
		return m
	}

	// String system + last user message as a string: both become one text
	// block carrying the breakpoint; the last tool is marked too.
	m := body(`{"system":"You are Zeina.","tools":[{"name":"a"},{"name":"b"}],
		"messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"hello"},{"role":"user","content":"slots?"}]}`)
	out, n := museWithCacheBreakpoints(m)
	if n != 3 {
		t.Fatalf("added=%d", n)
	}
	if string(out["system"]) != `[{"cache_control":{"type":"ephemeral"},"text":"You are Zeina.","type":"text"}]` {
		t.Errorf("system=%s", out["system"])
	}
	if !strings.Contains(string(out["tools"]), `{"cache_control":{"type":"ephemeral"},"name":"b"}`) || strings.Count(string(out["tools"]), "cache_control") != 1 {
		t.Errorf("tools=%s", out["tools"])
	}
	if !strings.HasSuffix(string(out["messages"]), `{"content":[{"cache_control":{"type":"ephemeral"},"text":"slots?","type":"text"}],"role":"user"}]`) || strings.Count(string(out["messages"]), "cache_control") != 1 {
		t.Errorf("messages=%s", out["messages"])
	}
	if strings.Contains(string(m["system"])+string(m["tools"])+string(m["messages"]), "cache_control") {
		t.Error("the caller's body must not be modified")
	}

	// A tool_result is the newest block: it carries the breakpoint.
	m = body(`{"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"x","input":{}}]},
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"ok"}]}]}`)
	out, n = museWithCacheBreakpoints(m)
	if n != 1 || !strings.Contains(string(out["messages"]), `"cache_control":{"type":"ephemeral"},"content":"ok","tool_use_id":"t1"`) {
		t.Errorf("tool_result: added=%d messages=%s", n, out["messages"])
	}

	// Not cacheable: a last assistant message, a thinking block, empty text.
	for _, raw := range []string{
		`{"messages":[{"role":"assistant","content":"x"}]}`,
		`{"messages":[{"role":"user","content":[{"type":"thinking","thinking":"x"}]}]}`,
		`{"messages":[{"role":"user","content":"  "}],"system":""}`,
	} {
		if _, n := museWithCacheBreakpoints(body(raw)); n != 0 {
			t.Errorf("%s: added %d", raw, n)
		}
	}

	// The caller's own breakpoints count against the limit of 4.
	m = body(`{"system":[{"type":"text","text":"a","cache_control":{"type":"ephemeral"}},{"type":"text","text":"b","cache_control":{"type":"ephemeral"}}],
		"tools":[{"name":"a","cache_control":{"type":"ephemeral"}},{"name":"b"}],"messages":[{"role":"user","content":"hi"}]}`)
	out, n = museWithCacheBreakpoints(m)
	if n != 1 || strings.Count(string(out["tools"])+string(out["system"])+string(out["messages"]), "cache_control") != 4 {
		t.Errorf("budget: added=%d", n)
	}
}

func TestMuseAPIToolLoopCacheBreakpoints(t *testing.T) {
	resetMuseKeyCache(t)
	fc := newFakeConnect(t)
	fm := newFakeMeta(t, func(n int, req fakeMetaReq, w http.ResponseWriter) {
		if n == 1 {
			metaJSON(w, 200, toolUseMessage(`{"type":"tool_use","id":"tu_1","name":"get_slots","input":{"date":"2026-10-16"}}`))
			return
		}
		metaJSON(w, 200, textMessage("Tomorrow at 10:00 is free."))
	})
	cfg := museAPITestConfig(fm.srv.URL)
	cfg.MusePromptCache = true
	body, hdr := toolRequest(fc, false)
	r := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", strings.NewReader(body))
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	handleMessages(cfg)(w, r)
	if w.Code != 200 {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	reqs := fm.requests()
	if len(reqs) != 2 {
		t.Fatalf("meta requests=%d", len(reqs))
	}
	for i, req := range reqs {
		// tools + system + newest message — the previous call's mark is gone.
		if c := strings.Count(req.Raw, `"cache_control"`); c != 3 {
			t.Errorf("request %d: %d breakpoints, want 3: %s", i+1, c, req.Raw)
		}
	}
	msgs := reqs[1].Body["messages"].([]any)
	last := msgs[len(msgs)-1].(map[string]any)["content"].([]any)[0].(map[string]any)
	if last["type"] != "tool_result" || last["cache_control"] == nil {
		t.Errorf("newest message not marked: %v", last)
	}
	if first := msgs[0].(map[string]any)["content"]; first != "Any slots tomorrow?" {
		t.Errorf("an older message kept a breakpoint: %v", first)
	}
}

func TestMuseAPICacheBreakpointsRejectedRetriesWithout(t *testing.T) {
	resetMuseKeyCache(t)
	fm := newFakeMeta(t, func(n int, req fakeMetaReq, w http.ResponseWriter) {
		if strings.Contains(req.Raw, "cache_control") {
			metaJSON(w, 400, `{"type":"error","error":{"type":"invalid_request_error","message":"cache_control is not supported here"}}`)
			return
		}
		metaJSON(w, 200, okMessage)
	})
	cfg := museAPITestConfig(fm.srv.URL)
	cfg.MusePromptCache = true
	w := postMessages(t, cfg, `{"model":"museapitest","max_tokens":64,"system":"sys","messages":[{"role":"user","content":"hi"}]}`, nil)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "hello") {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	reqs := fm.requests()
	if len(reqs) != 2 || !strings.Contains(reqs[0].Raw, "cache_control") || strings.Contains(reqs[1].Raw, "cache_control") {
		t.Fatalf("want one marked attempt then one clean retry, got %d requests", len(reqs))
	}

	// Switched off: no breakpoints at all.
	fm2 := newFakeMeta(t, func(n int, req fakeMetaReq, w http.ResponseWriter) { metaJSON(w, 200, okMessage) })
	cfg = museAPITestConfig(fm2.srv.URL)
	cfg.MusePromptCache = false
	postMessages(t, cfg, `{"model":"museapitest","max_tokens":64,"system":"sys","messages":[{"role":"user","content":"hi"}]}`, nil)
	if strings.Contains(fm2.requests()[0].Raw, "cache_control") {
		t.Error("PROXY_MUSE_PROMPT_CACHE=0 must send no breakpoints")
	}
}

// captureStdout returns what f prints (the [muse-*] lines go to stdout).
func captureStdout(t *testing.T, f func()) string {
	t.Helper()
	old := os.Stdout
	rd, wr, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = wr
	done := make(chan string)
	go func() {
		b, _ := io.ReadAll(rd)
		done <- string(b)
	}()
	defer func() { os.Stdout = old }()
	f()
	wr.Close()
	os.Stdout = old
	return <-done
}

func TestMuseAPIToolLoopLogs(t *testing.T) {
	resetMuseKeyCache(t)
	fc := newFakeConnect(t)
	fm := newFakeMeta(t, func(n int, req fakeMetaReq, w http.ResponseWriter) {
		if n == 1 {
			metaJSON(w, 200, `{"type":"message","role":"assistant","content":[{"type":"tool_use","id":"tu_1","name":"get_slots","input":{"date":"d1"}}],"stop_reason":"tool_use","usage":{"input_tokens":100,"cache_read_input_tokens":900,"output_tokens":20}}`)
			return
		}
		metaJSON(w, 200, textMessage("Tomorrow at 10:00 is free."))
	})
	body, hdr := toolRequest(fc, false)
	r := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", strings.NewReader(body))
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	logs := captureStdout(t, func() { handleMessages(museAPITestConfig(fm.srv.URL))(httptest.NewRecorder(), r) })

	for _, want := range []*regexp.Regexp{
		regexp.MustCompile(`\[muse-call\] conv=conv1 turn=1 ms=\d+ in=1000 cache_read=900 cache_write=0 out=20 stop=tool_use tool_uses=1 text_chars=0 final_forced=false`),
		regexp.MustCompile(`\[muse-tool\] conv=conv1 turn=1 name=get_slots ms=\d+ ok=true bytes=\d+`),
		regexp.MustCompile(`\[muse-call\] conv=conv1 turn=2 ms=\d+ in=150 cache_read=0 cache_write=0 out=30 stop=end_turn tool_uses=0 text_chars=26`),
		regexp.MustCompile(`\[muse-api\] model=museapitest tools=2 turns=2 tool_calls=1 repeats_skipped=0 ok=true ms=\d+ meta_ms=\d+ tool_ms=\d+ in=1150 cache_read=900 cache_write=0 out=50 effort=xhigh msgs=1 conv=conv1 seq=get_slots\n`),
	} {
		if !want.MatchString(logs) {
			t.Errorf("missing log line %s in:\n%s", want, logs)
		}
	}
}

// The request Connect really sends (orphan tool rows, connect_tools) reaches
// Meta without any tool_result that lacks its tool_use.
func TestMuseAPIToolLoopRepairsOrphanHistory(t *testing.T) {
	resetMuseKeyCache(t)
	fc := newFakeConnect(t)
	fm := newFakeMeta(t, func(n int, req fakeMetaReq, w http.ResponseWriter) {
		if strings.Contains(req.Raw, "toolu_old_1") {
			metaJSON(w, 400, `{"error":{"message":"No function call found for function call output with call_id 'toolu_old_1'","type":"invalid_request_error"},"type":"error"}`)
			return
		}
		metaJSON(w, 200, textMessage("Tomorrow at 10:00 is free."))
	})
	body, hdr := toolRequest(fc, false)
	body = strings.Replace(body, `"messages":[{"role":"user","content":"Any slots tomorrow?"}]`,
		`"messages":[{"role":"assistant","content":"Let me check."},{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_old_1","content":"{\"slots\":[\"09:00\"]}"}]},{"role":"user","content":"Any slots tomorrow?"}]`, 1)
	r := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", strings.NewReader(body))
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	handleMessages(museAPITestConfig(fm.srv.URL))(w, r)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "Tomorrow at 10:00 is free.") {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	reqs := fm.requests()
	if len(reqs) != 1 {
		t.Fatalf("meta requests=%d", len(reqs))
	}
	if !strings.Contains(reqs[0].Raw, `[result of an earlier tool call]\n{\"slots\":[\"09:00\"]}`) {
		t.Errorf("orphan result not carried as text: %s", reqs[0].Raw)
	}
}

// ---------------------------------------------------------------------------
// Passthrough
// ---------------------------------------------------------------------------

func TestMuseAPIPassthroughNonStream(t *testing.T) {
	resetMuseKeyCache(t)
	fm := newFakeMeta(t, func(n int, req fakeMetaReq, w http.ResponseWriter) { metaJSON(w, 200, okMessage) })
	cfg := museAPITestConfig(fm.srv.URL)
	w := postMessages(t, cfg, `{"model":"museapitest","max_tokens":5,"speed":"fast","messages":[{"role":"user","content":"hi"}]}`,
		map[string]string{"anthropic-beta": "context-1m-2025-08-07", "authorization": "Bearer caller"})
	if w.Code != 200 || w.Body.String() != okMessage {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	reqs := fm.requests()
	if len(reqs) != 1 {
		t.Fatalf("requests=%d", len(reqs))
	}
	q := reqs[0]
	if q.Path != "/messages" || q.Auth != "Bearer k-env" {
		t.Errorf("path=%q auth=%q (the caller's credentials must never be forwarded)", q.Path, q.Auth)
	}
	if q.Header.Get("x-api-key") != "" || q.Header.Get("anthropic-beta") != "context-1m-2025-08-07" || q.Header.Get("anthropic-version") != "2023-06-01" {
		t.Errorf("headers=%v", q.Header)
	}
	if q.Body["model"] != "muse-spark-1.3" || q.Body["speed"] != nil || q.Body["max_tokens"].(float64) != 16384 {
		t.Errorf("body=%v", q.Body)
	}
}

func TestMuseAPIPassthroughStream(t *testing.T) {
	resetMuseKeyCache(t)
	events := []string{
		`event: message_start` + "\n" + `data: {"type":"message_start","message":{"usage":{"input_tokens":90,"output_tokens":0}}}`,
		`event: content_block_delta` + "\n" + `data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"1 2"}}`,
		`event: content_block_delta` + "\n" + `data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":" 3"}}`,
		`event: message_delta` + "\n" + `data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":42}}`,
		`event: message_stop` + "\n" + `data: {"type":"message_stop"}`,
	}
	fm := newFakeMeta(t, func(n int, req fakeMetaReq, w http.ResponseWriter) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		for _, e := range events {
			io.WriteString(w, e+"\n\n")
			w.(http.Flusher).Flush()
		}
	})
	cfg := museAPITestConfig(fm.srv.URL)
	body := `{"model":"museapitest","max_tokens":100,"stream":true,"messages":[{"role":"user","content":"count"}]}`
	r := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", strings.NewReader(body))
	w := httptest.NewRecorder()
	handleMessages(cfg)(w, r)
	if w.Code != 200 || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/event-stream") {
		t.Fatalf("status=%d ct=%q", w.Code, w.Header().Get("Content-Type"))
	}
	out := w.Body.String()
	last := -1
	for _, e := range events {
		i := strings.Index(out, e)
		if i < 0 || i < last {
			t.Fatalf("event missing or out of order: %q\n%s", e, out)
		}
		last = i
	}
	if fm.requests()[0].Body["stream"] != true {
		t.Error("stream flag not forwarded")
	}
	stat := takeRequestStat(r)
	if stat.Upstream != "muse" || stat.InputTokens != 90 || stat.OutputTokens != 42 || stat.StopReason != "end_turn" {
		t.Errorf("stat=%+v", stat)
	}
}

func TestMuseAPIRetries(t *testing.T) {
	resetMuseKeyCache(t)
	t.Run("503 then 200", func(t *testing.T) {
		fm := newFakeMeta(t, func(n int, req fakeMetaReq, w http.ResponseWriter) {
			if n == 1 {
				metaJSON(w, 503, `{"type":"error","error":{"type":"api_error","message":"busy"}}`)
				return
			}
			metaJSON(w, 200, okMessage)
		})
		w := postMessages(t, museAPITestConfig(fm.srv.URL), `{"model":"museapitest","messages":[{"role":"user","content":"hi"}]}`, nil)
		if w.Code != 200 || len(fm.requests()) != 2 {
			t.Fatalf("status=%d requests=%d", w.Code, len(fm.requests()))
		}
	})
	t.Run("400 not retried, passed through", func(t *testing.T) {
		errBody := `{"error":{"message":"unknown parameter","type":"invalid_request_error"},"type":"error"}`
		fm := newFakeMeta(t, func(n int, req fakeMetaReq, w http.ResponseWriter) { metaJSON(w, 400, errBody) })
		w := postMessages(t, museAPITestConfig(fm.srv.URL), `{"model":"museapitest","messages":[]}`, nil)
		if w.Code != 400 || w.Body.String() != errBody || len(fm.requests()) != 1 {
			t.Fatalf("status=%d body=%s requests=%d", w.Code, w.Body.String(), len(fm.requests()))
		}
	})
	t.Run("dropped connection then 200", func(t *testing.T) {
		fm := newFakeMeta(t, func(n int, req fakeMetaReq, w http.ResponseWriter) {
			if n == 1 {
				conn, _, _ := w.(http.Hijacker).Hijack()
				conn.Close()
				return
			}
			metaJSON(w, 200, okMessage)
		})
		w := postMessages(t, museAPITestConfig(fm.srv.URL), `{"model":"museapitest","messages":[{"role":"user","content":"hi"}]}`, nil)
		if w.Code != 200 || len(fm.requests()) != 2 {
			t.Fatalf("status=%d requests=%d", w.Code, len(fm.requests()))
		}
	})
	t.Run("persistent 503 passed through after retries", func(t *testing.T) {
		fm := newFakeMeta(t, func(n int, req fakeMetaReq, w http.ResponseWriter) { metaJSON(w, 503, `{"error":"busy"}`) })
		w := postMessages(t, museAPITestConfig(fm.srv.URL), `{"model":"museapitest","messages":[]}`, nil)
		if w.Code != 503 || len(fm.requests()) != 3 {
			t.Fatalf("status=%d requests=%d (want 1 + 2 retries)", w.Code, len(fm.requests()))
		}
	})
}

func TestMuseAPIOpenAIPassthrough(t *testing.T) {
	resetMuseKeyCache(t)
	fm := newFakeMeta(t, func(n int, req fakeMetaReq, w http.ResponseWriter) {
		metaJSON(w, 200, `{"id":"c1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2}}`)
	})
	cfg := museAPITestConfig(fm.srv.URL)

	r := httptest.NewRequest(http.MethodPost, "/openai/v1/chat/completions", strings.NewReader(`{"model":"museapitest","max_tokens":10,"connect_tools":[],"messages":[{"role":"user","content":"hi"}]}`))
	w := httptest.NewRecorder()
	handleChatCompletions(cfg)(w, r)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"content":"ok"`) {
		t.Fatalf("chat status=%d body=%s", w.Code, w.Body.String())
	}
	q := fm.requests()[0]
	if q.Path != "/chat/completions" || q.Body["model"] != "muse-spark-1.3" || q.Body["connect_tools"] != nil || q.Body["max_tokens"].(float64) != 16384 {
		t.Errorf("chat upstream request=%+v", q)
	}
	if stat := takeRequestStat(r); stat.InputTokens != 3 || stat.OutputTokens != 2 || stat.StopReason != "stop" {
		t.Errorf("chat stat=%+v", stat)
	}

	r = httptest.NewRequest(http.MethodPost, "/openai/v1/responses", strings.NewReader(`{"model":"museapitest","input":[{"role":"user","content":"hi"}],"max_output_tokens":50}`))
	w = httptest.NewRecorder()
	handleResponses(cfg)(w, r)
	if w.Code != 200 {
		t.Fatalf("responses status=%d", w.Code)
	}
	q = fm.requests()[1]
	if q.Path != "/responses" || q.Body["model"] != "muse-spark-1.3" || q.Body["max_output_tokens"].(float64) != 16384 {
		t.Errorf("responses upstream request=%+v", q)
	}
}

func TestMuseAPITransportCLIStillRoutesToCLI(t *testing.T) {
	stub := buildMuseStub(t)
	museStubEnv(t, "ok")
	cfg := museStubConfig(t, stub)
	cfg.MuseTransport = "cli"
	cfg.Models = map[string]string{"museapitest": "muse-spark-1.3"}
	cfg.ModelForward = map[string]string{"museapitest": "muse"}
	w := postMessages(t, cfg, `{"model":"museapitest","max_tokens":5,"messages":[{"role":"user","content":"via cli"}]}`, nil)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "stub saw:") {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestMuseAPIUndownloadableAttachmentFallsBack(t *testing.T) {
	resetMuseKeyCache(t)
	mediaErr := `{"error":{"message":"failed to download media at input[0].content[0]: origin returned HTTP 400","type":"invalid_request_error"},"type":"error"}`
	fm := newFakeMeta(t, func(n int, req fakeMetaReq, w http.ResponseWriter) {
		if strings.Contains(req.Raw, `"type":"url"`) {
			metaJSON(w, 400, mediaErr)
			return
		}
		metaJSON(w, 200, okMessage)
	})
	body := `{"model":"museapitest","messages":[{"role":"user","content":[
		{"type":"image","source":{"type":"url","url":"https://media.example/expired.jpg"}},
		{"type":"image","source":{"type":"base64","media_type":"image/png","data":"iVBORw0KGgo="}},
		{"type":"text","text":"what is this?"}]}]}`
	w := postMessages(t, museAPITestConfig(fm.srv.URL), body, nil)
	if w.Code != 200 || w.Body.String() != okMessage {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	reqs := fm.requests()
	if len(reqs) != 2 {
		t.Fatalf("requests=%d, want the original + one retry", len(reqs))
	}
	retry := reqs[1].Raw
	if strings.Contains(retry, "expired.jpg") || !strings.Contains(retry, "could not be loaded") || !strings.Contains(retry, `"type":"base64"`) {
		t.Errorf("retry must drop only the URL attachment: %s", retry)
	}

	// A 400 that is not about media is passed through untouched, no retry.
	other := `{"error":{"message":"unknown parameter","type":"invalid_request_error"},"type":"error"}`
	fm2 := newFakeMeta(t, func(n int, req fakeMetaReq, w http.ResponseWriter) { metaJSON(w, 400, other) })
	w = postMessages(t, museAPITestConfig(fm2.srv.URL), body, nil)
	if w.Code != 400 || w.Body.String() != other || len(fm2.requests()) != 1 {
		t.Fatalf("status=%d body=%s requests=%d", w.Code, w.Body.String(), len(fm2.requests()))
	}
}

// ---------------------------------------------------------------------------
// Connect tool loop
// ---------------------------------------------------------------------------

type fakeConnect struct {
	mu    sync.Mutex
	calls []map[string]any
	srv   *httptest.Server
}

func newFakeConnect(t *testing.T) *fakeConnect {
	t.Helper()
	fc := &fakeConnect{}
	fc.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer cb-token" {
			w.WriteHeader(401)
			return
		}
		var p map[string]any
		json.NewDecoder(r.Body).Decode(&p)
		fc.mu.Lock()
		fc.calls = append(fc.calls, p)
		fc.mu.Unlock()
		metaJSON(w, 200, `{"success":true,"data":{"tool":"`+p["tool_name"].(string)+`","slots":["10:00"]}}`)
	}))
	t.Cleanup(fc.srv.Close)
	return fc
}

func (fc *fakeConnect) count() int {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	return len(fc.calls)
}

const toolCatalog = `[{"name":"get_slots","description":"Free slots for a date.","input_schema":{"type":"object","properties":{"date":{"type":"string"}},"required":["date"]}},{"name":"create_booking","description":"Book.","input_schema":{"type":"object","properties":{"slot":{"type":"string"}}}}]`

func toolRequest(fc *fakeConnect, stream bool) (string, map[string]string) {
	s := "false"
	if stream {
		s = "true"
	}
	body := `{"model":"museapitest","max_tokens":4096,"stream":` + s + `,"output_config":{"effort":"xhigh","reasoning_summary":"none"},"system":"You are Zeina.","messages":[{"role":"user","content":"Any slots tomorrow?"}],"connect_tools":` + toolCatalog + `}`
	return body, map[string]string{
		"X-Connect-Callback-Url":   fc.srv.URL + "/api/agent-tools/conv1/execute",
		"X-Connect-Callback-Token": "cb-token",
		"X-Connect-Conversation":   "conv1",
		"X-Connect-Agent":          "agent1",
	}
}

func toolUseMessage(blocks ...string) string {
	return `{"type":"message","role":"assistant","content":[{"type":"redacted_thinking","data":"r1"},` + strings.Join(blocks, ",") + `],"stop_reason":"tool_use","usage":{"input_tokens":100,"output_tokens":20}}`
}

func textMessage(text string) string {
	return `{"type":"message","role":"assistant","content":[{"type":"text","text":` + quoteJSON(text) + `}],"stop_reason":"end_turn","usage":{"input_tokens":150,"output_tokens":30}}`
}

func TestMuseAPIToolLoop(t *testing.T) {
	resetMuseKeyCache(t)
	fc := newFakeConnect(t)
	fm := newFakeMeta(t, func(n int, req fakeMetaReq, w http.ResponseWriter) {
		switch n {
		case 1:
			metaJSON(w, 200, toolUseMessage(`{"type":"text","text":"Let me check."}`, `{"type":"tool_use","id":"tu_1","name":"get_slots","input":{"date":"2026-10-14"}}`))
		default:
			metaJSON(w, 200, textMessage("Tomorrow at 10:00 is free."))
		}
	})
	body, hdr := toolRequest(fc, false)
	r := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", strings.NewReader(body))
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	handleMessages(museAPITestConfig(fm.srv.URL))(w, r)
	if w.Code != 200 {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "Tomorrow at 10:00 is free.") || strings.Contains(w.Body.String(), "Let me check") {
		t.Errorf("final answer must be only the last turn's text: %s", w.Body.String())
	}
	if fc.count() != 1 || fc.calls[0]["tool_name"] != "get_slots" || fc.calls[0]["arguments"].(map[string]any)["date"] != "2026-10-14" {
		t.Fatalf("callback calls=%v", fc.calls)
	}
	reqs := fm.requests()
	if len(reqs) != 2 {
		t.Fatalf("meta requests=%d", len(reqs))
	}
	first := reqs[0].Body
	if first["stream"] != nil || first["connect_tools"] != nil || len(first["tools"].([]any)) != 2 {
		t.Errorf("first request: stream=%v connect_tools=%v tools=%v", first["stream"], first["connect_tools"], first["tools"])
	}
	if strings.Contains(reqs[0].Raw, "connect_execute_tool") {
		t.Error("API tool loop must use native tools, not the MCP umbrella prompt")
	}
	msgs := reqs[1].Body["messages"].([]any)
	if len(msgs) != 3 {
		t.Fatalf("second request messages=%d", len(msgs))
	}
	asst := msgs[1].(map[string]any)
	if asst["role"] != "assistant" || !strings.Contains(reqs[1].Raw, `"redacted_thinking"`) {
		t.Errorf("assistant turn (with redacted thinking) not echoed back: %v", asst)
	}
	res := msgs[2].(map[string]any)["content"].([]any)[0].(map[string]any)
	if res["type"] != "tool_result" || res["tool_use_id"] != "tu_1" || !strings.Contains(res["content"].(string), `"slots":["10:00"]`) {
		t.Errorf("tool_result=%v", res)
	}
	stat := takeRequestStat(r)
	if stat.InputTokens != 250 || stat.OutputTokens != 50 {
		t.Errorf("stat=%+v (usage must be summed across calls)", stat)
	}
}

func TestMuseAPIToolLoopTwoToolsInOrderAndDisallowed(t *testing.T) {
	resetMuseKeyCache(t)
	fc := newFakeConnect(t)
	fm := newFakeMeta(t, func(n int, req fakeMetaReq, w http.ResponseWriter) {
		if n == 1 {
			metaJSON(w, 200, toolUseMessage(
				`{"type":"tool_use","id":"a","name":"get_slots","input":{"date":"d1"}}`,
				`{"type":"tool_use","id":"b","name":"drop_database","input":{}}`,
				`{"type":"tool_use","id":"c","name":"create_booking","input":{"slot":"10:00"}}`))
			return
		}
		metaJSON(w, 200, textMessage("Booked."))
	})
	body, hdr := toolRequest(fc, false)
	w := postMessages(t, museAPITestConfig(fm.srv.URL), body, hdr)
	if w.Code != 200 {
		t.Fatalf("status=%d", w.Code)
	}
	if fc.count() != 2 || fc.calls[0]["tool_name"] != "get_slots" || fc.calls[1]["tool_name"] != "create_booking" {
		t.Fatalf("calls=%v (in order; the disallowed tool must never reach Connect)", fc.calls)
	}
	results := fm.requests()[1].Body["messages"].([]any)[2].(map[string]any)["content"].([]any)
	if len(results) != 3 || results[1].(map[string]any)["tool_use_id"] != "b" || !strings.Contains(results[1].(map[string]any)["content"].(string), "not available") {
		t.Errorf("tool results=%v", results)
	}
}

func TestMuseAPIToolLoopRetryNeverRepeatsATool(t *testing.T) {
	resetMuseKeyCache(t)
	fc := newFakeConnect(t)
	fm := newFakeMeta(t, func(n int, req fakeMetaReq, w http.ResponseWriter) {
		switch n {
		case 1:
			metaJSON(w, 200, toolUseMessage(`{"type":"tool_use","id":"a","name":"create_booking","input":{"slot":"10:00"}}`))
		case 2:
			metaJSON(w, 529, `{"type":"error","error":{"type":"overloaded_error","message":"overloaded"}}`)
		default:
			metaJSON(w, 200, textMessage("Done."))
		}
	})
	body, hdr := toolRequest(fc, false)
	w := postMessages(t, museAPITestConfig(fm.srv.URL), body, hdr)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "Done.") {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if fc.count() != 1 {
		t.Fatalf("create_booking ran %d times; a Meta retry must never re-execute a tool", fc.count())
	}
}

func TestMuseAPIToolLoopRepeatedSuccessfulCallIsNotRerun(t *testing.T) {
	resetMuseKeyCache(t)
	fc := newFakeConnect(t)
	summary := `{"type":"tool_use","id":"s%d","name":"create_booking","input":{"slot":"10:00"}}`
	fm := newFakeMeta(t, func(n int, req fakeMetaReq, w http.ResponseWriter) {
		if tc, ok := req.Body["tool_choice"].(map[string]any); ok && tc["type"] == "none" {
			metaJSON(w, 200, textMessage("All done."))
			return
		}
		// The model keeps re-submitting the identical call (the scheduled-task loop).
		metaJSON(w, 200, toolUseMessage(strings.Replace(summary, "%d", strconv.Itoa(n), 1)))
	})
	body, hdr := toolRequest(fc, false)
	w := postMessages(t, museAPITestConfig(fm.srv.URL), body, hdr)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "All done.") {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if fc.count() != 1 {
		t.Fatalf("create_booking ran %d times; an identical successful call must run once", fc.count())
	}
	reqs := fm.requests()
	if len(reqs) != 3 {
		t.Fatalf("meta requests=%d, want call → repeat (skipped) → forced text", len(reqs))
	}
	if !strings.Contains(reqs[2].Raw, "duplicate_call") {
		t.Error("the skipped repeat must be answered with the duplicate_call note")
	}

	// A call that FAILED may be retried with the same arguments.
	if museCallKey("a", json.RawMessage(`{"x":1,"y":2}`)) != museCallKey("a", json.RawMessage(`{"y":2,"x":1}`)) {
		t.Error("argument order must not matter")
	}
	if museToolSucceeded(`{"success":false,"error":"x"}`) || !museToolSucceeded(`{"success":true}`) || !museToolSucceeded(`not json`) {
		t.Error("museToolSucceeded")
	}
}

func TestMuseAPIToolLoopFailedCallMayBeRetried(t *testing.T) {
	resetMuseKeyCache(t)
	var hits atomic.Int32
	cb := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) == 1 {
			metaJSON(w, 200, `{"success":false,"error":"slot busy, try again"}`)
			return
		}
		metaJSON(w, 200, `{"success":true,"data":{"booked":true}}`)
	}))
	defer cb.Close()
	fm := newFakeMeta(t, func(n int, req fakeMetaReq, w http.ResponseWriter) {
		if n <= 2 {
			metaJSON(w, 200, toolUseMessage(`{"type":"tool_use","id":"b`+strconv.Itoa(n)+`","name":"create_booking","input":{"slot":"10:00"}}`))
			return
		}
		metaJSON(w, 200, textMessage("Booked."))
	})
	body, hdr := toolRequest(&fakeConnect{srv: cb}, false)
	w := postMessages(t, museAPITestConfig(fm.srv.URL), body, hdr)
	if w.Code != 200 || hits.Load() != 2 {
		t.Fatalf("status=%d callback hits=%d, want the failed call retried once", w.Code, hits.Load())
	}
}

func TestMuseAPIToolLoopTurnLimitForcesText(t *testing.T) {
	resetMuseKeyCache(t)
	fc := newFakeConnect(t)
	fm := newFakeMeta(t, func(n int, req fakeMetaReq, w http.ResponseWriter) {
		if tc, ok := req.Body["tool_choice"].(map[string]any); ok && tc["type"] == "none" {
			metaJSON(w, 200, textMessage("Here is what I found."))
			return
		}
		metaJSON(w, 200, toolUseMessage(`{"type":"tool_use","id":"t`+strings.Repeat("x", n)+`","name":"get_slots","input":{"date":"d`+strconv.Itoa(n)+`"}}`))
	})
	cfg := museAPITestConfig(fm.srv.URL)
	cfg.MuseToolMaxTurns = 3
	body, hdr := toolRequest(fc, false)
	w := postMessages(t, cfg, body, hdr)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "Here is what I found.") {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if len(fm.requests()) != 4 || fc.count() != 3 {
		t.Fatalf("meta requests=%d callbacks=%d (want 3 tool turns + 1 forced text turn)", len(fm.requests()), fc.count())
	}
}

func TestMuseAPIToolLoopEmptyTextReasked(t *testing.T) {
	resetMuseKeyCache(t)
	fc := newFakeConnect(t)
	fm := newFakeMeta(t, func(n int, req fakeMetaReq, w http.ResponseWriter) {
		if n == 1 {
			metaJSON(w, 200, `{"type":"message","content":[{"type":"redacted_thinking","data":"r"}],"stop_reason":"max_tokens","usage":{"input_tokens":1,"output_tokens":1}}`)
			return
		}
		metaJSON(w, 200, textMessage("Answer."))
	})
	body, hdr := toolRequest(fc, false)
	w := postMessages(t, museAPITestConfig(fm.srv.URL), body, hdr)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "Answer.") || len(fm.requests()) != 2 {
		t.Fatalf("status=%d body=%s requests=%d", w.Code, w.Body.String(), len(fm.requests()))
	}
}

func TestMuseAPIToolLoopStreamCaller(t *testing.T) {
	resetMuseKeyCache(t)
	fc := newFakeConnect(t)
	fm := newFakeMeta(t, func(n int, req fakeMetaReq, w http.ResponseWriter) { metaJSON(w, 200, textMessage("Hi there.")) })
	body, hdr := toolRequest(fc, true)
	w := postMessages(t, museAPITestConfig(fm.srv.URL), body, hdr)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "event: message_stop") || !strings.Contains(w.Body.String(), "Hi there.") {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if fm.requests()[0].Body["stream"] != nil {
		t.Error("the tool loop must call Meta non-streamed")
	}
}

func TestMuseAPIToolsDisabledIsPlainPassthrough(t *testing.T) {
	resetMuseKeyCache(t)
	fc := newFakeConnect(t)
	fm := newFakeMeta(t, func(n int, req fakeMetaReq, w http.ResponseWriter) { metaJSON(w, 200, okMessage) })
	cfg := museAPITestConfig(fm.srv.URL)
	cfg.MuseToolsEnabled = false
	body, hdr := toolRequest(fc, false)
	w := postMessages(t, cfg, body, hdr)
	if w.Code != 200 || w.Body.String() != okMessage {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if q := fm.requests()[0].Body; q["connect_tools"] != nil || q["tools"] != nil {
		t.Errorf("body=%v", q)
	}
}

// ---------------------------------------------------------------------------
// Key guard
// ---------------------------------------------------------------------------

// keyGuardSetup prepares a login file with oldKey, a stub muse in the given mode
// and a fake Meta that accepts only goodKey.
func keyGuardSetup(t *testing.T, mode, oldKey, newKey, goodKey string, sleepMS int) (config, *fakeMeta, string) {
	t.Helper()
	resetMuseKeyCache(t)
	stub := buildMuseStub(t)
	museStubEnv(t, mode)
	dir := t.TempDir()
	auth := filepath.Join(dir, "auth.json")
	writeMuseAuth(t, auth, oldKey)
	counter := filepath.Join(dir, "runs.txt")
	t.Setenv("MUSE_STUB_AUTH", auth)
	t.Setenv("MUSE_STUB_NEW_KEY", newKey)
	t.Setenv("MUSE_STUB_COUNT", counter)
	if sleepMS > 0 {
		t.Setenv("MUSE_STUB_SLEEP_MS", strconv.Itoa(sleepMS))
	}
	fm := newFakeMeta(t, func(n int, req fakeMetaReq, w http.ResponseWriter) {
		if req.Auth != "Bearer "+goodKey {
			metaJSON(w, 401, `{"type":"error","error":{"type":"authentication_error","message":"invalid key"}}`)
			return
		}
		if req.Path == "/models" {
			metaJSON(w, 200, `{"object":"list","data":[]}`)
			return
		}
		metaJSON(w, 200, okMessage)
	})
	cfg := museAPITestConfig(fm.srv.URL)
	cfg.MuseAPIKey = ""
	cfg.MuseAuthPath = auth
	cfg.MuseBin = stub
	cfg.MuseWorkDir = t.TempDir()
	cfg.MuseProvider = "echo"
	return cfg, fm, counter
}

func stubRuns(t *testing.T, counter string) int {
	t.Helper()
	f, err := os.Open(counter)
	if err != nil {
		return 0
	}
	defer f.Close()
	n := 0
	s := bufio.NewScanner(f)
	for s.Scan() {
		n++
	}
	return n
}

func TestMuseKeyGuardAdoptsRotatedKeyAndRetries(t *testing.T) {
	cfg, fm, counter := keyGuardSetup(t, "rotate", "k-old", "k-new", "k-new", 0)
	w := postMessages(t, cfg, `{"model":"museapitest","messages":[{"role":"user","content":"hi"}]}`, nil)
	if w.Code != 200 || w.Body.String() != okMessage {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if stubRuns(t, counter) != 1 {
		t.Fatalf("muse runs=%d, want 1", stubRuns(t, counter))
	}
	var msgAuths []string
	for _, q := range fm.requests() {
		if q.Path == "/messages" {
			msgAuths = append(msgAuths, q.Auth)
		}
	}
	if strings.Join(msgAuths, ",") != "Bearer k-old,Bearer k-new" {
		t.Fatalf("message auths=%v (want the rejected key, then the rotated one)", msgAuths)
	}
	if cred, _ := museAPICredential(cfg, false); cred.Key != "k-new" {
		t.Fatalf("cached key=%q, want the new one adopted", cred.Key)
	}
}

func TestMuseKeyGuardUnchangedInvalidKeyPassesThe401(t *testing.T) {
	cfg, fm, counter := keyGuardSetup(t, "ok", "k-bad", "", "k-good", 0)
	w := postMessages(t, cfg, `{"model":"museapitest","messages":[{"role":"user","content":"hi"}]}`, nil)
	if w.Code != 401 {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if stubRuns(t, counter) != 1 {
		t.Fatalf("muse runs=%d, want 1", stubRuns(t, counter))
	}
	n := 0
	for _, q := range fm.requests() {
		if q.Path == "/messages" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("message attempts=%d, want 1 (no retry when the key did not change)", n)
	}
}

func TestMuseKeyGuardSingleFlight(t *testing.T) {
	cfg, _, counter := keyGuardSetup(t, "ok", "k-1", "", "k-1", 400)
	var wg sync.WaitGroup
	var changed atomic.Int32
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if res := museKeyRefresh(context.Background(), cfg, "test"); res.Changed {
				changed.Add(1)
			}
		}()
	}
	wg.Wait()
	if runs := stubRuns(t, counter); runs != 1 {
		t.Fatalf("muse runs=%d for 5 concurrent refreshes, want 1", runs)
	}
	if changed.Load() != 0 {
		t.Fatal("unchanged key reported as changed")
	}
	res := museKeyRefresh(context.Background(), cfg, "test")
	if res.CLI != "ok" || res.Models != 200 || res.Changed || res.Before != res.After || res.Before == "none" {
		t.Fatalf("refresh result=%+v", res)
	}
}

func TestMuseKeyGuardSkippedWithProxyKey(t *testing.T) {
	cfg, _, counter := keyGuardSetup(t, "rotate", "k-old", "k-new", "k-new", 0)
	cfg.MuseAPIKey = "k-env"
	res := museKeyRefresh(context.Background(), cfg, "test")
	if res.Skipped == "" || stubRuns(t, counter) != 0 {
		t.Fatalf("res=%+v runs=%d", res, stubRuns(t, counter))
	}
}

func TestMuseKeyCheckArgs(t *testing.T) {
	args := strings.Join(museKeyCheckArgs(config{Models: map[string]string{"spark": "muse-spark-1.3-contributor[1m]"}, ModelForward: map[string]string{"spark": "muse"}}), " ")
	for _, want := range []string{"exec", "--model muse-spark-1.3-contributor", "--json", "--max-model-steps 1", "--disable-write", "--disable-shell", "--disable-web-tools", "--disable-reminders", "--no-session-log"} {
		if !strings.Contains(args, want) {
			t.Errorf("args %q missing %q", args, want)
		}
	}
	if !strings.Contains(strings.Join(museChildEnv(config{}), "\n"), "MUSE_NO_AUTO_UPDATE=1") {
		t.Error("child env must disable the launcher's auto-update")
	}
}
