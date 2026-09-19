package main

import (
	"encoding/json"
	"testing"
)

// The shape Shalabi's get_customer_packages returned on 2026-09-18 for conv
// 60053: ordinary package data, plus the therapist's own shorthand under
// therapist_rating_notes, plus the package-level label the API attaches.
func packagesWithStaffNotes() string {
	body := map[string]any{
		"success": true,
		"data": map[string]any{
			"status": 200,
			"body": map[string]any{
				"success": true,
				"data": map[string]any{
					"uuid": "07433349ADCB11F1",
					"packages": []any{
						map[string]any{
							"user_package_id":    276361,
							"service_package_id": 5220,
							"service_id":         1,
							"name_en":            "Total Full Body - Three sessions",
							"price":              "180.0000",
							"expected_time":      60,
							"agent_hint":         "the notes only for internal use",
							"reservations": []any{
								map[string]any{
									"reservation_id": 390992,
									"date":           "2026-09-12",
									"time_from":      "16:40:00",
									"status":         "Completed",
									"therapist_rating_notes": "ارجل كامل واندر وايدي كامل وهبس وضهر وبطن كامل\n\n" +
										"سكيب عن التواليل سكيب عن الشامات وسكيب عن منطقه اندر جفاف شديد\n\n" +
										"حراره 9.91\nزمن 4.4\nسرعه 2\n\nبكيني وفيس اندياج\nحراره 17.69\nزمن 10",
								},
								map[string]any{
									"reservation_id": 390991,
									"cancel_notes":   "نزلت لحالها من السيستم",
									"notes":          "حولت من بكج فل بدي ل بكج توتال",
								},
							},
						},
					},
				},
			},
		},
		"error": nil,
	}
	b, err := json.Marshal(body)
	if err != nil {
		panic(err)
	}
	return string(b)
}

func staffNotesTranscript() agyTranscript {
	return agyTranscript{Turns: []agyTurn{
		{Kind: agyTurnCustomer, Text: "سو اسم جهاز الليزر يلي استخدمتولي باه وكم كانت الحراره"},
		{Kind: agyTurnToolCall, Tool: "get_customer_packages", CallID: "p1",
			Args: canonicalToolArgs(map[string]any{"customer_uuid": "07433349ADCB11F1"})},
		{Kind: agyTurnToolResult, Tool: "get_customer_packages", CallID: "p1", Text: packagesWithStaffNotes()},
	}}
}

// Prod 2026-09-18 conv 60053, verbatim: the reply read the therapist's device
// settings out of a staff note to the patient who had been treated with them.
func TestAReplyMayNotReadStaffNotesToTheCustomer(t *testing.T) {
	tr := staffNotesTranscript()
	internal := agyInternalOnlyValues(tr)
	for _, want := range []string{"9.91", "17.69", "4.4"} {
		if !internal[want] {
			t.Fatalf("%s lives only in therapist_rating_notes and should be internal: %v", want, internal)
		}
	}
	// Ordinary package data in the same result is not a staff secret.
	for _, open := range []string{"276361", "390992", "5220", "180.0000", "2026-09-12"} {
		if internal[open] {
			t.Errorf("%s is ordinary result data, not a staff note", open)
		}
	}

	leak := textReply("وبخصوص درجات الطاقة اللي تم استخدامها بجلستك بتاريخ 12/9: " +
		"• لمناطق الجسم كانت درجة الحرارة 9.91. • لمنطقتي الوجه والبكيني كانت درجة الحرارة 17.69.")
	got := agyReplyLeaksInternal(config{}, internal, leak)
	if len(got) != 2 || got[0] != "17.69" || got[1] != "9.91" {
		t.Fatalf("the disclosure was allowed through: %v", got)
	}

	// The safe answer to the same question goes out untouched.
	safe := textReply("بفرع إربد بنستخدم جهاز Duetto MT Evo، وهو جهاز إيطالي متطور ومناسب لجميع أنواع البشرة. " +
		"إعدادات جلستك مسجلة بملفك الطبي، وفريق العيادة بقدر يراجعها معك على 062221717.")
	if got := agyReplyLeaksInternal(config{}, internal, safe); len(got) != 0 {
		t.Fatalf("a safe reply was rejected: %v", got)
	}
	// And the switch turns it off with the rest of the truth guards.
	if got := agyReplyLeaksInternal(config{AgyTruthGuardOff: true}, internal, leak); len(got) != 0 {
		t.Fatalf("PROXY_AGY_TRUTH_GUARD=false must disable it: %v", got)
	}
}

