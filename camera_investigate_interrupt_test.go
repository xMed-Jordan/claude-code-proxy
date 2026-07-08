package main

// camera_investigate_interrupt_test.go — the pending-message (interrupt)
// side-channel and the final-answer quality guards, driven through the REAL
// runInvestigation loop with a scripted model (camInvestigateAnalyzeFn):
//
//   - drain point A: a message parked while the run works reaches the transcript
//     BEFORE the model's next action, and the pending table empties.
//   - drain point B: a pending row stranded against a settled row is delivered
//     (append + requeue); against a closed row it is dropped.
//   - guard B: answered with zero evidence despite minted media → exactly ONE
//     corrective round, then the second evidence-less answer is accepted.
//   - guard C: Arabic question + zero-Arabic answer → one corrective round; a
//     combined B+C failure still produces ONE round, not two.
//   - guard D (the completeness judge): an incomplete verdict folds into the SAME
//     one corrective round (naming the missing scope + a continue-investigating
//     instruction) then the corrected answer settles; a complete verdict settles
//     at once; a judge error/empty verdict fails open; the judge is never called a
//     second time in a pass, on a sub-investigation, or under the kill switch.
//
// Temp sqlite only — no DVRs, no network (both the analyze seam and the guard-D
// judge seam camAnswerCompletenessJudgeFn are scripted).

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
)

// seedInterruptInvestigation creates a site + a RUNNING investigation with its
// opening operator message, returning ids ready for a direct runInvestigation
// call (only a worker-claimed "running" row executes).
func seedInterruptInvestigation(t *testing.T, cfg config, question string) (invID string) {
	t.Helper()
	db, err := openProxyDB(cfg)
	if err != nil {
		t.Fatalf("openProxyDB: %v", err)
	}
	defer db.Close()
	siteID, serr := insertCameraSite(db, camSite{Name: "Interrupt Site"})
	if serr != nil {
		t.Fatalf("insertCameraSite: %v", serr)
	}
	invID, ierr := insertCameraInvestigation(db, camInvestigation{SiteID: siteID, Question: question, Status: "running"})
	if ierr != nil {
		t.Fatalf("insertCameraInvestigation: %v", ierr)
	}
	camAppendInvestigateMessage(db, invID, "operator", question, "", "", 0, nil)
	return invID
}

// TestRunInvestigationDrainsPendingInterrupt scripts a two-turn run and parks a
// pending message during turn 1: drain point A must fold it into the transcript
// BEFORE the model's turn-2 action, and the pending table must be empty after.
func TestRunInvestigationDrainsPendingInterrupt(t *testing.T) {
	cfg := newCamViewTestConfig(t)
	invID := seedInterruptInvestigation(t, cfg, "did anyone enter overnight?")
	db, err := openProxyDB(cfg)
	if err != nil {
		t.Fatalf("openProxyDB: %v", err)
	}
	defer db.Close()

	orig := camInvestigateAnalyzeFn
	defer func() { camInvestigateAnalyzeFn = orig }()
	calls := 0
	camInvestigateAnalyzeFn = func(ctx context.Context, c config, alias, sys, user string, images []string) (string, []camAnalyzeAttempt, error) {
		calls++
		if calls == 1 {
			// The interrupt arrives while the model is "thinking" on turn 1.
			if _, perr := insertCameraInvestigationPendingMessage(db, invID, "wait — check the loading dock first", ""); perr != nil {
				t.Fatalf("insert pending: %v", perr)
			}
			return `{"thought":"start cheap","action":{"type":"call_tool","tool":"roster"}}`, nil, nil
		}
		// Turn 2 must already SEE the interrupt in its transcript.
		if !strings.Contains(user, "wait — check the loading dock first") {
			t.Errorf("turn-2 model context missing the drained interrupt:\n%s", user)
		}
		return `{"thought":"done","action":{"type":"answer","answer":"no one entered overnight"}}`, nil, nil
	}

	if rerr := runInvestigation(context.Background(), cfg, nil, invID); rerr != nil {
		t.Fatalf("runInvestigation: %v", rerr)
	}
	if calls != 2 {
		t.Errorf("analyze calls = %d, want 2", calls)
	}

	msgs, _ := listCameraInvestigationMessages(db, invID)
	roles := make([]string, 0, len(msgs))
	for _, m := range msgs {
		roles = append(roles, m.Role)
	}
	// operator(q), ai(roster), tool(roster), operator(interrupt), ai(answer):
	// the interrupt lands BEFORE the model's turn-2 action.
	want := []string{"operator", "ai", "tool", "operator", "ai"}
	if strings.Join(roles, ",") != strings.Join(want, ",") {
		t.Fatalf("transcript roles = %v, want %v", roles, want)
	}
	if msgs[3].Content != "wait — check the loading dock first" {
		t.Errorf("interrupt row content = %q", msgs[3].Content)
	}
	if pend, _ := listCameraInvestigationPendingMessages(db, invID); len(pend) != 0 {
		t.Errorf("pending table not empty after drain: %+v", pend)
	}
	if inv, _ := getCameraInvestigation(db, invID); inv.Status != "answered" {
		t.Errorf("status = %q, want answered", inv.Status)
	}
}

