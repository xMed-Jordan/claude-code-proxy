package main

// camera_serviceapi_parity.go — the site-scoped service-API parity routes
// (design-connect.md Workstream A): every admin-only camera capability mirrored
// under /api/cameras/* so Connect can drive it with its per-site bearer token.
// Handlers here replicate the thin admin bodies with sc.SiteID scoping and the
// shared plumbing from camera_serviceapi.go (camSvcOpenDB/camSvcDecode/
// camSvcNotFound/camSvcMethodNotAllowed). Kept separate so camera_serviceapi.go
// doesn't keep growing.
//
// INVARIANTS (see the design doc): site_id is ALWAYS sc.SiteID, never the body;
// cross-site ids resolve to the uniform camSvcNotFound so they can't be probed;
// secrets are never echoed (has_secret booleans only — blank-on-update keeps the
// stored value, clear_secret removes it); response shapes match the admin
// equivalents except the deliberate deltas noted in the doc.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// ─────────────────────────── ownership gates ───────────────────────────
//
// These mirror camSvcDVR/camSvcCamera/camSvcPlaybook (camera_serviceapi.go):
// load the row, compare SiteID, and collapse either failure (missing row or
// wrong site) into the single camSvcNotFound response.

// camSvcWatch loads a watch and enforces sc-site ownership (404 on either failure).
func camSvcWatch(db *sql.DB, w http.ResponseWriter, id, siteID string) (watch, bool) {
	wt, err := getCameraWatch(db, strings.TrimSpace(id))
	if err != nil || wt.SiteID != siteID {
		camSvcNotFound(w)
		return watch{}, false
	}
	return wt, true
}

// camSvcWatchRun loads a watch run and enforces sc-site ownership.
func camSvcWatchRun(db *sql.DB, w http.ResponseWriter, id, siteID string) (camWatchRun, bool) {
	run, err := getCameraWatchRun(db, strings.TrimSpace(id))
	if err != nil || run.SiteID != siteID {
		camSvcNotFound(w)
		return camWatchRun{}, false
	}
	return run, true
}

// camSvcQuestion loads a setup question and enforces sc-site ownership.
func camSvcQuestion(db *sql.DB, w http.ResponseWriter, id, siteID string) (camQuestion, bool) {
	q, err := getCameraQuestion(db, strings.TrimSpace(id))
	if err != nil || q.SiteID != siteID {
		camSvcNotFound(w)
		return camQuestion{}, false
	}
	return q, true
}

// camSvcAPITool loads an API tool and enforces sc-site ownership.
func camSvcAPITool(db *sql.DB, w http.ResponseWriter, id, siteID string) (camAPITool, bool) {
	t, err := getCameraAPITool(db, strings.TrimSpace(id))
	if err != nil || t.SiteID != siteID {
		camSvcNotFound(w)
		return camAPITool{}, false
	}
	return t, true
}

// ─────────────────────────── watch action update ───────────────────────────

// camSvcActionRequest is the service-API shape of a watch action update: the
// admin camActionRequest plus an explicit ClearSecret flag. Unlike the admin
// path (which wipes the secret on any blank update and relies on client
// discipline), a blank secret here KEEPS the stored value; ClearSecret is the
// deliberate way to remove it.
type camSvcActionRequest struct {
	camActionRequest
	ClearSecret bool `json:"clear_secret"`
}

// camSvcWatchActionUpdate produces the new action_json for a watch update with
// blank-keeps / clear_secret-removes semantics. type/url are always taken from
// the request; the secret is carried forward from existingActionJSON when the
// request secret is blank and ClearSecret is false, re-encrypted when non-blank,
// and dropped when ClearSecret is true. A nil request leaves the action
// untouched. Mirrors encodeWatchAction's encoding otherwise.
func camSvcWatchActionUpdate(cfg config, existingActionJSON string, req *camSvcActionRequest) string {
	if req == nil {
		return existingActionJSON
	}
	typ := strings.ToLower(strings.TrimSpace(req.Type))
	if typ == "" {
		typ = "log"
	}
	ac := camActionConfig{Type: typ, URL: strings.TrimSpace(req.URL)}
	switch {
	case req.ClearSecret:
		// explicit removal — leave TokenEnc empty
	case strings.TrimSpace(req.Secret) != "":
		if enc, err := encryptSecret(cfg, strings.TrimSpace(req.Secret)); err == nil {
			ac.TokenEnc = enc
		}
	default:
		// blank secret without clear_secret — carry the existing secret forward
		ac.TokenEnc = parseCamActionConfig(existingActionJSON).TokenEnc
	}
	return mustJSON(ac)
}

// ─────────────────────────── group D — setup questions ───────────────────────────
// Mirrors handleCameraQuestions / handleCameraQuestionAnswer /
// handleCameraQuestionDismiss (camerahttp.go) with sc.SiteID scoping; the
// mutations ownership-gate the id through camSvcQuestion first.

// handleSvcQuestions serves GET /api/cameras/questions?status= →
// {questions:[questionJSON…]} for the token's site.
func handleSvcQuestions(cfg config) camSvcHandler {
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
		status := strings.TrimSpace(r.URL.Query().Get("status"))
		qs, err := listCameraQuestions(db, sc.SiteID, status)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		out := make([]map[string]any, 0, len(qs))
		for _, q := range qs {
			out = append(out, questionJSON(q))
		}
		writeJSON(w, http.StatusOK, map[string]any{"questions": out})
	}
}

