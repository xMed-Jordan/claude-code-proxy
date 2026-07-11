package main

// camerahttp.go (WP11) — registerCameraRoutes wires every /ui/api/cameras*
// control-panel endpoint (sites, DVRs + discovery, cameras + on-demand
// snapshot/clip, questions, watches + manual run, run history, captures, and a
// diagnostics summary) plus the admin-gated /camera/media/ serving route
// (handleCameraMedia, cameramedia.go). WP13 (camera_diag.go) adds the
// /ui/api/cameras/health, /events, and /recapture diagnostics endpoints;
// registerCameraRoutes below wires those too so all camera routes stay in one
// place, but their handlers live in camera_diag.go.
//
// Every handler follows the existing control-panel idiom (handleUIProviderKeys,
// main.go:~7433): a fresh openProxyDB per request, defer Close, writeJSON
// responses. All routes are wrapped noStore(requireAdmin(cfg, ...)) — this
// whole subsystem is admin-only (DVR credentials, CCTV footage). Store rows
// have no json tags (camera_store.go, WP0), so every row is mapped to a
// snake_case map[string]any here rather than marshaled directly, and any
// encrypted secret (DVR password, action token) is never echoed back — only a
// masked preview / a boolean.
//
// OBSERVABILITY: every mutating action camlog's the operation (ids, not
// secrets); reads are cheap enough not to need it. captureSnapshot/
// captureLiveClip/capturePlaybackClip/brand.Discover already camlog the masked
// external command + full ffmpeg stderr on failure — this layer only logs the
// higher-level "what did the operator ask for" line.
import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// registerCameraRoutes mounts the camera control-panel API and the media
// serving route onto the shared proxy mux. Called once from newProxyMux.
func registerCameraRoutes(cfg config, mux *http.ServeMux) {
	mux.HandleFunc("/ui/api/cameras/sites", noStore(requireAdmin(cfg, handleCameraSites(cfg))))
	mux.HandleFunc("/ui/api/cameras/sites/update", noStore(requireAdmin(cfg, handleCameraSiteUpdate(cfg))))
	mux.HandleFunc("/ui/api/cameras/sites/delete", noStore(requireAdmin(cfg, handleCameraSiteDelete(cfg))))
	mux.HandleFunc("/ui/api/cameras/sites/analyze", noStore(requireAdmin(cfg, handleCameraSiteAnalyze(cfg))))

	mux.HandleFunc("/ui/api/cameras/dvrs", noStore(requireAdmin(cfg, handleCameraDVRs(cfg))))
	mux.HandleFunc("/ui/api/cameras/dvrs/update", noStore(requireAdmin(cfg, handleCameraDVRUpdate(cfg))))
	mux.HandleFunc("/ui/api/cameras/dvrs/delete", noStore(requireAdmin(cfg, handleCameraDVRDelete(cfg))))
	mux.HandleFunc("/ui/api/cameras/dvrs/toggle", noStore(requireAdmin(cfg, handleCameraDVRToggle(cfg))))
	mux.HandleFunc("/ui/api/cameras/dvrs/discover", noStore(requireAdmin(cfg, handleCameraDVRDiscover(cfg))))

	mux.HandleFunc("/ui/api/cameras/list", noStore(requireAdmin(cfg, handleCamerasList(cfg))))
	mux.HandleFunc("/ui/api/cameras/update", noStore(requireAdmin(cfg, handleCameraUpdate(cfg))))
	mux.HandleFunc("/ui/api/cameras/snapshot", noStore(requireAdmin(cfg, handleCameraSnapshot(cfg))))
	mux.HandleFunc("/ui/api/cameras/clip", noStore(requireAdmin(cfg, handleCameraClip(cfg))))

	mux.HandleFunc("/ui/api/cameras/questions", noStore(requireAdmin(cfg, handleCameraQuestions(cfg))))
	mux.HandleFunc("/ui/api/cameras/questions/answer", noStore(requireAdmin(cfg, handleCameraQuestionAnswer(cfg))))
	mux.HandleFunc("/ui/api/cameras/questions/dismiss", noStore(requireAdmin(cfg, handleCameraQuestionDismiss(cfg))))

	mux.HandleFunc("/ui/api/cameras/watches", noStore(requireAdmin(cfg, handleCameraWatches(cfg))))
	mux.HandleFunc("/ui/api/cameras/watches/update", noStore(requireAdmin(cfg, handleCameraWatchUpdate(cfg))))
	mux.HandleFunc("/ui/api/cameras/watches/delete", noStore(requireAdmin(cfg, handleCameraWatchDelete(cfg))))
	mux.HandleFunc("/ui/api/cameras/watches/toggle", noStore(requireAdmin(cfg, handleCameraWatchToggle(cfg))))
	mux.HandleFunc("/ui/api/cameras/watches/run", noStore(requireAdmin(cfg, handleCameraWatchRun(cfg))))

	// Ask-AI investigation chat (camera_investigate.go): start a run, reply to an
	// ask_operator pause, list a site's investigations, and fetch a full transcript.
	mux.HandleFunc("/ui/api/cameras/investigations", noStore(requireAdmin(cfg, handleCameraInvestigations(cfg))))
	mux.HandleFunc("/ui/api/cameras/investigations/reply", noStore(requireAdmin(cfg, handleCameraInvestigationReply(cfg))))
	mux.HandleFunc("/ui/api/cameras/investigations/get", noStore(requireAdmin(cfg, handleCameraInvestigationGet(cfg))))

	// Multi-camera evidence-video exports (camera_export.go): enqueue/list/get.
	mux.HandleFunc("/ui/api/cameras/exports", noStore(requireAdmin(cfg, handleCameraExports(cfg))))
	mux.HandleFunc("/ui/api/cameras/exports/get", noStore(requireAdmin(cfg, handleCameraExportGet(cfg))))

	mux.HandleFunc("/ui/api/cameras/runs", noStore(requireAdmin(cfg, handleCameraRuns(cfg))))
	mux.HandleFunc("/ui/api/cameras/runs/get", noStore(requireAdmin(cfg, handleCameraRunGet(cfg))))
	mux.HandleFunc("/ui/api/cameras/captures", noStore(requireAdmin(cfg, handleCameraCaptures(cfg))))
	mux.HandleFunc("/ui/api/cameras/diagnostics", noStore(requireAdmin(cfg, handleCameraDiagnostics(cfg))))

	// WP13 diagnostics: live health/reachability, the filterable events trail,
	// and "re-run a single capture" (camera_diag.go).
	mux.HandleFunc("/ui/api/cameras/health", noStore(requireAdmin(cfg, handleCameraHealth(cfg))))
	mux.HandleFunc("/ui/api/cameras/events", noStore(requireAdmin(cfg, handleCameraEvents(cfg))))
	mux.HandleFunc("/ui/api/cameras/recapture", noStore(requireAdmin(cfg, handleCameraRecapture(cfg))))

	// Knowledge layer: operator playbooks + reference media (camera_playbooks.go)
	// and operator-defined external API tools (camera_apitools.go).
	mux.HandleFunc("/ui/api/cameras/playbooks", noStore(requireAdmin(cfg, handleCameraPlaybooks(cfg))))
	mux.HandleFunc("/ui/api/cameras/playbooks/update", noStore(requireAdmin(cfg, handleCameraPlaybookUpdate(cfg))))
	mux.HandleFunc("/ui/api/cameras/playbooks/delete", noStore(requireAdmin(cfg, handleCameraPlaybookDelete(cfg))))
	mux.HandleFunc("/ui/api/cameras/playbooks/media", noStore(requireAdmin(cfg, handleCameraPlaybookMedia(cfg))))
	mux.HandleFunc("/ui/api/cameras/playbooks/media/delete", noStore(requireAdmin(cfg, handleCameraPlaybookMediaDelete(cfg))))
	mux.HandleFunc("/ui/api/cameras/apitools", noStore(requireAdmin(cfg, handleCameraAPITools(cfg))))
	mux.HandleFunc("/ui/api/cameras/apitools/update", noStore(requireAdmin(cfg, handleCameraAPIToolUpdate(cfg))))
	mux.HandleFunc("/ui/api/cameras/apitools/delete", noStore(requireAdmin(cfg, handleCameraAPIToolDelete(cfg))))
	mux.HandleFunc("/ui/api/cameras/apitools/test", noStore(requireAdmin(cfg, handleCameraAPIToolTest(cfg))))

	// Avatars: registry + reference media (camera_avatars.go), enrollment scans +
	// candidate review (camera_avatar_scan.go handlers live in camera_avatars.go).
	mux.HandleFunc("/ui/api/cameras/avatars", noStore(requireAdmin(cfg, handleCameraAvatars(cfg))))
	mux.HandleFunc("/ui/api/cameras/avatars/update", noStore(requireAdmin(cfg, handleCameraAvatarUpdate(cfg))))
	mux.HandleFunc("/ui/api/cameras/avatars/delete", noStore(requireAdmin(cfg, handleCameraAvatarDelete(cfg))))
	mux.HandleFunc("/ui/api/cameras/avatars/media", noStore(requireAdmin(cfg, handleCameraAvatarMedia(cfg))))
	mux.HandleFunc("/ui/api/cameras/avatars/media/upload", noStore(requireAdmin(cfg, handleCameraAvatarMediaUpload(cfg))))
	mux.HandleFunc("/ui/api/cameras/avatars/media/delete", noStore(requireAdmin(cfg, handleCameraAvatarMediaDelete(cfg))))
	mux.HandleFunc("/ui/api/cameras/avatars/scan", noStore(requireAdmin(cfg, handleCameraAvatarScan(cfg))))
	mux.HandleFunc("/ui/api/cameras/avatars/scans", noStore(requireAdmin(cfg, handleCameraAvatarScans(cfg))))
	mux.HandleFunc("/ui/api/cameras/avatars/candidates", noStore(requireAdmin(cfg, handleCameraAvatarCandidates(cfg))))
	mux.HandleFunc("/ui/api/cameras/avatars/candidates/review", noStore(requireAdmin(cfg, handleCameraAvatarCandidateReview(cfg))))

	// Service tokens for the Connect-facing /api/cameras/* service API
	// (camera_serviceapi.go — the service routes themselves are registered by
	// registerCameraServiceRoutes from newProxyMux).
	mux.HandleFunc("/ui/api/cameras/service-tokens", noStore(requireAdmin(cfg, handleCameraServiceTokens(cfg))))
	mux.HandleFunc("/ui/api/cameras/service-tokens/revoke", noStore(requireAdmin(cfg, handleCameraServiceTokenRevoke(cfg))))

	// Media serving. The gate branches on ?v=<view_token>: a public timeline-page
	// fetch (authorized against the investigation's minted media) vs. the default
	// admin session (handleCameraMediaGated → requireAdmin(handleCameraMedia)).
	mux.HandleFunc("/camera/media/", noStore(handleCameraMediaGated(cfg)))

	// Public, no-admin-session live investigation timeline page + its state.json
	// poll (camera_investigate_view.go). Rate-limited per view-token and per IP;
	// the shell is a static const so an unknown token leaks nothing.
	mux.HandleFunc("/camera/investigations/", noStore(handleCameraInvestigationView(cfg)))

	// Public daily activity-report share page + its state.json
	// (camera_observer_view.go) — same hardening, text-only, read-only.
	mux.HandleFunc("/camera/reports/", noStore(handleCameraReportView(cfg)))
}

