package main

// camera_diag.go (WP13) — camera-namespaced diagnostics: a live health surface
// (ffmpeg/ffprobe presence+version + per-DVR reachability derived from the
// camera_events diagnostics trail), a filterable view over that trail, and a
// "re-run a single capture" endpoint so a failure can be reproduced live with
// the exact masked command + full ffmpeg stderr. Deliberately namespaced under
// /ui/api/cameras/* rather than editing main.go's proxy-wide /health.
//
// Every endpoint here is registered noStore(requireAdmin(cfg, ...)) from
// registerCameraRoutes (camerahttp.go) — DVR reachability, event detail, and
// re-run results are admin-only like the rest of the camera subsystem.
// Handlers follow the same fresh-openProxyDB-per-request / writeJSON idiom as
// camerahttp.go, and reuse camera_store.go's camEventCols/scanEvent/
// collectEvents rather than duplicating the row-scanning logic.
//
// OBSERVABILITY: /health and /events are read-only diagnostics views and do
// not camlog themselves (mirroring handleCameraDiagnostics, WP11 — reads are
// cheap enough not to need it). /recapture camlog's its own outcome (ids only,
// never credentials) in addition to returning the masked command + stderr
// inline; captureSnapshot/captureLiveClip/capturePlaybackClip (camera_capture.go)
// already camlog the full underlying attempt (masked command, exit code,
// duration, full ffmpeg stderr on failure) as part of the hard observability
// requirement, so camLastCaptureDiag below re-reads that just-written
// camera_events row instead of duplicating capture logic or changing
// camera_capture.go's return shape (out of scope for this file).
import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// ─────────────────────────────── GET /ui/api/cameras/health ───────────────────────────────

// handleCameraHealth reports live ffmpeg/ffprobe presence+version (an
// uncached check — see cameraBinaryStatus's doc comment: a binary can
// appear/disappear while the process keeps running) plus a per-DVR
// reachability summary derived entirely from the camera_events trail: the
// most recent discovery/capture attempt for that DVR and the most recent one
// that succeeded.
func handleCameraHealth(cfg config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
			return
		}
		db, err := openProxyDB(cfg)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		defer db.Close()

		ffmpegOK, ffmpegVer, ffprobeOK, ffprobeVer := cameraBinaryStatus(cfg)
		dvrs, derr := listCameraDVRViews(db, "")
		if derr != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": derr.Error()})
			return
		}
		out := make([]map[string]any, 0, len(dvrs))
		for _, d := range dvrs {
			out = append(out, camDVRHealthJSON(db, d))
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"enabled": cfg.CameraEnabled,
			"ffmpeg":  map[string]any{"ok": ffmpegOK, "version": ffmpegVer, "bin": cameraFFmpegBin(cfg)},
			"ffprobe": map[string]any{"ok": ffprobeOK, "version": ffprobeVer, "bin": cameraFFprobeBin(cfg)},
			"dvrs":    out,
		})
	}
}

// camDVRHealthJSON summarizes one DVR's reachability from its camera_events
// history: discovery and every snapshot/clip attempt against it are logged
// with dvr_id set (camera_capture.go, camera_hikvision.go, camera_dahua.go,
// camera_onvif.go, camera_rtspprobe.go, camerahttp.go's discovery handler are
// the only ops that carry one), so "the last event for this dvr_id" and "the
// last OK event for this dvr_id" are enough to answer "is it reachable right
// now" and "when did it last work" without any new schema or a live probe.
func camDVRHealthJSON(db *sql.DB, d camDVRView) map[string]any {
	out := map[string]any{
		"dvr_id": d.ID, "site_id": d.SiteID, "name": d.Name, "brand": d.Brand,
		"host": d.Host, "enabled": d.Enabled, "status": "unknown", "last_success_at": "",
	}
	if last, ok := camLastDVREvent(db, d.ID); ok {
		out["last_op"] = last.Op
		out["last_attempt_at"] = last.TS
		out["last_ok"] = last.OK
		if last.OK {
			out["status"] = "ok"
		} else {
			out["status"] = "failing"
			out["last_error"] = last.Error
		}
	}
	if ts := camLastDVRSuccessAt(db, d.ID); ts != "" {
		out["last_success_at"] = ts
	}
	return out
}

