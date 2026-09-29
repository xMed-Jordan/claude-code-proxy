package main

// agy_persona_agent_test.go — the persona-in-agent switch
// (PROXY_AGY_PERSONA_IN_AGENT, agy_persona_agent.go).
//
// TestPersonaAgentGolden pins the prompts the proxy sends TODAY, byte for byte,
// for inputs that contain no real customer data. The goldens in
// testdata/golden_prompts were generated from the code as it was BEFORE the
// switch existed and must never be regenerated: with the switch off (which is
// the default) every prompt has to stay exactly what it is now.
//
//	UPDATE_GOLDEN=1 go test -run TestPersonaAgentGolden .   # only ever to CREATE a new case

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Synthetic fixtures (deterministic; nothing here is real customer data)
// ---------------------------------------------------------------------------

// personaTestMarker is a sentence that occurs once in a synthetic persona, so a
// test can tell whether the persona reached the message.
const personaTestMarker = "PERSONA-MARKER-QX7: the assistant never quotes this sentence to a customer."

// syntheticPersona builds a persona of about size bytes of plain rules.
func syntheticPersona(size int) string {
	var b strings.Builder
	b.WriteString("You are Nadia, the booking assistant of Example Clinic (a synthetic persona for tests).\n")
	b.WriteString(personaTestMarker + "\n\n")
	for i := 0; b.Len() < size; i++ {
		fmt.Fprintf(&b, "## Section %03d\n", i)
		for j := 0; j < 6; j++ {
			fmt.Fprintf(&b, "Rule %d.%d: when the customer raises topic %d-%d, consult the tool lookup_%d first, then answer in her language, briefly and politely, and never promise anything the tool did not return.\n", i, j, i, j, (i+j)%5)
		}
		b.WriteString("\n")
	}
	return strings.TrimSpace(b.String())
}

func syntheticTools() []anthropicTool {
	obj := func(props map[string]any, required ...string) map[string]any {
		m := map[string]any{"type": "object", "properties": props}
		if len(required) > 0 {
			req := make([]any, len(required))
			for i, r := range required {
				req[i] = r
			}
			m["required"] = req
		}
		return m
	}
	str := map[string]any{"type": "string"}
	return []anthropicTool{
		{Name: "get_tool_instructions", Description: "Returns the parameters and rules of another tool.", InputSchema: obj(map[string]any{"tool_code": str}, "tool_code")},
		{Name: "get_customer_records", Description: "Lists the records held for the customer, one page at a time.", InputSchema: obj(map[string]any{"customer": str, "page": map[string]any{"type": "integer"}}, "customer")},
		{Name: "lookup_hours", Description: "Opening hours of a branch on a given weekday.", InputSchema: obj(map[string]any{"branch_id": str, "day": str}, "branch_id")},
		{Name: "create_booking", Description: "Books an appointment.", InputSchema: obj(map[string]any{"customer": str, "start": str, "service_id": str}, "customer", "start")},
	}
}

// syntheticItemsResult is a JSON tool result of about approxBytes: a list of
// small records, the shape the prompt fitter has to shorten.
func syntheticItemsResult(tag string, approxBytes int) string {
	var items []string
	total := 0
	for i := 0; total < approxBytes; i++ {
		item := fmt.Sprintf(`{"item_id":%d,"label":"%s item %d","status":"Active","branch_id":%d,"note":"%s"}`,
			70000+i, tag, i, i%3+1, strings.Repeat("synthetic note text ", 5))
		items = append(items, item)
		total += len(item) + 1
	}
	return fmt.Sprintf(`{"success":true,"data":{"body":{"data":{"items":[%s]}}}}`, strings.Join(items, ","))
}

func toolUseBlock(id, name string, input map[string]any) []any {
	return []any{map[string]any{"type": "tool_use", "id": id, "name": name, "input": input}}
}

func toolResultBlock(id, content string) []any {
	return []any{map[string]any{"type": "tool_result", "tool_use_id": id, "content": content}}
}

// syntheticLongRequest is a long conversation over a persona of personaBytes:
// oldResults earlier exchanges, each fetching a page of records of resultBytes,
// and a last customer message whose own lookup returns one more page.
func syntheticLongRequest(personaBytes, oldResults, resultBytes int) anthropicRequest {
	msgs := []anthropicMessage{
		{Role: "user", Content: `[AUTO-CONTEXT — the system already PREFLIGHTED (read the tool's instructions) AND executed the tool "get_customer_records" for this customer.]`},
	}
	for i := 0; i < oldResults; i++ {
		id := fmt.Sprintf("call-%d", i)
		msgs = append(msgs,
			anthropicMessage{Role: "user", Content: fmt.Sprintf("Customer question number %d: could you check page %d of my records please? مرحبا، بدي أتأكد من السجل رقم %d", i, i, i)},
			anthropicMessage{Role: "assistant", Content: toolUseBlock(id, "get_customer_records", map[string]any{"customer": "synthetic-1", "page": i})},
			anthropicMessage{Role: "user", Content: toolResultBlock(id, syntheticItemsResult(fmt.Sprintf("page %d", i), resultBytes))},
			anthropicMessage{Role: "assistant", Content: fmt.Sprintf("Answer number %d: I checked page %d of your records and everything on it is in order. تم التأكد من السجل رقم %d.", i, i, i)},
		)
	}
	last := fmt.Sprintf("call-%d", oldResults)
	msgs = append(msgs,
		anthropicMessage{Role: "user", Content: "And what does the newest page of my records show?"},
		anthropicMessage{Role: "assistant", Content: toolUseBlock(last, "get_customer_records", map[string]any{"customer": "synthetic-1", "page": oldResults})},
		anthropicMessage{Role: "user", Content: toolResultBlock(last, syntheticItemsResult(fmt.Sprintf("page %d", oldResults), resultBytes))},
	)
	return anthropicRequest{
		Model:    "gemini-3.8-flash-medium",
		System:   syntheticPersona(personaBytes),
		Tools:    syntheticTools(),
		Messages: msgs,
	}
}

// ---------------------------------------------------------------------------
// Golden prompts
// ---------------------------------------------------------------------------

type personaGoldenCase struct {
	name string
	cfg  config
	req  anthropicRequest
}

func personaGoldenCases() []personaGoldenCase {
	temp := 0.2
	return []personaGoldenCase{
		{
			// A short persona and a three-turn chat with one tool call and its
			// result; the default budget.
			name: "a_short_persona_three_turns",
			cfg:  config{},
			req: anthropicRequest{
				Model:  "gemini-3.8-flash-medium",
				System: "You are Nadia, the booking assistant of Example Clinic. Answer briefly and politely.",
				Tools:  syntheticTools(),
				Messages: []anthropicMessage{
					{Role: "user", Content: "Hi, what time do you open on Sunday? مرحبا، متى تفتحون يوم الأحد؟"},
					{Role: "assistant", Content: toolUseBlock("h1", "lookup_hours", map[string]any{"branch_id": "2", "day": "sunday"})},
					{Role: "user", Content: toolResultBlock("h1", `{"success":true,"data":{"branch_id":2,"day":"sunday","opens":"10:00","closes":"18:00"}}`)},
					{Role: "assistant", Content: "We are open on Sunday from 10:00 to 18:00."},
					{Role: "user", Content: "Great, and is there parking?"},
				},
			},
		},
		{
			// A 130KB persona over a long transcript of big JSON results: the
			// prompt does not fit and the fitter has to shorten results.
			name: "b_large_persona_fitted",
			cfg:  config{AgyPromptBudget: 186000},
			req:  syntheticLongRequest(130000, 5, 25000),
		},
		{
			// A persona with a temperature.
			name: "c_temperature",
			cfg:  config{},
			req: anthropicRequest{
				Model:       "gemini-3.8-flash-medium",
				System:      syntheticPersona(6000),
				Temperature: &temp,
				Tools:       syntheticTools(),
				Messages: []anthropicMessage{
					{Role: "user", Content: "Can I move my appointment to next week?"},
				},
			},
		},
		{
			// The bare single-turn chat: no system prompt, no tools.
			name: "d_bare_chat",
			cfg:  config{},
			req: anthropicRequest{
				Messages: []anthropicMessage{{Role: "user", Content: "What is 2+2?"}},
			},
		},
	}
}

// goldenPath is where the golden prompt for a case lives.
func goldenPath(name string) string {
	return filepath.Join("testdata", "golden_prompts", name+".txt")
}

// firstDiff describes where two strings first differ.
func firstDiff(want, got string) string {
	n := len(want)
	if len(got) < n {
		n = len(got)
	}
	i := 0
	for i < n && want[i] == got[i] {
		i++
	}
	lo := i - 60
	if lo < 0 {
		lo = 0
	}
	hiW, hiG := i+60, i+60
	if hiW > len(want) {
		hiW = len(want)
	}
	if hiG > len(got) {
		hiG = len(got)
	}
	return fmt.Sprintf("first difference at byte %d (golden %d bytes, got %d bytes)\ngolden: %q\ngot:    %q", i, len(want), len(got), want[lo:hiW], got[lo:hiG])
}

func TestPersonaAgentGolden(t *testing.T) {
	for _, c := range personaGoldenCases() {
		c := c
		t.Run(c.name, func(t *testing.T) {
			in := agyGenInputFromAnthropic(c.req, 0)
			render := func(cfg config) string {
				prompt, _ := renderAgyPromptFitted(cfg, in.System, in.Temperature, in.Tools, in.Transcript)
				return prompt
			}
			got := render(c.cfg)
			path := goldenPath(c.name)
			if os.Getenv("UPDATE_GOLDEN") == "1" {
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
					t.Fatal(err)
				}
				t.Logf("wrote %s (%d bytes)", path, len(got))
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("no golden prompt (%v); generate it with UPDATE_GOLDEN=1 from the code as it was before the switch", err)
			}
			// The same prompt, whether or not a chat agent is configured: neither
			// changes what the message says.
			for label, cfg := range map[string]config{
				"default":            c.cfg,
				"connect-chat agent": func() config { x := c.cfg; x.AgyAgent = agyChatAgentName; return x }(),
			} {
				if p := render(cfg); p != string(want) {
					t.Fatalf("%s: prompt differs from the golden\n%s", label, firstDiff(string(want), p))
				}
			}
		})
	}
}

