package main

// camera_avatar_scan_test.go — focused unit tests for the avatar-scan queue's
// PURE pieces (no network, no DVR, no VLM): match parsing tolerance, sighting
// clustering, instant coarsening, and the sqlite CAS/requeue/idempotency
// contract against a temp database (openCamTestDB + migrateCameraDB).

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"strings"
)

// ─────────────────────────── parseAvatarScanMatches ───────────────────────────

func TestAvScanParseMatchesPlain(t *testing.T) {
	raw := `{"matches":[{"frame":3,"confidence":0.82,"bbox":{"x0":412,"y0":180,"x1":530,"y1":690},"reason":"white coat"}]}`
	ms, err := parseAvatarScanMatches(raw)
	if err != nil {
		t.Fatalf("parseAvatarScanMatches: %v", err)
	}
	if len(ms) != 1 || int(ms[0].Frame) != 3 || ms[0].Confidence != 0.82 || ms[0].Reason != "white coat" {
		t.Fatalf("unexpected matches: %+v", ms)
	}
	if len(ms[0].BBox) == 0 {
		t.Fatalf("bbox raw JSON missing")
	}
}

func TestAvScanParseMatchesFenced(t *testing.T) {
	raw := "```json\n{\"matches\":[{\"frame\":1,\"confidence\":0.5,\"reason\":\"maybe\"}]}\n```"
	ms, err := parseAvatarScanMatches(raw)
	if err != nil {
		t.Fatalf("fenced: %v", err)
	}
	if len(ms) != 1 || int(ms[0].Frame) != 1 {
		t.Fatalf("fenced matches: %+v", ms)
	}
}

func TestAvScanParseMatchesProse(t *testing.T) {
	raw := `I looked at every frame carefully. Here is my answer: {"matches":[{"frame":"7","confidence":0.61,"reason":"same build"}]} hope this helps!`
	ms, err := parseAvatarScanMatches(raw)
	if err != nil {
		t.Fatalf("prose: %v", err)
	}
	// "7" exercises the flexInt string decode.
	if len(ms) != 1 || int(ms[0].Frame) != 7 || ms[0].Confidence != 0.61 {
		t.Fatalf("prose matches: %+v", ms)
	}
}

func TestAvScanParseMatchesStringifiedArray(t *testing.T) {
	raw := `{"matches":"[{\"frame\":2,\"confidence\":0.9,\"reason\":\"clear match\"}]"}`
	ms, err := parseAvatarScanMatches(raw)
	if err != nil {
		t.Fatalf("stringified: %v", err)
	}
	if len(ms) != 1 || int(ms[0].Frame) != 2 || ms[0].Confidence != 0.9 {
		t.Fatalf("stringified matches: %+v", ms)
	}
}

func TestAvScanParseMatchesSingleObject(t *testing.T) {
	raw := `{"matches":{"frame":4,"confidence":0.55,"reason":"one"}}`
	ms, err := parseAvatarScanMatches(raw)
	if err != nil {
		t.Fatalf("single object: %v", err)
	}
	if len(ms) != 1 || int(ms[0].Frame) != 4 {
		t.Fatalf("single-object matches: %+v", ms)
	}
}

func TestAvScanParseMatchesEmptyAndGarbage(t *testing.T) {
	ms, err := parseAvatarScanMatches(`{"matches":[]}`)
	if err != nil || len(ms) != 0 {
		t.Fatalf("empty matches: ms=%v err=%v", ms, err)
	}
	ms, err = parseAvatarScanMatches(`{"matches":null}`)
	if err != nil || len(ms) != 0 {
		t.Fatalf("null matches: ms=%v err=%v", ms, err)
	}
	if _, err = parseAvatarScanMatches("no json here at all"); err == nil {
		t.Fatalf("garbage should error")
	}
}

// ─────────────────────────── clustering ───────────────────────────

func TestAvScanClusterSightingsChains(t *testing.T) {
	base := time.Date(2026, 7, 2, 8, 0, 0, 0, time.UTC)
	// 0s,5s,15s chain (<20s gaps) into ONE cluster; 40s,45s form a second.
	keys := []avSightingKey{
		{T: base, Conf: 0.5},
		{T: base.Add(5 * time.Second), Conf: 0.9},
		{T: base.Add(15 * time.Second), Conf: 0.6},
		{T: base.Add(40 * time.Second), Conf: 0.7},
		{T: base.Add(45 * time.Second), Conf: 0.3},
	}
	got := avScanClusterSightings(keys, 20*time.Second)
	if len(got) != 2 || got[0] != 1 || got[1] != 3 {
		t.Fatalf("cluster indices = %v, want [1 3]", got)
	}
}