// ─────────────────────────────── JSON row mappers ───────────────────────────────
// Store row types (camera_store.go) have no json tags, so every response is
// built as an explicit snake_case map here instead of marshaling the row
// directly — and so any encrypted column (password_enc, action token_enc)
// never has a code path that could leak it.

func siteJSON(s camSite) map[string]any {
	pol := camParseSitePolicy(s.PolicyJSON)
	return map[string]any{
		"id": s.ID, "name": s.Name, "description": s.Description,
		"analysis_alias": s.AnalysisAlias, "analysis_status": s.AnalysisStatus,
		"last_analyzed_at": s.LastAnalyzedAt, "analysis": camDecodeJSON(s.AnalysisJSON),
		"policy": camSitePolicyResponseJSON(pol), "role_vocabulary": camRoleVocabularyJSON(pol),
		"created_at": s.CreatedAt, "updated_at": s.UpdatedAt,
	}
}

func dvrJSON(v camDVRView) map[string]any {
	return map[string]any{
		"id": v.ID, "site_id": v.SiteID, "name": v.Name, "brand": v.Brand,
		"host": v.Host, "port": v.Port, "http_port": v.HTTPPort, "username": v.Username,
		"password_preview": v.PasswordPreview, "timezone": v.Timezone,
		"ai_instructions":     v.AIInstructions,
		"channels_discovered": v.ChannelsDiscovered, "enabled": v.Enabled,
		"last_discovered_at": v.LastDiscoveredAt, "created_at": v.CreatedAt, "updated_at": v.UpdatedAt,
	}
}

