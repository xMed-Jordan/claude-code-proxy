package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func testBookingRequest() anthropicRequest {
	return anthropicRequest{
		Model:  "gemini-3.8-flash-medium",
		System: "You are Zeina.",
		Tools: []anthropicTool{
			{Name: "get_tool_instructions", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"tool_code": map[string]any{"type": "string"}}}},
			{Name: "get_available_slots", Description: "Slots", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"branch_id": map[string]any{"type": "string"}}}},
			{Name: "create_reservation", InputSchema: map[string]any{"type": "object"}},
		},
		Messages: []anthropicMessage{
			{Role: "user", Content: `[AUTO-CONTEXT — the system already PREFLIGHTED (read the tool's instructions) AND executed the tool "find_user_by_phone" for this customer.]`},
			{Role: "user", Content: "بدي احجز موعد"},
			{Role: "assistant", Content: []any{map[string]any{"type": "tool_use", "id": "c1", "name": "get_tool_instructions", "input": map[string]any{"tool_code": "get_available_slots"}}}},
			{Role: "user", Content: []any{map[string]any{"type": "tool_result", "tool_use_id": "c1", "content": `{"name":"get_tool_instructions","result":{"success":true}}`}}},
			{Role: "assistant", Content: []any{map[string]any{"type": "tool_use", "id": "c2", "name": "get_available_slots", "input": map[string]any{"from_date": "2026-09-09", "branch_id": "2"}}}},
			{Role: "user", Content: []any{map[string]any{"type": "tool_result", "tool_use_id": "c2", "content": strings.Repeat("x", 500)}}},
			{Role: "assistant", Content: "متوفر 8:00 أو 9:00، أي وقت بناسبك؟"},
			{Role: "user", Content: "9 الصبح"},
			{Role: "user", Content: "[Tue, Sep 8, 12:34 PM | Phone: +962782400075 | Platform: whatsapp]\n9 الصبح"},
			{Role: "assistant", Content: []any{map[string]any{"type": "tool_use", "id": "c3", "name": "get_available_slots", "input": map[string]any{"branch_id": "2", "from_date": "2026-09-09"}}}},
			{Role: "user", Content: []any{map[string]any{"type": "tool_result", "tool_use_id": "c3", "content": `{"merged_starts":["08:00","09:00"]}`}}},
		},
	}
}

func TestAgyTranscriptBuild(t *testing.T) {
	tr := buildAgyTranscriptFromAnthropic(testBookingRequest())

	// The duplicated (plain + headered) customer message collapses to one turn.
	customers := 0
	for _, tt := range tr.Turns {
		if tt.Kind == agyTurnCustomer {
			customers++
		}
	}
	if customers != 2 {
		t.Fatalf("customer turns = %d, want 2 (duplicate headered copy collapsed)", customers)
	}
	last := tr.Turns[tr.lastCustomerIndex()]
	if !strings.HasPrefix(last.Text, "[Tue, Sep 8") || !strings.HasSuffix(last.Text, "9 الصبح") {
		t.Fatalf("kept the wrong copy of the customer message: %q", last.Text)
	}

	// Preflight cache: explicit get_tool_instructions, executed tools, AUTO-CONTEXT tools.
	pre := strings.Join(tr.preflightedTools(), ",")
	for _, want := range []string{"find_user_by_phone", "get_available_slots"} {
		if !strings.Contains(pre, want) {
			t.Fatalf("preflighted %q missing %q", pre, want)
		}
	}

	// Current-turn calls: only the slot call after the latest customer message.
	calls := tr.currentTurnCalls()
	if len(calls) != 1 || calls[0].Tool != "get_available_slots" {
		t.Fatalf("currentTurnCalls = %+v", calls)
	}
	if !tr.calledThisTurn("get_available_slots", `{"from_date":"2026-09-09","branch_id":"2"}`) {
		t.Fatal("identical call with different key order must be detected as a repeat")
	}
	if tr.calledThisTurn("get_available_slots", `{"branch_id":"2","from_date":"2026-09-10"}`) {
		t.Fatal("different arguments must not be flagged as a repeat")
	}
	if tr.calledThisTurn("create_reservation", `{}`) {
		t.Fatal("a tool never called this turn must not be flagged")
	}

	// Tool results pair with their calls by id and carry the tool name.
	for _, tt := range tr.Turns {
		if tt.Kind == agyTurnToolResult && tt.CallID == "c2" && tt.Tool != "get_available_slots" {
			t.Fatalf("tool result c2 paired with %q", tt.Tool)
		}
	}
}

