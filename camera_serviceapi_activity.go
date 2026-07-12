package main

// camera_serviceapi_activity.go — the Connect-facing service API for the
// activity log (the "observer" feature, camera_observer*.go), all site-scoped:
//
//   /api/cameras/activity/streams            GET list / POST create
//   /api/cameras/activity/streams/update     POST {id, …}
//   /api/cameras/activity/streams/delete     POST {id}
//   /api/cameras/activity/streams/toggle     POST {id, enabled} (enable → due now)
//   /api/cameras/activity/streams/run        POST {id} → {started}
//   /api/cameras/activity/entries            GET ?from&to&stream_id?&camera_id?&limit?
//   /api/cameras/activity/hours              GET ?from&to&stream_id?&limit?
//   /api/cameras/activity/days               GET ?from&to&stream_id?&id?&limit?
//   /api/cameras/activity/days/generate      POST {stream_id, date?, force?}
//   /api/cameras/activity/sync               GET ?type&after_seq&limit — the cursor
//
// The SYNC route is the durability mechanism: Connect archives every entry and
// terminal rollup in its own database by polling `seq > after_seq` pages and
// upserting by id (resumable + idempotent — a webhook ladder that gives up
// after {0,30s,5m} cannot be a durability channel, and per-entry webhooks would
// fire every 1–5 minutes per stream). Browse routes are newest-first for UIs;
// the sync route is seq-ascending for the cursor.
//
// Conventions match camera_serviceapi_parity.go: site_id is ALWAYS sc.SiteID
// (never a request field), cross-site/missing ids collapse into the uniform
// camSvcNotFound (camSvcLogStream / camSvcLogDay), and responses are hand-built
// snake_case maps. The thin admin twins live in camera_observer_http.go and
// share the row mappers + helpers below.

