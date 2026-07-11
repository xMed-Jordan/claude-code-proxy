package main

// camera_serviceapi_sync_test.go — the DR sync surface (camera_serviceapi_sync.go):
// manifest shape + updated_at bumps + cross-site isolation, the flag-gated
// secrets export (decrypted round-trip for all three secret kinds when on,
// explicit 403 when off — the ONE route exempted from the no-password-echo
// rule), the camera-update ai_description/ai_location gap fix (set/keep/clear),
// the playbook-media raw image upload, and the end-to-end restore-path
// integration test (provision → dvr w/ creds → simulated discovery → metadata
// by (dvr,channel) → avatar photo through a stubbed face sidecar → playbook/
// watch/stream/arm/callback → manifest+export reflect it all). Harness: the
// camera_serviceapi_test.go helpers — temp sqlite + httptest only.

import (
	"bytes"
	"database/sql"
	"encoding/base64"
	"image"
	"image/png"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// svcTestPNGBase64 returns a tiny valid PNG as std base64 (the upload wire form).
func svcTestPNGBase64(t *testing.T) string {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 4, 4))
	for i := range img.Pix {
		img.Pix[i] = 0xff
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("png.Encode: %v", err)
	}
	return base64.StdEncoding.EncodeToString(buf.Bytes())
}

// svcManifestIDs extracts the "id" of every row in one manifest section.
func svcManifestIDs(t *testing.T, out map[string]any, key string) []string {
	t.Helper()
	rows, ok := out[key].([]any)
	if !ok {
		t.Fatalf("manifest %q = %T, want a list", key, out[key])
	}
	ids := make([]string, 0, len(rows))
	for _, r := range rows {
		row, ok := r.(map[string]any)
		if !ok {
			t.Fatalf("manifest %q row = %T, want a map", key, r)
		}
		id, _ := row["id"].(string)
		ids = append(ids, id)
	}
	return ids
}

// svcSyncSeedFullSite populates one of every restorable entity type for a site
// and returns the created ids keyed by manifest section name.
func svcSyncSeedFullSite(t *testing.T, db *sql.DB, cfg config, siteID string) map[string]string {
	t.Helper()
	ids := map[string]string{}
	dvrID, err := insertCameraDVR(db, cfg, CamDVR{SiteID: siteID, Name: "DVR", Host: "192.0.2.10", Port: 554, Username: "admin", Password: "pw-" + siteID, Enabled: true})
	if err != nil {
		t.Fatalf("insertCameraDVR: %v", err)
	}
	ids["dvrs"] = dvrID
	camID, err := insertCamera(db, camera{SiteID: siteID, DVRID: dvrID, Channel: 1, Name: "Gate", Enabled: true})
	if err != nil {
		t.Fatalf("insertCamera: %v", err)
	}
	ids["cameras"] = camID
	avID, err := insertCameraAvatar(db, camAvatar{SiteID: siteID, Name: "Dr. " + siteID, Enabled: true})
	if err != nil {
		t.Fatalf("insertCameraAvatar: %v", err)
	}
	ids["avatars"] = avID
	photoID, err := insertAvatarMedia(db, camAvatarMedia{AvatarID: avID, CaptureID: "cap_" + siteID, Token: "tokphoto_" + siteID, Source: "upload"})
	if err != nil {
		t.Fatalf("insertAvatarMedia: %v", err)
	}
	ids["avatar_photos"] = photoID
	pbID, err := insertCameraPlaybook(db, camPlaybook{SiteID: siteID, Name: "opening", Enabled: true})
	if err != nil {
		t.Fatalf("insertCameraPlaybook: %v", err)
	}
	ids["playbooks"] = pbID
	pbmID, err := insertCameraPlaybookMedia(db, camPlaybookMedia{PlaybookID: pbID, CaptureToken: "tokpb_" + siteID})
	if err != nil {
		t.Fatalf("insertCameraPlaybookMedia: %v", err)
	}
	ids["playbook_media"] = pbmID
	watchID, err := insertCameraWatch(db, watch{SiteID: siteID, Name: "W", CameraIDs: "[]"})
	if err != nil {
		t.Fatalf("insertCameraWatch: %v", err)
	}
	ids["watches"] = watchID
	streamID, err := insertLogStream(db, logStream{SiteID: siteID, Name: "Journal"})
	if err != nil {
		t.Fatalf("insertLogStream: %v", err)
	}
	ids["log_streams"] = streamID
	toolID, err := insertCameraAPITool(db, cfg, camAPITool{SiteID: siteID, Name: "roster", URLTemplate: "https://api.example/v1/roster"}, "")
	if err != nil {
		t.Fatalf("insertCameraAPITool: %v", err)
	}
	ids["apitools"] = toolID
	if err := upsertCameraMotionArm(db, siteID, camID, "[]", 0, true); err != nil {
		t.Fatalf("upsertCameraMotionArm: %v", err)
	}
	ids["motion_arm"] = camID
	return ids
}

