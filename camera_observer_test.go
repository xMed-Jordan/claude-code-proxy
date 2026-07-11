package main

// camera_observer_test.go — the activity-log observer RUNNER
// (camera_observer.go), driven through the real camExecuteLogCycle /
// camConsiderLogStream with both seams scripted (camObserverAnalyzeFn +
// camObserverFetchFrameFn, swap+defer-restore per the
// camera_investigate_force_answer_test.go pattern):
//
//   - normal cycle: frames sampled → sheets built+persisted → ONE analysis call
//     → entry row + continuity cursor advance; the system prompt carries the
//     roster, the English mandate and the stream's own recent entries.
//   - gated cycle: motion-gated idle window → idle entry, the model and the
//     frame fetcher are NEVER called.
//   - trust-guard matrix: motion-off / an unarmed camera / no events in the
//     trailing 24h each disable the gate → the model IS called; gated-with-
//     events analyzes WITH the motion digest.
//   - drill-down: needs_detail clamped to the max (4 fetches), exactly ONE
//     extra round (a second round's requests are ignored).
//   - repair + salvage: unparseable reply → one repair round; still failing →
//     the raw text is salvaged into the entry with error "salvaged_raw".
//   - catch-up never backfills: a stale cursor skips to the latest window; a
//     too-fresh cursor skips the cycle entirely (window < interval/2).
//   - active-hours skip: a closed window advances next_run_at without running.
//   - failure cycle: an analysis error still writes the "Logging failed" entry
//     and advances the cursor — no silent holes.
//
// Temp sqlite only, no DVRs, no S3, no network.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// newCamObserverTestConfig is the observer runner's test config: temp sqlite +
// media root (newCamViewTestConfig) plus a distinctive observer alias so the
// alias ladder is observable on the entry row.
func newCamObserverTestConfig(t *testing.T) config {
	t.Helper()
	cfg := newCamViewTestConfig(t)
	cfg.CameraObserverAlias = "obs-test-alias"
	return cfg
}

// seedObserverSite seeds one site with one enabled DVR (tz Asia/Amman) and two
// enabled cameras, returning the site id and the camera ids in creation order.
func seedObserverSite(t *testing.T, cfg config) (siteID string, camIDs []string) {
	t.Helper()
	db, err := openProxyDB(cfg)
	if err != nil {
		t.Fatalf("openProxyDB: %v", err)
	}
	defer db.Close()
	siteID, err = insertCameraSite(db, camSite{Name: "Obs Clinic", Description: "two-camera observer test site"})
	if err != nil {
		t.Fatalf("insertCameraSite: %v", err)
	}
	dvrID, err := insertCameraDVR(db, cfg, CamDVR{SiteID: siteID, Name: "DVR", Brand: "hikvision", Host: "192.0.2.50", Timezone: "Asia/Amman", Enabled: true})
	if err != nil {
		t.Fatalf("insertCameraDVR: %v", err)
	}
	cam1, err := insertCamera(db, camera{SiteID: siteID, DVRID: dvrID, Channel: 1, Name: "Gate", Enabled: true})
	if err != nil {
		t.Fatalf("insertCamera: %v", err)
	}
	cam2, err := insertCamera(db, camera{SiteID: siteID, DVRID: dvrID, Channel: 2, Name: "Reception", Enabled: true})
	if err != nil {
		t.Fatalf("insertCamera: %v", err)
	}
	return siteID, []string{cam1, cam2}
}

// seedObserverStream inserts a log stream and returns the stored row.
func seedObserverStream(t *testing.T, cfg config, s logStream) logStream {
	t.Helper()
	db, err := openProxyDB(cfg)
	if err != nil {
		t.Fatalf("openProxyDB: %v", err)
	}
	defer db.Close()
	id, err := insertLogStream(db, s)
	if err != nil {
		t.Fatalf("insertLogStream: %v", err)
	}
	row, err := getLogStream(db, id)
	if err != nil {
		t.Fatalf("getLogStream: %v", err)
	}
	return row
}