func dvrListJSON(dvrs []camDVRView) []map[string]any {
	out := make([]map[string]any, 0, len(dvrs))
	for _, d := range dvrs {
		out = append(out, dvrJSON(d))
	}
	return out
}

func cameraJSON(c camera, thumbToken string) map[string]any {
	return map[string]any{
		"id": c.ID, "site_id": c.SiteID, "dvr_id": c.DVRID, "channel": c.Channel,
		"name": c.Name, "area": c.Area, "ai_description": c.AIDescription, "ai_location": c.AILocation,
		"notes":   c.Notes,
		"enabled": c.Enabled, "disabled_reason": c.DisabledReason, "snapshot_token": thumbToken,
		"created_at": c.CreatedAt, "updated_at": c.UpdatedAt,
	}
}

// camSnapshotToken resolves a camera's current-thumbnail capability token. The
// stamped snapshot_capture_id pointer wins while its capture row is alive, but
// those captures expire and get reaped (site analysis runs rarely), so fall
// back to the camera's most recent unexpired still image — scan frames and
// motion snapshots keep grid thumbnails alive between explicit snapshots.
// "" when the camera has no servable imagery at all.
func camSnapshotToken(db *sql.DB, c camera) string {
	if id := strings.TrimSpace(c.SnapshotCaptureID); id != "" {
		if cap, err := getCameraCapture(db, id); err == nil {
			return cap.Token
		}
	}
	var token string
	err := db.QueryRow(`SELECT token FROM camera_captures
		WHERE camera_id = ? AND kind IN ('snapshot', 'frame')
		AND (expires_at = '' OR expires_at > ?)
		ORDER BY created_at DESC LIMIT 1`, c.ID, nowRFC3339()).Scan(&token)
	if err != nil {
		return ""
	}
	return token
}

func watchJSON(w watch) map[string]any {
	ac := parseCamActionConfig(w.ActionJSON)
	return map[string]any{
		"id": w.ID, "site_id": w.SiteID, "name": w.Name, "instruction": w.Instruction,
		"camera_ids": camParseIDArray(w.CameraIDs), "analysis_alias": w.AnalysisAlias,
		"mode": w.Mode, "interval_seconds": w.IntervalSeconds, "active_hours": w.ActiveHours,
		"max_rounds": w.MaxRounds,
		"action": map[string]any{
			"type": ac.Type, "url": ac.URL, "has_secret": strings.TrimSpace(ac.TokenEnc) != "",
		},
		"enabled": w.Enabled, "next_run_at": w.NextRunAt, "last_run_at": w.LastRunAt,
		"created_at": w.CreatedAt, "updated_at": w.UpdatedAt,
	}
}

func questionJSON(q camQuestion) map[string]any {
	return map[string]any{
		"id": q.ID, "site_id": q.SiteID, "analysis_run_id": q.AnalysisRunID,
		"camera_ids": camParseIDArray(q.CameraIDs), "question": q.Question, "answer": q.Answer,
		"status": q.Status, "asked_at": q.AskedAt, "answered_at": q.AnsweredAt,
	}
}

func runJSON(r camWatchRun, includeDetail bool) map[string]any {
	out := map[string]any{
		"id": r.ID, "watch_id": r.WatchID, "site_id": r.SiteID, "started_at": r.StartedAt,
		"finished_at": r.FinishedAt, "status": r.Status, "suspicion": r.Suspicion,
		"rule_status": r.RuleStatus, "rounds": r.Rounds, "summary": r.Summary,
		"fired_action": r.FiredAction, "error": r.Error, "created_at": r.CreatedAt,
	}
	if includeDetail {
		out["transcript"] = camDecodeJSON(r.TranscriptJSON)
		out["action_result"] = camDecodeJSON(r.ActionResult)
	}
	return out
}