// handleSvcQuestionAnswer serves POST /api/cameras/questions/answer {id, answer} → {ok}.
func handleSvcQuestionAnswer(cfg config) camSvcHandler {
	return func(w http.ResponseWriter, r *http.Request, sc camSvcCtx) {
		if r.Method != http.MethodPost {
			camSvcMethodNotAllowed(w)
			return
		}
		var body struct {
			ID     string `json:"id"`
			Answer string `json:"answer"`
		}
		if !camSvcDecode(w, r, &body) {
			return
		}
		answer := strings.TrimSpace(body.Answer)
		if answer == "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "answer is required"})
			return
		}
		db := camSvcOpenDB(cfg, w)
		if db == nil {
			return
		}
		defer db.Close()
		q, ok := camSvcQuestion(db, w, body.ID, sc.SiteID)
		if !ok {
			return
		}
		if err := answerCameraQuestion(db, q.ID, answer); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		camlog("info", "svc_question_answer", map[string]any{"question_id": q.ID, "site_id": sc.SiteID})
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	}
}

// handleSvcQuestionDismiss serves POST /api/cameras/questions/dismiss {id} → {ok}.
func handleSvcQuestionDismiss(cfg config) camSvcHandler {
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
		q, ok := camSvcQuestion(db, w, body.ID, sc.SiteID)
		if !ok {
			return
		}
		if err := setCameraQuestionStatus(db, q.ID, "dismissed"); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		camlog("info", "svc_question_dismiss", map[string]any{"question_id": q.ID, "site_id": sc.SiteID})
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	}
}

// ─────────────────────────── group E — watches, runs, captures ───────────────────────────
// Mirrors handleCameraWatches / …WatchUpdate / …WatchDelete / …WatchToggle /
// …WatchRun / …Runs / …RunGet / …Captures (camerahttp.go). site_id is never a
// request field — it's always sc.SiteID; every id is gated through camSvcWatch /
// camSvcWatchRun. The update path uses camSvcWatchActionUpdate (blank secret
// keeps, clear_secret removes) instead of the admin's blank-wipes encoding.

// handleSvcWatches serves GET (list) / POST (create) on /api/cameras/watches.
func handleSvcWatches(cfg config) camSvcHandler {
	return func(w http.ResponseWriter, r *http.Request, sc camSvcCtx) {
		db := camSvcOpenDB(cfg, w)
		if db == nil {
			return
		}
		defer db.Close()
		switch r.Method {
		case http.MethodGet:
			watches, err := listCameraWatches(db, sc.SiteID)
			if err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
				return
			}
			out := make([]map[string]any, 0, len(watches))
			for _, wt := range watches {
				out = append(out, watchJSON(wt))
			}
			writeJSON(w, http.StatusOK, map[string]any{"watches": out})
		case http.MethodPost:
			var body struct {
				Name            string            `json:"name"`
				Instruction     string            `json:"instruction"`
				CameraIDs       []string          `json:"camera_ids"`
				AnalysisAlias   string            `json:"analysis_alias"`
				Mode            string            `json:"mode"`
				IntervalSeconds int               `json:"interval_seconds"`
				ActiveHours     string            `json:"active_hours"`
				MaxRounds       int               `json:"max_rounds"`
				Enabled         *bool             `json:"enabled"`
				Action          *camActionRequest `json:"action"`
			}
			if !camSvcDecode(w, r, &body) {
				return
			}
			mode, ok := camValidateWatchMode(body.Mode)
			if !ok {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": "mode must be escalate or mosaic_only"})
				return
			}
			ids := body.CameraIDs
			if ids == nil {
				ids = []string{}
			}
			interval := body.IntervalSeconds
			if interval <= 0 {
				interval = 300
			}
			enabled := body.Enabled != nil && *body.Enabled
			row := watch{
				SiteID: sc.SiteID, Name: strings.TrimSpace(body.Name), Instruction: strings.TrimSpace(body.Instruction),
				CameraIDs: mustJSON(ids), AnalysisAlias: strings.TrimSpace(body.AnalysisAlias), Mode: mode,
				IntervalSeconds: interval, ActiveHours: strings.TrimSpace(body.ActiveHours),
				MaxRounds: body.MaxRounds, ActionJSON: encodeWatchAction(cfg, body.Action), Enabled: enabled,
			}
			if enabled {
				row.NextRunAt = nowRFC3339() // due immediately so a newly enabled watch doesn't wait a full interval
			}
			id, err := insertCameraWatch(db, row)
			if err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
				return
			}
			camlog("info", "svc_watch_create", map[string]any{"watch_id": id, "site_id": sc.SiteID, "mode": mode, "enabled": enabled})
			wRow, _ := getCameraWatch(db, id)
			writeJSON(w, http.StatusOK, map[string]any{"ok": true, "watch": watchJSON(wRow)})
		default:
			camSvcMethodNotAllowed(w)
		}
	}
}