// writeObsTestFrame writes a tiny decodable image "frame" at path (PNG bytes —
// loadAndFitCell sniffs the format, the .jpg extension is irrelevant).
func writeObsTestFrame(path string) error {
	img := image.NewRGBA(image.Rect(0, 0, 16, 9))
	for y := 0; y < 9; y++ {
		for x := 0; x < 16; x++ {
			img.Set(x, y, color.RGBA{uint8(x * 15), uint8(y * 28), 128, 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return err
	}
	return os.WriteFile(path, buf.Bytes(), 0o600)
}

// obsStreamEntries lists a stream's entries newest-first.
func obsStreamEntries(t *testing.T, cfg config, siteID, streamID string) []logEntry {
	t.Helper()
	db, err := openProxyDB(cfg)
	if err != nil {
		t.Fatalf("openProxyDB: %v", err)
	}
	defer db.Close()
	entries, err := listLogEntriesBySite(db, siteID, streamID, "", "", "", 50)
	if err != nil {
		t.Fatalf("listLogEntriesBySite: %v", err)
	}
	return entries
}

// TestCamObserverWindow: the window math — cursor continuity, catch-up that
// skips to the latest window instead of backfilling, and the too-small skip.
func TestCamObserverWindow(t *testing.T) {
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	lag := 60 * time.Second
	s := logStream{IntervalMinutes: 5}

	from, to, ok := camObserverWindow(s, now, lag)
	if !ok {
		t.Fatal("fresh stream: window unexpectedly skipped")
	}
	if want := now.Add(-lag); !to.Equal(want) {
		t.Errorf("to = %v, want now-lag %v", to, want)
	}
	if want := to.Add(-5 * time.Minute); !from.Equal(want) {
		t.Errorf("from = %v, want to-interval %v", from, want)
	}

	// A parseable cursor within 2×interval resumes exactly there.
	cursor := to.Add(-6 * time.Minute)
	s.LastWindowTo = cursor.Format(time.RFC3339)
	from, _, ok = camObserverWindow(s, now, lag)
	if !ok || !from.Equal(cursor) {
		t.Errorf("fresh cursor: from = %v ok=%v, want cursor %v", from, ok, cursor)
	}

	// A stale cursor (3h) must NOT backfill — catch-up skips to the latest window.
	s.LastWindowTo = to.Add(-3 * time.Hour).Format(time.RFC3339)
	from, _, ok = camObserverWindow(s, now, lag)
	if !ok || !from.Equal(to.Add(-5*time.Minute)) {
		t.Errorf("stale cursor: from = %v ok=%v, want to-interval (no backfill)", from, ok)
	}

	// A cursor too close to `to` (window < interval/2) skips the cycle.
	s.LastWindowTo = to.Add(-time.Minute).Format(time.RFC3339)
	if _, _, ok = camObserverWindow(s, now, lag); ok {
		t.Error("near cursor: expected skip (window < interval/2)")
	}

	// Garbage cursors fall back to the plain latest window.
	s.LastWindowTo = "not-a-time"
	from, _, ok = camObserverWindow(s, now, lag)
	if !ok || !from.Equal(to.Add(-5*time.Minute)) {
		t.Errorf("garbage cursor: from = %v ok=%v, want to-interval", from, ok)
	}
}

// TestObserverCycleNormal: the happy path end to end — sampling through the
// fetch seam, two per-camera sheets, ONE analysis call whose system prompt
// carries roster/continuity/English-mandate, and the persisted entry + cursor.
func TestObserverCycleNormal(t *testing.T) {
	cfg := newCamObserverTestConfig(t)
	siteID, camIDs := seedObserverSite(t, cfg)
	s := seedObserverStream(t, cfg, logStream{SiteID: siteID, Name: "Journal", Enabled: true})

	db, err := openProxyDB(cfg)
	if err != nil {
		t.Fatalf("openProxyDB: %v", err)
	}
	defer db.Close()
	// A prior entry: the continuity block must quote it back to the model.
	if _, err := insertLogEntry(db, logEntry{
		StreamID: s.ID, SiteID: siteID,
		FromTS:    time.Now().Add(-11 * time.Minute).UTC().Format(time.RFC3339),
		ToTS:      time.Now().Add(-6 * time.Minute).UTC().Format(time.RFC3339),
		CameraIDs: "[]", Headline: "Crew painting the corridor", Report: "Two painters at work near the gate.",
	}); err != nil {
		t.Fatalf("insertLogEntry (prior): %v", err)
	}

	origFetch := camObserverFetchFrameFn
	defer func() { camObserverFetchFrameFn = origFetch }()
	var mu sync.Mutex
	subFetches := 0
	camObserverFetchFrameFn = func(ctx context.Context, c config, cam camera, dvr CamDVR, quality string, ts time.Time, destPath string, allowDVR bool) (bool, error) {
		mu.Lock()
		subFetches++
		mu.Unlock()
		if quality != "sub" {
			t.Errorf("sampling quality = %q, want sub", quality)
		}
		if allowDVR {
			t.Error("first sampling pass must not allow the DVR fallback")
		}
		if err := writeObsTestFrame(destPath); err != nil {
			return false, err
		}
		return true, nil
	}

	origAnalyze := camObserverAnalyzeFn
	defer func() { camObserverAnalyzeFn = origAnalyze }()
	analyzeCalls := 0
	var sysSeen, userSeen, aliasSeen string
	imagesSeen := 0
	camObserverAnalyzeFn = func(ctx context.Context, c config, alias, sys, user string, images []string) (string, []camAnalyzeAttempt, error) {
		analyzeCalls++
		sysSeen, userSeen, aliasSeen, imagesSeen = sys, user, alias, len(images)
		return `{"headline":"Quiet corridor","report":"The two painters are STILL working near the gate; reception is empty."}`, nil, nil
	}

	camExecuteLogCycle(cfg, s)

	if analyzeCalls != 1 {
		t.Fatalf("analyze calls = %d, want exactly 1", analyzeCalls)
	}
	entries := obsStreamEntries(t, cfg, siteID, s.ID)
	if len(entries) != 2 {
		t.Fatalf("entries = %d, want 2 (prior + new)", len(entries))
	}
	e := entries[0]
	if e.Headline != "Quiet corridor" || !strings.Contains(e.Report, "STILL working") {
		t.Errorf("entry = %q / %q, want the model's headline+report", e.Headline, e.Report)
	}
	if e.Frames != 20 { // 2 cameras × 10 instants (5min window / 30s step)
		t.Errorf("frames = %d, want 20", e.Frames)
	}
	if e.MotionGated {
		t.Error("normal cycle must not be marked motion_gated")
	}
	if e.Alias != "obs-test-alias" || aliasSeen != "obs-test-alias" {
		t.Errorf("alias = entry %q / call %q, want obs-test-alias (config ladder)", e.Alias, aliasSeen)
	}
	if e.Error != "" {
		t.Errorf("entry error = %q, want empty (no misses, clean parse)", e.Error)
	}
	var tokens []string
	if err := json.Unmarshal([]byte(e.MediaTokens), &tokens); err != nil || len(tokens) != 2 {
		t.Errorf("media_tokens = %q (%v), want 2 sheet tokens", e.MediaTokens, err)
	}
	if imagesSeen != 2 {
		t.Errorf("images attached = %d, want 2 sheets (2 cameras ≤ max sheets)", imagesSeen)
	}
	for _, id := range camIDs {
		if !strings.Contains(e.CameraIDs, id) {
			t.Errorf("camera_ids = %q, want it to include %s", e.CameraIDs, id)
		}
	}
	for _, want := range []string{"CAMERA ROSTER", "ENGLISH", "RECENT LOG", "Crew painting the corridor", "KNOWN PEOPLE"} {
		if want == "KNOWN PEOPLE" { // no avatars seeded — block must be absent
			if strings.Contains(sysSeen, want) {
				t.Errorf("system prompt contains %q despite no avatars", want)
			}
			continue
		}
		if !strings.Contains(sysSeen, want) {
			t.Errorf("system prompt missing %q", want)
		}
	}
	for _, want := range []string{"LOG WINDOW", "SHEET 1", "SHEET 2", "Gate", "Reception"} {
		if !strings.Contains(userSeen, want) {
			t.Errorf("user prompt missing %q", want)
		}
	}

	st, err := getLogStream(db, s.ID)
	if err != nil {
		t.Fatalf("getLogStream: %v", err)
	}
	if st.LastWindowTo != e.ToTS {
		t.Errorf("cursor = %q, want the entry's to_ts %q", st.LastWindowTo, e.ToTS)
	}
	if st.LastRunAt == "" || st.LastError != "" {
		t.Errorf("last_run_at=%q last_error=%q, want stamped/empty", st.LastRunAt, st.LastError)
	}

	// Immediately re-running skips (window < interval/2 past the fresh cursor):
	// no new entry, no analyze call — the claim's re-arm owns the schedule.
	camExecuteLogCycle(cfg, st)
	if analyzeCalls != 1 {
		t.Errorf("analyze calls after immediate re-run = %d, want still 1", analyzeCalls)
	}
	if got := obsStreamEntries(t, cfg, siteID, s.ID); len(got) != 2 {
		t.Errorf("entries after immediate re-run = %d, want still 2 (skip, not a hole)", len(got))
	}
}

// TestObserverCycleMotionGatedIdle: gate fully armed (stream opt-in + motion
// on + every camera armed + a 24h event) and ZERO events in the window → the
// idle entry, with the model and the frame fetcher never called.
func TestObserverCycleMotionGatedIdle(t *testing.T) {
	cfg := newCamObserverTestConfig(t)
	cfg.CameraMotionEnabled = true
	siteID, camIDs := seedObserverSite(t, cfg)
	s := seedObserverStream(t, cfg, logStream{SiteID: siteID, Name: "Night journal", MotionGate: true, Enabled: true})

	db, err := openProxyDB(cfg)
	if err != nil {
		t.Fatalf("openProxyDB: %v", err)
	}
	defer db.Close()
	for _, id := range camIDs {
		if err := upsertCameraMotionArm(db, siteID, id, "[]", 0, true); err != nil {
			t.Fatalf("upsertCameraMotionArm: %v", err)
		}
	}
	// One event an hour ago: inside the trailing-24h trust probe, OUTSIDE the window.
	if _, err := insertCameraMotionEvent(db, siteID, camIDs[0], "VMD", time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)); err != nil {
		t.Fatalf("insertCameraMotionEvent: %v", err)
	}

	origFetch := camObserverFetchFrameFn
	defer func() { camObserverFetchFrameFn = origFetch }()
	camObserverFetchFrameFn = func(ctx context.Context, c config, cam camera, dvr CamDVR, quality string, ts time.Time, destPath string, allowDVR bool) (bool, error) {
		t.Error("gated idle cycle must never fetch frames")
		return false, nil
	}
	origAnalyze := camObserverAnalyzeFn
	defer func() { camObserverAnalyzeFn = origAnalyze }()
	camObserverAnalyzeFn = func(ctx context.Context, c config, alias, sys, user string, images []string) (string, []camAnalyzeAttempt, error) {
		t.Error("gated idle cycle must never call the model")
		return "", nil, nil
	}

	camExecuteLogCycle(cfg, s)

	entries := obsStreamEntries(t, cfg, siteID, s.ID)
	if len(entries) != 1 {
		t.Fatalf("entries = %d, want exactly 1 idle entry", len(entries))
	}
	e := entries[0]
	if e.Headline != camObserverIdleHeadline || !e.MotionGated {
		t.Errorf("idle entry = %q gated=%v, want %q gated", e.Headline, e.MotionGated, camObserverIdleHeadline)
	}
	if e.Alias != "" || e.Frames != 0 {
		t.Errorf("idle entry alias=%q frames=%d, want empty alias + 0 frames (no model call)", e.Alias, e.Frames)
	}
	for _, want := range []string{"Gate", "Reception", "Asia/Amman"} {
		if !strings.Contains(e.Report, want) {
			t.Errorf("idle report %q missing %q (must name cameras+window+tz)", e.Report, want)
		}
	}
	st, _ := getLogStream(db, s.ID)
	if st.LastWindowTo != e.ToTS {
		t.Errorf("cursor = %q, want %q (idle cycles advance the cursor too)", st.LastWindowTo, e.ToTS)
	}
}

// TestObserverTrustGuardMatrix: each broken gate precondition — motion feature
// off, an unarmed camera, no motion events in the trailing 24h — disables
// gating, so the model IS called even though the window itself has no events;
// and a gated stream WITH window events analyzes with the motion digest.
func TestObserverTrustGuardMatrix(t *testing.T) {
	cases := []struct {
		name        string
		motionOn    bool
		armBoth     bool
		armFirst    bool
		event24hAgo bool
		eventInWin  bool
		wantDigest  bool
	}{
		{name: "motion feature off", motionOn: false, armBoth: true, event24hAgo: true},
		{name: "one camera unarmed", motionOn: true, armFirst: true, event24hAgo: true},
		{name: "no events in trailing 24h", motionOn: true, armBoth: true},
		{name: "gated with events in window", motionOn: true, armBoth: true, event24hAgo: true, eventInWin: true, wantDigest: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := newCamObserverTestConfig(t)
			cfg.CameraMotionEnabled = tc.motionOn
			siteID, camIDs := seedObserverSite(t, cfg)
			s := seedObserverStream(t, cfg, logStream{SiteID: siteID, Name: "Guarded", MotionGate: true, Enabled: true})

			db, err := openProxyDB(cfg)
			if err != nil {
				t.Fatalf("openProxyDB: %v", err)
			}
			defer db.Close()
			if tc.armBoth || tc.armFirst {
				arm := camIDs
				if tc.armFirst && !tc.armBoth {
					arm = camIDs[:1]
				}
				for _, id := range arm {
					if err := upsertCameraMotionArm(db, siteID, id, "[]", 0, true); err != nil {
						t.Fatalf("upsertCameraMotionArm: %v", err)
					}
				}
			}
			if tc.event24hAgo {
				if _, err := insertCameraMotionEvent(db, siteID, camIDs[0], "VMD", time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)); err != nil {
					t.Fatalf("insertCameraMotionEvent: %v", err)
				}
			}
			if tc.eventInWin {
				// Inside [now-lag-interval, now-lag): 3 minutes ago.
				if _, err := insertCameraMotionEvent(db, siteID, camIDs[1], "VMD", time.Now().Add(-3*time.Minute).UTC().Format(time.RFC3339)); err != nil {
					t.Fatalf("insertCameraMotionEvent: %v", err)
				}
			}

			origFetch := camObserverFetchFrameFn
			defer func() { camObserverFetchFrameFn = origFetch }()
			camObserverFetchFrameFn = func(ctx context.Context, c config, cam camera, dvr CamDVR, quality string, ts time.Time, destPath string, allowDVR bool) (bool, error) {
				if err := writeObsTestFrame(destPath); err != nil {
					return false, err
				}
				return true, nil
			}
			origAnalyze := camObserverAnalyzeFn
			defer func() { camObserverAnalyzeFn = origAnalyze }()
			analyzeCalls := 0
			var userSeen string
			camObserverAnalyzeFn = func(ctx context.Context, c config, alias, sys, user string, images []string) (string, []camAnalyzeAttempt, error) {
				analyzeCalls++
				userSeen = user
				return `{"headline":"Watching","report":"All quiet on both cameras."}`, nil, nil
			}

			camExecuteLogCycle(cfg, s)

			if analyzeCalls != 1 {
				t.Fatalf("analyze calls = %d, want 1 (gate must not silence this cycle)", analyzeCalls)
			}
			entries := obsStreamEntries(t, cfg, siteID, s.ID)
			if len(entries) != 1 || entries[0].Headline != "Watching" || entries[0].MotionGated {
				t.Fatalf("entry = %+v, want the analyzed non-gated entry", entries)
			}
			if got := strings.Contains(userSeen, "MOTION EVENTS"); got != tc.wantDigest {
				t.Errorf("motion digest present = %v, want %v", got, tc.wantDigest)
			}
		})
	}
}