// TestCamFlushStrandedPendingInvestigateMessages covers drain point B directly:
// a pending row against a SETTLED row is delivered through the settled-reply
// semantics (operator row appended first, then status queued); against a CLOSED
// row it is dropped (deleted, not delivered); against a queued/running row it is
// left for the run's own drain point.
func TestCamFlushStrandedPendingInvestigateMessages(t *testing.T) {
	cfg := newCamViewTestConfig(t)
	db, err := openProxyDB(cfg)
	if err != nil {
		t.Fatalf("openProxyDB: %v", err)
	}
	defer db.Close()

	seed := func(status string) string {
		id, ierr := insertCameraInvestigation(db, camInvestigation{SiteID: "site_1", Question: "q", Status: status})
		if ierr != nil {
			t.Fatalf("insert investigation: %v", ierr)
		}
		camAppendInvestigateMessage(db, id, "operator", "q", "", "", 0, nil)
		return id
	}

	// Settled → delivered + requeued.
	settled := seed("answered")
	if _, err := insertCameraInvestigationPendingMessage(db, settled, "follow-up", ""); err != nil {
		t.Fatalf("insert pending: %v", err)
	}
	camFlushStrandedPendingInvestigateMessages(db, settled)
	if pend, _ := listCameraInvestigationPendingMessages(db, settled); len(pend) != 0 {
		t.Errorf("settled flush left pending rows: %+v", pend)
	}
	msgs, _ := listCameraInvestigationMessages(db, settled)
	if len(msgs) != 2 || msgs[1].Role != "operator" || msgs[1].Content != "follow-up" {
		t.Errorf("settled flush transcript = %+v, want appended operator follow-up", msgs)
	}
	if inv, _ := getCameraInvestigation(db, settled); inv.Status != "queued" {
		t.Errorf("settled flush status = %q, want queued", inv.Status)
	}

	// Closed → dropped, transcript and status untouched.
	closed := seed("closed")
	if _, err := insertCameraInvestigationPendingMessage(db, closed, "too late", ""); err != nil {
		t.Fatalf("insert pending: %v", err)
	}
	camFlushStrandedPendingInvestigateMessages(db, closed)
	if pend, _ := listCameraInvestigationPendingMessages(db, closed); len(pend) != 0 {
		t.Errorf("closed flush left pending rows: %+v", pend)
	}
	if msgs, _ := listCameraInvestigationMessages(db, closed); len(msgs) != 1 {
		t.Errorf("closed flush must not deliver: transcript = %+v", msgs)
	}
	if inv, _ := getCameraInvestigation(db, closed); inv.Status != "closed" {
		t.Errorf("closed flush status = %q, want closed (terminal)", inv.Status)
	}

	// Queued → left alone (the run's own drain point A owns delivery).
	queued := seed("queued")
	if _, err := insertCameraInvestigationPendingMessage(db, queued, "still parked", ""); err != nil {
		t.Fatalf("insert pending: %v", err)
	}
	camFlushStrandedPendingInvestigateMessages(db, queued)
	if pend, _ := listCameraInvestigationPendingMessages(db, queued); len(pend) != 1 {
		t.Errorf("queued flush must leave the row parked: %+v", pend)
	}
}

// TestCamFlushStrandedPendingExactlyOnce proves drain point B's per-row claim:
// many concurrent flushes over the same settled row deliver its pending message
// EXACTLY once — never the duplicated operator row (and its spurious budget
// reset on a later resume) that a list-append-delete interleaving allowed.
func TestCamFlushStrandedPendingExactlyOnce(t *testing.T) {
	cfg := newCamViewTestConfig(t)
	db, err := openProxyDB(cfg)
	if err != nil {
		t.Fatalf("openProxyDB: %v", err)
	}
	defer db.Close()
	invID, ierr := insertCameraInvestigation(db, camInvestigation{SiteID: "site_1", Question: "q", Status: "answered"})
	if ierr != nil {
		t.Fatalf("insert investigation: %v", ierr)
	}
	camAppendInvestigateMessage(db, invID, "operator", "q", "", "", 0, nil)
	if _, err := insertCameraInvestigationPendingMessage(db, invID, "exactly once please", ""); err != nil {
		t.Fatalf("insert pending: %v", err)
	}

	const flushers = 8
	var wg sync.WaitGroup
	for i := 0; i < flushers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			camFlushStrandedPendingInvestigateMessages(db, invID)
		}()
	}
	wg.Wait()

	msgs, _ := listCameraInvestigationMessages(db, invID)
	delivered := 0
	for _, m := range msgs {
		if m.Role == "operator" && m.Content == "exactly once please" {
			delivered++
		}
	}
	if delivered != 1 {
		t.Errorf("delivered operator rows = %d, want exactly 1 (concurrent flushes must not double-deliver)", delivered)
	}
	if pend, _ := listCameraInvestigationPendingMessages(db, invID); len(pend) != 0 {
		t.Errorf("pending rows after flush = %+v, want none", pend)
	}
	if inv, _ := getCameraInvestigation(db, invID); inv.Status != "queued" {
		t.Errorf("status = %q, want queued", inv.Status)
	}
}

// TestCamResumeSettledInvestigationClosedRace simulates a reply whose settled
// status read went stale against a concurrent operator close: the append happens
// first (the shared core's ordering), the settled→queued CAS then loses to the
// closed row — the appended operator row must be UNWOUND (closed transcripts are
// frozen) and the sentinel returned so callers answer "closed", never a false
// "queued".
func TestCamResumeSettledInvestigationClosedRace(t *testing.T) {
	cfg := newCamViewTestConfig(t)
	db, err := openProxyDB(cfg)
	if err != nil {
		t.Fatalf("openProxyDB: %v", err)
	}
	defer db.Close()
	invID, ierr := insertCameraInvestigation(db, camInvestigation{SiteID: "site_1", Question: "q", Status: "closed"})
	if ierr != nil {
		t.Fatalf("insert investigation: %v", ierr)
	}
	camAppendInvestigateMessage(db, invID, "operator", "q", "", "", 0, nil)

	serr := camResumeSettledInvestigation(db, invID, "sneaking into a closed record", "")
	if !errors.Is(serr, errCamInvestigateReplyClosed) {
		t.Fatalf("err = %v, want errCamInvestigateReplyClosed", serr)
	}
	msgs, _ := listCameraInvestigationMessages(db, invID)
	if len(msgs) != 1 {
		t.Errorf("transcript rows = %d, want 1 (the racing reply must be unwound)", len(msgs))
	}
	if inv, _ := getCameraInvestigation(db, invID); inv.Status != "closed" {
		t.Errorf("status = %q, want closed (terminal)", inv.Status)
	}
}

