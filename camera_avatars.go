package main

// camera_avatars.go — the avatar registry (design-avatars.md §2.2): named
// humans/vehicles/pets (and GROUPS — is_group=1, e.g. "patients") with
// operator-approved reference imagery, scoped to a subset of the site's DVRs.
//
// Reference media rows point at PINNED camera_captures (expires_at='' — kind
// "avatar_ref") and carry a per-reference face embedding when the sidecar is
// available; the avatar row caches the centroid (camAvatarCentroid), recomputed
// on every reference change. Deleting an avatar or a reference also removes the
// capture row + on-disk file. Operator photo upload is base64-in-JSON
// (window.api is JSON-only; no multipart exists in this codebase).
//
// Schema lives in migrateCameraDB (camera_store.go): camera_avatars +
// camera_avatar_media.

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// camAvatarUploadMax bounds an operator photo-upload request body (base64
// inflates ~4/3, so this comfortably fits a 10MB source image).
const camAvatarUploadMax = 16 << 20

// camAvatar is an in-memory view of a camera_avatars row.
//
// Role provenance is STRUCTURAL: the confirmed trio (Role/RoleNote/
// RoleConfirmedAt) is written only by setCameraAvatarRole (operator decisions
// arriving through the role-update handlers); the suggested/profile fields are
// written only by the profile builder (camera_profile.go). Neither path can
// touch the other's columns, so role != "" always means the operator confirmed
// it — an AI hypothesis can never masquerade as a label.
type camAvatar struct {
	ID          string
	SiteID      string
	Name        string
	Type        string // human | vehicle | pet
	IsGroup     bool   // true = class ("patients"), false = individual
	ExternalRef string // employee id / plate no / chip id
	Description string // rich free text (appearance, habits, schedule)
	DVRIDs      string // JSON array of camera_dvrs.id; '' or '[]' = all DVRs in site
	Enabled     bool
	Embedding   []byte // face-embedding centroid (float32 LE), nil when none

	// USER-owned (setCameraAvatarRole only): the confirmed role label.
	Role            string // canonical slug from the site vocabulary (camera_roles.go); "" = unlabeled
	RoleNote        string // operator note about the assignment
	RoleConfirmedAt string // stamped on every role write (including clearing)

	// AI-owned (profile builder only): the person profile + role hypothesis.
	SuggestedRole           string  // vocabulary slug or "unknown"; NEVER drives behavior
	SuggestedRoleConfidence float64 // 0..1
	ProfileJSON             string  // full profile object incl. role_hypothesis evidence
	ProfileUpdatedAt        string
	ProfileStatus           string // "" | queued | running | ready | error
	ProfileError            string

	CreatedAt string
	UpdatedAt string
}

// camAvatarMedia is one approved reference artifact for an avatar (a PINNED
// capture, kind "avatar_ref").
type camAvatarMedia struct {
	ID              string
	AvatarID        string
	CaptureID       string  // camera_captures.id of the pinned crop
	Token           string  // denormalized capture token (URL building without a join)
	CameraID        string  // "" for operator uploads
	Quality         string  // sub|main ("" for uploads)
	FrameTS         string  // RFC3339 UTC archive instant ("" for uploads)
	BBox            string  // JSON {"x0","y0","x1","y1"} 0-1000, in the SOURCE frame
	Source          string  // scan | upload | investigation
	Note            string  // operator note ("winter coat", "side view")
	MatchConfidence float64 // confidence when proposed (audit trail)
	Embedding       []byte  // per-reference face embedding (float32 LE), nil when none
	CreatedAt       string
}

// ─────────────────────────── CRUD ───────────────────────────

const camAvatarCols = `id, site_id, name, type, is_group, external_ref, description, dvr_ids, enabled, embedding,
	role, role_note, role_confirmed_at, suggested_role, suggested_role_confidence,
	profile_json, profile_updated_at, profile_status, profile_error, created_at, updated_at`

func scanAvatar(s rowScanner) (camAvatar, error) {
	var a camAvatar
	var isGroup, enabled int
	err := s.Scan(&a.ID, &a.SiteID, &a.Name, &a.Type, &isGroup, &a.ExternalRef,
		&a.Description, &a.DVRIDs, &enabled, &a.Embedding,
		&a.Role, &a.RoleNote, &a.RoleConfirmedAt, &a.SuggestedRole, &a.SuggestedRoleConfidence,
		&a.ProfileJSON, &a.ProfileUpdatedAt, &a.ProfileStatus, &a.ProfileError, &a.CreatedAt, &a.UpdatedAt)
	a.IsGroup = isGroup != 0
	a.Enabled = enabled != 0
	return a, err
}