// handleSvcWatchUpdate serves POST /api/cameras/watches/update — camSvcWatch gate
// then partial update with camSvcWatchActionUpdate secret semantics.
func handleSvcWatchUpdate(cfg config) camSvcHandler {
	return func(w http.ResponseWriter, r *http.Request, sc camSvcCtx) {
		if r.Method != http.MethodPost {
			camSvcMethodNotAllowed(w)
			return
		}
		var body struct {
			ID              string               `json:"id"`
			Name            string               `json:"name"`
			Instruction     string               `json:"instruction"`
			CameraIDs       []string             `json:"camera_ids"`
			AnalysisAlias   string               `json:"analysis_alias"`
			Mode            string               `json:"mode"`
			IntervalSeconds int                  `json:"interval_seconds"`
			ActiveHours     string               `json:"active_hours"`
			MaxRounds       int                  `json:"max_rounds"`
			Action          *camSvcActionRequest `json:"action"`
		}
		if !camSvcDecode(w, r, &body) {
			return
		}
		db := camSvcOpenDB(cfg, w)
		if db == nil {
			return
		}
		defer db.Close()
		existing, ok := camSvcWatch(db, w, body.ID, sc.SiteID)
		if !ok {
			return
		}
		if n := strings.TrimSpace(body.Name); n != "" {
			existing.Name = n
		}
		existing.Instruction = strings.TrimSpace(body.Instruction)
		if body.CameraIDs != nil {
			existing.CameraIDs = mustJSON(body.CameraIDs)
		}
		existing.AnalysisAlias = strings.TrimSpace(body.AnalysisAlias)
		if strings.TrimSpace(body.Mode) != "" {
			mode, mok := camValidateWatchMode(body.Mode)
			if !mok {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": "mode must be escalate or mosaic_only"})
				return
			}
			existing.Mode = mode
		}
		if body.IntervalSeconds > 0 {
			existing.IntervalSeconds = body.IntervalSeconds
		}
		existing.ActiveHours = strings.TrimSpace(body.ActiveHours)
		if body.MaxRounds > 0 {
			existing.MaxRounds = body.MaxRounds
		}
		existing.ActionJSON = camSvcWatchActionUpdate(cfg, existing.ActionJSON, body.Action)
		if err := updateCameraWatch(db, existing); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		camlog("info", "svc_watch_update", map[string]any{"watch_id": existing.ID, "site_id": sc.SiteID})
		wRow, _ := getCameraWatch(db, existing.ID)
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "watch": watchJSON(wRow)})
	}
}

// handleSvcWatchDelete serves POST /api/cameras/watches/delete {id} → {ok}.
func handleSvcWatchDelete(cfg config) camSvcHandler {
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
		wt, ok := camSvcWatch(db, w, body.ID, sc.SiteID)
		if !ok {
			return
		}
		if err := deleteCameraWatch(db, wt.ID); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		camlog("info", "svc_watch_delete", map[string]any{"watch_id": wt.ID, "site_id": sc.SiteID})
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	}
}

// handleSvcWatchToggle serves POST /api/cameras/watches/toggle {id, enabled} → {ok}.
func handleSvcWatchToggle(cfg config) camSvcHandler {
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
		wt, ok := camSvcWatch(db, w, body.ID, sc.SiteID)
		if !ok {
			return
		}
		nextRunAt := ""
		if body.Enabled {
			nextRunAt = nowRFC3339() // due immediately so enabling doesn't wait a full interval
		}
		if err := setWatchEnabled(db, wt.ID, body.Enabled, nextRunAt); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		camlog("info", "svc_watch_toggle", map[string]any{"watch_id": wt.ID, "site_id": sc.SiteID, "enabled": body.Enabled})
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	}
}

// handleSvcWatchRun serves POST /api/cameras/watches/run {id} → {ok, started:true}
// or {ok:false, error} when a run is already in progress.
func handleSvcWatchRun(cfg config) camSvcHandler {
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
		wt, ok := camSvcWatch(db, w, body.ID, sc.SiteID)
		if !ok {
			return
		}
		if !triggerWatchRun(cfg, wt) {
			writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": "this watch already has a run in progress"})
			return
		}
		camlog("info", "svc_watch_run_manual", map[string]any{"watch_id": wt.ID, "site_id": sc.SiteID})
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "started": true})
	}
}

// handleSvcRuns serves GET /api/cameras/runs?watch_id=&limit= →
// {runs:[runJSON(run,false)…]}: watch_id given → that watch's runs (gated);
// else all of the site's runs newest-first.
func handleSvcRuns(cfg config) camSvcHandler {
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
		limit := parseIntDefault(r.URL.Query().Get("limit"), 50)
		var (
			runs []camWatchRun
			err  error
		)
		if watchID := strings.TrimSpace(r.URL.Query().Get("watch_id")); watchID != "" {
			wt, ok := camSvcWatch(db, w, watchID, sc.SiteID)
			if !ok {
				return
			}
			runs, err = listCameraWatchRuns(db, wt.ID, limit)
		} else {
			runs, err = listCameraWatchRunsBySite(db, sc.SiteID, limit)
		}
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		out := make([]map[string]any, 0, len(runs))
		for _, run := range runs {
			out = append(out, runJSON(run, false))
		}
		writeJSON(w, http.StatusOK, map[string]any{"runs": out})
	}
}

// handleSvcRunGet serves GET /api/cameras/runs/get?id= →
// {run: runJSON(run,true) + captures} for a site-owned run.
func handleSvcRunGet(cfg config) camSvcHandler {
	return func(w http.ResponseWriter, r *http.Request, sc camSvcCtx) {
		if r.Method != http.MethodGet {
			camSvcMethodNotAllowed(w)
			return
		}
		id := strings.TrimSpace(r.URL.Query().Get("id"))
		if id == "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "id is required"})
			return
		}
		db := camSvcOpenDB(cfg, w)
		if db == nil {
			return
		}
		defer db.Close()
		run, ok := camSvcWatchRun(db, w, id, sc.SiteID)
		if !ok {
			return
		}
		caps, _ := listCameraCaptures(db, run.ID)
		capOut := make([]map[string]any, 0, len(caps))
		for _, c := range caps {
			capOut = append(capOut, captureJSON(c, cfg, r))
		}
		out := runJSON(run, true)
		out["captures"] = capOut
		writeJSON(w, http.StatusOK, map[string]any{"run": out})
	}
}

