package main

// camera_motion_test.go — DVR motion listener + history unit tests: Hikvision
// alert XML parsing, episode coalescing, cooldown/arm gating, the motion_search
// investigate tool's formatting, and the motion store CRUD + retention prune.

import (
	"database/sql"
	"strings"
	"testing"
	"time"
)

// ─────────────────────────── alert XML parsing ───────────────────────────

func TestCamMotionParseAlertVMDActiveChannelID(t *testing.T) {
	xml := `<?xml version="1.0" encoding="UTF-8"?>
<EventNotificationAlert version="2.0" xmlns="http://www.hikvision.com/ver20/XMLSchema">
  <ipAddress>192.168.1.64</ipAddress>
  <channelID>1</channelID>
  <dateTime>2026-07-03T08:02:15+03:00</dateTime>
  <activePostCount>1</activePostCount>
  <eventType>VMD</eventType>
  <eventState>active</eventState>
  <eventDescription>Motion alarm</eventDescription>
</EventNotificationAlert>`
	a, ok := camMotionParseAlert([]byte(xml))
	if !ok {
		t.Fatal("expected parse ok")
	}
	if a.EventType != "VMD" || a.EventState != "active" {
		t.Fatalf("unexpected type/state: %q/%q", a.EventType, a.EventState)
	}
	if ch := camMotionChannel(a); ch != 1 {
		t.Fatalf("channel = %d, want 1 (from channelID)", ch)
	}
	if got := camMotionStartUTC(a); got != "2026-07-03T05:02:15Z" {
		t.Fatalf("startUTC = %q, want 2026-07-03T05:02:15Z", got)
	}
}

func TestCamMotionParseAlertLineDetectionInactiveDynChannel(t *testing.T) {
	xml := `<EventNotificationAlert xmlns="http://www.hikvision.com/ver20/XMLSchema">
  <dynChannelID>3</dynChannelID>
  <dateTime>2026-07-03T08:05:40+03:00</dateTime>
  <eventType>linedetection</eventType>
  <eventState>inactive</eventState>
</EventNotificationAlert>`
	a, ok := camMotionParseAlert([]byte(xml))
	if !ok {
		t.Fatal("expected parse ok")
	}
	if a.EventType != "linedetection" || a.EventState != "inactive" {
		t.Fatalf("unexpected type/state: %q/%q", a.EventType, a.EventState)
	}
	if ch := camMotionChannel(a); ch != 3 {
		t.Fatalf("channel = %d, want 3 (fallback to dynChannelID)", ch)
	}
}

func TestCamMotionParseAlertGarbage(t *testing.T) {
	if _, ok := camMotionParseAlert([]byte("not xml at all")); ok {
		t.Fatal("expected parse to fail on garbage")
	}
	// A blank eventType defaults to "motion" so an episode always has a key.
	a, _ := camMotionParseAlert([]byte(`<EventNotificationAlert><channelID>2</channelID></EventNotificationAlert>`))
	if got := camMotionEventType(a); got != "motion" {
		t.Fatalf("blank eventType default = %q, want motion", got)
	}
}

// ─────────────────────────── episode coalescing ───────────────────────────

func seedMotionCamera(t *testing.T, db *sql.DB, siteID, dvrID string, channel int, name string) string {
	t.Helper()
	id, err := insertCamera(db, camera{SiteID: siteID, DVRID: dvrID, Channel: channel, Name: name, Enabled: true})
	if err != nil {
		t.Fatalf("insertCamera: %v", err)
	}
	return id
}

