package main

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// longDialogue builds a conversation whose MESSAGES alone are large, which is
// the only case fitting cannot answer.
func longDialogue(turns int) anthropicRequest {
	var msgs []anthropicMessage
	for i := 0; i < turns; i++ {
		msgs = append(msgs,
			anthropicMessage{Role: "user", Content: fmt.Sprintf("سؤال الزبونة رقم %d: %s", i, strings.Repeat("تفاصيل كثيرة عن طلبها. ", 40))},
			anthropicMessage{Role: "assistant", Content: fmt.Sprintf("رد المساعدة رقم %d: %s", i, strings.Repeat("شرح مفصل عن الخدمات. ", 40))},
		)
	}
	return anthropicRequest{System: "You are Zeina.", Messages: msgs}
}

func TestAgyCompactOnlyWhenOverBudget(t *testing.T) {
	orig := agyResolveFn
	defer func() { agyResolveFn = orig }()
	called := 0
	agyResolveFn = func(_ context.Context, _ config, _ []mediaPart, _ string, _ string) (agyResult, error) {
		called++
		return agyResult{Ok: true, Response: "- brief"}, nil
	}
	in := agyGenInputFromAnthropic(longDialogue(3), 0)
	// Budget large enough: no summarisation, no model call.
	if _, did := agyCompactIfNeeded(context.Background(), config{AgyCompact: true, AgyPromptBudget: 500000}, in); did || called != 0 {
		t.Fatalf("compaction ran when it was not needed (did=%v calls=%d)", did, called)
	}
	// Disabled: never runs, however tight the budget.
	if _, did := agyCompactIfNeeded(context.Background(), config{AgyCompact: false, AgyPromptBudget: 1000}, in); did || called != 0 {
		t.Fatalf("compaction ran while disabled (did=%v calls=%d)", did, called)
	}
}

func TestAgyCompactReplacesOldTurnsAndKeepsRecentOnes(t *testing.T) {
	orig := agyResolveFn
	defer func() { agyResolveFn = orig }()
	var material string
	calls := 0
	agyResolveFn = func(_ context.Context, _ config, _ []mediaPart, prompt, _ string) (agyResult, error) {
		calls++
		material = prompt
		return agyResult{Ok: true, Response: "- الزبونة سألت عن العضوية\n- تم حجز موعد 2026-09-10 الساعة 13:00"}, nil
	}
	req := longDialogue(12)
	in := agyGenInputFromAnthropic(req, 0)
	cfg := config{AgyCompact: true, AgyPromptBudget: 30000}
	out, did := agyCompactIfNeeded(context.Background(), cfg, in)
	if !did {
		t.Fatal("expected compaction")
	}
	if calls != 1 {
		t.Fatalf("summariser called %d times, want 1", calls)
	}
	// The material handed to the summariser is the OLD part only.
	if !strings.Contains(material, "سؤال الزبونة رقم 0") {
		t.Fatal("the summariser was not given the oldest turns")
	}
	if strings.Contains(material, "سؤال الزبونة رقم 11") {
		t.Fatal("the summariser was given turns that must stay verbatim")
	}
	// The result: one system note, then the recent turns unchanged.
	if out.Turns[0].Kind != agyTurnSystemNote || !strings.Contains(out.Turns[0].Text, agyBriefMarker) {
		t.Fatalf("first turn is not the brief: %+v", out.Turns[0])
	}
	if !strings.Contains(out.Turns[0].Text, "تم حجز موعد 2026-09-10") {
		t.Fatal("the brief content is missing")
	}
	if len(out.Turns) >= len(in.Transcript.Turns) {
		t.Fatalf("transcript did not shrink: %d → %d", len(in.Transcript.Turns), len(out.Turns))
	}
	// The last four customer messages, and everything after them, survive verbatim.
	rendered := renderAgyTranscript(out, 0, false)
	for i := 8; i < 12; i++ {
		if !strings.Contains(rendered, fmt.Sprintf("سؤال الزبونة رقم %d", i)) {
			t.Fatalf("recent customer message %d was summarised away", i)
		}
	}
	// And the compacted transcript is what makes the prompt fit again.
	in.Transcript = out
	if p, _ := renderAgyPromptFitted(cfg, in.System, nil, in.Tools, in.Transcript); len(p) > cfg.AgyPromptBudget {
		t.Fatalf("still %d bytes over budget after compaction", len(p)-cfg.AgyPromptBudget)
	}
}

