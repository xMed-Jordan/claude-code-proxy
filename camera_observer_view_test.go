package main

// camera_observer_view_test.go — the public daily-report share page
// (camera_observer_view.go), driven through handleCameraReportView with
// direct handler invocation (unique RemoteAddr per test so the per-IP rate
// buckets never bleed into other tests):
//
//   - 200: the static shell for any well-formed token, and state.json with the
//     report's fields for a real one (hardened headers on both).
//   - 404: unknown tokens on the JSON path (uniform — no token oracle), slashy
//     tokens, and non-GET methods.
//   - 429: the per-IP bucket cuts a flooding client off at the page ceiling.
//
// Temp sqlite only, no network.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// obsSeedViewDay seeds one finished daily report and returns its view token.
func obsSeedViewDay(t *testing.T, cfg config) (siteID string, viewToken string) {
	t.Helper()
	siteID, _ = seedObserverSite(t, cfg)
	s := seedObserverStream(t, cfg, logStream{SiteID: siteID, Name: "Front of house", Enabled: true})
	db, err := openProxyDB(cfg)
	if err != nil {
		t.Fatalf("openProxyDB: %v", err)
	}
	defer db.Close()
	id, won, err := claimLogDay(db, logDay{StreamID: s.ID, SiteID: siteID, Day: "2026-07-11"})
	if err != nil || !won {
		t.Fatalf("claimLogDay = %v, %v", won, err)
	}
	if err := finishLogDay(db, logDay{ID: id, FromTS: "2026-07-10T21:00:00Z", ToTS: "2026-07-11T17:00:00Z",
		Headline: "يوم هادئ في العيادة", Report: "مر اليوم بهدوء؛ زار العيادة عدد قليل من المراجعين.", Language: "Arabic",
		HoursUsed: 20, EntriesUsed: 240, Status: "done"}); err != nil {
		t.Fatalf("finishLogDay: %v", err)
	}
	d, err := getLogDay(db, id)
	if err != nil {
		t.Fatalf("getLogDay: %v", err)
	}
	return siteID, d.ViewToken
}

// obsReportViewGet drives the handler directly with a caller-chosen client IP.
func obsReportViewGet(t *testing.T, h http.HandlerFunc, method, path, remoteAddr string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	req.RemoteAddr = remoteAddr
	rec := httptest.NewRecorder()
	h(rec, req)
	return rec
}

// TestCameraReportViewPage: the shell + state.json happy path, the uniform
// 404s, and the security headers on every public response.
func TestCameraReportViewPage(t *testing.T) {
	camResetSvcRateLimiter()
	cfg := newCamObserverTestConfig(t)
	_, token := obsSeedViewDay(t, cfg)
	h := handleCameraReportView(cfg)
	const ip = "198.51.100.10:4444"

	// Shell: static HTML for the bare token.
	rec := obsReportViewGet(t, h, http.MethodGet, "/camera/reports/"+token, ip)
	if rec.Code != http.StatusOK {
		t.Fatalf("shell: %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("shell content-type = %q", ct)
	}
	if body := rec.Body.String(); !strings.Contains(body, "<!doctype html>") || !strings.Contains(body, "state.json") {
		t.Error("shell must be the self-contained report page")
	}
	if rec.Header().Get("X-Robots-Tag") == "" || rec.Header().Get("Content-Security-Policy") == "" {
		t.Error("shell missing the hardened view headers")
	}

	// state.json: the report fields.
	rec = obsReportViewGet(t, h, http.MethodGet, "/camera/reports/"+token+"/state.json", ip)
	if rec.Code != http.StatusOK {
		t.Fatalf("state.json: %d %s, want 200", rec.Code, rec.Body.String())
	}
	var st map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil {
		t.Fatalf("decode state.json: %v", err)
	}
	want := map[string]any{
		"date":        "2026-07-11",
		"site_name":   "Obs Clinic",
		"stream_name": "Front of house",
		"headline":    "يوم هادئ في العيادة",
		"report":      "مر اليوم بهدوء؛ زار العيادة عدد قليل من المراجعين.",
		"language":    "Arabic",
		"status":      "done",
	}
	for k, v := range want {
		if st[k] != v {
			t.Errorf("state[%s] = %v, want %v", k, st[k], v)
		}
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("state.json cache-control = %q, want no-store", rec.Header().Get("Cache-Control"))
	}

	// Unknown token: uniform 404 on the JSON path; the static shell still
	// renders (it leaks nothing — the page shows "not found" client-side).
	if rec := obsReportViewGet(t, h, http.MethodGet, "/camera/reports/"+randToken(16)+"/state.json", ip); rec.Code != http.StatusNotFound {
		t.Errorf("unknown token state.json: %d, want 404", rec.Code)
	}
	if rec := obsReportViewGet(t, h, http.MethodGet, "/camera/reports/"+randToken(16), ip); rec.Code != http.StatusOK {
		t.Errorf("unknown token shell: %d, want 200 (static shell)", rec.Code)
	}

	// Malformed paths and non-GET methods 404 like an absent route.
	if rec := obsReportViewGet(t, h, http.MethodGet, "/camera/reports/", ip); rec.Code != http.StatusNotFound {
		t.Errorf("bare route: %d, want 404", rec.Code)
	}
	if rec := obsReportViewGet(t, h, http.MethodPost, "/camera/reports/"+token, ip); rec.Code != http.StatusNotFound {
		t.Errorf("POST: %d, want 404 (read-only surface)", rec.Code)
	}
}

// TestCameraReportViewRateLimit: the per-IP page bucket 429s a flooding client
// at the ceiling — before the DB is ever touched.
func TestCameraReportViewRateLimit(t *testing.T) {
	camResetSvcRateLimiter()
	cfg := newCamObserverTestConfig(t)
	h := handleCameraReportView(cfg)
	const ip = "198.51.100.99:5555" // unique to this test — no bucket bleed
	path := "/camera/reports/" + randToken(16)

	for i := 0; i < int(camViewPageRatePerMin); i++ {
		if rec := obsReportViewGet(t, h, http.MethodGet, path, ip); rec.Code != http.StatusOK {
			t.Fatalf("request %d: %d, want 200", i+1, rec.Code)
		}
	}
	if rec := obsReportViewGet(t, h, http.MethodGet, path, ip); rec.Code != http.StatusTooManyRequests {
		t.Errorf("over-limit request: %d, want 429", rec.Code)
	}
}