func insertCameraAvatar(db *sql.DB, a camAvatar) (string, error) {
	if a.ID == "" {
		a.ID = randomID("avatar")
	}
	if strings.TrimSpace(a.Type) == "" {
		a.Type = "human"
	}
	now := nowRFC3339()
	if a.CreatedAt == "" {
		a.CreatedAt = now
	}
	if a.UpdatedAt == "" {
		a.UpdatedAt = now
	}
	// A role provided at creation is an operator decision (create handlers
	// validate it against the site vocabulary) — stamp the confirmation.
	if a.Role != "" && a.RoleConfirmedAt == "" {
		a.RoleConfirmedAt = now
	}
	_, err := db.Exec(`INSERT INTO camera_avatars
		(id, site_id, name, type, is_group, external_ref, description, dvr_ids, enabled, embedding,
		 role, role_note, role_confirmed_at, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		a.ID, a.SiteID, a.Name, a.Type, boolToInt(a.IsGroup), a.ExternalRef,
		a.Description, a.DVRIDs, boolToInt(a.Enabled), a.Embedding,
		a.Role, a.RoleNote, a.RoleConfirmedAt, a.CreatedAt, a.UpdatedAt)
	return a.ID, err
}

func getCameraAvatar(db *sql.DB, id string) (camAvatar, error) {
	return scanAvatar(db.QueryRow(`SELECT `+camAvatarCols+` FROM camera_avatars WHERE id = ?`, id))
}

// listCameraAvatars lists a site's avatars (includeDisabled=false → enabled only).
func listCameraAvatars(db *sql.DB, siteID string, includeDisabled bool) ([]camAvatar, error) {
	q := `SELECT ` + camAvatarCols + ` FROM camera_avatars WHERE site_id = ?`
	if !includeDisabled {
		q += ` AND enabled = 1`
	}
	q += ` ORDER BY name COLLATE NOCASE, id`
	rows, err := db.Query(q, siteID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []camAvatar
	for rows.Next() {
		a, err := scanAvatar(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// updateCameraAvatar rewrites the operator-owned descriptive fields. The
// embedding column is deliberately NOT touched here — recomputeAvatarCentroid
// owns it, so a stale in-memory row can never clobber a freshly recomputed
// centroid. Role and profile columns are likewise excluded: setCameraAvatarRole
// owns the confirmed-role trio and the profile builder owns the AI columns.
func updateCameraAvatar(db *sql.DB, a camAvatar) error {
	_, err := db.Exec(`UPDATE camera_avatars
		SET name = ?, type = ?, is_group = ?, external_ref = ?, description = ?, dvr_ids = ?, enabled = ?, updated_at = ?
		WHERE id = ?`,
		a.Name, a.Type, boolToInt(a.IsGroup), a.ExternalRef, a.Description,
		a.DVRIDs, boolToInt(a.Enabled), nowRFC3339(), a.ID)
	return err
}

// setCameraAvatarRole is the ONLY write path for the confirmed role trio —
// operator decisions arriving through the role-update handlers. Every write
// (including clearing to "") stamps role_confirmed_at, so the column always
// answers "when did a human last decide this?". Callers validate the slug
// against the site vocabulary first (camValidateRole).
func setCameraAvatarRole(db *sql.DB, avatarID, role, note string) error {
	now := nowRFC3339()
	_, err := db.Exec(`UPDATE camera_avatars
		SET role = ?, role_note = ?, role_confirmed_at = ?, updated_at = ?
		WHERE id = ?`,
		role, note, now, now, avatarID)
	return err
}

// setCameraAvatarRoleNote updates only the operator's role note (no
// re-confirmation stamp — the label itself didn't change).
func setCameraAvatarRoleNote(db *sql.DB, avatarID, note string) error {
	_, err := db.Exec(`UPDATE camera_avatars SET role_note = ?, updated_at = ? WHERE id = ?`,
		note, nowRFC3339(), avatarID)
	return err
}

// deleteCameraAvatar removes the avatar, its media rows, each reference's
// pinned capture row + on-disk file, and its scans/candidates. Candidate
// evidence captures (7-day expiry) are left for the normal reaper.
func deleteCameraAvatar(db *sql.DB, cfg config, id string) error {
	media, err := listAvatarMedia(db, id)
	if err != nil {
		return err
	}
	// Remove this avatar's media rows FIRST so camDeleteAvatarCapture's
	// reference check counts only OTHER holders of a shared token (a playbook or
	// a different avatar). Otherwise the guard would see these very rows and skip
	// every deletion, orphaning the pinned captures + files.
	if _, err := db.Exec(`DELETE FROM camera_avatar_media WHERE avatar_id = ?`, id); err != nil {
		return err
	}
	for _, m := range media {
		if derr := camDeleteAvatarCapture(db, m.CaptureID, m.Token); derr != nil {
			return derr
		}
	}
	for _, del := range []string{
		`DELETE FROM camera_avatar_candidates WHERE avatar_id = ?`,
		`DELETE FROM camera_avatar_scans WHERE avatar_id = ?`,
		`DELETE FROM camera_avatars WHERE id = ?`,
	} {
		if _, err := db.Exec(del, id); err != nil {
			return err
		}
	}
	return nil
}

// ─────────────────────────── avatar media ───────────────────────────

const camAvatarMediaCols = `id, avatar_id, capture_id, token, camera_id, quality, frame_ts, bbox, source, note, match_confidence, embedding, created_at`

func scanAvatarMedia(s rowScanner) (camAvatarMedia, error) {
	var m camAvatarMedia
	err := s.Scan(&m.ID, &m.AvatarID, &m.CaptureID, &m.Token, &m.CameraID, &m.Quality,
		&m.FrameTS, &m.BBox, &m.Source, &m.Note, &m.MatchConfidence, &m.Embedding, &m.CreatedAt)
	return m, err
}

func insertAvatarMedia(db *sql.DB, m camAvatarMedia) (string, error) {
	if m.ID == "" {
		m.ID = randomID("avref")
	}
	if strings.TrimSpace(m.Source) == "" {
		m.Source = "scan"
	}
	if m.CreatedAt == "" {
		m.CreatedAt = nowRFC3339()
	}
	_, err := db.Exec(`INSERT INTO camera_avatar_media
		(id, avatar_id, capture_id, token, camera_id, quality, frame_ts, bbox, source, note, match_confidence, embedding, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		m.ID, m.AvatarID, m.CaptureID, m.Token, m.CameraID, m.Quality, m.FrameTS,
		m.BBox, m.Source, m.Note, m.MatchConfidence, m.Embedding, m.CreatedAt)
	return m.ID, err
}

func getAvatarMedia(db *sql.DB, id string) (camAvatarMedia, error) {
	return scanAvatarMedia(db.QueryRow(`SELECT `+camAvatarMediaCols+` FROM camera_avatar_media WHERE id = ?`, id))
}

func listAvatarMedia(db *sql.DB, avatarID string) ([]camAvatarMedia, error) {
	rows, err := db.Query(`SELECT `+camAvatarMediaCols+` FROM camera_avatar_media WHERE avatar_id = ? ORDER BY created_at DESC, id`, avatarID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []camAvatarMedia
	for rows.Next() {
		m, err := scanAvatarMedia(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// deleteAvatarMedia removes the media row AND its pinned capture row + file,
// then recomputes the avatar's centroid.
func deleteAvatarMedia(db *sql.DB, cfg config, id string) error {
	m, err := getAvatarMedia(db, id)
	if err != nil {
		return err
	}
	// Delete the media row FIRST so camDeleteAvatarCapture's reference check sees
	// the true remaining references (a shared token stays on disk; an unshared one
	// is removed).
	if _, err := db.Exec(`DELETE FROM camera_avatar_media WHERE id = ?`, id); err != nil {
		return err
	}
	if err := camDeleteAvatarCapture(db, m.CaptureID, m.Token); err != nil {
		return err
	}
	return recomputeAvatarCentroid(db, m.AvatarID)
}

// camDeleteAvatarCapture removes one reference's capture row + on-disk file
// (best-effort on the file — it may already be gone). A missing row is fine.
func camDeleteAvatarCapture(db *sql.DB, captureID, token string) error {
	var (
		c   camCapture
		err error
	)
	if strings.TrimSpace(captureID) != "" {
		c, err = getCameraCapture(db, captureID)
	} else {
		c, err = getCameraCaptureByToken(db, token)
	}
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return err
	}
	// Keep the capture if a playbook or another avatar reference still points at
	// this token (shared pinned image) — destroying it would break the survivor.
	if camCaptureReferenced(db, c.Token) {
		return nil
	}
	if c.Path != "" {
		_ = os.Remove(c.Path)
	}
	_, err = db.Exec(`DELETE FROM camera_captures WHERE id = ?`, c.ID)
	return err
}

// ─────────────────────────── centroid maintenance ───────────────────────────

// recomputeAvatarCentroid reloads the avatar's per-reference embeddings,
// recomputes the L2-normalized centroid (camAvatarCentroid), and stores it on
// camera_avatars.embedding — NULL when no reference has an embedding. Called
// after every media insert/delete/review-approve.
func recomputeAvatarCentroid(db *sql.DB, avatarID string) error {
	rows, err := db.Query(`SELECT embedding FROM camera_avatar_media WHERE avatar_id = ? AND embedding IS NOT NULL`, avatarID)
	if err != nil {
		return err
	}
	defer rows.Close()
	var refs [][]float32
	for rows.Next() {
		var b []byte
		if err := rows.Scan(&b); err != nil {
			return err
		}
		if v := camEmbeddingFromBlob(b); len(v) > 0 {
			refs = append(refs, v)
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	var blob []byte
	if c := camAvatarCentroid(refs); len(c) > 0 {
		blob = camEmbeddingBlob(c)
	}
	_, err = db.Exec(`UPDATE camera_avatars SET embedding = ?, updated_at = ? WHERE id = ?`, blob, nowRFC3339(), avatarID)
	return err
}

// camUploadMaxPixels bounds operator/Connect-supplied images before they reach
// the face sidecar's full cv2.imdecode — a 16MB body can still carry a tiny
// highly-compressed PNG that decodes to a multi-gigabyte bitmap (decompression
// bomb). ~40 megapixels comfortably covers any real CCTV frame or phone photo.
const camUploadMaxPixels = 40_000_000

// camUploadPixelsOK reports whether a header-declared W×H is within the bomb
// guard. Unknown dimensions (0) pass — sniffImage already restricted the type
// and imageDimensions reads only the header, so a 0 means an odd-but-typed file,
// not an oversized one.
func camUploadPixelsOK(w, h int) bool {
	if w <= 0 || h <= 0 {
		return true
	}
	return int64(w)*int64(h) <= camUploadMaxPixels
}

// camAvatarBestFaceEmbedding runs the face sidecar over an image and returns
// the highest-scoring face's embedding blob — nil when the sidecar is off,
// errored, or saw no face (callers store NULL and the VLM path covers it).
func camAvatarBestFaceEmbedding(ctx context.Context, cfg config, img []byte) []byte {
	if !camFaceEnabled(cfg) || len(img) == 0 {
		return nil
	}
	faces, err := camFaceDetect(ctx, cfg, img)
	if err != nil {
		camlog("warn", "avatar_face_embed", map[string]any{"ok": false, "error": err.Error()})
		return nil
	}
	best := -1
	for i, f := range faces {
		if len(f.Embedding) == 0 {
			continue
		}
		if best < 0 || f.Score > faces[best].Score {
			best = i
		}
	}
	if best < 0 {
		return nil
	}
	return camEmbeddingBlob(faces[best].Embedding)
}

// ─────────────────────────── scope + roster ───────────────────────────

// camAvatarDVRScope parses an avatar's dvr_ids column: nil = all DVRs in the
// site (” / '[]' / malformed JSON — malformed data must not hide an avatar).
func camAvatarDVRScope(a camAvatar) []string {
	s := strings.TrimSpace(a.DVRIDs)
	if s == "" || s == "[]" {
		return nil
	}
	var ids []string
	if err := json.Unmarshal([]byte(s), &ids); err != nil {
		return nil
	}
	var out []string
	for _, id := range ids {
		if id = strings.TrimSpace(id); id != "" {
			out = append(out, id)
		}
	}
	return out
}

// camAvatarsForCameras filters avatars down to the ones whose dvr_ids scope
// covers at least one of cams' DVRs (blank or "[]" = all DVRs in the site).
func camAvatarsForCameras(avatars []camAvatar, cams []camera) []camAvatar {
	dvrs := make(map[string]bool, len(cams))
	for _, c := range cams {
		if c.DVRID != "" {
			dvrs[c.DVRID] = true
		}
	}
	var out []camAvatar
	for _, a := range avatars {
		scope := camAvatarDVRScope(a)
		if scope == nil {
			out = append(out, a)
			continue
		}
		for _, id := range scope {
			if dvrs[id] {
				out = append(out, a)
				break
			}
		}
	}
	return out
}

// camAvatarRoster renders the avatars-tool text listing, one line per avatar:
// "- <id> — <name> (<type>[, group]): <first line of description>".
func camAvatarRoster(avatars []camAvatar) string {
	var b strings.Builder
	for i, a := range avatars {
		if i > 0 {
			b.WriteString("\n")
		}
		kind := strings.TrimSpace(a.Type)
		if kind == "" {
			kind = "human"
		}
		if a.IsGroup {
			kind += ", group"
		}
		b.WriteString("- " + a.ID + " — " + a.Name + " (" + kind + ")")
		if d := camFirstLine(a.Description); d != "" {
			b.WriteString(": " + d)
		}
	}
	return b.String()
}

// camFirstLine returns the first non-empty line of s, trimmed.
func camFirstLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			return line
		}
	}
	return ""
}

// ─────────────────────────── JSON views ───────────────────────────

func cameraAvatarJSON(a camAvatar, refCount int) map[string]any {
	dvrIDs := camAvatarDVRScope(a)
	if dvrIDs == nil {
		dvrIDs = []string{}
	}
	return map[string]any{
		"id":            a.ID,
		"site_id":       a.SiteID,
		"name":          a.Name,
		"type":          a.Type,
		"is_group":      a.IsGroup,
		"external_ref":  a.ExternalRef,
		"description":   a.Description,
		"dvr_ids":       dvrIDs,
		"enabled":       a.Enabled,
		"ref_count":     refCount,
		"has_embedding": len(a.Embedding) > 0,
		// Confirmed role (user-owned) + AI suggestion (never authoritative).
		"role":                      a.Role,
		"role_note":                 a.RoleNote,
		"role_confirmed_at":         a.RoleConfirmedAt,
		"suggested_role":            a.SuggestedRole,
		"suggested_role_confidence": a.SuggestedRoleConfidence,
		"profile":                   camDecodeJSON(a.ProfileJSON),
		"profile_status":            a.ProfileStatus,
		"profile_updated_at":        a.ProfileUpdatedAt,
		"profile_error":             a.ProfileError,
		"created_at":                a.CreatedAt,
		"updated_at":                a.UpdatedAt,
	}
}

func cameraAvatarMediaJSON(cfg config, r *http.Request, m camAvatarMedia) map[string]any {
	return map[string]any{
		"id":               m.ID,
		"avatar_id":        m.AvatarID,
		"token":            m.Token,
		"url":              camMediaURL(cfg, r, m.Token),
		"camera_id":        m.CameraID,
		"quality":          m.Quality,
		"frame_ts":         m.FrameTS,
		"bbox":             m.BBox,
		"source":           m.Source,
		"note":             m.Note,
		"match_confidence": m.MatchConfidence,
		"has_embedding":    len(m.Embedding) > 0,
		"created_at":       m.CreatedAt,
	}
}

func cameraAvatarScanJSON(sc camAvatarScan) map[string]any {
	camIDs := []string{}
	if s := strings.TrimSpace(sc.CameraIDs); s != "" && s != "[]" {
		_ = json.Unmarshal([]byte(s), &camIDs)
	}
	return map[string]any{
		"id":         sc.ID,
		"avatar_id":  sc.AvatarID,
		"site_id":    sc.SiteID,
		"camera_ids": camIDs,
		"from_ts":    sc.FromTS,
		"to_ts":      sc.ToTS,
		"alias":      sc.Alias,
		"status":     sc.Status,
		"progress":   sc.Progress,
		"error":      sc.Error,
		"created_at": sc.CreatedAt,
		"updated_at": sc.UpdatedAt,
	}
}

func cameraAvatarCandidateJSON(cfg config, r *http.Request, c camAvatarCandidate) map[string]any {
	out := map[string]any{
		"id":                 c.ID,
		"scan_id":            c.ScanID,
		"avatar_id":          c.AvatarID,
		"camera_id":          c.CameraID,
		"frame_ts":           c.FrameTS,
		"quality":            c.Quality,
		"annotated_token":    c.AnnotatedToken,
		"crop_token":         c.CropToken,
		"bbox":               c.BBox,
		"match_kind":         c.MatchKind,
		"vlm_confidence":     c.VLMConfidence,
		"vlm_reason":         c.VLMReason,
		"status":             c.Status,
		"assigned_avatar_id": c.AssignedAvatarID,
		"note":               c.Note,
		"reviewed_at":        c.ReviewedAt,
		"created_at":         c.CreatedAt,
	}
	if c.AnnotatedToken != "" {
		out["annotated_url"] = camMediaURL(cfg, r, c.AnnotatedToken)
	}
	if c.CropToken != "" {
		out["crop_url"] = camMediaURL(cfg, r, c.CropToken)
	}
	return out
}

// camAvatarNormalizeType validates/normalizes an avatar type ("" → human).
func camAvatarNormalizeType(raw string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", "human":
		return "human", true
	case "vehicle":
		return "vehicle", true
	case "pet":
		return "pet", true
	}
	return "", false
}

// camAvatarDVRIDsJSON serializes a cleaned dvr_ids list ("" when empty = all).
func camAvatarDVRIDsJSON(ids []string) string {
	var clean []string
	for _, id := range ids {
		if id = strings.TrimSpace(id); id != "" {
			clean = append(clean, id)
		}
	}
	if len(clean) == 0 {
		return ""
	}
	return mustJSON(clean)
}

// camAvatarRefCounts returns media (reference) counts keyed by avatar id.
func camAvatarRefCounts(db *sql.DB) (map[string]int, error) {
	rows, err := db.Query(`SELECT avatar_id, COUNT(*) FROM camera_avatar_media GROUP BY avatar_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var id string
		var n int
		if err := rows.Scan(&id, &n); err != nil {
			return nil, err
		}
		out[id] = n
	}
	return out, rows.Err()
}

// camDecodeImageB64 decodes an operator-supplied base64 image, tolerating a
// data:*;base64, prefix and missing padding.
func camDecodeImageB64(raw string) ([]byte, error) {
	s := strings.TrimSpace(raw)
	if strings.HasPrefix(s, "data:") {
		i := strings.Index(s, ";base64,")
		if i < 0 {
			return nil, errors.New("data URL is not base64-encoded")
		}
		s = s[i+len(";base64,"):]
	}
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, errors.New("image_b64 is empty")
	}
	if b, err := base64.StdEncoding.DecodeString(s); err == nil {
		return b, nil
	}
	b, err := base64.RawStdEncoding.DecodeString(s)
	if err != nil {
		return nil, errors.New("invalid base64 image data")
	}
	return b, nil
}

// ─────────────────────────── HTTP handlers ───────────────────────────
// Registered in registerCameraRoutes (camerahttp.go), all noStore(requireAdmin(...)).
// The scan/candidate handlers delegate to camera_avatar_scan.go store funcs.

// handleCameraAvatars serves /ui/api/cameras/avatars:
// GET ?site_id= → {avatars:[{id,site_id,name,type,is_group,external_ref,description,
// dvr_ids,enabled,ref_count,created_at,updated_at}], pending_candidates:N}
// POST {site_id,name,type,is_group,external_ref,description,dvr_ids:[…]} → {ok,id}.
func handleCameraAvatars(cfg config) http.HandlerFunc {
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
			if siteID == "" {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": "site_id is required"})
				return
			}
			avatars, err := listCameraAvatars(db, siteID, true)
			if err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
				return
			}
			counts, cerr := camAvatarRefCounts(db)
			if cerr != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]any{"error": cerr.Error()})
				return
			}
			pending, perr := countPendingAvatarCandidates(db, siteID)
			if perr != nil {
				camlog("warn", "avatar_pending_count", map[string]any{"site_id": siteID, "ok": false, "error": perr.Error()})
				pending = 0
			}
			out := make([]map[string]any, 0, len(avatars))
			for _, a := range avatars {
				out = append(out, cameraAvatarJSON(a, counts[a.ID]))
			}
			writeJSON(w, http.StatusOK, map[string]any{"avatars": out, "pending_candidates": pending})
		case http.MethodPost:
			var body struct {
				SiteID      string   `json:"site_id"`
				Name        string   `json:"name"`
				Type        string   `json:"type"`
				IsGroup     bool     `json:"is_group"`
				ExternalRef string   `json:"external_ref"`
				Description string   `json:"description"`
				DVRIDs      []string `json:"dvr_ids"`
				Enabled     *bool    `json:"enabled"`
				Role        string   `json:"role"`
				RoleNote    string   `json:"role_note"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid JSON"})
				return
			}
			siteID, name := strings.TrimSpace(body.SiteID), strings.TrimSpace(body.Name)
			if siteID == "" || name == "" {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": "site_id and name are required"})
				return
			}
			typ, ok := camAvatarNormalizeType(body.Type)
			if !ok {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": "type must be human, vehicle, or pet"})
				return
			}
			site, serr := getCameraSite(db, siteID)
			if serr != nil {
				writeJSON(w, http.StatusNotFound, map[string]any{"error": "site not found"})
				return
			}
			role := strings.ToLower(strings.TrimSpace(body.Role))
			if !camValidateRole(camParseSitePolicy(site.PolicyJSON), role) {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": "unknown role " + camQuote(role) + " for this site"})
				return
			}
			enabled := true
			if body.Enabled != nil {
				enabled = *body.Enabled
			}
			id, err := insertCameraAvatar(db, camAvatar{
				SiteID: siteID, Name: name, Type: typ, IsGroup: body.IsGroup,
				ExternalRef: strings.TrimSpace(body.ExternalRef), Description: body.Description,
				DVRIDs: camAvatarDVRIDsJSON(body.DVRIDs), Enabled: enabled,
				Role: role, RoleNote: strings.TrimSpace(body.RoleNote),
			})
			if err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
				return
			}
			camlog("info", "avatar_create", map[string]any{"avatar_id": id, "site_id": siteID, "name": name, "type": typ, "is_group": body.IsGroup})
			writeJSON(w, http.StatusOK, map[string]any{"ok": true, "id": id})
		default:
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		}
	}
}

