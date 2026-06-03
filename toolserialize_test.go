package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// Verifies that the Anthropic->Responses tool conversion emits a top-level
// "name" on each tool in the exact bytes sent to the codex backend.
func TestOutboundToolJSONHasName(t *testing.T) {
	in := anthropicRequest{
		Model:     "claude-opus-4-7[1m]",
		MaxTokens: 64,
		Tools: []anthropicTool{{
			Name:        "get_clinic_info",
			Description: "Get clinic info",
			InputSchema: map[string]any{"type": "object", "properties": map[string]any{}, "required": []string{}},
		}},
	}
	cfg := config{Upstream: "codex"}
	out := toResponses(cfg, in)
	raw, err := json.Marshal(codexRequestBody(out))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	t.Logf("OUTBOUND JSON: %s", string(raw))
	if !strings.Contains(string(raw), `"name":"get_clinic_info"`) {
		t.Fatalf("FAIL: outbound tool JSON is missing top-level name")
	}
	t.Log("PASS: outbound tool carries top-level name")
}