func TestAvScanClusterSightingsUnsortedInput(t *testing.T) {
	base := time.Date(2026, 7, 2, 8, 0, 0, 0, time.UTC)
	keys := []avSightingKey{
		{T: base.Add(45 * time.Second), Conf: 0.3}, // idx 0 — cluster 2
		{T: base.Add(5 * time.Second), Conf: 0.9},  // idx 1 — cluster 1 best
		{T: base, Conf: 0.5},                       // idx 2 — cluster 1
		{T: base.Add(40 * time.Second), Conf: 0.7}, // idx 3 — cluster 2 best
	}
	got := avScanClusterSightings(keys, 20*time.Second)
	if len(got) != 2 || got[0] != 1 || got[1] != 3 {
		t.Fatalf("unsorted cluster indices = %v, want [1 3]", got)
	}
}

func TestAvScanClusterSightingsBoundaryAndEmpty(t *testing.T) {
	if got := avScanClusterSightings(nil, 20*time.Second); got != nil {
		t.Fatalf("empty input: %v", got)
	}
	base := time.Date(2026, 7, 2, 8, 0, 0, 0, time.UTC)
	// Exactly 20s apart is NOT < gap — two clusters.
	keys := []avSightingKey{{T: base, Conf: 0.4}, {T: base.Add(20 * time.Second), Conf: 0.8}}
	got := avScanClusterSightings(keys, 20*time.Second)
	if len(got) != 2 {
		t.Fatalf("boundary gap should split: %v", got)
	}
	// Single sighting → itself.
	got = avScanClusterSightings(keys[:1], 20*time.Second)
	if len(got) != 1 || got[0] != 0 {
		t.Fatalf("single sighting: %v", got)
	}
}

// ─────────────────────────── instant coarsening ───────────────────────────

func TestAvScanInstantsShortWindow(t *testing.T) {
	from := time.Date(2026, 7, 2, 8, 0, 0, 0, time.UTC)
	got := avScanInstants(from, from.Add(100*time.Second), 600)
	if len(got) != 101 {
		t.Fatalf("short window instants = %d, want 101 (1s grid inclusive)", len(got))
	}
	if step := got[1].Sub(got[0]); step != time.Second {
		t.Fatalf("short window step = %v, want 1s", step)
	}
}

func TestAvScanInstantsCoarsensLongWindow(t *testing.T) {
	from := time.Date(2026, 7, 2, 6, 0, 0, 0, time.UTC)
	got := avScanInstants(from, from.Add(time.Hour), 600) // 3600s / 600 → 6s step
	if len(got) == 0 || len(got) > 600 {
		t.Fatalf("long window instants = %d, want 1..600", len(got))
	}
	if step := got[1].Sub(got[0]); step != 6*time.Second {
		t.Fatalf("long window step = %v, want 6s", step)
	}
	if got[0] != from {
		t.Fatalf("first instant = %v, want %v", got[0], from)
	}
}

func TestAvScanInstantsBounds(t *testing.T) {
	from := time.Date(2026, 7, 2, 6, 0, 0, 0, time.UTC)
	// Degenerate window: from == to → exactly the one instant.
	if got := avScanInstants(from, from, 600); len(got) != 1 {
		t.Fatalf("from==to instants = %d, want 1", len(got))
	}
	// maxFrames <= 0 falls back to 600 and still caps.
	got := avScanInstants(from, from.Add(24*time.Hour), 0)
	if len(got) == 0 || len(got) > 600 {
		t.Fatalf("default-cap instants = %d, want 1..600", len(got))
	}
	// Tiny cap honored.
	got = avScanInstants(from, from.Add(time.Hour), 10)
	if len(got) > 10 {
		t.Fatalf("cap-10 instants = %d, want <= 10", len(got))
	}
}

// ─────────────────────────── queue store: CAS + requeue ───────────────────────────