func TestRenderAgyPromptStructure(t *testing.T) {
	in := testBookingRequest()
	cfg := config{AgyToolResultCap: 100}
	prompt := renderAgyPrompt(cfg, contentToTextNoMedia(in.System), nil, agyCatalogFromAnthropic(in.Tools), buildAgyTranscriptFromAnthropic(in))

	for _, want := range []string{
		"### SYSTEM INSTRUCTIONS & POLICIES",
		"You are Zeina.",
		"### TOOLS",
		`Input schema: {"properties":{"branch_id":{"type":"string"}},"type":"object"}`,
		"### CONVERSATION SO FAR",
		"[System note to you]: [AUTO-CONTEXT",
		"[Customer]: بدي احجز موعد",
		"[You called tool get_available_slots (c3)]: {\"branch_id\":\"2\",\"from_date\":\"2026-09-09\"}",
		"[Result of your get_available_slots call (c3)]: {\"merged_starts\"",
		"Your previous message to the customer:\nYou: متوفر 8:00 أو 9:00، أي وقت بناسبك؟",
		"Customer: 9 الصبح",
		"### YOUR NEXT TURN",
		"do NOT fetch them again): find_user_by_phone, get_available_slots",
		"• get_available_slots {\"branch_id\":\"2\",\"from_date\":\"2026-09-09\"}",
		"The transcript ends with a tool result",
		"…[truncated by the system: 400 more characters not shown]",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt missing %q\n---\n%s", want, prompt)
		}
	}
	// The state preface must precede the system instructions (reading order is
	// what stops the model from restarting an already-executed protocol flow).
	iState := strings.Index(prompt, "### CURRENT STATE (read before the instructions)")
	iSys := strings.Index(prompt, "### SYSTEM INSTRUCTIONS & POLICIES")
	if iState < 0 || iSys < 0 || iState > iSys {
		t.Fatalf("state preface must come before the system prompt: state=%d system=%d", iState, iSys)
	}
	if !strings.Contains(prompt[iState:iSys], "You have ALREADY executed these tools") || !strings.Contains(prompt[iState:iSys], "get_available_slots {\"branch_id\":\"2\",\"from_date\":\"2026-09-09\"}") {
		t.Fatalf("state preface lacks the executed-tools list:\n%s", prompt[iState:iSys])
	}
	// Nothing from the legacy heuristic layer may leak in.
	for _, banned := range []string{"CRITICAL DIRECTIVE", "APPOINTMENT", "CHRONOLOGICAL CUSTOMER STATEMENTS", "NEVER call `get_tool_instructions`"} {
		if strings.Contains(prompt, banned) {
			t.Fatalf("prompt must not contain legacy directive %q", banned)
		}
	}
	// Current-turn tool results are never truncated.
	if strings.Count(prompt, "truncated by the system") != 1 {
		t.Fatalf("only the older tool result should be truncated:\n%s", prompt)
	}
}

func TestRenderAgyPromptBareChat(t *testing.T) {
	tr := buildAgyTranscriptFromAnthropic(anthropicRequest{Messages: []anthropicMessage{{Role: "user", Content: "What is 2+2?"}}})
	if got := renderAgyPrompt(config{}, "", nil, nil, tr); got != "What is 2+2?" {
		t.Fatalf("bare single-turn prompt = %q", got)
	}
}

func TestAgyGenerateCorrectsRepeatedCall(t *testing.T) {
	in := testBookingRequest()
	var prompts []string
	replies := []string{
		"<tool_call>\n{\"tool\":\"get_available_slots\",\"input\":{\"branch_id\":\"2\",\"from_date\":\"2026-09-09\"}}\n</tool_call>",
		"<tool_call>\n{\"tool\":\"get_tool_instructions\",\"input\":{\"tool_code\":\"create_reservation\"}}\n</tool_call>",
	}
	orig := agyResolveFn
	agyResolveFn = func(_ context.Context, _ config, _ []mediaPart, prompt, _ string) (agyResult, error) {
		prompts = append(prompts, prompt)
		r := replies[0]
		if len(replies) > 1 {
			replies = replies[1:]
		}
		return agyResult{Ok: true, Response: r}, nil
	}
	defer func() { agyResolveFn = orig }()

	resp, trace, err := agyGenerate(context.Background(), config{}, agyGenInputFromAnthropic(in, 10))
	if err != nil {
		t.Fatal(err)
	}
	if len(trace.Attempts) != 2 {
		t.Fatalf("attempts = %d, want 2 (one correction)", len(trace.Attempts))
	}
	if !strings.Contains(prompts[1], "### CORRECTION FROM THE SYSTEM") || !strings.Contains(prompts[1], "do not repeat the same call") || strings.Contains(prompts[1], "already SUCCEEDED") {
		t.Fatalf("second prompt lacks the correction note:\n%s", prompts[1][len(prompts[1])-600:])
	}
	if !strings.HasPrefix(prompts[1], prompts[0]) {
		t.Fatal("the correction retry must keep the full original prompt")
	}
	if len(resp.Output) != 1 || resp.Output[0].Type != "function_call" || resp.Output[0].Name != "get_tool_instructions" {
		t.Fatalf("final output = %+v", resp.Output)
	}
}