// The large-persona golden is only worth anything if the prompt really had to
// be fitted to get under the budget.
func TestPersonaAgentGoldenFixtureNeedsFitting(t *testing.T) {
	c := personaGoldenCases()[1]
	in := agyGenInputFromAnthropic(c.req, 0)
	unfitted, _ := renderAgyPromptFitted(config{AgyPromptBudget: -1}, in.System, in.Temperature, in.Tools, in.Transcript)
	fitted, reduced := renderAgyPromptFitted(c.cfg, in.System, in.Temperature, in.Tools, in.Transcript)
	if len(unfitted) <= c.cfg.AgyPromptBudget {
		t.Fatalf("fixture: the unfitted prompt (%d bytes) already fits the %d-byte budget", len(unfitted), c.cfg.AgyPromptBudget)
	}
	if len(fitted) > c.cfg.AgyPromptBudget || len(reduced) == 0 {
		t.Fatalf("fixture: fitted prompt is %d bytes (budget %d), reduced tools %v", len(fitted), c.cfg.AgyPromptBudget, reduced)
	}
}

// ---------------------------------------------------------------------------
// Helpers for the switch tests
// ---------------------------------------------------------------------------

// The texts the design fixes, spelled out here independently of the constants in
// agy_persona_agent.go, so an accidental edit of a constant shows up.
const (
	wantPersonaInstructions = "# Instructions\nYou are the assistant defined by the SYSTEM INSTRUCTIONS & POLICIES below; they are your own standing instructions. Each user message is one chat turn request: it contains the current state, the tools you may call (described in the message, invoked ONLY by writing <tool_call> blocks in your reply), the conversation so far, and what to produce next. You have no files, shell, browser, web or other capabilities of your own; never attempt to read, search or run anything. Produce exactly the assistant's next turn as the message specifies, and nothing else.\n\n# SYSTEM INSTRUCTIONS & POLICIES\n\n"
	wantPersonaPointer      = "Your system instructions and policies are your own standing agent instructions; they are not repeated in this message. Follow them exactly.\n\n"
	wantHowToReadOn         = "a SYSTEM INSTRUCTIONS & POLICIES pointer (its identity and rules are its own agent instructions)"
	wantHowToReadOff        = "the assistant's SYSTEM INSTRUCTIONS & POLICIES (its identity and rules)"
)

// personaTestHome points agyAgentsDir() at a temp HOME, the way the other agent
// definition tests do.
func personaTestHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	return home
}

func personaAgentsDir(home string) string {
	return filepath.Join(home, ".gemini", "config", "agents")
}

func personaAgentFile(home, name string) string {
	return filepath.Join(personaAgentsDir(home), name, "agent.md")
}

// personaWantDefinition is the agent.md the design describes, spelled out.
func personaWantDefinition(name, persona string) string {
	return "---\n" +
		"name: " + name + "\n" +
		"description: connect-ai-proxy chat agent carrying one caller persona. No built-in tools.\n" +
		"mainAgent: true\n" +
		"subagent: false\n" +
		"inheritMcp: false\n" +
		"tools: []\n" +
		"---\n" +
		wantPersonaInstructions + strings.TrimSpace(persona) + "\n"
}

// captureLog collects the standard logger's output for the rest of the test.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(prev) })
	return &buf
}

// personaResolveCall is what one stubbed agyResolveFn call saw.
type personaResolveCall struct {
	cfg    config
	media  int
	prompt string
}

// stubPersonaResolve replaces agyResolveFn with a stand-in that records each call
// and answers with reply(prompt).
func stubPersonaResolve(t *testing.T, reply func(prompt string) string) *[]personaResolveCall {
	t.Helper()
	orig := agyResolveFn
	var calls []personaResolveCall
	agyResolveFn = func(_ context.Context, cfg config, parts []mediaPart, prompt, _ string) (agyResult, error) {
		calls = append(calls, personaResolveCall{cfg: cfg, media: len(parts), prompt: prompt})
		return agyResult{Ok: true, Response: reply(prompt)}, nil
	}
	t.Cleanup(func() { agyResolveFn = orig })
	return &calls
}

func constantReply(s string) func(string) string { return func(string) string { return s } }

// The customers of the allowlist tests. FAKE numbers only; nothing here is a real
// person's. personaTestNumber is the customer the switch is meant for (given in
// each of its three spellings below), personaTestOtherNumber is one who is not
// listed, and personaTestDocNumber is a fictional number that appears inside a
// persona's tool docs, which must never be read as the customer's.
const (
	personaTestNumber      = "+962790000001"
	personaTestOtherNumber = "+962790000002"
	personaTestDocNumber   = "+1 555 010 0199"
)

// personaTestForms are three spellings of personaTestNumber that must all be the
// same customer: international with punctuation, 00-prefixed, and local.
var personaTestForms = []string{"+962 79-000-0001", "00962790000001", "0790000001"}

// personaTestSystem is a persona followed by the Platform Context section Connect
// writes into its system prompts, naming identifier as the customer.
func personaTestSystem(persona, identifier string) string {
	return persona + "\n\n## Platform Context\nPlatform: whatsapp\nSpeaking with: Test Customer\nUser identifier: " + identifier + "\n"
}

// personaCustomerInput is agyGenInputFromAnthropic with the customer named in the
// system prompt (a request without a system prompt, or with no identifier given,
// is left as it is).
func personaCustomerInput(req anthropicRequest, identifier string) agyGenInput {
	in := agyGenInputFromAnthropic(req, 0)
	if identifier != "" && in.System != "" {
		in.System = personaTestSystem(in.System, identifier)
	}
	return in
}

// personaTestAllowlist sets cfg's allowlist from a PROXY_AGY_PERSONA_IN_AGENT_NUMBERS value.
func personaTestAllowlist(cfg config, list string) config {
	cfg.AgyPersonaInAgentNumbers, cfg.AgyPersonaInAgentAll = agyParsePersonaNumbers(list)
	return cfg
}

// personaTestOnConfig is a config with the switch on and personaTestNumber
// allowlisted.
func personaTestOnConfig() config {
	return personaTestAllowlist(config{AgyPersonaInAgent: true, AgyAgent: agyChatAgentName, AgyPersonaAgentPrefix: agyPersonaAgentDefaultPrefix}, personaTestNumber)
}

// personaLogLines returns the [agy-persona] lines of a captured log.
func personaLogLines(logs string) []string {
	var out []string
	for _, line := range strings.Split(logs, "\n") {
		if strings.Contains(line, "[agy-persona]") {
			out = append(out, line)
		}
	}
	return out
}

// resetAgyBriefCache empties the process-wide cache of written briefs, so a test
// that counts summariser calls does not depend on what ran before it.
func resetAgyBriefCache(t *testing.T) {
	t.Helper()
	wipe := func() {
		agyBriefCacheMu.Lock()
		agyBriefCache = map[string]agyBriefEntry{}
		agyBriefCacheMu.Unlock()
	}
	wipe()
	t.Cleanup(wipe)
}

func toolResultBytes(tr agyTranscript) int {
	n := 0
	for _, tt := range tr.Turns {
		if tt.Kind == agyTurnToolResult {
			n += len(tt.Text)
		}
	}
	return n
}

// personaFrontmatterKeys lists the keys of a definition's frontmatter.
func personaFrontmatterKeys(t *testing.T, def string) []string {
	t.Helper()
	rest := strings.TrimPrefix(def, "---\n")
	end := strings.Index(rest, "\n---\n")
	if rest == def || end < 0 {
		t.Fatalf("no frontmatter in:\n%s", truncateString(def, 300))
	}
	var keys []string
	for _, line := range strings.Split(rest[:end], "\n") {
		if i := strings.Index(line, ":"); i > 0 {
			keys = append(keys, line[:i])
		}
	}
	sort.Strings(keys)
	return keys
}

// uniquePersonaDialogue is a long conversation with no tools whose text no other
// test has summarised (the brief cache is keyed by the material).
func uniquePersonaDialogue(tag, system string, turns, bytesPerMessage int) anthropicRequest {
	filler := strings.Repeat("details the customer gave about the request. ", bytesPerMessage/45+1)
	var msgs []anthropicMessage
	for i := 0; i < turns; i++ {
		msgs = append(msgs,
			anthropicMessage{Role: "user", Content: fmt.Sprintf("%s question %d: %s", tag, i, filler)},
			anthropicMessage{Role: "assistant", Content: fmt.Sprintf("%s answer %d: %s", tag, i, filler)},
		)
	}
	msgs = append(msgs, anthropicMessage{Role: "user", Content: tag + " final question: is everything in order?"})
	return anthropicRequest{Model: "gemini-3.8-flash-medium", System: system, Messages: msgs}
}

// ---------------------------------------------------------------------------
// The prompt with the switch off is today's prompt
// ---------------------------------------------------------------------------

