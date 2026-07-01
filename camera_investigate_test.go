package main

// camera_investigate_test.go — table-driven tests for the ask-AI investigation
// feature's pure (network-free) logic:
//
//   - camBuildRoster: roster text formatting.
//   - parseCameraSelection: the roster-first selector's JSON reply parsing
//     (valid / fenced / garbage, dropping ids not in the roster). This is the
//     helper camSelectCameras delegates to (see camera_investigate.go) — split
//     out for exactly this kind of network-free unit test, mirroring
//     parseDecision's separation from camAnalyzeDecision in cameraorch.go.
//   - parseInvestigateAction: the per-turn action parser (call_tool /
//     ask_operator / answer) including its self-repair normalization for
//     garbled action types / tool names.
//   - migrateCameraDB: idempotency of the camera_investigations /
//     camera_investigation_messages tables specifically (openCamTestDB is
//     reused from camera_store_test.go).

import (
	"reflect"
	"testing"
)

// ─────────────────────── camBuildRoster ───────────────────────

func TestCamBuildRoster(t *testing.T) {
	cases := []struct {
		name string
		cams []camera
		want string
	}{
		{
			name: "full_fields",
			cams: []camera{
				{ID: "cam1", Name: "Front Door", Area: "Lobby", AIDescription: "wide entrance view", Enabled: true},
			},
			want: "- cam1 — Front Door [Lobby]: wide entrance view",
		},
		{
			name: "blank_optional_fields_omitted",
			cams: []camera{
				{ID: "cam2", Enabled: true},
			},
			want: "- cam2",
		},
		{
			name: "whitespace_only_fields_treated_as_blank",
			cams: []camera{
				{ID: "cam3", Name: "  ", Area: "\t", AIDescription: " ", Enabled: true},
			},
			want: "- cam3",
		},
		{
			name: "disabled_cameras_skipped",
			cams: []camera{
				{ID: "cam1", Name: "A", Enabled: true},
				{ID: "cam2", Name: "B", Enabled: false},
			},
			want: "- cam1 — A",
		},
		{
			name: "multiple_enabled_cameras_one_line_each",
			cams: []camera{
				{ID: "cam1", Name: "Front", Area: "Lobby", Enabled: true},
				{ID: "cam2", Name: "Back", AIDescription: "loading dock", Enabled: true},
			},
			want: "- cam1 — Front [Lobby]\n- cam2 — Back: loading dock",
		},
		{
			name: "no_enabled_cameras",
			cams: []camera{
				{ID: "cam1", Name: "A", Enabled: false},
			},
			want: "(no enabled cameras)",
		},
		{
			name: "empty_input",
			cams: nil,
			want: "(no enabled cameras)",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := camBuildRoster(c.cams); got != c.want {
				t.Errorf("camBuildRoster() = %q, want %q", got, c.want)
			}
		})
	}
}

// ─────────────────────── parseCameraSelection ───────────────────────

func TestParseCameraSelectionValid(t *testing.T) {
	allowed := map[string]bool{"cam1": true, "cam2": true, "cam3": true}
	raw := `{"camera_ids":["cam1","cam3"],"reason":"reception and back door"}`
	ids, requested, err := parseCameraSelection(raw, allowed)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if requested != 2 {
		t.Errorf("requested = %d, want 2", requested)
	}
	if !reflect.DeepEqual(ids, []string{"cam1", "cam3"}) {
		t.Errorf("ids = %v, want [cam1 cam3]", ids)
	}
}

func TestParseCameraSelectionFencedAndProse(t *testing.T) {
	allowed := map[string]bool{"cam1": true}
	raw := "Sure, here you go:\n```json\n" + `{"camera_ids":["cam1"],"reason":"only relevant camera"}` + "\n```\nLet me know if you need more."
	ids, requested, err := parseCameraSelection(raw, allowed)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if requested != 1 || !reflect.DeepEqual(ids, []string{"cam1"}) {
		t.Errorf("ids=%v requested=%d, want [cam1]/1", ids, requested)
	}
}

func TestParseCameraSelectionDropsUnknownIDsDedupesAndPreservesOrder(t *testing.T) {
	allowed := map[string]bool{"cam1": true, "cam2": true, "cam3": true}
	raw := `{"camera_ids":["ghost","cam3","cam1","cam3"," cam2 ","bogus","cam1"],"reason":"x"}`
	ids, requested, err := parseCameraSelection(raw, allowed)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if requested != 7 {
		t.Errorf("requested = %d, want 7 (raw count before filtering)", requested)
	}
	want := []string{"cam3", "cam1", "cam2"}
	if !reflect.DeepEqual(ids, want) {
		t.Errorf("ids = %v, want %v", ids, want)
	}
}

