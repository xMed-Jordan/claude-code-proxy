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
	PolicyJSON     string // per-site person-role vocabulary + notes (camera_roles.go); descriptive only
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
	AIInstructions     string
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
	S3URL       string // public object-storage URL for large evidence (clips); "" = proxy-served only
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
	ParentID  string // "" for a top-level run; a parent investigation id for a delegated sub-investigation (WS3)
	ViewToken string // capability token for the public read-only timeline page (/camera/investigations/<view_token>)
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
		`CREATE INDEX IF NOT EXISTS idx_camera_captures_camera ON camera_captures(camera_id, created_at)`,
		`CREATE INDEX IF NOT EXISTS idx_camera_investigations_site ON camera_investigations(site_id, status)`,
		`CREATE INDEX IF NOT EXISTS idx_camera_investigations_status ON camera_investigations(status)`,
		`CREATE INDEX IF NOT EXISTS idx_camera_investigation_messages_inv ON camera_investigation_messages(investigation_id, seq)`,
		`CREATE INDEX IF NOT EXISTS idx_camera_events_ts ON camera_events(ts)`,
		`CREATE INDEX IF NOT EXISTS idx_camera_events_dvr ON camera_events(dvr_id)`,
		`CREATE INDEX IF NOT EXISTS idx_camera_events_op ON camera_events(op, ok)`,
		// ── Knowledge layer: operator-authored playbooks + external API tools
		// (CRUD in camera_playbooks.go / camera_apitools.go; schema stays here).
		`CREATE TABLE IF NOT EXISTS camera_playbooks (
			id TEXT PRIMARY KEY,
			site_id TEXT NOT NULL,
			dvr_id TEXT NOT NULL DEFAULT '',
			name TEXT NOT NULL,
			when_to_use TEXT NOT NULL DEFAULT '',
			instructions TEXT NOT NULL DEFAULT '',
			enabled INTEGER NOT NULL DEFAULT 1,
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS camera_playbook_media (
			id TEXT PRIMARY KEY,
			playbook_id TEXT NOT NULL,
			capture_token TEXT NOT NULL,
			note TEXT NOT NULL DEFAULT '',
			created_at TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS camera_api_tools (
			id TEXT PRIMARY KEY,
			site_id TEXT NOT NULL,
			name TEXT NOT NULL,
			description TEXT NOT NULL DEFAULT '',
			method TEXT NOT NULL DEFAULT 'GET',
			url_template TEXT NOT NULL DEFAULT '',
			headers_json TEXT NOT NULL DEFAULT '',
			auth_secret_enc TEXT NOT NULL DEFAULT '',
			body_template TEXT NOT NULL DEFAULT '',
			request_instructions TEXT NOT NULL DEFAULT '',
			response_instructions TEXT NOT NULL DEFAULT '',
			enabled INTEGER NOT NULL DEFAULT 1,
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL
		)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_camera_playbooks_site_name ON camera_playbooks(site_id, name)`,
		`CREATE INDEX IF NOT EXISTS idx_camera_playbook_media_pb ON camera_playbook_media(playbook_id)`,
		`CREATE INDEX IF NOT EXISTS idx_camera_playbook_media_token ON camera_playbook_media(capture_token)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_camera_api_tools_site_name ON camera_api_tools(site_id, name)`,
		// ── Avatars: registered people/vehicles/pets with approved reference imagery,
		// enrollment scans, and operator-reviewed candidates (camera_avatars.go /
		// camera_avatar_scan.go). Groups are avatars with is_group=1.
		`CREATE TABLE IF NOT EXISTS camera_avatars (
			id TEXT PRIMARY KEY,
			site_id TEXT NOT NULL,
			name TEXT NOT NULL,
			type TEXT NOT NULL DEFAULT 'human',
			is_group INTEGER NOT NULL DEFAULT 0,
			external_ref TEXT NOT NULL DEFAULT '',
			description TEXT NOT NULL DEFAULT '',
			dvr_ids TEXT NOT NULL DEFAULT '',
			enabled INTEGER NOT NULL DEFAULT 1,
			embedding BLOB,
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS camera_avatar_media (
			id TEXT PRIMARY KEY,
			avatar_id TEXT NOT NULL,
			capture_id TEXT NOT NULL,
			token TEXT NOT NULL,
			camera_id TEXT NOT NULL DEFAULT '',
			quality TEXT NOT NULL DEFAULT '',
			frame_ts TEXT NOT NULL DEFAULT '',
			bbox TEXT NOT NULL DEFAULT '',
			source TEXT NOT NULL DEFAULT 'scan',
			note TEXT NOT NULL DEFAULT '',
			match_confidence REAL NOT NULL DEFAULT 0,
			embedding BLOB,
			created_at TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS camera_avatar_scans (
			id TEXT PRIMARY KEY,
			avatar_id TEXT NOT NULL,
			site_id TEXT NOT NULL,
			camera_ids TEXT NOT NULL DEFAULT '',
			from_ts TEXT NOT NULL,
			to_ts TEXT NOT NULL,
			alias TEXT NOT NULL DEFAULT '',
			status TEXT NOT NULL DEFAULT 'queued',
			progress TEXT NOT NULL DEFAULT '',
			error TEXT NOT NULL DEFAULT '',
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS camera_avatar_candidates (
			id TEXT PRIMARY KEY,
			scan_id TEXT NOT NULL,
			avatar_id TEXT NOT NULL,
			camera_id TEXT NOT NULL,
			frame_ts TEXT NOT NULL,
			quality TEXT NOT NULL DEFAULT 'sub',
			annotated_token TEXT NOT NULL DEFAULT '',
			crop_token TEXT NOT NULL DEFAULT '',
			bbox TEXT NOT NULL DEFAULT '',
			match_kind TEXT NOT NULL DEFAULT 'vlm',
			vlm_confidence REAL NOT NULL DEFAULT 0,
			vlm_reason TEXT NOT NULL DEFAULT '',
			status TEXT NOT NULL DEFAULT 'pending',
			assigned_avatar_id TEXT NOT NULL DEFAULT '',
			note TEXT NOT NULL DEFAULT '',
			reviewed_at TEXT NOT NULL DEFAULT '',
			created_at TEXT NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_camera_avatars_site ON camera_avatars(site_id, enabled)`,
		`CREATE INDEX IF NOT EXISTS idx_camera_avatar_media_avatar ON camera_avatar_media(avatar_id)`,
		`CREATE INDEX IF NOT EXISTS idx_camera_avatar_scans_status ON camera_avatar_scans(status)`,
		`CREATE INDEX IF NOT EXISTS idx_camera_avatar_scans_avatar ON camera_avatar_scans(avatar_id, created_at)`,
		`CREATE INDEX IF NOT EXISTS idx_camera_avatar_candidates_scan ON camera_avatar_candidates(scan_id, status)`,
		`CREATE INDEX IF NOT EXISTS idx_camera_avatar_candidates_avatar ON camera_avatar_candidates(avatar_id, status)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_camera_avatar_candidates_uniq ON camera_avatar_candidates(scan_id, camera_id, frame_ts)`,
		// ── Multi-camera evidence video export (camera_export.go): a durable job
		// queue (cloned from the avatar-scan queue's shape) that stitches recorded
		// footage across cameras into sequential/grid/separate MP4s and delivers
		// permanent public S3 links. progress/outputs are JSON checkpoints so a
		// crash-requeued run resumes past finished chunks.
		`CREATE TABLE IF NOT EXISTS camera_exports (
			id TEXT PRIMARY KEY,
			site_id TEXT NOT NULL,
			investigation_id TEXT NOT NULL DEFAULT '',
			camera_ids TEXT NOT NULL,
			from_ts TEXT NOT NULL,
			to_ts TEXT NOT NULL,
			layout TEXT NOT NULL DEFAULT 'separate',
			quality TEXT NOT NULL DEFAULT 'main',
			status TEXT NOT NULL DEFAULT 'queued',
			progress TEXT NOT NULL DEFAULT '',
			outputs TEXT NOT NULL DEFAULT '',
			error TEXT NOT NULL DEFAULT '',
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_camera_exports_site ON camera_exports(site_id, created_at)`,
		`CREATE INDEX IF NOT EXISTS idx_camera_exports_status ON camera_exports(status)`,
		// ── Service API for Connect: bearer tokens (stored hashed) + per-site settle
		// webhooks (camera_serviceapi.go).
		`CREATE TABLE IF NOT EXISTS camera_service_tokens (
			id TEXT PRIMARY KEY,
			scope TEXT NOT NULL,
			site_id TEXT NOT NULL DEFAULT '',
			label TEXT NOT NULL DEFAULT '',
			token_hash TEXT NOT NULL,
			token_preview TEXT NOT NULL,
			enabled INTEGER NOT NULL DEFAULT 1,
			last_used_at TEXT NOT NULL DEFAULT '',
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS camera_site_callbacks (
			site_id TEXT PRIMARY KEY,
			url TEXT NOT NULL,
			secret_enc TEXT NOT NULL DEFAULT '',
			enabled INTEGER NOT NULL DEFAULT 1,
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_cam_svc_tokens_hash ON camera_service_tokens(token_hash, enabled)`,
		// ── Motion detection (camera_motion.go): every coalesced DVR motion episode
		// (queryable history for the AI) plus the per-(site,camera) arming config that
		// decides which episodes fire a real-time camera_motion webhook.
		`CREATE TABLE IF NOT EXISTS camera_motion_events (
			id TEXT PRIMARY KEY,
			site_id TEXT NOT NULL,
			camera_id TEXT NOT NULL,
			event_type TEXT NOT NULL DEFAULT '',
			started_at TEXT NOT NULL,
			ended_at TEXT NOT NULL DEFAULT '',
			created_at TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS camera_motion_arm (
			id TEXT PRIMARY KEY,
			site_id TEXT NOT NULL,
			camera_id TEXT NOT NULL,
			event_types TEXT NOT NULL DEFAULT '',
			cooldown_seconds INTEGER NOT NULL DEFAULT 0,
			enabled INTEGER NOT NULL DEFAULT 1,
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_camera_motion_events_site ON camera_motion_events(site_id, camera_id, started_at)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_camera_motion_arm_uniq ON camera_motion_arm(site_id, camera_id)`,
		// ── Person-identity layer (Person Identity v2 plan). Landed INERT ahead of
		// the continuous-identification engine (R2) and unknown clustering (R4) so
		// later phases are pure additive code. NEUTRALITY INVARIANT: these tables
		// hold observations and user-assigned labels only — no severity/threat/alert
		// semantics may ever be added here (see camera_neutrality_test.go).
		//
		// camera_sightings — one row per presence INTERVAL (modeled on
		// camera_motion_events episodes): who (avatar, cluster, or an unresolved
		// embedding), where (camera + area snapshot), when. best_crop_token is an
		// EPHEMERAL camera_captures token (reaped hourly) — embedding +
		// best_frame_ts + best_bbox keep every sighting re-hydratable from the S3
		// frame archive; roles are NEVER stored here (they resolve by JOIN against
		// camera_avatars at query time so re-labeling applies retroactively).
		`CREATE TABLE IF NOT EXISTS camera_sightings (
			id TEXT PRIMARY KEY,
			site_id TEXT NOT NULL,
			camera_id TEXT NOT NULL,
			subject_kind TEXT NOT NULL DEFAULT '',
			avatar_id TEXT NOT NULL DEFAULT '',
			cluster_id TEXT NOT NULL DEFAULT '',
			area TEXT NOT NULL DEFAULT '',
			started_at TEXT NOT NULL,
			ended_at TEXT NOT NULL DEFAULT '',
			last_seen_at TEXT NOT NULL DEFAULT '',
			frame_count INTEGER NOT NULL DEFAULT 1,
			best_score REAL NOT NULL DEFAULT 0,
			best_frame_ts TEXT NOT NULL DEFAULT '',
			best_bbox TEXT NOT NULL DEFAULT '',
			best_quality TEXT NOT NULL DEFAULT 'sub',
			best_crop_token TEXT NOT NULL DEFAULT '',
			embedding BLOB,
			created_at TEXT NOT NULL
		)`,
		// camera_sighting_clusters — recurring unknown faces grouped by embedding
		// similarity (exact centroid = normalize(sum_embedding/member_count)). No
		// confirmed-role column exists here BY DESIGN: a role is confirmed only at
		// promotion, onto the new avatar row.
		`CREATE TABLE IF NOT EXISTS camera_sighting_clusters (
			id TEXT PRIMARY KEY,
			site_id TEXT NOT NULL,
			status TEXT NOT NULL DEFAULT 'provisional',
			member_count INTEGER NOT NULL DEFAULT 0,
			window_count INTEGER NOT NULL DEFAULT 0,
			sum_embedding BLOB,
			centroid BLOB,
			first_seen_at TEXT NOT NULL DEFAULT '',
			last_seen_at TEXT NOT NULL DEFAULT '',
			best_crop_token TEXT NOT NULL DEFAULT '',
			best_score REAL NOT NULL DEFAULT 0,
			suggested_role TEXT NOT NULL DEFAULT '',
			suggested_role_confidence REAL NOT NULL DEFAULT 0,
			profile_json TEXT NOT NULL DEFAULT '',
			profile_updated_at TEXT NOT NULL DEFAULT '',
			promoted_avatar_id TEXT NOT NULL DEFAULT '',
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_camera_sightings_site ON camera_sightings(site_id, started_at)`,
		`CREATE INDEX IF NOT EXISTS idx_camera_sightings_avatar ON camera_sightings(avatar_id, started_at)`,
		`CREATE INDEX IF NOT EXISTS idx_camera_sightings_cluster ON camera_sightings(cluster_id, started_at)`,
		`CREATE INDEX IF NOT EXISTS idx_camera_sightings_camera ON camera_sightings(camera_id, started_at)`,
		`CREATE INDEX IF NOT EXISTS idx_camera_sighting_clusters_site ON camera_sighting_clusters(site_id, status)`,
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
	// Operator knowledge columns (knowledge layer): per-DVR AI instructions and
	// per-camera operator notes. `cameras.ai_location` stays AI-owned (re-describe
	// clobbers it, camera_describe.go), so operator text gets its own column.
	if err := ensureSQLiteColumn(db, "camera_dvrs", "ai_instructions", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	if err := ensureSQLiteColumn(db, "cameras", "notes", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	// Public S3 URL for large evidence (recorded clips uploaded to object storage so
	// they can be delivered as a direct link instead of a proxy-served download).
	if err := ensureSQLiteColumn(db, "camera_captures", "s3_url", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	// Person-identity layer (Person Identity v2 plan): per-site role vocabulary +
	// per-avatar role/profile columns. The confirmed role trio is USER-owned
	// (written only by the role-update handlers, which stamp role_confirmed_at);
	// the suggested/profile columns are AI-owned (written only by the profile
	// builder, camera_profile.go). Disjoint write paths make provenance
	// structural: role != '' ⇔ the operator confirmed it.
	if err := ensureSQLiteColumn(db, "camera_sites", "policy_json", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	for _, col := range [][2]string{
		{"role", "TEXT NOT NULL DEFAULT ''"},
		{"role_note", "TEXT NOT NULL DEFAULT ''"},
		{"role_confirmed_at", "TEXT NOT NULL DEFAULT ''"},
		{"suggested_role", "TEXT NOT NULL DEFAULT ''"},
		{"suggested_role_confidence", "REAL NOT NULL DEFAULT 0"},
		{"profile_json", "TEXT NOT NULL DEFAULT ''"},
		{"profile_updated_at", "TEXT NOT NULL DEFAULT ''"},
		{"profile_status", "TEXT NOT NULL DEFAULT ''"},
		{"profile_error", "TEXT NOT NULL DEFAULT ''"},
	} {
		if err := ensureSQLiteColumn(db, "camera_avatars", col[0], col[1]); err != nil {
			return err
		}
	}
	// Sub-agent delegation + live timeline (WS3/WS4). parent_id links a delegated
	// sub-investigation to its lead run ('' = top-level); view_token is the
	// capability id for the public read-only timeline page. stream_progress opts a
	// site's callback into the coalesced progress webhook (default 0 → existing
	// Connect callbacks see zero new traffic until they opt in).
	if err := ensureSQLiteColumn(db, "camera_investigations", "parent_id", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	if err := ensureSQLiteColumn(db, "camera_investigations", "view_token", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	if err := ensureSQLiteColumn(db, "camera_site_callbacks", "stream_progress", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	// Index created AFTER the column exists (the stmts block above ran before
	// parent_id was added), so a pre-WS3 database still gets the child lookup index.
	if _, err := db.Exec(`CREATE INDEX IF NOT EXISTS idx_camera_investigations_parent ON camera_investigations(parent_id)`); err != nil {
		return err
	}
	// Backfill: every pre-WS4 investigation needs a distinct view_token so its
	// timeline page can be linked. One fresh token per row (a shared default would
	// let one page's link authorize another's media). Idempotent — a re-run finds
	// no blank tokens left.
	if err := backfillCameraInvestigationViewTokens(db); err != nil {
		return err
	}
	return nil
}

// backfillCameraInvestigationViewTokens assigns a fresh capability token to every
// camera_investigations row still carrying the empty default (rows created before
// view_token existed). Each row gets its own randToken(16) — never a shared value
// — because a view_token is what authorizes the public timeline page and its
// media, so two rows sharing one would cross-authorize each other's evidence.
func backfillCameraInvestigationViewTokens(db *sql.DB) error {
	rows, err := db.Query(`SELECT id FROM camera_investigations WHERE view_token = ''`)
	if err != nil {
		return err
	}
	var ids []string
	for rows.Next() {
		var id string
		if serr := rows.Scan(&id); serr != nil {
			rows.Close()
			return serr
		}
		ids = append(ids, id)
	}
	if cerr := rows.Err(); cerr != nil {
		rows.Close()
		return cerr
	}
	rows.Close()
	for _, id := range ids {
		if _, err := db.Exec(`UPDATE camera_investigations SET view_token = ? WHERE id = ? AND view_token = ''`, randToken(16), id); err != nil {
			return err
		}
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
		(id, name, description, analysis_alias, analysis_json, analysis_status, last_analyzed_at, policy_json, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		s.ID, s.Name, s.Description, s.AnalysisAlias, s.AnalysisJSON, s.AnalysisStatus, s.LastAnalyzedAt, s.PolicyJSON, now, now)
	return s.ID, err
}

func scanCameraSite(s rowScanner) (camSite, error) {
	var v camSite
	err := s.Scan(&v.ID, &v.Name, &v.Description, &v.AnalysisAlias, &v.AnalysisJSON,
		&v.AnalysisStatus, &v.LastAnalyzedAt, &v.PolicyJSON, &v.CreatedAt, &v.UpdatedAt)
	return v, err
}

const camSiteCols = `id, name, description, analysis_alias, analysis_json, analysis_status, last_analyzed_at, policy_json, created_at, updated_at`

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

// updateCameraSitePolicy writes ONLY policy_json (dedicated setter so unrelated
// site updates can never clobber the role vocabulary with a stale row).
func updateCameraSitePolicy(db *sql.DB, id, policyJSON string) error {
	_, err := db.Exec(`UPDATE camera_sites SET policy_json = ?, updated_at = ? WHERE id = ?`,
		policyJSON, nowRFC3339(), id)
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
		 timezone, ai_instructions, channels_discovered, enabled, last_discovered_at, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0, ?, '', ?, ?)`,
		dvr.ID, dvr.SiteID, dvr.Name, strings.ToLower(strings.TrimSpace(dvr.Brand)), dvr.Host, dvr.Port, dvr.HTTPPort,
		dvr.Username, enc, maskSecret(dvr.Password), dvr.Timezone, dvr.AIInstructions, boolToInt(dvr.Enabled), now, now)
	return dvr.ID, err
}

// getCameraDVR returns a DVR with its password DECRYPTED (backend use only).
func getCameraDVR(db *sql.DB, cfg config, id string) (CamDVR, error) {
	var d CamDVR
	var enc string
	var enabled int
	err := db.QueryRow(`SELECT id, site_id, name, brand, host, port, http_port, username, password_enc, timezone, ai_instructions, enabled
		FROM camera_dvrs WHERE id = ?`, id).
		Scan(&d.ID, &d.SiteID, &d.Name, &d.Brand, &d.Host, &d.Port, &d.HTTPPort, &d.Username, &enc, &d.Timezone, &d.AIInstructions, &enabled)
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
	const q = `SELECT id, site_id, name, brand, host, port, http_port, username, password_enc, timezone, ai_instructions, enabled FROM camera_dvrs`
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
			&d.Username, &enc, &d.Timezone, &d.AIInstructions, &enabled); err != nil {
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
	const cols = `id, site_id, name, brand, host, port, http_port, username, password_preview, timezone, ai_instructions, channels_discovered, enabled, last_discovered_at, created_at, updated_at`
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
			&v.Username, &v.PasswordPreview, &v.Timezone, &v.AIInstructions, &v.ChannelsDiscovered, &enabled,
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
			username = ?, password_enc = ?, password_preview = ?, timezone = ?, ai_instructions = ?, enabled = ?, updated_at = ? WHERE id = ?`,
			dvr.Name, strings.ToLower(strings.TrimSpace(dvr.Brand)), dvr.Host, dvr.Port, dvr.HTTPPort,
			dvr.Username, enc, maskSecret(dvr.Password), dvr.Timezone, dvr.AIInstructions, boolToInt(dvr.Enabled), now, dvr.ID)
		return err
	}
	_, err := db.Exec(`UPDATE camera_dvrs SET name = ?, brand = ?, host = ?, port = ?, http_port = ?,
		username = ?, timezone = ?, ai_instructions = ?, enabled = ?, updated_at = ? WHERE id = ?`,
		dvr.Name, strings.ToLower(strings.TrimSpace(dvr.Brand)), dvr.Host, dvr.Port, dvr.HTTPPort,
		dvr.Username, dvr.Timezone, dvr.AIInstructions, boolToInt(dvr.Enabled), now, dvr.ID)
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

const camCameraCols = `id, site_id, dvr_id, channel, name, area, ai_description, ai_location, notes, enabled, disabled_reason, snapshot_capture_id, caps, created_at, updated_at`

func scanCamera(s rowScanner) (camera, error) {
	var c camera
	var enabled int
	err := s.Scan(&c.ID, &c.SiteID, &c.DVRID, &c.Channel, &c.Name, &c.Area, &c.AIDescription,
		&c.AILocation, &c.Notes, &enabled, &c.DisabledReason, &c.SnapshotCaptureID, &c.Caps, &c.CreatedAt, &c.UpdatedAt)
	c.Enabled = enabled != 0
	return c, err
}

func insertCamera(db *sql.DB, c camera) (string, error) {
	if c.ID == "" {
		c.ID = randomID("cam")
	}
	now := nowRFC3339()
	_, err := db.Exec(`INSERT INTO cameras
		(id, site_id, dvr_id, channel, name, area, ai_description, ai_location, notes, enabled, disabled_reason, snapshot_capture_id, caps, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		c.ID, c.SiteID, c.DVRID, c.Channel, c.Name, c.Area, c.AIDescription, c.AILocation, c.Notes,
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
	_, err := db.Exec(`UPDATE cameras SET name = ?, area = ?, ai_description = ?, ai_location = ?, notes = ?,
		enabled = ?, disabled_reason = ?, snapshot_capture_id = ?, caps = ?, updated_at = ? WHERE id = ?`,
		c.Name, c.Area, c.AIDescription, c.AILocation, c.Notes, boolToInt(c.Enabled), c.DisabledReason,
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

// listCameraWatchRunsBySite lists a site's watch runs newest-first, across every
// watch — the service API's site-wide run browser (the admin listCameraWatchRuns
// is per-watch or global). Runs carry site_id directly, so no join is needed.
func listCameraWatchRunsBySite(db *sql.DB, siteID string, limit int) ([]camWatchRun, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := db.Query(`SELECT `+camRunCols+` FROM camera_watch_runs WHERE site_id = ? ORDER BY started_at DESC LIMIT ?`, siteID, limit)
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

const camCaptureCols = `id, site_id, camera_id, watch_run_id, kind, quality, token, path, content_type, width, height, bytes, from_ts, to_ts, created_at, expires_at, s3_url`

func scanCapture(s rowScanner) (camCapture, error) {
	var c camCapture
	err := s.Scan(&c.ID, &c.SiteID, &c.CameraID, &c.WatchRunID, &c.Kind, &c.Quality, &c.Token, &c.Path,
		&c.ContentType, &c.Width, &c.Height, &c.Bytes, &c.FromTS, &c.ToTS, &c.CreatedAt, &c.ExpiresAt, &c.S3URL)
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
		(id, site_id, camera_id, watch_run_id, kind, quality, token, path, content_type, width, height, bytes, from_ts, to_ts, created_at, expires_at, s3_url)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		c.ID, c.SiteID, c.CameraID, c.WatchRunID, c.Kind, c.Quality, c.Token, c.Path, c.ContentType,
		c.Width, c.Height, c.Bytes, c.FromTS, c.ToTS, c.CreatedAt, c.ExpiresAt, c.S3URL)
	return c.ID, err
}

// setCameraCaptureS3URL records the public object-storage URL for a capture (a
// clip uploaded to S3) so the settle payload can hand out a direct link.
func setCameraCaptureS3URL(db *sql.DB, token, url string) error {
	_, err := db.Exec(`UPDATE camera_captures SET s3_url = ? WHERE token = ?`, url, token)
	return err
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

// listCameraCapturesBySite lists a site's captures newest-first — the service
// API's site-wide recent-captures browser (the admin listCameraCaptures is
// run-scoped). Captures carry site_id directly, so no join is needed. Optional
// runID / cameraID narrow the result; both are already confined to the site by
// the site_id predicate, so a foreign id simply yields no rows.
func listCameraCapturesBySite(db *sql.DB, siteID, runID, cameraID string, limit int) ([]camCapture, error) {
	if limit <= 0 {
		limit = 50
	}
	q := `SELECT ` + camCaptureCols + ` FROM camera_captures WHERE site_id = ?`
	args := []any{siteID}
	if r := strings.TrimSpace(runID); r != "" {
		q += ` AND watch_run_id = ?`
		args = append(args, r)
	}
	if c := strings.TrimSpace(cameraID); c != "" {
		q += ` AND camera_id = ?`
		args = append(args, c)
	}
	q += ` ORDER BY created_at DESC LIMIT ?`
	args = append(args, limit)
	rows, err := db.Query(q, args...)
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

// pinCameraCapture makes a served capture permanent: a blank expires_at is
// exempt from every reaper path (deleteExpiredCameraCaptures / reapCameraMedia
// filter on a non-blank expires_at, and handleCameraMedia serves a blank one
// forever), so pinned reference imagery (playbook refs, approved avatar refs)
// is never reaped. Undone by camReleaseCaptureIfUnreferenced (camera_playbooks.go).
func pinCameraCapture(db *sql.DB, token string) error {
	_, err := db.Exec(`UPDATE camera_captures SET expires_at = '' WHERE token = ?`, token)
	return err
}

// camCaptureReferenced reports whether any pinned-reference row (playbook media
// OR avatar media) still points at this capture token. Both features pin the
// same camera_captures token namespace, so releasing/deleting a capture must
// consult BOTH tables — otherwise a playbook detach un-pins an avatar reference,
// or an avatar delete destroys a playbook reference's shared image. Fails safe
// (returns true) on query error so a shared capture is never destroyed.
func camCaptureReferenced(db *sql.DB, token string) bool {
	token = strings.TrimSpace(token)
	if token == "" {
		return false
	}
	var n int
	err := db.QueryRow(`SELECT
		(SELECT COUNT(*) FROM camera_playbook_media WHERE capture_token = ?) +
		(SELECT COUNT(*) FROM camera_avatar_media WHERE token = ?)`, token, token).Scan(&n)
	if err != nil {
		return true
	}
	return n > 0
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

// ─────────────────────────── camera_motion_events ───────────────────────────
//
// One coalesced motion EPISODE per row (camera_motion.go collapses the DVR's
// repeated active heartbeats into a single episode: started_at on first active,
// ended_at filled when the episode closes on an inactive event or an idle gap).
// started_at/ended_at are stored as UTC RFC3339 so lexicographic range filters
// hold across DVRs on different local offsets; callers render them in the site
// timezone. Written for EVERY camera regardless of arming — this is the
// queryable history the motion_search investigate tool reads.

// camMotionEvent is an in-memory view of a camera_motion_events row.
type camMotionEvent struct {
	ID        string
	SiteID    string
	CameraID  string
	EventType string
	StartedAt string // UTC RFC3339
	EndedAt   string // UTC RFC3339, "" while the episode is still open
	CreatedAt string
}

const camMotionEventCols = `id, site_id, camera_id, event_type, started_at, ended_at, created_at`

func scanMotionEvent(s rowScanner) (camMotionEvent, error) {
	var e camMotionEvent
	err := s.Scan(&e.ID, &e.SiteID, &e.CameraID, &e.EventType, &e.StartedAt, &e.EndedAt, &e.CreatedAt)
	return e, err
}

// insertCameraMotionEvent opens a new episode (ended_at blank) and returns its id
// so the caller can updateMotionEventEnd it when the episode closes.
func insertCameraMotionEvent(db *sql.DB, siteID, cameraID, eventType, startedAt string) (string, error) {
	id := randomID("mev")
	_, err := db.Exec(`INSERT INTO camera_motion_events
		(id, site_id, camera_id, event_type, started_at, ended_at, created_at)
		VALUES (?, ?, ?, ?, ?, '', ?)`,
		id, siteID, cameraID, eventType, startedAt, nowRFC3339())
	return id, err
}

// updateMotionEventEnd stamps an episode's ended_at (episode close).
func updateMotionEventEnd(db *sql.DB, id, endedAt string) error {
	_, err := db.Exec(`UPDATE camera_motion_events SET ended_at = ? WHERE id = ?`, endedAt, id)
	return err
}

// camMotionEventQueryLimit caps how many episodes a single motion query returns.
// The caller is told (truncated=true) when the cap is hit so a wide/busy-site
// window is never SILENTLY under-reported — the tool/API surface a "narrow the
// window" note rather than presenting a partial answer as complete.
const camMotionEventQueryLimit = 8000

// listCameraMotionEvents returns motion episodes for a site, oldest first,
// optionally filtered by a set of camera ids, a [fromTS,toTS] started_at window
// (both UTC RFC3339; "" disables that bound), and an exact event_type. Always
// scoped to siteID so a leaked/mismatched camera id can never cross sites. The
// returned truncated is true when more episodes matched than the cap returned
// (results are the OLDEST camMotionEventQueryLimit — the caller should narrow).
func listCameraMotionEvents(db *sql.DB, siteID string, cameraIDs []string, fromTS, toTS, eventType string) ([]camMotionEvent, bool, error) {
	q := `SELECT ` + camMotionEventCols + ` FROM camera_motion_events WHERE site_id = ?`
	args := []any{siteID}
	if len(cameraIDs) > 0 {
		q += ` AND camera_id IN (` + strings.TrimRight(strings.Repeat("?,", len(cameraIDs)), ",") + `)`
		for _, id := range cameraIDs {
			args = append(args, id)
		}
	}
	if strings.TrimSpace(fromTS) != "" {
		q += ` AND started_at >= ?`
		args = append(args, fromTS)
	}
	if strings.TrimSpace(toTS) != "" {
		q += ` AND started_at <= ?`
		args = append(args, toTS)
	}
	if strings.TrimSpace(eventType) != "" {
		q += ` AND event_type = ?`
		args = append(args, eventType)
	}
	// Fetch one past the cap so an exact-cap match is distinguishable from a real
	// overflow (truncated only when a genuine extra row exists).
	q += ` ORDER BY started_at LIMIT ?`
	args = append(args, camMotionEventQueryLimit+1)
	rows, err := db.Query(q, args...)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	var out []camMotionEvent
	for rows.Next() {
		e, serr := scanMotionEvent(rows)
		if serr != nil {
			return nil, false, serr
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	truncated := false
	if len(out) > camMotionEventQueryLimit {
		out = out[:camMotionEventQueryLimit]
		truncated = true
	}
	return out, truncated, nil
}

// deleteOldCameraMotionEvents prunes episodes created before cutoff (server-clock
// created_at, so a skewed DVR clock can't defeat retention). Returns rows deleted.
func deleteOldCameraMotionEvents(db *sql.DB, cutoffRFC3339 string) (int64, error) {
	res, err := db.Exec(`DELETE FROM camera_motion_events WHERE created_at < ?`, cutoffRFC3339)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// ─────────────────────────── camera_motion_arm ───────────────────────────
//
// Per-(site,camera) arming for real-time firing: which event types wake a
// camera_motion webhook and the per-camera cooldown between fires. Mirrors the
// getCameraSiteCallback/setCameraSiteCallback upsert shape. UNIQUE(site_id,
// camera_id) makes upsertCameraMotionArm an update-or-insert.

// camMotionArm is an in-memory view of a camera_motion_arm row. EventTypes is a
// JSON array string (empty/"[]" means "any event type").
type camMotionArm struct {
	ID              string
	SiteID          string
	CameraID        string
	EventTypes      string // JSON array
	CooldownSeconds int
	Enabled         bool
	CreatedAt       string
	UpdatedAt       string
}

const camMotionArmCols = `id, site_id, camera_id, event_types, cooldown_seconds, enabled, created_at, updated_at`

func scanMotionArm(s rowScanner) (camMotionArm, error) {
	var a camMotionArm
	var enabled int
	err := s.Scan(&a.ID, &a.SiteID, &a.CameraID, &a.EventTypes, &a.CooldownSeconds, &enabled, &a.CreatedAt, &a.UpdatedAt)
	a.Enabled = enabled != 0
	return a, err
}

// upsertCameraMotionArm inserts or updates the arm row for (siteID, cameraID).
func upsertCameraMotionArm(db *sql.DB, siteID, cameraID, eventTypesJSON string, cooldownSeconds int, enabled bool) error {
	now := nowRFC3339()
	res, err := db.Exec(`UPDATE camera_motion_arm
		SET event_types = ?, cooldown_seconds = ?, enabled = ?, updated_at = ?
		WHERE site_id = ? AND camera_id = ?`,
		eventTypesJSON, cooldownSeconds, boolToInt(enabled), now, siteID, cameraID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		return nil
	}
	_, err = db.Exec(`INSERT INTO camera_motion_arm
		(id, site_id, camera_id, event_types, cooldown_seconds, enabled, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		randomID("marm"), siteID, cameraID, eventTypesJSON, cooldownSeconds, boolToInt(enabled), now, now)
	return err
}

// listCameraMotionArm returns every arm row for a site.
func listCameraMotionArm(db *sql.DB, siteID string) ([]camMotionArm, error) {
	rows, err := db.Query(`SELECT `+camMotionArmCols+` FROM camera_motion_arm WHERE site_id = ? ORDER BY created_at`, siteID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []camMotionArm
	for rows.Next() {
		a, serr := scanMotionArm(rows)
		if serr != nil {
			return nil, serr
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// getCameraMotionArm loads the arm row for (siteID, cameraID); sql.ErrNoRows when
// the camera was never armed.
func getCameraMotionArm(db *sql.DB, siteID, cameraID string) (camMotionArm, error) {
	return scanMotionArm(db.QueryRow(`SELECT `+camMotionArmCols+`
		FROM camera_motion_arm WHERE site_id = ? AND camera_id = ?`, siteID, cameraID))
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

const camInvestigationCols = `id, site_id, title, question, alias, status, parent_id, view_token, created_at, updated_at`

func scanInvestigation(s rowScanner) (camInvestigation, error) {
	var v camInvestigation
	err := s.Scan(&v.ID, &v.SiteID, &v.Title, &v.Question, &v.Alias, &v.Status, &v.ParentID, &v.ViewToken, &v.CreatedAt, &v.UpdatedAt)
	return v, err
}

func insertCameraInvestigation(db *sql.DB, inv camInvestigation) (string, error) {
	if inv.ID == "" {
		inv.ID = randomID("inv")
	}
	if inv.Status == "" {
		inv.Status = "active"
	}
	// Every row owns a distinct view_token so its public timeline page (and the
	// media that page authorizes) is scoped to exactly one investigation.
	if inv.ViewToken == "" {
		inv.ViewToken = randToken(16)
	}
	now := nowRFC3339()
	if inv.CreatedAt == "" {
		inv.CreatedAt = now
	}
	if inv.UpdatedAt == "" {
		inv.UpdatedAt = now
	}
	_, err := db.Exec(`INSERT INTO camera_investigations
		(id, site_id, title, question, alias, status, parent_id, view_token, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		inv.ID, inv.SiteID, inv.Title, inv.Question, inv.Alias, inv.Status, inv.ParentID, inv.ViewToken, inv.CreatedAt, inv.UpdatedAt)
	return inv.ID, err
}

func getCameraInvestigation(db *sql.DB, id string) (camInvestigation, error) {
	return scanInvestigation(db.QueryRow(`SELECT `+camInvestigationCols+` FROM camera_investigations WHERE id = ?`, id))
}

// getCameraInvestigationByViewToken resolves the top-level (or child) investigation
// a public timeline page belongs to. The `view_token != ”` guard is defensive: a
// bare-default token must never match, so a caller can't pass "" to escalate into
// the first un-backfilled row.
func getCameraInvestigationByViewToken(db *sql.DB, token string) (camInvestigation, error) {
	return scanInvestigation(db.QueryRow(`SELECT `+camInvestigationCols+
		` FROM camera_investigations WHERE view_token = ? AND view_token != ''`, token))
}

// listCameraInvestigations lists a site's TOP-LEVEL investigations newest-first
// (siteID=="" spans every site). Delegated sub-investigations (parent_id != ”)
// are excluded — they surface only nested under their lead run's transcript, never
// as standalone rows in a site's list.
func listCameraInvestigations(db *sql.DB, siteID string) ([]camInvestigation, error) {
	var (
		rows *sql.Rows
		err  error
	)
	if strings.TrimSpace(siteID) == "" {
		rows, err = db.Query(`SELECT ` + camInvestigationCols + ` FROM camera_investigations WHERE parent_id = '' ORDER BY updated_at DESC`)
	} else {
		rows, err = db.Query(`SELECT `+camInvestigationCols+` FROM camera_investigations WHERE site_id = ? AND parent_id = '' ORDER BY updated_at DESC`, siteID)
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

// listCameraInvestigationChildren returns a lead run's delegated sub-investigations
// in creation order (the order they were fanned out), for the timeline page's
// collapsible child sections and the admin get response.
func listCameraInvestigationChildren(db *sql.DB, parentID string) ([]camInvestigation, error) {
	if strings.TrimSpace(parentID) == "" {
		return nil, nil
	}
	rows, err := db.Query(`SELECT `+camInvestigationCols+
		` FROM camera_investigations WHERE parent_id = ? ORDER BY created_at ASC`, parentID)
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

// requeueCameraInvestigationFromRun atomically returns a still-running investigation
// to "queued" so the worker (or the inline drain) re-claims and RESUMES it — the
// never-die counterpart to a terminal camStopInvestigation, driven by
// camDeferInvestigation when a pass runs out of budget or hits an analysis wall. It
// is a compare-and-swap (mirrors claimCameraInvestigation): the flip lands only
// while the row is genuinely "running", so a lost race with the stale-reaper or a
// concurrent terminal write can never double-transition it. Returns true only when
// THIS caller performed the flip.
func requeueCameraInvestigationFromRun(db *sql.DB, id string) (bool, error) {
	res, err := db.Exec(`UPDATE camera_investigations SET status = 'queued', updated_at = ?
		WHERE id = ? AND status = 'running'`, nowRFC3339(), id)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n == 1, nil
}

// cancelCameraInvestigation atomically closes a settled/queued investigation via a
// compare-and-swap over the cancellable states (queued/awaiting_operator/answered/
// exhausted), returning true only when THIS caller performed the close. 'running' is
// deliberately excluded — the CAS never touches a row a worker owns, so cancelling a
// live pass is out of scope — and 'closed' is excluded so a concurrent close is a lost
// race (RowsAffected==0), not a double-close. Mirrors claimCameraInvestigation /
// requeueCameraInvestigationFromRun.
func cancelCameraInvestigation(db *sql.DB, id string) (bool, error) {
	res, err := db.Exec(`UPDATE camera_investigations SET status = 'closed', updated_at = ?
		WHERE id = ? AND status IN ('queued','awaiting_operator','answered','exhausted')`, nowRFC3339(), id)
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
	// Only TOP-LEVEL rows requeue: a delegated sub-investigation (parent_id != '')
	// runs in-process inside its lead run's delegate turn, NEVER via the queue, so
	// flipping one to "queued" would strand it (no worker claims children). Also
	// catch legacy "active" rows: the pre-queue build set that status while a
	// synchronous run was in flight, so after upgrading such a row is orphaned too
	// (the new code never writes "active"). Resumable, so re-running is safe.
	res, err := db.Exec(`UPDATE camera_investigations SET status = 'queued', updated_at = ?
		WHERE status IN ('running', 'active') AND parent_id = ''`, nowRFC3339())
	if err != nil {
		return 0, err
	}
	// An orphaned running CHILD (its lead run's goroutine died with the process)
	// can't be resumed on its own — the parent's delegate turn owns the WaitGroup
	// — so terminalize it to "exhausted" instead of requeuing. The parent, once
	// requeued and re-run, re-delegates fresh children.
	if _, cerr := db.Exec(`UPDATE camera_investigations SET status = 'exhausted', updated_at = ?
		WHERE status IN ('running', 'active') AND parent_id != ''`, nowRFC3339()); cerr != nil {
		return 0, cerr
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
	// Top-level rows only (parent_id = ''): a delegated child is bounded by its
	// parent's budget and its lead run's WaitGroup, never the queue's stale reaper.
	res, err := db.Exec(`UPDATE camera_investigations SET status = 'queued', updated_at = ?
		WHERE status = 'running' AND parent_id = '' AND updated_at < ?`, nowRFC3339(), cutoff)
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
	if m.Seq > 0 {
		// Caller pinned an explicit seq — insert it verbatim.
		if _, err := db.Exec(`INSERT INTO camera_investigation_messages
			(id, investigation_id, seq, role, content, tool_name, tool_args, media_json, fetches, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			m.ID, m.InvestigationID, m.Seq, m.Role, m.Content, m.ToolName, m.ToolArgs, m.MediaJSON, m.Fetches, m.CreatedAt); err != nil {
			return "", err
		}
	} else {
		// Auto-assign the next seq ATOMICALLY by folding MAX(seq)+1 into the INSERT so
		// the read and the write execute inside one write-locked statement. A separate
		// `SELECT MAX(seq)` then `INSERT` races two writers into a DUPLICATE seq under
		// WAL — the live run loop and a detached evidence_export goroutine append to the
		// same transcript through separate *sql.DB handles, and only the INSERTs (not the
		// MAX reads) serialize. SQLite runs one writer at a time, so a single INSERT…SELECT
		// sees every prior committed row and cannot collide.
		if _, err := db.Exec(`INSERT INTO camera_investigation_messages
			(id, investigation_id, seq, role, content, tool_name, tool_args, media_json, fetches, created_at)
			SELECT ?, ?, COALESCE(MAX(seq), 0) + 1, ?, ?, ?, ?, ?, ?, ?
			FROM camera_investigation_messages WHERE investigation_id = ?`,
			m.ID, m.InvestigationID, m.Role, m.Content, m.ToolName, m.ToolArgs, m.MediaJSON, m.Fetches, m.CreatedAt, m.InvestigationID); err != nil {
			return "", err
		}
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
