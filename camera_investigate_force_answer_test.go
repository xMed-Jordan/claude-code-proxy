package main

// camera_investigate_force_answer_test.go — the forced best-effort final answer a
// TOP-LEVEL investigation makes before it terminalizes "exhausted", driven through
// the REAL runInvestigation loop with a scripted model (camInvestigateAnalyzeFn):
//
//   - max-turns exit: the forced answer SUCCEEDS (settles "answered", a forced_answer
//     breadcrumb + the answer row, evidence carried through, exactly ONE forced call)
//     or DECLINES (a tool-call / blank reply → the honest empty "exhausted" stop).
//   - requeue give-up exit: the forced answer fires on EXACTLY the give-up attempt —
//     never on an earlier requeue that still had streak budget (the pre-check predicts
//     camDeferInvestigation's give-up exactly).
//   - kill switch: CameraForceAnswerDisabled → no forced model call, empty stop.
//   - guard-D independence: the forced answer is never subjected to the completeness
//     judge (no answer_quality note, no second model call).
//   - fresh context: the forced answer works even when the per-pass ctx is already
//     cancelled (it builds its own context.Background() deadline).
//   - top-level only: a child row (ParentID != "") never force-answers.
//
// Temp sqlite only, no DVRs, no network (the analyze seam and the judge seam are
// scripted). seedInterruptInvestigation / newCamViewTestConfig / scriptCompletenessJudge
// / countAnswerQualityNotes are reused from camera_investigate_interrupt_test.go.

import (
	"context"
	"strings"
	"testing"
	"time"
)

// countForcedAnswerNotes returns how many "forced_answer" breadcrumb rows the
// transcript holds (the best-effort-under-exhaustion marker).
func countForcedAnswerNotes(msgs []camInvestigationMessage) int {
	n := 0
	for _, m := range msgs {
		if m.Role == "system" && m.ToolName == "forced_answer" {
			n++
		}
	}
	return n
}

// seedRunningChildInvestigation is seedInterruptInvestigation with a non-empty
// parent_id, so runInvestigation sees a sub-investigation row (top-level gating off).
func seedRunningChildInvestigation(t *testing.T, cfg config, question, parentID string) (invID string) {
	t.Helper()
	db, err := openProxyDB(cfg)
	if err != nil {
		t.Fatalf("openProxyDB: %v", err)
	}
	defer db.Close()
	siteID, serr := insertCameraSite(db, camSite{Name: "Child Site"})
	if serr != nil {
		t.Fatalf("insertCameraSite: %v", serr)
	}
	invID, ierr := insertCameraInvestigation(db, camInvestigation{
		SiteID: siteID, Question: question, Status: "running", ParentID: parentID,
	})
	if ierr != nil {
		t.Fatalf("insertCameraInvestigation: %v", ierr)
	}
	camAppendInvestigateMessage(db, invID, "operator", question, "", "", 0, nil)
	return invID
}

