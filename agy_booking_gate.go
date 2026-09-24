package main

// Bookings the customer is told about before they exist.
//
// Clinic agents show a booking summary and ask the customer to confirm before
// booking. Between 2026-09-14 and 2026-09-24 the same complaint came back five
// times: the summary went out, the customer confirmed, the booking call failed,
// and she got an apology. Each round had a different cause underneath — a
// catalogue id sent as a package id, a used-up package, a stale time, a move
// done as cancel + create, the membership contract booked as if it were a
// service — and fixing each cause left the pattern open. The one thing all of
// them share is that nothing validated the booking before the summary.
//
// The API now answers a check (dry_run) with exactly the rules the real call
// runs, and the agent has check_* tools that ask it. This file makes the order
// enforceable instead of merely documented:
//
//  1. A pre-confirmation summary may only go out after a check passed in the
//     same turn, for the date and time the summary quotes.
//  2. A check is never reported to the customer as a booking.
//  3. Cancelling and booking in one step is refused: when the booking fails,
//     the customer has lost the booking she had (2026-09-23, conv 60995).
//  4. "I recorded / cancelled it" with no action tool behind it is refused
//     (2026-09-23, conv 42126: a cancellation that never happened became a
//     no-show on the patient's record).
//
// Nothing here knows a booking rule. It reads success, failure and dry_run out
// of the tools' own result envelopes, and recognises the summary by the marker
// the caller's own template uses (PROXY_AGY_SUMMARY_MARKER).

import (
	"encoding/json"
	"fmt"
	"log"
	"regexp"
	"strconv"
	"strings"
)

// agyDefaultSummaryMarker matches the Shalabi agents' pre-confirmation summary:
// "تنبيه: هاد ملخص لموعدك للمراجعة، ومش حجز. الموعد لسا غير محجوز…".
const agyDefaultSummaryMarker = `ملخص[^\n]{0,80}(للمراجعة|ومش حجز|مش حجز)|(مش حجز|غير محجوز)[^\n]{0,40}(إلا بعد|الا بعد)`

// agyDefaultCheckTools names the tools that ask for a check instead of a booking.
const agyDefaultCheckTools = `^check_`

// agyBookingGateMode reports how the summary gate applies: "off", "test" (only
// to a conversation matching PROXY_AGY_SUMMARY_GATE_ONLY) or "on".
func agyBookingGateMode(cfg config) string {
	switch strings.ToLower(strings.TrimSpace(cfg.AgySummaryGate)) {
	case "on", "true", "1", "yes":
		return "on"
	case "test":
		return "test"
	default:
		return "off"
	}
}

func agySummaryMarkerRe(cfg config) *regexp.Regexp {
	if cfg.AgySummaryMarker != nil {
		return cfg.AgySummaryMarker
	}
	return agyDefaultSummaryMarkerRe
}

func agyCheckToolRe(cfg config) *regexp.Regexp {
	if cfg.AgyCheckTools != nil {
		return cfg.AgyCheckTools
	}
	return agyDefaultCheckToolsRe
}

var (
	agyDefaultSummaryMarkerRe = regexp.MustCompile(agyDefaultSummaryMarker)
	agyDefaultCheckToolsRe    = regexp.MustCompile(agyDefaultCheckTools)
)

// parseOptionalRegexp compiles an optional pattern from the environment. An
// empty value means "use the default"; an invalid one is logged and ignored
// rather than stopping the proxy.
func parseOptionalRegexp(name, v string) *regexp.Regexp {
	v = strings.TrimSpace(v)
	if v == "" {
		return nil
	}
	re, err := regexp.Compile(v)
	if err != nil {
		log.Printf("%s: invalid pattern %q ignored: %v", name, v, err)
		return nil
	}
	return re
}