// svcCaptureJSON is captureJSON plus a bare `token` key so Connect can machine-
// fetch the media via GET /api/cameras/media/{token} without parsing the URL
// (mirrors the svcInvestigateMessageJSON precedent).
func svcCaptureJSON(c camCapture, cfg config, r *http.Request) map[string]any {
	out := captureJSON(c, cfg, r)
	out["token"] = c.Token
	return out
}

// handleSvcCaptures serves GET /api/cameras/captures?run_id=&camera_id=&limit= →
// {captures:[…]}. Deliberately WIDER than the admin run-scoped handler: a
// site-wide recent-captures browser (Connect needs one), with optional run_id
// (ownership-gated) and camera_id filters. Each item carries a bare token.
func handleSvcCaptures(cfg config) camSvcHandler {
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
		runID := strings.TrimSpace(r.URL.Query().Get("run_id"))
		cameraID := strings.TrimSpace(r.URL.Query().Get("camera_id"))
		limit := parseIntDefault(r.URL.Query().Get("limit"), 50)
		if runID != "" {
			run, ok := camSvcWatchRun(db, w, runID, sc.SiteID)
			if !ok {
				return
			}
			runID = run.ID
		}
		caps, err := listCameraCapturesBySite(db, sc.SiteID, runID, cameraID, limit)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		out := make([]map[string]any, 0, len(caps))
		for _, c := range caps {
			out = append(out, svcCaptureJSON(c, cfg, r))
		}
		writeJSON(w, http.StatusOK, map[string]any{"captures": out})
	}
}

// ─────────────────────────── group C — API tools ───────────────────────────
// Mirrors handleCameraAPITools / …Update / …Delete / …Test (camera_apitools.go)
// with sc.SiteID scoping (no site_id field, no getCameraSite existence check).
// Secret semantics are copied as-is: blank auth_secret keeps the stored value,
// clear_secret removes it, non-blank replaces it. The test path inherits the
// SSRF guard via camAPIToolDo → camGuardedHTTPClient and never logs the
// substituted URL (host/status/latency only).

// handleSvcAPITools serves GET (list) / POST (create) on /api/cameras/apitools.
func handleSvcAPITools(cfg config) camSvcHandler {
	return func(w http.ResponseWriter, r *http.Request, sc camSvcCtx) {
		db := camSvcOpenDB(cfg, w)
		if db == nil {
			return
		}
		defer db.Close()
		switch r.Method {
		case http.MethodGet:
			tools, err := listCameraAPITools(db, sc.SiteID, false)
			if err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
				return
			}
			out := make([]map[string]any, 0, len(tools))
			for _, t := range tools {
				out = append(out, apiToolJSON(t))
			}
			writeJSON(w, http.StatusOK, map[string]any{"tools": out})
		case http.MethodPost:
			var body struct {
				Name                 string          `json:"name"`
				Description          string          `json:"description"`
				Method               string          `json:"method"`
				URLTemplate          string          `json:"url_template"`
				Headers              json.RawMessage `json:"headers"`
				AuthSecret           string          `json:"auth_secret"`
				BodyTemplate         string          `json:"body_template"`
				RequestInstructions  string          `json:"request_instructions"`
				ResponseInstructions string          `json:"response_instructions"`
				Enabled              *bool           `json:"enabled"`
			}
			if !camSvcDecode(w, r, &body) {
				return
			}
			name := strings.TrimSpace(body.Name)
			if name == "" {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": "name is required"})
				return
			}
			enabled := true
			if body.Enabled != nil {
				enabled = *body.Enabled
			}
			t := camAPITool{
				SiteID:               sc.SiteID,
				Name:                 name,
				Description:          strings.TrimSpace(body.Description),
				Method:               camAPIToolNormMethod(body.Method),
				URLTemplate:          strings.TrimSpace(body.URLTemplate),
				HeadersJSON:          camAPIToolHeadersString(body.Headers),
				BodyTemplate:         body.BodyTemplate,
				RequestInstructions:  strings.TrimSpace(body.RequestInstructions),
				ResponseInstructions: strings.TrimSpace(body.ResponseInstructions),
				Enabled:              enabled,
			}
			if verr := camAPIToolValidateTemplate(t); verr != nil {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": verr.Error()})
				return
			}
			if _, ferr := findCameraAPIToolByName(db, sc.SiteID, name); ferr == nil {
				writeJSON(w, http.StatusConflict, map[string]any{"error": "an API tool with that name already exists for this site"})
				return
			}
			id, err := insertCameraAPITool(db, cfg, t, body.AuthSecret)
			if err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
				return
			}
			camlog("info", "svc_apitool_create", map[string]any{"tool_id": id, "site_id": sc.SiteID, "name": name})
			created, _ := getCameraAPITool(db, id)
			writeJSON(w, http.StatusOK, map[string]any{"ok": true, "tool": apiToolJSON(created)})
		default:
			camSvcMethodNotAllowed(w)
		}
	}
}