import (
	"database/sql"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// ─────────────────────────── ownership gates ───────────────────────────

// camSvcLogStream loads an activity-log stream and enforces sc-site ownership
// (uniform 404 on either failure).
func camSvcLogStream(db *sql.DB, w http.ResponseWriter, id, siteID string) (logStream, bool) {
	s, err := getLogStream(db, strings.TrimSpace(id))
	if err != nil || s.SiteID != siteID {
		camSvcNotFound(w)
		return logStream{}, false
	}
	return s, true
}

// camSvcLogDay loads a daily report and enforces sc-site ownership.
func camSvcLogDay(db *sql.DB, w http.ResponseWriter, id, siteID string) (logDay, bool) {
	d, err := getLogDay(db, strings.TrimSpace(id))
	if err != nil || d.SiteID != siteID {
		camSvcNotFound(w)
		return logDay{}, false
	}
	return d, true
}

// ─────────────────────────── JSON row mappers ───────────────────────────
// Shared by the service routes and the admin twins (camera_observer_http.go).
// The sync route augments these with a bare "seq" — browse shapes stay
// cursor-free so UIs never depend on it.

func logStreamJSON(s logStream) map[string]any {
	return map[string]any{
		"id": s.ID, "site_id": s.SiteID, "name": s.Name,
		"camera_ids":       camParseIDArray(s.CameraIDs),
		"interval_minutes": s.IntervalMinutes, "frame_step_seconds": s.FrameStepSeconds,
		"analysis_alias": s.AnalysisAlias, "active_hours": s.ActiveHours,
		"motion_gate": s.MotionGate, "face_rec": s.FaceRec, "daily_report_time": s.DailyReportTime,
		"enabled": s.Enabled, "next_run_at": s.NextRunAt, "last_run_at": s.LastRunAt,
		"last_window_to": s.LastWindowTo, "last_error": s.LastError,
		"created_at": s.CreatedAt, "updated_at": s.UpdatedAt,
	}
}

func logEntryJSON(e logEntry) map[string]any {
	return map[string]any{
		"id": e.ID, "stream_id": e.StreamID, "site_id": e.SiteID,
		"from_ts": e.FromTS, "to_ts": e.ToTS,
		"camera_ids": camParseIDArray(e.CameraIDs),
		"headline":   e.Headline, "report": e.Report, "motion_gated": e.MotionGated,
		"frames": e.Frames, "media_tokens": camDecodeJSON(e.MediaTokens),
		"detail_tokens": camDecodeJSON(e.DetailTokens),
		"alias":         e.Alias, "error": e.Error, "created_at": e.CreatedAt,
	}
}

func logHourJSON(h logHour) map[string]any {
	return map[string]any{
		"id": h.ID, "stream_id": h.StreamID, "site_id": h.SiteID,
		"hour_start": h.HourStart, "tz": h.TZ, "summary": h.Summary, "language": h.Language,
		"entry_count": h.EntryCount, "gated_count": h.GatedCount,
		"status": h.Status, "error": h.Error, "created_at": h.CreatedAt,
	}
}

// logDayJSON exposes the share link as view_url (the same URL the webhook
// carries); the raw view_token never appears on its own.
func logDayJSON(cfg config, d logDay) map[string]any {
	return map[string]any{
		"id": d.ID, "stream_id": d.StreamID, "site_id": d.SiteID, "day": d.Day,
		"from_ts": d.FromTS, "to_ts": d.ToTS,
		"headline": d.Headline, "report": d.Report, "language": d.Language,
		"hours_used": d.HoursUsed, "entries_used": d.EntriesUsed,
		"status": d.Status, "error": d.Error, "notified_at": d.NotifiedAt,
		"view_url": camLogDayViewURL(cfg, d), "created_at": d.CreatedAt,
	}
}

// ─────────────────────────── shared stream helpers ───────────────────────────

// camActivityStreamBody is the shared create/update request shape (admin twins
// embed it next to their explicit site_id).
type camActivityStreamBody struct {
	Name             string   `json:"name"`
	CameraIDs        []string `json:"camera_ids"`
	IntervalMinutes  int      `json:"interval_minutes"`
	FrameStepSeconds int      `json:"frame_step_seconds"`
	AnalysisAlias    string   `json:"analysis_alias"`
	ActiveHours      string   `json:"active_hours"`
	MotionGate       *bool    `json:"motion_gate"`
	FaceRec          *bool    `json:"face_rec"`
	DailyReportTime  string   `json:"daily_report_time"`
	Enabled          *bool    `json:"enabled"`
}

// camActivityStreamCapFor resolves the per-site stream cap
// (PROXY_CAMERA_OBSERVER_MAX_STREAMS, default 8).
func camActivityStreamCapFor(cfg config) int {
	if n := cfg.CameraObserverMaxStreams; n > 0 {
		return n
	}
	return 8
}

// camActivityValidDate reports whether d is a well-formed "YYYY-MM-DD" day
// string — the generate routes' pre-check, so a malformed date is a clean 400
// instead of a store error.
func camActivityValidDate(d string) bool {
	_, err := time.Parse("2006-01-02", d)
	return err == nil
}

// camActivityValidateStream checks the RESOLVED interval/step/report-time trio
// (callers apply defaults first); "" means valid, anything else is the 400 body.
func camActivityValidateStream(interval, step int, reportTime string) string {
	if interval < 1 || interval > 15 {
		return "interval_minutes must be 1..15"
	}
	if step < 10 || step > interval*60 {
		return "frame_step_seconds must be 10..interval_minutes*60"
	}
	if rt := strings.TrimSpace(reportTime); rt != "" {
		if _, err := time.Parse("15:04", rt); err != nil {
			return "daily_report_time must be HH:MM (24h)"
		}
	}
	return ""
}

// camActivityBuildStream validates a create body and builds the logStream row
// for siteID (defaults: interval 5, step 30, motion_gate on, report time 20:00
// via insertLogStream, enabled off). problem != "" is the 400 body.
func camActivityBuildStream(siteID string, body camActivityStreamBody) (logStream, string) {
	interval := body.IntervalMinutes
	if interval == 0 {
		interval = 5
	}
	step := body.FrameStepSeconds
	if step == 0 {
		step = 30
	}
	if problem := camActivityValidateStream(interval, step, body.DailyReportTime); problem != "" {
		return logStream{}, problem
	}
	ids := body.CameraIDs
	if ids == nil {
		ids = []string{}
	}
	return logStream{
		SiteID: siteID, Name: strings.TrimSpace(body.Name), CameraIDs: mustJSON(ids),
		IntervalMinutes: interval, FrameStepSeconds: step,
		AnalysisAlias: strings.TrimSpace(body.AnalysisAlias), ActiveHours: strings.TrimSpace(body.ActiveHours),
		MotionGate:      body.MotionGate == nil || *body.MotionGate,
		FaceRec:         body.FaceRec == nil || *body.FaceRec,
		DailyReportTime: strings.TrimSpace(body.DailyReportTime),
		Enabled:         body.Enabled != nil && *body.Enabled,
		// NextRunAt stays "" — an enabled stream is due on the next tick.
	}, ""
}

// camActivityApplyStreamUpdate applies a partial update to existing (blank
// name keeps, nil camera_ids keeps, zero numbers keep, alias/active_hours
// replace — the watch-update semantics; enabled is toggle-only and never
// touched here). The RESULTING interval/step pair is validated, so shrinking
// the interval below an existing wide step is rejected. problem != "" is the
// 400 body.
func camActivityApplyStreamUpdate(existing *logStream, body camActivityStreamBody) string {
	interval := existing.IntervalMinutes
	if body.IntervalMinutes != 0 {
		interval = body.IntervalMinutes
	}
	step := existing.FrameStepSeconds
	if body.FrameStepSeconds != 0 {
		step = body.FrameStepSeconds
	}
	reportTime := existing.DailyReportTime
	if strings.TrimSpace(body.DailyReportTime) != "" {
		reportTime = strings.TrimSpace(body.DailyReportTime)
	}
	if problem := camActivityValidateStream(interval, step, reportTime); problem != "" {
		return problem
	}
	if n := strings.TrimSpace(body.Name); n != "" {
		existing.Name = n
	}
	if body.CameraIDs != nil {
		existing.CameraIDs = mustJSON(body.CameraIDs)
	}
	existing.IntervalMinutes = interval
	existing.FrameStepSeconds = step
	existing.DailyReportTime = reportTime
	existing.AnalysisAlias = strings.TrimSpace(body.AnalysisAlias)
	existing.ActiveHours = strings.TrimSpace(body.ActiveHours)
	if body.MotionGate != nil {
		existing.MotionGate = *body.MotionGate
	}
	if body.FaceRec != nil {
		existing.FaceRec = *body.FaceRec
	}
	return ""
}

// ─────────────────────────── shared browse helpers ───────────────────────────
// The service handlers gate a stream_id filter through camSvcLogStream FIRST
// (uniform 404, the handleSvcRuns precedent); the admin twins pass filters
// through unGated — either way these run the site-scoped store queries and map
// rows, so a foreign filter can never return foreign rows.

func camActivityEntryRows(db *sql.DB, siteID, streamID, cameraID, from, to string, limit int) ([]map[string]any, error) {
	entries, err := listLogEntriesBySite(db, siteID, streamID, cameraID, from, to, limit)
	if err != nil {
		return nil, err
	}
	out := make([]map[string]any, 0, len(entries))
	for _, e := range entries {
		out = append(out, logEntryJSON(e))
	}
	return out, nil
}

func camActivityHourRows(db *sql.DB, siteID, streamID, from, to string, limit int) ([]map[string]any, error) {
	hours, err := listLogHoursBySite(db, siteID, streamID, from, to, limit)
	if err != nil {
		return nil, err
	}
	out := make([]map[string]any, 0, len(hours))
	for _, h := range hours {
		out = append(out, logHourJSON(h))
	}
	return out, nil
}

func camActivityDayRows(cfg config, db *sql.DB, siteID, streamID, fromDay, toDay string, limit int) ([]map[string]any, error) {
	days, err := listLogDaysBySite(db, siteID, streamID, fromDay, toDay, limit)
	if err != nil {
		return nil, err
	}
	out := make([]map[string]any, 0, len(days))
	for _, d := range days {
		out = append(out, logDayJSON(cfg, d))
	}
	return out, nil
}

// ─────────────────────────── streams CRUD ───────────────────────────

// handleSvcActivityStreams serves GET (list) / POST (create) on
// /api/cameras/activity/streams. Create enforces the per-site stream cap
// (400 — journaling burns AI calls every few minutes, so runaway stream
// creation is refused, not queued).
func handleSvcActivityStreams(cfg config) camSvcHandler {
	return func(w http.ResponseWriter, r *http.Request, sc camSvcCtx) {
		db := camSvcOpenDB(cfg, w)
		if db == nil {
			return
		}
		defer db.Close()
		switch r.Method {
		case http.MethodGet:
			streams, err := listLogStreams(db, sc.SiteID)
			if err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
				return
			}
			out := make([]map[string]any, 0, len(streams))
			for _, s := range streams {
				out = append(out, logStreamJSON(s))
			}
			writeJSON(w, http.StatusOK, map[string]any{"streams": out})
		case http.MethodPost:
			var body camActivityStreamBody
			if !camSvcDecode(w, r, &body) {
				return
			}
			existing, err := listLogStreams(db, sc.SiteID)
			if err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
				return
			}
			if maxStreams := camActivityStreamCapFor(cfg); len(existing) >= maxStreams {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": "this site already has the maximum number of activity-log streams"})
				return
			}
			row, problem := camActivityBuildStream(sc.SiteID, body)
			if problem != "" {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": problem})
				return
			}
			id, ierr := insertLogStream(db, row)
			if ierr != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]any{"error": ierr.Error()})
				return
			}
			camlog("info", "svc_activity_stream_create", map[string]any{"stream_id": id, "site_id": sc.SiteID, "enabled": row.Enabled})
			created, _ := getLogStream(db, id)
			writeJSON(w, http.StatusOK, map[string]any{"ok": true, "stream": logStreamJSON(created)})
		default:
			camSvcMethodNotAllowed(w)
		}
	}
}

