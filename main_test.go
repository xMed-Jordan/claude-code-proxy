package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestPublicBaseURLNormalization(t *testing.T) {
	cases := map[string]string{
		"ai-api1.cus.cx":              "https://ai-api1.cus.cx",
		"https://ai-api1.cus.cx:443":  "https://ai-api1.cus.cx",
		"http://ai-api1.cus.cx:80":    "http://ai-api1.cus.cx",
		"https://ai-api1.cus.cx:8443": "https://ai-api1.cus.cx:8443",
		"https://ai-api1.cus.cx/":     "https://ai-api1.cus.cx",
	}
	for in, want := range cases {
		if got := normalizePublicBaseURL(in); got != want {
			t.Fatalf("normalizePublicBaseURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestClientFacingBaseURLPrefersPublicURL(t *testing.T) {
	cfg := config{Port: "4000", PublicURL: normalizePublicBaseURL("https://ai-api1.cus.cx:443")}
	if got := clientFacingBaseURL(cfg); got != "https://ai-api1.cus.cx" {
		t.Fatalf("clientFacingBaseURL() = %q", got)
	}
	cfg.PublicURL = ""
	if got := clientFacingBaseURL(cfg); got != "http://127.0.0.1:4000" {
		t.Fatalf("clientFacingBaseURL() fallback = %q", got)
	}
}

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

	for _, path := range []string{"/anthropic/v1/models", "/anthropic/v1/model-capabilities", "/openai/v1/models", "/openai/v1/model-capabilities"} {
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

func TestDocumentationRoutesPublicAndValid(t *testing.T) {
	cfg := config{Port: "4000", ProxyKey: "sk-local", Models: map[string]string{"sonnet[1m]": "gpt-5.5"}}
	handler := newProxyMux(cfg)

	for _, path := range []string{"/docs", "/openapi.json", "/postman.json"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s status = %d, want 200; body=%s", path, rec.Code, rec.Body.String())
		}
	}

	openAPIReq := httptest.NewRequest(http.MethodGet, "/openapi.json", nil)
	openAPIRec := httptest.NewRecorder()
	handler.ServeHTTP(openAPIRec, openAPIReq)
	var openAPI map[string]any
	if err := json.Unmarshal(openAPIRec.Body.Bytes(), &openAPI); err != nil {
		t.Fatalf("openapi.json is invalid JSON: %v", err)
	}
	if openAPI["openapi"] != "3.0.3" {
		t.Fatalf("openapi version = %v, want 3.0.3", openAPI["openapi"])
	}
	paths, ok := openAPI["paths"].(map[string]any)
	if !ok {
		t.Fatalf("openapi paths missing: %#v", openAPI["paths"])
	}
	for _, path := range []string{"/anthropic/v1/messages", "/anthropic/v1/model-capabilities", "/openai/v1/model-capabilities", "/openai/v1/chat/completions", "/ui/api/status", "/ui/api/update/status", "/ui/api/update/check", "/ui/api/update/start", "/ui/api/update/settings"} {
		if _, ok := paths[path]; !ok {
			t.Fatalf("openapi path %s missing", path)
		}
	}
	for path := range paths {
		if strings.HasPrefix(path, "/v1/") {
			t.Fatalf("legacy root path documented unexpectedly: %s", path)
		}
	}

	postmanReq := httptest.NewRequest(http.MethodGet, "/postman.json", nil)
	postmanRec := httptest.NewRecorder()
	handler.ServeHTTP(postmanRec, postmanReq)
	var collection map[string]any
	if err := json.Unmarshal(postmanRec.Body.Bytes(), &collection); err != nil {
		t.Fatalf("postman.json is invalid JSON: %v", err)
	}
	info, ok := collection["info"].(map[string]any)
	if !ok {
		t.Fatalf("postman info missing: %#v", collection["info"])
	}
	if info["schema"] != "https://schema.getpostman.com/json/collection/v2.1.0/collection.json" {
		t.Fatalf("postman schema = %v", info["schema"])
	}
	if items, ok := collection["item"].([]any); !ok || len(items) == 0 {
		t.Fatalf("postman collection has no items: %#v", collection["item"])
	}
}

func TestAPIDocumentationManifestCoverageAndAuth(t *testing.T) {
	routes := apiDocRoutes()
	coverage := map[string]bool{}
	operations := map[string]bool{}
	for _, route := range routes {
		if route.OperationID == "" {
			t.Fatalf("route missing operation id: %#v", route)
		}
		if operations[route.OperationID] {
			t.Fatalf("duplicate operation id: %s", route.OperationID)
		}
		operations[route.OperationID] = true
		if strings.HasPrefix(route.Path, "/v1/") || strings.HasPrefix(routeCoveragePath(route), "/v1/") {
			t.Fatalf("legacy root path documented unexpectedly: %#v", route)
		}
		coverage[route.Method+" "+routeCoveragePath(route)] = true
	}

	expected := []string{
		"GET /docs",
		"GET /openapi.json",
		"GET /postman.json",
		"GET /health",
		"GET /antigravity/bridge",
		"GET /anthropic/v1/models",
		"GET /anthropic/v1/model-capabilities",
		"POST /anthropic/v1/messages",
		"POST /anthropic/v1/messages/count_tokens",
		"GET /openai/v1/models",
		"GET /openai/v1/model-capabilities",
		"POST /openai/v1/chat/completions",
		"POST /openai/v1/responses",
		"GET /openai/v1/files",
		"POST /openai/v1/files",
		"GET /openai/v1/files/",
		"DELETE /openai/v1/files/",
		"GET /ui/api/auth/status",
		"POST /ui/api/auth/setup",
		"POST /ui/api/auth/login",
		"POST /ui/api/auth/logout",
		"GET /ui/api/status",
		"GET /ui/api/update/status",
		"POST /ui/api/update/check",
		"POST /ui/api/update/start",
		"POST /ui/api/update/settings",
		"GET /ui/api/config",
		"POST /ui/api/config",
		"GET /ui/api/models",
		"POST /ui/api/models",
		"GET /ui/api/keys",
		"POST /ui/api/keys/provider",
		"POST /ui/api/keys/client",
		"POST /ui/api/keys/toggle",
		"GET /ui/api/validate",
		"POST /ui/api/test",
		"GET /ui/api/logs",
		"GET /ui/api/antigravity",
		"POST /ui/api/antigravity/probe",
		"POST /ui/api/proxy/stop",
		"POST /ui/api/proxy/start",
		"POST /ui/api/proxy/restart",
	}
	for _, want := range expected {
		if !coverage[want] {
			t.Fatalf("documented route coverage missing %s", want)
		}
	}

	assertAuth := func(method, path string, want apiDocAuth) {
		t.Helper()
		for _, route := range routes {
			if route.Method == method && routeCoveragePath(route) == path {
				if route.Auth != want {
					t.Fatalf("%s %s auth = %s, want %s", method, path, route.Auth, want)
				}
				return
			}
		}
		t.Fatalf("%s %s not found in docs manifest", method, path)
	}
	assertAuth(http.MethodGet, "/health", apiDocAuthPublic)
	assertAuth(http.MethodGet, "/anthropic/v1/model-capabilities", apiDocAuthProxy)
	assertAuth(http.MethodPost, "/anthropic/v1/messages", apiDocAuthProxy)
	assertAuth(http.MethodGet, "/openai/v1/model-capabilities", apiDocAuthProxy)
	assertAuth(http.MethodPost, "/openai/v1/chat/completions", apiDocAuthProxy)
	assertAuth(http.MethodGet, "/ui/api/auth/status", apiDocAuthPublic)
	assertAuth(http.MethodGet, "/ui/api/status", apiDocAuthAdmin)
	assertAuth(http.MethodGet, "/ui/api/update/status", apiDocAuthAdmin)
	assertAuth(http.MethodPost, "/ui/api/update/check", apiDocAuthAdmin)
	assertAuth(http.MethodPost, "/ui/api/update/start", apiDocAuthAdmin)
	assertAuth(http.MethodPost, "/ui/api/update/settings", apiDocAuthAdmin)
}

func TestModelCapabilitiesEndpoint(t *testing.T) {
	restoreProxyEnabled := proxyEnabled.Load()
	proxyEnabled.Store(true)
	t.Cleanup(func() { proxyEnabled.Store(restoreProxyEnabled) })

	cfg := config{
		ProxyKey: "sk-local",
		Upstream: "codex",
		Models: map[string]string{
			"sonnet[1m]":     "gpt-5.5",
			"custom-small":   "local-model",
			"claude-3-haiku": "legacy-hidden",
		},
		ModelContexts: map[string]string{
			"custom-small": "200k",
		},
		ModelCustom: map[string]bool{
			"custom-small": true,
		},
	}
	handler := newProxyMux(cfg)

	for _, path := range []string{"/anthropic/v1/model-capabilities", "/openai/v1/model-capabilities"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("x-api-key", "sk-local")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s status = %d, want 200; body=%s", path, rec.Code, rec.Body.String())
		}

		var out struct {
			Object string           `json:"object"`
			Units  string           `json:"units"`
			Data   []map[string]any `json:"data"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("GET %s returned invalid JSON: %v", path, err)
		}
		if out.Object != "list" || out.Units != "tokens" {
			t.Fatalf("GET %s metadata = object:%q units:%q, want list/tokens", path, out.Object, out.Units)
		}

		sonnet := findCapabilityRow(out.Data, "sonnet[1m]")
		if sonnet == nil {
			t.Fatalf("GET %s missing sonnet[1m] row: %#v", path, out.Data)
		}
		assertJSONNumber(t, sonnet, "context_window_tokens", 200000)
		assertJSONNumber(t, sonnet, "max_output_tokens", 128000)
		assertJSONNumber(t, sonnet, "max_input_tokens", 72000)
		if got := sonnet["upstream_model"]; got != "gpt-5.5" {
			t.Fatalf("upstream_model = %#v, want gpt-5.5", got)
		}
		if got := sonnet["context"]; got != "200k" {
			t.Fatalf("context = %#v, want 200k", got)
		}
		if got := sonnet["limits_known"]; got != true {
			t.Fatalf("limits_known = %#v, want true", got)
		}

		custom := findCapabilityRow(out.Data, "custom-small")
		if custom == nil {
			t.Fatalf("GET %s missing custom-small row: %#v", path, out.Data)
		}
		assertJSONNumber(t, custom, "context_window_tokens", 200000)
		assertJSONNumber(t, custom, "max_output_tokens", 0)
		assertJSONNumber(t, custom, "max_input_tokens", 200000)
		if got := custom["limits_known"]; got != false {
			t.Fatalf("custom limits_known = %#v, want false", got)
		}
		if hidden := findCapabilityRow(out.Data, "claude-3-haiku"); hidden != nil {
			t.Fatalf("legacy-hidden model should not be advertised: %#v", hidden)
		}
	}
}

func findCapabilityRow(rows []map[string]any, id string) map[string]any {
	for _, row := range rows {
		if row["id"] == id {
			return row
		}
	}
	return nil
}

func assertJSONNumber(t *testing.T, row map[string]any, key string, want float64) {
	t.Helper()
	got, ok := row[key].(float64)
	if !ok {
		t.Fatalf("%s = %#v, want JSON number", key, row[key])
	}
	if got != want {
		t.Fatalf("%s = %v, want %v", key, got, want)
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

func TestCodexTokenLimitGuardHardBlockCanBeEnabled(t *testing.T) {
	restoreProxyEnabled := proxyEnabled.Load()
	proxyEnabled.Store(true)
	t.Cleanup(func() { proxyEnabled.Store(restoreProxyEnabled) })
	t.Setenv("CODEX_UPSTREAM_HARD_TOKENS", "20")
	t.Setenv("CODEX_UPSTREAM_BLOCK_AT_HARD", "1")

	cfg := config{Upstream: "codex", Models: map[string]string{"sonnet[1m]": "gpt-5.5"}}
	handler := newProxyMux(cfg)

	req := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", strings.NewReader(`{"model":"sonnet[1m]","max_tokens":1,"messages":[{"role":"user","content":"`+strings.Repeat("x", 88)+`"}]}`))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "context_length_exceeded") {
		t.Fatalf("body missing context error: %s", rec.Body.String())
	}
}

func TestCodexContextLengthUpstreamErrorReturnsAPIError(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/responses" {
			t.Fatalf("upstream path = %s, want /responses", r.URL.Path)
		}
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"detail":"context_length_exceeded: Your input exceeds the context window."}`)
	}))
	defer upstream.Close()

	authFile := filepath.Join(t.TempDir(), "auth.json")
	if err := os.WriteFile(authFile, []byte(`{"auth_mode":"chatgpt","tokens":{"access_token":"test-token","account_id":"test-account"}}`), 0600); err != nil {
		t.Fatal(err)
	}

	resp, status, msg, err := callCodexResponses(context.Background(), config{CodexBaseURL: upstream.URL, CodexAuthFile: authFile}, responsesRequest{
		Model: "gpt-5.5",
		Input: []any{map[string]any{"role": "user", "content": "hello"}},
	})
	if err == nil {
		t.Fatalf("callCodexResponses returned nil error and response %#v", resp)
	}
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; msg=%s", status, msg)
	}
	if !strings.Contains(msg, "context_length_exceeded") {
		t.Fatalf("msg missing upstream context error: %s", msg)
	}
}

