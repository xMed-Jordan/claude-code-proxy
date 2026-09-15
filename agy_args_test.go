package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The exact 422 Shalabi's booking API returned on 2026-09-09 (conv 58491) when
// the model invented a value for appointment_type, a parameter Connect's schema
// marks required and the tool's instructions say to omit.
const slots422 = `{"success":false,"data":{"status":422,"body":{"message":"The selected appointment type is invalid.","errors":{"appointment_type":["The selected appointment type is invalid."]}}},"error":"HTTP 422","status":422,"retryable":false}`

func slotsArgs(apptType string) string {
	return canonicalToolArgs(map[string]any{
		"appointment_type": apptType,
		"branch_id":        "1",
		"from_date":        "2026-09-10",
		"to_date":          "2026-09-10",
		"section_ids":      "9",
		"service_ids":      "185",
	})
}

// The turn that lost the booking: appointment_type="standard" was refused, and
// every later call carrying it was doomed. The value must be dropped so the
// call the customer is waiting on can succeed.
func TestAgyDropsAnArgumentValueTheAPIRefused(t *testing.T) {
	tr := agyTranscript{}
	tr.Turns = append(tr.Turns,
		agyTurn{Kind: agyTurnCustomer, Text: "بدي احجز بكرا جلستين تشقير حواجب"},
		agyTurn{Kind: agyTurnToolCall, Tool: "get_available_slots", CallID: "c1", Args: slotsArgs("standard")},
		agyTurn{Kind: agyTurnToolResult, Tool: "get_available_slots", CallID: "c1", Text: slots422},
	)
	rej := agyRejectedArgs(tr)
	if !rej.rejected("get_available_slots", "appointment_type", "standard") {
		t.Fatalf("the refusal was not learned from the tool's own error payload: %#v", rej)
	}
	got, dropped := agyStripRejectedArgs(rej, "get_available_slots", slotsArgs("standard"))
	if len(dropped) != 1 || dropped[0] != "appointment_type" {
		t.Fatalf("expected appointment_type to be dropped, got %v", dropped)
	}
	if strings.Contains(got, "appointment_type") {
		t.Fatalf("the refused parameter survived: %s", got)
	}
	// Everything the customer actually asked for must still be on the call.
	for _, keep := range []string{`"branch_id":"1"`, `"from_date":"2026-09-10"`, `"service_ids":"185"`, `"section_ids":"9"`} {
		if !strings.Contains(got, keep) {
			t.Fatalf("stripping removed real data %s: %s", keep, got)
		}
	}
}

// Only the refused value is refused. A retouch search still gets to say so.
func TestAgyKeepsValuesTheAPINeverRefused(t *testing.T) {
	tr := agyTranscript{}
	tr.Turns = append(tr.Turns,
		agyTurn{Kind: agyTurnToolCall, Tool: "get_available_slots", CallID: "c1", Args: slotsArgs("standard")},
		agyTurn{Kind: agyTurnToolResult, Tool: "get_available_slots", CallID: "c1", Text: slots422},
	)
	rej := agyRejectedArgs(tr)
	got, dropped := agyStripRejectedArgs(rej, "get_available_slots", slotsArgs("retouch"))
	if len(dropped) != 0 {
		t.Fatalf("a value the API never refused was dropped: %v", dropped)
	}
	if !strings.Contains(got, `"appointment_type":"retouch"`) {
		t.Fatalf("retouch must survive: %s", got)
	}
	// A different tool that happens to share the parameter name is untouched.
	other := canonicalToolArgs(map[string]any{"appointment_type": "standard"})
	if _, d := agyStripRejectedArgs(rej, "get_multi_service_slots", other); len(d) != 0 {
		t.Fatalf("the refusal leaked to another tool: %v", d)
	}
}

