package main

// camera_serviceapi_sync.go — the Connect-facing DR (disaster-recovery) sync
// surface (site scope), registered in registerCameraServiceRoutes next to the
// activity routes:
//
//   /api/cameras/sync/manifest   GET — index-only {id, updated_at} projections
//       of every restorable entity type plus the callback state and the
//       activity sync counter. Connect's mirror diffs these against its own
//       rows, re-fetches changed entities through the EXISTING list routes,
//       and treats id-set differences as deletes (tombstones). No payloads,
//       no secrets — the manifest is safe to poll frequently.
//   /api/cameras/sync/export     GET — the site's secrets (DVR credentials,
//       API-tool bearer secrets, watch-action tokens) DECRYPTED server-side so
//       a restore onto a fresh proxy can re-create devices/tools/actions
//       without manual re-entry (partial DR would be silent DR). Gated by
//       cfg.CameraSyncExportSecrets (PROXY_CAMERA_SYNC_EXPORT_SECRETS, default
//       on); when off the route answers an explicit 403 — NOT the uniform
//       404 — so an operator sees the knob, not a typo, blocked the export.
//       Every call (allowed or denied) is camlog'd as an audit line.
//
// TRUST BOUNDARY: a site token already commands snapshots, clips and DVR
// reconfiguration, and registers the callback HMAC secret — exporting the same
// site's secrets over the same TLS+Bearer channel does not extend a stolen
// token's blast radius. The export route is the ONE deliberate exemption from
// the "encrypted columns have no code path into JSON" rule (camerahttp.go);
// every other route keeps echoing masked previews / has_secret only.
//
// RESTORE NATURAL KEYS (the contract Connect's restore service builds on):
// camera ids are minted at discovery and never survive a proxy rebuild, so
// cameras are matched by (dvr_id, channel); DVRs match by (host, port),
// avatars and playbooks by name. Out of scope by design: motion-event history,
// captures/media files, watch runs and investigation history (history, not
// config — the activity log is archived through /api/cameras/activity/sync),
// the S3 frame archive (bucket config is env-side) and the proxy .env itself
// (ops-side restore).

import (
	"database/sql"
	"errors"
	"net/http"
)

// ─────────────────────────── projection helpers ───────────────────────────

// camSyncIDRows runs an index-only two-column projection (id, updated_at) and
// hand-builds the manifest rows. The motion-arm section reuses it with
// camera_id in the id position.
func camSyncIDRows(db *sql.DB, query string, args ...any) ([]map[string]any, error) {
	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]map[string]any, 0, 16)
	for rows.Next() {
		var id, updatedAt string
		if err := rows.Scan(&id, &updatedAt); err != nil {
			return nil, err
		}
		out = append(out, map[string]any{"id": id, "updated_at": updatedAt})
	}
	return out, rows.Err()
}

// camSyncChildRows is camSyncIDRows for the immutable child tables (avatar
// photos, playbook media): (id, parent_id, created_at) — created_at suffices
// because those rows are never updated, only inserted and deleted.
func camSyncChildRows(db *sql.DB, query string, args ...any) ([]map[string]any, error) {
	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]map[string]any, 0, 16)
	for rows.Next() {
		var id, parentID, createdAt string
		if err := rows.Scan(&id, &parentID, &createdAt); err != nil {
			return nil, err
		}
		out = append(out, map[string]any{"id": id, "parent_id": parentID, "created_at": createdAt})
	}
	return out, rows.Err()
}

// ─────────────────────────── the manifest ───────────────────────────

