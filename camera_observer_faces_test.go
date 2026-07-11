package main

// camera_observer_faces_test.go — the observer face-recognition step
// (camera_observer_faces.go), driven through the camObserverFaceDetectFn seam
// (swap+defer-restore per the camera_observer_test.go pattern) so no live
// sidecar is needed. Covers: frame thinning, candidate selection (human +
// usable embedding only), argmax face→avatar assignment, best-sighting +
// also-seen aggregation, threshold rejection, the sidecar-down guard, the
// per-camera detection cap, and the prompt block rendering.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newCamFaceTestConfig is a minimal config with the sidecar gate ON (the seam
// bypasses the real HTTP call, so the URL only needs to be non-empty) and the
// default 0.40 cosine threshold.
func newCamFaceTestConfig() config {
	return config{
		FaceEnabled:        true,
		FaceAPIURL:         "http://127.0.0.1:1",
		FaceMatchThreshold: 0.40,
		FaceMinScore:       0.5,
	}
}

// writeFrameFile writes content (also the camObserverFaceDetectFn lookup key)
// to a .jpg in dir and returns the path.
func writeFrameFile(t *testing.T, dir, key string) string {
	t.Helper()
	p := filepath.Join(dir, key+".jpg")
	if err := os.WriteFile(p, []byte(key), 0o600); err != nil {
		t.Fatalf("write frame %s: %v", key, err)
	}
	return p
}

func TestCamObserverThinFrames(t *testing.T) {
	base := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	mk := func(n int) []camObserverFrame {
		out := make([]camObserverFrame, n)
		for i := 0; i < n; i++ {
			out[i] = camObserverFrame{T: base.Add(time.Duration(i) * time.Minute)}
		}
		return out
	}
	// len <= max: unchanged.
	if got := camObserverThinFrames(mk(3), 4); len(got) != 3 {
		t.Fatalf("len<=max: want 3 got %d", len(got))
	}
	// max == 1: the latest frame.
	one := camObserverThinFrames(mk(5), 1)
	if len(one) != 1 || !one[0].T.Equal(base.Add(4*time.Minute)) {
		t.Fatalf("max==1: want latest got %+v", one)
	}
	// even spread keeps both endpoints, ascending, exactly max.
	got := camObserverThinFrames(mk(10), 4)
	if len(got) != 4 {
		t.Fatalf("even spread: want 4 got %d", len(got))
	}
	if !got[0].T.Equal(base) || !got[3].T.Equal(base.Add(9*time.Minute)) {
		t.Fatalf("even spread endpoints: got %v..%v", got[0].T, got[3].T)
	}
	for i := 1; i < len(got); i++ {
		if !got[i].T.After(got[i-1].T) {
			t.Fatalf("even spread not strictly ascending at %d: %v", i, got)
		}
	}
}

func TestCamObserverIdentityBlock(t *testing.T) {
	if camObserverIdentityBlock(nil, time.UTC, "UTC") != "" {
		t.Fatalf("empty identities must render empty block")
	}
	base := time.Date(2026, 7, 11, 17, 42, 15, 0, time.UTC)
	block := camObserverIdentityBlock([]camObserverIdentity{
		{Name: "Heba", Kind: "human", CameraName: "Reception", T: base, Similarity: 0.58, AlsoOn: []string{"Corridor"}},
		{Name: "Sara", Kind: "human", CameraName: "Reception", T: base, Similarity: 0.51},
	}, time.UTC, "UTC")
	for _, want := range []string{"CONFIRMED IDENTITIES", "Heba", "Reception", "17:42:15", "0.58", "also seen on Corridor", "BY NAME"} {
		if !strings.Contains(block, want) {
			t.Fatalf("identity block missing %q:\n%s", want, block)
		}
	}
	if strings.Contains(block, "Sara") && strings.Contains(block, "also seen on Reception") {
		t.Fatalf("no-AlsoOn identity should not render 'also seen on'")
	}
}

