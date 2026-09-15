package main

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Tool schemas can mark a parameter required that the tool's own instructions
// say to omit. Connect builds every HTTP tool's schema by scanning its request
// template for {{placeholders}} and marking all of them required, so
// get_available_slots arrives declaring appointment_type, duration, room_id,
// retouch_reservation_id and exclude_reservation_id as required while its
// instructions say to send appointment_type only for a retouch and never to
// send duration at all.
//
// A model that honours "required" literally then has to produce a value for a
// parameter nothing in the conversation defines, and it invents one. Observed
// in production on 2026-09-09: "standard", "new_appointment", "new", "normal",
// "full_session", and once a package name, "Full Body - Shalabi Pro". The API
// answers 422, Connect blocks the identical repeat, the model guesses again,
// and the booking dies with the customer told there is a system problem. The
// same schemas reach GPT, which satisfies "required" with an empty string
// instead — the template drops empty values — which is the only reason that
// backend books successfully.
//
// The two guards below close that gap from our side. Neither knows anything
// about appointments, clinics or any particular API: one reads the rejection
// out of the tool's own error payload, keyed by the argument names the model
// itself sent, and the other is a rule in the tool catalog.

// agyArgRejections records, per tool and parameter, the exact values that
// tool's API already rejected in this conversation.
type agyArgRejections map[string]map[string]map[string]bool

func (r agyArgRejections) add(tool, param, value string) {
	if tool == "" || param == "" {
		return
	}
	if r[tool] == nil {
		r[tool] = map[string]map[string]bool{}
	}
	if r[tool][param] == nil {
		r[tool][param] = map[string]bool{}
	}
	r[tool][param][value] = true
}

func (r agyArgRejections) rejected(tool, param, value string) bool {
	return r[tool] != nil && r[tool][param] != nil && r[tool][param][value]
}

// agyArgValueKey renders an argument value the same way whether it arrived as a
// string, a number or a bool, so "15" and 15 are one value.
func agyArgValueKey(v any) string {
	switch x := v.(type) {
	case string:
		return strings.TrimSpace(x)
	case nil:
		return ""
	default:
		if b, err := json.Marshal(x); err == nil {
			return string(b)
		}
		return fmt.Sprint(x)
	}
}

// agyResultLooksFailed reports whether a tool result carries one of the
// envelope conventions every JSON API uses to say "this did not work". It is
// deliberately about the envelope, not about any wording in the message.
func agyResultLooksFailed(node any) bool {
	switch x := node.(type) {
	case map[string]any:
		for k, v := range x {
			switch strings.ToLower(k) {
			case "success", "ok":
				if b, isBool := v.(bool); isBool && !b {
					return true
				}
			case "status", "status_code", "statuscode":
				if f, isNum := v.(float64); isNum && f >= 400 {
					return true
				}
			case "error":
				if v != nil {
					if s, isStr := v.(string); !isStr || strings.TrimSpace(s) != "" {
						return true
					}
				}
			case "errors":
				if m, isMap := v.(map[string]any); isMap && len(m) > 0 {
					return true
				}
			}
		}
		for _, v := range x {
			if agyResultLooksFailed(v) {
				return true
			}
		}
	case []any:
		for _, v := range x {
			if agyResultLooksFailed(v) {
				return true
			}
		}
	}
	return false
}

// agyCollectRejectedParams walks a failed tool result for objects whose keys are
// all parameter names of the call that produced it and whose values read as
// messages rather than data — the shape every validation error uses,
// {"appointment_type": ["The selected appointment type is invalid."]}. The keys
// come from the call's own arguments, so no list of field names is needed and
// any API's validation errors are understood.
func agyCollectRejectedParams(node any, args map[string]any, out map[string]bool) {
	switch x := node.(type) {
	case map[string]any:
		if len(x) > 0 {
			allKnown := true
			allMessages := true
			for k, v := range x {
				if _, ok := args[k]; !ok {
					allKnown = false
					break
				}
				if !agyIsMessageValue(v) {
					allMessages = false
				}
			}
			if allKnown && allMessages {
				for k := range x {
					// An echo of what we sent is data, not a complaint about it.
					if agyArgValueKey(x[k]) != agyArgValueKey(args[k]) {
						out[k] = true
					}
				}
			}
		}
		for _, v := range x {
			agyCollectRejectedParams(v, args, out)
		}
	case []any:
		for _, v := range x {
			agyCollectRejectedParams(v, args, out)
		}
	}
}

