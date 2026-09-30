package main

// muse_test.go — tests for the "muse" upstream (see muse.go).
//
// Golden parser tests run over testdata/muse/*.jsonl. Run/stream/handler
// tests execute a stub `muse` binary: a tiny trampoline compiled at test time
// that re-execs this test binary as TestMuseHelperProcess (the classic
// TestHelperProcess re-exec idiom). The trampoline is needed because muse's
// argv is fixed by museArgs, so the -test.run flag cannot be injected
// directly. Modes are selected with MUSE_STUB_MODE.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// museTrampolineSrc is a minimal `muse` stand-in: it re-execs the test binary
// (via MUSE_TESTBIN) as TestMuseHelperProcess, forwarding muse's argv after
// "--". Compiling a real executable keeps this portable (no shell scripts).
const museTrampolineSrc = `package main

import (
	"os"
	"os/exec"
)

func main() {
	bin := os.Getenv("MUSE_TESTBIN")
	if bin == "" {
		os.Exit(3)
	}
	args := append([]string{"-test.run=TestMuseHelperProcess", "--"}, os.Args[1:]...)
	cmd := exec.Command(bin, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin
	if err := cmd.Run(); err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			os.Exit(ee.ExitCode())
		}
		os.Exit(1)
	}
}
`

// TestMuseHelperProcess is not a real test. When GO_WANT_HELPER_PROCESS=1 it
// plays "muse", emitting canned --json JSONL on stdout per MUSE_STUB_MODE:
// ok (prompt echo), fail (terminal failed), noterminal, empty, garbage.
func TestMuseHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_HELPER_PROCESS") != "1" {
		return
	}
	prompt := ""
	args := os.Args
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "--prompt-file" {
			b, err := os.ReadFile(args[i+1])
			if err != nil {
				os.Exit(4)
			}
			prompt = string(b)
		}
	}
	emit := func(payloadType string, payload map[string]any) {
		line, _ := json.Marshal(map[string]any{"payload_type": payloadType, "payload": payload})
		os.Stdout.Write(append(line, '\n'))
	}
	switch os.Getenv("MUSE_STUB_MODE") {
	case "ok":
		full := "stub saw: " + prompt
		half := len(full) / 2
		emit("run.output.delta", map[string]any{"kind": "run_output_delta", "text": full[:half]})
		emit("run.output.delta", map[string]any{"kind": "run_output_delta", "text": full[half:]})
		emit("run.terminal.completed", map[string]any{"kind": "run_terminal", "terminal": "completed", "text": full, "reason": nil})
	case "fail":
		emit("task.lifecycle.failed", map[string]any{"kind": "task_lifecycle", "event": map[string]any{"kind": "failed", "reason": "stub boom"}})
		emit("run.terminal.failed", map[string]any{"kind": "run_terminal", "terminal": "failed", "text": "", "reason": "stub boom"})
	case "noterminal":
		emit("run.output.delta", map[string]any{"kind": "run_output_delta", "text": "partial"})
	case "empty":
		// exit 0 with no output
	case "garbage":
		os.Stdout.WriteString("this is not json\n")
	default:
		os.Stderr.WriteString("unknown MUSE_STUB_MODE\n")
		os.Exit(2)
	}
	os.Exit(0)
}

// buildMuseStub compiles the trampoline stub and returns its path.
func buildMuseStub(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	src := filepath.Join(dir, "musestub.go")
	if err := os.WriteFile(src, []byte(museTrampolineSrc), 0o644); err != nil {
		t.Fatal(err)
	}
	exe := filepath.Join(dir, "musestub.exe")
	cmd := exec.Command("go", "build", "-o", exe, src)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOFLAGS=-buildvcs=false", "GOTOOLCHAIN=local")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build muse stub: %v\n%s", err, out)
	}
	return exe
}

// museStubEnv points the stub plumbing at this test binary in the given mode.
func museStubEnv(t *testing.T, mode string) {
	t.Helper()
	t.Setenv("MUSE_TESTBIN", os.Args[0])
	t.Setenv("MUSE_STUB_MODE", mode)
	t.Setenv("GO_WANT_HELPER_PROCESS", "1")
}

