package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// The availability payload conv 57266 actually received on 2026-09-19: days,
// each holding therapists, each holding the start times that are free. The
// nesting matters — a flat scan for "a slot" finds nothing here, which is how
// this looked like an empty result at first glance.
func retouchSlots() string {
	body := map[string]any{
		"success": true,
		"data": map[string]any{
			"status": 200,
			"body": map[string]any{
				"success": true,
				"data": map[string]any{
					"appointment_type": "retouch",
					"duration_minutes": 15,
					"slots": []any{
						map[string]any{
							"date":        "2026-09-20",
							"day_of_week": "sun",
							"therapists": []any{
								map[string]any{
									"therapist_id":   53010,
									"therapist_name": "اماني الاحمد",
									"working_hours":  map[string]any{"from": "08:00", "to": "21:00"},
									"available_slots": []any{
										map[string]any{"time_from": "08:00", "time_to": "09:00"},
										map[string]any{"time_from": "12:15", "time_to": "14:00"},
									},
									"available_starts": []any{"08:00", "08:15", "12:15", "12:30", "13:00", "15:00"},
								},
							},
							"merged_starts": []any{"08:00", "08:15", "12:15", "12:30", "13:00", "15:00"},
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

func retouchTurn() agyTranscript {
	return agyTranscript{Turns: []agyTurn{
		{Kind: agyTurnCustomer, Text: "في مجال الساعة ١١؟"},
		{Kind: agyTurnToolCall, Tool: "get_available_slots", CallID: "s1",
			Args: canonicalToolArgs(map[string]any{"branch_id": "2", "from_date": "2026-09-20"})},
		{Kind: agyTurnToolResult, Tool: "get_available_slots", CallID: "s1", Text: retouchSlots()},
	}}
}

// The booking that produced the apology: 11:00, which nothing ever offered.
func TestABookingMayNotStartAtATimeNoToolOffered(t *testing.T) {
	offered := agyTimesToolsMentioned(retouchTurn())
	for _, want := range []string{"08:00", "12:15", "13:00", "15:00"} {
		if !offered[want] {
			t.Fatalf("%s was in the result and must be offered: %v", want, offered)
		}
	}
	if offered["11:00"] {
		t.Fatalf("11:00 appears nowhere in the result")
	}

	bad := agyUnofferedStarts(offered, canonicalToolArgs(map[string]any{
		"customer_uuid": "65540FB2956111ED", "reservation_id": "389348",
		"date": "2026-09-20", "time_from": "11:00", "time_to": "11:15", "therapist_id": "53010",
	}))
	if len(bad) != 1 || !strings.Contains(bad[0], "time_from=11:00") {
		t.Fatalf("the unavailable start was allowed through: %v", bad)
	}

	// The end time is derived from the start and is not checked: 14:00 is a
	// real slot end that is not itself an offered start in this payload.
	ok := agyUnofferedStarts(offered, canonicalToolArgs(map[string]any{
		"time_from": "12:15", "time_to": "12:30",
	}))
	if len(ok) != 0 {
		t.Fatalf("a booking at an offered start was rejected: %v", ok)
	}
	// Unpadded hours are the same time.
	if got := agyUnofferedStarts(offered, canonicalToolArgs(map[string]any{"time_from": "8:00"})); len(got) != 0 {
		t.Fatalf("8:00 and 08:00 are one time: %v", got)
	}
}

// A conversation whose tools never returned a time cannot be the judge of what
// is free, and a call carrying no start time is none of this guard's business.
func TestTheSlotGuardStandsDownWithNothingToCompareAgainst(t *testing.T) {
	none := agyTimesToolsMentioned(agyTranscript{Turns: []agyTurn{
		{Kind: agyTurnCustomer, Text: "بدي احجز"},
		{Kind: agyTurnToolResult, Tool: "find_user_by_phone", CallID: "f1",
			Text: `{"success":true,"data":{"body":{"data":{"uuid":"65540FB2956111ED"}}}}`},
	}})
	if got := agyUnofferedStarts(none, canonicalToolArgs(map[string]any{"time_from": "11:00"})); len(got) != 0 {
		t.Fatalf("with no times known, nothing may be rejected: %v", got)
	}

	offered := agyTimesToolsMentioned(retouchTurn())
	for _, args := range []string{
		canonicalToolArgs(map[string]any{"customer_uuid": "65540FB2956111ED"}),
		canonicalToolArgs(map[string]any{"date": "2026-09-20", "notes": "رتوش"}),
		canonicalToolArgs(map[string]any{"time_from": ""}),
	} {
		if got := agyUnofferedStarts(offered, args); len(got) != 0 {
			t.Fatalf("a call with no usable start time must pass: %v (%s)", got, args)
		}
	}
}

// The note has to hand the model something to say instead of an apology.
func TestTheCorrectionOffersRealAlternatives(t *testing.T) {
	offered := agyTimesToolsMentioned(retouchTurn())
	sample := agySomeOfferedTimes(offered, 12)
	for _, want := range []string{"08:00", "12:15", "15:00"} {
		if !strings.Contains(sample, want) {
			t.Fatalf("the sample must name real alternatives, got %q", sample)
		}
	}
	if strings.Contains(sample, "11:00") {
		t.Fatalf("the sample must not contain the time that was refused: %q", sample)
	}
	if got := agySomeOfferedTimes(offered, 2); strings.Count(got, ",") != 1 {
		t.Fatalf("the sample must respect its cap: %q", got)
	}
}
