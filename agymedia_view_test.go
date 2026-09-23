package main

// agymedia_view_test.go — tests for the view-only media agent (Change A,
// v0.28.0): agyMediaViewEligible, the agent definition/installation, the
// view-only prompt builder, agyMediaPrep's agent-selection wiring, and
// agyStreamJSONArgs.

import (
	"bytes"
	"context"
	"encoding/base64"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// tinyPNGBase64 returns a small valid PNG, base64-encoded, for mediaPart.B64.
func tinyPNGBase64(t *testing.T) string {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 2, 2))
	img.Set(0, 0, color.RGBA{R: 255, A: 255})
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode tiny png: %v", err)
	}
	return base64.StdEncoding.EncodeToString(buf.Bytes())
}

func TestAgyMediaViewEligible(t *testing.T) {
	cases := []struct {
		name  string
		items []mediaItem
		want  bool
	}{
		{"png", []mediaItem{{Kind: "image", Path: "a.png"}}, true},
		{"uppercase-jpg", []mediaItem{{Kind: "image", Path: "A.JPG"}}, true},
		{"webp", []mediaItem{{Kind: "image", Path: "a.webp"}}, true},
		{"gif", []mediaItem{{Kind: "image", Path: "a.gif"}}, true},
		{"heic", []mediaItem{{Kind: "image", Path: "a.heic"}}, false},
		{"bmp", []mediaItem{{Kind: "image", Path: "a.bmp"}}, false},
		{"tiff", []mediaItem{{Kind: "image", Path: "a.tiff"}}, false},
		{"pdf", []mediaItem{{Kind: "pdf", Path: "a.pdf"}}, false},
		{"audio", []mediaItem{{Kind: "audio", Path: "a.mp3"}}, false},
		{"mixed-image-pdf", []mediaItem{{Kind: "image", Path: "a.png"}, {Kind: "pdf", Path: "b.pdf"}}, false},
		{"empty", nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := agyMediaViewEligible(c.items); got != c.want {
				t.Fatalf("agyMediaViewEligible(%+v) = %v, want %v", c.items, got, c.want)
			}
		})
	}
}

func TestAgyMediaViewAgentDefinitionContents(t *testing.T) {
	def := agyMediaViewAgentDefinition
	if !strings.Contains(def, "tools: [view_file]") {
		t.Fatalf("definition missing 'tools: [view_file]':\n%s", def)
	}
	for _, forbidden := range []string{"run_command", "commandExecutionPolicy"} {
		if strings.Contains(def, forbidden) {
			t.Fatalf("definition must not contain %q:\n%s", forbidden, def)
		}
	}
	if !strings.Contains(def, "name: "+agyMediaViewAgentName) {
		t.Fatalf("definition missing agent name %q:\n%s", agyMediaViewAgentName, def)
	}
}

func TestEnsureAgyAgentDefinitionInstallsBoth(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	ensureAgyAgentDefinition(config{AgyAgent: agyChatAgentName, AgyMediaAgent: agyMediaViewAgentName})

	chatPath := filepath.Join(home, ".gemini", "config", "agents", agyChatAgentName, "agent.md")
	got, err := os.ReadFile(chatPath)
	if err != nil {
		t.Fatalf("chat definition not installed: %v", err)
	}
	if string(got) != agyChatAgentDefinition {
		t.Fatalf("unexpected chat definition content:\n%s", got)
	}

	viewPath := filepath.Join(home, ".gemini", "config", "agents", agyMediaViewAgentName, "agent.md")
	got, err = os.ReadFile(viewPath)
	if err != nil {
		t.Fatalf("media-view definition not installed: %v", err)
	}
	if string(got) != agyMediaViewAgentDefinition {
		t.Fatalf("unexpected media-view definition content:\n%s", got)
	}
}

func TestEnsureAgyAgentDefinitionSkipsMediaViewWhenUnconfigured(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	// AgyMediaAgent left at its zero value ("") — must not install anything for
	// the media-view agent, exactly like a foreign AgyAgent name today.
	ensureAgyAgentDefinition(config{AgyAgent: "custom-thing"})
	if _, err := os.Stat(filepath.Join(home, ".gemini", "config", "agents", agyMediaViewAgentName)); err == nil {
		t.Fatal("must not install the media-view definition when AgyMediaAgent is not configured to it")
	}
	if _, err := os.Stat(filepath.Join(home, ".gemini", "config", "agents", "custom-thing")); err == nil {
		t.Fatal("must not create definitions for an operator-managed chat agent name")
	}
}