func TestAgyGenerateRepeatAfterSuccessIsNamed(t *testing.T) {
	in := testBookingRequest()
	// Current turn: create_reservation already succeeded; the model repeats it.
	in.Messages = append(in.Messages,
		anthropicMessage{Role: "assistant", Content: []any{map[string]any{"type": "tool_use", "id": "c4", "name": "create_reservation", "input": map[string]any{"date": "2026-09-09", "time_from": "09:00"}}}},
		anthropicMessage{Role: "user", Content: []any{map[string]any{"type": "tool_result", "tool_use_id": "c4", "content": `{"success":true,"data":{"status":201,"body":{"reservation_id":1}}}`}}},
	)
	var prompts []string
	replies := []string{
		"<tool_call>\n{\"tool\":\"create_reservation\",\"input\":{\"time_from\":\"09:00\",\"date\":\"2026-09-09\"}}\n</tool_call>",
		"تم تثبيت موعدك الأربعاء الساعة 9:00 صباحاً.",
	}
	orig := agyResolveFn
	agyResolveFn = func(_ context.Context, _ config, _ []mediaPart, prompt, _ string) (agyResult, error) {
		prompts = append(prompts, prompt)
		r := replies[0]
		if len(replies) > 1 {
			replies = replies[1:]
		}
		return agyResult{Ok: true, Response: r}, nil
	}
	defer func() { agyResolveFn = orig }()
	resp, _, err := agyGenerate(context.Background(), config{}, agyGenInputFromAnthropic(in, 10))
	if err != nil {
		t.Fatal(err)
	}
	if len(prompts) != 2 || !strings.Contains(prompts[1], "already SUCCEEDED") {
		t.Fatalf("expected the correction to name the prior success; prompts=%d", len(prompts))
	}
	if hasAnyToolCall(resp.Output) || !strings.Contains(agyResponseText(resp), "تم تثبيت") {
		t.Fatalf("unexpected final output %+v", resp.Output)
	}
}

func TestAgyGenerateForcesTextAfterPersistentRepeat(t *testing.T) {
	in := testBookingRequest()
	repeat := "<tool_call>\n{\"tool\":\"get_available_slots\",\"input\":{\"branch_id\":\"2\",\"from_date\":\"2026-09-09\"}}\n</tool_call>"
	calls := 0
	orig := agyResolveFn
	agyResolveFn = func(_ context.Context, _ config, _ []mediaPart, prompt, _ string) (agyResult, error) {
		calls++
		if strings.Contains(prompt, "Do NOT call any tool now") {
			return agyResult{Ok: true, Response: "متوفر الساعة 9:00 الصبح، بتأكدي الحجز؟"}, nil
		}
		return agyResult{Ok: true, Response: repeat}, nil
	}
	defer func() { agyResolveFn = orig }()

	resp, _, err := agyGenerate(context.Background(), config{}, agyGenInputFromAnthropic(in, 10))
	if err != nil {
		t.Fatal(err)
	}
	if calls != agyMaxCorrectionRetries+2 {
		t.Fatalf("upstream calls = %d, want %d", calls, agyMaxCorrectionRetries+2)
	}
	if hasAnyToolCall(resp.Output) || !strings.Contains(agyResponseText(resp), "9:00") {
		t.Fatalf("expected a forced plain-text reply, got %+v", resp.Output)
	}
}