// ─────────────────────────── the manifest ───────────────────────────

func TestSvcSyncManifestShapeAndIsolation(t *testing.T) {
	cfg := newSvcTestConfig(t)
	db := svcTestDB(t, cfg)
	srv := newSvcTestServer(t, cfg)
	siteA, tokA := svcSeedSite(t, db, "Site A")
	siteB, tokB := svcSeedSite(t, db, "Site B")

	idsA := svcSyncSeedFullSite(t, db, cfg, siteA)
	idsB := svcSyncSeedFullSite(t, db, cfg, siteB)
	if err := setCameraSiteCallback(db, cfg, siteA, "https://connect.example/cb", "cb-secret", false); err != nil {
		t.Fatalf("setCameraSiteCallback: %v", err)
	}
	// One journal entry so the activity counter is non-zero.
	svcSeedLogEntry(t, db, siteA, idsA["log_streams"], "[]", "2026-07-11T10:00:00Z", "quiet morning")

	code, out, _ := svcCall(t, srv, http.MethodGet, "/api/cameras/sync/manifest", tokA, nil)
	if code != http.StatusOK {
		t.Fatalf("manifest: %d %v", code, out)
	}
	site, _ := out["site"].(map[string]any)
	if site["id"] != siteA || site["updated_at"] == "" {
		t.Errorf("site section = %v, want id %s + updated_at", site, siteA)
	}
	for _, key := range []string{"dvrs", "cameras", "avatars", "playbooks", "watches", "log_streams", "apitools", "motion_arm"} {
		got := svcManifestIDs(t, out, key)
		if len(got) != 1 || got[0] != idsA[key] {
			t.Errorf("manifest %s = %v, want [%s]", key, got, idsA[key])
		}
		row := out[key].([]any)[0].(map[string]any)
		if row["updated_at"] == "" {
			t.Errorf("manifest %s row missing updated_at: %v", key, row)
		}
	}
	// Child sections carry parent_id + created_at (immutable rows).
	for key, parent := range map[string]string{"avatar_photos": idsA["avatars"], "playbook_media": idsA["playbooks"]} {
		got := svcManifestIDs(t, out, key)
		if len(got) != 1 || got[0] != idsA[key] {
			t.Fatalf("manifest %s = %v, want [%s]", key, got, idsA[key])
		}
		row := out[key].([]any)[0].(map[string]any)
		if row["parent_id"] != parent || row["created_at"] == "" {
			t.Errorf("manifest %s row = %v, want parent_id %s + created_at", key, row, parent)
		}
	}
	cb, _ := out["callback"].(map[string]any)
	if cb["configured"] != true || cb["updated_at"] == "" {
		t.Errorf("callback = %v, want configured + updated_at", cb)
	}
	if seq, _ := out["activity_max_seq"].(float64); seq < 1 {
		t.Errorf("activity_max_seq = %v, want >= 1 after seeding an entry", out["activity_max_seq"])
	}
	if out["at"] == "" {
		t.Error("manifest missing at")
	}

	// Site B sees ONLY its own entities and no callback.
	code, outB, _ := svcCall(t, srv, http.MethodGet, "/api/cameras/sync/manifest", tokB, nil)
	if code != http.StatusOK {
		t.Fatalf("manifest B: %d %v", code, outB)
	}
	for key, want := range idsB {
		got := svcManifestIDs(t, outB, key)
		if len(got) != 1 || got[0] != want {
			t.Errorf("B manifest %s = %v, want [%s]", key, got, want)
		}
	}
	if cbB, _ := outB["callback"].(map[string]any); cbB["configured"] != false {
		t.Errorf("B callback = %v, want unconfigured", outB["callback"])
	}

	code, _, _ = svcCall(t, srv, http.MethodPost, "/api/cameras/sync/manifest", tokA, map[string]any{})
	if code != http.StatusMethodNotAllowed {
		t.Errorf("POST manifest = %d, want 405", code)
	}
}