// agyBookingGateScope reports whether this request is under the gate: it is
// switched on, or it is in test mode and the conversation is the one under
// test (its prompt or transcript matches PROXY_AGY_SUMMARY_GATE_ONLY). The
// check that a dry run is never reported as a booking is not scoped: the check
// tools can be called from any conversation.
func agyBookingGateScope(cfg config, in agyGenInput) bool {
	switch agyBookingGateMode(cfg) {
	case "on":
		return true
	case "test":
		if cfg.AgySummaryGateOnly == nil {
			return false
		}
		if cfg.AgySummaryGateOnly.MatchString(in.System) {
			return true
		}
		for _, t := range in.Transcript.Turns {
			if cfg.AgySummaryGateOnly.MatchString(t.Text) {
				return true
			}
		}
	}
	return false
}

// agyBookingGateApplies reports whether the summary gate governs this request:
// it is in scope and the caller gave the model at least one check tool. Without
// one the gate could only ever refuse.
func agyBookingGateApplies(cfg config, in agyGenInput) bool {
	return agyBookingGateScope(cfg, in) && agyCheckToolNames(cfg, in.Tools) != nil
}

// agyCheckToolNames lists the catalogue's check tools, sorted, or nil.
func agyCheckToolNames(cfg config, tools []agyToolCatalog) []string {
	re := agyCheckToolRe(cfg)
	var out []string
	for _, t := range tools {
		if re.MatchString(t.Name) {
			out = append(out, t.Name)
		}
	}
	return out
}

// agyTurnCall is one tool call of the current turn with what came back.
type agyTurnCall struct {
	Tool    string
	Args    string
	Result  any    // parsed result, nil when there was none or it was not JSON
	Failed  bool   // the result envelope reports a failure
	HasResp bool   // a result was found for this call
	DryRun  bool   // the result reports a check that passed (success + dry_run)
	Error   string // the API's own error text, for a failed call
}

// currentTurnOutcomes pairs every tool call of the turn the model is
// completing with its result, using the same pairing rules as
// unperformedThisTurn.
func (t *agyTranscript) currentTurnOutcomes() []agyTurnCall {
	var out []agyTurnCall
	start := t.currentTurnOpen() + 1
	for i := start; i < len(t.Turns); i++ {
		tt := t.Turns[i]
		if tt.Kind != agyTurnToolCall || tt.Tool == "" {
			continue
		}
		c := agyTurnCall{Tool: tt.Tool, Args: tt.Args}
		pastSibling := false
		for j := i + 1; j < len(t.Turns); j++ {
			r := t.Turns[j]
			if r.Kind == agyTurnCustomer {
				break
			}
			if r.Kind == agyTurnToolCall {
				if tt.CallID == "" {
					break
				}
				pastSibling = true
				continue
			}
			if r.Kind != agyTurnToolResult {
				continue
			}
			if tt.CallID != "" && r.CallID != tt.CallID && (r.CallID != "" || pastSibling) {
				continue
			}
			c.HasResp = true
			var parsed any
			if strings.TrimSpace(r.Text) != "" && json.Unmarshal([]byte(r.Text), &parsed) == nil {
				c.Result = parsed
				c.Failed = agyResultLooksFailed(parsed)
				c.DryRun = !c.Failed && agyJSONHasTrue(parsed, "dry_run")
				if c.Failed {
					c.Error = agyFirstErrorText(parsed)
				}
			}
			break
		}
		out = append(out, c)
	}
	return out
}

// agyJSONHasTrue reports whether key is true anywhere in a JSON value.
func agyJSONHasTrue(node any, key string) bool {
	switch x := node.(type) {
	case map[string]any:
		for k, v := range x {
			if strings.EqualFold(k, key) {
				if b, ok := v.(bool); ok && b {
					return true
				}
			}
			if agyJSONHasTrue(v, key) {
				return true
			}
		}
	case []any:
		for _, v := range x {
			if agyJSONHasTrue(v, key) {
				return true
			}
		}
	}
	return false
}

