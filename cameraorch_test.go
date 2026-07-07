package main

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// ─────────────────────── extractFirstJSONObject ───────────────────────

func TestExtractFirstJSONObject(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
		ok   bool
	}{
		{"plain", `{"a":1}`, `{"a":1}`, true},
		{"leading_prose", `Sure, here you go: {"a":1}`, `{"a":1}`, true},
		{"trailing_prose", `{"a":1} — hope that helps!`, `{"a":1}`, true},
		{"both_prose", `Result:\n{"a":1}\nDone.`, `{"a":1}`, true},
		{"json_fence", "```json\n{\"a\":1}\n```", `{"a":1}`, true},
		{"bare_fence", "```\n{\"a\":1,\"b\":2}\n```", `{"a":1,"b":2}`, true},
		{"nested", `{"a":{"b":{"c":3}}}`, `{"a":{"b":{"c":3}}}`, true},
		{"brace_in_string", `{"a":"}{"}`, `{"a":"}{"}`, true},
		{"escaped_quote_in_string", `{"a":"he said \"hi\" }"}`, `{"a":"he said \"hi\" }"}`, true},
		{"escaped_backslash", `{"a":"c:\\path\\"}`, `{"a":"c:\\path\\"}`, true},
		{"prose_brace_before_real", `note {nope} then {"a":1}`, `{"a":1}`, true},
		{"array_of_prose_ignored", `[not json] {"a":1}`, `{"a":1}`, true},
		{"garbage", `hello world no json here`, "", false},
		{"unterminated", `{"a":1`, "", false},
		{"empty", ``, "", false},
		{"only_open_brace", `{`, "", false},
		{"invalid_only", `{nope}`, "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := extractFirstJSONObject(c.in)
			if ok != c.ok {
				t.Fatalf("ok = %v, want %v (got=%q)", ok, c.ok, string(got))
			}
			if string(got) != c.want {
				t.Fatalf("got %q, want %q", string(got), c.want)
			}
		})
	}
}

// ─────────────────────── parseDecision: happy path ───────────────────────

func TestParseDecisionValid(t *testing.T) {
	raw := `{
		"round_summary": "quiet lobby",
		"per_camera": [
			{"camera_id": "cam1", "observation": "empty", "notable": false},
			{"camera_id": "cam2", "observation": "person waiting", "notable": true}
		],
		"suspicion": "medium",
		"rule_status": "possibly_met",
		"need_more": {"type": "clip", "camera_ids": ["cam2"], "clip": {"from_offset_sec": -180, "to_offset_sec": 0}},
		"triggered_actions": [{"action": "log", "severity": "warn", "reason": "loiter", "camera_ids": ["cam2"]}],
		"final": false,
		"confidence": 0.72
	}`
	allowed := map[string]bool{"cam1": true, "cam2": true}
	d, err := parseDecision(raw, allowed)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d.RoundSummary != "quiet lobby" {
		t.Errorf("round_summary = %q", d.RoundSummary)
	}
	if len(d.PerCamera) != 2 || d.PerCamera[1].CameraID != "cam2" || !d.PerCamera[1].Notable {
		t.Errorf("per_camera = %+v", d.PerCamera)
	}
	if d.Suspicion != "medium" || d.RuleStatus != "possibly_met" {
		t.Errorf("enums = %q / %q", d.Suspicion, d.RuleStatus)
	}
	if d.NeedMore.Type != "clip" || d.NeedMore.Clip == nil {
		t.Fatalf("need_more = %+v", d.NeedMore)
	}
	if d.NeedMore.Clip.FromOffsetSec != -180 || d.NeedMore.Clip.ToOffsetSec != 0 {
		t.Errorf("clip window = %+v", *d.NeedMore.Clip)
	}
	if len(d.TriggeredActions) != 1 || d.TriggeredActions[0].Action != "log" {
		t.Errorf("triggered_actions = %+v", d.TriggeredActions)
	}
	if d.Final != false {
		t.Errorf("final = %v", d.Final)
	}
	if d.Confidence != 0.72 {
		t.Errorf("confidence = %v", d.Confidence)
	}
}

