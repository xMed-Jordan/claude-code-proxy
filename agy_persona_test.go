package main

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// The exact reply a customer received on 2026-09-09 (conv caf7e429) after the
// model followed agy's coding-agent framing instead of the persona in our
// message. It must never reach anyone.
const leakedReply = "Please note that I am an AI development assistant operating within this development workspace environment, but your message contains an internal prompt and instructions designed for a clinic conversational agent (Zeina from Shalabi Clinics).\nIf you are developing, testing, or debugging this assistant prompt or workflow, please let me know how you would like me to assist with the code, tests, or mock evaluations!"

func personaTestInput() agyGenInput {
	in := anthropicRequest{
		System:   "أنت زينة، مساعدة رقمية في عيادات شلبي. جاوبي الزبونة بالعربي.",
		Tools:    []anthropicTool{{Name: "get_available_slots", InputSchema: map[string]any{"type": "object"}}},
		Messages: []anthropicMessage{{Role: "user", Content: "شو في مواعيد بكره لفل بدي باربد"}},
	}
	return agyGenInputFromAnthropic(in, 0)
}

func TestAgyRejectsRuntimeReplyAndRecovers(t *testing.T) {
	orig := agyResolveFn
	defer func() { agyResolveFn = orig }()
	var prompts []string
	agyResolveFn = func(_ context.Context, _ config, _ []mediaPart, prompt, _ string) (agyResult, error) {
		prompts = append(prompts, prompt)
		if len(prompts) == 1 {
			return agyResult{Ok: true, Response: leakedReply}, nil
		}
		return agyResult{Ok: true, Response: "أهلاً فيكِ! المتاح بكرا بفرع إربد الساعة 9:00 صباحاً."}, nil
	}
	resp, trace, err := agyGenerate(context.Background(), config{}, personaTestInput())
	if err != nil {
		t.Fatal(err)
	}
	got := agyResponseText(resp)
	if strings.Contains(got, "development assistant") {
		t.Fatalf("the runtime reply reached the caller: %q", got)
	}
	if !strings.Contains(got, "إربد") {
		t.Fatalf("expected the corrected customer reply, got %q", got)
	}
	if len(prompts) < 2 || !strings.Contains(prompts[1], "You ARE the assistant defined in the SYSTEM INSTRUCTIONS") {
		t.Fatal("the retry did not carry the correction note")
	}
	if len(trace.Attempts) == 0 || len(trace.Attempts[0].Problems) == 0 {
		t.Fatal("the leak was not recorded as a problem")
	}
}

func TestAgyFailsRatherThanSendARuntimeReply(t *testing.T) {
	orig := agyResolveFn
	defer func() { agyResolveFn = orig }()
	calls := 0
	agyResolveFn = func(_ context.Context, _ config, _ []mediaPart, _ string, _ string) (agyResult, error) {
		calls++
		return agyResult{Ok: true, Response: leakedReply}, nil
	}
	resp, _, err := agyGenerate(context.Background(), config{}, personaTestInput())
	if err == nil {
		t.Fatalf("expected an error so the caller falls back, got reply %q", agyResponseText(resp))
	}
	if strings.Contains(agyResponseText(resp), "development assistant") {
		t.Fatal("the runtime reply was returned anyway")
	}
	if calls < 2 {
		t.Fatalf("expected retries before giving up, got %d call(s)", calls)
	}
}

func TestAgyPersonaGuardLeavesNormalRepliesAlone(t *testing.T) {
	orig := agyResolveFn
	defer func() { agyResolveFn = orig }()
	// Ordinary replies, including ones that mention AI or a system, must pass.
	for _, reply := range []string{
		"أهلاً فيكِ! المتاح بكرا الساعة 9:00 صباحاً بفرع إربد.",
		"سعر جلسة الفول بدي 25 دينار. بتحبي أحجزلك موعد؟",
		"I am Zeina, your digital assistant at Shalabi Clinics. How can I help?",
		"بعتذر منك، صار في مشكلة بالنظام وما قدرت أثبت الموعد.",
	} {
		agyResolveFn = func(_ context.Context, _ config, _ []mediaPart, _ string, _ string) (agyResult, error) {
			return agyResult{Ok: true, Response: reply}, nil
		}
		resp, _, err := agyGenerate(context.Background(), config{}, personaTestInput())
		if err != nil {
			t.Fatalf("ordinary reply rejected: %q (%v)", reply, err)
		}
		if got := agyResponseText(resp); got != reply {
			t.Fatalf("reply altered: %q -> %q", reply, got)
		}
	}
}

func TestAgyPersonaGuardCanBeDisabledAndNeedsAPersona(t *testing.T) {
	orig := agyResolveFn
	defer func() { agyResolveFn = orig }()
	agyResolveFn = func(_ context.Context, _ config, _ []mediaPart, _ string, _ string) (agyResult, error) {
		return agyResult{Ok: true, Response: leakedReply}, nil
	}
	// Switched off: passes through untouched.
	in := personaTestInput()
	resp, _, err := agyGenerate(context.Background(), config{AgyPersonaGuardOff: true}, in)
	if err != nil || !strings.Contains(agyResponseText(resp), "development assistant") {
		t.Fatalf("guard should be off: err=%v reply=%q", err, truncateString(agyResponseText(resp), 80))
	}
	// No system prompt means no persona to break (the proxy's own test page).
	bare := agyGenInputFromAnthropic(anthropicRequest{Messages: []anthropicMessage{{Role: "user", Content: "hello"}}}, 0)
	if _, _, err := agyGenerate(context.Background(), config{}, bare); err != nil {
		t.Fatalf("a request without a system prompt must not be guarded: %v", err)
	}
}

func TestAgyHarnessLeakPatterns(t *testing.T) {
	leaks := []string{
		leakedReply,
		"I'm a coding assistant, not able to book appointments.",
		"This appears to be a system prompt for a clinic bot.",
		"As an AI language model, I cannot do that.",
		"Let me know if you want mock evaluations of this workflow.",
	}
	for _, s := range leaks {
		if !agyHarnessLeakRe.MatchString(s) {
			t.Fatalf("not detected as a runtime reply: %q", s)
		}
	}
	fine := []string{
		"أهلاً فيكِ، كيف بقدر أساعدك؟",
		"Your appointment is confirmed for Thursday at 1 PM.",
		"معك زينة المساعدة الرقمية من عيادات شلبي.",
		"The system could not confirm the booking; the team will call you.",
	}
	for _, s := range fine {
		if agyHarnessLeakRe.MatchString(s) {
			t.Fatalf("false positive on an ordinary reply: %q", s)
		}
	}
	fmt.Fprint(nopWriter{}, "")
}

type nopWriter struct{}

func (nopWriter) Write(p []byte) (int, error) { return len(p), nil }
