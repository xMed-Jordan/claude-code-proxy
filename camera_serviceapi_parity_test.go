package main

// camera_serviceapi_parity_test.go — tests for the site-scoped service-API
// parity routes (camera_serviceapi_parity.go): the cross-site denial table over
// every id-taking parity route, setup questions, watches (+watch-action secret
// keep/clear/replace asserted via direct action_json reads), runs+captures,
// API tools (secret hygiene + the loopback-502 SSRF-guard proof), playbook
// reference media (pin/release + the cross-site-token oracle), site name+analyze,
// diagnostics (health/events/recapture), investigation children, and the
// queued/settled cancel CAS. Reuses the harness helpers from
// camera_serviceapi_test.go (newSvcTestConfig/newSvcTestServer/svcCall/svcTestDB).
// Temp sqlite + httptest only — no DVRs, no model calls, no external network.

import (
	"database/sql"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ─────────────────────────── local seeding helpers ───────────────────────────

// svcSeedSite creates a camera site plus its site-scoped token in one step.
func svcSeedSite(t *testing.T, db *sql.DB, name string) (siteID, token string) {
	t.Helper()
	id, err := insertCameraSite(db, camSite{Name: name})
	if err != nil {
		t.Fatalf("insertCameraSite(%s): %v", name, err)
	}
	_, tok, terr := mintCameraServiceToken(db, "site", id, name)
	if terr != nil {
		t.Fatalf("mint site token(%s): %v", name, terr)
	}
	return id, tok
}

// svcSeedMediaFile writes a real capture file under the media root and returns
// its path (some parity handlers os.Stat the on-disk file before acting).
func svcSeedMediaFile(t *testing.T, cfg config, name string) string {
	t.Helper()
	p := filepath.Join(cameraMediaRoot(cfg), name)
	if err := os.WriteFile(p, []byte("jpegbytes"), 0o600); err != nil {
		t.Fatalf("write media file %s: %v", name, err)
	}
	return p
}

// ─────────────────────────── 1. cross-site denial table ───────────────────────────

// TestSvcParityCrossSiteDenial seeds a full set of site-B rows and asserts that
// site-A's token gets the uniform 404 {"error":"not found"} on EVERY id-taking
// parity route — indistinguishable from a missing id, so ids can't be probed.
func TestSvcParityCrossSiteDenial(t *testing.T) {
	cfg := newSvcTestConfig(t)
	db := svcTestDB(t, cfg)
	srv := newSvcTestServer(t, cfg)

	siteA, _ := insertCameraSite(db, camSite{Name: "Site A"})
	_, tokA, _ := mintCameraServiceToken(db, "site", siteA, "a")
	siteB, tokB := svcSeedSite(t, db, "Site B")

	// Site-B resources spanning every parity route family.
	dvrB, err := insertCameraDVR(db, cfg, CamDVR{SiteID: siteB, Name: "DVR B", Brand: "hikvision", Host: "192.0.2.9", Enabled: true})
	if err != nil {
		t.Fatalf("insertCameraDVR: %v", err)
	}
	camB, err := insertCamera(db, camera{SiteID: siteB, DVRID: dvrB, Channel: 1, Name: "Cam B", Enabled: true})
	if err != nil {
		t.Fatalf("insertCamera: %v", err)
	}
	watchB, err := insertCameraWatch(db, watch{SiteID: siteB, Name: "Watch B", Mode: "escalate", IntervalSeconds: 300})
	if err != nil {
		t.Fatalf("insertCameraWatch: %v", err)
	}
	runB, err := insertCameraWatchRun(db, camWatchRun{SiteID: siteB, WatchID: watchB, Status: "done"})
	if err != nil {
		t.Fatalf("insertCameraWatchRun: %v", err)
	}
	qB, err := insertCameraQuestion(db, camQuestion{SiteID: siteB, Question: "b?", Status: "open"})
	if err != nil {
		t.Fatalf("insertCameraQuestion: %v", err)
	}
	toolB, err := insertCameraAPITool(db, cfg, camAPITool{SiteID: siteB, Name: "toolb", Method: "GET", URLTemplate: "https://api.example/b"}, "")
	if err != nil {
		t.Fatalf("insertCameraAPITool: %v", err)
	}
	pbB, err := insertCameraPlaybook(db, camPlaybook{SiteID: siteB, Name: "pb-b", Enabled: true})
	if err != nil {
		t.Fatalf("insertCameraPlaybook: %v", err)
	}
	capPath := svcSeedMediaFile(t, cfg, "evidence-b.jpg")
	if _, err := insertCameraCapture(db, camCapture{SiteID: siteB, Kind: "snapshot", Token: "capbtok1", Path: capPath, ContentType: "image/jpeg"}); err != nil {
		t.Fatalf("insertCameraCapture: %v", err)
	}
	mediaB, err := insertCameraPlaybookMedia(db, camPlaybookMedia{PlaybookID: pbB, CaptureToken: "capbtok1", CreatedAt: nowRFC3339()})
	if err != nil {
		t.Fatalf("insertCameraPlaybookMedia: %v", err)
	}
	invB, err := insertCameraInvestigation(db, camInvestigation{SiteID: siteB, Title: "q", Question: "q", Status: "queued"})
	if err != nil {
		t.Fatalf("insertCameraInvestigation: %v", err)
	}

	denied := []struct {
		name, method, path string
		body               any
	}{
		{"question answer", http.MethodPost, "/api/cameras/questions/answer", map[string]any{"id": qB, "answer": "x"}},
		{"question dismiss", http.MethodPost, "/api/cameras/questions/dismiss", map[string]any{"id": qB}},
		{"watch update", http.MethodPost, "/api/cameras/watches/update", map[string]any{"id": watchB, "name": "hacked"}},
		{"watch delete", http.MethodPost, "/api/cameras/watches/delete", map[string]any{"id": watchB}},
		{"watch toggle", http.MethodPost, "/api/cameras/watches/toggle", map[string]any{"id": watchB, "enabled": false}},
		{"watch run", http.MethodPost, "/api/cameras/watches/run", map[string]any{"id": watchB}},
		{"runs by foreign watch", http.MethodGet, "/api/cameras/runs?watch_id=" + watchB, nil},
		{"run get", http.MethodGet, "/api/cameras/runs/get?id=" + runB, nil},
		{"captures by foreign run", http.MethodGet, "/api/cameras/captures?run_id=" + runB, nil},
		{"apitool update", http.MethodPost, "/api/cameras/apitools/update", map[string]any{"id": toolB, "name": "x"}},
		{"apitool delete", http.MethodPost, "/api/cameras/apitools/delete", map[string]any{"id": toolB}},
		{"apitool test", http.MethodPost, "/api/cameras/apitools/test", map[string]any{"id": toolB, "params": map[string]any{}}},
		{"clip", http.MethodPost, "/api/cameras/clip", map[string]any{"id": camB}},
		{"recapture", http.MethodPost, "/api/cameras/recapture", map[string]any{"camera_id": camB}},
		{"events by foreign dvr", http.MethodGet, "/api/cameras/events?dvr_id=" + dvrB, nil},
		{"playbook media list", http.MethodGet, "/api/cameras/playbooks/media?playbook_id=" + pbB, nil},
		{"playbook media attach", http.MethodPost, "/api/cameras/playbooks/media", map[string]any{"playbook_id": pbB, "token": "capbtok1"}},
		{"playbook media delete", http.MethodPost, "/api/cameras/playbooks/media/delete", map[string]any{"id": mediaB}},
		{"investigation cancel", http.MethodPost, "/api/cameras/investigations/cancel", map[string]any{"id": invB}},
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

	// Positive controls with the OWNING token — proves each 404 above is a
	// cross-site denial, not a broken route (reads only, no destructive writes).
	positives := []struct {
		name, path string
	}{
		{"own run get", "/api/cameras/runs/get?id=" + runB},
		{"own runs by watch", "/api/cameras/runs?watch_id=" + watchB},
		{"own playbook media list", "/api/cameras/playbooks/media?playbook_id=" + pbB},
		{"own events by dvr", "/api/cameras/events?dvr_id=" + dvrB},
	}
	for _, p := range positives {
		code, out, _ := svcCall(t, srv, http.MethodGet, p.path, tokB, nil)
		if code != http.StatusOK {
			t.Errorf("%s with owning token: status = %d %v, want 200", p.name, code, out)
		}
	}
}

// ─────────────────────────── 2. setup questions ───────────────────────────

func TestSvcQuestions(t *testing.T) {
	cfg := newSvcTestConfig(t)
	db := svcTestDB(t, cfg)
	srv := newSvcTestServer(t, cfg)

	siteA, tokA := svcSeedSite(t, db, "Site A")
	siteB, tokB := svcSeedSite(t, db, "Site B")

	qOpen1, _ := insertCameraQuestion(db, camQuestion{SiteID: siteA, Question: "Did the courier arrive?", Status: "open"})
	qOpen2, _ := insertCameraQuestion(db, camQuestion{SiteID: siteA, Question: "Which entrance?", Status: "open"})
	if _, err := insertCameraQuestion(db, camQuestion{SiteID: siteA, Question: "old?", Answer: "yes", Status: "answered"}); err != nil {
		t.Fatalf("seed answered question: %v", err)
	}
	// Site-B question — must never surface under site A's token.
	if _, err := insertCameraQuestion(db, camQuestion{SiteID: siteB, Question: "b?", Status: "open"}); err != nil {
		t.Fatalf("seed site-B question: %v", err)
	}

	// Two-site isolation + no-status listing returns all of A's three questions.
	code, out, _ := svcCall(t, srv, http.MethodGet, "/api/cameras/questions", tokA, nil)
	if code != http.StatusOK {
		t.Fatalf("questions list: %d %v", code, out)
	}
	if qs, _ := out["questions"].([]any); len(qs) != 3 {
		t.Errorf("A questions = %d, want 3", len(qs))
	}
	// Status filter narrows to the two open rows.
	code, out, _ = svcCall(t, srv, http.MethodGet, "/api/cameras/questions?status=open", tokA, nil)
	if qs, _ := out["questions"].([]any); code != http.StatusOK || len(qs) != 2 {
		t.Errorf("open questions = %d (%d), want 2", len(qs), code)
	}
	// Site B sees only its own single row.
	code, out, _ = svcCall(t, srv, http.MethodGet, "/api/cameras/questions", tokB, nil)
	if qs, _ := out["questions"].([]any); code != http.StatusOK || len(qs) != 1 {
		t.Errorf("B questions = %d (%d), want 1", len(qs), code)
	}

	// Empty answer is rejected before any write.
	code, out, _ = svcCall(t, srv, http.MethodPost, "/api/cameras/questions/answer", tokA, map[string]any{"id": qOpen1, "answer": "  "})
	if code != http.StatusBadRequest {
		t.Errorf("empty answer: status = %d %v, want 400", code, out)
	}

	// Answer flips status/answer/answered_at.
	code, out, _ = svcCall(t, srv, http.MethodPost, "/api/cameras/questions/answer", tokA, map[string]any{"id": qOpen1, "answer": "the courier came at 14:32"})
	if code != http.StatusOK || out["ok"] != true {
		t.Fatalf("answer: %d %v", code, out)
	}
	got, _ := getCameraQuestion(db, qOpen1)
	if got.Status != "answered" || got.Answer != "the courier came at 14:32" || got.AnsweredAt == "" {
		t.Errorf("answered question = %+v, want status answered + answer + answered_at", got)
	}

	// Dismiss flips status only.
	code, out, _ = svcCall(t, srv, http.MethodPost, "/api/cameras/questions/dismiss", tokA, map[string]any{"id": qOpen2})
	if code != http.StatusOK || out["ok"] != true {
		t.Fatalf("dismiss: %d %v", code, out)
	}
	if got, _ := getCameraQuestion(db, qOpen2); got.Status != "dismissed" {
		t.Errorf("dismissed question status = %q, want dismissed", got.Status)
	}
}

// ─────────────────────────── 3. watches (+ action secret semantics) ───────────────────────────

func TestSvcWatches(t *testing.T) {
	cfg := newSvcTestConfig(t)
	db := svcTestDB(t, cfg)
	srv := newSvcTestServer(t, cfg)

	_, tokA := svcSeedSite(t, db, "Site A")

	// Bad mode is a 400.
	code, out, _ := svcCall(t, srv, http.MethodPost, "/api/cameras/watches", tokA, map[string]any{"name": "bad", "mode": "bogus"})
	if code != http.StatusBadRequest {
		t.Errorf("bad mode: status = %d %v, want 400", code, out)
	}

	// Create with defaults (no mode/interval/enabled).
	code, out, _ = svcCall(t, srv, http.MethodPost, "/api/cameras/watches", tokA, map[string]any{"name": "W1", "instruction": "watch the door"})
	if code != http.StatusOK || out["ok"] != true {
		t.Fatalf("create W1: %d %v", code, out)
	}
	w1 := out["watch"].(map[string]any)
	w1ID := w1["id"].(string)
	if w1["mode"] != "escalate" || w1["interval_seconds"] != float64(300) || w1["enabled"] != false || w1["next_run_at"] != "" {
		t.Errorf("W1 defaults wrong: %v", w1)
	}
	if act := w1["action"].(map[string]any); act["has_secret"] != false {
		t.Errorf("W1 action has_secret = %v, want false", act["has_secret"])
	}

	// Create enabled with a webhook action secret.
	code, out, raw := svcCall(t, srv, http.MethodPost, "/api/cameras/watches", tokA, map[string]any{
		"name": "W2", "mode": "escalate", "enabled": true,
		"action": map[string]any{"type": "webhook", "url": "https://hook.example", "secret": "topsecret"},
	})
	if code != http.StatusOK || out["ok"] != true {
		t.Fatalf("create W2: %d %v", code, out)
	}
	if strings.Contains(string(raw), "topsecret") {
		t.Errorf("create W2 response leaked the secret: %s", raw)
	}
	w2 := out["watch"].(map[string]any)
	w2ID := w2["id"].(string)
	if w2["enabled"] != true || w2["next_run_at"] == "" {
		t.Errorf("W2 enabled should set next_run_at: %v", w2)
	}
	if act := w2["action"].(map[string]any); act["has_secret"] != true {
		t.Errorf("W2 action has_secret = %v, want true", act["has_secret"])
	}

	// Raw list body never carries the secret; has_secret flags are correct.
	code, _, raw = svcCall(t, srv, http.MethodGet, "/api/cameras/watches", tokA, nil)
	if code != http.StatusOK {
		t.Fatalf("watch list: %d", code)
	}
	if strings.Contains(string(raw), "topsecret") {
		t.Errorf("watch list leaked the secret: %s", raw)
	}

	// Stored secret is encrypted and round-trips.
	enc1 := parseCamActionConfig(mustGetWatch(t, db, w2ID).ActionJSON).TokenEnc
	if enc1 == "" || strings.Contains(enc1, "topsecret") {
		t.Errorf("stored token_enc = %q, want encrypted non-empty", enc1)
	}
	if dec, derr := decryptSecret(cfg, enc1); derr != nil || dec != "topsecret" {
		t.Errorf("token_enc decrypt = %q %v, want topsecret", dec, derr)
	}

	// Update with a BLANK secret KEEPS the stored token_enc byte-for-byte, and
	// still applies the type/url change (the deliberate divergence from admin).
	code, out, _ = svcCall(t, srv, http.MethodPost, "/api/cameras/watches/update", tokA, map[string]any{
		"id": w2ID, "name": "W2b",
		"action": map[string]any{"type": "webhook", "url": "https://hook2.example"},
	})
	if code != http.StatusOK || out["ok"] != true {
		t.Fatalf("update keep: %d %v", code, out)
	}
	kept := parseCamActionConfig(mustGetWatch(t, db, w2ID).ActionJSON)
	if kept.TokenEnc != enc1 {
		t.Errorf("blank-secret update changed token_enc: %q → %q", enc1, kept.TokenEnc)
	}
	if kept.URL != "https://hook2.example" {
		t.Errorf("blank-secret update dropped the url change: %q", kept.URL)
	}
	if act := out["watch"].(map[string]any)["action"].(map[string]any); act["has_secret"] != true {
		t.Errorf("kept-secret update has_secret = %v, want true", act["has_secret"])
	}

	// clear_secret removes it.
	code, out, _ = svcCall(t, srv, http.MethodPost, "/api/cameras/watches/update", tokA, map[string]any{
		"id": w2ID,
		"action": map[string]any{"type": "webhook", "url": "https://hook2.example", "clear_secret": true},
	})
	if code != http.StatusOK || out["ok"] != true {
		t.Fatalf("update clear: %d %v", code, out)
	}
	if te := parseCamActionConfig(mustGetWatch(t, db, w2ID).ActionJSON).TokenEnc; te != "" {
		t.Errorf("clear_secret left token_enc = %q, want empty", te)
	}
	if act := out["watch"].(map[string]any)["action"].(map[string]any); act["has_secret"] != false {
		t.Errorf("cleared-secret has_secret = %v, want false", act["has_secret"])
	}

	// A non-blank secret replaces it (decrypt round-trip proves the new value).
	code, out, _ = svcCall(t, srv, http.MethodPost, "/api/cameras/watches/update", tokA, map[string]any{
		"id": w2ID,
		"action": map[string]any{"type": "webhook", "url": "https://hook3.example", "secret": "newsecret"},
	})
	if code != http.StatusOK || out["ok"] != true {
		t.Fatalf("update replace: %d %v", code, out)
	}
	te := parseCamActionConfig(mustGetWatch(t, db, w2ID).ActionJSON).TokenEnc
	if dec, derr := decryptSecret(cfg, te); derr != nil || dec != "newsecret" {
		t.Errorf("replaced token_enc decrypt = %q %v, want newsecret", dec, derr)
	}

	// Toggle enable sets next_run_at; disable clears the flag.
	code, out, _ = svcCall(t, srv, http.MethodPost, "/api/cameras/watches/toggle", tokA, map[string]any{"id": w1ID, "enabled": true})
	if code != http.StatusOK || out["ok"] != true {
		t.Fatalf("toggle enable: %d %v", code, out)
	}
	if wt := mustGetWatch(t, db, w1ID); !wt.Enabled || wt.NextRunAt == "" {
		t.Errorf("enabled toggle: enabled=%v next_run_at=%q, want true + set", wt.Enabled, wt.NextRunAt)
	}
	code, out, _ = svcCall(t, srv, http.MethodPost, "/api/cameras/watches/toggle", tokA, map[string]any{"id": w1ID, "enabled": false})
	if code != http.StatusOK || out["ok"] != true {
		t.Fatalf("toggle disable: %d %v", code, out)
	}
	if wt := mustGetWatch(t, db, w1ID); wt.Enabled {
		t.Errorf("disabled toggle left enabled=true")
	}

	// Delete removes the row.
	code, out, _ = svcCall(t, srv, http.MethodPost, "/api/cameras/watches/delete", tokA, map[string]any{"id": w1ID})
	if code != http.StatusOK || out["ok"] != true {
		t.Fatalf("delete: %d %v", code, out)
	}
	if _, err := getCameraWatch(db, w1ID); err == nil {
		t.Error("deleted watch still resolves")
	}
}

// mustGetWatch loads a watch or fails the test.
func mustGetWatch(t *testing.T, db *sql.DB, id string) watch {
	t.Helper()
	wt, err := getCameraWatch(db, id)
	if err != nil {
		t.Fatalf("getCameraWatch(%s): %v", id, err)
	}
	return wt
}

// ─────────────────────────── 4. runs + captures ───────────────────────────

func TestSvcRunsAndCaptures(t *testing.T) {
	cfg := newSvcTestConfig(t)
	db := svcTestDB(t, cfg)
	srv := newSvcTestServer(t, cfg)

	siteA, tokA := svcSeedSite(t, db, "Site A")
	_, tokB := svcSeedSite(t, db, "Site B")

	wa1, _ := insertCameraWatch(db, watch{SiteID: siteA, Name: "WA1", Mode: "escalate", IntervalSeconds: 300})
	wa2, _ := insertCameraWatch(db, watch{SiteID: siteA, Name: "WA2", Mode: "escalate", IntervalSeconds: 300})
	r1, _ := insertCameraWatchRun(db, camWatchRun{SiteID: siteA, WatchID: wa1, Status: "done"})
	r2, _ := insertCameraWatchRun(db, camWatchRun{SiteID: siteA, WatchID: wa1, Status: "done"})
	_, _ = insertCameraWatchRun(db, camWatchRun{SiteID: siteA, WatchID: wa2, Status: "done"})

	// Captures: cap1 on r1 (camX), cap2 on r2 (camY) — bare tokens set explicitly.
	if _, err := insertCameraCapture(db, camCapture{SiteID: siteA, CameraID: "camX", WatchRunID: r1, Kind: "snapshot", Token: "captokX", ContentType: "image/jpeg"}); err != nil {
		t.Fatalf("insert cap1: %v", err)
	}
	if _, err := insertCameraCapture(db, camCapture{SiteID: siteA, CameraID: "camY", WatchRunID: r2, Kind: "snapshot", Token: "captokY", ContentType: "image/jpeg"}); err != nil {
		t.Fatalf("insert cap2: %v", err)
	}

	// Site-wide runs: all three.
	code, out, _ := svcCall(t, srv, http.MethodGet, "/api/cameras/runs", tokA, nil)
	if runs, _ := out["runs"].([]any); code != http.StatusOK || len(runs) != 3 {
		t.Fatalf("site-wide runs = %d (%d), want 3", len(runs), code)
	}
	// By-watch: only WA1's two runs.
	code, out, _ = svcCall(t, srv, http.MethodGet, "/api/cameras/runs?watch_id="+wa1, tokA, nil)
	if runs, _ := out["runs"].([]any); code != http.StatusOK || len(runs) != 2 {
		t.Fatalf("WA1 runs = %d (%d), want 2", len(runs), code)
	}
	// Limit caps the list.
	code, out, _ = svcCall(t, srv, http.MethodGet, "/api/cameras/runs?limit=1", tokA, nil)
	if runs, _ := out["runs"].([]any); code != http.StatusOK || len(runs) != 1 {
		t.Fatalf("limited runs = %d (%d), want 1", len(runs), code)
	}

	// runs/get includes the run's captures.
	code, out, _ = svcCall(t, srv, http.MethodGet, "/api/cameras/runs/get?id="+r1, tokA, nil)
	if code != http.StatusOK {
		t.Fatalf("run get: %d %v", code, out)
	}
	run := out["run"].(map[string]any)
	caps, _ := run["captures"].([]any)
	if len(caps) != 1 {
		t.Fatalf("run captures = %d, want 1", len(caps))
	}

	// Captures site-wide with the bare token key.
	code, out, _ = svcCall(t, srv, http.MethodGet, "/api/cameras/captures", tokA, nil)
	if code != http.StatusOK {
		t.Fatalf("captures list: %d %v", code, out)
	}
	all, _ := out["captures"].([]any)
	if len(all) != 2 {
		t.Fatalf("site captures = %d, want 2", len(all))
	}
	sawToken := false
	for _, c := range all {
		m := c.(map[string]any)
		if m["token"] == "captokX" || m["token"] == "captokY" {
			sawToken = true
		}
	}
	if !sawToken {
		t.Errorf("captures missing bare token key: %v", all)
	}
	// Camera filter.
	code, out, _ = svcCall(t, srv, http.MethodGet, "/api/cameras/captures?camera_id=camX", tokA, nil)
	if cc, _ := out["captures"].([]any); code != http.StatusOK || len(cc) != 1 {
		t.Errorf("camX captures = %d (%d), want 1", len(cc), code)
	}
	// Run filter.
	code, out, _ = svcCall(t, srv, http.MethodGet, "/api/cameras/captures?run_id="+r1, tokA, nil)
	if cc, _ := out["captures"].([]any); code != http.StatusOK || len(cc) != 1 {
		t.Errorf("r1 captures = %d (%d), want 1", len(cc), code)
	}

	// A foreign run is a uniform 404 across every run/capture entry point.
	for _, p := range []string{"/api/cameras/runs/get?id=" + r1, "/api/cameras/runs?watch_id=" + wa1, "/api/cameras/captures?run_id=" + r1} {
		code, out, _ := svcCall(t, srv, http.MethodGet, p, tokB, nil)
		if code != http.StatusNotFound || out["error"] != "not found" {
			t.Errorf("foreign %s: %d %v, want 404 not found", p, code, out)
		}
	}
}

// ─────────────────────────── 5. API tools ───────────────────────────

func TestSvcAPITools(t *testing.T) {
	cfg := newSvcTestConfig(t)
	db := svcTestDB(t, cfg)
	srv := newSvcTestServer(t, cfg)

	_, tokA := svcSeedSite(t, db, "Site A")

	// Create with a secret; response and DB must never expose the plaintext.
	code, out, raw := svcCall(t, srv, http.MethodPost, "/api/cameras/apitools", tokA, map[string]any{
		"name": "weather", "method": "GET", "url_template": "https://api.example/w?q={{q}}", "auth_secret": "sk-secret",
	})
	if code != http.StatusOK || out["ok"] != true {
		t.Fatalf("create tool: %d %v", code, out)
	}
	if strings.Contains(string(raw), "sk-secret") {
		t.Errorf("create response leaked the secret: %s", raw)
	}
	toolID := out["tool"].(map[string]any)["id"].(string)
	if hs := out["tool"].(map[string]any)["has_secret"]; hs != true {
		t.Errorf("created tool has_secret = %v, want true", hs)
	}
	stored, _ := getCameraAPITool(db, toolID)
	if stored.AuthSecretEnc == "" || strings.Contains(stored.AuthSecretEnc, "sk-secret") {
		t.Errorf("stored auth_secret_enc = %q, want encrypted", stored.AuthSecretEnc)
	}
	if dec, derr := decryptSecret(cfg, stored.AuthSecretEnc); derr != nil || dec != "sk-secret" {
		t.Errorf("stored secret decrypt = %q %v, want sk-secret", dec, derr)
	}
	// List never leaks the secret.
	code, _, raw = svcCall(t, srv, http.MethodGet, "/api/cameras/apitools", tokA, nil)
	if code != http.StatusOK || strings.Contains(string(raw), "sk-secret") {
		t.Errorf("tool list leaked the secret or failed: %d %s", code, raw)
	}

	// Duplicate name → 409.
	code, out, _ = svcCall(t, srv, http.MethodPost, "/api/cameras/apitools", tokA, map[string]any{
		"name": "weather", "method": "GET", "url_template": "https://api.example/other",
	})
	if code != http.StatusConflict {
		t.Errorf("duplicate name: status = %d %v, want 409", code, out)
	}

	// Update keeps the secret when auth_secret is blank.
	code, out, _ = svcCall(t, srv, http.MethodPost, "/api/cameras/apitools/update", tokA, map[string]any{"id": toolID, "description": "updated"})
	if code != http.StatusOK || out["ok"] != true {
		t.Fatalf("update keep: %d %v", code, out)
	}
	if s, _ := getCameraAPITool(db, toolID); s.AuthSecretEnc != stored.AuthSecretEnc {
		t.Errorf("blank auth_secret update changed the stored secret")
	}
	// clear_secret removes it.
	code, out, _ = svcCall(t, srv, http.MethodPost, "/api/cameras/apitools/update", tokA, map[string]any{"id": toolID, "clear_secret": true})
	if code != http.StatusOK || out["ok"] != true {
		t.Fatalf("update clear: %d %v", code, out)
	}
	if s, _ := getCameraAPITool(db, toolID); s.AuthSecretEnc != "" {
		t.Errorf("clear_secret left auth_secret_enc = %q, want empty", s.AuthSecretEnc)
	}
	if out["tool"].(map[string]any)["has_secret"] != false {
		t.Errorf("cleared tool has_secret should be false")
	}
	// A non-blank auth_secret replaces it.
	code, out, _ = svcCall(t, srv, http.MethodPost, "/api/cameras/apitools/update", tokA, map[string]any{"id": toolID, "auth_secret": "sk-new"})
	if code != http.StatusOK || out["ok"] != true {
		t.Fatalf("update replace: %d %v", code, out)
	}
	if s, _ := getCameraAPITool(db, toolID); func() bool { d, e := decryptSecret(cfg, s.AuthSecretEnc); return e != nil || d != "sk-new" }() {
		t.Errorf("replace did not store sk-new")
	}

	// Missing param → 400 (substitution fails before any HTTP).
	code, out, _ = svcCall(t, srv, http.MethodPost, "/api/cameras/apitools/test", tokA, map[string]any{"id": toolID, "params": map[string]any{}})
	if code != http.StatusBadRequest {
		t.Errorf("missing param test: status = %d %v, want 400", code, out)
	}

	// Loopback URL → 502: the SSRF-guarded client (camGuardedHTTPClient) blocks
	// the dial to 127.0.0.1 even though a server is listening, proving the guard
	// is on the test path.
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("should never be reached"))
	}))
	defer backend.Close()
	code, out, _ = svcCall(t, srv, http.MethodPost, "/api/cameras/apitools", tokA, map[string]any{
		"name": "loopback", "method": "GET", "url_template": backend.URL,
	})
	if code != http.StatusOK || out["ok"] != true {
		t.Fatalf("create loopback tool: %d %v", code, out)
	}
	loopID := out["tool"].(map[string]any)["id"].(string)
	code, out, _ = svcCall(t, srv, http.MethodPost, "/api/cameras/apitools/test", tokA, map[string]any{"id": loopID, "params": map[string]any{}})
	if code != http.StatusBadGateway {
		t.Errorf("loopback test: status = %d %v, want 502 (guard blocked the dial)", code, out)
	}

	// Delete removes the row.
	code, out, _ = svcCall(t, srv, http.MethodPost, "/api/cameras/apitools/delete", tokA, map[string]any{"id": toolID})
	if code != http.StatusOK || out["ok"] != true {
		t.Fatalf("delete: %d %v", code, out)
	}
	if _, err := getCameraAPITool(db, toolID); err == nil {
		t.Error("deleted tool still resolves")
	}
}

