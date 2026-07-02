package main

// camera_store.go — SQLite schema + typed CRUD for the camera subsystem.
//
// migrateCameraDB is hooked from migrateProxyDB (main.go) so the camera tables are
// created idempotently alongside the rest of the proxy schema. Conventions match
// the existing store: ids via randomID(prefix), RFC3339 text timestamps, bools as
// INTEGER, `?` placeholders. DVR passwords are encrypted at rest with
// encryptSecret(cfg,pass) into password_enc plus a maskSecret(pass) preview —
// exactly like provider_keys.api_key_enc/key_preview — so helpers that touch
// passwords take cfg.

import (
	"database/sql"
	"strings"
	"time"
)

// rowScanner is satisfied by both *sql.Row and *sql.Rows so scan helpers can be
// shared between get-one and list-many queries.
type rowScanner interface {
	Scan(dest ...any) error
}

// ─────────────────────────── row types ───────────────────────────

// camSite is a monitored place (e.g. a clinic) the operator describes.
type camSite struct {
	ID             string
	Name           string
	Description    string
	AnalysisAlias  string
	AnalysisJSON   string
	AnalysisStatus string // "", "pending", "running", "done", "error"
	LastAnalyzedAt string
	CreatedAt      string
	UpdatedAt      string
}

// camDVRView is a credential-safe view of a camera_dvrs row for the UI (preview
// only, never plaintext). Backend code that needs the password uses CamDVR via
// getCameraDVR/listCameraDVRs (which decrypt).
type camDVRView struct {
	ID                 string
	SiteID             string
	Name               string
	Brand              string
	Host               string
	Port               int
	HTTPPort           int
	Username           string
	PasswordPreview    string
	Timezone           string
	ChannelsDiscovered int
	Enabled            bool
	LastDiscoveredAt   string
	CreatedAt          string
	UpdatedAt          string
}

// camWatchRun is one execution of a watch (a monitoring run).
type camWatchRun struct {
	ID             string
	WatchID        string
	SiteID         string
	StartedAt      string
	FinishedAt     string
	Status         string // running | done | error
	Suspicion      string // none | low | medium | high
	RuleStatus     string // not_met | possibly_met | met
	Rounds         int
	Summary        string
	TranscriptJSON string
	FiredAction    string
	ActionResult   string
	Error          string
	CreatedAt      string
}

// camQuestion is a clarifying question the AI asked the operator during setup.
type camQuestion struct {
	ID            string
	SiteID        string
	AnalysisRunID string
	CameraIDs     string // JSON array
	Question      string
	Answer        string
	Status        string // open | answered | dismissed
	AskedAt       string
	AnsweredAt    string
}

// camCapture is a served media artifact (snapshot/mosaic/clip/frame) with a
// capability token, mirroring the generated-image serving model.
type camCapture struct {
	ID          string
	SiteID      string
	CameraID    string
	WatchRunID  string
	Kind        string // snapshot | mosaic | clip | frame
	Quality     string // main | sub
	Token       string // UNIQUE capability token
	Path        string
	ContentType string
	Width       int
	Height      int
	Bytes       int64
	FromTS      string
	ToTS        string
	CreatedAt   string
	ExpiresAt   string
}

// camInvestigation is one ask-AI investigation session: an operator's freeform
// question about a site, answered by a server-orchestrated agentic loop over the
// camera tools (camera_investigate.go). Status drives the UI: active (running),
// awaiting_operator (paused on an ask_operator turn), answered (a clean final
// narrative was produced; follow-ups may continue it), exhausted (the run stopped
// at a turn/media/time bound or an analysis error rather than a clean answer —
// still re-openable, and a follow-up starts a fresh run), closed (operator
// dismissed).
type camInvestigation struct {
	ID        string
	SiteID    string
	Title     string
	Question  string
	Alias     string
	Status    string // active | awaiting_operator | answered | exhausted | closed
	CreatedAt string
	UpdatedAt string
}

// camInvestigationMessage is one entry in an investigation's transcript. role is
// operator | ai | tool | system; tool_name/tool_args are set on tool rows and
// media_json carries the served capability tokens/URLs shown that turn. Fetches
// records how many DVR device fetches a tool row cost (0 on non-tool rows) so
// the media budget reconstructs across a pause/resume in the SAME unit the live
// loop spends it (mediaUsed += tr.Fetches) — persisted media artifacts alone
// diverge (past_frames: 1 fetch -> many frames; mosaic: many fetches -> 1 tile).
type camInvestigationMessage struct {
	ID              string
	InvestigationID string
	Seq             int
	Role            string // operator | ai | tool | system
	Content         string
	ToolName        string
	ToolArgs        string
	MediaJSON       string
	Fetches         int
	CreatedAt       string
}

// camEvent is one diagnostics-trail row (written by appendCameraEvent).
type camEvent struct {
	ID        string
	TS        string
	Level     string
	DVRID     string
	CameraID  string
	WatchID   string
	RunID     string
	Op        string
	OK        bool
	LatencyMs int64
	Detail    string
	Error     string
}

// ─────────────────────────── migration ───────────────────────────

