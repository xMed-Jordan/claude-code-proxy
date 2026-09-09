package main

// agy_transcript.go — the v2 prompt for the "agy" (Antigravity CLI) upstream.
//
// Why this exists. Codex/Claude receive the conversation as native structured
// turns (system / user / assistant tool_use / tool_result) plus native tool
// schemas, so the model always knows exactly where it is in the dialogue. agy
// only accepts one prompt string, and the previous flattening tried to make up
// for that with a thick layer of booking-specific keyword heuristics
// ("APPOINTMENT CONFIRMATION DETECTED", "DO NOT call get_available_slots",
// "Provide the available slots ... and ask which time she prefers", synthetic
// retry prompts without persona/tools/history, collapsing of repeated calls,
// duplicated customer-statement lists…). Whenever a customer phrase fell
// outside the keyword list the directives contradicted the actual state of the
// conversation and the model either re-asked an answered question or looped on
// the same tool. The Faiza conversation (2026-09-08, conv 58214) is the
// reference failure: three turns in a row ended in a proxy-authored "which
// time suits you?" reset.
//
// The v2 design mirrors what the native upstreams get, in text:
//   1. the caller's system prompt verbatim (persona untouched),
//   2. the tool catalog with the full JSON input schemas,
//   3. a faithful, role-labelled transcript (customer / assistant / tool call /
//      tool result / system note) with nothing removed or reordered,
//   4. a short, mechanically derived state block (which tools have been
//      preflighted, which calls already ran since the customer's latest
//      message) and a neutral "write your next turn" instruction.
// No business rules, no keyword matching. Loops are handled generically after
// generation (see agyGenerate): a tool call that repeats an already-executed
// call of the current turn, or names a tool that does not exist, triggers a
// correction retry that keeps the FULL prompt and simply tells the model what
// was wrong.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// agyTurnKind enumerates the transcript entry types.
type agyTurnKind int

const (
	agyTurnCustomer agyTurnKind = iota
	agyTurnAssistant
	agyTurnToolCall
	agyTurnToolResult
	agyTurnSystemNote
)

// agyTurn is one transcript entry.
type agyTurn struct {
	Kind    agyTurnKind
	Text    string // customer / assistant / system-note text, or tool-result content
	Tool    string // tool name (tool call + tool result)
	Args    string // canonical JSON arguments (tool call)
	CallID  string // tool_use id (tool call + tool result)
	Reduced bool   // tool result: shortened or dropped to fit the input window
}

// agyTranscript is the structured view of an incoming request.
type agyTranscript struct {
	Turns []agyTurn
}

// reducedTools names the tools whose results were shortened or dropped to fit
// the input window. Those tools must be exempt from "you already ran this, do
// not run it again": the data the model was pointed at is no longer all there,
// so calling again is the correct move, not a loop.
func (t *agyTranscript) reducedTools() map[string]bool {
	out := map[string]bool{}
	for _, tt := range t.Turns {
		if tt.Kind == agyTurnToolResult && tt.Reduced && tt.Tool != "" {
			out[tt.Tool] = true
		}
	}
	return out
}

var (
	agyCustomerHeaderRe   = regexp.MustCompile(`^\[[^\]\n]*\|[^\]\n]*\]\s*`)
	agyAutoContextToolRe  = regexp.MustCompile(`executed the tool "([^"]+)"`)
	agyPreflightedToolsRe = regexp.MustCompile(`(?i)PREFLIGHTED[^"\n]*"([^"]+)"`)
)

// canonicalToolArgs renders tool arguments as sorted-key compact JSON so two
// calls can be compared regardless of key order or whitespace.
func canonicalToolArgs(v any) string {
	if s, ok := v.(string); ok {
		var parsed any
		if json.Unmarshal([]byte(s), &parsed) == nil {
			v = parsed
		} else {
			return strings.TrimSpace(s)
		}
	}
	if v == nil {
		return "{}"
	}
	b, err := json.Marshal(v) // Go sorts map keys
	if err != nil {
		return fmt.Sprint(v)
	}
	return string(b)
}

// stripCustomerHeader removes Connect's "[Tue, Sep 8, 12:33 PM | Phone: … | Platform: …]"
// prefix so a customer message can be compared with its unheadered copy.
func stripCustomerHeader(s string) string {
	return strings.TrimSpace(agyCustomerHeaderRe.ReplaceAllString(strings.TrimSpace(s), ""))
}

func (t *agyTranscript) addCustomer(text string) {
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}
	if strings.HasPrefix(text, "[AUTO-CONTEXT") || strings.HasPrefix(text, "[SYSTEM ERROR") || strings.HasPrefix(text, "[SYSTEM NOTE") {
		t.Turns = append(t.Turns, agyTurn{Kind: agyTurnSystemNote, Text: text})
		return
	}
	// Connect sends the current customer message twice: once as the persisted
	// history row and once as the prompt with a metadata header. Keep only the
	// latest copy when the two are adjacent and identical modulo the header.
	if n := len(t.Turns); n > 0 && t.Turns[n-1].Kind == agyTurnCustomer &&
		stripCustomerHeader(t.Turns[n-1].Text) == stripCustomerHeader(text) {
		if len(text) >= len(t.Turns[n-1].Text) {
			t.Turns[n-1].Text = text
		}
		return
	}
	t.Turns = append(t.Turns, agyTurn{Kind: agyTurnCustomer, Text: text})
}

func (t *agyTranscript) addAssistantText(text string) {
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}
	// Some callers persist tool calls as <tool_call> text inside assistant
	// messages; lift those into structured tool-call turns.
	if strings.Contains(text, "<tool_call>") {
		clean, items := parseAgyToolCalls(text)
		if clean = strings.TrimSpace(clean); clean != "" {
			t.Turns = append(t.Turns, agyTurn{Kind: agyTurnAssistant, Text: clean})
		}
		for _, it := range items {
			t.addToolCall(it.CallID, it.Name, it.Arguments)
		}
		return
	}
	t.Turns = append(t.Turns, agyTurn{Kind: agyTurnAssistant, Text: text})
}

func (t *agyTranscript) addToolCall(id, name string, args any) {
	name = strings.TrimSpace(name)
	if name == "" {
		return
	}
	t.Turns = append(t.Turns, agyTurn{Kind: agyTurnToolCall, Tool: name, Args: canonicalToolArgs(args), CallID: strings.TrimSpace(id)})
}

func (t *agyTranscript) addToolResult(id, content string) {
	id = strings.TrimSpace(id)
	name := ""
	// Pair the result with the most recent unanswered call (by id first, then position).
	answered := map[string]bool{}
	for _, tt := range t.Turns {
		if tt.Kind == agyTurnToolResult && tt.CallID != "" {
			answered[tt.CallID] = true
		}
	}
	for i := len(t.Turns) - 1; i >= 0; i-- {
		tt := t.Turns[i]
		if tt.Kind == agyTurnCustomer {
			break
		}
		if tt.Kind != agyTurnToolCall {
			continue
		}
		if (id != "" && tt.CallID == id) || (id == "" && !answered[tt.CallID]) {
			name = tt.Tool
			if id == "" {
				id = tt.CallID
			}
			break
		}
	}
	if name == "" {
		// Connect wraps historical results as {"name":..,"result":..}; use that name.
		var wrapped struct {
			Name string `json:"name"`
		}
		if json.Unmarshal([]byte(strings.TrimSpace(content)), &wrapped) == nil {
			name = wrapped.Name
		}
	}
	t.Turns = append(t.Turns, agyTurn{Kind: agyTurnToolResult, Tool: name, CallID: id, Text: unescapeJSONUnicode(strings.TrimSpace(content))})
}

// unescapeJSONUnicode rewrites \uXXXX escapes (incl. surrogate pairs) and \/
// in a JSON text into literal UTF-8 while leaving the document valid: quotes,
// backslashes and control characters stay escaped. Connect serialises tool
// results with PHP's default json_encode, so Arabic payloads arrive as
// six-byte escapes per letter; decoding them makes the transcript ~3x smaller
// and readable for the model without changing its meaning.
func unescapeJSONUnicode(s string) string {
	if !strings.Contains(s, `\u`) && !strings.Contains(s, `\/`) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	hex4 := func(i int) (rune, bool) {
		if i+4 > len(s) {
			return 0, false
		}
		v, err := strconv.ParseUint(s[i:i+4], 16, 32)
		if err != nil {
			return 0, false
		}
		return rune(v), true
	}
	for i := 0; i < len(s); {
		c := s[i]
		if c != '\\' || i+1 >= len(s) {
			b.WriteByte(c)
			i++
			continue
		}
		switch s[i+1] {
		case '/':
			b.WriteByte('/')
			i += 2
		case 'u':
			r, ok := hex4(i + 2)
			if !ok {
				b.WriteString(s[i : i+2])
				i += 2
				continue
			}
			n := 6
			if r >= 0xD800 && r <= 0xDBFF && i+12 <= len(s) && s[i+6] == '\\' && s[i+7] == 'u' {
				if lo, ok := hex4(i + 8); ok && lo >= 0xDC00 && lo <= 0xDFFF {
					r = (r-0xD800)<<10 + (lo - 0xDC00) + 0x10000
					n = 12
				}
			}
			if r < 0x20 || r == '"' || r == '\\' || (r >= 0xD800 && r <= 0xDFFF) {
				b.WriteString(s[i : i+n]) // must stay escaped
			} else {
				b.WriteRune(r)
			}
			i += n
		default:
			b.WriteString(s[i : i+2]) // keep every other escape verbatim
			i += 2
		}
	}
	return b.String()
}

// lastCustomerIndex returns the index of the latest customer turn (-1 if none).
func (t *agyTranscript) lastCustomerIndex() int {
	for i := len(t.Turns) - 1; i >= 0; i-- {
		if t.Turns[i].Kind == agyTurnCustomer {
			return i
		}
	}
	return -1
}

// currentTurnCalls returns the tool calls made since the customer's latest
// message, i.e. the calls of the turn the model is currently completing.
func (t *agyTranscript) currentTurnCalls() []agyTurn {
	var out []agyTurn
	for i := t.lastCustomerIndex() + 1; i < len(t.Turns); i++ {
		if t.Turns[i].Kind == agyTurnToolCall {
			out = append(out, t.Turns[i])
		}
	}
	return out
}

// calledThisTurn reports whether an identical call (name + canonical args)
// already exists in the current turn.
func (t *agyTranscript) calledThisTurn(name string, args any) bool {
	want := canonicalToolArgs(args)
	for _, c := range t.currentTurnCalls() {
		if c.Tool == name && c.Args == want {
			return true
		}
	}
	return false
}

// agyToolStat summarises one tool's activity in the current turn.
type agyToolStat struct {
	Count            int  // calls made since the customer's latest message
	IdenticalResults bool // ≥2 calls returned byte-identical output (args don't matter)
}