// TestRunInvestigationForceAnswerMaxTurnsSucceeds: a model that only ever calls tools
// (never answers) exhausts the cumulative turn cap; the forced final-answer call then
// returns an "answer" → the run settles "answered" (not "exhausted"), the LAST turn is
// the forced answer, a forced_answer breadcrumb precedes it, cited evidence carries
// through, and the forced model call happens EXACTLY once.
func TestRunInvestigationForceAnswerMaxTurnsSucceeds(t *testing.T) {
	cfg := newCamViewTestConfig(t)
	cfg.CameraInvestigateMaxTurns = 2 // two tool turns, then the wall
	invID := seedInterruptInvestigation(t, cfg, "who entered the lobby overnight?")
	seedMintedMedia(t, cfg, invID) // mints /camera/media/tokSeed1 for the forced answer to cite
	db, err := openProxyDB(cfg)
	if err != nil {
		t.Fatalf("openProxyDB: %v", err)
	}
	defer db.Close()

	orig := camInvestigateAnalyzeFn
	defer func() { camInvestigateAnalyzeFn = orig }()
	calls, forcedCalls := 0, 0
	camInvestigateAnalyzeFn = func(ctx context.Context, c config, alias, sys, user string, images []string) (string, []camAnalyzeAttempt, error) {
		calls++
		if strings.Contains(user, "OUT of investigation budget") {
			forcedCalls++
			return `{"thought":"best effort under exhaustion","action":{"type":"answer","answer":"Partial: a person crossed the lobby near 02:10; the rest of the night is unconfirmed.","evidence":[{"media_url":"/camera/media/tokSeed1"}]}}`, nil, nil
		}
		return `{"thought":"keep looking","action":{"type":"call_tool","tool":"roster"}}`, nil, nil
	}

	if rerr := runInvestigation(context.Background(), cfg, nil, invID); rerr != nil {
		t.Fatalf("runInvestigation: %v", rerr)
	}
	if forcedCalls != 1 {
		t.Errorf("forced model calls = %d, want exactly 1", forcedCalls)
	}
	if calls != 3 {
		t.Errorf("analyze calls = %d, want 3 (2 tool turns + 1 forced answer)", calls)
	}
	if inv, _ := getCameraInvestigation(db, invID); inv.Status != "answered" {
		t.Errorf("status = %q, want answered", inv.Status)
	}
	msgs, _ := listCameraInvestigationMessages(db, invID)
	if n := countForcedAnswerNotes(msgs); n != 1 {
		t.Errorf("forced_answer breadcrumbs = %d, want exactly 1", n)
	}
	last := msgs[len(msgs)-1]
	if last.Role != "ai" || last.ToolName != "answer" || !strings.Contains(last.Content, "Partial:") {
		t.Fatalf("last row = %+v, want the forced ai/answer", last)
	}
	if !strings.Contains(last.MediaJSON, "tokSeed1") {
		t.Errorf("forced answer evidence = %q, want the minted tokSeed1 carried through", last.MediaJSON)
	}
}

// TestRunInvestigationForceAnswerMaxTurnsDeclines: when the forced call declines
// (returns a tool call, or a blank answer with no thought to promote) the run stops
// exactly as today — "exhausted" with the honest max-turns note, no forced answer.
func TestRunInvestigationForceAnswerMaxTurnsDeclines(t *testing.T) {
	cases := []struct {
		name         string
		forcedReply  string
		wantForcedFn bool // did the forced model call happen (declines still make the call)
	}{
		{"tool call reply", `{"thought":"I should look more","action":{"type":"call_tool","tool":"roster"}}`, true},
		{"blank answer reply", `{"action":{"type":"answer"}}`, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := newCamViewTestConfig(t)
			cfg.CameraInvestigateMaxTurns = 2
			invID := seedInterruptInvestigation(t, cfg, "count the cars in lot B overnight")
			db, err := openProxyDB(cfg)
			if err != nil {
				t.Fatalf("openProxyDB: %v", err)
			}
			defer db.Close()

			orig := camInvestigateAnalyzeFn
			defer func() { camInvestigateAnalyzeFn = orig }()
			forcedCalls := 0
			camInvestigateAnalyzeFn = func(ctx context.Context, c config, alias, sys, user string, images []string) (string, []camAnalyzeAttempt, error) {
				if strings.Contains(user, "OUT of investigation budget") {
					forcedCalls++
					return tc.forcedReply, nil, nil
				}
				return `{"thought":"keep looking","action":{"type":"call_tool","tool":"roster"}}`, nil, nil
			}

			if rerr := runInvestigation(context.Background(), cfg, nil, invID); rerr != nil {
				t.Fatalf("runInvestigation: %v", rerr)
			}
			if (forcedCalls == 1) != tc.wantForcedFn {
				t.Errorf("forced model calls = %d, want fired=%v", forcedCalls, tc.wantForcedFn)
			}
			if inv, _ := getCameraInvestigation(db, invID); inv.Status != "exhausted" {
				t.Errorf("status = %q, want exhausted (forced answer declined)", inv.Status)
			}
			msgs, _ := listCameraInvestigationMessages(db, invID)
			if n := countForcedAnswerNotes(msgs); n != 0 {
				t.Errorf("forced_answer breadcrumbs = %d, want 0 (declined → no persisted answer)", n)
			}
			last := msgs[len(msgs)-1]
			if last.Role != "system" || !strings.Contains(last.Content, "maximum number of turns") {
				t.Errorf("last row = %+v, want the honest max-turns note", last)
			}
		})
	}
}

