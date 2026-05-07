package main

import "testing"

func TestAntigravityMCPToolsExposeVisibleControls(t *testing.T) {
	tools := antigravityMCPTools()
	names := map[string]bool{}
	for _, tool := range tools {
		names[tool.Name] = true
	}
	for _, want := range []string{
		"browser_status",
		"browser_pages",
		"browser_navigate",
		"browser_snapshot",
		"browser_screenshot",
		"browser_move",
		"browser_click",
		"browser_type",
		"browser_press_key",
		"browser_wait",
	} {
		if !names[want] {
			t.Fatalf("tool %q missing from Antigravity MCP tool list", want)
		}
	}
}

func TestNormalizeBrowserURLAddsHTTPS(t *testing.T) {
	got, err := normalizeBrowserURL("example.com/search?q=test")
	if err != nil {
		t.Fatal(err)
	}
	if got != "https://example.com/search?q=test" {
		t.Fatalf("normalizeBrowserURL = %q, want https://example.com/search?q=test", got)
	}
}

func TestTargetSchemaAllowsSelectorTextOrCoordinates(t *testing.T) {
	schema := targetSchema()
	props, ok := schema["properties"].(map[string]any)
	if !ok {
		t.Fatalf("properties missing from target schema: %#v", schema)
	}
	for _, key := range []string{"selector", "text", "x", "y"} {
		if _, ok := props[key]; !ok {
			t.Fatalf("target schema missing %q", key)
		}
	}
}

func TestSafeChromeWindowTitle(t *testing.T) {
	if !safeChromeWindowTitle("Claude Code Codex Proxy - Control Panel - Google Chrome") {
		t.Fatal("proxy dashboard Chrome window should be safe to relaunch")
	}
	if !safeChromeWindowTitle("") {
		t.Fatal("empty/background Chrome window title should be safe")
	}
	if !safeChromeWindowTitle("about:blank - Google Chrome") {
		t.Fatal("blank Chrome fallback window should be safe to relaunch")
	}
	if !safeChromeWindowTitle("OleMainThreadWndName") {
		t.Fatal("Chrome internal helper window should be safe to relaunch")
	}
	if safeChromeWindowTitle("Inbox - Gmail - Google Chrome") {
		t.Fatal("user Chrome window should not be safe to relaunch")
	}
}

func TestDefaultRelaunchRequiresExplicitForce(t *testing.T) {
	processes := []chromeProcessInfo{{ID: 100, Title: "about:blank - Google Chrome", IsVisible: true}}
	t.Setenv("ANTIGRAVITY_BROWSER_FORCE_DEFAULT_CDP", "0")
	if canRelaunchDefaultChromeFrom(processes) {
		t.Fatal("Default Chrome relaunch should stay disabled unless explicitly forced")
	}

	t.Setenv("ANTIGRAVITY_BROWSER_FORCE_DEFAULT_CDP", "1")
	t.Setenv("ANTIGRAVITY_BROWSER_SAFE_DEFAULT_RELAUNCH", "1")
	if !canRelaunchDefaultChromeFrom(processes) {
		t.Fatal("forced Default Chrome relaunch should allow safe Chrome windows")
	}
}
