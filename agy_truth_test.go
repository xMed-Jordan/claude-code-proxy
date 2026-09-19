package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// The real instructions Shalabi's create_multi_service_reservation returned on
// 2026-09-18, trimmed to the parts the guard reads: the prose that names the
// shape, and the example that shows it.
func multiServiceInstructions() string {
	body := map[string]any{
		"success": true,
		"tool": map[string]any{
			"code": "create_multi_service_reservation",
			"name": "Create Multi-Service Reservation",
			"type": "http_request",
		},
		"instructions": "PURPOSE: the booking call for a multi-area membership visit.\n" +
			"PARAMS — REQUIRED:\n" +
			"- service_ids (array of integers, max 24)\n" +
			"- service_packages: a JSON OBJECT of service_id -> user_package_id.\n" +
			"  REJECTED: [271515,271524]      <- an array. The keys are not optional.\n" +
			"EXAMPLE 1: body {\"customer_uuid\":\"658D0ED4956111ED\",\"service_ids\":[8,10,9]," +
			"\"date\":\"2026-07-22\",\"time_from\":\"16:30\",\"branch_id\":2," +
			"\"service_packages\":{\"8\":55101,\"9\":55103,\"10\":55102}} → 201",
	}
	b, err := json.Marshal(body)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// Prod 2026-09-18 conv 60329: the model sent service_packages as a list holding
// the catalogue id. The API read the list positionally, found no entry under
// key 9, skipped the membership link entirely and answered 201 — so nothing in
// the result could ever teach the model it was wrong. The tool's own example
// says the parameter is an object, and that has to be enough.
func TestAListWhereTheToolDocumentsAnObjectIsRefused(t *testing.T) {
	tr := agyTranscript{Turns: []agyTurn{
		{Kind: agyTurnCustomer, Text: "بدي احجز جلسة بكيني بكرا"},
		{Kind: agyTurnToolCall, Tool: "get_tool_instructions", CallID: "i1",
			Args: canonicalToolArgs(map[string]any{"tool_code": "create_multi_service_reservation"})},
		{Kind: agyTurnToolResult, Tool: "get_tool_instructions", CallID: "i1", Text: multiServiceInstructions()},
	}}

	kinds := agyDocumentedArgKinds(tr)
	if got := kinds["create_multi_service_reservation"]["service_packages"]; got != "object" {
		t.Fatalf("service_packages should be documented as an object, got %q", got)
	}
	if got := kinds["create_multi_service_reservation"]["service_ids"]; got != "array" {
		t.Fatalf("service_ids should be documented as an array, got %q", got)
	}

	// The call that lost هبه's appointment, verbatim in shape.
	bad := canonicalToolArgs(map[string]any{
		"customer_uuid":    "4570AE73A1FE11F1",
		"service_ids":      "[9]",
		"service_packages": "[6033]",
		"date":             "2026-09-19",
		"time_from":        "16:30",
		"branch_id":        "1",
	})
	problems := agyArgShapeProblems(kinds, "create_multi_service_reservation", bad)
	if len(problems) != 1 {
		t.Fatalf("expected exactly one complaint, got %v", problems)
	}
	if !strings.Contains(problems[0], "service_packages") || !strings.Contains(problems[0], "keyed object") {
		t.Fatalf("the complaint must name the parameter and the shape wanted: %s", problems[0])
	}

	// The documented shape passes, and so does the JSON-in-a-string form the
	// models actually send.
	for _, good := range []string{
		canonicalToolArgs(map[string]any{"service_ids": "[9]", "service_packages": `{"9":273828}`}),
		canonicalToolArgs(map[string]any{"service_ids": []any{9}, "service_packages": map[string]any{"9": 273828}}),
	} {
		if p := agyArgShapeProblems(kinds, "create_multi_service_reservation", good); len(p) != 0 {
			t.Fatalf("the documented shape was rejected: %v (args %s)", p, good)
		}
	}
}

// Nothing may be concluded about a tool whose instructions this conversation
// never fetched, about a parameter its examples never show, or about a
// parameter shown both ways.
func TestShapesAreOnlyEnforcedWhereTheToolDocumentedThem(t *testing.T) {
	tr := agyTranscript{Turns: []agyTurn{
		{Kind: agyTurnToolResult, Tool: "get_tool_instructions", CallID: "i1", Text: multiServiceInstructions()},
	}}
	kinds := agyDocumentedArgKinds(tr)

	args := canonicalToolArgs(map[string]any{"service_packages": "[6033]"})
	if p := agyArgShapeProblems(kinds, "create_reservation", args); len(p) != 0 {
		t.Fatalf("a different tool must not inherit the shape: %v", p)
	}
	undocumented := canonicalToolArgs(map[string]any{"notes": "[1,2]", "therapist_id": "53895"})
	if p := agyArgShapeProblems(kinds, "create_multi_service_reservation", undocumented); len(p) != 0 {
		t.Fatalf("undocumented and scalar parameters must be left alone: %v", p)
	}

	// A parameter the examples show as both a list and an object is ambiguous.
	both := agyTranscript{Turns: []agyTurn{{Kind: agyTurnToolResult, Tool: "get_tool_instructions", CallID: "i1",
		Text: `{"tool":{"code":"t"},"instructions":"A: {\"ids\":[1,2]}  B: {\"ids\":{\"a\":1}}"}`}}}
	if got := agyDocumentedArgKinds(both)["t"]["ids"]; got != agyKindAmbiguous {
		t.Fatalf("a parameter shown both ways must be ambiguous, got %q", got)
	}
	if p := agyArgShapeProblems(agyDocumentedArgKinds(both), "t", canonicalToolArgs(map[string]any{"ids": "[1]"})); len(p) != 0 {
		t.Fatalf("an ambiguous parameter must never be enforced: %v", p)
	}
}

// The 400 Shalabi's booking API returned twice to منى on 2026-09-19, seconds
// before the model told her the appointment was confirmed.
const slotTaken400 = `{"success":false,"data":{"status":400,"body":{"success":false,"error_code":1010,` +
	`"error":"The selected time slot is not available for this therapist",` +
	`"agent_hint":"Try a different time or therapist."}},"error":"HTTP 400","status":400,"retryable":false}`

func bookingTurn(results ...string) agyTranscript {
	tr := agyTranscript{Turns: []agyTurn{{Kind: agyTurnCustomer, Text: "تثبيت"}}}
	for i, res := range results {
		id := string(rune('a' + i))
		tr.Turns = append(tr.Turns,
			agyTurn{Kind: agyTurnToolCall, Tool: "create_reservation", CallID: id,
				Args: canonicalToolArgs(map[string]any{"therapist_id": id, "time_from": "13:30"})},
			agyTurn{Kind: agyTurnToolResult, Tool: "create_reservation", CallID: id, Text: res})
	}
	return tr
}

func textReply(s string) responsesResponse {
	return responsesResponse{Output: []responsesOutputItem{{
		Type: "message", Role: "assistant",
		Content: []responsesOutputContent{{Type: "output_text", Text: s}},
	}}}
}

// Prod 2026-09-19 conv 60354: two create_reservation calls failed, and the
// reply said "تم تثبيت موعدك". She arrived at a clinic with no appointment.
func TestAReplyMayNotConfirmWhatTheTurnFailedToDo(t *testing.T) {
	tr := bookingTurn(slotTaken400, slotTaken400)
	if got := tr.unperformedThisTurn(); len(got) != 1 || got[0] != "create_reservation" {
		t.Fatalf("the failed booking was not recognised: %v", got)
	}
	lie := textReply("تم تثبيت موعدك عزيزتي منى اليوم السبت 19/09/2026 الساعة 1:30 بعد الظهر بفرع عمان.")
	if got := agyReplyClaimsUnperformed(config{}, tr, lie); len(got) != 1 {
		t.Fatalf("the false confirmation was allowed through: %v", got)
	}
	// The honest reply about the same failure must go out untouched.
	honest := textReply("بعتذر منك عزيزتي، ما تم تثبيت الموعد لأنه الوقت انحجز. بتحبي أشوفلك وقت تاني؟")
	if got := agyReplyClaimsUnperformed(config{}, tr, honest); len(got) != 0 {
		t.Fatalf("an honest reply about the failure was rejected: %v", got)
	}
	// And the switch turns the whole check off.
	if got := agyReplyClaimsUnperformed(config{AgyTruthGuardOff: true}, tr, lie); len(got) != 0 {
		t.Fatalf("PROXY_AGY_TRUTH_GUARD=false must disable the guard: %v", got)
	}
}

// A booking that failed once and then worked was carried out, and saying so is
// the truth. This is the retry path the guard must never punish.
func TestARetryThatSucceededMayBeConfirmed(t *testing.T) {
	ok := `{"success":true,"data":{"status":201,"body":{"success":true,"data":{"reservation_id":391818}}},"error":null}`
	tr := bookingTurn(slotTaken400, ok)
	if got := tr.unperformedThisTurn(); len(got) != 0 {
		t.Fatalf("a tool that succeeded after failing is not unperformed: %v", got)
	}
	reply := textReply("تم تثبيت موعدك عزيزتي اليوم الساعة 1:30 بعد الظهر.")
	if got := agyReplyClaimsUnperformed(config{}, tr, reply); len(got) != 0 {
		t.Fatalf("a true confirmation was rejected: %v", got)
	}
}

// With nothing failed, the guard is silent whatever the reply says; and a reply
// that still wants to call a tool is not a claim to anybody.
func TestTheClaimGuardIsSilentWithoutAFailure(t *testing.T) {
	tr := agyTranscript{Turns: []agyTurn{{Kind: agyTurnCustomer, Text: "تثبيت"}}}
	if got := agyReplyClaimsUnperformed(config{}, tr, textReply("تم تثبيت موعدك")); len(got) != 0 {
		t.Fatalf("no failed call means nothing to catch: %v", got)
	}
	withCall := responsesResponse{Output: []responsesOutputItem{
		{Type: "function_call", Name: "create_reservation", Arguments: "{}"},
		{Type: "message", Role: "assistant", Content: []responsesOutputContent{{Type: "output_text", Text: "تم تثبيت موعدك"}}},
	}}
	if got := agyReplyClaimsUnperformed(config{}, bookingTurn(slotTaken400), withCall); len(got) != 0 {
		t.Fatalf("a draft that is still calling tools is not a reply: %v", got)
	}
}

// The wording check: completion is caught, receipt and refusal are not.
func TestCompletionClaimsAreToldApartFromReceiptAndRefusal(t *testing.T) {
	claims := []string{
		"تم تثبيت موعدك عزيزتي",
		"تم حجز موعدك بنجاح",
		"تمام، تم تسجيل الاسم واعتماده مع الدفعة",
		"Your appointment has been booked for Saturday.",
		"I have booked your session at 4:30.",
		"The slot is now reserved.",
	}
	for _, s := range claims {
		if !agyClaimsCompletion(s) {
			t.Errorf("missed a completion claim: %s", s)
		}
	}
	notClaims := []string{
		"تم استلام إثبات الدفع وسيتم مراجعته من القسم المختص",
		"بعتذر منك عزيزتي، ما تم تثبيت الموعد",
		"لم يتم الحجز بسبب عدم توفر الوقت",
		"بتأكدي حجز هالموعد؟ الموعد ما بينحجز إلا بعد تأكيدك",
		"Your appointment was not booked — the slot is taken.",
		"I could not book the session; shall I try another time?",
		"المتاح اليوم السبت الساعة 1:30 بعد الظهر، أي وقت بناسبك؟",
	}
	for _, s := range notClaims {
		if agyClaimsCompletion(s) {
			t.Errorf("treated as a completion claim: %s", s)
		}
	}
}