func TestCamObserverFaceCandidates(t *testing.T) {
	db := openCamTestDB(t)
	if err := migrateCameraDB(db); err != nil {
		t.Fatalf("migrateCameraDB: %v", err)
	}
	cfg := newCamFaceTestConfig()

	// human + embedding → candidate
	heba, err := insertCameraAvatar(db, camAvatar{SiteID: "site_1", Name: "Heba", Type: "human", Enabled: true})
	if err != nil {
		t.Fatalf("insert Heba: %v", err)
	}
	if _, err := insertAvatarMedia(db, camAvatarMedia{AvatarID: heba, CaptureID: "cap_x", Token: "tok_h", Source: "upload", Embedding: camEmbeddingBlob([]float32{1, 0, 0})}); err != nil {
		t.Fatalf("media Heba: %v", err)
	}
	// human, description-only → NOT a candidate
	if _, err := insertCameraAvatar(db, camAvatar{SiteID: "site_1", Name: "Ghost", Type: "human", Enabled: true}); err != nil {
		t.Fatalf("insert Ghost: %v", err)
	}
	// pet with an embedding → NOT a candidate (face rec is human-only)
	pet, err := insertCameraAvatar(db, camAvatar{SiteID: "site_1", Name: "Rex", Type: "pet", Enabled: true})
	if err != nil {
		t.Fatalf("insert Rex: %v", err)
	}
	if _, err := insertAvatarMedia(db, camAvatarMedia{AvatarID: pet, CaptureID: "cap_y", Token: "tok_p", Source: "upload", Embedding: camEmbeddingBlob([]float32{0, 0, 1})}); err != nil {
		t.Fatalf("media Rex: %v", err)
	}

	avatars, err := listCameraAvatars(db, "site_1", false)
	if err != nil {
		t.Fatalf("listCameraAvatars: %v", err)
	}
	cands := camObserverFaceCandidates(db, avatars, cfg)
	if len(cands) != 1 || cands[0].av.Name != "Heba" {
		t.Fatalf("want [Heba], got %d candidates: %+v", len(cands), cands)
	}

	// sidecar disabled → no candidates at all
	off := cfg
	off.FaceEnabled = false
	if c := camObserverFaceCandidates(db, avatars, off); c != nil {
		t.Fatalf("disabled sidecar must yield nil candidates, got %d", len(c))
	}
}

func TestCamObserverRecognizeFaces(t *testing.T) {
	db := openCamTestDB(t)
	if err := migrateCameraDB(db); err != nil {
		t.Fatalf("migrateCameraDB: %v", err)
	}
	cfg := newCamFaceTestConfig()

	heba, _ := insertCameraAvatar(db, camAvatar{SiteID: "site_1", Name: "Heba", Type: "human", Enabled: true})
	if _, err := insertAvatarMedia(db, camAvatarMedia{AvatarID: heba, CaptureID: "c1", Token: "t1", Source: "upload", Embedding: camEmbeddingBlob([]float32{1, 0, 0})}); err != nil {
		t.Fatalf("media Heba: %v", err)
	}
	sara, _ := insertCameraAvatar(db, camAvatar{SiteID: "site_1", Name: "Sara", Type: "human", Enabled: true})
	if _, err := insertAvatarMedia(db, camAvatarMedia{AvatarID: sara, CaptureID: "c2", Token: "t2", Source: "upload", Embedding: camEmbeddingBlob([]float32{0, 1, 0})}); err != nil {
		t.Fatalf("media Sara: %v", err)
	}
	avatars, _ := listCameraAvatars(db, "site_1", false)

	cam1 := camera{ID: "cam_1", SiteID: "site_1", Name: "Reception", Enabled: true}
	cam2 := camera{ID: "cam_2", SiteID: "site_1", Name: "Corridor", Enabled: true}
	cams := []camera{cam1, cam2}
	dir := t.TempDir()
	base := time.Date(2026, 7, 11, 17, 40, 0, 0, time.UTC)

	// face embeddings the scripted detector returns, keyed by frame-file bytes.
	script := map[string][]camFace{
		"isHebaStrong": {{Score: 0.9, Embedding: []float32{1, 0, 0}}},     // cos 1.00 w/ Heba
		"isHebaWeak":   {{Score: 0.9, Embedding: []float32{0.8, 0.6, 0}}}, // cos 0.80 w/ Heba
		"isSara":       {{Score: 0.9, Embedding: []float32{0, 1, 0}}},     // cos 1.00 w/ Sara
		"unknown":      {{Score: 0.9, Embedding: []float32{0, 0, 1}}},     // cos 0 w/ both → below threshold
		"noface":       {},
	}
	orig := camObserverFaceDetectFn
	defer func() { camObserverFaceDetectFn = orig }()
	var detectCalls int
	camObserverFaceDetectFn = func(ctx context.Context, c config, img []byte) ([]camFace, error) {
		detectCalls++
		return script[string(img)], nil
	}

	frame := func(cam camera, key string, tOff time.Duration) camObserverFrame {
		return camObserverFrame{Cam: cam, T: base.Add(tOff), Path: writeFrameFile(t, dir, key)}
	}

	// Case A: Heba strong on cam1, weak on cam2; Sara on cam1; an unknown; a no-face.
	frames := map[string][]camObserverFrame{
		"cam_1": {frame(cam1, "isHebaStrong", time.Minute), frame(cam1, "isSara", 2*time.Minute), frame(cam1, "unknown", 3*time.Minute)},
		"cam_2": {frame(cam2, "isHebaWeak", time.Minute), frame(cam2, "noface", 2*time.Minute)},
	}
	ids := camObserverRecognizeFaces(context.Background(), cfg, db, cams, frames, avatars, time.UTC)
	byName := map[string]camObserverIdentity{}
	for _, id := range ids {
		byName[id.Name] = id
	}
	if len(ids) != 2 {
		t.Fatalf("want 2 identities (Heba, Sara), got %d: %+v", len(ids), ids)
	}
	h, ok := byName["Heba"]
	if !ok {
		t.Fatalf("Heba not recognized: %+v", ids)
	}
	if h.CameraName != "Reception" || h.Similarity < 0.99 {
		t.Fatalf("Heba best sighting should be Reception ~1.00, got %s %.2f", h.CameraName, h.Similarity)
	}
	if len(h.AlsoOn) != 1 || h.AlsoOn[0] != "Corridor" {
		t.Fatalf("Heba AlsoOn should be [Corridor], got %v", h.AlsoOn)
	}
	// Strongest-first ordering.
	if ids[0].Name != "Heba" {
		t.Fatalf("expected Heba first (highest similarity), got %s", ids[0].Name)
	}

	// Case B: only an unknown face → nil (nothing confirmed).
	unk := map[string][]camObserverFrame{"cam_1": {frame(cam1, "unknown", time.Minute)}}
	if got := camObserverRecognizeFaces(context.Background(), cfg, db, cams, unk, avatars, time.UTC); got != nil {
		t.Fatalf("unknown-only must return nil, got %+v", got)
	}

	// Case C: per-camera detection cap honored (max 2 frames/cam).
	capCfg := cfg
	capCfg.CameraObserverFaceMaxFrames = 2
	detectCalls = 0
	many := map[string][]camObserverFrame{"cam_1": {
		frame(cam1, "isHebaStrong", time.Minute), frame(cam1, "noface", 2*time.Minute),
		frame(cam1, "noface", 3*time.Minute), frame(cam1, "noface", 4*time.Minute), frame(cam1, "noface", 5*time.Minute),
	}}
	camObserverRecognizeFaces(context.Background(), capCfg, db, []camera{cam1}, many, avatars, time.UTC)
	if detectCalls != 2 {
		t.Fatalf("per-camera cap: want 2 detect calls, got %d", detectCalls)
	}
}