func captureJSON(c camCapture, cfg config, r *http.Request) map[string]any {
	return map[string]any{
		"id": c.ID, "site_id": c.SiteID, "camera_id": c.CameraID, "watch_run_id": c.WatchRunID,
		"kind": c.Kind, "quality": c.Quality, "content_type": c.ContentType,
		"width": c.Width, "height": c.Height, "bytes": c.Bytes,
		"from_ts": c.FromTS, "to_ts": c.ToTS, "created_at": c.CreatedAt, "expires_at": c.ExpiresAt,
		"url": camMediaURL(cfg, r, c.Token),
	}
}

func camEventJSON(e camEvent) map[string]any {
	return map[string]any{
		"id": e.ID, "ts": e.TS, "level": e.Level, "dvr_id": e.DVRID, "camera_id": e.CameraID,
		"watch_id": e.WatchID, "run_id": e.RunID, "op": e.Op, "ok": e.OK,
		"latency_ms": e.LatencyMs, "detail": camDecodeJSON(e.Detail), "error": e.Error,
	}
}

func camEventsJSON(events []camEvent) []map[string]any {
	out := make([]map[string]any, 0, len(events))
	for _, e := range events {
		out = append(out, camEventJSON(e))
	}
	return out
}

// camDecodeJSON best-effort decodes a stored JSON text column for embedding in
// a response ("" or invalid → nil, never an error the caller has to handle).
func camDecodeJSON(raw string) any {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	var v any
	if json.Unmarshal([]byte(raw), &v) == nil {
		return v
	}
	return nil
}

// ─────────────────────────────── sites ───────────────────────────────

func handleCameraSites(cfg config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		db, err := openProxyDB(cfg)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		defer db.Close()
		switch r.Method {
		case http.MethodGet:
			sites, err := listCameraSites(db)
			if err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
				return
			}
			out := make([]map[string]any, 0, len(sites))
			for _, s := range sites {
				out = append(out, siteJSON(s))
			}
			writeJSON(w, http.StatusOK, map[string]any{"sites": out})
		case http.MethodPost:
			var body struct {
				Name          string `json:"name"`
				Description   string `json:"description"`
				AnalysisAlias string `json:"analysis_alias"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid JSON"})
				return
			}
			name := strings.TrimSpace(body.Name)
			if name == "" {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": "name is required"})
				return
			}
			id, err := insertCameraSite(db, camSite{
				Name: name, Description: strings.TrimSpace(body.Description),
				AnalysisAlias: strings.TrimSpace(body.AnalysisAlias),
			})
			if err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
				return
			}
			camlog("info", "site_create", map[string]any{"site_id": id, "name": name})
			site, _ := getCameraSite(db, id)
			writeJSON(w, http.StatusOK, map[string]any{"ok": true, "site": siteJSON(site)})
		default:
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		}
	}
}

func handleCameraSiteUpdate(cfg config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
			return
		}
		var body struct {
			ID            string         `json:"id"`
			Name          string         `json:"name"`
			Description   string         `json:"description"`
			AnalysisAlias string         `json:"analysis_alias"`
			Policy        *camSitePolicy `json:"policy"`
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
		site, gerr := getCameraSite(db, id)
		if gerr != nil {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "site not found"})
			return
		}
		site.Name = strings.TrimSpace(firstNonEmpty(body.Name, site.Name))
		site.Description = body.Description
		site.AnalysisAlias = strings.TrimSpace(body.AnalysisAlias)
		// Validate the policy BEFORE any write so a 400 never leaves a partial
		// update behind (name/description persisted, policy rejected).
		if body.Policy != nil {
			if problem := camNormalizeSitePolicy(body.Policy); problem != "" {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": problem})
				return
			}
		}
		if err := updateCameraSite(db, site); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		if body.Policy != nil {
			if err := updateCameraSitePolicy(db, id, camSitePolicyJSON(*body.Policy)); err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
				return
			}
		}
		camlog("info", "site_update", map[string]any{"site_id": id})
		site, _ = getCameraSite(db, id)
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "site": siteJSON(site)})
	}
}

func handleCameraSiteDelete(cfg config) http.HandlerFunc {
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
		removed := camCascadeDeleteSite(db, cfg, id)
		camlog("info", "site_delete", map[string]any{"site_id": id, "dvrs_removed": removed})
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	}
}

// camCascadeDeleteSite removes a site's DVRs, their cameras, its watches, and all
// of the site-scoped feature data (playbooks, external API tools, avatars with
// their pinned reference captures + candidates + scans, the settle-callback row,
// and any service tokens) before the site row itself, reusing the granular
// delete helpers so pinned capture rows AND files are released — otherwise
// avatar/playbook reference images (expires_at=”) survive every reaper forever.
// Historical camera_watch_runs/camera_questions/investigation rows are left as-is
// (records of what already happened); expired capture files are still reaped.
func camCascadeDeleteSite(db *sql.DB, cfg config, siteID string) int {
	dvrs, _ := listCameraDVRViews(db, siteID)
	for _, d := range dvrs {
		cams, _ := listCamerasByDVR(db, d.ID)
		for _, c := range cams {
			_ = deleteCamera(db, c.ID)
		}
		_ = deleteCameraDVR(db, d.ID)
	}
	watches, _ := listCameraWatches(db, siteID)
	for _, wt := range watches {
		_ = deleteCameraWatch(db, wt.ID)
	}
	// Avatars: cascades avatar_media (+ pinned capture rows/files), candidates,
	// and scans via the granular helper.
	if avatars, err := listCameraAvatars(db, siteID, true); err == nil {
		for _, av := range avatars {
			_ = deleteCameraAvatar(db, cfg, av.ID)
		}
	}
	// Playbooks: cascades playbook_media and releases/unpins their captures.
	if playbooks, err := listCameraPlaybooks(db, siteID, false); err == nil {
		for _, pb := range playbooks {
			_ = deleteCameraPlaybook(db, cfg, pb.ID)
		}
	}
	// External API tools (rows carry encrypted bearer secrets).
	if tools, err := listCameraAPITools(db, siteID, false); err == nil {
		for _, t := range tools {
			_ = deleteCameraAPITool(db, t.ID)
		}
	}
	// Settle callback + any service tokens minted for this site.
	_, _ = db.Exec(`DELETE FROM camera_site_callbacks WHERE site_id = ?`, siteID)
	_, _ = db.Exec(`UPDATE camera_service_tokens SET enabled = 0, updated_at = ? WHERE site_id = ?`, nowRFC3339(), siteID)
	_ = deleteCameraSite(db, siteID)
	return len(dvrs)
}

func handleCameraSiteAnalyze(cfg config) http.HandlerFunc {
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
		started, err := startSiteAnalysis(cfg, id)
		if err != nil {
			writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		camlog("info", "site_analyze_request", map[string]any{"site_id": id, "started": started})
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "started": started})
	}
}

// ─────────────────────────────── DVRs ───────────────────────────────

func camValidBrand(brand string) bool {
	_, ok := brandRegistry[brand]
	return ok
}

func handleCameraDVRs(cfg config) http.HandlerFunc {
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
			dvrs, err := listCameraDVRViews(db, siteID)
			if err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{"dvrs": dvrListJSON(dvrs)})
		case http.MethodPost:
			var body struct {
				SiteID         string `json:"site_id"`
				Name           string `json:"name"`
				Brand          string `json:"brand"`
				Host           string `json:"host"`
				Port           int    `json:"port"`
				HTTPPort       int    `json:"http_port"`
				Username       string `json:"username"`
				Password       string `json:"password"`
				Timezone       string `json:"timezone"`
				AIInstructions string `json:"ai_instructions"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid JSON"})
				return
			}
			siteID := strings.TrimSpace(body.SiteID)
			host := strings.TrimSpace(body.Host)
			if siteID == "" || host == "" {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": "site_id and host are required"})
				return
			}
			brand := strings.ToLower(strings.TrimSpace(body.Brand))
			if brand != "" && !camValidBrand(brand) {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": "unknown brand " + brand})
				return
			}
			id, err := insertCameraDVR(db, cfg, CamDVR{
				SiteID: siteID, Name: strings.TrimSpace(body.Name), Brand: brand, Host: host,
				Port: body.Port, HTTPPort: body.HTTPPort, Username: strings.TrimSpace(body.Username),
				Password: body.Password, Timezone: strings.TrimSpace(body.Timezone),
				AIInstructions: strings.TrimSpace(body.AIInstructions), Enabled: true,
			})
			if err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
				return
			}
			camlog("info", "dvr_create", map[string]any{"dvr_id": id, "site_id": siteID, "brand": brand, "host": host})
			dvrs, _ := listCameraDVRViews(db, siteID)
			writeJSON(w, http.StatusOK, map[string]any{"ok": true, "id": id, "dvrs": dvrListJSON(dvrs)})
		default:
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		}
	}
}

