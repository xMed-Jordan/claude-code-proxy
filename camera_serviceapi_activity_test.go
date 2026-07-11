package main

// camera_serviceapi_activity_test.go — the activity-log service routes
// (camera_serviceapi_activity.go) and their thin admin twins
// (camera_observer_http.go): stream CRUD/toggle/run with validation + the
// per-site cap, the cross-site uniform-404 denial table, the browse routes
// (filters, ordering, site isolation), the days/generate path (400/idempotent/
// force keeps id+view_url, no webhook), the dedicated sync cursor (pagination,
// max_seq, has_more, type/after_seq 400s, cross-site isolation, pending rows
// hidden), and the report_language policy round-trip through /api/cameras/site.
// Harness: the camera_serviceapi_test.go helpers (newSvcTestConfig/
// newSvcTestServer/svcCall/svcTestDB) — temp sqlite + httptest only.

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// svcSeedLogEntry inserts one journal entry and returns its id.
func svcSeedLogEntry(t *testing.T, db *sql.DB, siteID, streamID, camIDs, fromTS, headline string) string {
	t.Helper()
	from, err := time.Parse(time.RFC3339, fromTS)
	if err != nil {
		t.Fatalf("bad fromTS %q: %v", fromTS, err)
	}
	id, ierr := insertLogEntry(db, logEntry{
		StreamID: streamID, SiteID: siteID,
		FromTS: fromTS, ToTS: from.Add(5 * time.Minute).Format(time.RFC3339),
		CameraIDs: camIDs, Headline: headline, Report: "report for " + headline,
	})
	if ierr != nil {
		t.Fatalf("insertLogEntry: %v", ierr)
	}
	return id
}

// svcSeedLogDay claims+finishes one daily report row and returns it.
func svcSeedLogDay(t *testing.T, db *sql.DB, siteID, streamID, day, status string) logDay {
	t.Helper()
	d := logDay{StreamID: streamID, SiteID: siteID, Day: day}
	id, won, err := claimLogDay(db, d)
	if err != nil || !won {
		t.Fatalf("claimLogDay(%s): won=%v err=%v", day, won, err)
	}
	d.ID, d.Status, d.Headline, d.Report, d.Language = id, status, "day headline", "day report", "English"
	if err := finishLogDay(db, d); err != nil {
		t.Fatalf("finishLogDay: %v", err)
	}
	got, gerr := getLogDay(db, id)
	if gerr != nil {
		t.Fatalf("getLogDay: %v", gerr)
	}
	return got
}

// ─────────────────────────── streams CRUD ───────────────────────────