// handleSvcActivityStreamUpdate serves POST /api/cameras/activity/streams/update —
// camSvcLogStream gate then the shared partial update.
func handleSvcActivityStreamUpdate(cfg config) camSvcHandler {
	return func(w http.ResponseWriter, r *http.Request, sc camSvcCtx) {
		if r.Method != http.MethodPost {
			camSvcMethodNotAllowed(w)
			return
		}
		var body struct {
			ID string `json:"id"`
			camActivityStreamBody
		}
		if !camSvcDecode(w, r, &body) {
			return
		}
		db := camSvcOpenDB(cfg, w)
		if db == nil {
			return
		}
		defer db.Close()
		s, ok := camSvcLogStream(db, w, body.ID, sc.SiteID)
		if !ok {
			return
		}
		if problem := camActivityApplyStreamUpdate(&s, body.camActivityStreamBody); problem != "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": problem})
			return
		}
		if err := updateLogStream(db, s); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		camlog("info", "svc_activity_stream_update", map[string]any{"stream_id": s.ID, "site_id": sc.SiteID})
		updated, _ := getLogStream(db, s.ID)
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "stream": logStreamJSON(updated)})
	}
}

// handleSvcActivityStreamDelete serves POST /api/cameras/activity/streams/delete
// {id} → {ok}. Entries/rollups are deliberately kept (history, pruned by
// retention; Connect archived them anyway).
func handleSvcActivityStreamDelete(cfg config) camSvcHandler {
	return func(w http.ResponseWriter, r *http.Request, sc camSvcCtx) {
		if r.Method != http.MethodPost {
			camSvcMethodNotAllowed(w)
			return
		}
		var body struct {
			ID string `json:"id"`
		}
		if !camSvcDecode(w, r, &body) {
			return
		}
		db := camSvcOpenDB(cfg, w)
		if db == nil {
			return
		}
		defer db.Close()
		s, ok := camSvcLogStream(db, w, body.ID, sc.SiteID)
		if !ok {
			return
		}
		if err := deleteLogStream(db, s.ID); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		camlog("info", "svc_activity_stream_delete", map[string]any{"stream_id": s.ID, "site_id": sc.SiteID})
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	}
}