// camLastDVREvent returns the most recent camera_events row for dvrID (no op
// filter needed — discovery and captures are the only ops that ever carry a
// dvr_id), or ok=false if that DVR has never attempted an operation yet.
func camLastDVREvent(db *sql.DB, dvrID string) (camEvent, bool) {
	e, err := scanEvent(db.QueryRow(`SELECT `+camEventCols+` FROM camera_events WHERE dvr_id = ? ORDER BY ts DESC, id DESC LIMIT 1`, dvrID))
	if err != nil {
		return camEvent{}, false
	}
	return e, true
}

// camLastDVRSuccessAt returns the timestamp of the most recent successful
// camera_events row for dvrID, or "" if it has never succeeded.
func camLastDVRSuccessAt(db *sql.DB, dvrID string) string {
	var ts string
	if err := db.QueryRow(`SELECT ts FROM camera_events WHERE dvr_id = ? AND ok = 1 ORDER BY ts DESC, id DESC LIMIT 1`, dvrID).Scan(&ts); err != nil {
		return ""
	}
	return ts
}

// ─────────────────────────────── GET /ui/api/cameras/events ───────────────────────────────

// handleCameraEvents is the diagnostics trail browser: recent camera_events,
// most-recent first, optionally filtered by ok/dvr_id/op so a panel can (for
// example) show only recent failures, or only events for one DVR.
func handleCameraEvents(cfg config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
			return
		}
		q := r.URL.Query()
		limit := parseIntDefault(q.Get("limit"), 100)
		dvrID := strings.TrimSpace(q.Get("dvr_id"))
		op := strings.TrimSpace(q.Get("op"))
		var okFilter *bool
		if raw := strings.TrimSpace(q.Get("ok")); raw != "" {
			b, perr := strconv.ParseBool(raw)
			if perr != nil {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": "ok must be true or false"})
				return
			}
			okFilter = &b
		}
		db, err := openProxyDB(cfg)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		defer db.Close()
		events, eerr := listCameraEventsFiltered(db, dvrID, op, okFilter, limit)
		if eerr != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": eerr.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"events": camEventsJSON(events)})
	}
}

