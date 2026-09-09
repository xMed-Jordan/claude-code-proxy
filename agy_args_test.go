package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
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

// On the real catalog the repair must fire on the generated schemas and leave
// the hand-written ones exactly as they came.
func TestSchemaRepairOnTheRealConnectCatalog(t *testing.T) {
	tools := agyRepairToolSchemas(agyCatalogFromAnthropic(loadConnectCatalog(t)))

	// Generated: every parameter required, every description the same template.
	for _, name := range []string{
		"get_available_slots", "create_reservation", "create_multi_service_reservation",
		"purchase_membership", "upgrade_membership", "create_customer", "add_item_to_cart",
	} {
		tool, ok := catalogByName(tools, name)
		if !ok {
			t.Fatalf("%s missing from the catalog", name)
		}
		if req := schemaRequired(t, tool.Schema); len(req) != 0 {
			t.Fatalf("%s kept a generated required list: %v", name, req)
		}
		// The parameters themselves must all survive — only the claim about
		// them is dropped.
		props, _, ok := agySchemaParts(tool.Schema)
		if !ok {
			t.Fatalf("%s lost its properties", name)
		}
		if name == "get_available_slots" {
			for _, p := range []string{"from_date", "to_date", "branch_id", "service_ids", "appointment_type", "duration", "room_id"} {
				if _, has := props[p]; !has {
					t.Fatalf("get_available_slots lost parameter %s", p)
				}
			}
		}
	}

	// Hand-written: descriptions written for this tool, so the requirement is real.
	for _, tc := range []struct {
		name string
		want []string
	}{
		{"save_memory", []string{"memory_type", "content"}},
		{"delete_memory", []string{"memory_type", "search"}},
		{"get_memory", []string{"memory_type"}},
		{"get_tool_instructions", []string{"tool_code"}},
		{"send_media", []string{"items"}},
	} {
		tool, ok := catalogByName(tools, tc.name)
		if !ok {
			t.Fatalf("%s missing from the catalog", tc.name)
		}
		got := schemaRequired(t, tool.Schema)
		sort.Strings(got)
		want := append([]string(nil), tc.want...)
		sort.Strings(want)
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("%s: a hand-written requirement was dropped: got %v want %v", tc.name, got, want)
		}
	}
}

// The invented value in production came from appointment_type being presented
// as required. After the repair the catalog the model reads must not say so,
// while still listing the parameter as available.
func TestRepairedCatalogNoLongerDemandsTheInventedParameter(t *testing.T) {
	tools := agyRepairToolSchemas(agyCatalogFromAnthropic(loadConnectCatalog(t)))
	full := renderAgyToolCatalog(tools, nil, false)
	if !strings.Contains(full, "appointment_type") {
		t.Fatal("the parameter must still be offered")
	}
	if strings.Contains(full, `"required":["from_date"`) || strings.Contains(full, `"appointment_type","retouch_reservation_id"`) {
		t.Fatalf("the generated required list survived into the prompt")
	}
	// Compact mode marks required parameters with a star; the generated ones
	// must no longer carry it, the hand-written ones must.
	compact := renderAgyToolCatalog(tools, map[string]bool{}, true)
	for _, line := range strings.Split(compact, "\n") {
		if strings.HasPrefix(line, "- get_available_slots ") && strings.Contains(line, "*") {
			t.Fatalf("generated parameters still marked required: %s", line)
		}
		if strings.HasPrefix(line, "- save_memory ") && !strings.Contains(line, "*") {
			t.Fatalf("a real requirement lost its marker: %s", line)
		}
	}
}

// The rule is about the shape of the catalog, not about Connect: a catalog of
// hand-written schemas is never touched, however many parameters are required.
func TestSchemaRepairLeavesHandWrittenCatalogsAlone(t *testing.T) {
	tools := agyRepairToolSchemas(agyCatalogFromAnthropic([]anthropicTool{
		{Name: "send_email", InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"to":      map[string]any{"type": "string", "description": "Recipient address."},
				"subject": map[string]any{"type": "string", "description": "Subject line, kept under 80 characters."},
			},
			"required": []any{"to", "subject"},
		}},
		{Name: "create_ticket", InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"title":    map[string]any{"type": "string", "description": "One-line summary of the problem."},
				"severity": map[string]any{"type": "string", "description": "One of sev1, sev2, sev3."},
			},
			"required": []any{"title", "severity"},
		}},
	}))
	for _, tool := range tools {
		if len(schemaRequired(t, tool.Schema)) != 2 {
			t.Fatalf("%s: a hand-written required list was dropped", tool.Name)
		}
	}
}

// A generator's fingerprint is the wording repeating across tools, not any
// particular words: the same shape in another vocabulary is repaired too.
func TestSchemaRepairRecognisesAnyGenerator(t *testing.T) {
	mk := func(name string, params ...string) anthropicTool {
		props := map[string]any{}
		var req []any
		for _, p := range params {
			props[p] = map[string]any{"type": "string", "description": "Champ " + p + " de la requête"}
			req = append(req, p)
		}
		return anthropicTool{Name: name, InputSchema: map[string]any{"type": "object", "properties": props, "required": req}}
	}
	tools := agyRepairToolSchemas(agyCatalogFromAnthropic([]anthropicTool{
		mk("reserver_creneau", "date", "duree", "salle"),
		mk("annuler_creneau", "reservation_id"),
	}))
	for _, tool := range tools {
		if req := schemaRequired(t, tool.Schema); len(req) != 0 {
			t.Fatalf("%s: a generated list in another language survived: %v", tool.Name, req)
		}
		if props, _, ok := agySchemaParts(tool.Schema); !ok || len(props) == 0 {
			t.Fatalf("%s lost its parameters", tool.Name)
		}
	}
}

// The repair is on by default and PROXY_AGY_SCHEMA_REPAIR=false turns it off,
// which is also how the live A/B below was run.
func TestSchemaRepairCanBeSwitchedOff(t *testing.T) {
	tools := agyCatalogFromAnthropic(loadConnectCatalog(t))
	tr := agyTranscript{}
	tr.Turns = append(tr.Turns, agyTurn{Kind: agyTurnCustomer, Text: "شو المتاح بكرا؟"})

	on := renderAgyPrompt(config{}, "You are Zeina.", nil, tools, tr)
	off := renderAgyPrompt(config{AgySchemaRepairOff: true}, "You are Zeina.", nil, tools, tr)

	if strings.Contains(on, `"appointment_type","retouch_reservation_id"`) {
		t.Fatal("the generated required list survived with the repair on")
	}
	if !strings.Contains(off, `"appointment_type","retouch_reservation_id"`) {
		t.Fatal("switching the repair off must present the caller's schema unchanged")
	}
	if len(on) >= len(off) {
		t.Fatalf("the repair should also shorten the catalog: on=%d off=%d", len(on), len(off))
	}
}