// handleCameraAvatarUpdate serves POST /ui/api/cameras/avatars/update:
// {id, …same fields…, enabled} → {ok,avatar}.
func handleCameraAvatarUpdate(cfg config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
			return
		}
		var body struct {
			ID          string   `json:"id"`
			Name        string   `json:"name"`
			Type        string   `json:"type"`
			IsGroup     *bool    `json:"is_group"`
			ExternalRef *string  `json:"external_ref"`
			Description *string  `json:"description"`
			DVRIDs      []string `json:"dvr_ids"`
			Enabled     *bool    `json:"enabled"`
			Role        *string  `json:"role"`
			RoleNote    *string  `json:"role_note"`
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
		a, gerr := getCameraAvatar(db, id)
		if gerr != nil {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "avatar not found"})
			return
		}
		if n := strings.TrimSpace(body.Name); n != "" {
			a.Name = n
		}
		if strings.TrimSpace(body.Type) != "" {
			typ, ok := camAvatarNormalizeType(body.Type)
			if !ok {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": "type must be human, vehicle, or pet"})
				return
			}
			a.Type = typ
		}
		if body.IsGroup != nil {
			a.IsGroup = *body.IsGroup
		}
		if body.ExternalRef != nil {
			a.ExternalRef = strings.TrimSpace(*body.ExternalRef)
		}
		if body.Description != nil {
			a.Description = *body.Description
		}
		if body.DVRIDs != nil {
			a.DVRIDs = camAvatarDVRIDsJSON(body.DVRIDs)
		}
		if body.Enabled != nil {
			a.Enabled = *body.Enabled
		}
		// Validate the role BEFORE any write so a 400 never leaves a partial
		// update behind (descriptive fields persisted, role rejected).
		role := ""
		if body.Role != nil {
			role = strings.ToLower(strings.TrimSpace(*body.Role))
			site, serr := getCameraSite(db, a.SiteID)
			if serr != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]any{"error": serr.Error()})
				return
			}
			if !camValidateRole(camParseSitePolicy(site.PolicyJSON), role) {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": "unknown role " + camQuote(role) + " for this site"})
				return
			}
		}
		if err := updateCameraAvatar(db, a); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		// Role writes go through the dedicated setter (stamps role_confirmed_at) —
		// an operator decision, validated against the site's vocabulary.
		if body.Role != nil {
			note := a.RoleNote
			if body.RoleNote != nil {
				note = strings.TrimSpace(*body.RoleNote)
			}
			if err := setCameraAvatarRole(db, a.ID, role, note); err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
				return
			}
		} else if body.RoleNote != nil {
			if err := setCameraAvatarRoleNote(db, a.ID, strings.TrimSpace(*body.RoleNote)); err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
				return
			}
		}
		camlog("info", "avatar_update", map[string]any{"avatar_id": id})
		a, _ = getCameraAvatar(db, id)
		counts, _ := camAvatarRefCounts(db)
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "avatar": cameraAvatarJSON(a, counts[a.ID])})
	}
}