func TestAvScanQueueCASAndRequeue(t *testing.T) {
	db := openCamTestDB(t)
	if err := migrateCameraDB(db); err != nil {
		t.Fatalf("migrateCameraDB: %v", err)
	}
	id, err := insertCameraAvatarScan(db, camAvatarScan{
		AvatarID: "avatar_1", SiteID: "site_1",
		FromTS: "2026-07-02T06:00:00Z", ToTS: "2026-07-02T08:00:00Z",
	})
	if err != nil {
		t.Fatalf("insertCameraAvatarScan: %v", err)
	}
	if !strings.HasPrefix(id, "avscan") {
		t.Errorf("scan id = %q, want avscan* prefix", id)
	}

	queued, err := listQueuedCameraAvatarScans(db)
	if err != nil || len(queued) != 1 || queued[0].ID != id {
		t.Fatalf("listQueued = %v, %v", queued, err)
	}

	// CAS: first claim wins, second loses.
	won, err := claimCameraAvatarScan(db, id)
	if err != nil || !won {
		t.Fatalf("first claim: won=%v err=%v", won, err)
	}
	won, err = claimCameraAvatarScan(db, id)
	if err != nil || won {
		t.Fatalf("second claim must lose: won=%v err=%v", won, err)
	}
	sc, err := getCameraAvatarScan(db, id)
	if err != nil || sc.Status != "running" {
		t.Fatalf("post-claim status = %q err=%v, want running", sc.Status, err)
	}

	// Startup recovery requeues running rows.
	n, err := requeueRunningCameraAvatarScans(db)
	if err != nil || n != 1 {
		t.Fatalf("requeueRunning = %d, %v, want 1", n, err)
	}
	sc, _ = getCameraAvatarScan(db, id)
	if sc.Status != "queued" {
		t.Fatalf("post-requeue status = %q, want queued", sc.Status)
	}

	// Stale reaper: reclaim then requeue with a cutoff in the FUTURE (negative
	// olderThan) so the just-touched row already counts as stale.
	if won, _ := claimCameraAvatarScan(db, id); !won {
		t.Fatalf("reclaim failed")
	}
	n, err = requeueStaleRunningCameraAvatarScans(db, -2*time.Second)
	if err != nil || n != 1 {
		t.Fatalf("requeueStale = %d, %v, want 1", n, err)
	}
	// A fresh running row with a normal olderThan is NOT stale.
	if won, _ := claimCameraAvatarScan(db, id); !won {
		t.Fatalf("re-reclaim failed")
	}
	n, err = requeueStaleRunningCameraAvatarScans(db, time.Hour)
	if err != nil || n != 0 {
		t.Fatalf("fresh row requeued as stale: n=%d err=%v", n, err)
	}

	// Terminalization + progress writes.
	if err := setCameraAvatarScanProgress(db, id, `{"cameras_done":["cam1"],"cameras_total":2}`); err != nil {
		t.Fatalf("setProgress: %v", err)
	}
	if err := setCameraAvatarScanStatus(db, id, "error", "boom"); err != nil {
		t.Fatalf("setStatus: %v", err)
	}
	sc, _ = getCameraAvatarScan(db, id)
	if sc.Status != "error" || sc.Error != "boom" || !strings.Contains(sc.Progress, "cam1") {
		t.Fatalf("terminal row = %+v", sc)
	}
	// A terminal row is neither queued nor requeued.
	if q, _ := listQueuedCameraAvatarScans(db); len(q) != 0 {
		t.Fatalf("terminal row still queued: %v", q)
	}
	if n, _ := requeueRunningCameraAvatarScans(db); n != 0 {
		t.Fatalf("terminal row requeued: %d", n)
	}

	// listCameraAvatarScans filters.
	if got, _ := listCameraAvatarScans(db, "avatar_1", ""); len(got) != 1 {
		t.Fatalf("list by avatar: %v", got)
	}
	if got, _ := listCameraAvatarScans(db, "", "site_1"); len(got) != 1 {
		t.Fatalf("list by site: %v", got)
	}
	if got, _ := listCameraAvatarScans(db, "nope", ""); len(got) != 0 {
		t.Fatalf("list by wrong avatar: %v", got)
	}
}

// ─────────────────────────── candidate store: INSERT OR IGNORE ───────────────────────────

