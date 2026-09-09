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