func TestSvcActivityStreamsCRUD(t *testing.T) {
	cfg := newSvcTestConfig(t)
	db := svcTestDB(t, cfg)
	srv := newSvcTestServer(t, cfg)
	_, tokA := svcSeedSite(t, db, "Site A")
	_, tokB := svcSeedSite(t, db, "Site B")

	// Create with defaults: interval 5, step 30, motion_gate on, report time
	// 20:00, disabled, empty camera list (= every enabled camera).
	code, out, _ := svcCall(t, srv, http.MethodPost, "/api/cameras/activity/streams", tokA, map[string]any{"name": "Journal"})
	if code != http.StatusOK || out["ok"] != true {
		t.Fatalf("create: %d %v", code, out)
	}
	s1 := out["stream"].(map[string]any)
	s1ID := s1["id"].(string)
	if s1["interval_minutes"] != float64(5) || s1["frame_step_seconds"] != float64(30) ||
		s1["motion_gate"] != true || s1["enabled"] != false || s1["daily_report_time"] != "20:00" {
		t.Errorf("create defaults wrong: %v", s1)
	}
	if ids, _ := s1["camera_ids"].([]any); len(ids) != 0 {
		t.Errorf("default camera_ids = %v, want empty", s1["camera_ids"])
	}

	// Create enabled → next_run_at stays "" (due on the next tick).
	code, out, _ = svcCall(t, srv, http.MethodPost, "/api/cameras/activity/streams", tokA, map[string]any{
		"name": "Night", "enabled": true, "interval_minutes": 10, "frame_step_seconds": 60,
		"camera_ids": []string{"cam_x"}, "motion_gate": false, "daily_report_time": "21:30",
	})
	if code != http.StatusOK {
		t.Fatalf("create enabled: %d %v", code, out)
	}
	s2 := out["stream"].(map[string]any)
	if s2["enabled"] != true || s2["next_run_at"] != "" || s2["motion_gate"] != false ||
		s2["interval_minutes"] != float64(10) || s2["daily_report_time"] != "21:30" {
		t.Errorf("enabled create wrong: %v", s2)
	}

	// Validation 400s.
	for name, body := range map[string]map[string]any{
		"interval too big": {"interval_minutes": 20},
		"step too small":   {"frame_step_seconds": 5},
		"step over window": {"interval_minutes": 1, "frame_step_seconds": 120},
		"bad report time":  {"daily_report_time": "25:99"},
	} {
		code, out, _ = svcCall(t, srv, http.MethodPost, "/api/cameras/activity/streams", tokA, body)
		if code != http.StatusBadRequest {
			t.Errorf("%s: status = %d %v, want 400", name, code, out)
		}
	}

	// List is site-scoped.
	code, out, _ = svcCall(t, srv, http.MethodGet, "/api/cameras/activity/streams", tokA, nil)
	if st, _ := out["streams"].([]any); code != http.StatusOK || len(st) != 2 {
		t.Errorf("A streams = %d (%d), want 2", len(st), code)
	}
	code, out, _ = svcCall(t, srv, http.MethodGet, "/api/cameras/activity/streams", tokB, nil)
	if st, _ := out["streams"].([]any); code != http.StatusOK || len(st) != 0 {
		t.Errorf("B streams = %d (%d), want 0", len(st), code)
	}

	// Partial update: name/cameras/interval move, step validated against the
	// RESULT (shrinking the interval below the stored step is a 400).
	code, out, _ = svcCall(t, srv, http.MethodPost, "/api/cameras/activity/streams/update", tokA, map[string]any{
		"id": s1ID, "name": "Journal v2", "camera_ids": []string{"cam_a", "cam_b"}, "interval_minutes": 10, "frame_step_seconds": 300,
	})
	if code != http.StatusOK {
		t.Fatalf("update: %d %v", code, out)
	}
	u := out["stream"].(map[string]any)
	if u["name"] != "Journal v2" || u["interval_minutes"] != float64(10) || u["frame_step_seconds"] != float64(300) {
		t.Errorf("update result wrong: %v", u)
	}
	if ids, _ := u["camera_ids"].([]any); len(ids) != 2 {
		t.Errorf("updated camera_ids = %v, want 2", u["camera_ids"])
	}
	code, out, _ = svcCall(t, srv, http.MethodPost, "/api/cameras/activity/streams/update", tokA, map[string]any{"id": s1ID, "interval_minutes": 2})
	if code != http.StatusBadRequest {
		t.Errorf("interval below stored step must 400: %d %v", code, out)
	}

	// Toggle on → due immediately (next_run_at ""); off keeps it inert.
	code, out, _ = svcCall(t, srv, http.MethodPost, "/api/cameras/activity/streams/toggle", tokA, map[string]any{"id": s1ID, "enabled": true})
	if code != http.StatusOK || out["ok"] != true {
		t.Fatalf("toggle on: %d %v", code, out)
	}
	got, _ := getLogStream(db, s1ID)
	if !got.Enabled || got.NextRunAt != "" {
		t.Errorf("toggled stream = enabled %v next_run_at %q, want true + \"\"", got.Enabled, got.NextRunAt)
	}
	code, _, _ = svcCall(t, srv, http.MethodPost, "/api/cameras/activity/streams/toggle", tokA, map[string]any{"id": s1ID, "enabled": false})
	if code != http.StatusOK {
		t.Fatalf("toggle off: %d", code)
	}
	if got, _ = getLogStream(db, s1ID); got.Enabled {
		t.Error("stream should be disabled after toggle off")
	}

	// Delete → the id is gone (uniform 404 on a later update).
	code, out, _ = svcCall(t, srv, http.MethodPost, "/api/cameras/activity/streams/delete", tokA, map[string]any{"id": s1ID})
	if code != http.StatusOK || out["ok"] != true {
		t.Fatalf("delete: %d %v", code, out)
	}
	code, _, _ = svcCall(t, srv, http.MethodPost, "/api/cameras/activity/streams/update", tokA, map[string]any{"id": s1ID, "name": "ghost"})
	if code != http.StatusNotFound {
		t.Errorf("update after delete = %d, want 404", code)
	}
}