// TestObserverDrillDown: needs_detail is clamped to the max (4), fetched at
// main quality through the seam, persisted as log_detail tokens, and the
// SECOND reply is final — its own needs_detail is ignored (one round cap).
func TestObserverDrillDown(t *testing.T) {
	cfg := newCamObserverTestConfig(t)
	siteID, camIDs := seedObserverSite(t, cfg)
	s := seedObserverStream(t, cfg, logStream{SiteID: siteID, Name: "Detail journal", Enabled: true})

	origFetch := camObserverFetchFrameFn
	defer func() { camObserverFetchFrameFn = origFetch }()
	var mu sync.Mutex
	mainFetches := 0
	camObserverFetchFrameFn = func(ctx context.Context, c config, cam camera, dvr CamDVR, quality string, ts time.Time, destPath string, allowDVR bool) (bool, error) {
		if quality == "main" {
			mu.Lock()
			mainFetches++
			mu.Unlock()
			if !allowDVR {
				t.Error("drill-down fetch must allow the bounded playback fallback")
			}
		}
		if err := writeObsTestFrame(destPath); err != nil {
			return false, err
		}
		return true, nil
	}

	origAnalyze := camObserverAnalyzeFn
	defer func() { camObserverAnalyzeFn = origAnalyze }()
	analyzeCalls := 0
	var secondUser string
	camObserverAnalyzeFn = func(ctx context.Context, c config, alias, sys, user string, images []string) (string, []camAnalyzeAttempt, error) {
		analyzeCalls++
		if analyzeCalls == 1 {
			// SIX requests — more than the max of 4 — all on the first camera.
			ts := time.Now().Add(-3 * time.Minute).UTC().Format(time.RFC3339)
			var reqs []string
			for i := 0; i < 6; i++ {
				reqs = append(reqs, fmt.Sprintf(`{"camera_id":%q,"time":%q}`, camIDs[0], ts))
			}
			return `{"headline":"Draft: someone at the gate","report":"A figure is near the gate; too small to identify.","needs_detail":[` + strings.Join(reqs, ",") + `]}`, nil, nil
		}
		secondUser = user
		// The final reply asks for MORE detail — the one-round cap must ignore it.
		return `{"headline":"Final confirmed","report":"The courier delivered a parcel at the gate.","needs_detail":[{"camera_id":"` + camIDs[0] + `","time":"` + time.Now().UTC().Format(time.RFC3339) + `"}]}`, nil, nil
	}

	camExecuteLogCycle(cfg, s)

	if analyzeCalls != 2 {
		t.Fatalf("analyze calls = %d, want exactly 2 (one drill-down round)", analyzeCalls)
	}
	if mainFetches != 4 {
		t.Errorf("main-quality fetches = %d, want 4 (needs_detail clamped)", mainFetches)
	}
	entries := obsStreamEntries(t, cfg, siteID, s.ID)
	if len(entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(entries))
	}
	e := entries[0]
	if e.Headline != "Final confirmed" {
		t.Errorf("headline = %q, want the FINAL round's headline", e.Headline)
	}
	var detail []string
	if err := json.Unmarshal([]byte(e.DetailTokens), &detail); err != nil || len(detail) != 4 {
		t.Errorf("detail_tokens = %q (%v), want 4 log_detail tokens", e.DetailTokens, err)
	}
	for _, want := range []string{"YOUR DRAFT ENTRY", "DETAIL FRAMES", "Draft: someone at the gate"} {
		if !strings.Contains(secondUser, want) {
			t.Errorf("second-round prompt missing %q", want)
		}
	}
}