func TestParseDecisionFencedAndProse(t *testing.T) {
	raw := "Here is my assessment.\n```json\n" +
		`{"round_summary":"ok","suspicion":"high","rule_status":"met","need_more":{"type":"none"},"final":true,"confidence":0.9}` +
		"\n```\nLet me know if you need more."
	d, err := parseDecision(raw, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d.Suspicion != "high" || d.RuleStatus != "met" || !d.Final {
		t.Errorf("decision = %+v", d)
	}
}

// ─────────────────────── parseDecision: normalization ───────────────────────

func TestParseDecisionEnumDefaults(t *testing.T) {
	cases := []struct {
		name     string
		raw      string
		wantSusp string
		wantRule string
		wantNeed string
	}{
		{"blank_all", `{"round_summary":"x"}`, "low", "not_met", "none"},
		{"blank_strings", `{"suspicion":"","rule_status":"","need_more":{"type":""}}`, "low", "not_met", "none"},
		{"unknown_values", `{"suspicion":"elevated","rule_status":"maybe","need_more":{"type":"video"}}`, "low", "not_met", "none"},
		{"uppercase", `{"suspicion":"HIGH","rule_status":"MET","need_more":{"type":"FULL_IMAGES"}}`, "high", "met", "full_images"},
		{"whitespace", `{"suspicion":" medium ","rule_status":" possibly_met "}`, "medium", "possibly_met", "none"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d, err := parseDecision(c.raw, nil)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if d.Suspicion != c.wantSusp {
				t.Errorf("suspicion = %q, want %q", d.Suspicion, c.wantSusp)
			}
			if d.RuleStatus != c.wantRule {
				t.Errorf("rule_status = %q, want %q", d.RuleStatus, c.wantRule)
			}
			if d.NeedMore.Type != c.wantNeed {
				t.Errorf("need_more.type = %q, want %q", d.NeedMore.Type, c.wantNeed)
			}
		})
	}
}

func TestParseDecisionCameraIDFiltering(t *testing.T) {
	raw := `{
		"suspicion":"high","rule_status":"met",
		"need_more":{"type":"full_images","camera_ids":["cam1","ghost","cam3"," cam2 "]},
		"triggered_actions":[
			{"action":"connect_callback","camera_ids":["cam1","bogus"]},
			{"action":"webhook","camera_ids":["nope"]}
		]
	}`
	allowed := map[string]bool{"cam1": true, "cam2": true, "cam3": true}
	d, err := parseDecision(raw, allowed)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	wantNeed := []string{"cam1", "cam3", "cam2"}
	if !reflect.DeepEqual(d.NeedMore.CameraIDs, wantNeed) {
		t.Errorf("need_more.camera_ids = %v, want %v", d.NeedMore.CameraIDs, wantNeed)
	}
	if !reflect.DeepEqual(d.TriggeredActions[0].CameraIDs, []string{"cam1"}) {
		t.Errorf("action0 camera_ids = %v, want [cam1]", d.TriggeredActions[0].CameraIDs)
	}
	if len(d.TriggeredActions[1].CameraIDs) != 0 {
		t.Errorf("action1 camera_ids = %v, want empty", d.TriggeredActions[1].CameraIDs)
	}
}

