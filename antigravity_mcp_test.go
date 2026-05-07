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