// TestObserverRepairThenSalvage: an unparseable reply gets exactly ONE repair
// round; a repair that parses wins, a repair that still fails salvages the raw
// text into the entry with error "salvaged_raw" — never a crashed cycle.
func TestObserverRepairThenSalvage(t *testing.T) {
	cases := []struct {
		name         string
		secondReply  string
		wantHeadline string
		wantSalvaged bool
	}{
		{
			name:         "repair round parses",
			secondReply:  `{"headline":"Repaired","report":"Recovered on the second try."}`,
			wantHeadline: "Repaired",
		},
		{
			name:         "both replies unparseable",
			secondReply:  "All quiet in the lobby.\nNothing else moved.",
			wantHeadline: "All quiet in the lobby.",
			wantSalvaged: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := newCamObserverTestConfig(t)
			siteID, _ := seedObserverSite(t, cfg)
			s := seedObserverStream(t, cfg, logStream{SiteID: siteID, Name: "Parse journal", Enabled: true})

			origFetch := camObserverFetchFrameFn
			defer func() { camObserverFetchFrameFn = origFetch }()
			camObserverFetchFrameFn = func(ctx context.Context, c config, cam camera, dvr CamDVR, quality string, ts time.Time, destPath string, allowDVR bool) (bool, error) {
				if err := writeObsTestFrame(destPath); err != nil {
					return false, err
				}
				return true, nil
			}
			origAnalyze := camObserverAnalyzeFn
			defer func() { camObserverAnalyzeFn = origAnalyze }()
			analyzeCalls := 0
			camObserverAnalyzeFn = func(ctx context.Context, c config, alias, sys, user string, images []string) (string, []camAnalyzeAttempt, error) {
				analyzeCalls++
				if analyzeCalls == 1 {
					return "I looked at the sheets but here is prose with no JSON at all.", nil, nil
				}
				if !strings.Contains(user, "could not be parsed") {
					t.Error("repair round must carry the repair instruction")
				}
				return tc.secondReply, nil, nil
			}

			camExecuteLogCycle(cfg, s)

			if analyzeCalls != 2 {
				t.Fatalf("analyze calls = %d, want 2 (one repair round, never more)", analyzeCalls)
			}
			entries := obsStreamEntries(t, cfg, siteID, s.ID)
			if len(entries) != 1 {
				t.Fatalf("entries = %d, want 1", len(entries))
			}
			e := entries[0]
			if e.Headline != tc.wantHeadline {
				t.Errorf("headline = %q, want %q", e.Headline, tc.wantHeadline)
			}
			if got := strings.Contains(e.Error, "salvaged_raw"); got != tc.wantSalvaged {
				t.Errorf("error = %q, salvaged_raw present = %v, want %v", e.Error, got, tc.wantSalvaged)
			}
			if tc.wantSalvaged && !strings.Contains(e.Report, "Nothing else moved.") {
				t.Errorf("salvaged report = %q, want the raw model text preserved", e.Report)
			}
		})
	}
}

