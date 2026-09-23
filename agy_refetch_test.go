package main

import (
	"context"
	"strings"
	"testing"
)

// Prod 2026-09-23 (conv 60576, a conversation carrying ~116 system
// notifications): with ~15KB left for tool results, the fitter dropped the
// current turn's packages AND slots, then the repeat guard refused four
// re-fetches of get_customer_packages because it "already SUCCEEDED", and the
// reply told the customer there were no openings while her therapist was free
// all afternoon.

const agySlotsFor1500 = `{"success":true,"data":{"slots":[{"date":"2026-09-27","therapists":[` +
	`{"therapist_id":53722,"available_starts":["13:00","14:00","14:30","15:00","15:30","16:00","16:30","17:00","17:30","19:00","19:30","20:00","20:30"]}],` +
	`"merged_starts":["13:00","14:00","14:30","15:00","15:30","16:00","16:30","17:00","17:30","19:00","19:30","20:00","20:30"]}]}}`

func dropHeavyTurn(repeats int) anthropicRequest {
	msgs := []anthropicMessage{{Role: "user", Content: "بدي أأخر موعدي لبعد الساعة 3"}}
	for i := 0; i < repeats; i++ {
		id := "p" + string(rune('1'+i))
		msgs = append(msgs, toolUse(id, "get_customer_packages", map[string]any{"customer_uuid": "X"}),
			toolResult(id, buildPackagesResult(24, 12)))
	}
	return anthropicRequest{
		Tools: []anthropicTool{
			{Name: "get_customer_packages", InputSchema: map[string]any{"type": "object"}},
			{Name: "create_reservation", InputSchema: map[string]any{"type": "object"}},
		},
		Messages: msgs,
	}
}

func generateOnce(t *testing.T, cfg config, in anthropicRequest, replies ...string) (responsesResponse, *agyGenTrace, []string) {
	t.Helper()
	var prompts []string
	orig := agyResolveFn
	defer func() { agyResolveFn = orig }()
	agyResolveFn = func(_ context.Context, _ config, _ []mediaPart, prompt, _ string) (agyResult, error) {
		prompts = append(prompts, prompt)
		r := replies[0]
		if len(replies) > 1 {
			replies = replies[1:]
		}
		return agyResult{Ok: true, Response: r}, nil
	}
	resp, trace, err := agyGenerate(context.Background(), cfg, agyGenInputFromAnthropic(in, 10))
	if err != nil {
		t.Fatal(err)
	}
	return resp, trace, prompts
}

const refetchPackages = `<tool_call>{"tool":"get_customer_packages","input":{"customer_uuid":"X"}}</tool_call>`

func TestADroppedReadMayBeFetchedAgainOnceInTheSameTurn(t *testing.T) {
	cfg := config{AgyPromptBudget: 9000}
	resp, _, prompts := generateOnce(t, cfg, dropHeavyTurn(1), refetchPackages, "ما قدرت أجيب البيانات")
	if len(prompts) != 1 || !hasAnyToolCall(resp.Output) {
		t.Fatalf("the re-fetch of a dropped read was refused (%d prompts)", len(prompts))
	}
	if !strings.Contains(prompts[0], "you MAY call it again once") {
		t.Fatal("the state block still tells the model never to call it again")
	}

	// A third identical fetch in one turn is a loop, not a recovery.
	_, _, prompts = generateOnce(t, cfg, dropHeavyTurn(2), refetchPackages, "ما قدرت أجيب البيانات")
	if len(prompts) < 2 || !strings.Contains(prompts[1], "do not repeat the same call") {
		t.Fatalf("a third identical fetch was let through (%d prompts)", len(prompts))
	}
}

func TestAnActionIsNeverRepeatedEvenIfItsResultWasCut(t *testing.T) {
	in := dropHeavyTurn(1)
	in.Messages = append(in.Messages,
		toolUse("c1", "create_reservation", map[string]any{"date": "2026-09-27", "time_from": "15:00"}),
		toolResult("c1", `{"success":true,"data":{"status":201,"body":{"reservation_id":1,"note":"`+strings.Repeat("x", 3000)+`"}}}`))
	_, _, prompts := generateOnce(t, config{AgyPromptBudget: 9000}, in,
		`<tool_call>{"tool":"create_reservation","input":{"date":"2026-09-27","time_from":"15:00"}}</tool_call>`,
		"تم تثبيت موعدك الساعة 3:00")
	if len(prompts) < 2 || !strings.Contains(prompts[1], "already SUCCEEDED") {
		t.Fatalf("a booking that already succeeded was allowed to run again (%d prompts)", len(prompts))
	}
}

func TestTheNewestOfferIsNeverDroppedWhole(t *testing.T) {
	in := dropHeavyTurn(2)
	in.Messages = append(in.Messages, toolUse("s1", "get_available_slots", map[string]any{"from_date": "2026-09-27"}),
		toolResult("s1", agySlotsFor1500))
	tr := buildAgyTranscriptFromAnthropic(in)
	budget := len(agySlotsFor1500) / 2 // less than the offer itself: something has to give
	fitted, _ := fitAgyTranscript(tr, 0, false, budget)
	got := renderAgyTranscript(fitted, 0, false)
	for _, want := range []string{`"15:00"`, `"17:30"`, `"19:00"`} {
		if !strings.Contains(got, want) {
			t.Fatalf("the offer lost %s:\n%s", want, got)
		}
	}
	if strings.Count(got, agyDroppedResultNote) < 2 {
		t.Fatalf("the package lists should have gone first:\n%s", got)
	}
}
