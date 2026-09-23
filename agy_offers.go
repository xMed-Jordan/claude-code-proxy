package main

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// A scheduling result offers its choices as lists of times, and usually per
// someone: Shalabi's get_available_slots answers
//
//	slots[] → { date, therapists[] → { therapist_id, available_starts[] }, merged_starts[] }
//
// where merged_starts is only the union. Two things went wrong with that shape
// on 2026-09-20/21, both behind the same customer-facing sentence — "بعتذر،
// صار في مشكلة بتثبيت الموعد", after she had already confirmed a summary:
//
//  1. The prompt fitter treated the per-therapist lists as "history nested in a
//     record" and halved them first — before the service-id lists sitting next
//     to them. Replayed through the fitter, the model had been shown
//     merged_starts: ["[13 entries dropped]"] and therapists: ["[3 entries
//     dropped]"] — no times and no therapists — and answered that the slots
//     "were not showing in the system" (conv 56865).
//  2. With the pairing gone, a time that one therapist offered was booked with
//     another: 16:00 belonged to Farah and was booked with Amani, whose own
//     list was 08:00 and 09:00 (conv 44671); 15:30 belonged to Rula and was
//     booked with a therapist who was not in the result at all (conv 57241).
//     Both were refused by the API, one after the old appointment had already
//     been cancelled.
//
// So a list of offered times is protected from thinning (agyHoldsClockList,
// used by the compactor), and a booking is checked against the times listed
// under the very id it names (agyUnofferedForID), from the full transcript —
// which holds the data even when the prompt could not.
//
// Nothing here knows about clinics or therapists. The field that scopes an
// offer is whichever *_id the call and the result share, and a list is an offer
// list because every entry in it is a clock time.

// agyClockOnlyRe matches a string that is a clock time and nothing else.
var agyClockOnlyRe = regexp.MustCompile(`^\s*([01]?[0-9]|2[0-3]):[0-5][0-9](:[0-5][0-9])?\s*$`)

// agyIsClockList reports whether an array is a list of clock times — ignoring
// the marker a previous compaction pass may have appended to it.
func agyIsClockList(arr []any) bool {
	clocks := 0
	for _, item := range arr {
		s, ok := item.(string)
		if !ok {
			return false
		}
		if strings.HasPrefix(s, agyOmissionMarker) {
			continue
		}
		if !agyClockOnlyRe.MatchString(s) {
			return false
		}
		clocks++
	}
	return clocks > 0
}

// agyHoldsClockList reports whether a JSON value is, or contains anywhere
// inside it, a list of clock times.
func agyHoldsClockList(v any) bool {
	switch x := v.(type) {
	case []any:
		if agyIsClockList(x) {
			return true
		}
		for _, item := range x {
			if agyHoldsClockList(item) {
				return true
			}
		}
	case map[string]any:
		for _, item := range x {
			if agyHoldsClockList(item) {
				return true
			}
		}
	}
	return false
}

// agyOffers is what the conversation's results offered, per id.
type agyOffers struct {
	// times[key][value] holds every clock time found inside an object that
	// carries key=value. Deliberately broad — working hours and past visits are
	// swept in too — because a bigger set can only make the check more lenient.
	times map[string]map[string]map[string]bool
	// offered[key][value] holds only the times found in LISTS of times inside
	// such an object — what was actually offered under that id. It is what the
	// correction note quotes, so the model is pointed at real alternatives and
	// never at a working-hours boundary or a past visit.
	offered map[string]map[string]map[string]bool
	// perKey marks the keys some result breaks its offer down by: sibling
	// records, each carrying a LIST of times, that differ in that key
	// (markBreakdowns). Only those keys are ever used to judge a call. A key seen
	// only next to a single time (a past reservation's reservation_id and
	// time_from) says when something happened, not what is on offer, and a key
	// every offer shares (one branch_id) says nothing about who offers what.
	perKey map[string]bool
}

// agyCollectOffers reads every JSON tool result in the transcript.
func agyCollectOffers(t agyTranscript) agyOffers {
	o := agyOffers{times: map[string]map[string]map[string]bool{}, offered: map[string]map[string]map[string]bool{}, perKey: map[string]bool{}}
	for _, turn := range t.Turns {
		if turn.Kind != agyTurnToolResult || strings.TrimSpace(turn.Text) == "" {
			continue
		}
		var parsed any
		if json.Unmarshal([]byte(turn.Text), &parsed) != nil {
			continue
		}
		o.walk(parsed)
	}
	return o
}

func (o agyOffers) walk(node any) {
	switch x := node.(type) {
	case []any:
		o.markBreakdowns(x)
		for _, item := range x {
			o.walk(item)
		}
	case map[string]any:
		var clocks, listed map[string]bool
		for k, v := range x {
			if !strings.HasSuffix(k, "_id") {
				continue
			}
			id := agyScalarKey(v)
			if id == "" {
				continue
			}
			if clocks == nil {
				clocks, listed = map[string]bool{}, map[string]bool{}
				agyGatherClocks(x, clocks)
				agyGatherListedClocks(x, listed)
			}
			agyAddAll(o.times, k, id, clocks)
			agyAddAll(o.offered, k, id, listed)
		}
		for _, v := range x {
			o.walk(v)
		}
	}
}