// currentTurnToolStats groups the current turn's calls per tool, pairing each
// call with its result, so agyGenerate can spot "same tool, reworded
// arguments" loops that identical-argument matching misses.
func (t *agyTranscript) currentTurnToolStats() map[string]agyToolStat {
	stats := map[string]agyToolStat{}
	results := map[string][]string{}
	start := t.lastCustomerIndex() + 1
	for i := start; i < len(t.Turns); i++ {
		tt := t.Turns[i]
		if tt.Kind != agyTurnToolCall {
			continue
		}
		st := stats[tt.Tool]
		st.Count++
		stats[tt.Tool] = st
		for j := i + 1; j < len(t.Turns); j++ {
			r := t.Turns[j]
			if r.Kind == agyTurnToolResult && (r.CallID == tt.CallID || r.CallID == "") {
				results[tt.Tool] = append(results[tt.Tool], r.Text)
				break
			}
			if r.Kind == agyTurnToolCall || r.Kind == agyTurnCustomer {
				break
			}
		}
	}
	for tool, rs := range results {
		for i := 1; i < len(rs); i++ {
			if rs[i] != "" && rs[i] == rs[i-1] {
				st := stats[tool]
				st.IdenticalResults = true
				stats[tool] = st
				break
			}
		}
	}
	return stats
}

// agyCallOutcome pairs a current-turn tool call with what its result looked
// like: Failure is a short excerpt when the result is an error envelope
// ("success":false, an HTTP 4xx/5xx status, or a non-null "error"), else "".
type agyCallOutcome struct {
	Call    agyTurn
	Failure string
}

var agyResultHTTPErrorRe = regexp.MustCompile(`"status":\s*"?([45]\d\d)`)

// currentTurnCallOutcomes lists the current turn's calls with their outcome so
// the state block can mark failed calls explicitly. A model reading a 33KB
// transcript can miss that its slots lookup came back as HTTP 422 and answer
// as if it had data (prod 2026-09-08 conv 58311: after a 422 for an invalid
// appointment_type the reply listed invented morning/afternoon/evening
// availability). Purely mechanical: nothing here knows what the tool does.
func (t *agyTranscript) currentTurnCallOutcomes() []agyCallOutcome {
	var out []agyCallOutcome
	for i := t.lastCustomerIndex() + 1; i < len(t.Turns); i++ {
		tt := t.Turns[i]
		if tt.Kind != agyTurnToolCall {
			continue
		}
		oc := agyCallOutcome{Call: tt}
		for j := i + 1; j < len(t.Turns); j++ {
			r := t.Turns[j]
			if r.Kind == agyTurnToolResult && (r.CallID == tt.CallID || r.CallID == "") {
				oc.Failure = toolResultFailure(r.Text)
				break
			}
			if r.Kind == agyTurnToolCall || r.Kind == agyTurnCustomer {
				break
			}
		}
		out = append(out, oc)
	}
	return out
}

// toolResultFailure returns a short description when a tool result looks like
// an error envelope, else "". The excerpt prefers the envelope's own message.
func toolResultFailure(result string) string {
	compact := strings.ReplaceAll(result, " ", "")
	failed := strings.Contains(compact, `"success":false`)
	if !failed {
		if m := agyResultHTTPErrorRe.FindStringSubmatch(compact); m != nil {
			failed = true
		}
	}
	if !failed && strings.HasPrefix(strings.TrimSpace(result), "{") && !strings.Contains(compact, `"success":true`) {
		// An "error" that is not null/empty in an envelope without success:true.
		if idx := strings.Index(compact, `"error":`); idx >= 0 {
			rest := compact[idx+len(`"error":`):]
			if !strings.HasPrefix(rest, "null") && !strings.HasPrefix(rest, `""`) && !strings.HasPrefix(rest, "{}") && !strings.HasPrefix(rest, "[]") {
				failed = true
			}
		}
	}
	if !failed {
		return ""
	}
	var parts []string
	if m := agyResultHTTPErrorRe.FindStringSubmatch(compact); m != nil {
		parts = append(parts, "HTTP "+m[1])
	}
	for _, key := range []string{`"message":"`, `"error":"`, `"hint":"`} {
		if idx := strings.Index(result, key); idx >= 0 {
			s := result[idx+len(key):]
			if end := strings.Index(s, `"`); end > 0 {
				s = s[:end]
			}
			if s = strings.TrimSpace(s); s != "" {
				parts = append(parts, truncateString(s, 160))
				break
			}
		}
	}
	if len(parts) == 0 {
		parts = append(parts, truncateString(strings.Join(strings.Fields(result), " "), 160))
	}
	return strings.Join(parts, ": ")
}

// currentTurnCallSucceeded reports whether an identical call in the current
// turn has a result that looks like a success ("success":true, or an HTTP
// 2xx status in the result envelope). Used to word the repeat correction.
func (t *agyTranscript) currentTurnCallSucceeded(name string, args any) bool {
	want := canonicalToolArgs(args)
	for i := t.lastCustomerIndex() + 1; i < len(t.Turns); i++ {
		tt := t.Turns[i]
		if tt.Kind != agyTurnToolCall || tt.Tool != name || tt.Args != want {
			continue
		}
		for j := i + 1; j < len(t.Turns); j++ {
			r := t.Turns[j]
			if r.Kind == agyTurnToolResult && (r.CallID == tt.CallID || r.CallID == "") {
				compact := strings.ReplaceAll(r.Text, " ", "")
				if strings.Contains(compact, `"success":true`) || strings.Contains(compact, `"status":20`) {
					return true
				}
				break
			}
			if r.Kind == agyTurnToolCall || r.Kind == agyTurnCustomer {
				break
			}
		}
	}
	return false
}

// previousAssistantText returns the assistant's last customer-facing message
// before the customer's latest message ("" when there is none).
func (t *agyTranscript) previousAssistantText() string {
	li := t.lastCustomerIndex()
	for i := li - 1; i >= 0; i-- {
		if t.Turns[i].Kind == agyTurnAssistant {
			return t.Turns[i].Text
		}
		if t.Turns[i].Kind == agyTurnCustomer {
			break
		}
	}
	return ""
}

// executedToolSummary lists the tools executed before the customer's latest
// message as "name args" lines (deduplicated, with a repeat count), so the
// model can see what it already knows without re-reading the whole transcript.
func (t *agyTranscript) executedToolSummary() []string {
	li := t.lastCustomerIndex()
	reduced := t.reducedTools()
	type key struct{ tool, args string }
	counts := map[key]int{}
	var order []key
	for i := 0; i < li; i++ {
		tt := t.Turns[i]
		if tt.Kind != agyTurnToolCall || tt.Tool == "get_tool_instructions" {
			continue
		}
		k := key{tt.Tool, tt.Args}
		if counts[k] == 0 {
			order = append(order, k)
		}
		counts[k]++
	}
	out := make([]string, 0, len(order))
	for _, k := range order {
		line := k.tool + " " + truncateString(k.args, 160)
		if counts[k] > 1 {
			line += fmt.Sprintf(" (×%d)", counts[k])
		}
		if reduced[k.tool] {
			line += " — its result in the transcript is INCOMPLETE (shortened or dropped by the system to fit this message); call it again whenever you need data it no longer shows"
		}
		out = append(out, line)
	}
	return out
}