func TestCodexRequestBodyStripsMaxOutputTokens(t *testing.T) {
	body := codexRequestBody(responsesRequest{
		Model:           "gpt-5.5",
		Input:           []any{"hello"},
		MaxOutputTokens: 1234,
		Stream:          true,
		Store:           false,
	})
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal sanitized body: %v", err)
	}
	if strings.Contains(string(raw), "max_output_tokens") {
		t.Fatalf("codex request leaked unsupported max_output_tokens: %s", raw)
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
			_, _ = io.WriteString(w, `{"id":"chatcmpl_test","model":"custom-flash-preview","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
		case "/responses":
			_, _ = io.WriteString(w, `{"id":"resp_test","object":"response","status":"completed","model":"custom-flash-preview","output":[],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`)
		default:
			t.Fatalf("unexpected upstream path %s", r.URL.Path)
		}
	}))
	defer upstream.Close()

	cfg := config{
		OpenAIAPIKey:  "sk-test",
		OpenAIBaseURL: upstream.URL,
		Upstream:      "openai",
		Models:        map[string]string{"custom-coder": "custom-flash-preview"},
	}
	handler := newProxyMux(cfg)

	chatReq := httptest.NewRequest(http.MethodPost, "/openai/v1/chat/completions", strings.NewReader(`{"model":"custom-coder","messages":[{"role":"user","content":"hi"}]}`))
	chatRec := httptest.NewRecorder()
	handler.ServeHTTP(chatRec, chatReq)
	if chatRec.Code != http.StatusOK {
		t.Fatalf("chat status = %d, want 200; body=%s", chatRec.Code, chatRec.Body.String())
	}

	respReq := httptest.NewRequest(http.MethodPost, "/openai/v1/responses", strings.NewReader(`{"model":"custom-coder","input":[{"role":"user","content":"hi"}]}`))
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

func TestModelForwardToPersistAndDefault(t *testing.T) {
	t.Setenv("PROXY_MODEL_ALIASES", "")
	t.Setenv("PROXY_MODEL_ALIASES_DISABLED", "")
	vals := map[string]string{}
	next, err := saveModelAliasesToEnvMap(vals, []modelAliasConfig{
		{Alias: "agy-model", Real: "gpt-5.4", Context: "200k", ForwardTo: "agy"},
		{Alias: "plain-model", Real: "gpt-5.4-mini", Context: "200k"},
		{Alias: "weird-model", Real: "gpt-5.5", Context: "200k", ForwardTo: "xyz"},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Runtime config carries the forward target, defaulting/normalizing to codex.
	if next.ModelForward["agy-model"] != "agy" {
		t.Fatalf("agy-model forward = %q, want agy", next.ModelForward["agy-model"])
	}
	if next.ModelForward["plain-model"] != "codex" {
		t.Fatalf("plain-model forward = %q, want codex (default)", next.ModelForward["plain-model"])
	}
	if next.ModelForward["weird-model"] != "codex" {
		t.Fatalf("weird-model forward = %q, want codex (invalid normalized)", next.ModelForward["weird-model"])
	}

	// Persisted JSON keeps forward_to only for non-codex targets (omitempty + blanked default).
	var overrides []modelAliasConfig
	if err := json.Unmarshal([]byte(vals["PROXY_MODEL_ALIASES"]), &overrides); err != nil {
		t.Fatalf("override JSON invalid: %v", err)
	}
	byAlias := map[string]modelAliasConfig{}
	for _, o := range overrides {
		byAlias[o.Alias] = o
	}
	if byAlias["agy-model"].ForwardTo != "agy" {
		t.Fatalf("persisted agy-model forward_to = %q, want agy", byAlias["agy-model"].ForwardTo)
	}
	if byAlias["plain-model"].ForwardTo != "" {
		t.Fatalf("default codex row should omit forward_to, got %q", byAlias["plain-model"].ForwardTo)
	}

	// modelRows surfaces forward_to (default codex) for the admin UI.
	got := map[string]any{}
	for _, r := range modelRows(next) {
		got[fmt.Sprint(r["alias"])] = r["forward_to"]
	}
	if got["agy-model"] != "agy" {
		t.Fatalf("modelRows agy-model forward_to = %v, want agy", got["agy-model"])
	}
	if got["plain-model"] != "codex" {
		t.Fatalf("modelRows plain-model forward_to = %v, want codex", got["plain-model"])
	}

	// A round-trip through the env loader restores the forward targets.
	env := func(key, def string) string {
		if v, ok := vals[key]; ok {
			return v
		}
		return def
	}
	if _, _, _, forwards, _, _ := modelAliasesFromValues(env); forwards["agy-model"] != "agy" {
		t.Fatalf("loader agy-model forward = %q, want agy", forwards["agy-model"])
	}
}

func TestModelDefaultRecommendedPersist(t *testing.T) {
	t.Setenv("PROXY_MODEL_ALIASES", "")
	t.Setenv("PROXY_MODEL_ALIASES_DISABLED", "")
	vals := map[string]string{}
	// "sonnet" is a built-in default; unset it and make "opus" the default +
	// recommended. The unset of a built-in flag must persist.
	next, err := saveModelAliasesToEnvMap(vals, []modelAliasConfig{
		{Alias: "opus", Real: "gpt-5.5", Context: "1m", Default: true, Recommended: true},
		{Alias: "sonnet", Real: "gpt-5.5", Context: "200k", Default: false},
		{Alias: "haiku", Real: "gpt-5.4-mini", Context: "200k"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !next.ModelDefault["opus"] || !next.ModelRecommended["opus"] {
		t.Fatalf("opus flags not set: default=%v recommended=%v", next.ModelDefault["opus"], next.ModelRecommended["opus"])
	}
	rows := map[string]map[string]any{}
	for _, r := range modelRows(next) {
		rows[fmt.Sprint(r["alias"])] = r
	}
	if rows["opus"]["default"] != true || rows["opus"]["recommended"] != true {
		t.Fatalf("opus row flags = %v / %v", rows["opus"]["default"], rows["opus"]["recommended"])
	}
	// sonnet's built-in default is now explicitly off, and it must round-trip.
	if rows["sonnet"]["default"] != false {
		t.Fatalf("sonnet default = %v, want false", rows["sonnet"]["default"])
	}
	var overrides []modelAliasConfig
	if err := json.Unmarshal([]byte(vals["PROXY_MODEL_ALIASES"]), &overrides); err != nil {
		t.Fatalf("override JSON invalid: %v", err)
	}
	byAlias := map[string]modelAliasConfig{}
	for _, o := range overrides {
		byAlias[o.Alias] = o
	}
	if !byAlias["opus"].Default || !byAlias["opus"].Recommended {
		t.Fatalf("persisted opus flags lost: %+v", byAlias["opus"])
	}
	if _, ok := byAlias["sonnet"]; !ok {
		t.Fatal("sonnet must be persisted as an override (built-in default unset)")
	}
	// Reload from the persisted env and confirm the flags survive.
	env := func(key, def string) string {
		if v, ok := vals[key]; ok {
			return v
		}
		return def
	}
	_, _, _, _, defaults, recommended := modelAliasesFromValues(env)
	if !defaults["opus"] || defaults["sonnet"] {
		t.Fatalf("reloaded defaults wrong: opus=%v sonnet=%v", defaults["opus"], defaults["sonnet"])
	}
	if !recommended["opus"] {
		t.Fatalf("reloaded opus recommended = %v, want true", recommended["opus"])
	}
}

func TestModelStatusForRow(t *testing.T) {
	cfg := config{
		Models:       map[string]string{"st-agy1": "Gemini 3.1 Pro (High)", "st-agy2": "totally-unknown", "st-c1": "gpt-5.5"},
		ModelForward: map[string]string{"st-agy1": "agy", "st-agy2": "agy"},
	}
	if got := modelStatusForRow(cfg, "st-agy1", "Gemini 3.1 Pro (High)"); got != "ok" {
		t.Fatalf("known Antigravity model status = %q, want ok", got)
	}
	if got := modelStatusForRow(cfg, "st-agy2", "totally-unknown"); got != "untested" {
		t.Fatalf("unknown agy model status = %q, want untested", got)
	}
	if got := modelStatusForRow(cfg, "st-x", ""); got != "unsupported" {
		t.Fatalf("empty real status = %q, want unsupported", got)
	}
	// A successful request verifies the alias → Available even if otherwise untested.
	markModelAliasVerified("st-agy2")
	if got := modelStatusForRow(cfg, "st-agy2", "totally-unknown"); got != "ok" {
		t.Fatalf("verified alias status = %q, want ok", got)
	}
}

func TestAnalyticsPeriodBounds(t *testing.T) {
	now := time.Date(2026, 6, 11, 15, 30, 0, 0, time.UTC) // Thursday
	hour := int64(3600_000)
	day := 24 * hour

	s, e, b := analyticsRange("today", 0, 0, now)
	if s != time.Date(2026, 6, 11, 0, 0, 0, 0, time.UTC).UnixMilli() {
		t.Fatalf("today start = %d", s)
	}
	if e != now.UnixMilli() {
		t.Fatalf("today end = %d, want now", e)
	}
	if b != hour {
		t.Fatalf("today bucket = %d, want hourly", b)
	}
	if _, _, by := analyticsRange("last_year", 0, 0, now); by != 30*day {
		t.Fatalf("last_year bucket = %d, want 30d", by)
	}
	from := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC).UnixMilli()
	to := time.Date(2026, 6, 11, 0, 0, 0, 0, time.UTC).UnixMilli()
	cs, ce, cb := analyticsRange("custom", from, to, now)
	if cs != from || ce != to {
		t.Fatalf("custom bounds = %d,%d want %d,%d", cs, ce, from, to)
	}
	if cb != day {
		t.Fatalf("custom 10d bucket = %d, want daily", cb)
	}
}

func TestMetricsMigrate(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := migrateMetricsDB(db); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO request_metrics (at_unix_ms) VALUES (1)`); err != nil {
		t.Fatalf("insert: %v", err)
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM request_metrics`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("count = %d err=%v", n, err)
	}
}

func TestAnalyticsAggregation(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "metrics.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := migrateMetricsDB(db); err != nil {
		t.Fatal(err)
	}
	prev := metricsDB
	metricsDB = db
	defer func() { metricsDB = prev; db.Close() }()

	day := int64(24) * 3600_000
	from := time.Date(2026, 6, 10, 0, 0, 0, 0, time.UTC).UnixMilli()
	to := time.Date(2026, 6, 12, 0, 0, 0, 0, time.UTC).UnixMilli() // 2 days -> daily buckets
	ins := func(at int64, status, dur, in, out, isErr int) {
		if _, err := db.Exec(`INSERT INTO request_metrics (at_unix_ms,status,duration_ms,input_tokens,output_tokens,is_error) VALUES (?,?,?,?,?,?)`,
			at, status, dur, in, out, isErr); err != nil {
			t.Fatal(err)
		}
	}
	ins(from+1000, 200, 100, 10, 5, 0)
	ins(from+2000, 500, 300, 20, 0, 1)
	ins(from+day+1000, 200, 200, 7, 3, 0)

	req := httptest.NewRequest(http.MethodGet,
		"/ui/api/analytics?period=custom&from="+strconv.FormatInt(from, 10)+"&to="+strconv.FormatInt(to, 10), nil)
	rec := httptest.NewRecorder()
	handleUIAnalytics(config{})(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		BucketMS int64 `json:"bucket_ms"`
		Buckets  []struct {
			Requests     int   `json:"requests"`
			AvgLatencyMs int64 `json:"avg_latency_ms"`
			TokensIn     int   `json:"tokens_in"`
			Errors       int   `json:"errors"`
		} `json:"buckets"`
		Totals struct {
			Requests  int     `json:"requests"`
			TokensIn  int     `json:"tokens_in"`
			TokensOut int     `json:"tokens_out"`
			Errors    int     `json:"errors"`
			ErrorRate float64 `json:"error_rate"`
		} `json:"totals"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v body=%s", err, rec.Body.String())
	}
	if resp.BucketMS != day {
		t.Fatalf("bucket_ms = %d, want %d (daily)", resp.BucketMS, day)
	}
	if len(resp.Buckets) != 2 {
		t.Fatalf("buckets = %d, want 2", len(resp.Buckets))
	}
	if resp.Buckets[0].Requests != 2 || resp.Buckets[1].Requests != 1 {
		t.Fatalf("bucket reqs = %d,%d want 2,1", resp.Buckets[0].Requests, resp.Buckets[1].Requests)
	}
	if resp.Buckets[0].AvgLatencyMs != 200 { // (100+300)/2
		t.Fatalf("day0 avg latency = %d, want 200", resp.Buckets[0].AvgLatencyMs)
	}
	if resp.Totals.Requests != 3 || resp.Totals.TokensIn != 37 || resp.Totals.TokensOut != 8 || resp.Totals.Errors != 1 {
		t.Fatalf("totals = %+v", resp.Totals)
	}
	if resp.Totals.ErrorRate < 33.0 || resp.Totals.ErrorRate > 33.4 { // 1/3
		t.Fatalf("error_rate = %v, want ~33.3", resp.Totals.ErrorRate)
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
	if resp.StreamError != "" {
		t.Fatalf("stream error = %q", resp.StreamError)
	}
	if len(resp.Output) < 2 {
		t.Fatalf("output = %#v", resp.Output)
	}
	if got := reasoningSummaryText(resp.Output[0]); got != "Checked the fix." {
		t.Fatalf("reasoning summary = %q, want Checked the fix.", got)
	}
}

func TestCollectCodexStreamCapturesStreamError(t *testing.T) {
	events := []responsesStreamEvent{
		{Type: "response.created"},
		{Type: "error", Error: responsesStreamError{Code: "context_length_exceeded", Message: "The request is too large."}},
		{Type: "response.failed", Response: responsesResponse{Error: responsesStreamError{Message: "Response failed."}}},
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
	if !strings.Contains(resp.StreamError, "context_length_exceeded") || !strings.Contains(resp.StreamError, "too large") {
		t.Fatalf("stream error = %q", resp.StreamError)
	}
}

func TestResponsesStreamEventErrorMessageIgnoresNormalDeltas(t *testing.T) {
	events := []responsesStreamEvent{
		{Type: "response.output_text.delta", Delta: "The answer is "},
		{Type: "response.output_text.done", Text: "The answer is done."},
		{Type: "response.reasoning_summary_text.delta", Delta: "Checked "},
		{Type: "response.reasoning_summary_text.done", Text: "Checked the logs."},
	}
	for _, event := range events {
		if msg := responsesStreamEventErrorMessage(event); msg != "" {
			t.Fatalf("%s reported stream error %q", event.Type, msg)
		}
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

func TestSubagentSummaryBlockReasonRequiresUsableHandoff(t *testing.T) {
	if reason := subagentSummaryBlockReason(""); !strings.Contains(reason, "empty") {
		t.Fatalf("empty summary reason = %q, want empty", reason)
	}
	if reason := subagentSummaryBlockReason("Done."); !strings.Contains(reason, "too short") {
		t.Fatalf("short summary reason = %q, want too short", reason)
	}
	incomplete := "Summary\nChecked the repository and found the service file. The work is described in prose, but the handoff omits the required headings."
	if reason := subagentSummaryBlockReason(incomplete); !strings.Contains(reason, "missing sections") {
		t.Fatalf("incomplete summary reason = %q, want missing sections", reason)
	}
}

func TestSubagentSummaryBlockReasonAcceptsStructuredHandoff(t *testing.T) {
	message := `Summary
Reviewed the proxy startup and browser MCP setup.

Key findings
- Browser MCP is configured separately from the proxy.

Files inspected or changed
- cli.go
- settings_sync.go

Checks run
- go test ./...

Blockers or risks
- None`
	if reason := subagentSummaryBlockReason(message); reason != "" {
		t.Fatalf("structured handoff was blocked: %q", reason)
	}
}

func TestEnsureClaudeIsolationHooksAddsSubagentStop(t *testing.T) {
	settings := map[string]any{}
	ensureClaudeIsolationHooks(settings)
	hooks := settings["hooks"].(map[string]any)
	if _, ok := hooks["SubagentStart"]; !ok {
		t.Fatal("SubagentStart hook was not configured")
	}
	if _, ok := hooks["SubagentStop"]; !ok {
		t.Fatal("SubagentStop hook was not configured")
	}
}

// sessionCookieExp decodes the username|exp payload from an admin session
// cookie value and returns the exp as a Unix timestamp.
func sessionCookieExp(t *testing.T, value string) int64 {
	t.Helper()
	parts := strings.SplitN(value, ".", 2)
	if len(parts) != 2 {
		t.Fatalf("cookie value %q is not payload.sig", value)
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		t.Fatalf("decode cookie payload: %v", err)
	}
	fields := strings.Split(string(raw), "|")
	if len(fields) != 2 {
		t.Fatalf("cookie payload %q is not username|exp", string(raw))
	}
	exp, err := strconv.ParseInt(fields[1], 10, 64)
	if err != nil {
		t.Fatalf("parse cookie exp %q: %v", fields[1], err)
	}
	return exp
}

func TestSetAdminSessionCookieRememberExpiry(t *testing.T) {
	cfg := config{AdminUsername: "admin", AdminSessionSecret: "test-secret"}
	const tolerance = 2 * 60 // seconds

	cases := []struct {
		name       string
		remember   bool
		wantMaxAge int
		ttl        time.Duration
	}{
		{"default", false, int(adminSessionTTL / time.Second), adminSessionTTL},
		{"remember", true, int(adminSessionRememberTTL / time.Second), adminSessionRememberTTL},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			before := time.Now()
			setAdminSessionCookie(rec, cfg, "admin", tc.remember)
			after := time.Now()

			res := rec.Result()
			cookies := res.Cookies()
			if len(cookies) != 1 {
				t.Fatalf("expected 1 cookie, got %d", len(cookies))
			}
			ck := cookies[0]
			if ck.Name != adminSessionCookieName {
				t.Fatalf("cookie name = %q, want %q", ck.Name, adminSessionCookieName)
			}
			if ck.MaxAge != tc.wantMaxAge {
				t.Fatalf("MaxAge = %d, want %d", ck.MaxAge, tc.wantMaxAge)
			}

			gotExp := sessionCookieExp(t, ck.Value)
			wantExpLo := before.Add(tc.ttl).Unix() - tolerance
			wantExpHi := after.Add(tc.ttl).Unix() + tolerance
			if gotExp < wantExpLo || gotExp > wantExpHi {
				t.Fatalf("payload exp = %d, want within [%d,%d]", gotExp, wantExpLo, wantExpHi)
			}

			// Cookie must round-trip through adminSessionValid.
			req := httptest.NewRequest(http.MethodGet, "/ui/api/status", nil)
			req.AddCookie(ck)
			if !adminSessionValid(cfg, req) {
				t.Fatalf("adminSessionValid rejected freshly issued cookie (remember=%v)", tc.remember)
			}
		})
	}
}

func TestLoginRememberFieldControlsSessionLength(t *testing.T) {
	// Isolate from any ambient admin env so the runtime config is fully ours.
	t.Setenv("ADMIN_USERNAME", "")
	t.Setenv("ADMIN_PASSWORD_HASH", "")
	t.Setenv("ADMIN_SESSION_SECRET", "")

	hash, err := hashAdminPassword("supersecret")
	if err != nil {
		t.Fatalf("hashAdminPassword: %v", err)
	}
	cfg := config{AdminUsername: "admin", AdminPasswordHash: hash, AdminSessionSecret: "test-secret"}
	handler := handleUIAuthLogin(cfg)

	cases := []struct {
		name       string
		body       string
		wantMaxAge int
	}{
		{"without remember", `{"username":"admin","password":"supersecret"}`, int(adminSessionTTL / time.Second)},
		{"with remember", `{"username":"admin","password":"supersecret","remember":true}`, int(adminSessionRememberTTL / time.Second)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/ui/api/auth/login", strings.NewReader(tc.body))
			rec := httptest.NewRecorder()
			handler(rec, req)

			res := rec.Result()
			if res.StatusCode != http.StatusOK {
				t.Fatalf("login status = %d, want 200; body=%s", res.StatusCode, rec.Body.String())
			}
			cookies := res.Cookies()
			if len(cookies) != 1 {
				t.Fatalf("expected 1 session cookie, got %d", len(cookies))
			}
			if cookies[0].MaxAge != tc.wantMaxAge {
				t.Fatalf("MaxAge = %d, want %d", cookies[0].MaxAge, tc.wantMaxAge)
			}
		})
	}
}

func TestUISlugRoutesServeSPA(t *testing.T) {
	dir := t.TempDir()
	uiDir := filepath.Join(dir, "ui")
	if err := os.MkdirAll(uiDir, 0o755); err != nil {
		t.Fatalf("mkdir ui: %v", err)
	}
	const marker = "<!--SPA-INDEX-MARKER-->"
	if err := os.WriteFile(filepath.Join(uiDir, "index.html"), []byte(marker), 0o644); err != nil {
		t.Fatalf("write index.html: %v", err)
	}
	t.Chdir(dir)

	allowed := []string{
		"/", "/dashboard", "/config", "/models", "/keys",
		"/test", "/logs", "/updates", "/browser", "/setup",
		"/dashboard/", // one trailing slash tolerated
	}
	for _, path := range allowed {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		handleUI(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s status = %d, want 200", path, rec.Code)
		}
		if !strings.Contains(rec.Body.String(), marker) {
			t.Fatalf("GET %s did not serve SPA index (marker missing)", path)
		}
	}

	for _, path := range []string{"/nope", "/dashboard/extra", "/api"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		handleUI(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("GET %s status = %d, want 404", path, rec.Code)
		}
	}

	// /health must not be shadowed by the SPA catch-all: it stays a JSON route.
	cfg := config{Models: map[string]string{"sonnet[1m]": "gpt-5.5"}}
	mux := newProxyMux(cfg)
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /health status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Fatalf("GET /health content-type = %q, want JSON", ct)
	}
	if strings.Contains(rec.Body.String(), marker) {
		t.Fatalf("GET /health was shadowed by the SPA index")
	}
}

func TestUIIndexFallsBackToLegacyFilename(t *testing.T) {
	dir := t.TempDir()
	uiDir := filepath.Join(dir, "ui")
	if err := os.MkdirAll(uiDir, 0o755); err != nil {
		t.Fatalf("mkdir ui: %v", err)
	}
	const marker = "<!--LEGACY-PANEL-MARKER-->"
	if err := os.WriteFile(filepath.Join(uiDir, "Connect AI Proxy Control Panel.html"), []byte(marker), 0o644); err != nil {
		t.Fatalf("write legacy panel: %v", err)
	}
	t.Chdir(dir)

	req := httptest.NewRequest(http.MethodGet, "/dashboard", nil)
	rec := httptest.NewRecorder()
	handleUI(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /dashboard status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), marker) {
		t.Fatalf("GET /dashboard did not fall back to the legacy panel filename")
	}
}

