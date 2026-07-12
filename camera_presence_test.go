package main

// camera_presence_test.go — the proactive presence/attendance layer
// (camera_presence.go): store lifecycle (open→extend→close, seq stamping),
// the observer-cycle detector (open once, extend quietly, day-rollover reopen),
// and the absence-grace close sweep. The push emitter is driven through the
// camPresenceNotifyFn + camPresenceDispatch seams (dispatch forced synchronous)
// so arrival/departure emissions are asserted deterministically without a live
// callback.

import (
	"database/sql"
	"sync"
	"testing"
	"time"
)

// presenceTestCfg enables the layer with a 10-min absence grace and points the
// DB path at the temp test DB (so the emitter's own openProxyDB, if ever hit,
// resolves the same file).
func presenceTestCfg() config {
	return config{CameraPresenceEnabled: true, CameraPresenceAbsenceGrace: 10 * time.Minute}
}

// withSyncPresencePushes forces dispatch synchronous and records every emission.
func withSyncPresencePushes(t *testing.T) *[]struct{ Kind, AvatarID, SightingID string } {
	t.Helper()
	var mu sync.Mutex
	rec := &[]struct{ Kind, AvatarID, SightingID string }{}
	origDispatch, origNotify := camPresenceDispatch, camPresenceNotifyFn
	camPresenceDispatch = func(fn func()) { fn() } // synchronous
	camPresenceNotifyFn = func(cfg config, kind string, s camSighting) {
		mu.Lock()
		*rec = append(*rec, struct{ Kind, AvatarID, SightingID string }{kind, s.AvatarID, s.ID})
		mu.Unlock()
	}
	t.Cleanup(func() { camPresenceDispatch, camPresenceNotifyFn = origDispatch, origNotify })
	return rec
}

func TestPresenceStoreLifecycle(t *testing.T) {
	db := openCamTestDB(t)
	if err := migrateCameraDB(db); err != nil {
		t.Fatalf("migrateCameraDB: %v", err)
	}

	id, seq, err := insertSighting(db, camSighting{
		SiteID: "site_1", CameraID: "cam_1", AvatarID: "av_1", Area: "Reception",
		StartedAt: "2026-07-12T05:10:00Z", BestScore: 0.48,
	})
	if err != nil || id == "" || seq <= 0 {
		t.Fatalf("insertSighting: id=%q seq=%d err=%v", id, seq, err)
	}

	open, err := getOpenSighting(db, "site_1", "av_1")
	if err != nil || open.ID != id || open.EndedAt != "" || open.StartedAt != "2026-07-12T05:10:00Z" {
		t.Fatalf("getOpenSighting = %+v err=%v", open, err)
	}
	if open.LastSeenAt != "2026-07-12T05:10:00Z" || open.FrameCount != 1 {
		t.Errorf("open defaults: last_seen=%q frames=%d", open.LastSeenAt, open.FrameCount)
	}

	// Extend: last_seen advances, frame_count bumps, still open, seq UNCHANGED.
	if err := updateSightingSeen(db, id, "2026-07-12T05:20:00Z"); err != nil {
		t.Fatalf("updateSightingSeen: %v", err)
	}
	open, _ = getOpenSighting(db, "site_1", "av_1")
	if open.LastSeenAt != "2026-07-12T05:20:00Z" || open.FrameCount != 2 {
		t.Errorf("post-extend: last_seen=%q frames=%d", open.LastSeenAt, open.FrameCount)
	}
	if open.Seq != seq {
		t.Errorf("extend must NOT re-stamp seq: was %d now %d", seq, open.Seq)
	}

	// Close (departure): re-stamps seq, ended_at set. Second close is a no-op.
	closed, err := closeSighting(db, id, "2026-07-12T13:30:00Z")
	if err != nil || !closed {
		t.Fatalf("first close: closed=%v err=%v", closed, err)
	}
	closedAgain, err := closeSighting(db, id, "2026-07-12T14:00:00Z")
	if err != nil || closedAgain {
		t.Fatalf("second close must be a no-op: closed=%v err=%v", closedAgain, err)
	}
	// No open interval remains; the closed row surfaces via the sync cursor with a
	// re-stamped (higher) seq so Connect re-pulls the departure.
	if _, err := getOpenSighting(db, "site_1", "av_1"); err == nil {
		t.Error("interval should be closed — getOpenSighting must return ErrNoRows")
	}
	after, err := listSightingsAfterSeq(db, "site_1", seq, 100)
	if err != nil || len(after) != 1 || after[0].ID != id || after[0].EndedAt != "2026-07-12T13:30:00Z" {
		t.Fatalf("listSightingsAfterSeq(seq) = %+v err=%v (want the closed row past the open seq)", after, err)
	}
	if after[0].Seq <= seq {
		t.Errorf("close must re-stamp seq higher than the open seq (%d), got %d", seq, after[0].Seq)
	}
	// Cross-site isolation.
	if other, _ := listSightingsAfterSeq(db, "site_other", 0, 100); len(other) != 0 {
		t.Errorf("cross-site leak: %d rows for a different site", len(other))
	}
}