// museStubConfig builds a direct (non-env) config running `muse` via stub.
func museStubConfig(t *testing.T, stub string) config {
	t.Helper()
	return config{
		MuseBin:          stub,
		MuseWorkDir:      t.TempDir(),
		MuseTimeout:      time.Minute,
		MuseTotalTimeout: 2 * time.Minute,
		MuseRetries:      0, // no backoff sleeps in error tests
	}
}

// TestMuseParseGoldenFixtures runs the parser over every committed muse
// fixture: final text, delta accumulation, and the failed-task-yet-Ok shape.
func TestMuseParseGoldenFixtures(t *testing.T) {
	files, err := filepath.Glob("testdata/muse/*.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no testdata/muse/*.jsonl fixtures found")
	}
	const want = "echo: say hi in five words"
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		res, err := parseMuseJSONL(raw)
		if err != nil {
			t.Errorf("%s: parse error: %v", f, err)
			continue
		}
		if !res.Ok {
			t.Errorf("%s: Ok=false, Error=%q", f, res.Error)
			continue
		}
		if res.Response != want {
			t.Errorf("%s: Response=%q, want %q", f, res.Response, want)
		}
		var acc strings.Builder
		for _, line := range strings.Split(string(raw), "\n") {
			acc.WriteString(museTextDelta([]byte(line)))
		}
		if acc.String() != res.Response {
			t.Errorf("%s: accumulated deltas=%q, want final text %q", f, acc.String(), res.Response)
		}
		// The echo fixtures carry a failed reminder task yet exit 0 with a
		// completed terminal: the terminal record stays authoritative.
		if !strings.Contains(string(raw), "task.lifecycle.failed") {
			t.Errorf("%s: expected a task.lifecycle.failed record in the fixture", f)
		}
	}
}

// TestMuseParseErrorShapes covers terminal failure, missing terminal,
// empty/non-JSONL output, the no-delta fallback, and delta ordering.
func TestMuseParseErrorShapes(t *testing.T) {
	delta := func(text string) string {
		return `{"payload_type":"run.output.delta","payload":{"kind":"run_output_delta","text":` + quoteJSON(text) + `}}`
	}
	terminal := func(tType, text, reason string) string {
		r := "null"
		if reason != "" {
			r = quoteJSON(reason)
		}
		return `{"payload_type":"run.terminal.` + tType + `","payload":{"kind":"run_terminal","terminal":` + quoteJSON(tType) + `,"text":` + quoteJSON(text) + `,"reason":` + r + `}}`
	}

	t.Run("failed terminal carries reason", func(t *testing.T) {
		raw := delta("partial") + "\n" + terminal("failed", "", "kaput") + "\n"
		res, err := parseMuseJSONL([]byte(raw))
		if err != nil {
			t.Fatalf("parse error: %v", err)
		}
		if res.Ok {
			t.Fatalf("Ok=true, want failure")
		}
		if !strings.Contains(res.Error, "kaput") {
			t.Fatalf("Error=%q, want it to contain %q", res.Error, "kaput")
		}
	})

	t.Run("failed task without terminal", func(t *testing.T) {
		raw := `{"payload_type":"task.lifecycle.failed","payload":{"kind":"task_lifecycle","event":{"kind":"failed","reason":"task went sideways"}}}` + "\n"
		res, err := parseMuseJSONL([]byte(raw))
		if err != nil {
			t.Fatalf("parse error: %v", err)
		}
		if res.Ok {
			t.Fatalf("Ok=true, want failure")
		}
		if !strings.Contains(res.Error, "task went sideways") {
			t.Fatalf("Error=%q, want the task reason", res.Error)
		}
	})

	t.Run("deltas without terminal", func(t *testing.T) {
		res, err := parseMuseJSONL([]byte(delta("partial") + "\n"))
		if err != nil {
			t.Fatalf("parse error: %v", err)
		}
		if res.Ok {
			t.Fatalf("Ok=true, want failure")
		}
		if !strings.Contains(res.Error, "no terminal record") {
			t.Fatalf("Error=%q, want missing-terminal error", res.Error)
		}
	})

	t.Run("empty output is a parse error", func(t *testing.T) {
		if _, err := parseMuseJSONL([]byte("  \n ")); err == nil {
			t.Fatal("expected an error for empty output")
		}
	})

	t.Run("non-JSONL output is a parse error", func(t *testing.T) {
		_, err := parseMuseJSONL([]byte("this is not json\n"))
		if err == nil || !strings.Contains(err.Error(), "not JSONL") {
			t.Fatalf("err=%v, want a not-JSONL error", err)
		}
	})

	t.Run("terminal text fallback without deltas", func(t *testing.T) {
		res, err := parseMuseJSONL([]byte(terminal("completed", "full text here", "") + "\n"))
		if err != nil {
			t.Fatalf("parse error: %v", err)
		}
		if !res.Ok || res.Response != "full text here" {
			t.Fatalf("res=%+v, want Ok with the terminal text", res)
		}
	})

	t.Run("deltas accumulate in order", func(t *testing.T) {
		raw := delta("a") + "\n" + delta("b") + "\n" + delta("c") + "\n" + terminal("completed", "abc", "") + "\n"
		res, err := parseMuseJSONL([]byte(raw))
		if err != nil {
			t.Fatalf("parse error: %v", err)
		}
		if !res.Ok || res.Response != "abc" {
			t.Fatalf("res=%+v, want Ok with %q", res, "abc")
		}
	})

	t.Run("failed task plus completed terminal still Ok", func(t *testing.T) {
		raw := `{"payload_type":"task.lifecycle.failed","payload":{"kind":"task_lifecycle","event":{"kind":"failed","reason":"reminder noise"}}}` + "\n" +
			delta("hi") + "\n" + terminal("completed", "hi", "") + "\n"
		res, err := parseMuseJSONL([]byte(raw))
		if err != nil {
			t.Fatalf("parse error: %v", err)
		}
		if !res.Ok || res.Response != "hi" {
			t.Fatalf("res=%+v, want Ok (terminal authoritative)", res)
		}
	})
}