// With a persona agent serving the request, the message differs from today's in
// exactly two places: the persona is replaced by the pointer, and the HOW TO READ
// sentence names a pointer. Put those two back and the golden comes out.
func TestPersonaAgentMessageDiffersFromGoldenOnlyWhereThePersonaWas(t *testing.T) {
	cases := personaGoldenCases()
	for _, c := range []personaGoldenCase{cases[0], cases[2]} {
		c := c
		t.Run(c.name, func(t *testing.T) {
			in := agyGenInputFromAnthropic(c.req, 0)
			cfg := c.cfg
			cfg.agyPersonaAgent = "connect-chat-p-0123456789ab"
			on, _ := renderAgyPromptFitted(cfg, in.System, in.Temperature, in.Tools, in.Transcript)
			if strings.Contains(on, in.System) {
				t.Fatal("the persona is still in the message")
			}
			if strings.Count(on, wantPersonaPointer) != 1 || strings.Count(on, wantHowToReadOn) != 1 {
				t.Fatalf("expected the pointer once and the new HOW TO READ wording once:\n%s", truncateString(on, 2500))
			}
			back := strings.Replace(on, wantPersonaPointer, in.System+"\n\n", 1)
			back = strings.Replace(back, wantHowToReadOn, wantHowToReadOff, 1)
			want, err := os.ReadFile(goldenPath(c.name))
			if err != nil {
				t.Fatal(err)
			}
			if back != string(want) {
				t.Fatalf("the message differs from today's beyond the persona and the HOW TO READ wording\n%s", firstDiff(string(want), back))
			}
		})
	}
}

// The temperature directive stays in the message, in front of the pointer.
func TestPersonaAgentMessageKeepsTheTemperatureDirective(t *testing.T) {
	temp := 0.2
	tr := buildAgyTranscriptFromAnthropic(anthropicRequest{Messages: []anthropicMessage{{Role: "user", Content: "Hello"}}})
	cfg := config{agyPersonaAgent: "connect-chat-p-0123456789ab"}
	got, _ := renderAgyPromptFitted(cfg, "You are Nadia.", &temp, nil, tr)
	want := "### SYSTEM INSTRUCTIONS & POLICIES\n\n" + buildAgyTempDirective(&temp) + "\n\n" + wantPersonaPointer
	if !strings.Contains(got, want) {
		t.Fatalf("expected heading, directive, pointer in that order:\n%s", got)
	}
	if strings.Contains(got, "You are Nadia.") {
		t.Fatal("the persona is still in the message")
	}
}

// The field does nothing without a persona to move: the bare chat is still the
// bare chat, and the pointer is never promised when there is nothing behind it.
func TestPersonaAgentFieldWithoutPersonaChangesNothing(t *testing.T) {
	cfg := config{agyPersonaAgent: "connect-chat-p-0123456789ab"}
	bare := buildAgyTranscriptFromAnthropic(anthropicRequest{Messages: []anthropicMessage{{Role: "user", Content: "What is 2+2?"}}})
	if got, _ := renderAgyPromptFitted(cfg, "", nil, nil, bare); got != "What is 2+2?" {
		t.Fatalf("bare chat = %q", got)
	}
	c := personaGoldenCases()[0]
	in := agyGenInputFromAnthropic(c.req, 0)
	withTools, _ := renderAgyPromptFitted(cfg, "", nil, in.Tools, in.Transcript)
	plain, _ := renderAgyPromptFitted(config{}, "", nil, in.Tools, in.Transcript)
	if withTools != plain || strings.Contains(withTools, wantPersonaPointer) {
		t.Fatal("a pointer was written although there is no persona")
	}
}

// ---------------------------------------------------------------------------
// The agent definition
// ---------------------------------------------------------------------------

func TestEnsureAgyPersonaAgentWritesTheDefinition(t *testing.T) {
	home := personaTestHome(t)
	logs := captureLog(t)
	persona := syntheticPersona(130000)
	cfg := config{AgyPersonaInAgent: true, AgyAgent: agyChatAgentName, AgyPersonaAgentPrefix: agyPersonaAgentDefaultPrefix}

	name, err := ensureAgyPersonaAgent(cfg, persona)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(name, "connect-chat-p-") || len(name) != len("connect-chat-p-")+12 {
		t.Fatalf("name = %q, want connect-chat-p-<12 hex>", name)
	}
	path := personaAgentFile(home, name)
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("agent.md not installed: %v", err)
	}
	if string(got) != personaWantDefinition(name, persona) {
		t.Fatalf("agent.md differs from the design\n%s", firstDiff(personaWantDefinition(name, persona), string(got)))
	}
	if !strings.Contains(string(got), "tools: []") || strings.Contains(string(got), "commandExecutionPolicy") {
		t.Fatal("the persona agent must have no tools and no execution policy")
	}
	if !strings.HasSuffix(string(got), persona+"\n") || strings.Contains(string(got), persona+"\n\n") {
		t.Fatal("the persona must end the file verbatim, followed by exactly one newline")
	}
	if want := fmt.Sprintf("[agy] installed persona agent %s (%d bytes)", name, len(got)); !strings.Contains(logs.String(), want) {
		t.Fatalf("missing log line %q in:\n%s", want, logs.String())
	}
	if entries, err := os.ReadDir(filepath.Dir(path)); err != nil || len(entries) != 1 || entries[0].Name() != "agent.md" {
		t.Fatalf("the agent directory should hold agent.md only (temp file left behind?): %v %v", entries, err)
	}
	if runtime.GOOS != "windows" {
		if st, err := os.Stat(path); err != nil || st.Mode().Perm() != 0o644 {
			t.Fatalf("agent.md mode = %v, want 0644 (%v)", st.Mode().Perm(), err)
		}
	}
}

// The frontmatter uses the key set of the built-in chat agent.
func TestAgyPersonaAgentFrontmatterHasTheChatAgentsKeys(t *testing.T) {
	def := agyPersonaAgentDefinition("connect-chat-p-0123456789ab", "You are Nadia.")
	if got, want := personaFrontmatterKeys(t, def), personaFrontmatterKeys(t, agyChatAgentDefinition); !reflect.DeepEqual(got, want) {
		t.Fatalf("frontmatter keys = %v, want %v", got, want)
	}
	if def != personaWantDefinition("connect-chat-p-0123456789ab", "You are Nadia.") {
		t.Fatalf("definition = %q", def)
	}
	if agyPersonaAgentInstructions != wantPersonaInstructions {
		t.Fatal("agyPersonaAgentInstructions was edited")
	}
}

func TestEnsureAgyPersonaAgentIsIdempotent(t *testing.T) {
	home := personaTestHome(t)
	cfg := config{AgyPersonaAgentPrefix: agyPersonaAgentDefaultPrefix}
	persona := syntheticPersona(20000)

	name, err := ensureAgyPersonaAgent(cfg, persona)
	if err != nil {
		t.Fatal(err)
	}
	path := personaAgentFile(home, name)
	old := time.Now().Add(-2 * time.Hour).Truncate(time.Second)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	unchanged := func(label string) {
		t.Helper()
		st, err := os.Stat(path)
		if err != nil {
			t.Fatalf("%s: %v", label, err)
		}
		if !st.ModTime().Equal(old) {
			t.Fatalf("%s: agent.md was rewritten (mtime %v, want %v)", label, st.ModTime(), old)
		}
	}

	// From the in-process cache.
	if again, err := ensureAgyPersonaAgent(cfg, persona); err != nil || again != name {
		t.Fatalf("second call = %q, %v; want %q", again, err, name)
	}
	unchanged("cached")

	// Without the cache: the file is read back, found identical, left alone.
	agyPersonaAgentVerified.Delete(path)
	if again, err := ensureAgyPersonaAgent(cfg, persona); err != nil || again != name {
		t.Fatalf("uncached call = %q, %v; want %q", again, err, name)
	}
	unchanged("uncached")

	// A different spelling of the same persona (surrounding whitespace) is the same agent.
	if again, err := ensureAgyPersonaAgent(cfg, "\n  "+persona+" \n\n"); err != nil || again != name {
		t.Fatalf("whitespace-padded persona = %q, %v; want %q", again, err, name)
	}
	unchanged("padded")

	// Deleted underneath the process: the stat fails and the agent is installed again.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if again, err := ensureAgyPersonaAgent(cfg, persona); err != nil || again != name {
		t.Fatalf("after deletion = %q, %v; want %q", again, err, name)
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != personaWantDefinition(name, persona) {
		t.Fatalf("agent.md was not restored: %v", err)
	}

	// Damaged underneath the process (truncated): repaired, cache or no cache.
	if err := os.WriteFile(path, []byte("---\nname: broken\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ensureAgyPersonaAgent(cfg, persona); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != personaWantDefinition(name, persona) {
		t.Fatalf("a truncated agent.md was not repaired: %v", err)
	}
}

func TestEnsureAgyPersonaAgentDistinguishesPersonas(t *testing.T) {
	home := personaTestHome(t)
	cfg := config{AgyPersonaAgentPrefix: agyPersonaAgentDefaultPrefix}
	a, err := ensureAgyPersonaAgent(cfg, syntheticPersona(5000))
	if err != nil {
		t.Fatal(err)
	}
	b, err := ensureAgyPersonaAgent(cfg, syntheticPersona(5000)+"\nOne more rule.")
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Fatalf("different personas share the agent %q", a)
	}
	for _, name := range []string{a, b} {
		if _, err := os.Stat(personaAgentFile(home, name)); err != nil {
			t.Fatalf("agent %s not installed: %v", name, err)
		}
	}
	// The name follows the instructions plus the persona, and the prefix.
	if agyPersonaAgentName("acme-p", "x") == agyPersonaAgentName("acme-q", "x") || !strings.HasPrefix(agyPersonaAgentName("acme-p", "x"), "acme-p-") {
		t.Fatal("the prefix is not part of the name")
	}
	if agyPersonaAgentName("acme-p", "x") != agyPersonaAgentName("acme-p", " x\n") {
		t.Fatal("surrounding whitespace changed the name")
	}
	if _, err := ensureAgyPersonaAgent(cfg, " \n\t "); err == nil {
		t.Fatal("an empty persona must not produce an agent")
	}
}

