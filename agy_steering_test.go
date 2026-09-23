package main

import (
	"strings"
	"testing"
)

// The note ConversationSteeringService puts in front of messages it folds into
// a running turn, verbatim.
const connectSteeringNote = "[SYSTEM] The customer sent 1 more message(s) while you were still working. " +
	"They are included below. Answer the whole conversation in ONE reply that covers the original request " +
	"and these additions. Do not apologise for the delay and do not send a separate answer for each message."

func toolUse(id, name string, input map[string]any) anthropicMessage {
	return anthropicMessage{Role: "assistant", Content: []any{map[string]any{"type": "tool_use", "id": id, "name": name, "input": input}}}
}

func toolResult(id, content string) anthropicMessage {
	return anthropicMessage{Role: "user", Content: []any{map[string]any{"type": "tool_result", "tool_use_id": id, "content": content}}}
}

// Prod 2026-09-23 conv 61071, as Connect sent it: the customer confirmed, the
// booking failed, and while the turn was still running she asked about a
// price. Connect appended her question after the failed result. The reply
// "تم تثبيت موعدك" went out: the failure sat before "the customer's latest
// message", outside the turn every check looks at.
func danaSteeredTurn() agyTranscript {
	return buildAgyTranscriptFromAnthropic(anthropicRequest{Messages: []anthropicMessage{
		{Role: "assistant", Content: "هاد ملخص لموعدك للمراجعة، ومش حجز. بتأكدي حجز هالموعد؟"},
		{Role: "user", Content: "تمام اكد"},
		toolUse("c1", "get_customer_packages", map[string]any{"customer_uuid": "65F064B9956111ED"}),
		toolResult("c1", `{"success":true,"data":{"packages":[]}}`),
		toolUse("c2", "create_reservation", map[string]any{"customer_package_id": "262396,277764,277765", "time_from": "15:15"}),
		toolResult("c2", `{"success":false,"data":{"status":400,"body":{"success":false,"error_code":1001,"error":"customer_uuid is required"}},"error":"HTTP 400","status":400}`),
		{Role: "user", Content: connectSteeringNote},
		{Role: "user", Content: "بس جلسة النص رجلين بالعادة ٢٠ مش ٢٥؟"},
		toolUse("c3", "get_offers_list", map[string]any{}),
		toolResult("c3", `{"success":true,"data":[{"offer_id":81}]}`),
	}})
}

func TestAMessageFoldedIntoARunningTurnDoesNotHideItsFailure(t *testing.T) {
	tr := danaSteeredTurn()
	if got := tr.unperformedThisTurn(); len(got) != 1 || got[0] != "create_reservation" {
		t.Fatalf("the failed booking fell out of the turn: %v", got)
	}
	reply := textReply("تم تثبيت موعدك عزيزتي دانا يوم الخميس 24/09/2026 الساعة 3:15 بعد الظهر")
	if got := agyReplyClaimsUnperformed(config{}, tr, reply); len(got) != 1 {
		t.Fatalf("the false confirmation passed: %v", got)
	}
	var failed bool
	for _, c := range tr.currentTurnCallOutcomes() {
		if c.Call.Tool == "create_reservation" && c.Failure != "" {
			failed = true
		}
	}
	if !failed {
		t.Fatal("the state block no longer marks the booking FAILED")
	}
}

func TestTheModelIsAnsweringBothMessages(t *testing.T) {
	tr := danaSteeredTurn()
	got := tr.currentTurnCustomerTexts()
	if len(got) != 2 || got[0] != "تمام اكد" || !strings.HasPrefix(got[1], "بس جلسة") {
		t.Fatalf("the turn is answering %q", got)
	}
	if !strings.Contains(tr.previousAssistantText(), "بتأكدي حجز هالموعد") {
		t.Fatalf("the message she confirmed is lost: %q", tr.previousAssistantText())
	}
	next := renderAgyNextTurn(tr, true)
	if !strings.Contains(next, "Customer: تمام اكد\nCustomer: بس جلسة") {
		t.Fatalf("the next-turn block does not show both messages:\n%s", next)
	}
	for _, tt := range tr.Turns {
		if tt.Kind == agyTurnCustomer && strings.HasPrefix(tt.Text, "[SYSTEM]") {
			t.Fatal("the steering note is shown as something the customer said")
		}
	}
}