// TestObserverCatchUpNoBackfill: a stream that was down for hours resumes with
// ONE latest-window entry (span == interval), never a burst of stale windows.
func TestObserverCatchUpNoBackfill(t *testing.T) {
	cfg := newCamObserverTestConfig(t)
	siteID, _ := seedObserverSite(t, cfg)
	s := seedObserverStream(t, cfg, logStream{SiteID: siteID, Name: "Catch-up journal", Enabled: true})

	db, err := openProxyDB(cfg)
	if err != nil {
		t.Fatalf("openProxyDB: %v", err)
	}
	defer db.Close()
	stale := time.Now().Add(-3 * time.Hour).UTC().Format(time.RFC3339)
	if err := setLogStreamCursor(db, s.ID, stale, stale, ""); err != nil {
		t.Fatalf("setLogStreamCursor: %v", err)
	}
	s, err = getLogStream(db, s.ID)
	if err != nil {
		t.Fatalf("getLogStream: %v", err)
	}

	origFetch := camObserverFetchFrameFn
	defer func() { camObserverFetchFrameFn = origFetch }()
	camObserverFetchFrameFn = func(ctx context.Context, c config, cam camera, dvr CamDVR, quality string, ts time.Time, destPath string, allowDVR bool) (bool, error) {
		if err := writeObsTestFrame(destPath); err != nil {
			return false, err
		}
		return true, nil
	}
	origAnalyze := camObserverAnalyzeFn
	defer func() { camObserverAnalyzeFn = origAnalyze }()
	camObserverAnalyzeFn = func(ctx context.Context, c config, alias, sys, user string, images []string) (string, []camAnalyzeAttempt, error) {
		return `{"headline":"Back online","report":"Resumed logging after an outage."}`, nil, nil
	}

	camExecuteLogCycle(cfg, s)

	entries := obsStreamEntries(t, cfg, siteID, s.ID)
	if len(entries) != 1 {
		t.Fatalf("entries = %d, want exactly 1 (no backfill burst)", len(entries))
	}
	from, ferr := time.Parse(time.RFC3339, entries[0].FromTS)
	to, terr := time.Parse(time.RFC3339, entries[0].ToTS)
	if ferr != nil || terr != nil {
		t.Fatalf("entry window parse: %v / %v", ferr, terr)
	}
	if span := to.Sub(from); span != 5*time.Minute {
		t.Errorf("entry span = %v, want the plain 5m interval (latest window, not 3h of catch-up)", span)
	}
	if to.Before(time.Now().Add(-10 * time.Minute)) {
		t.Errorf("entry to_ts = %v, want the LATEST window, not the stale cursor's era", to)
	}
}

