package main

import (
	"context"
	"regexp"
	"strings"
	"testing"
)

// The summary Zeina sends before she books, as sent to Iman on 2026-09-23.
const imanSummary = "تنبيه: هاد ملخص لموعدك للمراجعة، ومش حجز. الموعد لسا غير محجوز، وما رح ينحجز إلا بعد ما توافقي. " +
	"تفاصيل الموعد: - الخدمة: ليزر نصف جسم (سفلي) - اليوم والتاريخ: الأربعاء 30/09/2026 - الساعة: 5:00 مساء - الأخصائية: أماني"

// What capi answers a check that passed, inside Connect's HTTP envelope.
const checkPassed = `{"success":true,"data":{"status":200,"body":{"success":true,"dry_run":true,` +
	`"data":{"customer_uuid":"AA4945389A5D11ED","date":"2026-09-30","time_from":"17:00:00","time_to":"18:00:00","therapist_id":46},` +
	`"message":"Check passed: this booking would succeed. NOTHING WAS BOOKED."}},"error":null}`

// What capi answers a check on a used-up package.
const checkUsedUp = `{"success":false,"data":{"status":400,"body":{"success":false,"error_code":1011,` +
	`"error":"This package has no remaining sessions","agent_hint":"Book on a package with sessions left."}},"error":"HTTP 400","status":400}`

const bookedOK = `{"success":true,"data":{"status":201,"body":{"success":true,"data":{"reservation_id":392500}}},"error":null}`

var gateTools = []agyToolCatalog{
	{Name: "get_available_slots"}, {Name: "create_reservation"}, {Name: "check_reservation"},
	{Name: "cancel_reservation"}, {Name: "reschedule_reservation"},
}

func gateOn() config { return config{AgySummaryGate: "on"} }

// turnWith opens a turn with a customer message and appends each call with its
// result.
func turnWith(customer string, calls ...[3]string) agyTranscript {
	tr := agyTranscript{Turns: []agyTurn{{Kind: agyTurnCustomer, Text: customer}}}
	for i, c := range calls {
		id := "c" + string(rune('1'+i))
		tr.Turns = append(tr.Turns,
			agyTurn{Kind: agyTurnToolCall, Tool: c[0], CallID: id, Args: c[1]},
			agyTurn{Kind: agyTurnToolResult, Tool: c[0], CallID: id, Text: c[2]})
	}
	return tr
}

func gateIn(tr agyTranscript) agyGenInput {
	return agyGenInput{System: "أنت زينة. رقم الزبونة 962782400075", Tools: gateTools, Transcript: tr}
}

// The five complaints of 2026-09-14..24 share one shape: the summary went out
// before anything checked the booking. With the gate on, it cannot.
func TestASummaryNeedsACheckThatPassedThisTurn(t *testing.T) {
	in := gateIn(turnWith("الاربعا"))
	p := agySummaryGateProblem(gateOn(), in, textReply(imanSummary))
	if p == "" || !strings.Contains(p, "check_reservation") {
		t.Fatalf("a summary with no check went out: %q", p)
	}
	// A reply that is not a summary is none of the gate's business.
	if p := agySummaryGateProblem(gateOn(), in, textReply("الأيام المتاحة الأسبوع الجاي هي الاثنين والثلاثاء")); p != "" {
		t.Fatalf("an ordinary reply was held: %q", p)
	}
	// Nor is a draft that is still calling tools.
	withCall := responsesResponse{Output: []responsesOutputItem{
		{Type: "function_call", Name: "check_reservation", Arguments: "{}"},
		{Type: "message", Role: "assistant", Content: []responsesOutputContent{{Type: "output_text", Text: imanSummary}}},
	}}
	if p := agySummaryGateProblem(gateOn(), in, withCall); p != "" {
		t.Fatalf("a draft that is checking first was held: %q", p)
	}
}