// A reply ends a turn. Once the model has told her the booking failed, her
// next message is a new turn and the old failure no longer rules the reply.
func TestAMessageAfterAReplyStillStartsANewTurn(t *testing.T) {
	tr := buildAgyTranscriptFromAnthropic(anthropicRequest{Messages: []anthropicMessage{
		{Role: "user", Content: "تمام اكد"},
		toolUse("c1", "create_reservation", map[string]any{"time_from": "15:15"}),
		toolResult("c1", slotTaken400),
		{Role: "assistant", Content: "للأسف الساعة 3:15 ما عادت متاحة. بناسبك 4:00؟"},
		{Role: "user", Content: "تمام"},
		toolUse("c2", "create_reservation", map[string]any{"time_from": "16:00"}),
		toolResult("c2", `{"success":true,"data":{"reservation_id":392500}}`),
	}})
	if got := tr.unperformedThisTurn(); len(got) != 0 {
		t.Fatalf("a failure the customer was already told about is still held against the new turn: %v", got)
	}
	if got := tr.currentTurnCustomerTexts(); len(got) != 1 || got[0] != "تمام" {
		t.Fatalf("the new turn is answering %q", got)
	}
}

// Only Connect's steering note folds a message into the running turn. Other
// [SYSTEM] turns — a scheduled task's brief, the iteration-limit stop — keep
// the meaning they had.
func TestOtherSystemTurnsAreUntouched(t *testing.T) {
	tr := buildAgyTranscriptFromAnthropic(anthropicRequest{Messages: []anthropicMessage{
		{Role: "user", Content: "[SYSTEM] This is an autonomous scheduled task — there is no human reading your replies."},
	}})
	if len(tr.Turns) != 1 || tr.Turns[0].Kind != agyTurnCustomer {
		t.Fatalf("a scheduled task's brief changed kind: %+v", tr.Turns)
	}
}

// Two calls made in one step come before both results. The first call's
// result sits behind the second call and is still its own.
func TestCallsMadeTogetherKeepTheirOwnResults(t *testing.T) {
	tr := buildAgyTranscriptFromAnthropic(anthropicRequest{Messages: []anthropicMessage{
		{Role: "user", Content: "احجزيلي الثنتين"},
		{Role: "assistant", Content: []any{
			map[string]any{"type": "tool_use", "id": "a", "name": "create_reservation", "input": map[string]any{"time_from": "13:00"}},
			map[string]any{"type": "tool_use", "id": "b", "name": "create_retouch_reservation", "input": map[string]any{"time_from": "14:00"}},
		}},
		{Role: "user", Content: []any{
			map[string]any{"type": "tool_result", "tool_use_id": "a", "content": slotTaken400},
			map[string]any{"type": "tool_result", "tool_use_id": "b", "content": `{"success":true,"data":{"reservation_id":1}}`},
		}},
	}})
	if got := tr.unperformedThisTurn(); len(got) != 1 || got[0] != "create_reservation" {
		t.Fatalf("the first of two calls lost its failed result: %v", got)
	}
}

func TestAFailedResultIsNeverDroppedToFit(t *testing.T) {
	msgs := []anthropicMessage{{Role: "user", Content: "تمام اكد"}}
	msgs = append(msgs,
		toolUse("c1", "create_reservation", map[string]any{"time_from": "15:15"}),
		toolResult("c1", slotTaken400),
	)
	for i, id := range []string{"p1", "p2", "p3"} {
		msgs = append(msgs, toolUse(id, "get_customer_packages", map[string]any{"page": i}), toolResult(id, buildPackagesResult(24, 8)))
	}
	tr := buildAgyTranscriptFromAnthropic(anthropicRequest{Messages: msgs})
	budget := len(renderAgyTranscript(tr, 0, false)) / 40
	fitted, _ := fitAgyTranscript(tr, 0, false, budget)
	got := renderAgyTranscript(fitted, 0, false)
	if !strings.Contains(got, "The selected time slot is not available for this therapist") {
		t.Fatalf("the refusal was cut to fit:\n%s", got)
	}
	var failed bool
	for _, c := range fitted.currentTurnCallOutcomes() {
		if c.Call.Tool == "create_reservation" && c.Failure != "" {
			failed = true
		}
	}
	if !failed {
		t.Fatal("after fitting, the booking is no longer marked FAILED")
	}
}