// TestCamPendingOriginMarksDrainedRows pins the origin plumbing behind the
// view-reply cap: a pending row parked by the share-link page (origin
// camViewReplySource) drains into an operator row CARRYING that marker as its
// tool_name — at drain point A and at the stranded flush — and
// countCameraInvestigationViewReplies counts delivered markers plus still-parked
// view rows while ignoring authenticated (empty-origin) messages.
func TestCamPendingOriginMarksDrainedRows(t *testing.T) {
	cfg := newCamViewTestConfig(t)
	db, err := openProxyDB(cfg)
	if err != nil {
		t.Fatalf("openProxyDB: %v", err)
	}
	defer db.Close()

	// Drain point A: running row, one view message + one authenticated message.
	invID, _ := insertCameraInvestigation(db, camInvestigation{SiteID: "site_1", Question: "q", Status: "running"})
	msgs := []camInvestigationMessage{camAppendInvestigateMessage(db, invID, "operator", "q", "", "", 0, nil)}
	if _, err := insertCameraInvestigationPendingMessage(db, invID, "from the share link", camViewReplySource); err != nil {
		t.Fatalf("insert view pending: %v", err)
	}
	if _, err := insertCameraInvestigationPendingMessage(db, invID, "from the studio", ""); err != nil {
		t.Fatalf("insert svc pending: %v", err)
	}
	msgs, drained := camDrainPendingInvestigateMessages(db, invID, msgs)
	if drained != 2 {
		t.Fatalf("drained = %d, want 2", drained)
	}
	byContent := map[string]string{}
	for _, m := range msgs {
		byContent[m.Content] = m.ToolName
	}
	if byContent["from the share link"] != camViewReplySource {
		t.Errorf("view message tool_name = %q, want %q", byContent["from the share link"], camViewReplySource)
	}
	if byContent["from the studio"] != "" {
		t.Errorf("authenticated message tool_name = %q, want empty", byContent["from the studio"])
	}
	if n, _ := countCameraInvestigationViewReplies(db, invID); n != 1 {
		t.Errorf("view reply count = %d, want 1 (delivered marker only)", n)
	}

	// Stranded flush inherits the origin too, and parked view rows count.
	settled, _ := insertCameraInvestigation(db, camInvestigation{SiteID: "site_1", Question: "q2", Status: "answered"})
	camAppendInvestigateMessage(db, settled, "operator", "q2", "", "", 0, nil)
	if _, err := insertCameraInvestigationPendingMessage(db, settled, "stranded view note", camViewReplySource); err != nil {
		t.Fatalf("insert pending: %v", err)
	}
	if n, _ := countCameraInvestigationViewReplies(db, settled); n != 1 {
		t.Errorf("parked view reply count = %d, want 1", n)
	}
	camFlushStrandedPendingInvestigateMessages(db, settled)
	smsgs, _ := listCameraInvestigationMessages(db, settled)
	last := smsgs[len(smsgs)-1]
	if last.Content != "stranded view note" || last.ToolName != camViewReplySource {
		t.Errorf("flushed row = %+v, want the view marker inherited", last)
	}
	if n, _ := countCameraInvestigationViewReplies(db, settled); n != 1 {
		t.Errorf("view reply count after flush = %d, want still 1 (moved, not doubled)", n)
	}
}

// seedMintedMedia appends a tool row carrying minted media so the answer guard
// sees "media was captured this run" without any DVR.
func seedMintedMedia(t *testing.T, cfg config, invID string) {
	t.Helper()
	db, err := openProxyDB(cfg)
	if err != nil {
		t.Fatalf("openProxyDB: %v", err)
	}
	defer db.Close()
	camAppendInvestigateMessage(db, invID, "tool", "seeded snapshot", "snapshot", "", 1,
		[]evidenceItem{{MediaURL: "/camera/media/tokSeed1", Caption: "seed"}})
}

// countAnswerQualityNotes returns how many corrective-round system rows the
// transcript holds (tool_name "answer_quality").
func countAnswerQualityNotes(msgs []camInvestigationMessage) int {
	n := 0
	for _, m := range msgs {
		if m.Role == "system" && m.ToolName == "answer_quality" {
			n++
		}
	}
	return n
}

// TestRunInvestigationAnswerGuardEvidence covers guard B: the run minted media
// but the model answers with no evidence[] → exactly one corrective round; the
// second evidence-less answer is accepted (never a loop, never exhausted).
func TestRunInvestigationAnswerGuardEvidence(t *testing.T) {
	cfg := newCamViewTestConfig(t)
	invID := seedInterruptInvestigation(t, cfg, "was the till checked?")
	seedMintedMedia(t, cfg, invID)
	db, err := openProxyDB(cfg)
	if err != nil {
		t.Fatalf("openProxyDB: %v", err)
	}
	defer db.Close()

	orig := camInvestigateAnalyzeFn
	defer func() { camInvestigateAnalyzeFn = orig }()
	calls := 0
	camInvestigateAnalyzeFn = func(ctx context.Context, c config, alias, sys, user string, images []string) (string, []camAnalyzeAttempt, error) {
		calls++
		return `{"thought":"reviewing","action":{"type":"answer","answer":"the till was checked at 14:02"}}`, nil, nil
	}

	if rerr := runInvestigation(context.Background(), cfg, nil, invID); rerr != nil {
		t.Fatalf("runInvestigation: %v", rerr)
	}
	if calls != 2 {
		t.Errorf("analyze calls = %d, want 2 (one corrective round)", calls)
	}
	msgs, _ := listCameraInvestigationMessages(db, invID)
	if n := countAnswerQualityNotes(msgs); n != 1 {
		t.Errorf("answer_quality notes = %d, want exactly 1", n)
	}
	if inv, _ := getCameraInvestigation(db, invID); inv.Status != "answered" {
		t.Errorf("status = %q, want answered (second evidence-less answer accepted)", inv.Status)
	}
	last := msgs[len(msgs)-1]
	if last.Role != "ai" || last.ToolName != "answer" || !strings.Contains(last.Content, "the till was checked at 14:02") {
		t.Errorf("final row = %+v, want the accepted answer", last)
	}
}

// TestRunInvestigationAnswerGuardLanguage covers guard C alone: an Arabic
// question answered with zero Arabic runes triggers one corrective round, and a
// corrected Arabic answer is then accepted.
func TestRunInvestigationAnswerGuardLanguage(t *testing.T) {
	cfg := newCamViewTestConfig(t)
	invID := seedInterruptInvestigation(t, cfg, "هل وصل الساعي اليوم؟")
	db, err := openProxyDB(cfg)
	if err != nil {
		t.Fatalf("openProxyDB: %v", err)
	}
	defer db.Close()

	orig := camInvestigateAnalyzeFn
	defer func() { camInvestigateAnalyzeFn = orig }()
	calls := 0
	camInvestigateAnalyzeFn = func(ctx context.Context, c config, alias, sys, user string, images []string) (string, []camAnalyzeAttempt, error) {
		calls++
		if calls == 1 {
			return `{"thought":"checking arrivals","action":{"type":"answer","answer":"the courier arrived at noon"}}`, nil, nil
		}
		return `{"thought":"restating","action":{"type":"answer","answer":"نعم، وصل الساعي عند الظهر"}}`, nil, nil
	}

	if rerr := runInvestigation(context.Background(), cfg, nil, invID); rerr != nil {
		t.Fatalf("runInvestigation: %v", rerr)
	}
	if calls != 2 {
		t.Errorf("analyze calls = %d, want 2 (one corrective round)", calls)
	}
	msgs, _ := listCameraInvestigationMessages(db, invID)
	if n := countAnswerQualityNotes(msgs); n != 1 {
		t.Errorf("answer_quality notes = %d, want exactly 1", n)
	}
	last := msgs[len(msgs)-1]
	if !strings.Contains(last.Content, "وصل الساعي") {
		t.Errorf("final answer = %q, want the corrected Arabic answer", last.Content)
	}
	if inv, _ := getCameraInvestigation(db, invID); inv.Status != "answered" {
		t.Errorf("status = %q, want answered", inv.Status)
	}
}