func TestASummaryOfTheCheckedBookingGoesOut(t *testing.T) {
	in := gateIn(turnWith("الاربعا", [3]string{"check_reservation", `{"date":"2026-09-30"}`, checkPassed}))
	if p := agySummaryGateProblem(gateOn(), in, textReply(imanSummary)); p != "" {
		t.Fatalf("the checked summary was refused: %q", p)
	}
	// The same summary in Arabic-Indic digits.
	indic := strings.NewReplacer("30/09/2026", "٣٠/٠٩/٢٠٢٦", "5:00", "٥:٠٠").Replace(imanSummary)
	if p := agySummaryGateProblem(gateOn(), in, textReply(indic)); p != "" {
		t.Fatalf("Arabic-Indic digits were not read: %q", p)
	}
	// 24-hour form.
	if p := agySummaryGateProblem(gateOn(), in, textReply(strings.Replace(imanSummary, "5:00 مساء", "17:00", 1))); p != "" {
		t.Fatalf("the 24-hour time was not read: %q", p)
	}
}

// Aya, 2026-09-23: 17:30 had been taken four hours earlier. A summary that
// quotes a time the check did not cover is held.
func TestASummaryOfSomethingElseThanTheCheckIsHeld(t *testing.T) {
	in := gateIn(turnWith("الاربعا", [3]string{"check_reservation", `{}`, checkPassed}))
	other := strings.Replace(imanSummary, "5:00", "5:30", 1)
	if p := agySummaryGateProblem(gateOn(), in, textReply(other)); !strings.Contains(p, "17:00") {
		t.Fatalf("a summary of an unchecked time went out: %q", p)
	}
	otherDay := strings.Replace(imanSummary, "30/09/2026", "01/10/2026", 1)
	if p := agySummaryGateProblem(gateOn(), in, textReply(otherDay)); p == "" {
		t.Fatal("a summary of an unchecked day went out")
	}
}

// Wafaa, 2026-09-24: the package was used up. The check says so, and the
// summary may not go out over it.
func TestASummaryAfterAFailedCheckCarriesTheReason(t *testing.T) {
	in := gateIn(turnWith("اه", [3]string{"check_reservation", `{}`, checkUsedUp}))
	p := agySummaryGateProblem(gateOn(), in, textReply(imanSummary))
	if !strings.Contains(p, "FAILED") || !strings.Contains(p, "no remaining sessions") {
		t.Fatalf("the failed check's reason was not passed on: %q", p)
	}
}

func TestTheGateIsScopedUntilItIsSwitchedOn(t *testing.T) {
	in := gateIn(turnWith("الاربعا"))
	reply := textReply(imanSummary)
	if p := agySummaryGateProblem(config{}, in, reply); p != "" {
		t.Fatalf("the gate is off by default: %q", p)
	}
	test := config{AgySummaryGate: "test", AgySummaryGateOnly: regexp.MustCompile(`962782400075`)}
	if p := agySummaryGateProblem(test, in, reply); p == "" {
		t.Fatal("test mode did not govern the test number")
	}
	other := in
	other.System = "أنت زينة. رقم الزبونة 962799195874"
	if p := agySummaryGateProblem(test, other, reply); p != "" {
		t.Fatalf("test mode governed another customer: %q", p)
	}
	// The number may arrive in a tool result instead of the system prompt.
	other.Transcript = turnWith("الاربعا", [3]string{"get_customer_info", `{}`, `{"phone":"+962782400075"}`})
	if p := agySummaryGateProblem(test, other, reply); p == "" {
		t.Fatal("test mode missed the number in the transcript")
	}
	// Test mode with no pattern governs nobody.
	if p := agySummaryGateProblem(config{AgySummaryGate: "test"}, in, reply); p != "" {
		t.Fatalf("test mode with no pattern governed a conversation: %q", p)
	}
	// Without a check tool the gate could only refuse, so it stands aside.
	noCheck := in
	noCheck.Tools = []agyToolCatalog{{Name: "create_reservation"}}
	if p := agySummaryGateProblem(gateOn(), noCheck, reply); p != "" {
		t.Fatalf("the gate held a summary it gave no way to check: %q", p)
	}
}

