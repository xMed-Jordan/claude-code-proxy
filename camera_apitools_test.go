package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// ─────────────────────────── camAPIToolSubstitute ───────────────────────────

func TestCamAPIToolSubstituteURLEncoding(t *testing.T) {
	got, err := camAPIToolSubstitute(
		"https://api.example.com/emp?name={{employee}}&d={{date}}",
		map[string]string{"employee": "Omar Q&A/2", "date": "2026-07-02"},
		false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := "https://api.example.com/emp?name=Omar+Q%26A%2F2&d=2026-07-02"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestCamAPIToolSubstituteWhitespaceInPlaceholder(t *testing.T) {
	got, err := camAPIToolSubstitute("https://x.example/{{ id }}", map[string]string{"id": "42"}, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "https://x.example/42" {
		t.Fatalf("got %q", got)
	}
}

func TestCamAPIToolSubstituteMissingParams(t *testing.T) {
	_, err := camAPIToolSubstitute(
		"https://x.example/?a={{alpha}}&b={{beta}}&b2={{beta}}",
		map[string]string{"alpha": "1"},
		false)
	if err == nil {
		t.Fatal("expected error for unfilled placeholder")
	}
	msg := err.Error()
	if !strings.Contains(msg, "{{beta}}") {
		t.Fatalf("error should name the unfilled placeholder, got %q", msg)
	}
	if strings.Contains(msg, "{{alpha}}") {
		t.Fatalf("error should not list filled placeholders, got %q", msg)
	}
	// Duplicate unfilled placeholders are reported once.
	if strings.Count(msg, "{{beta}}") != 1 {
		t.Fatalf("duplicate placeholder should be listed once, got %q", msg)
	}
}

func TestCamAPIToolSubstituteJSONEscape(t *testing.T) {
	tmpl := `{"name":"{{n}}","note":"{{note}}"}`
	got, err := camAPIToolSubstitute(tmpl, map[string]string{
		"n":    "he said \"hi\"",
		"note": "line1\nline2\\end",
	}, true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var parsed map[string]string
	if uerr := json.Unmarshal([]byte(got), &parsed); uerr != nil {
		t.Fatalf("substituted body is not valid JSON: %v\nbody: %s", uerr, got)
	}
	if parsed["name"] != "he said \"hi\"" {
		t.Fatalf("name round-trip mismatch: %q", parsed["name"])
	}
	if parsed["note"] != "line1\nline2\\end" {
		t.Fatalf("note round-trip mismatch: %q", parsed["note"])
	}
}

func TestCamAPIToolSubstituteNoPlaceholders(t *testing.T) {
	got, err := camAPIToolSubstitute("https://x.example/static", map[string]string{}, false)
	if err != nil || got != "https://x.example/static" {
		t.Fatalf("got %q err %v", got, err)
	}
}

// ─────────────────────────── camAPIToolValidateTemplate ───────────────────────────

func TestCamAPIToolValidateTemplate(t *testing.T) {
	cases := []struct {
		name    string
		tool    camAPITool
		wantErr string // substring; "" = expect valid
	}{
		{
			name: "valid GET with path+query placeholders",
			tool: camAPITool{Method: "GET", URLTemplate: "https://api.example.com/{{id}}/status?d={{date}}"},
		},
		{
			name: "valid POST with headers",
			tool: camAPITool{Method: "post", URLTemplate: "http://api.example.com/check",
				HeadersJSON: `{"X-Api-Key":"k","Accept":"application/json"}`},
		},
		{
			name: "blank method defaults to GET",
			tool: camAPITool{URLTemplate: "https://api.example.com/x"},
		},
		{
			name:    "placeholder as whole host",
			tool:    camAPITool{Method: "GET", URLTemplate: "https://{{host}}/x"},
			wantErr: "scheme or host",
		},
		{
			name:    "placeholder inside host",
			tool:    camAPITool{Method: "GET", URLTemplate: "https://api.{{env}}.example.com/x"},
			wantErr: "scheme or host",
		},
		{
			name:    "placeholder in host with no path",
			tool:    camAPITool{Method: "GET", URLTemplate: "https://{{h}}.example.com"},
			wantErr: "scheme or host",
		},
		{
			name: "Authorization header rejected",
			tool: camAPITool{Method: "GET", URLTemplate: "https://api.example.com/x",
				HeadersJSON: `{"Authorization":"Bearer x"}`},
			wantErr: "Authorization",
		},
		{
			name: "authorization header rejected case-insensitively",
			tool: camAPITool{Method: "GET", URLTemplate: "https://api.example.com/x",
				HeadersJSON: `{"authorization":"Bearer x"}`},
			wantErr: "Authorization",
		},
		{
			name:    "bad method",
			tool:    camAPITool{Method: "PUT", URLTemplate: "https://api.example.com/x"},
			wantErr: "GET or POST",
		},
		{
			name:    "non-http scheme",
			tool:    camAPITool{Method: "GET", URLTemplate: "ftp://api.example.com/x"},
			wantErr: "http(s)",
		},
		{
			name:    "missing url",
			tool:    camAPITool{Method: "GET"},
			wantErr: "url_template is required",
		},
		{
			name:    "relative url",
			tool:    camAPITool{Method: "GET", URLTemplate: "/just/a/path"},
			wantErr: "http(s)",
		},
		{
			name: "headers not a string map",
			tool: camAPITool{Method: "GET", URLTemplate: "https://api.example.com/x",
				HeadersJSON: `{"a":1}`},
			wantErr: "JSON object of string values",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := camAPIToolValidateTemplate(tc.tool)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("expected valid, got %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error %q does not contain %q", err.Error(), tc.wantErr)
			}
		})
	}
}

// ─────────────────────────── camAPIToolCatalog ───────────────────────────

func TestCamAPIToolCatalogFormat(t *testing.T) {
	tools := []camAPITool{
		{Name: "attendance-check", Description: "Checks the attendance system for whether an employee is clocked in or on break.",
			RequestInstructions: "employee (the employee id or full name as registered)", Enabled: true},
		{Name: "disabled-tool", Description: "should not appear", Enabled: false},
		{Name: "no-params", Description: "A lookup\nwith a multiline\tdescription.", Enabled: true},
	}
	got := camAPIToolCatalog(tools)
	want := "- \"attendance-check\" — Checks the attendance system for whether an employee is clocked in or on break.\n" +
		"    params: employee (the employee id or full name as registered)\n" +
		"- \"no-params\" — A lookup with a multiline description."
	if got != want {
		t.Fatalf("catalog mismatch:\n got: %q\nwant: %q", got, want)
	}
	if strings.Contains(got, "disabled-tool") {
		t.Fatal("disabled tools must not appear in the catalog")
	}
}

func TestCamAPIToolCatalogEmpty(t *testing.T) {
	if got := camAPIToolCatalog(nil); got != "" {
		t.Fatalf("empty catalog should be \"\", got %q", got)
	}
	if got := camAPIToolCatalog([]camAPITool{{Name: "x", Enabled: false}}); got != "" {
		t.Fatalf("all-disabled catalog should be \"\", got %q", got)
	}
}