// TestObserverActiveHoursSkip: a due stream outside its active_hours window is
// re-armed (next_run_at advances) without running — no entry, no model call.
func TestObserverActiveHoursSkip(t *testing.T) {
	cfg := newCamObserverTestConfig(t)
	siteID, _ := seedObserverSite(t, cfg)
	now := time.Now()
	closed := fmt.Sprintf("%02d:00-%02d:00", (now.Hour()+2)%24, (now.Hour()+3)%24)
	s := seedObserverStream(t, cfg, logStream{SiteID: siteID, Name: "Office hours", ActiveHours: closed, Enabled: true})

	origAnalyze := camObserverAnalyzeFn
	defer func() { camObserverAnalyzeFn = origAnalyze }()
	camObserverAnalyzeFn = func(ctx context.Context, c config, alias, sys, user string, images []string) (string, []camAnalyzeAttempt, error) {
		t.Error("closed active_hours must never reach the model")
		return "", nil, nil
	}
	origFetch := camObserverFetchFrameFn
	defer func() { camObserverFetchFrameFn = origFetch }()
	camObserverFetchFrameFn = func(ctx context.Context, c config, cam camera, dvr CamDVR, quality string, ts time.Time, destPath string, allowDVR bool) (bool, error) {
		t.Error("closed active_hours must never sample frames")
		return false, nil
	}

	db, err := openProxyDB(cfg)
	if err != nil {
		t.Fatalf("openProxyDB: %v", err)
	}
	defer db.Close()
	camConsiderLogStream(cfg, db, s, now)

	st, err := getLogStream(db, s.ID)
	if err != nil {
		t.Fatalf("getLogStream: %v", err)
	}
	next, perr := time.Parse(time.RFC3339, st.NextRunAt)
	if perr != nil || !next.After(now) {
		t.Errorf("next_run_at = %q, want advanced past now (re-armed)", st.NextRunAt)
	}
	if entries := obsStreamEntries(t, cfg, siteID, s.ID); len(entries) != 0 {
		t.Errorf("entries = %d, want 0 (a skipped window is not a cycle)", len(entries))
	}
}