// A prefix that could not be a directory name or a CLI value is not used.
func TestAgyPersonaAgentSafePrefix(t *testing.T) {
	for in, want := range map[string]string{
		"":               agyPersonaAgentDefaultPrefix,
		"  ":             agyPersonaAgentDefaultPrefix,
		"acme-p":         "acme-p",
		" acme_p.1 ":     "acme_p.1",
		"-rf":            agyPersonaAgentDefaultPrefix,
		"../x":           agyPersonaAgentDefaultPrefix,
		"a/b":            agyPersonaAgentDefaultPrefix,
		`a\b`:            agyPersonaAgentDefaultPrefix,
		"with space":     agyPersonaAgentDefaultPrefix,
		".hidden":        agyPersonaAgentDefaultPrefix,
		"connect-chat-p": "connect-chat-p",
	} {
		if got := agyPersonaAgentSafePrefix(in); got != want {
			t.Errorf("agyPersonaAgentSafePrefix(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestEnsureAgyPersonaAgentConcurrentInstalls(t *testing.T) {
	home := personaTestHome(t)
	captureLog(t)
	cfg := config{AgyPersonaAgentPrefix: agyPersonaAgentDefaultPrefix}
	persona := syntheticPersona(60000)
	const n = 8
	names := make([]string, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			names[i], errs[i] = ensureAgyPersonaAgent(cfg, persona)
		}(i)
	}
	wg.Wait()
	for i := 0; i < n; i++ {
		if errs[i] != nil || names[i] != names[0] {
			t.Fatalf("call %d = %q, %v; want %q", i, names[i], errs[i], names[0])
		}
	}
	path := personaAgentFile(home, names[0])
	if got, err := os.ReadFile(path); err != nil || string(got) != personaWantDefinition(names[0], persona) {
		t.Fatalf("agent.md wrong after concurrent installs: %v", err)
	}
	if entries, _ := os.ReadDir(filepath.Dir(path)); len(entries) != 1 {
		t.Fatalf("leftover files in the agent directory: %v", entries)
	}
}

// ---------------------------------------------------------------------------
// When a request gets a persona agent
// ---------------------------------------------------------------------------

func TestAgyPersonaAgentForConditions(t *testing.T) {
	home := personaTestHome(t)
	captureLog(t)
	on := personaTestOnConfig()
	in := personaCustomerInput(syntheticLongRequest(3000, 1, 500), personaTestNumber)

	png := []mediaPart{{B64: tinyPNGBase64(t), MediaType: "image/png", Filename: "x.png"}}
	for _, c := range []struct {
		name   string
		cfg    config
		mutate func(*agyGenInput)
	}{
		{"switch off", func() config { c := on; c.AgyPersonaInAgent = false; return c }(), nil},
		{"no persona", on, func(in *agyGenInput) { in.System = "" }},
		{"blank persona", on, func(in *agyGenInput) { in.System = " \n\t " }},
		{"media run", on, func(in *agyGenInput) { in.Media = png }},
		{"agy default coding agent configured", func() config { c := on; c.AgyAgent = ""; return c }(), nil},
		{"blank agent", func() config { c := on; c.AgyAgent = "  "; return c }(), nil},
	} {
		x := in
		if c.mutate != nil {
			c.mutate(&x)
		}
		if got := agyPersonaAgentFor(c.cfg, x); got != "" {
			t.Errorf("%s: got persona agent %q, want none", c.name, got)
		}
	}
	if _, err := os.Stat(personaAgentsDir(home)); err == nil {
		t.Fatal("a request that gets no persona agent must not install one")
	}

	// And when every condition holds, it does.
	name := agyPersonaAgentFor(on, in)
	if name == "" {
		t.Fatal("no persona agent although every condition holds")
	}
	if name != agyPersonaAgentName(agyPersonaAgentDefaultPrefix, in.System) {
		t.Fatalf("name = %q", name)
	}
	if _, err := os.Stat(personaAgentFile(home, name)); err != nil {
		t.Fatalf("agent not installed: %v", err)
	}
}

// A persona agent that cannot be installed leaves the request exactly as it was:
// the persona travels in the message.
func TestAgyPersonaAgentInstallFailureFallsBack(t *testing.T) {
	home := personaTestHome(t)
	logs := captureLog(t)
	// ~/.gemini is a file, so nothing can be created under it.
	if err := os.WriteFile(filepath.Join(home, ".gemini"), []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := personaTestOnConfig()
	cfg.AgyPromptBudget = 186000
	req := syntheticLongRequest(3000, 1, 500)
	in := personaCustomerInput(req, personaTestNumber)
	if _, err := ensureAgyPersonaAgent(cfg, in.System); err == nil {
		t.Fatal("expected an error")
	}
	if got := agyPersonaAgentFor(cfg, in); got != "" {
		t.Fatalf("persona agent %q although the install failed", got)
	}
	if !strings.Contains(logs.String(), "persona agent unavailable") {
		t.Fatalf("the fallback was not logged:\n%s", logs.String())
	}

	calls := stubPersonaResolve(t, constantReply("Your newest page of records shows only active items."))
	if _, _, err := agyGenerate(context.Background(), cfg, in); err != nil {
		t.Fatal(err)
	}
	if len(*calls) != 1 {
		t.Fatalf("resolve called %d times", len(*calls))
	}
	if c := (*calls)[0]; c.cfg.agyPersonaAgent != "" || !strings.Contains(c.prompt, personaTestMarker) {
		t.Fatalf("fallback: persona agent %q, persona in message: %v", c.cfg.agyPersonaAgent, strings.Contains(c.prompt, personaTestMarker))
	}
}

// ---------------------------------------------------------------------------
// The room the persona no longer takes
// ---------------------------------------------------------------------------

func TestPersonaAgentGivesTheTranscriptTheRoomThePersonaTook(t *testing.T) {
	req := syntheticLongRequest(130000, 5, 25000)
	in := agyGenInputFromAnthropic(req, 0)
	full := toolResultBytes(in.Transcript)
	if full < 140000 || full > 170000 {
		t.Fatalf("fixture: tool results total %d bytes, want about 150KB", full)
	}
	if !strings.Contains(in.System, personaTestMarker) {
		t.Fatal("fixture: the persona lacks its marker")
	}

	off := config{AgyPromptBudget: 186000}
	on := off
	on.agyPersonaAgent = "connect-chat-p-0123456789ab"
	offPrompt, offFitted := renderAgyPromptFittedTranscript(off, in.System, in.Temperature, in.Tools, in.Transcript)
	onPrompt, onFitted := renderAgyPromptFittedTranscript(on, in.System, in.Temperature, in.Tools, in.Transcript)

	// The persona is in the message with the switch off, and not with it on.
	if !strings.Contains(offPrompt, personaTestMarker) {
		t.Fatal("control: the persona should be in the message when it is not served by an agent")
	}
	if strings.Contains(onPrompt, personaTestMarker) || strings.Contains(onPrompt, "Rule 100.0:") {
		t.Fatal("the persona is still in the message")
	}
	if !strings.Contains(onPrompt, "### SYSTEM INSTRUCTIONS & POLICIES\n\n"+wantPersonaPointer) {
		t.Fatalf("the pointer is missing:\n%s", truncateString(onPrompt, 3000))
	}
	if !strings.Contains(onPrompt, wantHowToReadOn+", its TOOLS, the CONVERSATION SO FAR with a customer, a DIALOGUE DIGEST, and YOUR NEXT TURN. ") ||
		strings.Contains(onPrompt, wantHowToReadOff) {
		t.Fatal("the HOW TO READ sentence does not describe the pointer")
	}
	for label, p := range map[string]string{"off": offPrompt, "on": onPrompt} {
		if len(p) > off.AgyPromptBudget {
			t.Fatalf("%s: prompt is %d bytes, over the %d-byte budget", label, len(p), off.AgyPromptBudget)
		}
	}

	// The room goes to the transcript.
	offKept, onKept := toolResultBytes(offFitted), toolResultBytes(onFitted)
	if onKept <= offKept {
		t.Fatalf("results kept: %d with the persona in the message, %d without; want more without", offKept, onKept)
	}
	if len(offFitted.reducedTools()) == 0 {
		t.Fatal("control: with the persona in the message the results should have been shortened")
	}
	if len(onFitted.reducedTools()) != 0 || onKept != full {
		t.Fatalf("with the room the persona no longer takes, every result should survive whole (kept %d of %d, reduced %v)", onKept, full, onFitted.reducedTools())
	}
	whole := func(fitted agyTranscript) int {
		n := 0
		for i, tt := range fitted.Turns {
			if tt.Kind == agyTurnToolResult && tt.Text == in.Transcript.Turns[i].Text {
				n++
			}
		}
		return n
	}
	if whole(onFitted) <= whole(offFitted) {
		t.Fatalf("results surviving whole: %d off, %d on", whole(offFitted), whole(onFitted))
	}
}

// ---------------------------------------------------------------------------
// agyResolve: a cold run under the persona agent
// ---------------------------------------------------------------------------

func TestAgyResolveRunsAPersonaAgentColdUnderItsOwnName(t *testing.T) {
	logs := captureLog(t)
	// An enabled warm pool that cannot serve anything. Whether agyResolve asks it
	// shows in the log; it never starts a process.
	pool := &AgyWorkerPool{size: 1, model: "pool-only-model", queueTimeout: 20 * time.Millisecond, idleWorkers: make(chan *AgyWorker, 1)}
	globalAgyPoolMu.Lock()
	prevPool := globalAgyPool
	globalAgyPool = pool
	globalAgyPoolMu.Unlock()
	t.Cleanup(func() {
		globalAgyPoolMu.Lock()
		globalAgyPool = prevPool
		globalAgyPoolMu.Unlock()
	})
	rec := fakeAgyRunAgentFn(t)
	ctx := context.Background()

	// Control: without a persona agent the request is offered to the pool first,
	// and its agy arguments are exactly what they have always been.
	cfg := config{AgyAgent: agyChatAgentName}
	if _, err := agyResolve(ctx, cfg, nil, "hello", "some-alias"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(logs.String(), "[agy-pool]") {
		t.Fatalf("control: the warm pool was not consulted:\n%s", logs.String())
	}
	if strings.Contains(logs.String(), "persona agent") {
		t.Fatalf("control: unexpected persona agent log:\n%s", logs.String())
	}
	if !rec.Called || !reflect.DeepEqual(rec.AgentArgs, agyAgentArgs(cfg, false)) || !reflect.DeepEqual(rec.AgentArgs, []string{"--agent", agyChatAgentName}) {
		t.Fatalf("agentArgs = %v, want %v", rec.AgentArgs, agyAgentArgs(cfg, false))
	}
	logs.Reset()
	rec.Called = false

	// With a persona agent: no warm pool, and --agent <name>.
	cfg.agyPersonaAgent = "connect-chat-p-0123456789ab"
	if _, err := agyResolve(ctx, cfg, nil, "hello", "some-alias"); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(logs.String(), "[agy-pool]") {
		t.Fatalf("the warm pool was used for a persona agent:\n%s", logs.String())
	}
	if want := "[agy] persona agent connect-chat-p-0123456789ab: cold run (warm pool skipped)"; strings.Count(logs.String(), want) != 1 {
		t.Fatalf("expected %q once in:\n%s", want, logs.String())
	}
	if !rec.Called || !reflect.DeepEqual(rec.AgentArgs, []string{"--agent", "connect-chat-p-0123456789ab"}) {
		t.Fatalf("agentArgs = %v, want [--agent connect-chat-p-0123456789ab]", rec.AgentArgs)
	}
	if len(rec.AddDirs) != 0 || rec.Prompt != "hello" {
		t.Fatalf("addDirs = %v, prompt = %q", rec.AddDirs, rec.Prompt)
	}
	if len(pool.idleWorkers) != 0 {
		t.Fatal("the pool was touched")
	}
}

// A run with attachments keeps its own agent; the persona agent is for plain chat.
func TestAgyResolveMediaRunIgnoresThePersonaAgent(t *testing.T) {
	logs := captureLog(t)
	initAgyWorkerPool(config{AgyWarmWorkers: 0})
	setVTRuntime("", false)
	rec := fakeAgyRunAgentFn(t)

	cfg := config{
		AgyMedia: true, AgyMediaDir: t.TempDir(), AgyMediaAgent: agyMediaViewAgentName,
		AgyAgent: agyChatAgentName, agyPersonaAgent: "connect-chat-p-0123456789ab",
	}
	parts := []mediaPart{{B64: tinyPNGBase64(t), MediaType: "image/png", Filename: "receipt.png"}}
	if _, err := agyResolve(context.Background(), cfg, parts, "what does it say?", "some-alias"); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(rec.AgentArgs, " "); got != "--agent "+agyMediaViewAgentName {
		t.Fatalf("agentArgs = %q, want the media view agent", got)
	}
	if len(rec.AddDirs) == 0 {
		t.Fatal("expected addDirs for a media run")
	}
	if strings.Contains(logs.String(), "persona agent") {
		t.Fatalf("a media run reported a persona agent:\n%s", logs.String())
	}
}

// ---------------------------------------------------------------------------
// agyGenerate end to end
// ---------------------------------------------------------------------------

func TestAgyGenerateDeliversThePersonaThroughTheAgent(t *testing.T) {
	home := personaTestHome(t)
	captureLog(t)
	req := syntheticLongRequest(130000, 5, 25000)
	in := personaCustomerInput(req, personaTestNumber)
	cfg := personaTestOnConfig()
	cfg.AgyPromptBudget = 186000
	reply := "Your newest page of records shows only active items."
	calls := stubPersonaResolve(t, constantReply(reply))

	resp, trace, err := agyGenerate(context.Background(), cfg, in)
	if err != nil {
		t.Fatal(err)
	}
	if got := agyResponseText(resp); got != reply {
		t.Fatalf("reply = %q", got)
	}
	if len(*calls) != 1 {
		t.Fatalf("resolve called %d times, want 1", len(*calls))
	}
	call := (*calls)[0]
	name := agyPersonaAgentName(agyPersonaAgentDefaultPrefix, in.System)
	if call.cfg.agyPersonaAgent != name {
		t.Fatalf("resolve saw persona agent %q, want %q", call.cfg.agyPersonaAgent, name)
	}
	if strings.Contains(call.prompt, personaTestMarker) || strings.Contains(trace.Prompt, personaTestMarker) || strings.Contains(trace.FinalPrompt, personaTestMarker) {
		t.Fatal("the persona is in the message")
	}
	if !strings.Contains(call.prompt, wantPersonaPointer) {
		t.Fatal("the message lacks the pointer")
	}
	if len(call.prompt) > cfg.AgyPromptBudget {
		t.Fatalf("prompt is %d bytes, over the budget", len(call.prompt))
	}
	got, err := os.ReadFile(personaAgentFile(home, name))
	if err != nil {
		t.Fatalf("agent not installed: %v", err)
	}
	if string(got) != personaWantDefinition(name, in.System) || !strings.Contains(string(got), personaTestMarker) {
		t.Fatal("the installed agent does not carry the persona")
	}
	if cfg.agyPersonaAgent != "" {
		t.Fatal("agyGenerate changed the caller's config")
	}

	// Control: the same request with the switch off sends the persona in the
	// message, as before, and asks for no agent.
	*calls = nil
	off := cfg
	off.AgyPersonaInAgent = false
	if _, _, err := agyGenerate(context.Background(), off, in); err != nil {
		t.Fatal(err)
	}
	if len(*calls) != 1 || (*calls)[0].cfg.agyPersonaAgent != "" || !strings.Contains((*calls)[0].prompt, personaTestMarker) {
		t.Fatalf("switch off: %d calls, persona agent %q, persona in message %v", len(*calls), (*calls)[0].cfg.agyPersonaAgent, strings.Contains((*calls)[0].prompt, personaTestMarker))
	}
}

// A run with attachments keeps the persona in the message and installs nothing.
func TestAgyGenerateKeepsThePersonaInTheMessageForMediaRuns(t *testing.T) {
	home := personaTestHome(t)
	captureLog(t)
	in := personaCustomerInput(syntheticLongRequest(20000, 1, 500), personaTestNumber)
	in.Media = []mediaPart{{B64: tinyPNGBase64(t), MediaType: "image/png", Filename: "x.png"}}
	cfg := personaTestOnConfig()
	calls := stubPersonaResolve(t, constantReply("It shows a receipt."))

	if _, _, err := agyGenerate(context.Background(), cfg, in); err != nil {
		t.Fatal(err)
	}
	if len(*calls) != 1 {
		t.Fatalf("resolve called %d times", len(*calls))
	}
	if c := (*calls)[0]; c.cfg.agyPersonaAgent != "" || c.media != 1 || !strings.Contains(c.prompt, personaTestMarker) {
		t.Fatalf("media run: persona agent %q, media %d, persona in message %v", c.cfg.agyPersonaAgent, c.media, strings.Contains(c.prompt, personaTestMarker))
	}
	if _, err := os.Stat(personaAgentsDir(home)); err == nil {
		t.Fatal("a media run must not install a persona agent")
	}
}

// ---------------------------------------------------------------------------
// Compaction
// ---------------------------------------------------------------------------

// The brief is written by a neutral summariser: its call carries no persona
// agent, while the turn itself does, and it is sent after the brief.
func TestAgyCompactionBriefRunsWithoutThePersonaAgent(t *testing.T) {
	personaTestHome(t)
	captureLog(t)
	resetAgyBriefCache(t)
	persona := personaTestSystem(syntheticPersona(3000), personaTestNumber)
	req := uniquePersonaDialogue("briefcall", persona, 20, 1000)
	in := agyGenInputFromAnthropic(req, 0)
	cfg := personaTestOnConfig()
	cfg.AgyCompact = true
	cfg.AgyPromptBudget = 30000
	calls := stubPersonaResolve(t, func(prompt string) string {
		if strings.HasPrefix(prompt, "### TASK") {
			return "- the customer asked several questions and every one was answered"
		}
		return "Yes, everything is in order."
	})

	if _, _, err := agyGenerate(context.Background(), cfg, in); err != nil {
		t.Fatal(err)
	}
	var briefs, turns []personaResolveCall
	for _, c := range *calls {
		if strings.HasPrefix(c.prompt, "### TASK") {
			briefs = append(briefs, c)
		} else {
			turns = append(turns, c)
		}
	}
	if len(briefs) != 1 || len(turns) != 1 {
		t.Fatalf("expected one brief call and one turn call, got %d and %d", len(briefs), len(turns))
	}
	if briefs[0].cfg.agyPersonaAgent != "" {
		t.Fatalf("the brief was written under the persona agent %q", briefs[0].cfg.agyPersonaAgent)
	}
	if briefs[0].cfg.AgyAgent != agyChatAgentName {
		t.Fatal("the brief call lost the configured chat agent")
	}
	if want := agyPersonaAgentName(agyPersonaAgentDefaultPrefix, persona); turns[0].cfg.agyPersonaAgent != want {
		t.Fatalf("the turn ran under %q, want %q", turns[0].cfg.agyPersonaAgent, want)
	}
	if !strings.Contains(turns[0].prompt, agyBriefMarker) || strings.Contains(turns[0].prompt, personaTestMarker) {
		t.Fatal("the turn should carry the brief and not the persona")
	}
	if len(turns[0].prompt) > cfg.AgyPromptBudget {
		t.Fatalf("turn prompt is %d bytes, over the budget", len(turns[0].prompt))
	}
	if strings.Contains(briefs[0].prompt, personaTestMarker) {
		t.Fatal("the persona leaked into the summariser's prompt")
	}
}

// The decision to compact measures the prompt that will really be sent: a
// dialogue that only fits once the persona is out of the message is not
// summarised, and the same dialogue with the persona in the message is.
func TestAgyCompactionDecisionMeasuresThePromptThatIsSent(t *testing.T) {
	captureLog(t)
	resetAgyBriefCache(t)
	req := uniquePersonaDialogue("decision", syntheticPersona(130000), 20, 1000)
	in := agyGenInputFromAnthropic(req, 0)
	calls := stubPersonaResolve(t, constantReply("- the customer asked several questions"))
	base := config{AgyCompact: true, AgyPromptBudget: 186000}

	if _, did := agyCompactIfNeeded(context.Background(), base, in); !did || len(*calls) != 1 {
		t.Fatalf("control: the persona in the message should force a brief (did=%v, calls=%d)", did, len(*calls))
	}
	*calls = nil
	withAgent := base
	withAgent.agyPersonaAgent = "connect-chat-p-0123456789ab"
	if _, did := agyCompactIfNeeded(context.Background(), withAgent, in); did || len(*calls) != 0 {
		t.Fatalf("a dialogue that fits without the persona was summarised (did=%v, calls=%d)", did, len(*calls))
	}

	// And when it does compact under a persona agent, only the brief call is stripped of it.
	tight := withAgent
	tight.AgyPromptBudget = 30000
	small := agyGenInputFromAnthropic(uniquePersonaDialogue("decision-tight", "You are Nadia.", 20, 1000), 0)
	*calls = nil
	if _, did := agyCompactIfNeeded(context.Background(), tight, small); !did || len(*calls) != 1 {
		t.Fatalf("expected a brief (did=%v, calls=%d)", did, len(*calls))
	}
	if (*calls)[0].cfg.agyPersonaAgent != "" {
		t.Fatalf("the brief call carried the persona agent %q", (*calls)[0].cfg.agyPersonaAgent)
	}
}

// ---------------------------------------------------------------------------
// Configuration
// ---------------------------------------------------------------------------

func TestLoadConfigPersonaAgentSwitch(t *testing.T) {
	unset := func(key string) {
		t.Helper()
		t.Setenv(key, "") // registers the restore
		os.Unsetenv(key)
	}
	unset("PROXY_AGY_PERSONA_IN_AGENT")
	unset("PROXY_AGY_PERSONA_AGENT_PREFIX")

	cfg := loadConfig()
	if cfg.AgyPersonaInAgent {
		t.Fatal("the switch must be off when the variable is unset")
	}
	if cfg.AgyPersonaAgentPrefix != "connect-chat-p" {
		t.Fatalf("prefix = %q, want connect-chat-p", cfg.AgyPersonaAgentPrefix)
	}
	if cfg.agyPersonaAgent != "" {
		t.Fatalf("the per-request field = %q, want empty", cfg.agyPersonaAgent)
	}

	for value, want := range map[string]bool{"true": true, "1": true, "on": true, "YES": true, "false": false, "0": false, "off": false, "maybe": false, "": false} {
		t.Setenv("PROXY_AGY_PERSONA_IN_AGENT", value)
		if got := loadConfig().AgyPersonaInAgent; got != want {
			t.Errorf("PROXY_AGY_PERSONA_IN_AGENT=%q -> %v, want %v", value, got, want)
		}
	}

	t.Setenv("PROXY_AGY_PERSONA_AGENT_PREFIX", "  acme-persona ")
	if got := loadConfig().AgyPersonaAgentPrefix; got != "acme-persona" {
		t.Fatalf("prefix = %q, want acme-persona", got)
	}

	// The per-request field is never read from the environment.
	t.Setenv("PROXY_AGY_PERSONA_AGENT", "sneaky")
	t.Setenv("AGY_PERSONA_AGENT", "sneaky")
	if got := loadConfig().agyPersonaAgent; got != "" {
		t.Fatalf("the per-request field came from the environment: %q", got)
	}
}

func TestLoadConfigPersonaAgentNumbers(t *testing.T) {
	const key = "PROXY_AGY_PERSONA_IN_AGENT_NUMBERS"
	t.Setenv(key, "") // registers the restore
	os.Unsetenv(key)

	cfg := loadConfig()
	if len(cfg.AgyPersonaInAgentNumbers) != 0 || cfg.AgyPersonaInAgentAll {
		t.Fatalf("unset: numbers %v, all %v; want an empty list", cfg.AgyPersonaInAgentNumbers, cfg.AgyPersonaInAgentAll)
	}

	for _, c := range []struct {
		name, value string
		numbers     []string
		all         bool
	}{
		{"empty", "", nil, false},
		{"blanks and junk", " , ,abc, 00 ,+", nil, false},
		{"two numbers and a star", "+962790000001, 0790000002 ,*", []string{"962790000001", "790000002"}, true},
		{"star alone", " * ", nil, true},
		{"one number in three spellings", "+962790000001,00962790000001, 962-79-000-0001", []string{"962790000001"}, false},
		{"order kept", "0790000003,+962790000001", []string{"790000003", "962790000001"}, false},
	} {
		t.Setenv(key, c.value)
		got := loadConfig()
		if !reflect.DeepEqual(got.AgyPersonaInAgentNumbers, c.numbers) || got.AgyPersonaInAgentAll != c.all {
			t.Errorf("%s: %q -> numbers %v, all %v; want %v, %v", c.name, c.value, got.AgyPersonaInAgentNumbers, got.AgyPersonaInAgentAll, c.numbers, c.all)
		}
	}

	// The allowlist and the switch are independent settings.
	t.Setenv(key, personaTestNumber)
	t.Setenv("PROXY_AGY_PERSONA_IN_AGENT", "false")
	if cfg := loadConfig(); cfg.AgyPersonaInAgent || len(cfg.AgyPersonaInAgentNumbers) != 1 {
		t.Fatalf("switch %v, numbers %v", cfg.AgyPersonaInAgent, cfg.AgyPersonaInAgentNumbers)
	}
}

// ---------------------------------------------------------------------------
// The allowlist: whose conversation gets a persona agent
// ---------------------------------------------------------------------------

func TestAgyNormalizePhone(t *testing.T) {
	for in, want := range map[string]string{
		"+962 79-000-0001":  "962790000001",
		"00962790000001":    "962790000001",
		"+00962790000001":   "962790000001",
		"962790000001":      "962790000001",
		"(962) 79 000 0001": "962790000001",
		"0790000001":        "790000001",
		"790000001":         "790000001",
		"000123":            "123",
		"":                  "",
		"abc":               "",
		"+":                 "",
		"0":                 "",
		"00":                "",
		"0000":              "",
	} {
		if got := agyNormalizePhone(in); got != want {
			t.Errorf("agyNormalizePhone(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestAgyPhoneMatches(t *testing.T) {
	// The three spellings of one number are the same customer, whichever way round.
	var same []string
	for _, form := range personaTestForms {
		same = append(same, agyNormalizePhone(form))
	}
	if same[0] != "962790000001" || same[1] != "962790000001" || same[2] != "790000001" {
		t.Fatalf("fixture: normalised forms = %v", same)
	}
	for _, a := range same {
		for _, b := range same {
			if !agyPhoneMatches(a, b) {
				t.Errorf("agyPhoneMatches(%q, %q) = false, want true", a, b)
			}
		}
	}

	// A different number is a different customer, in every spelling.
	for _, other := range []string{agyNormalizePhone(personaTestOtherNumber), agyNormalizePhone("0790000002")} {
		for _, a := range same {
			if agyPhoneMatches(a, other) || agyPhoneMatches(other, a) {
				t.Errorf("%q matches the different number %q", a, other)
			}
		}
	}

	// A tail matches only from 8 digits: 7 digits are a fragment, not a number.
	long := agyNormalizePhone("+962791234567")
	if agyPhoneMatches(long, "1234567") || agyPhoneMatches("1234567", long) {
		t.Error("a 7-digit tail matched")
	}
	if !agyPhoneMatches(long, "91234567") || !agyPhoneMatches("91234567", long) {
		t.Error("an 8-digit tail did not match")
	}
	// It has to be the tail: the same digits from the middle are not the number.
	if agyPhoneMatches(long, "62791234") || agyPhoneMatches("96279123", long) {
		t.Error("digits from the middle or the head matched")
	}

	// Empty never matches, not even itself.
	for _, c := range [][2]string{{"", ""}, {"", long}, {long, ""}, {"", "1"}} {
		if agyPhoneMatches(c[0], c[1]) {
			t.Errorf("agyPhoneMatches(%q, %q) = true, want false", c[0], c[1])
		}
	}
}

func TestAgyConversationIdentifier(t *testing.T) {
	persona := syntheticPersona(2000)
	// Tool docs and examples inside a persona carry other numbers, some of them
	// even on a "User identifier:" line.
	docs := "\n\n## Tool docs\nlookup_customer(phone): e.g. lookup_customer(\"" + personaTestDocNumber + "\") returns the record of " + personaTestNumber + ".\n"
	section := "## Platform Context\nPlatform: whatsapp\nSpeaking with: Test Customer\nUser identifier: " + personaTestOtherNumber + "\n"

	for _, c := range []struct {
		name, system, want string
	}{
		{"platform context section", persona + "\n\n" + section, personaTestOtherNumber},
		{"section first, persona after", section + "\n" + persona + docs, personaTestOtherNumber},
		{"extra spaces and a CRLF", persona + "\n\n## Platform Context\r\nPlatform: whatsapp\r\n   User identifier:    +962 79-000-0002  \t\r\nNext: x\r\n", "+962 79-000-0002"},
		{"tail of the prompt without a newline", persona + "\n\n" + strings.TrimSuffix(section, "\n"), personaTestOtherNumber},
		{"deeper heading", persona + "\n\n### Platform Context\nUser identifier: 0790000002\n", "0790000002"},
		{"absent", persona + docs, ""},
		{"empty system", "", ""},
		{"numbers in tool docs are never the identifier", persona + docs + "\n## Platform Context\nPlatform: whatsapp\nSpeaking with: " + personaTestDocNumber + "\n", ""},
		{"empty value does not take the next line", persona + "\n\n## Platform Context\nUser identifier:\nSpeaking with: " + personaTestNumber + "\n", ""},
		{"blank value", persona + "\n\n## Platform Context\nUser identifier:   \nSpeaking with: " + personaTestNumber + "\n", ""},
		{"not at the start of a line", persona + "\n\n## Platform Context\nSee the User identifier: " + personaTestNumber + " line\n", ""},
		// The line after the heading wins over an example that came before it.
		{"an earlier example is not the customer", persona + "\nExample:\nUser identifier: " + personaTestNumber + "\n\n" + section, personaTestOtherNumber},
		{"heading with nothing after it", persona + "\nExample:\nUser identifier: " + personaTestNumber + "\n\n## Platform Context\nPlatform: whatsapp\n", ""},
		// No heading at all: the first such line anywhere.
		{"no heading", persona + "\nUser identifier: " + personaTestOtherNumber + "\n", personaTestOtherNumber},
		{"no heading, first of two", "User identifier: " + personaTestOtherNumber + "\nUser identifier: " + personaTestNumber + "\n", personaTestOtherNumber},
	} {
		if got := agyConversationIdentifier(c.system); got != c.want {
			t.Errorf("%s: identifier = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestAgyPersonaAgentForAllowlist(t *testing.T) {
	base := syntheticLongRequest(3000, 1, 500)
	prefix := agyPersonaAgentDefaultPrefix

	// path B: the agent is named after the system prompt and installed.
	wantAgent := func(t *testing.T, home string, cfg config, in agyGenInput) {
		t.Helper()
		got := agyPersonaAgentFor(cfg, in)
		want := agyPersonaAgentName(prefix, in.System)
		if got != want {
			t.Fatalf("persona agent = %q, want %q", got, want)
		}
		if def, err := os.ReadFile(personaAgentFile(home, got)); err != nil || string(def) != personaWantDefinition(got, in.System) {
			t.Fatalf("agent %s not installed with the persona: %v", got, err)
		}
	}
	// path A: no agent, nothing installed.
	wantNone := func(t *testing.T, home string, cfg config, in agyGenInput) {
		t.Helper()
		if got := agyPersonaAgentFor(cfg, in); got != "" {
			t.Fatalf("persona agent = %q, want none", got)
		}
		if _, err := os.Stat(personaAgentsDir(home)); err == nil {
			t.Fatal("a request on path A must not install a persona agent")
		}
	}

	t.Run("allowlisted, in each of the three spellings", func(t *testing.T) {
		home := personaTestHome(t)
		captureLog(t)
		for _, listed := range personaTestForms {
			cfg := personaTestAllowlist(personaTestOnConfig(), listed)
			for _, identifier := range personaTestForms {
				wantAgent(t, home, cfg, personaCustomerInput(base, identifier))
			}
		}
		// And the international form with the list holding a comma-separated mix.
		cfg := personaTestAllowlist(personaTestOnConfig(), personaTestOtherNumber+", 0790000009 ,"+personaTestNumber)
		wantAgent(t, home, cfg, personaCustomerInput(base, "00962790000001"))
	})

	t.Run("not on the list", func(t *testing.T) {
		home := personaTestHome(t)
		captureLog(t)
		for _, identifier := range []string{personaTestOtherNumber, "0790000002", "0000001", "790000", "90000001x2"} {
			wantNone(t, home, personaTestOnConfig(), personaCustomerInput(base, identifier))
		}
	})

	t.Run("empty list means no one", func(t *testing.T) {
		home := personaTestHome(t)
		captureLog(t)
		cfg := personaTestOnConfig()
		cfg.AgyPersonaInAgentNumbers = nil
		wantNone(t, home, cfg, personaCustomerInput(base, personaTestNumber))
		wantNone(t, home, personaTestAllowlist(cfg, ""), personaCustomerInput(base, personaTestNumber))
		wantNone(t, home, personaTestAllowlist(cfg, " , ,abc"), personaCustomerInput(base, personaTestNumber))
		wantNone(t, home, cfg, personaCustomerInput(base, ""))
	})

	t.Run("star lists everyone", func(t *testing.T) {
		home := personaTestHome(t)
		captureLog(t)
		cfg := personaTestAllowlist(personaTestOnConfig(), "*")
		if len(cfg.AgyPersonaInAgentNumbers) != 0 || !cfg.AgyPersonaInAgentAll {
			t.Fatalf("fixture: numbers %v, all %v", cfg.AgyPersonaInAgentNumbers, cfg.AgyPersonaInAgentAll)
		}
		wantAgent(t, home, cfg, personaCustomerInput(base, personaTestNumber))
		wantAgent(t, home, cfg, personaCustomerInput(base, personaTestOtherNumber))
		wantAgent(t, home, cfg, personaCustomerInput(base, "")) // not even an identifier line is needed
		// The other conditions still hold with a star: no persona, or attachments, is path A.
		x := personaCustomerInput(base, personaTestNumber)
		x.System = ""
		if got := agyPersonaAgentFor(cfg, x); got != "" {
			t.Fatalf("star with no persona: %q", got)
		}
		x = personaCustomerInput(base, personaTestNumber)
		x.Media = []mediaPart{{B64: tinyPNGBase64(t), MediaType: "image/png", Filename: "x.png"}}
		if got := agyPersonaAgentFor(cfg, x); got != "" {
			t.Fatalf("star with attachments: %q", got)
		}
		// And a switch that is off stays off, whatever the list says.
		cfg.AgyPersonaInAgent = false
		if got := agyPersonaAgentFor(cfg, personaCustomerInput(base, personaTestNumber)); got != "" {
			t.Fatalf("star with the switch off: %q", got)
		}
	})

	t.Run("no identifier line", func(t *testing.T) {
		home := personaTestHome(t)
		captureLog(t)
		wantNone(t, home, personaTestOnConfig(), personaCustomerInput(base, ""))
		// An identifier line with nothing to compare.
		wantNone(t, home, personaTestOnConfig(), personaCustomerInput(base, "n/a"))
		blank := personaCustomerInput(base, "")
		blank.System += "\n\n## Platform Context\nUser identifier:\n"
		wantNone(t, home, personaTestOnConfig(), blank)
	})

	t.Run("numbers inside the persona are never the customer", func(t *testing.T) {
		home := personaTestHome(t)
		captureLog(t)
		docs := "\n\n## Tool docs\nExample: lookup_customer(\"" + personaTestNumber + "\") and lookup_customer(\"" + personaTestDocNumber + "\"); the owner is 0790000001.\n"
		cfg := personaTestAllowlist(personaTestOnConfig(), personaTestNumber+","+personaTestDocNumber)

		// The docs mention the listed numbers, the customer is somebody else: path A.
		in := agyGenInputFromAnthropic(base, 0)
		in.System = personaTestSystem(in.System+docs, personaTestOtherNumber)
		wantNone(t, home, cfg, in)

		// A listed number in an earlier "User identifier:" example is no better.
		in = agyGenInputFromAnthropic(base, 0)
		in.System = personaTestSystem(in.System+docs+"\nUser identifier: "+personaTestNumber+"\n", personaTestOtherNumber)
		wantNone(t, home, cfg, in)

		// The customer is listed and the docs carry only other numbers: the customer decides.
		cfg = personaTestAllowlist(personaTestOnConfig(), personaTestNumber)
		in = agyGenInputFromAnthropic(base, 0)
		in.System = personaTestSystem(in.System+"\n\nExample: "+personaTestDocNumber+"\n", personaTestNumber)
		wantAgent(t, home, cfg, in)
	})
}

// A customer who is not on the allowlist gets exactly the request they get with
// the switch off: the same prompt byte for byte, the same agy arguments, no
// agent installed. Checked over the golden prompts' inputs.
func TestAgyPersonaNotAllowlistedRequestIsByteIdenticalToSwitchOff(t *testing.T) {
	home := personaTestHome(t)
	logs := captureLog(t)

	// What one agyGenerate call sends, and the agy arguments a real agyResolve
	// builds from the config it was given.
	type sent struct {
		call   personaResolveCall
		argv   []string
		prompt string
		lines  []string
	}
	run := func(t *testing.T, cfg config, in agyGenInput) sent {
		t.Helper()
		calls := stubPersonaResolve(t, constantReply("Everything is in order."))
		logs.Reset()
		if _, _, err := agyGenerate(context.Background(), cfg, in); err != nil {
			t.Fatal(err)
		}
		if len(*calls) != 1 {
			t.Fatalf("resolve called %d times, want 1", len(*calls))
		}
		s := sent{call: (*calls)[0], lines: personaLogLines(logs.String())}
		initAgyWorkerPool(config{AgyWarmWorkers: 0})
		rec := fakeAgyRunAgentFn(t)
		if _, err := agyResolve(context.Background(), s.call.cfg, nil, s.call.prompt, "some-alias"); err != nil {
			t.Fatal(err)
		}
		if !rec.Called {
			t.Fatal("agyResolve did not reach the runner")
		}
		s.argv, s.prompt = rec.AgentArgs, rec.Prompt
		return s
	}

	for _, c := range personaGoldenCases() {
		c := c
		t.Run(c.name, func(t *testing.T) {
			golden, err := os.ReadFile(goldenPath(c.name))
			if err != nil {
				t.Fatal(err)
			}
			off := c.cfg
			off.AgyAgent = agyChatAgentName
			on := personaTestAllowlist(off, personaTestNumber)
			on.AgyPersonaInAgent = true
			on.AgyPersonaAgentPrefix = agyPersonaAgentDefaultPrefix
			wantArgv := []string{"--agent", agyChatAgentName}

			// The golden inputs as they are: the switch off sends the golden prompt,
			// and so does the switch on when the request names no customer.
			plain := agyGenInputFromAnthropic(c.req, 0)
			offPlain := run(t, off, plain)
			onPlain := run(t, on, plain)
			for label, s := range map[string]sent{"switch off": offPlain, "switch on, no identifier": onPlain} {
				if s.call.prompt != string(golden) {
					t.Fatalf("%s: prompt differs from the golden\n%s", label, firstDiff(string(golden), s.call.prompt))
				}
				if s.call.cfg.agyPersonaAgent != "" || s.prompt != s.call.prompt || !reflect.DeepEqual(s.argv, wantArgv) {
					t.Fatalf("%s: persona agent %q, argv %v", label, s.call.cfg.agyPersonaAgent, s.argv)
				}
			}
			if len(offPlain.lines) != 0 || len(onPlain.lines) != 1 || !strings.Contains(onPlain.lines[0], "path=A reason=") {
				t.Fatalf("log lines: switch off %v, switch on %v", offPlain.lines, onPlain.lines)
			}

			// The same inputs for a customer who is named in the system prompt and is
			// not on the list: the switch on and the switch off send the same request.
			named := personaCustomerInput(c.req, personaTestOtherNumber)
			offNamed := run(t, off, named)
			onNamed := run(t, on, named)
			if onNamed.call.prompt != offNamed.call.prompt {
				t.Fatalf("not allowlisted: prompt differs from the switch-off prompt\n%s", firstDiff(offNamed.call.prompt, onNamed.call.prompt))
			}
			if onNamed.call.cfg.agyPersonaAgent != "" || onNamed.prompt != offNamed.prompt || !reflect.DeepEqual(onNamed.argv, offNamed.argv) || !reflect.DeepEqual(onNamed.argv, wantArgv) {
				t.Fatalf("not allowlisted: persona agent %q, argv %v (switch off %v)", onNamed.call.cfg.agyPersonaAgent, onNamed.argv, offNamed.argv)
			}
			if named.System != "" && !strings.Contains(onNamed.call.prompt, strings.TrimSpace(named.System)) {
				t.Fatal("not allowlisted: the persona is not in the message")
			}
			if len(onNamed.lines) != 1 || len(offNamed.lines) != 0 {
				t.Fatalf("log lines: switch on %v, switch off %v", onNamed.lines, offNamed.lines)
			}
			if _, err := os.Stat(personaAgentsDir(home)); err == nil {
				t.Fatal("a customer on path A must not get a persona agent installed")
			}

			// Control: the listed customer, same inputs, does get the agent and the pointer.
			if named.System == "" {
				return // a bare chat has no persona to move
			}
			listed := personaCustomerInput(c.req, personaTestForms[2])
			onListed := run(t, on, listed)
			if onListed.call.cfg.agyPersonaAgent == "" || onListed.call.prompt == offNamed.call.prompt {
				t.Fatal("control: the listed customer did not take path B")
			}
			if !strings.Contains(onListed.call.prompt, wantPersonaPointer) || strings.Contains(onListed.call.prompt, personaTestMarker) || strings.Contains(onListed.call.prompt, strings.TrimSpace(listed.System)) {
				t.Fatalf("control: the listed customer's message should carry the pointer, not the persona:\n%s", truncateString(onListed.call.prompt, 1500))
			}
			if want := []string{"--agent", onListed.call.cfg.agyPersonaAgent}; !reflect.DeepEqual(onListed.argv, want) {
				t.Fatalf("control: argv = %v, want %v", onListed.argv, want)
			}
			if len(onListed.lines) != 1 || !strings.Contains(onListed.lines[0], "path=B agent="+onListed.call.cfg.agyPersonaAgent+" ") {
				t.Fatalf("control: log lines %v", onListed.lines)
			}
			if err := os.RemoveAll(personaAgentsDir(home)); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestAgyPersonaAgentForLogsOneLinePerCall(t *testing.T) {
	home := personaTestHome(t)
	logs := captureLog(t)
	base := syntheticLongRequest(3000, 1, 500)
	png := []mediaPart{{B64: tinyPNGBase64(t), MediaType: "image/png", Filename: "x.png"}}
	tagOf := func(id string) string {
		sum := sha256.Sum256([]byte(id))
		return hex.EncodeToString(sum[:])[:8]
	}
	listedTag, otherTag := tagOf("962790000001"), tagOf("962790000002")
	on := personaTestOnConfig()
	nameOf := func(in agyGenInput) string { return agyPersonaAgentName(agyPersonaAgentDefaultPrefix, in.System) }

	listed := personaCustomerInput(base, personaTestForms[0])
	listedLocal := personaCustomerInput(base, personaTestForms[2])
	other := personaCustomerInput(base, personaTestOtherNumber)
	noID := personaCustomerInput(base, "")
	noDigits := personaCustomerInput(base, "n/a")
	withMedia := personaCustomerInput(base, personaTestNumber)
	withMedia.Media = png
	noPersona := personaCustomerInput(base, personaTestNumber)
	noPersona.System = ""
	noAgent := on
	noAgent.AgyAgent = ""
	star := personaTestAllowlist(on, "*")

	for _, c := range []struct {
		name string
		cfg  config
		in   agyGenInput
		want string
	}{
		{"listed", on, listed, "path=B agent=" + nameOf(listed) + " id#" + listedTag},
		{"listed, local spelling (the tag is of the normalised spelling it was given)", on, listedLocal, "path=B agent=" + nameOf(listedLocal) + " id#" + tagOf("790000001")},
		{"star", star, other, "path=B agent=" + nameOf(other) + " id#" + otherTag},
		{"star, no identifier", star, noID, "path=B agent=" + nameOf(noID) + " id#-"},
		{"not allowlisted", on, other, "path=A reason=not-allowlisted id#" + otherTag},
		{"empty list", personaTestAllowlist(on, ""), listed, "path=A reason=not-allowlisted id#" + listedTag},
		{"no identifier", on, noID, "path=A reason=no-identifier id#-"},
		{"identifier without digits", on, noDigits, "path=A reason=no-identifier id#-"},
		{"media", on, withMedia, "path=A reason=media id#" + listedTag},
		{"no persona", on, noPersona, "path=A reason=no-persona id#-"},
		{"no chat agent", noAgent, listed, "path=A reason=no-chat-agent id#" + listedTag},
	} {
		logs.Reset()
		agyPersonaAgentFor(c.cfg, c.in)
		lines := personaLogLines(logs.String())
		if len(lines) != 1 {
			t.Errorf("%s: %d [agy-persona] lines, want 1:\n%s", c.name, len(lines), logs.String())
			continue
		}
		if !strings.HasSuffix(lines[0], "[agy-persona] "+c.want) {
			t.Errorf("%s: log line %q, want it to end with %q", c.name, lines[0], "[agy-persona] "+c.want)
		}
		if !strings.Contains(lines[0], "path=") || !strings.Contains(lines[0], "id#") {
			t.Errorf("%s: the line lacks path= or id#: %q", c.name, lines[0])
		}
		// Neither the number (in any spelling) nor the persona is ever logged.
		for _, secret := range []string{"962790000001", "790000001", "962790000002", "790000002", "79-000-0001", personaTestMarker, "Rule 1.0", personaTestDocNumber} {
			if strings.Contains(logs.String(), secret) {
				t.Errorf("%s: the log contains %q:\n%s", c.name, secret, logs.String())
			}
		}
	}

	// An install that fails is path A, once, with its own reason.
	logs.Reset()
	fresh := t.TempDir()
	t.Setenv("HOME", fresh)
	t.Setenv("USERPROFILE", fresh)
	if err := os.WriteFile(filepath.Join(fresh, ".gemini"), []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := agyPersonaAgentFor(on, listed); got != "" {
		t.Fatalf("install failure: persona agent %q", got)
	}
	if lines := personaLogLines(logs.String()); len(lines) != 1 || !strings.HasSuffix(lines[0], "[agy-persona] path=A reason=install-error id#"+listedTag) {
		t.Errorf("install failure: log lines %v", lines)
	}
	if strings.Contains(logs.String(), "962790000001") || strings.Contains(logs.String(), personaTestMarker) {
		t.Errorf("install failure: the log contains the number or the persona:\n%s", logs.String())
	}
	_ = home

	// With the switch off nothing is logged at all, for any customer.
	off := on
	off.AgyPersonaInAgent = false
	for _, in := range []agyGenInput{listed, other, noID, withMedia, noPersona} {
		logs.Reset()
		if got := agyPersonaAgentFor(off, in); got != "" {
			t.Fatalf("switch off: persona agent %q", got)
		}
		if logs.Len() != 0 {
			t.Errorf("switch off: something was logged:\n%s", logs.String())
		}
	}
	off.AgyPersonaInAgentAll = true
	logs.Reset()
	if got := agyPersonaAgentFor(off, listed); got != "" || logs.Len() != 0 {
		t.Errorf("switch off with a star: persona agent %q, log %q", got, logs.String())
	}
}