// handleSvcAPIToolUpdate serves POST /api/cameras/apitools/update — camSvcAPITool
// gate then partial update; secret semantics copied from the admin path.
func handleSvcAPIToolUpdate(cfg config) camSvcHandler {
	return func(w http.ResponseWriter, r *http.Request, sc camSvcCtx) {
		if r.Method != http.MethodPost {
			camSvcMethodNotAllowed(w)
			return
		}
		var body struct {
			ID                   string          `json:"id"`
			Name                 string          `json:"name"`
			Description          *string         `json:"description"`
			Method               string          `json:"method"`
			URLTemplate          string          `json:"url_template"`
			Headers              json.RawMessage `json:"headers"`
			AuthSecret           string          `json:"auth_secret"`
			ClearSecret          bool            `json:"clear_secret"`
			BodyTemplate         *string         `json:"body_template"`
			RequestInstructions  *string         `json:"request_instructions"`
			ResponseInstructions *string         `json:"response_instructions"`
			Enabled              *bool           `json:"enabled"`
		}
		if !camSvcDecode(w, r, &body) {
			return
		}
		db := camSvcOpenDB(cfg, w)
		if db == nil {
			return
		}
		defer db.Close()
		t, ok := camSvcAPITool(db, w, body.ID, sc.SiteID)
		if !ok {
			return
		}
		if n := strings.TrimSpace(body.Name); n != "" {
			if other, ferr := findCameraAPIToolByName(db, t.SiteID, n); ferr == nil && other.ID != t.ID {
				writeJSON(w, http.StatusConflict, map[string]any{"error": "an API tool with that name already exists for this site"})
				return
			}
			t.Name = n
		}
		if body.Description != nil {
			t.Description = strings.TrimSpace(*body.Description)
		}
		if m := strings.TrimSpace(body.Method); m != "" {
			t.Method = camAPIToolNormMethod(m)
		}
		if u := strings.TrimSpace(body.URLTemplate); u != "" {
			t.URLTemplate = u
		}
		if len(body.Headers) > 0 {
			t.HeadersJSON = camAPIToolHeadersString(body.Headers)
		}
		if body.BodyTemplate != nil {
			t.BodyTemplate = *body.BodyTemplate
		}
		if body.RequestInstructions != nil {
			t.RequestInstructions = strings.TrimSpace(*body.RequestInstructions)
		}
		if body.ResponseInstructions != nil {
			t.ResponseInstructions = strings.TrimSpace(*body.ResponseInstructions)
		}
		if body.Enabled != nil {
			t.Enabled = *body.Enabled
		}
		if verr := camAPIToolValidateTemplate(t); verr != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": verr.Error()})
			return
		}
		// Blank secret keeps the stored one; clear_secret removes it; non-blank
		// replaces it (DVR-password idiom).
		changeSecret := body.ClearSecret || strings.TrimSpace(body.AuthSecret) != ""
		secret := ""
		if !body.ClearSecret {
			secret = body.AuthSecret
		}
		if err := updateCameraAPITool(db, cfg, t, changeSecret, secret); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		camlog("info", "svc_apitool_update", map[string]any{"tool_id": t.ID, "site_id": sc.SiteID, "name": t.Name})
		updated, _ := getCameraAPITool(db, t.ID)
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "tool": apiToolJSON(updated)})
	}
}

// handleSvcAPIToolDelete serves POST /api/cameras/apitools/delete {id} → {ok}.
func handleSvcAPIToolDelete(cfg config) camSvcHandler {
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
		t, ok := camSvcAPITool(db, w, body.ID, sc.SiteID)
		if !ok {
			return
		}
		if err := deleteCameraAPITool(db, t.ID); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		camlog("info", "svc_apitool_delete", map[string]any{"tool_id": t.ID, "site_id": sc.SiteID, "name": t.Name})
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	}
}

// handleSvcAPIToolTest serves POST /api/cameras/apitools/test {id, params} →
// {ok, status, response} | 502. Runs the full substitution + guarded call
// (camGuardedHTTPClient SSRF guard on the path); never logs the substituted URL.
func handleSvcAPIToolTest(cfg config) camSvcHandler {
	return func(w http.ResponseWriter, r *http.Request, sc camSvcCtx) {
		if r.Method != http.MethodPost {
			camSvcMethodNotAllowed(w)
			return
		}
		var body struct {
			ID     string         `json:"id"`
			Params map[string]any `json:"params"`
		}
		if !camSvcDecode(w, r, &body) {
			return
		}
		db := camSvcOpenDB(cfg, w)
		if db == nil {
			return
		}
		defer db.Close()
		t, ok := camSvcAPITool(db, w, body.ID, sc.SiteID)
		if !ok {
			return
		}
		params := make(map[string]string, len(body.Params))
		for k, v := range body.Params {
			params[strings.TrimSpace(k)] = strings.TrimSpace(fmt.Sprint(v))
		}
		rawURL, serr := camAPIToolSubstitute(t.URLTemplate, params, false)
		if serr != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": serr.Error()})
			return
		}
		method := camAPIToolNormMethod(t.Method)
		var reqBody []byte
		if method == http.MethodPost && strings.TrimSpace(t.BodyTemplate) != "" {
			b, berr := camAPIToolSubstitute(t.BodyTemplate, params, true)
			if berr != nil {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": berr.Error()})
				return
			}
			reqBody = []byte(b)
		}
		headers := map[string]string{}
		if hj := strings.TrimSpace(t.HeadersJSON); hj != "" {
			if uerr := json.Unmarshal([]byte(hj), &headers); uerr != nil {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": "headers must be a JSON object of string values"})
				return
			}
		}
		if strings.TrimSpace(t.AuthSecretEnc) != "" {
			secret, derr := decryptSecret(cfg, t.AuthSecretEnc)
			if derr != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "stored secret cannot be decrypted — re-save it"})
				return
			}
			headers["Authorization"] = "Bearer " + secret
		}
		host := ""
		if u, perr := url.Parse(rawURL); perr == nil {
			host = u.Host
		}
		start := time.Now()
		status, respBody, derr := camAPIToolDo(r.Context(), method, rawURL, headers, reqBody)
		latency := time.Since(start).Milliseconds()
		if derr != nil {
			// Never log the substituted URL/headers; the error is echoed to the caller only.
			camlog("warn", "svc_apitool_test", map[string]any{
				"tool_id": t.ID, "tool": t.Name, "site_id": sc.SiteID, "host": host, "status": 0, "latency_ms": latency,
			})
			writeJSON(w, http.StatusBadGateway, map[string]any{"error": "request failed: " + derr.Error()})
			return
		}
		camlog("info", "svc_apitool_test", map[string]any{
			"tool_id": t.ID, "tool": t.Name, "site_id": sc.SiteID, "host": host, "status": status, "latency_ms": latency, "resp_bytes": len(respBody),
		})
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "status": status, "response": respBody})
	}
}