func TestParseDecisionNilAllowedKeepsIDs(t *testing.T) {
	raw := `{"need_more":{"type":"clip","camera_ids":["anything","x"]}}`
	d, err := parseDecision(raw, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !reflect.DeepEqual(d.NeedMore.CameraIDs, []string{"anything", "x"}) {
		t.Errorf("camera_ids = %v, want unchanged", d.NeedMore.CameraIDs)
	}
}

// ─────────────────────── parseDecision: tolerant scalars ───────────────────────

func TestParseDecisionFlexibleScalars(t *testing.T) {
	// final as a string, confidence as a string, clip offsets as float/string.
	raw := `{
		"suspicion":"low","rule_status":"not_met",
		"need_more":{"type":"clip","clip":{"from_offset_sec":-120.0,"to_offset_sec":"0"}},
		"final":"true",
		"confidence":"0.5"
	}`
	d, err := parseDecision(raw, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !d.Final {
		t.Errorf("final = %v, want true", d.Final)
	}
	if d.Confidence != 0.5 {
		t.Errorf("confidence = %v, want 0.5", d.Confidence)
	}
	if d.NeedMore.Clip == nil || d.NeedMore.Clip.FromOffsetSec != -120 || d.NeedMore.Clip.ToOffsetSec != 0 {
		t.Errorf("clip = %+v", d.NeedMore.Clip)
	}
}

func TestParseDecisionConfidenceClamped(t *testing.T) {
	for _, c := range []struct {
		in   string
		want float64
	}{
		{`{"confidence":2.5}`, 1},
		{`{"confidence":-0.4}`, 0},
		{`{"confidence":0.33}`, 0.33},
	} {
		d, err := parseDecision(c.in, nil)
		if err != nil {
			t.Fatalf("unexpected error for %q: %v", c.in, err)
		}
		if d.Confidence != c.want {
			t.Errorf("confidence(%q) = %v, want %v", c.in, d.Confidence, c.want)
		}
	}
}

func TestParseDecisionClipAbsentIsNil(t *testing.T) {
	d, err := parseDecision(`{"need_more":{"type":"full_images"}}`, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d.NeedMore.Clip != nil {
		t.Errorf("clip = %+v, want nil", d.NeedMore.Clip)
	}
}

// ─────────────────────── parseDecision: failures ───────────────────────

func TestParseDecisionErrors(t *testing.T) {
	cases := []struct {
		name string
		raw  string
	}{
		{"no_json", "I could not analyze the images, sorry."},
		{"empty", ""},
		{"unterminated", `{"suspicion":"high"`},
		{"only_prose_braces", `the {quick} {brown} fox`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := parseDecision(c.raw, nil); err == nil {
				t.Fatalf("expected error, got nil")
			}
		})
	}
}

// ─────────────────────── helper unit tests ───────────────────────

func TestCamNormalizeEnum(t *testing.T) {
	if got := camNormalizeEnum("", camSuspicionLevels, "low"); got != "low" {
		t.Errorf("blank -> %q", got)
	}
	if got := camNormalizeEnum("HIGH", camSuspicionLevels, "low"); got != "high" {
		t.Errorf("HIGH -> %q", got)
	}
	if got := camNormalizeEnum("bogus", camRuleStatuses, "not_met"); got != "not_met" {
		t.Errorf("bogus -> %q", got)
	}
}

func TestCamImageLabelAndClip(t *testing.T) {
	cases := []struct {
		path      string
		wantLabel string
		wantClip  bool
	}{
		{"/tmp/x/cam1.jpg", "cam1", false},
		{"/tmp/x/front-door.png", "front-door", false},
		{`C:\caps\lobby.mp4`, "lobby", true},
		{"/caps/rear.MOV", "rear", true},
		{"noext", "noext", false},
	}
	for _, c := range cases {
		if got := camImageLabel(c.path); got != c.wantLabel {
			t.Errorf("camImageLabel(%q) = %q, want %q", c.path, got, c.wantLabel)
		}
		if got := camIsClip(c.path); got != c.wantClip {
			t.Errorf("camIsClip(%q) = %v, want %v", c.path, got, c.wantClip)
		}
	}
}

func TestCamUniqueParentDirs(t *testing.T) {
	// Build inputs/expectations with filepath so the separators match the host OS
	// (camUniqueParentDirs uses filepath.Dir → "\a" on Windows, "/a" on Unix).
	in := []string{
		filepath.Join("/a", "1.jpg"),
		filepath.Join("/a", "2.jpg"),
		filepath.Join("/b", "3.jpg"),
		filepath.Join("/a", "4.jpg"),
	}
	got := camUniqueParentDirs(in)
	want := []string{filepath.Clean("/a"), filepath.Clean("/b")}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("dirs = %v, want %v", got, want)
	}
}

func TestCamFoldSystemPrompt(t *testing.T) {
	if got := camFoldSystemPrompt("SYS", "USER"); got != "SYS\n\nUSER" {
		t.Errorf("fold = %q", got)
	}
	if got := camFoldSystemPrompt("", "USER"); got != "USER" {
		t.Errorf("fold empty-sys = %q", got)
	}
	if got := camFoldSystemPrompt("SYS", ""); got != "SYS" {
		t.Errorf("fold empty-user = %q", got)
	}
}

func TestCamPerImageBudget(t *testing.T) {
	if got := camPerImageBudget(0); got != claudeMaxInlineMediaBytes {
		t.Errorf("budget(0) = %d", got)
	}
	if got := camPerImageBudget(2); got != claudeMaxInlineMediaBytes/2 {
		t.Errorf("budget(2) = %d", got)
	}
	// Large fleet hits the floor.
	if got := camPerImageBudget(100000); got != 64*1024 {
		t.Errorf("budget(100000) = %d, want floor 65536", got)
	}
}

// ─────────────────────── never-die analysis: classifier ───────────────────────

func TestCamClassifyAnalyzeErr(t *testing.T) {
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	cases := []struct {
		name string
		ctx  context.Context
		err  error
		want camAnalyzeErrClass
	}{
		{"agy_timeout", context.Background(), errors.New("agy analysis failed: agy timed out after 300s"), camErrRetryable},
		{"claude_timeout", context.Background(), errors.New("claude analysis failed: timed out after 180s"), camErrRetryable},
		{"produced_no_output", context.Background(), errors.New("agyj produced no output"), camErrRetryable},
		{"returned_no_text", context.Background(), errors.New("agy returned no text"), camErrRetryable},
		{"overloaded_529", context.Background(), errors.New("api error: 529 overloaded"), camErrRetryable},
		{"rate_limit_429", context.Background(), errors.New("http 429 too many requests"), camErrRetryable},
		{"conn_reset", context.Background(), errors.New("read tcp: connection reset by peer"), camErrRetryable},
		{"vision_unsupported", context.Background(), fmt.Errorf("%w: no images inlined", errCameraVisionUnsupported), camErrSkipAlias},
		{"analysis_backend", context.Background(), fmt.Errorf("%w: backend off", errCameraAnalysisBackend), camErrSkipAlias},
		{"cancelled_beats_retryable", cancelled, errors.New("timed out after 5s"), camErrCancelled},
		{"permanent_random", context.Background(), errors.New("bad request: invalid model alias"), camErrPermanent},
		{"nil_defensive", context.Background(), nil, camErrPermanent},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := camClassifyAnalyzeErr(c.ctx, c.err); got != c.want {
				t.Errorf("class = %d, want %d", got, c.want)
			}
		})
	}
}

