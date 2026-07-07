package main

// camera_export.go — the durable multi-camera evidence-video export queue.
// CLONED from the avatar-scan queue's shape (camera_avatar_scan.go): a
// camera_exports row is CAS-claimed queued→running, executed on a detached
// goroutine with recover(), and terminalized (done|failed); startup requeue +
// a runtime stale reaper recover crashed runs. Started from cli.go right after
// startCameraAvatarScanWorker.
//
// An export answers "give me the full evidence video from t1 to t2 across
// cameras A,B,C". The window may span up to cfg.CameraExportMaxSeconds (default
// 1h) — far past the 300s live-clip cap — so each camera's window is split into
// ≤ cfg.CameraClipMaxSeconds chunks pulled SEQUENTIALLY through the existing
// capturePlaybackClip/captureSem path (a missing recording is a disclosed GAP,
// not a failure). The chunks are then composed by ffmpeg into one of three
// layouts and uploaded to public object storage as permanent links:
//   - separate:   one MP4 per camera, lossless concat of that camera's raw
//                 chunks (no re-encode; Hik OSD is usually burned in already).
//   - sequential: every camera's chunks normalized (scale/pad/fps + a camera
//                 label & wall-clock overlay) onto a common canvas, then all
//                 concatenated back-to-back into one MP4.
//   - grid:       each camera's timeline padded to the full window with black
//                 filler for gaps, then xstack'd into a 2/4/6/9-pane wall.
// Delivery: the camera_export_ready webhook (camera_serviceapi.go) plus, when
// the export was started from an investigation, a tool note appended to that
// transcript. The scratch dir under cfg.CameraExportTmp is DURABLE (survives a
// crash-requeue so finished chunks/segments are skipped on resume) and is swept
// of >48h orphans by the tick.
//
// Every ffmpeg subprocess runs through runCamCommand (context deadline is the
// authoritative kill). Unit tests never require ffmpeg: the pure helpers (chunk
// math, grid layout, label/overlay assembly, payload shape) and the store layer
// carry the coverage.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// camExportLayouts is the set of accepted layout values (default "separate").
var camExportLayouts = map[string]bool{"sequential": true, "grid": true, "separate": true}

// camExportGridHardCap is the absolute pane ceiling for a grid layout (3x3).
const camExportGridHardCap = 9

// camExport is an in-memory view of a camera_exports row.
type camExport struct {
	ID              string
	SiteID          string
	InvestigationID string // "" for a service/admin-initiated export
	CameraIDs       string // JSON array
	FromTS          string // RFC3339
	ToTS            string // RFC3339
	Layout          string // sequential | grid | separate
	Quality         string // main | sub
	Status          string // queued | running | done | failed
	Progress        string // JSON (camExportProgress)
	Outputs         string // JSON ([]camExportOutput)
	Error           string
	CreatedAt       string
	UpdatedAt       string
}

// camExportCamProgress is one camera's download checkpoint inside the progress
// JSON: how many chunks are finished and which windows came back as gaps (so a
// crash-requeued run skips both the finished chunk files and the known gaps).
type camExportCamProgress struct {
	ChunksDone  int      `json:"chunks_done"`
	ChunksTotal int      `json:"chunks_total"`
	Gaps        []string `json:"gaps,omitempty"` // "fromISO/toISO" windows with no recording
}

// camExportProgress is the export row's progress JSON checkpoint.
type camExportProgress struct {
	Stage   string                           `json:"stage"` // download | compose | upload
	Cameras map[string]*camExportCamProgress `json:"cameras,omitempty"`
	Note    string                           `json:"note,omitempty"`
}

// camExportOutput is one produced MP4 recorded in the outputs JSON — the durable
// record of the export's deliverables (S3 link is permanent).
type camExportOutput struct {
	CameraID    string `json:"camera_id"` // "" for a combined sequential/grid output
	Label       string `json:"label"`
	Token       string `json:"token"`  // served /camera/media/<token> (retention-bound)
	S3URL       string `json:"s3_url"` // permanent public link
	Bytes       int64  `json:"bytes"`
	ContentType string `json:"content_type"`
	Caption     string `json:"caption"`
	local       string // on-disk path during the compose→upload handoff (unexported → never serialized)
}

// ─────────────────────────── export store (queue) ───────────────────────────

const camExportCols = `id, site_id, investigation_id, camera_ids, from_ts, to_ts, layout, quality, status, progress, outputs, error, created_at, updated_at`

func scanExportRow(s rowScanner) (camExport, error) {
	var v camExport
	err := s.Scan(&v.ID, &v.SiteID, &v.InvestigationID, &v.CameraIDs, &v.FromTS, &v.ToTS,
		&v.Layout, &v.Quality, &v.Status, &v.Progress, &v.Outputs, &v.Error, &v.CreatedAt, &v.UpdatedAt)
	return v, err
}