func TestCamObserverRecognizeFacesSidecarDown(t *testing.T) {
	db := openCamTestDB(t)
	if err := migrateCameraDB(db); err != nil {
		t.Fatalf("migrateCameraDB: %v", err)
	}
	cfg := newCamFaceTestConfig()
	heba, _ := insertCameraAvatar(db, camAvatar{SiteID: "site_1", Name: "Heba", Type: "human", Enabled: true})
	if _, err := insertAvatarMedia(db, camAvatarMedia{AvatarID: heba, CaptureID: "c1", Token: "t1", Source: "upload", Embedding: camEmbeddingBlob([]float32{1, 0, 0})}); err != nil {
		t.Fatalf("media: %v", err)
	}
	avatars, _ := listCameraAvatars(db, "site_1", false)
	cam1 := camera{ID: "cam_1", SiteID: "site_1", Name: "Reception", Enabled: true}
	dir := t.TempDir()
	base := time.Date(2026, 7, 11, 17, 40, 0, 0, time.UTC)

	orig := camObserverFaceDetectFn
	defer func() { camObserverFaceDetectFn = orig }()
	camObserverFaceDetectFn = func(ctx context.Context, c config, img []byte) ([]camFace, error) {
		return nil, context.DeadlineExceeded
	}
	frames := map[string][]camObserverFrame{"cam_1": {
		{Cam: cam1, T: base.Add(time.Minute), Path: writeFrameFile(t, dir, "a")},
		{Cam: cam1, T: base.Add(2 * time.Minute), Path: writeFrameFile(t, dir, "b")},
		{Cam: cam1, T: base.Add(3 * time.Minute), Path: writeFrameFile(t, dir, "c")},
		{Cam: cam1, T: base.Add(4 * time.Minute), Path: writeFrameFile(t, dir, "d")},
	}}
	if got := camObserverRecognizeFaces(context.Background(), cfg, db, []camera{cam1}, frames, avatars, time.UTC); got != nil {
		t.Fatalf("sidecar-down must return nil (fall back to description-only), got %+v", got)
	}
}