// TestRunInvestigationForceAnswerRequeueGiveUpLastAttemptOnly drives the auto-requeue
// streak to its cap across successive passes (each pass's budget is dead on entry, so
// it makes zero progress and camDeferInvestigation requeues). The forced answer must
// fire on EXACTLY the give-up attempt — NOT on the earlier requeues that still had
// streak budget — proving the pre-check predicts camDeferInvestigation's give-up
// exactly (off-by-one either way would force-answer early or never).
func TestRunInvestigationForceAnswerRequeueGiveUpLastAttemptOnly(t *testing.T) {
	cfg := newCamViewTestConfig(t)
	cfg.CameraInvestigateMaxRequeues = 2 // give up on the 3rd attempt (streak 2+1 > 2)
	invID := seedInterruptInvestigation(t, cfg, "was the safe opened after hours?")
	db, err := openProxyDB(cfg)
	if err != nil {
		t.Fatalf("openProxyDB: %v", err)
	}
	defer db.Close()

	orig := camInvestigateAnalyzeFn
	defer func() { camInvestigateAnalyzeFn = orig }()
	forcedCalls := 0
	camInvestigateAnalyzeFn = func(ctx context.Context, c config, alias, sys, user string, images []string) (string, []camAnalyzeAttempt, error) {
		// The ONLY time the seam is invoked is the forced call — every pass enters with
		// a dead budget, so the loop never reaches an in-loop analysis call.
		if !strings.Contains(user, "OUT of investigation budget") {
			t.Errorf("seam called for a non-forced turn:\n%s", user)
		}
		forcedCalls++
		return `{"thought":"out of budget","action":{"type":"answer","answer":"Best effort: no safe access is confirmed in the footage reviewed; the unreviewed windows remain unknown."}}`, nil, nil
	}

	// Each pass enters with an already-cancelled context, so cctx.Err()!=nil at the top
	// of the loop before any turn runs — the zero-progress budget wall.
	deadCtx, cancel := context.WithCancel(context.Background())
	cancel()

	runPass := func() {
		if serr := setCameraInvestigationStatus(db, invID, "running"); serr != nil {
			t.Fatalf("set running: %v", serr)
		}
		if rerr := runInvestigation(deadCtx, cfg, nil, invID); rerr != nil {
			t.Fatalf("runInvestigation: %v", rerr)
		}
	}

	// Passes 1 and 2: still under the streak cap → requeue (queued), NO forced call.
	for pass := 1; pass <= 2; pass++ {
		runPass()
		if inv, _ := getCameraInvestigation(db, invID); inv.Status != "queued" {
			t.Fatalf("pass %d: status = %q, want queued (requeued, not given up)", pass, inv.Status)
		}
		if forcedCalls != 0 {
			t.Fatalf("pass %d: forced calls = %d, want 0 (still had requeue budget)", pass, forcedCalls)
		}
		msgs, _ := listCameraInvestigationMessages(db, invID)
		if n := countForcedAnswerNotes(msgs); n != 0 {
			t.Fatalf("pass %d: forced_answer breadcrumbs = %d, want 0", pass, n)
		}
	}

	// Pass 3 is the give-up attempt (streak 2, 2+1 > 2): the forced answer fires INSTEAD
	// of the empty "gave up" stop.
	runPass()
	if forcedCalls != 1 {
		t.Errorf("forced calls = %d, want exactly 1 (fires only on the give-up attempt)", forcedCalls)
	}
	if inv, _ := getCameraInvestigation(db, invID); inv.Status != "answered" {
		t.Errorf("final status = %q, want answered (forced best-effort answer)", inv.Status)
	}
	msgs, _ := listCameraInvestigationMessages(db, invID)
	if n := countForcedAnswerNotes(msgs); n != 1 {
		t.Errorf("forced_answer breadcrumbs = %d, want exactly 1", n)
	}
	last := msgs[len(msgs)-1]
	if last.Role != "ai" || last.ToolName != "answer" || !strings.Contains(last.Content, "Best effort") {
		t.Errorf("last row = %+v, want the forced ai/answer", last)
	}
}