// preflightedTools lists the tools whose instructions were already fetched in
// this conversation (Connect's get_tool_instructions cache): explicit
// get_tool_instructions calls, tools that already executed (Connect only lets a
// tool run after its preflight), and tools named in [AUTO-CONTEXT] notes.
func (t *agyTranscript) preflightedTools() []string {
	set := map[string]bool{}
	for _, tt := range t.Turns {
		switch tt.Kind {
		case agyTurnToolCall:
			if tt.Tool == "get_tool_instructions" {
				var a struct {
					ToolCode string `json:"tool_code"`
				}
				if json.Unmarshal([]byte(tt.Args), &a) == nil && strings.TrimSpace(a.ToolCode) != "" {
					set[strings.TrimSpace(a.ToolCode)] = true
				}
			} else {
				set[tt.Tool] = true
			}
		case agyTurnSystemNote:
			for _, m := range agyAutoContextToolRe.FindAllStringSubmatch(tt.Text, -1) {
				set[m[1]] = true
			}
			for _, m := range agyPreflightedToolsRe.FindAllStringSubmatch(tt.Text, -1) {
				set[m[1]] = true
			}
		}
	}
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ---------------------------------------------------------------------------
// Builders (one per inbound request shape)
// ---------------------------------------------------------------------------

func buildAgyTranscriptFromAnthropic(in anthropicRequest) agyTranscript {
	var t agyTranscript
	for _, msg := range in.Messages {
		role := strings.ToLower(strings.TrimSpace(msg.Role))
		switch role {
		case "tool":
			t.addToolResult(msg.ToolCallID, contentToTextNoMedia(msg.Content))
		case "assistant":
			if blocks, ok := msg.Content.([]any); ok {
				for _, b := range blocks {
					m, ok := b.(map[string]any)
					if !ok {
						t.addAssistantText(contentToTextNoMedia(b))
						continue
					}
					switch fmt.Sprint(m["type"]) {
					case "text":
						t.addAssistantText(fmt.Sprint(m["text"]))
					case "tool_use":
						id, _ := m["id"].(string)
						name, _ := m["name"].(string)
						t.addToolCall(id, name, m["input"])
					case "thinking", "redacted_thinking", "image", "document", "file", "audio", "video":
						// not part of the dialogue
					default:
						t.addAssistantText(contentToTextNoMedia(m))
					}
				}
			} else if msg.Content != nil {
				t.addAssistantText(contentToTextNoMedia(msg.Content))
			}
			for _, tc := range msg.ToolCalls {
				t.addToolCall(tc.ID, tc.GetName(), tc.GetArguments())
			}
		case "system", "developer":
			if s := strings.TrimSpace(contentToTextNoMedia(msg.Content)); s != "" {
				t.Turns = append(t.Turns, agyTurn{Kind: agyTurnSystemNote, Text: s})
			}
		default: // user
			if blocks, ok := msg.Content.([]any); ok {
				var texts []string
				for _, b := range blocks {
					m, ok := b.(map[string]any)
					if !ok {
						if s := strings.TrimSpace(contentToTextNoMedia(b)); s != "" {
							texts = append(texts, s)
						}
						continue
					}
					switch fmt.Sprint(m["type"]) {
					case "tool_result":
						if len(texts) > 0 {
							t.addCustomer(strings.Join(texts, "\n"))
							texts = nil
						}
						id, _ := m["tool_use_id"].(string)
						t.addToolResult(id, contentToTextNoMedia(m["content"]))
					case "text":
						if s := strings.TrimSpace(fmt.Sprint(m["text"])); s != "" {
							texts = append(texts, s)
						}
					default:
						if _, isMedia := mediaPartFromBlock(m); isMedia {
							continue
						}
						if s := strings.TrimSpace(contentToTextNoMedia(m)); s != "" {
							texts = append(texts, s)
						}
					}
				}
				if len(texts) > 0 {
					t.addCustomer(strings.Join(texts, "\n"))
				}
			} else {
				t.addCustomer(contentToTextNoMedia(msg.Content))
			}
		}
	}
	return t
}

func buildAgyTranscriptFromOpenAIChat(in openAIRequest) (agyTranscript, []string) {
	var t agyTranscript
	var sysParts []string
	for _, msg := range in.Messages {
		role := strings.ToLower(strings.TrimSpace(msg.Role))
		switch role {
		case "system", "developer":
			if s := strings.TrimSpace(contentToTextNoMedia(msg.Content)); s != "" {
				sysParts = append(sysParts, s)
			}
		case "tool":
			t.addToolResult(msg.ToolCallID, contentToTextNoMedia(msg.Content))
		case "assistant":
			t.addAssistantText(contentToTextNoMedia(msg.Content))
			for _, tc := range msg.ToolCalls {
				t.addToolCall(tc.ID, tc.GetName(), tc.GetArguments())
			}
		default:
			t.addCustomer(contentToTextNoMedia(msg.Content))
		}
	}
	return t, sysParts
}

func buildAgyTranscriptFromResponses(in responsesRequest) agyTranscript {
	var t agyTranscript
	for _, raw := range in.Input {
		m, ok := raw.(map[string]any)
		if !ok {
			t.addCustomer(contentToTextNoMedia(raw))
			continue
		}
		itemType, _ := m["type"].(string)
		role, _ := m["role"].(string)
		switch itemType {
		case "function_call":
			name, _ := m["name"].(string)
			id, _ := m["call_id"].(string)
			t.addToolCall(id, name, m["arguments"])
			continue
		case "function_call_output":
			id, _ := m["call_id"].(string)
			t.addToolResult(id, contentToTextNoMedia(m["output"]))
			continue
		}
		text := strings.TrimSpace(contentToTextNoMedia(m["content"]))
		if text == "" {
			if o, ok := m["output"].(string); ok {
				text = strings.TrimSpace(o)
			}
		}
		switch strings.ToLower(strings.TrimSpace(role)) {
		case "assistant":
			t.addAssistantText(text)
		case "system", "developer":
			if text != "" {
				t.Turns = append(t.Turns, agyTurn{Kind: agyTurnSystemNote, Text: text})
			}
		default:
			t.addCustomer(text)
		}
	}
	return t
}

// ---------------------------------------------------------------------------
// Rendering
// ---------------------------------------------------------------------------

// agyToolCatalog is the provider-neutral tool list rendered into the prompt.
type agyToolCatalog struct {
	Name        string
	Description string
	Schema      any
}

func agyCatalogFromAnthropic(tools []anthropicTool) []agyToolCatalog {
	out := make([]agyToolCatalog, 0, len(tools))
	for _, t := range tools {
		if strings.TrimSpace(t.Name) == "" {
			continue
		}
		out = append(out, agyToolCatalog{Name: t.Name, Description: t.Description, Schema: t.InputSchema})
	}
	return out
}

func agyCatalogFromOpenAI(tools []openAITool) []agyToolCatalog {
	out := make([]agyToolCatalog, 0, len(tools))
	for _, t := range tools {
		if strings.TrimSpace(t.Function.Name) == "" {
			continue
		}
		out = append(out, agyToolCatalog{Name: t.Function.Name, Description: t.Function.Description, Schema: t.Function.Parameters})
	}
	return out
}

func agyCatalogFromResponses(tools []responsesTool) []agyToolCatalog {
	out := make([]agyToolCatalog, 0, len(tools))
	for _, t := range tools {
		if strings.TrimSpace(t.Name) == "" {
			continue
		}
		out = append(out, agyToolCatalog{Name: t.Name, Description: t.Description, Schema: t.Parameters})
	}
	return out
}

// agySchemaParamNames lists a JSON-schema object's property names, required
// ones first, so a compacted entry stays callable without its full schema.
func agySchemaParamNames(schema any) []string {
	m, ok := schema.(map[string]any)
	if !ok {
		return nil
	}
	props, ok := m["properties"].(map[string]any)
	if !ok || len(props) == 0 {
		return nil
	}
	required := map[string]bool{}
	if reqs, ok := m["required"].([]any); ok {
		for _, r := range reqs {
			if s, ok := r.(string); ok {
				required[s] = true
			}
		}
	}
	var req, opt []string
	for name := range props {
		if required[name] {
			req = append(req, name)
		} else {
			opt = append(opt, name)
		}
	}
	sort.Strings(req)
	sort.Strings(opt)
	for i, name := range req {
		req[i] = name + "*"
	}
	return append(req, opt...)
}

// firstSentence returns the leading sentence of s, capped, for compact entries.
func firstSentence(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if i := strings.IndexAny(s, ".\n"); i > 0 && i < max {
		s = s[:i+1]
	}
	return truncateString(s, max)
}

// renderAgyToolCatalog renders the tool section. In compact mode only the tools
// in detailed keep their full input schema; the rest are listed with a one-line
// description and their parameter names (required marked with *). The full
// catalog of 63 Connect tools is ~44KB, a quarter of agy's whole input window,
// and a turn typically uses six of them; the compact form keeps every tool
// callable while freeing that space for the conversation. Connect's own flow
// covers the rest: get_tool_instructions returns a tool's parameters before its
// first use.
func renderAgyToolCatalog(tools []agyToolCatalog, detailed map[string]bool, compact bool) string {
	if len(tools) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("### TOOLS\n\n")
	b.WriteString("You can call the tools listed below. To call a tool, output a <tool_call> block with a JSON object holding the tool name and its input:\n\n")
	b.WriteString("<tool_call>\n{\"tool\": \"tool_name\", \"input\": {\"param\": \"value\"}}\n</tool_call>\n\n")
	b.WriteString("Tool-calling rules:\n")
	b.WriteString("- A response that calls tools must contain ONLY <tool_call> blocks (one block per call, several blocks allowed) and nothing else.\n")
	b.WriteString("- After you emit tool calls, STOP. The system runs them and sends you their results in the transcript. Never write or guess a tool result yourself.\n")
	b.WriteString("- Use only tool names listed here and only the parameters in each tool's input schema.\n")
	b.WriteString("- The list below is the complete set of tools you have. Do not use shell commands, files, code execution, web search or any other capability of the runtime you are running in.\n")
	b.WriteString("- When no (further) tool call is needed, answer the customer in plain text with no <tool_call> block.\n\n")
	b.WriteString("Available tools:\n\n")
	var brief []agyToolCatalog
	for _, t := range tools {
		if compact && !detailed[t.Name] {
			brief = append(brief, t)
			continue
		}
		b.WriteString("#### ")
		b.WriteString(t.Name)
		b.WriteString("\n")
		if d := strings.TrimSpace(t.Description); d != "" {
			b.WriteString(d)
			b.WriteString("\n")
		}
		if t.Schema != nil {
			if raw, err := json.Marshal(t.Schema); err == nil && len(raw) > 2 {
				b.WriteString("Input schema: ")
				b.Write(raw)
				b.WriteString("\n")
			}
		}
		b.WriteString("\n")
	}
	if len(brief) > 0 {
		b.WriteString("The tools below are listed with their parameter names only (a * marks a required one). They are callable exactly like the ones above; call get_tool_instructions with the tool's name first to get its full instructions and parameter descriptions.\n\n")
		for _, t := range brief {
			b.WriteString("- ")
			b.WriteString(t.Name)
			if d := firstSentence(t.Description, 80); d != "" {
				b.WriteString(" — ")
				b.WriteString(d)
			}
			if names := agySchemaParamNames(t.Schema); len(names) > 0 {
				b.WriteString(" [params: ")
				b.WriteString(strings.Join(names, ", "))
				b.WriteString("]")
			}
			b.WriteString("\n")
		}
	}
	return strings.TrimSpace(b.String())
}

// agyTurnLabel renders transcript labels in the second person: the assistant's
// own past messages and tool calls are "You …", the way a native chat history
// presents a model's previous turns to it, rather than third-person narration.
func agyTurnLabel(tt agyTurn) string {
	switch tt.Kind {
	case agyTurnCustomer:
		return "[Customer]"
	case agyTurnAssistant:
		return "[You (assistant)]"
	case agyTurnSystemNote:
		return "[System note to you]"
	case agyTurnToolCall:
		if tt.CallID != "" {
			return "[You called tool " + tt.Tool + " (" + tt.CallID + ")]"
		}
		return "[You called tool " + tt.Tool + "]"
	case agyTurnToolResult:
		name := tt.Tool
		if name == "" {
			name = "tool"
		}
		if tt.CallID != "" {
			return "[Result of your " + name + " call (" + tt.CallID + ")]"
		}
		return "[Result of your " + name + " call]"
	}
	return "[Message]"
}

// agyToolResultCap returns the per-result character cap applied to tool
// results from EARLIER turns (0 = unlimited). Results of the current turn are
// never truncated. Configured via PROXY_AGY_TOOL_RESULT_CAP.
func agyToolResultCap(cfg config) int {
	if cfg.AgyToolResultCap < 0 {
		return 0
	}
	return cfg.AgyToolResultCap
}

func truncateToolResult(s string, capChars int) string {
	if capChars <= 0 || len(s) <= capChars {
		return s
	}
	cut := capChars
	for cut > 0 && cut < len(s) && (s[cut]&0xC0) == 0x80 { // don't split a UTF-8 rune
		cut--
	}
	return s[:cut] + fmt.Sprintf(" …[truncated by the system: %d more characters not shown]", len(s)-cut)
}

// renderJSONReadable re-renders a JSON tool result so that every object member
// sits on its own line, indented one space per nesting level, while arrays of
// scalars stay on one line. Key order and every value are preserved (numbers
// are copied verbatim, strings re-quoted without HTML escaping); anything that
// is not exactly one JSON object or array is returned unchanged.
//
// Why: a model that has no code interpreter must read a minified 30KB payload
// holding two dozen records and copy the right record's id into its next
// call. Without tools, gemini-3.8-flash picked the id of the first record or
// of the enclosing membership; with the coding tools it used to answer this by
// running python over the payload (slow, expensive, unsafe). Laid out one
// member per line, a record's id and its name are adjacent lines, which is
// what a person would want too. Whitespace costs almost nothing in tokens.
// Configured via PROXY_AGY_READABLE_RESULTS; off by default because two full
// B1 replays with it on still booked on the wrong package id (the model reads
// the protocol's "record the visit on her membership" literally and takes the
// membership record's id rather than the Full Body package's), so the layout
// was not the limiting factor.
func renderJSONReadable(s string) string {
	trimmed := strings.TrimSpace(s)
	if len(trimmed) < 2 || (trimmed[0] != '{' && trimmed[0] != '[') {
		return s
	}
	dec := json.NewDecoder(strings.NewReader(trimmed))
	dec.UseNumber()
	var b strings.Builder
	if err := writeReadableJSONValue(dec, &b, 0); err != nil {
		return s
	}
	if _, err := dec.Token(); err != io.EOF { // trailing content: not a single document
		return s
	}
	return b.String()
}

func writeReadableJSONValue(dec *json.Decoder, b *strings.Builder, depth int) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	pad := strings.Repeat(" ", depth)
	switch v := tok.(type) {
	case json.Delim:
		switch v {
		case '{':
			n := 0
			for dec.More() {
				keyTok, err := dec.Token()
				if err != nil {
					return err
				}
				key, _ := keyTok.(string)
				if n == 0 {
					b.WriteString("{\n")
				}
				b.WriteString(pad)
				b.WriteString(" ")
				b.WriteString(jsonQuote(key))
				b.WriteString(": ")
				if err := writeReadableJSONValue(dec, b, depth+1); err != nil {
					return err
				}
				b.WriteString("\n")
				n++
			}
			if _, err := dec.Token(); err != nil { // '}'
				return err
			}
			if n == 0 {
				b.WriteString("{}")
			} else {
				b.WriteString(pad)
				b.WriteString("}")
			}
		case '[':
			var items []string
			scalar := true
			for dec.More() {
				var ib strings.Builder
				if err := writeReadableJSONValue(dec, &ib, depth+1); err != nil {
					return err
				}
				item := ib.String()
				if (strings.HasPrefix(item, "{") && item != "{}") || (strings.HasPrefix(item, "[") && item != "[]") {
					scalar = false
				}
				items = append(items, item)
			}
			if _, err := dec.Token(); err != nil { // ']'
				return err
			}
			switch {
			case len(items) == 0:
				b.WriteString("[]")
			case scalar:
				b.WriteString("[" + strings.Join(items, ", ") + "]")
			default:
				b.WriteString("[\n")
				for _, item := range items {
					b.WriteString(pad)
					b.WriteString(" ")
					b.WriteString(item)
					b.WriteString("\n")
				}
				b.WriteString(pad)
				b.WriteString("]")
			}
		default:
			return fmt.Errorf("unexpected delimiter %q", v)
		}
	case string:
		b.WriteString(jsonQuote(v))
	case json.Number:
		b.WriteString(v.String())
	case bool:
		if v {
			b.WriteString("true")
		} else {
			b.WriteString("false")
		}
	case nil:
		b.WriteString("null")
	default:
		return fmt.Errorf("unexpected token %T", tok)
	}
	return nil
}