// A call that worked must never poison later calls, even when the result echoes
// the arguments back.
func TestAgySuccessfulCallsTeachNothing(t *testing.T) {
	tr := agyTranscript{}
	tr.Turns = append(tr.Turns,
		agyTurn{Kind: agyTurnToolCall, Tool: "get_available_slots", CallID: "c1", Args: slotsArgs("retouch")},
		agyTurn{Kind: agyTurnToolResult, Tool: "get_available_slots", CallID: "c1",
			Text: `{"success":true,"data":{"status":200,"body":{"success":true,"data":{"appointment_type":"retouch","duration_minutes":15}}}}`},
	)
	if rej := agyRejectedArgs(tr); len(rej) != 0 {
		t.Fatalf("a successful call was read as a refusal: %#v", rej)
	}
}

// Nothing in the guard knows this API, its tools or its field names: the keys
// come from the arguments the model itself sent.
func TestAgyRefusalReadingUsesNoFieldNames(t *testing.T) {
	tr := agyTranscript{}
	tr.Turns = append(tr.Turns,
		agyTurn{Kind: agyTurnToolCall, Tool: "create_shipment", CallID: "s1",
			Args: canonicalToolArgs(map[string]any{"carrier_code": "fastest", "weight_kg": 3, "destination": "AMM"})},
		agyTurn{Kind: agyTurnToolResult, Tool: "create_shipment", CallID: "s1",
			Text: `{"ok":false,"errors":{"carrier_code":"unknown carrier"}}`},
	)
	rej := agyRejectedArgs(tr)
	if !rej.rejected("create_shipment", "carrier_code", "fastest") {
		t.Fatalf("a different API's refusal was not understood: %#v", rej)
	}
	got, dropped := agyStripRejectedArgs(rej, "create_shipment",
		canonicalToolArgs(map[string]any{"carrier_code": "fastest", "weight_kg": 3, "destination": "AMM"}))
	if len(dropped) != 1 || dropped[0] != "carrier_code" {
		t.Fatalf("expected carrier_code dropped, got %v", dropped)
	}
	if !strings.Contains(got, `"destination":"AMM"`) || !strings.Contains(got, `"weight_kg":3`) {
		t.Fatalf("unrelated arguments were disturbed: %s", got)
	}
}

// Numbers and strings are the same value: duration 15 and "15" both count.
func TestAgyRefusalIgnoresArgumentEncoding(t *testing.T) {
	tr := agyTranscript{}
	tr.Turns = append(tr.Turns,
		agyTurn{Kind: agyTurnToolCall, Tool: "t", CallID: "c1", Args: `{"duration":15}`},
		agyTurn{Kind: agyTurnToolResult, Tool: "t", CallID: "c1", Text: `{"success":false,"errors":{"duration":["not allowed"]}}`},
	)
	rej := agyRejectedArgs(tr)
	if _, dropped := agyStripRejectedArgs(rej, "t", `{"duration":15}`); len(dropped) != 1 {
		t.Fatalf("the same value in the same encoding was not matched: %v", dropped)
	}
}

// End to end through agyGenerate: the model repeats the invented value, and the
// call that reaches Connect no longer carries it.
func TestAgyGenerateStripsTheInventedValueBeforeItReachesConnect(t *testing.T) {
	orig := agyResolveFn
	defer func() { agyResolveFn = orig }()
	agyResolveFn = func(_ context.Context, _ config, _ []mediaPart, _ string, _ string) (agyResult, error) {
		return agyResult{Ok: true, Response: "<tool_call>\n{\"tool\": \"get_available_slots\", \"input\": " + slotsArgs("standard") + "}\n</tool_call>"}, nil
	}
	tr := agyTranscript{}
	tr.Turns = append(tr.Turns,
		agyTurn{Kind: agyTurnCustomer, Text: "احجزي"},
		agyTurn{Kind: agyTurnToolCall, Tool: "get_available_slots", CallID: "c1", Args: slotsArgs("standard")},
		agyTurn{Kind: agyTurnToolResult, Tool: "get_available_slots", CallID: "c1", Text: slots422},
	)
	in := agyGenInput{
		System:     "أنت زينة، مساعدة رقمية في عيادات شلبي.",
		Tools:      []agyToolCatalog{{Name: "get_available_slots", Schema: map[string]any{"type": "object"}}},
		Transcript: tr,
	}
	resp, _, err := agyGenerate(context.Background(), config{}, in)
	if err != nil {
		t.Fatal(err)
	}
	var calls int
	for _, item := range resp.Output {
		if item.Type != "function_call" {
			continue
		}
		calls++
		if strings.Contains(item.Arguments, "appointment_type") {
			t.Fatalf("the refused value reached Connect: %s", item.Arguments)
		}
		if !strings.Contains(item.Arguments, `"service_ids":"185"`) {
			t.Fatalf("the real arguments were lost: %s", item.Arguments)
		}
	}
	if calls != 1 {
		t.Fatalf("expected the corrected call to go through, got %d call(s)", calls)
	}
}