func TestSvcSyncManifestBumps(t *testing.T) {
	cfg := newSvcTestConfig(t)
	db := svcTestDB(t, cfg)
	srv := newSvcTestServer(t, cfg)
	siteA, tokA := svcSeedSite(t, db, "Site A")
	ids := svcSyncSeedFullSite(t, db, cfg, siteA)

	const old = "2000-01-01T00:00:00Z"
	for _, stmt := range []struct{ query, id string }{
		{`UPDATE cameras SET updated_at = ? WHERE id = ?`, ids["cameras"]},
		{`UPDATE camera_motion_arm SET updated_at = ? WHERE camera_id = ?`, ids["motion_arm"]},
	} {
		if _, err := db.Exec(stmt.query, old, stmt.id); err != nil {
			t.Fatalf("age row: %v", err)
		}
	}
	manifestUpdatedAt := func(key, id string) string {
		t.Helper()
		code, out, _ := svcCall(t, srv, http.MethodGet, "/api/cameras/sync/manifest", tokA, nil)
		if code != http.StatusOK {
			t.Fatalf("manifest: %d %v", code, out)
		}
		for _, r := range out[key].([]any) {
			row := r.(map[string]any)
			if row["id"] == id {
				ts, _ := row["updated_at"].(string)
				return ts
			}
		}
		t.Fatalf("manifest %s has no row %s", key, id)
		return ""
	}
	if ts := manifestUpdatedAt("cameras", ids["cameras"]); ts != old {
		t.Fatalf("aged camera updated_at = %q, want %q", ts, old)
	}
	if ts := manifestUpdatedAt("motion_arm", ids["motion_arm"]); ts != old {
		t.Fatalf("aged arm updated_at = %q, want %q", ts, old)
	}

	// A content write through the service API bumps the manifest timestamp.
	code, out, _ := svcCall(t, srv, http.MethodPost, "/api/cameras/update", tokA, map[string]any{"id": ids["cameras"], "name": "Renamed"})
	if code != http.StatusOK {
		t.Fatalf("camera update: %d %v", code, out)
	}
	if ts := manifestUpdatedAt("cameras", ids["cameras"]); ts == old {
		t.Error("camera update did not bump the manifest updated_at")
	}
	code, out, _ = svcCall(t, srv, http.MethodPost, "/api/cameras/motion/arm", tokA, map[string]any{"camera_ids": []string{ids["motion_arm"]}, "cooldown_seconds": 30})
	if code != http.StatusOK {
		t.Fatalf("re-arm: %d %v", code, out)
	}
	if ts := manifestUpdatedAt("motion_arm", ids["motion_arm"]); ts == old {
		t.Error("re-arm did not bump the manifest updated_at")
	}
}

// ─────────────────────────── the secrets export ───────────────────────────

