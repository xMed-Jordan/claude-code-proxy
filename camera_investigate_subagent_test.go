package main

// camera_investigate_subagent_test.go (WS3) — the sub-agent delegation surface that
// needs no DVR or model backend: tolerant subtask JSON parsing, the tools_allowed
// intersection (and the recursion refusal baked into the tool ceiling), the per-child
// media split math, the ask_operator→answer coercion, the child-evidence→parent-citable
// chain, verdict aggregation, the prompt gate, and camToolDelegate's early refusals.

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestParseInvestigateActionDelegate confirms a delegate action decodes its subtasks
// tolerantly: known fields land, unknown fields are ignored, and a missing
// tools_allowed leaves a nil slice (→ the full sub-agent ceiling downstream).
func TestParseInvestigateActionDelegate(t *testing.T) {
	raw := `Sure, here you go:
	{"thought":"fan out","action":{"type":"call_tool","tool":"delegate","args":{"subtasks":[
	  {"question":"who entered the lobby","camera_ids":["cam1","cam2"],"from":"2026-07-01T09:00:00Z","to":"2026-07-01T10:00:00Z","tools_allowed":["contact_sheet","past_frames"],"extra":"ignored"},
	  {"question":"back door activity","camera_ids":["cam9"],"from":"2026-07-01T09:00:00Z","to":"2026-07-01T09:30:00Z"}
	]}}}`
	act, err := parseInvestigateAction(raw)
	if err != nil {
		t.Fatalf("parseInvestigateAction: %v", err)
	}
	if act.Action.Type != "call_tool" || act.Action.Tool != "delegate" {
		t.Fatalf("action = %q tool %q, want call_tool/delegate", act.Action.Type, act.Action.Tool)
	}
	subs := act.Action.Args.Subtasks
	if len(subs) != 2 {
		t.Fatalf("subtasks = %d, want 2", len(subs))
	}
	if subs[0].Question != "who entered the lobby" || len(subs[0].CameraIDs) != 2 || subs[0].CameraIDs[1] != "cam2" {
		t.Errorf("subtask[0] decoded wrong: %+v", subs[0])
	}
	if len(subs[0].Tools) != 2 || subs[0].Tools[0] != "contact_sheet" {
		t.Errorf("subtask[0].tools_allowed = %v, want [contact_sheet past_frames]", subs[0].Tools)
	}
	if subs[1].Tools != nil {
		t.Errorf("subtask[1].tools_allowed = %v, want nil (defaults to full ceiling)", subs[1].Tools)
	}
	if subs[1].From != "2026-07-01T09:00:00Z" {
		t.Errorf("subtask[1].from = %q, want the RFC3339 window start", subs[1].From)
	}
}