func quoteJSON(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// TestMuseTextDelta checks the single-line delta extractor.
func TestMuseTextDelta(t *testing.T) {
	if got := museTextDelta([]byte(`{"payload_type":"run.output.delta","payload":{"kind":"run_output_delta","text":"hi"}}`)); got != "hi" {
		t.Fatalf("delta line = %q, want %q", got, "hi")
	}
	if got := museTextDelta([]byte(`{"payload_type":"run.terminal.completed","payload":{"text":"hi"}}`)); got != "" {
		t.Fatalf("terminal line = %q, want empty", got)
	}
	if got := museTextDelta([]byte(`not json`)); got != "" {
		t.Fatalf("garbage line = %q, want empty", got)
	}
}

// TestMuseModelFor covers the global override, alias passthrough, and the
// trailing context-marker strip.
func TestMuseModelFor(t *testing.T) {
	cfg := config{Models: map[string]string{"spark": "muse-spark-1.2"}}
	cfg.MuseModel = "muse-spark-1.3"
	if got := museModelFor(cfg, "spark"); got != "muse-spark-1.3" {
		t.Fatalf("override = %q, want muse-spark-1.3", got)
	}
	cfg.MuseModel = ""
	if got := museModelFor(cfg, "spark"); got != "muse-spark-1.2" {
		t.Fatalf("passthrough = %q, want muse-spark-1.2", got)
	}
	cfg.Models["spark"] = "muse-spark-1.2[1m]"
	if got := museModelFor(cfg, "spark"); got != "muse-spark-1.2" {
		t.Fatalf("marker strip = %q, want muse-spark-1.2", got)
	}
	if got := museModelFor(config{}, "muse-spark-1.1"); got != "muse-spark-1.1" {
		t.Fatalf("unknown alias = %q, want verbatim passthrough", got)
	}
}

// TestMuseArgs asserts the exact discovery-verified flag set.
func TestMuseArgs(t *testing.T) {
	pf := string(filepath.Separator) + filepath.Join("tmp", "prompt-1.txt")
	got := museArgs(config{MuseProvider: "echo"}, "muse-spark-1.3", pf)
	want := []string{"exec", "--provider", "echo", "--model", "muse-spark-1.3", "--json",
		"--prompt-file", pf, "--no-session-log", "--max-model-steps", "1",
		"--disable-write", "--disable-shell", "--disable-web-tools", "--no-foreign-personal-context"}
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("args = %q, want %q", got, want)
	}
	for _, a := range got {
		if strings.Contains(a, "reasoning") {
			t.Fatalf("args %q must not contain --reasoning-effort (rejected by echo provider)", got)
		}
	}

	bare := museArgs(config{}, "", pf)
	for _, a := range bare {
		if a == "--provider" || a == "--model" {
			t.Fatalf("empty provider/model must be omitted, got %q", bare)
		}
	}
	if bare[0] != "exec" {
		t.Fatalf("bare args = %q, want exec first", bare)
	}
}

