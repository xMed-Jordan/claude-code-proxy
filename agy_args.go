package main

import (
	"encoding/json"
	"fmt"
	"log"
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

// A tool schema that marks every one of its parameters required, and describes
// each of them with the same boilerplate the rest of the catalog uses, is not
// stating a requirement — it is a generator that had nothing to say. Connect
// builds one for every HTTP tool by scanning the request template for
// {{placeholders}}: all of them land in `required`, each described as "Value for
// X parameter". The tool's real instructions say the opposite for several of
// them, so the model is told it MUST send a parameter nothing defines, and it
// invents a value.
//
// Dropping such a list is what stops the invention at the source, before the
// model drafts anything. The two conditions below are what make that safe, and
// both are read off the catalog in front of us rather than from any knowledge of
// this platform:
//
//   - Every parameter is required. A list naming all of them constrains nothing
//     that leaving it out would not, so no information is lost by dropping it.
//   - Every parameter's description is boilerplate: once the parameter's own
//     name is removed, the same wording appears under other tools too. A
//     description written for this tool is information; one shared with forty
//     other tools is a template.
//
// Hand-written schemas fail the second test and keep their required lists:
// across the 63 tools Connect sends, save_memory, get_tool_instructions,
// send_media and get_memory all describe their parameters in their own words
// and are left exactly as they came.

// agyDescTemplate reduces a parameter description to the wording it shares with
// other parameters: its own name blanked out, lowercased, whitespace collapsed.
func agyDescTemplate(name, desc string) string {
	d := strings.ToLower(strings.TrimSpace(desc))
	if d == "" {
		return ""
	}
	if n := strings.ToLower(strings.TrimSpace(name)); n != "" {
		d = strings.ReplaceAll(d, n, "@")
	}
	return strings.Join(strings.Fields(d), " ")
}

// agySchemaParts pulls the properties map and required set out of a schema.
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

// agyPropDescription reads a property's description, whatever shape it arrived in.
func agyPropDescription(def any) string {
	m, ok := def.(map[string]any)
	if !ok {
		return ""
	}
	s, _ := m["description"].(string)
	return s
}

// agyBoilerplateTemplates returns the description wordings that appear under
// more than one tool, which is what makes them boilerplate rather than
// documentation. An empty description says nothing either way and counts too.
func agyBoilerplateTemplates(tools []agyToolCatalog) map[string]bool {
	perTemplate := map[string]map[string]bool{}
	for _, t := range tools {
		props, _, ok := agySchemaParts(t.Schema)
		if !ok {
			continue
		}
		for name, def := range props {
			tpl := agyDescTemplate(name, agyPropDescription(def))
			if perTemplate[tpl] == nil {
				perTemplate[tpl] = map[string]bool{}
			}
			perTemplate[tpl][t.Name] = true
		}
	}
	out := map[string]bool{"": true}
	for tpl, tools := range perTemplate {
		if len(tools) > 1 {
			out[tpl] = true
		}
	}
	return out
}

// agyRepairToolSchemas drops the required list of every tool whose list names
// all of its parameters and whose parameters are all described in boilerplate.
// The properties themselves are untouched: the model still sees every parameter
// the tool accepts, and the tool's own instructions still say which ones matter.
func agyRepairToolSchemas(tools []agyToolCatalog) []agyToolCatalog {
	if len(tools) == 0 {
		return tools
	}
	boilerplate := agyBoilerplateTemplates(tools)
	var repaired []string
	out := make([]agyToolCatalog, len(tools))
	copy(out, tools)
	for i, t := range out {
		props, required, ok := agySchemaParts(t.Schema)
		if !ok || len(required) == 0 || len(required) != len(props) {
			continue
		}
		informative := false
		for name, def := range props {
			if !boilerplate[agyDescTemplate(name, agyPropDescription(def))] {
				informative = true
				break
			}
		}
		if informative {
			continue
		}
		m := t.Schema.(map[string]any)
		clone := make(map[string]any, len(m))
		for k, v := range m {
			if k == "required" {
				continue
			}
			clone[k] = v
		}
		out[i].Schema = clone
		repaired = append(repaired, t.Name)
	}
	if len(repaired) > 0 {
		sort.Strings(repaired)
		log.Printf("[agy-schema] %d of %d tools marked every parameter required with boilerplate descriptions; dropped those lists so the model is not pushed to invent values (%s%s)",
			len(repaired), len(tools), strings.Join(repaired[:min(4, len(repaired))], ", "),
			map[bool]string{true: ", …"}[len(repaired) > 4])
	}
	return out
}