// TestCamSubagentEffectiveTools covers the intersection contract: the ceiling drops
// avatar tools with no avatars, tools_allowed only ever NARROWS the ceiling, an
// all-invalid request falls back to the ceiling, and delegate/past_clip/etc. can never
// enter the set (recursion + lead-only tools stay out).
func TestCamSubagentEffectiveTools(t *testing.T) {
	cases := []struct {
		name       string
		requested  []string
		hasAvatars bool
		wantHas    []string
		wantNot    []string
	}{
		{
			name: "full ceiling with avatars", requested: nil, hasAvatars: true,
			wantHas: []string{"roster", "snapshot", "past_frames", "contact_sheet", "motion_search", "avatars", "avatar_info", "avatar_check", "avatar_find", "annotate"},
			wantNot: []string{"delegate", "past_clip", "evidence_export", "playbook", "call_api", "mosaic"},
		},
		{
			name: "no avatars drops avatar tools", requested: nil, hasAvatars: false,
			wantHas: []string{"roster", "snapshot", "past_frames", "contact_sheet", "motion_search", "annotate"},
			wantNot: []string{"avatars", "avatar_info", "avatar_check", "avatar_find", "delegate"},
		},
		{
			name: "tools_allowed narrows", requested: []string{"snapshot", "motion_search"}, hasAvatars: true,
			wantHas: []string{"snapshot", "motion_search"},
			wantNot: []string{"contact_sheet", "avatars", "annotate", "delegate"},
		},
		{
			name: "never widens past the ceiling", requested: []string{"snapshot", "past_clip", "delegate", "evidence_export"}, hasAvatars: true,
			wantHas: []string{"snapshot"},
			wantNot: []string{"past_clip", "delegate", "evidence_export"},
		},
		{
			name: "all-invalid request falls back to ceiling", requested: []string{"past_clip", "delegate"}, hasAvatars: true,
			wantHas: []string{"roster", "snapshot", "avatar_find"},
			wantNot: []string{"past_clip", "delegate"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ordered, set := camSubagentEffectiveTools(tc.requested, tc.hasAvatars)
			// ordered and set must agree exactly.
			if len(ordered) != len(set) {
				t.Fatalf("ordered=%v vs set=%v size mismatch", ordered, set)
			}
			for _, tool := range ordered {
				if !set[tool] {
					t.Errorf("ordered lists %q but set omits it", tool)
				}
			}
			for _, tool := range tc.wantHas {
				if !set[tool] {
					t.Errorf("want %q in effective set, missing (%v)", tool, ordered)
				}
			}
			for _, tool := range tc.wantNot {
				if set[tool] {
					t.Errorf("%q must NOT be in the effective set (%v)", tool, ordered)
				}
			}
		})
	}
}

// TestCamSubagentAllowedToolsExclusions pins the recursion refusal and the lead-only
// exclusions at the ceiling itself, independent of any subtask request.
func TestCamSubagentAllowedToolsExclusions(t *testing.T) {
	for _, forbidden := range []string{"delegate", "past_clip", "evidence_export", "playbook", "call_api", "mosaic"} {
		if camSubagentAllowedTools[forbidden] {
			t.Errorf("%q must never be in the sub-agent tool ceiling", forbidden)
		}
	}
	for _, allowed := range []string{"roster", "snapshot", "past_frames", "contact_sheet", "motion_search", "avatar_check", "annotate"} {
		if !camSubagentAllowedTools[allowed] {
			t.Errorf("%q should be in the sub-agent tool ceiling", allowed)
		}
	}
}

// TestCamSubagentPerChildMedia checks the split formula min(subMax, max(4, left/n)).
func TestCamSubagentPerChildMedia(t *testing.T) {
	cases := []struct{ subMax, mediaLeft, children, want int }{
		{10, 30, 3, 10}, // even split hits the ceiling
		{10, 6, 3, 4},   // tiny pool → the floor of 4
		{10, 100, 2, 10},
		{6, 40, 2, 6},  // ceiling below the even split wins
		{3, 30, 3, 3},  // low ceiling wins
		{10, 30, 0, 0}, // no children
	}
	for _, tc := range cases {
		if got := camSubagentPerChildMedia(tc.subMax, tc.mediaLeft, tc.children); got != tc.want {
			t.Errorf("camSubagentPerChildMedia(%d,%d,%d) = %d, want %d", tc.subMax, tc.mediaLeft, tc.children, got, tc.want)
		}
	}
}

// TestCamSubagentCoerceAnswer verifies a sub-investigator's ask_operator turn is
// rewritten into an answer (the question text becomes the verdict), while other
// action types pass through untouched.
func TestCamSubagentCoerceAnswer(t *testing.T) {
	ask := investigateAction{Thought: "I need the operator", Action: investigateCommand{Type: "ask_operator", Question: "which entrance?"}}
	got := camSubagentCoerceAnswer(ask)
	if got.Action.Type != "answer" {
		t.Fatalf("coerced type = %q, want answer", got.Action.Type)
	}
	if got.Action.Answer != "which entrance?" {
		t.Errorf("coerced answer = %q, want the question text", got.Action.Answer)
	}

	// A blank question falls back to the thought.
	blank := investigateAction{Thought: "no idea", Action: investigateCommand{Type: "ask_operator"}}
	if got := camSubagentCoerceAnswer(blank); got.Action.Answer != "no idea" {
		t.Errorf("blank-question coercion answer = %q, want the thought", got.Action.Answer)
	}

	// A real answer is untouched.
	ans := investigateAction{Action: investigateCommand{Type: "answer", Answer: "done"}}
	if got := camSubagentCoerceAnswer(ans); got.Action.Type != "answer" || got.Action.Answer != "done" {
		t.Errorf("answer coercion mutated a real answer: %+v", got.Action)
	}
}

