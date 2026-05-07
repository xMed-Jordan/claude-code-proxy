package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
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
	block := content[0].(map[string]any)
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
