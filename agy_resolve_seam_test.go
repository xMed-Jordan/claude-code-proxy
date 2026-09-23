package main

// agy_resolve_seam_test.go — tests for two production-routing lines that
// review of v0.29.1 found no test covered (v0.29.2):
//
//   - B4: agyResolve's final dispatch picking runAgyj (default agent) vs
//     runAgyjAgent (explicit agentArgs) via `if agentArgs != nil`. Both
//     branches were folded into one call through the agyRunAgentFn seam
//     below, but the SELECTION of what agentArgs to pass is exactly what a
//     mutation could silently break again, with the whole existing
//     media/agent test set staying green (those tests exercise
//     agyMediaPrep and parseAgyMediaAgent directly, never agyResolve's own
//     final dispatch).
//   - B2: runAgyjAgent's non-stream-json ("agyj wrapper") path building its
//     argv from agentArgs — extracted into the pure agyjWrapperArgs so it is
//     directly testable without spawning agyj.

import (
	"context"
	"encoding/base64"
	"strings"
	"testing"
)

// TestAgyjWrapperArgs covers B2: the agyj-wrapper argv must actually carry
// whatever agentArgs it was given (not recompute a different selection from
// cfg), must include every --add-dir, and must end with "-p <prompt>".
func TestAgyjWrapperArgs(t *testing.T) {
	args := agyjWrapperArgs("Gemini 3.6 Flash (Low)", "the prompt text", []string{"/tmp/a", "/tmp/b"}, []string{"--agent", agyMediaViewAgentName})

	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "--agent "+agyMediaViewAgentName) {
		t.Fatalf("args missing the view-only --agent flag: %q", joined)
	}
	if !strings.Contains(joined, "--add-dir /tmp/a") || !strings.Contains(joined, "--add-dir /tmp/b") {
		t.Fatalf("args missing an --add-dir: %q", joined)
	}
	if !strings.Contains(joined, "--model Gemini 3.6 Flash (Low)") {
		t.Fatalf("args missing --model: %q", joined)
	}
	if !strings.Contains(joined, "--dangerously-skip-permissions") {
		t.Fatalf("args missing --dangerously-skip-permissions: %q", joined)
	}
	if len(args) < 2 || args[len(args)-2] != "-p" || args[len(args)-1] != "the prompt text" {
		t.Fatalf("expected \"-p <prompt>\" as the LAST two args, got %v", args)
	}

	// A chat run (no addDirs, connect-chat's --agent) must still carry its
	// agentArgs and still end with -p <prompt>.
	chatArgs := agyjWrapperArgs("", "hi", nil, []string{"--agent", agyChatAgentName})
	if got := strings.Join(chatArgs, " "); !strings.Contains(got, "--agent "+agyChatAgentName) {
		t.Fatalf("chat run args = %q, want --agent %s", got, agyChatAgentName)
	}
	if len(chatArgs) < 2 || chatArgs[len(chatArgs)-2] != "-p" || chatArgs[len(chatArgs)-1] != "hi" {
		t.Fatalf("expected \"-p hi\" as the last two args, got %v", chatArgs)
	}
}

// fakeAgyRunAgentFn installs a stand-in for agyRunAgentFn that records the
// call it received and returns a canned success, restoring the real
// function via t.Cleanup.
func fakeAgyRunAgentFn(t *testing.T) *struct {
	Called    bool
	Model     string
	Prompt    string
	AddDirs   []string
	AgentArgs []string
} {
	t.Helper()
	rec := &struct {
		Called    bool
		Model     string
		Prompt    string
		AddDirs   []string
		AgentArgs []string
	}{}
	orig := agyRunAgentFn
	agyRunAgentFn = func(ctx context.Context, cfg config, prompt, model string, addDirs []string, agentArgs []string) (agyResult, error) {
		rec.Called = true
		rec.Model = model
		rec.Prompt = prompt
		rec.AddDirs = addDirs
		rec.AgentArgs = agentArgs
		return agyResult{Ok: true, Response: "canned"}, nil
	}
	t.Cleanup(func() { agyRunAgentFn = orig })
	return rec
}