// TestRunInvestigationAnswerGuardCombinedOneRound proves B+C fold into ONE
// corrective round: Arabic question + minted media + an English evidence-less
// answer twice → exactly one answer_quality note naming both defects, exactly
// two analyze calls, and the second answer accepted as-is.
func TestRunInvestigationAnswerGuardCombinedOneRound(t *testing.T) {
	cfg := newCamViewTestConfig(t)
	invID := seedInterruptInvestigation(t, cfg, "من دخل المستودع مساء أمس؟")
	seedMintedMedia(t, cfg, invID)
	db, err := openProxyDB(cfg)
	if err != nil {
		t.Fatalf("openProxyDB: %v", err)
	}
	defer db.Close()

	orig := camInvestigateAnalyzeFn
	defer func() { camInvestigateAnalyzeFn = orig }()
	calls := 0
	camInvestigateAnalyzeFn = func(ctx context.Context, c config, alias, sys, user string, images []string) (string, []camAnalyzeAttempt, error) {
		calls++
		return `{"thought":"summarize now","action":{"type":"answer","answer":"two staff members entered the warehouse"}}`, nil, nil
	}

	if rerr := runInvestigation(context.Background(), cfg, nil, invID); rerr != nil {
		t.Fatalf("runInvestigation: %v", rerr)
	}
	if calls != 2 {
		t.Errorf("analyze calls = %d, want 2 — combined B+C must be ONE round, not two", calls)
	}
	msgs, _ := listCameraInvestigationMessages(db, invID)
	if n := countAnswerQualityNotes(msgs); n != 1 {
		t.Fatalf("answer_quality notes = %d, want exactly 1", n)
	}
	for _, m := range msgs {
		if m.Role == "system" && m.ToolName == "answer_quality" {
			if !strings.Contains(m.Content, "evidence[]") || !strings.Contains(m.Content, "Arabic") {
				t.Errorf("combined corrective note must name both defects: %q", m.Content)
			}
		}
	}
	if inv, _ := getCameraInvestigation(db, invID); inv.Status != "answered" {
		t.Errorf("status = %q, want answered", inv.Status)
	}
}

// ─────────────────────── answer guard D (completeness judge) ───────────────────────

// scriptCompletenessJudge installs a scripted guard-D judge seam returning a fixed
// verdict and counts its calls; restores the original on cleanup.
func scriptCompletenessJudge(t *testing.T, complete bool, missing string, jerr error) *int {
	t.Helper()
	orig := camAnswerCompletenessJudgeFn
	t.Cleanup(func() { camAnswerCompletenessJudgeFn = orig })
	calls := 0
	camAnswerCompletenessJudgeFn = func(ctx context.Context, cfg config, alias, question, answer string) (bool, string, error) {
		calls++
		return complete, missing, jerr
	}
	return &calls
}

// TestRunInvestigationCompletenessIncomplete covers guard D's incomplete path AND
// the "no second judge call" rule (spec bullets 1 + 5): the judge rejects the first
// answer for scope, exactly ONE corrective round fires (its note carries the judge's
// missing text and the CONTINUE-INVESTIGATING instruction), the model's second REAL
// answer settles the run, and the judge is consulted EXACTLY once (never on the
// post-corrective answer).
func TestRunInvestigationCompletenessIncomplete(t *testing.T) {
	cfg := newCamViewTestConfig(t)
	cfg.CameraAnswerJudgeDisabled = false
	invID := seedInterruptInvestigation(t, cfg, "give me a summary of what happened 07:00-09:00 across all cameras")
	db, err := openProxyDB(cfg)
	if err != nil {
		t.Fatalf("openProxyDB: %v", err)
	}
	defer db.Close()

	const missing = "only the 06:56-07:06 window was examined, not the full 07:00-09:00 the operator asked for"
	judgeCalls := scriptCompletenessJudge(t, false, missing, nil)

	orig := camInvestigateAnalyzeFn
	defer func() { camInvestigateAnalyzeFn = orig }()
	calls := 0
	camInvestigateAnalyzeFn = func(ctx context.Context, c config, alias, sys, user string, images []string) (string, []camAnalyzeAttempt, error) {
		calls++
		if calls == 1 {
			return `{"thought":"peeked at 07:00","action":{"type":"answer","answer":"At 07:01 a car parked near gate 2."}}`, nil, nil
		}
		if !strings.Contains(user, missing) {
			t.Errorf("turn-2 context is missing the judge's missing text:\n%s", user)
		}
		return `{"thought":"covered the whole window now","action":{"type":"answer","answer":"Across 07:00-09:00: a car parked at 07:01, staff arrived 07:40, a delivery at 08:30, quiet after."}}`, nil, nil
	}

	if rerr := runInvestigation(context.Background(), cfg, nil, invID); rerr != nil {
		t.Fatalf("runInvestigation: %v", rerr)
	}
	if *judgeCalls != 1 {
		t.Errorf("judge calls = %d, want exactly 1 (once on the first answer, never after the round)", *judgeCalls)
	}
	if calls != 2 {
		t.Errorf("analyze calls = %d, want 2 (one corrective round)", calls)
	}
	msgs, _ := listCameraInvestigationMessages(db, invID)
	if n := countAnswerQualityNotes(msgs); n != 1 {
		t.Fatalf("answer_quality notes = %d, want exactly 1", n)
	}
	for _, m := range msgs {
		if m.Role == "system" && m.ToolName == "answer_quality" {
			if !strings.Contains(m.Content, missing) {
				t.Errorf("corrective note is missing the judge's missing text: %q", m.Content)
			}
			if !strings.Contains(m.Content, "CONTINUE INVESTIGATING") {
				t.Errorf("corrective note is missing the continue-investigating instruction: %q", m.Content)
			}
		}
	}
	last := msgs[len(msgs)-1]
	if last.Role != "ai" || last.ToolName != "answer" || !strings.Contains(last.Content, "08:30") {
		t.Errorf("final row = %+v, want the corrected full-window answer", last)
	}
	if inv, _ := getCameraInvestigation(db, invID); inv.Status != "answered" {
		t.Errorf("status = %q, want answered", inv.Status)
	}
}