// TestMuseForwardNormalization checks that "muse" is an accepted forward
// target and that the Models-page status honors it.
func TestMuseForwardNormalization(t *testing.T) {
	for in, want := range map[string]string{"muse": "muse", " Muse ": "muse", "MUSE": "muse", "xyz": "codex", "": "codex"} {
		if got := normalizeForwardTarget(in); got != want {
			t.Errorf("normalizeForwardTarget(%q) = %q, want %q", in, got, want)
		}
	}
	cfg := config{ModelForward: map[string]string{"spark": "muse"}}
	if got := forwardForAlias(cfg, "spark"); got != "muse" {
		t.Fatalf("forwardForAlias(spark) = %q, want muse", got)
	}
	if got := forwardForAlias(cfg, "other"); got != "codex" {
		t.Fatalf("forwardForAlias(other) = %q, want codex", got)
	}
	if got := modelStatusForRow(cfg, "spark", "muse-spark-1.3"); got != "ok" {
		t.Fatalf("status spark/muse-spark-1.3 = %q, want ok", got)
	}
	if got := modelStatusForRow(cfg, "spark", "muse-spark-1.2[1m]"); got != "ok" {
		t.Fatalf("status with context marker = %q, want ok", got)
	}
	if got := modelStatusForRow(cfg, "spark", "gpt-5"); got != "untested" {
		t.Fatalf("status spark/gpt-5 = %q, want untested", got)
	}
}