func TestSvcActivityStreamCap(t *testing.T) {
	cfg := newSvcTestConfig(t)
	cfg.CameraObserverMaxStreams = 2
	db := svcTestDB(t, cfg)
	srv := newSvcTestServer(t, cfg)
	_, tokA := svcSeedSite(t, db, "Site A")
	_, tokB := svcSeedSite(t, db, "Site B")

	for i := 0; i < 2; i++ {
		code, out, _ := svcCall(t, srv, http.MethodPost, "/api/cameras/activity/streams", tokA, map[string]any{"name": fmt.Sprintf("S%d", i)})
		if code != http.StatusOK {
			t.Fatalf("create %d: %d %v", i, code, out)
		}
	}
	code, out, _ := svcCall(t, srv, http.MethodPost, "/api/cameras/activity/streams", tokA, map[string]any{"name": "overflow"})
	if code != http.StatusBadRequest || !strings.Contains(fmt.Sprint(out["error"]), "maximum number") {
		t.Errorf("cap overflow = %d %v, want 400 with the cap message", code, out)
	}
	// The cap is per-site — another site still creates freely.
	code, _, _ = svcCall(t, srv, http.MethodPost, "/api/cameras/activity/streams", tokB, map[string]any{"name": "B1"})
	if code != http.StatusOK {
		t.Errorf("site B create under its own cap = %d, want 200", code)
	}
}

// TestSvcActivityStreamRun covers both run branches through the real in-flight
// guard: a stream mid-cycle refuses ({ok:false}); a free stream starts and —
// having no cameras — journals the "Logging failed" entry (the no-silent-holes
// contract) without any model call.
func TestSvcActivityStreamRun(t *testing.T) {
	cfg := newSvcTestConfig(t)
	db := svcTestDB(t, cfg)
	srv := newSvcTestServer(t, cfg)
	siteA, tokA := svcSeedSite(t, db, "Site A")

	sID, err := insertLogStream(db, logStream{SiteID: siteA, Name: "Journal", Enabled: true})
	if err != nil {
		t.Fatalf("insertLogStream: %v", err)
	}

	// In-flight → {ok:false}.
	if !camObserverTryAcquire(sID) {
		t.Fatal("acquire in-flight guard")
	}
	code, out, _ := svcCall(t, srv, http.MethodPost, "/api/cameras/activity/streams/run", tokA, map[string]any{"id": sID})
	if code != http.StatusOK || out["ok"] != false {
		t.Fatalf("run while in flight = %d %v, want ok:false", code, out)
	}
	camObserverRelease(sID)

	// Free → started:true; the cycle (site has no cameras) still writes an entry.
	code, out, _ = svcCall(t, srv, http.MethodPost, "/api/cameras/activity/streams/run", tokA, map[string]any{"id": sID})
	if code != http.StatusOK || out["ok"] != true || out["started"] != true {
		t.Fatalf("run = %d %v, want started:true", code, out)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if camObserverTryAcquire(sID) { // cycle finished and released the guard
			camObserverRelease(sID)
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("manual run never released the in-flight guard")
		}
		time.Sleep(10 * time.Millisecond)
	}
	entries, lerr := listLogEntriesBySite(db, siteA, sID, "", "", "", 0)
	if lerr != nil || len(entries) != 1 {
		t.Fatalf("entries after manual run = %d (%v), want 1", len(entries), lerr)
	}
	if entries[0].Headline != camObserverFailedHeadline || !strings.Contains(entries[0].Error, "no enabled cameras") {
		t.Errorf("cameraless run should journal the failed entry: %+v", entries[0])
	}
}

// ─────────────────────────── cross-site denial ───────────────────────────

func TestSvcActivityCrossSiteDenial(t *testing.T) {
	cfg := newSvcTestConfig(t)
	db := svcTestDB(t, cfg)
	srv := newSvcTestServer(t, cfg)
	_, tokA := svcSeedSite(t, db, "Site A")
	siteB, tokB := svcSeedSite(t, db, "Site B")

	streamB, err := insertLogStream(db, logStream{SiteID: siteB, Name: "B journal"})
	if err != nil {
		t.Fatalf("insertLogStream: %v", err)
	}
	dayB := svcSeedLogDay(t, db, siteB, streamB, "2020-01-15", "done")

	denied := []struct {
		name, method, path string
		body               any
	}{
		{"stream update", http.MethodPost, "/api/cameras/activity/streams/update", map[string]any{"id": streamB, "name": "hacked"}},
		{"stream delete", http.MethodPost, "/api/cameras/activity/streams/delete", map[string]any{"id": streamB}},
		{"stream toggle", http.MethodPost, "/api/cameras/activity/streams/toggle", map[string]any{"id": streamB, "enabled": false}},
		{"stream run", http.MethodPost, "/api/cameras/activity/streams/run", map[string]any{"id": streamB}},
		{"day generate", http.MethodPost, "/api/cameras/activity/days/generate", map[string]any{"stream_id": streamB}},
		{"entries by foreign stream", http.MethodGet, "/api/cameras/activity/entries?stream_id=" + streamB, nil},
		{"hours by foreign stream", http.MethodGet, "/api/cameras/activity/hours?stream_id=" + streamB, nil},
		{"days by foreign stream", http.MethodGet, "/api/cameras/activity/days?stream_id=" + streamB, nil},
		{"day by foreign id", http.MethodGet, "/api/cameras/activity/days?id=" + dayB.ID, nil},
	}
	for _, d := range denied {
		code, out, _ := svcCall(t, srv, d.method, d.path, tokA, d.body)
		if code != http.StatusNotFound {
			t.Errorf("%s with foreign token: status = %d %v, want 404", d.name, code, out)
			continue
		}
		if out["error"] != "not found" {
			t.Errorf(`%s: body = %v, want {"error":"not found"}`, d.name, out)
		}
	}

	// Positive controls with the owning token (reads only).
	for _, p := range []string{
		"/api/cameras/activity/entries?stream_id=" + streamB,
		"/api/cameras/activity/days?id=" + dayB.ID,
	} {
		code, out, _ := svcCall(t, srv, http.MethodGet, p, tokB, nil)
		if code != http.StatusOK {
			t.Errorf("owning token on %s: status = %d %v, want 200", p, code, out)
		}
	}
}