// ─────────────────────────── group A — on-demand clip ───────────────────────────
// Mirrors handleCameraClip (camerahttp.go): a live clip (Seconds from now) when
// From/To are blank, otherwise a past-playback clip for [From,To]. site_id comes
// from the token via the camSvcCamera gate; a getCameraDVR miss collapses to the
// uniform camSvcNotFound. The response augments the admin shape with a bare
// media_token like handleSvcSnapshot so Connect can machine-fetch the clip via
// GET /api/cameras/media/{token} without parsing the URL.

// handleSvcClip serves POST /api/cameras/clip {id, quality?, seconds?, from?, to?} →
// {ok:true, media_token, url, content_type:"video/mp4", bytes} (or {ok:false,error}
// on a disabled DVR / no recording / capture failure; 400 on a bad RFC3339 window;
// 404 for a cross-site id).
func handleSvcClip(cfg config) camSvcHandler {
	return func(w http.ResponseWriter, r *http.Request, sc camSvcCtx) {
		if r.Method != http.MethodPost {
			camSvcMethodNotAllowed(w)
			return
		}
		var body struct {
			ID      string `json:"id"`
			Quality string `json:"quality"`
			Seconds int    `json:"seconds"` // live clip length when From/To are empty
			From    string `json:"from"`    // RFC3339 — playback window start
			To      string `json:"to"`      // RFC3339 — playback window end
		}
		if !camSvcDecode(w, r, &body) {
			return
		}
		db := camSvcOpenDB(cfg, w)
		if db == nil {
			return
		}
		defer db.Close()
		cam, ok := camSvcCamera(db, w, body.ID, sc.SiteID)
		if !ok {
			return
		}
		dvr, derr := getCameraDVR(db, cfg, cam.DVRID)
		if derr != nil {
			camSvcNotFound(w)
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

		scratch, serr := os.MkdirTemp("", "camsvcclip-")
		if serr != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": serr.Error()})
			return
		}
		defer os.RemoveAll(scratch)
		dest := filepath.Join(scratch, "clip.mp4")

		ctx, cancel := context.WithTimeout(r.Context(), camClipRequestTimeout(cfg))
		defer cancel()

		var (
			res          captureResult
			cerr         error
			fromTS, toTS string
		)
		fromRaw, toRaw := strings.TrimSpace(body.From), strings.TrimSpace(body.To)
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
			res, cerr = captureLiveClip(ctx, cfg, dvr, cam.Channel, q, body.Seconds, dest)
		}
		if cerr != nil {
			if errors.Is(cerr, errNoRecording) {
				writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": "no recording found in that time window"})
				return
			}
			// capturePlaybackClip/captureLiveClip already camlog'd the masked command + full ffmpeg stderr.
			writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": cerr.Error()})
			return
		}
		token, perr := camPersistCapture(db, cfg, "", cam.SiteID, cam.ID, "clip", q.String(), res.Path, "video/mp4", 0, 0, fromTS, toTS)
		if perr != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": perr.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"ok": true, "media_token": token, "url": camMediaURL(cfg, r, token),
			"content_type": "video/mp4", "bytes": res.Bytes,
		})
	}
}

// ─────────────────────────── group F — diagnostics ───────────────────────────
// Mirrors handleCameraHealth / handleCameraEvents / handleCameraRecapture
// (camera_diag.go), scoped to sc.SiteID. Health drops the ffmpeg/ffprobe `bin`
// absolute-path fields (server-local paths don't belong in a tenant response);
// events runs through the site-filtered listCameraEventsFiltered; recapture
// gates the camera, then hands off to the same handler-agnostic snapshot/clip
// funcs the admin path uses so the reproduction (and its masked-command
// diagnostic block) is byte-identical.

// handleSvcHealth serves GET /api/cameras/health → ffmpeg/ffprobe presence+version
// (no `bin` paths) plus this site's per-DVR reachability summary.
func handleSvcHealth(cfg config) camSvcHandler {
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
		ffmpegOK, ffmpegVer, ffprobeOK, ffprobeVer := cameraBinaryStatus(cfg)
		dvrs, derr := listCameraDVRViews(db, sc.SiteID)
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
			// Deliberate delta vs handleCameraHealth: the `bin` absolute paths are
			// omitted — server-local filesystem paths don't belong in a tenant response.
			"ffmpeg":  map[string]any{"ok": ffmpegOK, "version": ffmpegVer},
			"ffprobe": map[string]any{"ok": ffprobeOK, "version": ffprobeVer},
			"dvrs":    out,
		})
	}
}