func TestUIAPIResponsesSetNoStore(t *testing.T) {
	cfg := config{Models: map[string]string{"sonnet[1m]": "gpt-5.5"}}
	mux := newProxyMux(cfg)

	for _, path := range []string{"/ui/api/auth/status", "/ui/api/status"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if got := rec.Header().Get("Cache-Control"); got != "no-store" {
			t.Fatalf("GET %s Cache-Control = %q, want %q", path, got, "no-store")
		}
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

func TestParseAgyjOutput(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		raw := []byte(`{"ok":true,"session_id":"abc","response":"hello from agy","model":"gemini-3-pro","duration_ms":5804,"exit_code":0}`)
		res, err := parseAgyjOutput(raw)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !res.Ok || res.Response != "hello from agy" || res.Model != "gemini-3-pro" {
			t.Fatalf("parsed = %#v", res)
		}
		if res.DurationMs != 5804 {
			t.Fatalf("duration = %d", res.DurationMs)
		}
	})
	t.Run("agy error stays a valid parse", func(t *testing.T) {
		raw := []byte(`{"ok":false,"error":"agy timed out after 600 seconds","exit_code":1}`)
		res, err := parseAgyjOutput(raw)
		if err != nil {
			t.Fatalf("ok=false must not be a parse error: %v", err)
		}
		if res.Ok {
			t.Fatal("expected Ok=false")
		}
		if res.Error != "agy timed out after 600 seconds" {
			t.Fatalf("error = %q", res.Error)
		}
	})
	t.Run("ok false without message gets a default", func(t *testing.T) {
		res, err := parseAgyjOutput([]byte(`{"ok":false}`))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if res.Error == "" {
			t.Fatal("expected a default error message")
		}
	})
	t.Run("empty", func(t *testing.T) {
		if _, err := parseAgyjOutput([]byte("   ")); err == nil {
			t.Fatal("expected error for empty output")
		}
	})
	t.Run("not json", func(t *testing.T) {
		if _, err := parseAgyjOutput([]byte("agy: command not found")); err == nil {
			t.Fatal("expected error for non-JSON output")
		}
	})
}

