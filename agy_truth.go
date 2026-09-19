package main

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Two things a model must never do with a tool: send a parameter in a shape the
// tool does not accept, and tell the customer an action happened when the call
// that would have performed it failed. Both were still reaching patients on
// 2026-09-18/19 with every instruction in place, so both are checked here
// instead of asked for in prose.
//
// Neither check knows anything about clinics, bookings or any business rule.
// The first reads the shapes out of the tool's own documentation; the second
// reads success and failure out of the tool's own result envelope.

// ---------------------------------------------------------------------------
// 1. The shape a tool documents for a parameter
// ---------------------------------------------------------------------------

// Connect builds every HTTP tool's JSON schema from the {{placeholders}} in its
// request template, so every parameter arrives typed "string" with the
// description "Value for X parameter" (see agy_args.go). The schema therefore
// cannot say that one parameter is a list and another is a keyed object — only
// the tool's written instructions can, and they do, in their examples.
//
// Prod 2026-09-18, two membership bookings lost this way. The tool documents
//
//	"service_packages":{"8":55101,"9":55103}   an object: service -> the
//	                                           customer's own package id
//
// and the model sent ["6033"] — a list, holding the catalogue id. The API reads
// a list positionally, finds no entry under the key it wants, skips the check
// that would have linked the visit to the customer's package, and answers 201.
// The customer is told her appointment is booked and nothing is on her file.
// The instructions already said, in capitals, that a list is rejected. This
// makes the tool's own example enforceable instead of merely readable.

// agyExampleFieldRe finds "name": followed by the start of an object or a list
// inside a tool's instructions, i.e. a documented example of that field.
var agyExampleFieldRe = regexp.MustCompile(`"([A-Za-z_][A-Za-z0-9_]*)"\s*:\s*([\[{])`)

// agyKindAmbiguous marks a parameter whose examples show more than one shape,
// so nothing can be concluded from the one the model chose.
const agyKindAmbiguous = "?"

// agyDocumentedArgKinds reads every tool-instruction result in the transcript
// and returns, per tool, the JSON shape ("object" or "array") that tool's own
// examples give each parameter. A parameter shown both ways is recorded as
// ambiguous and never enforced.
func agyDocumentedArgKinds(t agyTranscript) map[string]map[string]string {
	out := map[string]map[string]string{}
	for _, turn := range t.Turns {
		if turn.Kind != agyTurnToolResult || strings.TrimSpace(turn.Text) == "" {
			continue
		}
		tool, instructions, ok := agyInstructionPayload(turn.Text)
		if !ok {
			continue
		}
		for _, m := range agyExampleFieldRe.FindAllStringSubmatch(instructions, -1) {
			field, kind := m[1], "object"
			if m[2] == "[" {
				kind = "array"
			}
			if out[tool] == nil {
				out[tool] = map[string]string{}
			}
			if prev, seen := out[tool][field]; seen && prev != kind {
				out[tool][field] = agyKindAmbiguous
				continue
			}
			out[tool][field] = kind
		}
	}
	return out
}

// agyInstructionPayload pulls the tool code and instruction text out of a
// get_tool_instructions result. A result that does not name the tool it
// documents is skipped: its examples cannot be attributed to anything.
func agyInstructionPayload(text string) (tool, instructions string, ok bool) {
	var body map[string]any
	if json.Unmarshal([]byte(text), &body) != nil {
		return "", "", false
	}
	meta, isMap := body["tool"].(map[string]any)
	if !isMap {
		return "", "", false
	}
	code, isStr := meta["code"].(string)
	if !isStr || strings.TrimSpace(code) == "" {
		return "", "", false
	}
	instructions, isStr = body["instructions"].(string)
	if !isStr || strings.TrimSpace(instructions) == "" {
		return "", "", false
	}
	return code, instructions, true
}

// agyValueKind reports whether an argument value is a JSON object or list.
// A value that arrived as a string holding JSON — the models routinely send
// "[6033]" rather than [6033], and the request templates accept both — is read
// through to the shape it carries.
func agyValueKind(v any) string {
	switch x := v.(type) {
	case map[string]any:
		return "object"
	case []any:
		return "array"
	case string:
		s := strings.TrimSpace(x)
		if len(s) < 2 || (s[0] != '[' && s[0] != '{') {
			return ""
		}
		var inner any
		if json.Unmarshal([]byte(s), &inner) != nil {
			return ""
		}
		return agyValueKind(inner)
	}
	return ""
}