// TestObserverFailureCycleEntry: an analysis failure still writes an entry —
// headline "Logging failed" with the cause in the error column — and advances
// the continuity cursor, so the journal never has silent holes.
func TestObserverFailureCycleEntry(t *testing.T) {
	cfg := newCamObserverTestConfig(t)
	siteID, _ := seedObserverSite(t, cfg)
	s := seedObserverStream(t, cfg, logStream{SiteID: siteID, Name: "Failing journal", Enabled: true})

	origFetch := camObserverFetchFrameFn
	defer func() { camObserverFetchFrameFn = origFetch }()
	camObserverFetchFrameFn = func(ctx context.Context, c config, cam camera, dvr CamDVR, quality string, ts time.Time, destPath string, allowDVR bool) (bool, error) {
		if err := writeObsTestFrame(destPath); err != nil {
			return false, err
		}
		return true, nil
	}
	origAnalyze := camObserverAnalyzeFn
	defer func() { camObserverAnalyzeFn = origAnalyze }()
	analyzeCalls := 0
	camObserverAnalyzeFn = func(ctx context.Context, c config, alias, sys, user string, images []string) (string, []camAnalyzeAttempt, error) {
		analyzeCalls++
		return "", nil, fmt.Errorf("backend down")
	}

	camExecuteLogCycle(cfg, s)

	if analyzeCalls != 1 {
		t.Errorf("analyze calls = %d, want 1 (an infrastructure error gets no repair round)", analyzeCalls)
	}
	entries := obsStreamEntries(t, cfg, siteID, s.ID)
	if len(entries) != 1 {
		t.Fatalf("entries = %d, want the failure entry", len(entries))
	}
	e := entries[0]
	if e.Headline != camObserverFailedHeadline || !strings.Contains(e.Error, "backend down") {
		t.Errorf("failure entry = %q / error %q, want %q + the cause", e.Headline, e.Error, camObserverFailedHeadline)
	}
	db, err := openProxyDB(cfg)
	if err != nil {
		t.Fatalf("openProxyDB: %v", err)
	}
	defer db.Close()
	st, _ := getLogStream(db, s.ID)
	if st.LastWindowTo != e.ToTS {
		t.Errorf("cursor = %q, want %q (failed cycles advance the cursor — no re-analysis loop)", st.LastWindowTo, e.ToTS)
	}
	if !strings.Contains(st.LastError, "backend down") {
		t.Errorf("stream last_error = %q, want the analysis error surfaced", st.LastError)
	}
}