// handleCameraAvatarDelete serves POST /ui/api/cameras/avatars/delete: {id} —
// cascades media rows + pinned files + candidates → {ok}.
func handleCameraAvatarDelete(cfg config) http.HandlerFunc {
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
		if _, gerr := getCameraAvatar(db, id); gerr != nil {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "avatar not found"})
			return
		}
		if err := deleteCameraAvatar(db, cfg, id); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		camlog("info", "avatar_delete", map[string]any{"avatar_id": id})
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	}
}

// handleCameraAvatarMedia serves GET /ui/api/cameras/avatars/media?avatar_id= →
// {media:[{id,token,url,camera_id,frame_ts,source,note,match_confidence,created_at}]}.
func handleCameraAvatarMedia(cfg config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
			return
		}
		avatarID := strings.TrimSpace(r.URL.Query().Get("avatar_id"))
		if avatarID == "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "avatar_id is required"})
			return
		}
		db, err := openProxyDB(cfg)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		defer db.Close()
		media, err := listAvatarMedia(db, avatarID)
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

// handleCameraAvatarMediaUpload serves POST /ui/api/cameras/avatars/media/upload:
// {avatar_id, image_b64, note} — http.MaxBytesReader 16MB, strips any
// data:*;base64, prefix, sniffs jpeg/png, persists PINNED (kind "avatar_ref"),
// embeds the face via the sidecar when available, recomputes the centroid → {ok,media}.
func handleCameraAvatarMediaUpload(cfg config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, camAvatarUploadMax)
		var body struct {
			AvatarID string `json:"avatar_id"`
			ImageB64 string `json:"image_b64"`
			Note     string `json:"note"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			var tooBig *http.MaxBytesError
			if errors.As(err, &tooBig) {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": "image too large (max 16MB)"})
				return
			}
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid JSON"})
			return
		}
		avatarID := strings.TrimSpace(body.AvatarID)
		if avatarID == "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "avatar_id is required"})
			return
		}
		db, err := openProxyDB(cfg)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		defer db.Close()
		avatar, gerr := getCameraAvatar(db, avatarID)
		if gerr != nil {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "avatar not found"})
			return
		}
		data, derr := camDecodeImageB64(body.ImageB64)
		if derr != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": derr.Error()})
			return
		}
		scratch, serr := os.MkdirTemp("", "camavatar-")
		if serr != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": serr.Error()})
			return
		}
		defer os.RemoveAll(scratch)
		tmp := filepath.Join(scratch, "upload.bin")
		if err := os.WriteFile(tmp, data, 0o600); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		ct, _ := sniffImage(tmp)
		if ct != "image/jpeg" && ct != "image/png" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "image must be JPEG or PNG"})
			return
		}
		wpx, hpx := imageDimensions(tmp)
		if !camUploadPixelsOK(wpx, hpx) {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "image resolution is too large — a decompression-bomb guard (max ~40 megapixels) rejected it"})
			return
		}
		token, perr := camPersistCaptureExpires(db, cfg, "", avatar.SiteID, "", "avatar_ref", "", tmp, ct, wpx, hpx, "", "", "")
		if perr != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": perr.Error()})
			return
		}
		capRow, cerr := getCameraCaptureByToken(db, token)
		if cerr != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": cerr.Error()})
			return
		}
		emb := camAvatarBestFaceEmbedding(r.Context(), cfg, data)
		mediaID, merr := insertAvatarMedia(db, camAvatarMedia{
			AvatarID: avatar.ID, CaptureID: capRow.ID, Token: token,
			Source: "upload", Note: strings.TrimSpace(body.Note), Embedding: emb,
		})
		if merr != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": merr.Error()})
			return
		}
		if rerr := recomputeAvatarCentroid(db, avatar.ID); rerr != nil {
			camlog("warn", "avatar_centroid", map[string]any{"avatar_id": avatar.ID, "ok": false, "error": rerr.Error()})
		}
		camlog("info", "avatar_media_upload", map[string]any{
			"avatar_id": avatar.ID, "media_id": mediaID, "bytes": len(data),
			"content_type": ct, "face_embedded": len(emb) > 0,
		})
		m, _ := getAvatarMedia(db, mediaID)
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "media": cameraAvatarMediaJSON(cfg, r, m)})
	}
}

// handleCameraAvatarMediaDelete serves POST /ui/api/cameras/avatars/media/delete:
// {id} → deleteAvatarMedia → {ok}.
func handleCameraAvatarMediaDelete(cfg config) http.HandlerFunc {
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
		if err := deleteAvatarMedia(db, cfg, id); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				writeJSON(w, http.StatusNotFound, map[string]any{"error": "media not found"})
				return
			}
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		camlog("info", "avatar_media_delete", map[string]any{"media_id": id})
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	}
}

// handleCameraAvatarScan serves POST /ui/api/cameras/avatars/scan:
// {avatar_id, camera_ids:[…], from, to, alias?} → validates S3 is enabled + the
// window, enqueues a camera_avatar_scans row (status "queued") → {ok,id}.
func handleCameraAvatarScan(cfg config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
			return
		}
		var body struct {
			AvatarID  string   `json:"avatar_id"`
			CameraIDs []string `json:"camera_ids"`
			From      string   `json:"from"`
			To        string   `json:"to"`
			Alias     string   `json:"alias"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid JSON"})
			return
		}
		avatarID := strings.TrimSpace(body.AvatarID)
		if avatarID == "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "avatar_id is required"})
			return
		}
		if !camS3Enabled(cfg) {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "enrollment scans need the S3 frame archive — configure PROXY_CAMERA_S3_* first"})
			return
		}
		from, to, werr := camParseWindow(body.From, body.To, cfg, false)
		if werr != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": werr.Error()})
			return
		}
		db, err := openProxyDB(cfg)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		defer db.Close()
		avatar, gerr := getCameraAvatar(db, avatarID)
		if gerr != nil {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "avatar not found"})
			return
		}
		if !cfg.CameraAvatarScanWorkerEnabled {
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "the avatar enrollment-scan worker is disabled (PROXY_CAM_AVATAR_SCAN_WORKER) — a scan would never run; enable it and retry"})
			return
		}
		id, ierr := insertCameraAvatarScan(db, camAvatarScan{
			AvatarID: avatar.ID, SiteID: avatar.SiteID,
			CameraIDs: camAvatarDVRIDsJSON(body.CameraIDs),
			FromTS:    from.UTC().Format(time.RFC3339), ToTS: to.UTC().Format(time.RFC3339),
			Alias: strings.TrimSpace(body.Alias), Status: "queued",
		})
		if ierr != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": ierr.Error()})
			return
		}
		camlog("info", "avatar_scan_enqueue", map[string]any{
			"scan_id": id, "avatar_id": avatar.ID, "site_id": avatar.SiteID,
			"from": from.UTC().Format(time.RFC3339), "to": to.UTC().Format(time.RFC3339),
			"cameras": len(body.CameraIDs),
		})
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "id": id})
	}
}

