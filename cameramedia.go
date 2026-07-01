package main

// cameramedia.go (WP11) — the served-capture root's hourly reaper and the
// admin-gated /camera/media/<token> serving handler. Mirrors agyimage.go's
// startImageReaper/reapImageRoot + handleGeneratedImage, with two deliberate
// differences:
//
//  1. A capture is looked up by its camera_captures.token column (a DB row
//     that also carries content_type/expires_at/an on-disk path), not trusted
//     from the URL path directly the way handleGeneratedImage validates a
//     filename pattern against the filesystem.
//  2. Serving is ADMIN-GATED — registerCameraRoutes (camerahttp.go) wraps
//     handleCameraMedia in noStore(requireAdmin(...)) — because CCTV footage
//     is sensitive, unlike the public /media/generated/ image serving.
//
// cameraMediaRoot itself lives in camera.go (WP0), since initCameras also
// needs it at startup to create the directory before any capture runs.
import (
	"database/sql"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// startCameraMediaReaper runs hourly regardless of cfg.CameraEnabled: on-demand
// snapshots/clips (the setup/describe phase, the camera grid's "Live" button)
// accumulate independently of whether scheduled monitoring is on, so disk
// hygiene is unconditional. Also prunes the camera_events diagnostics trail
// (mirrors metricsPruner), since this is the only hourly ticker in the camera
// subsystem.
func startCameraMediaReaper(cfg config) {
	root := cameraMediaRoot(cfg)
	if err := os.MkdirAll(root, 0o700); err != nil {
		camlog("error", "media_reap_init", map[string]any{"ok": false, "error": err.Error(), "root": root})
	}
	go func() {
		ticker := time.NewTicker(time.Hour)
		defer ticker.Stop()
		reapCameraMedia(cfg, root)
		for range ticker.C {
			reapCameraMedia(cfg, root)
		}
	}()
}

// reapCameraMedia deletes camera_captures rows whose expires_at has passed,
// removes their on-disk files, sweeps any orphaned file that has no DB row at
// all (defensive: a crash between copyFile and insertCameraCapture can leak
// one file), and prunes old camera_events rows.
func reapCameraMedia(cfg config, root string) {
	db, err := openProxyDB(cfg)
	if err != nil {
		camlog("warn", "media_reap", map[string]any{"ok": false, "error": err.Error()})
		return
	}
	defer db.Close()

	now := nowRFC3339()
	var paths []string
	rows, qerr := db.Query(`SELECT path FROM camera_captures WHERE expires_at != '' AND expires_at <= ?`, now)
	if qerr == nil {
		for rows.Next() {
			var p string
			if rows.Scan(&p) == nil && strings.TrimSpace(p) != "" {
				paths = append(paths, p)
			}
		}
		rows.Close()
	}

	expired, derr := deleteExpiredCameraCaptures(db, now)
	if derr != nil {
		camlog("warn", "media_reap", map[string]any{"ok": false, "error": derr.Error()})
		return
	}
	removed := 0
	for _, p := range paths {
		if os.Remove(p) == nil {
			removed++
		}
	}
	orphans := sweepOrphanCameraMedia(db, root)

	cutoff := time.Now().Add(-cameraEventRetention(cfg)).Format(time.RFC3339)
	evPruned, everr := pruneCameraEvents(db, cutoff)
	if everr != nil {
		camlog("warn", "events_prune", map[string]any{"ok": false, "error": everr.Error()})
	}

	camlog("info", "media_reap", map[string]any{
		"ok": true, "expired_rows": expired, "files_removed": removed,
		"orphans_removed": orphans, "events_pruned": evPruned, "root": root,
	})
}

// sweepOrphanCameraMedia removes files sitting in root with no camera_captures
// row referencing them. Only considers files older than an hour so an
// in-progress capture (file written, DB insert still pending) is never raced.
func sweepOrphanCameraMedia(db *sql.DB, root string) int {
	entries, err := os.ReadDir(root)
	if err != nil {
		return 0
	}
	cutoff := time.Now().Add(-time.Hour)
	removed := 0
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		info, ierr := e.Info()
		if ierr != nil || info.ModTime().After(cutoff) {
			continue
		}
		full := filepath.Join(root, e.Name())
		var exists int
		_ = db.QueryRow(`SELECT 1 FROM camera_captures WHERE path = ? LIMIT 1`, full).Scan(&exists)
		if exists == 0 {
			if os.Remove(full) == nil {
				removed++
			}
		}
	}
	return removed
}

// cameraEventRetention resolves how long camera_events rows are kept before
// reapCameraMedia prunes them. The plan's env-key list has no dedicated
// "events retention" knob, so this reuses PROXY_CAM_MEDIA_RETENTION — both are
// "how long is the evidence/diagnostics trail worth keeping" knobs — falling
// back to 24h.
func cameraEventRetention(cfg config) time.Duration {
	if cfg.CameraMediaRetention > 0 {
		return cfg.CameraMediaRetention
	}
	return 24 * time.Hour
}

// ─────────────────────────────── serving ───────────────────────────────

// handleCameraMedia serves a captured artifact (snapshot/mosaic/clip/frame) by
// its capability token. Registered admin-gated by registerCameraRoutes.
func handleCameraMedia(cfg config) http.HandlerFunc {
	root := cameraMediaRoot(cfg)
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
			return
		}
		token := strings.TrimSpace(strings.TrimPrefix(r.URL.Path, "/camera/media/"))
		if token == "" || strings.ContainsAny(token, "/\\") {
			http.NotFound(w, r)
			return
		}
		db, err := openProxyDB(cfg)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "database unavailable"})
			return
		}
		defer db.Close()
		cap, cerr := getCameraCaptureByToken(db, token)
		if cerr != nil {
			http.NotFound(w, r)
			return
		}
		if cap.ExpiresAt != "" && cap.ExpiresAt <= nowRFC3339() {
			http.NotFound(w, r)
			return
		}
		full := cap.Path
		if rel, rerr := filepath.Rel(root, full); rerr != nil || strings.HasPrefix(rel, "..") {
			camlog("error", "media_serve", map[string]any{"ok": false, "error": "capture path escapes media root", "token": token})
			http.NotFound(w, r)
			return
		}
		fi, serr := os.Stat(full)
		if serr != nil || fi.IsDir() {
			http.NotFound(w, r)
			return
		}
		ct := strings.TrimSpace(cap.ContentType)
		if ct == "" {
			ct = "application/octet-stream"
		}
		w.Header().Set("Content-Type", ct)
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Cache-Control", "private, max-age=3600")
		http.ServeFile(w, r, full)
	}
}

// camMediaURL builds the capability URL for a served capture token, reusing
// agyimage.go's imageBaseURL (PublicURL, else the request's own scheme+host).
func camMediaURL(cfg config, r *http.Request, token string) string {
	return strings.TrimRight(imageBaseURL(cfg, r), "/") + "/camera/media/" + token
}