// ─────────────────────────── 6. playbook reference media ───────────────────────────

func TestSvcPlaybookMedia(t *testing.T) {
	cfg := newSvcTestConfig(t)
	db := svcTestDB(t, cfg)
	srv := newSvcTestServer(t, cfg)

	siteA, tokA := svcSeedSite(t, db, "Site A")
	siteB, _ := svcSeedSite(t, db, "Site B")

	pbA, _ := insertCameraPlaybook(db, camPlaybook{SiteID: siteA, Name: "pb-a", Enabled: true})
	pbB, _ := insertCameraPlaybook(db, camPlaybook{SiteID: siteB, Name: "pb-b", Enabled: true})

	// A live capture in site A (real file, future expiry).
	pathA := svcSeedMediaFile(t, cfg, "ref-a.jpg")
	future := time.Now().Add(time.Hour).Format(time.RFC3339)
	if _, err := insertCameraCapture(db, camCapture{SiteID: siteA, Kind: "snapshot", Token: "capa1", Path: pathA, ContentType: "image/jpeg", ExpiresAt: future}); err != nil {
		t.Fatalf("insert capA: %v", err)
	}

	// Attach pins the capture (expires_at cleared).
	code, out, _ := svcCall(t, srv, http.MethodPost, "/api/cameras/playbooks/media", tokA, map[string]any{"playbook_id": pbA, "token": "capa1", "note": "standby position"})
	if code != http.StatusOK || out["ok"] != true {
		t.Fatalf("attach: %d %v", code, out)
	}
	media := out["media"].(map[string]any)
	mediaID := media["id"].(string)
	if media["capture_token"] != "capa1" {
		t.Errorf("attached media capture_token = %v, want capa1", media["capture_token"])
	}
	if cpt, _ := getCameraCaptureByToken(db, "capa1"); cpt.ExpiresAt != "" {
		t.Errorf("attach did not pin the capture: expires_at = %q, want empty", cpt.ExpiresAt)
	}

	// List shows the one attached row.
	code, out, _ = svcCall(t, srv, http.MethodGet, "/api/cameras/playbooks/media?playbook_id="+pbA, tokA, nil)
	if m, _ := out["media"].([]any); code != http.StatusOK || len(m) != 1 {
		t.Fatalf("media list = %d (%d), want 1", len(m), code)
	}

	// Delete releases the capture back to normal retention (expires_at re-set).
	code, out, _ = svcCall(t, srv, http.MethodPost, "/api/cameras/playbooks/media/delete", tokA, map[string]any{"id": mediaID})
	if code != http.StatusOK || out["ok"] != true {
		t.Fatalf("delete media: %d %v", code, out)
	}
	code, out, _ = svcCall(t, srv, http.MethodGet, "/api/cameras/playbooks/media?playbook_id="+pbA, tokA, nil)
	if m, _ := out["media"].([]any); len(m) != 0 {
		t.Errorf("media list after delete = %d, want 0", len(m))
	}
	if cpt, _ := getCameraCaptureByToken(db, "capa1"); cpt.ExpiresAt == "" {
		t.Errorf("delete did not release the capture: expires_at still empty (pinned)")
	}

	// Missing / expired / cross-site tokens all return the IDENTICAL 400 body so
	// the token namespace can't be probed across sites (the oracle check).
	pathExp := svcSeedMediaFile(t, cfg, "ref-expired.jpg")
	past := time.Now().Add(-time.Hour).Format(time.RFC3339)
	if _, err := insertCameraCapture(db, camCapture{SiteID: siteA, Kind: "snapshot", Token: "capexp", Path: pathExp, ContentType: "image/jpeg", ExpiresAt: past}); err != nil {
		t.Fatalf("insert expired capture: %v", err)
	}
	pathB := svcSeedMediaFile(t, cfg, "ref-b.jpg")
	if _, err := insertCameraCapture(db, camCapture{SiteID: siteB, Kind: "snapshot", Token: "capbx", Path: pathB, ContentType: "image/jpeg", ExpiresAt: future}); err != nil {
		t.Fatalf("insert site-B capture: %v", err)
	}

	_, _, rawMissing := svcCall(t, srv, http.MethodPost, "/api/cameras/playbooks/media", tokA, map[string]any{"playbook_id": pbA, "token": "does-not-exist"})
	codeExp, _, rawExpired := svcCall(t, srv, http.MethodPost, "/api/cameras/playbooks/media", tokA, map[string]any{"playbook_id": pbA, "token": "capexp"})
	codeCross, _, rawCross := svcCall(t, srv, http.MethodPost, "/api/cameras/playbooks/media", tokA, map[string]any{"playbook_id": pbA, "token": "capbx"})
	if codeExp != http.StatusBadRequest || codeCross != http.StatusBadRequest {
		t.Fatalf("expired/cross-site attach codes = %d/%d, want 400/400", codeExp, codeCross)
	}
	if string(rawMissing) != string(rawExpired) || string(rawMissing) != string(rawCross) {
		t.Errorf("400 bodies diverge (probe oracle):\n missing=%s\n expired=%s\n cross=%s", rawMissing, rawExpired, rawCross)
	}

	// A foreign playbook is a uniform 404 on both arms.
	code, out, _ = svcCall(t, srv, http.MethodGet, "/api/cameras/playbooks/media?playbook_id="+pbB, tokA, nil)
	if code != http.StatusNotFound || out["error"] != "not found" {
		t.Errorf("foreign playbook list: %d %v, want 404 not found", code, out)
	}
	code, out, _ = svcCall(t, srv, http.MethodPost, "/api/cameras/playbooks/media", tokA, map[string]any{"playbook_id": pbB, "token": "capa1"})
	if code != http.StatusNotFound || out["error"] != "not found" {
		t.Errorf("foreign playbook attach: %d %v, want 404 not found", code, out)
	}
}