// A check books nothing, and the customer must never be told otherwise. This
// holds everywhere, whatever the gate mode: the check tools exist for all.
func TestACheckIsNeverReportedAsABooking(t *testing.T) {
	in := gateIn(turnWith("اوك", [3]string{"check_reservation", `{}`, checkPassed}))
	if !agyCheckReportedAsBooking(config{}, in, textReply("تم تثبيت موعدك عزيزتي الأربعاء الساعة 5:00")) {
		t.Fatal("a check was reported as a booking")
	}
	if agyCheckReportedAsBooking(config{}, in, textReply(imanSummary)) {
		t.Fatal("a summary says it is not a booking and was held as one")
	}
	booked := gateIn(turnWith("اوك", [3]string{"check_reservation", `{}`, checkPassed}, [3]string{"create_reservation", `{}`, bookedOK}))
	if agyCheckReportedAsBooking(config{}, booked, textReply("تم تثبيت موعدك عزيزتي")) {
		t.Fatal("a real booking after a check may be confirmed")
	}
	if agyCheckReportedAsBooking(config{AgyTruthGuardOff: true}, in, textReply("تم تثبيت موعدك")) {
		t.Fatal("PROXY_AGY_TRUTH_GUARD=false must disable it")
	}
}

// Dr. Mona, 2026-09-23: cancel and book in one step; the cancel landed, the
// booking was refused, and she lost the appointment she had.
func TestCancelAndBookInOneStepIsRefused(t *testing.T) {
	in := gateIn(turnWith("غيريلي الموعد"))
	out := []responsesOutputItem{
		{Type: "function_call", Name: "cancel_reservation", Arguments: `{"reservation_id":1}`},
		{Type: "function_call", Name: "create_retouch_reservation", Arguments: `{}`},
	}
	if c, b := agyCancelWithBooking(gateOn(), in, out); c != "cancel_reservation" || b != "create_retouch_reservation" {
		t.Fatalf("cancel + book in one step went through: %q %q", c, b)
	}
	if c, _ := agyCancelWithBooking(gateOn(), in, out[:1]); c != "" {
		t.Fatal("a cancel on its own is allowed")
	}
	move := []responsesOutputItem{{Type: "function_call", Name: "reschedule_reservation", Arguments: `{}`}}
	if c, _ := agyCancelWithBooking(gateOn(), in, move); c != "" {
		t.Fatal("a move in place is allowed")
	}
	if c, _ := agyCancelWithBooking(config{}, in, out); c != "" {
		t.Fatal("outside the gate's scope nothing changes yet")
	}
}