func TestAgyMediaPrepViewOnlyAgent(t *testing.T) {
	setVTRuntime("", false) // never touch the network from a unit test
	dir := t.TempDir()
	cfg := config{AgyMedia: true, AgyMediaDir: dir, AgyMediaAgent: agyMediaViewAgentName}
	parts := []mediaPart{{B64: tinyPNGBase64(t), MediaType: "image/png", Filename: "receipt.png"}}

	prompt, addDirs, agentArgs, err := agyMediaPrep(context.Background(), cfg, "what does it say?", parts)
	if err != nil {
		t.Fatalf("agyMediaPrep: %v", err)
	}
	if got := strings.Join(agentArgs, " "); got != "--agent "+agyMediaViewAgentName {
		t.Fatalf("agentArgs = %q, want \"--agent %s\"", got, agyMediaViewAgentName)
	}
	if len(addDirs) != 1 {
		t.Fatalf("addDirs = %+v, want exactly one content dir", addDirs)
	}
	// The prompt must reference the materialized file's actual on-disk path.
	entries, err := os.ReadDir(addDirs[0])
	if err != nil {
		t.Fatalf("reading content dir: %v", err)
	}
	var materializedPath string
	for _, e := range entries {
		if !e.IsDir() && e.Name() != "_map.md" {
			materializedPath = filepath.Join(addDirs[0], e.Name())
		}
	}
	if materializedPath == "" {
		t.Fatal("could not find the materialized file in the content dir")
	}
	if !strings.Contains(prompt, materializedPath) {
		t.Fatalf("prompt does not contain the materialized path %q:\n%s", materializedPath, prompt)
	}
	if !strings.Contains(prompt, "view_file") {
		t.Fatalf("prompt does not mention view_file:\n%s", prompt)
	}
	for _, forbidden := range []string{"python", "ffmpeg", "_map.md"} {
		if strings.Contains(prompt, forbidden) {
			t.Fatalf("view-only prompt must not mention %q:\n%s", forbidden, prompt)
		}
	}
}

func TestAgyMediaPrepDefaultAgentWhenUnconfigured(t *testing.T) {
	setVTRuntime("", false)
	dir := t.TempDir()
	cfg := config{AgyMedia: true, AgyMediaDir: dir, AgyMediaAgent: ""}
	parts := []mediaPart{{B64: tinyPNGBase64(t), MediaType: "image/png", Filename: "receipt.png"}}

	prompt, addDirs, agentArgs, err := agyMediaPrep(context.Background(), cfg, "what does it say?", parts)
	if err != nil {
		t.Fatalf("agyMediaPrep: %v", err)
	}
	if agentArgs != nil {
		t.Fatalf("agentArgs = %v, want nil when PROXY_AGY_MEDIA_AGENT is empty", agentArgs)
	}
	if len(addDirs) != 1 {
		t.Fatalf("addDirs = %+v, want exactly one content dir", addDirs)
	}
	// Falls back to today's default-agent prompt (mentions running tools).
	if !strings.Contains(prompt, "python") || !strings.Contains(prompt, "ffmpeg") {
		t.Fatalf("expected the legacy default-agent prompt, got:\n%s", prompt)
	}
}

func TestAgyMediaPrepPDFStaysOnDefaultAgent(t *testing.T) {
	setVTRuntime("", false)
	dir := t.TempDir()
	cfg := config{AgyMedia: true, AgyMediaDir: dir, AgyMediaAgent: agyMediaViewAgentName}
	parts := []mediaPart{{B64: base64.StdEncoding.EncodeToString([]byte("%PDF-1.4 fake pdf bytes")), MediaType: "application/pdf", Filename: "doc.pdf"}}

	_, addDirs, agentArgs, err := agyMediaPrep(context.Background(), cfg, "summarize", parts)
	if err != nil {
		t.Fatalf("agyMediaPrep: %v", err)
	}
	if agentArgs != nil {
		t.Fatalf("agentArgs = %v, want nil — a PDF is not view-eligible in this change", agentArgs)
	}
	if len(addDirs) != 1 {
		t.Fatalf("addDirs = %+v, want exactly one content dir", addDirs)
	}
}

func TestAgyStreamJSONArgs(t *testing.T) {
	args := agyStreamJSONArgs("Gemini 3.6 Flash (Low)", []string{"/tmp/f-abc"}, []string{"--agent", agyMediaViewAgentName})
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "--agent "+agyMediaViewAgentName) {
		t.Fatalf("args missing --agent %s: %q", agyMediaViewAgentName, joined)
	}
	if !strings.Contains(joined, "--add-dir /tmp/f-abc") {
		t.Fatalf("args missing --add-dir: %q", joined)
	}

	// Multiple add-dirs — every one must appear as its own flag.
	args = agyStreamJSONArgs("", []string{"/tmp/a", "/tmp/b"}, nil)
	joined = strings.Join(args, " ")
	if !strings.Contains(joined, "--add-dir /tmp/a") || !strings.Contains(joined, "--add-dir /tmp/b") {
		t.Fatalf("args missing one of the add-dirs: %q", joined)
	}

	// A chat run (no addDirs) still gets its configured --agent.
	chatArgs := agyStreamJSONArgs("", nil, agyAgentArgs(config{AgyAgent: agyChatAgentName}, false))
	if got := strings.Join(chatArgs, " "); !strings.Contains(got, "--agent "+agyChatAgentName) {
		t.Fatalf("chat run args = %q, want --agent %s", got, agyChatAgentName)
	}
}

func TestAgyStreamJSONArgsIgnoresAgentArgsRegression(t *testing.T) {
	// Guard against a future edit that builds the argv without agentArgs at
	// all — agyMediaPrep's view-only selection would silently stop reaching
	// the spawned process.
	args := agyStreamJSONArgs("", nil, []string{"--agent", "connect-media-view"})
	if !contains(args, "--agent") {
		t.Fatalf("agentArgs did not make it into the argv: %v", args)
	}
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}
