package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func anthropicToolUseBlock(t *testing.T, out map[string]any) map[string]any {
	t.Helper()
	content, ok := out["content"].([]any)
	if !ok {
		t.Fatalf("content = %#v, want []any", out["content"])
	}
	for _, item := range content {
		block, ok := item.(map[string]any)
		if ok && block["type"] == "tool_use" {
			return block
		}
	}
	t.Fatalf("tool_use block missing: %#v", content)
	return nil
}

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
	block := anthropicToolUseBlock(t, out)
	if block["type"] != "tool_use" || block["id"] != "call_1" || block["name"] != "read_file" {
		t.Fatalf("tool block = %#v", block)
	}
	input := block["input"].(map[string]any)
	if input["path"] != `C:\tmp\x.txt` {
		t.Fatalf("input path = %v", input["path"])
	}
}

func TestResponsesFunctionCallDropsEmptyPagesArgument(t *testing.T) {
	resp := responsesResponse{
		ID:     "resp_test",
		Model:  "gpt-5.5",
		Output: []responsesOutputItem{{Type: "function_call", ID: "fc_1", CallID: "call_1", Name: "Read", Arguments: `{"file_path":"C:\\tmp\\x.php","pages":"","limit":200}`}},
	}
	out := toAnthropicResponsesResponse(resp, "opus[1m]")
	block := anthropicToolUseBlock(t, out)
	input := block["input"].(map[string]any)
	if _, ok := input["pages"]; ok {
		t.Fatalf("pages argument was not removed: %#v", input)
	}
	if input["file_path"] != `C:\tmp\x.php` {
		t.Fatalf("file_path = %v", input["file_path"])
	}
}