func TestSvcSyncExportSecretsOn(t *testing.T) {
	cfg := newSvcTestConfig(t)
	cfg.CameraSyncExportSecrets = true
	db := svcTestDB(t, cfg)
	srv := newSvcTestServer(t, cfg)
	siteA, tokA := svcSeedSite(t, db, "Site A")
	siteB, tokB := svcSeedSite(t, db, "Site B")

	dvrA, err := insertCameraDVR(db, cfg, CamDVR{SiteID: siteA, Name: "DVR A", Host: "192.0.2.10", Username: "admin", Password: "dvr-secret-1", Enabled: true})
	if err != nil {
		t.Fatalf("insertCameraDVR A: %v", err)
	}
	if _, err := insertCameraDVR(db, cfg, CamDVR{SiteID: siteB, Name: "DVR B", Host: "192.0.2.20", Username: "root", Password: "pw-b-hidden", Enabled: true}); err != nil {
		t.Fatalf("insertCameraDVR B: %v", err)
	}
	toolA, err := insertCameraAPITool(db, cfg, camAPITool{SiteID: siteA, Name: "roster", URLTemplate: "https://api.example/v1/roster"}, "tool-secret-2")
	if err != nil {
		t.Fatalf("insertCameraAPITool: %v", err)
	}
	// A secretless tool never appears in the export (nothing to carry).
	if _, err := insertCameraAPITool(db, cfg, camAPITool{SiteID: siteA, Name: "weather", URLTemplate: "https://api.example/v1/weather"}, ""); err != nil {
		t.Fatalf("insertCameraAPITool plain: %v", err)
	}
	watchA, err := insertCameraWatch(db, watch{SiteID: siteA, Name: "Night", CameraIDs: "[]",
		ActionJSON: encodeWatchAction(cfg, &camActionRequest{Type: "webhook", URL: "https://hook.example/x", Secret: "watch-secret-3"})})
	if err != nil {
		t.Fatalf("insertCameraWatch: %v", err)
	}
	// A secretless action never appears either.
	if _, err := insertCameraWatch(db, watch{SiteID: siteA, Name: "Day", CameraIDs: "[]"}); err != nil {
		t.Fatalf("insertCameraWatch plain: %v", err)
	}

	code, out, raw := svcCall(t, srv, http.MethodGet, "/api/cameras/sync/export", tokA, nil)
	if code != http.StatusOK {
		t.Fatalf("export: %d %v", code, out)
	}
	dvrs, _ := out["dvrs"].([]any)
	if len(dvrs) != 1 {
		t.Fatalf("export dvrs = %v, want 1 row", out["dvrs"])
	}
	if d := dvrs[0].(map[string]any); d["id"] != dvrA || d["username"] != "admin" || d["password"] != "dvr-secret-1" {
		t.Errorf("export dvr = %v, want decrypted credentials for %s", d, dvrA)
	}
	tools, _ := out["apitools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("export apitools = %v, want 1 row (secretless tool excluded)", out["apitools"])
	}
	if tl := tools[0].(map[string]any); tl["id"] != toolA || tl["auth_secret"] != "tool-secret-2" {
		t.Errorf("export apitool = %v, want decrypted secret for %s", tl, toolA)
	}
	actions, _ := out["watch_actions"].([]any)
	if len(actions) != 1 {
		t.Fatalf("export watch_actions = %v, want 1 row (secretless action excluded)", out["watch_actions"])
	}
	if a := actions[0].(map[string]any); a["watch_id"] != watchA || a["token"] != "watch-secret-3" {
		t.Errorf("export watch action = %v, want decrypted token for %s", a, watchA)
	}
	if out["at"] == "" {
		t.Error("export missing at")
	}
	// This route is the KNOWING no-password-echo exemption — but strictly
	// site-scoped: another site's secrets never leak through it.
	if strings.Contains(string(raw), "pw-b-hidden") {
		t.Error("export leaked another site's dvr password")
	}
	code, out, _ = svcCall(t, srv, http.MethodGet, "/api/cameras/sync/export", tokB, nil)
	if code != http.StatusOK {
		t.Fatalf("export B: %d %v", code, out)
	}
	if dvrsB, _ := out["dvrs"].([]any); len(dvrsB) != 1 || dvrsB[0].(map[string]any)["password"] != "pw-b-hidden" {
		t.Errorf("B export dvrs = %v, want its own decrypted credential", out["dvrs"])
	}

	code, _, _ = svcCall(t, srv, http.MethodPost, "/api/cameras/sync/export", tokA, map[string]any{})
	if code != http.StatusMethodNotAllowed {
		t.Errorf("POST export = %d, want 405", code)
	}
}