func TestCamInvestigateFailoverAliases(t *testing.T) {
	if got := camInvestigateFailoverAliases(config{}); len(got) != 0 {
		t.Errorf("unset = %v, want empty", got)
	}
	got := camInvestigateFailoverAliases(config{CameraInvestigateFailoverCSV: " claude-sonnet-5 , , gemini-3.5-flash , "})
	if !reflect.DeepEqual(got, []string{"claude-sonnet-5", "gemini-3.5-flash"}) {
		t.Errorf("parsed = %v", got)
	}
	if got := camDedupAliases("primary", []string{"primary", "backup", "backup"}); !reflect.DeepEqual(got, []string{"primary", "backup"}) {
		t.Errorf("dedup = %v, want [primary backup]", got)
	}
	if got := camAnalyzeTimeout(config{}); got != 600*time.Second {
		t.Errorf("default analyze timeout = %v, want 600s", got)
	}
}

// ─────────────────────── never-die analysis: resilient ladder ───────────────────────

func TestCamResilientAnalyzeFailover(t *testing.T) {
	// (a) one transient failure then success on the SAME alias; assert the injected
	// analyze receives the timeout-raised cfg clone.
	t.Run("retry_same_alias", func(t *testing.T) {
		calls := 0
		var gotCfg config
		fake := func(ctx context.Context, cfg config, alias, sys, user string, images []string) (string, error) {
			calls++
			gotCfg = cfg
			if calls == 1 {
				return "", errors.New("agy timed out after 5s")
			}
			return "OK:" + alias, nil
		}
		out, attempts, err := camResilientAnalyzeWith(context.Background(), config{}, "primary", "s", "u", nil, fake)
		if err != nil {
			t.Fatalf("err = %v, want nil", err)
		}
		if out != "OK:primary" {
			t.Errorf("out = %q, want OK:primary", out)
		}
		if calls != 2 || len(attempts) != 2 {
			t.Fatalf("calls=%d attempts=%d, want 2/2", calls, len(attempts))
		}
		if attempts[0].Err == "" || attempts[1].Err != "" {
			t.Errorf("attempt errs = %q / %q, want (failed, ok)", attempts[0].Err, attempts[1].Err)
		}
		floor := 600 * time.Second
		if gotCfg.AgyTimeout != floor || gotCfg.AgyMediaTimeout != floor || gotCfg.ClaudeTimeout != floor {
			t.Errorf("raised clone = agy=%v media=%v claude=%v, want all %v", gotCfg.AgyTimeout, gotCfg.AgyMediaTimeout, gotCfg.ClaudeTimeout, floor)
		}
	})

	// (b) vision-unsupported on the primary jumps STRAIGHT to the failover alias —
	// no wasted same-alias retry.
	t.Run("skip_alias_to_failover", func(t *testing.T) {
		calls := 0
		fake := func(ctx context.Context, cfg config, alias, sys, user string, images []string) (string, error) {
			calls++
			if alias == "primary" {
				return "", fmt.Errorf("%w: codex has no vision", errCameraVisionUnsupported)
			}
			return "OK:" + alias, nil
		}
		cfg := config{CameraInvestigateFailoverCSV: "backup"}
		out, attempts, err := camResilientAnalyzeWith(context.Background(), cfg, "primary", "s", "u", nil, fake)
		if err != nil {
			t.Fatalf("err = %v, want nil", err)
		}
		if out != "OK:backup" || calls != 2 {
			t.Errorf("out=%q calls=%d, want OK:backup / 2 (primary tried once, no retry)", out, calls)
		}
		if len(attempts) != 2 || attempts[0].Alias != "primary" || attempts[1].Alias != "backup" {
			t.Errorf("attempts = %+v", attempts)
		}
	})

	// (c) every rung fails (retryable) → wrapped error naming the attempt/alias counts.
	t.Run("all_fail_wrapped", func(t *testing.T) {
		fake := func(ctx context.Context, cfg config, alias, sys, user string, images []string) (string, error) {
			return "", errors.New("service unavailable")
		}
		cfg := config{CameraInvestigateFailoverCSV: "backup"}
		out, attempts, err := camResilientAnalyzeWith(context.Background(), cfg, "primary", "s", "u", nil, fake)
		if err == nil {
			t.Fatalf("err = nil, want wrapped failure")
		}
		if out != "" {
			t.Errorf("out = %q, want empty", out)
		}
		// 2 aliases × 2 attempts each.
		if len(attempts) != 4 {
			t.Errorf("attempts = %d, want 4", len(attempts))
		}
		if !strings.Contains(err.Error(), "4 attempt(s)") || !strings.Contains(err.Error(), "2 alias(es)") {
			t.Errorf("err = %q, want it to name 4 attempts across 2 aliases", err.Error())
		}
	})

	// (d) a dead run context aborts the ladder immediately (every retry would fail
	// the same way) — one call, no backoff.
	t.Run("cancelled_aborts", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		calls := 0
		fake := func(ctx context.Context, cfg config, alias, sys, user string, images []string) (string, error) {
			calls++
			return "", errors.New("timed out after 5s")
		}
		cfg := config{CameraInvestigateFailoverCSV: "backup"}
		if _, _, err := camResilientAnalyzeWith(ctx, cfg, "primary", "s", "u", nil, fake); err == nil {
			t.Fatalf("err = nil, want the abort error")
		}
		if calls != 1 {
			t.Errorf("calls = %d, want 1 (aborted after first cancelled classification)", calls)
		}
	})
}