// agyIsMessageValue reports whether a value is a string or a list of strings,
// which is how validation errors are carried.
func agyIsMessageValue(v any) bool {
	switch x := v.(type) {
	case string:
		return true
	case []any:
		if len(x) == 0 {
			return false
		}
		for _, e := range x {
			if _, ok := e.(string); !ok {
				return false
			}
		}
		return true
	}
	return false
}

// agyRejectedArgs reads the whole transcript and returns every (tool,
// parameter, value) the API has already refused.
func agyRejectedArgs(t agyTranscript) agyArgRejections {
	out := agyArgRejections{}
	for i, turn := range t.Turns {
		if turn.Kind != agyTurnToolResult || turn.Tool == "" || strings.TrimSpace(turn.Text) == "" {
			continue
		}
		call, ok := agyCallForResult(t.Turns, i)
		if !ok {
			continue
		}
		var args map[string]any
		if json.Unmarshal([]byte(call.Args), &args) != nil || len(args) == 0 {
			continue
		}
		var result any
		if json.Unmarshal([]byte(turn.Text), &result) != nil {
			continue
		}
		if !agyResultLooksFailed(result) {
			continue
		}
		bad := map[string]bool{}
		agyCollectRejectedParams(result, args, bad)
		for param := range bad {
			out.add(turn.Tool, param, agyArgValueKey(args[param]))
		}
	}
	return out
}

// agyCallForResult finds the tool call a result at index i belongs to: the
// nearest preceding call with the same id, or failing that the same tool.
func agyCallForResult(turns []agyTurn, i int) (agyTurn, bool) {
	res := turns[i]
	for j := i - 1; j >= 0; j-- {
		c := turns[j]
		if c.Kind != agyTurnToolCall {
			continue
		}
		if res.CallID != "" && c.CallID == res.CallID {
			return c, true
		}
		if res.CallID == "" && c.Tool == res.Tool {
			return c, true
		}
	}
	return agyTurn{}, false
}

// agyStripRejectedArgs drops arguments whose exact value this tool's API has
// already refused in this conversation, and reports what it dropped. A value
// the API accepted, or one it has never seen, is never touched — so a genuine
// "retouch" still goes through after an invented "standard" was refused.
func agyStripRejectedArgs(rejections agyArgRejections, tool, argsJSON string) (string, []string) {
	if len(rejections) == 0 || rejections[tool] == nil {
		return argsJSON, nil
	}
	var args map[string]any
	if json.Unmarshal([]byte(argsJSON), &args) != nil || len(args) == 0 {
		return argsJSON, nil
	}
	var dropped []string
	for k, v := range args {
		if rejections.rejected(tool, k, agyArgValueKey(v)) {
			dropped = append(dropped, k)
		}
	}
	if len(dropped) == 0 {
		return argsJSON, nil
	}
	sort.Strings(dropped)
	for _, k := range dropped {
		delete(args, k)
	}
	return canonicalToolArgs(args), dropped
}

// agySchemaParts pulls the properties map and required set out of a tool schema.
// Used by the tests that lock down what the caller's schema must still say when
// it reaches the model.
func agySchemaParts(schema any) (map[string]any, map[string]bool, bool) {
	m, ok := schema.(map[string]any)
	if !ok {
		return nil, nil, false
	}
	props, ok := m["properties"].(map[string]any)
	if !ok || len(props) == 0 {
		return nil, nil, false
	}
	required := map[string]bool{}
	switch reqs := m["required"].(type) {
	case []any:
		for _, r := range reqs {
			if s, isStr := r.(string); isStr {
				required[s] = true
			}
		}
	case []string:
		for _, s := range reqs {
			required[s] = true
		}
	}
	return props, required, true
}

