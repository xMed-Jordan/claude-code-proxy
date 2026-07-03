package main

// camera_avatars_test.go — focused unit tests for the avatar registry's pure
// helpers (scope filter, roster rendering) plus temp-sqlite coverage of the
// centroid recompute path, the delete cascade, and the base64 upload handler's
// reject paths. No network, no DVR.

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCamAvatarsForCamerasDVRScope(t *testing.T) {
	cams := []camera{{ID: "cam1", DVRID: "dvr_a"}, {ID: "cam2", DVRID: "dvr_b"}}
	avatars := []camAvatar{
		{ID: "av_blank", DVRIDs: ""},               // all DVRs
		{ID: "av_empty", DVRIDs: "[]"},             // all DVRs
		{ID: "av_a", DVRIDs: `["dvr_a"]`},          // matches cam1
		{ID: "av_c", DVRIDs: `["dvr_c"]`},          // out of scope
		{ID: "av_cb", DVRIDs: `["dvr_c","dvr_b"]`}, // matches cam2
		{ID: "av_bad", DVRIDs: `{not json`},        // malformed = treated as all
	}
	got := camAvatarsForCameras(avatars, cams)
	want := []string{"av_blank", "av_empty", "av_a", "av_cb", "av_bad"}
	if len(got) != len(want) {
		t.Fatalf("got %d avatars, want %d (%+v)", len(got), len(want), got)
	}
	for i, id := range want {
		if got[i].ID != id {
			t.Errorf("avatar[%d] = %q, want %q", i, got[i].ID, id)
		}
	}
	// With no cameras only the all-scope avatars survive.
	got = camAvatarsForCameras(avatars, nil)
	want = []string{"av_blank", "av_empty", "av_bad"}
	if len(got) != len(want) {
		t.Fatalf("no-cams: got %d avatars, want %d", len(got), len(want))
	}
	for i, id := range want {
		if got[i].ID != id {
			t.Errorf("no-cams avatar[%d] = %q, want %q", i, got[i].ID, id)
		}
	}
}

func TestCamAvatarRosterFormat(t *testing.T) {
	avatars := []camAvatar{
		{ID: "avatar_1", Name: "Dr. Ahmad", Type: "human", Description: "White coat, arrives 8am.\nUsually in ward 2."},
		{ID: "avatar_2", Name: "Patients", IsGroup: true}, // blank type defaults to human; no description
		{ID: "avatar_3", Name: "Delivery van", Type: "vehicle", Description: "\n  White Toyota HiAce  \nplate 123"},
	}
	got := camAvatarRoster(avatars)
	want := "- avatar_1 — Dr. Ahmad (human): White coat, arrives 8am." +
		"\n- avatar_2 — Patients (human, group)" +
		"\n- avatar_3 — Delivery van (vehicle): White Toyota HiAce"
	if got != want {
		t.Errorf("roster mismatch:\ngot:\n%s\nwant:\n%s", got, want)
	}
	if camAvatarRoster(nil) != "" {
		t.Errorf("empty roster should be \"\", got %q", camAvatarRoster(nil))
	}
}

func TestCamAvatarCRUDRoundTrip(t *testing.T) {
	db := openCamTestDB(t)
	if err := migrateCameraDB(db); err != nil {
		t.Fatalf("migrateCameraDB: %v", err)
	}
	id, err := insertCameraAvatar(db, camAvatar{
		SiteID: "site_1", Name: "Dr. Ahmad", IsGroup: false,
		ExternalRef: "EMP-7", Description: "white coat", DVRIDs: `["dvr_1"]`, Enabled: true,
	})
	if err != nil {
		t.Fatalf("insertCameraAvatar: %v", err)
	}
	a, err := getCameraAvatar(db, id)
	if err != nil {
		t.Fatalf("getCameraAvatar: %v", err)
	}
	if a.Type != "human" {
		t.Errorf("blank type should default to human, got %q", a.Type)
	}
	if a.Name != "Dr. Ahmad" || a.ExternalRef != "EMP-7" || a.DVRIDs != `["dvr_1"]` || !a.Enabled {
		t.Errorf("round-trip mismatch: %+v", a)
	}
	// enabled-only listing excludes a disabled avatar.
	if _, err := insertCameraAvatar(db, camAvatar{SiteID: "site_1", Name: "Old guard", Enabled: false}); err != nil {
		t.Fatalf("insert disabled: %v", err)
	}
	all, err := listCameraAvatars(db, "site_1", true)
	if err != nil || len(all) != 2 {
		t.Fatalf("listCameraAvatars(all) = %d avatars, err %v; want 2", len(all), err)
	}
	enabled, err := listCameraAvatars(db, "site_1", false)
	if err != nil || len(enabled) != 1 || enabled[0].ID != id {
		t.Fatalf("listCameraAvatars(enabled) = %+v, err %v; want just %s", enabled, err, id)
	}
	// update rewrites fields but never clobbers the embedding column.
	if _, err := db.Exec(`UPDATE camera_avatars SET embedding = ? WHERE id = ?`, []byte{1, 2, 3, 4}, id); err != nil {
		t.Fatalf("seed embedding: %v", err)
	}
	a.Name = "Dr. A."
	a.Embedding = nil // stale in-memory row
	if err := updateCameraAvatar(db, a); err != nil {
		t.Fatalf("updateCameraAvatar: %v", err)
	}
	a2, _ := getCameraAvatar(db, id)
	if a2.Name != "Dr. A." {
		t.Errorf("update did not apply, name %q", a2.Name)
	}
	if !bytes.Equal(a2.Embedding, []byte{1, 2, 3, 4}) {
		t.Errorf("updateCameraAvatar clobbered embedding: %v", a2.Embedding)
	}
}