func TestAgyCompactReusesTheBriefForTheSameMaterial(t *testing.T) {
	orig := agyResolveFn
	defer func() { agyResolveFn = orig }()
	calls := 0
	agyResolveFn = func(_ context.Context, _ config, _ []mediaPart, _ string, _ string) (agyResult, error) {
		calls++
		return agyResult{Ok: true, Response: fmt.Sprintf("- brief number %d", calls)}, nil
	}
	cfg := config{AgyCompact: true, AgyPromptBudget: 30000}
	in := agyGenInputFromAnthropic(longDialogue(13), 0) // material no other test has cached
	first, _ := agyCompactIfNeeded(context.Background(), cfg, in)
	second, _ := agyCompactIfNeeded(context.Background(), cfg, in)
	if calls != 1 {
		t.Fatalf("the brief was rewritten (%d calls); the same material must be reused", calls)
	}
	if first.Turns[0].Text != second.Turns[0].Text {
		t.Fatal("cached brief differs between calls")
	}
}

func TestAgyCompactCutIndexKeepsShortConversationsWhole(t *testing.T) {
	in := agyGenInputFromAnthropic(longDialogue(3), 0)
	if cut := agyCompactCutIndex(in.Transcript, agyCompactKeepCustomerTurns); cut != 0 {
		t.Fatalf("a 3-turn conversation should not be summarised, cut=%d", cut)
	}
}

func customerTurns(n int) agyTranscript {
	var tr agyTranscript
	for i := 0; i < n; i++ {
		tr.Turns = append(tr.Turns, agyTurn{Kind: agyTurnCustomer, Text: fmt.Sprintf("q%d", i)}, agyTurn{Kind: agyTurnAssistant, Text: fmt.Sprintf("a%d", i)})
	}
	return tr
}

// The cut for summarising by choice moves in steps of keep customer messages,
// so the same brief serves keep turns in a row, and the verbatim tail always
// holds between keep and 2·keep−1 of them.
func TestAgyCompactStepCutMovesInSteps(t *testing.T) {
	const keep = 4
	for _, c := range []struct{ customers, tailFrom int }{
		{3, 0}, {5, 0}, {7, 0}, {8, 4}, {11, 4}, {12, 8}, {15, 8}, {16, 12},
	} {
		tr := customerTurns(c.customers)
		if got := agyCompactStepCutIndex(tr, keep); got != 2*c.tailFrom {
			t.Errorf("%d customer messages: cut at turn %d, want %d (customer message %d)", c.customers, got, 2*c.tailFrom, c.tailFrom)
		}
	}
}

// Messages the business sent before the customer ever wrote are older than all
// of the dialogue, however short the dialogue is.
func TestAgyCompactCutCoversWhatPrecedesTheFirstMessage(t *testing.T) {
	tr := agyTranscript{Turns: []agyTurn{
		{Kind: agyTurnAssistant, Text: "XMED check-in: a notice for staff"},
		{Kind: agyTurnAssistant, Text: "A follow-up case was opened"},
	}}
	tr.Turns = append(tr.Turns, customerTurns(3).Turns...)
	if cut := agyCompactCutIndex(tr, agyCompactKeepCustomerTurns); cut != 2 {
		t.Fatalf("cut=%d, want 2 (the notices before the first customer message)", cut)
	}
	if cut := agyCompactStepCutIndex(tr, agyCompactKeepCustomerTurns); cut != 2 {
		t.Fatalf("step cut=%d, want 2", cut)
	}
}

// starvedRequest is a long dialogue whose newest turn fetched a large result:
// the prompt can be made to fit, but only by cutting that result to a sliver.
func starvedRequest(dialogue func(i int) (string, string)) anthropicRequest {
	msgs := []anthropicMessage{{Role: "user", Content: "[AUTO-CONTEXT] customer_id=48213, branch_id=2"}}
	for i := 0; i < 10; i++ {
		q, a := dialogue(i)
		msgs = append(msgs, anthropicMessage{Role: "user", Content: q}, anthropicMessage{Role: "assistant", Content: a})
	}
	msgs = append(msgs,
		anthropicMessage{Role: "user", Content: "كم سعر البوتوكس بعمان؟"},
		anthropicMessage{Role: "assistant", Content: []any{map[string]any{"type": "tool_use", "id": "pk", "name": "get_customer_packages", "input": map[string]any{}}}},
		anthropicMessage{Role: "user", Content: []any{map[string]any{"type": "tool_result", "tool_use_id": "pk", "content": buildPackagesResult(12, 8)}}},
	)
	return anthropicRequest{System: "You are Zeina.", Messages: msgs}
}

func starvingBudget(t *testing.T, in agyGenInput) int {
	t.Helper()
	unfitted, _ := renderAgyPromptFitted(config{AgyPromptBudget: 1 << 30}, in.System, in.Temperature, in.Tools, in.Transcript)
	full := len(unfitted)
	result := 0
	for _, tt := range in.Transcript.Turns {
		if tt.Kind == agyTurnToolResult {
			result = len(tt.Text)
		}
	}
	return full - result*3/4
}