// ---------------------------------------------------------------------------
// Persona leak: the shape, not the words
// ---------------------------------------------------------------------------

// A model that has decided it is being evaluated writes to the evaluator first
// and to the customer second, and the two audiences show up as two languages in
// one message. Prod 2026-09-13 conv 58921 reached a patient as ~370 characters
// of English — "It looks like there's an active automated workflow / benchmark
// prompt structure being passed here without direct tool access to the clinic
// systems … here is the direct, compliant response for Zeina:" — glued in front
// of the correct Arabic answer. The phrase list did not have those words, and
// never will have the next set.
//
// The shape is checkable without them: the customer has written only in one
// script all conversation, and the reply carries a long uninterrupted passage in
// a different one plus the real answer in theirs. Brand names, device names and
// URLs are short; a paragraph addressed to somebody else is not. Nothing here
// knows Arabic, English or any business domain — it compares the reply's scripts
// against the customer's own.

// agyForeignPreambleBytes is the shortest run of other-script prose worth
// treating as a passage rather than a name. The leak was 370; the longest
// legitimate run in the same clinic's replies (a maps URL, "Candela GentleMax
// Pro", "Duetto MT Evo … Nd:YAG") is under 60.
const agyForeignPreambleBytes = 200

// agyMinAnswerLetters is how much of the customer's own script the reply must
// also carry before a foreign passage counts as a preamble glued to an answer.
const agyMinAnswerLetters = 40

// agyScriptCounts returns how many letters of a text are Latin and how many
// belong to some other alphabet. Digits, spaces and punctuation count as
// neither, so they never tip the balance.
func agyScriptCounts(s string) (latin, other int) {
	for _, r := range s {
		if !unicode.IsLetter(r) {
			continue
		}
		if r < unicode.MaxASCII || unicode.Is(unicode.Latin, r) {
			latin++
		} else {
			other++
		}
	}
	return latin, other
}

// agyLongestLatinRun measures the longest stretch of text that contains Latin
// letters and no letters of any other script. Punctuation, digits and spaces
// continue a run; a letter from another script ends it.
func agyLongestLatinRun(s string) int {
	best, cur := 0, 0
	for _, r := range s {
		if unicode.IsLetter(r) && !(r < unicode.MaxASCII || unicode.Is(unicode.Latin, r)) {
			cur = 0
			continue
		}
		cur += utf8.RuneLen(r)
		if cur > best {
			best = cur
		}
	}
	return best
}

// agyCustomerWritesNonLatin reports whether every customer message in this
// conversation is written in a non-Latin script. A customer who writes in
// English at all makes an English reply ordinary, and the check stands down.
func agyCustomerWritesNonLatin(t agyTranscript) bool {
	var latin, other int
	for _, turn := range t.Turns {
		if turn.Kind != agyTurnCustomer {
			continue
		}
		l, o := agyScriptCounts(stripCustomerHeader(turn.Text))
		latin += l
		other += o
	}
	// Enough of their words to judge, and overwhelmingly not Latin. Names and
	// brand words they type in English are what the margin is for.
	return other >= 20 && latin*4 < other
}

// agyReplyHasForeignPreamble reports whether a reply pairs a long passage in a
// script the customer never writes with a real answer in the script they do —
// the two-audiences shape of a leaked runtime preamble.
func agyReplyHasForeignPreamble(t agyTranscript, reply string) bool {
	if !agyCustomerWritesNonLatin(t) {
		return false
	}
	if _, other := agyScriptCounts(reply); other < agyMinAnswerLetters {
		return false
	}
	return agyLongestLatinRun(reply) >= agyForeignPreambleBytes
}