func migrateCameraDB(db *sql.DB) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS camera_sites (
			id TEXT PRIMARY KEY,
			name TEXT NOT NULL,
			description TEXT NOT NULL DEFAULT '',
			analysis_alias TEXT NOT NULL DEFAULT '',
			analysis_json TEXT NOT NULL DEFAULT '',
			analysis_status TEXT NOT NULL DEFAULT '',
			last_analyzed_at TEXT NOT NULL DEFAULT '',
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS camera_dvrs (
			id TEXT PRIMARY KEY,
			site_id TEXT NOT NULL,
			name TEXT NOT NULL DEFAULT '',
			brand TEXT NOT NULL DEFAULT '',
			host TEXT NOT NULL DEFAULT '',
			port INTEGER NOT NULL DEFAULT 0,
			http_port INTEGER NOT NULL DEFAULT 0,
			username TEXT NOT NULL DEFAULT '',
			password_enc TEXT NOT NULL DEFAULT '',
			password_preview TEXT NOT NULL DEFAULT '',
			timezone TEXT NOT NULL DEFAULT '',
			channels_discovered INTEGER NOT NULL DEFAULT 0,
			enabled INTEGER NOT NULL DEFAULT 1,
			last_discovered_at TEXT NOT NULL DEFAULT '',
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS cameras (
			id TEXT PRIMARY KEY,
			site_id TEXT NOT NULL,
			dvr_id TEXT NOT NULL,
			channel INTEGER NOT NULL DEFAULT 0,
			name TEXT NOT NULL DEFAULT '',
			area TEXT NOT NULL DEFAULT '',
			ai_description TEXT NOT NULL DEFAULT '',
			ai_location TEXT NOT NULL DEFAULT '',
			enabled INTEGER NOT NULL DEFAULT 1,
			disabled_reason TEXT NOT NULL DEFAULT '',
			snapshot_capture_id TEXT NOT NULL DEFAULT '',
			caps TEXT NOT NULL DEFAULT '',
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS camera_watches (
			id TEXT PRIMARY KEY,
			site_id TEXT NOT NULL,
			name TEXT NOT NULL DEFAULT '',
			instruction TEXT NOT NULL DEFAULT '',
			camera_ids TEXT NOT NULL DEFAULT '',
			analysis_alias TEXT NOT NULL DEFAULT '',
			mode TEXT NOT NULL DEFAULT 'escalate',
			interval_seconds INTEGER NOT NULL DEFAULT 300,
			active_hours TEXT NOT NULL DEFAULT '',
			max_rounds INTEGER NOT NULL DEFAULT 0,
			action_json TEXT NOT NULL DEFAULT '',
			enabled INTEGER NOT NULL DEFAULT 0,
			next_run_at TEXT NOT NULL DEFAULT '',
			last_run_at TEXT NOT NULL DEFAULT '',
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS camera_watch_runs (
			id TEXT PRIMARY KEY,
			watch_id TEXT NOT NULL,
			site_id TEXT NOT NULL,
			started_at TEXT NOT NULL DEFAULT '',
			finished_at TEXT NOT NULL DEFAULT '',
			status TEXT NOT NULL DEFAULT '',
			suspicion TEXT NOT NULL DEFAULT '',
			rule_status TEXT NOT NULL DEFAULT '',
			rounds INTEGER NOT NULL DEFAULT 0,
			summary TEXT NOT NULL DEFAULT '',
			transcript_json TEXT NOT NULL DEFAULT '',
			fired_action TEXT NOT NULL DEFAULT '',
			action_result TEXT NOT NULL DEFAULT '',
			error TEXT NOT NULL DEFAULT '',
			created_at TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS camera_questions (
			id TEXT PRIMARY KEY,
			site_id TEXT NOT NULL,
			analysis_run_id TEXT NOT NULL DEFAULT '',
			camera_ids TEXT NOT NULL DEFAULT '',
			question TEXT NOT NULL DEFAULT '',
			answer TEXT NOT NULL DEFAULT '',
			status TEXT NOT NULL DEFAULT 'open',
			asked_at TEXT NOT NULL DEFAULT '',
			answered_at TEXT NOT NULL DEFAULT ''
		)`,
		`CREATE TABLE IF NOT EXISTS camera_captures (
			id TEXT PRIMARY KEY,
			site_id TEXT NOT NULL DEFAULT '',
			camera_id TEXT NOT NULL DEFAULT '',
			watch_run_id TEXT NOT NULL DEFAULT '',
			kind TEXT NOT NULL DEFAULT '',
			quality TEXT NOT NULL DEFAULT '',
			token TEXT NOT NULL UNIQUE,
			path TEXT NOT NULL DEFAULT '',
			content_type TEXT NOT NULL DEFAULT '',
			width INTEGER NOT NULL DEFAULT 0,
			height INTEGER NOT NULL DEFAULT 0,
			bytes INTEGER NOT NULL DEFAULT 0,
			from_ts TEXT NOT NULL DEFAULT '',
			to_ts TEXT NOT NULL DEFAULT '',
			created_at TEXT NOT NULL,
			expires_at TEXT NOT NULL DEFAULT ''
		)`,
		`CREATE TABLE IF NOT EXISTS camera_events (
			id TEXT PRIMARY KEY,
			ts TEXT NOT NULL,
			level TEXT NOT NULL DEFAULT '',
			dvr_id TEXT NOT NULL DEFAULT '',
			camera_id TEXT NOT NULL DEFAULT '',
			watch_id TEXT NOT NULL DEFAULT '',
			run_id TEXT NOT NULL DEFAULT '',
			op TEXT NOT NULL DEFAULT '',
			ok INTEGER NOT NULL DEFAULT 1,
			latency_ms INTEGER NOT NULL DEFAULT 0,
			detail TEXT NOT NULL DEFAULT '',
			error TEXT NOT NULL DEFAULT ''
		)`,
		`CREATE TABLE IF NOT EXISTS camera_investigations (
			id TEXT PRIMARY KEY,
			site_id TEXT NOT NULL,
			title TEXT NOT NULL DEFAULT '',
			question TEXT NOT NULL DEFAULT '',
			alias TEXT NOT NULL DEFAULT '',
			status TEXT NOT NULL DEFAULT 'active',
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS camera_investigation_messages (
			id TEXT PRIMARY KEY,
			investigation_id TEXT NOT NULL,
			seq INTEGER NOT NULL DEFAULT 0,
			role TEXT NOT NULL DEFAULT '',
			content TEXT NOT NULL DEFAULT '',
			tool_name TEXT NOT NULL DEFAULT '',
			tool_args TEXT NOT NULL DEFAULT '',
			media_json TEXT NOT NULL DEFAULT '',
			fetches INTEGER NOT NULL DEFAULT 0,
			created_at TEXT NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_cameras_site ON cameras(site_id, enabled)`,
		`CREATE INDEX IF NOT EXISTS idx_cameras_dvr ON cameras(dvr_id)`,
		`CREATE INDEX IF NOT EXISTS idx_camera_dvrs_site ON camera_dvrs(site_id)`,
		`CREATE INDEX IF NOT EXISTS idx_camera_watches_due ON camera_watches(enabled, next_run_at)`,
		`CREATE INDEX IF NOT EXISTS idx_camera_watch_runs_watch ON camera_watch_runs(watch_id, started_at)`,
		`CREATE INDEX IF NOT EXISTS idx_camera_questions_site ON camera_questions(site_id, status)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_camera_captures_token ON camera_captures(token)`,
		`CREATE INDEX IF NOT EXISTS idx_camera_captures_run ON camera_captures(watch_run_id)`,
		`CREATE INDEX IF NOT EXISTS idx_camera_captures_expires ON camera_captures(expires_at)`,
		`CREATE INDEX IF NOT EXISTS idx_camera_investigations_site ON camera_investigations(site_id, status)`,
		`CREATE INDEX IF NOT EXISTS idx_camera_investigations_status ON camera_investigations(status)`,
		`CREATE INDEX IF NOT EXISTS idx_camera_investigation_messages_inv ON camera_investigation_messages(investigation_id, seq)`,
		`CREATE INDEX IF NOT EXISTS idx_camera_events_ts ON camera_events(ts)`,
		`CREATE INDEX IF NOT EXISTS idx_camera_events_dvr ON camera_events(dvr_id)`,
		`CREATE INDEX IF NOT EXISTS idx_camera_events_op ON camera_events(op, ok)`,
	}
	for _, stmt := range stmts {
		if _, err := db.Exec(stmt); err != nil {
			return err
		}
	}
	// Additive column for databases created before the investigation media budget
	// was tracked in device-fetch units (idempotent — a no-op once present).
	if err := ensureSQLiteColumn(db, "camera_investigation_messages", "fetches", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	return nil
}

func nowRFC3339() string { return time.Now().Format(time.RFC3339) }

// ─────────────────────────── camera_sites ───────────────────────────

func insertCameraSite(db *sql.DB, s camSite) (string, error) {
	if s.ID == "" {
		s.ID = randomID("site")
	}
	now := nowRFC3339()
	_, err := db.Exec(`INSERT INTO camera_sites
		(id, name, description, analysis_alias, analysis_json, analysis_status, last_analyzed_at, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		s.ID, s.Name, s.Description, s.AnalysisAlias, s.AnalysisJSON, s.AnalysisStatus, s.LastAnalyzedAt, now, now)
	return s.ID, err
}

func scanCameraSite(s rowScanner) (camSite, error) {
	var v camSite
	err := s.Scan(&v.ID, &v.Name, &v.Description, &v.AnalysisAlias, &v.AnalysisJSON,
		&v.AnalysisStatus, &v.LastAnalyzedAt, &v.CreatedAt, &v.UpdatedAt)
	return v, err
}

const camSiteCols = `id, name, description, analysis_alias, analysis_json, analysis_status, last_analyzed_at, created_at, updated_at`

func getCameraSite(db *sql.DB, id string) (camSite, error) {
	return scanCameraSite(db.QueryRow(`SELECT `+camSiteCols+` FROM camera_sites WHERE id = ?`, id))
}

func listCameraSites(db *sql.DB) ([]camSite, error) {
	rows, err := db.Query(`SELECT ` + camSiteCols + ` FROM camera_sites ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []camSite
	for rows.Next() {
		v, err := scanCameraSite(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func updateCameraSite(db *sql.DB, s camSite) error {
	_, err := db.Exec(`UPDATE camera_sites SET name = ?, description = ?, analysis_alias = ?, updated_at = ? WHERE id = ?`,
		s.Name, s.Description, s.AnalysisAlias, nowRFC3339(), s.ID)
	return err
}

func setSiteAnalysis(db *sql.DB, id, analysisJSON, status, lastAnalyzedAt string) error {
	_, err := db.Exec(`UPDATE camera_sites SET analysis_json = ?, analysis_status = ?, last_analyzed_at = ?, updated_at = ? WHERE id = ?`,
		analysisJSON, status, lastAnalyzedAt, nowRFC3339(), id)
	return err
}

func setSiteAnalysisStatus(db *sql.DB, id, status string) error {
	_, err := db.Exec(`UPDATE camera_sites SET analysis_status = ?, updated_at = ? WHERE id = ?`, status, nowRFC3339(), id)
	return err
}

func deleteCameraSite(db *sql.DB, id string) error {
	_, err := db.Exec(`DELETE FROM camera_sites WHERE id = ?`, id)
	return err
}

// ─────────────────────────── camera_dvrs ───────────────────────────

func insertCameraDVR(db *sql.DB, cfg config, dvr CamDVR) (string, error) {
	if dvr.ID == "" {
		dvr.ID = randomID("dvr")
	}
	enc, err := encryptSecret(cfg, dvr.Password)
	if err != nil {
		return "", err
	}
	now := nowRFC3339()
	_, err = db.Exec(`INSERT INTO camera_dvrs
		(id, site_id, name, brand, host, port, http_port, username, password_enc, password_preview,
		 timezone, channels_discovered, enabled, last_discovered_at, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0, ?, '', ?, ?)`,
		dvr.ID, dvr.SiteID, dvr.Name, strings.ToLower(strings.TrimSpace(dvr.Brand)), dvr.Host, dvr.Port, dvr.HTTPPort,
		dvr.Username, enc, maskSecret(dvr.Password), dvr.Timezone, boolToInt(dvr.Enabled), now, now)
	return dvr.ID, err
}

// getCameraDVR returns a DVR with its password DECRYPTED (backend use only).
func getCameraDVR(db *sql.DB, cfg config, id string) (CamDVR, error) {
	var d CamDVR
	var enc string
	var enabled int
	err := db.QueryRow(`SELECT id, site_id, name, brand, host, port, http_port, username, password_enc, timezone, enabled
		FROM camera_dvrs WHERE id = ?`, id).
		Scan(&d.ID, &d.SiteID, &d.Name, &d.Brand, &d.Host, &d.Port, &d.HTTPPort, &d.Username, &enc, &d.Timezone, &enabled)
	if err != nil {
		return CamDVR{}, err
	}
	d.Enabled = enabled != 0
	if pass, derr := decryptSecret(cfg, enc); derr == nil {
		d.Password = pass
	}
	return d, nil
}

// listCameraDVRs returns DVRs with DECRYPTED passwords (backend use). siteID=="" →
// all DVRs (e.g. the scheduler iterating every site).
func listCameraDVRs(db *sql.DB, cfg config, siteID string) ([]CamDVR, error) {
	var (
		rows *sql.Rows
		err  error
	)
	const q = `SELECT id, site_id, name, brand, host, port, http_port, username, password_enc, timezone, enabled FROM camera_dvrs`
	if strings.TrimSpace(siteID) == "" {
		rows, err = db.Query(q + ` ORDER BY created_at DESC`)
	} else {
		rows, err = db.Query(q+` WHERE site_id = ? ORDER BY created_at DESC`, siteID)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CamDVR
	for rows.Next() {
		var d CamDVR
		var enc string
		var enabled int
		if err := rows.Scan(&d.ID, &d.SiteID, &d.Name, &d.Brand, &d.Host, &d.Port, &d.HTTPPort,
			&d.Username, &enc, &d.Timezone, &enabled); err != nil {
			return nil, err
		}
		d.Enabled = enabled != 0
		if pass, derr := decryptSecret(cfg, enc); derr == nil {
			d.Password = pass
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// listCameraDVRViews returns credential-safe DVR views (preview only) for the UI.
func listCameraDVRViews(db *sql.DB, siteID string) ([]camDVRView, error) {
	const cols = `id, site_id, name, brand, host, port, http_port, username, password_preview, timezone, channels_discovered, enabled, last_discovered_at, created_at, updated_at`
	var (
		rows *sql.Rows
		err  error
	)
	if strings.TrimSpace(siteID) == "" {
		rows, err = db.Query(`SELECT ` + cols + ` FROM camera_dvrs ORDER BY created_at DESC`)
	} else {
		rows, err = db.Query(`SELECT `+cols+` FROM camera_dvrs WHERE site_id = ? ORDER BY created_at DESC`, siteID)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []camDVRView
	for rows.Next() {
		var v camDVRView
		var enabled int
		if err := rows.Scan(&v.ID, &v.SiteID, &v.Name, &v.Brand, &v.Host, &v.Port, &v.HTTPPort,
			&v.Username, &v.PasswordPreview, &v.Timezone, &v.ChannelsDiscovered, &enabled,
			&v.LastDiscoveredAt, &v.CreatedAt, &v.UpdatedAt); err != nil {
			return nil, err
		}
		v.Enabled = enabled != 0
		out = append(out, v)
	}
	return out, rows.Err()
}

// updateCameraDVR updates the mutable DVR fields. When changePassword is true the
// password_enc/preview are rewritten from dvr.Password; otherwise they are left
// untouched (so editing metadata doesn't require re-entering the password).
func updateCameraDVR(db *sql.DB, cfg config, dvr CamDVR, changePassword bool) error {
	now := nowRFC3339()
	if changePassword {
		enc, err := encryptSecret(cfg, dvr.Password)
		if err != nil {
			return err
		}
		_, err = db.Exec(`UPDATE camera_dvrs SET name = ?, brand = ?, host = ?, port = ?, http_port = ?,
			username = ?, password_enc = ?, password_preview = ?, timezone = ?, enabled = ?, updated_at = ? WHERE id = ?`,
			dvr.Name, strings.ToLower(strings.TrimSpace(dvr.Brand)), dvr.Host, dvr.Port, dvr.HTTPPort,
			dvr.Username, enc, maskSecret(dvr.Password), dvr.Timezone, boolToInt(dvr.Enabled), now, dvr.ID)
		return err
	}
	_, err := db.Exec(`UPDATE camera_dvrs SET name = ?, brand = ?, host = ?, port = ?, http_port = ?,
		username = ?, timezone = ?, enabled = ?, updated_at = ? WHERE id = ?`,
		dvr.Name, strings.ToLower(strings.TrimSpace(dvr.Brand)), dvr.Host, dvr.Port, dvr.HTTPPort,
		dvr.Username, dvr.Timezone, boolToInt(dvr.Enabled), now, dvr.ID)
	return err
}

func setDVRDiscovered(db *sql.DB, id string, channels int) error {
	now := nowRFC3339()
	_, err := db.Exec(`UPDATE camera_dvrs SET channels_discovered = ?, last_discovered_at = ?, updated_at = ? WHERE id = ?`,
		channels, now, now, id)
	return err
}

func setDVREnabled(db *sql.DB, id string, enabled bool) error {
	_, err := db.Exec(`UPDATE camera_dvrs SET enabled = ?, updated_at = ? WHERE id = ?`, boolToInt(enabled), nowRFC3339(), id)
	return err
}

func deleteCameraDVR(db *sql.DB, id string) error {
	_, err := db.Exec(`DELETE FROM camera_dvrs WHERE id = ?`, id)
	return err
}

// ─────────────────────────── cameras ───────────────────────────

const camCameraCols = `id, site_id, dvr_id, channel, name, area, ai_description, ai_location, enabled, disabled_reason, snapshot_capture_id, caps, created_at, updated_at`

func scanCamera(s rowScanner) (camera, error) {
	var c camera
	var enabled int
	err := s.Scan(&c.ID, &c.SiteID, &c.DVRID, &c.Channel, &c.Name, &c.Area, &c.AIDescription,
		&c.AILocation, &enabled, &c.DisabledReason, &c.SnapshotCaptureID, &c.Caps, &c.CreatedAt, &c.UpdatedAt)
	c.Enabled = enabled != 0
	return c, err
}

func insertCamera(db *sql.DB, c camera) (string, error) {
	if c.ID == "" {
		c.ID = randomID("cam")
	}
	now := nowRFC3339()
	_, err := db.Exec(`INSERT INTO cameras
		(id, site_id, dvr_id, channel, name, area, ai_description, ai_location, enabled, disabled_reason, snapshot_capture_id, caps, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		c.ID, c.SiteID, c.DVRID, c.Channel, c.Name, c.Area, c.AIDescription, c.AILocation,
		boolToInt(c.Enabled), c.DisabledReason, c.SnapshotCaptureID, c.Caps, now, now)
	return c.ID, err
}

func getCamera(db *sql.DB, id string) (camera, error) {
	return scanCamera(db.QueryRow(`SELECT `+camCameraCols+` FROM cameras WHERE id = ?`, id))
}

func listCamerasBySite(db *sql.DB, siteID string) ([]camera, error) {
	return queryCameras(db, `SELECT `+camCameraCols+` FROM cameras WHERE site_id = ? ORDER BY dvr_id, channel`, siteID)
}

func listCamerasByDVR(db *sql.DB, dvrID string) ([]camera, error) {
	return queryCameras(db, `SELECT `+camCameraCols+` FROM cameras WHERE dvr_id = ? ORDER BY channel`, dvrID)
}

func queryCameras(db *sql.DB, query string, args ...any) ([]camera, error) {
	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []camera
	for rows.Next() {
		c, err := scanCamera(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// findCameraByChannel returns the existing camera for a (dvr, channel) pair, or
// sql.ErrNoRows if none — used by discovery to upsert without duplicating.
func findCameraByChannel(db *sql.DB, dvrID string, channel int) (camera, error) {
	return scanCamera(db.QueryRow(`SELECT `+camCameraCols+` FROM cameras WHERE dvr_id = ? AND channel = ?`, dvrID, channel))
}

func updateCamera(db *sql.DB, c camera) error {
	_, err := db.Exec(`UPDATE cameras SET name = ?, area = ?, ai_description = ?, ai_location = ?,
		enabled = ?, disabled_reason = ?, snapshot_capture_id = ?, caps = ?, updated_at = ? WHERE id = ?`,
		c.Name, c.Area, c.AIDescription, c.AILocation, boolToInt(c.Enabled), c.DisabledReason,
		c.SnapshotCaptureID, c.Caps, nowRFC3339(), c.ID)
	return err
}

func setCameraEnabled(db *sql.DB, id string, enabled bool, reason string) error {
	_, err := db.Exec(`UPDATE cameras SET enabled = ?, disabled_reason = ?, updated_at = ? WHERE id = ?`,
		boolToInt(enabled), reason, nowRFC3339(), id)
	return err
}

func setCameraSnapshot(db *sql.DB, id, captureID string) error {
	_, err := db.Exec(`UPDATE cameras SET snapshot_capture_id = ?, updated_at = ? WHERE id = ?`, captureID, nowRFC3339(), id)
	return err
}

func deleteCamera(db *sql.DB, id string) error {
	_, err := db.Exec(`DELETE FROM cameras WHERE id = ?`, id)
	return err
}

// ─────────────────────────── camera_watches ───────────────────────────

const camWatchCols = `id, site_id, name, instruction, camera_ids, analysis_alias, mode, interval_seconds, active_hours, max_rounds, action_json, enabled, next_run_at, last_run_at, created_at, updated_at`

func scanWatch(s rowScanner) (watch, error) {
	var w watch
	var enabled int
	err := s.Scan(&w.ID, &w.SiteID, &w.Name, &w.Instruction, &w.CameraIDs, &w.AnalysisAlias, &w.Mode,
		&w.IntervalSeconds, &w.ActiveHours, &w.MaxRounds, &w.ActionJSON, &enabled, &w.NextRunAt,
		&w.LastRunAt, &w.CreatedAt, &w.UpdatedAt)
	w.Enabled = enabled != 0
	return w, err
}

func insertCameraWatch(db *sql.DB, w watch) (string, error) {
	if w.ID == "" {
		w.ID = randomID("watch")
	}
	now := nowRFC3339()
	_, err := db.Exec(`INSERT INTO camera_watches
		(id, site_id, name, instruction, camera_ids, analysis_alias, mode, interval_seconds, active_hours, max_rounds, action_json, enabled, next_run_at, last_run_at, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		w.ID, w.SiteID, w.Name, w.Instruction, w.CameraIDs, w.AnalysisAlias, w.Mode, w.IntervalSeconds,
		w.ActiveHours, w.MaxRounds, w.ActionJSON, boolToInt(w.Enabled), w.NextRunAt, w.LastRunAt, now, now)
	return w.ID, err
}

func getCameraWatch(db *sql.DB, id string) (watch, error) {
	return scanWatch(db.QueryRow(`SELECT `+camWatchCols+` FROM camera_watches WHERE id = ?`, id))
}

func listCameraWatches(db *sql.DB, siteID string) ([]watch, error) {
	if strings.TrimSpace(siteID) == "" {
		return queryWatches(db, `SELECT `+camWatchCols+` FROM camera_watches ORDER BY created_at DESC`)
	}
	return queryWatches(db, `SELECT `+camWatchCols+` FROM camera_watches WHERE site_id = ? ORDER BY created_at DESC`, siteID)
}

// listDueCameraWatches returns enabled watches whose next_run_at is due (empty or
// <= now). The scheduler then atomically claims each via claimCameraWatch.
func listDueCameraWatches(db *sql.DB, now string) ([]watch, error) {
	return queryWatches(db, `SELECT `+camWatchCols+` FROM camera_watches
		WHERE enabled = 1 AND (next_run_at = '' OR next_run_at <= ?) ORDER BY next_run_at`, now)
}

func queryWatches(db *sql.DB, query string, args ...any) ([]watch, error) {
	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []watch
	for rows.Next() {
		w, err := scanWatch(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// claimCameraWatch atomically advances next_run_at from expectedNextRun to
// newNextRun, returning true only if THIS caller won the claim (RowsAffected==1).
// This is the no-double-run guard for concurrent scheduler ticks.
func claimCameraWatch(db *sql.DB, id, expectedNextRun, newNextRun string) (bool, error) {
	res, err := db.Exec(`UPDATE camera_watches SET next_run_at = ?, updated_at = ? WHERE id = ? AND next_run_at = ? AND enabled = 1`,
		newNextRun, nowRFC3339(), id, expectedNextRun)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n == 1, nil
}

func updateCameraWatch(db *sql.DB, w watch) error {
	_, err := db.Exec(`UPDATE camera_watches SET name = ?, instruction = ?, camera_ids = ?, analysis_alias = ?,
		mode = ?, interval_seconds = ?, active_hours = ?, max_rounds = ?, action_json = ?, enabled = ?, updated_at = ? WHERE id = ?`,
		w.Name, w.Instruction, w.CameraIDs, w.AnalysisAlias, w.Mode, w.IntervalSeconds, w.ActiveHours,
		w.MaxRounds, w.ActionJSON, boolToInt(w.Enabled), nowRFC3339(), w.ID)
	return err
}

func setWatchEnabled(db *sql.DB, id string, enabled bool, nextRunAt string) error {
	_, err := db.Exec(`UPDATE camera_watches SET enabled = ?, next_run_at = ?, updated_at = ? WHERE id = ?`,
		boolToInt(enabled), nextRunAt, nowRFC3339(), id)
	return err
}

func setWatchLastRun(db *sql.DB, id, lastRunAt, nextRunAt string) error {
	_, err := db.Exec(`UPDATE camera_watches SET last_run_at = ?, next_run_at = ?, updated_at = ? WHERE id = ?`,
		lastRunAt, nextRunAt, nowRFC3339(), id)
	return err
}

// setWatchLastRunOnly stamps last_run_at WITHOUT touching next_run_at. The manual
// "run now" path uses this: its in-memory watch snapshot carries a next_run_at
// captured at trigger time, but a concurrent scheduler tick may have atomically
// claimed and advanced next_run_at while the manual run was in flight. Re-writing
// the stale snapshot (as setWatchLastRun would) reverts that claim to a now-past
// timestamp, so the next tick treats the watch as due again — an unintended
// duplicate scheduled run. Leaving next_run_at alone preserves the atomic claim.
func setWatchLastRunOnly(db *sql.DB, id, lastRunAt string) error {
	_, err := db.Exec(`UPDATE camera_watches SET last_run_at = ?, updated_at = ? WHERE id = ?`,
		lastRunAt, nowRFC3339(), id)
	return err
}

func deleteCameraWatch(db *sql.DB, id string) error {
	_, err := db.Exec(`DELETE FROM camera_watches WHERE id = ?`, id)
	return err
}

// ─────────────────────────── camera_watch_runs ───────────────────────────

const camRunCols = `id, watch_id, site_id, started_at, finished_at, status, suspicion, rule_status, rounds, summary, transcript_json, fired_action, action_result, error, created_at`

func scanWatchRun(s rowScanner) (camWatchRun, error) {
	var r camWatchRun
	err := s.Scan(&r.ID, &r.WatchID, &r.SiteID, &r.StartedAt, &r.FinishedAt, &r.Status, &r.Suspicion,
		&r.RuleStatus, &r.Rounds, &r.Summary, &r.TranscriptJSON, &r.FiredAction, &r.ActionResult, &r.Error, &r.CreatedAt)
	return r, err
}

func insertCameraWatchRun(db *sql.DB, r camWatchRun) (string, error) {
	if r.ID == "" {
		r.ID = randomID("run")
	}
	now := nowRFC3339()
	if r.StartedAt == "" {
		r.StartedAt = now
	}
	if r.Status == "" {
		r.Status = "running"
	}
	_, err := db.Exec(`INSERT INTO camera_watch_runs
		(id, watch_id, site_id, started_at, finished_at, status, suspicion, rule_status, rounds, summary, transcript_json, fired_action, action_result, error, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.ID, r.WatchID, r.SiteID, r.StartedAt, r.FinishedAt, r.Status, r.Suspicion, r.RuleStatus,
		r.Rounds, r.Summary, r.TranscriptJSON, r.FiredAction, r.ActionResult, r.Error, now)
	return r.ID, err
}

// finishCameraWatchRun writes the terminal state of a run.
func finishCameraWatchRun(db *sql.DB, r camWatchRun) error {
	if r.FinishedAt == "" {
		r.FinishedAt = nowRFC3339()
	}
	_, err := db.Exec(`UPDATE camera_watch_runs SET finished_at = ?, status = ?, suspicion = ?, rule_status = ?,
		rounds = ?, summary = ?, transcript_json = ?, fired_action = ?, action_result = ?, error = ? WHERE id = ?`,
		r.FinishedAt, r.Status, r.Suspicion, r.RuleStatus, r.Rounds, r.Summary, r.TranscriptJSON,
		r.FiredAction, r.ActionResult, r.Error, r.ID)
	return err
}

func getCameraWatchRun(db *sql.DB, id string) (camWatchRun, error) {
	return scanWatchRun(db.QueryRow(`SELECT `+camRunCols+` FROM camera_watch_runs WHERE id = ?`, id))
}

func listCameraWatchRuns(db *sql.DB, watchID string, limit int) ([]camWatchRun, error) {
	if limit <= 0 {
		limit = 50
	}
	var (
		rows *sql.Rows
		err  error
	)
	if strings.TrimSpace(watchID) == "" {
		rows, err = db.Query(`SELECT `+camRunCols+` FROM camera_watch_runs ORDER BY started_at DESC LIMIT ?`, limit)
	} else {
		rows, err = db.Query(`SELECT `+camRunCols+` FROM camera_watch_runs WHERE watch_id = ? ORDER BY started_at DESC LIMIT ?`, watchID, limit)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []camWatchRun
	for rows.Next() {
		r, err := scanWatchRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ─────────────────────────── camera_questions ───────────────────────────

const camQuestionCols = `id, site_id, analysis_run_id, camera_ids, question, answer, status, asked_at, answered_at`

func scanQuestion(s rowScanner) (camQuestion, error) {
	var q camQuestion
	err := s.Scan(&q.ID, &q.SiteID, &q.AnalysisRunID, &q.CameraIDs, &q.Question, &q.Answer, &q.Status, &q.AskedAt, &q.AnsweredAt)
	return q, err
}

func insertCameraQuestion(db *sql.DB, q camQuestion) (string, error) {
	if q.ID == "" {
		q.ID = randomID("q")
	}
	if q.AskedAt == "" {
		q.AskedAt = nowRFC3339()
	}
	if q.Status == "" {
		q.Status = "open"
	}
	_, err := db.Exec(`INSERT INTO camera_questions
		(id, site_id, analysis_run_id, camera_ids, question, answer, status, asked_at, answered_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		q.ID, q.SiteID, q.AnalysisRunID, q.CameraIDs, q.Question, q.Answer, q.Status, q.AskedAt, q.AnsweredAt)
	return q.ID, err
}

func getCameraQuestion(db *sql.DB, id string) (camQuestion, error) {
	return scanQuestion(db.QueryRow(`SELECT `+camQuestionCols+` FROM camera_questions WHERE id = ?`, id))
}

// listCameraQuestions filters by site and (optionally) status ("" → all statuses).
func listCameraQuestions(db *sql.DB, siteID, status string) ([]camQuestion, error) {
	var (
		rows *sql.Rows
		err  error
	)
	switch {
	case strings.TrimSpace(status) == "":
		rows, err = db.Query(`SELECT `+camQuestionCols+` FROM camera_questions WHERE site_id = ? ORDER BY asked_at DESC`, siteID)
	default:
		rows, err = db.Query(`SELECT `+camQuestionCols+` FROM camera_questions WHERE site_id = ? AND status = ? ORDER BY asked_at DESC`, siteID, status)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []camQuestion
	for rows.Next() {
		q, err := scanQuestion(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, q)
	}
	return out, rows.Err()
}

func answerCameraQuestion(db *sql.DB, id, answer string) error {
	_, err := db.Exec(`UPDATE camera_questions SET answer = ?, status = 'answered', answered_at = ? WHERE id = ?`,
		answer, nowRFC3339(), id)
	return err
}

func setCameraQuestionStatus(db *sql.DB, id, status string) error {
	_, err := db.Exec(`UPDATE camera_questions SET status = ? WHERE id = ?`, status, id)
	return err
}

// ─────────────────────────── camera_captures ───────────────────────────

const camCaptureCols = `id, site_id, camera_id, watch_run_id, kind, quality, token, path, content_type, width, height, bytes, from_ts, to_ts, created_at, expires_at`

func scanCapture(s rowScanner) (camCapture, error) {
	var c camCapture
	err := s.Scan(&c.ID, &c.SiteID, &c.CameraID, &c.WatchRunID, &c.Kind, &c.Quality, &c.Token, &c.Path,
		&c.ContentType, &c.Width, &c.Height, &c.Bytes, &c.FromTS, &c.ToTS, &c.CreatedAt, &c.ExpiresAt)
	return c, err
}

func insertCameraCapture(db *sql.DB, c camCapture) (string, error) {
	if c.ID == "" {
		c.ID = randomID("cap")
	}
	if c.Token == "" {
		c.Token = randToken(16)
	}
	if c.CreatedAt == "" {
		c.CreatedAt = nowRFC3339()
	}
	_, err := db.Exec(`INSERT INTO camera_captures
		(id, site_id, camera_id, watch_run_id, kind, quality, token, path, content_type, width, height, bytes, from_ts, to_ts, created_at, expires_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		c.ID, c.SiteID, c.CameraID, c.WatchRunID, c.Kind, c.Quality, c.Token, c.Path, c.ContentType,
		c.Width, c.Height, c.Bytes, c.FromTS, c.ToTS, c.CreatedAt, c.ExpiresAt)
	return c.ID, err
}

func getCameraCapture(db *sql.DB, id string) (camCapture, error) {
	return scanCapture(db.QueryRow(`SELECT `+camCaptureCols+` FROM camera_captures WHERE id = ?`, id))
}

func getCameraCaptureByToken(db *sql.DB, token string) (camCapture, error) {
	return scanCapture(db.QueryRow(`SELECT `+camCaptureCols+` FROM camera_captures WHERE token = ?`, token))
}

func listCameraCaptures(db *sql.DB, watchRunID string) ([]camCapture, error) {
	rows, err := db.Query(`SELECT `+camCaptureCols+` FROM camera_captures WHERE watch_run_id = ? ORDER BY created_at`, watchRunID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []camCapture
	for rows.Next() {
		c, err := scanCapture(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// deleteExpiredCameraCaptures removes capture rows whose expires_at has passed
// (non-empty and <= now). Returns the number of rows deleted. The reaper deletes
// the on-disk files separately.
func deleteExpiredCameraCaptures(db *sql.DB, now string) (int64, error) {
	res, err := db.Exec(`DELETE FROM camera_captures WHERE expires_at != '' AND expires_at <= ?`, now)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// ─────────────────────────── camera_events ───────────────────────────

const camEventCols = `id, ts, level, dvr_id, camera_id, watch_id, run_id, op, ok, latency_ms, detail, error`

func scanEvent(s rowScanner) (camEvent, error) {
	var e camEvent
	var ok int
	err := s.Scan(&e.ID, &e.TS, &e.Level, &e.DVRID, &e.CameraID, &e.WatchID, &e.RunID, &e.Op, &ok, &e.LatencyMs, &e.Detail, &e.Error)
	e.OK = ok != 0
	return e, err
}

// listRecentCameraEvents returns the most recent events (newest first).
func listRecentCameraEvents(db *sql.DB, limit int) ([]camEvent, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := db.Query(`SELECT `+camEventCols+` FROM camera_events ORDER BY ts DESC, id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collectEvents(rows)
}

// listRecentCameraEventFailures returns the most recent failed events (ok=0).
func listRecentCameraEventFailures(db *sql.DB, limit int) ([]camEvent, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := db.Query(`SELECT `+camEventCols+` FROM camera_events WHERE ok = 0 ORDER BY ts DESC, id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collectEvents(rows)
}

func collectEvents(rows *sql.Rows) ([]camEvent, error) {
	var out []camEvent
	for rows.Next() {
		e, err := scanEvent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// pruneCameraEvents deletes events older than the given RFC3339 cutoff (mirrors
// metricsPruner). Returns the number of rows removed.
func pruneCameraEvents(db *sql.DB, olderThan string) (int64, error) {
	res, err := db.Exec(`DELETE FROM camera_events WHERE ts < ?`, olderThan)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// ─────────────────────────── camera_investigations ───────────────────────────

const camInvestigationCols = `id, site_id, title, question, alias, status, created_at, updated_at`

func scanInvestigation(s rowScanner) (camInvestigation, error) {
	var v camInvestigation
	err := s.Scan(&v.ID, &v.SiteID, &v.Title, &v.Question, &v.Alias, &v.Status, &v.CreatedAt, &v.UpdatedAt)
	return v, err
}

func insertCameraInvestigation(db *sql.DB, inv camInvestigation) (string, error) {
	if inv.ID == "" {
		inv.ID = randomID("inv")
	}
	if inv.Status == "" {
		inv.Status = "active"
	}
	now := nowRFC3339()
	if inv.CreatedAt == "" {
		inv.CreatedAt = now
	}
	if inv.UpdatedAt == "" {
		inv.UpdatedAt = now
	}
	_, err := db.Exec(`INSERT INTO camera_investigations
		(id, site_id, title, question, alias, status, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		inv.ID, inv.SiteID, inv.Title, inv.Question, inv.Alias, inv.Status, inv.CreatedAt, inv.UpdatedAt)
	return inv.ID, err
}

func getCameraInvestigation(db *sql.DB, id string) (camInvestigation, error) {
	return scanInvestigation(db.QueryRow(`SELECT `+camInvestigationCols+` FROM camera_investigations WHERE id = ?`, id))
}

// listCameraInvestigations lists a site's investigations newest-first; siteID==""
// lists every site's investigations.
func listCameraInvestigations(db *sql.DB, siteID string) ([]camInvestigation, error) {
	var (
		rows *sql.Rows
		err  error
	)
	if strings.TrimSpace(siteID) == "" {
		rows, err = db.Query(`SELECT ` + camInvestigationCols + ` FROM camera_investigations ORDER BY updated_at DESC`)
	} else {
		rows, err = db.Query(`SELECT `+camInvestigationCols+` FROM camera_investigations WHERE site_id = ? ORDER BY updated_at DESC`, siteID)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []camInvestigation
	for rows.Next() {
		v, err := scanInvestigation(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// setCameraInvestigationStatus updates an investigation's lifecycle status and
// stamps updated_at.
func setCameraInvestigationStatus(db *sql.DB, id, status string) error {
	_, err := db.Exec(`UPDATE camera_investigations SET status = ?, updated_at = ? WHERE id = ?`, status, nowRFC3339(), id)
	return err
}

// claimCameraInvestigation atomically transitions an investigation from "queued"
// to "running", returning true only if THIS caller won the claim (RowsAffected==1)
// — the same compare-and-swap idiom as claimCameraWatch, so two worker ticks (or
// two proxy instances) can never run the same investigation twice.
func claimCameraInvestigation(db *sql.DB, id string) (bool, error) {
	res, err := db.Exec(`UPDATE camera_investigations SET status = 'running', updated_at = ?
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

// listQueuedCameraInvestigations returns investigations awaiting a worker claim
// (status='queued'), oldest-activity first so the queue is fair/FIFO.
func listQueuedCameraInvestigations(db *sql.DB) ([]camInvestigation, error) {
	rows, err := db.Query(`SELECT ` + camInvestigationCols +
		` FROM camera_investigations WHERE status = 'queued' ORDER BY updated_at ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []camInvestigation
	for rows.Next() {
		v, err := scanInvestigation(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// requeueRunningCameraInvestigations resets investigations stranded in "running"
// (a crash/restart left them with no live worker goroutine) back to "queued" for
// retry. runInvestigation is resumable — it reloads the persisted transcript and
// recomputes turn/media budgets — so at most the single in-flight turn is lost.
// Returns how many were reset; run once at worker startup, before the ticker.
func requeueRunningCameraInvestigations(db *sql.DB) (int64, error) {
	// Also catch legacy "active" rows: the pre-queue build set that status while a
	// synchronous run was in flight, so after upgrading such a row is orphaned too
	// (the new code never writes "active"). Resumable, so re-running is safe.
	res, err := db.Exec(`UPDATE camera_investigations SET status = 'queued', updated_at = ?
		WHERE status IN ('running', 'active')`, nowRFC3339())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// requeueStaleRunningCameraInvestigations is the RUNTIME reaper (called every tick):
// it requeues "running" rows with no activity for longer than olderThan. A run's
// context can't outlive the budget, and a healthy run bumps updated_at every turn,
// so a "running" row idle for > budget+margin is orphaned (its goroutine crashed,
// or its terminalization write was lost) and safe to re-run — which self-heals the
// stuck-"running" case without waiting for a process restart. Returns how many.
func requeueStaleRunningCameraInvestigations(db *sql.DB, olderThan time.Duration) (int64, error) {
	cutoff := time.Now().Add(-olderThan).Format(time.RFC3339)
	res, err := db.Exec(`UPDATE camera_investigations SET status = 'queued', updated_at = ?
		WHERE status = 'running' AND updated_at < ?`, nowRFC3339(), cutoff)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// ─────────────────────── camera_investigation_messages ───────────────────────

const camInvMsgCols = `id, investigation_id, seq, role, content, tool_name, tool_args, media_json, fetches, created_at`

func scanInvestigationMessage(s rowScanner) (camInvestigationMessage, error) {
	var m camInvestigationMessage
	err := s.Scan(&m.ID, &m.InvestigationID, &m.Seq, &m.Role, &m.Content, &m.ToolName, &m.ToolArgs, &m.MediaJSON, &m.Fetches, &m.CreatedAt)
	return m, err
}

// appendCameraInvestigationMessage appends a transcript message, auto-assigning
// the next seq within the investigation (when m.Seq is 0) and bumping the
// investigation's updated_at so lists sort by recent activity. Returns the new id.
func appendCameraInvestigationMessage(db *sql.DB, m camInvestigationMessage) (string, error) {
	if m.ID == "" {
		m.ID = randomID("imsg")
	}
	if m.CreatedAt == "" {
		m.CreatedAt = nowRFC3339()
	}
	if m.Seq <= 0 {
		var maxSeq sql.NullInt64
		_ = db.QueryRow(`SELECT MAX(seq) FROM camera_investigation_messages WHERE investigation_id = ?`, m.InvestigationID).Scan(&maxSeq)
		m.Seq = int(maxSeq.Int64) + 1
	}
	_, err := db.Exec(`INSERT INTO camera_investigation_messages
		(id, investigation_id, seq, role, content, tool_name, tool_args, media_json, fetches, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		m.ID, m.InvestigationID, m.Seq, m.Role, m.Content, m.ToolName, m.ToolArgs, m.MediaJSON, m.Fetches, m.CreatedAt)
	if err != nil {
		return "", err
	}
	_, _ = db.Exec(`UPDATE camera_investigations SET updated_at = ? WHERE id = ?`, nowRFC3339(), m.InvestigationID)
	return m.ID, nil
}

// listCameraInvestigationMessages returns an investigation's transcript in seq order.
func listCameraInvestigationMessages(db *sql.DB, investigationID string) ([]camInvestigationMessage, error) {
	rows, err := db.Query(`SELECT `+camInvMsgCols+` FROM camera_investigation_messages WHERE investigation_id = ? ORDER BY seq`, investigationID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []camInvestigationMessage
	for rows.Next() {
		m, err := scanInvestigationMessage(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// getCameraInvestigationWithMessages loads an investigation plus its full ordered
// transcript in one call (the /get endpoint's shape).
func getCameraInvestigationWithMessages(db *sql.DB, id string) (camInvestigation, []camInvestigationMessage, error) {
	inv, err := getCameraInvestigation(db, id)
	if err != nil {
		return camInvestigation{}, nil, err
	}
	msgs, err := listCameraInvestigationMessages(db, id)
	if err != nil {
		return inv, nil, err
	}
	return inv, msgs, nil
}
