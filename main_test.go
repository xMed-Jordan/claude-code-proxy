package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func TestAnthropicResponsesTextPreservesRequestedModel(t *testing.T) {
	resp := responsesResponse{
		ID:     "resp_test",
		Model:  "gpt-5.5",
		Output: []responsesOutputItem{{Type: "message", Role: "assistant", Content: []responsesOutputContent{{Type: "output_text", Text: "Hi!"}}}},
	}
	out := toAnthropicResponsesResponse(resp, "opus[1m]")
	if out["model"] != "opus[1m]" {
		t.Fatalf("model = %v, want opus[1m]", out["model"])
	}
	content, ok := out["content"].([]any)
	if !ok || len(content) != 1 {
		t.Fatalf("content = %#v, want one block", out["content"])
	}
	block := content[0].(map[string]any)
	if block["type"] != "text" || block["text"] != "Hi!" {
		t.Fatalf("block = %#v, want text Hi!", block)
	}
}

func TestResponsesFunctionCallConvertsToAnthropicToolUse(t *testing.T) {
	resp := responsesResponse{
		ID:     "resp_test",
		Model:  "gpt-5.5",
		Output: []responsesOutputItem{{Type: "function_call", ID: "fc_1", CallID: "call_1", Name: "read_file", Arguments: `{"path":"C:\\tmp\\x.txt"}`}},
	}
	out := toAnthropicResponsesResponse(resp, "opus[1m]")
	if out["stop_reason"] != "tool_use" {
		t.Fatalf("stop_reason = %v, want tool_use", out["stop_reason"])
	}
	content := out["content"].([]any)
	var block map[string]any
	for _, item := range content {
		candidate := item.(map[string]any)
		if candidate["type"] == "tool_use" {
			block = candidate
			break
		}
	}
	if block == nil {
		t.Fatalf("tool_use block missing: %#v", content)
	}
	if block["type"] != "tool_use" || block["id"] != "call_1" || block["name"] != "read_file" {
		t.Fatalf("tool block = %#v", block)
	}
	input := block["input"].(map[string]any)
	if input["path"] != `C:\tmp\x.txt` {
		t.Fatalf("input path = %v", input["path"])
	}
}

func TestBufferedAnthropicStreamOrderAndDone(t *testing.T) {
	resp := responsesResponse{
		ID:     "resp_test",
		Model:  "gpt-5.5",
		Output: []responsesOutputItem{{Type: "message", Role: "assistant", Content: []responsesOutputContent{{Type: "output_text", Text: "Hi!"}}}},
	}
	var buf bytes.Buffer
	sendAnthropicMessageStart(&buf, "msg_test", "opus[1m]", 123, 0)
	writeAnthropicBufferedStream(context.Background(), &buf, resp)
	buf.WriteString("data: [DONE]\n\n")
	raw := buf.String()
	order := []string{"event: message_start", "event: content_block_start", "event: content_block_delta", "event: content_block_stop", "event: message_delta", "event: message_stop", "data: [DONE]"}
	last := -1
	for _, want := range order {
		idx := strings.Index(raw, want)
		if idx <= last {
			t.Fatalf("%q order invalid in stream:\n%s", want, raw)
		}
		last = idx
	}
	if !strings.Contains(raw, `"input_tokens":123`) {
		t.Fatalf("message_start did not include input token usage:\n%s", raw)
	}
}

func TestAnthropicErrorTypes(t *testing.T) {
	cases := map[int]string{
		http.StatusUnauthorized:     "authentication_error",
		http.StatusForbidden:        "authentication_error",
		http.StatusBadRequest:       "invalid_request_error",
		http.StatusMethodNotAllowed: "invalid_request_error",
		http.StatusBadGateway:       "api_error",
	}
	for code, want := range cases {
		if got := anthropicErrorType(code); got != want {
			t.Fatalf("anthropicErrorType(%d) = %s, want %s", code, got, want)
		}
	}
}

