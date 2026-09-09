package main

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
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
