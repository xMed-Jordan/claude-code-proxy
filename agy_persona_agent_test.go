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
	on := config{AgyPersonaInAgent: true, AgyAgent: agyChatAgentName, AgyPersonaAgentPrefix: agyPersonaAgentDefaultPrefix}
	in := agyGenInputFromAnthropic(syntheticLongRequest(3000, 1, 500), 0)

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
	cfg := config{AgyPersonaInAgent: true, AgyAgent: agyChatAgentName, AgyPersonaAgentPrefix: agyPersonaAgentDefaultPrefix, AgyPromptBudget: 186000}
	req := syntheticLongRequest(3000, 1, 500)
	in := agyGenInputFromAnthropic(req, 0)
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
	in := agyGenInputFromAnthropic(req, 0)
	cfg := config{AgyPersonaInAgent: true, AgyAgent: agyChatAgentName, AgyPersonaAgentPrefix: agyPersonaAgentDefaultPrefix, AgyPromptBudget: 186000}
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
	in := agyGenInputFromAnthropic(syntheticLongRequest(20000, 1, 500), 0)
	in.Media = []mediaPart{{B64: tinyPNGBase64(t), MediaType: "image/png", Filename: "x.png"}}
	cfg := config{AgyPersonaInAgent: true, AgyAgent: agyChatAgentName, AgyPersonaAgentPrefix: agyPersonaAgentDefaultPrefix}
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
	persona := syntheticPersona(3000)
	req := uniquePersonaDialogue("briefcall", persona, 20, 1000)
	in := agyGenInputFromAnthropic(req, 0)
	cfg := config{AgyPersonaInAgent: true, AgyAgent: agyChatAgentName, AgyPersonaAgentPrefix: agyPersonaAgentDefaultPrefix, AgyCompact: true, AgyPromptBudget: 30000}
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