// TestRunInvestigationCompletenessComplete: a complete verdict settles the first
// answer immediately — judge called exactly once, no corrective round.
func TestRunInvestigationCompletenessComplete(t *testing.T) {
	cfg := newCamViewTestConfig(t)
	cfg.CameraAnswerJudgeDisabled = false
	invID := seedInterruptInvestigation(t, cfg, "summarize 07:00-09:00 across all cameras")
	db, err := openProxyDB(cfg)
	if err != nil {
		t.Fatalf("openProxyDB: %v", err)
	}
	defer db.Close()

	judgeCalls := scriptCompletenessJudge(t, true, "", nil)

	orig := camInvestigateAnalyzeFn
	defer func() { camInvestigateAnalyzeFn = orig }()
	calls := 0
	camInvestigateAnalyzeFn = func(ctx context.Context, c config, alias, sys, user string, images []string) (string, []camAnalyzeAttempt, error) {
		calls++
		return `{"thought":"full sweep done","action":{"type":"answer","answer":"Across 07:00-09:00: quiet until 07:40, staff arrived, a delivery at 08:30, nothing else."}}`, nil, nil
	}

	if rerr := runInvestigation(context.Background(), cfg, nil, invID); rerr != nil {
		t.Fatalf("runInvestigation: %v", rerr)
	}
	if *judgeCalls != 1 {
		t.Errorf("judge calls = %d, want exactly 1", *judgeCalls)
	}
	if calls != 1 {
		t.Errorf("analyze calls = %d, want 1 (complete answer settles at once)", calls)
	}
	msgs, _ := listCameraInvestigationMessages(db, invID)
	if n := countAnswerQualityNotes(msgs); n != 0 {
		t.Errorf("answer_quality notes = %d, want 0 (complete verdict)", n)
	}
	if inv, _ := getCameraInvestigation(db, invID); inv.Status != "answered" {
		t.Errorf("status = %q, want answered", inv.Status)
	}
}

// TestRunInvestigationCompletenessFailOpen: every judge failure mode — a transport
// error and an "incomplete but nothing actionable" verdict (empty / whitespace
// missing, i.e. the empty-object case) — FAILS OPEN: the answer settles with no
// corrective round. The judge is still consulted exactly once.
func TestRunInvestigationCompletenessFailOpen(t *testing.T) {
	cases := []struct {
		name     string
		complete bool
		missing  string
		jerr     error
	}{
		{"transport error", false, "would-be-scope", errors.New("judge upstream unreachable")},
		{"empty verdict object", false, "", nil},
		{"whitespace-only missing", false, "   ", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := newCamViewTestConfig(t)
			cfg.CameraAnswerJudgeDisabled = false
			invID := seedInterruptInvestigation(t, cfg, "summarize 07:00-09:00 across all cameras")
			db, err := openProxyDB(cfg)
			if err != nil {
				t.Fatalf("openProxyDB: %v", err)
			}
			defer db.Close()

			judgeCalls := scriptCompletenessJudge(t, tc.complete, tc.missing, tc.jerr)

			orig := camInvestigateAnalyzeFn
			defer func() { camInvestigateAnalyzeFn = orig }()
			calls := 0
			camInvestigateAnalyzeFn = func(ctx context.Context, c config, alias, sys, user string, images []string) (string, []camAnalyzeAttempt, error) {
				calls++
				return `{"thought":"answering","action":{"type":"answer","answer":"Between 07:00 and 09:00 it was quiet."}}`, nil, nil
			}

			if rerr := runInvestigation(context.Background(), cfg, nil, invID); rerr != nil {
				t.Fatalf("runInvestigation: %v", rerr)
			}
			if *judgeCalls != 1 {
				t.Errorf("judge calls = %d, want exactly 1", *judgeCalls)
			}
			if calls != 1 {
				t.Errorf("analyze calls = %d, want 1 (fail open → settle, no round)", calls)
			}
			msgs, _ := listCameraInvestigationMessages(db, invID)
			if n := countAnswerQualityNotes(msgs); n != 0 {
				t.Errorf("answer_quality notes = %d, want 0 (judge must fail open)", n)
			}
			if inv, _ := getCameraInvestigation(db, invID); inv.Status != "answered" {
				t.Errorf("status = %q, want answered", inv.Status)
			}
		})
	}
}

// TestRunInvestigationCompletenessCombinedWithBC proves guard D folds into the SAME
// single corrective round as B (no evidence) and C (Arabic answered in English):
// one answer_quality note naming ALL THREE defects, exactly one judge call, one
// corrective round, then the corrected answer settles.
func TestRunInvestigationCompletenessCombinedWithBC(t *testing.T) {
	cfg := newCamViewTestConfig(t)
	cfg.CameraAnswerJudgeDisabled = false
	invID := seedInterruptInvestigation(t, cfg, "لخّص ما حدث بين الساعة 07:00 و09:00 على كل الكاميرات")
	seedMintedMedia(t, cfg, invID)
	db, err := openProxyDB(cfg)
	if err != nil {
		t.Fatalf("openProxyDB: %v", err)
	}
	defer db.Close()

	const missing = "only a single frame near 07:00 was described, not the whole 07:00-09:00 window"
	judgeCalls := scriptCompletenessJudge(t, false, missing, nil)

	orig := camInvestigateAnalyzeFn
	defer func() { camInvestigateAnalyzeFn = orig }()
	calls := 0
	camInvestigateAnalyzeFn = func(ctx context.Context, c config, alias, sys, user string, images []string) (string, []camAnalyzeAttempt, error) {
		calls++
		if calls == 1 {
			return `{"thought":"quick answer","action":{"type":"answer","answer":"A car parked at 07:01."}}`, nil, nil
		}
		return `{"thought":"restating fully in Arabic with evidence","action":{"type":"answer","answer":"بين 07:00 و09:00: توقفت سيارة 07:01، وصل الموظفون 07:40، وتسليم 08:30.","evidence":[{"media_url":"/camera/media/tokSeed1"}]}}`, nil, nil
	}

	if rerr := runInvestigation(context.Background(), cfg, nil, invID); rerr != nil {
		t.Fatalf("runInvestigation: %v", rerr)
	}
	if *judgeCalls != 1 {
		t.Errorf("judge calls = %d, want exactly 1", *judgeCalls)
	}
	if calls != 2 {
		t.Errorf("analyze calls = %d, want 2 — B+C+D must be ONE round", calls)
	}
	msgs, _ := listCameraInvestigationMessages(db, invID)
	if n := countAnswerQualityNotes(msgs); n != 1 {
		t.Fatalf("answer_quality notes = %d, want exactly 1", n)
	}
	for _, m := range msgs {
		if m.Role == "system" && m.ToolName == "answer_quality" {
			if !strings.Contains(m.Content, "evidence[]") {
				t.Errorf("combined note must name the evidence defect: %q", m.Content)
			}
			if !strings.Contains(m.Content, "Arabic") {
				t.Errorf("combined note must name the language defect: %q", m.Content)
			}
			if !strings.Contains(m.Content, missing) || !strings.Contains(m.Content, "CONTINUE INVESTIGATING") {
				t.Errorf("combined note must name the completeness defect + continue instruction: %q", m.Content)
			}
		}
	}
	if inv, _ := getCameraInvestigation(db, invID); inv.Status != "answered" {
		t.Errorf("status = %q, want answered", inv.Status)
	}
}

