package main

// camera_strategies_test.go — unit tests for the built-in investigation methods:
// catalog rendering, case-insensitive lookup, the playbook-tool fallback (built-in
// fetch + operator-name precedence), the unknown-name summary, and the system-prompt
// surfacing (methods catalog + core principles). No network, no DVR.

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// ─────────────────────────── catalog + lookup ───────────────────────────

func TestCamStrategyCatalogRendersAll(t *testing.T) {
	cat := camStrategyCatalog()
	lines := strings.Split(cat, "\n")
	if len(lines) != len(camBuiltinStrategies) {
		t.Fatalf("catalog has %d lines, want one per method (%d):\n%s", len(lines), len(camBuiltinStrategies), cat)
	}
	for i, s := range camBuiltinStrategies {
		if strings.TrimSpace(s.Name) == "" || !strings.HasPrefix(s.Name, "how-to: ") {
			t.Errorf("method %d name %q must be non-empty and prefixed \"how-to: \"", i, s.Name)
		}
		if strings.TrimSpace(s.WhenToUse) == "" {
			t.Errorf("method %q has empty WhenToUse", s.Name)
		}
		if strings.TrimSpace(s.Instructions) == "" {
			t.Errorf("method %q has empty Instructions", s.Name)
		}
		if !strings.Contains(cat, `- "`+s.Name+`"`) {
			t.Errorf("catalog missing method line for %q:\n%s", s.Name, cat)
		}
	}
	// Long when_to_use lines are truncated to the rune cap (never mid-byte).
	for _, line := range lines {
		if r := []rune(line); len(r) > camStrategyWhenToUseMax+len(`- "how-to: package evidence for the operator" — `)+8 {
			// generous ceiling: name + separator + capped when_to_use + ellipsis.
			t.Errorf("catalog line unexpectedly long (%d runes): %q", len(r), line)
		}
	}
}

func TestCamFindBuiltinStrategyCaseInsensitive(t *testing.T) {
	if _, ok := camFindBuiltinStrategy("  HOW-TO: FIND WHO TOOK AN OBJECT  "); !ok {
		t.Error("lookup should be case-insensitive and trim surrounding space")
	}
	if s, ok := camFindBuiltinStrategy("how-to: read fine detail"); !ok || s.Name != "how-to: read fine detail" {
		t.Errorf("exact-name lookup failed: ok=%v name=%q", ok, s.Name)
	}
	if _, ok := camFindBuiltinStrategy(""); ok {
		t.Error("empty name must not resolve")
	}
	if _, ok := camFindBuiltinStrategy("how-to: teleport"); ok {
		t.Error("unknown name must not resolve")
	}
}

// ─────────────────────────── playbook-tool fallback ───────────────────────────

func TestCamToolPlaybookReturnsBuiltin(t *testing.T) {
	db, siteID := newPlaybookTestDB(t)
	r := httptest.NewRequest("GET", "http://proxy.test/", nil)
	site := camSite{ID: siteID}

	// No operator playbook of this name exists → the built-in method answers.
	res := camToolPlaybook(t.Context(), config{}, db, r, site, investigateArgs{Name: "HOW-TO: FIND WHO TOOK AN OBJECT"}, t.TempDir())
	if !strings.Contains(res.Summary, `INVESTIGATION METHOD "how-to: find who took an object"`) {
		t.Errorf("built-in method not returned:\n%s", res.Summary)
	}
	if !strings.Contains(res.Summary, "DISAPPEARANCE") {
		t.Errorf("built-in method body missing its steps:\n%s", res.Summary)
	}
	if res.Fetches != 0 || len(res.Media) != 0 || len(res.Images) != 0 {
		t.Errorf("built-in method result must be text-only: %+v", res)
	}
}

func TestCamToolPlaybookOperatorWinsOnCollision(t *testing.T) {
	db, siteID := newPlaybookTestDB(t)
	// An operator playbook whose name collides with a built-in method: the operator
	// version (site-specific override) must win.
	if _, err := insertCameraPlaybook(db, camPlaybook{
		SiteID:       siteID,
		Name:         "how-to: read fine detail",
		WhenToUse:    "our site-specific override",
		Instructions: "OPERATOR OVERRIDE STEPS: use the lobby PTZ preset 3.",
		Enabled:      true,
	}); err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("GET", "http://proxy.test/", nil)
	res := camToolPlaybook(t.Context(), config{}, db, r, camSite{ID: siteID}, investigateArgs{Name: "how-to: read fine detail"}, t.TempDir())
	if !strings.Contains(res.Summary, `PLAYBOOK "how-to: read fine detail"`) {
		t.Errorf("operator playbook should win on name collision:\n%s", res.Summary)
	}
	if !strings.Contains(res.Summary, "OPERATOR OVERRIDE STEPS") {
		t.Errorf("operator instructions missing:\n%s", res.Summary)
	}
	if strings.Contains(res.Summary, "INVESTIGATION METHOD") {
		t.Errorf("built-in method must NOT be returned when an operator playbook collides:\n%s", res.Summary)
	}
}

func TestCamPlaybookUnknownSummaryListsBuiltins(t *testing.T) {
	db, siteID := newPlaybookTestDB(t)
	got := camPlaybookUnknownSummary(db, siteID, "totally-made-up")
	if !strings.Contains(got, `no playbook named "totally-made-up"`) {
		t.Errorf("summary missing the miss: %q", got)
	}
	if !strings.Contains(got, "Built-in methods:") {
		t.Errorf("summary must list built-in methods: %q", got)
	}
	if !strings.Contains(got, `"how-to: trace everything a person did"`) {
		t.Errorf("summary must name at least one built-in method: %q", got)
	}
}

// ─────────────────────────── system prompt surfacing ───────────────────────────

func TestCamInvestigateSystemPromptStrategies(t *testing.T) {
	active := []camera{{ID: "cam1", Name: "Front Door", Enabled: true}}
	prompt := camInvestigateSystemPrompt(
		camSite{Name: "Test Clinic"}, nil, active,
		nil, nil, nil,
		time.Now(), time.Time{}, false, "UTC",
	)
	// The methods catalog is always present, even with no operator playbooks.
	if !strings.Contains(prompt, "INVESTIGATION METHODS (built-in, environment-agnostic") {
		t.Error("prompt missing the INVESTIGATION METHODS catalog header")
	}
	if !strings.Contains(prompt, `- "how-to: trace everything a person did"`) {
		t.Error("prompt missing a built-in method catalog line")
	}
	// Core principles replace the old ad-hoc STRATEGY blocks.
	if !strings.Contains(prompt, "CORE PRINCIPLES (apply to EVERY investigation):") {
		t.Error("prompt missing the CORE PRINCIPLES block")
	}
	for _, kw := range []string{"Motion-first", "Binary search", "Endpoint sampling", "Honest limits"} {
		if !strings.Contains(prompt, kw) {
			t.Errorf("prompt missing core principle keyword %q", kw)
		}
	}
	// The delegate principle is gated off until WS3 ships the delegate tool, so the
	// prompt must not advertise a tool the loop cannot execute.
	if camDelegateToolEnabled {
		if !strings.Contains(prompt, "Fan out with delegate") {
			t.Error("delegate principle should be present when the tool is enabled")
		}
	} else if strings.Contains(prompt, "delegate") {
		t.Error("delegate must not appear in the prompt while the tool is disabled")
	}
}
