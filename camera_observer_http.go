package main

// camera_observer_http.go — the admin control-panel routes for the activity
// log, mounted under /ui/api/cameras/activity/* by registerCameraRoutes
// (camerahttp.go) behind requireAdmin. THIN twins of the Connect-facing
// service routes (camera_serviceapi_activity.go): they share the row mappers
// (logStreamJSON/logEntryJSON/logHourJSON/logDayJSON), the stream
// build/update/validation helpers and the browse query helpers, differing only
// in scoping — an admin names the site explicitly (site_id query/body field,
// any site) instead of inheriting it from a bearer token, and missing ids are
// plain 404s (no cross-site probing concern inside the admin session). There
// is deliberately NO admin twin of /activity/sync — the cursor is Connect's
// durability channel, not a UI surface (the browse routes serve the UI).

import (
	"encoding/json"
	"net/http"
	"strings"
)

// handleCameraActivityStreams serves GET /ui/api/cameras/activity/streams
// ?site_id= (optional — "" lists every site's streams, the admin watches
// shape) and POST {site_id, …camActivityStreamBody} to create one (per-site
// stream cap enforced like the service twin).
func handleCameraActivityStreams(cfg config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		db, err := openProxyDB(cfg)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		defer db.Close()
		switch r.Method {
		case http.MethodGet:
			streams, lerr := listLogStreams(db, strings.TrimSpace(r.URL.Query().Get("site_id")))
			if lerr != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]any{"error": lerr.Error()})
				return
			}
			out := make([]map[string]any, 0, len(streams))
			for _, s := range streams {
				out = append(out, logStreamJSON(s))
			}
			writeJSON(w, http.StatusOK, map[string]any{"streams": out})
		case http.MethodPost:
			var body struct {
				SiteID string `json:"site_id"`
				camActivityStreamBody
			}
			if derr := json.NewDecoder(r.Body).Decode(&body); derr != nil {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid JSON"})
				return
			}
			siteID := strings.TrimSpace(body.SiteID)
			if siteID == "" {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": "site_id is required"})
				return
			}
			existing, lerr := listLogStreams(db, siteID)
			if lerr != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]any{"error": lerr.Error()})
				return
			}
			if maxStreams := camActivityStreamCapFor(cfg); len(existing) >= maxStreams {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": "this site already has the maximum number of activity-log streams"})
				return
			}
			row, problem := camActivityBuildStream(siteID, body.camActivityStreamBody)
			if problem != "" {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": problem})
				return
			}
			id, ierr := insertLogStream(db, row)
			if ierr != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]any{"error": ierr.Error()})
				return
			}
			camlog("info", "activity_stream_create", map[string]any{"stream_id": id, "site_id": siteID, "enabled": row.Enabled})
			created, _ := getLogStream(db, id)
			writeJSON(w, http.StatusOK, map[string]any{"ok": true, "stream": logStreamJSON(created)})
		default:
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		}
	}
}

// handleCameraActivityStreamUpdate serves POST /ui/api/cameras/activity/streams/update
// {id, …} — the shared partial update (camActivityApplyStreamUpdate).
func handleCameraActivityStreamUpdate(cfg config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
			return
		}
		var body struct {
			ID string `json:"id"`
			camActivityStreamBody
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid JSON"})
			return
		}
		db, err := openProxyDB(cfg)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		defer db.Close()
		s, gerr := getLogStream(db, strings.TrimSpace(body.ID))
		if gerr != nil {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "stream not found"})
			return
		}
		if problem := camActivityApplyStreamUpdate(&s, body.camActivityStreamBody); problem != "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": problem})
			return
		}
		if uerr := updateLogStream(db, s); uerr != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": uerr.Error()})
			return
		}
		camlog("info", "activity_stream_update", map[string]any{"stream_id": s.ID, "site_id": s.SiteID})
		updated, _ := getLogStream(db, s.ID)
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "stream": logStreamJSON(updated)})
	}
}

// handleCameraActivityStreamDelete serves POST /ui/api/cameras/activity/streams/delete {id}.
func handleCameraActivityStreamDelete(cfg config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
			return
		}
		var body struct {
			ID string `json:"id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid JSON"})
			return
		}
		id := strings.TrimSpace(body.ID)
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
		if derr := deleteLogStream(db, id); derr != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": derr.Error()})
			return
		}
		camlog("info", "activity_stream_delete", map[string]any{"stream_id": id})
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	}
}

// handleCameraActivityStreamToggle serves POST /ui/api/cameras/activity/streams/toggle
// {id, enabled}. Enabling clears next_run_at ("" = due on the next tick).
func handleCameraActivityStreamToggle(cfg config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
			return
		}
		var body struct {
			ID      string `json:"id"`
			Enabled bool   `json:"enabled"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid JSON"})
			return
		}
		id := strings.TrimSpace(body.ID)
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
		if serr := setLogStreamEnabled(db, id, body.Enabled, ""); serr != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": serr.Error()})
			return
		}
		camlog("info", "activity_stream_toggle", map[string]any{"stream_id": id, "enabled": body.Enabled})
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	}
}