// agyFirstErrorText finds the API's own words for a failure: error, then
// agent_hint, searched depth-first.
func agyFirstErrorText(node any) string {
	var msg, hint string
	var walk func(any)
	walk = func(n any) {
		switch x := n.(type) {
		case map[string]any:
			if s, ok := x["error"].(string); ok && strings.TrimSpace(s) != "" && s != "HTTP 400" && msg == "" {
				msg = s
			}
			if s, ok := x["agent_hint"].(string); ok && strings.TrimSpace(s) != "" && hint == "" {
				hint = s
			}
			for _, v := range x {
				walk(v)
			}
		case []any:
			for _, v := range x {
				walk(v)
			}
		}
	}
	walk(node)
	return strings.TrimSpace(strings.Trim(msg+" — "+hint, " —"))
}

// agyCheckedSlot is the date and time window a passed check vouched for.
type agyCheckedSlot struct {
	Tool, Date, From, To string
}

// agyCheckedSlots reads the date/time_from/time_to out of every check that
// passed in this turn. A check's answer echoes what it would book.
func agyCheckedSlots(calls []agyTurnCall) []agyCheckedSlot {
	var out []agyCheckedSlot
	for _, c := range calls {
		if !c.DryRun {
			continue
		}
		s := agyCheckedSlot{Tool: c.Tool}
		var walk func(any)
		walk = func(n any) {
			switch x := n.(type) {
			case map[string]any:
				if v, ok := x["date"].(string); ok && s.Date == "" {
					s.Date = v
				}
				if v, ok := x["time_from"].(string); ok && s.From == "" {
					s.From = v
				}
				if v, ok := x["time_to"].(string); ok && s.To == "" {
					s.To = v
				}
				for _, v := range x {
					walk(v)
				}
			case []any:
				for _, v := range x {
					walk(v)
				}
			}
		}
		walk(c.Result)
		out = append(out, s)
	}
	return out
}

// agyArabicDigits maps Arabic-Indic and Persian digits to ASCII so a quoted
// "٥:٣٠" is read as 5:30.
var agyArabicDigits = strings.NewReplacer(
	"٠", "0", "١", "1", "٢", "2", "٣", "3", "٤", "4", "٥", "5", "٦", "6", "٧", "7", "٨", "8", "٩", "9",
	"۰", "0", "۱", "1", "۲", "2", "۳", "3", "۴", "4", "۵", "5", "۶", "6", "۷", "7", "۸", "8", "۹", "9",
)

var (
	agyQuotedDateRe = regexp.MustCompile(`(^|\D)(\d{1,2})\s*[/\-.]\s*(\d{1,2})(\s*[/\-.]\s*\d{2,4})?(\D|$)`)
	agyQuotedTimeRe = regexp.MustCompile(`(^|\D)(\d{1,2})[:.](\d{2})(\D|$)`)
)

// agySummaryQuotes reports whether a summary quotes the checked slot: its day
// and month, and its start time in 12- or 24-hour form. A summary that quotes
// no date or no time at all is not held to that part.
func agySummaryQuotes(text string, s agyCheckedSlot) bool {
	text = agyArabicDigits.Replace(text)
	dateOK, timeOK := true, true
	var dates [][2]int // day, month
	for _, q := range agyQuotedDateRe.FindAllStringSubmatch(text, -1) {
		qd, _ := strconv.Atoi(q[2])
		qm, _ := strconv.Atoi(q[3])
		if qd >= 1 && qd <= 31 && qm >= 1 && qm <= 12 {
			dates = append(dates, [2]int{qd, qm})
		}
	}
	if len(dates) > 0 && s.Date != "" && !strings.Contains(text, s.Date) {
		dateOK = false
		if parts := strings.Split(s.Date, "-"); len(parts) == 3 {
			m, _ := strconv.Atoi(parts[1])
			d, _ := strconv.Atoi(parts[2])
			for _, q := range dates {
				if q[0] == d && q[1] == m {
					dateOK = true
					break
				}
			}
		}
	}
	if times := agyQuotedTimeRe.FindAllStringSubmatch(text, -1); len(times) > 0 && s.From != "" {
		timeOK = false
		if hm := strings.Split(s.From, ":"); len(hm) >= 2 {
			h, _ := strconv.Atoi(hm[0])
			m, _ := strconv.Atoi(hm[1])
			for _, q := range times {
				qh, _ := strconv.Atoi(q[2])
				qm, _ := strconv.Atoi(q[3])
				if qm == m && (qh == h || (h > 12 && qh == h-12) || (h == 12 && qh == 12) || (h == 0 && qh == 12)) {
					timeOK = true
					break
				}
			}
		}
	}
	return dateOK && timeOK
}

