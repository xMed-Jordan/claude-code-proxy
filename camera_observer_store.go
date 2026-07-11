package main

// camera_observer_store.go — typed CRUD for the activity-log ("observer")
// tables: camera_log_streams (the per-site periodic journal configs),
// camera_log_entries (one journal entry per cycle), camera_log_hours (hourly
// rollups) and camera_log_days (daily reports). Schema lives in
// migrateCameraDB (camera_store.go); conventions match the rest of the store —
// ids via randomID(prefix), RFC3339 text timestamps, bools as INTEGER, `?`
// placeholders.
//
// SYNC CURSOR: all three log tables carry a `seq` column stamped from the
// shared camera_sync_counter inside the writing transaction (nextCamSyncSeq).
// Connect pull-syncs with `WHERE site_id=? AND seq>cursor ORDER BY seq`
// (listLog*AfterSeq) and upserts by id, so:
//   - entry inserts stamp seq exactly once (entries are immutable);
//   - hour/day claim-inserts stamp seq, and their terminal updates
//     (finishLogHour/finishLogDay — pending→done/empty/error, and every daily
//     regenerate) RE-stamp it, making the mutated row reappear past any cursor
//     Connect already held;
//   - pending hour/day rows are hidden from the sync queries (a claim is not
//     content), so Connect only ever archives terminal rollups;
//   - the counter is global (one sequence across all sites and tables), so a
//     per-site cursor sees harmless gaps but never misses or repeats a row.
// Counter transactions stay tiny — stamp + one write, never enclosing model
// calls or S3 fetches — and the counter row is never pruned, so seq never
// regresses even after retention deletes.

import (
	"database/sql"
	"errors"
	"strings"
)

// ─────────────────────────── row types ───────────────────────────

// logStream is one activity-log stream: which cameras to journal, how often,
// and when the daily report goes out. camera_ids uses the watch encoding (JSON
// string array, camParseIDArray; empty = every enabled camera on the site);
// active_hours uses the watch spec ("HH:MM-HH:MM,..."); last_window_to is the
// UTC continuity cursor the next cycle resumes from.
type logStream struct {
	ID               string
	SiteID           string
	Name             string
	CameraIDs        string // JSON array (watch encoding)
	IntervalMinutes  int    // 1..15, default 5
	FrameStepSeconds int    // 10..interval*60, default 30
	AnalysisAlias    string
	ActiveHours      string // watch spec, "" = always
	MotionGate       bool
	DailyReportTime  string // "HH:MM" site-local, default "20:00"
	Enabled          bool
	NextRunAt        string
	LastRunAt        string
	LastWindowTo     string // UTC RFC3339 continuity cursor
	LastError        string
	CreatedAt        string
	UpdatedAt        string
}

// logEntry is one journal entry covering the [FromTS,ToTS) UTC window. Report
// is always English (the entry is the AI's own continuity memory); rollups
// translate into the site's report language. MediaTokens/DetailTokens are JSON
// arrays of capture tokens (composite sheets / drill-down frames) that ride
// normal media retention — old entries lose thumbnails but keep text.
type logEntry struct {
	ID           string
	Seq          int64
	StreamID     string
	SiteID       string
	FromTS       string // UTC RFC3339, window is [FromTS,ToTS)
	ToTS         string
	CameraIDs    string // JSON array — the cameras actually sampled
	Headline     string
	Report       string // always English
	MotionGated  bool
	Frames       int
	MediaTokens  string // JSON array of sheet capture tokens
	DetailTokens string // JSON array of drill-down capture tokens
	Alias        string
	Error        string // e.g. "s3 misses: 3", "salvaged_raw"
	CreatedAt    string
}

// logHour is one hourly rollup. HourStart is the UTC instant of the SITE-LOCAL
// hour boundary; TZ records the zone that boundary was computed in for audit.
// status: pending (claimed, rollup in flight) | done | empty | error.
type logHour struct {
	ID         string
	Seq        int64
	StreamID   string
	SiteID     string
	HourStart  string // UTC RFC3339 of the site-local hour boundary
	TZ         string
	Summary    string // report language
	Language   string
	EntryCount int
	GatedCount int
	Status     string // pending | done | empty | error
	Error      string
	CreatedAt  string
}