func TestFlattenAnthropicToPrompt(t *testing.T) {
	// Single user turn, no system → raw prompt (no role tags).
	single := anthropicRequest{Messages: []anthropicMessage{{Role: "user", Content: "What is 2+2?"}}}
	if got := flattenAnthropicToPrompt(single); got != "What is 2+2?" {
		t.Fatalf("single-turn prompt = %q", got)
	}

	// System + multi-turn → ordered, role-tagged transcript; tool blocks dropped.
	multi := anthropicRequest{
		System: "You are terse.",
		Messages: []anthropicMessage{
			{Role: "user", Content: "Hi"},
			{Role: "assistant", Content: "Hello."},
			{Role: "user", Content: []any{
				map[string]any{"type": "tool_result", "tool_use_id": "t1", "content": "ignored"},
				map[string]any{"type": "text", "text": "And now?"},
			}},
		},
	}
	got := flattenAnthropicToPrompt(multi)
	for _, want := range []string{"You are terse.", "User: Hi", "Assistant: Hello.", "User:", "And now?"} {
		if !strings.Contains(got, want) {
			t.Fatalf("prompt missing %q; got:\n%s", want, got)
		}
	}
	if strings.Index(got, "User: Hi") > strings.Index(got, "Assistant: Hello.") {
		t.Fatalf("turns out of order:\n%s", got)
	}
}