// handleSvcSyncManifest serves GET /api/cameras/sync/manifest → {site:{id,
// updated_at}, dvrs|cameras|avatars|playbooks|watches|log_streams|apitools:
// [{id,updated_at}], avatar_photos|playbook_media:[{id,parent_id,created_at}],
// motion_arm:[{id=camera_id,updated_at}], callback:{configured,updated_at},
// activity_max_seq, at}. Everything is an index-only projection — Connect
// diffs updated_at per id and re-fetches only what changed via the existing
// list routes. motion_arm is queried directly because camMotionArmView (the
// svc list shape) deliberately omits updated_at. Note avatars.updated_at also
// bumps on centroid recompute — a harmless spurious refetch.
func handleSvcSyncManifest(cfg config) camSvcHandler {
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
		var siteUpdated string
		err := db.QueryRow(`SELECT updated_at FROM camera_sites WHERE id = ?`, sc.SiteID).Scan(&siteUpdated)
		if errors.Is(err, sql.ErrNoRows) {
			camSvcNotFound(w) // deprovision race: the token outlived its site
			return
		}
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		out := map[string]any{
			"site": map[string]any{"id": sc.SiteID, "updated_at": siteUpdated},
			"at":   nowRFC3339(),
		}
		idSections := []struct{ key, query string }{
			{"dvrs", `SELECT id, updated_at FROM camera_dvrs WHERE site_id = ? ORDER BY id`},
			{"cameras", `SELECT id, updated_at FROM cameras WHERE site_id = ? ORDER BY id`},
			{"avatars", `SELECT id, updated_at FROM camera_avatars WHERE site_id = ? ORDER BY id`},
			{"playbooks", `SELECT id, updated_at FROM camera_playbooks WHERE site_id = ? ORDER BY id`},
			{"watches", `SELECT id, updated_at FROM camera_watches WHERE site_id = ? ORDER BY id`},
			{"log_streams", `SELECT id, updated_at FROM camera_log_streams WHERE site_id = ? ORDER BY id`},
			{"apitools", `SELECT id, updated_at FROM camera_api_tools WHERE site_id = ? ORDER BY id`},
			// One arm row per camera — camera_id doubles as the row identity
			// (UNIQUE(site_id, camera_id)), so it rides the id position.
			{"motion_arm", `SELECT camera_id, updated_at FROM camera_motion_arm WHERE site_id = ? ORDER BY camera_id`},
		}
		for _, s := range idSections {
			rows, qerr := camSyncIDRows(db, s.query, sc.SiteID)
			if qerr != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]any{"error": qerr.Error()})
				return
			}
			out[s.key] = rows
		}
		childSections := []struct{ key, query string }{
			{"avatar_photos", `SELECT m.id, m.avatar_id, m.created_at FROM camera_avatar_media m
				JOIN camera_avatars a ON a.id = m.avatar_id WHERE a.site_id = ? ORDER BY m.id`},
			{"playbook_media", `SELECT m.id, m.playbook_id, m.created_at FROM camera_playbook_media m
				JOIN camera_playbooks p ON p.id = m.playbook_id WHERE p.site_id = ? ORDER BY m.id`},
		}
		for _, s := range childSections {
			rows, qerr := camSyncChildRows(db, s.query, sc.SiteID)
			if qerr != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]any{"error": qerr.Error()})
				return
			}
			out[s.key] = rows
		}
		callback := map[string]any{"configured": false, "updated_at": ""}
		var cbUpdated string
		cerr := db.QueryRow(`SELECT updated_at FROM camera_site_callbacks WHERE site_id = ?`, sc.SiteID).Scan(&cbUpdated)
		if cerr != nil && !errors.Is(cerr, sql.ErrNoRows) {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": cerr.Error()})
			return
		}
		if cerr == nil {
			callback["configured"] = true
			callback["updated_at"] = cbUpdated
		}
		out["callback"] = callback
		maxSeq, merr := camSyncMaxSeq(db)
		if merr != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": merr.Error()})
			return
		}
		out["activity_max_seq"] = maxSeq
		writeJSON(w, http.StatusOK, out)
	}
}

// ─────────────────────────── the secrets export ───────────────────────────