func TestRecomputeAvatarCentroid(t *testing.T) {
	db := openCamTestDB(t)
	if err := migrateCameraDB(db); err != nil {
		t.Fatalf("migrateCameraDB: %v", err)
	}
	id, err := insertCameraAvatar(db, camAvatar{SiteID: "site_1", Name: "Centroid", Enabled: true})
	if err != nil {
		t.Fatalf("insertCameraAvatar: %v", err)
	}
	blobs := [][]byte{
		camEmbeddingBlob([]float32{1, 0, 0}),
		camEmbeddingBlob([]float32{0, 1, 0}),
	}
	for i, b := range blobs {
		if _, err := insertAvatarMedia(db, camAvatarMedia{
			AvatarID: id, CaptureID: "cap_x", Token: "tok_" + string(rune('a'+i)),
			Source: "upload", Embedding: b,
		}); err != nil {
			t.Fatalf("insertAvatarMedia[%d]: %v", i, err)
		}
	}
	if err := recomputeAvatarCentroid(db, id); err != nil {
		t.Fatalf("recomputeAvatarCentroid: %v", err)
	}
	// Self-consistent expectation: decode the same blobs the helper reads and
	// feed them through camAvatarCentroid/camEmbeddingBlob.
	var refs [][]float32
	for _, b := range blobs {
		if v := camEmbeddingFromBlob(b); len(v) > 0 {
			refs = append(refs, v)
		}
	}
	var want []byte
	if c := camAvatarCentroid(refs); len(c) > 0 {
		want = camEmbeddingBlob(c)
	}
	a, err := getCameraAvatar(db, id)
	if err != nil {
		t.Fatalf("getCameraAvatar: %v", err)
	}
	if !bytes.Equal(a.Embedding, want) {
		t.Errorf("centroid blob mismatch: got %d bytes, want %d bytes", len(a.Embedding), len(want))
	}
	// Removing every reference must NULL the centroid.
	if _, err := db.Exec(`DELETE FROM camera_avatar_media WHERE avatar_id = ?`, id); err != nil {
		t.Fatalf("clear media: %v", err)
	}
	if err := recomputeAvatarCentroid(db, id); err != nil {
		t.Fatalf("recomputeAvatarCentroid(empty): %v", err)
	}
	a, _ = getCameraAvatar(db, id)
	if len(a.Embedding) != 0 {
		t.Errorf("centroid should be nil with no references, got %d bytes", len(a.Embedding))
	}
}