// handleCameraAvatarScans serves GET /ui/api/cameras/avatars/scans?avatar_id= or
// ?site_id= → {scans:[{id,avatar_id,status,progress,from_ts,to_ts,error,created_at}]}.
func handleCameraAvatarScans(cfg config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
			return
		}
		avatarID := strings.TrimSpace(r.URL.Query().Get("avatar_id"))
		siteID := strings.TrimSpace(r.URL.Query().Get("site_id"))
		if avatarID == "" && siteID == "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "avatar_id or site_id is required"})
			return
		}
		db, err := openProxyDB(cfg)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		defer db.Close()
		scans, err := listCameraAvatarScans(db, avatarID, siteID)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		out := make([]map[string]any, 0, len(scans))
		for _, sc := range scans {
			out = append(out, cameraAvatarScanJSON(sc))
		}
		writeJSON(w, http.StatusOK, map[string]any{"scans": out})
	}
}

// handleCameraAvatarCandidates serves GET /ui/api/cameras/avatars/candidates
// ?scan_id= or ?avatar_id=&status=pending → {candidates:[…]} (annotated/crop
// tokens rendered as urls via camMediaURL).
func handleCameraAvatarCandidates(cfg config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
			return
		}
		scanID := strings.TrimSpace(r.URL.Query().Get("scan_id"))
		avatarID := strings.TrimSpace(r.URL.Query().Get("avatar_id"))
		status := strings.TrimSpace(r.URL.Query().Get("status"))
		if scanID == "" && avatarID == "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "scan_id or avatar_id is required"})
			return
		}
		db, err := openProxyDB(cfg)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		defer db.Close()
		cands, err := listCameraAvatarCandidates(db, scanID, avatarID, status)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		out := make([]map[string]any, 0, len(cands))
		for _, c := range cands {
			out = append(out, cameraAvatarCandidateJSON(cfg, r, c))
		}
		writeJSON(w, http.StatusOK, map[string]any{"candidates": out})
	}
}

