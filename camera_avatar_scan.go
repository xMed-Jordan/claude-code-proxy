package main

// camera_avatar_scan.go — the durable avatar enrollment-scan queue
// (design-avatars.md §2.3), CLONED from the investigation queue's shape
// (camera_investigate.go / camera_store.go): status CAS claim, listQueued,
// startup requeue, stale-running reaper, worker tick + detached goroutine with
// recover(). Started from cli.go right after startCameraInvestigationWorker.
//
// A scan walks the S3 1-fps frame archive of the selected cameras across
// [from_ts,to_ts] (instants coarsened to ≤ cfg.CameraAvatarScanMaxFrames,
// 16-way parallel sub-quality GETs, camMotionSignature/camMotionScore > 2.0
// prefilter), matches each active instant against the avatar — FACE PATH first
// (humans with embedded refs + sidecar up: MAIN frame, camFaceDetect, cosine ≥
// cfg.FaceMatchThreshold ⇒ match_kind "face") with the VLM batch path as
// fallback (cfg.CameraAvatarScanBatch frames + ≤ cfg.CameraAvatarMaxRefs refs
// per analyzeWithAlias call, confidence ≥ 0.4 ⇒ match_kind "vlm") — clusters
// sightings <20s apart, annotates (circle) + crops each keeper, persists both
// via camPersistCaptureExpires(now + cfg.CameraAvatarCandidateRetention), and
// INSERT OR IGNOREs a candidate row (the UNIQUE(scan_id,camera_id,frame_ts)
// index makes resume idempotent). Budgets: cfg.CameraAvatarScanMaxVLM calls,
// cfg.CameraAvatarScanMaxCandidates rows, camAvatarScanBudget wall clock —
// exhaustion finishes gracefully as "done" with a progress note.

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	avScanFetchConc  = 16               // parallel S3 GETs while scanning (camContactFetchConc)
	avScanMotionMin  = 2.0              // motion-score floor vs the previous KEPT frame
	avScanVLMConfMin = 0.4              // VLM confidence floor for a candidate proposal
	avScanClusterGap = 20 * time.Second // sightings closer than this chain into one cluster
)

// camAvatarScan is an in-memory view of a camera_avatar_scans row.
type camAvatarScan struct {
	ID        string
	AvatarID  string
	SiteID    string
	CameraIDs string // JSON array; '' = all cameras on the avatar's DVRs
	FromTS    string // RFC3339
	ToTS      string // RFC3339
	Alias     string // "" → cfg.CameraAvatarScanAlias → cfg.CameraInvestigateAlias
	Status    string // queued | running | done | error | cancelled
	Progress  string // JSON {"cameras_done","cameras_total","frames_scanned","vlm_calls","candidates","current_camera"}
	Error     string
	CreatedAt string
	UpdatedAt string
}

// camAvatarCandidate is an in-memory view of a camera_avatar_candidates row —
// one proposed sighting awaiting operator review.
type camAvatarCandidate struct {
	ID               string
	ScanID           string
	AvatarID         string
	CameraID         string
	FrameTS          string  // RFC3339 UTC (S3 grid instant → re-fetchable forever)
	Quality          string  // sub | main
	AnnotatedToken   string  // circled FULL frame (review display), retention expiry
	CropToken        string  // person crop (becomes the reference), retention expiry
	BBox             string  // normalized bbox JSON
	MatchKind        string  // face | vlm
	VLMConfidence    float64 // VLM confidence OR face cosine similarity
	VLMReason        string  // shown in review UI ("white coat, staff door" / "face similarity 0.62")
	Status           string  // pending | approved | rejected | grouped
	AssignedAvatarID string  // set when approved into a DIFFERENT avatar (a group)
	Note             string
	ReviewedAt       string
	CreatedAt        string
}

// avScanMatch is one frame match parsed from a VLM batch reply
// ({"matches":[{"frame":3,"confidence":0.82,"bbox":{...},"reason":"..."}]}).
type avScanMatch struct {
	Frame      flexInt         `json:"frame"`
	Confidence float64         `json:"confidence"`
	BBox       json.RawMessage `json:"bbox"`
	Reason     string          `json:"reason"`
}

// avScanProgress is the scan row's progress JSON — updated after each camera so
// a crash-requeued run resumes past cameras_done and the UI can render a bar.
type avScanProgress struct {
	CamerasDone   []string `json:"cameras_done"`
	CamerasTotal  int      `json:"cameras_total"`
	FramesScanned int      `json:"frames_scanned"`
	VLMCalls      int      `json:"vlm_calls"`
	Candidates    int      `json:"candidates"`
	CurrentCamera string   `json:"current_camera"`
	Note          string   `json:"note,omitempty"`
}

// ─────────────────────────── scan store (queue) ───────────────────────────

const camAvatarScanCols = `id, avatar_id, site_id, camera_ids, from_ts, to_ts, alias, status, progress, error, created_at, updated_at`

func scanAvatarScanRow(s rowScanner) (camAvatarScan, error) {
	var v camAvatarScan
	err := s.Scan(&v.ID, &v.AvatarID, &v.SiteID, &v.CameraIDs, &v.FromTS, &v.ToTS,
		&v.Alias, &v.Status, &v.Progress, &v.Error, &v.CreatedAt, &v.UpdatedAt)
	return v, err
}