func TestSvcSyncExportSecretsOff(t *testing.T) {
	// newSvcTestConfig builds a literal config, so the knob is OFF here —
	// mirroring PROXY_CAMERA_SYNC_EXPORT_SECRETS=false at runtime.
	cfg := newSvcTestConfig(t)
	db := svcTestDB(t, cfg)
	srv := newSvcTestServer(t, cfg)
	siteA, tokA := svcSeedSite(t, db, "Site A")
	if _, err := insertCameraDVR(db, cfg, CamDVR{SiteID: siteA, Host: "192.0.2.10", Password: "pw-hidden", Enabled: true}); err != nil {
		t.Fatalf("insertCameraDVR: %v", err)
	}

	code, out, raw := svcCall(t, srv, http.MethodGet, "/api/cameras/sync/export", tokA, nil)
	if code != http.StatusForbidden {
		t.Fatalf("export with the flag off = %d %v, want an EXPLICIT 403 (not the uniform 404)", code, out)
	}
	if msg, _ := out["error"].(string); !strings.Contains(msg, "PROXY_CAMERA_SYNC_EXPORT_SECRETS") {
		t.Errorf("403 error = %q, want it to name the knob", msg)
	}
	if strings.Contains(string(raw), "pw-hidden") {
		t.Error("disabled export leaked a password")
	}
}

// ─────────────────────────── camera ai_* gap fix ───────────────────────────

func TestSvcCameraUpdateAIFields(t *testing.T) {
	cfg := newSvcTestConfig(t)
	db := svcTestDB(t, cfg)
	srv := newSvcTestServer(t, cfg)
	siteA, tokA := svcSeedSite(t, db, "Site A")
	dvrID, err := insertCameraDVR(db, cfg, CamDVR{SiteID: siteA, Host: "192.0.2.10", Enabled: true})
	if err != nil {
		t.Fatalf("insertCameraDVR: %v", err)
	}
	camID, err := insertCamera(db, camera{SiteID: siteA, DVRID: dvrID, Channel: 1, Name: "Gate", Enabled: true})
	if err != nil {
		t.Fatalf("insertCamera: %v", err)
	}

	// Set both AI-authored fields (the restore path re-applies mirrored values).
	code, out, _ := svcCall(t, srv, http.MethodPost, "/api/cameras/update", tokA, map[string]any{
		"id": camID, "ai_description": "faces the reception desk", "ai_location": "lobby north wall",
	})
	if code != http.StatusOK {
		t.Fatalf("set: %d %v", code, out)
	}
	if c := out["camera"].(map[string]any); c["ai_description"] != "faces the reception desk" || c["ai_location"] != "lobby north wall" {
		t.Errorf("set response = %v, want both ai fields", c)
	}
	got, _ := getCamera(db, camID)
	if got.AIDescription != "faces the reception desk" || got.AILocation != "lobby north wall" {
		t.Errorf("stored ai fields = (%q, %q)", got.AIDescription, got.AILocation)
	}

	// Omitted pointers leave the values alone.
	code, _, _ = svcCall(t, srv, http.MethodPost, "/api/cameras/update", tokA, map[string]any{"id": camID, "name": "Renamed"})
	if code != http.StatusOK {
		t.Fatalf("rename: %d", code)
	}
	if got, _ = getCamera(db, camID); got.AIDescription == "" || got.AILocation == "" {
		t.Error("omitted ai fields must not be cleared by an unrelated update")
	}

	// Explicit empty strings clear them.
	code, _, _ = svcCall(t, srv, http.MethodPost, "/api/cameras/update", tokA, map[string]any{
		"id": camID, "ai_description": "", "ai_location": "",
	})
	if code != http.StatusOK {
		t.Fatalf("clear: %d", code)
	}
	if got, _ = getCamera(db, camID); got.AIDescription != "" || got.AILocation != "" {
		t.Errorf("cleared ai fields = (%q, %q), want empty", got.AIDescription, got.AILocation)
	}
}

// ─────────────────────────── playbook media raw upload ───────────────────────────