// handleSvcActivityStreamToggle serves POST /api/cameras/activity/streams/toggle
// {id, enabled} → {ok}. Enabling clears next_run_at ("" = due on the next
// scheduler tick, listDueLogStreams).
func handleSvcActivityStreamToggle(cfg config) camSvcHandler {
	return func(w http.ResponseWriter, r *http.Request, sc camSvcCtx) {
		if r.Method != http.MethodPost {
			camSvcMethodNotAllowed(w)
			return
		}
		var body struct {
			ID      string `json:"id"`
			Enabled bool   `json:"enabled"`
		}
		if !camSvcDecode(w, r, &body) {
			return
		}
		db := camSvcOpenDB(cfg, w)
		if db == nil {
			return
		}
		defer db.Close()
		s, ok := camSvcLogStream(db, w, body.ID, sc.SiteID)
		if !ok {
			return
		}
		if err := setLogStreamEnabled(db, s.ID, body.Enabled, ""); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		camlog("info", "svc_activity_stream_toggle", map[string]any{"stream_id": s.ID, "site_id": sc.SiteID, "enabled": body.Enabled})
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	}
}

// handleSvcActivityStreamRun serves POST /api/cameras/activity/streams/run {id}
// → {ok, started:true} | {ok:false, error} when a cycle is already in flight
// (the triggerLogStreamRun in-flight guard).
func handleSvcActivityStreamRun(cfg config) camSvcHandler {
	return func(w http.ResponseWriter, r *http.Request, sc camSvcCtx) {
		if r.Method != http.MethodPost {
			camSvcMethodNotAllowed(w)
			return
		}
		var body struct {
			ID string `json:"id"`
		}
		if !camSvcDecode(w, r, &body) {
			return
		}
		db := camSvcOpenDB(cfg, w)
		if db == nil {
			return
		}
		defer db.Close()
		s, ok := camSvcLogStream(db, w, body.ID, sc.SiteID)
		if !ok {
			return
		}
		if !triggerLogStreamRun(cfg, s) {
			writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": "this stream already has a run in progress"})
			return
		}
		camlog("info", "svc_activity_stream_run", map[string]any{"stream_id": s.ID, "site_id": sc.SiteID})
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "started": true})
	}
}