// jsonQuote quotes s as a JSON string without escaping <, > and &.
func jsonQuote(s string) string {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(s); err != nil {
		return strconv.Quote(s)
	}
	return strings.TrimRight(b.String(), "\n")
}

// ---------------------------------------------------------------------------
// Fitting the prompt into agy's input window
// ---------------------------------------------------------------------------
//
// agy silently truncates the user message it forwards to the model and marks
// the cut with "<truncated N bytes>". Measured on ai-api1 2026-09-09 with the
// tool-less agent: a 392,989-byte booking prompt reached the model as ~191,577
// bytes, i.e. everything after the persona and the tool catalog was gone —
// packages, slots, the dialogue digest and the whole YOUR NEXT TURN block. The
// model then answered from fragments: it booked the membership record instead
// of the Full Body package, invented availability, and copied a therapist id
// out of an old reservation. Asked to explain itself it said so plainly ("the
// JSON output of get_customer_packages was cut off at <truncated 202410
// bytes>"). It is also why the coding agent used to be MORE accurate: it read
// the full transcript from disk with python instead of from its context.
//
// So the proxy must do the cutting itself, deliberately, keeping what the turn
// needs. Everything below is mechanical: sizes and nesting only, no knowledge
// of what any tool means.

// agyPromptBudget is the byte budget for the rendered prompt
// (PROXY_AGY_PROMPT_BUDGET, 0 disables the fitting entirely).
func agyPromptBudget(cfg config) int {
	if cfg.AgyPromptBudget < 0 {
		return 0
	}
	if cfg.AgyPromptBudget == 0 {
		return defaultAgyPromptBudget
	}
	return cfg.AgyPromptBudget
}

// defaultAgyPromptBudget sits under agy's measured ceiling. Bisected on
// ai-api1 2026-09-09 with a marker on the final line: 186,000 and 190,000 bytes
// come back, 194,000 does not — matching the 191,577 bytes kept from the
// 392,989-byte prompt above.
const defaultAgyPromptBudget = 186000

// agyMinResultBytes is the floor a single tool result is never compacted below
// while it is still being shrunk in place. Deliberately small: a booking turn
// carries a dozen instruction results whose content the TOOLS section already
// states, and at a 1,200-byte floor those alone locked up ~10KB of the ~30KB
// the conversation gets — enough to cost the packages list its records
// (prod 2026-09-09 conv 9f24dc22). A result cut to this size keeps its opening
// and carries the note saying it is incomplete and may be fetched again.
const agyMinResultBytes = 400

// agyMinTranscriptBytes is the room the transcript must get before the tool
// catalog is compacted to make space.
const agyMinTranscriptBytes = 30000

// agyMinTranscriptFloor is the smallest transcript room worth aiming for when
// the caller's system prompt has already eaten the budget.
const agyMinTranscriptFloor = 4000

// agyMinStringBytes is the floor a single JSON string is not shortened below.
const agyMinStringBytes = 400

// marshalJSONNoHTML encodes v without escaping <, > and &, which would inflate
// Arabic-free payloads and change quoted text.
func marshalJSONNoHTML(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// compactJSONForPrompt shrinks a JSON tool result toward maxBytes by pruning
// its heaviest arrays instead of cutting the text mid-value. Deeper arrays go
// first, so a list of records loses the bulky history nested inside each record
// (which the turn rarely needs) before it loses any record (which carries the
// ids and names the turn is about). Every prune leaves a marker saying how many
// entries were dropped. Returns the original string when it cannot help.
// Compaction levels, applied across the whole transcript in order, so the
// cheapest information is always spent first:
//
//	agyLevelHistory — thin the arrays nested inside records (a package's past
//	                  reservations, a therapist's shifts). Records and prose intact.
//	agyLevelProse   — additionally shorten long strings (instruction sheets).
//	agyLevelRecords — additionally drop records. Last resort.
const (
	agyLevelSuperseded = iota // older results of a tool that has been called again since
	agyLevelHistory
	agyLevelProse
	agyLevelFields  // drop the heaviest column of a record list, keeping every record
	agyLevelOldest  // drop the oldest tool results outright, oldest first
	agyLevelRecords // drop records from a list. Last resort of all.
)

// agyDroppedResultNote replaces a tool result that had to go entirely. The call
// that produced it stays in the transcript, so the model still knows the tool
// ran and with which arguments, and can call it again if it needs the data.
const agyDroppedResultNote = "[system note: this result is not shown any more — it was dropped to fit the context window. The call above did run; call the tool again if you need its data.]"

// agyRecallNote is appended to a result the system had to shorten, so the model
// is told in the same place it reads the data that the data is incomplete and
// that calling again is allowed. Without it the state block's "you already ran
// this, do not run it again" would point at a result that no longer holds what
// it claims.
const agyRecallNote = "\n[system note: this result was shortened by the system to fit the context window, so it is NOT the complete output. If you need what is missing, call the tool again — that is not a repeat call.]"

// agyKeepRecentPerTool is how many results of the same tool are treated as
// live. One: a tool describes some resource, and its newest result is the
// current state of that resource — every older result of the same tool is a
// stale copy of the same thing and is the cheapest byte in the transcript.
// Production 2026-09-09 (conv ed674543) carried five copies of a 117KB protocol
// and six of a 28KB package list in one booking turn.
const agyKeepRecentPerTool = 1

func compactJSONForPrompt(raw string, maxBytes, level int) (string, bool) {
	trimmed := strings.TrimSpace(raw)
	if maxBytes <= 0 || len(raw) <= maxBytes || len(trimmed) < 2 || (trimmed[0] != '{' && trimmed[0] != '[') {
		return raw, false
	}
	dec := json.NewDecoder(strings.NewReader(trimmed))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return raw, false
	}
	if _, err := dec.Token(); err != io.EOF {
		return raw, false
	}
	best := raw
	changed := false
	for i := 0; i < 500; i++ {
		out, err := marshalJSONNoHTML(v)
		if err != nil {
			break
		}
		if changed && len(out) < len(best) {
			best = string(out)
		}
		if len(out) <= maxBytes {
			break
		}
		if !shrinkJSONOnce(&v, level) {
			break
		}
		changed = true
	}
	if !changed || len(best) >= len(raw) {
		return raw, false
	}
	return best, true
}

// jsonRef locates one shrinkable node inside a decoded JSON document.
type jsonRef struct {
	isString bool
	depth    int
	size     int
	setArr   func([]any)
	setStr   func(string)
	arr      []any
	str      string
}

// agyLongStringBytes is the length above which a JSON string is treated as
// prose to be shortened rather than structure to be preserved.
const agyLongStringBytes = 1000

// pruneHeaviestArray takes one bite out of the document and reports whether it
// changed anything. Long strings go first: an instruction sheet delivered as
// one 117KB string degrades gracefully when shortened, whereas dropping an
// element from a list of records destroys a whole entity — and it is exactly
// those records (a package, a slot, a therapist) that the next tool call has to
// name. Production 2026-09-09: with records dropped first, the model reported
// "23 of the 24 package entries were truncated" and booked the only membership
// record still visible.
func shrinkJSONOnce(root *any, level int) bool {
	var refs []jsonRef
	var walk func(node any, depth int, setArr func([]any), setStr func(string))
	walk = func(node any, depth int, setArr func([]any), setStr func(string)) {
		switch n := node.(type) {
		case []any:
			if setArr != nil && len(n) > 0 {
				if raw, err := marshalJSONNoHTML(n); err == nil {
					refs = append(refs, jsonRef{depth: depth, size: len(raw), setArr: setArr, arr: n})
				}
			}
			for i := range n {
				i := i
				walk(n[i], depth+1, func(v []any) { n[i] = v }, func(s string) { n[i] = s })
			}
		case map[string]any:
			for k := range n {
				k := k
				walk(n[k], depth+1, func(v []any) { n[k] = v }, func(s string) { n[k] = s })
			}
		case string:
			if setStr != nil && len(n) > agyLongStringBytes {
				refs = append(refs, jsonRef{isString: true, depth: depth, size: len(n), setStr: setStr, str: n})
			}
		}
	}
	walk(*root, 0, func(v []any) { *root = v }, nil)
	if len(refs) == 0 {
		return false
	}
	// Prose before structure: halve the longest long string first.
	var longest *jsonRef
	for i := range refs {
		if !refs[i].isString || level < agyLevelProse {
			continue
		}
		if longest == nil || refs[i].size > longest.size {
			longest = &refs[i]
		}
	}
	if longest != nil {
		keep := longest.size / 2
		if keep < agyMinStringBytes {
			keep = agyMinStringBytes
		}
		if keep < longest.size {
			for keep > 0 && keep < len(longest.str) && (longest.str[keep]&0xC0) == 0x80 {
				keep-- // never split a UTF-8 rune
			}
			longest.setStr(fmt.Sprintf("%s… [system note: %d characters dropped by the system to fit the context window]", longest.str[:keep], longest.size-keep))
			return true
		}
	}
	// Before any record is dropped, take the heaviest column off the record
	// list: 25 packages each keeping their id and name are worth far more to
	// the next tool call than 12 packages keeping every field.
	if level >= agyLevelFields {
		if dropHeaviestColumn(refs) {
			return true
		}
	}

	// No prose left to shorten: fall back to thinning arrays. Below the record
	// level the shallowest array is the record list itself and is left alone,
	// so only the history nested inside records is thinned.
	minDepth := -1
	for _, r := range refs {
		if r.isString {
			continue
		}
		if minDepth < 0 || r.depth < minDepth {
			minDepth = r.depth
		}
	}
	var arrays []jsonRef
	for _, r := range refs {
		if r.isString {
			continue
		}
		if level < agyLevelRecords && r.depth == minDepth {
			continue
		}
		arrays = append(arrays, r)
	}
	refs = arrays
	if len(refs) == 0 {
		return false
	}
	// Deepest first, then heaviest: nested history goes before top-level records.
	sort.SliceStable(refs, func(i, j int) bool {
		if refs[i].depth != refs[j].depth {
			return refs[i].depth > refs[j].depth
		}
		return refs[i].size > refs[j].size
	})
	for _, ref := range refs {
		// Fold any marker this array already carries, so repeated passes keep
		// making progress instead of rewriting the same two elements forever.
		items := ref.arr
		alreadyDropped := 0
		if n := len(items); n > 0 {
			if s, ok := items[n-1].(string); ok && strings.HasPrefix(s, agyOmissionMarker) {
				if m := agyOmissionCountRe.FindStringSubmatch(s); m != nil {
					alreadyDropped, _ = strconv.Atoi(m[1])
				}
				items = items[:n-1]
			}
		}
		if len(items) == 0 {
			continue // nothing but a marker left here
		}
		keep := len(items) / 2
		next := make([]any, 0, keep+1)
		next = append(next, items[:keep]...)
		next = append(next, fmt.Sprintf("%s%d entries dropped by the system to fit the context window]", agyOmissionMarker, alreadyDropped+len(items)-keep))
		ref.setArr(next)
		return true
	}
	return false
}