// TestCamSubagentEvidenceCitableChain is the whole point of delegation: a child's kept
// evidence, once aggregated into the delegate tool row's media_json, is citable by the
// LEAD through the same camCollectMintedMedia/camFilterEvidence path every tool uses —
// while a hallucinated url the lead invents is still dropped.
func TestCamSubagentEvidenceCitableChain(t *testing.T) {
	childURL := "https://proxy.example/camera/media/child_tok_a"
	verdicts := []camSubagentVerdict{
		{ChildID: "inv_c1", Question: "lobby", CamIDs: []string{"cam1"}, Window: "09:00-10:00", Status: "answered",
			Answer: "one person entered", Evidence: []evidenceItem{{MediaURL: childURL, Caption: "entry frame"}}},
	}
	res := camAggregateSubagentVerdicts(verdicts, nil) // nil notifier must be safe
	if len(res.Media) != 1 || res.Media[0].MediaURL != childURL {
		t.Fatalf("aggregated media = %+v, want the child's one evidence item", res.Media)
	}
	if !strings.Contains(res.Summary, "SUBTASK 1") || !strings.Contains(res.Summary, "one person entered") {
		t.Errorf("summary missing the subtask verdict block: %q", res.Summary)
	}

	// The delegate tool row carries that media_json; the lead's citation validation
	// then keeps the real child url and drops an invented one.
	msgs := []camInvestigationMessage{
		{Role: "operator", Content: "who came in?"},
		{Role: "tool", ToolName: "delegate", MediaJSON: mustJSON(res.Media)},
	}
	set, byToken := camCollectMintedMedia(msgs)
	if !set[childURL] {
		t.Fatalf("child evidence url %q not collected as minted media", childURL)
	}
	cited := []evidenceItem{
		{MediaURL: childURL, Caption: "the person"},
		{MediaURL: "https://evil.example/camera/media/deadbeef", Caption: "fabricated"},
	}
	kept, dropped := camFilterEvidence(cited, set, byToken)
	if len(kept) != 1 || kept[0].MediaURL != childURL {
		t.Errorf("kept = %+v, want only the real child url", kept)
	}
	if dropped != 1 {
		t.Errorf("dropped = %d, want 1 (the fabricated url)", dropped)
	}
}

// TestCamAggregateSubagentVerdictsCaps confirms the per-child evidence cap holds and
// the summary reflects every subtask's status.
func TestCamAggregateSubagentVerdictsCaps(t *testing.T) {
	var many []evidenceItem
	for i := 0; i < 9; i++ {
		many = append(many, evidenceItem{MediaURL: "https://h/camera/media/e" + string(rune('a'+i)), Caption: "f"})
	}
	verdicts := []camSubagentVerdict{
		{ChildID: "c1", CamIDs: []string{"cam1"}, Window: "w1", Status: "answered", Answer: "a1", Evidence: many},
		{ChildID: "c2", CamIDs: []string{"cam2"}, Window: "w2", Status: "exhausted", Answer: "", Evidence: nil},
	}
	res := camAggregateSubagentVerdicts(verdicts, nil)
	if len(res.Media) != camSubagentEvidencePerChild {
		t.Errorf("media = %d, want the per-child cap %d", len(res.Media), camSubagentEvidencePerChild)
	}
	if !strings.Contains(res.Summary, "ANSWERED") || !strings.Contains(res.Summary, "EXHAUSTED") {
		t.Errorf("summary missing a status label: %q", res.Summary)
	}
	if !strings.Contains(res.Summary, "(no conclusion reached)") {
		t.Errorf("empty-answer child should read as a no-conclusion block: %q", res.Summary)
	}
}