// handleSvcEvents serves GET /api/cameras/events?limit=&dvr_id=&op=&ok= → the
// site-filtered diagnostics trail (events referencing nothing site-owned are
// invisible). A dvr_id, when given, is gated through camSvcDVR first (uniform 404).
func handleSvcEvents(cfg config) camSvcHandler {
	return func(w http.ResponseWriter, r *http.Request, sc camSvcCtx) {
		if r.Method != http.MethodGet {
			camSvcMethodNotAllowed(w)
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
		db := camSvcOpenDB(cfg, w)
		if db == nil {
			return
		}
		defer db.Close()
		if dvrID != "" {
			dvr, ok := camSvcDVR(db, cfg, w, dvrID, sc.SiteID)
			if !ok {
				return
			}
			dvrID = dvr.ID
		}
		events, eerr := listCameraEventsFiltered(db, sc.SiteID, dvrID, op, okFilter, limit)
		if eerr != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": eerr.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"events": camEventsJSON(events)})
	}
}

// handleSvcRecapture serves POST /api/cameras/recapture — re-run a single
// snapshot/clip capture for one owned camera and return the masked command +
// full ffmpeg stderr + (on success) the capture token/URL. Decodes the same body
// as handleCameraRecapture, gates the camera through camSvcCamera, then hands off
// to the shared handleCameraRecaptureSnapshot/handleCameraRecaptureClip so the
// response (incl. the diagnostic block) is identical to the admin path.
func handleSvcRecapture(cfg config) camSvcHandler {
	return func(w http.ResponseWriter, r *http.Request, sc camSvcCtx) {
		if r.Method != http.MethodPost {
			camSvcMethodNotAllowed(w)
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
		if !camSvcDecode(w, r, &body) {
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
		db := camSvcOpenDB(cfg, w)
		if db == nil {
			return
		}
		defer db.Close()
		cam, ok := camSvcCamera(db, w, body.CameraID, sc.SiteID)
		if !ok {
			return
		}
		dvr, derr := getCameraDVR(db, cfg, cam.DVRID)
		if derr != nil {
			camSvcNotFound(w)
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
		scratch, serr := os.MkdirTemp("", "camsvcrecap-")
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

// ─────────────────────────── group B — playbook reference media ───────────────────────────
// Mirrors handleCameraPlaybookMedia / handleCameraPlaybookMediaDelete
// (camera_playbooks.go). Every path is gated through camSvcPlaybook so a
// cross-site playbook resolves to the uniform 404. The attach path adds a
// service-only cpt.SiteID == sc.SiteID check that returns the IDENTICAL 400 body
// as a missing/expired capture, so a cross-site capture token can't be probed.

// handleSvcPlaybookMedia serves GET (list) / POST (attach) on
// /api/cameras/playbooks/media, both gated by camSvcPlaybook.
func handleSvcPlaybookMedia(cfg config) camSvcHandler {
	return func(w http.ResponseWriter, r *http.Request, sc camSvcCtx) {
		db := camSvcOpenDB(cfg, w)
		if db == nil {
			return
		}
		defer db.Close()
		switch r.Method {
		case http.MethodGet:
			pbID := strings.TrimSpace(r.URL.Query().Get("playbook_id"))
			if pbID == "" {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": "playbook_id is required"})
				return
			}
			pb, ok := camSvcPlaybook(db, w, pbID, sc.SiteID)
			if !ok {
				return
			}
			rowsM, lerr := listCameraPlaybookMedia(db, pb.ID)
			if lerr != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]any{"error": lerr.Error()})
				return
			}
			out := make([]map[string]any, 0, len(rowsM))
			for _, m := range rowsM {
				ct := ""
				if cpt, cerr := getCameraCaptureByToken(db, m.CaptureToken); cerr == nil {
					ct = cpt.ContentType
				}
				out = append(out, cameraPlaybookMediaJSON(cfg, r, m, ct))
			}
			writeJSON(w, http.StatusOK, map[string]any{"media": out})
		case http.MethodPost:
			var body struct {
				PlaybookID string `json:"playbook_id"`
				Token      string `json:"token"`
				Note       string `json:"note"`
			}
			if !camSvcDecode(w, r, &body) {
				return
			}
			pbID := strings.TrimSpace(body.PlaybookID)
			token := strings.TrimSpace(body.Token)
			if pbID == "" || token == "" {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": "playbook_id and token are required"})
				return
			}
			pb, ok := camSvcPlaybook(db, w, pbID, sc.SiteID)
			if !ok {
				return
			}
			// Attach validation: the capture row must exist, its file must still be
			// on disk, it must not be expired, AND it must belong to this site. A
			// cross-site token returns the IDENTICAL body as missing/expired so the
			// token namespace can't be probed for existence across sites.
			const unavailable = "capture no longer available — re-run the setup investigation"
			cpt, cerr := getCameraCaptureByToken(db, token)
			if cerr != nil {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": unavailable})
				return
			}
			if _, serr := os.Stat(cpt.Path); serr != nil {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": unavailable})
				return
			}
			if cpt.ExpiresAt != "" && cpt.ExpiresAt <= nowRFC3339() {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": unavailable})
				return
			}
			if cpt.SiteID != sc.SiteID {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": unavailable})
				return
			}
			if perr := pinCameraCapture(db, token); perr != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]any{"error": perr.Error()})
				return
			}
			m := camPlaybookMedia{
				PlaybookID:   pb.ID,
				CaptureToken: token,
				Note:         strings.TrimSpace(body.Note),
				CreatedAt:    nowRFC3339(),
			}
			id, ierr := insertCameraPlaybookMedia(db, m)
			if ierr != nil {
				// Undo the pin we just took if the row could not be recorded.
				camReleaseCaptureIfUnreferenced(db, cfg, token)
				writeJSON(w, http.StatusInternalServerError, map[string]any{"error": ierr.Error()})
				return
			}
			m.ID = id
			camlog("info", "svc_playbook_media_attach", map[string]any{"playbook_id": pb.ID, "media_id": id, "site_id": sc.SiteID, "kind": cpt.Kind})
			resp := map[string]any{"ok": true, "media": cameraPlaybookMediaJSON(cfg, r, m, cpt.ContentType)}
			if cfg.CameraMediaDir == "" {
				resp["warning"] = "PROXY_CAM_MEDIA_DIR is not set — reference images live in a temp directory"
			}
			writeJSON(w, http.StatusOK, resp)
		default:
			camSvcMethodNotAllowed(w)
		}
	}
}