func TestForwardForAliasRouting(t *testing.T) {
	cfg := config{
		Upstream:     "codex",
		ModelForward: map[string]string{"agy-model": "agy", "claude-model": "claude", "plain": "codex"},
	}
	if got := forwardForAlias(cfg, "agy-model"); got != "agy" {
		t.Fatalf("agy-model → %q, want agy", got)
	}
	// Reserved-but-unwired targets and unknown aliases still resolve to codex,
	// so only an explicit "agy" diverts off the codex/openai path.
	if got := forwardForAlias(cfg, "claude-model"); got != "claude" {
		t.Fatalf("claude-model → %q, want claude (stored, but routing treats non-agy as codex)", got)
	}
	if got := forwardForAlias(cfg, "plain"); got != "codex" {
		t.Fatalf("plain → %q, want codex", got)
	}
	if got := forwardForAlias(cfg, "unknown"); got != "codex" {
		t.Fatalf("unknown → %q, want codex default", got)
	}
}

func TestAgyModelFor(t *testing.T) {
	// Global override wins.
	if got := agyModelFor(config{AgyModel: "gemini-x", Models: map[string]string{"m": "gpt-5.5"}}, "m"); got != "gemini-x" {
		t.Fatalf("override → %q, want gemini-x", got)
	}
	// A bare codex id left on an agy alias is dropped (agy would reject it).
	if got := agyModelFor(config{Models: map[string]string{"m": "gpt-5.5"}}, "m"); got != "" {
		t.Fatalf("bare gpt id → %q, want empty", got)
	}
	if got := agyModelFor(config{Models: map[string]string{"m": "o4-mini"}}, "m"); got != "" {
		t.Fatalf("bare o-series id → %q, want empty", got)
	}
	// Antigravity display names (always contain a space) are forwarded verbatim —
	// including "GPT-OSS 120B (Medium)", which must NOT be dropped by the gpt guard.
	for _, name := range []string{"Gemini 3.1 Pro (High)", "Claude Opus 4.6 (Thinking)", "GPT-OSS 120B (Medium)"} {
		if got := agyModelFor(config{Models: map[string]string{"m": name}}, "m"); got != name {
			t.Fatalf("antigravity model %q → %q, want verbatim", name, got)
		}
	}
}

