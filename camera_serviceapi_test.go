package main

// camera_serviceapi_test.go — focused tests for the Connect-facing service API
// (camera_serviceapi.go): token mint/lookup/revoke, the identical-401 auth
// middleware (bad token / wrong scope / revoked), cross-site ownership denial
// on id-bearing routes, the provision → dvr → investigation happy path, the
// per-token rate limit, and the pure settle-payload builder. Temp sqlite +
// httptest only — no DVRs, no model calls, no external network.

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newSvcTestConfig builds a config whose DB and media root live in t.TempDir().
// The investigate worker flag is ON so enqueue paths never drain inline (no
// model calls from tests); the worker itself is never started here.
func newSvcTestConfig(t *testing.T) config {
	t.Helper()
	dir := t.TempDir()
	cfg := config{
		DBPath:                         filepath.Join(dir, ".proxy.db"),
		CameraMediaDir:                 filepath.Join(dir, "media"),
		CameraInvestigateWorkerEnabled: true,
		CameraServiceRate:              1000,
	}
	if err := os.MkdirAll(cfg.CameraMediaDir, 0o700); err != nil {
		t.Fatalf("mkdir media root: %v", err)
	}
	db, err := openProxyDB(cfg)
	if err != nil {
		t.Fatalf("openProxyDB: %v", err)
	}
	defer db.Close()
	if err := migrateCameraDB(db); err != nil {
		t.Fatalf("migrateCameraDB: %v", err)
	}
	return cfg
}

func newSvcTestServer(t *testing.T, cfg config) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	registerCameraServiceRoutes(cfg, mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// svcCall issues one service-API request and decodes the JSON reply (nil out
// map for non-JSON bodies like media bytes).
func svcCall(t *testing.T, srv *httptest.Server, method, path, token string, body any) (int, map[string]any, []byte) {
	t.Helper()
	var rd io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		rd = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, srv.URL+path, rd)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return resp.StatusCode, out, raw
}