const agyOmissionMarker = "[system note: "

// agyMinRecordKeys is the number of fields a record keeps whatever happens, so
// that thinning never leaves an unidentifiable object. Two is deliberate: an id
// and a name are what the next tool call needs, and everything else must be
// spendable before a record is dropped. Production 2026-09-09 (conv 9f24dc22):
// with this at 4, the packages payload could not shrink past ~3,600 bytes
// without dropping records, the turn had ~1,000 bytes to give it, so 24 of the
// 25 records went and the model booked on the only survivor — the membership
// record — and every create_reservation failed with "the selected therapist
// does not provide this service".
const agyMinRecordKeys = 2

// dropHeaviestColumn removes, from the heaviest array of objects, the single
// field that costs the most across its elements, and notes it. Ids and short
// names are the cheapest fields, so they are the last to go — which is what
// the model needs to name a record in its next tool call.
func dropHeaviestColumn(refs []jsonRef) bool {
	best, bestSize := -1, 0
	for i, r := range refs {
		if r.isString || len(r.arr) < 2 {
			continue
		}
		objects := 0
		for _, item := range r.arr {
			if _, ok := item.(map[string]any); ok {
				objects++
			}
		}
		if objects < 2 {
			continue
		}
		if r.size > bestSize {
			best, bestSize = i, r.size
		}
	}
	if best < 0 {
		return false
	}
	weight := map[string]int{}
	distinct := map[string]map[string]bool{}
	scalarCol := map[string]bool{}
	keys := 0
	for _, item := range refs[best].arr {
		obj, ok := item.(map[string]any)
		if !ok {
			continue
		}
		n := len(obj)
		if _, marked := obj[agyDroppedFieldsKey]; marked {
			n--
		}
		if n > keys {
			keys = n
		}
		for k, v := range obj {
			if k == agyDroppedFieldsKey {
				continue // never drop the note about what was dropped
			}
			raw, err := marshalJSONNoHTML(v)
			if err != nil {
				continue
			}
			weight[k] += len(raw)
			if distinct[k] == nil {
				distinct[k] = map[string]bool{}
				scalarCol[k] = true
			}
			distinct[k][string(raw)] = true
			switch v.(type) {
			case map[string]any, []any:
				scalarCol[k] = false
			}
		}
	}
	if keys <= agyMinRecordKeys || len(weight) == 0 {
		return false
	}
	// Which column to spend is decided by how much it distinguishes one record
	// from another, not by what it is called. A column holding the same value in
	// every record separates nothing and is free to drop; a column holding a
	// different value in every record is what the next call will use to name
	// the record it wants. Between two columns that distinguish equally, the
	// bulkier one goes first.
	//
	// This replaces a field-name heuristic (id/uuid/code/name/title) that only
	// worked for APIs that happen to use those English words, and that ranked a
	// 25-byte name above a 6-byte id, leaving records present but unnameable.
	// Counting distinct values needs no vocabulary and holds for any payload.
	type colStat struct {
		name     string
		weight   int
		distinct int
		scalar   bool
	}
	stats := make([]colStat, 0, len(weight))
	for k, w := range weight {
		stats = append(stats, colStat{name: k, weight: w, distinct: len(distinct[k]), scalar: scalarCol[k]})
	}
	sort.Slice(stats, func(i, j int) bool {
		a, b := stats[i], stats[j]
		if a.scalar != b.scalar {
			return !a.scalar // structures (nested objects/arrays) go before plain values
		}
		if a.distinct != b.distinct {
			return a.distinct < b.distinct // least distinguishing first
		}
		if a.weight != b.weight {
			return a.weight > b.weight // among equals, the bulkier one
		}
		return a.name < b.name
	})
	heaviest := stats[0].name
	for _, item := range refs[best].arr {
		if obj, ok := item.(map[string]any); ok {
			delete(obj, heaviest)
		}
	}
	if obj, ok := refs[best].arr[0].(map[string]any); ok {
		note, _ := obj[agyDroppedFieldsKey].(string)
		if note != "" {
			note += ", "
		}
		obj[agyDroppedFieldsKey] = note + heaviest
	}
	return true
}

// agyDroppedFieldsKey names the field that records which columns were removed.
const agyDroppedFieldsKey = "_fields_dropped_by_the_system"

var agyOmissionCountRe = regexp.MustCompile(`^\[system note: (\d+) entries dropped`)

// agyProseBytes reports the length of the longest long string inside a JSON
// tool result, i.e. how many bytes it can still give up without losing a
// record. Non-JSON text counts as prose in full.
func agyProseBytes(raw string) int {
	trimmed := strings.TrimSpace(raw)
	if len(trimmed) < 2 || (trimmed[0] != '{' && trimmed[0] != '[') {
		if len(raw) > agyLongStringBytes {
			return len(raw)
		}
		return 0
	}
	dec := json.NewDecoder(strings.NewReader(trimmed))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return 0
	}
	longest := 0
	var walk func(any)
	walk = func(node any) {
		switch n := node.(type) {
		case string:
			if len(n) > longest {
				longest = len(n)
			}
		case []any:
			for _, x := range n {
				walk(x)
			}
		case map[string]any:
			for _, x := range n {
				walk(x)
			}
		}
	}
	walk(v)
	if longest > agyLongStringBytes {
		return longest
	}
	return 0
}

// fitAgyTranscript compacts tool results until the rendered transcript fits
// budget, largest first and earlier turns before the current one, so the data
// the model is about to act on is the last thing to lose detail. Returns the
// adjusted transcript and one line per compacted result for the log.
func fitAgyTranscript(t agyTranscript, oldResultCap int, readable bool, budget int) (agyTranscript, []string) {
	if budget <= 0 || len(t.Turns) == 0 {
		return t, nil
	}
	out := t
	out.Turns = append([]agyTurn(nil), t.Turns...)
	last := out.lastCustomerIndex()
	// Which results are superseded: all but the most recent few per tool.
	superseded := map[int]bool{}
	seenPerTool := map[string]int{}
	for i := len(out.Turns) - 1; i >= 0; i-- {
		tt := out.Turns[i]
		if tt.Kind != agyTurnToolResult || tt.Tool == "" {
			continue
		}
		seenPerTool[tt.Tool]++
		if seenPerTool[tt.Tool] > agyKeepRecentPerTool {
			superseded[i] = true
		}
	}
	shrunk := map[int][2]int{} // turn index → {original, current} bytes
	// Three phases, and the order is the whole design. What gets spent first is
	// decided by how reconstructible it is, which needs no knowledge of what any
	// tool returns:
	//
	//	0. stale copies — an older result of a tool that has been called again
	//	   since. The newer call already replaced it, so it costs nothing to lose.
	//	1. the live result of a tool the CURRENT turn has not consulted. It can
	//	   be fetched again if the model needs it.
	//	2. the results the current turn just fetched — the state it is acting on,
	//	   which nothing else can reconstruct. Last, and usually untouched.
	//
	// Keeping one live copy per tool also stops a feedback loop: reference away
	// what the previous turn fetched and the model simply fetches it again next
	// turn, which is how one booking ended up carrying five copies of a 117KB
	// protocol (prod conv ed674543).
	for phase := 0; phase < 3; phase++ {
		for level := agyLevelSuperseded; level <= agyLevelOldest; level++ {
			exhausted := map[int]bool{}
			inPhase := func(j int) bool {
				switch phase {
				case 0:
					return superseded[j] // a stale copy of a tool called again since
				case 1:
					return !superseded[j] && j <= last // live, but not from this turn
				default:
					return !superseded[j] && j > last // the state being acted on
				}
			}
			for i := 0; i < 300; i++ {
				size := len(renderAgyTranscript(out, oldResultCap, readable))
				if size <= budget {
					level, phase = agyLevelRecords+1, 3 // done
					break
				}
				over := size - budget
				// Last resort in a long conversation: rather than let agy cut the
				// newest messages off the end, drop whole results starting with the
				// oldest. The dialogue itself is never touched — a customer's own
				// words are the one thing the turn cannot be rebuilt without.
				if level == agyLevelOldest {
					dropped := -1
					for j, tt := range out.Turns {
						if tt.Kind == agyTurnToolResult && inPhase(j) && len(tt.Text) > len(agyDroppedResultNote) {
							dropped = j
							break
						}
					}
					if dropped < 0 {
						break
					}
					if _, seen := shrunk[dropped]; !seen {
						shrunk[dropped] = [2]int{len(out.Turns[dropped].Text), len(agyDroppedResultNote)}
					} else {
						shrunk[dropped] = [2]int{shrunk[dropped][0], len(agyDroppedResultNote)}
					}
					out.Turns[dropped].Text = agyDroppedResultNote
					out.Turns[dropped].Reduced = true
					continue
				}
				idx, bestSize := -1, 0
				for j, tt := range out.Turns {
					if tt.Kind != agyTurnToolResult || exhausted[j] || len(tt.Text) <= agyMinResultBytes {
						continue
					}
					if !inPhase(j) {
						continue
					}
					if level == agyLevelSuperseded && !superseded[j] {
						continue
					}
					if len(tt.Text) > bestSize {
						bestSize, idx = len(tt.Text), j
					}
				}
				if idx < 0 {
					break // nothing left at this level in this phase
				}
				target := bestSize - over
				if target < bestSize/2 {
					target = bestSize / 2
				}
				if target < agyMinResultBytes {
					target = agyMinResultBytes
				}
				if level == agyLevelSuperseded {
					// Stale by definition: cut straight to the floor, whatever it holds.
					text := truncateToolResult(out.Turns[idx].Text, agyMinResultBytes)
					if len(text) >= len(out.Turns[idx].Text) {
						exhausted[idx] = true
						continue
					}
					if _, seen := shrunk[idx]; !seen {
						shrunk[idx] = [2]int{len(out.Turns[idx].Text), len(text)}
					} else {
						shrunk[idx] = [2]int{shrunk[idx][0], len(text)}
					}
					out.Turns[idx].Text = text
					out.Turns[idx].Reduced = true
					continue
				}
				text, ok := compactJSONForPrompt(out.Turns[idx].Text, target, level)
				if (!ok || len(text) >= len(out.Turns[idx].Text)) && level == agyLevelRecords {
					// Not JSON, or JSON that cannot give up anything else.
					text = truncateToolResult(out.Turns[idx].Text, target)
				}
				if len(text) >= len(out.Turns[idx].Text) {
					exhausted[idx] = true
					continue
				}
				if _, seen := shrunk[idx]; !seen {
					shrunk[idx] = [2]int{len(out.Turns[idx].Text), len(text)}
				} else {
					shrunk[idx] = [2]int{shrunk[idx][0], len(text)}
				}
				out.Turns[idx].Text = text
				out.Turns[idx].Reduced = true
			}
		}
	}
	var notes []string
	for idx, sizes := range shrunk {
		notes = append(notes, fmt.Sprintf("%s %d→%d", firstNonEmpty(out.Turns[idx].Tool, "result"), sizes[0], sizes[1]))
	}
	sort.Strings(notes)
	return out, notes
}