// ─────────────────────────── browse routes ───────────────────────────

// handleSvcActivityEntries serves GET /api/cameras/activity/entries
// ?from&to&stream_id?&camera_id?&limit? → {entries:[…]} newest-first. A
// stream_id filter is ownership-gated (uniform 404 for a foreign stream).
func handleSvcActivityEntries(cfg config) camSvcHandler {
	return func(w http.ResponseWriter, r *http.Request, sc camSvcCtx) {
		if r.Method != http.MethodGet {
			camSvcMethodNotAllowed(w)
			return
		}
		db := camSvcOpenDB(cfg, w)
		if db == nil {
			return
		}
		defer db.Close()
		q := r.URL.Query()
		streamID := strings.TrimSpace(q.Get("stream_id"))
		if streamID != "" {
			s, ok := camSvcLogStream(db, w, streamID, sc.SiteID)
			if !ok {
				return
			}
			streamID = s.ID
		}
		rows, err := camActivityEntryRows(db, sc.SiteID, streamID, strings.TrimSpace(q.Get("camera_id")),
			strings.TrimSpace(q.Get("from")), strings.TrimSpace(q.Get("to")), parseIntDefault(q.Get("limit"), 200))
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"entries": rows})
	}
}

// handleSvcActivityHours serves GET /api/cameras/activity/hours
// ?from&to&stream_id?&limit? → {hours:[…]} newest-first.
func handleSvcActivityHours(cfg config) camSvcHandler {
	return func(w http.ResponseWriter, r *http.Request, sc camSvcCtx) {
		if r.Method != http.MethodGet {
			camSvcMethodNotAllowed(w)
			return
		}
		db := camSvcOpenDB(cfg, w)
		if db == nil {
			return
		}
		defer db.Close()
		q := r.URL.Query()
		streamID := strings.TrimSpace(q.Get("stream_id"))
		if streamID != "" {
			s, ok := camSvcLogStream(db, w, streamID, sc.SiteID)
			if !ok {
				return
			}
			streamID = s.ID
		}
		rows, err := camActivityHourRows(db, sc.SiteID, streamID,
			strings.TrimSpace(q.Get("from")), strings.TrimSpace(q.Get("to")), parseIntDefault(q.Get("limit"), 200))
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"hours": rows})
	}
}