// TestRunInvestigationForceAnswerKillSwitch: with CameraForceAnswerDisabled the run
// terminalizes empty exactly as before and NO forced model call is made (the switch is
// checked before the analysis call).
func TestRunInvestigationForceAnswerKillSwitch(t *testing.T) {
	cfg := newCamViewTestConfig(t)
	cfg.CameraInvestigateMaxTurns = 2
	cfg.CameraForceAnswerDisabled = true // the kill switch
	invID := seedInterruptInvestigation(t, cfg, "did the gate stay shut?")
	db, err := openProxyDB(cfg)
	if err != nil {
		t.Fatalf("openProxyDB: %v", err)
	}
	defer db.Close()

	orig := camInvestigateAnalyzeFn
	defer func() { camInvestigateAnalyzeFn = orig }()
	calls, forcedCalls := 0, 0
	camInvestigateAnalyzeFn = func(ctx context.Context, c config, alias, sys, user string, images []string) (string, []camAnalyzeAttempt, error) {
		calls++
		if strings.Contains(user, "OUT of investigation budget") {
			forcedCalls++
		}
		return `{"thought":"keep looking","action":{"type":"call_tool","tool":"roster"}}`, nil, nil
	}

	if rerr := runInvestigation(context.Background(), cfg, nil, invID); rerr != nil {
		t.Fatalf("runInvestigation: %v", rerr)
	}
	if forcedCalls != 0 {
		t.Errorf("forced model calls = %d, want 0 (kill switch)", forcedCalls)
	}
	if calls != 2 {
		t.Errorf("analyze calls = %d, want 2 (no forced call under the kill switch)", calls)
	}
	if inv, _ := getCameraInvestigation(db, invID); inv.Status != "exhausted" {
		t.Errorf("status = %q, want exhausted", inv.Status)
	}
	msgs, _ := listCameraInvestigationMessages(db, invID)
	if n := countForcedAnswerNotes(msgs); n != 0 {
		t.Errorf("forced_answer breadcrumbs = %d, want 0", n)
	}
}

// TestRunInvestigationForceAnswerBypassesGuardD: the forced answer is the last resort
// and must NOT be re-judged for completeness. Even with a judge scripted to reject on
// scope, the forced answer settles "answered" with ZERO judge calls and NO
// answer_quality corrective note.
func TestRunInvestigationForceAnswerBypassesGuardD(t *testing.T) {
	cfg := newCamViewTestConfig(t)
	cfg.CameraInvestigateMaxTurns = 2
	cfg.CameraAnswerJudgeDisabled = false // guard D live; the forced path must still bypass it
	invID := seedInterruptInvestigation(t, cfg, "summarize 07:00-09:00 across all cameras")
	db, err := openProxyDB(cfg)
	if err != nil {
		t.Fatalf("openProxyDB: %v", err)
	}
	defer db.Close()

	judgeCalls := scriptCompletenessJudge(t, false, "would reject the whole window", nil)

	orig := camInvestigateAnalyzeFn
	defer func() { camInvestigateAnalyzeFn = orig }()
	camInvestigateAnalyzeFn = func(ctx context.Context, c config, alias, sys, user string, images []string) (string, []camAnalyzeAttempt, error) {
		if strings.Contains(user, "OUT of investigation budget") {
			return `{"thought":"partial","action":{"type":"answer","answer":"Partial: only 07:00-07:06 was reviewed; the rest is unknown."}}`, nil, nil
		}
		return `{"thought":"keep looking","action":{"type":"call_tool","tool":"roster"}}`, nil, nil
	}

	if rerr := runInvestigation(context.Background(), cfg, nil, invID); rerr != nil {
		t.Fatalf("runInvestigation: %v", rerr)
	}
	if *judgeCalls != 0 {
		t.Errorf("judge calls = %d, want 0 (forced answer bypasses guard D)", *judgeCalls)
	}
	if inv, _ := getCameraInvestigation(db, invID); inv.Status != "answered" {
		t.Errorf("status = %q, want answered", inv.Status)
	}
	msgs, _ := listCameraInvestigationMessages(db, invID)
	if n := countAnswerQualityNotes(msgs); n != 0 {
		t.Errorf("answer_quality notes = %d, want 0 (no corrective round on the forced answer)", n)
	}
	if n := countForcedAnswerNotes(msgs); n != 1 {
		t.Errorf("forced_answer breadcrumbs = %d, want 1", n)
	}
}