// renderAgyTranscript renders the dialogue section. When readable is set, JSON
// tool results are laid out one member per line (renderJSONReadable).
func renderAgyTranscript(t agyTranscript, oldResultCap int, readable bool) string {
	if len(t.Turns) == 0 {
		return ""
	}
	last := t.lastCustomerIndex()
	var b strings.Builder
	b.WriteString("### CONVERSATION SO FAR\n\n")
	b.WriteString("Transcript between the customer and you (the assistant), oldest first. \"You called tool\" entries are tool calls you made earlier, and \"Result of your … call\" entries are the complete original outputs of those calls (not summaries). \"System note\" entries come from the system, not from the customer, and the customer cannot see them.\n\n")
	// A result byte-identical to an earlier result of the same tool is shown
	// once; repeats are replaced by a pointer. Lossless, and it stops a
	// repeated 200KB payload from inflating the prompt on every iteration.
	seenResult := map[string]string{} // tool + "\x00" + text → call id of first occurrence
	for i, tt := range t.Turns {
		text := tt.Text
		switch tt.Kind {
		case agyTurnToolCall:
			text = tt.Args
		case agyTurnToolResult:
			key := tt.Tool + "\x00" + tt.Text
			if firstID, dup := seenResult[key]; dup && len(tt.Text) > 200 {
				ref := "your earlier " + tt.Tool + " call"
				if firstID != "" {
					ref += " (" + firstID + ")"
				}
				text = "(identical to the result of " + ref + " above — not repeated)"
			} else {
				if !dup {
					seenResult[key] = tt.CallID
				}
				if readable {
					text = renderJSONReadable(text)
				}
				if i < last {
					text = truncateToolResult(text, oldResultCap)
				}
				if tt.Reduced && tt.Text != agyDroppedResultNote {
					text += agyRecallNote
				}
			}
		}
		b.WriteString(agyTurnLabel(tt))
		b.WriteString(": ")
		b.WriteString(strings.TrimSpace(text))
		b.WriteString("\n\n")
	}
	return strings.TrimSpace(b.String())
}

// renderAgyDialogueDigest renders only the customer-visible exchange (customer
// and assistant text, no tool activity) so the conversational thread stays
// legible next to the instruction even when the full transcript is dominated
// by large tool payloads. It is a projection of the transcript, not a rewrite.
func renderAgyDialogueDigest(t agyTranscript) string {
	const perMessage = 400
	var lines []string
	for _, tt := range t.Turns {
		switch tt.Kind {
		case agyTurnCustomer:
			lines = append(lines, "Customer: "+truncateString(strings.Join(strings.Fields(stripCustomerHeader(tt.Text)), " "), perMessage))
		case agyTurnAssistant:
			lines = append(lines, "You: "+truncateString(strings.Join(strings.Fields(tt.Text), " "), perMessage))
		}
	}
	if len(lines) < 2 {
		return ""
	}
	var b strings.Builder
	b.WriteString("### DIALOGUE DIGEST\n\n")
	b.WriteString("Only the messages exchanged between you and the customer, oldest first (the full transcript with your tool calls and their results is above):\n\n")
	for _, l := range lines {
		b.WriteString(l)
		b.WriteString("\n")
	}
	return strings.TrimSpace(b.String())
}

// renderAgyStatePreface renders a compact "where we are" block placed BEFORE
// the system instructions. A causal reader applies rules in the order it
// meets them: when the persona says "on a booking request, open protocol X"
// the model must already know that X was executed earlier in this very
// conversation, otherwise it restarts the flow. Same facts as YOUR NEXT TURN,
// stated first.
func renderAgyStatePreface(t agyTranscript, hasTools bool) string {
	if len(t.Turns) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("### CURRENT STATE (read before the instructions)\n\n")
	b.WriteString("This is an ongoing conversation, not its start. ")
	if hasTools {
		if done := t.executedToolSummary(); len(done) > 0 {
			b.WriteString("You have ALREADY executed these tools earlier in this conversation; their outputs are in the CONVERSATION SO FAR section and any instructions or protocols they returned are already loaded and in effect, so do not open, preflight or run them again just to re-read what is still shown there. Where a line below says its result is INCOMPLETE, the opposite applies: the system shortened that output to fit this message, and you should call the tool again when you need what it no longer shows.\n")
			for _, d := range done {
				b.WriteString("  • ")
				b.WriteString(d)
				b.WriteString("\n")
			}
		}
		if pre := t.preflightedTools(); len(pre) > 0 {
			b.WriteString("Preflighted (get_tool_instructions already done, do not repeat): ")
			b.WriteString(strings.Join(pre, ", "))
			b.WriteString(".\n")
		}
		if calls := t.currentTurnCallOutcomes(); len(calls) > 0 {
			b.WriteString("In the CURRENT turn (since the customer's latest message) you have already called, with their results in the transcript: ")
			parts := make([]string, 0, len(calls))
			failed := 0
			for _, c := range calls {
				line := c.Call.Tool + " " + truncateString(c.Call.Args, 120)
				if c.Failure != "" {
					line += " → FAILED (" + c.Failure + ")"
					failed++
				}
				parts = append(parts, line)
			}
			b.WriteString(strings.Join(parts, "; "))
			b.WriteString(". Do not call any of these again this turn — rewording an argument does not make it a new call; call a tool again only for genuinely different data.")
			if failed > 0 {
				b.WriteString(" A call marked FAILED returned an error instead of data, so you do NOT have that data: correct the arguments and call the tool again, or tell the customer you could not retrieve it — never answer as if the call had succeeded.")
			}
			b.WriteString("\n")
		}
	}
	if prev := t.previousAssistantText(); prev != "" {
		b.WriteString("Your previous message to the customer: \"")
		b.WriteString(truncateString(strings.Join(strings.Fields(prev), " "), 400))
		b.WriteString("\"\n")
	}
	if li := t.lastCustomerIndex(); li >= 0 {
		b.WriteString("The customer's reply you are answering now: \"")
		b.WriteString(truncateString(strings.Join(strings.Fields(stripCustomerHeader(t.Turns[li].Text)), " "), 400))
		b.WriteString("\"\n")
	}
	return strings.TrimSpace(b.String())
}

// renderAgyNextTurn renders the state summary + the neutral instruction.
func renderAgyNextTurn(t agyTranscript, hasTools bool) string {
	var b strings.Builder
	b.WriteString("### YOUR NEXT TURN\n\n")
	b.WriteString("Facts about the current state (derived by the system from the transcript above):\n")
	b.WriteString("- Everything the customer has said in this conversation is in the transcript and the dialogue digest; treat details already given as known and do not ask for them again.\n")
	if hasTools {
		if pre := t.preflightedTools(); len(pre) > 0 {
			b.WriteString("- Tools whose instructions have already been retrieved with get_tool_instructions in this conversation (the preflight cache is valid for the whole conversation, so do NOT fetch them again): ")
			b.WriteString(strings.Join(pre, ", "))
			b.WriteString(". Any other tool still needs get_tool_instructions before its first use.\n")
		} else {
			b.WriteString("- No tool has been preflighted with get_tool_instructions yet in this conversation.\n")
		}
	}
	if hasTools {
		if done := t.executedToolSummary(); len(done) > 0 {
			b.WriteString("- Tools already executed earlier in this conversation, listed below. Their outputs are in the transcript above (\"Result of your … call\" entries are the tool's own output, never your summary of it). If your system instructions mention cached data or summaries of recent tool results, they refer to exactly these calls. Instructions or protocols that a tool returned are already loaded and remain in effect for the whole conversation. Do NOT call a tool again, and do NOT call get_tool_instructions for it, just to re-read what is still shown in the transcript; re-run a tool when its data is live and may have changed, when you need it with different arguments, or when its result below is marked INCOMPLETE and you need the part that is missing:\n")
			for _, d := range done {
				b.WriteString("  • ")
				b.WriteString(d)
				b.WriteString("\n")
			}
		}
	}
	calls := t.currentTurnCallOutcomes()
	if len(calls) > 0 {
		b.WriteString("- Tool calls already made since the customer's latest message (their results are in the transcript; do NOT call any of them again this turn — a reworded argument is the same call; use their results). A call marked FAILED returned an error, not data — you do not have that data; fix the arguments and call again, or tell the customer, but never answer as if it had succeeded:\n")
		for _, c := range calls {
			b.WriteString("  • ")
			b.WriteString(c.Call.Tool)
			b.WriteString(" ")
			b.WriteString(c.Call.Args)
			if c.Failure != "" {
				b.WriteString(" → FAILED (")
				b.WriteString(c.Failure)
				b.WriteString(")")
			}
			b.WriteString("\n")
		}
	}
	n := len(t.Turns)
	if n > 0 {
		switch t.Turns[n-1].Kind {
		case agyTurnToolResult:
			b.WriteString("- The transcript ends with a tool result: you are in the middle of handling the customer's latest message. Continue from that result.\n")
		case agyTurnCustomer:
			b.WriteString("- The transcript ends with a new customer message that has not been answered yet.\n")
		case agyTurnSystemNote:
			b.WriteString("- The transcript ends with a system note addressed to you.\n")
		}
	}
	// The immediate exchange, verbatim, as the last thing before the
	// instruction: what you last said and what the customer answered. This is
	// the part of the dialogue the reply must fit.
	if prev := t.previousAssistantText(); prev != "" {
		b.WriteString("\nYour previous message to the customer:\n")
		b.WriteString("You: ")
		b.WriteString(strings.Join(strings.Fields(prev), " "))
		b.WriteString("\n")
	}
	if li := t.lastCustomerIndex(); li >= 0 {
		b.WriteString("\nThe customer's reply to it — the message you are answering now:\n")
		b.WriteString("Customer: ")
		b.WriteString(strings.Join(strings.Fields(stripCustomerHeader(t.Turns[li].Text)), " "))
		b.WriteString("\n")
	}
	b.WriteString("\nWrite the assistant's next turn now, following the system instructions above:\n")
	if hasTools {
		b.WriteString("- If a tool call is needed to serve the customer correctly, output ONLY the <tool_call> block(s).\n")
		b.WriteString("- Otherwise, reply to the customer in plain text: no <tool_call> blocks, no JSON, no transcript labels, no commentary about tools.\n")
	} else {
		b.WriteString("- Reply to the customer in plain text: no transcript labels, no commentary.\n")
	}
	return strings.TrimSpace(b.String())
}

