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
//
// Temp sqlite only — no DVRs, no network (the analyze seam is scripted).

import (
	"context"
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