// TestRunInvestigationForceAnswerFreshContext proves the forced answer builds its OWN
// context: it is reached at a terminal exit PRECISELY because the per-pass ctx is dead
// (a cancelled parent ctx makes cctx.Err()!=nil immediately), yet the forced model
// call still receives a LIVE context. The streak is pre-seeded to the give-up cap so
// the budget wall routes straight to the forced answer.
func TestRunInvestigationForceAnswerFreshContext(t *testing.T) {
	cfg := newCamViewTestConfig(t)
	cfg.CameraInvestigateMaxRequeues = 1 // streak 1 → 1+1 > 1 gives up (forces) at once
	invID := seedInterruptInvestigation(t, cfg, "any motion at the rear door?")
	db, err := openProxyDB(cfg)
	if err != nil {
		t.Fatalf("openProxyDB: %v", err)
	}
	defer db.Close()
	// One prior auto_requeue makes the NEXT deferral the give-up (streak 1).
	camAppendInvestigateMessage(db, invID, "system",
		"Time budget for this pass was exhausted before a final answer. Auto-resuming (attempt 1 of 1).",
		"auto_requeue", "", 0, nil)

	orig := camInvestigateAnalyzeFn
	defer func() { camInvestigateAnalyzeFn = orig }()
	forcedCalls := 0
	var seamCtxErr error = context.Canceled // must be overwritten with the live ctx's nil Err
	camInvestigateAnalyzeFn = func(ctx context.Context, c config, alias, sys, user string, images []string) (string, []camAnalyzeAttempt, error) {
		forcedCalls++
		seamCtxErr = ctx.Err()
		return `{"thought":"fresh ctx","action":{"type":"answer","answer":"Best effort: no rear-door motion in the reviewed windows; unreviewed spans remain unknown."}}`, nil, nil
	}

	// Cancel the parent BEFORE the run so the per-pass cctx is dead at the top of the loop.
	deadCtx, cancel := context.WithCancel(context.Background())
	cancel()
	if rerr := runInvestigation(deadCtx, cfg, nil, invID); rerr != nil {
		t.Fatalf("runInvestigation: %v", rerr)
	}
	if forcedCalls != 1 {
		t.Fatalf("forced calls = %d, want 1", forcedCalls)
	}
	if seamCtxErr != nil {
		t.Errorf("forced call ctx.Err() = %v, want nil (a FRESH context, not the dead per-pass ctx)", seamCtxErr)
	}
	if inv, _ := getCameraInvestigation(db, invID); inv.Status != "answered" {
		t.Errorf("status = %q, want answered", inv.Status)
	}
	if msgs, _ := listCameraInvestigationMessages(db, invID); countForcedAnswerNotes(msgs) != 1 {
		t.Errorf("forced_answer breadcrumbs = %d, want 1", countForcedAnswerNotes(msgs))
	}
}

