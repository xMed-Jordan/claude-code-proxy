package main

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Prod 2026-09-19 conv 57266. A customer asked whether 11:00 was free. The
// availability tool was called seven times and answered every time with an
// explicit list of free start times — 08:00…09:45, 12:15…13:45, 15:00…15:45 —
// which never contained 11:00. The assistant replied "أكيد في مجال" (of course
// there is room), built a booking summary around 11:00, got her confirmation,
// and only then sent 11:00 to the booking API, which refused it. She was told
// the team would get back to her. The therapist she had asked for was free at
// 12:15 the same morning.
//
// The reply cannot be policed by words: "11:00 is not free, but 12:15 is" is
// the sentence we want, and it names 11:00 too. The call can be. A booking that
// starts at a time nothing in the conversation ever offered is wrong before it
// is sent, whatever the prose around it says — and catching it at the call
// turns an apology into a second draft that can offer the customer a real time.
//
// Nothing here knows about clinics or appointments: it compares a start time an
// outgoing call carries against the start times this conversation's own tool
// results have mentioned.

// agyClockTimeRe matches a 24-hour clock time, the one format every scheduling
// API in this shape agrees on.
var agyClockTimeRe = regexp.MustCompile(`\b([01]?[0-9]|2[0-3]):([0-5][0-9])\b`)

// agyStartArgRe matches the argument names that carry when something begins.
// An end time is deliberately not checked: it is derived from the start plus a
// duration and legitimately falls outside any list of offered starts.
var agyStartArgRe = regexp.MustCompile(`(?i)(^|_)(time_from|from_time|start_time|start_at|starts_at|start)($|_)`)

// agyNormalizeClock pads an hour so "9:15" and "09:15" are one value.
func agyNormalizeClock(s string) string {
	m := agyClockTimeRe.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return ""
	}
	h := m[1]
	if len(h) == 1 {
		h = "0" + h
	}
	return h + ":" + m[2]
}

// agyTimesToolsMentioned collects every clock time appearing anywhere in this
// conversation's tool results. It is deliberately permissive — working hours
// and leave windows are swept up alongside free slots — because the cost of
// missing a bad booking is one API refusal, while the cost of blocking a good
// one is a customer who cannot book at all.
func agyTimesToolsMentioned(t agyTranscript) map[string]bool {
	out := map[string]bool{}
	for _, turn := range t.Turns {
		if turn.Kind != agyTurnToolResult || strings.TrimSpace(turn.Text) == "" {
			continue
		}
		for _, m := range agyClockTimeRe.FindAllString(turn.Text, -1) {
			if norm := agyNormalizeClock(m); norm != "" {
				out[norm] = true
			}
		}
	}
	return out
}

// agyUnofferedStarts names the start times a call is about to book that no tool
// result in this conversation ever mentioned. It reports nothing when the
// conversation holds no times at all: a tool that never returned one cannot be
// the source of truth for what is free.
func agyUnofferedStarts(offered map[string]bool, argsJSON string) []string {
	if len(offered) == 0 {
		return nil
	}
	var args map[string]any
	if json.Unmarshal([]byte(argsJSON), &args) != nil || len(args) == 0 {
		return nil
	}
	names := make([]string, 0, len(args))
	for k := range args {
		names = append(names, k)
	}
	sort.Strings(names)

	var bad []string
	for _, k := range names {
		if !agyStartArgRe.MatchString(k) {
			continue
		}
		s, isStr := args[k].(string)
		if !isStr {
			continue
		}
		norm := agyNormalizeClock(s)
		if norm == "" || offered[norm] {
			continue
		}
		bad = append(bad, fmt.Sprintf("%s=%s", k, strings.TrimSpace(s)))
	}
	return bad
}

// agySomeOfferedTimes renders a short, sorted sample of what the tools did
// offer, so the correction note can point at real alternatives rather than just
// refusing.
func agySomeOfferedTimes(offered map[string]bool, max int) string {
	all := make([]string, 0, len(offered))
	for t := range offered {
		all = append(all, t)
	}
	sort.Strings(all)
	if len(all) > max {
		all = all[:max]
	}
	return strings.Join(all, ", ")
}
