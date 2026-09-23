package main

// agy_usage_test.go — tests for v0.31.0's real-usage plumbing (Change 1):
// agyTraceUsage, agyApplyRealUsage, and their wiring into serveAgyAnthropic
// (end to end, both the JSON and streaming paths) and serveAgyResponses.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAgyTraceUsageSumsAllAttempts(t *testing.T) {
	trace := &agyGenTrace{Attempts: []agyGenAttempt{
		{Usage: agyResult{InputTokens: 1000, OutputTokens: 200, ThinkingTokens: 150}},
		{Usage: agyResult{InputTokens: 500, OutputTokens: 100, ThinkingTokens: 80}},
	}}
	in, out := agyTraceUsage(trace)
	if in != 1500 {
		t.Fatalf("in = %d, want 1500 (sum of both attempts)", in)
	}
	if out != 300 {
		t.Fatalf("out = %d, want 300 (sum of both attempts)", out)
	}
}

func TestAgyTraceUsageNilAndEmpty(t *testing.T) {
	if in, out := agyTraceUsage(nil); in != 0 || out != 0 {
		t.Fatalf("nil trace: got (%d, %d), want (0, 0)", in, out)
	}
	if in, out := agyTraceUsage(&agyGenTrace{}); in != 0 || out != 0 {
		t.Fatalf("empty trace: got (%d, %d), want (0, 0)", in, out)
	}
}

func TestAgyApplyRealUsage(t *testing.T) {
	resp := responsesResponse{}
	resp.Usage.InputTokens = 42 // the estimate, pre-real-usage
	resp.Usage.OutputTokens = 7

	agyApplyRealUsage(&resp, 0, 0) // unknown — must leave the estimate alone
	if resp.Usage.InputTokens != 42 || resp.Usage.OutputTokens != 7 {
		t.Fatalf("unknown usage changed the estimate: %+v", resp.Usage)
	}

	agyApplyRealUsage(&resp, 16378, 4860)
	if resp.Usage.InputTokens != 16378 {
		t.Fatalf("InputTokens = %d, want 16378", resp.Usage.InputTokens)
	}
	if resp.Usage.OutputTokens != 4860 {
		t.Fatalf("OutputTokens = %d, want 4860", resp.Usage.OutputTokens)
	}

	// Only one side known (e.g. input known, output still unknown) — only
	// that side updates.
	resp2 := responsesResponse{}
	resp2.Usage.OutputTokens = 99
	agyApplyRealUsage(&resp2, 123, 0)
	if resp2.Usage.InputTokens != 123 {
		t.Fatalf("InputTokens = %d, want 123", resp2.Usage.InputTokens)
	}
	if resp2.Usage.OutputTokens != 99 {
		t.Fatalf("OutputTokens = %d, want 99 (left alone)", resp2.Usage.OutputTokens)
	}
}

// fakeAgyResolveFn installs a stand-in for agyResolveFn (the seam agyGenerate
// itself calls) that always returns the same canned agyResult, restoring
// the real function via t.Cleanup.
func fakeAgyResolveFn(t *testing.T, res agyResult) {
	t.Helper()
	orig := agyResolveFn
	agyResolveFn = func(_ context.Context, _ config, _ []mediaPart, _ string, _ string) (agyResult, error) {
		return res, nil
	}
	t.Cleanup(func() { agyResolveFn = orig })
}