func TestCamMotionEpisodeCoalescing(t *testing.T) {
	db := openCamTestDB(t)
	if err := migrateCameraDB(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	cam := seedMotionCamera(t, db, "siteA", "dvr1", 1, "Reception")
	dvr := CamDVR{ID: "dvr1"}
	cfg := config{}
	episodes := map[string]*camMotionEpisode{}

	// Three active heartbeats for the same (camera, eventType) → ONE episode.
	for _, dt := range []string{"2026-07-03T08:02:15Z", "2026-07-03T08:02:16Z", "2026-07-03T08:02:20Z"} {
		camMotionHandleEvent(cfg, db, dvr, hikEventAlert{EventType: "VMD", EventState: "active", DateTime: dt, ChannelID: "1"}, episodes)
	}
	if n := countMotionRows(t, db); n != 1 {
		t.Fatalf("after 3 actives: %d rows, want 1 episode", n)
	}
	if len(episodes) != 1 {
		t.Fatalf("expected 1 open episode, got %d", len(episodes))
	}

	// inactive closes it (ended_at filled, episode removed from the map).
	camMotionHandleEvent(cfg, db, dvr, hikEventAlert{EventType: "VMD", EventState: "inactive", DateTime: "2026-07-03T08:02:40Z", ChannelID: "1"}, episodes)
	if len(episodes) != 0 {
		t.Fatalf("inactive should close the episode, %d still open", len(episodes))
	}
	var started, ended string
	if err := db.QueryRow(`SELECT started_at, ended_at FROM camera_motion_events WHERE camera_id = ?`, cam).Scan(&started, &ended); err != nil {
		t.Fatalf("select episode: %v", err)
	}
	if started != "2026-07-03T08:02:15Z" {
		t.Fatalf("started_at = %q", started)
	}
	if ended != "2026-07-03T08:02:40Z" {
		t.Fatalf("ended_at = %q, want the inactive time", ended)
	}

	// A different eventType on the same camera is a SEPARATE episode.
	camMotionHandleEvent(cfg, db, dvr, hikEventAlert{EventType: "linedetection", EventState: "active", DateTime: "2026-07-03T08:10:00Z", ChannelID: "1"}, episodes)
	if n := countMotionRows(t, db); n != 2 {
		t.Fatalf("distinct eventType should open a new episode: %d rows, want 2", n)
	}
}

func TestCamMotionUnmatchedChannelSkipped(t *testing.T) {
	db := openCamTestDB(t)
	if err := migrateCameraDB(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	seedMotionCamera(t, db, "siteA", "dvr1", 1, "Reception")
	episodes := map[string]*camMotionEpisode{}
	// Channel 99 maps to no camera → no row, no episode.
	camMotionHandleEvent(config{}, db, CamDVR{ID: "dvr1"}, hikEventAlert{EventType: "VMD", EventState: "active", DateTime: "2026-07-03T08:00:00Z", ChannelID: "99"}, episodes)
	if n := countMotionRows(t, db); n != 0 {
		t.Fatalf("unmatched channel should store nothing, got %d rows", n)
	}
	if len(episodes) != 0 {
		t.Fatalf("unmatched channel should open no episode, got %d", len(episodes))
	}
}

func TestCamMotionIdleSweepClosesEpisode(t *testing.T) {
	db := openCamTestDB(t)
	if err := migrateCameraDB(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	seedMotionCamera(t, db, "siteA", "dvr1", 1, "Reception")
	episodes := map[string]*camMotionEpisode{}
	camMotionHandleEvent(config{}, db, CamDVR{ID: "dvr1"}, hikEventAlert{EventType: "VMD", EventState: "active", DateTime: "2026-07-03T08:00:00Z", ChannelID: "1"}, episodes)
	if len(episodes) != 1 {
		t.Fatalf("expected 1 open episode")
	}
	// A sweep well past the idle gap closes the episode (firmware that never
	// sends inactive).
	camMotionSweep(db, episodes, time.Now().Add(camMotionIdleGap+time.Second))
	if len(episodes) != 0 {
		t.Fatalf("idle sweep should close the episode, %d still open", len(episodes))
	}
	var ended string
	if err := db.QueryRow(`SELECT ended_at FROM camera_motion_events`).Scan(&ended); err != nil {
		t.Fatalf("select: %v", err)
	}
	if strings.TrimSpace(ended) == "" {
		t.Fatal("idle sweep should have stamped ended_at")
	}
}

// ─────────────────────────── cooldown + arm matching ───────────────────────────

func TestCamMotionCooldownAllow(t *testing.T) {
	key := "siteX|camX|VMD|" + time.Now().Format(time.RFC3339Nano) // unique per run
	if !camMotionCooldownAllow(key, time.Minute) {
		t.Fatal("first fire should be allowed")
	}
	if camMotionCooldownAllow(key, time.Minute) {
		t.Fatal("second fire within cooldown must be suppressed")
	}

	short := key + "-short"
	if !camMotionCooldownAllow(short, 5*time.Millisecond) {
		t.Fatal("first short fire should allow")
	}
	time.Sleep(10 * time.Millisecond)
	if !camMotionCooldownAllow(short, 5*time.Millisecond) {
		t.Fatal("after ttl elapsed the fire should be allowed again")
	}

	if !camMotionCooldownAllow(key+"-zero", 0) {
		t.Fatal("ttl<=0 must always allow")
	}
}

func TestCamMotionArmMatches(t *testing.T) {
	if !camMotionArmMatches(camMotionArm{EventTypes: ""}, "VMD") {
		t.Fatal("empty filter should match any type")
	}
	if !camMotionArmMatches(camMotionArm{EventTypes: "[]"}, "VMD") {
		t.Fatal("[] filter should match any type")
	}
	if !camMotionArmMatches(camMotionArm{EventTypes: `["VMD","linedetection"]`}, "vmd") {
		t.Fatal("filter should match case-insensitively")
	}
	if camMotionArmMatches(camMotionArm{EventTypes: `["linedetection"]`}, "VMD") {
		t.Fatal("filter that omits VMD should not match VMD")
	}
}

// ─────────────────────────── motion_search tool ───────────────────────────

func TestCamToolMotionSearchFormatting(t *testing.T) {
	db := openCamTestDB(t)
	if err := migrateCameraDB(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	cam := seedMotionCamera(t, db, "siteA", "dvr1", 1, "Reception")

	ev1, _ := insertCameraMotionEvent(db, "siteA", cam, "VMD", "2020-01-15T05:02:15Z")
	if err := updateMotionEventEnd(db, ev1, "2020-01-15T05:02:40Z"); err != nil {
		t.Fatalf("updateEnd: %v", err)
	}
	if _, err := insertCameraMotionEvent(db, "siteA", cam, "linedetection", "2020-01-15T05:05:01Z"); err != nil {
		t.Fatalf("insert ev2: %v", err)
	}

	site := camSite{ID: "siteA", Name: "Clinic"}
	allowed := map[string]bool{cam: true}
	camByID := map[string]camera{cam: {ID: cam, Name: "Reception"}}
	dvrByID := map[string]CamDVR{"dvr1": {ID: "dvr1", Timezone: "UTC"}} // deterministic clock rendering

	args := investigateArgs{CameraIDs: []string{cam}, From: "2020-01-15T00:00:00Z", To: "2020-01-15T23:59:59Z"}
	res := camToolMotionSearch(config{}, db, site, args, allowed, camByID, dvrByID)

	if res.Fetches != 0 {
		t.Fatalf("motion_search must cost no media budget, Fetches=%d", res.Fetches)
	}
	if len(res.Media) != 0 || len(res.Images) != 0 {
		t.Fatal("motion_search must return no media/images")
	}
	for _, want := range []string{"2 motion episode", cam, "Reception", "05:02:15–05:02:40", "VMD", "05:05:01", "linedetection"} {
		if !strings.Contains(res.Summary, want) {
			t.Fatalf("summary missing %q:\n%s", want, res.Summary)
		}
	}

	// event_type filter narrows to one type.
	res2 := camToolMotionSearch(config{}, db, site, investigateArgs{CameraIDs: []string{cam}, From: "2020-01-15T00:00:00Z", To: "2020-01-15T23:59:59Z", EventType: "VMD"}, allowed, camByID, dvrByID)
	if !strings.Contains(res2.Summary, "1 motion episode") || strings.Contains(res2.Summary, "linedetection") {
		t.Fatalf("event_type filter did not narrow results:\n%s", res2.Summary)
	}

	// Empty log window → clean "no episodes" message, not an error.
	res3 := camToolMotionSearch(config{}, db, site, investigateArgs{CameraIDs: []string{cam}, From: "2019-01-01T00:00:00Z", To: "2019-01-02T00:00:00Z"}, allowed, camByID, dvrByID)
	if !strings.Contains(res3.Summary, "no motion episodes") {
		t.Fatalf("expected no-episodes message, got:\n%s", res3.Summary)
	}
}

// TestCameraMotionSearchDefaultAllowedOnly proves that when the model omits
// camera_ids, motion_search searches ONLY the investigation's allowed cameras
// (enabled cameras on enabled DVRs) — never disabled cameras that happen to have
// motion history in the same site (the LOW finding from the adversarial review).
func TestCameraMotionSearchDefaultAllowedOnly(t *testing.T) {
	db := openCamTestDB(t)
	if err := migrateCameraDB(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	enabled := seedMotionCamera(t, db, "siteA", "dvr1", 1, "Reception")
	// A disabled camera in the SAME site with its own motion history; it is NOT in
	// the allowed set, so an omitted-camera_ids search must not surface it.
	disabled := "cam_disabled"
	if _, err := insertCameraMotionEvent(db, "siteA", enabled, "VMD", "2020-01-15T05:00:00Z"); err != nil {
		t.Fatalf("insert enabled event: %v", err)
	}
	if _, err := insertCameraMotionEvent(db, "siteA", disabled, "fielddetection", "2020-01-15T06:00:00Z"); err != nil {
		t.Fatalf("insert disabled event: %v", err)
	}

	site := camSite{ID: "siteA", Name: "Clinic"}
	allowed := map[string]bool{enabled: true} // disabled deliberately absent
	camByID := map[string]camera{enabled: {ID: enabled, Name: "Reception"}, disabled: {ID: disabled, Name: "Backroom"}}
	dvrByID := map[string]CamDVR{"dvr1": {ID: "dvr1", Timezone: "UTC"}}

	// No camera_ids → default to the allowed set only.
	res := camToolMotionSearch(config{}, db, site, investigateArgs{From: "2020-01-15T00:00:00Z", To: "2020-01-15T23:59:59Z"}, allowed, camByID, dvrByID)
	if !strings.Contains(res.Summary, "1 motion episode") {
		t.Fatalf("expected only the 1 allowed-camera episode, got:\n%s", res.Summary)
	}
	if strings.Contains(res.Summary, "fielddetection") || strings.Contains(res.Summary, disabled) {
		t.Fatalf("disabled camera leaked into default-all motion_search:\n%s", res.Summary)
	}
}

// ─────────────────────────── store CRUD + prune ───────────────────────────

func TestCameraMotionArmUpsertAndList(t *testing.T) {
	db := openCamTestDB(t)
	if err := migrateCameraDB(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if err := upsertCameraMotionArm(db, "siteA", "cam1", `["VMD"]`, 30, true); err != nil {
		t.Fatalf("insert arm: %v", err)
	}
	// Upsert the SAME (site,camera) updates in place (UNIQUE constraint) — no dup.
	if err := upsertCameraMotionArm(db, "siteA", "cam1", `["VMD","linedetection"]`, 90, false); err != nil {
		t.Fatalf("update arm: %v", err)
	}
	arms, err := listCameraMotionArm(db, "siteA")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(arms) != 1 {
		t.Fatalf("expected 1 arm row after upsert, got %d", len(arms))
	}
	a := arms[0]
	if a.EventTypes != `["VMD","linedetection"]` || a.CooldownSeconds != 90 || a.Enabled {
		t.Fatalf("arm not updated in place: %+v", a)
	}
	got, err := getCameraMotionArm(db, "siteA", "cam1")
	if err != nil || got.CameraID != "cam1" {
		t.Fatalf("getCameraMotionArm: %v %+v", err, got)
	}
	if _, err := getCameraMotionArm(db, "siteA", "nope"); err != sql.ErrNoRows {
		t.Fatalf("missing arm should be ErrNoRows, got %v", err)
	}
}

func TestCameraMotionEventsInsertListPrune(t *testing.T) {
	db := openCamTestDB(t)
	if err := migrateCameraDB(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	mustInsert := func(cam, etype, started string) {
		if _, err := insertCameraMotionEvent(db, "siteA", cam, etype, started); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}
	mustInsert("cam1", "VMD", "2020-01-15T05:00:00Z")
	mustInsert("cam1", "linedetection", "2020-01-15T06:00:00Z")
	mustInsert("cam2", "VMD", "2020-01-15T07:00:00Z")
	// Another site — must never leak into siteA queries.
	mustInsert2 := func() {
		if _, err := insertCameraMotionEvent(db, "siteB", "camX", "VMD", "2020-01-15T05:00:00Z"); err != nil {
			t.Fatalf("insert siteB: %v", err)
		}
	}
	mustInsert2()

	all, _, err := listCameraMotionEvents(db, "siteA", nil, "", "", "")
	if err != nil {
		t.Fatalf("list all: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("siteA all = %d, want 3 (siteB must not leak)", len(all))
	}
	// Ordered oldest-first.
	if all[0].StartedAt > all[len(all)-1].StartedAt {
		t.Fatal("events not ordered oldest-first")
	}
	byCam, _, _ := listCameraMotionEvents(db, "siteA", []string{"cam1"}, "", "", "")
	if len(byCam) != 2 {
		t.Fatalf("cam1 filter = %d, want 2", len(byCam))
	}
	byType, _, _ := listCameraMotionEvents(db, "siteA", nil, "", "", "VMD")
	if len(byType) != 2 {
		t.Fatalf("VMD filter = %d, want 2", len(byType))
	}
	windowed, _, _ := listCameraMotionEvents(db, "siteA", nil, "2020-01-15T05:30:00Z", "2020-01-15T06:30:00Z", "")
	if len(windowed) != 1 || windowed[0].EventType != "linedetection" {
		t.Fatalf("window filter unexpected: %+v", windowed)
	}

	// Retention prune: created_at is server-now, so a future cutoff removes all.
	cutoff := time.Now().Add(time.Hour).Format(time.RFC3339)
	n, err := deleteOldCameraMotionEvents(db, cutoff)
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if n != 4 {
		t.Fatalf("pruned %d rows, want 4 (all)", n)
	}
	left, _, _ := listCameraMotionEvents(db, "siteA", nil, "", "", "")
	if len(left) != 0 {
		t.Fatalf("after prune siteA has %d rows, want 0", len(left))
	}
}

// TestMigrateCameraDBMotionIdempotent runs the migration twice and confirms the
// motion tables exist and seeded data survives (idempotent DDL).
func TestMigrateCameraDBMotionIdempotent(t *testing.T) {
	db := openCamTestDB(t)
	if err := migrateCameraDB(db); err != nil {
		t.Fatalf("migrate 1: %v", err)
	}
	if err := upsertCameraMotionArm(db, "siteA", "cam1", `["VMD"]`, 10, true); err != nil {
		t.Fatalf("seed arm: %v", err)
	}
	if _, err := insertCameraMotionEvent(db, "siteA", "cam1", "VMD", "2020-01-15T05:00:00Z"); err != nil {
		t.Fatalf("seed event: %v", err)
	}
	if err := migrateCameraDB(db); err != nil {
		t.Fatalf("migrate 2 (idempotent): %v", err)
	}
	if arms, _ := listCameraMotionArm(db, "siteA"); len(arms) != 1 {
		t.Fatalf("arm row lost across re-migration: %d", len(arms))
	}
	if evs, _, _ := listCameraMotionEvents(db, "siteA", nil, "", "", ""); len(evs) != 1 {
		t.Fatalf("event row lost across re-migration: %d", len(evs))
	}
}

func countMotionRows(t *testing.T, db *sql.DB) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM camera_motion_events`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	return n
}