// handleSvcSyncExport serves GET /api/cameras/sync/export → {dvrs:[{id,
// username,password}], apitools:[{id,auth_secret}], watch_actions:[{watch_id,
// token}], at} — every secret decrypted server-side. apitools/watch_actions
// list only rows that HAVE a secret (an absent row means "nothing to carry",
// not "unknown"). A decrypt failure is a loud 500, never a silent "" — a
// partial export would make disaster recovery silently incomplete. Gated by
// cfg.CameraSyncExportSecrets (403 when off); each call is camlog'd.
func handleSvcSyncExport(cfg config) camSvcHandler {
	return func(w http.ResponseWriter, r *http.Request, sc camSvcCtx) {
		if r.Method != http.MethodGet {
			camSvcMethodNotAllowed(w)
			return
		}
		if !cfg.CameraSyncExportSecrets {
			camlog("warn", "svc_sync_export_denied", map[string]any{"site_id": sc.SiteID, "token_id": sc.TokenID})
			writeJSON(w, http.StatusForbidden, map[string]any{
				"error": "secrets export is disabled on this proxy (PROXY_CAMERA_SYNC_EXPORT_SECRETS=false)",
			})
			return
		}
		db := camSvcOpenDB(cfg, w)
		if db == nil {
			return
		}
		defer db.Close()

		dvrs := make([]map[string]any, 0, 4)
		rows, err := db.Query(`SELECT id, username, password_enc FROM camera_dvrs WHERE site_id = ? ORDER BY id`, sc.SiteID)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		for rows.Next() {
			var id, username, enc string
			if serr := rows.Scan(&id, &username, &enc); serr != nil {
				rows.Close()
				writeJSON(w, http.StatusInternalServerError, map[string]any{"error": serr.Error()})
				return
			}
			password, derr := decryptSecret(cfg, enc)
			if derr != nil {
				rows.Close()
				writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "decrypt dvr password for " + id + ": " + derr.Error()})
				return
			}
			dvrs = append(dvrs, map[string]any{"id": id, "username": username, "password": password})
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		rows.Close()

		apitools := make([]map[string]any, 0, 4)
		rows, err = db.Query(`SELECT id, auth_secret_enc FROM camera_api_tools WHERE site_id = ? AND auth_secret_enc != '' ORDER BY id`, sc.SiteID)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		for rows.Next() {
			var id, enc string
			if serr := rows.Scan(&id, &enc); serr != nil {
				rows.Close()
				writeJSON(w, http.StatusInternalServerError, map[string]any{"error": serr.Error()})
				return
			}
			secret, derr := decryptSecret(cfg, enc)
			if derr != nil {
				rows.Close()
				writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "decrypt api-tool secret for " + id + ": " + derr.Error()})
				return
			}
			apitools = append(apitools, map[string]any{"id": id, "auth_secret": secret})
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		rows.Close()

		watchActions := make([]map[string]any, 0, 4)
		rows, err = db.Query(`SELECT id, action_json FROM camera_watches WHERE site_id = ? ORDER BY id`, sc.SiteID)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		for rows.Next() {
			var id, actionJSON string
			if serr := rows.Scan(&id, &actionJSON); serr != nil {
				rows.Close()
				writeJSON(w, http.StatusInternalServerError, map[string]any{"error": serr.Error()})
				return
			}
			ac := parseCamActionConfig(actionJSON)
			if ac.TokenEnc == "" {
				continue // secretless action — nothing to carry
			}
			token, derr := decryptSecret(cfg, ac.TokenEnc)
			if derr != nil {
				rows.Close()
				writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "decrypt watch-action token for " + id + ": " + derr.Error()})
				return
			}
			watchActions = append(watchActions, map[string]any{"watch_id": id, "token": token})
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		rows.Close()

		camlog("info", "svc_sync_export", map[string]any{
			"site_id": sc.SiteID, "token_id": sc.TokenID,
			"dvrs": len(dvrs), "apitools": len(apitools), "watch_actions": len(watchActions),
		})
		writeJSON(w, http.StatusOK, map[string]any{
			"dvrs": dvrs, "apitools": apitools, "watch_actions": watchActions, "at": nowRFC3339(),
		})
	}
}