func handleCameraDVRUpdate(cfg config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
			return
		}
		var body struct {
			ID       string `json:"id"`
			Name     string `json:"name"`
			Brand    string `json:"brand"`
			Host     string `json:"host"`
			Port     int    `json:"port"`
			HTTPPort int    `json:"http_port"`
			Username string `json:"username"`
			Password string `json:"password"`
			Timezone string `json:"timezone"`
			// Pointer so an operator can CLEAR the instructions (the "non-empty
			// means set" idiom would make clearing impossible).
			AIInstructions *string `json:"ai_instructions"`
			Enabled        *bool   `json:"enabled"`
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
		dvr, gerr := getCameraDVR(db, cfg, id)
		if gerr != nil {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "dvr not found"})
			return
		}
		if n := strings.TrimSpace(body.Name); n != "" {
			dvr.Name = n
		}
		if b := strings.ToLower(strings.TrimSpace(body.Brand)); b != "" {
			if !camValidBrand(b) {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": "unknown brand " + b})
				return
			}
			dvr.Brand = b
		}
		if h := strings.TrimSpace(body.Host); h != "" {
			dvr.Host = h
		}
		if body.Port > 0 {
			dvr.Port = body.Port
		}
		if body.HTTPPort > 0 {
			dvr.HTTPPort = body.HTTPPort
		}
		if u := strings.TrimSpace(body.Username); u != "" {
			dvr.Username = u
		}
		if tz := strings.TrimSpace(body.Timezone); tz != "" {
			dvr.Timezone = tz
		}
		if body.AIInstructions != nil {
			dvr.AIInstructions = strings.TrimSpace(*body.AIInstructions)
		}
		if body.Enabled != nil {
			dvr.Enabled = *body.Enabled
		}
		changePassword := strings.TrimSpace(body.Password) != ""
		if changePassword {
			dvr.Password = body.Password
		}
		if err := updateCameraDVR(db, cfg, dvr, changePassword); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		camlog("info", "dvr_update", map[string]any{"dvr_id": id, "password_changed": changePassword})
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	}
}

func handleCameraDVRDelete(cfg config) http.HandlerFunc {
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
		cams, _ := listCamerasByDVR(db, id)
		for _, c := range cams {
			_ = deleteCamera(db, c.ID)
		}
		if err := deleteCameraDVR(db, id); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		camlog("info", "dvr_delete", map[string]any{"dvr_id": id, "cameras_removed": len(cams)})
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	}
}

func handleCameraDVRToggle(cfg config) http.HandlerFunc {
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
		if err := setDVREnabled(db, id, body.Enabled); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		camlog("info", "dvr_toggle", map[string]any{"dvr_id": id, "enabled": body.Enabled})
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	}
}