func TestCollectCodexStreamKeepsFinalFunctionCall(t *testing.T) {
	final := responsesResponse{
		ID:     "resp_test",
		Model:  "gpt-5.5",
		Output: []responsesOutputItem{{Type: "function_call", ID: "fc_1", CallID: "call_1", Name: "list_dir", Arguments: `{"path":"."}`}},
	}
	payload, err := json.Marshal(responsesStreamEvent{Type: "response.completed", Response: final})
	if err != nil {
		t.Fatal(err)
	}
	resp := collectCodexStream(context.Background(), strings.NewReader("data: "+string(payload)+"\n\ndata: [DONE]\n\n"), "gpt-5.5")
	if len(resp.Output) != 1 || resp.Output[0].Type != "function_call" || resp.Output[0].Name != "list_dir" {
		t.Fatalf("output = %#v", resp.Output)
	}
}

func TestToolResultInputOmitsMessageRole(t *testing.T) {
	items := convertAnthropicMessageToResponses("user", []any{
		map[string]any{"type": "tool_result", "tool_use_id": "call_1", "content": "done"},
	})
	if len(items) != 1 {
		t.Fatalf("items = %#v", items)
	}
	raw, err := json.Marshal(items[0])
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), `"role"`) {
		t.Fatalf("tool result serialized role field: %s", raw)
	}
	if !strings.Contains(string(raw), `"type":"function_call_output"`) {
		t.Fatalf("tool result did not serialize function_call_output: %s", raw)
	}
}

func TestCollectCodexStreamHandlesLargeSSELines(t *testing.T) {
	largeText := strings.Repeat("x", 128*1024)
	event := responsesStreamEvent{Type: "response.output_text.delta", Delta: largeText}
	payload, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	resp := collectCodexStream(context.Background(), strings.NewReader("data: "+string(payload)+"\n\ndata: [DONE]\n\n"), "gpt-5.5")
	if got := responsesText(resp); got != largeText {
		t.Fatalf("text length = %d, want %d", len(got), len(largeText))
	}
}

func TestClaudeWebSearchToolMapsToCodexBuiltInSearch(t *testing.T) {
	t.Setenv("CODEX_WEB_SEARCH_TOOL_TYPE", "web_search")
	cfg := config{Models: map[string]string{"opus[1m]": "gpt-5.5"}}
	out := toResponses(cfg, anthropicRequest{
		Model:    "opus[1m]",
		Messages: []anthropicMessage{{Role: "user", Content: "Search the web."}},
		Tools: []anthropicTool{
			{Name: "WebSearch"},
			{Name: "PowerShell", InputSchema: map[string]any{"type": "object"}},
		},
	})
	if len(out.Tools) != 2 {
		t.Fatalf("tools = %#v, want web_search plus PowerShell", out.Tools)
	}
	if out.Tools[0].Type != "web_search" || out.Tools[0].Name != "" {
		t.Fatalf("first tool = %#v, want built-in web_search without function name", out.Tools[0])
	}
	if out.Tools[1].Type != "function" || out.Tools[1].Name != "PowerShell" {
		t.Fatalf("second tool = %#v, want PowerShell function", out.Tools[1])
	}
}

func TestDuplicateClaudeWebSearchToolsCollapse(t *testing.T) {
	cfg := config{Models: map[string]string{"opus[1m]": "gpt-5.5"}}
	out := toResponses(cfg, anthropicRequest{
		Model:    "opus[1m]",
		Messages: []anthropicMessage{{Role: "user", Content: "Search the web."}},
		Tools: []anthropicTool{
			{Name: "WebSearch"},
			{Name: "mcp__web-search__web_search"},
			{Name: "WebFetch"},
		},
	})
	webSearchTools := 0
	for _, tool := range out.Tools {
		if strings.HasPrefix(tool.Type, "web_search") {
			webSearchTools++
		}
		if tool.Name == "WebSearch" || tool.Name == "mcp__web-search__web_search" {
			t.Fatalf("web search leaked as function: %#v", tool)
		}
	}
	if webSearchTools != 1 {
		t.Fatalf("webSearchTools = %d, want 1", webSearchTools)
	}
}