// logDay is one daily report. Day is the site-local calendar date
// ("2006-01-02"); ViewToken is the capability token for the public share page
// (/camera/reports/<view_token>) — minted at claim and preserved across
// regeneration so an already-shared link keeps working.
type logDay struct {
	ID          string
	Seq         int64
	StreamID    string
	SiteID      string
	Day         string // site-local "2006-01-02"
	FromTS      string // UTC RFC3339
	ToTS        string
	Headline    string
	Report      string // report language
	Language    string
	HoursUsed   int
	EntriesUsed int
	ViewToken   string
	Status      string // pending | done | empty | error
	Error       string
	NotifiedAt  string
	CreatedAt   string
}

// ─────────────────────────── sync counter ───────────────────────────

// nextCamSyncSeq returns the next value of the shared camera_sync_counter
// INSIDE the caller's transaction — upsert-then-increment-then-select, no
// RETURNING dependency. Callers that abandon the write (e.g. a lost hour/day
// claim) roll the transaction back, which also returns the counter value, so
// only committed writes consume sequence numbers.
func nextCamSyncSeq(tx *sql.Tx) (int64, error) {
	if _, err := tx.Exec(`INSERT OR IGNORE INTO camera_sync_counter (id, val) VALUES (1, 0)`); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(`UPDATE camera_sync_counter SET val = val + 1 WHERE id = 1`); err != nil {
		return 0, err
	}
	var v int64
	err := tx.QueryRow(`SELECT val FROM camera_sync_counter WHERE id = 1`).Scan(&v)
	return v, err
}

// camSyncMaxSeq reads the counter's current value (the max_seq the sync route
// reports so Connect knows when it has caught up). A database that has never
// stamped a row simply has no counter row yet → 0.
func camSyncMaxSeq(db *sql.DB) (int64, error) {
	var v int64
	err := db.QueryRow(`SELECT val FROM camera_sync_counter WHERE id = 1`).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return v, err
}

// ─────────────────────────── camera_log_streams ───────────────────────────

const camLogStreamCols = `id, site_id, name, camera_ids, interval_minutes, frame_step_seconds, analysis_alias, active_hours, motion_gate, daily_report_time, enabled, next_run_at, last_run_at, last_window_to, last_error, created_at, updated_at`

func scanLogStream(s rowScanner) (logStream, error) {
	var v logStream
	var motionGate, enabled int
	err := s.Scan(&v.ID, &v.SiteID, &v.Name, &v.CameraIDs, &v.IntervalMinutes, &v.FrameStepSeconds,
		&v.AnalysisAlias, &v.ActiveHours, &motionGate, &v.DailyReportTime, &enabled,
		&v.NextRunAt, &v.LastRunAt, &v.LastWindowTo, &v.LastError, &v.CreatedAt, &v.UpdatedAt)
	v.MotionGate = motionGate != 0
	v.Enabled = enabled != 0
	return v, err
}