func TestAgyResponseShape(t *testing.T) {
	resp := agyToResponsesResponse("the answer", "agy-model", 12)
	out := toAnthropicResponsesResponse(resp, "agy-model")
	if out["type"] != "message" || out["role"] != "assistant" {
		t.Fatalf("bad envelope: %#v", out)
	}
	content, ok := out["content"].([]any)
	if !ok || len(content) != 1 {
		t.Fatalf("content = %#v", out["content"])
	}
	block := content[0].(map[string]any)
	if block["type"] != "text" || block["text"] != "the answer" {
		t.Fatalf("text block = %#v", block)
	}
	usage := out["usage"].(map[string]any)
	if usage["input_tokens"] != 12 {
		t.Fatalf("input_tokens = %v, want 12", usage["input_tokens"])
	}
	if out["stop_reason"] != "end_turn" {
		t.Fatalf("stop_reason = %v", out["stop_reason"])
	}
}

func TestParseAgyConcurrencyAndTimeout(t *testing.T) {
	if parseAgyConcurrency("") != 2 || parseAgyConcurrency("0") != 2 || parseAgyConcurrency("x") != 2 {
		t.Fatal("bad/empty concurrency should default to 2")
	}
	if parseAgyConcurrency("3") != 3 {
		t.Fatal("concurrency 3 not honored")
	}
	if parseAgyConcurrency("999") != 16 {
		t.Fatal("concurrency should clamp to 16")
	}
	if parseAgyTimeout("") != 180*time.Second || parseAgyTimeout("0") != 180*time.Second {
		t.Fatal("bad/empty timeout should default to 180s")
	}
	if parseAgyTimeout("45") != 45*time.Second {
		t.Fatal("timeout 45 not honored")
	}
}