func TestParseCameraSelectionEmptySelectionIsValid(t *testing.T) {
	allowed := map[string]bool{"cam1": true}
	ids, requested, err := parseCameraSelection(`{"camera_ids":[],"reason":"nothing relevant"}`, allowed)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if requested != 0 || len(ids) != 0 {
		t.Errorf("ids=%v requested=%d, want empty/0", ids, requested)
	}
}

func TestParseCameraSelectionNilAllowedDropsEverything(t *testing.T) {
	// allowed with no entries (e.g. a site with zero enabled cameras) must drop
	// every requested id rather than panicking on a nil/empty map lookup.
	ids, requested, err := parseCameraSelection(`{"camera_ids":["cam1","cam2"]}`, map[string]bool{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if requested != 2 {
		t.Errorf("requested = %d, want 2", requested)
	}
	if len(ids) != 0 {
		t.Errorf("ids = %v, want empty (nothing allowed)", ids)
	}
}

func TestParseCameraSelectionErrors(t *testing.T) {
	cases := []struct {
		name string
		raw  string
	}{
		{"garbage", "I couldn't find anything relevant, sorry."},
		{"empty", ""},
		{"unterminated", `{"camera_ids":["cam1"]`},
		{"only_prose_braces", "the {quick} {brown} fox"},
	}
	allowed := map[string]bool{"cam1": true}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, _, err := parseCameraSelection(c.raw, allowed); err == nil {
				t.Fatalf("expected error, got nil")
			}
		})
	}
}

// ─────────────────────── parseInvestigateAction ───────────────────────