// handleCameraActivityStreamRun serves POST /ui/api/cameras/activity/streams/run
// {id} → {ok, started:true} | {ok:false, error} while a cycle is in flight.
func handleCameraActivityStreamRun(cfg config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
			return
		}
		var body struct {
			ID string `json:"id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid JSON"})
			return
		}
		db, err := openProxyDB(cfg)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		s, gerr := getLogStream(db, strings.TrimSpace(body.ID))
		db.Close()
		if gerr != nil {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "stream not found"})
			return
		}
		if !triggerLogStreamRun(cfg, s) {
			writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": "this stream already has a run in progress"})
			return
		}
		camlog("info", "activity_stream_run_manual", map[string]any{"stream_id": s.ID, "site_id": s.SiteID})
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "started": true})
	}
}

// camActivityAdminBrowse is the shared skeleton of the three admin browse
// twins: method/site_id checks, then the rows func over the parsed filters.
func camActivityAdminBrowse(w http.ResponseWriter, r *http.Request, key string,
	rows func(siteID, streamID, from, to string, limit int) ([]map[string]any, error)) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	q := r.URL.Query()
	siteID := strings.TrimSpace(q.Get("site_id"))
	if siteID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "site_id is required"})
		return
	}
	out, err := rows(siteID, strings.TrimSpace(q.Get("stream_id")),
		strings.TrimSpace(q.Get("from")), strings.TrimSpace(q.Get("to")), parseIntDefault(q.Get("limit"), 200))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{key: out})
}

// handleCameraActivityEntries serves GET /ui/api/cameras/activity/entries
// ?site_id&from&to&stream_id?&camera_id?&limit? → {entries:[…]} newest-first.
func handleCameraActivityEntries(cfg config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		db, err := openProxyDB(cfg)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		defer db.Close()
		cameraID := strings.TrimSpace(r.URL.Query().Get("camera_id"))
		camActivityAdminBrowse(w, r, "entries", func(siteID, streamID, from, to string, limit int) ([]map[string]any, error) {
			return camActivityEntryRows(db, siteID, streamID, cameraID, from, to, limit)
		})
	}
}

// handleCameraActivityHours serves GET /ui/api/cameras/activity/hours
// ?site_id&from&to&stream_id?&limit? → {hours:[…]} newest-first.
func handleCameraActivityHours(cfg config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		db, err := openProxyDB(cfg)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		defer db.Close()
		camActivityAdminBrowse(w, r, "hours", func(siteID, streamID, from, to string, limit int) ([]map[string]any, error) {
			return camActivityHourRows(db, siteID, streamID, from, to, limit)
		})
	}
}

// handleCameraActivityDays serves GET /ui/api/cameras/activity/days
// ?site_id&from&to&stream_id?&limit? → {days:[…]} newest-first (from/to are
// site-local "YYYY-MM-DD" day strings).
func handleCameraActivityDays(cfg config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		db, err := openProxyDB(cfg)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		defer db.Close()
		camActivityAdminBrowse(w, r, "days", func(siteID, streamID, from, to string, limit int) ([]map[string]any, error) {
			return camActivityDayRows(cfg, db, siteID, streamID, from, to, limit)
		})
	}
}

// handleCameraActivityDayGenerate serves POST /ui/api/cameras/activity/days/generate
// {stream_id, date?, force?} → {ok, report} — the "Generate now" button. Like
// the service twin it rides camObserverGenerateLogDay, so id + view_token
// survive a forced rebuild and no webhook is ever emitted from this path.
func handleCameraActivityDayGenerate(cfg config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
			return
		}
		var body struct {
			StreamID string `json:"stream_id"`
			Date     string `json:"date"`
			Force    bool   `json:"force"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid JSON"})
			return
		}
		if d := strings.TrimSpace(body.Date); d != "" {
			if !camActivityValidDate(d) {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": "date must be YYYY-MM-DD"})
				return
			}
		}
		db, err := openProxyDB(cfg)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		defer db.Close()
		s, gerr := getLogStream(db, strings.TrimSpace(body.StreamID))
		if gerr != nil {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "stream not found"})
			return
		}
		d, derr := camObserverGenerateLogDay(cfg, db, s, body.Date, body.Force)
		if derr != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": derr.Error()})
			return
		}
		camlog("info", "activity_generate", map[string]any{"stream_id": s.ID, "site_id": s.SiteID, "day": d.Day, "status": d.Status, "forced": body.Force})
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "report": logDayJSON(cfg, d)})
	}
}