// ─────────────────────────── browse routes ───────────────────────────

func TestSvcActivityBrowse(t *testing.T) {
	cfg := newSvcTestConfig(t)
	db := svcTestDB(t, cfg)
	srv := newSvcTestServer(t, cfg)
	siteA, tokA := svcSeedSite(t, db, "Site A")
	siteB, tokB := svcSeedSite(t, db, "Site B")

	s1, _ := insertLogStream(db, logStream{SiteID: siteA, Name: "S1"})
	s2, _ := insertLogStream(db, logStream{SiteID: siteA, Name: "S2"})
	svcSeedLogEntry(t, db, siteA, s1, `["cam_a"]`, "2020-01-15T05:00:00Z", "gate courier")
	svcSeedLogEntry(t, db, siteA, s1, `["cam_b"]`, "2020-01-15T06:00:00Z", "reception visitors")
	svcSeedLogEntry(t, db, siteA, s2, `["cam_a"]`, "2020-01-15T07:00:00Z", "second stream")
	svcSeedLogEntry(t, db, siteB, "stream_b", `["cam_z"]`, "2020-01-15T05:00:00Z", "site B secret")

	// All of A, newest first.
	code, out, _ := svcCall(t, srv, http.MethodGet, "/api/cameras/activity/entries", tokA, nil)
	rows, _ := out["entries"].([]any)
	if code != http.StatusOK || len(rows) != 3 {
		t.Fatalf("A entries = %d (%d), want 3", len(rows), code)
	}
	if first := rows[0].(map[string]any); first["headline"] != "second stream" {
		t.Errorf("entries must be newest-first, got %v first", first["headline"])
	}
	// Site isolation both ways.
	for _, row := range rows {
		if row.(map[string]any)["site_id"] != siteA {
			t.Errorf("foreign row leaked into A's browse: %v", row)
		}
	}
	code, out, _ = svcCall(t, srv, http.MethodGet, "/api/cameras/activity/entries", tokB, nil)
	if rows, _ := out["entries"].([]any); code != http.StatusOK || len(rows) != 1 {
		t.Errorf("B entries = %d (%d), want 1", len(rows), code)
	}

	// stream_id / camera_id / window / limit filters.
	code, out, _ = svcCall(t, srv, http.MethodGet, "/api/cameras/activity/entries?stream_id="+s1, tokA, nil)
	if rows, _ := out["entries"].([]any); code != http.StatusOK || len(rows) != 2 {
		t.Errorf("s1 entries = %d, want 2", len(rows))
	}
	code, out, _ = svcCall(t, srv, http.MethodGet, "/api/cameras/activity/entries?camera_id=cam_b", tokA, nil)
	rows, _ = out["entries"].([]any)
	if code != http.StatusOK || len(rows) != 1 || rows[0].(map[string]any)["headline"] != "reception visitors" {
		t.Errorf("camera_id filter wrong: %v", out)
	}
	code, out, _ = svcCall(t, srv, http.MethodGet, "/api/cameras/activity/entries?from=2020-01-15T05:30:00Z&to=2020-01-15T06:30:00Z", tokA, nil)
	if rows, _ := out["entries"].([]any); code != http.StatusOK || len(rows) != 1 {
		t.Errorf("window filter = %d rows, want 1", len(rows))
	}
	code, out, _ = svcCall(t, srv, http.MethodGet, "/api/cameras/activity/entries?limit=1", tokA, nil)
	if rows, _ := out["entries"].([]any); code != http.StatusOK || len(rows) != 1 {
		t.Errorf("limit=1 = %d rows, want 1", len(rows))
	}

	// Hours + days shapes.
	h := logHour{StreamID: s1, SiteID: siteA, HourStart: "2020-01-15T05:00:00Z", TZ: "UTC"}
	hID, won, herr := claimLogHour(db, h)
	if herr != nil || !won {
		t.Fatalf("claimLogHour: %v", herr)
	}
	h.ID, h.Status, h.Summary, h.Language = hID, "done", "hour summary", "English"
	if err := finishLogHour(db, h); err != nil {
		t.Fatalf("finishLogHour: %v", err)
	}
	dayA := svcSeedLogDay(t, db, siteA, s1, "2020-01-15", "done")

	code, out, _ = svcCall(t, srv, http.MethodGet, "/api/cameras/activity/hours", tokA, nil)
	rows, _ = out["hours"].([]any)
	if code != http.StatusOK || len(rows) != 1 {
		t.Fatalf("hours = %d (%d), want 1", len(rows), code)
	}
	hr := rows[0].(map[string]any)
	if hr["summary"] != "hour summary" || hr["status"] != "done" || hr["hour_start"] != "2020-01-15T05:00:00Z" {
		t.Errorf("hour shape wrong: %v", hr)
	}
	if _, hasSeq := hr["seq"]; hasSeq {
		t.Error("browse shapes must not carry seq (sync-only)")
	}

	code, out, _ = svcCall(t, srv, http.MethodGet, "/api/cameras/activity/days?from=2020-01-01&to=2020-12-31", tokA, nil)
	rows, _ = out["days"].([]any)
	if code != http.StatusOK || len(rows) != 1 {
		t.Fatalf("days = %d (%d), want 1", len(rows), code)
	}
	dr := rows[0].(map[string]any)
	if dr["day"] != "2020-01-15" || dr["report"] != "day report" {
		t.Errorf("day shape wrong: %v", dr)
	}
	if u, _ := dr["view_url"].(string); !strings.Contains(u, "/camera/reports/") {
		t.Errorf("day view_url = %q, want a /camera/reports/ link", u)
	}
	if v, _ := dr["view_url"].(string); strings.Contains(fmt.Sprint(dr), dayA.ViewToken) && !strings.Contains(v, dayA.ViewToken) {
		t.Errorf("view_token must only surface inside view_url: %v", dr)
	}
}