func handleCameraDVRDiscover(cfg config) http.HandlerFunc {
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
		dvr, gerr := getCameraDVR(db, cfg, id)
		if gerr != nil {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "dvr not found"})
			return
		}
		channels, added, updated, derr := runCameraDiscovery(r.Context(), cfg, db, dvr)
		if derr != nil {
			writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": derr.Error()})
			return
		}
		cams, _ := listCamerasByDVR(db, id)
		out := make([]map[string]any, 0, len(cams))
		for _, c := range cams {
			out = append(out, cameraJSON(c, camSnapshotToken(db, c)))
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"ok": true, "channels_found": len(channels), "added": added, "updated": updated, "cameras": out,
		})
	}
}

// runCameraDiscovery probes a DVR for its real channel list via the matching
// brand adapter and upserts each channel into the cameras table — NEVER
// assuming contiguous ids, since NVRs renumber/skip channels. Existing cameras
// are left alone except for a device-name refresh, and only when the operator
// hasn't already renamed the camera away from the generic "Channel N" default.
func runCameraDiscovery(ctx context.Context, cfg config, db *sql.DB, dvr CamDVR) ([]CamChannel, int, int, error) {
	brand := brandFor(dvr.Brand)
	if brand == nil {
		return nil, 0, 0, fmt.Errorf("no adapter available for brand %q", dvr.Brand)
	}
	timeout := cfg.CameraDiscoverTimeout
	if timeout <= 0 {
		timeout = 20 * time.Second
	}
	dctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	start := time.Now()
	channels, err := brand.Discover(dctx, cfg, dvr)
	if err != nil {
		camlog("error", "discover", map[string]any{
			"dvr_id": dvr.ID, "site_id": dvr.SiteID, "brand": brand.Name(), "host": dvr.Host,
			"ok": false, "error": err.Error(), "latency_ms": time.Since(start).Milliseconds(),
		})
		return nil, 0, 0, err
	}
	if max := cfg.CameraDiscoverMaxChannels; max > 0 && len(channels) > max {
		channels = channels[:max]
	}

	added, updated := 0, 0
	for _, ch := range channels {
		existing, gerr := findCameraByChannel(db, dvr.ID, ch.Channel)
		switch {
		case errors.Is(gerr, sql.ErrNoRows):
			name := strings.TrimSpace(ch.Name)
			if name == "" {
				name = fmt.Sprintf("Channel %d", ch.Channel)
			}
			if _, ierr := insertCamera(db, camera{SiteID: dvr.SiteID, DVRID: dvr.ID, Channel: ch.Channel, Name: name, Enabled: true}); ierr == nil {
				added++
			} else {
				camlog("warn", "discover_upsert", map[string]any{"dvr_id": dvr.ID, "channel": ch.Channel, "ok": false, "error": ierr.Error()})
			}
		case gerr != nil:
			camlog("warn", "discover_upsert", map[string]any{"dvr_id": dvr.ID, "channel": ch.Channel, "ok": false, "error": gerr.Error()})
		default:
			if name := strings.TrimSpace(ch.Name); name != "" && (existing.Name == "" || existing.Name == fmt.Sprintf("Channel %d", ch.Channel)) {
				existing.Name = name
				if uerr := updateCamera(db, existing); uerr == nil {
					updated++
				}
			}
		}
	}
	_ = setDVRDiscovered(db, dvr.ID, len(channels))
	camlog("info", "discover", map[string]any{
		"dvr_id": dvr.ID, "site_id": dvr.SiteID, "brand": brand.Name(), "host": dvr.Host, "ok": true,
		"channels": len(channels), "added": added, "updated": updated, "latency_ms": time.Since(start).Milliseconds(),
	})
	return channels, added, updated, nil
}

// ─────────────────────────────── cameras ───────────────────────────────

func handleCamerasList(cfg config) http.HandlerFunc {
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

		var (
			cams []camera
			lerr error
		)
		switch {
		case strings.TrimSpace(r.URL.Query().Get("dvr_id")) != "":
			cams, lerr = listCamerasByDVR(db, strings.TrimSpace(r.URL.Query().Get("dvr_id")))
		case strings.TrimSpace(r.URL.Query().Get("site_id")) != "":
			cams, lerr = listCamerasBySite(db, strings.TrimSpace(r.URL.Query().Get("site_id")))
		default:
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "site_id or dvr_id is required"})
			return
		}
		if lerr != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": lerr.Error()})
			return
		}
		out := make([]map[string]any, 0, len(cams))
		for _, c := range cams {
			out = append(out, cameraJSON(c, camSnapshotToken(db, c)))
		}
		writeJSON(w, http.StatusOK, map[string]any{"cameras": out})
	}
}

func handleCameraUpdate(cfg config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
			return
		}
		var body struct {
			ID   string `json:"id"`
			Name string `json:"name"`
			Area string `json:"area"`
			// Pointer so an operator can CLEAR the notes; area keeps its existing
			// unconditional-set behavior untouched.
			Notes   *string `json:"notes"`
			Enabled *bool   `json:"enabled"`
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
		cam, gerr := getCamera(db, id)
		if gerr != nil {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "camera not found"})
			return
		}
		if n := strings.TrimSpace(body.Name); n != "" {
			cam.Name = n
		}
		cam.Area = strings.TrimSpace(body.Area)
		if body.Notes != nil {
			cam.Notes = strings.TrimSpace(*body.Notes)
		}
		if body.Enabled != nil {
			cam.Enabled = *body.Enabled
			if cam.Enabled {
				cam.DisabledReason = "" // an operator override clears any prior auto-disable reason
			}
		}
		if err := updateCamera(db, cam); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		camlog("info", "camera_update", map[string]any{"camera_id": id, "enabled": cam.Enabled})
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "camera": cameraJSON(cam, camSnapshotToken(db, cam))})
	}
}