func svcTestDB(t *testing.T, cfg config) *sql.DB {
	t.Helper()
	db, err := openProxyDB(cfg)
	if err != nil {
		t.Fatalf("openProxyDB: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// ─────────────────────────── token store ───────────────────────────

func TestCamServiceTokenMintLookupRevoke(t *testing.T) {
	cfg := newSvcTestConfig(t)
	db := svcTestDB(t, cfg)

	tok, plaintext, err := mintCameraServiceToken(db, "management", "", "connect registry")
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if !strings.HasPrefix(plaintext, "cpsk_") || len(plaintext) != len("cpsk_")+48 {
		t.Errorf("plaintext = %q, want cpsk_ + 48 hex chars", plaintext)
	}
	if tok.TokenPreview == plaintext || tok.TokenPreview == "" {
		t.Errorf("preview %q must be masked, non-empty", tok.TokenPreview)
	}
	if strings.Contains(tok.TokenHash, plaintext) || len(tok.TokenHash) != 64 {
		t.Errorf("token_hash = %q, want 64 hex chars of sha256", tok.TokenHash)
	}
	if !strings.HasPrefix(tok.ID, "svct_") {
		t.Errorf("token id = %q, want svct_ prefix", tok.ID)
	}

	got, lerr := lookupCameraServiceToken(db, plaintext)
	if lerr != nil || got.ID != tok.ID {
		t.Fatalf("lookup = (%+v, %v), want id %s", got, lerr, tok.ID)
	}
	if _, werr := lookupCameraServiceToken(db, "cpsk_wrong"); werr == nil {
		t.Error("lookup of unknown plaintext must fail")
	}

	// site scope requires a site id; management ignores it.
	if _, _, serr := mintCameraServiceToken(db, "site", "", "x"); serr == nil {
		t.Error("site-scope mint without site_id must fail")
	}
	if _, _, serr := mintCameraServiceToken(db, "bogus", "", "x"); serr == nil {
		t.Error("unknown scope must fail")
	}

	if rerr := revokeCameraServiceToken(db, tok.ID); rerr != nil {
		t.Fatalf("revoke: %v", rerr)
	}
	if _, lerr := lookupCameraServiceToken(db, plaintext); lerr == nil {
		t.Error("revoked token must not resolve")
	}
	tokens, lerr2 := listCameraServiceTokens(db)
	if lerr2 != nil || len(tokens) != 1 || tokens[0].Enabled {
		t.Errorf("list = (%d rows, %v), want 1 disabled row", len(tokens), lerr2)
	}
}

// ─────────────────────────── auth middleware ───────────────────────────

func TestCamServiceAuthRejections(t *testing.T) {
	cfg := newSvcTestConfig(t)
	db := svcTestDB(t, cfg)
	srv := newSvcTestServer(t, cfg)

	siteID, err := insertCameraSite(db, camSite{Name: "Site A"})
	if err != nil {
		t.Fatalf("insertCameraSite: %v", err)
	}
	_, siteTok, _ := mintCameraServiceToken(db, "site", siteID, "a")
	_, mgmtTok, _ := mintCameraServiceToken(db, "management", "", "m")

	wantUnauthorized := func(name string, code int, out map[string]any) {
		t.Helper()
		if code != http.StatusUnauthorized {
			t.Errorf("%s: status = %d, want 401", name, code)
		}
		if len(out) != 1 || out["error"] != "unauthorized" {
			t.Errorf(`%s: body = %v, want exactly {"error":"unauthorized"}`, name, out)
		}
	}

	code, out, _ := svcCall(t, srv, http.MethodGet, "/api/cameras/site", "", nil)
	wantUnauthorized("no header", code, out)
	code, out, _ = svcCall(t, srv, http.MethodGet, "/api/cameras/site", "cpsk_bogus", nil)
	wantUnauthorized("bad token", code, out)
	code, out, _ = svcCall(t, srv, http.MethodGet, "/api/cameras/site", mgmtTok, nil)
	wantUnauthorized("management token on site route", code, out)
	code, out, _ = svcCall(t, srv, http.MethodGet, "/api/cameras/usage", siteTok, nil)
	wantUnauthorized("site token on management route", code, out)

	// Correct scope works …
	code, out, _ = svcCall(t, srv, http.MethodGet, "/api/cameras/site", siteTok, nil)
	if code != http.StatusOK {
		t.Fatalf("site token on own site route: %d %v", code, out)
	}
	// … until revoked.
	tokens, _ := listCameraServiceTokens(db)
	for _, tk := range tokens {
		if tk.Scope == "site" {
			_ = revokeCameraServiceToken(db, tk.ID)
		}
	}
	code, out, _ = svcCall(t, srv, http.MethodGet, "/api/cameras/site", siteTok, nil)
	wantUnauthorized("revoked token", code, out)
}

func TestCamServiceRateLimit(t *testing.T) {
	cfg := newSvcTestConfig(t)
	cfg.CameraServiceRate = 3
	db := svcTestDB(t, cfg)
	srv := newSvcTestServer(t, cfg)

	siteID, _ := insertCameraSite(db, camSite{Name: "Rate Site"})
	_, tok, _ := mintCameraServiceToken(db, "site", siteID, "r")

	for i := 0; i < 3; i++ {
		code, out, _ := svcCall(t, srv, http.MethodGet, "/api/cameras/site", tok, nil)
		if code != http.StatusOK {
			t.Fatalf("request %d: status = %d %v, want 200", i+1, code, out)
		}
	}
	code, out, _ := svcCall(t, srv, http.MethodGet, "/api/cameras/site", tok, nil)
	if code != http.StatusTooManyRequests || out["error"] != "rate limited" {
		t.Fatalf("4th request: status = %d %v, want 429 rate limited", code, out)
	}
}

// ─────────────────────────── cross-site denial ───────────────────────────

func TestCamServiceCrossSiteDenial(t *testing.T) {
	cfg := newSvcTestConfig(t)
	db := svcTestDB(t, cfg)
	srv := newSvcTestServer(t, cfg)

	siteA, _ := insertCameraSite(db, camSite{Name: "Site A"})
	siteB, _ := insertCameraSite(db, camSite{Name: "Site B"})
	_, tokA, _ := mintCameraServiceToken(db, "site", siteA, "a")
	_, tokB, _ := mintCameraServiceToken(db, "site", siteB, "b")

	// Site B resources: DVR, investigation, capture (real file), avatar,
	// playbook. Avatar/playbook rows are inserted with direct SQL so this test
	// does not depend on sibling work packages' store functions.
	dvrB, err := insertCameraDVR(db, cfg, CamDVR{SiteID: siteB, Name: "DVR B", Brand: "hikvision", Host: "192.0.2.9", Enabled: true})
	if err != nil {
		t.Fatalf("insertCameraDVR: %v", err)
	}
	invB, err := insertCameraInvestigation(db, camInvestigation{SiteID: siteB, Title: "q", Question: "q", Status: "queued"})
	if err != nil {
		t.Fatalf("insertCameraInvestigation: %v", err)
	}
	mediaPath := filepath.Join(cameraMediaRoot(cfg), "evidence.jpg")
	if err := os.WriteFile(mediaPath, []byte("jpegbytes"), 0o600); err != nil {
		t.Fatalf("write media file: %v", err)
	}
	if _, err := insertCameraCapture(db, camCapture{
		SiteID: siteB, Kind: "snapshot", Token: "tokb1234", Path: mediaPath, ContentType: "image/jpeg",
	}); err != nil {
		t.Fatalf("insertCameraCapture: %v", err)
	}
	now := nowRFC3339()
	if _, err := db.Exec(`INSERT INTO camera_avatars (id, site_id, name, type, is_group, external_ref, description, dvr_ids, enabled, embedding, created_at, updated_at)
		VALUES ('av_b', ?, 'Bob', 'human', 0, '', '', '', 1, NULL, ?, ?)`, siteB, now, now); err != nil {
		t.Fatalf("insert avatar row: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO camera_playbooks (id, site_id, dvr_id, name, when_to_use, instructions, enabled, created_at, updated_at)
		VALUES ('pb_b', ?, '', 'closing-time', '', '', 1, ?, ?)`, siteB, now, now); err != nil {
		t.Fatalf("insert playbook row: %v", err)
	}

	denied := []struct {
		name, method, path string
		body               any
	}{
		{"dvr update", http.MethodPost, "/api/cameras/dvrs/update", map[string]any{"id": dvrB, "name": "hacked"}},
		{"dvr delete", http.MethodPost, "/api/cameras/dvrs/delete", map[string]any{"id": dvrB}},
		{"dvr toggle", http.MethodPost, "/api/cameras/dvrs/toggle", map[string]any{"id": dvrB, "enabled": false}},
		{"cameras list by foreign dvr", http.MethodGet, "/api/cameras/list?dvr_id=" + dvrB, nil},
		{"investigation get", http.MethodGet, "/api/cameras/investigations/get?id=" + invB, nil},
		{"investigation reply", http.MethodPost, "/api/cameras/investigations/reply", map[string]any{"id": invB, "message": "hi"}},
		{"media", http.MethodGet, "/api/cameras/media/tokb1234", nil},
		{"avatar update", http.MethodPost, "/api/cameras/avatars/update", map[string]any{"id": "av_b", "name": "Eve"}},
		{"avatar delete", http.MethodPost, "/api/cameras/avatars/delete", map[string]any{"id": "av_b"}},
		{"playbook update", http.MethodPost, "/api/cameras/playbooks/update", map[string]any{"id": "pb_b", "name": "x"}},
		{"playbook delete", http.MethodPost, "/api/cameras/playbooks/delete", map[string]any{"id": "pb_b"}},
	}
	for _, d := range denied {
		code, out, _ := svcCall(t, srv, d.method, d.path, tokA, d.body)
		if code != http.StatusNotFound {
			t.Errorf("%s with foreign token: status = %d %v, want 404", d.name, code, out)
			continue
		}
		if out["error"] != "not found" {
			t.Errorf(`%s: body = %v, want {"error":"not found"}`, d.name, out)
		}
	}

	// Positive controls with the owning token (only routes whose store layer is
	// already implemented — DVR / investigation / media).
	code, out, _ := svcCall(t, srv, http.MethodPost, "/api/cameras/dvrs/update", tokB, map[string]any{"id": dvrB, "name": "DVR B2"})
	if code != http.StatusOK || out["ok"] != true {
		t.Errorf("own dvr update: %d %v", code, out)
	}
	code, out, _ = svcCall(t, srv, http.MethodGet, "/api/cameras/investigations/get?id="+invB, tokB, nil)
	if code != http.StatusOK || out["investigation"] == nil {
		t.Errorf("own investigation get: %d %v", code, out)
	}
	code, _, raw := svcCall(t, srv, http.MethodGet, "/api/cameras/media/tokb1234", tokB, nil)
	if code != http.StatusOK || string(raw) != "jpegbytes" {
		t.Errorf("own media fetch: %d %q", code, raw)
	}

	// An expired capture in the OWN site must also 404.
	if _, err := insertCameraCapture(db, camCapture{
		SiteID: siteB, Kind: "snapshot", Token: "tokbold1", Path: mediaPath,
		ContentType: "image/jpeg", ExpiresAt: time.Now().Add(-time.Hour).Format(time.RFC3339),
	}); err != nil {
		t.Fatalf("insert expired capture: %v", err)
	}
	code, _, _ = svcCall(t, srv, http.MethodGet, "/api/cameras/media/tokbold1", tokB, nil)
	if code != http.StatusNotFound {
		t.Errorf("expired media: status = %d, want 404", code)
	}
}

// ─────────────────────────── provision happy path ───────────────────────────

func TestCamServiceProvisionFlow(t *testing.T) {
	cfg := newSvcTestConfig(t)
	db := svcTestDB(t, cfg)
	srv := newSvcTestServer(t, cfg)

	_, mgmtTok, err := mintCameraServiceToken(db, "management", "", "connect")
	if err != nil {
		t.Fatalf("mint management token: %v", err)
	}

	// Provision creates the site AND its site token in one call.
	code, out, _ := svcCall(t, srv, http.MethodPost, "/api/cameras/provision", mgmtTok, map[string]any{"name": "Clinic A", "description": "front lobby"})
	if code != http.StatusOK || out["ok"] != true {
		t.Fatalf("provision: %d %v", code, out)
	}
	siteID, _ := out["site_id"].(string)
	siteTok, _ := out["token"].(string)
	if siteID == "" || !strings.HasPrefix(siteTok, "cpsk_") {
		t.Fatalf("provision returned site_id=%q token=%q", siteID, siteTok)
	}

	// The minted token reads its own site — and ONLY its own site.
	code, out, _ = svcCall(t, srv, http.MethodGet, "/api/cameras/site", siteTok, nil)
	if code != http.StatusOK {
		t.Fatalf("site get: %d %v", code, out)
	}
	site, _ := out["site"].(map[string]any)
	if site["name"] != "Clinic A" || site["id"] != siteID {
		t.Errorf("site = %v, want Clinic A / %s", site, siteID)
	}

	// DVR create (no discovery — that needs a device).
	code, out, _ = svcCall(t, srv, http.MethodPost, "/api/cameras/dvrs", siteTok, map[string]any{
		"name": "Main DVR", "brand": "hikvision", "host": "192.0.2.10", "username": "admin", "password": "secret", "timezone": "Asia/Amman",
	})
	if code != http.StatusOK || out["ok"] != true {
		t.Fatalf("dvr create: %d %v", code, out)
	}
	code, out, raw := svcCall(t, srv, http.MethodGet, "/api/cameras/dvrs", siteTok, nil)
	if code != http.StatusOK {
		t.Fatalf("dvr list: %d %v", code, out)
	}
	dvrs, _ := out["dvrs"].([]any)
	if len(dvrs) != 1 {
		t.Fatalf("dvr list = %v, want 1", out)
	}
	if strings.Contains(string(raw), `"secret"`) || strings.Contains(string(raw), `"password":`) {
		t.Errorf("dvr list must never echo the password: %s", raw)
	}

	// Investigation enqueue: returns queued immediately (worker enabled → no
	// inline model run), with the opening operator message persisted.
	code, out, _ = svcCall(t, srv, http.MethodPost, "/api/cameras/investigations", siteTok, map[string]any{"question": "Did the courier arrive today?"})
	if code != http.StatusOK || out["ok"] != true || out["status"] != "queued" {
		t.Fatalf("investigation enqueue: %d %v", code, out)
	}
	invID, _ := out["id"].(string)
	code, out, _ = svcCall(t, srv, http.MethodGet, "/api/cameras/investigations/get?id="+invID, siteTok, nil)
	if code != http.StatusOK {
		t.Fatalf("investigation get: %d %v", code, out)
	}
	msgs, _ := out["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("messages = %v, want the opening operator message", out)
	}
	if m, _ := msgs[0].(map[string]any); m["role"] != "operator" || m["content"] != "Did the courier arrive today?" {
		t.Errorf("opening message = %v", msgs[0])
	}
	// Replying to a queued row must bounce (a worker owns it).
	code, out, _ = svcCall(t, srv, http.MethodPost, "/api/cameras/investigations/reply", siteTok, map[string]any{"id": invID, "message": "also check the back door"})
	if code != http.StatusOK || out["ok"] != false {
		t.Errorf("reply while queued: %d %v, want ok=false bounce", code, out)
	}

	// Callback registration round-trips (has_secret only, never the secret).
	code, out, _ = svcCall(t, srv, http.MethodPost, "/api/cameras/callback", siteTok, map[string]any{"url": "https://connect.example/api/camera-system/callback", "secret": "s3cret"})
	if code != http.StatusOK || out["ok"] != true {
		t.Fatalf("callback register: %d %v", code, out)
	}
	code, out, raw = svcCall(t, srv, http.MethodGet, "/api/cameras/callback", siteTok, nil)
	if code != http.StatusOK || out["url"] != "https://connect.example/api/camera-system/callback" || out["has_secret"] != true {
		t.Fatalf("callback get: %d %v", code, out)
	}
	if strings.Contains(string(raw), "s3cret") {
		t.Errorf("callback get must not leak the secret: %s", raw)
	}
	cb, cerr := getCameraSiteCallback(db, siteID)
	if cerr != nil || cb.SecretEnc == "" || strings.Contains(cb.SecretEnc, "s3cret") {
		t.Errorf("stored secret must be encrypted: %+v %v", cb, cerr)
	}
	if dec, derr := decryptSecret(cfg, cb.SecretEnc); derr != nil || dec != "s3cret" {
		t.Errorf("secret must round-trip through decryptSecret: %q %v", dec, derr)
	}

	// Usage reflects the site.
	code, out, _ = svcCall(t, srv, http.MethodGet, "/api/cameras/usage", mgmtTok, nil)
	if code != http.StatusOK {
		t.Fatalf("usage: %d %v", code, out)
	}
	sites, _ := out["sites"].([]any)
	found := false
	for _, s := range sites {
		m, _ := s.(map[string]any)
		if m["site_id"] == siteID {
			found = true
			if m["dvr_count"] != float64(1) || m["open_investigations"] != float64(1) {
				t.Errorf("usage row = %v, want dvr_count 1 / open_investigations 1", m)
			}
		}
	}
	if !found {
		t.Fatalf("usage does not list site %s: %v", siteID, out)
	}

	// Deprovision: cascade delete + token revocation + callback removal.
	code, out, _ = svcCall(t, srv, http.MethodPost, "/api/cameras/deprovision", mgmtTok, map[string]any{"site_id": siteID})
	if code != http.StatusOK || out["ok"] != true {
		t.Fatalf("deprovision: %d %v", code, out)
	}
	code, out, _ = svcCall(t, srv, http.MethodGet, "/api/cameras/site", siteTok, nil)
	if code != http.StatusUnauthorized {
		t.Errorf("site token after deprovision: %d %v, want 401", code, out)
	}
	if _, gerr := getCameraSite(db, siteID); gerr == nil {
		t.Error("site row must be gone after deprovision")
	}
	if _, gerr := getCameraSiteCallback(db, siteID); gerr == nil {
		t.Error("callback row must be gone after deprovision")
	}
}

// ─────────────────────────── settle payload builder ───────────────────────────

func TestCamSettlePayload(t *testing.T) {
	cfg := config{PublicURL: "https://proxy.example"}
	inv := camInvestigation{ID: "inv_1", SiteID: "site_1", Status: "answered", ViewToken: "vtok1"}
	at := time.Date(2026, 7, 2, 14, 32, 11, 0, time.UTC)

	mediaJSON := mustJSON([]evidenceItem{
		{MediaURL: "https://proxy.example/camera/media/tok1", Caption: "North gate, 14:32"},
		{MediaURL: "https://proxy.example/camera/media/gone", Caption: "expired artifact"},
	})
	msgs := []camInvestigationMessage{
		{Role: "operator", Content: "Did Majdi arrive?"},
		{Role: "ai", Content: "Checking the gate camera.", ToolName: "snapshot"},
		{Role: "tool", Content: "snapshot ok", ToolName: "snapshot", MediaJSON: mediaJSON},
		{Role: "ai", Content: "Yes — he arrived at 14:32.", ToolName: "answer"},
	}
	caps := map[string]camCapture{
		"tok1": {Token: "tok1", ContentType: "image/jpeg", Kind: "snapshot", CameraID: "cam_9"},
	}

	p := camSettlePayload(cfg, inv, msgs, "answered", caps, at)
	if p["event"] != "camera_investigation_settled" || p["investigation_id"] != "inv_1" || p["site_id"] != "site_1" {
		t.Errorf("identity fields wrong: %v", p)
	}
	if p["status"] != "answered" || p["answer"] != "Yes — he arrived at 14:32." || p["turns"] != 2 {
		t.Errorf("answer/turns wrong: status=%v answer=%v turns=%v", p["status"], p["answer"], p["turns"])
	}
	if _, has := p["question_for_operator"]; has {
		t.Error("answered payload must not carry question_for_operator")
	}
	if p["at"] != "2026-07-02T14:32:11Z" {
		t.Errorf("at = %v", p["at"])
	}
	if p["view_url"] != "https://proxy.example/camera/investigations/vtok1" {
		t.Errorf("view_url = %v, want the public timeline URL", p["view_url"])
	}
	ev, _ := p["evidence"].([]map[string]any)
	if len(ev) != 1 {
		t.Fatalf("evidence = %v, want exactly the capture-backed item", p["evidence"])
	}
	want := map[string]any{
		"token": "tok1", "media_url": "https://proxy.example/camera/media/tok1",
		"caption": "North gate, 14:32", "content_type": "image/jpeg", "kind": "snapshot", "camera_id": "cam_9",
	}
	for k, v := range want {
		if ev[0][k] != v {
			t.Errorf("evidence[%s] = %v, want %v", k, ev[0][k], v)
		}
	}

	// awaiting_operator carries the ask_operator question.
	msgs2 := append(msgs[:3:3], camInvestigationMessage{Role: "ai", Content: "Which entrance do you mean?", ToolName: "ask_operator"})
	p2 := camSettlePayload(cfg, inv, msgs2, "awaiting_operator", caps, at)
	if p2["question_for_operator"] != "Which entrance do you mean?" {
		t.Errorf("question_for_operator = %v", p2["question_for_operator"])
	}

	// Evidence is de-duped by token and capped at camSettleEvidenceMax.
	var items []evidenceItem
	caps3 := map[string]camCapture{}
	for i := 0; i < camSettleEvidenceMax+5; i++ {
		tok := fmt.Sprintf("tk%02d", i)
		items = append(items, evidenceItem{MediaURL: "https://proxy.example/camera/media/" + tok, Caption: tok})
		items = append(items, evidenceItem{MediaURL: "https://proxy.example/camera/media/" + tok, Caption: "dup"})
		caps3[tok] = camCapture{Token: tok, ContentType: "image/jpeg", Kind: "frame"}
	}
	msgs3 := []camInvestigationMessage{
		{Role: "tool", ToolName: "past_frames", MediaJSON: mustJSON(items)},
		{Role: "ai", Content: "done", ToolName: "answer"},
	}
	p3 := camSettlePayload(cfg, inv, msgs3, "answered", caps3, at)
	ev3, _ := p3["evidence"].([]map[string]any)
	if len(ev3) != camSettleEvidenceMax {
		t.Errorf("evidence len = %d, want cap %d", len(ev3), camSettleEvidenceMax)
	}
	if ev3[0]["token"] != "tk00" || ev3[0]["caption"] != "tk00" {
		t.Errorf("evidence order/dedupe wrong: %v", ev3[0])
	}
}

// TestCamServiceInvestigationMediaTokens verifies the service transcript
// augments media items with the bare token (the admin response does not).
func TestCamServiceInvestigationMediaTokens(t *testing.T) {
	m := camInvestigationMessage{
		Role: "tool", ToolName: "snapshot",
		MediaJSON: mustJSON([]evidenceItem{{MediaURL: "https://x.example/camera/media/abc123", Caption: "gate"}}),
	}
	out := svcInvestigateMessageJSON(m)
	media, _ := out["media"].([]map[string]any)
	if len(media) != 1 || media[0]["token"] != "abc123" || media[0]["caption"] != "gate" {
		t.Fatalf("svc media = %v, want token abc123", out["media"])
	}
}