// ─────────────────────────── days/generate ───────────────────────────

func TestSvcActivityDayGenerate(t *testing.T) {
	restore := camObserverAnalyzeFn
	var calls atomic.Int64
	camObserverAnalyzeFn = func(ctx context.Context, cfg config, alias, sys, user string, images []string) (string, []camAnalyzeAttempt, error) {
		calls.Add(1)
		return `{"headline":"H","report":"R"}`, nil, nil
	}
	defer func() { camObserverAnalyzeFn = restore }()

	cfg := newSvcTestConfig(t)
	db := svcTestDB(t, cfg)
	srv := newSvcTestServer(t, cfg)
	siteA, tokA := svcSeedSite(t, db, "Site A")
	sID, err := insertLogStream(db, logStream{SiteID: siteA, Name: "Journal"})
	if err != nil {
		t.Fatalf("insertLogStream: %v", err)
	}

	// Malformed date → 400 before any write.
	code, out, _ := svcCall(t, srv, http.MethodPost, "/api/cameras/activity/days/generate", tokA, map[string]any{"stream_id": sID, "date": "15/01/2020"})
	if code != http.StatusBadRequest {
		t.Fatalf("bad date = %d %v, want 400", code, out)
	}

	// A day with no entries settles "empty" with NO model call.
	code, out, _ = svcCall(t, srv, http.MethodPost, "/api/cameras/activity/days/generate", tokA, map[string]any{"stream_id": sID, "date": "2020-01-15"})
	if code != http.StatusOK || out["ok"] != true {
		t.Fatalf("generate: %d %v", code, out)
	}
	rep := out["report"].(map[string]any)
	if rep["status"] != "empty" || rep["day"] != "2020-01-15" {
		t.Errorf("empty-day report wrong: %v", rep)
	}
	if calls.Load() != 0 {
		t.Errorf("empty day must not call the model, calls = %d", calls.Load())
	}
	repID := rep["id"].(string)
	viewURL := rep["view_url"].(string)
	if viewURL == "" {
		t.Error("generated report must carry a view_url")
	}

	// Unforced regenerate is a free no-op; force rebuilds the SAME row — id and
	// view_url (the shared link) survive.
	code, out, _ = svcCall(t, srv, http.MethodPost, "/api/cameras/activity/days/generate", tokA, map[string]any{"stream_id": sID, "date": "2020-01-15"})
	if code != http.StatusOK || out["report"].(map[string]any)["id"] != repID {
		t.Fatalf("idempotent generate changed the row: %v", out)
	}
	code, out, _ = svcCall(t, srv, http.MethodPost, "/api/cameras/activity/days/generate", tokA, map[string]any{"stream_id": sID, "date": "2020-01-15", "force": true})
	if code != http.StatusOK {
		t.Fatalf("forced generate: %d %v", code, out)
	}
	rep = out["report"].(map[string]any)
	if rep["id"] != repID || rep["view_url"] != viewURL {
		t.Errorf("force must keep id+view_url: %v", rep)
	}
}