func TestSvcPlaybookMediaImageUpload(t *testing.T) {
	cfg := newSvcTestConfig(t)
	db := svcTestDB(t, cfg)
	srv := newSvcTestServer(t, cfg)
	siteA, tokA := svcSeedSite(t, db, "Site A")
	_, tokB := svcSeedSite(t, db, "Site B")
	pbID, err := insertCameraPlaybook(db, camPlaybook{SiteID: siteA, Name: "opening", Enabled: true})
	if err != nil {
		t.Fatalf("insertCameraPlaybook: %v", err)
	}
	pngB64 := svcTestPNGBase64(t)

	code, out, _ := svcCall(t, srv, http.MethodPost, "/api/cameras/playbooks/media", tokA, map[string]any{
		"playbook_id": pbID, "image_base64": pngB64, "note": "correct standby position",
	})
	if code != http.StatusOK || out["ok"] != true {
		t.Fatalf("upload: %d %v", code, out)
	}
	media := out["media"].(map[string]any)
	if media["content_type"] != "image/png" || media["note"] != "correct standby position" {
		t.Errorf("upload media = %v, want image/png + the note", media)
	}
	token, _ := media["capture_token"].(string)
	cpt, cerr := getCameraCaptureByToken(db, token)
	if cerr != nil {
		t.Fatalf("uploaded capture missing: %v", cerr)
	}
	if cpt.Kind != "playbook_ref" || cpt.ExpiresAt != "" || cpt.SiteID != siteA {
		t.Errorf("capture = kind %q expires %q site %q, want pinned playbook_ref in site A", cpt.Kind, cpt.ExpiresAt, cpt.SiteID)
	}

	// Validation 400s: both forms, neither form, junk base64.
	for name, body := range map[string]map[string]any{
		"both token and image": {"playbook_id": pbID, "token": "tok_x", "image_base64": pngB64},
		"neither":              {"playbook_id": pbID},
		"bad base64":           {"playbook_id": pbID, "image_base64": "!!!not-base64!!!"},
	} {
		code, out, _ = svcCall(t, srv, http.MethodPost, "/api/cameras/playbooks/media", tokA, body)
		if code != http.StatusBadRequest {
			t.Errorf("%s: status = %d %v, want 400", name, code, out)
		}
	}

	// Cross-site playbook stays the uniform 404 on the upload path too.
	code, _, _ = svcCall(t, srv, http.MethodPost, "/api/cameras/playbooks/media", tokB, map[string]any{
		"playbook_id": pbID, "image_base64": pngB64,
	})
	if code != http.StatusNotFound {
		t.Errorf("cross-site upload = %d, want 404", code)
	}
}

// ─────────────────────────── the restore path ───────────────────────────