func insertCameraAvatarScan(db *sql.DB, sc camAvatarScan) (string, error) {
	if sc.ID == "" {
		sc.ID = randomID("avscan")
	}
	if sc.Status == "" {
		sc.Status = "queued"
	}
	now := nowRFC3339()
	if sc.CreatedAt == "" {
		sc.CreatedAt = now
	}
	if sc.UpdatedAt == "" {
		sc.UpdatedAt = now
	}
	_, err := db.Exec(`INSERT INTO camera_avatar_scans
		(id, avatar_id, site_id, camera_ids, from_ts, to_ts, alias, status, progress, error, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		sc.ID, sc.AvatarID, sc.SiteID, sc.CameraIDs, sc.FromTS, sc.ToTS, sc.Alias,
		sc.Status, sc.Progress, sc.Error, sc.CreatedAt, sc.UpdatedAt)
	if err != nil {
		return "", err
	}
	return sc.ID, nil
}

func getCameraAvatarScan(db *sql.DB, id string) (camAvatarScan, error) {
	return scanAvatarScanRow(db.QueryRow(`SELECT `+camAvatarScanCols+` FROM camera_avatar_scans WHERE id = ?`, id))
}

// listCameraAvatarScans lists scans newest-first by avatar (avatarID != "") or
// site (siteID != "").
func listCameraAvatarScans(db *sql.DB, avatarID, siteID string) ([]camAvatarScan, error) {
	var (
		rows *sql.Rows
		err  error
	)
	switch {
	case avatarID != "":
		rows, err = db.Query(`SELECT `+camAvatarScanCols+` FROM camera_avatar_scans WHERE avatar_id = ? ORDER BY created_at DESC`, avatarID)
	case siteID != "":
		rows, err = db.Query(`SELECT `+camAvatarScanCols+` FROM camera_avatar_scans WHERE site_id = ? ORDER BY created_at DESC`, siteID)
	default:
		rows, err = db.Query(`SELECT ` + camAvatarScanCols + ` FROM camera_avatar_scans ORDER BY created_at DESC`)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []camAvatarScan
	for rows.Next() {
		v, err := scanAvatarScanRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// claimCameraAvatarScan atomically transitions queued→running (CAS; the
// claimCameraInvestigation idiom) — true only when THIS caller won the claim,
// so two worker ticks (or two proxy instances) can never run the same scan twice.
func claimCameraAvatarScan(db *sql.DB, id string) (bool, error) {
	res, err := db.Exec(`UPDATE camera_avatar_scans SET status = 'running', updated_at = ?
		WHERE id = ? AND status = 'queued'`, nowRFC3339(), id)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n == 1, nil
}

func listQueuedCameraAvatarScans(db *sql.DB) ([]camAvatarScan, error) {
	rows, err := db.Query(`SELECT ` + camAvatarScanCols +
		` FROM camera_avatar_scans WHERE status = 'queued' ORDER BY updated_at ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []camAvatarScan
	for rows.Next() {
		v, err := scanAvatarScanRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// requeueRunningCameraAvatarScans resets scans stranded in "running" back to
// "queued" (startup crash recovery; resume skips cameras_done + the unique
// candidate index dedupes).
func requeueRunningCameraAvatarScans(db *sql.DB) (int64, error) {
	res, err := db.Exec(`UPDATE camera_avatar_scans SET status = 'queued', updated_at = ?
		WHERE status = 'running'`, nowRFC3339())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// requeueStaleRunningCameraAvatarScans is the RUNTIME reaper: requeues
// "running" rows idle longer than olderThan (orphaned goroutine / lost
// terminalization write). A healthy run bumps updated_at on every progress
// write and its context cannot outlive the budget, so idle > budget+margin is
// safe to re-run.
func requeueStaleRunningCameraAvatarScans(db *sql.DB, olderThan time.Duration) (int64, error) {
	cutoff := time.Now().Add(-olderThan).Format(time.RFC3339)
	res, err := db.Exec(`UPDATE camera_avatar_scans SET status = 'queued', updated_at = ?
		WHERE status = 'running' AND updated_at < ?`, nowRFC3339(), cutoff)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func setCameraAvatarScanStatus(db *sql.DB, id, status, errMsg string) error {
	_, err := db.Exec(`UPDATE camera_avatar_scans SET status = ?, error = ?, updated_at = ? WHERE id = ?`,
		status, errMsg, nowRFC3339(), id)
	return err
}

func setCameraAvatarScanProgress(db *sql.DB, id, progressJSON string) error {
	_, err := db.Exec(`UPDATE camera_avatar_scans SET progress = ?, updated_at = ? WHERE id = ?`,
		progressJSON, nowRFC3339(), id)
	return err
}

// ─────────────────────────── candidate store ───────────────────────────

const camAvatarCandidateCols = `id, scan_id, avatar_id, camera_id, frame_ts, quality, annotated_token, crop_token,
	bbox, match_kind, vlm_confidence, vlm_reason, status, assigned_avatar_id, note, reviewed_at, created_at`

func scanAvatarCandidateRow(s rowScanner) (camAvatarCandidate, error) {
	var c camAvatarCandidate
	err := s.Scan(&c.ID, &c.ScanID, &c.AvatarID, &c.CameraID, &c.FrameTS, &c.Quality,
		&c.AnnotatedToken, &c.CropToken, &c.BBox, &c.MatchKind, &c.VLMConfidence,
		&c.VLMReason, &c.Status, &c.AssignedAvatarID, &c.Note, &c.ReviewedAt, &c.CreatedAt)
	return c, err
}

// insertCameraAvatarCandidate INSERT OR IGNOREs a candidate (the
// UNIQUE(scan_id, camera_id, frame_ts) index makes scan resume idempotent).
// Returns the id ("" when ignored as a duplicate).
func insertCameraAvatarCandidate(db *sql.DB, c camAvatarCandidate) (string, error) {
	if c.ID == "" {
		c.ID = randomID("avcand")
	}
	if c.Status == "" {
		c.Status = "pending"
	}
	if c.CreatedAt == "" {
		c.CreatedAt = nowRFC3339()
	}
	res, err := db.Exec(`INSERT OR IGNORE INTO camera_avatar_candidates
		(id, scan_id, avatar_id, camera_id, frame_ts, quality, annotated_token, crop_token,
		 bbox, match_kind, vlm_confidence, vlm_reason, status, assigned_avatar_id, note, reviewed_at, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		c.ID, c.ScanID, c.AvatarID, c.CameraID, c.FrameTS, c.Quality, c.AnnotatedToken, c.CropToken,
		c.BBox, c.MatchKind, c.VLMConfidence, c.VLMReason, c.Status, c.AssignedAvatarID, c.Note, c.ReviewedAt, c.CreatedAt)
	if err != nil {
		return "", err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return "", err
	}
	if n == 0 {
		return "", nil // duplicate (scan_id, camera_id, frame_ts) — already proposed
	}
	return c.ID, nil
}

func getCameraAvatarCandidate(db *sql.DB, id string) (camAvatarCandidate, error) {
	return scanAvatarCandidateRow(db.QueryRow(`SELECT `+camAvatarCandidateCols+` FROM camera_avatar_candidates WHERE id = ?`, id))
}

// listCameraAvatarCandidates filters by scan (scanID != "") or avatar
// (avatarID != ""), and by status ("" = all).
func listCameraAvatarCandidates(db *sql.DB, scanID, avatarID, status string) ([]camAvatarCandidate, error) {
	var conds []string
	var args []any
	if scanID != "" {
		conds = append(conds, "scan_id = ?")
		args = append(args, scanID)
	}
	if avatarID != "" {
		conds = append(conds, "avatar_id = ?")
		args = append(args, avatarID)
	}
	if status != "" {
		conds = append(conds, "status = ?")
		args = append(args, status)
	}
	q := `SELECT ` + camAvatarCandidateCols + ` FROM camera_avatar_candidates`
	if len(conds) > 0 {
		q += ` WHERE ` + strings.Join(conds, " AND ")
	}
	q += ` ORDER BY created_at DESC`
	rows, err := db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []camAvatarCandidate
	for rows.Next() {
		c, err := scanAvatarCandidateRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// countPendingAvatarCandidates returns the site-wide pending-review count (the
// avatars list response's pending_candidates / the UI tab badge). siteID "" =
// pending across all sites.
func countPendingAvatarCandidates(db *sql.DB, siteID string) (int, error) {
	var n int
	var err error
	if siteID == "" {
		err = db.QueryRow(`SELECT COUNT(*) FROM camera_avatar_candidates WHERE status = 'pending'`).Scan(&n)
	} else {
		err = db.QueryRow(`SELECT COUNT(*) FROM camera_avatar_candidates c
			JOIN camera_avatars a ON a.id = c.avatar_id
			WHERE a.site_id = ? AND c.status = 'pending'`, siteID).Scan(&n)
	}
	return n, err
}

// setCameraAvatarCandidateReview stamps a review decision: status
// (approved|rejected|grouped), the group's assignedAvatarID (when grouped),
// the operator note, and reviewed_at.
func setCameraAvatarCandidateReview(db *sql.DB, id, status, assignedAvatarID, note string) error {
	_, err := db.Exec(`UPDATE camera_avatar_candidates
		SET status = ?, assigned_avatar_id = ?, note = ?, reviewed_at = ? WHERE id = ?`,
		status, assignedAvatarID, note, nowRFC3339(), id)
	return err
}

// deleteExpiredAvatarCandidates prunes candidate rows past the retention
// window (created_at < olderThan, an RFC3339 cutoff) whose media the reaper
// already reclaimed. Returns rows removed.
func deleteExpiredAvatarCandidates(db *sql.DB, olderThan string) (int64, error) {
	res, err := db.Exec(`DELETE FROM camera_avatar_candidates WHERE created_at < ?`, olderThan)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// ─────────────────────────── worker ───────────────────────────

// camAvatarScanBudget resolves the wall-clock budget for one scan run, hard
// capped at 1h so a misconfigured override can never run a scan unbounded.
func camAvatarScanBudget(cfg config) time.Duration {
	d := cfg.CameraAvatarScanBudget
	if d <= 0 {
		d = 30 * time.Minute
	}
	if d > time.Hour {
		d = time.Hour
	}
	return d
}

// camAvatarScanSem bounds concurrently RUNNING scans across the worker. Sized
// once by startCameraAvatarScanWorker; stays nil when the worker is off.
var camAvatarScanSem chan struct{}

// startCameraAvatarScanWorker is the runServe hook for the durable avatar-scan
// queue (mirrors startCameraInvestigationWorker: startup requeue, sized
// semaphore, jittered ticker driving camAvatarScanTick).
func startCameraAvatarScanWorker(cfg config) {
	if !cfg.CameraAvatarScanWorkerEnabled {
		camlog("info", "avatar_scan_worker", map[string]any{"enabled": false})
		return
	}
	// Crash recovery: a "running" row is orphaned (its goroutine died with the
	// process) — requeue for retry. runAvatarScan is resumable (progress skips
	// cameras_done; the unique candidate index dedupes), so at most the single
	// in-flight camera is rescanned. Done once, before the ticker starts.
	if db, err := openProxyDB(cfg); err == nil {
		if n, rerr := requeueRunningCameraAvatarScans(db); rerr != nil {
			camlog("warn", "avatar_scan_recover", map[string]any{"ok": false, "error": rerr.Error()})
		} else if n > 0 {
			camlog("info", "avatar_scan_recover", map[string]any{"requeued": n})
		}
		db.Close()
	}

	interval := cfg.CameraAvatarScanTickInterval
	if interval <= 0 {
		interval = 5 * time.Second
	}
	conc := cfg.CameraAvatarScanConcurrency
	if conc < 1 {
		conc = 1
	}
	camAvatarScanSem = make(chan struct{}, conc)
	camlog("info", "avatar_scan_worker_start", map[string]any{"tick_ms": interval.Milliseconds(), "concurrency": conc})

	go func() {
		time.Sleep(camStartupJitter(interval))
		camAvatarScanTick(cfg)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for range ticker.C {
			camAvatarScanTick(cfg)
		}
	}()
}

// camAvatarScanTick claims and dispatches queued scans (no-ops while the proxy
// is globally paused); also runs the stale-running reaper and opportunistically
// prunes candidate rows past the retention window.
func camAvatarScanTick(cfg config) {
	if !proxyEnabled.Load() {
		return
	}
	db, err := openProxyDB(cfg)
	if err != nil {
		camlog("error", "avatar_scan_tick", map[string]any{"ok": false, "error": err.Error()})
		return
	}
	defer db.Close()

	// Runtime reaper: requeue "running" rows stranded by a crashed run or a lost
	// terminalization write (idle longer than a whole run could take).
	if n, rerr := requeueStaleRunningCameraAvatarScans(db, camAvatarScanBudget(cfg)+2*time.Minute); rerr != nil {
		camlog("warn", "avatar_scan_reap", map[string]any{"ok": false, "error": rerr.Error()})
	} else if n > 0 {
		camlog("info", "avatar_scan_reap", map[string]any{"requeued_stale_running": n})
	}

	// Opportunistic retention pruning: candidate MEDIA expires via the normal
	// capture reaper; this removes the now-unreviewable rows themselves.
	if ret := cfg.CameraAvatarCandidateRetention; ret > 0 {
		cutoff := time.Now().Add(-ret).Format(time.RFC3339)
		if n, derr := deleteExpiredAvatarCandidates(db, cutoff); derr != nil {
			camlog("warn", "avatar_scan_prune", map[string]any{"ok": false, "error": derr.Error()})
		} else if n > 0 {
			camlog("info", "avatar_scan_prune", map[string]any{"deleted": n})
		}
	}

	queued, err := listQueuedCameraAvatarScans(db)
	if err != nil {
		camlog("error", "avatar_scan_tick", map[string]any{"ok": false, "error": err.Error()})
		return
	}
	for _, sc := range queued {
		camConsiderAvatarScan(cfg, db, sc)
	}
}

// camConsiderAvatarScan acquires a concurrency slot, CAS-claims the row, and
// spawns the bounded DETACHED run with recover() + terminalize-on-panic (the
// camConsiderInvestigation shape).
func camConsiderAvatarScan(cfg config, db *sql.DB, sc camAvatarScan) {
	select {
	case camAvatarScanSem <- struct{}{}:
	default:
		camlog("debug", "avatar_scan_busy", map[string]any{"scan_id": sc.ID})
		return // reconsidered next tick; row stays "queued"
	}
	claimed, cerr := claimCameraAvatarScan(db, sc.ID)
	if cerr != nil {
		<-camAvatarScanSem
		camlog("warn", "avatar_scan_claim", map[string]any{"scan_id": sc.ID, "ok": false, "error": cerr.Error()})
		return
	}
	if !claimed {
		<-camAvatarScanSem // another tick/instance won the claim — not an error
		return
	}
	go func() {
		defer func() { <-camAvatarScanSem }()
		// recover() so a panic anywhere in the run (image decode / model-output
		// parsing) fails just THIS scan instead of crashing the shared proxy — and
		// terminalize the row so a deterministic panic can't become a restart
		// crash-loop poison pill.
		defer func() {
			if rec := recover(); rec != nil {
				camlog("error", "avatar_scan_panic", map[string]any{"scan_id": sc.ID, "panic": fmt.Sprint(rec)})
				camTerminalizeAvatarScan(cfg, sc.ID, "scan stopped: an internal error occurred")
			}
		}()
		// Detached context (NOT an HTTP request); runAvatarScan applies the
		// wall-clock budget internally.
		if rerr := runAvatarScan(context.Background(), cfg, sc.ID); rerr != nil {
			// Non-nil == a setup failure that left the row "running" with no progress;
			// mark it terminal so the poll shows a final state (in-run failures already
			// self-terminalize; if this write is lost, the runtime reaper picks the row
			// back up).
			camlog("error", "avatar_scan", map[string]any{"scan_id": sc.ID, "ok": false, "error": rerr.Error()})
			camTerminalizeAvatarScan(cfg, sc.ID, rerr.Error())
		}
	}()
}

// camTerminalizeAvatarScan best-effort marks a scan errored (the worker's
// error/panic paths). Best-effort by design: if the write fails, the runtime
// reaper eventually requeues the still-"running" row.
func camTerminalizeAvatarScan(cfg config, id, msg string) {
	if db, err := openProxyDB(cfg); err == nil {
		_ = setCameraAvatarScanStatus(db, id, "error", msg)
		db.Close()
	}
}

// ─────────────────────────── the run ───────────────────────────

// avScanFrame is one archived instant that survived fetch (+ motion filtering).
type avScanFrame struct {
	t    time.Time
	data []byte // sub-quality frame bytes
}

// avScanSighting is one matched instant (face or VLM path) awaiting clustering
// and candidate persistence.
type avScanSighting struct {
	T       time.Time
	Conf    float64
	Frame   []byte // the FULL frame the match was judged on
	Quality string // "main" (face path) | "sub" (VLM path)
	BBox    camBBox
	HasBBox bool
	Kind    string // face | vlm
	Reason  string
}

// avScanRun carries one claimed scan's shared state across its per-camera passes.
type avScanRun struct {
	cfg       config
	db        *sql.DB
	sc        camAvatarScan
	av        camAvatar
	alias     string
	scratch   string
	from, to  time.Time
	refPaths  []string    // scratch copies of the newest ≤ maxRefs reference images (VLM attachments)
	refLines  []string    // "ref 1 — winter coat" attachment-order lines for the user text
	refEmbeds [][]float32 // every embedded reference (+ the cached centroid) for the face path
	faceMode  bool        // human avatar + sidecar up + ≥1 embedded ref
	prog      *avScanProgress
	maxFrames int
	batch     int
	maxVLM    int
	maxCand   int
	faceMin   float64
	retention time.Duration
	persisted map[string]bool // cam.ID|frameTS already minted — keeps incremental + final persistence idempotent
}

func (r *avScanRun) saveProgress() {
	if err := setCameraAvatarScanProgress(r.db, r.sc.ID, mustJSON(r.prog)); err != nil {
		camlog("warn", "avatar_scan", map[string]any{"scan_id": r.sc.ID, "progress_ok": false, "error": err.Error()})
	}
}

// finish terminalizes the run as "done", folding an early-stop reason into the
// progress note (budget exhaustion is a graceful completion, not an error).
func (r *avScanRun) finish(note string, start time.Time) error {
	r.prog.CurrentCamera = ""
	if note != "" {
		if r.prog.Note != "" {
			r.prog.Note += "; "
		}
		r.prog.Note += "stopped early: " + note
	}
	r.saveProgress()
	if err := setCameraAvatarScanStatus(r.db, r.sc.ID, "done", ""); err != nil {
		return err
	}
	camlog("info", "avatar_scan", map[string]any{
		"scan_id": r.sc.ID, "status": "done", "note": r.prog.Note,
		"cameras_done": len(r.prog.CamerasDone), "cameras_total": r.prog.CamerasTotal,
		"frames_scanned": r.prog.FramesScanned, "vlm_calls": r.prog.VLMCalls,
		"candidates": r.prog.Candidates, "latency_ms": time.Since(start).Milliseconds(),
	})
	return nil
}

// runAvatarScan executes one claimed scan to completion (see the file header
// for the pipeline). A non-nil return is reserved for setup failures that made
// no progress; in-run failures/budget exhaustion terminalize the row themselves.
func runAvatarScan(ctx context.Context, cfg config, scanID string) error {
	start := time.Now()
	db, err := openProxyDB(cfg)
	if err != nil {
		return fmt.Errorf("open proxy db: %w", err)
	}
	defer db.Close()

	sc, err := getCameraAvatarScan(db, scanID)
	if err != nil {
		return fmt.Errorf("load scan: %w", err)
	}
	if sc.Status != "running" {
		return nil // only a worker-claimed (running) row executes — the queue owns queued→running
	}
	av, err := getCameraAvatar(db, sc.AvatarID)
	if err != nil {
		return fmt.Errorf("load avatar: %w", err)
	}
	if !camS3Enabled(cfg) {
		return errors.New("the frame archive (S3) is not configured — avatar scans walk archived frames")
	}
	from, ferr := time.Parse(time.RFC3339, strings.TrimSpace(sc.FromTS))
	if ferr != nil {
		return fmt.Errorf("invalid from_ts: %w", ferr)
	}
	to, terr := time.Parse(time.RFC3339, strings.TrimSpace(sc.ToTS))
	if terr != nil {
		return fmt.Errorf("invalid to_ts: %w", terr)
	}
	if now := time.Now(); to.After(now) {
		to = now // the archive cannot contain the future
	}
	if !from.Before(to) {
		return errors.New(`scan window: "from" must be before "to"`)
	}

	// Camera scope: the scan's explicit camera_ids, else every enabled camera on
	// the avatar's DVRs (av.DVRIDs blank/"[]" = all DVRs in the site) — the
	// camAvatarsForCameras scoping inverted.
	cams, _ := listCamerasBySite(db, sc.SiteID)
	dvrs, _ := listCameraDVRs(db, cfg, sc.SiteID)
	dvrByID := make(map[string]CamDVR, len(dvrs))
	for _, d := range dvrs {
		dvrByID[d.ID] = d
	}
	enabledByID := make(map[string]camera, len(cams))
	var enabled []camera
	for _, c := range cams {
		if !c.Enabled {
			continue
		}
		if dv, ok := dvrByID[c.DVRID]; !ok || !dv.Enabled {
			continue
		}
		enabledByID[c.ID] = c
		enabled = append(enabled, c)
	}
	var reqIDs []string
	if s := strings.TrimSpace(sc.CameraIDs); s != "" && s != "[]" {
		_ = json.Unmarshal([]byte(s), &reqIDs)
	}
	var scanCams []camera
	if len(reqIDs) > 0 {
		for _, id := range reqIDs {
			if c, ok := enabledByID[id]; ok {
				scanCams = append(scanCams, c)
			}
		}
	} else {
		dvrScope := map[string]bool{}
		allDVRs := true
		if s := strings.TrimSpace(av.DVRIDs); s != "" && s != "[]" {
			var ids []string
			if json.Unmarshal([]byte(s), &ids) == nil && len(ids) > 0 {
				allDVRs = false
				for _, id := range ids {
					dvrScope[id] = true
				}
			}
		}
		for _, c := range enabled {
			if allDVRs || dvrScope[c.DVRID] {
				scanCams = append(scanCams, c)
			}
		}
	}
	if len(scanCams) == 0 {
		return errors.New("no enabled cameras in scope for this scan")
	}

	scratch, serr := os.MkdirTemp("", "camavscan-")
	if serr != nil {
		return fmt.Errorf("scratch dir: %w", serr)
	}
	defer os.RemoveAll(scratch)

	// Reference material: newest ≤ maxRefs image files for the VLM path (copied
	// to scratch as ref_N so analyzeWithAlias labels them "ref_N"), and EVERY
	// embedded reference (+ the cached centroid) for the face path.
	refs, _ := listAvatarMedia(db, av.ID)
	// Diversity-ordered, not newest-first: a small attachment cap should cover
	// distinct cameras and spread across time (camOrderRefsForAttachment) —
	// embeddings below still come from EVERY row regardless of this order.
	refs = camOrderRefsForAttachment(refs)
	maxRefs := cfg.CameraAvatarMaxRefs
	if maxRefs <= 0 {
		maxRefs = 3
	}
	var refPaths, refLines []string
	var refEmbeds [][]float32
	hasRefEmbed := false
	for _, m := range refs {
		if v := camEmbeddingFromBlob(m.Embedding); v != nil {
			refEmbeds = append(refEmbeds, v)
			hasRefEmbed = true
		}
		if len(refPaths) >= maxRefs {
			continue
		}
		capRow, cerr := getCameraCaptureByToken(db, m.Token)
		if cerr != nil || capRow.Path == "" {
			continue
		}
		ext := filepath.Ext(capRow.Path)
		if ext == "" {
			ext = ".jpg"
		}
		dst := filepath.Join(scratch, fmt.Sprintf("ref_%d%s", len(refPaths)+1, ext))
		if copyFile(capRow.Path, dst, 0o600) != nil {
			continue
		}
		refPaths = append(refPaths, dst)
		line := fmt.Sprintf("ref %d", len(refPaths))
		if n := strings.TrimSpace(m.Note); n != "" {
			line += " — " + n
		}
		refLines = append(refLines, line)
	}
	if v := camEmbeddingFromBlob(av.Embedding); v != nil {
		refEmbeds = append(refEmbeds, v)
	}
	faceMode := av.Type == "human" && camFaceEnabled(cfg) && hasRefEmbed

	// Resume state: cameras already finished by a pre-crash pass are skipped;
	// counters continue from where they stopped.
	prog := &avScanProgress{}
	if s := strings.TrimSpace(sc.Progress); s != "" {
		_ = json.Unmarshal([]byte(s), prog)
	}
	prog.CamerasTotal = len(scanCams)
	doneSet := make(map[string]bool, len(prog.CamerasDone))
	for _, id := range prog.CamerasDone {
		doneSet[id] = true
	}

	maxFrames := cfg.CameraAvatarScanMaxFrames
	if maxFrames <= 0 {
		maxFrames = 600
	}
	batch := cfg.CameraAvatarScanBatch
	if batch <= 0 {
		batch = 8
	}
	faceMin := cfg.FaceMatchThreshold
	if faceMin <= 0 {
		faceMin = 0.40
	}
	retention := cfg.CameraAvatarCandidateRetention
	if retention <= 0 {
		retention = 168 * time.Hour
	}
	alias := firstNonEmpty(strings.TrimSpace(sc.Alias), strings.TrimSpace(cfg.CameraAvatarScanAlias), strings.TrimSpace(cfg.CameraInvestigateAlias))

	cctx, cancel := context.WithTimeout(ctx, camAvatarScanBudget(cfg))
	defer cancel()

	r := &avScanRun{
		cfg: cfg, db: db, sc: sc, av: av, alias: alias, scratch: scratch,
		from: from, to: to, refPaths: refPaths, refLines: refLines,
		refEmbeds: refEmbeds, faceMode: faceMode, prog: prog,
		maxFrames: maxFrames, batch: batch,
		maxVLM: cfg.CameraAvatarScanMaxVLM, maxCand: cfg.CameraAvatarScanMaxCandidates,
		faceMin: faceMin, retention: retention,
	}

	camlog("info", "avatar_scan", map[string]any{
		"scan_id": scanID, "avatar_id": av.ID, "site_id": sc.SiteID, "alias": alias,
		"cameras": len(scanCams), "window": windowLabel(from, to), "face_mode": faceMode,
		"refs": len(refPaths), "resumed_cameras_done": len(prog.CamerasDone),
	})

	for _, cam := range scanCams {
		if doneSet[cam.ID] {
			continue
		}
		if cctx.Err() != nil {
			return r.finish("wall-clock budget exhausted", start)
		}
		if r.maxVLM > 0 && prog.VLMCalls >= r.maxVLM {
			return r.finish("VLM call budget exhausted", start)
		}
		if r.maxCand > 0 && prog.Candidates >= r.maxCand {
			return r.finish("candidate budget exhausted", start)
		}
		prog.CurrentCamera = cam.ID
		r.saveProgress()

		loc := time.Local
		if dv, ok := dvrByID[cam.DVRID]; ok && dv.Timezone != "" {
			if l, e := time.LoadLocation(dv.Timezone); e == nil {
				loc = l
			}
		}
		stop, fatal := r.scanCamera(cctx, cam, loc)
		if fatal != nil {
			prog.CurrentCamera = ""
			r.saveProgress()
			_ = setCameraAvatarScanStatus(db, scanID, "error", fatal.Error())
			camlog("error", "avatar_scan", map[string]any{"scan_id": scanID, "camera_id": cam.ID, "ok": false, "error": fatal.Error()})
			return nil // self-terminalized: the run DID make progress
		}
		if stop != "" {
			// Partially scanned camera — deliberately NOT added to cameras_done.
			return r.finish(stop, start)
		}
		prog.CamerasDone = append(prog.CamerasDone, cam.ID)
		prog.CurrentCamera = ""
		r.saveProgress()
		camlog("info", "avatar_scan", map[string]any{
			"scan_id": scanID, "camera_id": cam.ID, "cameras_done": len(prog.CamerasDone),
			"frames_scanned": prog.FramesScanned, "vlm_calls": prog.VLMCalls, "candidates": prog.Candidates,
		})
	}
	return r.finish("", start)
}

// scanCamera runs the full per-camera pipeline: coarsened instants → parallel
// sub-quality S3 fetch → motion prefilter → face or VLM matching → clustering →
// candidate persistence. Returns a non-empty stop reason on budget exhaustion
// (graceful "done") or a fatal error when the analysis backend keeps failing.
func (r *avScanRun) scanCamera(ctx context.Context, cam camera, loc *time.Location) (string, error) {
	instants := avScanInstants(r.from, r.to, r.maxFrames)

	type scanSlot struct {
		t    time.Time
		sig  []uint8
		data []byte
		ok   bool
	}
	slots := make([]scanSlot, len(instants))
	sem := make(chan struct{}, avScanFetchConc)
	var wg sync.WaitGroup
	var errMu sync.Mutex
	realErrs := 0 // genuine S3 failures (timeout/5xx/auth), NOT plain 404 misses
	for i, t := range instants {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, t time.Time) {
			defer wg.Done()
			defer func() { <-sem }()
			data, err := s3GetObject(ctx, r.cfg, camS3FrameKey(cam.ID, "sub", t))
			if err != nil {
				if !errors.Is(err, errS3NotFound) {
					errMu.Lock()
					realErrs++
					errMu.Unlock()
				}
				return // archive miss — skipped
			}
			sig, serr := camMotionSignature(data)
			if serr != nil {
				return
			}
			slots[i] = scanSlot{t: t, sig: sig, data: data, ok: true}
		}(i, t)
	}
	wg.Wait()

	// Motion prefilter: keep a frame only when it differs enough from the
	// previous KEPT frame (first fetched frame is the baseline, kept).
	var survivors []avScanFrame
	var prevSig []uint8
	fetched := 0
	for i := range slots {
		if !slots[i].ok {
			continue
		}
		fetched++
		if prevSig == nil || camMotionScore(slots[i].sig, prevSig) > avScanMotionMin {
			survivors = append(survivors, avScanFrame{t: slots[i].t, data: slots[i].data})
			prevSig = slots[i].sig
		}
	}
	r.prog.FramesScanned += fetched
	camlog("info", "avatar_scan", map[string]any{
		"scan_id": r.sc.ID, "camera_id": cam.ID, "instants": len(instants),
		"fetched": fetched, "active": len(survivors), "real_errs": realErrs, "path": map[bool]string{true: "face", false: "vlm"}[r.faceMode],
	})
	if len(survivors) == 0 {
		// Distinguish an empty window (all genuine 404 misses → legitimately "done"
		// with zero candidates) from an S3 outage (real failures → do NOT mark the
		// camera done and terminalize the whole scan; a false "done" would silently
		// swallow the window and never retry).
		if fetched == 0 && realErrs > 0 {
			return "", fmt.Errorf("archive unreadable for camera %s (%d S3 errors) — not marking scanned", cam.ID, realErrs)
		}
		return "", nil
	}

	var sightings []avScanSighting
	var stop string
	var fatal stopErr
	if r.faceMode {
		var faceDown bool
		sightings, faceDown, stop = r.faceScan(ctx, cam, survivors)
		if faceDown && stop == "" {
			// Sidecar unavailable mid-run — fall back to appearance matching and
			// record it so the operator understands the match_kind mix.
			if r.prog.Note == "" {
				r.prog.Note = "face engine unavailable — used appearance matching"
			}
			camlog("warn", "avatar_scan", map[string]any{"scan_id": r.sc.ID, "camera_id": cam.ID, "note": "face engine unavailable — used appearance matching"})
			// Persist the face sightings gathered before the sidecar died; the VLM
			// fallback then persists its own matches incrementally.
			r.persistSightings(cam, sightings, &stop)
			_, stop = r.vlmScan(ctx, cam, survivors, loc, &fatal)
			return stop, fatal.err
		}
		// Persist whatever was matched even when the backend died mid-camera.
		r.persistSightings(cam, sightings, &stop)
		return stop, fatal.err
	}
	// VLM path: candidates are persisted incrementally (per batch) inside vlmScan
	// so they surface in the UI as they're found — a slow analysis backend no
	// longer means a blank screen for minutes — and partial evidence survives a
	// mid-camera backend failure.
	_, stop = r.vlmScan(ctx, cam, survivors, loc, &fatal)
	return stop, fatal.err
}

// stopErr carries a fatal analysis-backend error out of vlmScan without
// entangling it with the graceful budget-stop return.
type stopErr struct{ err error }

// persistSightings clusters a camera's sightings (<20s apart chain into one
// cluster, best confidence wins) and mints one candidate per keeper, honoring
// the candidate budget.
func (r *avScanRun) persistSightings(cam camera, sightings []avScanSighting, stop *string) {
	if len(sightings) == 0 {
		return
	}
	if r.persisted == nil {
		r.persisted = map[string]bool{}
	}
	keys := make([]avSightingKey, len(sightings))
	for i, s := range sightings {
		keys[i] = avSightingKey{T: s.T, Conf: s.Conf}
	}
	for _, idx := range avScanClusterSightings(keys, avScanClusterGap) {
		// Idempotency across the incremental per-batch flushes and the final
		// per-camera pass: a keeper already minted (keyed by frame instant) is
		// never re-persisted, so we neither orphan capture blobs nor double-count.
		pk := cam.ID + "|" + sightings[idx].T.UTC().Format(time.RFC3339)
		if r.persisted[pk] {
			continue
		}
		if r.maxCand > 0 && r.prog.Candidates >= r.maxCand {
			if *stop == "" {
				*stop = "candidate budget exhausted"
			}
			return
		}
		inserted, perr := r.persistCandidate(cam, sightings[idx])
		if perr != nil {
			camlog("warn", "avatar_scan", map[string]any{"scan_id": r.sc.ID, "camera_id": cam.ID, "candidate_ok": false, "error": perr.Error()})
			continue
		}
		r.persisted[pk] = true
		if inserted {
			r.prog.Candidates++
			r.saveProgress() // surface each new candidate to the live UI immediately
		}
	}
}

// faceScan is the primary path for humans with embedded references: for each
// motion-active instant it pulls the MAIN-quality frame (faces need pixels),
// detects+embeds faces via the sidecar, and keeps the best cosine ≥ threshold.
// faceDown reports the sidecar failing before ANY frame was processed (the
// caller then falls back to the VLM path for this camera).
func (r *avScanRun) faceScan(ctx context.Context, cam camera, survivors []avScanFrame) (sightings []avScanSighting, faceDown bool, stop string) {
	errs, processed := 0, 0
	for _, f := range survivors {
		if ctx.Err() != nil {
			stop = "wall-clock budget exhausted"
			return
		}
		// Prefer the higher-res MAIN frame at this exact second (the streaming
		// archiver keys main on the 1-fps grid); fall back to the sub bytes we
		// already fetched so a missing keyframe never silently drops the instant.
		data := f.data
		quality := "sub"
		if b, gerr := s3GetObject(ctx, r.cfg, camS3FrameKey(cam.ID, "main", f.t)); gerr == nil {
			data, quality = b, "main"
		}
		icfg, _, derr := image.DecodeConfig(bytes.NewReader(data))
		if derr != nil {
			continue
		}
		faces, ferr := camFaceDetect(ctx, r.cfg, data)
		if ferr != nil {
			errs++
			if processed == 0 && errs >= 3 {
				faceDown = true // dead sidecar — stop hammering it
				return
			}
			continue
		}
		processed++
		best := -1.0
		var bestFace camFace
		for _, fc := range faces {
			for _, ref := range r.refEmbeds {
				if c := camCosine(fc.Embedding, ref); c > best {
					best, bestFace = c, fc
				}
			}
		}
		if best >= r.faceMin {
			sightings = append(sightings, avScanSighting{
				T: f.t, Conf: best, Frame: data, Quality: quality,
				BBox: camBBoxFromPixels(bestFace.BBox, icfg.Width, icfg.Height), HasBBox: true,
				Kind: "face", Reason: fmt.Sprintf("face similarity %.2f", best),
			})
		}
	}
	if processed == 0 && errs > 0 {
		faceDown = true
	}
	return
}

// expandFaceToPerson grows a detected FACE box into an approximate head-and-upper-
// body region (CCTV subjects are usually seated or waist-occluded, so we stay to
// head+torso rather than guess full-body), clamped to the frame. It turns an
// accurate face detection into a sensible person crop for the review artifacts.
func expandFaceToPerson(face image.Rectangle, w, h int) image.Rectangle {
	face = face.Canon()
	fw, fh := face.Dx(), face.Dy()
	if fw <= 0 || fh <= 0 {
		return face.Intersect(image.Rect(0, 0, w, h))
	}
	cx := (face.Min.X + face.Max.X) / 2
	r := image.Rect(
		cx-(fw*8)/5,         // ~1.6× face width to each side (shoulders)
		face.Min.Y-(fh*3)/5, // a little headroom above the crown
		cx+(fw*8)/5,
		face.Max.Y+(fh*9)/2, // ~4.5 face-heights of torso below the chin
	).Intersect(image.Rect(0, 0, w, h))
	if r.Empty() {
		return face.Intersect(image.Rect(0, 0, w, h))
	}
	return r
}

// detectPersonBox localizes a VLM appearance match on a REAL detected face rather
// than the model's (frequently hallucinated) bbox — the VLM is unreliable at pixel
// coordinates and routinely boxes furniture. It runs the face sidecar and returns
// a head+upper-body box (in camBBox 0..1000 space) around the best face: the one
// nearest the VLM's bbox hint when several are present, else the highest-scoring.
// found=false means the sidecar answered but saw no face; a non-nil error means the
// sidecar itself was unavailable (callers fall back to the VLM bbox so a dead
// sidecar never silently drops matches).
func (r *avScanRun) detectPersonBox(ctx context.Context, frame []byte, hint *camBBox) (camBBox, bool, error) {
	if !camFaceEnabled(r.cfg) {
		return camBBox{}, false, errors.New("face sidecar disabled")
	}
	icfg, _, derr := image.DecodeConfig(bytes.NewReader(frame))
	if derr != nil {
		return camBBox{}, false, derr
	}
	faces, ferr := camFaceDetect(ctx, r.cfg, frame)
	if ferr != nil {
		return camBBox{}, false, ferr
	}
	if len(faces) == 0 {
		return camBBox{}, false, nil
	}
	best := faces[0]
	if hint != nil {
		// hint is 0..1000-normalized; project its centre to pixels and pick the
		// nearest detected face — the VLM is roughly right about the region even
		// when its exact box is wrong.
		hx := (hint.X0 + hint.X1) * icfg.Width / 2000
		hy := (hint.Y0 + hint.Y1) * icfg.Height / 2000
		bestD := int64(-1)
		for _, f := range faces {
			cx := (f.BBox.Min.X + f.BBox.Max.X) / 2
			cy := (f.BBox.Min.Y + f.BBox.Max.Y) / 2
			dx, dy := int64(cx-hx), int64(cy-hy)
			if d := dx*dx + dy*dy; bestD < 0 || d < bestD {
				bestD, best = d, f
			}
		}
	} else {
		for _, f := range faces {
			if f.Score > best.Score {
				best = f
			}
		}
	}
	return camBBoxFromPixels(expandFaceToPerson(best.BBox, icfg.Width, icfg.Height), icfg.Width, icfg.Height), true, nil
}

// vlmScan is the appearance-matching path: motion survivors batched
// cfg.CameraAvatarScanBatch per analyzeWithAlias call, each call carrying the
// newest reference images plus the batch frames and a frame→time legend.
// fatal.err is set after 3 consecutive backend failures.
func (r *avScanRun) vlmScan(ctx context.Context, cam camera, survivors []avScanFrame, loc *time.Location, fatal *stopErr) (sightings []avScanSighting, stop string) {
	w, h := 0, 0
	if icfg, _, err := image.DecodeConfig(bytes.NewReader(survivors[0].data)); err == nil {
		w, h = icfg.Width, icfg.Height
	}
	sys := camAvatarScanSystemPrompt(r.av, len(r.refPaths) > 0, w, h)
	header := fmt.Sprintf("TARGET: %s (%s). DESCRIPTION: %s", r.av.Name, avScanType(r.av), strings.TrimSpace(r.av.Description))
	// Human avatars get their box from real face detection (accurate); non-human
	// avatars (vehicle/pet — no face) keep the VLM's proposed box.
	faceLoc := r.av.Type == "human" && camFaceEnabled(r.cfg)

	consec := 0
	for bstart := 0; bstart < len(survivors); bstart += r.batch {
		if ctx.Err() != nil {
			stop = "wall-clock budget exhausted"
			return
		}
		if r.maxVLM > 0 && r.prog.VLMCalls >= r.maxVLM {
			stop = "VLM call budget exhausted"
			return
		}
		bend := bstart + r.batch
		if bend > len(survivors) {
			bend = len(survivors)
		}
		frames := survivors[bstart:bend]

		paths := make([]string, 0, len(r.refPaths)+len(frames))
		paths = append(paths, r.refPaths...)
		lines := make([]string, 0, len(r.refLines)+len(frames))
		lines = append(lines, r.refLines...)
		writeFail := false
		for i, f := range frames {
			p := filepath.Join(r.scratch, fmt.Sprintf("frame_%02d.jpg", i+1))
			if os.WriteFile(p, f.data, 0o600) != nil {
				writeFail = true
				break
			}
			paths = append(paths, p)
			lines = append(lines, fmt.Sprintf("frame %d = %s", i+1, f.t.In(loc).Format(time.RFC3339)))
		}
		if writeFail {
			continue
		}
		user := header + "\nATTACHMENTS IN ORDER:\n" + strings.Join(lines, "\n")

		out, aerr := analyzeWithAlias(ctx, r.cfg, r.alias, sys, user, paths)
		r.prog.VLMCalls++
		r.saveProgress() // heartbeat + live UI counters
		if aerr != nil {
			consec++
			camlog("warn", "avatar_scan", map[string]any{"scan_id": r.sc.ID, "camera_id": cam.ID, "vlm_ok": false, "error": aerr.Error()})
			if consec >= 3 {
				fatal.err = fmt.Errorf("analysis backend failing (%d consecutive errors): %w", consec, aerr)
				return
			}
			continue
		}
		consec = 0
		matches, perr := parseAvatarScanMatches(out)
		if perr != nil {
			camlog("warn", "avatar_scan", map[string]any{"scan_id": r.sc.ID, "camera_id": cam.ID, "parse_ok": false, "error": perr.Error()})
			continue
		}
		var batch []avScanSighting
		for _, m := range matches {
			fi := int(m.Frame)
			if fi < 1 || fi > len(frames) || m.Confidence < avScanVLMConfMin {
				continue
			}
			// Prefer the higher-res MAIN frame at this instant for face detection
			// and the review artifacts; fall back to the sub bytes already in hand.
			frameData, frameQ := frames[fi-1].data, "sub"
			if b, gerr := s3GetObject(ctx, r.cfg, camS3FrameKey(cam.ID, "main", frames[fi-1].t)); gerr == nil {
				frameData, frameQ = b, "main"
			}
			s := avScanSighting{
				T: frames[fi-1].t, Conf: m.Confidence, Frame: frameData,
				Quality: frameQ, Kind: "vlm", Reason: strings.TrimSpace(m.Reason),
			}
			var hint *camBBox
			if bb, ok := camParseBBox(m.BBox); ok {
				hint = &bb
			}
			if faceLoc {
				// Localize on a REAL detected face when one exists — the VLM box is
				// then only a tie-break hint among faces. But when the sidecar finds
				// NO face (target far away, back turned), the VLM's own box is still
				// the only thing that tells the reviewer WHICH person was matched —
				// an imprecise box on the right person beats an unmarked frame with
				// several people in it, so the hint is used rather than discarded.
				if box, found, detErr := r.detectPersonBox(ctx, frameData, hint); detErr == nil && found {
					s.BBox, s.HasBBox = box, true
				} else if hint != nil {
					s.BBox, s.HasBBox = *hint, true
				}
			} else if hint != nil {
				s.BBox, s.HasBBox = *hint, true
			}
			batch = append(batch, s)
			sightings = append(sightings, s)
		}
		// Flush this batch's matches immediately so candidates surface in the UI as
		// they're found rather than only when the whole (possibly slow) camera ends.
		r.persistSightings(cam, batch, &stop)
		if stop != "" {
			return
		}
	}
	return
}

// persistCandidate mints the review artifacts for one kept sighting — the
// circled FULL frame + the person crop, both expiring at
// now+CameraAvatarCandidateRetention — and INSERT OR IGNOREs the candidate row.
// Returns whether a NEW row was inserted (false = resume duplicate).
func (r *avScanRun) persistCandidate(cam camera, s avScanSighting) (bool, error) {
	var boxes []camBBox
	bboxJSON := ""
	cropBox := camBBox{X0: 0, Y0: 0, X1: 1000, Y1: 1000}
	if s.HasBBox {
		boxes = []camBBox{s.BBox}
		bboxJSON = fmt.Sprintf(`{"x0":%d,"y0":%d,"x1":%d,"y1":%d}`, s.BBox.X0, s.BBox.Y0, s.BBox.X1, s.BBox.Y1)
		cropBox = s.BBox
	}
	annJPG, w, h, err := camAnnotateJPEG(s.Frame, boxes)
	if err != nil {
		return false, fmt.Errorf("annotate: %w", err)
	}
	cropJPG, cw, ch, err := camCropJPEG(s.Frame, cropBox, 15)
	if err != nil {
		return false, fmt.Errorf("crop: %w", err)
	}
	annPath := filepath.Join(r.scratch, fmt.Sprintf("cand_ann_%s_%d.jpg", cam.ID, s.T.Unix()))
	cropPath := filepath.Join(r.scratch, fmt.Sprintf("cand_crop_%s_%d.jpg", cam.ID, s.T.Unix()))
	if err := os.WriteFile(annPath, annJPG, 0o600); err != nil {
		return false, err
	}
	if err := os.WriteFile(cropPath, cropJPG, 0o600); err != nil {
		return false, err
	}
	frameTS := s.T.UTC().Format(time.RFC3339)
	expires := time.Now().Add(r.retention).Format(time.RFC3339)
	annToken, err := camPersistCaptureExpires(r.db, r.cfg, "", r.sc.SiteID, cam.ID, "avatar_candidate", s.Quality,
		annPath, "image/jpeg", w, h, frameTS, "", expires)
	if err != nil {
		return false, fmt.Errorf("persist annotated: %w", err)
	}
	cropToken, err := camPersistCaptureExpires(r.db, r.cfg, "", r.sc.SiteID, cam.ID, "avatar_candidate", s.Quality,
		cropPath, "image/jpeg", cw, ch, frameTS, "", expires)
	if err != nil {
		return false, fmt.Errorf("persist crop: %w", err)
	}
	id, err := insertCameraAvatarCandidate(r.db, camAvatarCandidate{
		ScanID: r.sc.ID, AvatarID: r.sc.AvatarID, CameraID: cam.ID,
		FrameTS: frameTS, Quality: s.Quality,
		AnnotatedToken: annToken, CropToken: cropToken, BBox: bboxJSON,
		MatchKind: s.Kind, VLMConfidence: s.Conf, VLMReason: s.Reason, Status: "pending",
	})
	if err != nil {
		return false, fmt.Errorf("insert candidate: %w", err)
	}
	return id != "", nil
}

// ─────────────────────────── pure helpers ───────────────────────────

// avScanType normalizes an avatar's type for prompt text ("human" default).
func avScanType(av camAvatar) string {
	if t := strings.TrimSpace(av.Type); t != "" {
		return t
	}
	return "human"
}

// avScanInstants returns candidate instants on the archive's 1-second grid
// across [from,to], coarsened so at most maxFrames are produced — the same
// stepping as camToolContactSheet (long windows get a wider step).
func avScanInstants(from, to time.Time, maxFrames int) []time.Time {
	if maxFrames <= 0 {
		maxFrames = 600
	}
	step := time.Second
	if span := to.Sub(from); span > 0 {
		if secs := int64(span / time.Second); secs > int64(maxFrames) {
			step = time.Duration(secs/int64(maxFrames)) * time.Second
			if step < time.Second {
				step = time.Second
			}
		}
	}
	var instants []time.Time
	for t := from; !t.After(to); t = t.Add(step) {
		instants = append(instants, t.Truncate(time.Second))
		if len(instants) >= maxFrames {
			break
		}
	}
	return instants
}

// avSightingKey is the clustering view of one matched sighting.
type avSightingKey struct {
	T    time.Time
	Conf float64
}

// avScanClusterSightings groups one camera's sightings into clusters —
// consecutive sightings (in time order) less than gap apart CHAIN into the
// same cluster — and returns the input index of each cluster's
// highest-confidence member, in chronological cluster order.
func avScanClusterSightings(keys []avSightingKey, gap time.Duration) []int {
	if len(keys) == 0 {
		return nil
	}
	order := make([]int, len(keys))
	for i := range order {
		order[i] = i
	}
	sort.Slice(order, func(a, b int) bool { return keys[order[a]].T.Before(keys[order[b]].T) })
	var out []int
	best := order[0]
	prev := keys[order[0]].T
	for _, idx := range order[1:] {
		if keys[idx].T.Sub(prev) < gap {
			if keys[idx].Conf > keys[best].Conf {
				best = idx
			}
		} else {
			out = append(out, best)
			best = idx
		}
		prev = keys[idx].T
	}
	return append(out, best)
}

// ─────────────────────────── VLM prompt + parsing ───────────────────────────

// camAvatarScanSystemPrompt builds the enrollment batch-matching system prompt
// (design-avatars.md 3.1: refs + candidate frames, judge overall appearance,
// honest uncertainty, camBBoxConvention(w,h), strict {"matches":[...]} JSON;
// hasRefs=false switches to the "match on the DESCRIPTION alone, extra
// conservative" bootstrap variant).
func camAvatarScanSystemPrompt(av camAvatar, hasRefs bool, w, h int) string {
	typ := avScanType(av)
	var b strings.Builder
	b.WriteString("You are helping enroll a known " + typ + " into a security-camera appearance registry. ")
	if hasRefs {
		b.WriteString("You are given: a text DESCRIPTION of the target, approved REFERENCE image(s) of them (attached first, labeled ref 1, ref 2, …), and a batch of CANDIDATE frames from one camera (labeled frame 1, frame 2, …), each with its capture time listed below. For EACH candidate frame decide whether the target is visible in it.\n\n")
	} else {
		b.WriteString("You are given: a text DESCRIPTION of the target and a batch of CANDIDATE frames from one camera (labeled frame 1, frame 2, …), each with its capture time listed below. There are no reference images yet — match on the DESCRIPTION alone, and be extra conservative. For EACH candidate frame decide whether the target is visible in it.\n\n")
	}
	b.WriteString("Judge by OVERALL appearance — build, clothing, posture, and whether the time/place fits the description's habits — not just the face: CCTV resolution is low and faces are rarely legible. Be honest about uncertainty: a human operator reviews every proposal, so a confident wrong answer is worse than a hesitant right one. Do still propose plausible matches — finding them is the point.\n\n")
	b.WriteString(camBBoxConvention(w, h))
	b.WriteString("\n\nReply with EXACTLY ONE JSON object and nothing else (no prose, no markdown fences):\n")
	b.WriteString(`{"matches":[{"frame":3,"confidence":0.82,"bbox":{"x0":412,"y0":180,"x1":530,"y1":690},"reason":"male, white coat, entering via the staff door as described"}]}`)
	b.WriteString("\nEvery match MUST include bbox — a box around THE matched " + typ + " in that frame (whole " + typ + ", not just the face). The reviewer sees your box to know WHICH one you mean; when several people are visible, a match without a box is unusable.")
	b.WriteString("\nInclude ONLY frames where the target is plausibly present (confidence ≥ 0.4); omit all others. If none match: {\"matches\":[]}.")
	return b.String()
}

// parseAvatarScanMatches extracts the single {"matches":[...]} object from a
// (possibly prose-wrapped/fenced) VLM reply via extractFirstJSONObject and
// decodes it tolerantly: a stringified matches array, a bare single object, and
// flexInt frame numbers are all accepted.
func parseAvatarScanMatches(raw string) ([]avScanMatch, error) {
	obj, ok := extractFirstJSONObject(raw)
	if !ok {
		// Whole reply may be a JSON STRING containing the object.
		var s string
		if json.Unmarshal([]byte(strings.TrimSpace(raw)), &s) == nil {
			obj, ok = extractFirstJSONObject(s)
		}
	}
	if !ok {
		return nil, errors.New("no JSON object found in reply")
	}
	var env struct {
		Matches json.RawMessage `json:"matches"`
	}
	if err := json.Unmarshal(obj, &env); err != nil {
		return nil, fmt.Errorf("decode matches envelope: %w", err)
	}
	body := bytes.TrimSpace(env.Matches)
	if len(body) == 0 || string(body) == "null" {
		return []avScanMatch{}, nil
	}
	// Stringified array: {"matches":"[{...}]"}.
	if body[0] == '"' {
		var s string
		if err := json.Unmarshal(body, &s); err == nil {
			body = bytes.TrimSpace([]byte(s))
		}
	}
	var ms []avScanMatch
	if err := json.Unmarshal(body, &ms); err != nil {
		// A single bare object instead of an array.
		var one avScanMatch
		if err2 := json.Unmarshal(body, &one); err2 == nil {
			return []avScanMatch{one}, nil
		}
		return nil, fmt.Errorf("decode matches array: %w", err)
	}
	return ms, nil
}