// Hala, 2026-09-23/24: "سجّلت طلبك" and "سجّلت عندك طلب إلغاء" with no tool
// call at all. The cancellation never happened and became a no-show.
func TestARecordingNobodyMadeIsRefused(t *testing.T) {
	in := gateIn(turnWith("بدي الغي الموعد"))
	for _, s := range []string{
		"تكرمي عزيزتي حلا، سجّلت طلبك لإضافة جلسة الإبطين بنفس موعدك بكرا الخميس الساعة 10:00 صباحاً",
		"ولا يهمك عزيزتي حلا، سجّلت عندك طلب إلغاء موعد بكرا الخميس (الساعة 10:00 صباحاً)",
		"ألغيت موعدك عزيزتي",
		"تم الإلغاء عزيزتي",
		"ووصلت ملاحظتك للفريق حتى يتابعوا إلغاء الموعد",
		// Conv 61123 (E2E 2026-09-24), after a check failed: a handoff that does not exist.
		"تكرمي عزيزتي فايزة، سجّلت عندي رغبتك بالموعد يوم الاثنين 28/09/2026 الساعة 3:00 بعد الظهر بفرع إربد، وحوّلت الطلب للفريق بالعيادة ورح يتواصلوا معك",
		"حوّلت الطلب للفريق بالعيادة",
		"تم تحويل طلبك للفريق",
		"I've cancelled your appointment.",
	} {
		if !agyUnbackedRecordClaim(gateOn(), in, textReply(s)) {
			t.Errorf("an unbacked claim went out: %q", s)
		}
	}
	for _, s := range []string{
		"ما سجلت طلبك لسا، بتحبي ألغيلك الموعد؟",
		"رح يتم الإلغاء بعد ما توافقي",
		"لم يتم الإلغاء",
		"بتحبي ألغيلك الموعد؟",
		"رح أحوّل طلبك للفريق إذا بتحبي",
	} {
		if agyUnbackedRecordClaim(gateOn(), in, textReply(s)) {
			t.Errorf("an honest reply was refused: %q", s)
		}
	}
	cancelled := gateIn(turnWith("بدي الغي", [3]string{"cancel_reservation", `{}`, `{"success":true,"data":{"status":200,"body":{"success":true}}}`}))
	if agyUnbackedRecordClaim(gateOn(), cancelled, textReply("ألغيت موعدك عزيزتي")) {
		t.Fatal("a cancellation that happened may be reported")
	}
	followUp := gateIn(turnWith("بدي الغي", [3]string{"schedule_follow_up", `{}`, `{"success":true,"message":"Follow-up scheduled"}`}))
	if !agyUnbackedRecordClaim(gateOn(), followUp, textReply("ألغيت موعدك عزيزتي")) {
		t.Fatal("scheduling a follow-up does not cancel anything")
	}
	if agyUnbackedRecordClaim(config{}, in, textReply("ألغيت موعدك عزيزتي")) {
		t.Fatal("outside the gate's scope nothing changes yet")
	}
}

// End to end: the summary is refused, the correction names the check tool, and
// the model's next draft checks first.
func TestAgyGenerateTurnsAnUncheckedSummaryIntoACheck(t *testing.T) {
	orig := agyResolveFn
	defer func() { agyResolveFn = orig }()
	var prompts []string
	agyResolveFn = func(_ context.Context, _ config, _ []mediaPart, prompt, _ string) (agyResult, error) {
		prompts = append(prompts, prompt)
		if len(prompts) == 1 {
			return agyResult{Ok: true, Response: imanSummary}, nil
		}
		return agyResult{Ok: true, Response: "<tool_call>\n{\"tool\": \"check_reservation\", \"input\": {\"date\": \"2026-09-30\", \"time_from\": \"17:00\"}}\n</tool_call>"}, nil
	}
	resp, _, err := agyGenerate(context.Background(), gateOn(), gateIn(turnWith("الاربعا")))
	if err != nil {
		t.Fatal(err)
	}
	if len(prompts) != 2 || !strings.Contains(prompts[1], "no booking check ran") {
		t.Fatalf("the correction did not reach the model: %d prompts", len(prompts))
	}
	if !hasAnyToolCall(resp.Output) || resp.Output[0].Name != "check_reservation" {
		t.Fatalf("expected the check call, got %+v", resp.Output)
	}
}

// And a model that will not stop summarising is not let through by the
// fallbacks: the request fails and the caller's chain serves the turn.
func TestAgyGenerateNeverLetsAnUncheckedSummaryOut(t *testing.T) {
	orig := agyResolveFn
	defer func() { agyResolveFn = orig }()
	var last string
	agyResolveFn = func(_ context.Context, _ config, _ []mediaPart, prompt, _ string) (agyResult, error) {
		last = prompt
		return agyResult{Ok: true, Response: imanSummary}, nil
	}
	_, _, err := agyGenerate(context.Background(), gateOn(), gateIn(turnWith("الاربعا")))
	if err == nil || !strings.Contains(err.Error(), "booking gate") {
		t.Fatalf("an unchecked summary got out: %v", err)
	}
	if !strings.Contains(last, "do not show a booking summary") {
		t.Fatal("the forced reply was not told why")
	}
}