// TestSvcSyncRestorePath drives the full DR restore sequence a fresh proxy
// sees from Connect's RestoreCameraSiteService — provision, a DVR carrying
// exported credentials, discovery (simulated: handleSvcDVRDiscover talks to
// real hardware, so the test inserts the channel rows discovery would),
// camera metadata re-applied by the (dvr, channel) natural key, an avatar
// reference photo re-embedded through a stubbed face sidecar (centroid
// non-NULL), a playbook with raw-uploaded reference media, then watch/stream/
// arm/callback — and asserts the manifest and secrets export reflect all of it.
func TestSvcSyncRestorePath(t *testing.T) {
	cfg := newSvcTestConfig(t)
	cfg.CameraSyncExportSecrets = true
	emb := make([]float32, 512)
	for i := range emb {
		emb[i] = 0.5
	}
	faceStub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/embed" {
			http.NotFound(w, r)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"faces": []map[string]any{{
			"bbox": []float64{0, 0, 4, 4}, "det_score": 0.9, "embedding": emb,
		}}})
	}))
	defer faceStub.Close()
	cfg.FaceEnabled, cfg.FaceAPIURL, cfg.FaceMinScore = true, faceStub.URL, 0.5

	db := svcTestDB(t, cfg)
	srv := newSvcTestServer(t, cfg)
	_, mgmtTok, err := mintCameraServiceToken(db, "management", "", "connect registry")
	if err != nil {
		t.Fatalf("mint management token: %v", err)
	}

	// 1. Fresh site (Connect re-provisions, then re-points the plugin).
	code, out, _ := svcCall(t, srv, http.MethodPost, "/api/cameras/provision", mgmtTok, map[string]any{"name": "Restored Clinic"})
	if code != http.StatusOK {
		t.Fatalf("provision: %d %v", code, out)
	}
	siteID, _ := out["site_id"].(string)
	tok, _ := out["token"].(string)
	if siteID == "" || tok == "" {
		t.Fatalf("provision returned %v", out)
	}

	// 2. DVR with the credentials carried from the dead proxy's export.
	code, out, _ = svcCall(t, srv, http.MethodPost, "/api/cameras/dvrs", tok, map[string]any{
		"name": "Main DVR", "brand": "hikvision", "host": "192.0.2.99", "port": 1554, "http_port": 85,
		"username": "admin", "password": "dvr-pass",
	})
	if code != http.StatusOK {
		t.Fatalf("dvr create: %d %v", code, out)
	}
	dvrID, _ := out["id"].(string)

	// 3. Discovery's outcome, simulated: the two channel rows discoverDvr
	// would insert (camera ids are freshly minted — they never survive a
	// rebuild, which is exactly why the natural key is (dvr, channel)).
	if _, err := insertCamera(db, camera{SiteID: siteID, DVRID: dvrID, Channel: 1, Name: "Channel 1", Enabled: true}); err != nil {
		t.Fatalf("insert channel 1: %v", err)
	}
	cam2, err := insertCamera(db, camera{SiteID: siteID, DVRID: dvrID, Channel: 2, Name: "Channel 2", Enabled: true})
	if err != nil {
		t.Fatalf("insert channel 2: %v", err)
	}

	// 4. Restore camera metadata by (dvr, channel) — incl. the ai_* gap fix.
	code, out, _ = svcCall(t, srv, http.MethodGet, "/api/cameras/list?dvr_id="+dvrID, tok, nil)
	if code != http.StatusOK {
		t.Fatalf("camera list: %d %v", code, out)
	}
	mapped := ""
	for _, c := range out["cameras"].([]any) {
		row := c.(map[string]any)
		if row["channel"] == float64(2) {
			mapped, _ = row["id"].(string)
		}
	}
	if mapped != cam2 {
		t.Fatalf("(dvr, channel 2) mapped to %q, want %s", mapped, cam2)
	}
	code, out, _ = svcCall(t, srv, http.MethodPost, "/api/cameras/update", tok, map[string]any{
		"id": mapped, "name": "Reception", "area": "Front",
		"ai_description": "faces the reception desk", "ai_location": "lobby north",
	})
	if code != http.StatusOK {
		t.Fatalf("camera metadata: %d %v", code, out)
	}
	if c := out["camera"].(map[string]any); c["ai_description"] != "faces the reception desk" {
		t.Errorf("restored camera = %v, want the ai_description applied", c)
	}

	// 5. Avatar + reference photo: the embedding is recomputed proxy-side via
	// the sidecar, so the mirrored binary alone restores face recognition.
	code, out, _ = svcCall(t, srv, http.MethodPost, "/api/cameras/avatars", tok, map[string]any{"name": "Dr. Restored"})
	if code != http.StatusOK {
		t.Fatalf("avatar create: %d %v", code, out)
	}
	avID, _ := out["id"].(string)
	code, out, _ = svcCall(t, srv, http.MethodPost, "/api/cameras/avatars/photos", tok, map[string]any{
		"avatar_id": avID, "image_base64": svcTestPNGBase64(t),
	})
	if code != http.StatusOK {
		t.Fatalf("avatar photo: %d %v", code, out)
	}
	var centroid []byte
	if err := db.QueryRow(`SELECT embedding FROM camera_avatars WHERE id = ?`, avID).Scan(&centroid); err != nil {
		t.Fatalf("read centroid: %v", err)
	}
	if len(centroid) == 0 {
		t.Error("avatar centroid is NULL — the re-uploaded photo was not re-embedded")
	}

	// 6. Playbook + reference media through the raw upload (S1b).
	code, out, _ = svcCall(t, srv, http.MethodPost, "/api/cameras/playbooks", tok, map[string]any{
		"name": "opening-procedure", "instructions": "unlock at 08:00",
	})
	if code != http.StatusOK {
		t.Fatalf("playbook create: %d %v", code, out)
	}
	pbID, _ := out["id"].(string)
	code, out, _ = svcCall(t, srv, http.MethodPost, "/api/cameras/playbooks/media", tok, map[string]any{
		"playbook_id": pbID, "image_base64": svcTestPNGBase64(t),
	})
	if code != http.StatusOK {
		t.Fatalf("playbook media upload: %d %v", code, out)
	}

	// 7. Watch (action secret from the export), activity stream, arm, callback.
	code, out, _ = svcCall(t, srv, http.MethodPost, "/api/cameras/watches", tok, map[string]any{
		"name": "After hours", "instruction": "watch the door", "mode": "escalate",
		"camera_ids": []string{mapped},
		"action":     map[string]any{"type": "webhook", "url": "https://hook.example/x", "secret": "watch-secret"},
	})
	if code != http.StatusOK {
		t.Fatalf("watch create: %d %v", code, out)
	}
	code, out, _ = svcCall(t, srv, http.MethodPost, "/api/cameras/activity/streams", tok, map[string]any{"name": "Journal", "enabled": true})
	if code != http.StatusOK {
		t.Fatalf("stream create: %d %v", code, out)
	}
	code, out, _ = svcCall(t, srv, http.MethodPost, "/api/cameras/motion/arm", tok, map[string]any{"camera_ids": []string{mapped}})
	if code != http.StatusOK {
		t.Fatalf("arm: %d %v", code, out)
	}
	code, out, _ = svcCall(t, srv, http.MethodPost, "/api/cameras/callback", tok, map[string]any{
		"url": "https://connect.example/cb", "secret": "cb-secret",
	})
	if code != http.StatusOK {
		t.Fatalf("callback: %d %v", code, out)
	}

	// 8. One API tool with a bearer secret.
	code, out, _ = svcCall(t, srv, http.MethodPost, "/api/cameras/apitools", tok, map[string]any{
		"name": "roster", "url_template": "https://api.example/v1/roster", "auth_secret": "tool-secret",
	})
	if code != http.StatusOK {
		t.Fatalf("apitool create: %d %v", code, out)
	}

	// 9. The manifest reflects every restored entity type.
	code, out, _ = svcCall(t, srv, http.MethodGet, "/api/cameras/sync/manifest", tok, nil)
	if code != http.StatusOK {
		t.Fatalf("manifest: %d %v", code, out)
	}
	for key, want := range map[string]int{
		"dvrs": 1, "cameras": 2, "avatars": 1, "avatar_photos": 1,
		"playbooks": 1, "playbook_media": 1, "watches": 1, "log_streams": 1,
		"apitools": 1, "motion_arm": 1,
	} {
		if got := svcManifestIDs(t, out, key); len(got) != want {
			t.Errorf("manifest %s = %d rows, want %d", key, len(got), want)
		}
	}
	if cb, _ := out["callback"].(map[string]any); cb["configured"] != true {
		t.Errorf("callback = %v, want configured after the restore", out["callback"])
	}

	// 10. And the export round-trips the secrets the NEXT disaster would need.
	code, out, _ = svcCall(t, srv, http.MethodGet, "/api/cameras/sync/export", tok, nil)
	if code != http.StatusOK {
		t.Fatalf("export: %d %v", code, out)
	}
	if d := out["dvrs"].([]any)[0].(map[string]any); d["password"] != "dvr-pass" {
		t.Errorf("export dvr = %v, want the restored credential", d)
	}
	if tl := out["apitools"].([]any)[0].(map[string]any); tl["auth_secret"] != "tool-secret" {
		t.Errorf("export apitool = %v, want the restored secret", tl)
	}
	if a := out["watch_actions"].([]any)[0].(map[string]any); a["token"] != "watch-secret" {
		t.Errorf("export watch action = %v, want the restored token", a)
	}
}