// markBreakdowns records the keys a list of offers is broken down by: sibling
// records that each carry a list of times and that name at least two DIFFERENT
// values of the key. Therapists in a day's result differ by therapist_id and so
// break the offer down by it; they usually all share one branch_id, which then
// says nothing about who offers what (backtest 2026-09-23: a doctor booked at a
// branch whose one-branch lookup listed only another therapist's evening times).
func (o agyOffers) markBreakdowns(list []any) {
	values := map[string]map[string]bool{}
	for _, item := range list {
		obj, ok := item.(map[string]any)
		if !ok || !agyHoldsClockList(obj) {
			continue
		}
		for k, v := range obj {
			if !strings.HasSuffix(k, "_id") {
				continue
			}
			if id := agyScalarKey(v); id != "" {
				if values[k] == nil {
					values[k] = map[string]bool{}
				}
				values[k][id] = true
			}
		}
	}
	for k, ids := range values {
		if len(ids) >= 2 {
			o.perKey[k] = true
		}
	}
}

// agyScalarKey renders an id value found in a result the same way agyArgValueKey
// renders it in a call, so 53010 and "53010" meet. Objects and lists are not ids.
func agyScalarKey(v any) string {
	switch v.(type) {
	case string, float64, json.Number, bool:
		return agyArgValueKey(v)
	}
	return ""
}

// agyGatherClocks collects every clock time inside a JSON value, normalised.
func agyGatherClocks(v any, into map[string]bool) {
	switch x := v.(type) {
	case string:
		for _, m := range agyClockTimeRe.FindAllString(x, -1) {
			if n := agyNormalizeClock(m); n != "" {
				into[n] = true
			}
		}
	case []any:
		for _, item := range x {
			agyGatherClocks(item, into)
		}
	case map[string]any:
		for _, item := range x {
			agyGatherClocks(item, into)
		}
	}
}

// agyGatherListedClocks collects the times that sit in lists of clock times
// inside a JSON value — the offer, as opposed to a single time field.
func agyGatherListedClocks(v any, into map[string]bool) {
	switch x := v.(type) {
	case []any:
		if agyIsClockList(x) {
			for _, item := range x {
				if s, ok := item.(string); ok {
					if n := agyNormalizeClock(s); n != "" {
						into[n] = true
					}
				}
			}
			return
		}
		for _, item := range x {
			agyGatherListedClocks(item, into)
		}
	case map[string]any:
		for _, item := range x {
			agyGatherListedClocks(item, into)
		}
	}
}

// agyAddAll records times under key=id, creating the entry even when there are
// none, so "this id was seen and offered nothing" is distinguishable from
// "this id was never seen".
func agyAddAll(m map[string]map[string]map[string]bool, k, id string, times map[string]bool) {
	if m[k] == nil {
		m[k] = map[string]map[string]bool{}
	}
	if m[k][id] == nil {
		m[k][id] = map[string]bool{}
	}
	for t := range times {
		m[k][id][t] = true
	}
}

// agyUnofferedForID checks a booking call's start time against the times
// listed under the id it names, for every *_id argument the conversation's
// results break their offers down by. It reports nothing for an id of "0" or
// empty (anyone), for a call without a start time, or when no result breaks
// its offer down by that key at all.
func agyUnofferedForID(o agyOffers, argsJSON string) []string {
	if len(o.perKey) == 0 {
		return nil
	}
	var args map[string]any
	if json.Unmarshal([]byte(argsJSON), &args) != nil || len(args) == 0 {
		return nil
	}
	start := ""
	for k, v := range args {
		if s, ok := v.(string); ok && agyStartArgRe.MatchString(k) {
			start = agyNormalizeClock(s)
		}
	}
	if start == "" {
		return nil
	}
	keys := make([]string, 0, len(args))
	for k := range args {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var problems []string
	for _, k := range keys {
		if !o.perKey[k] {
			continue
		}
		id := agyScalarKey(args[k])
		if id == "" || id == "0" {
			continue
		}
		listed, seen := o.times[k][id]
		if seen && listed[start] {
			continue
		}
		others := o.idsOffering(k, start, id)
		switch {
		case !seen:
			problems = append(problems, fmt.Sprintf(
				"you are booking %s=%s at %s, but %s=%s does not appear in any result in this conversation%s. Book the start with an id a result actually offered it under, and tell the customer if that changes who she is booked with",
				k, id, start, k, id, agyOfferedByNote(k, start, others)))
		default:
			problems = append(problems, fmt.Sprintf(
				"you are booking %s=%s at %s, but the results list %s=%s's times as %s%s. Book the start with the id that offers it and tell the customer if that changes who she is booked with, or offer her %s=%s's own times",
				k, id, start, k, id, agyTimeList(o.offered[k][id]), agyOfferedByNote(k, start, others), k, id))
		}
	}
	return problems
}

// idsOffering names the other ids under key k whose offered times include start.
func (o agyOffers) idsOffering(k, start, except string) []string {
	var out []string
	for id, times := range o.offered[k] {
		if id != except && times[start] {
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}

func agyOfferedByNote(k, start string, ids []string) string {
	if len(ids) == 0 {
		return ""
	}
	return fmt.Sprintf("; %s is listed under %s=%s", start, k, strings.Join(ids, ", "+k+"="))
}

func agyTimeList(times map[string]bool) string {
	if len(times) == 0 {
		return "none"
	}
	all := make([]string, 0, len(times))
	for t := range times {
		all = append(all, t)
	}
	sort.Strings(all)
	if len(all) > 16 {
		all = append(all[:16], "…")
	}
	return strings.Join(all, ", ")
}