// TestMuseForwardToPersistRoundTrip mirrors the agy persist test with a muse
// row: runtime map, persisted JSON, modelRows, and env-loader round-trip.
func TestMuseForwardToPersistRoundTrip(t *testing.T) {
	t.Setenv("PROXY_MODEL_ALIASES", "")
	t.Setenv("PROXY_MODEL_ALIASES_DISABLED", "")
	vals := map[string]string{}
	next, err := saveModelAliasesToEnvMap(vals, []modelAliasConfig{
		{Alias: "spark", Real: "muse-spark-1.3", Context: "200k", ForwardTo: "muse"},
		{Alias: "plain", Real: "gpt-5.4-mini", Context: "200k"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if next.ModelForward["spark"] != "muse" {
		t.Fatalf("spark forward = %q, want muse", next.ModelForward["spark"])
	}
	if next.ModelForward["plain"] != "codex" {
		t.Fatalf("plain forward = %q, want codex (default)", next.ModelForward["plain"])
	}
	var overrides []modelAliasConfig
	if err := json.Unmarshal([]byte(vals["PROXY_MODEL_ALIASES"]), &overrides); err != nil {
		t.Fatalf("override JSON invalid: %v", err)
	}
	byAlias := map[string]modelAliasConfig{}
	for _, o := range overrides {
		byAlias[o.Alias] = o
	}
	if byAlias["spark"].ForwardTo != "muse" {
		t.Fatalf("persisted spark forward_to = %q, want muse", byAlias["spark"].ForwardTo)
	}
	got := map[string]any{}
	for _, r := range modelRows(next) {
		got[r["alias"].(string)] = r["forward_to"]
	}
	if got["spark"] != "muse" {
		t.Fatalf("modelRows spark forward_to = %v, want muse", got["spark"])
	}
	env := func(key, def string) string {
		if v, ok := vals[key]; ok {
			return v
		}
		return def
	}
	if _, _, _, forwards, _, _ := modelAliasesFromValues(env); forwards["spark"] != "muse" {
		t.Fatalf("loader spark forward = %q, want muse", forwards["spark"])
	}
}

// TestMuseRunSuccessViaStub runs runMuse against the stub: the prompt must
// arrive via --prompt-file and come back in the parsed response.
func TestMuseRunSuccessViaStub(t *testing.T) {
	stub := buildMuseStub(t)
	museStubEnv(t, "ok")
	cfg := museStubConfig(t, stub)

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	res, err := runMuse(ctx, cfg, "say hi", "muse-spark-1.3")
	if err != nil {
		t.Fatalf("runMuse: %v", err)
	}
	if !res.Ok {
		t.Fatalf("Ok=false, Error=%q", res.Error)
	}
	if res.Response != "stub saw: say hi" {
		t.Fatalf("Response=%q, want %q", res.Response, "stub saw: say hi")
	}
}

// TestMuseRunErrorsViaStub covers failure terminal, missing terminal, empty,
// and non-JSONL stub output.
func TestMuseRunErrorsViaStub(t *testing.T) {
	stub := buildMuseStub(t)
	cases := []struct {
		mode    string
		wantErr string // substring of err; "" means err must be nil
		wantRes string // substring of res.Error when wantErr == ""
	}{
		{"fail", "", "stub boom"},
		{"noterminal", "", "no terminal record"},
		{"empty", "produced no output", ""},
		{"garbage", "not JSONL", ""},
	}
	for _, c := range cases {
		t.Run(c.mode, func(t *testing.T) {
			museStubEnv(t, c.mode)
			cfg := museStubConfig(t, stub)
			ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
			defer cancel()
			res, err := runMuse(ctx, cfg, "hi", "muse-spark-1.3")
			if c.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), c.wantErr) {
					t.Fatalf("err=%v, want substring %q", err, c.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("err=%v, want parsed failure instead", err)
			}
			if res.Ok {
				t.Fatalf("Ok=true, want failure")
			}
			if !strings.Contains(res.Error, c.wantRes) {
				t.Fatalf("Error=%q, want substring %q", res.Error, c.wantRes)
			}
		})
	}
}

// TestMuseStreamViaStub checks that runMuseStream delivers each delta to
// onText and that the joined deltas equal the final response.
func TestMuseStreamViaStub(t *testing.T) {
	stub := buildMuseStub(t)
	museStubEnv(t, "ok")
	cfg := museStubConfig(t, stub)

	var chunks []string
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	res, err := runMuseStream(ctx, cfg, "stream me", "muse-spark-1.3", func(s string) { chunks = append(chunks, s) })
	if err != nil {
		t.Fatalf("runMuseStream: %v", err)
	}
	if !res.Ok {
		t.Fatalf("Ok=false, Error=%q", res.Error)
	}
	if len(chunks) != 2 {
		t.Fatalf("chunks=%q, want the 2 stub deltas", chunks)
	}
	if joined := strings.Join(chunks, ""); joined != res.Response {
		t.Fatalf("joined=%q, response=%q, want them equal", joined, res.Response)
	}
	if res.Response != "stub saw: stream me" {
		t.Fatalf("Response=%q, want the echoed prompt", res.Response)
	}
}

// TestMuseAnthropicHandlerShapeViaStub loads a real config with PROXY_MUSE_BIN
// pointed at the stub and drives serveMuseAnthropic, asserting the Anthropic
// response shape and the recorded request stat.
func TestMuseAnthropicHandlerShapeViaStub(t *testing.T) {
	stub := buildMuseStub(t)
	t.Setenv("PROXY_MUSE_BIN", stub)
	t.Setenv("PROXY_MUSE_ENABLED", "1")
	museStubEnv(t, "ok")

	cfg := loadConfig()
	if cfg.MuseBin != stub {
		t.Fatalf("MuseBin=%q, want the stub (PROXY_MUSE_BIN did not flow through)", cfg.MuseBin)
	}
	cfg.MuseWorkDir = t.TempDir()
	cfg.MuseRetries = 0
	cfg.MuseTimeout = time.Minute
	cfg.MuseTotalTimeout = 2 * time.Minute

	in := anthropicRequest{
		Model:     "muse-stub-shape-test",
		MaxTokens: 64,
		Messages:  []anthropicMessage{{Role: "user", Content: "say hi in five words"}},
	}
	r := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", nil)
	w := httptest.NewRecorder()
	serveMuseAnthropic(context.Background(), cfg, in, w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d, body=%s", w.Code, w.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v; body=%s", err, w.Body.String())
	}
	if body["type"] != "message" {
		t.Fatalf("type=%v, want message; body=%s", body["type"], w.Body.String())
	}
	content, _ := body["content"].([]any)
	if len(content) == 0 {
		t.Fatalf("empty content; body=%s", w.Body.String())
	}
	first, _ := content[0].(map[string]any)
	if text, _ := first["text"].(string); !strings.Contains(text, "stub saw:") {
		t.Fatalf("content text=%q, want the stub echo; body=%s", text, w.Body.String())
	}
	if _, ok := body["usage"].(map[string]any); !ok {
		t.Fatalf("no usage object; body=%s", w.Body.String())
	}
	if stat := takeRequestStat(r); stat.Upstream != "muse" {
		t.Fatalf("requestStat.Upstream=%q, want muse", stat.Upstream)
	}
}

// TestMuseDisabledRefusesRequests checks the kill switch: a muse-routed alias
// is refused with 400 while the upstream is off, without spawning anything.
func TestMuseDisabledRefusesRequests(t *testing.T) {
	prev := upstreamOffMuse.Load()
	t.Cleanup(func() { upstreamOffMuse.Store(prev) })
	upstreamOffMuse.Store(true)

	cfg := config{
		Models:       map[string]string{"spark": "muse-spark-1.3"},
		ModelForward: map[string]string{"spark": "muse"},
		MuseBin:      filepath.Join(t.TempDir(), "must-not-exist-muse"),
	}
	reqBody := `{"model":"spark","max_tokens":5,"messages":[{"role":"user","content":"hi"}]}`
	r := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", strings.NewReader(reqBody))
	w := httptest.NewRecorder()
	handleMessages(cfg)(w, r)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400; body=%s", w.Code, w.Body.String())
	}
	if body := w.Body.String(); !strings.Contains(body, "muse") || !strings.Contains(strings.ToLower(body), "disabled") {
		t.Fatalf("body=%s, want a muse-disabled refusal", body)
	}
}