// (a) one tiny PNG, AgyMediaAgent set -> agyRunAgentFn receives
// [--agent connect-media-view], non-empty addDirs, and a view_file prompt.
// This is exactly the case B4's mutation (never take the view-only branch)
// breaks — see TestAgyResolveB4MutationControl below.
func TestAgyResolveRoutesViewEligibleImageThroughSeam(t *testing.T) {
	initAgyWorkerPool(config{AgyWarmWorkers: 0}) // the warm pool must not be used
	setVTRuntime("", false)                      // no network from a unit test

	rec := fakeAgyRunAgentFn(t)

	cfg := config{AgyMedia: true, AgyMediaDir: t.TempDir(), AgyMediaAgent: agyMediaViewAgentName}
	parts := []mediaPart{{B64: tinyPNGBase64(t), MediaType: "image/png", Filename: "receipt.png"}}

	res, err := agyResolve(context.Background(), cfg, parts, "what does it say?", "some-alias")
	if err != nil {
		t.Fatalf("agyResolve: %v", err)
	}
	if !res.Ok || res.Response != "canned" {
		t.Fatalf("unexpected result: %+v", res)
	}
	if !rec.Called {
		t.Fatal("expected agyRunAgentFn to be invoked")
	}
	if got := strings.Join(rec.AgentArgs, " "); got != "--agent "+agyMediaViewAgentName {
		t.Fatalf("agentArgs = %q, want \"--agent %s\"", got, agyMediaViewAgentName)
	}
	if len(rec.AddDirs) == 0 {
		t.Fatal("expected non-empty addDirs for a media run")
	}
	if !strings.Contains(rec.Prompt, "view_file") {
		t.Fatalf("prompt does not mention view_file:\n%s", rec.Prompt)
	}
}

// (b) same attachment, AgyMediaAgent "" -> agentArgs nil (default coding
// agent) and today's legacy prompt.
func TestAgyResolveDefaultAgentWhenMediaAgentUnconfigured(t *testing.T) {
	initAgyWorkerPool(config{AgyWarmWorkers: 0})
	setVTRuntime("", false)

	rec := fakeAgyRunAgentFn(t)

	cfg := config{AgyMedia: true, AgyMediaDir: t.TempDir(), AgyMediaAgent: ""}
	parts := []mediaPart{{B64: tinyPNGBase64(t), MediaType: "image/png", Filename: "receipt.png"}}

	res, err := agyResolve(context.Background(), cfg, parts, "what does it say?", "some-alias")
	if err != nil {
		t.Fatalf("agyResolve: %v", err)
	}
	if !res.Ok {
		t.Fatalf("unexpected result: %+v", res)
	}
	if rec.AgentArgs != nil {
		t.Fatalf("agentArgs = %v, want nil (default coding agent) when PROXY_AGY_MEDIA_AGENT is empty", rec.AgentArgs)
	}
	if !strings.Contains(rec.Prompt, "python") || !strings.Contains(rec.Prompt, "ffmpeg") {
		t.Fatalf("expected the legacy default-agent prompt, got:\n%s", rec.Prompt)
	}
}

// (c) no media at all, cfg.AgyAgent "connect-chat", warm pool disabled ->
// agentArgs == [--agent connect-chat] (runAgyj's own selection, now reached
// via agyAgentArgs(cfg, len(addDirs) > 0) inside agyResolve itself).
func TestAgyResolveNoMediaUsesConfiguredChatAgent(t *testing.T) {
	initAgyWorkerPool(config{AgyWarmWorkers: 0}) // pool disabled

	rec := fakeAgyRunAgentFn(t)

	cfg := config{AgyAgent: agyChatAgentName}
	res, err := agyResolve(context.Background(), cfg, nil, "hello", "some-alias")
	if err != nil {
		t.Fatalf("agyResolve: %v", err)
	}
	if !res.Ok {
		t.Fatalf("unexpected result: %+v", res)
	}
	if got := strings.Join(rec.AgentArgs, " "); got != "--agent "+agyChatAgentName {
		t.Fatalf("agentArgs = %q, want \"--agent %s\"", got, agyChatAgentName)
	}
	if len(rec.AddDirs) != 0 {
		t.Fatalf("addDirs = %v, want none for a no-media chat run", rec.AddDirs)
	}
}

// (d) an audio-only attachment (Groq unconfigured, so agyAudioTranscript
// defers to the normal agy media flow instead of short-circuiting) ->
// agentArgs nil even though the view-only agent IS configured, because
// audio is not view-eligible.
func TestAgyResolveAudioStaysOnDefaultAgent(t *testing.T) {
	initAgyWorkerPool(config{AgyWarmWorkers: 0})
	setVTRuntime("", false)

	rec := fakeAgyRunAgentFn(t)

	cfg := config{
		AgyMedia: true, AgyMediaDir: t.TempDir(), AgyMediaAgent: agyMediaViewAgentName,
		GroqAPIKey: "", // Groq off — agyAudioTranscript must return ok=false
	}
	parts := []mediaPart{{B64: base64.StdEncoding.EncodeToString([]byte("fake mp3 bytes")), MediaType: "audio/mpeg", Filename: "note.mp3"}}

	res, err := agyResolve(context.Background(), cfg, parts, "what did they say?", "some-alias")
	if err != nil {
		t.Fatalf("agyResolve: %v", err)
	}
	if !res.Ok {
		t.Fatalf("unexpected result: %+v", res)
	}
	if !rec.Called {
		t.Fatal("expected agyRunAgentFn to be invoked (Groq is off, so there is no direct-transcript short-circuit)")
	}
	if rec.AgentArgs != nil {
		t.Fatalf("agentArgs = %v, want nil — audio is not view-eligible even with the view-only agent configured", rec.AgentArgs)
	}
}
