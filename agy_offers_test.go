package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// The real get_available_slots results behind the 2026-09-20/21 failures, as
// Connect stored them. Each is one day: therapists_summary (with service-id
// lists) plus slots[] → therapists[] → available_starts.
func slotFixture(t *testing.T, name string) string {
	t.Helper()
	raw, err := os.ReadFile("testdata/slots/" + name + ".json")
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// transcriptWithResults builds a transcript the way a Connect request is parsed,
// one tool call + result per entry, after a customer message.
func transcriptWithResults(results ...[2]string) agyTranscript {
	msgs := []anthropicMessage{{Role: "user", Content: "بدي احجز"}}
	for i, r := range results {
		id := "c" + string(rune('a'+i))
		msgs = append(msgs,
			anthropicMessage{Role: "assistant", Content: []any{map[string]any{"type": "tool_use", "id": id, "name": r[0], "input": map[string]any{"n": i}}}},
			anthropicMessage{Role: "user", Content: []any{map[string]any{"type": "tool_result", "tool_use_id": id, "content": r[1]}}},
		)
	}
	return buildAgyTranscriptFromAnthropic(anthropicRequest{Messages: msgs})
}

func bookingArgs(kv map[string]any) string { return canonicalToolArgs(kv) }

// Prod 2026-09-21 conv 44671. Thursday's result listed Amani (53010) with 08:00
// and 09:00 and Farah (53737) with 13:00, 16:00, 17:00, 18:00, 20:00. The summary
// and the booking paired 16:00 with Amani; the API refused it after the old
// appointment had already been cancelled.
func TestABookingMustUseTheTherapistWhoOfferedTheTime(t *testing.T) {
	o := agyCollectOffers(transcriptWithResults([2]string{"get_available_slots", slotFixture(t, "batool")}))

	bad := agyUnofferedForID(o, bookingArgs(map[string]any{
		"therapist_id": "53010", "time_from": "16:00", "time_to": "17:00", "date": "2026-09-24", "branch_id": "2",
	}))
	if len(bad) != 1 {
		t.Fatalf("Amani at 16:00 must be refused exactly once, got %v", bad)
	}
	for _, want := range []string{"therapist_id=53010", "08:00, 09:00", "listed under therapist_id=53737"} {
		if !strings.Contains(bad[0], want) {
			t.Fatalf("the note must say %q so the re-draft can fix it:\n%s", want, bad[0])
		}
	}
	// The same start with the therapist who offered it, or with "anyone", is fine.
	for _, th := range []string{"53737", "0", ""} {
		if got := agyUnofferedForID(o, bookingArgs(map[string]any{
			"therapist_id": th, "time_from": "16:00", "time_to": "17:00", "branch_id": "2",
		})); len(got) != 0 {
			t.Fatalf("therapist_id=%q at 16:00 is a valid booking, got %v", th, got)
		}
	}
}

// Prod 2026-09-20 conv 57241. The result listed Amna (53961) at 13:30 and Rula
// (54196) from 14:15; the retouch was booked at 15:30 with Riham (53895), who
// offered nothing that day.
func TestATherapistWhoOfferedNothingIsRefused(t *testing.T) {
	o := agyCollectOffers(transcriptWithResults([2]string{"get_available_slots", slotFixture(t, "ayah")}))
	bad := agyUnofferedForID(o, bookingArgs(map[string]any{
		"therapist_id": "53895", "time_from": "15:30", "time_to": "15:45", "reservation_id": "389605",
	}))
	if len(bad) != 1 || !strings.Contains(bad[0], "listed under therapist_id=54196") {
		t.Fatalf("Riham at 15:30 must be refused and Rula named, got %v", bad)
	}
	if got := agyUnofferedForID(o, bookingArgs(map[string]any{
		"therapist_id": "54196", "time_from": "15:30", "time_to": "15:45", "reservation_id": "389605",
	})); len(got) != 0 {
		t.Fatalf("Rula at 15:30 is what was offered, got %v", got)
	}
}

// A past visit carries an id and a time too — reservation 389605 happened at
// 15:30. That says when something was, not what is on offer, and a retouch of
// that visit at another time must not be refused because of it.
func TestAPastVisitIsNeverReadAsAnOffer(t *testing.T) {
	packages := `{"success":true,"data":{"status":200,"body":{"success":true,"data":{"packages":[{"user_package_id":275040,` +
		`"reservations":[{"reservation_id":389605,"date":"2026-09-03","time_from":"15:30:00","time_to":"15:45:00","therapist_id":53961}]}]}}}}`
	o := agyCollectOffers(transcriptWithResults(
		[2]string{"get_customer_packages", packages},
		[2]string{"get_available_slots", slotFixture(t, "ayah")},
	))
	if o.perKey["reservation_id"] {
		t.Fatal("reservation_id only ever sits next to a single time; it is not an offer breakdown")
	}
	if got := agyUnofferedForID(o, bookingArgs(map[string]any{
		"reservation_id": "389605", "therapist_id": "54196", "time_from": "17:15", "time_to": "17:30",
	})); len(got) != 0 {
		t.Fatalf("a retouch at an offered time must pass whatever the original visit's time was, got %v", got)
	}
}

// Offers also come flat — one record per start with the therapist beside it —
// and a booking taken from such an offer must pass even when another result in
// the same conversation breaks its offer down by therapist.
func TestAFlatOfferIsHonoured(t *testing.T) {
	flat := `{"success":true,"data":{"status":200,"body":{"success":true,"data":{"branch_id":1,"slots":[` +
		`{"date":"2026-09-19","time_from":"16:30","time_to":"16:45","therapist_id":53895}]}}}}`
	o := agyCollectOffers(transcriptWithResults(
		[2]string{"get_available_slots", slotFixture(t, "ayah")},
		[2]string{"get_multi_service_slots", flat},
	))
	if got := agyUnofferedForID(o, bookingArgs(map[string]any{
		"therapist_id": "53895", "time_from": "16:30", "branch_id": "1",
	})); len(got) != 0 {
		t.Fatalf("16:30 was offered under 53895 by the flat result, got %v", got)
	}
}

// With no result breaking its offer down by a key, nothing is judged by it.
func TestNoBreakdownNoJudgement(t *testing.T) {
	flat := `{"data":{"slots":[{"time_from":"09:00","therapist_id":7}]}}`
	o := agyCollectOffers(transcriptWithResults([2]string{"get_multi_service_slots", flat}))
	if got := agyUnofferedForID(o, bookingArgs(map[string]any{"therapist_id": "99", "time_from": "11:00"})); len(got) != 0 {
		t.Fatalf("no per-therapist lists in the conversation, so no per-therapist verdict: %v", got)
	}
}

// Prod 2026-09-21 conv 56865, replayed: squeezed to the room the transcript had,
// the result reached the model with every time and every therapist dropped and
// the service-id lists kept. The times are the result; they go last.
func TestOfferedTimesSurviveCompaction(t *testing.T) {
	raw := slotFixture(t, "nour")
	for _, level := range []int{agyLevelHistory, agyLevelProse, agyLevelFields} {
		out, changed := compactJSONForPrompt(raw, 2400, level)
		if !changed {
			t.Fatalf("level %d: the fixture is over budget and should have been compacted", level)
		}
		var v any
		if err := json.Unmarshal([]byte(out), &v); err != nil {
			t.Fatalf("level %d produced invalid JSON: %v", level, err)
		}
		for _, want := range []string{
			`"available_starts":["10:00","10:30","12:00","12:30","13:00","13:30","14:00","14:30","15:30"]`,
			`"available_starts":["14:30","17:30"]`,
			`"available_starts":["08:00","08:30","09:00"]`,
			`"therapist_id":53722`, `"therapist_id":53737`, `"therapist_id":53010`,
		} {
			if !strings.Contains(out, want) {
				t.Fatalf("level %d lost %s:\n%s", level, want, truncateString(out, 1500))
			}
		}
	}
}

// The column dropper must not spend the offer either, even when it is the
// bulkiest distinguishing column of the therapist records.
func TestTheOfferIsNeverDroppedAsAColumn(t *testing.T) {
	out, _ := compactJSONForPrompt(slotFixture(t, "ayah"), 1200, agyLevelFields)
	if !strings.Contains(out, `"available_starts":["14:15","14:30","14:45","15:15","15:30","15:45","17:15","17:30","19:00","19:15","20:45"]`) {
		t.Fatalf("Rula's offered starts were spent:\n%s", truncateString(out, 1500))
	}
}

// A lookup for another therapist, date or tool sheet is a different question,
// not a stale copy of the newest one. The same question asked again, or the same
// answer under reworded arguments, is.
func TestOnlyTheSameQuestionOrAnswerIsStale(t *testing.T) {
	msgs := []anthropicMessage{{Role: "user", Content: "الخميس بعد ال٣"}}
	add := func(id, tool string, input map[string]any, result string) {
		msgs = append(msgs,
			anthropicMessage{Role: "assistant", Content: []any{map[string]any{"type": "tool_use", "id": id, "name": tool, "input": input}}},
			anthropicMessage{Role: "user", Content: []any{map[string]any{"type": "tool_result", "tool_use_id": id, "content": result}}},
		)
	}
	add("all1", "get_available_slots", map[string]any{"from_date": "2026-09-24"}, `{"day":"all","v":1}`)
	add("all2", "get_available_slots", map[string]any{"from_date": "2026-09-24"}, `{"day":"all","v":2}`)
	add("amani", "get_available_slots", map[string]any{"from_date": "2026-09-24", "therapist_ids": "53010"}, `{"who":"amani"}`)
	add("hiba", "get_available_slots", map[string]any{"from_date": "2026-09-24", "therapist_ids": "53722"}, `{"who":"hiba"}`)
	add("p1", "membership_protocol", map[string]any{"context": "booking"}, `{"sheet":"same"}`)
	add("p2", "membership_protocol", map[string]any{"context": "booking a full body"}, `{"sheet":"same"}`)
	tr := buildAgyTranscriptFromAnthropic(anthropicRequest{Messages: msgs})

	stale := agySupersededResults(tr.Turns)
	resultText := func(i int) string { return tr.Turns[i].Text }
	var staleTexts []string
	for i := range tr.Turns {
		if stale[i] {
			staleTexts = append(staleTexts, resultText(i))
		}
	}
	joined := strings.Join(staleTexts, " | ")
	if !strings.Contains(joined, `"v":1`) {
		t.Fatalf("the first identical lookup is a stale copy of the second: %s", joined)
	}
	if strings.Contains(joined, `"who":"amani"`) || strings.Contains(joined, `"v":2`) {
		t.Fatalf("Amani's lookup and the newest all-therapist lookup answer different questions and are live: %s", joined)
	}
	if len(staleTexts) != 2 {
		t.Fatalf("expected exactly the repeated lookup and the reworded protocol to be stale, got %d: %s", len(staleTexts), joined)
	}
}

// Lookups for six different dates are six live answers now, not five stale
// copies — and the transcript must still fit without a package record lost.
func TestDistinctLookupsStayLiveAndStillFit(t *testing.T) {
	msgs := []anthropicMessage{{Role: "user", Content: "احجزيلي"}}
	for i := 0; i < 6; i++ {
		id := "s" + string(rune('0'+i))
		msgs = append(msgs,
			anthropicMessage{Role: "assistant", Content: []any{map[string]any{"type": "tool_use", "id": id, "name": "get_available_slots", "input": map[string]any{"from_date": "2026-09-1" + string(rune('0'+i))}}}},
			anthropicMessage{Role: "user", Content: []any{map[string]any{"type": "tool_result", "tool_use_id": id, "content": `{"day":` + string(rune('0'+i)) + `,"slots":[` + strings.TrimSuffix(strings.Repeat(`{"time_from":"09:00","time_to":"10:00","therapist_name":"اسم الأخصائية الطويل هنا"},`, 60), ",") + `]}`}}},
		)
	}
	msgs = append(msgs,
		anthropicMessage{Role: "assistant", Content: []any{map[string]any{"type": "tool_use", "id": "pk", "name": "get_customer_packages", "input": map[string]any{}}}},
		anthropicMessage{Role: "user", Content: []any{map[string]any{"type": "tool_result", "tool_use_id": "pk", "content": buildPackagesResult(24, 8)}}},
	)
	tr := buildAgyTranscriptFromAnthropic(anthropicRequest{Messages: msgs})
	budget := len(renderAgyTranscript(tr, 0, false)) / 3
	fitted, _ := fitAgyTranscript(tr, 0, false, budget)
	got := renderAgyTranscript(fitted, 0, false)
	if len(got) > budget {
		t.Fatalf("%d bytes over budget %d", len(got)-budget, budget)
	}
	for i := 0; i < 24; i++ {
		if !strings.Contains(got, `"user_package_id":`+itoa(263660+i)) {
			t.Fatalf("package record %d lost", 263660+i)
		}
	}
}

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }

// Every therapist in a one-branch lookup carries the same branch_id, so the
// lookup says nothing about which branch offers what: a booking at another
// branch is not judged by it (backtest 2026-09-23, conv 60790).
func TestASharedKeyIsNotABreakdown(t *testing.T) {
	o := agyCollectOffers(transcriptWithResults([2]string{"get_available_slots", slotFixture(t, "batool")}))
	if !o.perKey["therapist_id"] {
		t.Fatal("two therapists with their own lists: a breakdown by therapist_id")
	}
	if o.perKey["branch_id"] {
		t.Fatal("every therapist shares branch 2: not a breakdown by branch_id")
	}
	if got := agyUnofferedForID(o, bookingArgs(map[string]any{"branch_id": "1", "therapist_id": "0", "time_from": "16:00"})); len(got) != 0 {
		t.Fatalf("branch_id must not be judged by a one-branch lookup: %v", got)
	}
}