// agySummaryGateProblem explains why a text reply that is a booking summary
// may not go out yet, or returns "" when it may.
func agySummaryGateProblem(cfg config, in agyGenInput, resp responsesResponse) string {
	if !agyBookingGateApplies(cfg, in) || hasAnyToolCall(resp.Output) {
		return ""
	}
	text := agyResponseText(resp)
	if !agySummaryMarkerRe(cfg).MatchString(text) {
		return ""
	}
	checks := agyCheckToolNames(cfg, in.Tools)
	calls := in.Transcript.currentTurnOutcomes()
	slots := agyCheckedSlots(calls)
	if len(slots) == 0 {
		var failed []string
		for _, c := range calls {
			if agyCheckToolRe(cfg).MatchString(c.Tool) && c.Failed {
				failed = append(failed, fmt.Sprintf("%s answered: %s", c.Tool, firstNonEmpty(c.Error, "failed")))
			}
		}
		if len(failed) > 0 {
			return "you are showing the customer a booking summary, but the booking check FAILED in this turn (" + strings.Join(failed, "; ") +
				"). That booking would fail when she confirms. Fix what the check says — the right package, a time that is free for the full length, the right tool for a move — run the check again, and show a summary only for a booking whose check passed. If no bookable option is left, tell her plainly what is possible instead of summarising something that cannot be booked"
		}
		return "you are showing the customer a booking summary, but no booking check ran in this turn. Before any summary, call the check tool for what you are about to book (" + strings.Join(checks, ", ") +
			") with EXACTLY the arguments you will use when she confirms. Show the summary only if the check answers dry_run:true; if it fails, fix what it says first"
	}
	for _, s := range slots {
		if agySummaryQuotes(text, s) {
			return ""
		}
	}
	var passed []string
	for _, s := range slots {
		passed = append(passed, strings.TrimSpace(s.Date+" "+s.From+"-"+s.To))
	}
	return "your summary quotes a date or time that no passed booking check covers. The checks that passed in this turn were for: " + strings.Join(passed, ", ") +
		". Quote exactly the checked booking, or check the one you want to quote first"
}

// agyPerformsAction reports whether a tool acts on the customer's file, as
// opposed to reading, checking, fetching instructions, or scheduling the
// agent's own follow-up message.
func agyPerformsAction(cfg config, tool string) bool {
	return !agyReadToolRe.MatchString(tool) && !agyCheckToolRe(cfg).MatchString(tool) &&
		tool != "get_tool_instructions" && tool != "schedule_follow_up"
}

// agyCheckReportedAsBooking reports whether a reply tells the customer a booking
// was made when the only booking-shaped calls that succeeded this turn were
// checks, which book nothing. A summary — which says it is not a booking — is
// not such a report.
func agyCheckReportedAsBooking(cfg config, in agyGenInput, resp responsesResponse) bool {
	if !agyTruthGuard(cfg) || hasAnyToolCall(resp.Output) {
		return false
	}
	text := agyResponseText(resp)
	if !agyClaimsCompletion(text) || agySummaryMarkerRe(cfg).MatchString(text) {
		return false
	}
	sawCheck := false
	for _, c := range in.Transcript.currentTurnOutcomes() {
		if c.DryRun {
			sawCheck = true
			continue
		}
		if c.HasResp && !c.Failed && agyPerformsAction(cfg, c.Tool) {
			return false // a real action succeeded; the claim may be about it
		}
	}
	return sawCheck
}

// agyCancelAndBookRe / agyBookToolRe name the two halves of a move done the
// dangerous way.
var (
	agyCancelToolRe = regexp.MustCompile(`(?i)cancel`)
	agyBookToolRe   = regexp.MustCompile(`(?i)^(create|reschedule|book)_`)
)