// TestRunInvestigationCompletenessSubInvestigationSkipped: a delegated child run
// (ParentID set) is never judged, even with the judge enabled — a child answers
// inside its lead's delegate turn.
func TestRunInvestigationCompletenessSubInvestigationSkipped(t *testing.T) {
	cfg := newCamViewTestConfig(t)
	cfg.CameraAnswerJudgeDisabled = false
	db, err := openProxyDB(cfg)
	if err != nil {
		t.Fatalf("openProxyDB: %v", err)
	}
	defer db.Close()
	siteID, serr := insertCameraSite(db, camSite{Name: "Sub-Investigation Site"})
	if serr != nil {
		t.Fatalf("insertCameraSite: %v", serr)
	}
	parentID, _ := insertCameraInvestigation(db, camInvestigation{SiteID: siteID, Question: "lead question", Status: "running"})
	childID, cerr := insertCameraInvestigation(db, camInvestigation{SiteID: siteID, ParentID: parentID, Question: "summarize 07:00-09:00", Status: "running"})
	if cerr != nil {
		t.Fatalf("insert child: %v", cerr)
	}
	camAppendInvestigateMessage(db, childID, "operator", "summarize 07:00-09:00", "", "", 0, nil)

	judgeCalls := scriptCompletenessJudge(t, false, "should never be consulted", nil)

	orig := camInvestigateAnalyzeFn
	defer func() { camInvestigateAnalyzeFn = orig }()
	camInvestigateAnalyzeFn = func(ctx context.Context, c config, alias, sys, user string, images []string) (string, []camAnalyzeAttempt, error) {
		return `{"thought":"child answer","action":{"type":"answer","answer":"Between 07:00 and 09:00 it was quiet."}}`, nil, nil
	}

	if rerr := runInvestigation(context.Background(), cfg, nil, childID); rerr != nil {
		t.Fatalf("runInvestigation: %v", rerr)
	}
	if *judgeCalls != 0 {
		t.Errorf("judge calls = %d, want 0 (sub-investigations are never judged)", *judgeCalls)
	}
	if inv, _ := getCameraInvestigation(db, childID); inv.Status != "answered" {
		t.Errorf("status = %q, want answered", inv.Status)
	}
}

// TestRunInvestigationCompletenessKillSwitch: with PROXY_CAM_ANSWER_JUDGE_DISABLED
// the judge is never consulted and the answer settles unchanged.
func TestRunInvestigationCompletenessKillSwitch(t *testing.T) {
	cfg := newCamViewTestConfig(t)
	cfg.CameraAnswerJudgeDisabled = true // the kill switch
	invID := seedInterruptInvestigation(t, cfg, "summarize 07:00-09:00 across all cameras")
	db, err := openProxyDB(cfg)
	if err != nil {
		t.Fatalf("openProxyDB: %v", err)
	}
	defer db.Close()

	judgeCalls := scriptCompletenessJudge(t, false, "would-be-scope", nil)

	orig := camInvestigateAnalyzeFn
	defer func() { camInvestigateAnalyzeFn = orig }()
	calls := 0
	camInvestigateAnalyzeFn = func(ctx context.Context, c config, alias, sys, user string, images []string) (string, []camAnalyzeAttempt, error) {
		calls++
		return `{"thought":"answering","action":{"type":"answer","answer":"Between 07:00 and 09:00 it was quiet."}}`, nil, nil
	}

	if rerr := runInvestigation(context.Background(), cfg, nil, invID); rerr != nil {
		t.Fatalf("runInvestigation: %v", rerr)
	}
	if *judgeCalls != 0 {
		t.Errorf("judge calls = %d, want 0 (kill switch)", *judgeCalls)
	}
	if calls != 1 {
		t.Errorf("analyze calls = %d, want 1 (settle unchanged)", calls)
	}
	msgs, _ := listCameraInvestigationMessages(db, invID)
	if n := countAnswerQualityNotes(msgs); n != 0 {
		t.Errorf("answer_quality notes = %d, want 0 (judge disabled)", n)
	}
	if inv, _ := getCameraInvestigation(db, invID); inv.Status != "answered" {
		t.Errorf("status = %q, want answered", inv.Status)
	}
}

