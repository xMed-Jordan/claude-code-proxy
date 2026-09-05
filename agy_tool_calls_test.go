package main

import (
	"strings"
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

func TestParseAgyToolCalls_DiscardsInternalTools(t *testing.T) {
	text := "<tool_call>\n{\n  \"tool\": \"run_command\",\n  \"input\": {\"CommandLine\": \"ls -la\"}\n}\n</tool_call>\nHello!"
	clean, items := parseAgyToolCalls(text)
	if len(items) != 0 {
		t.Fatalf("expected 0 items because run_command should be discarded, got %d", len(items))
	}
	if clean != "Hello!" {
		t.Errorf("expected 'Hello!', got %q", clean)
	}
}

func TestFlattenAnthropicToPrompt_Structuring(t *testing.T) {
	req := anthropicRequest{
		System: "You are a clinic AI assistant.",
		Messages: []anthropicMessage{
			{Role: "user", Content: "Hello, I want to book laser"},
			{Role: "assistant", Content: []any{
				map[string]any{
					"type": "tool_use",
					"id":   "toolu_123",
					"name": "get_services",
					"input": map[string]any{},
				},
			}},
			{Role: "user", Content: []any{
				map[string]any{
					"type":        "tool_result",
					"tool_use_id": "toolu_123",
					"content":     `[{"id": 1, "name": "Laser Face"}]`,
				},
			}},
			{Role: "user", Content: "Book face tomorrow at Irbid branch"},
		},
	}

	prompt := flattenAnthropicToPrompt(req)
	if !strings.Contains(prompt, "### SYSTEM INSTRUCTIONS & POLICIES") {
		t.Errorf("expected system section in prompt")
	}
	if !strings.Contains(prompt, "### CONVERSATION HISTORY") {
		t.Errorf("expected history section in prompt")
	}
	if !strings.Contains(prompt, "[Tool Result (toolu_123)]") {
		t.Errorf("expected structured tool result in prompt")
	}
	if !strings.Contains(prompt, "### CURRENT CUSTOMER MESSAGE & REQUIRED ACTION") {
		t.Errorf("expected current customer message section")
	}
	if !strings.Contains(prompt, "Customer: Book face tomorrow at Irbid branch") {
		t.Errorf("expected last customer message")
	}
	if !strings.Contains(prompt, "CRITICAL DIRECTIVE FOR ASSISTANT") {
		t.Errorf("expected critical directive")
	}
}

func TestParseAgyToolCalls_StripsSimulatedToolResult(t *testing.T) {
	text := "[Tool Result (call_K8q8Z51c6bZ8jY3WvF30rU6R)]:\n{\"name\":\"get_memory\",\"result\":{\"success\":true,\"data\":\"Customer name: Nancy\"}}\n\nHello Nancy, I see your package.\n<tool_call>\n{\"tool\": \"get_available_slots\", \"input\": {\"branch_id\": \"1\"}}\n</tool_call>"
	clean, items := parseAgyToolCalls(text)
	if len(items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(items))
	}
	if items[0].Name != "get_available_slots" {
		t.Errorf("expected get_available_slots, got %s", items[0].Name)
	}
	if strings.Contains(clean, "Tool Result") || strings.Contains(clean, "get_memory") {
		t.Errorf("simulated tool result should be stripped from clean text; got: %q", clean)
	}
	if clean != "Hello Nancy, I see your package." {
		t.Errorf("expected 'Hello Nancy, I see your package.', got: %q", clean)
	}
}

func TestFlattenAnthropicToPrompt_PreflightAndDuplicateCollapsing(t *testing.T) {
	req := anthropicRequest{
		System: "You are a clinic AI assistant.",
		Messages: []anthropicMessage{
			{Role: "user", Content: "احجز لي موعد فايزة"},
			// First call
			{Role: "assistant", Content: []any{
				map[string]any{
					"type":  "tool_use",
					"id":    "call_1",
					"name":  "get_tool_instructions",
					"input": map[string]any{"tool_code": "membership_protocol"},
				},
			}},
			{Role: "user", Content: []any{
				map[string]any{
					"type":        "tool_result",
					"tool_use_id": "call_1",
					"content":     `{"tool_code": "membership_protocol", "instructions": "Call membership_protocol FIRST"}`,
				},
			}},
			// Second identical call (duplicate loop)
			{Role: "assistant", Content: []any{
				map[string]any{
					"type":  "tool_use",
					"id":    "call_2",
					"name":  "get_tool_instructions",
					"input": map[string]any{"tool_code": "membership_protocol"},
				},
			}},
			{Role: "user", Content: []any{
				map[string]any{
					"type":        "tool_result",
					"tool_use_id": "call_2",
					"content":     `{"tool_code": "membership_protocol", "instructions": "Call membership_protocol FIRST"}`,
				},
			}},
		},
	}

	prompt := flattenAnthropicToPrompt(req)

	if !strings.Contains(prompt, "CRITICAL TOOL PREFLIGHT & PROTOCOL RULES:") {
		t.Errorf("expected CRITICAL TOOL PREFLIGHT & PROTOCOL RULES in prompt")
	}
	if !strings.Contains(prompt, "Instructions for the following tools have ALREADY been retrieved in this conversation: [membership_protocol]") {
		t.Errorf("expected membership_protocol in preflighted tools list")
	}
	if !strings.Contains(prompt, "NEVER call `get_tool_instructions` for any of these tools again!") {
		t.Errorf("expected never call get_tool_instructions instruction")
	}

	assistantCallCount := strings.Count(prompt, "Assistant: <tool_call>")
	if assistantCallCount != 1 {
		t.Errorf("expected duplicate assistant tool call to be collapsed to 1, got %d", assistantCallCount)
	}
}