// camCaptureTimeout bounds an on-demand snapshot request with headroom over
// the internal ffmpeg/HTTP capture timeout.
func camCaptureTimeout(cfg config) time.Duration {
	t := cfg.CameraCaptureTimeout
	if t <= 0 {
		t = 30 * time.Second
	}
	return t + 10*time.Second
}

// camClipRequestTimeout bounds an on-demand clip request with headroom over
// the longest clip the config allows (capturePlaybackClip/captureLiveClip
// clamp the actual encode length internally).
func camClipRequestTimeout(cfg config) time.Duration {
	maxSec := cfg.CameraClipMaxSeconds
	if maxSec <= 0 {
		maxSec = 300
	}
	return time.Duration(maxSec)*time.Second + 60*time.Second
}

// handleCameraSnapshot is the dashboard's on-demand "Live" capture: a fresh
// snapshot right now (the plan's scope decision — no continuous restream).
func handleCameraSnapshot(cfg config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
			return
		}
		var body struct {
			ID      string `json:"id"`
			Quality string `json:"quality"`
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
		cam, gerr := getCamera(db, id)
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
		ctx, cancel := context.WithTimeout(r.Context(), camCaptureTimeout(cfg))
		defer cancel()
		scratch, serr := os.MkdirTemp("", "camsnap-")
		if serr != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": serr.Error()})
			return
		}
		defer os.RemoveAll(scratch)

		res, cerr := captureSnapshot(ctx, cfg, dvr, cam.Channel, q, filepath.Join(scratch, "snap.jpg"))
		if cerr != nil {
			// captureSnapshot already camlog'd the masked command + full ffmpeg stderr.
			writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": cerr.Error()})
			return
		}
		capID, token, perr := persistSnapshotCapture(db, cfg, cam.SiteID, cam.ID, res, q)
		if perr != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": perr.Error()})
			return
		}
		_ = setCameraSnapshot(db, cam.ID, capID) // keep the grid thumbnail pointed at the latest frame
		writeJSON(w, http.StatusOK, map[string]any{
			"ok": true, "url": camMediaURL(cfg, r, token), "width": res.Width, "height": res.Height,
			"content_type": res.ContentType, "quality": q.String(),
		})
	}
}

// handleCameraClip captures a short on-demand clip: a live clip (Seconds from
// now) when From/To are blank, otherwise a past-playback clip for [From,To].
func handleCameraClip(cfg config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
			return
		}
		var body struct {
			ID      string `json:"id"`
			Quality string `json:"quality"`
			Seconds int    `json:"seconds"` // live clip length when From/To are empty
			From    string `json:"from"`    // RFC3339 — playback window start
			To      string `json:"to"`      // RFC3339 — playback window end
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
		cam, gerr := getCamera(db, id)
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

		scratch, serr := os.MkdirTemp("", "camclip-")
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
			"ok": true, "url": camMediaURL(cfg, r, token), "content_type": "video/mp4", "bytes": res.Bytes,
		})
	}
}

// ─────────────────────────────── questions ───────────────────────────────

func handleCameraQuestions(cfg config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
			return
		}
		siteID := strings.TrimSpace(r.URL.Query().Get("site_id"))
		if siteID == "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "site_id is required"})
			return
		}
		status := strings.TrimSpace(r.URL.Query().Get("status"))
		db, err := openProxyDB(cfg)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		defer db.Close()
		qs, err := listCameraQuestions(db, siteID, status)
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