func TestAvScanCandidateInsertIdempotent(t *testing.T) {
	db := openCamTestDB(t)
	if err := migrateCameraDB(db); err != nil {
		t.Fatalf("migrateCameraDB: %v", err)
	}
	c := camAvatarCandidate{
		ScanID: "avscan_1", AvatarID: "avatar_1", CameraID: "cam_1",
		FrameTS: "2026-07-02T08:12:45Z", Quality: "sub",
		MatchKind: "vlm", VLMConfidence: 0.82, VLMReason: "white coat",
	}
	id1, err := insertCameraAvatarCandidate(db, c)
	if err != nil || id1 == "" {
		t.Fatalf("first insert: id=%q err=%v", id1, err)
	}
	if !strings.HasPrefix(id1, "avcand") {
		t.Errorf("candidate id = %q, want avcand* prefix", id1)
	}
	// Same (scan_id, camera_id, frame_ts) → ignored, "" id, still one row.
	id2, err := insertCameraAvatarCandidate(db, c)
	if err != nil {
		t.Fatalf("duplicate insert errored: %v", err)
	}
	if id2 != "" {
		t.Fatalf("duplicate insert id = %q, want \"\"", id2)
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM camera_avatar_candidates`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("candidate count = %d err=%v, want 1", n, err)
	}
	// A different frame_ts inserts fine.
	c.FrameTS = "2026-07-02T08:13:05Z"
	if id3, err := insertCameraAvatarCandidate(db, c); err != nil || id3 == "" {
		t.Fatalf("distinct insert: id=%q err=%v", id3, err)
	}

	// Filters + review stamping + pending count (site join via camera_avatars).
	if _, err := db.Exec(`INSERT INTO camera_avatars (id, site_id, name, type, created_at, updated_at)
		VALUES ('avatar_1', 'site_1', 'Dr. Ahmad', 'human', ?, ?)`, nowRFC3339(), nowRFC3339()); err != nil {
		t.Fatalf("seed avatar row: %v", err)
	}
	if got, _ := listCameraAvatarCandidates(db, "avscan_1", "", ""); len(got) != 2 {
		t.Fatalf("list by scan: %d, want 2", len(got))
	}
	if got, _ := listCameraAvatarCandidates(db, "", "avatar_1", "pending"); len(got) != 2 {
		t.Fatalf("list pending by avatar: %d, want 2", len(got))
	}
	if n, err := countPendingAvatarCandidates(db, "site_1"); err != nil || n != 2 {
		t.Fatalf("countPending = %d err=%v, want 2", n, err)
	}
	if err := setCameraAvatarCandidateReview(db, id1, "grouped", "avatar_grp", "our patients"); err != nil {
		t.Fatalf("review: %v", err)
	}
	got, err := getCameraAvatarCandidate(db, id1)
	if err != nil || got.Status != "grouped" || got.AssignedAvatarID != "avatar_grp" || got.Note != "our patients" || got.ReviewedAt == "" {
		t.Fatalf("reviewed candidate = %+v err=%v", got, err)
	}
	if n, _ := countPendingAvatarCandidates(db, "site_1"); n != 1 {
		t.Fatalf("countPending after review = %d, want 1", n)
	}

	// Retention pruning: rows created before the cutoff go away.
	old := camAvatarCandidate{
		ScanID: "avscan_old", AvatarID: "avatar_1", CameraID: "cam_1",
		FrameTS: "2026-06-01T08:00:00Z", CreatedAt: "2026-06-01T09:00:00Z",
	}
	if _, err := insertCameraAvatarCandidate(db, old); err != nil {
		t.Fatalf("old insert: %v", err)
	}
	deleted, err := deleteExpiredAvatarCandidates(db, "2026-06-25T00:00:00Z")
	if err != nil || deleted != 1 {
		t.Fatalf("deleteExpired = %d err=%v, want 1", deleted, err)
	}
	if got, _ := listCameraAvatarCandidates(db, "avscan_old", "", ""); len(got) != 0 {
		t.Fatalf("expired candidate survived: %v", got)
	}
}

// ─────────────────────────── concurrent camera scanning ───────────────────────────

// TestAvScanConcurrentProgressMutators drives every shared-state mutator from
// many goroutines at once — the exact contention the per-camera worker pool
// creates — and asserts (a) zero lost updates (mutex correctness) and (b) no
// data race when run under `go test -race`. Progress-row DB writes all happen
// under r.mu (via saveProgressLocked), so they self-serialize; production's
// concurrent candidate INSERTs instead lean on openProxyDB's WAL+busy_timeout.
func TestAvScanConcurrentProgressMutators(t *testing.T) {
	db := openCamTestDB(t)
	if err := migrateCameraDB(db); err != nil {
		t.Fatalf("migrateCameraDB: %v", err)
	}
	id, err := insertCameraAvatarScan(db, camAvatarScan{
		AvatarID: "avatar_1", SiteID: "site_1",
		FromTS: "2026-07-02T06:00:00Z", ToTS: "2026-07-02T08:00:00Z",
	})
	if err != nil {
		t.Fatalf("insertCameraAvatarScan: %v", err)
	}

	r := &avScanRun{
		db:        db,
		sc:        camAvatarScan{ID: id, SiteID: "site_1"},
		prog:      &avScanProgress{CamerasTotal: 999},
		persisted: map[string]bool{},
		fetchSem:  make(chan struct{}, avScanFetchConc),
	}

	const workers, each = 8, 150
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < each; i++ {
				r.addFrames(1)
				r.bumpVLM()
				r.setCurrentCamera(fmt.Sprintf("cam_%d_%d", w, i))
				r.markCameraDone(fmt.Sprintf("cam_%d_%d", w, i))
				// Mirror persistSightings' locked critical section (map + counter).
				pk := fmt.Sprintf("cam_%d|%d", w, i)
				r.mu.Lock()
				r.persisted[pk] = true
				r.prog.Candidates++
				r.saveProgressLocked()
				r.mu.Unlock()
				// Concurrent readers must also be race-free.
				_ = r.vlmBudgetHit()
				_ = r.candBudgetHit()
				_, _, _, _ = r.progressSnapshot()
				r.noteOnce("touched")
			}
		}(w)
	}
	wg.Wait()

	want := workers * each
	if r.prog.FramesScanned != want {
		t.Errorf("FramesScanned = %d, want %d (lost updates)", r.prog.FramesScanned, want)
	}
	if r.prog.VLMCalls != want {
		t.Errorf("VLMCalls = %d, want %d (lost updates)", r.prog.VLMCalls, want)
	}
	if r.prog.Candidates != want {
		t.Errorf("Candidates = %d, want %d (lost updates)", r.prog.Candidates, want)
	}
	if len(r.prog.CamerasDone) != want {
		t.Errorf("CamerasDone = %d, want %d (lost appends)", len(r.prog.CamerasDone), want)
	}
	if len(r.persisted) != want {
		t.Errorf("persisted = %d, want %d (map races/lost writes)", len(r.persisted), want)
	}
	if r.prog.Note != "touched" {
		t.Errorf("Note = %q, want stable single value", r.prog.Note)
	}
	// The final progress row round-trips.
	sc, _ := getCameraAvatarScan(db, id)
	if !strings.Contains(sc.Progress, `"frames_scanned":`) {
		t.Errorf("progress row not persisted: %q", sc.Progress)
	}
}

// ─────────────────────────── system prompt variants ───────────────────────────

func TestAvScanSystemPromptVariants(t *testing.T) {
	av := camAvatar{Name: "Dr. Ahmad", Type: "human", Description: "white coat"}
	withRefs := camAvatarScanSystemPrompt(av, true, 704, 576)
	if !strings.Contains(withRefs, "REFERENCE image(s)") || strings.Contains(withRefs, "no reference images yet") {
		t.Fatalf("with-refs prompt wrong variant:\n%s", withRefs)
	}
	if !strings.Contains(withRefs, `{"matches":[{"frame":3`) {
		t.Fatalf("with-refs prompt missing JSON contract:\n%s", withRefs)
	}
	noRefs := camAvatarScanSystemPrompt(av, false, 704, 576)
	if !strings.Contains(noRefs, "no reference images yet") || !strings.Contains(noRefs, "extra conservative") {
		t.Fatalf("no-refs prompt missing bootstrap variant:\n%s", noRefs)
	}
	// Type threads into the opener; blank type defaults to human.
	if !strings.Contains(camAvatarScanSystemPrompt(camAvatar{Type: "vehicle"}, true, 0, 0), "known vehicle") {
		t.Fatalf("type not threaded")
	}
	if !strings.Contains(camAvatarScanSystemPrompt(camAvatar{}, true, 0, 0), "known human") {
		t.Fatalf("blank type should default to human")
	}
}