// agyCancelWithBooking reports the cancel and booking calls a single step makes
// together, or nil. Made together, the cancel lands even when the booking is
// refused, and the customer is left with nothing.
func agyCancelWithBooking(cfg config, in agyGenInput, output []responsesOutputItem) (cancel, book string) {
	if !agyTruthGuard(cfg) || !agyBookingGateScope(cfg, in) {
		return "", ""
	}
	for _, item := range output {
		if item.Type != "function_call" {
			continue
		}
		if agyCancelToolRe.MatchString(item.Name) && cancel == "" {
			cancel = item.Name
		} else if agyBookToolRe.MatchString(item.Name) && book == "" {
			book = item.Name
		}
	}
	if cancel == "" || book == "" {
		return "", ""
	}
	return cancel, book
}

// agyRecordClaimRe matches a first-person report of having recorded, cancelled,
// or passed something on — the phrasing that slipped past the completion
// guard: "سجّلت طلبك", "سجّلت عندك طلب إلغاء", "ألغيت موعدك", "تم الإلغاء".
var agyRecordClaimRe = regexp.MustCompile(
	`(?i)(سجّ?لت(لك|لكِ)?\s+(طلب|عندك|ملاحظ|موعد|اسم|ال)|سجّ?لتلك|(ألغيت|الغيت|لغيت)(لك|لكِ)?\s|تمّ?\s+(ال)?(إلغاء|الغاء|إرسال|ارسال)|` +
		`(وصّ?لت|حوّ?لت|رفعت)\s+(ملاحظتك|طلبك)\s+(لل|ل)|(بلّ?غت|خبّ?رت)\s+(ال)?فريق|` +
		`\bi(?:'ve|\s+have)?\s+(recorded|registered|cancel+ed|logged|noted down)\s+(your|the|it)\b|\bhas\s+been\s+cancel+ed\b)`)

// agyClaimsRecord reports whether a reply claims a recording/cancelling action,
// skipping sentences that deny it.
func agyClaimsRecord(s string) bool {
	for _, loc := range agyRecordClaimRe.FindAllStringIndex(s, -1) {
		from := loc[0] - agyClaimLookback
		if from < 0 {
			from = 0
		}
		for from > 0 && !isRuneStart(s[from]) {
			from--
		}
		if agyClaimNegationRe.MatchString(s[from:loc[0]]) {
			continue
		}
		if strings.HasPrefix(s[loc[0]:], "تم") && agyEndsWithAny(s[from:loc[0]], agyImperfectPrefixes) {
			continue
		}
		return true
	}
	return false
}

// agyUnbackedRecordClaim reports whether a reply says something was recorded or
// cancelled although no action tool succeeded in this turn.
func agyUnbackedRecordClaim(cfg config, in agyGenInput, resp responsesResponse) bool {
	if !agyTruthGuard(cfg) || !agyBookingGateScope(cfg, in) || hasAnyToolCall(resp.Output) || !agyClaimsRecord(agyResponseText(resp)) {
		return false
	}
	for _, c := range in.Transcript.currentTurnOutcomes() {
		if c.HasResp && !c.Failed && agyPerformsAction(cfg, c.Tool) {
			return false
		}
	}
	return true
}

// agyBookingReplyProblems runs the text-reply checks of this file.
func agyBookingReplyProblems(cfg config, in agyGenInput, resp responsesResponse) []string {
	var out []string
	if p := agySummaryGateProblem(cfg, in, resp); p != "" {
		out = append(out, p)
	}
	if agyCheckReportedAsBooking(cfg, in, resp) {
		out = append(out, "your reply tells the customer something was booked, but the only booking call that succeeded in this turn was a CHECK (dry_run), and a check books nothing. If she has not confirmed yet, show her the summary and ask her to confirm; if she has, make the real booking call now")
	}
	if agyUnbackedRecordClaim(cfg, in, resp) {
		out = append(out, "your reply says you recorded, cancelled or sent something, but no tool performed any action in this turn, so nothing was recorded and nobody was told. The customer will rely on it. Do it with the tool that does it (for a cancellation, cancel_reservation), or tell her plainly what you can and cannot do")
	}
	return out
}