// The catalog must tell the model what to do when a required parameter has no
// real value, since that is what made it invent one.
func TestAgyToolCatalogForbidsInventedValues(t *testing.T) {
	out := renderAgyToolCatalog([]agyToolCatalog{{Name: "get_available_slots", Description: "Get slots."}}, nil, false)
	for _, want := range []string{"send an empty string", "Never invent a value"} {
		if !strings.Contains(out, want) {
			t.Fatalf("tool-calling rules missing %q:\n%s", want, out)
		}
	}
}

// A system prompt and tool catalog that together leave less than the floor for
// the conversation used to skip fitting altogether and send the whole
// transcript unfitted — the exact case the floor clamp exists to rescue.
func TestAgyStillFitsWhenTheCallerLeavesNoRoom(t *testing.T) {
	big := buildPackagesResult(24, 12)
	in := anthropicRequest{
		Tools: []anthropicTool{{Name: "get_customer_packages", InputSchema: map[string]any{"type": "object"}}},
		Messages: []anthropicMessage{
			{Role: "user", Content: "شو الباقات عندي؟"},
			{Role: "assistant", Content: []any{map[string]any{"type": "tool_use", "id": "p1", "name": "get_customer_packages", "input": map[string]any{"customer_uuid": "X"}}}},
			{Role: "user", Content: []any{map[string]any{"type": "tool_result", "tool_use_id": "p1", "content": big}}},
			{Role: "user", Content: "طيب احجزيلي على باقة الجسم كامل"},
		},
	}
	tr := buildAgyTranscriptFromAnthropic(in)
	// Budgets from comfortable down to smaller than the fixed overhead itself.
	for _, budget := range []int{20000, 9000, 6000, 3000} {
		prompt, reduced := renderAgyPromptFitted(config{AgyPromptBudget: budget}, "You are Zeina.", nil, agyCatalogFromAnthropic(in.Tools), tr)
		if len(prompt) > 4*budget {
			t.Fatalf("budget %d produced an unfitted %d-byte prompt", budget, len(prompt))
		}
		if !reduced["get_customer_packages"] {
			t.Fatalf("budget %d shortened the result without reporting it: %v", budget, reduced)
		}
		if !strings.Contains(prompt, "طيب احجزيلي على باقة الجسم كامل") {
			t.Fatalf("budget %d lost the customer's own words", budget)
		}
	}
}

// loadConnectCatalog reads the 63 tool schemas Connect actually sends for the
// Shalabi agent, captured from getNativeToolSchemas on 2026-09-10.
func loadConnectCatalog(t *testing.T) []anthropicTool {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "connect_tools.json"))
	if err != nil {
		t.Fatal(err)
	}
	var tools []anthropicTool
	if err := json.Unmarshal(raw, &tools); err != nil {
		t.Fatal(err)
	}
	if len(tools) != 63 {
		t.Fatalf("expected the 63-tool catalog, got %d", len(tools))
	}
	return tools
}

func catalogByName(tools []agyToolCatalog, name string) (agyToolCatalog, bool) {
	for _, t := range tools {
		if t.Name == name {
			return t, true
		}
	}
	return agyToolCatalog{}, false
}