// agyArgShapeProblems compares a call's arguments against the shapes the tool's
// own instructions document, and describes every mismatch. Only a parameter the
// instructions show unambiguously, and that the model sent as the other
// container shape, is reported: a scalar, an unknown parameter or an
// undocumented one is left alone.
func agyArgShapeProblems(kinds map[string]map[string]string, tool, argsJSON string) []string {
	want := kinds[tool]
	if len(want) == 0 {
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

	var problems []string
	for _, k := range names {
		documented := want[k]
		if documented == "" || documented == agyKindAmbiguous {
			continue
		}
		got := agyValueKind(args[k])
		if got == "" || got == documented {
			continue
		}
		problems = append(problems, fmt.Sprintf(
			"you sent %s to %s as a %s, but this tool's own instructions document %s as a %s — send it in the shape the instructions show, with the keys they show; a %s is read positionally and silently loses the values",
			k, tool, agyShapeWord(got), k, agyShapeWord(documented), agyShapeWord(got)))
	}
	return problems
}

// agyShapeWord names a JSON container the way the correction note reads best.
func agyShapeWord(kind string) string {
	if kind == "array" {
		return "list"
	}
	return "keyed object"
}

// ---------------------------------------------------------------------------
// 2. An action that failed is not an action that happened
// ---------------------------------------------------------------------------

// The system instructions say "NEVER CONFIRM WITHOUT TOOL SUCCESS", the tool's
// own instructions say "confirm ONLY after this success", and on 2026-09-19 the
// model called create_reservation twice, was told both times that the slot was
// not available for that therapist, and wrote "تم تثبيت موعدك" — your
// appointment is confirmed — to the customer. Reception found out when she
// arrived. The same shape has now been reported for bookings and for a payment.
// Prose has not stopped it, so the claim is checked against what the turn
// actually achieved.

// agyCompletionClaimRe matches a reply asserting that something was carried
// out. It is deliberately about completion ("done", "booked", "registered"),
// never about receipt ("we have received your message"), which stays true
// whatever the tools did.
var agyCompletionClaimRe = regexp.MustCompile(
	`(?i)(تمّ?\s+(ال)?(تثبيت|تأكيد|حجز|تسجيل|اعتماد|إتمام)|` +
		`(حجزت|حجزنا|ثبتت|ثبتنا|سجلنا|اعتمدنا)\s|` +
		`\b(has|have|is|are|was|were)\s+been\s+(booked|confirmed|reserved|scheduled|registered|recorded)\b|` +
		`\b(successfully|now)\s+(booked|confirmed|reserved|scheduled|registered)\b|` +
		`\bi(?:'ve|\s+have)?\s+(booked|reserved|scheduled|confirmed|registered)\s+(your|the|it)\b)`)

// agyClaimNegationRe matches the particles that turn such a sentence into its
// opposite — "ما تم الحجز", "the appointment was not booked" — which is exactly
// the honest reply this guard is trying to get.
var agyClaimNegationRe = regexp.MustCompile(`(?i)(ما|لم|لن|مش|بدون|غير)\s*$|(not|n't|never|unable|cannot|can't|couldn't)\s+$`)

// agyClaimLookback is how much text before a match is searched for a negation:
// long enough for "the appointment was not " and "ما ", short enough that a
// negation about something else does not cancel a claim.
const agyClaimLookback = 40

// agyImperfectPrefixes are the letters that turn the perfect "تم" (it was
// done) into an imperfect or future verb — "يتم", "سيتم", "ستتم" — which
// promises or describes rather than reports. "لم يتم الحجز" is a denial, and
// matching the "تم" inside it would reject the very reply this guard wants.
const agyImperfectPrefixes = "يتنس"

// agyClaimsCompletion reports whether a reply tells the reader that an action
// was carried out, ignoring the sentences that say it was not.
func agyClaimsCompletion(s string) bool {
	for _, loc := range agyCompletionClaimRe.FindAllStringIndex(s, -1) {
		from := loc[0] - agyClaimLookback
		if from < 0 {
			from = 0
		}
		for from > 0 && !isRuneStart(s[from]) {
			from--
		}
		before := s[from:loc[0]]
		if strings.HasPrefix(s[loc[0]:], "تم") && agyEndsWithAny(before, agyImperfectPrefixes) {
			continue
		}
		if agyClaimNegationRe.MatchString(before) {
			continue
		}
		return true
	}
	return false
}

// agyEndsWithAny reports whether the last rune of s is one of the runes in set.
func agyEndsWithAny(s, set string) bool {
	r := []rune(s)
	if len(r) == 0 {
		return false
	}
	return strings.ContainsRune(set, r[len(r)-1])
}

// isRuneStart reports whether a byte begins a UTF-8 rune, so a lookback window
// never starts in the middle of one.
func isRuneStart(b byte) bool { return b&0xC0 != 0x80 }

// unperformedThisTurn names the tools that failed in the turn the model is
// completing and never succeeded in it. Those are the actions the model has NOT
// carried out, whatever its draft says. A tool that failed and then succeeded
// is not listed: the retry did the work.
func (t *agyTranscript) unperformedThisTurn() []string {
	failed, succeeded := map[string]bool{}, map[string]bool{}
	start := t.lastCustomerIndex() + 1
	for i := start; i < len(t.Turns); i++ {
		tt := t.Turns[i]
		if tt.Kind != agyTurnToolCall || tt.Tool == "" {
			continue
		}
		for j := i + 1; j < len(t.Turns); j++ {
			r := t.Turns[j]
			if r.Kind == agyTurnToolCall || r.Kind == agyTurnCustomer {
				break
			}
			if r.Kind != agyTurnToolResult || (r.CallID != "" && tt.CallID != "" && r.CallID != tt.CallID) {
				continue
			}
			var parsed any
			if strings.TrimSpace(r.Text) == "" || json.Unmarshal([]byte(r.Text), &parsed) != nil {
				break
			}
			if agyResultLooksFailed(parsed) {
				failed[tt.Tool] = true
			} else {
				succeeded[tt.Tool] = true
			}
			break
		}
	}
	var out []string
	for tool := range failed {
		if !succeeded[tool] {
			out = append(out, tool)
		}
	}
	sort.Strings(out)
	return out
}

// agyTruthGuard reports whether replies are checked against what the turn
// achieved. PROXY_AGY_TRUTH_GUARD=false turns it off.
func agyTruthGuard(cfg config) bool { return !cfg.AgyTruthGuardOff }

// agyReplyClaimsUnperformed names the tools a text-only reply is claiming to
// have carried out although they failed in this turn. Empty means the reply and
// the transcript agree.
func agyReplyClaimsUnperformed(cfg config, t agyTranscript, resp responsesResponse) []string {
	if !agyTruthGuard(cfg) || hasAnyToolCall(resp.Output) {
		return nil
	}
	if !agyClaimsCompletion(agyResponseText(resp)) {
		return nil
	}
	return t.unperformedThisTurn()
}