// agyToolsInPlay names the tools this conversation has already touched, which
// are the ones whose full schema is worth its bytes.
func agyToolsInPlay(t agyTranscript) map[string]bool {
	inPlay := map[string]bool{"get_tool_instructions": true}
	for _, name := range t.preflightedTools() {
		inPlay[name] = true
	}
	for _, tt := range t.Turns {
		if tt.Kind == agyTurnToolCall && tt.Tool != "" {
			inPlay[tt.Tool] = true
		}
	}
	return inPlay
}

// renderAgyPromptFitted assembles the full v2 prompt, fitted to agy's input
// window, and reports which tools had their results shortened or dropped in the
// process — the caller must exempt those from its "do not repeat a call" rules.
func renderAgyPromptFitted(cfg config, system string, temp *float64, tools []agyToolCatalog, t agyTranscript) (string, map[string]bool) {
	system = strings.TrimSpace(system)
	tempDirective := buildAgyTempDirective(temp)
	toolsPrompt := renderAgyToolCatalog(tools, nil, false)

	// Bare single-turn chat with no system/tools keeps the raw prompt (Test page).
	if system == "" && toolsPrompt == "" && tempDirective == "" && len(t.Turns) == 1 && t.Turns[0].Kind == agyTurnCustomer {
		return t.Turns[0].Text, nil
	}

	var sysBlock strings.Builder
	if system != "" || tempDirective != "" {
		sysBlock.WriteString("### SYSTEM INSTRUCTIONS & POLICIES\n\n")
		if tempDirective != "" {
			sysBlock.WriteString(tempDirective)
			sysBlock.WriteString("\n\n")
		}
		if system != "" {
			sysBlock.WriteString(system)
			sysBlock.WriteString("\n\n")
		}
	}

	// agy delivers this text to the model inside its own coding-agent harness
	// (as a "user request"). Say up front what this message is, so the model
	// adopts the role defined here instead of treating the content as pasted
	// material to comment on.
	const howToRead = "### HOW TO READ THIS MESSAGE\n\n" +
		"This message is a complete, self-contained turn request for a conversational assistant. It contains, in order: the CURRENT STATE, the assistant's SYSTEM INSTRUCTIONS & POLICIES (its identity and rules), its TOOLS, the CONVERSATION SO FAR with a customer, a DIALOGUE DIGEST, and YOUR NEXT TURN. " +
		"You ARE that assistant for the duration of this reply. Do not describe, review or summarize this material, do not address anyone but the customer, and do not use any tool other than those listed in TOOLS. Produce exactly one thing: the assistant's next turn as specified at the end.\n\n"

	// Everything except the tool catalog and the transcript is fixed cost: the
	// persona is the caller's and is never trimmed. What is left of the budget
	// buys the catalog first (compacted if it does not fit) and the transcript
	// second, so the turn's own data survives instead of being cut blindly by
	// agy at whatever byte its window happens to end on. The state block is
	// rendered again after fitting, so it can say which results were shortened.
	if budget := agyPromptBudget(cfg); budget > 0 {
		fixed := len(howToRead) + len(renderAgyStatePreface(t, toolsPrompt != "")) + len(sysBlock.String()) +
			len(renderAgyDialogueDigest(t)) + len(renderAgyNextTurn(t, toolsPrompt != "")) + 16
		if toolsPrompt != "" && fixed+len(toolsPrompt)+agyMinTranscriptBytes > budget {
			if compacted := renderAgyToolCatalog(tools, agyToolsInPlay(t), true); len(compacted) < len(toolsPrompt) {
				log.Printf("[agy-prompt] tool catalog compacted %d→%d bytes to fit the input window", len(toolsPrompt), len(compacted))
				toolsPrompt = compacted
			}
		}
		room := budget - fixed - len(toolsPrompt)
		if room < agyMinTranscriptFloor {
			// The caller's own system prompt does not leave room for a
			// conversation. Compact the transcript to its floor anyway — agy
			// would otherwise cut it at an arbitrary byte — and say so.
			log.Printf("[agy-prompt] WARNING: the system prompt and tool catalog alone (%d bytes) leave %d bytes of the %d-byte budget for the conversation", fixed+len(toolsPrompt), room, budget)
			room = agyMinTranscriptFloor
		}
		// Fitting changes the blocks that were measured to size it: every
		// shortened result adds a line to the state block saying so. Measure the
		// assembled prompt and give back the overshoot until it really fits.
		var notes []string
		for attempt := 0; attempt < 4 && room > agyMinTranscriptFloor; attempt++ {
			fitted, n := fitAgyTranscript(t, agyToolResultCap(cfg), cfg.AgyReadableResults, room)
			over := len(assembleAgyPrompt(howToRead, sysBlock.String(), toolsPrompt, fitted, cfg)) - budget
			if over <= 0 || attempt == 3 {
				t, notes = fitted, n
				break
			}
			room -= over
			if room < agyMinTranscriptFloor {
				room = agyMinTranscriptFloor
			}
		}
		if len(notes) > 0 {
			log.Printf("[agy-prompt] transcript compacted to fit %d bytes: %s", room, strings.Join(notes, ", "))
		}
	}

	return assembleAgyPrompt(howToRead, sysBlock.String(), toolsPrompt, t, cfg), t.reducedTools()
}

// assembleAgyPrompt writes the sections in their fixed order. Order matters
// (verified on gemini-3.8-flash / 3.1-pro, 2026-09-08): the state preface MUST
// precede the system prompt. With it after the persona, every model restarted
// the persona's "on booking → open protocol X" flow even though X was already
// in the transcript; with it first, 8/9 trials produced the correct next step.
func assembleAgyPrompt(howToRead, sysBlock, toolsPrompt string, t agyTranscript, cfg config) string {
	var b strings.Builder
	b.WriteString(howToRead)
	if state := renderAgyStatePreface(t, toolsPrompt != ""); state != "" {
		b.WriteString(state)
		b.WriteString("\n\n")
	}
	b.WriteString(sysBlock)
	if toolsPrompt != "" {
		b.WriteString(toolsPrompt)
		b.WriteString("\n\n")
	}
	if tr := renderAgyTranscript(t, agyToolResultCap(cfg), cfg.AgyReadableResults); tr != "" {
		b.WriteString(tr)
		b.WriteString("\n\n")
	}
	if dg := renderAgyDialogueDigest(t); dg != "" {
		b.WriteString(dg)
		b.WriteString("\n\n")
	}
	b.WriteString(renderAgyNextTurn(t, toolsPrompt != ""))
	return strings.TrimSpace(b.String())
}

// renderAgyPrompt assembles the full v2 prompt.
func renderAgyPrompt(cfg config, system string, temp *float64, tools []agyToolCatalog, t agyTranscript) string {
	prompt, _ := renderAgyPromptFitted(cfg, system, temp, tools, t)
	return prompt
}

// ---------------------------------------------------------------------------
// Generation with generic loop correction
// ---------------------------------------------------------------------------

// agyGenInput is the provider-neutral description of one generation.
type agyGenInput struct {
	Model       string
	System      string
	Temperature *float64
	Tools       []agyToolCatalog
	Transcript  agyTranscript
	Media       []mediaPart
	InputTokens int
}

// agyGenTrace records what happened during one generation (for logs and the simulator).
type agyGenTrace struct {
	Prompt      string
	Attempts    []agyGenAttempt
	FinalPrompt string
}

type agyGenAttempt struct {
	Note       string
	Raw        string
	Problems   []string
	DurationMs int64
	Usage      agyResult // real agy usage for this attempt (tokens; 0 when unknown)
}

// agyReportedInputTokens prefers agy's real input count (every internal model
// call of the turn) over the prompt-size estimate.
func agyReportedInputTokens(res agyResult, prompt string) int {
	if res.InputTokens > 0 {
		return res.InputTokens
	}
	return estimateTextTokens(prompt)
}

// agyLogUsage writes one journal line per generation so quota consumption is
// visible per request (input counts every internal model call; a large gap
// between input and the prompt size means the runtime looped internally).
func agyLogUsage(res agyResult, attempt int) {
	if res.InputTokens == 0 && res.OutputTokens == 0 {
		return
	}
	log.Printf("[agy] generation attempt=%d %dms in=%d out=%d think=%d", attempt, res.DurationMs, res.InputTokens, res.OutputTokens, res.ThinkingTokens)
}

const agyMaxCorrectionRetries = 2

// agyToolCallCap is the number of calls to ONE tool within a single turn after
// which further calls with NEW arguments are pushed back with a correction
// note (PROXY_AGY_TOOL_CALL_CAP, default 6; 0 disables). Repeats with identical
// arguments or identical results are rejected outright regardless of the cap;
// this only guards against open-ended paging. It is deliberately above the
// number of distinct tools a single turn legitimately preflights (a booking
// turn fetches instructions for 4–5 tools), and a call the model insists on
// after the notes is let through (see agyGenerate). Connect's own loop cap is
// 27 iterations, far too late when each iteration appends a 200KB result.
func agyToolCallCap(cfg config) int {
	if cfg.AgyToolCallCap < 0 {
		return 0
	}
	if cfg.AgyToolCallCap == 0 {
		return 6
	}
	return cfg.AgyToolCallCap
}

// agyHarnessLeakRe matches a reply that describes the runtime the model is
// executing in — agy's coding-agent framing — instead of answering the person
// on the other end. agy wraps our prompt inside its own "user request" to a
// software assistant, and a model that follows that framing rather than ours
// replies to the developer: prod 2026-09-09 conv caf7e429 sent a customer "I am
// an AI development assistant operating within this development workspace…
// please let me know how you would like me to assist with the code, tests, or
// mock evaluations". This is about the harness, not about any business domain,
// so the guard applies to every caller and every model.
var agyHarnessLeakRe = regexp.MustCompile(`(?i)(development assistant|coding assistant|software (engineering )?assistant|ai development|development workspace|this workspace|internal prompt|system prompt|prompt or workflow|mock evaluation|as an ai (language )?model|i am an ai assistant (operating|running))`)

// agyPersonaGuard reports whether replies are checked for that leak
// (PROXY_AGY_PERSONA_GUARD, default on). It only applies when the caller gave a
// system prompt, i.e. when there is a persona to break. The config field is the
// negative so that a zero-valued config — any caller that forgets to set it,
// and every test — still has the guard on: this one protects customers.
func agyPersonaGuard(cfg config) bool { return !cfg.AgyPersonaGuardOff }

// agyResolveFn is the upstream call used by agyGenerate (overridable in tests).
var agyResolveFn = agyResolve

