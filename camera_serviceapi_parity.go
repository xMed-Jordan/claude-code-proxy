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
	"database/sql"
	"net/http"
	"strings"
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