// TestServeAgyAnthropicUsesRealUsageEndToEnd drives serveAgyAnthropic
// through the real v2 path (agyGenerate -> agyResolveFn), with
// agyResolveFn returning agy's real production evidence numbers
// (in=16378 out=4860 think=4619, 2026-09-23 journal). The JSON response's
// usage block and the recorded request stat must both carry the REAL
// numbers, not the request-size estimate.
func TestServeAgyAnthropicUsesRealUsageEndToEnd(t *testing.T) {
	fakeAgyResolveFn(t, agyResult{Ok: true, Response: "Sure, here is the answer you asked for.", InputTokens: 16378, OutputTokens: 4860, ThinkingTokens: 4619})

	in := anthropicRequest{
		Model:    "agy",
		Messages: []anthropicMessage{{Role: "user", Content: "what does the receipt say?"}},
	}
	r := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", nil)
	w := httptest.NewRecorder()
	serveAgyAnthropic(context.Background(), config{}, in, w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}

	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v; body=%s", err, w.Body.String())
	}
	usage, ok := body["usage"].(map[string]any)
	if !ok {
		t.Fatalf("no usage object in response: %s", w.Body.String())
	}
	if got, want := int(usage["input_tokens"].(float64)), 16378; got != want {
		t.Fatalf("usage.input_tokens = %d, want %d", got, want)
	}
	if got, want := int(usage["output_tokens"].(float64)), 4860; got != want {
		t.Fatalf("usage.output_tokens = %d, want %d", got, want)
	}

	stat := takeRequestStat(r)
	if stat.InputTokens != 16378 {
		t.Fatalf("requestStat.InputTokens = %d, want 16378 (the estimate was recorded first via setRequestStat, then must be overwritten with the real count)", stat.InputTokens)
	}
	if stat.OutputTokens != 4860 {
		t.Fatalf("requestStat.OutputTokens = %d, want 4860", stat.OutputTokens)
	}
}

// TestServeAgyAnthropicStreamingMessageStartUsesRealInput covers the
// streaming variant: message_start must carry the real input token count,
// not estimateAnthropicRequestTokens' estimate.
func TestServeAgyAnthropicStreamingMessageStartUsesRealInput(t *testing.T) {
	fakeAgyResolveFn(t, agyResult{Ok: true, Response: "Sure.", InputTokens: 16378, OutputTokens: 4860, ThinkingTokens: 4619})

	in := anthropicRequest{
		Model:    "agy",
		Stream:   true,
		Messages: []anthropicMessage{{Role: "user", Content: "what does the receipt say?"}},
	}
	r := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", nil)
	w := httptest.NewRecorder()
	serveAgyAnthropic(context.Background(), config{}, in, w, r)

	body := w.Body.String()
	if !strings.Contains(body, `"input_tokens":16378`) {
		t.Fatalf("message_start did not carry the real input token count:\n%s", body)
	}
	// The small ~9-char prompt would estimate to a single-digit token count —
	// make sure that number is nowhere in the stream as the reported input.
	if strings.Contains(body, `"input_tokens":1,`) || strings.Contains(body, `"input_tokens":2,`) {
		t.Fatalf("message_start looks like it used the tiny prompt-size estimate instead of the real count:\n%s", body)
	}
}

// TestServeAgyResponsesUsesRealUsageEndToEnd covers the codex/responses
// handler the same way as serveAgyAnthropic above (the brief asks for at
// least one of the other two handlers covered the same way).
func TestServeAgyResponsesUsesRealUsageEndToEnd(t *testing.T) {
	fakeAgyResolveFn(t, agyResult{Ok: true, Response: "Sure, here is the answer you asked for.", InputTokens: 16378, OutputTokens: 4860, ThinkingTokens: 4619})

	in := responsesRequest{
		Model: "agy",
		Input: []any{
			map[string]any{"type": "message", "role": "user", "content": []any{
				map[string]any{"type": "input_text", "text": "what does the receipt say?"},
			}},
		},
	}
	r := httptest.NewRequest(http.MethodPost, "/openai/v1/responses", nil)
	w := httptest.NewRecorder()
	serveAgyResponses(context.Background(), config{}, in, w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v; body=%s", err, w.Body.String())
	}
	usage, ok := body["usage"].(map[string]any)
	if !ok {
		t.Fatalf("no usage object in response: %s", w.Body.String())
	}
	if got, want := int(usage["input_tokens"].(float64)), 16378; got != want {
		t.Fatalf("usage.input_tokens = %d, want %d", got, want)
	}
	if got, want := int(usage["output_tokens"].(float64)), 4860; got != want {
		t.Fatalf("usage.output_tokens = %d, want %d", got, want)
	}

	stat := takeRequestStat(r)
	if stat.InputTokens != 16378 {
		t.Fatalf("requestStat.InputTokens = %d, want 16378", stat.InputTokens)
	}
	if stat.OutputTokens != 4860 {
		t.Fatalf("requestStat.OutputTokens = %d, want 4860", stat.OutputTokens)
	}
}