// ─────────────────────────── the sync cursor ───────────────────────────

func TestSvcActivitySyncCursor(t *testing.T) {
	cfg := newSvcTestConfig(t)
	db := svcTestDB(t, cfg)
	srv := newSvcTestServer(t, cfg)
	siteA, tokA := svcSeedSite(t, db, "Site A")
	siteB, tokB := svcSeedSite(t, db, "Site B")

	// Interleave A/B inserts so the GLOBAL seq alternates between sites — the
	// per-site cursor must page over gaps without missing or leaking rows.
	idsA, idsB := map[string]bool{}, map[string]bool{}
	base := time.Date(2020, 1, 15, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 5; i++ {
		ts := base.Add(time.Duration(i) * time.Hour).Format(time.RFC3339)
		idsA[svcSeedLogEntry(t, db, siteA, "sA", `["cam_a"]`, ts, fmt.Sprintf("A%d", i))] = true
		idsB[svcSeedLogEntry(t, db, siteB, "sB", `["cam_b"]`, ts, fmt.Sprintf("B%d", i))] = true
	}

	// Validation 400s: bad type, malformed / negative after_seq.
	for _, path := range []string{
		"/api/cameras/activity/sync?type=bogus",
		"/api/cameras/activity/sync?type=entries&after_seq=abc",
		"/api/cameras/activity/sync?type=entries&after_seq=-1",
	} {
		if code, out, _ := svcCall(t, srv, http.MethodGet, path, tokA, nil); code != http.StatusBadRequest {
			t.Errorf("%s = %d %v, want 400", path, code, out)
		}
	}

	// Page A's entries with limit=2: 2+2+1 rows, seq strictly ascending, only
	// A's ids, has_more true/true/false, max_seq = the global counter (10).
	var (
		afterSeq float64
		seen     []string
		pages    int
	)
	for {
		pages++
		code, out, _ := svcCall(t, srv, http.MethodGet,
			fmt.Sprintf("/api/cameras/activity/sync?type=entries&after_seq=%d&limit=2", int64(afterSeq)), tokA, nil)
		if code != http.StatusOK {
			t.Fatalf("sync page %d: %d %v", pages, code, out)
		}
		if out["max_seq"] != float64(10) {
			t.Errorf("max_seq = %v, want 10 (the global counter)", out["max_seq"])
		}
		rows, _ := out["rows"].([]any)
		for _, raw := range rows {
			row := raw.(map[string]any)
			if row["site_id"] != siteA || !idsA[row["id"].(string)] {
				t.Fatalf("foreign/unknown row in A's sync page: %v", row)
			}
			seq := row["seq"].(float64)
			if seq <= afterSeq {
				t.Fatalf("seq %v not ascending past cursor %v", seq, afterSeq)
			}
			afterSeq = seq
			seen = append(seen, row["id"].(string))
		}
		hasMore, _ := out["has_more"].(bool)
		wantLen := 2
		if pages == 3 {
			wantLen = 1
		}
		if len(rows) != wantLen {
			t.Fatalf("page %d rows = %d, want %d", pages, len(rows), wantLen)
		}
		if pages < 3 && !hasMore {
			t.Fatalf("page %d has_more = false, want true", pages)
		}
		if pages == 3 {
			if hasMore {
				t.Fatal("final page has_more = true, want false")
			}
			break
		}
	}
	if len(seen) != 5 {
		t.Fatalf("A cursor walked %d rows, want all 5", len(seen))
	}

	// B's cursor sees exactly its own five, despite the interleaved global seqs.
	code, out, _ := svcCall(t, srv, http.MethodGet, "/api/cameras/activity/sync?type=entries&limit=500", tokB, nil)
	rows, _ := out["rows"].([]any)
	if code != http.StatusOK || len(rows) != 5 {
		t.Fatalf("B sync = %d rows (%d), want 5", len(rows), code)
	}
	for _, raw := range rows {
		if row := raw.(map[string]any); !idsB[row["id"].(string)] {
			t.Fatalf("A's row leaked into B's sync: %v", row)
		}
	}

	// Pending hour claims are hidden; the terminal update surfaces the row with
	// a fresh seq past the counter value the claim consumed.
	h := logHour{StreamID: "sA", SiteID: siteA, HourStart: "2020-01-15T05:00:00Z", TZ: "UTC"}
	hID, won, herr := claimLogHour(db, h)
	if herr != nil || !won {
		t.Fatalf("claimLogHour: %v", herr)
	}
	code, out, _ = svcCall(t, srv, http.MethodGet, "/api/cameras/activity/sync?type=hours", tokA, nil)
	if rows, _ := out["rows"].([]any); code != http.StatusOK || len(rows) != 0 {
		t.Fatalf("pending hour must be hidden from sync: %d rows", len(rows))
	}
	h.ID, h.Status, h.Summary = hID, "done", "the summary"
	if err := finishLogHour(db, h); err != nil {
		t.Fatalf("finishLogHour: %v", err)
	}
	code, out, _ = svcCall(t, srv, http.MethodGet, "/api/cameras/activity/sync?type=hours", tokA, nil)
	rows, _ = out["rows"].([]any)
	if code != http.StatusOK || len(rows) != 1 {
		t.Fatalf("finished hour rows = %d (%d), want 1", len(rows), code)
	}
	if seq := rows[0].(map[string]any)["seq"].(float64); seq != 12 {
		t.Errorf("finished hour seq = %v, want 12 (claim consumed 11, finish re-stamped 12)", seq)
	}
	if out["max_seq"] != float64(12) {
		t.Errorf("max_seq after finish = %v, want 12", out["max_seq"])
	}

	// Days sync: a finished day surfaces once with its stamped seq.
	svcSeedLogDay(t, db, siteA, "sA", "2020-01-15", "done")
	code, out, _ = svcCall(t, srv, http.MethodGet, "/api/cameras/activity/sync?type=days", tokA, nil)
	rows, _ = out["rows"].([]any)
	if code != http.StatusOK || len(rows) != 1 {
		t.Fatalf("days sync rows = %d (%d), want 1", len(rows), code)
	}
	if rows[0].(map[string]any)["day"] != "2020-01-15" {
		t.Errorf("days sync row wrong: %v", rows[0])
	}
}

// ─────────────────────────── policy round-trip ───────────────────────────

// TestSvcActivityPolicyRoundTrip: report_language rides the EXISTING site
// policy routes (no new route) — Connect GETs the policy, mutates
// report_language, POSTs the WHOLE object back, and reads the same value on
// the next GET (policy POSTs replace the whole object).
func TestSvcActivityPolicyRoundTrip(t *testing.T) {
	cfg := newSvcTestConfig(t)
	db := svcTestDB(t, cfg)
	srv := newSvcTestServer(t, cfg)
	_, tokA := svcSeedSite(t, db, "Site A")

	code, out, _ := svcCall(t, srv, http.MethodGet, "/api/cameras/site", tokA, nil)
	if code != http.StatusOK {
		t.Fatalf("site get: %d %v", code, out)
	}
	policy := out["site"].(map[string]any)["policy"].(map[string]any)
	if policy["report_language"] != "" {
		t.Fatalf("fresh site report_language = %v, want empty (English default)", policy["report_language"])
	}
	policy["report_language"] = "Arabic"

	code, out, _ = svcCall(t, srv, http.MethodPost, "/api/cameras/site", tokA, map[string]any{"policy": policy})
	if code != http.StatusOK || out["ok"] != true {
		t.Fatalf("site policy post: %d %v", code, out)
	}
	code, out, _ = svcCall(t, srv, http.MethodGet, "/api/cameras/site", tokA, nil)
	got := out["site"].(map[string]any)["policy"].(map[string]any)
	if code != http.StatusOK || got["report_language"] != "Arabic" {
		t.Errorf("round-tripped report_language = %v, want Arabic", got["report_language"])
	}
}

// ─────────────────────────── admin twins ───────────────────────────

// adminCall drives a bare admin handler (the requireAdmin wrapper is exercised
// elsewhere; these twins share their bodies with the tested service routes).
func adminCall(t *testing.T, h http.HandlerFunc, method, target string, body any) (int, map[string]any) {
	t.Helper()
	var rd *strings.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		rd = strings.NewReader(string(raw))
	} else {
		rd = strings.NewReader("")
	}
	req := httptest.NewRequest(method, target, rd)
	rec := httptest.NewRecorder()
	h(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

// TestCameraActivityAdminTwins: the /ui/api/cameras/activity/* twins share the
// service helpers — create/list scoped by an explicit site_id, browse requires
// site_id, update applies the same validation, generate rides the same
// no-webhook path.
func TestCameraActivityAdminTwins(t *testing.T) {
	restore := camObserverAnalyzeFn
	var calls atomic.Int64
	camObserverAnalyzeFn = func(ctx context.Context, cfg config, alias, sys, user string, images []string) (string, []camAnalyzeAttempt, error) {
		calls.Add(1)
		return `{"headline":"H","report":"R"}`, nil, nil
	}
	defer func() { camObserverAnalyzeFn = restore }()

	cfg := newSvcTestConfig(t)
	db := svcTestDB(t, cfg)
	siteA, _ := svcSeedSite(t, db, "Site A")

	// Create requires site_id; defaults match the service twin.
	code, out := adminCall(t, handleCameraActivityStreams(cfg), http.MethodPost, "/ui/api/cameras/activity/streams", map[string]any{"name": "no site"})
	if code != http.StatusBadRequest {
		t.Fatalf("create without site_id = %d %v, want 400", code, out)
	}
	code, out = adminCall(t, handleCameraActivityStreams(cfg), http.MethodPost, "/ui/api/cameras/activity/streams", map[string]any{"site_id": siteA, "name": "Journal"})
	if code != http.StatusOK || out["ok"] != true {
		t.Fatalf("admin create: %d %v", code, out)
	}
	s := out["stream"].(map[string]any)
	sID := s["id"].(string)
	if s["interval_minutes"] != float64(5) || s["motion_gate"] != true {
		t.Errorf("admin create defaults wrong: %v", s)
	}

	// Shared validation on update.
	code, out = adminCall(t, handleCameraActivityStreamUpdate(cfg), http.MethodPost, "/ui/api/cameras/activity/streams/update", map[string]any{"id": sID, "interval_minutes": 99})
	if code != http.StatusBadRequest {
		t.Errorf("admin update validation = %d %v, want 400", code, out)
	}
	code, out = adminCall(t, handleCameraActivityStreamUpdate(cfg), http.MethodPost, "/ui/api/cameras/activity/streams/update", map[string]any{"id": "logstream_ghost", "name": "x"})
	if code != http.StatusNotFound {
		t.Errorf("admin update missing id = %d %v, want 404", code, out)
	}

	// Browse twins demand a site_id, then serve the shared shapes.
	code, out = adminCall(t, handleCameraActivityEntries(cfg), http.MethodGet, "/ui/api/cameras/activity/entries", nil)
	if code != http.StatusBadRequest {
		t.Fatalf("admin entries without site_id = %d %v, want 400", code, out)
	}
	svcSeedLogEntry(t, db, siteA, sID, `["cam_a"]`, "2020-01-15T05:00:00Z", "admin visible")
	code, out = adminCall(t, handleCameraActivityEntries(cfg), http.MethodGet, "/ui/api/cameras/activity/entries?site_id="+siteA, nil)
	rows, _ := out["entries"].([]any)
	if code != http.StatusOK || len(rows) != 1 || rows[0].(map[string]any)["headline"] != "admin visible" {
		t.Fatalf("admin entries = %d %v", code, out)
	}

	// Toggle + generate share the service semantics (empty day, no model call).
	code, out = adminCall(t, handleCameraActivityStreamToggle(cfg), http.MethodPost, "/ui/api/cameras/activity/streams/toggle", map[string]any{"id": sID, "enabled": true})
	if code != http.StatusOK || out["ok"] != true {
		t.Fatalf("admin toggle: %d %v", code, out)
	}
	if got, _ := getLogStream(db, sID); !got.Enabled || got.NextRunAt != "" {
		t.Errorf("admin toggle result wrong: %+v", got)
	}
	// An entry-free day settles "empty" with no model call (2019 predates the
	// seeded entry regardless of the server's local timezone).
	code, out = adminCall(t, handleCameraActivityDayGenerate(cfg), http.MethodPost, "/ui/api/cameras/activity/days/generate", map[string]any{"stream_id": sID, "date": "2019-03-03"})
	if code != http.StatusOK || out["ok"] != true {
		t.Fatalf("admin generate: %d %v", code, out)
	}
	if rep := out["report"].(map[string]any); rep["status"] != "empty" {
		t.Errorf("admin generate report = %v, want empty status", rep)
	}
	if calls.Load() != 0 {
		t.Errorf("empty admin generate must not call the model, calls = %d", calls.Load())
	}
}