// camCropHTTPError carries the HTTP status a review handler should return when
// a candidate crop could not be acquired (default 500 for plain errors).
type camCropHTTPError struct {
	status int
	msg    string
}

func (e *camCropHTTPError) Error() string { return e.msg }

// camCropErrStatus maps a camCandidatePinnedCrop error to its HTTP status.
func camCropErrStatus(err error) int {
	var he *camCropHTTPError
	if errors.As(err, &he) {
		return he.status
	}
	return http.StatusInternalServerError
}

// camCandidatePinnedCrop acquires a candidate's crop as a PINNED capture: it
// pins the candidate's crop capture if it (row + file) still exists, else
// re-fetches the archived frame from S3 by (camera_id, quality, frame_ts),
// re-crops with the stored bbox, and mints a fresh pinned capture. Shared by
// the admin and service review handlers so an expired crop is re-hydrated on
// BOTH paths instead of failing the review.
func camCandidatePinnedCrop(ctx context.Context, cfg config, db *sql.DB, cand camAvatarCandidate, siteID string) (camCapture, []byte, error) {
	var (
		capRow    camCapture
		cropBytes []byte
	)
	if existing, cerr := getCameraCaptureByToken(db, cand.CropToken); cerr == nil && strings.TrimSpace(cand.CropToken) != "" {
		if b, rerr := os.ReadFile(existing.Path); rerr == nil {
			cropBytes = b
			capRow = existing
		}
	}
	if capRow.ID != "" {
		if err := pinCameraCapture(db, cand.CropToken); err != nil {
			return camCapture{}, nil, err
		}
		return capRow, cropBytes, nil
	}
	if !camS3Enabled(cfg) {
		return camCapture{}, nil, &camCropHTTPError{http.StatusNotFound, "candidate media expired and the S3 frame archive is not configured"}
	}
	ts, perr := time.Parse(time.RFC3339, cand.FrameTS)
	if perr != nil {
		return camCapture{}, nil, &camCropHTTPError{http.StatusInternalServerError, "candidate has an unparseable frame_ts: " + cand.FrameTS}
	}
	frame, ferr := s3GetObject(ctx, cfg, camS3FrameKey(cand.CameraID, firstNonEmpty(cand.Quality, "sub"), ts))
	if ferr != nil {
		if errors.Is(ferr, errS3NotFound) {
			return camCapture{}, nil, &camCropHTTPError{http.StatusNotFound, "archived frame no longer available"}
		}
		return camCapture{}, nil, &camCropHTTPError{http.StatusBadGateway, "fetch archived frame: " + ferr.Error()}
	}
	cropBytes = frame
	if bb, ok := camParseBBox(json.RawMessage(cand.BBox)); ok {
		if crop, _, _, cerr := camCropJPEG(frame, bb, 15); cerr == nil {
			cropBytes = crop
		} else {
			camlog("warn", "avatar_candidate_recrop", map[string]any{"candidate_id": cand.ID, "ok": false, "error": cerr.Error()})
		}
	}
	scratch, serr := os.MkdirTemp("", "camavatar-")
	if serr != nil {
		return camCapture{}, nil, serr
	}
	defer os.RemoveAll(scratch)
	tmp := filepath.Join(scratch, "crop.jpg")
	if err := os.WriteFile(tmp, cropBytes, 0o600); err != nil {
		return camCapture{}, nil, err
	}
	wpx, hpx := imageDimensions(tmp)
	token, perr2 := camPersistCaptureExpires(db, cfg, "", siteID, cand.CameraID, "avatar_ref", cand.Quality, tmp, "image/jpeg", wpx, hpx, cand.FrameTS, "", "")
	if perr2 != nil {
		return camCapture{}, nil, perr2
	}
	capRow, gerr := getCameraCaptureByToken(db, token)
	if gerr != nil {
		return camCapture{}, nil, gerr
	}
	return capRow, cropBytes, nil
}