// handleSvcPlaybookMediaDelete serves POST /api/cameras/playbooks/media/delete
// {id} → {ok}. Ownership runs through the parent playbook (camSvcPlaybook), so a
// media row on another site's playbook is a uniform 404.
func handleSvcPlaybookMediaDelete(cfg config) camSvcHandler {
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
		m, gerr := getCameraPlaybookMedia(db, strings.TrimSpace(body.ID))
		if gerr != nil {
			camSvcNotFound(w)
			return
		}
		if _, ok := camSvcPlaybook(db, w, m.PlaybookID, sc.SiteID); !ok {
			return
		}
		if derr := deleteCameraPlaybookMedia(db, m.ID); derr != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": derr.Error()})
			return
		}
		camReleaseCaptureIfUnreferenced(db, cfg, m.CaptureToken)
		camlog("info", "svc_playbook_media_delete", map[string]any{"playbook_id": m.PlaybookID, "media_id": m.ID, "site_id": sc.SiteID})
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	}
}

// ─────────────────────────── group G — site analyze ───────────────────────────
// Mirrors handleCameraSiteAnalyze (camerahttp.go). The site is the token's own
// (sc.SiteID), so there's no body id to decode; startSiteAnalysis flips the status
// to "running" synchronously and runs the describe pass in the background. The
// companion site-name update lives on handleSvcSite (POST {name?}) in
// camera_serviceapi.go, next to the description/policy fields.

// handleSvcSiteAnalyze serves POST /api/cameras/site/analyze {} →
// {ok:true, started} (started=false when an analysis for this site is already
// running) or {ok:false, error} when it can't start.
func handleSvcSiteAnalyze(cfg config) camSvcHandler {
	return func(w http.ResponseWriter, r *http.Request, sc camSvcCtx) {
		if r.Method != http.MethodPost {
			camSvcMethodNotAllowed(w)
			return
		}
		started, err := startSiteAnalysis(cfg, sc.SiteID)
		if err != nil {
			writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		camlog("info", "svc_site_analyze", map[string]any{"site_id": sc.SiteID, "started": started})
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "started": started})
	}
}

// ─────────────────────────── group I — investigation cancel ───────────────────────────
// Closes a queued/settled investigation. Only the cancellable states
// (queued/awaiting_operator/answered/exhausted) can be closed; a `running` pass is
// refused ({ok:false}) — cooperative cancel of a live pass is deliberately out of
// scope, and the CAS in cancelCameraInvestigation never touches a row a worker owns.
// A won CAS means no worker will claim it (claimCameraInvestigation only claims from
// 'queued') and the reply paths already refuse to reopen a `closed` row, so closing
// is safe without cooperating with any live goroutine. NO settle webhook is fired.

// handleSvcInvestigationCancel serves POST /api/cameras/investigations/cancel {id}.
// Already-closed → {ok:true, status:"closed"} (idempotent); running →
// {ok:false, error}; a successful close appends a system transcript note and returns
// {ok:true, status:"closed"}. A cross-site (or missing) id is the uniform 404.
func handleSvcInvestigationCancel(cfg config) camSvcHandler {
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
		inv, gerr := getCameraInvestigation(db, strings.TrimSpace(body.ID))
		if gerr != nil || inv.SiteID != sc.SiteID {
			camSvcNotFound(w)
			return
		}
		switch inv.Status {
		case "closed":
			// Idempotent: already in the terminal state the caller wants.
			writeJSON(w, http.StatusOK, map[string]any{"ok": true, "status": "closed"})
			return
		case "running":
			// A worker owns the row; cooperative cancel of a live pass is out of scope.
			writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": "investigation is running — try again when it pauses"})
			return
		}
		won, cerr := cancelCameraInvestigation(db, inv.ID)
		if cerr != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": cerr.Error()})
			return
		}
		if !won {
			// A concurrent transition (worker claim, operator reply, another cancel) moved
			// the row out of the cancellable set between our read and the CAS — re-read and
			// report the status that actually won, so the caller sees the true state.
			cur, rerr := getCameraInvestigation(db, inv.ID)
			if rerr != nil {
				camSvcNotFound(w)
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{"ok": false, "status": cur.Status})
			return
		}
		// Won the CAS: record why it closed (no settle webhook — cancel is not a settle).
		camAppendInvestigateMessage(db, inv.ID, "system", "closed by service API", "", "", 0, nil)
		camlog("info", "svc_investigate_cancel", map[string]any{"investigation_id": inv.ID, "site_id": sc.SiteID, "prev_status": inv.Status})
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "status": "closed"})
	}
}