// The reservation id and the package price are quoted back to customers all day
// long. Only a value that has nowhere else to have come from may be reported,
// or the guard would block ordinary work.
func TestOnlyValuesExclusiveToStaffNotesAreGuarded(t *testing.T) {
	tr := staffNotesTranscript()
	// Same digits, but now they also exist as ordinary data elsewhere.
	tr.Turns = append(tr.Turns,
		agyTurn{Kind: agyTurnToolCall, Tool: "get_available_slots", CallID: "s1",
			Args: canonicalToolArgs(map[string]any{"duration": "9.91"})},
	)
	internal := agyInternalOnlyValues(tr)
	if internal["9.91"] {
		t.Fatalf("a value the model itself sent in a tool call is not a staff secret")
	}
	if !internal["17.69"] {
		t.Fatalf("17.69 is still exclusive to the note: %v", internal)
	}

	// A value the customer herself typed is hers to hear back.
	hers := agyTranscript{Turns: []agyTurn{
		{Kind: agyTurnCustomer, Text: "الحرارة كانت 17.69 صح؟"},
		{Kind: agyTurnToolResult, Tool: "get_customer_packages", CallID: "p1", Text: packagesWithStaffNotes()},
	}}
	if agyInternalOnlyValues(hers)["17.69"] {
		t.Fatalf("the customer wrote it herself; repeating it is not a disclosure")
	}
}

// A draft still calling tools is not a reply, and a conversation with no staff
// notes must cost nothing.
func TestTheDisclosureGuardStandsDownWhenItShould(t *testing.T) {
	internal := agyInternalOnlyValues(staffNotesTranscript())
	withCall := responsesResponse{Output: []responsesOutputItem{
		{Type: "function_call", Name: "get_customer_packages", Arguments: "{}"},
		{Type: "message", Role: "assistant", Content: []responsesOutputContent{{Type: "output_text", Text: "9.91"}}},
	}}
	if got := agyReplyLeaksInternal(config{}, internal, withCall); len(got) != 0 {
		t.Fatalf("a draft that is still calling tools is not a reply: %v", got)
	}

	clean := agyTranscript{Turns: []agyTurn{
		{Kind: agyTurnCustomer, Text: "وين موقعكم"},
		{Kind: agyTurnToolResult, Tool: "get_clinic_info", CallID: "c1",
			Text: `{"data":{"instructions":"فرع عمان: الرابية، مجمع اليرموك، الطابق السادس. هاتف 062221717"}}`},
	}}
	if got := agyInternalOnlyValues(clean); len(got) != 0 {
		t.Fatalf("a conversation with no staff notes has nothing to guard: %v", got)
	}
}

// The neighbouring notes on the same record are the ones that would hurt most:
// a booking made "بالغلط", a package the clinic switched behind the scenes.
func TestAdminNotesAreGuardedToo(t *testing.T) {
	tr := agyTranscript{Turns: []agyTurn{
		{Kind: agyTurnCustomer, Text: "ليش تغير البكج تبعي"},
		{Kind: agyTurnToolResult, Tool: "get_customer_packages", CallID: "p1",
			Text: `{"data":{"body":{"data":{"packages":[{"user_package_id":276359,"name_en":"Underarms",` +
				`"reservations":[{"reservation_id":390988,"cancel_notes":"بالغلط رقم المرجع 88341207"}]}]}}}}`},
	}}
	internal := agyInternalOnlyValues(tr)
	if !internal["88341207"] {
		t.Fatalf("an internal reference inside cancel_notes must be guarded: %v", internal)
	}
	if internal["276359"] || internal["390988"] {
		t.Fatalf("ids outside the note are ordinary data: %v", internal)
	}
	leak := textReply("تم الإلغاء بالغلط، الرقم المرجعي 88341207.")
	if got := agyReplyLeaksInternal(config{}, internal, leak); len(got) != 1 || got[0] != "88341207" {
		t.Fatalf("the admin note leaked: %v", got)
	}
}