func insertLogStream(db *sql.DB, s logStream) (string, error) {
	if s.ID == "" {
		s.ID = randomID("logstream")
	}
	if s.IntervalMinutes <= 0 {
		s.IntervalMinutes = 5
	}
	if s.FrameStepSeconds <= 0 {
		s.FrameStepSeconds = 30
	}
	if strings.TrimSpace(s.DailyReportTime) == "" {
		s.DailyReportTime = "20:00"
	}
	now := nowRFC3339()
	_, err := db.Exec(`INSERT INTO camera_log_streams
		(id, site_id, name, camera_ids, interval_minutes, frame_step_seconds, analysis_alias, active_hours, motion_gate, daily_report_time, enabled, next_run_at, last_run_at, last_window_to, last_error, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		s.ID, s.SiteID, s.Name, s.CameraIDs, s.IntervalMinutes, s.FrameStepSeconds, s.AnalysisAlias,
		s.ActiveHours, boolToInt(s.MotionGate), s.DailyReportTime, boolToInt(s.Enabled),
		s.NextRunAt, s.LastRunAt, s.LastWindowTo, s.LastError, now, now)
	return s.ID, err
}

func getLogStream(db *sql.DB, id string) (logStream, error) {
	return scanLogStream(db.QueryRow(`SELECT `+camLogStreamCols+` FROM camera_log_streams WHERE id = ?`, id))
}

// listLogStreams lists a site's streams (siteID "" → all sites, e.g. the
// scheduler's view).
func listLogStreams(db *sql.DB, siteID string) ([]logStream, error) {
	if strings.TrimSpace(siteID) == "" {
		return queryLogStreams(db, `SELECT `+camLogStreamCols+` FROM camera_log_streams ORDER BY created_at DESC`)
	}
	return queryLogStreams(db, `SELECT `+camLogStreamCols+` FROM camera_log_streams WHERE site_id = ? ORDER BY created_at DESC`, siteID)
}

// listDueLogStreams returns enabled streams whose next_run_at is due (empty or
// <= now). The scheduler then atomically claims each via claimLogStream.
func listDueLogStreams(db *sql.DB, now string) ([]logStream, error) {
	return queryLogStreams(db, `SELECT `+camLogStreamCols+` FROM camera_log_streams
		WHERE enabled = 1 AND (next_run_at = '' OR next_run_at <= ?) ORDER BY next_run_at`, now)
}

func queryLogStreams(db *sql.DB, query string, args ...any) ([]logStream, error) {
	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []logStream
	for rows.Next() {
		s, err := scanLogStream(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// claimLogStream atomically advances next_run_at from expectedNextRun to
// newNextRun, returning true only if THIS caller won the claim
// (RowsAffected==1) — the no-double-run guard cloned from claimCameraWatch.
func claimLogStream(db *sql.DB, id, expectedNextRun, newNextRun string) (bool, error) {
	res, err := db.Exec(`UPDATE camera_log_streams SET next_run_at = ?, updated_at = ? WHERE id = ? AND next_run_at = ? AND enabled = 1`,
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

// updateLogStream updates the operator-editable fields (never the scheduler's
// next/last columns or the continuity cursor).
func updateLogStream(db *sql.DB, s logStream) error {
	_, err := db.Exec(`UPDATE camera_log_streams SET name = ?, camera_ids = ?, interval_minutes = ?,
		frame_step_seconds = ?, analysis_alias = ?, active_hours = ?, motion_gate = ?, daily_report_time = ?,
		enabled = ?, updated_at = ? WHERE id = ?`,
		s.Name, s.CameraIDs, s.IntervalMinutes, s.FrameStepSeconds, s.AnalysisAlias, s.ActiveHours,
		boolToInt(s.MotionGate), s.DailyReportTime, boolToInt(s.Enabled), nowRFC3339(), s.ID)
	return err
}

// setLogStreamEnabled toggles a stream; enabling passes nextRunAt="" so the
// stream is due on the next tick.
func setLogStreamEnabled(db *sql.DB, id string, enabled bool, nextRunAt string) error {
	_, err := db.Exec(`UPDATE camera_log_streams SET enabled = ?, next_run_at = ?, updated_at = ? WHERE id = ?`,
		boolToInt(enabled), nextRunAt, nowRFC3339(), id)
	return err
}

// setLogStreamCursor records a completed (or failed) cycle: last_run_at,
// last_window_to (the UTC continuity cursor the next cycle resumes from) and
// last_error, WITHOUT touching next_run_at — the scheduler's atomic claim
// already reserved that, and a manual run must never revert it (the
// setWatchLastRunOnly lesson).
func setLogStreamCursor(db *sql.DB, id, lastRunAt, lastWindowTo, lastError string) error {
	_, err := db.Exec(`UPDATE camera_log_streams SET last_run_at = ?, last_window_to = ?, last_error = ?, updated_at = ? WHERE id = ?`,
		lastRunAt, lastWindowTo, lastError, nowRFC3339(), id)
	return err
}

func deleteLogStream(db *sql.DB, id string) error {
	_, err := db.Exec(`DELETE FROM camera_log_streams WHERE id = ?`, id)
	return err
}

// ─────────────────────────── camera_log_entries ───────────────────────────

const camLogEntryCols = `id, seq, stream_id, site_id, from_ts, to_ts, camera_ids, headline, report, motion_gated, frames, media_tokens, detail_tokens, alias, error, created_at`

func scanLogEntry(s rowScanner) (logEntry, error) {
	var v logEntry
	var gated int
	err := s.Scan(&v.ID, &v.Seq, &v.StreamID, &v.SiteID, &v.FromTS, &v.ToTS, &v.CameraIDs, &v.Headline,
		&v.Report, &gated, &v.Frames, &v.MediaTokens, &v.DetailTokens, &v.Alias, &v.Error, &v.CreatedAt)
	v.MotionGated = gated != 0
	return v, err
}

// insertLogEntry writes one immutable journal entry, stamping its sync seq
// from the shared counter in the same (tiny) transaction.
func insertLogEntry(db *sql.DB, e logEntry) (string, error) {
	if e.ID == "" {
		e.ID = randomID("logent")
	}
	if e.CreatedAt == "" {
		e.CreatedAt = nowRFC3339()
	}
	tx, err := db.Begin()
	if err != nil {
		return "", err
	}
	seq, err := nextCamSyncSeq(tx)
	if err != nil {
		tx.Rollback()
		return "", err
	}
	if _, err := tx.Exec(`INSERT INTO camera_log_entries
		(id, seq, stream_id, site_id, from_ts, to_ts, camera_ids, headline, report, motion_gated, frames, media_tokens, detail_tokens, alias, error, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		e.ID, seq, e.StreamID, e.SiteID, e.FromTS, e.ToTS, e.CameraIDs, e.Headline, e.Report,
		boolToInt(e.MotionGated), e.Frames, e.MediaTokens, e.DetailTokens, e.Alias, e.Error, e.CreatedAt); err != nil {
		tx.Rollback()
		return "", err
	}
	return e.ID, tx.Commit()
}

func getLogEntry(db *sql.DB, id string) (logEntry, error) {
	return scanLogEntry(db.QueryRow(`SELECT `+camLogEntryCols+` FROM camera_log_entries WHERE id = ?`, id))
}

// listLogEntriesBySite is the browse query (admin UI + service routes):
// newest-first within an optional [fromTS,toTS] from_ts window, optionally
// narrowed to one stream and/or one sampled camera. The camera filter matches
// the JSON-quoted id ("cam_x" including quotes), so a full id never
// substring-matches inside another. Always site-scoped so a leaked stream or
// camera id can never cross sites.
func listLogEntriesBySite(db *sql.DB, siteID, streamID, cameraID, fromTS, toTS string, limit int) ([]logEntry, error) {
	if limit <= 0 {
		limit = 200
	}
	q := `SELECT ` + camLogEntryCols + ` FROM camera_log_entries WHERE site_id = ?`
	args := []any{siteID}
	if s := strings.TrimSpace(streamID); s != "" {
		q += ` AND stream_id = ?`
		args = append(args, s)
	}
	if c := strings.TrimSpace(cameraID); c != "" {
		q += ` AND camera_ids LIKE ?`
		args = append(args, `%"`+c+`"%`)
	}
	if f := strings.TrimSpace(fromTS); f != "" {
		q += ` AND from_ts >= ?`
		args = append(args, f)
	}
	if t := strings.TrimSpace(toTS); t != "" {
		q += ` AND from_ts <= ?`
		args = append(args, t)
	}
	q += ` ORDER BY from_ts DESC LIMIT ?`
	args = append(args, limit)
	return queryLogEntries(db, q, args...)
}

// listLogEntriesForRollup returns a stream's entries with from_ts in
// [fromTS,toTS), oldest-first — the exact input order the hour/day rollup
// prompts want.
func listLogEntriesForRollup(db *sql.DB, streamID, fromTS, toTS string) ([]logEntry, error) {
	return queryLogEntries(db, `SELECT `+camLogEntryCols+` FROM camera_log_entries
		WHERE stream_id = ? AND from_ts >= ? AND from_ts < ? ORDER BY from_ts`, streamID, fromTS, toTS)
}

// listRecentLogEntries returns a stream's newest entries (newest first) — the
// continuity block's source ("the crew is STILL painting").
func listRecentLogEntries(db *sql.DB, streamID string, limit int) ([]logEntry, error) {
	if limit <= 0 {
		limit = 50
	}
	return queryLogEntries(db, `SELECT `+camLogEntryCols+` FROM camera_log_entries
		WHERE stream_id = ? ORDER BY from_ts DESC LIMIT ?`, streamID, limit)
}

// listLogEntriesAfterSeq is the pull-sync cursor query: a site's entries with
// seq > afterSeq, oldest-seq first, capped at limit. Entries are immutable so
// every row appears exactly once per cursor pass.
func listLogEntriesAfterSeq(db *sql.DB, siteID string, afterSeq int64, limit int) ([]logEntry, error) {
	if limit <= 0 {
		limit = 200
	}
	return queryLogEntries(db, `SELECT `+camLogEntryCols+` FROM camera_log_entries
		WHERE site_id = ? AND seq > ? ORDER BY seq LIMIT ?`, siteID, afterSeq, limit)
}

func queryLogEntries(db *sql.DB, query string, args ...any) ([]logEntry, error) {
	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []logEntry
	for rows.Next() {
		e, err := scanLogEntry(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// deleteOldLogEntries prunes entries created before cutoff (retention:
// PROXY_CAMERA_ACTIVITY_RETENTION; Connect keeps its archive forever, so no
// sync bookkeeping is needed here). Returns rows deleted.
func deleteOldLogEntries(db *sql.DB, cutoffRFC3339 string) (int64, error) {
	res, err := db.Exec(`DELETE FROM camera_log_entries WHERE created_at < ?`, cutoffRFC3339)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// ─────────────────────────── camera_log_hours ───────────────────────────

const camLogHourCols = `id, seq, stream_id, site_id, hour_start, tz, summary, language, entry_count, gated_count, status, error, created_at`

func scanLogHour(s rowScanner) (logHour, error) {
	var v logHour
	err := s.Scan(&v.ID, &v.Seq, &v.StreamID, &v.SiteID, &v.HourStart, &v.TZ, &v.Summary, &v.Language,
		&v.EntryCount, &v.GatedCount, &v.Status, &v.Error, &v.CreatedAt)
	return v, err
}

// claimLogHour atomically claims the (stream_id, hour_start) rollup by
// inserting a pending row under the unique claim index. Returns the row id and
// whether THIS caller won; a lost claim rolls the transaction back so the
// consumed seq is returned to the counter.
func claimLogHour(db *sql.DB, h logHour) (string, bool, error) {
	if h.ID == "" {
		h.ID = randomID("loghour")
	}
	if h.Status == "" {
		h.Status = "pending"
	}
	if h.CreatedAt == "" {
		h.CreatedAt = nowRFC3339()
	}
	tx, err := db.Begin()
	if err != nil {
		return "", false, err
	}
	seq, err := nextCamSyncSeq(tx)
	if err != nil {
		tx.Rollback()
		return "", false, err
	}
	res, err := tx.Exec(`INSERT OR IGNORE INTO camera_log_hours
		(id, seq, stream_id, site_id, hour_start, tz, summary, language, entry_count, gated_count, status, error, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		h.ID, seq, h.StreamID, h.SiteID, h.HourStart, h.TZ, h.Summary, h.Language,
		h.EntryCount, h.GatedCount, h.Status, h.Error, h.CreatedAt)
	if err != nil {
		tx.Rollback()
		return "", false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		tx.Rollback()
		return "", false, err
	}
	if n == 0 {
		tx.Rollback() // lost the claim to a concurrent rollup tick
		return "", false, nil
	}
	if err := tx.Commit(); err != nil {
		return "", false, err
	}
	return h.ID, true, nil
}

// finishLogHour writes an hour rollup's terminal state (done|empty|error) and
// RE-stamps seq in the same transaction, so the completed row reappears past
// any sync cursor Connect captured while it was pending.
func finishLogHour(db *sql.DB, h logHour) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	seq, err := nextCamSyncSeq(tx)
	if err != nil {
		tx.Rollback()
		return err
	}
	if _, err := tx.Exec(`UPDATE camera_log_hours SET seq = ?, summary = ?, language = ?,
		entry_count = ?, gated_count = ?, status = ?, error = ? WHERE id = ?`,
		seq, h.Summary, h.Language, h.EntryCount, h.GatedCount, h.Status, h.Error, h.ID); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

func getLogHour(db *sql.DB, id string) (logHour, error) {
	return scanLogHour(db.QueryRow(`SELECT `+camLogHourCols+` FROM camera_log_hours WHERE id = ?`, id))
}

// getLogHourByStreamHour resolves the claim key; sql.ErrNoRows when the hour
// was never claimed.
func getLogHourByStreamHour(db *sql.DB, streamID, hourStart string) (logHour, error) {
	return scanLogHour(db.QueryRow(`SELECT `+camLogHourCols+` FROM camera_log_hours
		WHERE stream_id = ? AND hour_start = ?`, streamID, hourStart))
}

// listLogHoursBySite is the browse query: newest-first within an optional
// [fromTS,toTS] hour_start window, optionally narrowed to one stream.
func listLogHoursBySite(db *sql.DB, siteID, streamID, fromTS, toTS string, limit int) ([]logHour, error) {
	if limit <= 0 {
		limit = 200
	}
	q := `SELECT ` + camLogHourCols + ` FROM camera_log_hours WHERE site_id = ?`
	args := []any{siteID}
	if s := strings.TrimSpace(streamID); s != "" {
		q += ` AND stream_id = ?`
		args = append(args, s)
	}
	if f := strings.TrimSpace(fromTS); f != "" {
		q += ` AND hour_start >= ?`
		args = append(args, f)
	}
	if t := strings.TrimSpace(toTS); t != "" {
		q += ` AND hour_start <= ?`
		args = append(args, t)
	}
	q += ` ORDER BY hour_start DESC LIMIT ?`
	args = append(args, limit)
	return queryLogHours(db, q, args...)
}

// listLogHoursAfterSeq is the pull-sync cursor query for hourly rollups.
// Pending rows are hidden — a claim is not content; the terminal update
// re-stamps seq so the finished row surfaces exactly once per cursor.
func listLogHoursAfterSeq(db *sql.DB, siteID string, afterSeq int64, limit int) ([]logHour, error) {
	if limit <= 0 {
		limit = 200
	}
	return queryLogHours(db, `SELECT `+camLogHourCols+` FROM camera_log_hours
		WHERE site_id = ? AND seq > ? AND status != 'pending' ORDER BY seq LIMIT ?`, siteID, afterSeq, limit)
}

func queryLogHours(db *sql.DB, query string, args ...any) ([]logHour, error) {
	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []logHour
	for rows.Next() {
		h, err := scanLogHour(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// deleteOldLogHours prunes hour rollups created before cutoff (same retention
// window as entries). Returns rows deleted.
func deleteOldLogHours(db *sql.DB, cutoffRFC3339 string) (int64, error) {
	res, err := db.Exec(`DELETE FROM camera_log_hours WHERE created_at < ?`, cutoffRFC3339)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// ─────────────────────────── camera_log_days ───────────────────────────

const camLogDayCols = `id, seq, stream_id, site_id, day, from_ts, to_ts, headline, report, language, hours_used, entries_used, view_token, status, error, notified_at, created_at`

func scanLogDay(s rowScanner) (logDay, error) {
	var v logDay
	err := s.Scan(&v.ID, &v.Seq, &v.StreamID, &v.SiteID, &v.Day, &v.FromTS, &v.ToTS, &v.Headline,
		&v.Report, &v.Language, &v.HoursUsed, &v.EntriesUsed, &v.ViewToken, &v.Status, &v.Error,
		&v.NotifiedAt, &v.CreatedAt)
	return v, err
}

// claimLogDay atomically claims the (stream_id, day) daily report by inserting
// a pending row under the unique claim index, minting the view_token that will
// survive every later regeneration. Returns the row id and whether THIS caller
// won; a lost claim rolls back (returning the consumed seq).
func claimLogDay(db *sql.DB, d logDay) (string, bool, error) {
	if d.ID == "" {
		d.ID = randomID("logday")
	}
	if d.ViewToken == "" {
		d.ViewToken = randToken(16)
	}
	if d.Status == "" {
		d.Status = "pending"
	}
	if d.CreatedAt == "" {
		d.CreatedAt = nowRFC3339()
	}
	tx, err := db.Begin()
	if err != nil {
		return "", false, err
	}
	seq, err := nextCamSyncSeq(tx)
	if err != nil {
		tx.Rollback()
		return "", false, err
	}
	res, err := tx.Exec(`INSERT OR IGNORE INTO camera_log_days
		(id, seq, stream_id, site_id, day, from_ts, to_ts, headline, report, language, hours_used, entries_used, view_token, status, error, notified_at, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		d.ID, seq, d.StreamID, d.SiteID, d.Day, d.FromTS, d.ToTS, d.Headline, d.Report, d.Language,
		d.HoursUsed, d.EntriesUsed, d.ViewToken, d.Status, d.Error, d.NotifiedAt, d.CreatedAt)
	if err != nil {
		tx.Rollback()
		return "", false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		tx.Rollback()
		return "", false, err
	}
	if n == 0 {
		tx.Rollback() // lost the claim (row already exists for this stream+day)
		return "", false, nil
	}
	if err := tx.Commit(); err != nil {
		return "", false, err
	}
	return d.ID, true, nil
}

// finishLogDay writes a daily report's terminal content and RE-stamps seq in
// the same transaction. id, view_token, day and created_at are deliberately
// untouched — regeneration (days/generate) calls this again on the SAME row,
// so an already-shared view link keeps working while the fresh seq makes the
// updated report reappear past any cursor Connect already consumed.
func finishLogDay(db *sql.DB, d logDay) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	seq, err := nextCamSyncSeq(tx)
	if err != nil {
		tx.Rollback()
		return err
	}
	if _, err := tx.Exec(`UPDATE camera_log_days SET seq = ?, from_ts = ?, to_ts = ?, headline = ?,
		report = ?, language = ?, hours_used = ?, entries_used = ?, status = ?, error = ? WHERE id = ?`,
		seq, d.FromTS, d.ToTS, d.Headline, d.Report, d.Language, d.HoursUsed, d.EntriesUsed,
		d.Status, d.Error, d.ID); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

// setLogDayNotified stamps notified_at after the daily-report webhook settles
// (delivery bookkeeping, not content — no seq re-stamp, Connect got the report
// through the webhook and/or the sync row itself).
func setLogDayNotified(db *sql.DB, id, at string) error {
	_, err := db.Exec(`UPDATE camera_log_days SET notified_at = ? WHERE id = ?`, at, id)
	return err
}

func getLogDay(db *sql.DB, id string) (logDay, error) {
	return scanLogDay(db.QueryRow(`SELECT `+camLogDayCols+` FROM camera_log_days WHERE id = ?`, id))
}

// getLogDayByStreamDay resolves the claim key; sql.ErrNoRows when day D was
// never claimed for this stream.
func getLogDayByStreamDay(db *sql.DB, streamID, day string) (logDay, error) {
	return scanLogDay(db.QueryRow(`SELECT `+camLogDayCols+` FROM camera_log_days
		WHERE stream_id = ? AND day = ?`, streamID, day))
}

// getLogDayByViewToken resolves the public report page's capability token. An
// empty token never resolves (defense in depth — claims always mint one, but a
// blank must not become a skeleton key).
func getLogDayByViewToken(db *sql.DB, token string) (logDay, error) {
	if strings.TrimSpace(token) == "" {
		return logDay{}, sql.ErrNoRows
	}
	return scanLogDay(db.QueryRow(`SELECT `+camLogDayCols+` FROM camera_log_days WHERE view_token = ?`, token))
}

// listLogDaysBySite is the browse query: newest-first within an optional
// [fromDay,toDay] day window (site-local "2006-01-02" strings compare
// lexicographically), optionally narrowed to one stream.
func listLogDaysBySite(db *sql.DB, siteID, streamID, fromDay, toDay string, limit int) ([]logDay, error) {
	if limit <= 0 {
		limit = 200
	}
	q := `SELECT ` + camLogDayCols + ` FROM camera_log_days WHERE site_id = ?`
	args := []any{siteID}
	if s := strings.TrimSpace(streamID); s != "" {
		q += ` AND stream_id = ?`
		args = append(args, s)
	}
	if f := strings.TrimSpace(fromDay); f != "" {
		q += ` AND day >= ?`
		args = append(args, f)
	}
	if t := strings.TrimSpace(toDay); t != "" {
		q += ` AND day <= ?`
		args = append(args, t)
	}
	q += ` ORDER BY day DESC LIMIT ?`
	args = append(args, limit)
	return queryLogDays(db, q, args...)
}

// listLogDaysAfterSeq is the pull-sync cursor query for daily reports. Pending
// rows are hidden; terminal updates AND regenerations re-stamp seq, so Connect
// upserts the same id with fresh content each time it reappears.
func listLogDaysAfterSeq(db *sql.DB, siteID string, afterSeq int64, limit int) ([]logDay, error) {
	if limit <= 0 {
		limit = 200
	}
	return queryLogDays(db, `SELECT `+camLogDayCols+` FROM camera_log_days
		WHERE site_id = ? AND seq > ? AND status != 'pending' ORDER BY seq LIMIT ?`, siteID, afterSeq, limit)
}

func queryLogDays(db *sql.DB, query string, args ...any) ([]logDay, error) {
	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []logDay
	for rows.Next() {
		d, err := scanLogDay(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// deleteOldLogDays prunes daily reports created before cutoff (retention:
// PROXY_CAMERA_ACTIVITY_REPORT_RETENTION — a year by default, longer than the
// entry/hour window because the daily report is the operator-facing artifact).
// Returns rows deleted.
func deleteOldLogDays(db *sql.DB, cutoffRFC3339 string) (int64, error) {
	res, err := db.Exec(`DELETE FROM camera_log_days WHERE created_at < ?`, cutoffRFC3339)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