func schemaRequired(t *testing.T, schema any) []string {
	t.Helper()
	m, ok := schema.(map[string]any)
	if !ok {
		return nil
	}
	var out []string
	if reqs, ok := m["required"].([]any); ok {
		for _, r := range reqs {
			if s, ok := r.(string); ok {
				out = append(out, s)
			}
		}
	}
	return out
}

// v0.20.0 dropped the required list of every schema whose list named all of its
// parameters in boilerplate, reasoning that such a list states nothing. On the
// real catalog that included get_available_slots.service_ids, which the tool's
// instructions do require: over the five days it was live, the same model went
// from sending service_ids on 27/27 slot searches to 579/643, and a search
// without it is priced for the wrong session length, so the agent offered times
// that could not then be booked (conv 58846, 2026-09-12). The caller's schema
// must reach the model exactly as the caller wrote it.
func TestCallerSchemasReachTheModelUnchanged(t *testing.T) {
	raw := loadConnectCatalog(t)
	tools := agyCatalogFromAnthropic(raw)
	if len(tools) != len(raw) {
		t.Fatalf("catalog lost tools: %d -> %d", len(raw), len(tools))
	}
	for i, tool := range tools {
		props, required, ok := agySchemaParts(tool.Schema)
		if !ok {
			continue
		}
		wantProps, wantRequired, _ := agySchemaParts(raw[i].InputSchema)
		if len(props) != len(wantProps) || len(required) != len(wantRequired) {
			t.Fatalf("%s: schema altered (props %d->%d, required %d->%d)",
				tool.Name, len(wantProps), len(props), len(wantRequired), len(required))
		}
	}
	// The parameter the regression turned loose, spelled out.
	var slots agyToolCatalog
	for _, tool := range tools {
		if tool.Name == "get_available_slots" {
			slots = tool
		}
	}
	_, required, ok := agySchemaParts(slots.Schema)
	if !ok || !required["service_ids"] {
		t.Fatalf("service_ids must still reach the model as required: %v", required)
	}
	// And it must survive into the prompt the model actually reads.
	tr := agyTranscript{}
	tr.Turns = append(tr.Turns, agyTurn{Kind: agyTurnCustomer, Text: "بدي احجز فل بدي"})
	if p := renderAgyPrompt(config{}, "You are Zeina.", nil, tools, tr); !strings.Contains(p, "service_ids") {
		t.Fatal("service_ids is not in the rendered catalog")
	}
}

// The reply a patient received on 2026-09-13 (conv 58921): an English paragraph
// written to an imagined evaluator, glued in front of the correct Arabic answer.
const leakedPreamble = "It looks like there's an active automated workflow / benchmark prompt structure being passed here without direct tool access to the clinic systems (the available tools provided in the environment are restricted to background task management). If you are evaluating or testing the assistant turn, here is the direct, compliant response for Zeina:\n***\n" +
	"يا أهلا وسهلا! معك زينة مساعدتك الرقمية من Shalabi Clinics. عضوية Shalabi Pro بتغطي الجسم الكامل، وسعرها السنوي 180 دينار. بتحبي أحجزلك موعد؟"

func arabicTranscript() agyTranscript {
	tr := agyTranscript{}
	tr.Turns = append(tr.Turns,
		agyTurn{Kind: agyTurnCustomer, Text: "مرحبا حابة أعرف تفاصيل عضوية الليزر بلا حدود"},
		agyTurn{Kind: agyTurnCustomer, Text: "كم جلسة الديرما بن للوجه"},
	)
	return tr
}

