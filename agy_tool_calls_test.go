package main

import (
	"testing"
)

func TestParseAgyToolCalls_Single(t *testing.T) {
	text := "Here is some text before.\n<tool_call>\n{\n  \"tool\": \"get_clinic_info\",\n  \"input\": {}\n}\n</tool_call>\nAnd after."
	clean, items := parseAgyToolCalls(text)
	if len(items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(items))
	}
	if items[0].Name != "get_clinic_info" {
		t.Errorf("expected get_clinic_info, got %s", items[0].Name)
	}
	if items[0].Arguments != "{}" {
		t.Errorf("expected {}, got %s", items[0].Arguments)
	}
	if clean != "Here is some text before.\n\nAnd after." {
		t.Errorf("unexpected clean text: %q", clean)
	}
}

func TestParseAgyToolCalls_WithMarkdownFences(t *testing.T) {
	text := "<tool_call>\n```json\n{\n  \"tool\": \"find_user_by_phone\",\n  \"input\": {\"phone\": \"0799999999\"}\n}\n```\n</tool_call>"
	clean, items := parseAgyToolCalls(text)
	if len(items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(items))
	}
	if items[0].Name != "find_user_by_phone" {
		t.Errorf("expected find_user_by_phone, got %s", items[0].Name)
	}
	if clean != "" {
		t.Errorf("expected empty clean text, got %q", clean)
	}
}

func TestParseAgyToolCalls_Array(t *testing.T) {
	text := "<tool_call>\n[\n  {\"tool\": \"get_tool_instructions\", \"input\": {\"tool_code\": \"step1\"}},\n  {\"tool\": \"get_tool_instructions\", \"input\": {\"tool_code\": \"step2\"}}\n]\n</tool_call>"
	clean, items := parseAgyToolCalls(text)
	if len(items) != 2 {
		t.Fatalf("expected 2 items, got %d", len(items))
	}
	if items[0].Name != "get_tool_instructions" || items[1].Name != "get_tool_instructions" {
		t.Errorf("unexpected tool names: %s, %s", items[0].Name, items[1].Name)
	}
	if items[0].CallID == items[1].CallID {
		t.Errorf("expected unique CallIDs, got identical: %s", items[0].CallID)
	}
	if clean != "" {
		t.Errorf("expected empty clean text, got %q", clean)
	}
}

func TestParseAgyToolCalls_BracketAndFenced(t *testing.T) {
	text := "[TOOL_CALL]\n{\"name\": \"my_tool\", \"parameters\": {\"x\": 1}}\n[/TOOL_CALL]"
	_, items := parseAgyToolCalls(text)
	if len(items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(items))
	}
	if items[0].Name != "my_tool" {
		t.Errorf("expected my_tool, got %s", items[0].Name)
	}

	text2 := "```json\n{\"tool\": \"another_tool\", \"input\": {}}\n```"
	_, items2 := parseAgyToolCalls(text2)
	if len(items2) != 1 {
		t.Fatalf("expected 1 item, got %d", len(items2))
	}
	if items2[0].Name != "another_tool" {
		t.Errorf("expected another_tool, got %s", items2[0].Name)
	}
}

func TestParseAgyToolCalls_RawJSONFallback(t *testing.T) {
	text := "{\n  \"tool\": \"check_status\",\n  \"input\": {\"id\": 123}\n}"
	clean, items := parseAgyToolCalls(text)
	if len(items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(items))
	}
	if items[0].Name != "check_status" {
		t.Errorf("expected check_status, got %s", items[0].Name)
	}
	if clean != "" {
		t.Errorf("expected empty clean text, got %q", clean)
	}
}

func TestAgyModelFor_Remapping(t *testing.T) {
	cfg := config{
		Models: defaultModelAliasesFromValues(func(k, d string) string { return d }),
	}

	if got := agyModelFor(cfg, "gemini-3.5-flash-low"); got != "Gemini 3.6 Flash (Low)" {
		t.Errorf("expected Gemini 3.6 Flash (Low), got %s", got)
	}
	if got := agyModelFor(cfg, "gemini-3.5-flash-high"); got != "Gemini 3.6 Flash (High)" {
		t.Errorf("expected Gemini 3.6 Flash (High), got %s", got)
	}
	if got := agyModelFor(cfg, "gemini-3.6-flash-high"); got != "Gemini 3.6 Flash (High)" {
		t.Errorf("expected Gemini 3.6 Flash (High), got %s", got)
	}
	if got := agyModelFor(cfg, "gemini-3.5-flash"); got != "gemini-3.6-flash" {
		t.Errorf("expected gemini-3.6-flash, got %s", got)
	}
	if got := agyModelFor(cfg, "gemini-3.7-flash-high"); got != "Gemini 3.7 Flash (High)" {
		t.Errorf("expected Gemini 3.7 Flash (High), got %s", got)
	}
	if got := agyModelFor(cfg, "gemini-3.8-flash-high"); got != "Gemini 3.8 Flash (High)" {
		t.Errorf("expected Gemini 3.8 Flash (High), got %s", got)
	}
}

func TestForwardForAlias_GeminiDefaultsToAgy(t *testing.T) {
	cfg := config{}
	for _, alias := range []string{"gemini-3.6-flash-high", "gemini-3.7-flash-high", "gemini-3.8-flash-high", "antigravity-opus"} {
		if got := forwardForAlias(cfg, alias); got != "agy" {
			t.Errorf("expected %s to forward to agy, got %s", alias, got)
		}
	}
	if got := forwardForAlias(cfg, "claude-sonnet-4-6"); got != "codex" {
		t.Errorf("expected claude-sonnet-4-6 to forward to codex, got %s", got)
	}
}