// museToolTestSpec builds a minimal tool spec for settings/prompt tests.
func museToolTestSpec() *museToolSpec {
	return &museToolSpec{
		CallbackURL:   "https://connect.test/api/tools/cbuid123",
		CallbackToken: "tok-abc",
		Conversation:  "cbuid123",
		Agent:         "auid456",
		Tools: []connectToolDef{
			{Name: "get_bookings", Description: "List bookings.", InputSchema: json.RawMessage(`{"type":"object","properties":{"date":{"type":"string"}}}`)},
			{Name: "cancel_booking", Description: "", InputSchema: nil},
			{Name: "  ", Description: "nameless, must be skipped"},
		},
	}
}

func TestParseMuseToolMaxTurns(t *testing.T) {
	cases := map[string]int{"": 20, "0": 20, "-3": 20, "abc": 20, "5": 5, "20": 20, "60": 60, "99": 60}
	for in, want := range cases {
		if got := parseMuseToolMaxTurns(in); got != want {
			t.Errorf("parseMuseToolMaxTurns(%q)=%d, want %d", in, got, want)
		}
	}
	if got := museToolMaxTurns(config{}); got != 20 {
		t.Errorf("museToolMaxTurns(zero)=%d, want 20", got)
	}
	if got := museToolTimeout(config{}); got != 600*time.Second {
		t.Errorf("museToolTimeout(zero)=%v, want 600s", got)
	}
}