func TestFastModeMapsClaudeOpus46ToCodexPriority(t *testing.T) {
	t.Setenv("CODEX_FAST_SERVICE_TIER", "priority")
	cfg := config{Models: map[string]string{"claude-opus-4-6": "gpt-5.5"}}
	out := toResponses(cfg, anthropicRequest{
		Model:    "claude-opus-4-6",
		FastMode: true,
		Messages: []anthropicMessage{{Role: "user", Content: "hi"}},
	})
	if out.Model != "gpt-5.5" {
		t.Fatalf("model = %q, want gpt-5.5", out.Model)
	}
	if out.ServiceTier != "priority" {
		t.Fatalf("service_tier = %q, want priority", out.ServiceTier)
	}
}

func TestHistoricalWebSearchBlocksBecomePlainMessages(t *testing.T) {
	items := convertAnthropicMessageToResponses("assistant", []any{
		map[string]any{"type": "tool_use", "id": "call_1", "name": "WebSearch", "input": map[string]any{"query": "Shalabi Clinic official website"}},
	})
	raw, err := json.Marshal(items)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "function_call") || strings.Contains(string(raw), `"name":"WebSearch"`) {
		t.Fatalf("historical WebSearch should not remain a function call: %s", raw)
	}

	items = convertAnthropicMessageToResponses("user", []any{
		map[string]any{"type": "tool_result", "tool_use_id": "call_1", "content": "Web search results for query: \"x\"\n\nResult"},
	})
	raw, err = json.Marshal(items)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "function_call_output") {
		t.Fatalf("historical WebSearch result should not remain a function output: %s", raw)
	}
}

func TestReasoningSummaryConvertsToAnthropicThinkingBlock(t *testing.T) {
	resp := responsesResponse{
		ID: "resp_test",
		Output: []responsesOutputItem{
			{Type: "reasoning", ID: "rs_1", Summary: []responsesReasoningPart{{Type: "summary_text", Text: "Checked the likely fix."}}},
			{Type: "message", Role: "assistant", Content: []responsesOutputContent{{Type: "output_text", Text: "Done."}}},
		},
	}
	out := toAnthropicResponsesResponse(resp, "opus[1m]")
	content := out["content"].([]any)
	if len(content) != 2 {
		t.Fatalf("content = %#v, want thinking and text", content)
	}
	thinking := content[0].(map[string]any)
	if thinking["type"] != "thinking" || thinking["thinking"] != "Checked the likely fix." {
		t.Fatalf("thinking block = %#v", thinking)
	}
	if thinking["signature"] == "" {
		t.Fatalf("thinking signature missing: %#v", thinking)
	}
}

func TestBufferedStreamWritesThinkingEventsBeforeText(t *testing.T) {
	resp := responsesResponse{
		Output: []responsesOutputItem{
			{Type: "reasoning", ID: "rs_1", Summary: []responsesReasoningPart{{Type: "summary_text", Text: "Reasoning summary."}}},
			{Type: "message", Role: "assistant", Content: []responsesOutputContent{{Type: "output_text", Text: "Answer."}}},
		},
	}
	var buf bytes.Buffer
	writeAnthropicBufferedStream(context.Background(), &buf, resp)
	raw := buf.String()
	order := []string{`"type":"thinking"`, `"type":"thinking_delta"`, `"type":"signature_delta"`, `"type":"text_delta"`}
	last := -1
	for _, want := range order {
		idx := strings.Index(raw, want)
		if idx <= last {
			t.Fatalf("stream order missing or wrong for %q in %s", want, raw)
		}
		last = idx
	}
}