// handleSvcActivityDays serves GET /api/cameras/activity/days
// ?from&to&stream_id?&limit? → {days:[…]} newest-first (from/to are site-local
// "YYYY-MM-DD" day strings). ?id= fetches ONE report through the camSvcLogDay
// gate instead — how Connect re-fetches a specific report by webhook report_id.
func handleSvcActivityDays(cfg config) camSvcHandler {
	return func(w http.ResponseWriter, r *http.Request, sc camSvcCtx) {
		if r.Method != http.MethodGet {
			camSvcMethodNotAllowed(w)
			return
		}
		db := camSvcOpenDB(cfg, w)
		if db == nil {
			return
		}
		defer db.Close()
		q := r.URL.Query()
		if id := strings.TrimSpace(q.Get("id")); id != "" {
			d, ok := camSvcLogDay(db, w, id, sc.SiteID)
			if !ok {
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{"days": []map[string]any{logDayJSON(cfg, d)}})
			return
		}
		streamID := strings.TrimSpace(q.Get("stream_id"))
		if streamID != "" {
			s, ok := camSvcLogStream(db, w, streamID, sc.SiteID)
			if !ok {
				return
			}
			streamID = s.ID
		}
		rows, err := camActivityDayRows(cfg, db, sc.SiteID, streamID,
			strings.TrimSpace(q.Get("from")), strings.TrimSpace(q.Get("to")), parseIntDefault(q.Get("limit"), 200))
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"days": rows})
	}
}

// handleSvcActivityDayGenerate serves POST /api/cameras/activity/days/generate
// {stream_id, date?, force?} → {ok, report}. Build-if-missing / rebuild with
// force on the SAME row (id + view_token survive; only the sync seq moves) —
// and this path NEVER emits the camera_daily_report webhook, so a Connect-
// initiated regenerate can't start a webhook loop (camObserverGenerateLogDay).
func handleSvcActivityDayGenerate(cfg config) camSvcHandler {
	return func(w http.ResponseWriter, r *http.Request, sc camSvcCtx) {
		if r.Method != http.MethodPost {
			camSvcMethodNotAllowed(w)
			return
		}
		var body struct {
			StreamID string `json:"stream_id"`
			Date     string `json:"date"`
			Force    bool   `json:"force"`
		}
		if !camSvcDecode(w, r, &body) {
			return
		}
		if d := strings.TrimSpace(body.Date); d != "" && !camActivityValidDate(d) {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "date must be YYYY-MM-DD"})
			return
		}
		db := camSvcOpenDB(cfg, w)
		if db == nil {
			return
		}
		defer db.Close()
		s, ok := camSvcLogStream(db, w, body.StreamID, sc.SiteID)
		if !ok {
			return
		}
		d, err := camObserverGenerateLogDay(cfg, db, s, body.Date, body.Force)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		camlog("info", "svc_activity_generate", map[string]any{"stream_id": s.ID, "site_id": sc.SiteID, "day": d.Day, "status": d.Status, "forced": body.Force})
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "report": logDayJSON(cfg, d)})
	}
}