// handleCameraAvatarCandidateReview serves POST /ui/api/cameras/avatars/candidates/review:
// {id, action:"approve"|"reject"|"group", group_avatar_id?, note?} — approve pins
// the crop (re-fetching the frame from S3 by (camera_id, quality, frame_ts) +
// stored bbox when the 7-day candidate media already expired), inserts an
// avatar_media row (into avatar_id, or group_avatar_id for "group"), stores the
// face embedding, recomputes the centroid → {ok}.
func handleCameraAvatarCandidateReview(cfg config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
			return
		}
		var body struct {
			ID            string `json:"id"`
			Action        string `json:"action"`
			GroupAvatarID string `json:"group_avatar_id"`
			Note          string `json:"note"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid JSON"})
			return
		}
		id := strings.TrimSpace(body.ID)
		action := strings.ToLower(strings.TrimSpace(body.Action))
		if id == "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "id is required"})
			return
		}
		if action != "approve" && action != "reject" && action != "group" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "action must be approve, reject, or group"})
			return
		}
		note := strings.TrimSpace(body.Note)
		db, err := openProxyDB(cfg)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		defer db.Close()
		cand, gerr := getCameraAvatarCandidate(db, id)
		if gerr != nil {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "candidate not found"})
			return
		}
		if cand.Status != "pending" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "candidate already reviewed (" + cand.Status + ")"})
			return
		}
		if action == "reject" {
			if err := setCameraAvatarCandidateReview(db, id, "rejected", "", note); err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
				return
			}
			camlog("info", "avatar_candidate_review", map[string]any{"candidate_id": id, "action": "reject"})
			writeJSON(w, http.StatusOK, map[string]any{"ok": true, "status": "rejected"})
			return
		}
		targetID, status, assigned := cand.AvatarID, "approved", ""
		if action == "group" {
			targetID = strings.TrimSpace(body.GroupAvatarID)
			if targetID == "" {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": "group_avatar_id is required for action group"})
				return
			}
			status, assigned = "grouped", targetID
		}
		target, terr := getCameraAvatar(db, targetID)
		if terr != nil {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "avatar not found"})
			return
		}
		capRow, cropBytes, aerr := camCandidatePinnedCrop(r.Context(), cfg, db, cand, target.SiteID)
		if aerr != nil {
			writeJSON(w, camCropErrStatus(aerr), map[string]any{"error": aerr.Error()})
			return
		}
		emb := camAvatarBestFaceEmbedding(r.Context(), cfg, cropBytes)
		mediaID, merr := insertAvatarMedia(db, camAvatarMedia{
			AvatarID: target.ID, CaptureID: capRow.ID, Token: capRow.Token,
			CameraID: cand.CameraID, Quality: cand.Quality, FrameTS: cand.FrameTS,
			BBox: cand.BBox, Source: "scan", Note: note,
			MatchConfidence: cand.VLMConfidence, Embedding: emb,
		})
		if merr != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": merr.Error()})
			return
		}
		if rerr := recomputeAvatarCentroid(db, target.ID); rerr != nil {
			camlog("warn", "avatar_centroid", map[string]any{"avatar_id": target.ID, "ok": false, "error": rerr.Error()})
		}
		if err := setCameraAvatarCandidateReview(db, id, status, assigned, note); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		camlog("info", "avatar_candidate_review", map[string]any{
			"candidate_id": id, "action": action, "avatar_id": target.ID,
			"media_id": mediaID, "face_embedded": len(emb) > 0,
		})
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "status": status, "media_id": mediaID})
	}
}
