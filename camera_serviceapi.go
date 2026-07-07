package main

// camera_serviceapi.go — the Connect-facing bearer-token service API
// (design-connect.md Part A): /api/cameras/* routes registered by
// registerCameraServiceRoutes (called from newProxyMux), plus the per-site
// investigation-settle webhook (camNotifyInvestigationSettled) and the admin
// UI's service-token CRUD.
//
// TOKEN MODEL (camera_service_tokens, schema in migrateCameraDB): plaintext
// `cpsk_<randToken(24)>` shown ONCE at mint; stored as hex(sha256) — a DB leak
// yields nothing. Scopes: "management" (Connect's server registry; ONLY
// provision/deprovision/usage) and "site" (minted per plugin instance;
// everything else, confined to its site_id — site_id is ALWAYS taken from the
// token, NEVER from the request). requireCameraService authenticates
// (constant-time), scope-checks, rate-limits (cfg.CameraServiceRate/min,
// media 5x), stamps last_used_at (throttled), and passes camSvcCtx. 401s use
// one identical body — no oracle.
//
// SETTLE WEBHOOK (camera_site_callbacks): camNotifyInvestigationSettled is
// spawned `go ...` from the answered / awaiting_operator transitions and
// camStopInvestigation (camera_investigate.go — covers every exhausted path
// incl. panic terminalize). Payload {event:"camera_investigation_settled",
// investigation_id, site_id, status, answer, question_for_operator?, turns,
// evidence:[{token, media_url, caption, content_type, kind, camera_id}] cap 10,
// at}; auth = Bearer per-site secret + X-Camera-Signature: sha256=<HMAC(secret,
// body)>; sent via camGuardedHTTPClient (cfg.CameraCallbackAllowPrivate swaps a
// plain client for local dev); retries 0s/30s/5min, at-least-once (Connect's
// poller is the durable net). Every attempt camlog'd (op investigation_callback).

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// camServiceToken is an in-memory view of a camera_service_tokens row
// (id via randomID("svct"); the plaintext is never stored).
type camServiceToken struct {
	ID           string
	Scope        string // management | site
	SiteID       string // required when scope == "site"
	Label        string
	TokenHash    string // hex(sha256(plaintext))
	TokenPreview string // "cpsk_ab…yz" masked preview
	Enabled      bool
	LastUsedAt   string
	CreatedAt    string
	UpdatedAt    string
}

// camSiteCallback is an in-memory view of a camera_site_callbacks row (one
// settle-webhook registration per site; secret_enc via encryptSecret).
type camSiteCallback struct {
	SiteID         string
	URL            string
	SecretEnc      string
	Enabled        bool
	StreamProgress bool // opt-in: also deliver the coalesced camera_investigation_progress webhook (default off)
	CreatedAt      string
	UpdatedAt      string
}

// camSvcCtx is the authenticated identity requireCameraService passes to every
// service handler. SiteID is "" for management tokens.
type camSvcCtx struct {
	TokenID string
	Scope   string // management | site
	SiteID  string
}

// camSvcHandler is the service-API handler shape: a normal handler plus the
// authenticated token context.
type camSvcHandler func(w http.ResponseWriter, r *http.Request, sc camSvcCtx)

// ─────────────────────────── token store ───────────────────────────

// camServiceTokenHash is the storage form of a presented plaintext token.
func camServiceTokenHash(plaintext string) string {
	sum := sha256.Sum256([]byte(plaintext))
	return hex.EncodeToString(sum[:])
}

// mintCameraServiceToken creates a token row and returns it together with the
// PLAINTEXT (`cpsk_<randToken(24)>`) — shown exactly once, never persisted.
func mintCameraServiceToken(db *sql.DB, scope, siteID, label string) (camServiceToken, string, error) {
	scope = strings.TrimSpace(scope)
	siteID = strings.TrimSpace(siteID)
	switch scope {
	case "management":
		siteID = "" // a management token is never site-bound
	case "site":
		if siteID == "" {
			return camServiceToken{}, "", errors.New("site_id is required for a site-scoped token")
		}
	default:
		return camServiceToken{}, "", errors.New(`scope must be "management" or "site"`)
	}
	plaintext := "cpsk_" + randToken(24)
	now := nowRFC3339()
	t := camServiceToken{
		ID: randomID("svct"), Scope: scope, SiteID: siteID, Label: strings.TrimSpace(label),
		TokenHash: camServiceTokenHash(plaintext), TokenPreview: maskSecret(plaintext),
		Enabled: true, CreatedAt: now, UpdatedAt: now,
	}
	_, err := db.Exec(`INSERT INTO camera_service_tokens
		(id, scope, site_id, label, token_hash, token_preview, enabled, last_used_at, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, 1, '', ?, ?)`,
		t.ID, t.Scope, t.SiteID, t.Label, t.TokenHash, t.TokenPreview, t.CreatedAt, t.UpdatedAt)
	if err != nil {
		return camServiceToken{}, "", err
	}
	return t, plaintext, nil
}

func scanCameraServiceToken(s rowScanner) (camServiceToken, error) {
	var t camServiceToken
	var enabled int
	err := s.Scan(&t.ID, &t.Scope, &t.SiteID, &t.Label, &t.TokenHash, &t.TokenPreview,
		&enabled, &t.LastUsedAt, &t.CreatedAt, &t.UpdatedAt)
	t.Enabled = enabled != 0
	return t, err
}

const camServiceTokenCols = `id, scope, site_id, label, token_hash, token_preview, enabled, last_used_at, created_at, updated_at`

// lookupCameraServiceToken resolves a presented plaintext to its enabled row
// via sha256 + constant-time compare (sql.ErrNoRows when absent/disabled).
func lookupCameraServiceToken(db *sql.DB, plaintext string) (camServiceToken, error) {
	plaintext = strings.TrimSpace(plaintext)
	if plaintext == "" {
		return camServiceToken{}, sql.ErrNoRows
	}
	hash := camServiceTokenHash(plaintext)
	t, err := scanCameraServiceToken(db.QueryRow(`SELECT `+camServiceTokenCols+`
		FROM camera_service_tokens WHERE token_hash = ? AND enabled = 1`, hash))
	if err != nil {
		return camServiceToken{}, err
	}
	// Belt-and-suspenders constant-time confirmation of the hash the row was
	// selected by (the SQL equality already matched; this removes any doubt about
	// collation/trimming surprises without ever comparing plaintexts).
	if subtle.ConstantTimeCompare([]byte(t.TokenHash), []byte(hash)) != 1 {
		return camServiceToken{}, sql.ErrNoRows
	}
	return t, nil
}