func TestPresenceDetectorOpenExtendReopen(t *testing.T) {
	db := openCamTestDB(t)
	if err := migrateCameraDB(db); err != nil {
		t.Fatalf("migrateCameraDB: %v", err)
	}
	rec := withSyncPresencePushes(t)
	cfg := presenceTestCfg()
	cams := []camera{{ID: "cam_1", Name: "Reception", Area: "Reception and Waiting Area"}}
	loc := time.UTC

	ident := func(first, best time.Time) []camObserverIdentity {
		return []camObserverIdentity{{
			Name: "Razan", AvatarID: "av_1", CameraID: "cam_1", CameraName: "Reception",
			FirstCameraID: "cam_1", FirstT: first, T: best, Similarity: 0.48,
		}}
	}

	// Cycle 1 — first sighting today → ONE open interval + ONE arrival push, at
	// the EARLIEST instant (FirstT), not the best-similarity instant.
	day := time.Date(2026, 7, 12, 5, 9, 40, 0, time.UTC)
	camPresenceObserve(cfg, db, "site_1", ident(day, day.Add(2*time.Minute)), cams, day.Add(5*time.Minute), loc)
	open, err := getOpenSighting(db, "site_1", "av_1")
	if err != nil {
		t.Fatalf("no interval opened: %v", err)
	}
	if open.StartedAt != day.UTC().Format(time.RFC3339) {
		t.Errorf("arrival = %q, want the earliest FirstT %q", open.StartedAt, day.UTC().Format(time.RFC3339))
	}
	if len(*rec) != 1 || (*rec)[0].Kind != "arrival" {
		t.Fatalf("pushes after cycle 1 = %+v, want one arrival", *rec)
	}

	// Cycle 2 — same person, same day, later → EXTEND only. No new row, no push.
	later := day.Add(30 * time.Minute)
	camPresenceObserve(cfg, db, "site_1", ident(later, later), cams, later.Add(5*time.Minute), loc)
	if got := countPresenceRows(t, db, "av_1"); got != 1 {
		t.Fatalf("row count after extend = %d, want 1 (no new interval)", got)
	}
	if len(*rec) != 1 {
		t.Fatalf("pushes after extend = %+v, want still one (extends are silent)", *rec)
	}
	open, _ = getOpenSighting(db, "site_1", "av_1")
	if open.EndedAt != "" {
		t.Error("same-day extend must keep the interval open")
	}

	// Next day — a stale open from yesterday → close it (departure) AND open a
	// fresh interval (arrival): two pushes, two rows.
	tomorrow := day.Add(24 * time.Hour)
	camPresenceObserve(cfg, db, "site_1", ident(tomorrow, tomorrow), cams, tomorrow.Add(5*time.Minute), loc)
	if got := countPresenceRows(t, db, "av_1"); got != 2 {
		t.Fatalf("row count after day rollover = %d, want 2", got)
	}
	kinds := pushKinds(*rec)
	if len(kinds) != 3 || kinds[1] != "departure" || kinds[2] != "arrival" {
		t.Fatalf("push sequence = %v, want [arrival departure arrival]", kinds)
	}
}

func TestPresenceSweepClosesStale(t *testing.T) {
	db := openCamTestDB(t)
	if err := migrateCameraDB(db); err != nil {
		t.Fatalf("migrateCameraDB: %v", err)
	}
	rec := withSyncPresencePushes(t)
	cfg := presenceTestCfg() // 10-min grace

	// A stale open (last seen 30 min ago) and a fresh open (last seen 1 min ago).
	staleSeen := time.Now().Add(-30 * time.Minute).UTC().Format(time.RFC3339)
	freshSeen := time.Now().Add(-1 * time.Minute).UTC().Format(time.RFC3339)
	staleID, _, _ := insertSighting(db, camSighting{SiteID: "site_1", CameraID: "cam_1", AvatarID: "av_stale", StartedAt: staleSeen, LastSeenAt: staleSeen})
	freshID, _, _ := insertSighting(db, camSighting{SiteID: "site_1", CameraID: "cam_1", AvatarID: "av_fresh", StartedAt: freshSeen, LastSeenAt: freshSeen})

	camPresenceSweep(cfg, db)

	// Stale closed (departure pushed once); fresh untouched.
	if _, err := getOpenSighting(db, "site_1", "av_stale"); err == nil {
		t.Error("stale interval should have been closed by the sweep")
	}
	if _, err := getOpenSighting(db, "site_1", "av_fresh"); err != nil {
		t.Error("fresh interval must NOT be closed within the grace window")
	}
	if len(*rec) != 1 || (*rec)[0].Kind != "departure" || (*rec)[0].SightingID != staleID {
		t.Fatalf("sweep pushes = %+v, want one departure for the stale row", *rec)
	}
	_ = freshID

	// A second sweep closes nothing new (idempotent) and pushes nothing.
	camPresenceSweep(cfg, db)
	if len(*rec) != 1 {
		t.Fatalf("second sweep must be a no-op, pushes = %+v", *rec)
	}
}

// ─── helpers ───

func countPresenceRows(t *testing.T, db *sql.DB, avatarID string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM camera_sightings WHERE avatar_id = ?`, avatarID).Scan(&n); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	return n
}

func pushKinds(rec []struct{ Kind, AvatarID, SightingID string }) []string {
	out := make([]string, len(rec))
	for i, r := range rec {
		out[i] = r.Kind
	}
	return out
}
