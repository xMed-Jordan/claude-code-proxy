package main

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Staff annotations travel inside ordinary tool results, next to the data the
// assistant is supposed to relay. Prod 2026-09-18 conv 60053: a customer asked
// which laser was used on her and at what setting, and the reply gave her the
// therapist's own shorthand — "حراره 9.91 … حراره 17.69" — read out of a
// reservation's therapist_rating_notes, along with what the therapist had
// written about skipping areas. The same field carried "سكيب عن الشامات"
// (moles skipped) and neighbouring records carried "بالغلط" (booked by
// mistake) and "حولنا بكج الفل ل بكج توتال" (we switched her package).
//
// The API does mark them: every package carrying notes arrives with
// agent_hint "the notes only for internal use". That is a label on the parent
// object, in a language the note is not written in, with no verb in it — and
// the tool's instructions and the assistant's own persona say nothing about
// notes at all. So nothing ever told the model not to.
//
// The check below does not rely on the model having been told. A value that
// exists in this conversation ONLY inside a staff field is a value the
// assistant can only have got from there, so it may not appear in a reply.
// Paraphrase escapes it — that needs the instructions — but a measurement, an
// internal code or an identifier does not paraphrase, and those are what turn a
// clinical record into a disclosure.

// agyInternalFieldRe matches the field names that hold staff annotations:
// notes, cancel_notes, therapist_rating_notes, internal_comment, staff_remark.
var agyInternalFieldRe = regexp.MustCompile(`(?i)(note|internal|comment|remark)`)

// agyDistinctiveTokenRe matches the kinds of value that cannot survive being
// paraphrased: a decimal measurement, a long number, or an alphanumeric code.
// Short words and small integers are deliberately not tokens — they occur
// everywhere and would make the check noise.
var agyDistinctiveTokenRe = regexp.MustCompile(`[0-9]+\.[0-9]+|[0-9]{4,}|[A-Za-z0-9]*[0-9][A-Za-z0-9]*`)

// agyMinCodeToken is how long a non-decimal token must be to count as an
// identifier rather than a quantity anyone might mention.
const agyMinCodeToken = 4

// agyInternalOnlyValues returns the distinctive tokens that appear in this
// conversation ONLY inside staff-annotation fields of tool results. A token
// that also occurs in ordinary result data, in a tool call's own arguments, or
// in something the customer wrote is not exclusive to the staff record and is
// never reported. Earlier assistant turns are deliberately NOT counted as
// somewhere else: a value that leaked once must not launder itself.
func agyInternalOnlyValues(t agyTranscript) map[string]bool {
	internal, elsewhere := map[string]bool{}, map[string]bool{}
	for _, turn := range t.Turns {
		switch turn.Kind {
		case agyTurnToolResult:
			var parsed any
			if strings.TrimSpace(turn.Text) == "" || json.Unmarshal([]byte(turn.Text), &parsed) != nil {
				// Not JSON: it is all ordinary text the assistant may use.
				addTokens(elsewhere, turn.Text)
				continue
			}
			agyWalkResult(parsed, false, internal, elsewhere)
		case agyTurnToolCall:
			addTokens(elsewhere, turn.Args)
		case agyTurnCustomer, agyTurnSystemNote:
			addTokens(elsewhere, turn.Text)
		}
	}
	for tok := range elsewhere {
		delete(internal, tok)
	}
	return internal
}

// agyWalkResult splits a tool result's tokens into those reachable only under a
// staff-annotation field and those reachable anywhere else. Once inside such a
// field everything below it is internal too.
func agyWalkResult(node any, inInternal bool, internal, elsewhere map[string]bool) {
	switch x := node.(type) {
	case map[string]any:
		for k, v := range x {
			agyWalkResult(v, inInternal || agyInternalFieldRe.MatchString(k), internal, elsewhere)
		}
	case []any:
		for _, v := range x {
			agyWalkResult(v, inInternal, internal, elsewhere)
		}
	case string:
		if inInternal {
			addTokens(internal, x)
		} else {
			addTokens(elsewhere, x)
		}
	case float64, bool, nil:
		if !inInternal {
			addTokens(elsewhere, fmt.Sprint(x))
		}
	}
}

// addTokens records every distinctive token in a piece of text.
func addTokens(into map[string]bool, s string) {
	for _, tok := range agyDistinctiveTokenRe.FindAllString(s, -1) {
		if strings.Contains(tok, ".") || len(tok) >= agyMinCodeToken {
			into[tok] = true
		}
	}
}

// agyReplyLeaksInternal names the staff-record values a reply is about to hand
// to the customer. Empty means the reply carries nothing that could only have
// come from a staff annotation.
func agyReplyLeaksInternal(cfg config, internalOnly map[string]bool, resp responsesResponse) []string {
	if !agyTruthGuard(cfg) || len(internalOnly) == 0 || hasAnyToolCall(resp.Output) {
		return nil
	}
	found := map[string]bool{}
	for _, tok := range agyDistinctiveTokenRe.FindAllString(agyResponseText(resp), -1) {
		if internalOnly[tok] {
			found[tok] = true
		}
	}
	out := make([]string, 0, len(found))
	for tok := range found {
		out = append(out, tok)
	}
	sort.Strings(out)
	return out
}