func TestParseInvestigateActionCallTool(t *testing.T) {
	raw := `{"thought":"need to see reception now","action":{"type":"call_tool","tool":"snapshot",` +
		`"args":{"camera_ids":["cam1","cam2"],"quality":"sub"}}}`
	act, err := parseInvestigateAction(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if act.Thought != "need to see reception now" {
		t.Errorf("thought = %q", act.Thought)
	}
	if act.Action.Type != "call_tool" || act.Action.Tool != "snapshot" {
		t.Errorf("action = %+v", act.Action)
	}
	if !reflect.DeepEqual(act.Action.Args.CameraIDs, []string{"cam1", "cam2"}) {
		t.Errorf("camera_ids = %v", act.Action.Args.CameraIDs)
	}
	if act.Action.Args.Quality != "sub" {
		t.Errorf("quality = %q", act.Action.Args.Quality)
	}
}

func TestParseInvestigateActionAskOperator(t *testing.T) {
	raw := `{"thought":"unclear which entrance","action":{"type":"ask_operator","question":"which entrance do you mean?"}}`
	act, err := parseInvestigateAction(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if act.Action.Type != "ask_operator" {
		t.Errorf("type = %q, want ask_operator", act.Action.Type)
	}
	if act.Action.Question != "which entrance do you mean?" {
		t.Errorf("question = %q", act.Action.Question)
	}
}

func TestParseInvestigateActionAnswer(t *testing.T) {
	raw := `{"thought":"reviewed the footage","action":{"type":"answer","answer":"a courier dropped a package at 15:02.",` +
		`"evidence":[{"media_url":"/camera/media/tok1","caption":"courier at door"}]}}`
	act, err := parseInvestigateAction(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if act.Action.Type != "answer" {
		t.Errorf("type = %q, want answer", act.Action.Type)
	}
	if act.Action.Answer != "a courier dropped a package at 15:02." {
		t.Errorf("answer = %q", act.Action.Answer)
	}
	if len(act.Action.Evidence) != 1 || act.Action.Evidence[0].MediaURL != "/camera/media/tok1" {
		t.Errorf("evidence = %+v", act.Action.Evidence)
	}
}

func TestParseInvestigateActionFencedAndProse(t *testing.T) {
	raw := "Here is my next step.\n```json\n" +
		`{"thought":"check the roster first","action":{"type":"call_tool","tool":"roster"}}` +
		"\n```\nLet me know."
	act, err := parseInvestigateAction(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if act.Action.Type != "call_tool" || act.Action.Tool != "roster" {
		t.Errorf("action = %+v", act.Action)
	}
}

// TestParseInvestigateActionRepairsUnknownType covers the parser's self-healing
// ("repair") behavior for a garbled action.type: it falls back to "call_tool"
// with tool "roster" (cheap, no device I/O) instead of erroring or ending the
// investigation.
func TestParseInvestigateActionRepairsUnknownType(t *testing.T) {
	raw := `{"thought":"hmm","action":{"type":"do_something_weird"}}`
	act, err := parseInvestigateAction(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if act.Action.Type != "call_tool" {
		t.Errorf("type = %q, want call_tool (repaired)", act.Action.Type)
	}
	if act.Action.Tool != "roster" {
		t.Errorf("tool = %q, want roster (repaired default)", act.Action.Tool)
	}
}

// TestParseInvestigateActionRepairsUnknownTool covers a valid call_tool whose
// tool name isn't in investigateToolNames: it must fall back to "roster" rather
// than propagate an unexecutable tool name.
func TestParseInvestigateActionRepairsUnknownTool(t *testing.T) {
	raw := `{"thought":"x","action":{"type":"call_tool","tool":"time_travel","args":{"camera_ids":["cam1"]}}}`
	act, err := parseInvestigateAction(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if act.Action.Tool != "roster" {
		t.Errorf("tool = %q, want roster (repaired)", act.Action.Tool)
	}
}

// TestParseInvestigateActionToolNameCaseInsensitive confirms an uppercase/mixed
// case tool name from the model is still recognized (normalized to lowercase)
// rather than triggering the "unknown tool" repair.
func TestParseInvestigateActionToolNameCaseInsensitive(t *testing.T) {
	raw := `{"thought":"x","action":{"type":"call_tool","tool":" Snapshot ","args":{"camera_ids":["cam1"]}}}`
	act, err := parseInvestigateAction(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if act.Action.Tool != "snapshot" {
		t.Errorf("tool = %q, want snapshot (case/space normalized)", act.Action.Tool)
	}
}

// TestParseInvestigateActionAnswerFallsBackToThought covers the "answer" repair:
// a blank action.answer falls back to the turn's thought so the transcript is
// never left with a blank final message.
func TestParseInvestigateActionAnswerFallsBackToThought(t *testing.T) {
	raw := `{"thought":"nothing suspicious found in the footage","action":{"type":"answer"}}`
	act, err := parseInvestigateAction(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if act.Action.Answer != "nothing suspicious found in the footage" {
		t.Errorf("answer = %q, want fallback to thought", act.Action.Answer)
	}
}

// TestParseInvestigateActionAnswerFallsBackToGenericNotice covers the case where
// BOTH answer and thought are blank: a generic non-empty notice must be used so
// downstream consumers never see an empty final message.
func TestParseInvestigateActionAnswerFallsBackToGenericNotice(t *testing.T) {
	raw := `{"action":{"type":"answer"}}`
	act, err := parseInvestigateAction(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if act.Action.Answer == "" {
		t.Errorf("answer = %q, want a non-empty generic fallback", act.Action.Answer)
	}
}

func TestParseInvestigateActionErrors(t *testing.T) {
	cases := []struct {
		name string
		raw  string
	}{
		{"no_json", "I could not decide what to do next."},
		{"empty", ""},
		{"unterminated", `{"thought":"x","action":{"type":"answer"`},
		{"only_prose_braces", "the {quick} {brown} fox"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := parseInvestigateAction(c.raw); err == nil {
				t.Fatalf("expected error, got nil")
			}
		})
	}
}

// ─────────────────────── camCountInvestigateProgress ───────────────────────

// TestCamCountInvestigateProgressFetchUnits confirms the media budget is
// reconstructed from each tool row's persisted device-fetch count (the same unit
// the live loop spends: mediaUsed += tr.Fetches), NOT from the number of persisted
// media artifacts — which diverges (past_frames: 1 fetch -> many frames; mosaic:
// many fetches -> 1 composite) and used to silently exhaust the budget on resume.
func TestCamCountInvestigateProgressFetchUnits(t *testing.T) {
	msgs := []camInvestigationMessage{
		{Role: "operator", Content: "what happened at reception?"},
		{Role: "ai", ToolName: "past_frames"},
		{Role: "tool", ToolName: "past_frames", Fetches: 1, MediaJSON: mustJSON([]evidenceItem{{MediaURL: "u1"}, {MediaURL: "u2"}, {MediaURL: "u3"}, {MediaURL: "u4"}, {MediaURL: "u5"}, {MediaURL: "u6"}})},
		{Role: "ai", ToolName: "mosaic"},
		{Role: "tool", ToolName: "mosaic", Fetches: 4, MediaJSON: mustJSON([]evidenceItem{{MediaURL: "m1"}})},
	}
	turns, mediaUsed := camCountInvestigateProgress(msgs)
	if turns != 2 {
		t.Errorf("turns = %d, want 2", turns)
	}
	if mediaUsed != 5 { // 1 (past_frames clip fetch) + 4 (mosaic snapshot fetches), NOT 6+1 artifacts
		t.Errorf("mediaUsed = %d, want 5 (device fetches, not 7 persisted artifacts)", mediaUsed)
	}
}

// TestCamCountInvestigateProgressResetsOnFreshFollowUp confirms a follow-up sent
// after the run already terminated (a clean answer, or a bound-terminated system
// note) begins a FRESH run with a fresh budget — so it isn't instantly re-tripped
// by the prior run's cumulative turn/media counts.
func TestCamCountInvestigateProgressResetsOnFreshFollowUp(t *testing.T) {
	msgs := []camInvestigationMessage{
		{Role: "operator", Content: "q1"},
		{Role: "ai", ToolName: "snapshot"},
		{Role: "tool", ToolName: "snapshot", Fetches: 3},
		{Role: "ai", ToolName: "answer"},
		{Role: "operator", Content: "follow-up"}, // preceding ai is "answer", not ask_operator -> reset
		{Role: "ai", ToolName: "snapshot"},
		{Role: "tool", ToolName: "snapshot", Fetches: 2},
	}
	turns, mediaUsed := camCountInvestigateProgress(msgs)
	if turns != 1 || mediaUsed != 2 {
		t.Errorf("turns=%d mediaUsed=%d, want 1/2 (fresh run after answer)", turns, mediaUsed)
	}

	// A bound-terminated run ends with a call_tool ai row + a system note, then the
	// operator's follow-up — that must reset too (the finding's core bug).
	bound := []camInvestigationMessage{
		{Role: "operator", Content: "q1"},
		{Role: "ai", ToolName: "past_frames"},
		{Role: "tool", ToolName: "past_frames", Fetches: 12},
		{Role: "system", Content: "Investigation stopped: reached the maximum number of turns."},
		{Role: "operator", Content: "follow-up after exhaustion"},
		{Role: "ai", ToolName: "snapshot"},
	}
	if turns, mediaUsed := camCountInvestigateProgress(bound); turns != 1 || mediaUsed != 0 {
		t.Errorf("post-bound follow-up: turns=%d mediaUsed=%d, want 1/0 (fresh budget)", turns, mediaUsed)
	}
}

// TestCamCountInvestigateProgressPreservesBudgetAcrossAskOperator confirms the
// security invariant: resuming an ask_operator PAUSE carries the cumulative budget
// over, so a model can never reset its turn/media budget by looping on ask_operator.
func TestCamCountInvestigateProgressPreservesBudgetAcrossAskOperator(t *testing.T) {
	msgs := []camInvestigationMessage{
		{Role: "operator", Content: "q1"},
		{Role: "ai", ToolName: "past_frames"},
		{Role: "tool", ToolName: "past_frames", Fetches: 10},
		{Role: "ai", ToolName: "ask_operator"},
		{Role: "operator", Content: "the reception desk"}, // answers the pause -> NO reset
		{Role: "ai", ToolName: "snapshot"},
		{Role: "tool", ToolName: "snapshot", Fetches: 5},
	}
	turns, mediaUsed := camCountInvestigateProgress(msgs)
	if turns != 3 || mediaUsed != 15 {
		t.Errorf("turns=%d mediaUsed=%d, want 3/15 (cumulative across ask_operator resume)", turns, mediaUsed)
	}
}

// ─────────────────────── evidence validation ───────────────────────

// TestCamFilterEvidence confirms only tool-minted media_urls survive: a
// hallucinated/mistyped URL is dropped (never rendered as a broken evidence link),
// exact-but-whitespace-padded matches are kept, and empty URLs are dropped.
func TestCamFilterEvidence(t *testing.T) {
	msgs := []camInvestigationMessage{
		{Role: "ai", ToolName: "past_frames"},
		{Role: "tool", ToolName: "past_frames", MediaJSON: mustJSON([]evidenceItem{{MediaURL: "/camera/media/tokA"}, {MediaURL: "/camera/media/tokB"}})},
		{Role: "ai", ToolName: "answer", MediaJSON: mustJSON([]evidenceItem{{MediaURL: "/camera/media/should_be_ignored"}})}, // ai row, not a mint source
	}
	allowed := camCollectMintedMediaURLs(msgs)
	if !allowed["/camera/media/tokA"] || !allowed["/camera/media/tokB"] {
		t.Fatalf("minted set = %v, want tokA+tokB", allowed)
	}
	if allowed["/camera/media/should_be_ignored"] {
		t.Errorf("ai-row media must not be treated as tool-minted")
	}

	cited := []evidenceItem{
		{MediaURL: "/camera/media/tokA", Caption: "keep"},
		{MediaURL: "/camera/media/ghost", Caption: "hallucinated"},
		{MediaURL: " /camera/media/tokB ", Caption: "padded but real"},
		{MediaURL: "", Caption: "empty"},
	}
	kept, dropped := camFilterEvidence(cited, allowed)
	if dropped != 2 {
		t.Errorf("dropped = %d, want 2 (ghost + empty)", dropped)
	}
	if len(kept) != 2 || kept[0].MediaURL != "/camera/media/tokA" || kept[1].Caption != "padded but real" {
		t.Errorf("kept = %+v, want [tokA, padded tokB]", kept)
	}
}

// ─────────────────────── investigations schema migration ───────────────────────

// TestMigrateCameraDBInvestigationTablesIdempotent focuses camera_store_test.go's
// broader migration-idempotency coverage specifically on the two tables this
// feature added (camera_investigations, camera_investigation_messages): running
// the migration repeatedly — including after data has been inserted and after a
// full close/reopen of the underlying file — must never error, duplicate a
// table/index, or disturb rows already written. openCamTestDB is defined in
// camera_store_test.go and reused here (same package/test binary).
func TestMigrateCameraDBInvestigationTablesIdempotent(t *testing.T) {
	db := openCamTestDB(t)
	if err := migrateCameraDB(db); err != nil {
		t.Fatalf("first migrateCameraDB: %v", err)
	}
	if err := migrateCameraDB(db); err != nil {
		t.Fatalf("second migrateCameraDB (idempotency): %v", err)
	}

	for _, table := range []string{"camera_investigations", "camera_investigation_messages"} {
		var name string
		if err := db.QueryRow(`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&name); err != nil {
			t.Fatalf("table %q missing after migrateCameraDB: %v", table, err)
		}
	}
	for _, idx := range []string{"idx_camera_investigations_site", "idx_camera_investigation_messages_inv"} {
		var name string
		if err := db.QueryRow(`SELECT name FROM sqlite_master WHERE type='index' AND name=?`, idx).Scan(&name); err != nil {
			t.Fatalf("index %q missing after migrateCameraDB: %v", idx, err)
		}
	}

	invID, err := insertCameraInvestigation(db, camInvestigation{SiteID: "site1", Title: "t", Question: "what happened at reception?"})
	if err != nil {
		t.Fatalf("insertCameraInvestigation: %v", err)
	}
	if _, err := appendCameraInvestigationMessage(db, camInvestigationMessage{InvestigationID: invID, Role: "operator", Content: "what happened at reception?"}); err != nil {
		t.Fatalf("appendCameraInvestigationMessage: %v", err)
	}
	if _, err := appendCameraInvestigationMessage(db, camInvestigationMessage{InvestigationID: invID, Role: "ai", Content: "checking now", ToolName: "roster"}); err != nil {
		t.Fatalf("appendCameraInvestigationMessage: %v", err)
	}

	// Re-run the migration a third time now that rows exist — CREATE TABLE/INDEX
	// IF NOT EXISTS must be a true no-op and must not touch existing data.
	if err := migrateCameraDB(db); err != nil {
		t.Fatalf("third migrateCameraDB (post-data): %v", err)
	}

	var invCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM camera_investigations`).Scan(&invCount); err != nil {
		t.Fatalf("count camera_investigations: %v", err)
	}
	if invCount != 1 {
		t.Fatalf("camera_investigations count = %d, want 1", invCount)
	}
	var msgCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM camera_investigation_messages WHERE investigation_id = ?`, invID).Scan(&msgCount); err != nil {
		t.Fatalf("count camera_investigation_messages: %v", err)
	}
	if msgCount != 2 {
		t.Fatalf("camera_investigation_messages count = %d, want 2", msgCount)
	}

	// seq must have been auto-assigned in insertion order, ascending from 1.
	msgs, err := listCameraInvestigationMessages(db, invID)
	if err != nil {
		t.Fatalf("listCameraInvestigationMessages: %v", err)
	}
	if len(msgs) != 2 || msgs[0].Seq != 1 || msgs[1].Seq != 2 {
		t.Fatalf("seqs = %+v, want [1 2]", msgs)
	}

	// Each documented index must survive exactly once after triple migration.
	for _, idx := range []string{"idx_camera_investigations_site", "idx_camera_investigation_messages_inv"} {
		var cnt int
		if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name=?`, idx).Scan(&cnt); err != nil {
			t.Fatalf("count index %q: %v", idx, err)
		}
		if cnt != 1 {
			t.Errorf("index %q count = %d, want exactly 1 after triple migration", idx, cnt)
		}
	}
}