func listCameraServiceTokens(db *sql.DB) ([]camServiceToken, error) {
	rows, err := db.Query(`SELECT ` + camServiceTokenCols + ` FROM camera_service_tokens ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []camServiceToken
	for rows.Next() {
		t, serr := scanCameraServiceToken(rows)
		if serr != nil {
			return nil, serr
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// revokeCameraServiceToken disables a token (enabled=0) — single-row revocation.
func revokeCameraServiceToken(db *sql.DB, id string) error {
	_, err := db.Exec(`UPDATE camera_service_tokens SET enabled = 0, updated_at = ? WHERE id = ?`, nowRFC3339(), id)
	return err
}

// revokeCameraServiceTokensForSite disables every token bound to a site
// (deprovision path).
func revokeCameraServiceTokensForSite(db *sql.DB, siteID string) error {
	_, err := db.Exec(`UPDATE camera_service_tokens SET enabled = 0, updated_at = ? WHERE site_id = ?`, nowRFC3339(), siteID)
	return err
}

// ─────────────────────────── site callbacks ───────────────────────────

func getCameraSiteCallback(db *sql.DB, siteID string) (camSiteCallback, error) {
	var cb camSiteCallback
	var enabled, streamProgress int
	err := db.QueryRow(`SELECT site_id, url, secret_enc, enabled, stream_progress, created_at, updated_at
		FROM camera_site_callbacks WHERE site_id = ?`, siteID).
		Scan(&cb.SiteID, &cb.URL, &cb.SecretEnc, &enabled, &streamProgress, &cb.CreatedAt, &cb.UpdatedAt)
	cb.Enabled = enabled != 0
	cb.StreamProgress = streamProgress != 0
	return cb, err
}

// setCameraSiteCallback upserts a site's settle-webhook registration (secret is
// plaintext here; encryptSecret'd into secret_enc; "" keeps the existing secret).
// streamProgress opts the site into the coalesced progress webhook.
func setCameraSiteCallback(db *sql.DB, cfg config, siteID, rawURL, secret string, streamProgress bool) error {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return errors.New("url is required")
	}
	if u, err := url.Parse(rawURL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return errors.New("url must be an absolute http(s) URL")
	}
	existing, gerr := getCameraSiteCallback(db, siteID)
	secretEnc := existing.SecretEnc
	if strings.TrimSpace(secret) != "" {
		enc, eerr := encryptSecret(cfg, secret)
		if eerr != nil {
			return eerr
		}
		secretEnc = enc
	}
	sp := 0
	if streamProgress {
		sp = 1
	}
	now := nowRFC3339()
	if gerr != nil { // no row yet
		_, err := db.Exec(`INSERT INTO camera_site_callbacks (site_id, url, secret_enc, enabled, stream_progress, created_at, updated_at)
			VALUES (?, ?, ?, 1, ?, ?, ?)`, siteID, rawURL, secretEnc, sp, now, now)
		return err
	}
	_, err := db.Exec(`UPDATE camera_site_callbacks SET url = ?, secret_enc = ?, enabled = 1, stream_progress = ?, updated_at = ? WHERE site_id = ?`,
		rawURL, secretEnc, sp, now, siteID)
	return err
}

func deleteCameraSiteCallback(db *sql.DB, siteID string) error {
	_, err := db.Exec(`DELETE FROM camera_site_callbacks WHERE site_id = ?`, siteID)
	return err
}

// ─────────────────────────── settle webhook ───────────────────────────

// camSettleEvidenceMax caps the evidence items included in a settle payload.
const camSettleEvidenceMax = 10

// camCallbackRetryDelays is the at-least-once retry ladder (the function runs
// on its own goroutine, so sleeping inline is fine).
var camCallbackRetryDelays = []time.Duration{0, 30 * time.Second, 5 * time.Minute}

// camSettlePayload builds the settle-webhook body (design-connect.md A4) from
// already-loaded rows — pure over its inputs so it is unit-testable. answer is
// the last ai message's content; question_for_operator is the last ask_operator
// question (only when awaiting_operator); turns counts ai messages; evidence is
// every tool-minted artifact (transcript order, de-duped by token, capped at
// camSettleEvidenceMax) whose capture row still exists.
func camSettlePayload(cfg config, inv camInvestigation, msgs []camInvestigationMessage, status string, capByToken map[string]camCapture, at time.Time) map[string]any {
	answer, question := "", ""
	turns := 0
	for _, m := range msgs {
		if m.Role != "ai" {
			continue
		}
		turns++
		answer = m.Content
		if strings.TrimSpace(m.ToolName) == "ask_operator" {
			question = m.Content
		}
	}

	evidence := make([]map[string]any, 0, camSettleEvidenceMax)
	seen := map[string]bool{}
	for _, m := range msgs {
		if m.Role != "tool" || strings.TrimSpace(m.MediaJSON) == "" {
			continue
		}
		var items []evidenceItem
		if json.Unmarshal([]byte(m.MediaJSON), &items) != nil {
			continue
		}
		for _, it := range items {
			tok := camMediaToken(it.MediaURL)
			if tok == "" || seen[tok] {
				continue
			}
			cap, ok := capByToken[tok]
			if !ok {
				continue // capture row gone (expired/reaped) — not fetchable, skip
			}
			seen[tok] = true
			ev := map[string]any{
				"token": tok, "media_url": camMediaURL(cfg, nil, tok), "caption": it.Caption,
				"content_type": cap.ContentType, "kind": cap.Kind, "camera_id": cap.CameraID,
			}
			// A clip uploaded to object storage carries a direct public link; hand it
			// out so large videos can be delivered as a URL instead of a re-hosted file.
			if strings.TrimSpace(cap.S3URL) != "" {
				ev["public_url"] = cap.S3URL
			}
			evidence = append(evidence, ev)
			if len(evidence) >= camSettleEvidenceMax {
				break
			}
		}
		if len(evidence) >= camSettleEvidenceMax {
			break
		}
	}

	payload := map[string]any{
		"event":            "camera_investigation_settled",
		"investigation_id": inv.ID,
		"site_id":          inv.SiteID,
		"status":           status,
		"answer":           answer,
		"turns":            turns,
		"evidence":         evidence,
		"view_url":         camInvestigationViewURL(cfg, nil, inv),
		"at":               at.UTC().Format(time.RFC3339),
	}
	if status == "awaiting_operator" && question != "" {
		payload["question_for_operator"] = question
	}
	return payload
}

// camNotifyInvestigationSettled delivers the settle webhook for an
// investigation transition (answered | awaiting_operator | exhausted) to the
// site's registered callback, with retries. Always spawned `go ...`; a site
// with no callback row is a cheap no-op.
func camNotifyInvestigationSettled(cfg config, invID, status string) {
	db, err := openProxyDB(cfg)
	if err != nil {
		camlog("warn", "investigation_callback", map[string]any{"investigation_id": invID, "ok": false, "error": err.Error()})
		return
	}
	inv, msgs, gerr := getCameraInvestigationWithMessages(db, invID)
	if gerr != nil {
		db.Close()
		camlog("warn", "investigation_callback", map[string]any{"investigation_id": invID, "ok": false, "error": gerr.Error()})
		return
	}
	cb, cerr := getCameraSiteCallback(db, inv.SiteID)
	if cerr != nil || !cb.Enabled || strings.TrimSpace(cb.URL) == "" {
		db.Close()
		return // no registered callback — cheap no-op
	}
	capByToken := map[string]camCapture{}
	if _, byToken := camCollectMintedMedia(msgs); len(byToken) > 0 {
		for tok := range byToken {
			if c, e := getCameraCaptureByToken(db, tok); e == nil {
				capByToken[tok] = c
			}
		}
	}
	db.Close() // release before the (potentially minutes-long) retry ladder

	body, merr := json.Marshal(camSettlePayload(cfg, inv, msgs, status, capByToken, time.Now()))
	if merr != nil {
		camlog("error", "investigation_callback", map[string]any{"investigation_id": invID, "ok": false, "error": merr.Error()})
		return
	}
	camPostSignedCallback(cfg, cb, body, "investigation_callback", map[string]any{
		"investigation_id": invID, "site_id": inv.SiteID, "status": status,
	})
}

// camMergeFields returns a fresh map that is base overlaid with extra, so the
// per-attempt log fields never mutate the caller's base map across retries.
func camMergeFields(base, extra map[string]any) map[string]any {
	out := make(map[string]any, len(base)+len(extra))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range extra {
		out[k] = v
	}
	return out
}

// camPostSignedCallback delivers a JSON body to a site's registered callback with
// the same auth (Bearer per-site secret + X-Camera-Signature: sha256=HMAC(secret,
// body)), client selection (SSRF-guarded unless cfg.CameraCallbackAllowPrivate),
// per-attempt timeout, and {0,30s,5min} at-least-once retry ladder used by every
// camera webhook. logOp/logFields let each caller (investigation_callback,
// motion_callback) label its own log line; per-attempt fields (attempt/ok/
// http_status) are merged in. The caller runs this on its own goroutine (the
// retry ladder sleeps inline), decrypting nothing itself — the secret is derived
// here from cb.SecretEnc.
func camPostSignedCallback(cfg config, cb camSiteCallback, body []byte, logOp string, logFields map[string]any) {
	camPostSignedCallbackDelays(cfg, cb, body, camCallbackRetryDelays, logOp, logFields)
}

// camPostSignedCallbackDelays is camPostSignedCallback with a caller-chosen retry
// ladder. The settle webhook uses the reliable {0,30s,5m} ladder (camPostSignedCallback);
// the best-effort progress stream uses a short {0,15s} ladder (camProgressRetryDelays)
// so a slow/unreachable Connect never wedges a delivery goroutine for minutes.
func camPostSignedCallbackDelays(cfg config, cb camSiteCallback, body []byte, delays []time.Duration, logOp string, logFields map[string]any) {
	secret := ""
	if strings.TrimSpace(cb.SecretEnc) != "" {
		if s, derr := decryptSecret(cfg, cb.SecretEnc); derr == nil {
			secret = s
		} else {
			camlog("warn", logOp, camMergeFields(logFields, map[string]any{"ok": false, "error": "secret decrypt failed"}))
		}
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	signature := "sha256=" + hex.EncodeToString(mac.Sum(nil))

	timeout := cfg.CameraCallbackTimeout
	if timeout <= 0 {
		timeout = 20 * time.Second
	}
	client := camGuardedHTTPClient()
	if cfg.CameraCallbackAllowPrivate {
		client = &http.Client{} // local dev: the guard blocks loopback by design
	}
	client.Timeout = timeout

	for i, delay := range delays {
		if delay > 0 {
			time.Sleep(delay)
		}
		req, rerr := http.NewRequest(http.MethodPost, cb.URL, bytes.NewReader(body))
		if rerr != nil {
			camlog("error", logOp, camMergeFields(logFields, map[string]any{"ok": false, "error": rerr.Error()}))
			return
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Camera-Signature", signature)
		if secret != "" {
			req.Header.Set("Authorization", "Bearer "+secret)
		}
		resp, derr := client.Do(req)
		if derr != nil {
			camlog("warn", logOp, camMergeFields(logFields, map[string]any{"attempt": i + 1, "ok": false, "error": derr.Error()}))
			continue
		}
		_ = readAllLimited(resp.Body, 4096) // drain for connection reuse; body unused
		resp.Body.Close()
		ok := resp.StatusCode >= 200 && resp.StatusCode < 300
		level := "info"
		if !ok {
			level = "warn"
		}
		camlog(level, logOp, camMergeFields(logFields, map[string]any{"attempt": i + 1, "ok": ok, "http_status": resp.StatusCode}))
		if ok {
			return
		}
	}
}

// camNotifyMotion delivers the real-time camera_motion webhook for a fired motion
// episode to the site's registered callback. A site with no enabled callback is a
// cheap no-op. Payload {event:"camera_motion", site_id, camera_id, camera_name,
// event_type, at, snapshot:{token, media_url}} — the snapshot media_url is a
// served /camera/media/<token> URL (blank when the snapshot couldn't be captured).
// Always spawned `go ...`; the retry ladder runs inline on that goroutine.
func camNotifyMotion(cfg config, siteID, cameraID, cameraName, eventType, at, snapshotToken string) {
	db, err := openProxyDB(cfg)
	if err != nil {
		camlog("warn", "motion_callback", map[string]any{"site_id": siteID, "camera_id": cameraID, "ok": false, "error": err.Error()})
		return
	}
	cb, cerr := getCameraSiteCallback(db, siteID)
	db.Close()
	if cerr != nil || !cb.Enabled || strings.TrimSpace(cb.URL) == "" {
		return // no registered callback — cheap no-op
	}
	snapshot := map[string]any{"token": snapshotToken, "media_url": ""}
	if strings.TrimSpace(snapshotToken) != "" {
		snapshot["media_url"] = camMediaURL(cfg, nil, snapshotToken)
	}
	payload := map[string]any{
		"event":       "camera_motion",
		"site_id":     siteID,
		"camera_id":   cameraID,
		"camera_name": cameraName,
		"event_type":  eventType,
		"at":          at,
		"snapshot":    snapshot,
	}
	body, merr := json.Marshal(payload)
	if merr != nil {
		camlog("error", "motion_callback", map[string]any{"site_id": siteID, "camera_id": cameraID, "ok": false, "error": merr.Error()})
		return
	}
	camPostSignedCallback(cfg, cb, body, "motion_callback", map[string]any{
		"site_id": siteID, "camera_id": cameraID, "event_type": eventType,
	})
}

// ─────────────────────────── rate limiting + last-used ───────────────────────────

// camSvcBucket is one token bucket (capacity = per-minute rate, so a full
// minute's allowance can burst).
type camSvcBucket struct {
	tokens float64
	last   time.Time
}

var camSvcRL = struct {
	sync.Mutex
	buckets map[string]*camSvcBucket
}{buckets: map[string]*camSvcBucket{}}

// camSvcRLMaxBuckets caps the rate-limiter map. The PUBLIC view/media routes key it
// on unauthenticated, caller-supplied tokens (camera_investigate_view.go), so without
// a ceiling an attacker spraying unique tokens — each still inserted even while being
// 429'd — grows the map without bound until the process OOMs. When the map reaches
// this size a new key first reaps idle (fully-refilled, hence stateless) buckets; if
// that frees nothing the new key is DENIED rather than inserted, so the map can never
// exceed the bound. Denying an unknown key just means "rate limited" — the safe
// failure mode under an active flood. The legitimate footprint (authenticated service
// tokens + a bucket per active view/IP) stays far below this.
const camSvcRLMaxBuckets = 50000

// camSvcRLIdle is how long a bucket must be untouched to be reap-eligible: a bucket
// refills to full within a minute, after which it holds no rate-limiting state and is
// indistinguishable from never having existed, so dropping it is free.
const camSvcRLIdle = 2 * time.Minute

// camSvcRateAllow spends one request from key's bucket (refill perMin/minute,
// cap perMin). perMin <= 0 disables limiting.
func camSvcRateAllow(key string, perMin float64) bool {
	if perMin <= 0 {
		return true
	}
	now := time.Now()
	camSvcRL.Lock()
	defer camSvcRL.Unlock()
	b, ok := camSvcRL.buckets[key]
	if !ok {
		// Bound the map before inserting a possibly attacker-chosen key.
		if len(camSvcRL.buckets) >= camSvcRLMaxBuckets {
			camSvcRLReapLocked(now)
			if len(camSvcRL.buckets) >= camSvcRLMaxBuckets {
				return false // full of live buckets → fail safe (rate limited)
			}
		}
		b = &camSvcBucket{tokens: perMin, last: now}
		camSvcRL.buckets[key] = b
	}
	b.tokens += now.Sub(b.last).Minutes() * perMin
	if b.tokens > perMin {
		b.tokens = perMin
	}
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// camSvcRLReapLocked drops every bucket idle for at least camSvcRLIdle (fully
// refilled, so stateless). Caller holds the lock. O(n), but only runs when the map is
// already at its cap, so its cost is amortized against the flood that grew it there.
func camSvcRLReapLocked(now time.Time) {
	for k, b := range camSvcRL.buckets {
		if now.Sub(b.last) >= camSvcRLIdle {
			delete(camSvcRL.buckets, k)
		}
	}
}

var camSvcLastUsed = struct {
	sync.Mutex
	at map[string]time.Time
}{at: map[string]time.Time{}}

// camSvcStampLastUsed best-effort stamps last_used_at, throttled to ~1/min per
// token so the auth path doesn't write on every request.
func camSvcStampLastUsed(db *sql.DB, tokenID string) {
	now := time.Now()
	camSvcLastUsed.Lock()
	if last, ok := camSvcLastUsed.at[tokenID]; ok && now.Sub(last) < time.Minute {
		camSvcLastUsed.Unlock()
		return
	}
	camSvcLastUsed.at[tokenID] = now
	camSvcLastUsed.Unlock()
	_, _ = db.Exec(`UPDATE camera_service_tokens SET last_used_at = ? WHERE id = ?`, nowRFC3339(), tokenID)
}

// ─────────────────────────── auth middleware ───────────────────────────

// requireCameraService parses Authorization: Bearer, sha256s the plaintext,
// looks up the enabled row (constant-time), rejects wrong scope, enforces the
// per-token rate limit, stamps last_used_at (best-effort, throttled 1/min), and
// invokes next with the camSvcCtx. Every rejection is the identical
// {"error":"unauthorized"} 401 body.
func requireCameraService(cfg config, scope string, next camSvcHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		unauthorized := func() {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
		}
		auth := strings.TrimSpace(r.Header.Get("Authorization"))
		if len(auth) < 7 || !strings.EqualFold(auth[:7], "Bearer ") {
			unauthorized()
			return
		}
		plaintext := strings.TrimSpace(auth[7:])
		if plaintext == "" {
			unauthorized()
			return
		}
		db, err := openProxyDB(cfg)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "database unavailable"})
			return
		}
		defer db.Close()
		tok, lerr := lookupCameraServiceToken(db, plaintext)
		if lerr != nil || tok.Scope != scope || (tok.Scope == "site" && strings.TrimSpace(tok.SiteID) == "") {
			unauthorized()
			return
		}
		rate := cfg.CameraServiceRate
		if rate <= 0 {
			rate = 120
		}
		class := "api"
		if strings.HasPrefix(r.URL.Path, "/api/cameras/media/") {
			rate *= 5 // media fetches are cheap static serves; Connect pulls evidence in bursts
			class = "media"
		}
		if !camSvcRateAllow(tok.ID+":"+class, float64(rate)) {
			writeJSON(w, http.StatusTooManyRequests, map[string]any{"error": "rate limited"})
			return
		}
		camSvcStampLastUsed(db, tok.ID)
		next(w, r, camSvcCtx{TokenID: tok.ID, Scope: tok.Scope, SiteID: tok.SiteID})
	}
}

// ─────────────────────────── route registration ───────────────────────────

// registerCameraServiceRoutes mounts the Connect-facing service API onto the
// shared proxy mux. Called once from newProxyMux next to registerCameraRoutes.
// The API is inert until an admin mints a token (/ui/api/cameras/service-tokens).
func registerCameraServiceRoutes(cfg config, mux *http.ServeMux) {
	// Management scope — held by Connect's server registry.
	mux.HandleFunc("/api/cameras/provision", noStore(requireCameraService(cfg, "management", handleSvcProvision(cfg))))
	mux.HandleFunc("/api/cameras/deprovision", noStore(requireCameraService(cfg, "management", handleSvcDeprovision(cfg))))
	mux.HandleFunc("/api/cameras/usage", noStore(requireCameraService(cfg, "management", handleSvcUsage(cfg))))

	// Site scope — one token per Connect plugin instance; site_id from the token.
	mux.HandleFunc("/api/cameras/site", noStore(requireCameraService(cfg, "site", handleSvcSite(cfg))))
	mux.HandleFunc("/api/cameras/dvrs", noStore(requireCameraService(cfg, "site", handleSvcDVRs(cfg))))
	mux.HandleFunc("/api/cameras/dvrs/update", noStore(requireCameraService(cfg, "site", handleSvcDVRUpdate(cfg))))
	mux.HandleFunc("/api/cameras/dvrs/delete", noStore(requireCameraService(cfg, "site", handleSvcDVRDelete(cfg))))
	mux.HandleFunc("/api/cameras/dvrs/toggle", noStore(requireCameraService(cfg, "site", handleSvcDVRToggle(cfg))))
	mux.HandleFunc("/api/cameras/dvrs/discover", noStore(requireCameraService(cfg, "site", handleSvcDVRDiscover(cfg))))
	mux.HandleFunc("/api/cameras/list", noStore(requireCameraService(cfg, "site", handleSvcCamerasList(cfg))))
	mux.HandleFunc("/api/cameras/update", noStore(requireCameraService(cfg, "site", handleSvcCameraUpdate(cfg))))
	mux.HandleFunc("/api/cameras/snapshot", noStore(requireCameraService(cfg, "site", handleSvcSnapshot(cfg))))
	mux.HandleFunc("/api/cameras/investigations", noStore(requireCameraService(cfg, "site", handleSvcInvestigations(cfg))))
	mux.HandleFunc("/api/cameras/investigations/get", noStore(requireCameraService(cfg, "site", handleSvcInvestigationGet(cfg))))
	mux.HandleFunc("/api/cameras/investigations/reply", noStore(requireCameraService(cfg, "site", handleSvcInvestigationReply(cfg))))
	mux.HandleFunc("/api/cameras/exports", noStore(requireCameraService(cfg, "site", handleSvcExports(cfg))))
	mux.HandleFunc("/api/cameras/exports/get", noStore(requireCameraService(cfg, "site", handleSvcExportGet(cfg))))
	mux.HandleFunc("/api/cameras/avatars", noStore(requireCameraService(cfg, "site", handleSvcAvatars(cfg))))
	mux.HandleFunc("/api/cameras/avatars/update", noStore(requireCameraService(cfg, "site", handleSvcAvatarUpdate(cfg))))
	mux.HandleFunc("/api/cameras/avatars/delete", noStore(requireCameraService(cfg, "site", handleSvcAvatarDelete(cfg))))
	mux.HandleFunc("/api/cameras/avatars/photos", noStore(requireCameraService(cfg, "site", handleSvcAvatarPhotos(cfg))))
	mux.HandleFunc("/api/cameras/avatars/media", noStore(requireCameraService(cfg, "site", handleSvcAvatarMedia(cfg))))
	mux.HandleFunc("/api/cameras/avatars/media/delete", noStore(requireCameraService(cfg, "site", handleSvcAvatarMediaDelete(cfg))))
	mux.HandleFunc("/api/cameras/avatars/scan", noStore(requireCameraService(cfg, "site", handleSvcAvatarScan(cfg))))
	mux.HandleFunc("/api/cameras/avatars/scans", noStore(requireCameraService(cfg, "site", handleSvcAvatarScans(cfg))))
	mux.HandleFunc("/api/cameras/avatars/candidates", noStore(requireCameraService(cfg, "site", handleSvcAvatarCandidates(cfg))))
	mux.HandleFunc("/api/cameras/avatars/candidates/review", noStore(requireCameraService(cfg, "site", handleSvcAvatarCandidateReview(cfg))))
	mux.HandleFunc("/api/cameras/playbooks", noStore(requireCameraService(cfg, "site", handleSvcPlaybooks(cfg))))
	mux.HandleFunc("/api/cameras/playbooks/update", noStore(requireCameraService(cfg, "site", handleSvcPlaybookUpdate(cfg))))
	mux.HandleFunc("/api/cameras/playbooks/delete", noStore(requireCameraService(cfg, "site", handleSvcPlaybookDelete(cfg))))
	mux.HandleFunc("/api/cameras/media/", noStore(requireCameraService(cfg, "site", handleSvcMedia(cfg))))
	mux.HandleFunc("/api/cameras/callback", noStore(requireCameraService(cfg, "site", handleSvcCallback(cfg))))
	mux.HandleFunc("/api/cameras/motion/arm", noStore(requireCameraService(cfg, "site", handleSvcMotionArm(cfg))))
	mux.HandleFunc("/api/cameras/motion/events", noStore(requireCameraService(cfg, "site", handleSvcMotionEvents(cfg))))
}

// ─────────────────────────── shared handler plumbing ───────────────────────────

// camSvcNotFound is the single not-found/ownership-denied response — identical
// for "does not exist" and "exists in another site" so ids can't be probed.
func camSvcNotFound(w http.ResponseWriter) {
	writeJSON(w, http.StatusNotFound, map[string]any{"error": "not found"})
}

func camSvcMethodNotAllowed(w http.ResponseWriter) {
	writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
}

// camSvcOpenDB opens the proxy DB or writes the 500 itself (nil return).
func camSvcOpenDB(cfg config, w http.ResponseWriter) *sql.DB {
	db, err := openProxyDB(cfg)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return nil
	}
	return db
}

// camSvcDecode decodes a JSON body or writes the 400 itself (false return).
func camSvcDecode(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid JSON"})
		return false
	}
	return true
}

// camSvcDVR loads a DVR and enforces sc-site ownership (404 on either failure).
func camSvcDVR(db *sql.DB, cfg config, w http.ResponseWriter, id, siteID string) (CamDVR, bool) {
	dvr, err := getCameraDVR(db, cfg, strings.TrimSpace(id))
	if err != nil || dvr.SiteID != siteID {
		camSvcNotFound(w)
		return CamDVR{}, false
	}
	return dvr, true
}

// camSvcCamera loads a camera and enforces sc-site ownership.
func camSvcCamera(db *sql.DB, w http.ResponseWriter, id, siteID string) (camera, bool) {
	cam, err := getCamera(db, strings.TrimSpace(id))
	if err != nil || cam.SiteID != siteID {
		camSvcNotFound(w)
		return camera{}, false
	}
	return cam, true
}

// camSvcAvatar loads an avatar and enforces sc-site ownership.
func camSvcAvatar(db *sql.DB, w http.ResponseWriter, id, siteID string) (camAvatar, bool) {
	av, err := getCameraAvatar(db, strings.TrimSpace(id))
	if err != nil || av.SiteID != siteID {
		camSvcNotFound(w)
		return camAvatar{}, false
	}
	return av, true
}

// camSvcSitePolicy loads a site's decoded role-vocabulary policy.
func camSvcSitePolicy(db *sql.DB, siteID string) (camSitePolicy, error) {
	site, err := getCameraSite(db, siteID)
	if err != nil {
		return camSitePolicy{}, err
	}
	return camParseSitePolicy(site.PolicyJSON), nil
}

// camSvcPlaybook loads a playbook and enforces sc-site ownership.
func camSvcPlaybook(db *sql.DB, w http.ResponseWriter, id, siteID string) (camPlaybook, bool) {
	pb, err := getCameraPlaybook(db, strings.TrimSpace(id))
	if err != nil || pb.SiteID != siteID {
		camSvcNotFound(w)
		return camPlaybook{}, false
	}
	return pb, true
}

// camSvcRecomputeAvatarCentroid best-effort recomputes an avatar's cached
// face-embedding centroid from its reference media (direct column write so it
// never depends on updateCameraAvatar's field coverage).
func camSvcRecomputeAvatarCentroid(db *sql.DB, avatarID string) {
	media, err := listAvatarMedia(db, avatarID)
	if err != nil {
		return
	}
	var refs [][]float32
	for _, m := range media {
		if v := camEmbeddingFromBlob(m.Embedding); len(v) > 0 {
			refs = append(refs, v)
		}
	}
	var blob []byte
	if c := camAvatarCentroid(refs); len(c) > 0 {
		blob = camEmbeddingBlob(c)
	}
	_, _ = db.Exec(`UPDATE camera_avatars SET embedding = ?, updated_at = ? WHERE id = ?`, blob, nowRFC3339(), avatarID)
}

// ─────────────────────────── management handlers ───────────────────────────

// handleSvcProvision serves POST /api/cameras/provision {name, description?} —
// creates the site + its site-scoped token in one tx-ish sequence (best-effort
// site rollback on token failure) → {ok, site_id, token}.
func handleSvcProvision(cfg config) camSvcHandler {
	return func(w http.ResponseWriter, r *http.Request, sc camSvcCtx) {
		if r.Method != http.MethodPost {
			camSvcMethodNotAllowed(w)
			return
		}
		var body struct {
			Name        string `json:"name"`
			Description string `json:"description"`
		}
		if !camSvcDecode(w, r, &body) {
			return
		}
		name := strings.TrimSpace(body.Name)
		if name == "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "name is required"})
			return
		}
		db := camSvcOpenDB(cfg, w)
		if db == nil {
			return
		}
		defer db.Close()
		siteID, serr := insertCameraSite(db, camSite{Name: name, Description: strings.TrimSpace(body.Description)})
		if serr != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": serr.Error()})
			return
		}
		_, plaintext, terr := mintCameraServiceToken(db, "site", siteID, name)
		if terr != nil {
			_ = deleteCameraSite(db, siteID) // best-effort rollback — no site without a token
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": terr.Error()})
			return
		}
		camlog("info", "svc_provision", map[string]any{"site_id": siteID, "name": name, "token_id_scope": "site"})
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "site_id": siteID, "token": plaintext})
	}
}

// handleSvcDeprovision serves POST /api/cameras/deprovision {site_id} — cascade
// deletes the site, revokes its tokens, deletes its callback row → {ok}.
func handleSvcDeprovision(cfg config) camSvcHandler {
	return func(w http.ResponseWriter, r *http.Request, sc camSvcCtx) {
		if r.Method != http.MethodPost {
			camSvcMethodNotAllowed(w)
			return
		}
		var body struct {
			SiteID string `json:"site_id"`
		}
		if !camSvcDecode(w, r, &body) {
			return
		}
		siteID := strings.TrimSpace(body.SiteID)
		if siteID == "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "site_id is required"})
			return
		}
		db := camSvcOpenDB(cfg, w)
		if db == nil {
			return
		}
		defer db.Close()
		if _, gerr := getCameraSite(db, siteID); gerr != nil {
			camSvcNotFound(w)
			return
		}
		removed := camCascadeDeleteSite(db, cfg, siteID)
		if rerr := revokeCameraServiceTokensForSite(db, siteID); rerr != nil {
			camlog("warn", "svc_deprovision", map[string]any{"site_id": siteID, "ok": false, "error": rerr.Error()})
		}
		_ = deleteCameraSiteCallback(db, siteID)
		camlog("info", "svc_deprovision", map[string]any{"site_id": siteID, "dvrs_removed": removed})
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	}
}

// handleSvcUsage serves GET /api/cameras/usage → {sites:[{site_id, name,
// dvr_count, camera_count, open_investigations}]} (quota reconciliation).
func handleSvcUsage(cfg config) camSvcHandler {
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
		sites, err := listCameraSites(db)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		out := make([]map[string]any, 0, len(sites))
		for _, s := range sites {
			var dvrs, cams, open int
			_ = db.QueryRow(`SELECT COUNT(*) FROM camera_dvrs WHERE site_id = ?`, s.ID).Scan(&dvrs)
			_ = db.QueryRow(`SELECT COUNT(*) FROM cameras WHERE site_id = ?`, s.ID).Scan(&cams)
			_ = db.QueryRow(`SELECT COUNT(*) FROM camera_investigations
				WHERE site_id = ? AND parent_id = '' AND status IN ('queued','running','awaiting_operator')`, s.ID).Scan(&open)
			out = append(out, map[string]any{
				"site_id": s.ID, "name": s.Name, "dvr_count": dvrs,
				"camera_count": cams, "open_investigations": open,
			})
		}
		writeJSON(w, http.StatusOK, map[string]any{"sites": out})
	}
}

// ─────────────────────────── site handlers ───────────────────────────
// Every handler works on sc.SiteID only and ownership-checks every id in the
// request against it; they maximally delegate to the existing helper layer
// (insertCameraDVR, runCameraDiscovery, insertCameraInvestigation, avatar/
// playbook store funcs, ...).

// handleSvcSite serves GET (siteJSON incl. analysis, policy and the resolved
// role_vocabulary) / POST {description?, analysis_alias?, policy?} partial
// update on /api/cameras/site. policy carries the per-site custom person-role
// labels ({custom_roles:[{slug,label,description}], notes}) — descriptive
// vocabulary only, validated by camNormalizeSitePolicy.
func handleSvcSite(cfg config) camSvcHandler {
	return func(w http.ResponseWriter, r *http.Request, sc camSvcCtx) {
		db := camSvcOpenDB(cfg, w)
		if db == nil {
			return
		}
		defer db.Close()
		site, gerr := getCameraSite(db, sc.SiteID)
		if gerr != nil {
			camSvcNotFound(w)
			return
		}
		switch r.Method {
		case http.MethodGet:
			writeJSON(w, http.StatusOK, map[string]any{"site": siteJSON(site)})
		case http.MethodPost:
			var body struct {
				Description   *string        `json:"description"`
				AnalysisAlias *string        `json:"analysis_alias"`
				Policy        *camSitePolicy `json:"policy"`
			}
			if !camSvcDecode(w, r, &body) {
				return
			}
			if body.Description != nil {
				site.Description = strings.TrimSpace(*body.Description)
			}
			if body.AnalysisAlias != nil {
				site.AnalysisAlias = strings.TrimSpace(*body.AnalysisAlias)
			}
			// Validate the policy BEFORE any write so a 400 never leaves a partial
			// update behind (description persisted, policy rejected).
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
				if err := updateCameraSitePolicy(db, sc.SiteID, camSitePolicyJSON(*body.Policy)); err != nil {
					writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
					return
				}
			}
			camlog("info", "svc_site_update", map[string]any{"site_id": sc.SiteID})
			site, _ = getCameraSite(db, sc.SiteID)
			writeJSON(w, http.StatusOK, map[string]any{"ok": true, "site": siteJSON(site)})
		default:
			camSvcMethodNotAllowed(w)
		}
	}
}

// handleSvcDVRs serves GET (dvr list incl. ai_instructions; passwords never
// echoed — masked previews only) / POST {name,brand,host,port,http_port,
// username,password,timezone,ai_instructions?} → {ok,id} on /api/cameras/dvrs.
func handleSvcDVRs(cfg config) camSvcHandler {
	return func(w http.ResponseWriter, r *http.Request, sc camSvcCtx) {
		db := camSvcOpenDB(cfg, w)
		if db == nil {
			return
		}
		defer db.Close()
		switch r.Method {
		case http.MethodGet:
			dvrs, err := listCameraDVRViews(db, sc.SiteID)
			if err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{"dvrs": dvrListJSON(dvrs)})
		case http.MethodPost:
			var body struct {
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
			if !camSvcDecode(w, r, &body) {
				return
			}
			host := strings.TrimSpace(body.Host)
			if host == "" {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": "host is required"})
				return
			}
			brand := strings.ToLower(strings.TrimSpace(body.Brand))
			if brand != "" && !camValidBrand(brand) {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": "unknown brand " + brand})
				return
			}
			id, err := insertCameraDVR(db, cfg, CamDVR{
				SiteID: sc.SiteID, Name: strings.TrimSpace(body.Name), Brand: brand, Host: host,
				Port: body.Port, HTTPPort: body.HTTPPort, Username: strings.TrimSpace(body.Username),
				Password: body.Password, Timezone: strings.TrimSpace(body.Timezone),
				AIInstructions: strings.TrimSpace(body.AIInstructions), Enabled: true,
			})
			if err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
				return
			}
			camlog("info", "svc_dvr_create", map[string]any{"dvr_id": id, "site_id": sc.SiteID, "brand": brand, "host": host})
			writeJSON(w, http.StatusOK, map[string]any{"ok": true, "id": id})
		default:
			camSvcMethodNotAllowed(w)
		}
	}
}

// handleSvcDVRUpdate serves POST /api/cameras/dvrs/update {id, ...optional...,
// ai_instructions?} (pointer semantics mirror handleCameraDVRUpdate).
func handleSvcDVRUpdate(cfg config) camSvcHandler {
	return func(w http.ResponseWriter, r *http.Request, sc camSvcCtx) {
		if r.Method != http.MethodPost {
			camSvcMethodNotAllowed(w)
			return
		}
		var body struct {
			ID             string  `json:"id"`
			Name           string  `json:"name"`
			Brand          string  `json:"brand"`
			Host           string  `json:"host"`
			Port           int     `json:"port"`
			HTTPPort       int     `json:"http_port"`
			Username       string  `json:"username"`
			Password       string  `json:"password"`
			Timezone       string  `json:"timezone"`
			AIInstructions *string `json:"ai_instructions"`
			Enabled        *bool   `json:"enabled"`
		}
		if !camSvcDecode(w, r, &body) {
			return
		}
		db := camSvcOpenDB(cfg, w)
		if db == nil {
			return
		}
		defer db.Close()
		dvr, ok := camSvcDVR(db, cfg, w, body.ID, sc.SiteID)
		if !ok {
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
		camlog("info", "svc_dvr_update", map[string]any{"dvr_id": dvr.ID, "site_id": sc.SiteID, "password_changed": changePassword})
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	}
}

// handleSvcDVRDelete serves POST /api/cameras/dvrs/delete {id}.
func handleSvcDVRDelete(cfg config) camSvcHandler {
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
		dvr, ok := camSvcDVR(db, cfg, w, body.ID, sc.SiteID)
		if !ok {
			return
		}
		cams, _ := listCamerasByDVR(db, dvr.ID)
		for _, c := range cams {
			_ = deleteCamera(db, c.ID)
		}
		if err := deleteCameraDVR(db, dvr.ID); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		camlog("info", "svc_dvr_delete", map[string]any{"dvr_id": dvr.ID, "site_id": sc.SiteID, "cameras_removed": len(cams)})
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	}
}

// handleSvcDVRToggle serves POST /api/cameras/dvrs/toggle {id, enabled}.
func handleSvcDVRToggle(cfg config) camSvcHandler {
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
		dvr, ok := camSvcDVR(db, cfg, w, body.ID, sc.SiteID)
		if !ok {
			return
		}
		if err := setDVREnabled(db, dvr.ID, body.Enabled); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		camlog("info", "svc_dvr_toggle", map[string]any{"dvr_id": dvr.ID, "site_id": sc.SiteID, "enabled": body.Enabled})
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	}
}

// handleSvcDVRDiscover serves POST /api/cameras/dvrs/discover {id} →
// {channels_found, added, updated, cameras}.
func handleSvcDVRDiscover(cfg config) camSvcHandler {
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
		dvr, ok := camSvcDVR(db, cfg, w, body.ID, sc.SiteID)
		if !ok {
			return
		}
		channels, added, updated, derr := runCameraDiscovery(r.Context(), cfg, db, dvr)
		if derr != nil {
			writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": derr.Error()})
			return
		}
		cams, _ := listCamerasByDVR(db, dvr.ID)
		out := make([]map[string]any, 0, len(cams))
		for _, c := range cams {
			out = append(out, cameraJSON(c, camSnapshotToken(db, c)))
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"ok": true, "channels_found": len(channels), "added": added, "updated": updated, "cameras": out,
		})
	}
}

// handleSvcCamerasList serves GET /api/cameras/list?dvr_id= → {cameras}.
func handleSvcCamerasList(cfg config) camSvcHandler {
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
		var (
			cams []camera
			lerr error
		)
		if dvrID := strings.TrimSpace(r.URL.Query().Get("dvr_id")); dvrID != "" {
			dvr, ok := camSvcDVR(db, cfg, w, dvrID, sc.SiteID)
			if !ok {
				return
			}
			cams, lerr = listCamerasByDVR(db, dvr.ID)
		} else {
			cams, lerr = listCamerasBySite(db, sc.SiteID)
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

// handleSvcCameraUpdate serves POST /api/cameras/update {id, name?, area?,
// notes?, enabled?} (all optional — pointers where clearing must be possible).
func handleSvcCameraUpdate(cfg config) camSvcHandler {
	return func(w http.ResponseWriter, r *http.Request, sc camSvcCtx) {
		if r.Method != http.MethodPost {
			camSvcMethodNotAllowed(w)
			return
		}
		var body struct {
			ID      string  `json:"id"`
			Name    string  `json:"name"`
			Area    *string `json:"area"`
			Notes   *string `json:"notes"`
			Enabled *bool   `json:"enabled"`
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
		if n := strings.TrimSpace(body.Name); n != "" {
			cam.Name = n
		}
		if body.Area != nil {
			cam.Area = strings.TrimSpace(*body.Area)
		}
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
		camlog("info", "svc_camera_update", map[string]any{"camera_id": cam.ID, "site_id": sc.SiteID, "enabled": cam.Enabled})
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "camera": cameraJSON(cam, camSnapshotToken(db, cam))})
	}
}

// handleSvcSnapshot serves POST /api/cameras/snapshot {id, quality?} →
// {ok, media_token, url, content_type} (mirrors handleCameraSnapshot).
func handleSvcSnapshot(cfg config) camSvcHandler {
	return func(w http.ResponseWriter, r *http.Request, sc camSvcCtx) {
		if r.Method != http.MethodPost {
			camSvcMethodNotAllowed(w)
			return
		}
		var body struct {
			ID      string `json:"id"`
			Quality string `json:"quality"`
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
		ctx, cancel := context.WithTimeout(r.Context(), camCaptureTimeout(cfg))
		defer cancel()
		scratch, serr := os.MkdirTemp("", "camsvcsnap-")
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
			"ok": true, "media_token": token, "url": camMediaURL(cfg, r, token),
			"content_type": res.ContentType, "width": res.Width, "height": res.Height, "quality": q.String(),
		})
	}
}

// ─────────────────────────── investigations ───────────────────────────

// svcInvestigateMessageJSON mirrors investigateMessageJSON but augments every
// media item with the bare capability `token` so Connect can machine-fetch it
// via GET /api/cameras/media/{token} (design-connect.md A2).
func svcInvestigateMessageJSON(m camInvestigationMessage) map[string]any {
	out := investigateMessageJSON(m)
	if strings.TrimSpace(m.MediaJSON) == "" {
		return out
	}
	var items []evidenceItem
	if json.Unmarshal([]byte(m.MediaJSON), &items) != nil || len(items) == 0 {
		return out
	}
	media := make([]map[string]any, 0, len(items))
	for _, it := range items {
		media = append(media, map[string]any{
			"media_url": it.MediaURL, "caption": it.Caption, "token": camMediaToken(it.MediaURL),
		})
	}
	out["media"] = media
	return out
}

func svcInvestigateMessagesJSON(msgs []camInvestigationMessage) []map[string]any {
	out := make([]map[string]any, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, svcInvestigateMessageJSON(m))
	}
	return out
}

// handleSvcInvestigations serves POST {question, alias?} → {ok, id,
// status:"queued"} (the same enqueue path as handleCameraInvestigationStart —
// queued row + opening operator message + inline drain when the worker is
// disabled) / GET ?limit= list, on /api/cameras/investigations.
func handleSvcInvestigations(cfg config) camSvcHandler {
	return func(w http.ResponseWriter, r *http.Request, sc camSvcCtx) {
		db := camSvcOpenDB(cfg, w)
		if db == nil {
			return
		}
		defer db.Close()
		switch r.Method {
		case http.MethodGet:
			invs, lerr := listCameraInvestigations(db, sc.SiteID)
			if lerr != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]any{"error": lerr.Error()})
				return
			}
			if limit, _ := strconv.Atoi(strings.TrimSpace(r.URL.Query().Get("limit"))); limit > 0 && len(invs) > limit {
				invs = invs[:limit]
			}
			out := make([]map[string]any, 0, len(invs))
			for _, inv := range invs {
				out = append(out, investigationJSON(cfg, r, inv))
			}
			writeJSON(w, http.StatusOK, map[string]any{"investigations": out})
		case http.MethodPost:
			var body struct {
				Question string `json:"question"`
				Alias    string `json:"alias"`
			}
			if !camSvcDecode(w, r, &body) {
				return
			}
			question := strings.TrimSpace(body.Question)
			if question == "" {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": "question is required"})
				return
			}
			alias := firstNonEmpty(strings.TrimSpace(body.Alias), strings.TrimSpace(cfg.CameraInvestigateAlias))
			id, ierr := insertCameraInvestigation(db, camInvestigation{
				SiteID: sc.SiteID, Title: truncateString(question, 80), Question: question, Alias: alias, Status: "queued",
			})
			if ierr != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]any{"error": ierr.Error()})
				return
			}
			camAppendInvestigateMessage(db, id, "operator", question, "", "", 0, nil)
			camlog("info", "svc_investigate_create", map[string]any{"investigation_id": id, "site_id": sc.SiteID, "alias": alias})

			// Background queue runs it (startCameraInvestigationWorker); if the worker is
			// DISABLED, drain inline so the row never strands (r is non-nil here).
			if !cfg.CameraInvestigateWorkerEnabled {
				camRunInvestigationInline(db, cfg, r, id)
			}
			inv, gerr := getCameraInvestigation(db, id)
			if gerr != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]any{"error": gerr.Error()})
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{
				"ok": true, "id": inv.ID, "status": inv.Status, "view_url": camInvestigationViewURL(cfg, r, inv),
			})
		default:
			camSvcMethodNotAllowed(w)
		}
	}
}

// handleSvcInvestigationGet serves GET /api/cameras/investigations/get?id= →
// {investigation, messages}; media items additionally carry a bare `token`
// field for machine fetch via /api/cameras/media/{token}.
func handleSvcInvestigationGet(cfg config) camSvcHandler {
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
		inv, msgs, gerr := getCameraInvestigationWithMessages(db, id)
		if gerr != nil || inv.SiteID != sc.SiteID {
			camSvcNotFound(w)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"investigation": investigationJSON(cfg, r, inv), "messages": svcInvestigateMessagesJSON(msgs),
		})
	}
}

// handleSvcInvestigationReply serves POST /api/cameras/investigations/reply
// {id, message} — mirrors handleCameraInvestigationReply incl. the
// still-running rejection and the append-then-flip settle ordering.
func handleSvcInvestigationReply(cfg config) camSvcHandler {
	return func(w http.ResponseWriter, r *http.Request, sc camSvcCtx) {
		if r.Method != http.MethodPost {
			camSvcMethodNotAllowed(w)
			return
		}
		var body struct {
			ID      string `json:"id"`
			Message string `json:"message"`
		}
		if !camSvcDecode(w, r, &body) {
			return
		}
		id := strings.TrimSpace(body.ID)
		message := strings.TrimSpace(body.Message)
		if id == "" || message == "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "id and message are required"})
			return
		}
		db := camSvcOpenDB(cfg, w)
		if db == nil {
			return
		}
		defer db.Close()
		inv, gerr := getCameraInvestigation(db, id)
		if gerr != nil || inv.SiteID != sc.SiteID {
			camSvcNotFound(w)
			return
		}
		switch inv.Status {
		case "closed":
			writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": "investigation is closed"})
			return
		case "queued", "running":
			// A worker owns this row right now; refusing to mutate it is what prevents
			// the worker-vs-request lost-update race.
			writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": "investigation is still working — wait for it to pause or finish, then reply"})
			return
		}
		// Settled: append the operator message FIRST (row still settled, so no worker
		// touches it), THEN flip to "queued" — the resume-on-stale-transcript ordering.
		camAppendInvestigateMessage(db, id, "operator", message, "", "", 0, nil)
		if serr := setCameraInvestigationStatus(db, id, "queued"); serr != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": serr.Error()})
			return
		}
		camlog("info", "svc_investigate_reply", map[string]any{"investigation_id": id, "site_id": sc.SiteID, "prev_status": inv.Status})
		if !cfg.CameraInvestigateWorkerEnabled {
			camRunInvestigationInline(db, cfg, r, id)
		}
		out, gerr2 := getCameraInvestigation(db, id)
		if gerr2 != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": gerr2.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "id": out.ID, "status": out.Status})
	}
}

// ─────────────────────────── evidence exports ───────────────────────────

// handleSvcExports serves /api/cameras/exports (site scope): GET lists this
// site's exports newest-first; POST {camera_ids, from, to, layout?, quality?}
// enqueues a durable multi-camera evidence-video export and returns the queued
// row. Mirrors handleSvcAvatarScan's shape; the heavy validation lives in the
// shared camExportCreateFromBody core.
func handleSvcExports(cfg config) camSvcHandler {
	return func(w http.ResponseWriter, r *http.Request, sc camSvcCtx) {
		db := camSvcOpenDB(cfg, w)
		if db == nil {
			return
		}
		defer db.Close()
		switch r.Method {
		case http.MethodGet:
			exports, lerr := listCameraExports(db, sc.SiteID)
			if lerr != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]any{"error": lerr.Error()})
				return
			}
			if limit, _ := strconv.Atoi(strings.TrimSpace(r.URL.Query().Get("limit"))); limit > 0 && len(exports) > limit {
				exports = exports[:limit]
			}
			out := make([]map[string]any, 0, len(exports))
			for _, ex := range exports {
				out = append(out, cameraExportJSON(cfg, r, ex))
			}
			writeJSON(w, http.StatusOK, map[string]any{"exports": out})
		case http.MethodPost:
			var body struct {
				CameraIDs []string `json:"camera_ids"`
				From      string   `json:"from"`
				To        string   `json:"to"`
				Layout    string   `json:"layout"`
				Quality   string   `json:"quality"`
			}
			if !camSvcDecode(w, r, &body) {
				return
			}
			ex, cerr := camExportCreateFromBody(cfg, db, sc.SiteID, body.CameraIDs, body.From, body.To, body.Layout, body.Quality)
			if cerr != nil {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": cerr.Error()})
				return
			}
			camlog("info", "svc_export_create", map[string]any{"export_id": ex.ID, "site_id": sc.SiteID, "layout": ex.Layout})
			writeJSON(w, http.StatusOK, map[string]any{"ok": true, "export": cameraExportJSON(cfg, r, ex)})
		default:
			camSvcMethodNotAllowed(w)
		}
	}
}

// handleSvcExportGet serves GET /api/cameras/exports/get?id= → {export} (404 on
// a cross-site id, uniform with every other site-scoped lookup).
func handleSvcExportGet(cfg config) camSvcHandler {
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
		ex, gerr := getCameraExport(db, id)
		if gerr != nil || ex.SiteID != sc.SiteID {
			camSvcNotFound(w)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"export": cameraExportJSON(cfg, r, ex)})
	}
}

// camExportReadyPayload builds the camera_export_ready webhook body from an
// export row — pure over its input so it is unit-testable. links is one entry per
// produced MP4 (permanent url + caption + bytes); gaps flattens the per-camera
// download gaps from the progress checkpoint so the operator learns exactly which
// windows had no recording.
func camExportReadyPayload(ex camExport, at time.Time) map[string]any {
	outs := camExportParseOutputs(ex.Outputs)
	links := make([]map[string]any, 0, len(outs))
	for _, o := range outs {
		links = append(links, map[string]any{
			"camera_id": o.CameraID, "label": o.Label, "url": o.S3URL,
			"caption": o.Caption, "bytes": o.Bytes, "content_type": o.ContentType,
		})
	}
	prog := camExportParseProgress(ex.Progress)
	gaps := make([]map[string]any, 0)
	for camID, cp := range prog.Cameras {
		if cp == nil {
			continue
		}
		for _, g := range cp.Gaps {
			gf, gt, _ := strings.Cut(g, "/")
			gaps = append(gaps, map[string]any{"camera_id": camID, "from": gf, "to": gt})
		}
	}
	return map[string]any{
		"event":     "camera_export_ready",
		"export_id": ex.ID,
		// investigation_id ties an investigation-initiated export back to the
		// conversation Connect tracked for that investigation ("" when the export
		// was requested directly through the service API).
		"investigation_id": ex.InvestigationID,
		"site_id":          ex.SiteID,
		"status":           ex.Status,
		"layout":           ex.Layout,
		"from":             ex.FromTS,
		"to":               ex.ToTS,
		"links":            links,
		"gaps":             gaps,
		"error":            ex.Error,
		"at":               at.UTC().Format(time.RFC3339),
	}
}

// camNotifyExportReady delivers the camera_export_ready webhook for a terminal
// export (done | failed) to the site's registered callback, with retries. Always
// spawned `go ...`; a site with no callback is a cheap no-op.
func camNotifyExportReady(cfg config, exportID string) {
	db, err := openProxyDB(cfg)
	if err != nil {
		camlog("warn", "export_callback", map[string]any{"export_id": exportID, "ok": false, "error": err.Error()})
		return
	}
	ex, gerr := getCameraExport(db, exportID)
	if gerr != nil {
		db.Close()
		camlog("warn", "export_callback", map[string]any{"export_id": exportID, "ok": false, "error": gerr.Error()})
		return
	}
	cb, cerr := getCameraSiteCallback(db, ex.SiteID)
	db.Close()
	if cerr != nil || !cb.Enabled || strings.TrimSpace(cb.URL) == "" {
		return // no registered callback — cheap no-op
	}
	body, merr := json.Marshal(camExportReadyPayload(ex, time.Now()))
	if merr != nil {
		camlog("error", "export_callback", map[string]any{"export_id": exportID, "ok": false, "error": merr.Error()})
		return
	}
	camPostSignedCallback(cfg, cb, body, "export_callback", map[string]any{
		"export_id": exportID, "site_id": ex.SiteID, "status": ex.Status,
	})
}

// ─────────────────────────── avatars ───────────────────────────

func svcAvatarJSON(db *sql.DB, a camAvatar) map[string]any {
	refCount := 0
	if media, err := listAvatarMedia(db, a.ID); err == nil {
		refCount = len(media)
	}
	return map[string]any{
		"id": a.ID, "site_id": a.SiteID, "name": a.Name, "type": a.Type, "is_group": a.IsGroup,
		"external_ref": a.ExternalRef, "description": a.Description,
		"dvr_ids": camParseIDArray(a.DVRIDs), "enabled": a.Enabled, "ref_count": refCount,
		"has_embedding": len(a.Embedding) > 0,
		// Confirmed role (user-owned; role != "" ⇔ operator confirmed) + the AI's
		// suggestion/profile (advisory only — Connect must never automate on it).
		"role":                      a.Role,
		"role_note":                 a.RoleNote,
		"role_confirmed_at":         a.RoleConfirmedAt,
		"suggested_role":            a.SuggestedRole,
		"suggested_role_confidence": a.SuggestedRoleConfidence,
		"profile":                   camDecodeJSON(a.ProfileJSON),
		"profile_status":            a.ProfileStatus,
		"profile_updated_at":        a.ProfileUpdatedAt,
		"profile_error":             a.ProfileError,
		"created_at":                a.CreatedAt, "updated_at": a.UpdatedAt,
	}
}

// handleSvcAvatars serves GET (list) / POST (create) /api/cameras/avatars —
// mirror of the admin avatar endpoints, site-confined.
func handleSvcAvatars(cfg config) camSvcHandler {
	return func(w http.ResponseWriter, r *http.Request, sc camSvcCtx) {
		db := camSvcOpenDB(cfg, w)
		if db == nil {
			return
		}
		defer db.Close()
		switch r.Method {
		case http.MethodGet:
			avatars, err := listCameraAvatars(db, sc.SiteID, true)
			if err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
				return
			}
			out := make([]map[string]any, 0, len(avatars))
			for _, a := range avatars {
				out = append(out, svcAvatarJSON(db, a))
			}
			pending, _ := countPendingAvatarCandidates(db, sc.SiteID)
			writeJSON(w, http.StatusOK, map[string]any{"avatars": out, "pending_candidates": pending})
		case http.MethodPost:
			var body struct {
				Name        string   `json:"name"`
				Type        string   `json:"type"`
				IsGroup     bool     `json:"is_group"`
				ExternalRef string   `json:"external_ref"`
				Description string   `json:"description"`
				DVRIDs      []string `json:"dvr_ids"`
				Role        string   `json:"role"`
				RoleNote    string   `json:"role_note"`
			}
			if !camSvcDecode(w, r, &body) {
				return
			}
			name := strings.TrimSpace(body.Name)
			if name == "" {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": "name is required"})
				return
			}
			dvrIDs := ""
			if len(body.DVRIDs) > 0 {
				dvrIDs = mustJSON(body.DVRIDs)
			}
			typ, tok := camAvatarNormalizeType(body.Type)
			if !tok {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": "type must be human, vehicle or pet"})
				return
			}
			role := strings.ToLower(strings.TrimSpace(body.Role))
			if role != "" {
				pol, perr := camSvcSitePolicy(db, sc.SiteID)
				if perr != nil {
					writeJSON(w, http.StatusInternalServerError, map[string]any{"error": perr.Error()})
					return
				}
				if !camValidateRole(pol, role) {
					writeJSON(w, http.StatusBadRequest, map[string]any{"error": "unknown role " + camQuote(role) + " for this site"})
					return
				}
			}
			id, err := insertCameraAvatar(db, camAvatar{
				SiteID: sc.SiteID, Name: name, Type: typ,
				IsGroup: body.IsGroup, ExternalRef: strings.TrimSpace(body.ExternalRef),
				Description: strings.TrimSpace(body.Description), DVRIDs: dvrIDs, Enabled: true,
				Role: role, RoleNote: strings.TrimSpace(body.RoleNote),
			})
			if err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
				return
			}
			camlog("info", "svc_avatar_create", map[string]any{"avatar_id": id, "site_id": sc.SiteID, "name": name})
			writeJSON(w, http.StatusOK, map[string]any{"ok": true, "id": id})
		default:
			camSvcMethodNotAllowed(w)
		}
	}
}

// handleSvcAvatarUpdate serves POST /api/cameras/avatars/update.
func handleSvcAvatarUpdate(cfg config) camSvcHandler {
	return func(w http.ResponseWriter, r *http.Request, sc camSvcCtx) {
		if r.Method != http.MethodPost {
			camSvcMethodNotAllowed(w)
			return
		}
		var body struct {
			ID          string    `json:"id"`
			Name        string    `json:"name"`
			Type        string    `json:"type"`
			IsGroup     *bool     `json:"is_group"`
			ExternalRef *string   `json:"external_ref"`
			Description *string   `json:"description"`
			DVRIDs      *[]string `json:"dvr_ids"`
			Enabled     *bool     `json:"enabled"`
			Role        *string   `json:"role"`
			RoleNote    *string   `json:"role_note"`
		}
		if !camSvcDecode(w, r, &body) {
			return
		}
		db := camSvcOpenDB(cfg, w)
		if db == nil {
			return
		}
		defer db.Close()
		av, ok := camSvcAvatar(db, w, body.ID, sc.SiteID)
		if !ok {
			return
		}
		if n := strings.TrimSpace(body.Name); n != "" {
			av.Name = n
		}
		if strings.TrimSpace(body.Type) != "" {
			t, tok := camAvatarNormalizeType(body.Type)
			if !tok {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": "type must be human, vehicle or pet"})
				return
			}
			av.Type = t
		}
		if body.IsGroup != nil {
			av.IsGroup = *body.IsGroup
		}
		if body.ExternalRef != nil {
			av.ExternalRef = strings.TrimSpace(*body.ExternalRef)
		}
		if body.Description != nil {
			av.Description = strings.TrimSpace(*body.Description)
		}
		if body.DVRIDs != nil {
			av.DVRIDs = ""
			if len(*body.DVRIDs) > 0 {
				av.DVRIDs = mustJSON(*body.DVRIDs)
			}
		}
		if body.Enabled != nil {
			av.Enabled = *body.Enabled
		}
		// Validate the role BEFORE any write so a 400 never leaves a partial
		// update behind (descriptive fields persisted, role rejected).
		role := ""
		if body.Role != nil {
			role = strings.ToLower(strings.TrimSpace(*body.Role))
			pol, perr := camSvcSitePolicy(db, sc.SiteID)
			if perr != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]any{"error": perr.Error()})
				return
			}
			if !camValidateRole(pol, role) {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": "unknown role " + camQuote(role) + " for this site"})
				return
			}
		}
		if err := updateCameraAvatar(db, av); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		// Role writes go through the dedicated setter (stamps role_confirmed_at) —
		// an operator decision from Connect, validated against the site vocabulary.
		if body.Role != nil {
			note := av.RoleNote
			if body.RoleNote != nil {
				note = strings.TrimSpace(*body.RoleNote)
			}
			if err := setCameraAvatarRole(db, av.ID, role, note); err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
				return
			}
		} else if body.RoleNote != nil {
			if err := setCameraAvatarRoleNote(db, av.ID, strings.TrimSpace(*body.RoleNote)); err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
				return
			}
		}
		av, _ = getCameraAvatar(db, av.ID)
		camlog("info", "svc_avatar_update", map[string]any{"avatar_id": av.ID, "site_id": sc.SiteID})
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "avatar": svcAvatarJSON(db, av)})
	}
}

// handleSvcAvatarDelete serves POST /api/cameras/avatars/delete {id}.
func handleSvcAvatarDelete(cfg config) camSvcHandler {
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
		av, ok := camSvcAvatar(db, w, body.ID, sc.SiteID)
		if !ok {
			return
		}
		if err := deleteCameraAvatar(db, cfg, av.ID); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		camlog("info", "svc_avatar_delete", map[string]any{"avatar_id": av.ID, "site_id": sc.SiteID})
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	}
}

// handleSvcAvatarPhotos serves POST /api/cameras/avatars/photos {avatar_id,
// image_base64, content_type?, note?} → {ok, photo_id}: decodes the upload,
// persists it PINNED (kind "avatar_ref"), best-effort face-embeds it via the
// sidecar, inserts the reference row and recomputes the avatar centroid.
func handleSvcAvatarPhotos(cfg config) camSvcHandler {
	return func(w http.ResponseWriter, r *http.Request, sc camSvcCtx) {
		if r.Method != http.MethodPost {
			camSvcMethodNotAllowed(w)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 16<<20) // base64-in-JSON upload cap
		var body struct {
			AvatarID    string `json:"avatar_id"`
			ImageBase64 string `json:"image_base64"`
			ContentType string `json:"content_type"`
			Note        string `json:"note"`
		}
		if !camSvcDecode(w, r, &body) {
			return
		}
		db := camSvcOpenDB(cfg, w)
		if db == nil {
			return
		}
		defer db.Close()
		av, ok := camSvcAvatar(db, w, body.AvatarID, sc.SiteID)
		if !ok {
			return
		}
		b64 := strings.TrimSpace(body.ImageBase64)
		if i := strings.Index(b64, ";base64,"); i >= 0 && strings.HasPrefix(b64, "data:") {
			b64 = b64[i+len(";base64,"):]
		}
		raw, derr := base64.StdEncoding.DecodeString(b64)
		if derr != nil {
			if raw, derr = base64.RawStdEncoding.DecodeString(b64); derr != nil {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": "image_base64 is not valid base64"})
				return
			}
		}
		if len(raw) == 0 {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "image_base64 is required"})
			return
		}
		scratch, serr := os.MkdirTemp("", "camsvcphoto-")
		if serr != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": serr.Error()})
			return
		}
		defer os.RemoveAll(scratch)
		tmp := filepath.Join(scratch, "photo.bin")
		if werr := os.WriteFile(tmp, raw, 0o600); werr != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": werr.Error()})
			return
		}
		ct, _ := sniffImage(tmp)
		if ct == "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "unsupported image type (jpeg/png/webp expected)"})
			return
		}
		wpx, hpx := imageDimensions(tmp)
		if !camUploadPixelsOK(wpx, hpx) {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "image resolution is too large (max ~40 megapixels)"})
			return
		}
		// Pinned (expires_at="") — reference imagery is never reaped.
		token, perr := camPersistCaptureExpires(db, cfg, "", sc.SiteID, "", "avatar_ref", "", tmp, ct, wpx, hpx, "", "", "")
		if perr != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": perr.Error()})
			return
		}
		capRow, cerr := getCameraCaptureByToken(db, token)
		if cerr != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": cerr.Error()})
			return
		}
		var emb []byte
		if camFaceEnabled(cfg) {
			if faces, ferr := camFaceDetect(r.Context(), cfg, raw); ferr == nil {
				best := -1
				for i, f := range faces {
					if f.Score >= cfg.FaceMinScore && (best < 0 || f.Score > faces[best].Score) {
						best = i
					}
				}
				if best >= 0 {
					emb = camEmbeddingBlob(faces[best].Embedding)
				}
			}
		}
		photoID, ierr := insertAvatarMedia(db, camAvatarMedia{
			AvatarID: av.ID, CaptureID: capRow.ID, Token: token,
			Source: "upload", Note: strings.TrimSpace(body.Note), Embedding: emb,
		})
		if ierr != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": ierr.Error()})
			return
		}
		camSvcRecomputeAvatarCentroid(db, av.ID)
		camlog("info", "svc_avatar_photo", map[string]any{
			"avatar_id": av.ID, "site_id": sc.SiteID, "photo_id": photoID, "bytes": len(raw), "face_embedded": emb != nil,
		})
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "photo_id": photoID, "token": token, "url": camMediaURL(cfg, r, token)})
	}
}

// handleSvcAvatarMedia serves GET /api/cameras/avatars/media?avatar_id= →
// {media:[...]} — the avatar's approved reference photos (service parity with
// the admin /ui/api/cameras/avatars/media route), site-confined.
func handleSvcAvatarMedia(cfg config) camSvcHandler {
	return func(w http.ResponseWriter, r *http.Request, sc camSvcCtx) {
		if r.Method != http.MethodGet {
			camSvcMethodNotAllowed(w)
			return
		}
		avatarID := strings.TrimSpace(r.URL.Query().Get("avatar_id"))
		if avatarID == "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "avatar_id is required"})
			return
		}
		db := camSvcOpenDB(cfg, w)
		if db == nil {
			return
		}
		defer db.Close()
		av, ok := camSvcAvatar(db, w, avatarID, sc.SiteID)
		if !ok {
			return
		}
		media, err := listAvatarMedia(db, av.ID)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		out := make([]map[string]any, 0, len(media))
		for _, m := range media {
			out = append(out, cameraAvatarMediaJSON(cfg, r, m))
		}
		writeJSON(w, http.StatusOK, map[string]any{"media": out})
	}
}

// handleSvcAvatarMediaDelete serves POST /api/cameras/avatars/media/delete {id}
// — removes one reference photo (row + pinned capture + file) and recomputes
// the centroid. Ownership is checked media → avatar → site.
func handleSvcAvatarMediaDelete(cfg config) camSvcHandler {
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
		m, gerr := getAvatarMedia(db, strings.TrimSpace(body.ID))
		if gerr != nil {
			camSvcNotFound(w)
			return
		}
		if _, ok := camSvcAvatar(db, w, m.AvatarID, sc.SiteID); !ok {
			return
		}
		if err := deleteAvatarMedia(db, cfg, m.ID); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		camlog("info", "svc_avatar_media_delete", map[string]any{"media_id": m.ID, "avatar_id": m.AvatarID, "site_id": sc.SiteID})
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	}
}

// handleSvcAvatarScans serves GET /api/cameras/avatars/scans?avatar_id= →
// {scans:[...]} — enrollment-scan history (status/progress/error), site-confined.
// Without avatar_id it lists the whole site's scans.
func handleSvcAvatarScans(cfg config) camSvcHandler {
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
		avatarID := strings.TrimSpace(r.URL.Query().Get("avatar_id"))
		if avatarID != "" {
			if _, ok := camSvcAvatar(db, w, avatarID, sc.SiteID); !ok {
				return
			}
		}
		// listCameraAvatarScans filters by avatar OR site (either/or): the avatar
		// branch is safe because camSvcAvatar above already enforced site ownership;
		// the empty-avatar branch scopes by sc.SiteID.
		scans, err := listCameraAvatarScans(db, avatarID, sc.SiteID)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		out := make([]map[string]any, 0, len(scans))
		for _, s := range scans {
			out = append(out, cameraAvatarScanJSON(s))
		}
		writeJSON(w, http.StatusOK, map[string]any{"scans": out})
	}
}

// handleSvcAvatarScan serves POST /api/cameras/avatars/scan {avatar_id,
// camera_ids?, from?, to?} → {ok, id} (defaults to the last 24h window).
func handleSvcAvatarScan(cfg config) camSvcHandler {
	return func(w http.ResponseWriter, r *http.Request, sc camSvcCtx) {
		if r.Method != http.MethodPost {
			camSvcMethodNotAllowed(w)
			return
		}
		var body struct {
			AvatarID  string   `json:"avatar_id"`
			CameraIDs []string `json:"camera_ids"`
			From      string   `json:"from"`
			To        string   `json:"to"`
			Alias     string   `json:"alias"`
		}
		if !camSvcDecode(w, r, &body) {
			return
		}
		if !camS3Enabled(cfg) {
			writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": "S3 frame archive is not configured — avatar scans need the snapshot archiver"})
			return
		}
		db := camSvcOpenDB(cfg, w)
		if db == nil {
			return
		}
		defer db.Close()
		av, ok := camSvcAvatar(db, w, body.AvatarID, sc.SiteID)
		if !ok {
			return
		}
		fromRaw, toRaw := strings.TrimSpace(body.From), strings.TrimSpace(body.To)
		if fromRaw == "" && toRaw == "" {
			now := time.Now()
			fromRaw, toRaw = now.Add(-24*time.Hour).Format(time.RFC3339), now.Format(time.RFC3339)
		}
		from, to, werr := camParseWindow(fromRaw, toRaw, cfg, false)
		if werr != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": werr.Error()})
			return
		}
		cameraIDs := ""
		if len(body.CameraIDs) > 0 {
			cams, _ := listCamerasBySite(db, sc.SiteID)
			allowed := make(map[string]bool, len(cams))
			for _, c := range cams {
				allowed[c.ID] = true
			}
			kept := camFilterAllowed(body.CameraIDs, allowed)
			if len(kept) == 0 {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": "camera_ids do not belong to this site"})
				return
			}
			cameraIDs = mustJSON(kept)
		}
		if !cfg.CameraAvatarScanWorkerEnabled {
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "the avatar enrollment-scan worker is disabled on the camera server — the scan cannot run"})
			return
		}
		id, ierr := insertCameraAvatarScan(db, camAvatarScan{
			AvatarID: av.ID, SiteID: sc.SiteID, CameraIDs: cameraIDs,
			FromTS: from.Format(time.RFC3339), ToTS: to.Format(time.RFC3339),
			Alias: strings.TrimSpace(body.Alias), Status: "queued",
		})
		if ierr != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": ierr.Error()})
			return
		}
		camlog("info", "svc_avatar_scan", map[string]any{"scan_id": id, "avatar_id": av.ID, "site_id": sc.SiteID})
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "id": id})
	}
}

// handleSvcAvatarCandidates serves GET /api/cameras/avatars/candidates
// ?avatar_id=&scan_id=&status= → {candidates:[...]}. scan_id scopes the review
// to one enrollment scan (either filter alone works; both AND together).
func handleSvcAvatarCandidates(cfg config) camSvcHandler {
	return func(w http.ResponseWriter, r *http.Request, sc camSvcCtx) {
		if r.Method != http.MethodGet {
			camSvcMethodNotAllowed(w)
			return
		}
		avatarID := strings.TrimSpace(r.URL.Query().Get("avatar_id"))
		scanID := strings.TrimSpace(r.URL.Query().Get("scan_id"))
		if avatarID == "" && scanID == "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "avatar_id or scan_id is required"})
			return
		}
		db := camSvcOpenDB(cfg, w)
		if db == nil {
			return
		}
		defer db.Close()
		if avatarID != "" {
			if _, ok := camSvcAvatar(db, w, avatarID, sc.SiteID); !ok {
				return
			}
		}
		if scanID != "" {
			scan, serr := getCameraAvatarScan(db, scanID)
			if serr != nil || scan.SiteID != sc.SiteID {
				camSvcNotFound(w)
				return
			}
		}
		cands, lerr := listCameraAvatarCandidates(db, scanID, avatarID, strings.TrimSpace(r.URL.Query().Get("status")))
		if lerr != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": lerr.Error()})
			return
		}
		out := make([]map[string]any, 0, len(cands))
		for _, c := range cands {
			item := map[string]any{
				"id": c.ID, "scan_id": c.ScanID, "avatar_id": c.AvatarID, "camera_id": c.CameraID,
				"frame_ts": c.FrameTS, "quality": c.Quality,
				"annotated_token": c.AnnotatedToken, "crop_token": c.CropToken,
				"bbox": camDecodeJSON(c.BBox), "match_kind": c.MatchKind,
				"confidence": c.VLMConfidence, "reason": c.VLMReason,
				"status": c.Status, "assigned_avatar_id": c.AssignedAvatarID,
				"note": c.Note, "reviewed_at": c.ReviewedAt, "created_at": c.CreatedAt,
			}
			if c.AnnotatedToken != "" {
				item["annotated_url"] = camMediaURL(cfg, r, c.AnnotatedToken)
			}
			if c.CropToken != "" {
				item["crop_url"] = camMediaURL(cfg, r, c.CropToken)
			}
			out = append(out, item)
		}
		writeJSON(w, http.StatusOK, map[string]any{"candidates": out})
	}
}

// handleSvcAvatarCandidateReview serves POST /api/cameras/avatars/candidates/review
// {id, decision:"approved"|"rejected"|"grouped", group_avatar_id?, notes?}.
// approved/grouped pin the crop capture, insert an avatar reference (into the
// candidate's avatar, or the group avatar for "grouped") with a best-effort
// face embedding, and recompute the centroid; rejected only stamps the review.
func handleSvcAvatarCandidateReview(cfg config) camSvcHandler {
	return func(w http.ResponseWriter, r *http.Request, sc camSvcCtx) {
		if r.Method != http.MethodPost {
			camSvcMethodNotAllowed(w)
			return
		}
		var body struct {
			ID            string `json:"id"`
			Decision      string `json:"decision"`
			GroupAvatarID string `json:"group_avatar_id"`
			Notes         string `json:"notes"`
		}
		if !camSvcDecode(w, r, &body) {
			return
		}
		decision := strings.ToLower(strings.TrimSpace(body.Decision))
		if decision != "approved" && decision != "rejected" && decision != "grouped" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": `decision must be "approved", "rejected" or "grouped"`})
			return
		}
		db := camSvcOpenDB(cfg, w)
		if db == nil {
			return
		}
		defer db.Close()
		cand, gerr := getCameraAvatarCandidate(db, strings.TrimSpace(body.ID))
		if gerr != nil {
			camSvcNotFound(w)
			return
		}
		// Ownership FIRST: a foreign candidate id must get the identical 404 before
		// any state-dependent response — answering "already reviewed" for another
		// site's candidate would be an existence/status oracle.
		av, ok := camSvcAvatar(db, w, cand.AvatarID, sc.SiteID)
		if !ok {
			return
		}
		// At-least-once delivery from Connect means this POST can be replayed;
		// without this guard a retry pins the crop again and inserts a duplicate
		// avatar_media row sharing one capture (later deletion of either breaks the
		// survivor). Mirror the admin handler's fast-fail on already-reviewed rows.
		if cand.Status != "pending" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "candidate already reviewed (" + cand.Status + ")"})
			return
		}
		note := strings.TrimSpace(body.Notes)

		if decision == "rejected" {
			if err := setCameraAvatarCandidateReview(db, cand.ID, "rejected", "", note); err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
				return
			}
			camlog("info", "svc_candidate_review", map[string]any{"candidate_id": cand.ID, "site_id": sc.SiteID, "decision": decision})
			writeJSON(w, http.StatusOK, map[string]any{"ok": true})
			return
		}

		target := av
		assigned := ""
		if decision == "grouped" {
			gav, gok := camSvcAvatar(db, w, body.GroupAvatarID, sc.SiteID)
			if !gok {
				return
			}
			target, assigned = gav, gav.ID
		}
		// Acquire the crop as a PINNED capture — re-hydrating an expired crop from
		// the S3 frame archive exactly like the admin handler (shared helper) instead
		// of failing the review with "run a new scan".
		capRow, cropBytes, aerr := camCandidatePinnedCrop(r.Context(), cfg, db, cand, sc.SiteID)
		if aerr != nil {
			writeJSON(w, camCropErrStatus(aerr), map[string]any{"error": aerr.Error()})
			return
		}
		var emb []byte
		if camFaceEnabled(cfg) && target.Type == "human" {
			if faces, ferr := camFaceDetect(r.Context(), cfg, cropBytes); ferr == nil {
				best := -1
				for i, f := range faces {
					if f.Score >= cfg.FaceMinScore && (best < 0 || f.Score > faces[best].Score) {
						best = i
					}
				}
				if best >= 0 {
					emb = camEmbeddingBlob(faces[best].Embedding)
				}
			}
		}
		if _, ierr := insertAvatarMedia(db, camAvatarMedia{
			AvatarID: target.ID, CaptureID: capRow.ID, Token: capRow.Token,
			CameraID: cand.CameraID, Quality: cand.Quality, FrameTS: cand.FrameTS, BBox: cand.BBox,
			Source: "scan", Note: note, MatchConfidence: cand.VLMConfidence, Embedding: emb,
		}); ierr != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": ierr.Error()})
			return
		}
		camSvcRecomputeAvatarCentroid(db, target.ID)
		if err := setCameraAvatarCandidateReview(db, cand.ID, decision, assigned, note); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		camlog("info", "svc_candidate_review", map[string]any{
			"candidate_id": cand.ID, "site_id": sc.SiteID, "decision": decision, "target_avatar_id": target.ID,
		})
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	}
}

// ─────────────────────────── playbooks ───────────────────────────

func svcPlaybookJSON(db *sql.DB, p camPlaybook) map[string]any {
	mediaCount := 0
	if media, err := listCameraPlaybookMedia(db, p.ID); err == nil {
		mediaCount = len(media)
	}
	return map[string]any{
		"id": p.ID, "site_id": p.SiteID, "dvr_id": p.DVRID, "name": p.Name,
		"when_to_use": p.WhenToUse, "instructions": p.Instructions, "enabled": p.Enabled,
		"media_count": mediaCount, "created_at": p.CreatedAt, "updated_at": p.UpdatedAt,
	}
}

// handleSvcPlaybooks serves GET (list) / POST (create) /api/cameras/playbooks.
func handleSvcPlaybooks(cfg config) camSvcHandler {
	return func(w http.ResponseWriter, r *http.Request, sc camSvcCtx) {
		db := camSvcOpenDB(cfg, w)
		if db == nil {
			return
		}
		defer db.Close()
		switch r.Method {
		case http.MethodGet:
			playbooks, err := listCameraPlaybooks(db, sc.SiteID, false)
			if err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
				return
			}
			out := make([]map[string]any, 0, len(playbooks))
			for _, p := range playbooks {
				out = append(out, svcPlaybookJSON(db, p))
			}
			writeJSON(w, http.StatusOK, map[string]any{"playbooks": out})
		case http.MethodPost:
			var body struct {
				DVRID        string `json:"dvr_id"`
				Name         string `json:"name"`
				WhenToUse    string `json:"when_to_use"`
				Instructions string `json:"instructions"`
				Enabled      *bool  `json:"enabled"`
			}
			if !camSvcDecode(w, r, &body) {
				return
			}
			name := strings.TrimSpace(body.Name)
			if name == "" {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": "name is required"})
				return
			}
			if dvrID := strings.TrimSpace(body.DVRID); dvrID != "" {
				if _, ok := camSvcDVR(db, cfg, w, dvrID, sc.SiteID); !ok {
					return
				}
			}
			enabled := true
			if body.Enabled != nil {
				enabled = *body.Enabled
			}
			id, err := insertCameraPlaybook(db, camPlaybook{
				SiteID: sc.SiteID, DVRID: strings.TrimSpace(body.DVRID), Name: name,
				WhenToUse: strings.TrimSpace(body.WhenToUse), Instructions: strings.TrimSpace(body.Instructions),
				Enabled: enabled,
			})
			if err != nil {
				if strings.Contains(strings.ToUpper(err.Error()), "UNIQUE") {
					writeJSON(w, http.StatusBadRequest, map[string]any{"error": "a playbook with that name already exists"})
					return
				}
				writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
				return
			}
			camlog("info", "svc_playbook_create", map[string]any{"playbook_id": id, "site_id": sc.SiteID, "name": name})
			writeJSON(w, http.StatusOK, map[string]any{"ok": true, "id": id})
		default:
			camSvcMethodNotAllowed(w)
		}
	}
}

// handleSvcPlaybookUpdate serves POST /api/cameras/playbooks/update.
func handleSvcPlaybookUpdate(cfg config) camSvcHandler {
	return func(w http.ResponseWriter, r *http.Request, sc camSvcCtx) {
		if r.Method != http.MethodPost {
			camSvcMethodNotAllowed(w)
			return
		}
		var body struct {
			ID           string  `json:"id"`
			Name         string  `json:"name"`
			WhenToUse    *string `json:"when_to_use"`
			Instructions *string `json:"instructions"`
			DVRID        *string `json:"dvr_id"`
			Enabled      *bool   `json:"enabled"`
		}
		if !camSvcDecode(w, r, &body) {
			return
		}
		db := camSvcOpenDB(cfg, w)
		if db == nil {
			return
		}
		defer db.Close()
		pb, ok := camSvcPlaybook(db, w, body.ID, sc.SiteID)
		if !ok {
			return
		}
		if n := strings.TrimSpace(body.Name); n != "" {
			pb.Name = n
		}
		if body.WhenToUse != nil {
			pb.WhenToUse = strings.TrimSpace(*body.WhenToUse)
		}
		if body.Instructions != nil {
			pb.Instructions = strings.TrimSpace(*body.Instructions)
		}
		if body.DVRID != nil {
			dvrID := strings.TrimSpace(*body.DVRID)
			if dvrID != "" {
				if _, dok := camSvcDVR(db, cfg, w, dvrID, sc.SiteID); !dok {
					return
				}
			}
			pb.DVRID = dvrID
		}
		if body.Enabled != nil {
			pb.Enabled = *body.Enabled
		}
		if err := updateCameraPlaybook(db, pb); err != nil {
			if strings.Contains(strings.ToUpper(err.Error()), "UNIQUE") {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": "a playbook with that name already exists"})
				return
			}
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		camlog("info", "svc_playbook_update", map[string]any{"playbook_id": pb.ID, "site_id": sc.SiteID})
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "playbook": svcPlaybookJSON(db, pb)})
	}
}

// handleSvcPlaybookDelete serves POST /api/cameras/playbooks/delete {id}.
func handleSvcPlaybookDelete(cfg config) camSvcHandler {
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
		pb, ok := camSvcPlaybook(db, w, body.ID, sc.SiteID)
		if !ok {
			return
		}
		if err := deleteCameraPlaybook(db, cfg, pb.ID); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		camlog("info", "svc_playbook_delete", map[string]any{"playbook_id": pb.ID, "site_id": sc.SiteID})
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	}
}

// ─────────────────────────── media + callback ───────────────────────────

// handleSvcMedia serves GET /api/cameras/media/{token} — evidence bytes; the
// same serving body as handleCameraMedia (token lookup, expiry, path-escape
// check, ServeFile) minus requireAdmin plus a capture.site_id == sc.SiteID
// ownership check. Rate-limited at 5x the base rate. /camera/media/ stays
// admin-gated; Connect downloads and re-hosts bytes.
func handleSvcMedia(cfg config) camSvcHandler {
	return func(w http.ResponseWriter, r *http.Request, sc camSvcCtx) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			camSvcMethodNotAllowed(w)
			return
		}
		token := strings.TrimSpace(strings.TrimPrefix(r.URL.Path, "/api/cameras/media/"))
		if token == "" || strings.ContainsAny(token, "/\\") {
			camSvcNotFound(w)
			return
		}
		db := camSvcOpenDB(cfg, w)
		if db == nil {
			return
		}
		defer db.Close()
		// Ownership gate: the token's capture must belong to the caller's site.
		// camServeCapture re-checks existence/expiry/path before streaming.
		cap, cerr := getCameraCaptureByToken(db, token)
		if cerr != nil || cap.SiteID != sc.SiteID {
			camSvcNotFound(w)
			return
		}
		camServeCapture(w, r, cfg, db, token, func() { camSvcNotFound(w) })
	}
}

// handleSvcCallback serves POST /api/cameras/callback {url, secret} (registers
// the settle webhook for the token's own site; secret encryptSecret'd) and GET
// → {url, has_secret}.
func handleSvcCallback(cfg config) camSvcHandler {
	return func(w http.ResponseWriter, r *http.Request, sc camSvcCtx) {
		db := camSvcOpenDB(cfg, w)
		if db == nil {
			return
		}
		defer db.Close()
		switch r.Method {
		case http.MethodGet:
			cb, err := getCameraSiteCallback(db, sc.SiteID)
			if err != nil {
				writeJSON(w, http.StatusOK, map[string]any{"url": "", "has_secret": false, "stream_progress": false})
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{
				"url": cb.URL, "has_secret": strings.TrimSpace(cb.SecretEnc) != "", "stream_progress": cb.StreamProgress,
			})
		case http.MethodPost:
			var body struct {
				URL            string `json:"url"`
				Secret         string `json:"secret"`
				StreamProgress *bool  `json:"stream_progress"`
			}
			if !camSvcDecode(w, r, &body) {
				return
			}
			// stream_progress is sticky: an omitted field keeps the current opt-in
			// state so a plain url/secret update never silently disables streaming.
			existing, _ := getCameraSiteCallback(db, sc.SiteID)
			streamProgress := existing.StreamProgress
			if body.StreamProgress != nil {
				streamProgress = *body.StreamProgress
			}
			if err := setCameraSiteCallback(db, cfg, sc.SiteID, body.URL, body.Secret, streamProgress); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
				return
			}
			camlog("info", "svc_callback_register", map[string]any{
				"site_id": sc.SiteID, "url": strings.TrimSpace(body.URL),
				"secret_set": strings.TrimSpace(body.Secret) != "", "stream_progress": streamProgress,
			})
			writeJSON(w, http.StatusOK, map[string]any{"ok": true})
		default:
			camSvcMethodNotAllowed(w)
		}
	}
}

// ─────────────────────────── admin token CRUD ───────────────────────────
// Registered in registerCameraRoutes (camerahttp.go), noStore(requireAdmin(...)).

// handleCameraServiceTokens serves /ui/api/cameras/service-tokens:
// GET → {tokens:[{id,scope,site_id,label,token_preview,enabled,last_used_at,
// created_at}]} (previews only) / POST {scope, site_id?, label} →
// {ok, id, token} — the PLAINTEXT is returned exactly once here.
func handleCameraServiceTokens(cfg config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		db, err := openProxyDB(cfg)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		defer db.Close()
		switch r.Method {
		case http.MethodGet:
			tokens, lerr := listCameraServiceTokens(db)
			if lerr != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]any{"error": lerr.Error()})
				return
			}
			out := make([]map[string]any, 0, len(tokens))
			for _, t := range tokens {
				out = append(out, map[string]any{
					"id": t.ID, "scope": t.Scope, "site_id": t.SiteID, "label": t.Label,
					"token_preview": t.TokenPreview, "enabled": t.Enabled,
					"last_used_at": t.LastUsedAt, "created_at": t.CreatedAt,
				})
			}
			writeJSON(w, http.StatusOK, map[string]any{"tokens": out})
		case http.MethodPost:
			var body struct {
				Scope  string `json:"scope"`
				SiteID string `json:"site_id"`
				Label  string `json:"label"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid JSON"})
				return
			}
			if strings.TrimSpace(body.Scope) == "site" {
				if _, serr := getCameraSite(db, strings.TrimSpace(body.SiteID)); serr != nil {
					writeJSON(w, http.StatusNotFound, map[string]any{"error": "site not found"})
					return
				}
			}
			t, plaintext, merr := mintCameraServiceToken(db, body.Scope, body.SiteID, body.Label)
			if merr != nil {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": merr.Error()})
				return
			}
			camlog("info", "service_token_mint", map[string]any{"token_id": t.ID, "scope": t.Scope, "site_id": t.SiteID})
			writeJSON(w, http.StatusOK, map[string]any{"ok": true, "id": t.ID, "token": plaintext})
		default:
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		}
	}
}