func handleCameraQuestionAnswer(cfg config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
			return
		}
		var body struct {
			ID     string `json:"id"`
			Answer string `json:"answer"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid JSON"})
			return
		}
		id := strings.TrimSpace(body.ID)
		answer := strings.TrimSpace(body.Answer)
		if id == "" || answer == "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "id and answer are required"})
			return
		}
		db, err := openProxyDB(cfg)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		defer db.Close()
		if err := answerCameraQuestion(db, id, answer); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		camlog("info", "question_answer", map[string]any{"question_id": id})
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	}
}

func handleCameraQuestionDismiss(cfg config) http.HandlerFunc {
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
		if err := setCameraQuestionStatus(db, id, "dismissed"); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	}
}

// ─────────────────────────────── watches ───────────────────────────────

// camActionRequest is the HTTP-layer shape of a watch's action config: Secret
// is a plaintext bearer token / HMAC signing secret that is encrypted before
// storage and never echoed back (watchJSON only reports has_secret).
type camActionRequest struct {
	Type   string `json:"type"`
	URL    string `json:"url"`
	Secret string `json:"secret"`
}

func encodeWatchAction(cfg config, a *camActionRequest) string {
	if a == nil {
		return ""
	}
	typ := strings.ToLower(strings.TrimSpace(a.Type))
	if typ == "" {
		typ = "log"
	}
	ac := camActionConfig{Type: typ, URL: strings.TrimSpace(a.URL)}
	if s := strings.TrimSpace(a.Secret); s != "" {
		if enc, err := encryptSecret(cfg, s); err == nil {
			ac.TokenEnc = enc
		}
	}
	return mustJSON(ac)
}

// camValidateWatchMode normalizes+validates a watch's mode ("" → "escalate").
func camValidateWatchMode(m string) (string, bool) {
	m = strings.ToLower(strings.TrimSpace(m))
	if m == "" {
		m = "escalate"
	}
	if m != "escalate" && m != "mosaic_only" {
		return "", false
	}
	return m, true
}

func handleCameraWatches(cfg config) http.HandlerFunc {
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
			watches, err := listCameraWatches(db, siteID)
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
				SiteID          string            `json:"site_id"`
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
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid JSON"})
				return
			}
			siteID := strings.TrimSpace(body.SiteID)
			if siteID == "" {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": "site_id is required"})
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
				SiteID: siteID, Name: strings.TrimSpace(body.Name), Instruction: strings.TrimSpace(body.Instruction),
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
			camlog("info", "watch_create", map[string]any{"watch_id": id, "site_id": siteID, "mode": mode, "enabled": enabled})
			wRow, _ := getCameraWatch(db, id)
			writeJSON(w, http.StatusOK, map[string]any{"ok": true, "watch": watchJSON(wRow)})
		default:
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		}
	}
}

func handleCameraWatchUpdate(cfg config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
			return
		}
		var body struct {
			ID              string            `json:"id"`
			Name            string            `json:"name"`
			Instruction     string            `json:"instruction"`
			CameraIDs       []string          `json:"camera_ids"`
			AnalysisAlias   string            `json:"analysis_alias"`
			Mode            string            `json:"mode"`
			IntervalSeconds int               `json:"interval_seconds"`
			ActiveHours     string            `json:"active_hours"`
			MaxRounds       int               `json:"max_rounds"`
			Action          *camActionRequest `json:"action"`
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
		existing, gerr := getCameraWatch(db, id)
		if gerr != nil {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "watch not found"})
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
			mode, ok := camValidateWatchMode(body.Mode)
			if !ok {
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
		if body.Action != nil {
			existing.ActionJSON = encodeWatchAction(cfg, body.Action)
		}
		if err := updateCameraWatch(db, existing); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		camlog("info", "watch_update", map[string]any{"watch_id": id})
		wRow, _ := getCameraWatch(db, id)
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "watch": watchJSON(wRow)})
	}
}

func handleCameraWatchDelete(cfg config) http.HandlerFunc {
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
		if err := deleteCameraWatch(db, id); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		camlog("info", "watch_delete", map[string]any{"watch_id": id})
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	}
}

func handleCameraWatchToggle(cfg config) http.HandlerFunc {
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
		nextRunAt := ""
		if body.Enabled {
			nextRunAt = nowRFC3339() // due immediately so enabling doesn't wait a full interval
		}
		if err := setWatchEnabled(db, id, body.Enabled, nextRunAt); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		camlog("info", "watch_toggle", map[string]any{"watch_id": id, "enabled": body.Enabled})
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	}
}

func handleCameraWatchRun(cfg config) http.HandlerFunc {
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
		wRow, gerr := getCameraWatch(db, id)
		db.Close()
		if gerr != nil {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "watch not found"})
			return
		}
		if !triggerWatchRun(cfg, wRow) {
			writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": "this watch already has a run in progress"})
			return
		}
		camlog("info", "watch_run_manual", map[string]any{"watch_id": id})
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "started": true})
	}
}

// ─────────────────────────────── runs & captures ───────────────────────────────

func handleCameraRuns(cfg config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
			return
		}
		watchID := strings.TrimSpace(r.URL.Query().Get("watch_id"))
		limit := parseIntDefault(r.URL.Query().Get("limit"), 50)
		db, err := openProxyDB(cfg)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		defer db.Close()
		runs, err := listCameraWatchRuns(db, watchID, limit)
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

func handleCameraRunGet(cfg config) http.HandlerFunc {
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
		run, gerr := getCameraWatchRun(db, id)
		if gerr != nil {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "run not found"})
			return
		}
		caps, _ := listCameraCaptures(db, id)
		capOut := make([]map[string]any, 0, len(caps))
		for _, c := range caps {
			capOut = append(capOut, captureJSON(c, cfg, r))
		}
		out := runJSON(run, true)
		out["captures"] = capOut
		writeJSON(w, http.StatusOK, map[string]any{"run": out})
	}
}

func handleCameraCaptures(cfg config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
			return
		}
		runID := strings.TrimSpace(r.URL.Query().Get("run_id"))
		if runID == "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "run_id is required"})
			return
		}
		db, err := openProxyDB(cfg)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		defer db.Close()
		caps, err := listCameraCaptures(db, runID)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		out := make([]map[string]any, 0, len(caps))
		for _, c := range caps {
			out = append(out, captureJSON(c, cfg, r))
		}
		writeJSON(w, http.StatusOK, map[string]any{"captures": out})
	}
}

// ─────────────────────────────── diagnostics ───────────────────────────────

// handleCameraDiagnostics is WP11's baseline diagnostics summary (ffmpeg/
// ffprobe presence+version, recent camera_events, recent failures). WP13 later
// extends this area with a "re-run a single capture" endpoint and folds a
// per-DVR reachability summary into /health.
func handleCameraDiagnostics(cfg config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
			return
		}
		limit := parseIntDefault(r.URL.Query().Get("limit"), 100)
		db, err := openProxyDB(cfg)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		defer db.Close()
		events, _ := listRecentCameraEvents(db, limit)
		failures, _ := listRecentCameraEventFailures(db, limit)
		ffmpegOK, ffmpegVer, ffprobeOK, ffprobeVer := cameraBinaryStatus(cfg)
		writeJSON(w, http.StatusOK, map[string]any{
			"enabled":        cfg.CameraEnabled,
			"orch_mode":      resolveCamOrchMode(cfg),
			"analysis_alias": cfg.CameraAnalysisAlias,
			"ffmpeg":         map[string]any{"ok": ffmpegOK, "version": ffmpegVer, "bin": cameraFFmpegBin(cfg)},
			"ffprobe":        map[string]any{"ok": ffprobeOK, "version": ffprobeVer, "bin": cameraFFprobeBin(cfg)},
			"events":         camEventsJSON(events),
			"failures":       camEventsJSON(failures),
		})
	}
}