// TestCamObserverContinuityBlockIdleCompression: consecutive motion-gated idle
// entries compress into one "[from–to] No activity (N idle cycles)" line while
// active entries keep their own lines, and the newest entries quote verbatim.
func TestCamObserverContinuityBlockIdleCompression(t *testing.T) {
	mk := func(offsetMin int, headline, report string, gated bool) logEntry {
		base := time.Date(2026, 7, 11, 8, 0, 0, 0, time.UTC)
		return logEntry{
			FromTS:      base.Add(time.Duration(offsetMin) * time.Minute).Format(time.RFC3339),
			ToTS:        base.Add(time.Duration(offsetMin+5) * time.Minute).Format(time.RFC3339),
			Headline:    headline,
			Report:      report,
			MotionGated: gated,
		}
	}
	// Oldest → newest: active, idle ×3, active. listRecentLogEntries returns
	// newest-first, so the block receives the reversed order.
	oldest := []logEntry{
		mk(0, "Crew arrives", "Two workers enter through the gate.", false),
		mk(5, camObserverIdleHeadline, "idle", true),
		mk(10, camObserverIdleHeadline, "idle", true),
		mk(15, camObserverIdleHeadline, "idle", true),
		mk(20, "Door opens", "A visitor enters reception.", false),
	}
	recent := make([]logEntry, len(oldest))
	for i, e := range oldest {
		recent[len(oldest)-1-i] = e
	}

	block := camObserverContinuityBlock(recent, time.UTC, 1)
	if !strings.Contains(block, "Crew arrives") {
		t.Errorf("block missing the oldest active headline:\n%s", block)
	}
	if !strings.Contains(block, "(3 idle cycles)") {
		t.Errorf("block missing the compressed idle run:\n%s", block)
	}
	if strings.Count(block, camObserverIdleHeadline) != 1 {
		t.Errorf("idle headline should appear exactly once (compressed):\n%s", block)
	}
	if !strings.Contains(block, "A visitor enters reception.") {
		t.Errorf("block missing the newest entry's verbatim report:\n%s", block)
	}
	if camObserverContinuityBlock(nil, time.UTC, 3) != "" {
		t.Error("empty history must render no continuity block")
	}
}
