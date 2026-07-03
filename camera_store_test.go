package main

// camera_store_test.go — migrateCameraDB idempotency: running the migration
// twice against the same on-disk .proxy.db must not error and must not disturb
// data already written (mirrors the existing TestMetricsMigrate pattern for the
// metrics DB in main_test.go).

import (
	"database/sql"
	"path/filepath"
	"testing"
)

// camTestTables lists every table migrateCameraDB is documented to create, so a
// test failure that drops one is immediately legible.
var camTestTables = []string{
	"camera_sites",
	"camera_dvrs",
	"cameras",
	"camera_watches",
	"camera_watch_runs",
	"camera_questions",
	"camera_captures",
	"camera_events",
	"camera_playbooks",
	"camera_playbook_media",
	"camera_api_tools",
	"camera_avatars",
	"camera_avatar_media",
	"camera_avatar_scans",
	"camera_avatar_candidates",
	"camera_service_tokens",
	"camera_site_callbacks",
	"camera_motion_events",
	"camera_motion_arm",
}

func openCamTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), ".proxy.db"))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestMigrateCameraDBCreatesAllTables(t *testing.T) {
	db := openCamTestDB(t)
	if err := migrateCameraDB(db); err != nil {
		t.Fatalf("migrateCameraDB: %v", err)
	}
	for _, table := range camTestTables {
		var name string
		err := db.QueryRow(`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&name)
		if err != nil {
			t.Errorf("table %q missing after migrateCameraDB: %v", table, err)
		}
	}
}

// TestMigrateCameraDBIdempotent runs the migration twice on the same on-disk
// database (as would happen every proxy restart), then a third time after data
// has been inserted, and confirms: no error, no duplicate/renamed tables, and the
// inserted row survives untouched.
func TestMigrateCameraDBIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".proxy.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()

	if err := migrateCameraDB(db); err != nil {
		t.Fatalf("first migrateCameraDB: %v", err)
	}
	if err := migrateCameraDB(db); err != nil {
		t.Fatalf("second migrateCameraDB (idempotency) failed: %v", err)
	}

	id, err := insertCameraSite(db, camSite{Name: "Clinic A", Description: "front lobby"})
	if err != nil {
		t.Fatalf("insertCameraSite: %v", err)
	}

	// Re-run the migration a third time, now that there is data + indexes in
	// place — CREATE TABLE/INDEX IF NOT EXISTS must be a true no-op.
	if err := migrateCameraDB(db); err != nil {
		t.Fatalf("third migrateCameraDB (post-data) failed: %v", err)
	}

	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM camera_sites`).Scan(&n); err != nil {
		t.Fatalf("count camera_sites: %v", err)
	}
	if n != 1 {
		t.Fatalf("camera_sites count = %d, want 1 (migration must not duplicate/lose rows)", n)
	}

	var name string
	if err := db.QueryRow(`SELECT name FROM camera_sites WHERE id=?`, id).Scan(&name); err != nil {
		t.Fatalf("select camera_sites: %v", err)
	}
	if name != "Clinic A" {
		t.Errorf("name = %q, want %q", name, "Clinic A")
	}

	// Every documented index should also survive re-migration exactly once.
	wantIndexes := []string{
		"idx_cameras_site", "idx_cameras_dvr", "idx_camera_dvrs_site",
		"idx_camera_watches_due", "idx_camera_watch_runs_watch",
		"idx_camera_questions_site", "idx_camera_captures_token",
		"idx_camera_captures_run", "idx_camera_captures_expires",
		"idx_camera_events_ts", "idx_camera_events_dvr", "idx_camera_events_op",
	}
	for _, idx := range wantIndexes {
		var cnt int
		if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name=?`, idx).Scan(&cnt); err != nil {
			t.Fatalf("count index %q: %v", idx, err)
		}
		if cnt != 1 {
			t.Errorf("index %q count = %d, want exactly 1 after triple migration", idx, cnt)
		}
	}
}

// TestMigrateCameraDBIdempotentOnReopenedConnection simulates the real-world
// restart path: close the DB handle entirely and reopen the same file before
// migrating again (openProxyDB/migrateProxyDB's actual call pattern).
func TestMigrateCameraDBIdempotentOnReopenedConnection(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".proxy.db")

	db1, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	if err := migrateCameraDB(db1); err != nil {
		t.Fatalf("migrateCameraDB (first open): %v", err)
	}
	if _, err := insertCameraSite(db1, camSite{Name: "Site 1"}); err != nil {
		t.Fatalf("insertCameraSite: %v", err)
	}
	if err := db1.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	db2, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("sql.Open (reopen): %v", err)
	}
	defer db2.Close()
	if err := migrateCameraDB(db2); err != nil {
		t.Fatalf("migrateCameraDB (reopened connection) failed: %v", err)
	}

	var n int
	if err := db2.QueryRow(`SELECT COUNT(*) FROM camera_sites`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Fatalf("camera_sites count = %d, want 1 (data must survive a reopen+re-migrate)", n)
	}
}