func TestMuseToolSpecFromRequest(t *testing.T) {
	catalog := json.RawMessage(`[{"name":"a","description":"d","input_schema":{"type":"object"}}]`)
	withHeaders := func() *http.Request {
		r := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", nil)
		r.Header.Set("X-Connect-Callback-Url", "https://connect.test/cb")
		r.Header.Set("X-Connect-Callback-Token", "tok")
		r.Header.Set("X-Connect-Conversation", "c1")
		r.Header.Set("X-Connect-Agent", "a1")
		return r
	}
	spec := museToolSpecFromRequest(config{MuseToolsEnabled: true}, catalog, withHeaders())
	if spec == nil {
		t.Fatal("enabled+full request returned nil spec")
	}
	if spec.CallbackURL != "https://connect.test/cb" || spec.CallbackToken != "tok" || len(spec.Tools) != 1 || spec.Tools[0].Name != "a" {
		t.Fatalf("spec=%+v, want url/token/1 tool", spec)
	}
	if got := museToolSpecFromRequest(config{}, catalog, withHeaders()); got != nil {
		t.Errorf("disabled switch returned %+v, want nil", got)
	}
	if got := museToolSpecFromRequest(config{MuseToolsEnabled: true}, catalog, nil); got != nil {
		t.Errorf("nil request returned %+v, want nil", got)
	}
	if got := museToolSpecFromRequest(config{MuseToolsEnabled: true}, nil, withHeaders()); got != nil {
		t.Errorf("empty catalog returned %+v, want nil", got)
	}
	if got := museToolSpecFromRequest(config{MuseToolsEnabled: true}, json.RawMessage(`[]`), withHeaders()); got != nil {
		t.Errorf("empty tools array returned %+v, want nil", got)
	}
	if got := museToolSpecFromRequest(config{MuseToolsEnabled: true}, json.RawMessage(`{bad`), withHeaders()); got != nil {
		t.Errorf("bad JSON returned %+v, want nil", got)
	}
	noToken := withHeaders()
	noToken.Header.Del("X-Connect-Callback-Token")
	if got := museToolSpecFromRequest(config{MuseToolsEnabled: true}, catalog, noToken); got != nil {
		t.Errorf("missing token returned %+v, want nil", got)
	}
}