// TestCamInvestigatePromptDelegateGating locks the runtime gate: the delegate tool
// surfaces in the lead prompt only when subagentMax > 0.
func TestCamInvestigatePromptDelegateGating(t *testing.T) {
	active := []camera{{ID: "cam1", Name: "Front Door", Enabled: true}}
	off := camInvestigateSystemPrompt(camSite{Name: "Clinic"}, nil, active, nil, nil, nil, time.Now(), time.Time{}, false, "UTC", 0)
	if strings.Contains(off, "delegate") {
		t.Error("delegate must not appear when subagentMax == 0")
	}
	on := camInvestigateSystemPrompt(camSite{Name: "Clinic"}, nil, active, nil, nil, nil, time.Now(), time.Time{}, false, "UTC", 3)
	if !strings.Contains(on, "- delegate:") {
		t.Error("delegate tool line must appear when subagentMax > 0")
	}
	if !strings.Contains(on, "(max 3)") {
		t.Error("delegate tool line must carry the configured cap")
	}
	if !strings.Contains(on, "Fan out with delegate") {
		t.Error("the fan-out CORE PRINCIPLE must appear when delegation is enabled")
	}
	if !strings.Contains(on, `annotate|evidence_export|delegate`) {
		t.Error("the OUTPUT tool enum must include delegate when enabled")
	}
}

// TestCamToolDelegateSubagentAliasOverride proves the PROXY_CAMERA_SUBAGENT_ALIAS
// reroute: when set, EVERY child analysis call and the child's persisted row use the
// override alias while the parent alias passed into delegate is untouched; when unset,
// children run on the parent alias exactly as before (byte-identical regression guard).
// It scripts the child model to answer in one turn and records the alias each child
// analysis was invoked with via the camInvestigateAnalyzeFn seam.
func TestCamToolDelegateSubagentAliasOverride(t *testing.T) {
	const parentAlias = "gemini-3.5-flash-medium"
	cases := []struct {
		name      string
		subagent  string
		wantChild string
	}{
		{name: "override reroutes children", subagent: "claude-sonnet-5", wantChild: "claude-sonnet-5"},
		{name: "unset keeps the parent alias", subagent: "", wantChild: parentAlias},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := openCamTestDB(t)
			if err := migrateCameraDB(db); err != nil {
				t.Fatalf("migrateCameraDB: %v", err)
			}
			siteID, err := insertCameraSite(db, camSite{Name: "Clinic"})
			if err != nil {
				t.Fatalf("insertCameraSite: %v", err)
			}
			site := camSite{ID: siteID, Name: "Clinic"}
			inv := camInvestigation{ID: "inv_lead", SiteID: siteID}

			// Script the child analysis: record the alias it ran under and answer at
			// once so the child completes in one turn needing no tool/DVR backend.
			orig := camInvestigateAnalyzeFn
			defer func() { camInvestigateAnalyzeFn = orig }()
			var (
				mu   sync.Mutex
				seen []string
			)
			camInvestigateAnalyzeFn = func(ctx context.Context, c config, alias, sys, user string, images []string) (string, []camAnalyzeAttempt, error) {
				mu.Lock()
				seen = append(seen, alias)
				mu.Unlock()
				return `{"thought":"done","action":{"type":"answer","answer":"nothing of note"}}`, nil, nil
			}

			cfg := config{CameraSubagentMax: 4, CameraSubagentAlias: tc.subagent}
			cam := camera{ID: "cam1", Name: "Front Door", Enabled: true}
			camByID := map[string]camera{"cam1": cam}
			allowed := map[string]bool{"cam1": true}
			sub := investigateSubtask{Question: "who entered", CameraIDs: []string{"cam1"}, From: "2026-07-01T09:00:00Z", To: "2026-07-01T10:00:00Z"}

			res := camToolDelegate(context.Background(), cfg, db, nil, site, parentAlias, inv,
				investigateArgs{Subtasks: []investigateSubtask{sub}}, camByID, nil, allowed, []camera{cam}, t.TempDir(), 30, nil)
			if !strings.Contains(res.Summary, "SUBTASK 1") {
				t.Fatalf("delegate did not run a child: %q", res.Summary)
			}

			// Every recorded child analysis alias must be the expected one.
			mu.Lock()
			got := append([]string(nil), seen...)
			mu.Unlock()
			if len(got) == 0 {
				t.Fatalf("no child analysis call was recorded")
			}
			for _, a := range got {
				if a != tc.wantChild {
					t.Errorf("child analysis alias = %q, want %q", a, tc.wantChild)
				}
			}

			// The persisted child row's stored alias reflects where it actually ran.
			children, err := listCameraInvestigationChildren(db, inv.ID)
			if err != nil {
				t.Fatalf("listCameraInvestigationChildren: %v", err)
			}
			if len(children) != 1 {
				t.Fatalf("children = %d, want exactly 1", len(children))
			}
			if children[0].Alias != tc.wantChild {
				t.Errorf("persisted child row alias = %q, want %q", children[0].Alias, tc.wantChild)
			}
		})
	}
}