// handleCameraServiceTokenRevoke serves POST /ui/api/cameras/service-tokens/revoke
// {id} → {ok}.
func handleCameraServiceTokenRevoke(cfg config) http.HandlerFunc {
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
		if rerr := revokeCameraServiceToken(db, id); rerr != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": rerr.Error()})
			return
		}
		camlog("info", "service_token_revoke", map[string]any{"token_id": id})
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	}
}

// ─────────────────────────── motion ───────────────────────────

// camMotionArmView renders an arm row for the API (event_types decoded back into
// a JSON array; an unparseable stored value degrades to []).
func camMotionArmView(a camMotionArm) map[string]any {
	types := []string{}
	if strings.TrimSpace(a.EventTypes) != "" {
		_ = json.Unmarshal([]byte(a.EventTypes), &types)
	}
	if types == nil {
		types = []string{}
	}
	return map[string]any{
		"camera_id": a.CameraID, "event_types": types,
		"cooldown_seconds": a.CooldownSeconds, "enabled": a.Enabled,
	}
}

// handleSvcMotionArm serves POST /api/cameras/motion/arm {camera_ids[],
// event_types[], cooldown_seconds, enabled} → upserts one arm row per camera for
// sc.SiteID (every camera_id must belong to the token's site) / GET → the site's
// current arm config. Site is ALWAYS sc.SiteID, never the request.
func handleSvcMotionArm(cfg config) camSvcHandler {
	return func(w http.ResponseWriter, r *http.Request, sc camSvcCtx) {
		db := camSvcOpenDB(cfg, w)
		if db == nil {
			return
		}
		defer db.Close()
		switch r.Method {
		case http.MethodGet:
			arms, err := listCameraMotionArm(db, sc.SiteID)
			if err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
				return
			}
			out := make([]map[string]any, 0, len(arms))
			for _, a := range arms {
				out = append(out, camMotionArmView(a))
			}
			writeJSON(w, http.StatusOK, map[string]any{"arms": out})
		case http.MethodPost:
			var body struct {
				CameraIDs       []string `json:"camera_ids"`
				EventTypes      []string `json:"event_types"`
				CooldownSeconds int      `json:"cooldown_seconds"`
				Enabled         *bool    `json:"enabled"`
			}
			if !camSvcDecode(w, r, &body) {
				return
			}
			if len(body.CameraIDs) == 0 {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": "camera_ids is required"})
				return
			}
			// Validate every camera belongs to the token's site before writing any.
			ids := make([]string, 0, len(body.CameraIDs))
			for _, raw := range body.CameraIDs {
				cam, ok := camSvcCamera(db, w, raw, sc.SiteID)
				if !ok {
					return // camSvcCamera already wrote the 404
				}
				ids = append(ids, cam.ID)
			}
			// Normalize event_types: trim, drop blanks. Empty list = "any type".
			types := make([]string, 0, len(body.EventTypes))
			for _, t := range body.EventTypes {
				if t = strings.TrimSpace(t); t != "" {
					types = append(types, t)
				}
			}
			typesJSON, _ := json.Marshal(types)
			enabled := true
			if body.Enabled != nil {
				enabled = *body.Enabled
			}
			cooldown := body.CooldownSeconds
			if cooldown < 0 {
				cooldown = 0
			}
			for _, id := range ids {
				if err := upsertCameraMotionArm(db, sc.SiteID, id, string(typesJSON), cooldown, enabled); err != nil {
					writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
					return
				}
			}
			camlog("info", "svc_motion_arm", map[string]any{
				"site_id": sc.SiteID, "cameras": len(ids), "enabled": enabled,
				"cooldown_seconds": cooldown, "event_types": types,
			})
			writeJSON(w, http.StatusOK, map[string]any{
				"ok": true, "armed": len(ids), "event_types": types,
				"cooldown_seconds": cooldown, "enabled": enabled,
			})
		default:
			camSvcMethodNotAllowed(w)
		}
	}
}