// ─────────────────────────── the sync cursor ───────────────────────────

// handleSvcActivitySync serves GET /api/cameras/activity/sync
// ?type=entries|hours|days&after_seq=<n>&limit=<1..500, default 200> →
// {rows:[…svc shapes + "seq"…], max_seq, has_more}. Pages ascend by the global
// sync seq (per-site cursors see harmless gaps, never misses); pending
// hour/day claims are hidden (a claim is not content — the terminal update
// re-stamps seq so the finished row surfaces). max_seq is the counter's
// current value so Connect knows when it has caught up; invalid type/after_seq
// are 400s (a malformed cursor must fail loudly, not restart from zero).
func handleSvcActivitySync(cfg config) camSvcHandler {
	return func(w http.ResponseWriter, r *http.Request, sc camSvcCtx) {
		if r.Method != http.MethodGet {
			camSvcMethodNotAllowed(w)
			return
		}
		q := r.URL.Query()
		typ := strings.ToLower(strings.TrimSpace(q.Get("type")))
		if typ != "entries" && typ != "hours" && typ != "days" && typ != "sightings" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": `type must be "entries", "hours", "days" or "sightings"`})
			return
		}
		var afterSeq int64
		if raw := strings.TrimSpace(q.Get("after_seq")); raw != "" {
			v, perr := strconv.ParseInt(raw, 10, 64)
			if perr != nil || v < 0 {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": "after_seq must be a non-negative integer"})
				return
			}
			afterSeq = v
		}
		limit := parseIntDefault(q.Get("limit"), 200)
		if limit < 1 {
			limit = 200
		}
		if limit > 500 {
			limit = 500
		}
		db := camSvcOpenDB(cfg, w)
		if db == nil {
			return
		}
		defer db.Close()

		// Fetch limit+1 so has_more never needs a second query.
		rows := make([]map[string]any, 0, limit)
		hasMore := false
		switch typ {
		case "entries":
			list, err := listLogEntriesAfterSeq(db, sc.SiteID, afterSeq, limit+1)
			if err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
				return
			}
			if len(list) > limit {
				hasMore, list = true, list[:limit]
			}
			for _, e := range list {
				row := logEntryJSON(e)
				row["seq"] = e.Seq
				rows = append(rows, row)
			}
		case "hours":
			list, err := listLogHoursAfterSeq(db, sc.SiteID, afterSeq, limit+1)
			if err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
				return
			}
			if len(list) > limit {
				hasMore, list = true, list[:limit]
			}
			for _, h := range list {
				row := logHourJSON(h)
				row["seq"] = h.Seq
				rows = append(rows, row)
			}
		case "days":
			list, err := listLogDaysAfterSeq(db, sc.SiteID, afterSeq, limit+1)
			if err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
				return
			}
			if len(list) > limit {
				hasMore, list = true, list[:limit]
			}
			for _, d := range list {
				row := logDayJSON(cfg, d)
				row["seq"] = d.Seq
				rows = append(rows, row)
			}
		case "sightings":
			list, err := listSightingsAfterSeq(db, sc.SiteID, afterSeq, limit+1)
			if err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
				return
			}
			if len(list) > limit {
				hasMore, list = true, list[:limit]
			}
			for _, s := range list {
				rows = append(rows, camSightingSyncJSON(s))
			}
		}
		maxSeq, err := camSyncMaxSeq(db)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"rows": rows, "max_seq": maxSeq, "has_more": hasMore})
	}
}