func TestAgyGenerateDropsUnknownTool(t *testing.T) {
	in := testBookingRequest()
	replies := []string{
		"<tool_call>\n{\"tool\":\"book_now\",\"input\":{}}\n</tool_call>",
		"تمام، بثبتلك الموعد.",
	}
	orig := agyResolveFn
	agyResolveFn = func(_ context.Context, _ config, _ []mediaPart, _ string, _ string) (agyResult, error) {
		r := replies[0]
		if len(replies) > 1 {
			replies = replies[1:]
		}
		return agyResult{Ok: true, Response: r}, nil
	}
	defer func() { agyResolveFn = orig }()
	resp, trace, err := agyGenerate(context.Background(), config{}, agyGenInputFromAnthropic(in, 10))
	if err != nil {
		t.Fatal(err)
	}
	if len(trace.Attempts) != 2 || !strings.Contains(trace.Attempts[0].Problems[0], "does not exist") {
		t.Fatalf("trace = %+v", trace.Attempts)
	}
	if hasAnyToolCall(resp.Output) {
		t.Fatalf("unknown tool must not reach the caller: %+v", resp.Output)
	}
}

func TestStripSimulatedToolResultsNewLabel(t *testing.T) {
	text := "[Result of your get_memory call (c9)]: {\"success\":true}\n\n[Tool Result (c10)]: {\"x\":1}\n\nأهلاً فايزة."
	if got := stripSimulatedToolResults(text); strings.Contains(got, "call (c9)") || strings.Contains(got, "Tool Result") || !strings.Contains(got, "أهلاً فايزة.") {
		t.Fatalf("got %q", got)
	}
}

func TestUnescapeJSONUnicode(t *testing.T) {
	in := `{"name":"\u0641\u0631\u062d","url":"https:\/\/x.y","q":"say \u0022hi\u0022 \\u0041","emoji":"\ud83d\ude00","ctl":"a\u0001b\n"}`
	got := unescapeJSONUnicode(in)
	want := `{"name":"فرح","url":"https://x.y","q":"say \u0022hi\u0022 \\u0041","emoji":"😀","ctl":"a\u0001b\n"}`
	if got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
	var v map[string]any
	if err := json.Unmarshal([]byte(got), &v); err != nil {
		t.Fatalf("result is not valid JSON: %v", err)
	}
	if v["name"] != "فرح" || v["q"] != `say "hi" \u0041` || v["emoji"] != "😀" {
		t.Fatalf("decoded values wrong: %#v", v)
	}
	if s := "plain text without escapes"; unescapeJSONUnicode(s) != s {
		t.Fatal("plain text must pass through unchanged")
	}
}

func TestRenderAgyPromptSectionOrderAndDigest(t *testing.T) {
	in := testBookingRequest()
	prompt := renderAgyPrompt(config{}, contentToTextNoMedia(in.System), nil, agyCatalogFromAnthropic(in.Tools), buildAgyTranscriptFromAnthropic(in))
	iHow := strings.Index(prompt, "### HOW TO READ THIS MESSAGE")
	iState := strings.Index(prompt, "### CURRENT STATE")
	iSys := strings.Index(prompt, "### SYSTEM INSTRUCTIONS & POLICIES")
	iTools := strings.Index(prompt, "### TOOLS")
	iTr := strings.Index(prompt, "### CONVERSATION SO FAR")
	iDg := strings.Index(prompt, "### DIALOGUE DIGEST")
	iNext := strings.Index(prompt, "### YOUR NEXT TURN")
	if !(iHow == 0 && iHow < iState && iState < iSys && iSys < iTools && iTools < iTr && iTr < iDg && iDg < iNext) {
		t.Fatalf("section order wrong: how=%d state=%d system=%d tools=%d transcript=%d digest=%d next=%d", iHow, iState, iSys, iTools, iTr, iDg, iNext)
	}
	if !strings.Contains(prompt, "Customer: 9 الصبح\n") || !strings.Contains(prompt, "You: متوفر 8:00 أو 9:00") {
		t.Fatalf("digest missing dialogue lines:\n%s", prompt[iDg:iNext])
	}
	digest := prompt[iDg:iNext]
	if strings.Contains(digest, "tool call") && strings.Contains(digest, "get_available_slots") || strings.Contains(digest, "Tool result") || strings.Contains(digest, "merged_starts") {
		t.Fatalf("digest must not contain tool activity:\n%s", digest)
	}
}

func TestCanonicalToolArgs(t *testing.T) {
	if got := canonicalToolArgs(map[string]any{"b": 1, "a": "x"}); got != `{"a":"x","b":1}` {
		t.Fatalf("got %q", got)
	}
	if got := canonicalToolArgs(`{ "b" : 1, "a":"x" }`); got != `{"a":"x","b":1}` {
		t.Fatalf("got %q", got)
	}
	if got := canonicalToolArgs(nil); got != "{}" {
		t.Fatalf("got %q", got)
	}
}