func TestDeleteCameraAvatarCascades(t *testing.T) {
	db := openCamTestDB(t)
	if err := migrateCameraDB(db); err != nil {
		t.Fatalf("migrateCameraDB: %v", err)
	}
	id, err := insertCameraAvatar(db, camAvatar{SiteID: "site_1", Name: "Cascade", Enabled: true})
	if err != nil {
		t.Fatalf("insertCameraAvatar: %v", err)
	}
	// A pinned reference capture with a real on-disk file.
	refPath := filepath.Join(t.TempDir(), "ref.jpg")
	if err := os.WriteFile(refPath, []byte("fake-jpeg-bytes"), 0o600); err != nil {
		t.Fatalf("write ref file: %v", err)
	}
	capID, err := insertCameraCapture(db, camCapture{
		SiteID: "site_1", Kind: "avatar_ref", Token: "tok_cascade",
		Path: refPath, ContentType: "image/jpeg", ExpiresAt: "",
	})
	if err != nil {
		t.Fatalf("insertCameraCapture: %v", err)
	}
	if _, err := insertAvatarMedia(db, camAvatarMedia{AvatarID: id, CaptureID: capID, Token: "tok_cascade", Source: "upload"}); err != nil {
		t.Fatalf("insertAvatarMedia: %v", err)
	}
	// A scan + candidate row (inserted directly; the W5 store owns the real API).
	if _, err := db.Exec(`INSERT INTO camera_avatar_scans (id, avatar_id, site_id, from_ts, to_ts, created_at, updated_at)
		VALUES ('avscan_t', ?, 'site_1', 'f', 't', 'c', 'u')`, id); err != nil {
		t.Fatalf("insert scan: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO camera_avatar_candidates (id, scan_id, avatar_id, camera_id, frame_ts, created_at)
		VALUES ('avcand_t', 'avscan_t', ?, 'cam1', 'f', 'c')`, id); err != nil {
		t.Fatalf("insert candidate: %v", err)
	}
	if err := deleteCameraAvatar(db, config{}, id); err != nil {
		t.Fatalf("deleteCameraAvatar: %v", err)
	}
	for _, check := range []struct{ table, where string }{
		{"camera_avatars", "id"},
		{"camera_avatar_media", "avatar_id"},
		{"camera_avatar_scans", "avatar_id"},
		{"camera_avatar_candidates", "avatar_id"},
	} {
		var n int
		if err := db.QueryRow(`SELECT COUNT(*) FROM `+check.table+` WHERE `+check.where+` = ?`, id).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", check.table, err)
		}
		if n != 0 {
			t.Errorf("%s still has %d rows for %s after delete", check.table, n, id)
		}
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM camera_captures WHERE id = ?`, capID).Scan(&n); err != nil || n != 0 {
		t.Errorf("capture row survived cascade (n=%d, err=%v)", n, err)
	}
	if _, err := os.Stat(refPath); !os.IsNotExist(err) {
		t.Errorf("reference file survived cascade: %v", err)
	}
}

func TestHandleCameraAvatarMediaUploadRejects(t *testing.T) {
	dir := t.TempDir()
	cfg := config{DBPath: filepath.Join(dir, "proxy.db"), CameraMediaDir: filepath.Join(dir, "media")}
	db, err := openProxyDB(cfg)
	if err != nil {
		t.Fatalf("openProxyDB: %v", err)
	}
	if err := migrateCameraDB(db); err != nil {
		t.Fatalf("migrateCameraDB: %v", err)
	}
	avatarID, err := insertCameraAvatar(db, camAvatar{SiteID: "site_1", Name: "Upload", Enabled: true})
	if err != nil {
		t.Fatalf("insertCameraAvatar: %v", err)
	}
	db.Close()

	h := handleCameraAvatarMediaUpload(cfg)
	post := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/ui/api/cameras/avatars/media/upload", strings.NewReader(body))
		rec := httptest.NewRecorder()
		h(rec, req)
		return rec
	}

	// Bad base64 → clean 400.
	if rec := post(`{"avatar_id":"` + avatarID + `","image_b64":"@@@not-base64@@@"}`); rec.Code != http.StatusBadRequest {
		t.Errorf("bad base64: status %d, want 400 (body %s)", rec.Code, rec.Body.String())
	}
	// Valid base64 that is not an image → 400.
	notImg := base64.StdEncoding.EncodeToString([]byte("plain text, definitely not an image"))
	if rec := post(`{"avatar_id":"` + avatarID + `","image_b64":"` + notImg + `"}`); rec.Code != http.StatusBadRequest {
		t.Errorf("non-image: status %d, want 400 (body %s)", rec.Code, rec.Body.String())
	}
	// Oversize body (> 16MB) → clean 400.
	big := `{"avatar_id":"` + avatarID + `","image_b64":"` + strings.Repeat("A", camAvatarUploadMax+1024) + `"}`
	if rec := post(big); rec.Code != http.StatusBadRequest {
		t.Errorf("oversize: status %d, want 400 (body %s)", rec.Code, rec.Body.String())
	}
	// Unknown avatar → 404.
	tiny := base64.StdEncoding.EncodeToString(tinyPNG(t))
	if rec := post(`{"avatar_id":"avatar_missing","image_b64":"` + tiny + `"}`); rec.Code != http.StatusNotFound {
		t.Errorf("missing avatar: status %d, want 404 (body %s)", rec.Code, rec.Body.String())
	}
	// Happy path: PNG with a data-URL prefix → 200, media row + PINNED capture.
	rec := post(`{"avatar_id":"` + avatarID + `","image_b64":"data:image/png;base64,` + tiny + `","note":"front view"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("upload: status %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	db, err = openProxyDB(cfg)
	if err != nil {
		t.Fatalf("reopen db: %v", err)
	}
	defer db.Close()
	media, err := listAvatarMedia(db, avatarID)
	if err != nil || len(media) != 1 {
		t.Fatalf("listAvatarMedia = %d rows, err %v; want 1", len(media), err)
	}
	if media[0].Source != "upload" || media[0].Note != "front view" {
		t.Errorf("media row mismatch: %+v", media[0])
	}
	capRow, err := getCameraCaptureByToken(db, media[0].Token)
	if err != nil {
		t.Fatalf("getCameraCaptureByToken: %v", err)
	}
	if capRow.ExpiresAt != "" {
		t.Errorf("uploaded reference must be PINNED (expires_at=\"\"), got %q", capRow.ExpiresAt)
	}
	if capRow.Kind != "avatar_ref" || capRow.ContentType != "image/png" {
		t.Errorf("capture row mismatch: kind=%q ct=%q", capRow.Kind, capRow.ContentType)
	}
	if _, err := os.Stat(capRow.Path); err != nil {
		t.Errorf("pinned file missing on disk: %v", err)
	}
}

// tinyPNG encodes a 4x4 PNG in-process (no fixtures on disk).
func tinyPNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 4, 4))
	img.Set(1, 1, color.RGBA{R: 255, A: 255})
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("png.Encode: %v", err)
	}
	return buf.Bytes()
}