func TestAgyCompactWhenTheTurnsResultsWouldBeGutted(t *testing.T) {
	orig := agyResolveFn
	defer func() { agyResolveFn = orig }()
	var material string
	agyResolveFn = func(_ context.Context, _ config, _ []mediaPart, prompt, _ string) (agyResult, error) {
		material = prompt
		return agyResult{Ok: true, Response: "- الزبونة سألت عن أسعار الليزر"}, nil
	}
	in := agyGenInputFromAnthropic(starvedRequest(func(i int) (string, string) {
		return fmt.Sprintf("سؤال رقم %d: %s", i, strings.Repeat("تفاصيل كثيرة عن طلبها. ", 30)),
			fmt.Sprintf("رد رقم %d: %s", i, strings.Repeat("شرح مفصل عن الخدمات. ", 30))
	}), 0)
	cfg := config{AgyCompact: true}
	cfg.AgyPromptBudget = starvingBudget(t, in)
	p, before := renderAgyPromptFittedTranscript(cfg, in.System, nil, in.Tools, in.Transcript)
	if len(p) > cfg.AgyPromptBudget {
		t.Fatalf("fixture: fitting alone should make the prompt fit (%d > %d)", len(p), cfg.AgyPromptBudget)
	}
	if last := before.Turns[len(before.Turns)-1].Text; !strings.Contains(last, agyStubKey) && !strings.Contains(last, agyOmissionMarker) {
		t.Fatalf("fixture: without a brief the result should have to give up details:\n%s", truncateString(last, 600))
	}
	out, did := agyCompactIfNeeded(context.Background(), cfg, in)
	if !did {
		t.Fatal("the turn's own result was gutted while older dialogue sat intact, and nothing was summarised")
	}
	if strings.Contains(material, "customer_id=48213") {
		t.Fatal("a system note went to the summariser; it must be carried over verbatim")
	}
	if !strings.Contains(renderAgyTranscript(out, 0, false), "customer_id=48213, branch_id=2") {
		t.Fatal("the system note from the summarised part was lost")
	}
	in.Transcript = out
	_, fitted := renderAgyPromptFittedTranscript(cfg, in.System, nil, in.Tools, in.Transcript)
	last := fitted.Turns[len(fitted.Turns)-1].Text
	if strings.Contains(last, agyStubKey) || strings.Contains(last, agyOmissionMarker) {
		t.Fatalf("after the brief the turn's result still gave up details:\n%s", truncateString(last, 600))
	}
	if records := agyAsRecords(t, last); strings.Count(records, `"reservation_id"`) != 12*8 {
		t.Fatalf("after the brief %d of %d reservations reach the model", strings.Count(records, `"reservation_id"`), 12*8)
	}
}

// A brief costs a model call; when little dialogue precedes the recent turns
// it cannot buy the result much room, and none is written.
func TestAgyCompactNotForASmallGain(t *testing.T) {
	orig := agyResolveFn
	defer func() { agyResolveFn = orig }()
	calls := 0
	agyResolveFn = func(_ context.Context, _ config, _ []mediaPart, _ string, _ string) (agyResult, error) {
		calls++
		return agyResult{Ok: true, Response: "- brief"}, nil
	}
	in := agyGenInputFromAnthropic(starvedRequest(func(i int) (string, string) {
		return fmt.Sprintf("سؤال %d", i), fmt.Sprintf("رد %d", i)
	}), 0)
	cfg := config{AgyCompact: true}
	cfg.AgyPromptBudget = starvingBudget(t, in)
	if _, did := agyCompactIfNeeded(context.Background(), cfg, in); did || calls != 0 {
		t.Fatalf("summarised %d bytes of dialogue (did=%v calls=%d)", agyDialogueBytes(in.Transcript.Turns), did, calls)
	}
}

func TestAgyCompactSurvivesASummariserFailure(t *testing.T) {
	orig := agyResolveFn
	defer func() { agyResolveFn = orig }()
	agyResolveFn = func(_ context.Context, _ config, _ []mediaPart, _ string, _ string) (agyResult, error) {
		return agyResult{}, fmt.Errorf("upstream down")
	}
	in := agyGenInputFromAnthropic(longDialogue(14), 0)
	out, did := agyCompactIfNeeded(context.Background(), config{AgyCompact: true, AgyPromptBudget: 30000}, in)
	if did {
		t.Fatal("compaction reported success although the summariser failed")
	}
	if len(out.Turns) != len(in.Transcript.Turns) {
		t.Fatal("the transcript was altered although the brief failed")
	}
}