func TestResponsesFunctionCallPreservesNonEmptyPagesArgument(t *testing.T) {
	resp := responsesResponse{
		ID:     "resp_test",
		Model:  "gpt-5.5",
		Output: []responsesOutputItem{{Type: "function_call", ID: "fc_1", CallID: "call_1", Name: "Read", Arguments: `{"file_path":"C:\\tmp\\x.pdf","pages":"1-2"}`}},
	}
	out := toAnthropicResponsesResponse(resp, "opus[1m]")
	block := anthropicToolUseBlock(t, out)
	input := block["input"].(map[string]any)
	if input["pages"] != "1-2" {
		t.Fatalf("pages = %v, want 1-2", input["pages"])
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

func TestNamespacedModelRoutesAndLegacyV1Removed(t *testing.T) {
	restoreProxyEnabled := proxyEnabled.Load()
	proxyEnabled.Store(true)
	t.Cleanup(func() { proxyEnabled.Store(restoreProxyEnabled) })

	cfg := config{Models: map[string]string{"sonnet[1m]": "gpt-5.5"}}
	handler := newProxyMux(cfg)

	for _, path := range []string{"/anthropic/v1/models", "/openai/v1/models"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s status = %d, want 200; body=%s", path, rec.Code, rec.Body.String())
		}
	}

	for _, path := range []string{"/v1/models", "/v1/messages", "/v1/messages/count_tokens", "/v1/chat/completions", "/v1/responses"} {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{}`))
		if path == "/v1/models" {
			req = httptest.NewRequest(http.MethodGet, path, nil)
		}
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s status = %d, want 404", path, rec.Code)
		}
	}
}

func TestAnthropicNamespaceRoutes(t *testing.T) {
	restoreProxyEnabled := proxyEnabled.Load()
	proxyEnabled.Store(true)
	t.Cleanup(func() { proxyEnabled.Store(restoreProxyEnabled) })

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Fatalf("upstream path = %s, want /chat/completions", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":    "chatcmpl_test",
			"model": "gpt-5.5",
			"choices": []map[string]any{{
				"finish_reason": "stop",
				"message":       map[string]any{"role": "assistant", "content": "hello from upstream"},
			}},
			"usage": map[string]int{"prompt_tokens": 3, "completion_tokens": 4},
		})
	}))
	defer upstream.Close()

	cfg := config{
		OpenAIAPIKey:  "sk-test",
		OpenAIBaseURL: upstream.URL,
		Upstream:      "openai",
		Models:        map[string]string{"sonnet[1m]": "gpt-5.5"},
	}
	handler := newProxyMux(cfg)

	tokenReq := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages/count_tokens", strings.NewReader(`{"model":"sonnet[1m]","messages":[{"role":"user","content":"hi"}]}`))
	tokenRec := httptest.NewRecorder()
	handler.ServeHTTP(tokenRec, tokenReq)
	if tokenRec.Code != http.StatusOK {
		t.Fatalf("count_tokens status = %d, want 200; body=%s", tokenRec.Code, tokenRec.Body.String())
	}

	msgReq := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", strings.NewReader(`{"model":"sonnet[1m]","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`))
	msgRec := httptest.NewRecorder()
	handler.ServeHTTP(msgRec, msgReq)
	if msgRec.Code != http.StatusOK {
		t.Fatalf("messages status = %d, want 200; body=%s", msgRec.Code, msgRec.Body.String())
	}
	if !strings.Contains(msgRec.Body.String(), "hello from upstream") {
		t.Fatalf("messages body missing upstream text: %s", msgRec.Body.String())
	}
}

func TestAnthropicNamespaceStreamingRoute(t *testing.T) {
	restoreProxyEnabled := proxyEnabled.Load()
	proxyEnabled.Store(true)
	t.Cleanup(func() { proxyEnabled.Store(restoreProxyEnabled) })

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, `data: {"choices":[{"delta":{"content":"hello"},"finish_reason":""}]}`+"\n\n")
		fmt.Fprint(w, `data: {"choices":[{"delta":{"content":" stream"},"finish_reason":"stop"}]}`+"\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer upstream.Close()

	cfg := config{
		OpenAIAPIKey:  "sk-test",
		OpenAIBaseURL: upstream.URL,
		Upstream:      "openai",
		Models:        map[string]string{"sonnet[1m]": "gpt-5.5"},
	}
	handler := newProxyMux(cfg)

	req := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", strings.NewReader(`{"model":"sonnet[1m]","max_tokens":16,"stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("stream status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	raw := rec.Body.String()
	for _, want := range []string{"event: message_start", "event: content_block_delta", "hello", "stream", "event: message_stop", "data: [DONE]"} {
		if !strings.Contains(raw, want) {
			t.Fatalf("stream missing %q:\n%s", want, raw)
		}
	}
}

func TestOpenAINamespaceRoutesUseOpenAICompatibleUpstream(t *testing.T) {
	restoreProxyEnabled := proxyEnabled.Load()
	proxyEnabled.Store(true)
	t.Cleanup(func() { proxyEnabled.Store(restoreProxyEnabled) })

	var paths []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		if got := r.Header.Get("Authorization"); got != "Bearer sk-test" {
			t.Fatalf("Authorization = %q, want Bearer sk-test", got)
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/chat/completions":
			_, _ = io.WriteString(w, `{"id":"chatcmpl_test","model":"gemini-3-flash-preview","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
		case "/responses":
			_, _ = io.WriteString(w, `{"id":"resp_test","object":"response","status":"completed","model":"gemini-3-flash-preview","output":[],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`)
		default:
			t.Fatalf("unexpected upstream path %s", r.URL.Path)
		}
	}))
	defer upstream.Close()

	cfg := config{
		OpenAIAPIKey:  "sk-test",
		OpenAIBaseURL: upstream.URL,
		Upstream:      "openai",
		Models:        map[string]string{"gemini-coder": "gemini-3-flash-preview"},
	}
	handler := newProxyMux(cfg)

	chatReq := httptest.NewRequest(http.MethodPost, "/openai/v1/chat/completions", strings.NewReader(`{"model":"gemini-coder","messages":[{"role":"user","content":"hi"}]}`))
	chatRec := httptest.NewRecorder()
	handler.ServeHTTP(chatRec, chatReq)
	if chatRec.Code != http.StatusOK {
		t.Fatalf("chat status = %d, want 200; body=%s", chatRec.Code, chatRec.Body.String())
	}

	respReq := httptest.NewRequest(http.MethodPost, "/openai/v1/responses", strings.NewReader(`{"model":"gemini-coder","input":[{"role":"user","content":"hi"}]}`))
	respRec := httptest.NewRecorder()
	handler.ServeHTTP(respRec, respReq)
	if respRec.Code != http.StatusOK {
		t.Fatalf("responses status = %d, want 200; body=%s", respRec.Code, respRec.Body.String())
	}

	if strings.Join(paths, ",") != "/chat/completions,/responses" {
		t.Fatalf("upstream paths = %#v", paths)
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

func TestSaveModelAliasesPersistsOverridesAndDisabledRows(t *testing.T) {
	t.Setenv("PROXY_MODEL_ALIASES", "")
	t.Setenv("PROXY_MODEL_ALIASES_DISABLED", "")
	vals := map[string]string{}
	next, err := saveModelAliasesToEnvMap(vals, []modelAliasConfig{
		{Alias: "sonnet", Real: "gpt-5.4", Context: "1m"},
		{Alias: "custom-claude", Real: "gpt-5.4-mini", Context: "200k"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if next.Models["sonnet"] != "gpt-5.4" {
		t.Fatalf("sonnet = %q, want gpt-5.4", next.Models["sonnet"])
	}
	if next.Models["custom-claude"] != "gpt-5.4-mini" || !next.ModelCustom["custom-claude"] {
		t.Fatalf("custom alias not active/custom: %#v %#v", next.Models, next.ModelCustom)
	}
	if _, ok := next.Models["opus"]; ok {
		t.Fatalf("deleted built-in alias remained active: %#v", next.Models)
	}

	var overrides []modelAliasConfig
	if err := json.Unmarshal([]byte(vals["PROXY_MODEL_ALIASES"]), &overrides); err != nil {
		t.Fatalf("override JSON invalid: %v", err)
	}
	if len(overrides) != 2 {
		t.Fatalf("overrides = %#v, want 2 rows", overrides)
	}
	var disabled []string
	if err := json.Unmarshal([]byte(vals["PROXY_MODEL_ALIASES_DISABLED"]), &disabled); err != nil {
		t.Fatalf("disabled JSON invalid: %v", err)
	}
	if !stringSliceContains(disabled, "opus") {
		t.Fatalf("disabled aliases = %#v, want opus disabled", disabled)
	}
	if stringSliceContains(disabled, "claude-3-7-sonnet-latest") {
		t.Fatalf("hidden legacy aliases should not be written as disabled: %#v", disabled)
	}
}

func TestNormalizeSubmittedModelAliasesRejectsDuplicates(t *testing.T) {
	_, err := normalizeSubmittedModelAliases([]modelAliasConfig{
		{Alias: "sonnet", Real: "gpt-5.5", Context: "200k"},
		{Alias: "SONNET", Real: "gpt-5.4", Context: "1m"},
	})
	if err == nil || !strings.Contains(err.Error(), "duplicated") {
		t.Fatalf("err = %v, want duplicate error", err)
	}
}

func stringSliceContains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
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

func TestWebSearchCallAddsToolActivityThinkingBlock(t *testing.T) {
	resp := responsesResponse{
		ID: "resp_test",
		Output: []responsesOutputItem{{
			Type:   "web_search_call",
			ID:     "ws_1234567890abcdef",
			Status: "completed",
			Action: map[string]any{"query": "Shalabi Clinic official website"},
		}},
	}
	out := toAnthropicResponsesResponse(resp, "opus[1m]")
	content := out["content"].([]any)
	if len(content) != 1 {
		t.Fatalf("content = %#v, want one thinking block", content)
	}
	thinking := content[0].(map[string]any)
	text := fmt.Sprint(thinking["thinking"])
	if thinking["type"] != "thinking" || !strings.Contains(text, "web_search") || !strings.Contains(text, "Shalabi Clinic official website") {
		t.Fatalf("web search thinking block = %#v", thinking)
	}
}

func TestWebSearchPreflightThinkingUsesLatestUserRequest(t *testing.T) {
	out := responsesRequest{Tools: []responsesTool{{Type: "web_search"}}}
	in := anthropicRequest{Messages: []anthropicMessage{{Role: "user", Content: []any{map[string]any{"type": "text", "text": "Find the official website for Shalabi Clinic"}}}}}
	text := webSearchPreflightThinking(in, out)
	if !strings.Contains(text, "Codex web_search is enabled") || !strings.Contains(text, "Find the official website") {
		t.Fatalf("preflight thinking = %q", text)
	}

	in = anthropicRequest{Messages: []anthropicMessage{{Role: "user", Content: "hi"}}}
	if text := webSearchPreflightThinking(in, out); text != "" {
		t.Fatalf("generic request should not get web preflight thinking: %q", text)
	}
}

func TestAnthropicMediaBlocksConvertToOpenAICompatibleParts(t *testing.T) {
	in := anthropicRequest{
		Model:        "gemini-3-flash-preview",
		MaxTokens:    64,
		OutputConfig: &anthropicOutputConfig{Effort: "xhigh"},
		Messages: []anthropicMessage{{Role: "user", Content: []any{
			map[string]any{"type": "text", "text": "Read these."},
			map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": "image/png", "data": "iVBORw0KGgo="}},
			map[string]any{"type": "document", "title": "brief.pdf", "source": map[string]any{"type": "base64", "media_type": "application/pdf", "data": "JVBERi0="}},
		}}},
	}

	out, err := toOpenAI(config{}, in)
	if err != nil {
		t.Fatal(err)
	}
	if out.ReasoningEffort != "xhigh" {
		t.Fatalf("reasoning_effort = %q, want xhigh before provider normalization", out.ReasoningEffort)
	}
	prepared := prepareOpenAIRequestForUpstream(config{}, providerCredential{Provider: "gemini"}, out)
	if prepared.ReasoningEffort != "high" {
		t.Fatalf("gemini reasoning_effort = %q, want high", prepared.ReasoningEffort)
	}
	if len(out.Messages) != 1 {
		t.Fatalf("messages = %#v", out.Messages)
	}
	parts, ok := out.Messages[0].Content.([]any)
	if !ok {
		t.Fatalf("content = %#v, want content parts", out.Messages[0].Content)
	}
	if len(parts) != 3 {
		t.Fatalf("parts = %#v", parts)
	}
	image := parts[1].(map[string]any)
	if image["type"] != "image_url" {
		t.Fatalf("image part = %#v", image)
	}
	imageURL := image["image_url"].(map[string]any)
	if imageURL["url"] != "data:image/png;base64,iVBORw0KGgo=" {
		t.Fatalf("image URL = %#v", imageURL)
	}
	file := parts[2].(map[string]any)
	if file["type"] != "file" {
		t.Fatalf("file part = %#v", file)
	}
	fileBody := file["file"].(map[string]any)
	if fileBody["filename"] != "brief.pdf" || fileBody["file_data"] != "JVBERi0=" {
		t.Fatalf("file body = %#v", fileBody)
	}
}

func TestOpenAIFileContentConvertsToResponsesInputFile(t *testing.T) {
	content := []any{
		map[string]any{"type": "text", "text": "Summarize this file."},
		map[string]any{"type": "file", "file": map[string]any{"filename": "notes.txt", "file_data": "SGVsbG8="}},
	}
	converted := openAIContentToResponses("user", content)
	parts, ok := converted.([]map[string]any)
	if !ok {
		t.Fatalf("converted = %#v, want response content parts", converted)
	}
	if len(parts) != 2 {
		t.Fatalf("parts = %#v", parts)
	}
	if parts[1]["type"] != "input_file" || parts[1]["filename"] != "notes.txt" || parts[1]["file_data"] != "SGVsbG8=" {
		t.Fatalf("file part = %#v", parts[1])
	}
}

func TestOpenAIFilesProxyPassesThroughToProvider(t *testing.T) {
	var gotPath, gotAuth string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.String()
		gotAuth = r.Header.Get("Authorization")
		writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": []any{}})
	}))
	defer upstream.Close()

	cfg := config{Upstream: "openai", OpenAIAPIKey: "sk-test", OpenAIBaseURL: upstream.URL}
	req := httptest.NewRequest(http.MethodGet, "/openai/v1/files?limit=1", nil)
	rr := httptest.NewRecorder()
	handleOpenAIFiles(cfg)(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rr.Code, rr.Body.String())
	}
	if gotPath != "/files?limit=1" {
		t.Fatalf("upstream path = %q", gotPath)
	}
	if gotAuth != "Bearer sk-test" {
		t.Fatalf("auth header = %q", gotAuth)
	}
}

func TestOpenAICompatibleCustomSessionID(t *testing.T) {
	t.Setenv("CODEX_SESSION_ISOLATION", "1")
	t.Setenv("CODEX_PROMPT_CACHE_KEY", "1")
	cfg := config{CodexSessionFile: filepath.Join(t.TempDir(), "sessions.json")}
	req := httptest.NewRequest(http.MethodPost, "/openai/v1/chat/completions", strings.NewReader("{}"))
	req.Header.Set("X-Proxy-Session-Id", "workspace-session-7")
	in := openAIRequest{Model: "gpt-5.5", Messages: []openAIMessage{{Role: "user", Content: "hi"}}}
	applyCustomSessionToOpenAIRequest(req, &in)
	if in.User != "workspace-session-7" {
		t.Fatalf("user = %q", in.User)
	}

	out := responsesRequest{}
	info := configureOpenAICompatibleSession(cfg, req, in, &out)
	if !info.Enabled || out.PromptCacheKey == "" {
		t.Fatalf("session was not configured: info=%#v out=%#v", info, out)
	}

	again := responsesRequest{}
	againInfo := configureOpenAICompatibleSession(cfg, req, in, &again)
	if out.PromptCacheKey != again.PromptCacheKey || info.CodexSessionID != againInfo.CodexSessionID {
		t.Fatalf("session was not stable: %#v %#v", info, againInfo)
	}
}

func TestClientKeySchemaAllowsOnlySelectedNamespace(t *testing.T) {
	if !clientKeySchemaAllowsPath("anthropic", "/anthropic/v1/messages") || clientKeySchemaAllowsPath("anthropic", "/openai/v1/chat/completions") {
		t.Fatal("anthropic schema gating failed")
	}
	if !clientKeySchemaAllowsPath("openai", "/openai/v1/chat/completions") || clientKeySchemaAllowsPath("openai", "/anthropic/v1/messages") {
		t.Fatal("openai schema gating failed")
	}
	if !clientKeySchemaAllowsPath("both", "/anthropic/v1/messages") || !clientKeySchemaAllowsPath("both", "/openai/v1/responses") {
		t.Fatal("both schema gating failed")
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
	firstReq := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", bytes.NewReader(body))
	firstReq.Header.Set("X-Claude-Code-Session-Id", "flow-a")
	firstInfo := configureCodexSession(cfg, firstReq, body, in, &first)

	second := responsesRequest{}
	secondReq := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", bytes.NewReader(body))
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
	firstReq := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", bytes.NewReader(body))
	firstReq.Header.Set("X-Claude-Code-Session-Id", "flow-a")
	configureCodexSession(cfg, firstReq, body, in, &first)

	second := responsesRequest{}
	secondReq := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", bytes.NewReader(body))
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
	normalReq := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", bytes.NewReader(normalBody))
	normalReq.Header.Set("X-Claude-Code-Session-Id", "flow-a")
	normalInfo := configureCodexSession(cfg, normalReq, normalBody, normal, &normalOut)

	sideOut := responsesRequest{}
	sideReq := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", bytes.NewReader(sideBody))
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

func TestCodexSubagentMarkerGetsStableChildSession(t *testing.T) {
	t.Setenv("CODEX_SESSION_ISOLATION", "1")
	t.Setenv("CODEX_PROMPT_CACHE_KEY", "1")
	cfg := config{CodexSessionFile: filepath.Join(t.TempDir(), "sessions.json")}
	normal := anthropicRequest{Messages: []anthropicMessage{{Role: "user", Content: "hi"}}}
	firstAgent := anthropicRequest{Messages: []anthropicMessage{{Role: "user", Content: []any{
		map[string]any{"type": "text", "text": "Internal proxy routing note: codex_proxy_subagent_id=agent-downloads; codex_proxy_subagent_type=general-purpose."},
		map[string]any{"type": "text", "text": "Inspect downloads."},
	}}}}
	secondAgent := anthropicRequest{Messages: []anthropicMessage{{Role: "user", Content: []any{
		map[string]any{"type": "text", "text": "Internal proxy routing note: codex_proxy_subagent_id=agent-downloads; codex_proxy_subagent_type=general-purpose."},
		map[string]any{"type": "text", "text": "Continue inspecting downloads."},
	}}}}

	normalOut := responsesRequest{}
	normalReq := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", bytes.NewReader([]byte(`{"messages":[{"role":"user","content":"hi"}]}`)))
	normalReq.Header.Set("X-Claude-Code-Session-Id", "flow-a")
	configureCodexSession(cfg, normalReq, []byte(`{"messages":[{"role":"user","content":"hi"}]}`), normal, &normalOut)

	firstOut := responsesRequest{}
	firstBody := []byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"Internal proxy routing note: codex_proxy_subagent_id=agent-downloads; codex_proxy_subagent_type=general-purpose."},{"type":"text","text":"Inspect downloads."}]}]}`)
	firstReq := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", bytes.NewReader(firstBody))
	firstReq.Header.Set("X-Claude-Code-Session-Id", "flow-a")
	firstInfo := configureCodexSession(cfg, firstReq, firstBody, firstAgent, &firstOut)

	secondOut := responsesRequest{}
	secondBody := []byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"Internal proxy routing note: codex_proxy_subagent_id=agent-downloads; codex_proxy_subagent_type=general-purpose."},{"type":"text","text":"Continue inspecting downloads."}]}]}`)
	secondReq := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", bytes.NewReader(secondBody))
	secondReq.Header.Set("X-Claude-Code-Session-Id", "flow-a")
	secondInfo := configureCodexSession(cfg, secondReq, secondBody, secondAgent, &secondOut)

	if !firstInfo.SideThread || firstInfo.SideThreadKind != "subagent" {
		t.Fatalf("subagent marker not detected: %#v", firstInfo)
	}
	if firstOut.PromptCacheKey == normalOut.PromptCacheKey {
		t.Fatalf("subagent shared parent prompt_cache_key %q", firstOut.PromptCacheKey)
	}
	if firstOut.PromptCacheKey != secondOut.PromptCacheKey {
		t.Fatalf("same subagent did not keep a stable prompt_cache_key: %q vs %q", firstOut.PromptCacheKey, secondOut.PromptCacheKey)
	}
	if firstInfo.RegistryKey != secondInfo.RegistryKey {
		t.Fatalf("same subagent did not keep a stable registry key: %q vs %q", firstInfo.RegistryKey, secondInfo.RegistryKey)
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

func TestCodexRequestsOmitTemperature(t *testing.T) {
	temp := 1.0
	out := toResponses(config{}, anthropicRequest{
		Model:       "claude-sonnet-4-6",
		Temperature: &temp,
		Messages:    []anthropicMessage{{Role: "user", Content: "hi"}},
	})
	if out.Temperature != nil {
		t.Fatalf("Codex Anthropic conversion kept temperature: %#v", out.Temperature)
	}

	chatOut := openAIChatToResponses(config{}, openAIRequest{
		Model:       "gpt-5.5",
		Temperature: &temp,
		Messages:    []openAIMessage{{Role: "user", Content: "hi"}},
	})
	if chatOut.Temperature != nil {
		t.Fatalf("Codex chat conversion kept temperature: %#v", chatOut.Temperature)
	}

	retry, ok := retryCodexRequestAfter400(responsesRequest{Temperature: &temp}, http.StatusBadRequest, `{"detail":"Unsupported parameter: temperature"}`)
	if !ok {
		t.Fatal("temperature retry was not requested")
	}
	if retry.reason != "temperature_not_accepted" {
		t.Fatalf("retry reason = %q", retry.reason)
	}
	if retry.request.Temperature != nil {
		t.Fatalf("temperature still set after retry: %#v", retry.request.Temperature)
	}
}