// ─────────────────────────── 7. site name + analyze ───────────────────────────

func TestSvcSiteNameAndAnalyze(t *testing.T) {
	cfg := newSvcTestConfig(t)
	db := svcTestDB(t, cfg)
	srv := newSvcTestServer(t, cfg)

	siteID, tok := svcSeedSite(t, db, "Original Name")

	// Name round-trips through POST then GET.
	code, out, _ := svcCall(t, srv, http.MethodPost, "/api/cameras/site", tok, map[string]any{"name": "Renamed Clinic"})
	if code != http.StatusOK || out["ok"] != true {
		t.Fatalf("rename: %d %v", code, out)
	}
	code, out, _ = svcCall(t, srv, http.MethodGet, "/api/cameras/site", tok, nil)
	if code != http.StatusOK {
		t.Fatalf("site get: %d %v", code, out)
	}
	if site, _ := out["site"].(map[string]any); site["name"] != "Renamed Clinic" {
		t.Errorf("site name = %v, want Renamed Clinic", site["name"])
	}
	if s, _ := getCameraSite(db, siteID); s.Name != "Renamed Clinic" {
		t.Errorf("stored site name = %q, want Renamed Clinic", s.Name)
	}

	// A blank name is rejected.
	code, out, _ = svcCall(t, srv, http.MethodPost, "/api/cameras/site", tok, map[string]any{"name": "   "})
	if code != http.StatusBadRequest {
		t.Errorf("blank name: status = %d %v, want 400", code, out)
	}

	// Analyze on an empty site returns the started shape (no cameras → the
	// background pass records an idle resting state without a model call).
	code, out, _ = svcCall(t, srv, http.MethodPost, "/api/cameras/site/analyze", tok, map[string]any{})
	if code != http.StatusOK || out["ok"] != true {
		t.Fatalf("analyze: %d %v", code, out)
	}
	if _, has := out["started"]; !has {
		t.Errorf("analyze response missing started: %v", out)
	}
	// Drain the background goroutine (running → idle) before the temp dir is
	// reaped, so its short-lived DB handle is closed first.
	for i := 0; i < 200; i++ {
		if s, _ := getCameraSite(db, siteID); s.AnalysisStatus != "running" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// ─────────────────────────── 8. diagnostics ───────────────────────────

func TestSvcDiagnostics(t *testing.T) {
	cfg := newSvcTestConfig(t)
	db := svcTestDB(t, cfg)
	srv := newSvcTestServer(t, cfg)

	siteA, tokA := svcSeedSite(t, db, "Site A")
	siteB, _ := svcSeedSite(t, db, "Site B")

	dvrA, _ := insertCameraDVR(db, cfg, CamDVR{SiteID: siteA, Name: "DVR A", Brand: "hikvision", Host: "192.0.2.10", Enabled: true})
	dvrDisabled, _ := insertCameraDVR(db, cfg, CamDVR{SiteID: siteA, Name: "DVR A-off", Brand: "hikvision", Host: "192.0.2.11", Enabled: false})
	dvrB, _ := insertCameraDVR(db, cfg, CamDVR{SiteID: siteB, Name: "DVR B", Brand: "hikvision", Host: "192.0.2.12", Enabled: true})
	camEnabled, _ := insertCamera(db, camera{SiteID: siteA, DVRID: dvrA, Channel: 1, Name: "Cam A", Enabled: true})
	camDisabled, _ := insertCamera(db, camera{SiteID: siteA, DVRID: dvrDisabled, Channel: 1, Name: "Cam A-off", Enabled: true})

	// Health lists only this site's DVRs and omits the server-local `bin` paths.
	code, out, _ := svcCall(t, srv, http.MethodGet, "/api/cameras/health", tokA, nil)
	if code != http.StatusOK {
		t.Fatalf("health: %d %v", code, out)
	}
	if _, has := out["ffmpeg"].(map[string]any)["bin"]; has {
		t.Errorf("health ffmpeg leaked a bin path: %v", out["ffmpeg"])
	}
	if _, has := out["ffprobe"].(map[string]any)["bin"]; has {
		t.Errorf("health ffprobe leaked a bin path: %v", out["ffprobe"])
	}
	dvrs, _ := out["dvrs"].([]any)
	for _, d := range dvrs {
		m := d.(map[string]any)
		if m["site_id"] != siteA {
			t.Errorf("health listed a foreign DVR: %v", m)
		}
		if m["dvr_id"] == dvrB {
			t.Errorf("health leaked site-B DVR %s", dvrB)
		}
	}

	// Events: seed one site-A event, one site-B event, one globally-unreferenced
	// event; the site token sees only the site-A one.
	insEvent := func(op, dvrID string) {
		if _, err := db.Exec(`INSERT INTO camera_events
			(id, ts, level, dvr_id, camera_id, watch_id, run_id, op, ok, latency_ms, detail, error)
			VALUES (?, ?, 'info', ?, '', '', '', ?, 1, 0, '', '')`, randomID("cev"), nowRFC3339(), dvrID, op); err != nil {
			t.Fatalf("insert event %s: %v", op, err)
		}
	}
	insEvent("op_a", dvrA)
	insEvent("op_b", dvrB)
	insEvent("op_global", "")

	code, out, _ = svcCall(t, srv, http.MethodGet, "/api/cameras/events", tokA, nil)
	if code != http.StatusOK {
		t.Fatalf("events: %d %v", code, out)
	}
	events, _ := out["events"].([]any)
	hasOp := func(op string) bool {
		for _, e := range events {
			if m, _ := e.(map[string]any); m["op"] == op {
				return true
			}
		}
		return false
	}
	if !hasOp("op_a") {
		t.Errorf("events missing own site event op_a: %v", events)
	}
	if hasOp("op_b") {
		t.Errorf("events leaked foreign-site event op_b")
	}
	if hasOp("op_global") {
		t.Errorf("events leaked globally-unreferenced event op_global")
	}
	// A malformed ok filter is a 400.
	code, out, _ = svcCall(t, srv, http.MethodGet, "/api/cameras/events?ok=maybe", tokA, nil)
	if code != http.StatusBadRequest {
		t.Errorf("bad ok filter: status = %d %v, want 400", code, out)
	}

	// Recapture on a disabled DVR is a graceful {ok:false} (pre-ffmpeg).
	code, out, _ = svcCall(t, srv, http.MethodPost, "/api/cameras/recapture", tokA, map[string]any{"camera_id": camDisabled})
	if code != http.StatusOK || out["ok"] != false {
		t.Errorf("recapture disabled DVR: %d %v, want ok=false", code, out)
	}
	// A bad from/to window is a 400 before any capture attempt (enabled DVR).
	code, out, _ = svcCall(t, srv, http.MethodPost, "/api/cameras/recapture", tokA, map[string]any{
		"camera_id": camEnabled, "kind": "clip", "from": "not-a-date", "to": "also-bad",
	})
	if code != http.StatusBadRequest {
		t.Errorf("recapture bad from/to: status = %d %v, want 400", code, out)
	}
}

// ─────────────────────────── 9. investigation children ───────────────────────────

func TestSvcInvestigationChildren(t *testing.T) {
	cfg := newSvcTestConfig(t)
	db := svcTestDB(t, cfg)
	srv := newSvcTestServer(t, cfg)

	siteA, tokA := svcSeedSite(t, db, "Site A")

	parent, _ := insertCameraInvestigation(db, camInvestigation{SiteID: siteA, Title: "lead", Question: "who entered?", Status: "answered"})
	child, _ := insertCameraInvestigation(db, camInvestigation{SiteID: siteA, Title: "sub", Question: "gate?", Status: "answered", ParentID: parent})
	childMsg := camAppendInvestigateMessage(db, child, "tool", "snapshot ok", "snapshot", "", 0,
		[]evidenceItem{{MediaURL: "https://x.example/camera/media/childtok", Caption: "gate"}})

	code, out, _ := svcCall(t, srv, http.MethodGet, "/api/cameras/investigations/get?id="+parent, tokA, nil)
	if code != http.StatusOK {
		t.Fatalf("investigation get: %d %v", code, out)
	}
	children, _ := out["children"].([]any)
	if len(children) != 1 {
		t.Fatalf("children = %d, want 1", len(children))
	}
	cmsgs, _ := children[0].(map[string]any)["messages"].([]any)
	if len(cmsgs) != 1 {
		t.Fatalf("child messages = %d, want 1", len(cmsgs))
	}
	cmedia, _ := cmsgs[0].(map[string]any)["media"].([]any)
	if len(cmedia) != 1 || cmedia[0].(map[string]any)["token"] != "childtok" {
		t.Errorf("child media not token-augmented: %v", cmsgs[0])
	}

	// The admin mapper for the SAME message is byte-identical to before — its
	// media items never gain a bare token (only the service view augments them).
	adminMedia, _ := investigateMessageJSON(childMsg)["media"].([]any)
	if len(adminMedia) != 1 {
		t.Fatalf("admin media = %d, want 1", len(adminMedia))
	}
	if _, hasTok := adminMedia[0].(map[string]any)["token"]; hasTok {
		t.Errorf("admin message media must NOT carry a token: %v", adminMedia[0])
	}
}

// ─────────────────────────── 10. investigation cancel ───────────────────────────

func TestSvcInvestigationCancel(t *testing.T) {
	cfg := newSvcTestConfig(t)
	db := svcTestDB(t, cfg)
	srv := newSvcTestServer(t, cfg)

	siteA, tokA := svcSeedSite(t, db, "Site A")
	siteB, _ := svcSeedSite(t, db, "Site B")

	queued, _ := insertCameraInvestigation(db, camInvestigation{SiteID: siteA, Title: "q", Question: "q", Status: "queued"})
	answered, _ := insertCameraInvestigation(db, camInvestigation{SiteID: siteA, Title: "a", Question: "a", Status: "answered"})
	running, _ := insertCameraInvestigation(db, camInvestigation{SiteID: siteA, Title: "r", Question: "r", Status: "running"})
	foreign, _ := insertCameraInvestigation(db, camInvestigation{SiteID: siteB, Title: "b", Question: "b", Status: "queued"})

	// Queued → closed, with a system transcript note and no way to reply after.
	code, out, _ := svcCall(t, srv, http.MethodPost, "/api/cameras/investigations/cancel", tokA, map[string]any{"id": queued})
	if code != http.StatusOK || out["ok"] != true || out["status"] != "closed" {
		t.Fatalf("cancel queued: %d %v", code, out)
	}
	if inv, _ := getCameraInvestigation(db, queued); inv.Status != "closed" {
		t.Errorf("queued investigation status = %q, want closed", inv.Status)
	}
	msgs, _ := listCameraInvestigationMessages(db, queued)
	sawNote := false
	for _, m := range msgs {
		if m.Role == "system" && m.Content == "closed by service API" {
			sawNote = true
		}
	}
	if !sawNote {
		t.Errorf("cancel did not append the system close note: %v", msgs)
	}
	// A later reply to the closed row is refused.
	code, out, _ = svcCall(t, srv, http.MethodPost, "/api/cameras/investigations/reply", tokA, map[string]any{"id": queued, "message": "hi"})
	if code != http.StatusOK || out["ok"] != false {
		t.Errorf("reply after close: %d %v, want ok=false", code, out)
	}

	// Idempotent: cancelling an already-closed row is a clean ok=true.
	code, out, _ = svcCall(t, srv, http.MethodPost, "/api/cameras/investigations/cancel", tokA, map[string]any{"id": queued})
	if code != http.StatusOK || out["ok"] != true || out["status"] != "closed" {
		t.Errorf("idempotent cancel: %d %v", code, out)
	}

	// A settled (answered) row also closes via the CAS.
	code, out, _ = svcCall(t, srv, http.MethodPost, "/api/cameras/investigations/cancel", tokA, map[string]any{"id": answered})
	if code != http.StatusOK || out["ok"] != true || out["status"] != "closed" {
		t.Errorf("cancel answered: %d %v", code, out)
	}

	// A running pass is refused (cooperative cancel is out of scope) and left running.
	code, out, _ = svcCall(t, srv, http.MethodPost, "/api/cameras/investigations/cancel", tokA, map[string]any{"id": running})
	if code != http.StatusOK || out["ok"] != false {
		t.Errorf("cancel running: %d %v, want ok=false", code, out)
	}
	if inv, _ := getCameraInvestigation(db, running); inv.Status != "running" {
		t.Errorf("running investigation was mutated to %q", inv.Status)
	}

	// A cross-site id is the uniform 404.
	code, out, _ = svcCall(t, srv, http.MethodPost, "/api/cameras/investigations/cancel", tokA, map[string]any{"id": foreign})
	if code != http.StatusNotFound || out["error"] != "not found" {
		t.Errorf("cancel foreign: %d %v, want 404 not found", code, out)
	}
}

// ─────────────────────────── 11. camera list snapshot thumbnails ───────────────────────────

// TestSvcCamerasListSnapshotToken proves the svc camera list carries each
// camera's current snapshot_token — the same thumbnail-capability token the
// ADMIN cameraJSON derives via camSnapshotToken (camerahttp.go). A camera with
// no snapshot pointer reports an empty token, and a FOREIGN site's snapshot
// token never leaks into another site's list (the list is site-scoped; the
// tokens themselves are only redeemed later via /api/cameras/media, which
// site-checks).
func TestSvcCamerasListSnapshotToken(t *testing.T) {
	cfg := newSvcTestConfig(t)
	db := svcTestDB(t, cfg)
	srv := newSvcTestServer(t, cfg)

	siteA, tokA := svcSeedSite(t, db, "Site A")
	siteB, tokB := svcSeedSite(t, db, "Site B")

	dvrA, err := insertCameraDVR(db, cfg, CamDVR{SiteID: siteA, Name: "DVR A", Brand: "hikvision", Host: "192.0.2.20", Enabled: true})
	if err != nil {
		t.Fatalf("insertCameraDVR A: %v", err)
	}
	dvrB, err := insertCameraDVR(db, cfg, CamDVR{SiteID: siteB, Name: "DVR B", Brand: "hikvision", Host: "192.0.2.21", Enabled: true})
	if err != nil {
		t.Fatalf("insertCameraDVR B: %v", err)
	}

	// Site A: one camera WITH a snapshot pointer, one WITHOUT.
	camWithSnap, err := insertCamera(db, camera{SiteID: siteA, DVRID: dvrA, Channel: 1, Name: "Lobby", Enabled: true})
	if err != nil {
		t.Fatalf("insertCamera withSnap: %v", err)
	}
	camNoSnap, err := insertCamera(db, camera{SiteID: siteA, DVRID: dvrA, Channel: 2, Name: "Hallway", Enabled: true})
	if err != nil {
		t.Fatalf("insertCamera noSnap: %v", err)
	}
	_ = camNoSnap // referenced only by its rendered row below
	capA, err := insertCameraCapture(db, camCapture{SiteID: siteA, CameraID: camWithSnap, Kind: "snapshot", Token: "snapatok1", ContentType: "image/jpeg"})
	if err != nil {
		t.Fatalf("insert capA: %v", err)
	}
	if err := setCameraSnapshot(db, camWithSnap, capA); err != nil {
		t.Fatalf("setCameraSnapshot A: %v", err)
	}

	// Site B: a camera whose snapshot token must NEVER surface under site A.
	camB, err := insertCamera(db, camera{SiteID: siteB, DVRID: dvrB, Channel: 1, Name: "Cam B", Enabled: true})
	if err != nil {
		t.Fatalf("insertCamera B: %v", err)
	}
	capB, err := insertCameraCapture(db, camCapture{SiteID: siteB, CameraID: camB, Kind: "snapshot", Token: "snapbtok9", ContentType: "image/jpeg"})
	if err != nil {
		t.Fatalf("insert capB: %v", err)
	}
	if err := setCameraSnapshot(db, camB, capB); err != nil {
		t.Fatalf("setCameraSnapshot B: %v", err)
	}

	// Site A's list carries snapatok1 on the pointed camera, "" on the other, and
	// never mentions site B's snapbtok9.
	code, out, raw := svcCall(t, srv, http.MethodGet, "/api/cameras/list?dvr_id="+dvrA, tokA, nil)
	if code != http.StatusOK {
		t.Fatalf("cameras list A: %d %v", code, out)
	}
	cams, _ := out["cameras"].([]any)
	if len(cams) != 2 {
		t.Fatalf("site A cameras = %d, want 2", len(cams))
	}
	tokens := map[string]string{}
	for _, c := range cams {
		m := c.(map[string]any)
		name, _ := m["name"].(string)
		st, _ := m["snapshot_token"].(string)
		tokens[name] = st
	}
	if tokens["Lobby"] != "snapatok1" {
		t.Errorf("Lobby snapshot_token = %q, want snapatok1", tokens["Lobby"])
	}
	if tokens["Hallway"] != "" {
		t.Errorf("Hallway (no capture) snapshot_token = %q, want empty", tokens["Hallway"])
	}
	if strings.Contains(string(raw), "snapbtok9") {
		t.Errorf("site A list leaked site B snapshot token: %s", raw)
	}

	// Site B's own list carries snapbtok9 — proving the token exists and is only
	// withheld cross-site, not globally dropped.
	code, out, _ = svcCall(t, srv, http.MethodGet, "/api/cameras/list?dvr_id="+dvrB, tokB, nil)
	if code != http.StatusOK {
		t.Fatalf("cameras list B: %d %v", code, out)
	}
	bcams, _ := out["cameras"].([]any)
	if len(bcams) != 1 || bcams[0].(map[string]any)["snapshot_token"] != "snapbtok9" {
		t.Errorf("site B list snapshot_token wrong: %v", bcams)
	}
}