func insertCameraExport(db *sql.DB, ex camExport) (string, error) {
	if ex.ID == "" {
		ex.ID = randomID("export")
	}
	if ex.Status == "" {
		ex.Status = "queued"
	}
	if ex.Layout == "" {
		ex.Layout = "separate"
	}
	if ex.Quality == "" {
		ex.Quality = "main"
	}
	now := nowRFC3339()
	if ex.CreatedAt == "" {
		ex.CreatedAt = now
	}
	if ex.UpdatedAt == "" {
		ex.UpdatedAt = now
	}
	_, err := db.Exec(`INSERT INTO camera_exports
		(id, site_id, investigation_id, camera_ids, from_ts, to_ts, layout, quality, status, progress, outputs, error, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		ex.ID, ex.SiteID, ex.InvestigationID, ex.CameraIDs, ex.FromTS, ex.ToTS, ex.Layout, ex.Quality,
		ex.Status, ex.Progress, ex.Outputs, ex.Error, ex.CreatedAt, ex.UpdatedAt)
	if err != nil {
		return "", err
	}
	return ex.ID, nil
}

func getCameraExport(db *sql.DB, id string) (camExport, error) {
	return scanExportRow(db.QueryRow(`SELECT `+camExportCols+` FROM camera_exports WHERE id = ?`, id))
}

// listCameraExports lists a site's exports newest-first.
func listCameraExports(db *sql.DB, siteID string) ([]camExport, error) {
	rows, err := db.Query(`SELECT `+camExportCols+` FROM camera_exports WHERE site_id = ? ORDER BY created_at DESC`, siteID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []camExport
	for rows.Next() {
		v, err := scanExportRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func listQueuedCameraExports(db *sql.DB) ([]camExport, error) {
	rows, err := db.Query(`SELECT ` + camExportCols +
		` FROM camera_exports WHERE status = 'queued' ORDER BY updated_at ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []camExport
	for rows.Next() {
		v, err := scanExportRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// claimCameraExport atomically transitions queued→running (CAS) — true only when
// THIS caller won the claim, so two ticks/instances never run one export twice.
func claimCameraExport(db *sql.DB, id string) (bool, error) {
	res, err := db.Exec(`UPDATE camera_exports SET status = 'running', updated_at = ?
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

// requeueRunningCameraExports resets exports stranded in "running" back to
// "queued" (startup crash recovery). The durable scratch dir + progress
// checkpoint make a resume skip finished chunks/segments.
func requeueRunningCameraExports(db *sql.DB) (int64, error) {
	res, err := db.Exec(`UPDATE camera_exports SET status = 'queued', updated_at = ?
		WHERE status = 'running'`, nowRFC3339())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// requeueStaleRunningCameraExports is the RUNTIME reaper: requeues "running"
// rows idle longer than olderThan (orphaned goroutine / lost terminalization
// write). A healthy run bumps updated_at on every progress checkpoint and cannot
// outlive its budget, so idle > budget+margin is safe to re-run.
func requeueStaleRunningCameraExports(db *sql.DB, olderThan time.Duration) (int64, error) {
	cutoff := time.Now().Add(-olderThan).Format(time.RFC3339)
	res, err := db.Exec(`UPDATE camera_exports SET status = 'queued', updated_at = ?
		WHERE status = 'running' AND updated_at < ?`, nowRFC3339(), cutoff)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func setCameraExportStatus(db *sql.DB, id, status, errMsg string) error {
	_, err := db.Exec(`UPDATE camera_exports SET status = ?, error = ?, updated_at = ? WHERE id = ?`,
		status, errMsg, nowRFC3339(), id)
	return err
}

func setCameraExportProgress(db *sql.DB, id, progressJSON string) error {
	_, err := db.Exec(`UPDATE camera_exports SET progress = ?, updated_at = ? WHERE id = ?`,
		progressJSON, nowRFC3339(), id)
	return err
}

func setCameraExportOutputs(db *sql.DB, id, outputsJSON string) error {
	_, err := db.Exec(`UPDATE camera_exports SET outputs = ?, updated_at = ? WHERE id = ?`,
		outputsJSON, nowRFC3339(), id)
	return err
}

// camExportParseCameraIDs decodes the camera_ids JSON array (tolerant of "" / "[]").
func camExportParseCameraIDs(raw string) []string {
	var ids []string
	if s := strings.TrimSpace(raw); s != "" && s != "[]" {
		_ = json.Unmarshal([]byte(s), &ids)
	}
	return ids
}

// camExportParseOutputs decodes the outputs JSON ("" → nil).
func camExportParseOutputs(raw string) []camExportOutput {
	var out []camExportOutput
	if s := strings.TrimSpace(raw); s != "" && s != "null" {
		_ = json.Unmarshal([]byte(s), &out)
	}
	return out
}

// camExportParseProgress decodes the progress JSON ("" → zero value).
func camExportParseProgress(raw string) camExportProgress {
	var p camExportProgress
	if s := strings.TrimSpace(raw); s != "" && s != "null" {
		_ = json.Unmarshal([]byte(s), &p)
	}
	return p
}

// ─────────────────────────── pure helpers (unit-tested) ───────────────────────────

// camExportChunks splits [from,to] into consecutive windows of at most chunkSec
// seconds each. A trailing window shorter than 30s is merged back into its
// predecessor so a stub tail never produces a near-empty segment. Returns nil
// when the range is empty.
func camExportChunks(from, to time.Time, chunkSec int) [][2]time.Time {
	if !from.Before(to) {
		return nil
	}
	if chunkSec <= 0 {
		chunkSec = 300
	}
	chunk := time.Duration(chunkSec) * time.Second
	var out [][2]time.Time
	for start := from; start.Before(to); {
		end := start.Add(chunk)
		if end.After(to) {
			end = to
		}
		out = append(out, [2]time.Time{start, end})
		start = end
	}
	if len(out) >= 2 {
		last := out[len(out)-1]
		if last[1].Sub(last[0]) < 30*time.Second {
			out[len(out)-2][1] = last[1]
			out = out[:len(out)-1]
		}
	}
	return out
}

// camExportChunkKey is the deterministic "fromISO/toISO" identity of a chunk
// window, used to record and match recorded gaps across a resume.
func camExportChunkKey(from, to time.Time) string {
	return from.UTC().Format(time.RFC3339) + "/" + to.UTC().Format(time.RFC3339)
}

// camExportGridDims picks the pane grid for n cameras: 2→2x1, 3-4→2x2, 5-6→3x2,
// 7-9→3x3 (n is expected already clamped to camExportGridHardCap).
func camExportGridDims(n int) (cols, rows int) {
	switch {
	case n <= 1:
		return 1, 1
	case n == 2:
		return 2, 1
	case n <= 4:
		return 2, 2
	case n <= 6:
		return 3, 2
	default:
		return 3, 3
	}
}

// camExportGridLayout returns the pane grid and the ffmpeg xstack `layout`
// expression for n cameras. Every pane is the same tile size, so a column's x
// offset is the sum of the preceding row-0 tile widths and a row's y offset is
// the sum of the preceding column-0 tile heights (e.g. 2x2 → "0_0|w0_0|0_h0|
// w0_h0"). panes = cols*rows may exceed n; the compose stage pads the surplus
// with black timelines so the layout is always fully covered.
func camExportGridLayout(n int) (cols, rows int, layout string) {
	cols, rows = camExportGridDims(n)
	panes := cols * rows
	parts := make([]string, 0, panes)
	for i := 0; i < panes; i++ {
		r := i / cols
		c := i % cols
		xexpr := "0"
		if c > 0 {
			terms := make([]string, 0, c)
			for k := 0; k < c; k++ { // widths of the row-0 tiles to the left
				terms = append(terms, "w"+strconv.Itoa(k))
			}
			xexpr = strings.Join(terms, "+")
		}
		yexpr := "0"
		if r > 0 {
			terms := make([]string, 0, r)
			for k := 0; k < r; k++ { // heights of the column-0 tiles above
				terms = append(terms, "h"+strconv.Itoa(k*cols))
			}
			yexpr = strings.Join(terms, "+")
		}
		parts = append(parts, xexpr+"_"+yexpr)
	}
	return cols, rows, strings.Join(parts, "|")
}

// camExportSafeLabel keeps only [A-Za-z0-9 _.-] so a camera name can be dropped
// verbatim into a single-quoted drawtext text= value without any quoting hazard.
func camExportSafeLabel(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z', r >= '0' && r <= '9',
			r == ' ', r == '_', r == '.', r == '-':
			b.WriteRune(r)
		}
	}
	return strings.TrimSpace(b.String())
}

// camExportDrawtextFilters builds the two drawtext filters overlaid on a
// normalized segment: the sanitized camera label (top-left) and a burned-in
// wall-clock timestamp (bottom-left) rendered from the chunk's epoch via
// %{pts:gmtime:<epoch>:%F %T} — the epoch is precomputed so gmtime prints the
// SITE local clock (see camExportEpochFor). The gmtime argument separators are
// backslash-escaped so ffmpeg's filtergraph parser doesn't treat them as option
// delimiters; %F %T deliberately avoids any parse-time colon. Returns "" when
// font is blank so the caller simply skips the overlay (Hik OSD is usually
// burned in anyway). h scales the font size.
func camExportDrawtextFilters(label string, epoch int64, h int, font string) string {
	if strings.TrimSpace(font) == "" {
		return ""
	}
	fs := h / 28
	if fs < 12 {
		fs = 12
	}
	label = camExportSafeLabel(label)
	common := fmt.Sprintf("fontfile='%s':fontcolor=white:fontsize=%d:box=1:boxcolor=black@0.5:boxborderw=4", font, fs)
	top := fmt.Sprintf("drawtext=%s:x=8:y=8:text='%s'", common, label)
	bottom := fmt.Sprintf("drawtext=%s:x=8:y=h-th-8:text='%%{pts\\:gmtime\\:%d\\:%%F %%T}'", common, epoch)
	return top + "," + bottom
}

// camExportEpochFor converts a chunk's start instant into the epoch to feed
// gmtime so the overlay prints the SITE wall clock: gmtime renders UTC, so
// adding the location's UTC offset at that instant makes the displayed digits
// the local clock.
func camExportEpochFor(t time.Time, loc *time.Location) int64 {
	if loc == nil {
		loc = time.Local
	}
	_, offset := t.In(loc).Zone()
	return t.Unix() + int64(offset)
}

// camExportFontFile resolves the drawtext font: the explicit override else the
// first present common Linux DejaVu/Liberation path else "" (overlay skipped).
func camExportFontFile(cfg config) string {
	if f := strings.TrimSpace(cfg.CameraExportFont); f != "" {
		return f
	}
	for _, p := range []string{
		"/usr/share/fonts/truetype/dejavu/DejaVuSans.ttf",
		"/usr/share/fonts/dejavu/DejaVuSans.ttf",
		"/usr/share/fonts/TTF/DejaVuSans.ttf",
		"/usr/share/fonts/truetype/liberation/LiberationSans-Regular.ttf",
		"/usr/share/fonts/liberation/LiberationSans-Regular.ttf",
	} {
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			return p
		}
	}
	return ""
}

// camExportRawUsable reports whether a downloaded raw chunk file exists and is
// large enough to be a real clip (>= camMinValidClipBytes) — the resume test for
// "this chunk already downloaded".
func camExportRawUsable(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.Size() >= camMinValidClipBytes
}

// ─────────────────────────── config resolvers ───────────────────────────

// camExportBudget resolves the wall-clock budget for one export run, hard-capped
// at 4h so a misconfigured override can never run unbounded.
func camExportBudget(cfg config) time.Duration {
	d := cfg.CameraExportBudget
	if d <= 0 {
		d = 2 * time.Hour
	}
	if d > 4*time.Hour {
		d = 4 * time.Hour
	}
	return d
}

// camExportMaxSeconds resolves the export window cap (default 1h), hard-capped at
// 4h — deliberately far above the 300s live-clip cap.
func camExportMaxSeconds(cfg config) int {
	s := cfg.CameraExportMaxSeconds
	if s <= 0 {
		s = 3600
	}
	if s > 4*3600 {
		s = 4 * 3600
	}
	return s
}

// camExportMaxCameras resolves the per-export camera cap (default 6), never above
// the grid pane ceiling.
func camExportMaxCameras(cfg config) int {
	n := cfg.CameraExportMaxCameras
	if n <= 0 {
		n = 6
	}
	if n > camExportGridHardCap {
		n = camExportGridHardCap
	}
	return n
}

// camExportChunkSeconds resolves the per-chunk download length (the live-clip
// cap; default 300).
func camExportChunkSeconds(cfg config) int {
	s := cfg.CameraClipMaxSeconds
	if s <= 0 {
		s = 300
	}
	return s
}

// camExportTmpRoot resolves the durable scratch root (override else
// cameraMediaRoot/export-tmp).
func camExportTmpRoot(cfg config) string {
	if t := strings.TrimSpace(cfg.CameraExportTmp); t != "" {
		return t
	}
	return filepath.Join(cameraMediaRoot(cfg), "export-tmp")
}

// ─────────────────────────── worker ───────────────────────────

// camExportSem bounds concurrently RUNNING exports. Sized once by
// startCameraExportWorker; stays nil when the worker is off.
var camExportSem chan struct{}

// startCameraExportWorker is the runServe hook for the durable export queue
// (mirrors startCameraAvatarScanWorker: startup requeue, sized semaphore,
// jittered ticker driving camExportTick).
func startCameraExportWorker(cfg config) {
	if !cfg.CameraExportWorkerEnabled {
		camlog("info", "export_worker", map[string]any{"enabled": false})
		return
	}
	// Crash recovery: a "running" row is orphaned (its goroutine died with the
	// process) — requeue for retry. runCameraExport is resumable (finished chunk
	// files + segments are skipped), so at most the in-flight step repeats.
	if db, err := openProxyDB(cfg); err == nil {
		if n, rerr := requeueRunningCameraExports(db); rerr != nil {
			camlog("warn", "export_recover", map[string]any{"ok": false, "error": rerr.Error()})
		} else if n > 0 {
			camlog("info", "export_recover", map[string]any{"requeued": n})
		}
		db.Close()
	}

	interval := cfg.CameraExportTickInterval
	if interval <= 0 {
		interval = 5 * time.Second
	}
	conc := cfg.CameraExportConcurrency
	if conc < 1 {
		conc = 1
	}
	camExportSem = make(chan struct{}, conc)
	camlog("info", "export_worker_start", map[string]any{"tick_ms": interval.Milliseconds(), "concurrency": conc})

	go func() {
		time.Sleep(camStartupJitter(interval))
		camExportTick(cfg)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for range ticker.C {
			camExportTick(cfg)
		}
	}()
}

// camExportTick claims and dispatches queued exports (no-ops while the proxy is
// globally paused); also runs the stale-running reaper and sweeps orphaned
// scratch dirs older than 48h.
func camExportTick(cfg config) {
	if !proxyEnabled.Load() {
		return
	}
	db, err := openProxyDB(cfg)
	if err != nil {
		camlog("error", "export_tick", map[string]any{"ok": false, "error": err.Error()})
		return
	}
	defer db.Close()

	if n, rerr := requeueStaleRunningCameraExports(db, camExportBudget(cfg)+2*time.Minute); rerr != nil {
		camlog("warn", "export_reap", map[string]any{"ok": false, "error": rerr.Error()})
	} else if n > 0 {
		camlog("info", "export_reap", map[string]any{"requeued_stale_running": n})
	}

	camExportSweepOrphans(cfg)

	queued, err := listQueuedCameraExports(db)
	if err != nil {
		camlog("error", "export_tick", map[string]any{"ok": false, "error": err.Error()})
		return
	}
	for _, ex := range queued {
		camConsiderExport(cfg, db, ex)
	}
}

// camExportSweepOrphans removes scratch dirs under the export-tmp root whose
// modtime is older than 48h — leftovers from a run that terminalized but whose
// cleanup was lost (a healthy run removes its own dir).
func camExportSweepOrphans(cfg config) {
	root := camExportTmpRoot(cfg)
	entries, err := os.ReadDir(root)
	if err != nil {
		return // no root yet is fine
	}
	cutoff := time.Now().Add(-48 * time.Hour)
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		info, ierr := e.Info()
		if ierr != nil || info.ModTime().After(cutoff) {
			continue
		}
		p := filepath.Join(root, e.Name())
		if rerr := os.RemoveAll(p); rerr != nil {
			camlog("warn", "export_sweep", map[string]any{"dir": p, "ok": false, "error": rerr.Error()})
		} else {
			camlog("info", "export_sweep", map[string]any{"dir": p, "removed": true})
		}
	}
}

// camConsiderExport acquires a slot, CAS-claims the row, and spawns the bounded
// DETACHED run with recover() + terminalize-on-panic (the camConsiderAvatarScan
// shape).
func camConsiderExport(cfg config, db *sql.DB, ex camExport) {
	select {
	case camExportSem <- struct{}{}:
	default:
		camlog("debug", "export_busy", map[string]any{"export_id": ex.ID})
		return // reconsidered next tick; row stays "queued"
	}
	claimed, cerr := claimCameraExport(db, ex.ID)
	if cerr != nil {
		<-camExportSem
		camlog("warn", "export_claim", map[string]any{"export_id": ex.ID, "ok": false, "error": cerr.Error()})
		return
	}
	if !claimed {
		<-camExportSem // another tick/instance won the claim — not an error
		return
	}
	go func() {
		defer func() { <-camExportSem }()
		defer func() {
			if rec := recover(); rec != nil {
				camlog("error", "export_panic", map[string]any{"export_id": ex.ID, "panic": fmt.Sprint(rec)})
				camTerminalizeExport(cfg, ex.ID, "export stopped: an internal error occurred")
			}
		}()
		if rerr := runCameraExport(context.Background(), cfg, ex.ID); rerr != nil {
			camlog("error", "export", map[string]any{"export_id": ex.ID, "ok": false, "error": rerr.Error()})
			camTerminalizeExport(cfg, ex.ID, rerr.Error())
		}
	}()
}

// camTerminalizeExport best-effort marks an export failed (the worker's
// error/panic paths) and fires the delivery webhook so the operator learns the
// outcome. Best-effort: if the write fails, the runtime reaper re-runs the row.
func camTerminalizeExport(cfg config, id, msg string) {
	if db, err := openProxyDB(cfg); err == nil {
		_ = setCameraExportStatus(db, id, "failed", msg)
		db.Close()
	}
	go camNotifyExportReady(cfg, id)
}

// ─────────────────────────── the run ───────────────────────────

// camExportRun carries one claimed export's shared state across its stages.
type camExportRun struct {
	cfg      config
	db       *sql.DB
	ex       camExport
	jobdir   string
	from, to time.Time
	cams     []camera // resolved, site-owned, in request order
	dvrByID  map[string]CamDVR
	q        StreamQuality
	prog     *camExportProgress
	chunkSec int
	font     string
}

func (r *camExportRun) saveProgress() {
	if err := setCameraExportProgress(r.db, r.ex.ID, mustJSON(r.prog)); err != nil {
		camlog("warn", "export", map[string]any{"export_id": r.ex.ID, "progress_ok": false, "error": err.Error()})
	}
}

func (r *camExportRun) camProgress(id string) *camExportCamProgress {
	if r.prog.Cameras == nil {
		r.prog.Cameras = map[string]*camExportCamProgress{}
	}
	cp := r.prog.Cameras[id]
	if cp == nil {
		cp = &camExportCamProgress{}
		r.prog.Cameras[id] = cp
	}
	return cp
}

// runCameraExport executes one claimed export to completion. A non-nil return is
// reserved for setup failures that left the row "running"; in-run failures and a
// clean "no footage" both self-terminalize (failed|done) with the webhook fired.
func runCameraExport(ctx context.Context, cfg config, exportID string) error {
	start := time.Now()
	db, err := openProxyDB(cfg)
	if err != nil {
		return fmt.Errorf("open proxy db: %w", err)
	}
	defer db.Close()

	ex, err := getCameraExport(db, exportID)
	if err != nil {
		return fmt.Errorf("load export: %w", err)
	}
	if ex.Status != "running" {
		return nil // only a worker-claimed (running) row executes
	}

	// Preflight: ffmpeg, object storage, a valid non-future window within the cap.
	if ok, _ := ffmpegAvailable(cfg); !ok {
		return r0Fail(db, cfg, ex, "ffmpeg is not available on the camera server — evidence export needs it")
	}
	if !camS3Enabled(cfg) {
		return r0Fail(db, cfg, ex, "object storage (S3) is not configured — evidence export delivers permanent links")
	}
	from, ferr := time.Parse(time.RFC3339, strings.TrimSpace(ex.FromTS))
	if ferr != nil {
		return r0Fail(db, cfg, ex, "invalid from_ts: "+ferr.Error())
	}
	to, terr := time.Parse(time.RFC3339, strings.TrimSpace(ex.ToTS))
	if terr != nil {
		return r0Fail(db, cfg, ex, "invalid to_ts: "+terr.Error())
	}
	if now := time.Now(); to.After(now) {
		to = now
	}
	if !from.Before(to) {
		return r0Fail(db, cfg, ex, `export window: "from" must be before "to"`)
	}
	if maxSpan := time.Duration(camExportMaxSeconds(cfg)) * time.Second; to.Sub(from) > maxSpan {
		return r0Fail(db, cfg, ex, fmt.Sprintf("export window exceeds the %s cap", maxSpan))
	}

	// Camera scope: the requested ids, filtered to enabled cameras on enabled
	// DVRs owned by this site, in request order, capped.
	reqIDs := camExportParseCameraIDs(ex.CameraIDs)
	cams, _ := listCamerasBySite(db, ex.SiteID)
	dvrs, _ := listCameraDVRs(db, cfg, ex.SiteID)
	dvrByID := make(map[string]CamDVR, len(dvrs))
	for _, d := range dvrs {
		dvrByID[d.ID] = d
	}
	camByID := make(map[string]camera, len(cams))
	for _, c := range cams {
		camByID[c.ID] = c
	}
	var scope []camera
	for _, id := range reqIDs {
		c, ok := camByID[id]
		if !ok || !c.Enabled {
			continue
		}
		if dv, ok := dvrByID[c.DVRID]; !ok || !dv.Enabled {
			continue
		}
		scope = append(scope, c)
	}
	if len(scope) == 0 {
		return r0Fail(db, cfg, ex, "no enabled, site-owned cameras in scope for this export")
	}
	if maxCams := camExportMaxCameras(cfg); ex.Layout == "grid" {
		if len(scope) > camExportGridHardCap {
			scope = scope[:camExportGridHardCap]
		}
	} else if len(scope) > maxCams {
		scope = scope[:maxCams]
	}

	q := StreamMain
	if strings.EqualFold(strings.TrimSpace(ex.Quality), "sub") {
		q = StreamSub
	}

	jobdir := filepath.Join(camExportTmpRoot(cfg), ex.ID)
	if err := os.MkdirAll(jobdir, 0o700); err != nil {
		return r0Fail(db, cfg, ex, "create scratch dir: "+err.Error())
	}

	prog := camExportParseProgress(ex.Progress)
	r := &camExportRun{
		cfg: cfg, db: db, ex: ex, jobdir: jobdir, from: from, to: to,
		cams: scope, dvrByID: dvrByID, q: q, prog: &prog,
		chunkSec: camExportChunkSeconds(cfg), font: camExportFontFile(cfg),
	}

	cctx, cancel := context.WithTimeout(ctx, camExportBudget(cfg))
	defer cancel()

	camlog("info", "export", map[string]any{
		"export_id": ex.ID, "site_id": ex.SiteID, "layout": ex.Layout, "quality": q.String(),
		"cameras": len(scope), "window": windowLabel(from, to),
	})

	// 1) Download every camera's chunks (sequential, gaps disclosed).
	if err := r.download(cctx); err != nil {
		return r.fail(err.Error(), start)
	}
	// 2) Compose per the layout.
	outputs, cerr := r.compose(cctx)
	if cerr != nil {
		return r.fail(cerr.Error(), start)
	}
	if len(outputs) == 0 {
		return r.fail("no recorded footage was found in the requested window for any camera", start)
	}
	// 3) Upload each output + persist, then finish.
	return r.finish(cctx, outputs, start)
}

// r0Fail terminalizes a setup-stage failure (the row is "running" but made no
// progress) and fires the delivery webhook. Returns nil so the worker doesn't
// double-terminalize.
func r0Fail(db *sql.DB, cfg config, ex camExport, msg string) error {
	_ = setCameraExportStatus(db, ex.ID, "failed", msg)
	camlog("warn", "export", map[string]any{"export_id": ex.ID, "ok": false, "error": msg})
	go camNotifyExportReady(cfg, ex.ID)
	return nil
}

// fail terminalizes an in-run failure (failed) and fires the webhook. Returns
// nil so the worker's caller doesn't re-terminalize.
func (r *camExportRun) fail(msg string, start time.Time) error {
	r.prog.Note = strings.TrimSpace(strings.TrimPrefix(r.prog.Note+"; "+msg, "; "))
	r.saveProgress()
	_ = setCameraExportStatus(r.db, r.ex.ID, "failed", msg)
	// Reclaim the scratch dir on the TERMINAL-fail path exactly as finish() does on
	// success. "failed" is terminal (the reaper only re-runs "running" rows), so this
	// run never resumes and the jobdir — up to several GB of downloaded/normalized
	// footage — would otherwise linger until camExportSweepOrphans' 48h cutoff, letting
	// repeated failures (e.g. bad S3 creds surfacing only at upload) fill the disk.
	_ = os.RemoveAll(r.jobdir)
	camlog("error", "export", map[string]any{
		"export_id": r.ex.ID, "status": "failed", "error": msg, "latency_ms": time.Since(start).Milliseconds(),
	})
	go camNotifyExportReady(r.cfg, r.ex.ID)
	return nil
}

// download pulls every camera's window as ≤chunkSec chunks SEQUENTIALLY through
// capturePlaybackClip (which itself queues behind captureSem). errNoRecording is
// a disclosed gap; any other error is retried once then recorded as a
// gap-with-note. Progress is checkpointed after each chunk so a crash-requeued
// run resumes past finished files and known gaps.
func (r *camExportRun) download(ctx context.Context) error {
	r.prog.Stage = "download"
	r.saveProgress()
	for _, cam := range r.cams {
		dv, ok := r.dvrByID[cam.DVRID]
		if !ok {
			continue
		}
		chunks := camExportChunks(r.from, r.to, r.chunkSec)
		cp := r.camProgress(cam.ID)
		cp.ChunksTotal = len(chunks)
		gapSet := make(map[string]bool, len(cp.Gaps))
		for _, g := range cp.Gaps {
			gapSet[g] = true
		}
		done := 0
		for i, win := range chunks {
			if ctx.Err() != nil {
				return errors.New("wall-clock budget exhausted during download")
			}
			key := camExportChunkKey(win[0], win[1])
			raw := filepath.Join(r.jobdir, fmt.Sprintf("raw_%s_%03d.mp4", cam.ID, i))
			if camExportRawUsable(raw) || gapSet[key] {
				done++
				continue // resume: already downloaded, or a known gap
			}
			res, cerr := capturePlaybackClip(ctx, r.cfg, dv, cam.Channel, r.q, win[0], win[1], raw)
			if cerr != nil && !errors.Is(cerr, errNoRecording) {
				// One retry on a transient/device error before conceding a gap.
				res, cerr = capturePlaybackClip(ctx, r.cfg, dv, cam.Channel, r.q, win[0], win[1], raw)
			}
			switch {
			case cerr == nil && camExportRawUsable(res.Path):
				// keep the file (its path is raw with the .mp4 extension enforced)
			default:
				gapSet[key] = true
				cp.Gaps = append(cp.Gaps, key)
			}
			done++
			cp.ChunksDone = done
			r.saveProgress()
		}
		cp.ChunksDone = len(chunks)
		r.saveProgress()
		camlog("info", "export", map[string]any{
			"export_id": r.ex.ID, "camera_id": cam.ID, "chunks": len(chunks), "gaps": len(cp.Gaps),
		})
	}
	return nil
}

// rawChunkPath returns the download path for camera cam's i-th chunk.
func (r *camExportRun) rawChunkPath(camID string, i int) string {
	return filepath.Join(r.jobdir, fmt.Sprintf("raw_%s_%03d.mp4", camID, i))
}

// compose builds the deliverable MP4(s) for the layout and returns their outputs
// (token/S3 filled in by finish). Empty result = every camera was all-gap.
func (r *camExportRun) compose(ctx context.Context) ([]camExportOutput, error) {
	r.prog.Stage = "compose"
	r.saveProgress()
	switch r.ex.Layout {
	case "sequential":
		return r.composeSequential(ctx)
	case "grid":
		return r.composeGrid(ctx)
	default: // "separate"
		return r.composeSeparate(ctx)
	}
}

// composeSeparate concatenates each camera's RAW chunks losslessly (-c copy) into
// one MP4 per camera — no re-encode, so the DVR's own on-screen timestamp is
// preserved. Cameras with no usable footage are omitted (disclosed as gaps).
func (r *camExportRun) composeSeparate(ctx context.Context) ([]camExportOutput, error) {
	var outputs []camExportOutput
	for idx, cam := range r.cams {
		chunks := camExportChunks(r.from, r.to, r.chunkSec)
		var raws []string
		for i := range chunks {
			p := r.rawChunkPath(cam.ID, i)
			if camExportRawUsable(p) {
				raws = append(raws, p)
			}
		}
		if len(raws) == 0 {
			continue
		}
		out := filepath.Join(r.jobdir, fmt.Sprintf("out_%02d_%s.mp4", idx+1, cam.ID))
		if err := r.concatCopy(ctx, raws, out); err != nil {
			return nil, fmt.Errorf("concat camera %s: %w", cam.ID, err)
		}
		o := camExportOutput{
			CameraID: cam.ID, Label: camDisplayName(cam), ContentType: "video/mp4",
			Caption: fmt.Sprintf("%s — %s", camDisplayName(cam), windowLabel(r.from, r.to)),
		}
		o.localPathSet(out)
		outputs = append(outputs, o)
	}
	return outputs, nil
}

// composeSequential normalizes every camera's chunks onto a common 16:9 canvas
// (with the label + wall-clock overlay) and concatenates them all back-to-back
// into one MP4, camera after camera in request order.
func (r *camExportRun) composeSequential(ctx context.Context) ([]camExportOutput, error) {
	h := r.cfg.CameraExportHeight
	if h <= 0 {
		h = 720
	}
	w := roundEven(h * 16 / 9)
	var segs []string
	for _, cam := range r.cams {
		chunks := camExportChunks(r.from, r.to, r.chunkSec)
		loc, _ := camSiteTimezone([]CamDVR{r.dvrByID[cam.DVRID]})
		for i, win := range chunks {
			raw := r.rawChunkPath(cam.ID, i)
			if !camExportRawUsable(raw) {
				continue
			}
			seg := filepath.Join(r.jobdir, fmt.Sprintf("seq_%s_%03d.mp4", cam.ID, i))
			if !camExportRawUsable(seg) {
				dt := camExportDrawtextFilters(camDisplayName(cam), camExportEpochFor(win[0], loc), h, r.font)
				if err := r.normalizeSeg(ctx, raw, seg, w, h, dt); err != nil {
					return nil, fmt.Errorf("normalize %s chunk %d: %w", cam.ID, i, err)
				}
			}
			segs = append(segs, seg)
		}
	}
	if len(segs) == 0 {
		return nil, nil
	}
	out := filepath.Join(r.jobdir, "sequential.mp4")
	if err := r.concatCopy(ctx, segs, out); err != nil {
		return nil, fmt.Errorf("concat sequential: %w", err)
	}
	o := camExportOutput{
		Label: "sequential", ContentType: "video/mp4",
		Caption: fmt.Sprintf("Sequential evidence video across %d cameras — %s", len(r.cams), windowLabel(r.from, r.to)),
	}
	o.localPathSet(out)
	return []camExportOutput{o}, nil
}

// composeGrid builds one full-window timeline per camera (real chunks normalized
// to the tile size, gaps/missing filled with black of the nominal duration),
// then xstack's them into a 2/4/6/9-pane wall with one final encode. Surplus
// panes (n < cols*rows) are filled with a black timeline.
//
// NOTE: exact cross-pane time sync depends on each timeline being the same
// length; partial-recording drift is validated on the live site (WS5 E2E),
// where the operator confirms the black-pane + sync behaviour.
func (r *camExportRun) composeGrid(ctx context.Context) ([]camExportOutput, error) {
	tw := r.cfg.CameraExportGridTileW
	if tw <= 0 {
		tw = 640
	}
	th := r.cfg.CameraExportGridTileH
	if th <= 0 {
		th = 360
	}
	cols, rows, layout := camExportGridLayout(len(r.cams))
	panes := cols * rows

	var timelines []string
	anyReal := false
	for _, cam := range r.cams {
		chunks := camExportChunks(r.from, r.to, r.chunkSec)
		loc, _ := camSiteTimezone([]CamDVR{r.dvrByID[cam.DVRID]})
		var segs []string
		for i, win := range chunks {
			seg := filepath.Join(r.jobdir, fmt.Sprintf("grid_%s_%03d.mp4", cam.ID, i))
			raw := r.rawChunkPath(cam.ID, i)
			if camExportRawUsable(raw) {
				anyReal = true
				if !camExportRawUsable(seg) {
					dt := camExportDrawtextFilters(camDisplayName(cam), camExportEpochFor(win[0], loc), th, r.font)
					if err := r.normalizeSeg(ctx, raw, seg, tw, th, dt); err != nil {
						return nil, fmt.Errorf("normalize grid %s chunk %d: %w", cam.ID, i, err)
					}
				}
			} else {
				if err := r.blackSeg(ctx, seg, tw, th, win[1].Sub(win[0]).Seconds()); err != nil {
					return nil, fmt.Errorf("black filler %s chunk %d: %w", cam.ID, i, err)
				}
			}
			segs = append(segs, seg)
		}
		timeline := filepath.Join(r.jobdir, fmt.Sprintf("timeline_%s.mp4", cam.ID))
		if err := r.concatCopy(ctx, segs, timeline); err != nil {
			return nil, fmt.Errorf("concat timeline %s: %w", cam.ID, err)
		}
		timelines = append(timelines, timeline)
	}
	if !anyReal {
		return nil, nil
	}
	// Pad surplus panes with a full-window black timeline.
	for len(timelines) < panes {
		filler := filepath.Join(r.jobdir, fmt.Sprintf("timeline_black_%d.mp4", len(timelines)))
		if err := r.blackSeg(ctx, filler, tw, th, r.to.Sub(r.from).Seconds()); err != nil {
			return nil, fmt.Errorf("black pane %d: %w", len(timelines), err)
		}
		timelines = append(timelines, filler)
	}

	out := filepath.Join(r.jobdir, "grid.mp4")
	if err := r.xstack(ctx, timelines, layout, out); err != nil {
		return nil, fmt.Errorf("xstack: %w", err)
	}
	o := camExportOutput{
		Label: "grid", ContentType: "video/mp4",
		Caption: fmt.Sprintf("%dx%d grid evidence video across %d cameras — %s", cols, rows, len(r.cams), windowLabel(r.from, r.to)),
	}
	o.localPathSet(out)
	return []camExportOutput{o}, nil
}

// finish uploads each output to public object storage, persists it as a
// retention-bound capture (kind "export") with its permanent S3 url, records the
// outputs JSON, marks the row done, removes the scratch dir, and fires delivery.
func (r *camExportRun) finish(ctx context.Context, outputs []camExportOutput, start time.Time) error {
	r.prog.Stage = "upload"
	r.saveProgress()
	final := make([]camExportOutput, 0, len(outputs))
	for _, o := range outputs {
		local := o.localPath()
		name := camExportOutputName(o)
		token, perr := camPersistCapture(r.db, r.cfg, "", r.ex.SiteID, o.CameraID, "export", r.q.String(),
			local, "video/mp4", 0, 0, r.from.Format(time.RFC3339), r.to.Format(time.RFC3339))
		if perr != nil {
			return r.fail("persist output: "+perr.Error(), start)
		}
		if fi, e := os.Stat(local); e == nil {
			o.Bytes = fi.Size()
		}
		s3url, uerr := s3PutObjectPublicFile(ctx, r.cfg, camS3ExportKey(r.ex.SiteID, r.ex.ID, name), local, "video/mp4")
		if uerr != nil {
			return r.fail("upload output: "+uerr.Error(), start)
		}
		if serr := setCameraCaptureS3URL(r.db, token, s3url); serr != nil {
			camlog("warn", "export", map[string]any{"export_id": r.ex.ID, "s3_persist_ok": false, "error": serr.Error()})
		}
		o.Token = token
		o.S3URL = s3url
		o.ContentType = "video/mp4"
		final = append(final, o)
	}
	if err := setCameraExportOutputs(r.db, r.ex.ID, mustJSON(final)); err != nil {
		return r.fail("record outputs: "+err.Error(), start)
	}
	// The run has fully SUCCEEDED here — outputs persisted, every file uploaded to S3.
	// Only the terminal status write remains, so a transient failure must NOT be
	// reported as a failed export: propagating the error makes camConsiderExport call
	// camTerminalizeExport, which sets status="failed" while the payload still carries
	// the valid permanent S3 links (a self-contradictory camera_export_ready webhook).
	// Retry briefly, then leave the row "running" and return nil so the stale-running
	// reaper re-runs idempotently (S3 objects overwrite) instead of terminalizing.
	var derr error
	for attempt := 0; attempt < 3; attempt++ {
		if derr = setCameraExportStatus(r.db, r.ex.ID, "done", ""); derr == nil {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if derr != nil {
		camlog("error", "export", map[string]any{
			"export_id": r.ex.ID, "status": "done_write_failed", "error": derr.Error(),
		})
		return nil // leave "running"; the reaper re-runs (idempotent — S3 objects overwrite)
	}
	_ = os.RemoveAll(r.jobdir)

	camlog("info", "export", map[string]any{
		"export_id": r.ex.ID, "status": "done", "outputs": len(final),
		"latency_ms": time.Since(start).Milliseconds(),
	})
	// Deliver AFTER the terminal write so a re-poll always agrees with the webhook.
	go camNotifyExportReady(r.cfg, r.ex.ID)
	if strings.TrimSpace(r.ex.InvestigationID) != "" {
		r.appendInvestigationNote(final)
	}
	return nil
}

// appendInvestigationNote drops a tool row into the originating investigation's
// transcript so the operator (and the model, on a follow-up) sees the delivered
// links. The media MediaURL is the served token URL (so settle can resolve the
// capture); the caption carries the permanent S3 link.
func (r *camExportRun) appendInvestigationNote(outputs []camExportOutput) {
	var links []string
	var media []evidenceItem
	for _, o := range outputs {
		link := firstNonEmpty(o.S3URL, camMediaURL(r.cfg, nil, o.Token))
		links = append(links, link)
		media = append(media, evidenceItem{
			MediaURL: camMediaURL(r.cfg, nil, o.Token),
			Caption:  strings.TrimSpace(o.Caption + " — " + link),
		})
	}
	summary := fmt.Sprintf("evidence_export completed: %d link(s) — %s", len(links), strings.Join(links, " | "))
	camAppendInvestigateMessage(r.db, r.ex.InvestigationID, "tool", summary, "evidence_export", "", 0, media)
}

// ─────────────────────────── ffmpeg composition ───────────────────────────

// camExportEncodeTimeout scales the subprocess deadline to the work: a base plus
// 4x the media duration, clamped to the run budget.
func (r *camExportRun) encodeTimeout(base time.Duration, durSec float64) time.Duration {
	t := base + time.Duration(durSec*4)*time.Second
	if budget := camExportBudget(r.cfg); t > budget {
		t = budget
	}
	return t
}

// normalizeSeg re-encodes one raw chunk onto a w×h canvas at the configured fps
// with the optional label/timestamp overlay: scale (keep aspect) → pad → fps →
// yuv420p [→ drawtext], libx264 veryfast. Identical params across every seg make
// the later concat lossless (-c copy).
func (r *camExportRun) normalizeSeg(ctx context.Context, raw, seg string, w, h int, drawtext string) error {
	fps := r.cfg.CameraExportFPS
	if fps <= 0 {
		fps = 15
	}
	crf := r.cfg.CameraExportCRF
	if crf <= 0 {
		crf = 26
	}
	vf := fmt.Sprintf("scale=w=%d:h=%d:force_original_aspect_ratio=decrease,pad=%d:%d:(ow-iw)/2:(oh-ih)/2,fps=%d,format=yuv420p", w, h, w, h, fps)
	if strings.TrimSpace(drawtext) != "" {
		vf += "," + drawtext
	}
	args := []string{
		"-nostdin", "-loglevel", "error",
		"-i", raw,
		"-vf", vf,
		"-c:v", "libx264", "-preset", "veryfast", "-crf", strconv.Itoa(crf),
		"-an", "-movflags", "+faststart", "-y", seg,
	}
	return r.runFFmpeg(ctx, r.encodeTimeout(2*time.Minute, float64(r.chunkSec)), args, "normalize", seg)
}

// blackSeg synthesizes a w×h black filler of durSec seconds with the SAME codec
// params as normalizeSeg (libx264 yuv420p at fps) so it concats losslessly into a
// camera timeline for gaps / missing footage.
func (r *camExportRun) blackSeg(ctx context.Context, seg string, w, h int, durSec float64) error {
	if durSec <= 0 {
		durSec = 1
	}
	fps := r.cfg.CameraExportFPS
	if fps <= 0 {
		fps = 15
	}
	crf := r.cfg.CameraExportCRF
	if crf <= 0 {
		crf = 26
	}
	args := []string{
		"-nostdin", "-loglevel", "error",
		"-f", "lavfi", "-i", fmt.Sprintf("color=c=black:s=%dx%d:r=%d:d=%.3f", w, h, fps, durSec),
		"-c:v", "libx264", "-preset", "veryfast", "-crf", strconv.Itoa(crf),
		"-pix_fmt", "yuv420p", "-an", "-movflags", "+faststart", "-y", seg,
	}
	return r.runFFmpeg(ctx, r.encodeTimeout(1*time.Minute, durSec), args, "black", seg)
}

// concatCopy losslessly joins parts (in order) via the concat demuxer (-c copy).
// The demuxer list file references each part by absolute path with any single
// quote escaped per the concat syntax.
func (r *camExportRun) concatCopy(ctx context.Context, parts []string, out string) error {
	if len(parts) == 0 {
		return errors.New("nothing to concat")
	}
	// A single part still goes through the demuxer so the output is a clean,
	// faststart-remuxed MP4 (copying, no re-encode).
	var b strings.Builder
	for _, p := range parts {
		abs, err := filepath.Abs(p)
		if err != nil {
			abs = p
		}
		b.WriteString("file '")
		b.WriteString(strings.ReplaceAll(abs, "'", `'\''`))
		b.WriteString("'\n")
	}
	listPath := out + ".concat.txt"
	if err := os.WriteFile(listPath, []byte(b.String()), 0o600); err != nil {
		return fmt.Errorf("write concat list: %w", err)
	}
	args := []string{
		"-nostdin", "-loglevel", "error",
		"-f", "concat", "-safe", "0", "-i", listPath,
		"-c", "copy", "-movflags", "+faststart", "-y", out,
	}
	total := r.to.Sub(r.from).Seconds() * float64(max(1, len(r.cams)))
	return r.runFFmpeg(ctx, r.encodeTimeout(2*time.Minute, total), args, "concat", out)
}

// xstack composes the per-camera timelines into a single pane wall with one final
// encode, capped by cfg.CameraExportMaxBytes and the total window duration.
func (r *camExportRun) xstack(ctx context.Context, inputs []string, layout, out string) error {
	crf := r.cfg.CameraExportCRF
	if crf <= 0 {
		crf = 26
	}
	maxBytes := r.cfg.CameraExportMaxBytes
	if maxBytes <= 0 {
		maxBytes = 2 << 30
	}
	args := []string{"-nostdin", "-loglevel", "error"}
	var labels strings.Builder
	for i, in := range inputs {
		args = append(args, "-i", in)
		labels.WriteString(fmt.Sprintf("[%d:v]", i))
	}
	filter := fmt.Sprintf("%sxstack=inputs=%d:layout=%s:fill=black[v]", labels.String(), len(inputs), layout)
	args = append(args,
		"-filter_complex", filter,
		"-map", "[v]",
		"-c:v", "libx264", "-preset", "veryfast", "-crf", strconv.Itoa(crf),
		"-an", "-movflags", "+faststart",
		"-t", strconv.Itoa(int(r.to.Sub(r.from).Seconds())),
		"-fs", strconv.FormatInt(maxBytes, 10),
		"-y", out,
	)
	return r.runFFmpeg(ctx, r.encodeTimeout(5*time.Minute, r.to.Sub(r.from).Seconds()), args, "xstack", out)
}

// runFFmpeg executes an ffmpeg subprocess through runCamCommand, returning an
// error (with the full stderr logged) on a timeout, a non-zero exit, or a
// missing/empty output. The command is never credential-bearing (local files).
func (r *camExportRun) runFFmpeg(ctx context.Context, timeout time.Duration, args []string, op, out string) error {
	bin := cameraFFmpegBin(r.cfg)
	res, err := runCamCommand(ctx, timeout, bin, args...)
	if err != nil {
		camlog("error", "export_ffmpeg", map[string]any{"export_id": r.ex.ID, "op": op, "ok": false, "error": err.Error()})
		return fmt.Errorf("ffmpeg %s exec: %w", op, err)
	}
	if res.TimedOut {
		camlog("error", "export_ffmpeg", map[string]any{"export_id": r.ex.ID, "op": op, "timed_out": true, "stderr": res.Stderr})
		return fmt.Errorf("ffmpeg %s timed out after %s", op, timeout)
	}
	if res.ExitCode != 0 {
		camlog("error", "export_ffmpeg", map[string]any{"export_id": r.ex.ID, "op": op, "exit_code": res.ExitCode, "stderr": res.Stderr})
		return fmt.Errorf("ffmpeg %s exited %d: %s", op, res.ExitCode, truncateString(res.Stderr, 500))
	}
	if fi, e := os.Stat(out); e != nil || fi.Size() < camMinValidClipBytes {
		return fmt.Errorf("ffmpeg %s produced no usable output", op)
	}
	return nil
}

// ─────────────────────────── output path plumbing ───────────────────────────

// localPath / localPathSet stash the on-disk path on the output value during the
// compose→upload handoff (the field is unexported, so it never escapes the JSON
// contract).
func (o *camExportOutput) localPath() string     { return o.local }
func (o *camExportOutput) localPathSet(p string) { o.local = p }

// camExportOutputName derives the S3/object file name for an output.
func camExportOutputName(o camExportOutput) string {
	switch {
	case o.CameraID != "":
		return camExportSafeFileToken(o.Label) + "_" + camExportSafeFileToken(o.CameraID) + ".mp4"
	default:
		return camExportSafeFileToken(o.Label) + ".mp4"
	}
}

// camExportSafeFileToken reduces a label/id to a filesystem/URL-safe token.
func camExportSafeFileToken(s string) string {
	s = strings.TrimSpace(s)
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	out := strings.Trim(b.String(), "_")
	if out == "" {
		out = "clip"
	}
	return out
}

// roundEven rounds up to the nearest even integer (h264 needs even dimensions).
func roundEven(n int) int {
	if n < 2 {
		return 2
	}
	if n%2 != 0 {
		n++
	}
	return n
}

// ─────────────────────────── investigation tool ───────────────────────────

// camParseExportWindow parses an evidence-export [from,to] window: RFC3339 both
// ends, "to" clamped to now, span clamped to cfg's export max (NOT the 300s clip
// cap). Cloned from camParseWindow but for the export ceiling.
func camParseExportWindow(fromRaw, toRaw string, cfg config) (from, to time.Time, err error) {
	fromRaw, toRaw = strings.TrimSpace(fromRaw), strings.TrimSpace(toRaw)
	if fromRaw == "" || toRaw == "" {
		return time.Time{}, time.Time{}, errors.New(`"from" and "to" must both be RFC3339 timestamps`)
	}
	from, ferr := time.Parse(time.RFC3339, fromRaw)
	if ferr != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("invalid \"from\": %w", ferr)
	}
	to, terr := time.Parse(time.RFC3339, toRaw)
	if terr != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("invalid \"to\": %w", terr)
	}
	if now := time.Now(); to.After(now) {
		to = now
	}
	if !from.Before(to) {
		return time.Time{}, time.Time{}, errors.New(`"from" must be before "to"`)
	}
	if maxSec := camExportMaxSeconds(cfg); to.Sub(from) > time.Duration(maxSec)*time.Second {
		from = to.Add(-time.Duration(maxSec) * time.Second)
	}
	return from, to, nil
}

// camNormalizeExportLayout returns the accepted layout (default "separate").
func camNormalizeExportLayout(raw string) string {
	l := strings.ToLower(strings.TrimSpace(raw))
	if camExportLayouts[l] {
		return l
	}
	return "separate"
}

// camToolEvidenceExport is the investigation tool: it validates the cameras +
// window + layout, enqueues a durable export, and returns IMMEDIATELY with a
// queued acknowledgement (Fetches:0). The links are delivered to the operator
// out-of-band (webhook + a transcript note), so the model must NOT wait on it.
func camToolEvidenceExport(cfg config, db *sql.DB, site camSite, invID string, args investigateArgs, allowed map[string]bool) investigateToolResult {
	if !cfg.CameraExportWorkerEnabled {
		return investigateToolResult{Summary: "evidence_export: the export worker is disabled on the camera server — the export cannot run."}
	}
	if !camS3Enabled(cfg) {
		return investigateToolResult{Summary: "evidence_export: object storage is not configured — exports are delivered as permanent links and need it."}
	}
	ids := camFilterAllowed(args.CameraIDs, allowed)
	if len(ids) == 0 {
		return investigateToolResult{Summary: "evidence_export: no valid camera_ids given (must be exact ids from the roster)."}
	}
	layout := camNormalizeExportLayout(args.Layout)
	maxCams := camExportMaxCameras(cfg)
	if layout == "grid" {
		maxCams = camExportGridHardCap
	}
	if len(ids) > maxCams {
		ids = ids[:maxCams]
	}
	from, to, werr := camParseExportWindow(args.From, args.To, cfg)
	if werr != nil {
		return investigateToolResult{Summary: "evidence_export: " + werr.Error()}
	}
	quality := "main"
	if strings.EqualFold(strings.TrimSpace(args.Quality), "sub") {
		quality = "sub"
	}
	id, ierr := insertCameraExport(db, camExport{
		SiteID: site.ID, InvestigationID: invID, CameraIDs: mustJSON(ids),
		FromTS: from.Format(time.RFC3339), ToTS: to.Format(time.RFC3339),
		Layout: layout, Quality: quality, Status: "queued",
	})
	if ierr != nil {
		return investigateToolResult{Summary: "evidence_export: could not queue the export: " + ierr.Error()}
	}
	camlog("info", "export_enqueue", map[string]any{
		"export_id": id, "site_id": site.ID, "investigation_id": invID,
		"cameras": len(ids), "layout": layout, "window": windowLabel(from, to),
	})
	return investigateToolResult{
		Summary: fmt.Sprintf("evidence_export queued (id=%s): a %s video across %d camera(s) for %s is rendering in the BACKGROUND. Permanent link(s) will be delivered to the operator automatically when ready — do NOT wait for it or re-check it; finish the investigation and answer normally.",
			id, layout, len(ids), windowLabel(from, to)),
		Fetches: 0,
	}
}

// ─────────────────────────── admin handlers ───────────────────────────

// cameraExportJSON renders an export row for API responses (parsed camera_ids /
// progress / outputs; each output carries its served media_url + permanent s3_url).
func cameraExportJSON(cfg config, r *http.Request, ex camExport) map[string]any {
	outs := camExportParseOutputs(ex.Outputs)
	outList := make([]map[string]any, 0, len(outs))
	for _, o := range outs {
		item := map[string]any{
			"camera_id": o.CameraID, "label": o.Label, "token": o.Token,
			"s3_url": o.S3URL, "bytes": o.Bytes, "content_type": o.ContentType, "caption": o.Caption,
		}
		if o.Token != "" {
			item["media_url"] = camMediaURL(cfg, r, o.Token)
		}
		outList = append(outList, item)
	}
	return map[string]any{
		"id": ex.ID, "site_id": ex.SiteID, "investigation_id": ex.InvestigationID,
		"camera_ids": camExportParseCameraIDs(ex.CameraIDs),
		"from_ts":    ex.FromTS, "to_ts": ex.ToTS, "layout": ex.Layout, "quality": ex.Quality,
		"status": ex.Status, "progress": camDecodeJSON(ex.Progress), "outputs": outList,
		"error": ex.Error, "created_at": ex.CreatedAt, "updated_at": ex.UpdatedAt,
	}
}

// camExportCreateFromBody is the shared create core for the admin + service
// routes: it validates the window/layout/cameras against siteID and enqueues the
// row. cameraIDs are filtered to the site's own cameras.
func camExportCreateFromBody(cfg config, db *sql.DB, siteID string, cameraIDs []string, fromRaw, toRaw, layout, quality string) (camExport, error) {
	if !cfg.CameraExportWorkerEnabled {
		return camExport{}, errors.New("the evidence-export worker is disabled on the camera server")
	}
	if !camS3Enabled(cfg) {
		return camExport{}, errors.New("object storage is not configured — exports deliver permanent links")
	}
	cams, _ := listCamerasBySite(db, siteID)
	allowed := make(map[string]bool, len(cams))
	for _, c := range cams {
		allowed[c.ID] = true
	}
	ids := camFilterAllowed(cameraIDs, allowed)
	if len(ids) == 0 {
		return camExport{}, errors.New("camera_ids must be one or more cameras that belong to this site")
	}
	l := camNormalizeExportLayout(layout)
	maxCams := camExportMaxCameras(cfg)
	if l == "grid" {
		maxCams = camExportGridHardCap
	}
	if len(ids) > maxCams {
		ids = ids[:maxCams]
	}
	from, to, werr := camParseExportWindow(fromRaw, toRaw, cfg)
	if werr != nil {
		return camExport{}, werr
	}
	q := "main"
	if strings.EqualFold(strings.TrimSpace(quality), "sub") {
		q = "sub"
	}
	id, ierr := insertCameraExport(db, camExport{
		SiteID: siteID, CameraIDs: mustJSON(ids),
		FromTS: from.Format(time.RFC3339), ToTS: to.Format(time.RFC3339),
		Layout: l, Quality: q, Status: "queued",
	})
	if ierr != nil {
		return camExport{}, ierr
	}
	return getCameraExport(db, id)
}

// handleCameraExports serves /ui/api/cameras/exports (admin): GET ?site_id= lists
// newest-first; POST {site_id, camera_ids, from, to, layout?, quality?} enqueues.
func handleCameraExports(cfg config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		db, err := openProxyDB(cfg)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		defer db.Close()
		switch r.Method {
		case http.MethodGet:
			siteID := strings.TrimSpace(r.URL.Query().Get("site_id"))
			if siteID == "" {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": "site_id is required"})
				return
			}
			exports, lerr := listCameraExports(db, siteID)
			if lerr != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]any{"error": lerr.Error()})
				return
			}
			out := make([]map[string]any, 0, len(exports))
			for _, ex := range exports {
				out = append(out, cameraExportJSON(cfg, r, ex))
			}
			writeJSON(w, http.StatusOK, map[string]any{"exports": out})
		case http.MethodPost:
			var body struct {
				SiteID    string   `json:"site_id"`
				CameraIDs []string `json:"camera_ids"`
				From      string   `json:"from"`
				To        string   `json:"to"`
				Layout    string   `json:"layout"`
				Quality   string   `json:"quality"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid JSON"})
				return
			}
			if strings.TrimSpace(body.SiteID) == "" {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": "site_id is required"})
				return
			}
			ex, cerr := camExportCreateFromBody(cfg, db, strings.TrimSpace(body.SiteID), body.CameraIDs, body.From, body.To, body.Layout, body.Quality)
			if cerr != nil {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": cerr.Error()})
				return
			}
			camlog("info", "export_enqueue", map[string]any{"export_id": ex.ID, "site_id": ex.SiteID, "layout": ex.Layout, "admin": true})
			writeJSON(w, http.StatusOK, map[string]any{"ok": true, "export": cameraExportJSON(cfg, r, ex)})
		default:
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		}
	}
}

// handleCameraExportGet serves GET /ui/api/cameras/exports/get?id= (admin).
func handleCameraExportGet(cfg config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
			return
		}
		id := strings.TrimSpace(r.URL.Query().Get("id"))
		if id == "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "id is required"})
			return
		}
		db, err := openProxyDB(cfg)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		defer db.Close()
		ex, gerr := getCameraExport(db, id)
		if gerr != nil {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "not found"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"export": cameraExportJSON(cfg, r, ex)})
	}
}