// TestRunInvestigationForceAnswerSkippedForChild: a child row (ParentID != "") is NOT
// top-level, so it must never force-answer — it terminalizes "exhausted" exactly as a
// sub-investigation does, and the forced model call is never made.
func TestRunInvestigationForceAnswerSkippedForChild(t *testing.T) {
	cfg := newCamViewTestConfig(t)
	cfg.CameraInvestigateMaxTurns = 2
	invID := seedRunningChildInvestigation(t, cfg, "child scope: check camera 3", "inv_parent_dummy")
	db, err := openProxyDB(cfg)
	if err != nil {
		t.Fatalf("openProxyDB: %v", err)
	}
	defer db.Close()

	orig := camInvestigateAnalyzeFn
	defer func() { camInvestigateAnalyzeFn = orig }()
	forcedCalls := 0
	camInvestigateAnalyzeFn = func(ctx context.Context, c config, alias, sys, user string, images []string) (string, []camAnalyzeAttempt, error) {
		if strings.Contains(user, "OUT of investigation budget") {
			forcedCalls++
		}
		return `{"thought":"keep looking","action":{"type":"call_tool","tool":"roster"}}`, nil, nil
	}

	if rerr := runInvestigation(context.Background(), cfg, nil, invID); rerr != nil {
		t.Fatalf("runInvestigation: %v", rerr)
	}
	if forcedCalls != 0 {
		t.Errorf("forced model calls = %d, want 0 (child rows never force-answer)", forcedCalls)
	}
	if inv, _ := getCameraInvestigation(db, invID); inv.Status != "exhausted" {
		t.Errorf("status = %q, want exhausted", inv.Status)
	}
	if msgs, _ := listCameraInvestigationMessages(db, invID); countForcedAnswerNotes(msgs) != 0 {
		t.Errorf("forced_answer breadcrumbs = %d, want 0", countForcedAnswerNotes(msgs))
	}
}

// TestCamAutoRequeueExhaustedPredictsGiveUp pins the streak-math lockstep: for every
// streak/cap pairing, camAutoRequeueExhausted returns the SAME verdict
// camDeferInvestigation uses internally (camCountAutoRequeues(msgs)+1 > cap). An
// off-by-one here would force-answer one requeue early or never.
func TestCamAutoRequeueExhaustedPredictsGiveUp(t *testing.T) {
	reqRows := func(n int) []camInvestigationMessage {
		msgs := []camInvestigationMessage{{Role: "operator"}}
		for i := 0; i < n; i++ {
			msgs = append(msgs, camInvestigationMessage{Role: "system", ToolName: "auto_requeue"})
		}
		return msgs
	}
	for _, capN := range []int{1, 2, 5} {
		cfg := config{CameraInvestigateMaxRequeues: capN}
		for streak := 0; streak <= capN+1; streak++ {
			msgs := reqRows(streak)
			// The exact expression camDeferInvestigation gives up on.
			wantGiveUp := camCountAutoRequeues(msgs)+1 > camInvestigateMaxAutoRequeues(cfg)
			if got := camAutoRequeueExhausted(msgs, cfg); got != wantGiveUp {
				t.Errorf("cap=%d streak=%d: camAutoRequeueExhausted=%v, want %v", capN, streak, got, wantGiveUp)
			}
		}
	}
	// A model "ai" turn since the last requeue resets the streak → never exhausted.
	msgs := []camInvestigationMessage{
		{Role: "operator"},
		{Role: "system", ToolName: "auto_requeue"},
		{Role: "system", ToolName: "auto_requeue"},
		{Role: "ai", ToolName: "roster"}, // progress resets the streak
	}
	if camAutoRequeueExhausted(msgs, config{CameraInvestigateMaxRequeues: 1}) {
		t.Errorf("progress since the last requeue must reset the streak (not exhausted)")
	}
}

// TestCamForceAnswerTimeout pins the zero-default clamp idiom (mirrors camAnalyzeTimeout).
func TestCamForceAnswerTimeout(t *testing.T) {
	if got := camForceAnswerTimeout(config{}); got != 180*time.Second {
		t.Errorf("default = %v, want 180s", got)
	}
	if got := camForceAnswerTimeout(config{CameraForceAnswerTimeout: 45 * time.Second}); got != 45*time.Second {
		t.Errorf("override = %v, want 45s", got)
	}
	if got := camForceAnswerTimeout(config{CameraForceAnswerTimeout: -5 * time.Second}); got != 180*time.Second {
		t.Errorf("negative = %v, want the 180s default", got)
	}
}