// handleSvcMotionEvents serves GET /api/cameras/motion/events?camera_ids=&from=
// &to=&event_type= → the site's coalesced motion episodes (started_at/ended_at
// are UTC RFC3339). from/to are parsed as RFC3339 and normalized to UTC for the
// range filter; an unparseable bound is ignored. Always scoped to sc.SiteID.
func handleSvcMotionEvents(cfg config) camSvcHandler {
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
		var ids []string
		for _, part := range strings.Split(q.Get("camera_ids"), ",") {
			if p := strings.TrimSpace(part); p != "" {
				ids = append(ids, p)
			}
		}
		fromTS := camMotionParseBoundUTC(q.Get("from"))
		toTS := camMotionParseBoundUTC(q.Get("to"))
		eventType := strings.TrimSpace(q.Get("event_type"))
		events, truncated, err := listCameraMotionEvents(db, sc.SiteID, ids, fromTS, toTS, eventType)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		names := map[string]string{}
		if cams, cerr := listCamerasBySite(db, sc.SiteID); cerr == nil {
			for _, c := range cams {
				names[c.ID] = camDisplayName(c)
			}
		}
		out := make([]map[string]any, 0, len(events))
		for _, e := range events {
			out = append(out, map[string]any{
				"camera_id": e.CameraID, "camera_name": names[e.CameraID],
				"event_type": e.EventType, "started_at": e.StartedAt, "ended_at": e.EndedAt,
			})
		}
		writeJSON(w, http.StatusOK, map[string]any{"events": out, "truncated": truncated})
	}
}

// camMotionParseBoundUTC parses an RFC3339 range bound into a UTC RFC3339 string
// suitable for the started_at (UTC-stored) comparison; "" for blank/unparseable.
func camMotionParseBoundUTC(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}