// TestRunInvestigationCompletenessMaxTurnsEdge: when the corrective round would
// exceed the cumulative turn cap (maxTurns=1, so turnsSoFar+1 is not < maxTurns),
// the guard block is skipped entirely — the imperfect answer is accepted as-is and
// the judge is NOT spent (there is no budget to act on its verdict).
func TestRunInvestigationCompletenessMaxTurnsEdge(t *testing.T) {
	cfg := newCamViewTestConfig(t)
	cfg.CameraAnswerJudgeDisabled = false
	cfg.CameraInvestigateMaxTurns = 1 // no room for a corrective round
	invID := seedInterruptInvestigation(t, cfg, "summarize 07:00-09:00 across all cameras")
	db, err := openProxyDB(cfg)
	if err != nil {
		t.Fatalf("openProxyDB: %v", err)
	}
	defer db.Close()

	judgeCalls := scriptCompletenessJudge(t, false, "the rest of the window", nil)

	orig := camInvestigateAnalyzeFn
	defer func() { camInvestigateAnalyzeFn = orig }()
	calls := 0
	camInvestigateAnalyzeFn = func(ctx context.Context, c config, alias, sys, user string, images []string) (string, []camAnalyzeAttempt, error) {
		calls++
		return `{"thought":"only saw the start","action":{"type":"answer","answer":"At 07:01 a car parked."}}`, nil, nil
	}

	if rerr := runInvestigation(context.Background(), cfg, nil, invID); rerr != nil {
		t.Fatalf("runInvestigation: %v", rerr)
	}
	if *judgeCalls != 0 {
		t.Errorf("judge calls = %d, want 0 (no budget for a round → judge not spent)", *judgeCalls)
	}
	if calls != 1 {
		t.Errorf("analyze calls = %d, want 1 (imperfect answer accepted as-is)", calls)
	}
	msgs, _ := listCameraInvestigationMessages(db, invID)
	if n := countAnswerQualityNotes(msgs); n != 0 {
		t.Errorf("answer_quality notes = %d, want 0 (turn cap)", n)
	}
	if inv, _ := getCameraInvestigation(db, invID); inv.Status != "answered" {
		t.Errorf("status = %q, want answered", inv.Status)
	}
}

// TestCamAnswerCompletenessVerdictParse pins the tolerant parse behind the default
// judge: extractFirstJSONObject pulls the object out of surrounding prose and the
// flexBool "complete" decodes a stringified boolean.
func TestCamAnswerCompletenessVerdictParse(t *testing.T) {
	for _, raw := range []string{
		`{"complete":false,"missing":"the 08:00-09:00 hour"}`,
		"here is my verdict: {\"complete\":\"false\",\"missing\":\"the 08:00-09:00 hour\"} — done",
	} {
		obj, ok := extractFirstJSONObject(raw)
		if !ok {
			t.Fatalf("no JSON object found in %q", raw)
		}
		var v camAnswerCompletenessVerdict
		if err := json.Unmarshal(obj, &v); err != nil {
			t.Fatalf("decode %q: %v", raw, err)
		}
		if bool(v.Complete) {
			t.Errorf("complete = true, want false for %q", raw)
		}
		if v.Missing == "" {
			t.Errorf("missing decoded empty for %q", raw)
		}
	}
}

// TestCamJudgeAnswerCompletenessFunnel exercises the fail-open funnel directly (no
// runInvestigation): a transport error, a blank "missing", and a complete verdict
// all resolve to "" (settle); only complete=false WITH real missing text produces a
// defect string.
func TestCamJudgeAnswerCompletenessFunnel(t *testing.T) {
	orig := camAnswerCompletenessJudgeFn
	defer func() { camAnswerCompletenessJudgeFn = orig }()
	set := func(complete bool, missing string, jerr error) {
		camAnswerCompletenessJudgeFn = func(ctx context.Context, cfg config, alias, question, answer string) (bool, string, error) {
			return complete, missing, jerr
		}
	}
	call := func() string {
		return camJudgeAnswerCompleteness(context.Background(), config{}, "inv_test", "alias", "q", "ans")
	}

	set(false, "actionable", errors.New("boom"))
	if got := call(); got != "" {
		t.Errorf("transport error: got %q, want \"\" (fail open)", got)
	}
	set(false, "   ", nil)
	if got := call(); got != "" {
		t.Errorf("blank missing: got %q, want \"\" (fail open)", got)
	}
	set(true, "ignored when complete", nil)
	if got := call(); got != "" {
		t.Errorf("complete verdict: got %q, want \"\"", got)
	}
	set(false, "the whole 07:00-09:00 window", nil)
	if got := call(); got != "the whole 07:00-09:00 window" {
		t.Errorf("actionable incomplete verdict: got %q, want the missing text", got)
	}
}