func TestMuseToolSystemPrompt(t *testing.T) {
	out := museToolSystemPrompt(museToolTestSpec())
	for _, want := range []string{"connect_execute_tool", "get_bookings", "List bookings.", `"date":{"type":"string"}`, "cancel_booking"} {
		if !strings.Contains(out, want) {
			t.Errorf("prompt missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "nameless, must be skipped") {
		t.Errorf("nameless tool leaked into prompt:\n%s", out)
	}
}

func TestWriteMuseToolSettingsWithAPIKey(t *testing.T) {
	cfg := config{MuseAPIKey: "test-key-not-real"}
	dir, cleanup, err := writeMuseToolSettings(cfg, museToolTestSpec())
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	raw, err := os.ReadFile(filepath.Join(dir, "muse", "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	var conf map[string]any
	if err := json.Unmarshal(raw, &conf); err != nil {
		t.Fatalf("settings.json invalid: %v", err)
	}
	if conf["schema_version"] != float64(1) {
		t.Errorf("schema_version=%v, want 1", conf["schema_version"])
	}
	servers, _ := conf["mcp_servers"].(map[string]any)
	srv, _ := servers["connect"].(map[string]any)
	if srv["transport"] != "stdio" || srv["enabled"] != true {
		t.Errorf("server=%v, want stdio+enabled", srv)
	}
	if cmd, _ := srv["command"].(string); cmd == "" {
		t.Error("empty MCP command")
	}
	args, _ := srv["args"].([]any)
	if len(args) != 1 || args[0] != "claude-mcp-gateway" {
		t.Errorf("args=%v, want [claude-mcp-gateway]", args)
	}
	env, _ := srv["env"].(map[string]any)
	if env["CONNECT_CALLBACK_URL"] != "https://connect.test/api/tools/cbuid123" || env["CONNECT_CALLBACK_TOKEN"] != "tok-abc" {
		t.Errorf("env=%v, want callback url+token", env)
	}
	var names []string
	allowedStr, _ := env["CONNECT_ALLOWED_TOOLS"].(string)
	if err := json.Unmarshal([]byte(allowedStr), &names); err != nil || len(names) != 2 {
		t.Errorf("allowed=%v/%v, want the 2 named tools", allowedStr, err)
	}
	if runtime.GOOS != "windows" {
		if fi, _ := os.Stat(filepath.Join(dir, "muse", "settings.json")); fi.Mode().Perm() != 0o600 {
			t.Errorf("settings.json perms=%o, want 600", fi.Mode().Perm())
		}
		if fi, _ := os.Stat(dir); fi.Mode().Perm() != 0o700 {
			t.Errorf("xdg dir perms=%o, want 700", fi.Mode().Perm())
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "muse", "auth.json")); !os.IsNotExist(err) {
		t.Error("auth.json linked despite API key being set")
	}
	cleanup()
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Error("cleanup did not remove the xdg dir")
	}
}

func TestWriteMuseToolSettingsLinksAmbientLogin(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation needs privileges on Windows; covered on Linux CI")
	}
	home := t.TempDir()
	museDir := filepath.Join(home, ".config", "muse")
	if err := os.MkdirAll(museDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(museDir, "auth.json"), []byte(`{"fake":"auth"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	dir, cleanup, err := writeMuseToolSettings(config{}, museToolTestSpec())
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	got, err := os.ReadFile(filepath.Join(dir, "muse", "auth.json"))
	if err != nil {
		t.Fatalf("auth link unreadable: %v", err)
	}
	if string(got) != `{"fake":"auth"}` {
		t.Errorf("auth link content=%q, want the fixture", got)
	}
}

func TestWriteMuseToolSettingsNoAuthFailsClean(t *testing.T) {
	empty := t.TempDir()
	t.Setenv("HOME", empty)
	t.Setenv("USERPROFILE", empty)
	_, _, err := writeMuseToolSettings(config{}, museToolTestSpec())
	if err == nil || !strings.Contains(err.Error(), "PROXY_MUSE_API_KEY") {
		t.Errorf("err=%v, want the login-or-key error", err)
	}
}

func TestMuseToolArgs(t *testing.T) {
	args := museToolArgs(config{MuseToolMaxTurns: 7}, "muse-spark-1.3", "/tmp/p.txt")
	joined := strings.Join(args, " ")
	for _, want := range []string{"--json", "--prompt-file /tmp/p.txt", "--max-model-steps 7", "--disable-write", "--disable-shell", "--disable-web-tools", "--approval-mode never", "--model muse-spark-1.3"} {
		if !strings.Contains(joined, want) {
			t.Errorf("args %q missing %q", joined, want)
		}
	}
	if args[0] != "exec" {
		t.Errorf("args[0]=%q, want exec", args[0])
	}
}

func TestMuseToolJSONLGolden(t *testing.T) {
	// Unknown tool-event record types must not break parsing; the terminal
	// record stays authoritative (shape from the live spike capture).
	raw := []byte("{\"payload_type\":\"run.tool.invoked\",\"payload\":{\"kind\":\"x\"}}\n" +
		"{\"payload_type\":\"run.output.delta\",\"payload\":{\"kind\":\"run_output_delta\",\"text\":\"STUB-ECHO:hi\"}}\n" +
		"{\"payload_type\":\"run.terminal.completed\",\"payload\":{\"kind\":\"run_terminal\",\"terminal\":\"completed\",\"text\":\"STUB-ECHO:hi\",\"reason\":null}}\n")
	res, err := parseMuseJSONL(raw)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Ok || res.Response != "STUB-ECHO:hi" {
		t.Errorf("res=%+v, want Ok with the terminal text", res)
	}
}

func TestMuseAnthropicToolsHandlerViaStub(t *testing.T) {
	stub := buildMuseStub(t)
	museStubEnv(t, "ok")
	cfg := museStubConfig(t, stub)
	cfg.MuseAPIKey = "test-key-not-real" // skip the ambient-login symlink
	cfg.MuseToolsEnabled = true
	in := anthropicRequest{
		Model:        "muse-stub-tools-test",
		MaxTokens:    64,
		Messages:     []anthropicMessage{{Role: "user", Content: "book me"}},
		ConnectTools: json.RawMessage(`[{"name":"get_bookings","description":"d","input_schema":{"type":"object"}}]`),
	}
	r := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", nil)
	r.Header.Set("X-Connect-Callback-Url", "https://connect.test/cb")
	r.Header.Set("X-Connect-Callback-Token", "tok")
	w := httptest.NewRecorder()
	serveMuseAnthropic(context.Background(), cfg, in, w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d, body=%s", w.Code, w.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v; body=%s", err, w.Body.String())
	}
	content, _ := body["content"].([]any)
	if len(content) == 0 {
		t.Fatalf("empty content; body=%s", w.Body.String())
	}
	first, _ := content[0].(map[string]any)
	text, _ := first["text"].(string)
	if !strings.Contains(text, "connect_execute_tool") || !strings.Contains(text, "get_bookings") {
		t.Errorf("text=%q, want the catalog instructions echoed by the stub", text)
	}
}
