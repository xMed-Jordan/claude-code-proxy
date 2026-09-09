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
	Kind   agyTurnKind
	Text   string // customer / assistant / system-note text, or tool-result content
	Tool   string // tool name (tool call + tool result)
	Args   string // canonical JSON arguments (tool call)
	CallID string // tool_use id (tool call + tool result)
}

// agyTranscript is the structured view of an incoming request.
type agyTranscript struct {
	Turns []agyTurn
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

func renderAgyToolCatalog(tools []agyToolCatalog) string {
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
	for _, t := range tools {
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
			b.WriteString("You have ALREADY executed these tools earlier in this conversation; their complete outputs are in the CONVERSATION SO FAR section and any instructions or protocols they returned are already loaded and in effect, so do not open, preflight or run them again just to re-read them:\n")
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
			b.WriteString("- Tools already executed earlier in this conversation, listed below. Their COMPLETE original outputs are in the transcript above (\"Result of your … call\" entries are full outputs, never summaries). If your system instructions mention cached data or summaries of recent tool results, they refer to exactly these calls, and the detail those summaries omit is already available to you in the transcript — there is nothing to call again for it. Instructions or protocols that a tool returned are already loaded and remain in effect for the whole conversation. Do NOT call a tool again, and do NOT call get_tool_instructions for it, just to re-read what is already in the transcript; re-run a tool only when its data is live and may have changed, or when you need it with different arguments:\n")
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

// renderAgyPrompt assembles the full v2 prompt.
func renderAgyPrompt(cfg config, system string, temp *float64, tools []agyToolCatalog, t agyTranscript) string {
	system = strings.TrimSpace(system)
	tempDirective := buildAgyTempDirective(temp)
	toolsPrompt := renderAgyToolCatalog(tools)

	// Bare single-turn chat with no system/tools keeps the raw prompt (Test page).
	if system == "" && toolsPrompt == "" && tempDirective == "" && len(t.Turns) == 1 && t.Turns[0].Kind == agyTurnCustomer {
		return t.Turns[0].Text
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

	var b strings.Builder
	// agy delivers this text to the model inside its own coding-agent harness
	// (as a "user request"). Say up front what this message is, so the model
	// adopts the role defined here instead of treating the content as pasted
	// material to comment on.
	b.WriteString("### HOW TO READ THIS MESSAGE\n\n")
	b.WriteString("This message is a complete, self-contained turn request for a conversational assistant. It contains, in order: the CURRENT STATE, the assistant's SYSTEM INSTRUCTIONS & POLICIES (its identity and rules), its TOOLS, the CONVERSATION SO FAR with a customer, a DIALOGUE DIGEST, and YOUR NEXT TURN. ")
	b.WriteString("You ARE that assistant for the duration of this reply. Do not describe, review or summarize this material, do not address anyone but the customer, and do not use any tool other than those listed in TOOLS. Produce exactly one thing: the assistant's next turn as specified at the end.\n\n")
	// Order matters (verified on gemini-3.8-flash / 3.1-pro, 2026-09-08): the
	// state preface MUST precede the system prompt. With it after the persona,
	// every model restarted the persona's "on booking → open protocol X" flow
	// even though X was already in the transcript; with it first, 8/9 trials
	// produced the correct next step.
	if state := renderAgyStatePreface(t, toolsPrompt != ""); state != "" {
		b.WriteString(state)
		b.WriteString("\n\n")
	}
	b.WriteString(sysBlock.String())
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
	prompt := renderAgyPrompt(cfg, in.System, in.Temperature, in.Tools, in.Transcript)
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

	// Retries exhausted: keep whatever is usable from the last draft.
	if haveResp && len(lastResp.Output) > 0 && (hasAnyToolCall(lastResp.Output) || agyResponseText(lastResp) != "") {
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
	log.Printf("[agy-loop] forced plain-text reply (%d chars)", len(agyResponseText(resp)))
	return resp, trace, nil
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