// listCameraEventsFiltered queries camera_events with any combination of
// dvr_id/op/ok filters, newest first. camera_store.go's
// listRecentCameraEvents/listRecentCameraEventFailures cover the two fixed
// cases the WP11 baseline needed; the Diagnostics panel needs arbitrary
// combinations, so this builds the WHERE clause dynamically instead of
// stacking a third/fourth near-duplicate fixed helper there.
func listCameraEventsFiltered(db *sql.DB, dvrID, op string, ok *bool, limit int) ([]camEvent, error) {
	if limit <= 0 {
		limit = 100
	}
	if limit > 1000 {
		limit = 1000 // sanity cap — this is a human-facing diagnostics view, not an export
	}
	var (
		clauses []string
		args    []any
	)
	if dvrID != "" {
		clauses = append(clauses, "dvr_id = ?")
		args = append(args, dvrID)
	}
	if op != "" {
		clauses = append(clauses, "op = ?")
		args = append(args, op)
	}
	if ok != nil {
		clauses = append(clauses, "ok = ?")
		args = append(args, boolToInt(*ok))
	}
	query := `SELECT ` + camEventCols + ` FROM camera_events`
	if len(clauses) > 0 {
		query += " WHERE " + strings.Join(clauses, " AND ")
	}
	query += ` ORDER BY ts DESC, id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collectEvents(rows)
}

// ─────────────────────────────── POST /ui/api/cameras/recapture ───────────────────────────────

// camRecaptureDiag is the masked-command diagnostic detail recovered from the
// camera_events row a capture call just wrote, so a failure can be reproduced
// live from the Diagnostics panel without re-plumbing camera_capture.go's
// return shape.
type camRecaptureDiag struct {
	Cmd      string `json:"cmd"`
	Stderr   string `json:"stderr"`
	ExitCode int    `json:"exit_code"`
	TimedOut bool   `json:"timed_out"`
}

// handleCameraRecapture re-runs a single snapshot or clip capture for one
// camera on demand and returns the exact masked external command, full
// ffmpeg stderr, exit code, and (on success) the resulting capture
// token/URL — the plan's "reproduce a failure live" diagnostics primitive. It
// deliberately calls the SAME capture functions the dashboard's Live/Clip
// buttons use (captureSnapshot/captureLiveClip/capturePlaybackClip) rather
// than a diagnostics-only code path, so the reproduction is faithful; the
// masked command/stderr are then recovered from the camera_events row that
// call just camlog'd (camLastCaptureDiag), not threaded back through a
// changed return signature.
func handleCameraRecapture(cfg config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
			return
		}
		var body struct {
			CameraID string `json:"camera_id"`
			Kind     string `json:"kind"`    // "snapshot" (default) | "clip"
			Quality  string `json:"quality"` // "main" (default) | "sub"
			Seconds  int    `json:"seconds"` // live clip length when Kind=="clip" and From/To are blank
			From     string `json:"from"`    // RFC3339 — playback window start (clip only)
			To       string `json:"to"`      // RFC3339 — playback window end (clip only)
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid JSON"})
			return
		}
		cameraID := strings.TrimSpace(body.CameraID)
		if cameraID == "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "camera_id is required"})
			return
		}
		kind := strings.ToLower(strings.TrimSpace(body.Kind))
		if kind == "" {
			kind = "snapshot"
		}
		if kind != "snapshot" && kind != "clip" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "kind must be snapshot or clip"})
			return
		}

		db, err := openProxyDB(cfg)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		defer db.Close()
		cam, gerr := getCamera(db, cameraID)
		if gerr != nil {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "camera not found"})
			return
		}
		dvr, derr := getCameraDVR(db, cfg, cam.DVRID)
		if derr != nil {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "dvr not found"})
			return
		}
		if !dvr.Enabled {
			writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": "dvr is disabled"})
			return
		}
		q := StreamMain
		if strings.EqualFold(strings.TrimSpace(body.Quality), "sub") {
			q = StreamSub
		}

		scratch, serr := os.MkdirTemp("", "camrecap-")
		if serr != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": serr.Error()})
			return
		}
		defer os.RemoveAll(scratch)

		if kind == "clip" {
			handleCameraRecaptureClip(w, r, cfg, db, cam, dvr, q, scratch, body.Seconds, body.From, body.To)
			return
		}
		handleCameraRecaptureSnapshot(w, r, cfg, db, cam, dvr, q, scratch)
	}
}

// handleCameraRecaptureSnapshot runs the snapshot half of /recapture, sharing
// the exact capture call (captureSnapshot) and persistence helper
// (persistSnapshotCapture) the dashboard's on-demand "Live" button uses
// (handleCameraSnapshot, camerahttp.go), so a diagnostic re-run behaves
// identically to what an operator would see live.
func handleCameraRecaptureSnapshot(w http.ResponseWriter, r *http.Request, cfg config, db *sql.DB, cam camera, dvr CamDVR, q StreamQuality, scratch string) {
	ctx, cancel := context.WithTimeout(r.Context(), camCaptureTimeout(cfg))
	defer cancel()
	res, cerr := captureSnapshot(ctx, cfg, dvr, cam.Channel, q, filepath.Join(scratch, "snap.jpg"))
	diag := camLastCaptureDiag(db, dvr.ID, "capture_snapshot")
	if cerr != nil {
		// captureSnapshot already camlog'd the masked command + full ffmpeg stderr
		// (folded into diag above); this line just records that a diagnostic
		// re-run happened (ids only, never credentials).
		camlog("info", "recapture", map[string]any{
			"camera_id": cam.ID, "dvr_id": dvr.ID, "kind": "snapshot", "ok": false, "error": cerr.Error(),
		})
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": cerr.Error(), "diagnostic": diag})
		return
	}
	_, token, perr := persistSnapshotCapture(db, cfg, cam.SiteID, cam.ID, res, q)
	if perr != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": perr.Error()})
		return
	}
	camlog("info", "recapture", map[string]any{"camera_id": cam.ID, "dvr_id": dvr.ID, "kind": "snapshot", "ok": true, "token": token})
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "url": camMediaURL(cfg, r, token), "token": token,
		"content_type": res.ContentType, "width": res.Width, "height": res.Height, "bytes": res.Bytes,
		"diagnostic": diag,
	})
}

// handleCameraRecaptureClip runs the clip half of /recapture: a live clip
// (Seconds from now) when From/To are blank, otherwise a past-playback clip
// for [From,To] — mirroring handleCameraClip (camerahttp.go) exactly, so the
// diagnostic re-run is a faithful reproduction of what the operator's own
// request would do.
func handleCameraRecaptureClip(w http.ResponseWriter, r *http.Request, cfg config, db *sql.DB, cam camera, dvr CamDVR, q StreamQuality, scratch string, seconds int, fromRaw, toRaw string) {
	dest := filepath.Join(scratch, "clip.mp4")
	ctx, cancel := context.WithTimeout(r.Context(), camClipRequestTimeout(cfg))
	defer cancel()

	var (
		res          captureResult
		cerr         error
		fromTS, toTS string
	)
	fromRaw, toRaw = strings.TrimSpace(fromRaw), strings.TrimSpace(toRaw)
	if fromRaw != "" || toRaw != "" {
		from, ferr := time.Parse(time.RFC3339, fromRaw)
		to, terr := time.Parse(time.RFC3339, toRaw)
		if ferr != nil || terr != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "from/to must be RFC3339 timestamps"})
			return
		}
		fromTS, toTS = from.Format(time.RFC3339), to.Format(time.RFC3339)
		res, cerr = capturePlaybackClip(ctx, cfg, dvr, cam.Channel, q, from, to, dest)
	} else {
		res, cerr = captureLiveClip(ctx, cfg, dvr, cam.Channel, q, seconds, dest)
	}
	diag := camLastCaptureDiag(db, dvr.ID, "capture_clip")
	if cerr != nil {
		errMsg := cerr.Error()
		if errors.Is(cerr, errNoRecording) {
			errMsg = "no recording found in that time window"
		}
		camlog("info", "recapture", map[string]any{
			"camera_id": cam.ID, "dvr_id": dvr.ID, "kind": "clip", "ok": false, "error": errMsg,
		})
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": errMsg, "diagnostic": diag})
		return
	}
	token, perr := camPersistCapture(db, cfg, "", cam.SiteID, cam.ID, "clip", q.String(), res.Path, "video/mp4", 0, 0, fromTS, toTS)
	if perr != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": perr.Error()})
		return
	}
	camlog("info", "recapture", map[string]any{"camera_id": cam.ID, "dvr_id": dvr.ID, "kind": "clip", "ok": true, "token": token})
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "url": camMediaURL(cfg, r, token), "token": token,
		"content_type": "video/mp4", "bytes": res.Bytes, "diagnostic": diag,
	})
}

// camLastCaptureDiag recovers the masked command / full stderr / exit code /
// timed-out flag that captureSnapshot/captureLiveClip/capturePlaybackClip just
// camlog'd for this dvr+op: camlog folds any field not among camera_events'
// well-known columns (camera_log.go's camEventFromFields) into the row's JSON
// detail blob, and camera_capture.go always passes "cmd"/"stderr"/"exit_code"/
// "timed_out" alongside dvr_id/op, so this is how /recapture surfaces "the
// exact external command with credentials masked... and full ffmpeg stderr on
// failure" without camera_capture.go having to change its return signature.
// Best-effort: a zero-value camRecaptureDiag is returned if, for any reason,
// no matching event can be read back (e.g. a capture path that logs no
// dvr_id, such as "no brand adapter available").
func camLastCaptureDiag(db *sql.DB, dvrID, op string) camRecaptureDiag {
	var detail string
	if err := db.QueryRow(`SELECT detail FROM camera_events WHERE dvr_id = ? AND op = ? ORDER BY ts DESC, id DESC LIMIT 1`, dvrID, op).Scan(&detail); err != nil {
		return camRecaptureDiag{}
	}
	if strings.TrimSpace(detail) == "" {
		return camRecaptureDiag{}
	}
	var raw map[string]any
	if json.Unmarshal([]byte(detail), &raw) != nil {
		return camRecaptureDiag{}
	}
	diag := camRecaptureDiag{}
	if v, ok := raw["cmd"].(string); ok {
		diag.Cmd = v
	}
	if v, ok := raw["stderr"].(string); ok {
		diag.Stderr = v
	}
	if v, ok := raw["exit_code"].(float64); ok {
		diag.ExitCode = int(v)
	}
	if v, ok := raw["timed_out"].(bool); ok {
		diag.TimedOut = v
	}
	return diag
}