// TestCamAnswerGuardAlreadyFired pins the one-shot reconstruction: within a budget
// epoch the persisted answer_quality note means the corrective round already fired
// (so a resumed pass must NOT re-fire it), a non-ask_operator operator row starts a
// fresh epoch and clears it, a reply to an ask_operator continues the same epoch,
// and unrelated system rows (auto_requeue, analysis_retry) neither set nor clear it.
func TestCamAnswerGuardAlreadyFired(t *testing.T) {
	op := func(tool string) camInvestigationMessage {
		return camInvestigationMessage{Role: "operator", ToolName: tool}
	}
	ai := func(tool string) camInvestigationMessage { return camInvestigationMessage{Role: "ai", ToolName: tool} }
	sysrow := func(tool string) camInvestigationMessage {
		return camInvestigationMessage{Role: "system", ToolName: tool}
	}
	cases := []struct {
		name string
		msgs []camInvestigationMessage
		want bool
	}{
		{"fresh question, no round yet", []camInvestigationMessage{op("")}, false},
		{"round fired this epoch (the requeue case)", []camInvestigationMessage{op(""), ai("answer"), sysrow("answer_quality")}, true},
		{"round fired then requeued (auto_requeue after the note)", []camInvestigationMessage{op(""), ai("answer"), sysrow("answer_quality"), sysrow("auto_requeue")}, true},
		{"new operator interrupt clears the one-shot", []camInvestigationMessage{op(""), ai("answer"), sysrow("answer_quality"), op("view_reply")}, false},
		{"ask_operator reply continues the same epoch", []camInvestigationMessage{op(""), ai("answer"), sysrow("answer_quality"), ai("ask_operator"), op("")}, true},
		{"analysis_retry row is not a corrective round", []camInvestigationMessage{op(""), sysrow("analysis_retry"), ai("snapshot")}, false},
		{"only the note in the LATEST epoch counts", []camInvestigationMessage{op(""), ai("answer"), sysrow("answer_quality"), op(""), ai("snapshot")}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := camAnswerGuardAlreadyFired(tc.msgs); got != tc.want {
				t.Errorf("camAnswerGuardAlreadyFired = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestRunInvestigationAnswerGuardSurvivesRequeue is the never-die regression for the
// finding: a pass whose per-pass budget died right AFTER the single corrective round
// persisted its answer_quality note is re-claimed and runInvestigation restarts. The
// one-shot must be reconstructed from that persisted note (NOT reset to false), so
// the resumed pass spends ZERO judge calls and fires NO second corrective round — it
// accepts the model's next answer even though the scripted judge would still vote
// "incomplete". This holds the "exactly one corrective round" + "zero judge calls once
// the round fired" guarantees across a requeue.
func TestRunInvestigationAnswerGuardSurvivesRequeue(t *testing.T) {
	cfg := newCamViewTestConfig(t)
	cfg.CameraAnswerJudgeDisabled = false
	invID := seedInterruptInvestigation(t, cfg, "give me a summary of what happened 07:00-09:00 across all cameras")
	db, err := openProxyDB(cfg)
	if err != nil {
		t.Fatalf("openProxyDB: %v", err)
	}
	defer db.Close()

	// Reconstruct the exact mid-correction transcript a requeue would resume from:
	// the model's rejected first answer, the corrective answer_quality note, and the
	// auto_requeue breadcrumb left when the per-pass budget then exhausted. The row is
	// already "running" (a re-claim), so runInvestigation executes.
	camAppendInvestigateMessage(db, invID, "ai", "At 07:01 a car parked near gate 2.", "answer", "", 0, nil)
	camAppendInvestigateMessage(db, invID, "system",
		"Final answer needs one correction before it is delivered: the answer does not cover the full scope the operator asked about — only 07:00-07:06 was examined. CONTINUE INVESTIGATING to cover the missing scope.",
		"answer_quality", "", 0, nil)
	camAppendInvestigateMessage(db, invID, "system",
		"Time budget for this pass was exhausted before a final answer. Auto-resuming (attempt 1 of 5).", "auto_requeue", "", 0, nil)

	// The judge would STILL reject on scope — but it must never be consulted on the
	// resumed pass, because the round already fired.
	judgeCalls := scriptCompletenessJudge(t, false, "still only saw the first six minutes", nil)

	orig := camInvestigateAnalyzeFn
	defer func() { camInvestigateAnalyzeFn = orig }()
	calls := 0
	camInvestigateAnalyzeFn = func(ctx context.Context, c config, alias, sys, user string, images []string) (string, []camAnalyzeAttempt, error) {
		calls++
		return `{"thought":"resuming after requeue","action":{"type":"answer","answer":"Across 07:00-09:00: a car parked at 07:01, staff arrived 07:40, a delivery at 08:30."}}`, nil, nil
	}

	if rerr := runInvestigation(context.Background(), cfg, nil, invID); rerr != nil {
		t.Fatalf("runInvestigation: %v", rerr)
	}
	if *judgeCalls != 0 {
		t.Errorf("judge calls = %d, want 0 (the round already fired before the requeue — no second judge call)", *judgeCalls)
	}
	if calls != 1 {
		t.Errorf("analyze calls = %d, want 1 (the resumed answer is accepted as-is, no second corrective round)", calls)
	}
	msgs, _ := listCameraInvestigationMessages(db, invID)
	if n := countAnswerQualityNotes(msgs); n != 1 {
		t.Errorf("answer_quality notes = %d, want exactly 1 (the one from before the requeue — no second round)", n)
	}
	last := msgs[len(msgs)-1]
	if last.Role != "ai" || last.ToolName != "answer" || !strings.Contains(last.Content, "08:30") {
		t.Errorf("final row = %+v, want the resumed answer accepted", last)
	}
	if inv, _ := getCameraInvestigation(db, invID); inv.Status != "answered" {
		t.Errorf("status = %q, want answered", inv.Status)
	}
}

// TestRunInvestigationAnswerGuardRequeueThenInterrupt proves the one-shot RESETS when
// a genuinely new operator interrupt lands after a round already fired: the resumed
// pass reads a fresh question (its own budget epoch), so the guard-D judge is
// consulted again for the NEW scope and one fresh corrective round is allowed — the
// live drain reset and the reconstruction agree on the epoch boundary.
func TestRunInvestigationAnswerGuardRequeueThenInterrupt(t *testing.T) {
	cfg := newCamViewTestConfig(t)
	cfg.CameraAnswerJudgeDisabled = false
	invID := seedInterruptInvestigation(t, cfg, "summarize 07:00-09:00 across all cameras")
	db, err := openProxyDB(cfg)
	if err != nil {
		t.Fatalf("openProxyDB: %v", err)
	}
	defer db.Close()

	// A prior epoch that already fired its corrective round, THEN a new operator
	// interrupt (a fresh question) — this starts a new budget epoch, so the one-shot
	// is spent no longer.
	camAppendInvestigateMessage(db, invID, "ai", "At 07:01 a car parked.", "answer", "", 0, nil)
	camAppendInvestigateMessage(db, invID, "system", "Final answer needs one correction: scope. CONTINUE INVESTIGATING.", "answer_quality", "", 0, nil)
	camAppendInvestigateMessage(db, invID, "operator", "now also cover 09:00-10:00", "", "", 0, nil)

	judgeCalls := scriptCompletenessJudge(t, false, "the new 09:00-10:00 hour is not covered", nil)

	orig := camInvestigateAnalyzeFn
	defer func() { camInvestigateAnalyzeFn = orig }()
	calls := 0
	camInvestigateAnalyzeFn = func(ctx context.Context, c config, alias, sys, user string, images []string) (string, []camAnalyzeAttempt, error) {
		calls++
		if calls == 1 {
			return `{"thought":"quick reply","action":{"type":"answer","answer":"Nothing at 09:15."}}`, nil, nil
		}
		return `{"thought":"covered the new hour","action":{"type":"answer","answer":"Across 09:00-10:00: quiet, one delivery at 09:40."}}`, nil, nil
	}

	if rerr := runInvestigation(context.Background(), cfg, nil, invID); rerr != nil {
		t.Fatalf("runInvestigation: %v", rerr)
	}
	if *judgeCalls != 1 {
		t.Errorf("judge calls = %d, want 1 (new epoch after the interrupt earns a fresh judge call)", *judgeCalls)
	}
	if calls != 2 {
		t.Errorf("analyze calls = %d, want 2 (one fresh corrective round for the new scope)", calls)
	}
	msgs, _ := listCameraInvestigationMessages(db, invID)
	if n := countAnswerQualityNotes(msgs); n != 2 {
		t.Errorf("answer_quality notes = %d, want 2 (one per epoch: the seeded round + the new one)", n)
	}
	if inv, _ := getCameraInvestigation(db, invID); inv.Status != "answered" {
		t.Errorf("status = %q, want answered", inv.Status)
	}
}
