package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestProxyBinaryNameForPlatforms(t *testing.T) {
	if got := proxyBinaryNameFor("windows"); got != "claude-code-proxy.exe" {
		t.Fatalf("windows binary = %q", got)
	}
	for _, goos := range []string{"linux", "darwin"} {
		if got := proxyBinaryNameFor(goos); got != "claude-code-proxy" {
			t.Fatalf("%s binary = %q", goos, got)
		}
	}
}

func TestHookCommandStringIncludesBinaryAndSubcommand(t *testing.T) {
	cmd := hookCommandString("hook-worktree-create")
	if !strings.Contains(cmd, "hook-worktree-create") {
		t.Fatalf("hook command missing subcommand: %s", cmd)
	}
	if !strings.Contains(cmd, "claude-code-proxy") {
		t.Fatalf("hook command missing binary name: %s", cmd)
	}
	if runtime.GOOS == "windows" && !strings.Contains(cmd, `"`) {
		t.Fatalf("windows hook command should quote paths: %s", cmd)
	}
	if runtime.GOOS != "windows" && !strings.Contains(cmd, `'`) {
		t.Fatalf("unix hook command should quote paths: %s", cmd)
	}
}

func TestSnapshotAndRestoreRoundTrip(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "settings.json")
	snapshot := filepath.Join(dir, "snapshot.json")
	meta := filepath.Join(dir, "snapshot.meta.json")
	if err := os.WriteFile(target, []byte(`{"before":true}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := ensureSnapshot(target, snapshot, meta, "settings_path", "{}"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte(`{"after":true}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := restoreSnapshot(target, snapshot, meta, "settings_path"); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(raw)) != `{"before":true}` {
		t.Fatalf("restored content = %s", raw)
	}
}

func TestChromeExecutableEnvOverride(t *testing.T) {
	dir := t.TempDir()
	name := "chrome"
	if runtime.GOOS == "windows" {
		name = "chrome.exe"
	}
	chrome := filepath.Join(dir, name)
	if err := os.WriteFile(chrome, []byte(""), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ANTIGRAVITY_CHROME_PATH", chrome)
	if got := findChromeExecutablePath(); got == "" || filepath.Clean(got) != filepath.Clean(chrome) {
		t.Fatalf("chrome path = %q, want %q", got, chrome)
	}
}

func TestSafeChildPathRejectsSibling(t *testing.T) {
	dir := t.TempDir()
	child := filepath.Join(dir, "child")
	sibling := filepath.Join(filepath.Dir(dir), filepath.Base(dir)+"-sibling")
	if !safeChildPath(dir, child) {
		t.Fatalf("expected child path to be allowed")
	}
	if safeChildPath(dir, sibling) {
		t.Fatalf("expected sibling path to be rejected")
	}
}

func TestMCPServerConfigUsesGoBinary(t *testing.T) {
	server := mcpServerConfig(true)
	if server["type"] != "stdio" {
		t.Fatalf("server type = %#v", server["type"])
	}
	if !strings.Contains(server["command"].(string), "claude-code-proxy") {
		t.Fatalf("server command = %#v", server["command"])
	}
	args, ok := server["args"].([]string)
	if !ok || len(args) != 1 || args[0] != "browser-mcp" {
		t.Fatalf("server args = %#v", server["args"])
	}
}