// TestCamToolDelegateRefusals covers the early-return paths that need no model backend:
// disabled deployment, too little budget remaining, and no valid subtasks.
func TestCamToolDelegateRefusals(t *testing.T) {
	db := openCamTestDB(t)
	if err := migrateCameraDB(db); err != nil {
		t.Fatalf("migrateCameraDB: %v", err)
	}
	siteID, err := insertCameraSite(db, camSite{Name: "Clinic"})
	if err != nil {
		t.Fatalf("insertCameraSite: %v", err)
	}
	site := camSite{ID: siteID, Name: "Clinic"}
	inv := camInvestigation{ID: "inv_lead", SiteID: siteID}
	sub := investigateSubtask{Question: "q", CameraIDs: []string{"cam1"}, From: "2026-07-01T09:00:00Z", To: "2026-07-01T10:00:00Z"}

	// Disabled: subagentMax resolves to 0.
	res := camToolDelegate(context.Background(), config{}, db, nil, site, "alias", inv,
		investigateArgs{Subtasks: []investigateSubtask{sub}}, nil, nil, nil, nil, t.TempDir(), 30, nil)
	if res.Fetches != 0 || !strings.Contains(res.Summary, "disabled") {
		t.Errorf("disabled delegate = %+v, want a 'disabled' refusal with 0 fetches", res)
	}

	// Too little budget: enabled, but under the 90s remaining floor.
	cfgOn := config{CameraSubagentMax: 4}
	near, cancel := context.WithDeadline(context.Background(), time.Now().Add(10*time.Second))
	defer cancel()
	res = camToolDelegate(near, cfgOn, db, nil, site, "alias", inv,
		investigateArgs{Subtasks: []investigateSubtask{sub}}, nil, nil, nil, nil, t.TempDir(), 30, nil)
	if res.Fetches != 0 || !strings.Contains(res.Summary, "not enough time") {
		t.Errorf("low-budget delegate = %+v, want a time-budget refusal", res)
	}

	// No valid subtasks: enabled, ample budget, but the subtask names no allowed camera.
	allowed := map[string]bool{} // empty → camFilterAllowed drops every requested id
	res = camToolDelegate(context.Background(), cfgOn, db, nil, site, "alias", inv,
		investigateArgs{Subtasks: []investigateSubtask{sub}}, nil, nil, allowed, nil, t.TempDir(), 30, nil)
	if res.Fetches != 0 || !strings.Contains(res.Summary, "no valid subtasks") {
		t.Errorf("no-valid-subtasks delegate = %+v, want a validation refusal", res)
	}
}