func TestForeignPreambleIsCaughtWithoutKnowingThePhrases(t *testing.T) {
	tr := arabicTranscript()
	if !agyReplyHasForeignPreamble(tr, leakedPreamble) {
		t.Fatalf("the leak was not recognised by shape (longest latin run = %d)", agyLongestLatinRun(leakedPreamble))
	}
	// Ordinary replies to the same customer, including ones carrying English
	// brand names, device names and a URL, must pass.
	for _, ok := range []string{
		"يا أهلا وسهلا بتول! معك زينة مساعدتك الرقمية من Shalabi Clinics. كيف بقدر أساعدك؟",
		"موقعنا بفرع عمّان: الرابية - شارع صقلية. رابط الموقع: https://share.google/iVyVlotMOkDl9lzI4 بتحبي أثبتلك الموعد؟",
		"بفرع عمّان عنا جهازين: Candela GentleMax Pro (أمريكي) بيعتمد موجة Alexandrite، و Duetto MT Evo (إيطالي) بتقنية مزدوجة بتدمج موجتي Alexandrite و Nd:YAG، ومناسب لكل درجات البشرة.",
		"عضوية Shalabi Pro بتغطي الجسم الكامل 18 منطقة، وعندنا كمان Shalabi Light و Ultra و Max.",
	} {
		if agyReplyHasForeignPreamble(tr, ok) {
			t.Fatalf("false positive on an ordinary reply (latin run %d): %s", agyLongestLatinRun(ok), ok)
		}
	}
}

// A customer who writes in English gets English answers; the check stands down.
func TestForeignPreambleStandsDownForEnglishCustomers(t *testing.T) {
	tr := agyTranscript{}
	tr.Turns = append(tr.Turns, agyTurn{Kind: agyTurnCustomer,
		Text: "Hello, I would like to know the details of the unlimited laser membership and the Dermapen sessions please."})
	if agyReplyHasForeignPreamble(tr, leakedPreamble) {
		t.Fatal("an English-speaking customer must not make English replies suspicious")
	}
	// And with no customer text at all there is nothing to compare against.
	if agyReplyHasForeignPreamble(agyTranscript{}, leakedPreamble) {
		t.Fatal("an empty transcript must not trigger the check")
	}
}

// End to end: the leak is rejected and the retry produces a clean Arabic reply.
func TestAgyRejectsTheForeignPreambleAndRecovers(t *testing.T) {
	orig := agyResolveFn
	defer func() { agyResolveFn = orig }()
	var n int
	agyResolveFn = func(_ context.Context, _ config, _ []mediaPart, _ string, _ string) (agyResult, error) {
		n++
		if n == 1 {
			return agyResult{Ok: true, Response: leakedPreamble}, nil
		}
		return agyResult{Ok: true, Response: "يا أهلا وسهلا! عضوية Shalabi Pro سعرها السنوي 180 دينار. بتحبي أحجزلك موعد؟"}, nil
	}
	in := agyGenInput{
		System:     "أنت زينة، مساعدة رقمية في عيادات شلبي. جاوبي الزبونة بالعربي.",
		Tools:      []agyToolCatalog{{Name: "get_membership_plans", Schema: map[string]any{"type": "object"}}},
		Transcript: arabicTranscript(),
	}
	resp, _, err := agyGenerate(context.Background(), config{}, in)
	if err != nil {
		t.Fatal(err)
	}
	got := agyResponseText(resp)
	if strings.Contains(got, "benchmark") || strings.Contains(got, "evaluating") {
		t.Fatalf("the preamble reached the customer: %q", got)
	}
	if !strings.Contains(got, "180") {
		t.Fatalf("expected the corrected Arabic reply, got %q", got)
	}
}

// The phrase list must also know this family, so the leak is caught twice over.
func TestHarnessPhrasesCoverTheEvaluationFraming(t *testing.T) {
	for _, s := range []string{
		"It looks like there's an active automated workflow / benchmark prompt structure being passed here",
		"without direct tool access to the clinic systems",
		"the available tools provided in the environment are restricted",
		"If you are evaluating or testing the assistant turn",
	} {
		if !agyHarnessLeakRe.MatchString(s) {
			t.Fatalf("not detected as runtime talk: %q", s)
		}
	}
	for _, s := range []string{
		"أهلاً فيكِ، كيف بقدر أساعدك؟",
		"Your appointment is confirmed for Thursday at 1 PM.",
		"سعر جلسة الفل بدي 55 دينار.",
	} {
		if agyHarnessLeakRe.MatchString(s) {
			t.Fatalf("false positive on an ordinary reply: %q", s)
		}
	}
}