// agyGenerate runs the v2 prompt through agy and applies the generic
// correction loop:
//   - a tool call that repeats (same name + same arguments) a call already
//     executed in the current turn, or names an unknown tool, is a "problem";
//   - on a problem the FULL prompt is re-sent with a short correction note
//     (up to agyMaxCorrectionRetries times);
//   - if problems persist, the offending calls are dropped; if nothing usable
//     is left, one last run asks for a plain-text customer reply.
//
// Nothing here knows about bookings, slots or any business rule.
func agyGenerate(ctx context.Context, cfg config, in agyGenInput) (responsesResponse, *agyGenTrace, error) {
	// When the dialogue itself has outgrown the window, replace its oldest part
	// with a written brief and carry on from there (agy_compact.go).
	if compacted, did := agyCompactIfNeeded(ctx, cfg, in); did {
		in.Transcript = compacted
	}
	prompt, reduced := renderAgyPromptFitted(cfg, in.System, in.Temperature, in.Tools, in.Transcript)
	trace := &agyGenTrace{Prompt: prompt}
	model := firstNonEmpty(in.Model, "agy")

	known := map[string]bool{}
	for _, t := range in.Tools {
		known[t.Name] = true
	}
	stats := in.Transcript.currentTurnToolStats()
	callCap := agyToolCallCap(cfg)

	var notes []string
	var lastResp responsesResponse
	haveResp := false
	// Calls rejected ONLY by the per-tool cap (new arguments, no repeated
	// result). Unlike identical repeats these may be legitimate — Connect's
	// get_tool_instructions is called once per distinct tool, and a turn that
	// needs four tools needs four fetches — so if the model keeps insisting
	// after the correction notes, the last such draft is let through instead
	// of forcing an empty plain-text reply (prod 2026-09-08 conv 58311: the
	// 4th distinct fetch was rejected 3×, the forced reply came back empty,
	// and Connect's next iteration re-asked the customer everything).
	var capOnly []responsesOutputItem
	for attempt := 0; attempt <= agyMaxCorrectionRetries; attempt++ {
		p := prompt
		if len(notes) > 0 {
			p += "\n\n### CORRECTION FROM THE SYSTEM (not from the customer)\n\nYour previous draft was rejected:\n- " + strings.Join(notes, "\n- ") +
				"\nContinue from the tool results already in the transcript: call a DIFFERENT tool only if one is genuinely needed, otherwise reply to the customer in plain text now."
		}
		t0 := time.Now()
		res, err := agyResolveWithFormatRetry(ctx, cfg, in.Media, p, in.Model)
		if err != nil {
			return responsesResponse{}, trace, err
		}
		resp := agyToResponsesResponse(res.Response, model, agyReportedInputTokens(res, p))
		agyLogUsage(res, attempt+1)
		var problems []string
		kept := resp.Output[:0]
		capOnly = capOnly[:0]
		hardProblem := false
		for _, item := range resp.Output {
			if item.Type == "function_call" {
				if len(known) > 0 && !known[item.Name] {
					problems = append(problems, fmt.Sprintf("tool %q does not exist; only the tools listed in the TOOLS section can be called", item.Name))
					hardProblem = true
					continue
				}
				// A tool whose result the system shortened is exempt from the
				// repeat rules: the transcript no longer holds what it is being
				// told to reuse, so calling again is the correct move. Only a
				// call that already SUCCEEDED as an action is still refused,
				// since repeating that would perform it twice.
				if reduced[item.Name] && !in.Transcript.currentTurnCallSucceeded(item.Name, item.Arguments) {
					kept = append(kept, item)
					continue
				}
				if in.Transcript.calledThisTurn(item.Name, item.Arguments) {
					outcome := "its result is in the transcript"
					if in.Transcript.currentTurnCallSucceeded(item.Name, item.Arguments) {
						outcome = "it already SUCCEEDED (see its result in the transcript) and repeating it would perform the same action twice"
					}
					problems = append(problems, fmt.Sprintf("you called %s with exactly these arguments %s already in this turn; %s; do not repeat the same call", item.Name, canonicalToolArgs(item.Arguments), outcome))
					hardProblem = true
					continue
				}
				// Same tool, reworded arguments: the loop that identical-argument
				// matching misses (prod 2026-09-08: membership_protocol called 23×
				// with a different "context" string each time, 218KB result each).
				if st := stats[item.Name]; st.IdenticalResults {
					problems = append(problems, fmt.Sprintf("you already called %s %d times this turn and it returned exactly the same output each time — its output does not depend on the wording of its arguments; use the result already in the transcript and do not call it again this turn", item.Name, st.Count))
					hardProblem = true
					continue
				} else if callCap > 0 && st.Count >= callCap {
					problems = append(problems, fmt.Sprintf("you already called %s %d times this turn; do not call it again in this turn unless it is genuinely needed for different data — use the results already in the transcript", item.Name, st.Count))
					capOnly = append(capOnly, item)
					continue
				}
			}
			kept = append(kept, item)
		}
		if hardProblem {
			capOnly = capOnly[:0]
		}
		// A reply that talks about the runtime is never a customer turn.
		if len(problems) == 0 && in.System != "" && agyPersonaGuard(cfg) && !hasAnyToolCall(resp.Output) {
			if txt := agyResponseText(resp); agyHarnessLeakRe.MatchString(txt) {
				log.Printf("[agy-loop] reply described the runtime instead of answering the customer; rejecting: %s", truncateString(strings.Join(strings.Fields(txt), " "), 200))
				problems = append(problems, "your reply described the runtime you are executing in, or the message you were given, instead of answering. You ARE the assistant defined in the SYSTEM INSTRUCTIONS above, this is a real conversation with a real customer, and the customer sees exactly what you write. Reply to the customer, in their language, as that assistant — never mention prompts, workspaces, development, testing or being a development assistant")
				hardProblem = true
				kept = kept[:0]
			}
		}
		trace.Attempts = append(trace.Attempts, agyGenAttempt{Note: strings.Join(notes, " | "), Raw: res.Response, Problems: problems, DurationMs: time.Since(t0).Milliseconds(), Usage: res})
		trace.FinalPrompt = p
		if len(problems) == 0 {
			return resp, trace, nil
		}
		log.Printf("[agy-loop] attempt %d rejected: %s", attempt+1, strings.Join(problems, "; "))
		resp.Output = kept
		lastResp, haveResp = resp, true
		notes = problems
	}

	// Retries exhausted: keep whatever is usable from the last draft, unless it
	// is a reply that broke persona — that must never reach a customer.
	if haveResp && len(lastResp.Output) > 0 && (hasAnyToolCall(lastResp.Output) || agyResponseText(lastResp) != "") &&
		!agyReplyBreaksPersona(cfg, in.System, lastResp) {
		log.Printf("[agy-loop] retries exhausted; returning the last draft without the rejected calls")
		return lastResp, trace, nil
	}
	// The model insisted, with new arguments and no repeated result, on a call
	// over the per-tool cap: let it through rather than answer with nothing.
	if haveResp && len(capOnly) > 0 {
		names := make([]string, 0, len(capOnly))
		for _, item := range capOnly {
			names = append(names, item.Name)
		}
		log.Printf("[agy-loop] retries exhausted; allowing the over-cap call(s) the model insisted on (%s)", strings.Join(names, ", "))
		lastResp.Output = append([]responsesOutputItem(nil), capOnly...)
		return lastResp, trace, nil
	}

	// Last resort: ask for a plain-text reply using the results already present.
	p := prompt + "\n\n### CORRECTION FROM THE SYSTEM (not from the customer)\n\nYour previous drafts only repeated tool calls whose results are already in the transcript. Do NOT call any tool now. Using the tool results above, reply to the customer in plain text, following the system instructions."
	t0 := time.Now()
	res, err := agyResolveWithFormatRetry(ctx, cfg, in.Media, p, in.Model)
	if err != nil {
		return responsesResponse{}, trace, err
	}
	resp := agyToResponsesResponse(res.Response, model, agyReportedInputTokens(res, p))
	agyLogUsage(res, agyMaxCorrectionRetries+2)
	trace.Attempts = append(trace.Attempts, agyGenAttempt{Note: "forced plain-text reply", Raw: res.Response, DurationMs: time.Since(t0).Milliseconds(), Usage: res})
	trace.FinalPrompt = p
	if hasAnyToolCall(resp.Output) {
		kept := resp.Output[:0]
		for _, item := range resp.Output {
			if item.Type != "function_call" {
				kept = append(kept, item)
			}
		}
		resp.Output = kept
		if len(resp.Output) == 0 {
			resp.Output = []responsesOutputItem{{Type: "message", Role: "assistant", Content: []responsesOutputContent{{Type: "output_text", Text: ""}}}}
		}
	}
	if agyReplyBreaksPersona(cfg, in.System, resp) {
		// Everything has been tried and the model is still answering as the
		// runtime. Fail the request rather than send that to a customer: the
		// caller's own fallback chain will serve the turn with another model.
		log.Printf("[agy-loop] every attempt described the runtime instead of answering; failing the request so the caller can fall back")
		return responsesResponse{}, trace, fmt.Errorf("agy replied as the runtime instead of the assistant defined in the request")
	}
	log.Printf("[agy-loop] forced plain-text reply (%d chars)", len(agyResponseText(resp)))
	return resp, trace, nil
}

// agyReplyBreaksPersona reports whether a text-only reply describes the runtime
// rather than answering as the assistant the caller defined.
func agyReplyBreaksPersona(cfg config, system string, resp responsesResponse) bool {
	if system == "" || !agyPersonaGuard(cfg) || hasAnyToolCall(resp.Output) {
		return false
	}
	return agyHarnessLeakRe.MatchString(agyResponseText(resp))
}

// agyResolveWithFormatRetry wraps the upstream call: when agy itself rejects
// the model's draft ("improperly formatted function call" — the model tried
// to use the runtime's native tool channel), the turn is re-run once. That is
// a transient of the harness, not of the conversation, so it must not surface
// as a 502 to the caller.
func agyResolveWithFormatRetry(ctx context.Context, cfg config, media []mediaPart, prompt, model string) (agyResult, error) {
	res, err := agyResolveFn(ctx, cfg, media, prompt, model)
	for attempt := 1; attempt <= agyNativeCallRetries && err != nil && ctx.Err() == nil && isAgyNativeCallRejection(err); attempt++ {
		log.Printf("[agy-loop] runtime rejected a native function call attempt (%d/%d); re-running the turn with a note", attempt, agyNativeCallRetries)
		res, err = agyResolveFn(ctx, cfg, media, prompt+agyNativeCallNote, model)
	}
	return res, err
}

// agyNativeCallRetries bounds the re-runs after the runtime rejected a native
// function call. Under the tool-less agent the model has no native functions
// at all, so such an attempt is always a slip (typically at the booking step,
// where it "calls" create_reservation as a function instead of writing the
// <tool_call> block); a re-run with the note below recovers it.
const agyNativeCallRetries = 2

const agyNativeCallNote = "\n\n### SYSTEM NOTE (retry of this same turn)\nYour previous attempt at this turn was rejected by the runtime because it tried to invoke a function natively. You have no native functions here. The tools listed under TOOLS are used ONLY by writing a <tool_call> block in your reply text, exactly in the format described there; everything else in your reply is plain text for the customer. Write the turn again.\n"

func isAgyNativeCallRejection(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "improperly formatted function call") || strings.Contains(msg, "malformed function call")
}

// agyResponseText concatenates the text of message items.
func agyResponseText(resp responsesResponse) string {
	var parts []string
	for _, item := range resp.Output {
		if item.Type != "message" {
			continue
		}
		for _, c := range item.Content {
			if s := strings.TrimSpace(c.Text); s != "" {
				parts = append(parts, s)
			}
		}
	}
	return strings.Join(parts, "\n")
}