func TestFlattenOpenAIChatToPrompt_PreflightAndDuplicateCollapsing(t *testing.T) {
	req := openAIRequest{
		Messages: []openAIMessage{
			{Role: "user", Content: "احجز لي موعد"},
			{
				Role: "assistant",
				ToolCalls: []openAIToolCall{
					{
						ID:   "call_1",
						Type: "function",
						Function: openAIToolFunction{
							Name:      "get_tool_instructions",
							Arguments: `{"tool_code": "membership_protocol"}`,
						},
					},
				},
			},
			{
				Role:       "tool",
				ToolCallID: "call_1",
				Content:    `{"tool_code": "membership_protocol", "instructions": "Call membership_protocol"}`,
			},
			{
				Role: "assistant",
				ToolCalls: []openAIToolCall{
					{
						ID:   "call_2",
						Type: "function",
						Function: openAIToolFunction{
							Name:      "get_tool_instructions",
							Arguments: `{"tool_code": "membership_protocol"}`,
						},
					},
				},
			},
			{
				Role:       "tool",
				ToolCallID: "call_2",
				Content:    `{"tool_code": "membership_protocol", "instructions": "Call membership_protocol"}`,
			},
		},
	}

	prompt := flattenOpenAIChatToPrompt(req)

	if !strings.Contains(prompt, "CRITICAL TOOL PREFLIGHT & PROTOCOL RULES:") {
		t.Errorf("expected CRITICAL TOOL PREFLIGHT & PROTOCOL RULES in prompt")
	}
	if !strings.Contains(prompt, "Instructions for the following tools have ALREADY been retrieved in this conversation: [membership_protocol]") {
		t.Errorf("expected membership_protocol in preflighted list")
	}

	assistantCallCount := strings.Count(prompt, "Assistant: <tool_call>")
	if assistantCallCount != 1 {
		t.Errorf("expected duplicate assistant tool call to be collapsed to 1, got %d", assistantCallCount)
	}
}

func TestFlattenResponsesToPrompt_PreflightAndDuplicateCollapsing(t *testing.T) {
	req := responsesRequest{
		Input: []any{
			map[string]any{"role": "user", "content": "احجز لي موعد"},
			map[string]any{
				"type":      "function_call",
				"name":      "get_tool_instructions",
				"arguments": `{"tool_code": "membership_protocol"}`,
			},
			map[string]any{
				"type":    "function_call_output",
				"call_id": "call_1",
				"output":  `{"tool_code": "membership_protocol", "instructions": "Call membership_protocol"}`,
			},
			map[string]any{
				"type":      "function_call",
				"name":      "get_tool_instructions",
				"arguments": `{"tool_code": "membership_protocol"}`,
			},
			map[string]any{
				"type":    "function_call_output",
				"call_id": "call_2",
				"output":  `{"tool_code": "membership_protocol", "instructions": "Call membership_protocol"}`,
			},
		},
	}

	prompt := flattenResponsesToPrompt(req)

	if !strings.Contains(prompt, "CRITICAL TOOL PREFLIGHT & PROTOCOL RULES:") {
		t.Errorf("expected CRITICAL TOOL PREFLIGHT & PROTOCOL RULES in prompt")
	}
	if !strings.Contains(prompt, "Instructions for the following tools have ALREADY been retrieved in this conversation: [membership_protocol]") {
		t.Errorf("expected membership_protocol in preflighted list")
	}

	assistantCallCount := strings.Count(prompt, "Assistant: <tool_call>")
	if assistantCallCount != 1 {
		t.Errorf("expected duplicate assistant tool call to be collapsed to 1, got %d", assistantCallCount)
	}
}