func TestFunctionCallAddsToolActivityThinkingBlock(t *testing.T) {
	t.Setenv("CLAUDE_TOOL_ACTIVITY_THINKING", "1")
	resp := responsesResponse{
		ID: "resp_test",
		Output: []responsesOutputItem{{
			Type:      "function_call",
			ID:        "fc_1",
			CallID:    "call_1",
			Name:      "Wolfram Search",
			Arguments: `{"query":"integral of sin x","api_key":"secret-value"}`,
		}},
	}
	out := toAnthropicResponsesResponse(resp, "opus[1m]")
	content := out["content"].([]any)
	if len(content) != 2 {
		t.Fatalf("content = %#v, want thinking and tool_use", content)
	}
	thinking := content[0].(map[string]any)
	if thinking["type"] != "thinking" {
		t.Fatalf("first block = %#v, want thinking", thinking)
	}
	text := fmt.Sprint(thinking["thinking"])
	if !strings.Contains(text, "Wolfram Search") || !strings.Contains(text, "integral of sin x") {
		t.Fatalf("thinking text missing tool details: %q", text)
	}
	if strings.Contains(text, "secret-value") || !strings.Contains(text, "[redacted]") {
		t.Fatalf("thinking text did not redact sensitive field: %q", text)
	}
	if content[1].(map[string]any)["type"] != "tool_use" {
		t.Fatalf("second block = %#v, want tool_use", content[1])
	}
}

func TestCollectCodexStreamCapturesReasoningSummary(t *testing.T) {
	events := []responsesStreamEvent{
		{Type: "response.reasoning_summary_text.delta", ItemID: "rs_1", OutputIndex: 0, SummaryIndex: 0, Delta: "Checked "},
		{Type: "response.reasoning_summary_text.done", ItemID: "rs_1", OutputIndex: 0, SummaryIndex: 0, Text: "Checked the fix."},
		{Type: "response.output_text.delta", ItemID: "msg_1", OutputIndex: 1, Delta: "Done."},
	}
	var raw strings.Builder
	for _, event := range events {
		payload, err := json.Marshal(event)
		if err != nil {
			t.Fatal(err)
		}
		raw.WriteString("data: " + string(payload) + "\n\n")
	}
	raw.WriteString("data: [DONE]\n\n")
	resp := collectCodexStream(context.Background(), strings.NewReader(raw.String()), "gpt-5.5")
	if len(resp.Output) < 2 {
		t.Fatalf("output = %#v", resp.Output)
	}
	if got := reasoningSummaryText(resp.Output[0]); got != "Checked the fix." {
		t.Fatalf("reasoning summary = %q, want Checked the fix.", got)
	}
}

func TestCodexSessionKeyStablePerClaudeFlow(t *testing.T) {
	t.Setenv("CODEX_SESSION_ISOLATION", "1")
	t.Setenv("CODEX_PROMPT_CACHE_KEY", "1")
	cfg := config{CodexSessionFile: filepath.Join(t.TempDir(), "sessions.json")}
	in := anthropicRequest{Messages: []anthropicMessage{{Role: "user", Content: "hi"}}}
	body := []byte(`{"messages":[{"role":"user","content":"hi"}]}`)

	first := responsesRequest{}
	firstReq := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	firstReq.Header.Set("X-Claude-Code-Session-Id", "flow-a")
	firstInfo := configureCodexSession(cfg, firstReq, body, in, &first)

	second := responsesRequest{}
	secondReq := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	secondReq.Header.Set("X-Claude-Code-Session-Id", "flow-a")
	secondInfo := configureCodexSession(cfg, secondReq, body, in, &second)

	if first.PromptCacheKey == "" {
		t.Fatal("prompt_cache_key was not assigned")
	}
	if first.PromptCacheKey != second.PromptCacheKey {
		t.Fatalf("prompt cache keys differ: %q vs %q", first.PromptCacheKey, second.PromptCacheKey)
	}
	if firstInfo.FlowHash != secondInfo.FlowHash || firstInfo.CodexSessionID != secondInfo.CodexSessionID {
		t.Fatalf("session info did not stay stable: %#v %#v", firstInfo, secondInfo)
	}
}

func TestCodexSessionKeyDifferentPerClaudeFlow(t *testing.T) {
	t.Setenv("CODEX_SESSION_ISOLATION", "1")
	t.Setenv("CODEX_PROMPT_CACHE_KEY", "1")
	cfg := config{CodexSessionFile: filepath.Join(t.TempDir(), "sessions.json")}
	in := anthropicRequest{Messages: []anthropicMessage{{Role: "user", Content: "hi"}}}
	body := []byte(`{"messages":[{"role":"user","content":"hi"}]}`)

	first := responsesRequest{}
	firstReq := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	firstReq.Header.Set("X-Claude-Code-Session-Id", "flow-a")
	configureCodexSession(cfg, firstReq, body, in, &first)

	second := responsesRequest{}
	secondReq := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	secondReq.Header.Set("X-Claude-Code-Session-Id", "flow-b")
	configureCodexSession(cfg, secondReq, body, in, &second)

	if first.PromptCacheKey == second.PromptCacheKey {
		t.Fatalf("different flows shared prompt_cache_key %q", first.PromptCacheKey)
	}
}

func TestCodexSideThreadGetsChildSession(t *testing.T) {
	t.Setenv("CODEX_SESSION_ISOLATION", "1")
	t.Setenv("CODEX_PROMPT_CACHE_KEY", "1")
	cfg := config{CodexSessionFile: filepath.Join(t.TempDir(), "sessions.json")}
	normal := anthropicRequest{Messages: []anthropicMessage{{Role: "user", Content: []any{map[string]any{"type": "text", "text": "hi"}}}}}
	side := anthropicRequest{Messages: []anthropicMessage{{Role: "user", Content: []any{
		map[string]any{"type": "text", "text": "older context"},
		map[string]any{"type": "text", "text": "/btw how are you"},
	}}}}
	normalBody := []byte(`{"messages":[{"role":"user","content":"hi"}]}`)
	sideBody := []byte(`{"messages":[{"role":"user","content":"/btw how are you"}]}`)

	normalOut := responsesRequest{}
	normalReq := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(normalBody))
	normalReq.Header.Set("X-Claude-Code-Session-Id", "flow-a")
	normalInfo := configureCodexSession(cfg, normalReq, normalBody, normal, &normalOut)

	sideOut := responsesRequest{}
	sideReq := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(sideBody))
	sideReq.Header.Set("X-Claude-Code-Session-Id", "flow-a")
	sideInfo := configureCodexSession(cfg, sideReq, sideBody, side, &sideOut)

	if !sideInfo.SideThread || sideInfo.SideThreadKind != "btw" {
		t.Fatalf("side thread not detected: %#v", sideInfo)
	}
	if normalInfo.FlowHash != sideInfo.FlowHash {
		t.Fatalf("child session should keep parent flow hash: normal=%#v side=%#v", normalInfo, sideInfo)
	}
	if normalOut.PromptCacheKey == sideOut.PromptCacheKey {
		t.Fatalf("side thread shared parent prompt_cache_key %q", sideOut.PromptCacheKey)
	}
}

func TestRetryRemovesPromptCacheKeyWhenRejected(t *testing.T) {
	out := responsesRequest{PromptCacheKey: "ccp_test"}
	retry, ok := retryCodexRequestAfter400(out, http.StatusBadRequest, `{"error":{"message":"Unknown parameter: 'prompt_cache_key'."}}`)
	if !ok {
		t.Fatal("retry was not requested")
	}
	if retry.reason != "prompt_cache_key_not_accepted" {
		t.Fatalf("retry reason = %q", retry.reason)
	}
	if retry.request.PromptCacheKey != "" {
		t.Fatalf("prompt_cache_key still set: %#v", retry.request)
	}
}
