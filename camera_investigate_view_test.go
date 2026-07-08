package main

// camera_investigate_view_test.go (WS4) — the public timeline surface: the
// view-token media auth matrix (valid / not-minted / cross-investigation / wrong
// view-token), the state.json shape (gated relative media URLs + expiry flag), and
// the progress notifier (opt-out silence, nil-safety, key-tool filtering, and the
// coalesced "media" flush). Temp sqlite + httptest only — no DVRs, no model calls.

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newCamViewTestConfig builds a config whose DB + media root live in t.TempDir(),
// with the callback SSRF guard relaxed so a loopback httptest receiver is reachable.
func newCamViewTestConfig(t *testing.T) config {
	t.Helper()
	dir := t.TempDir()
	cfg := config{
		DBPath:                     filepath.Join(dir, ".proxy.db"),
		CameraMediaDir:             filepath.Join(dir, "media"),
		CameraCallbackAllowPrivate: true,
		// Mirror production (PROXY_CAMERA_INVESTIGATE_WORKER defaults true): with
		// the worker "enabled" (no worker actually runs in tests) the reply paths
		// enqueue only — the worker-disabled inline drain has its own test.
		CameraInvestigateWorkerEnabled: true,
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

// seedCamViewInvestigation inserts a settled investigation that minted one on-disk
// capture, returning the view_token that authorizes its page and the media token it
// cited.
func seedCamViewInvestigation(t *testing.T, cfg config, siteID, parentID, question string) (invID, viewToken, mediaToken string) {
	t.Helper()
	db, err := openProxyDB(cfg)
	if err != nil {
		t.Fatalf("openProxyDB: %v", err)
	}
	defer db.Close()
	invID, err = insertCameraInvestigation(db, camInvestigation{SiteID: siteID, ParentID: parentID, Question: question, Status: "answered"})
	if err != nil {
		t.Fatalf("insert investigation: %v", err)
	}
	inv, _ := getCameraInvestigation(db, invID)
	viewToken = inv.ViewToken

	mediaToken = randToken(16)
	path := filepath.Join(cameraMediaRoot(cfg), mediaToken+".jpg")
	if err := os.WriteFile(path, []byte("JPEGDATA"), 0o600); err != nil {
		t.Fatalf("write capture file: %v", err)
	}
	if _, err := insertCameraCapture(db, camCapture{
		SiteID: siteID, CameraID: "cam1", Kind: "snapshot", Quality: "sub",
		Token: mediaToken, Path: path, ContentType: "image/jpeg",
	}); err != nil {
		t.Fatalf("insert capture: %v", err)
	}
	camAppendInvestigateMessage(db, invID, "operator", question, "", "", 0, nil)
	camAppendInvestigateMessage(db, invID, "tool", "snapshot ok", "snapshot", "", 1,
		[]evidenceItem{{MediaURL: camMediaURL(cfg, nil, mediaToken), Caption: "gate"}})
	return invID, viewToken, mediaToken
}

func newCamViewTestServer(t *testing.T, cfg config) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/camera/media/", noStore(handleCameraMediaGated(cfg)))
	mux.HandleFunc("/camera/investigations/", noStore(handleCameraInvestigationView(cfg)))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func camViewGet(t *testing.T, srv *httptest.Server, path string) (int, []byte) {
	t.Helper()
	resp, err := srv.Client().Get(srv.URL + path)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, body
}

// TestCameraViewTokenMediaMatrix is the authorization matrix for ?v= media fetches:
// a view_token authorizes exactly the media its own investigation minted, and
// nothing else.
func TestCameraViewTokenMediaMatrix(t *testing.T) {
	cfg := newCamViewTestConfig(t)
	srv := newCamViewTestServer(t, cfg)

	_, viewA, mediaA := seedCamViewInvestigation(t, cfg, "site_1", "", "Did the courier arrive?")
	_, viewB, mediaB := seedCamViewInvestigation(t, cfg, "site_1", "", "Who used the back door?")

	// Valid: A's token serves A's media.
	if code, body := camViewGet(t, srv, "/camera/media/"+mediaA+"?v="+viewA); code != http.StatusOK || string(body) != "JPEGDATA" {
		t.Errorf("A's own media: %d %q, want 200 JPEGDATA", code, body)
	}
	// Cross-investigation: A's token must NOT serve B's media.
	if code, _ := camViewGet(t, srv, "/camera/media/"+mediaB+"?v="+viewA); code != http.StatusNotFound {
		t.Errorf("cross-investigation media: %d, want 404", code)
	}
	// A never-minted token 404s even with a valid view_token.
	if code, _ := camViewGet(t, srv, "/camera/media/"+randToken(16)+"?v="+viewA); code != http.StatusNotFound {
		t.Errorf("unminted media: %d, want 404", code)
	}
	// A wrong/unknown view_token 404s even for a real media token.
	if code, _ := camViewGet(t, srv, "/camera/media/"+mediaB+"?v="+randToken(16)); code != http.StatusNotFound {
		t.Errorf("unknown view_token: %d, want 404", code)
	}
	// Sanity: B's own pairing still works (proves the matrix isn't just failing).
	if code, _ := camViewGet(t, srv, "/camera/media/"+mediaB+"?v="+viewB); code != http.StatusOK {
		t.Errorf("B's own media: %d, want 200", code)
	}
}

// TestCameraViewStateJSON checks the poll payload: identity fields, the gated
// relative media URL, and the not-expired flag for a live capture; plus a hardened
// header and a uniform 404 for an unknown token.
func TestCameraViewStateJSON(t *testing.T) {
	cfg := newCamViewTestConfig(t)
	srv := newCamViewTestServer(t, cfg)
	_, viewA, mediaA := seedCamViewInvestigation(t, cfg, "site_1", "", "Did the courier arrive?")

	code, body := camViewGet(t, srv, "/camera/investigations/"+viewA+"/state.json")
	if code != http.StatusOK {
		t.Fatalf("state.json: %d %s", code, body)
	}
	var st map[string]any
	if err := json.Unmarshal(body, &st); err != nil {
		t.Fatalf("decode state.json: %v", err)
	}
	if st["status"] != "answered" || st["question"] != "Did the courier arrive?" {
		t.Errorf("state identity = %v", st)
	}
	turns, _ := st["turns"].([]any)
	if len(turns) != 2 {
		t.Fatalf("turns = %v, want operator + tool", st["turns"])
	}
	tool, _ := turns[1].(map[string]any)
	media, _ := tool["media"].([]any)
	if len(media) != 1 {
		t.Fatalf("tool media = %v, want one item", tool["media"])
	}
	m0, _ := media[0].(map[string]any)
	if m0["url"] != "/camera/media/"+mediaA+"?v="+viewA {
		t.Errorf("media url = %v, want the gated relative form", m0["url"])
	}
	if m0["content_type"] != "image/jpeg" || m0["expired"] != false {
		t.Errorf("media enrichment = %v, want image/jpeg not-expired", m0)
	}

	// The HTML shell is served for a well-formed token with hardening headers.
	resp, err := srv.Client().Get(srv.URL + "/camera/investigations/" + viewA)
	if err != nil {
		t.Fatalf("GET shell: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("shell status = %d, want 200", resp.StatusCode)
	}
	if got := resp.Header.Get("X-Robots-Tag"); got != "noindex, nofollow" {
		t.Errorf("X-Robots-Tag = %q", got)
	}
	if csp := resp.Header.Get("Content-Security-Policy"); csp == "" {
		t.Error("state view must set a Content-Security-Policy")
	}

	// Unknown token → uniform 404 on the JSON path.
	if code, _ := camViewGet(t, srv, "/camera/investigations/"+randToken(16)+"/state.json"); code != http.StatusNotFound {
		t.Errorf("unknown token state.json: %d, want 404", code)
	}
}

// TestCameraViewChildMediaAuthorized proves a delegated child's media is reachable
// through the PARENT's view_token (the page authorizes the parent∪children union)
// and that state.json nests the child.
func TestCameraViewChildMediaAuthorized(t *testing.T) {
	cfg := newCamViewTestConfig(t)
	srv := newCamViewTestServer(t, cfg)
	parentID, viewP, mediaP := seedCamViewInvestigation(t, cfg, "site_1", "", "lead question")
	_, _, mediaC := seedCamViewInvestigation(t, cfg, "site_1", parentID, "sub question")

	// Both the parent's own media and its child's media serve under viewP.
	if code, _ := camViewGet(t, srv, "/camera/media/"+mediaP+"?v="+viewP); code != http.StatusOK {
		t.Errorf("parent media via parent token: %d, want 200", code)
	}
	if code, _ := camViewGet(t, srv, "/camera/media/"+mediaC+"?v="+viewP); code != http.StatusOK {
		t.Errorf("child media via parent token: %d, want 200", code)
	}

	code, body := camViewGet(t, srv, "/camera/investigations/"+viewP+"/state.json")
	if code != http.StatusOK {
		t.Fatalf("state.json: %d %s", code, body)
	}
	var st map[string]any
	_ = json.Unmarshal(body, &st)
	children, _ := st["children"].([]any)
	if len(children) != 1 {
		t.Fatalf("children = %v, want one nested sub-investigation", st["children"])
	}
}

// ─────────────────────────── progress notifier ───────────────────────────

// TestCamProgressNotifierOptOutAndNilSafe covers the two silence guarantees: a site
// that did NOT opt into stream_progress yields a nil notifier, and every method on a
// nil notifier is a no-op (never panics).
func TestCamProgressNotifierOptOutAndNilSafe(t *testing.T) {
	cfg := newCamViewTestConfig(t)
	db, err := openProxyDB(cfg)
	if err != nil {
		t.Fatalf("openProxyDB: %v", err)
	}
	defer db.Close()

	// A callback WITHOUT stream_progress must not produce a notifier.
	if err := setCameraSiteCallback(db, cfg, "site_1", "https://connect.example/cb", "", false); err != nil {
		t.Fatalf("setCameraSiteCallback: %v", err)
	}
	inv := camInvestigation{ID: "inv_1", SiteID: "site_1", ViewToken: "vt", Question: "q"}
	if n := newCamProgressNotifier(cfg, db, inv, nil); n != nil {
		t.Error("opt-out site must yield a nil notifier")
	}
	// No callback at all → also nil.
	if n := newCamProgressNotifier(cfg, db, camInvestigation{ID: "x", SiteID: "no_site"}, nil); n != nil {
		t.Error("site without a callback must yield a nil notifier")
	}

	// Nil-receiver methods must all be safe.
	var nilN *camProgressNotifier
	nilN.Start("q")
	nilN.ObserveMedia("inv_1", "annotate", []evidenceItem{{MediaURL: "u"}})
	nilN.NotifySubagents([]camSubagentSummary{{ID: "c"}})
	nilN.Flush(true)
}

// TestCamProgressNotifierCoalescing drives a live notifier against a loopback
// receiver: it emits "started" once, filters non-key tools, and coalesces a burst of
// key-tool media into a SINGLE delayed "media" event carrying every item.
func TestCamProgressNotifierCoalescing(t *testing.T) {
	cfg := newCamViewTestConfig(t)
	cfg.CameraProgressMinInterval = 80 * time.Millisecond

	events := make(chan map[string]any, 16)
	rcv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var m map[string]any
		_ = json.NewDecoder(r.Body).Decode(&m)
		events <- m
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(rcv.Close)

	db, err := openProxyDB(cfg)
	if err != nil {
		t.Fatalf("openProxyDB: %v", err)
	}
	defer db.Close()
	if err := setCameraSiteCallback(db, cfg, "site_1", rcv.URL, "", true); err != nil {
		t.Fatalf("setCameraSiteCallback: %v", err)
	}
	inv := camInvestigation{ID: "inv_1", SiteID: "site_1", ViewToken: "vt", Question: "who entered?"}
	n := newCamProgressNotifier(cfg, db, inv, nil)
	if n == nil {
		t.Fatal("opted-in site must yield a live notifier")
	}

	n.Start(inv.Question)
	// A burst of key-tool media inside the interval, plus one NON-key tool that must
	// be ignored — all coalesce into one delayed "media" event with two items.
	n.ObserveMedia("inv_1", "annotate", []evidenceItem{{MediaURL: camMediaURL(cfg, nil, "tokA"), Caption: "A"}})
	n.ObserveMedia("inv_1", "avatar_check", []evidenceItem{{MediaURL: camMediaURL(cfg, nil, "tokB"), Caption: "B"}})
	n.ObserveMedia("inv_1", "snapshot", []evidenceItem{{MediaURL: camMediaURL(cfg, nil, "tokC"), Caption: "C"}})

	started := waitProgressEvent(t, events, "started")
	if started["question"] != "who entered?" || started["view_url"] == nil {
		t.Errorf("started event = %v, want question + view_url", started)
	}
	media := waitProgressEvent(t, events, "media")
	ev, _ := media["evidence"].([]any)
	if len(ev) != 2 {
		t.Fatalf("coalesced media evidence = %v, want exactly the two key-tool items", media["evidence"])
	}

	// No further event (the non-key snapshot buffered nothing).
	select {
	case extra := <-events:
		t.Errorf("unexpected extra event: %v", extra)
	case <-time.After(200 * time.Millisecond):
	}
}

// waitProgressEvent reads the next event and asserts its phase, failing on timeout.
func waitProgressEvent(t *testing.T, ch chan map[string]any, phase string) map[string]any {
	t.Helper()
	select {
	case m := <-ch:
		if m["event"] != "camera_investigation_progress" || m["phase"] != phase {
			t.Fatalf("event = %v, want phase %q", m, phase)
		}
		return m
	case <-time.After(3 * time.Second):
		t.Fatalf("timed out waiting for phase %q", phase)
		return nil
	}
}

// ─────────────────────────── reply endpoint ───────────────────────────

// camResetSvcRateLimiter clears the process-wide token buckets so each reply
// test starts with a full budget — the reply ceiling is only 6/min per IP and
// the limiter is a package global shared across the whole test binary.
func camResetSvcRateLimiter() {
	camSvcRL.Lock()
	camSvcRL.buckets = map[string]*camSvcBucket{}
	camSvcRL.Unlock()
}

// camViewReplyPost POSTs a reply message to the public timeline endpoint.
func camViewReplyPost(t *testing.T, srv *httptest.Server, token, message string) (int, map[string]any) {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"message": message})
	resp, err := srv.Client().Post(srv.URL+"/camera/investigations/"+token+"/reply",
		"application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST reply: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return resp.StatusCode, out
}

// TestCameraViewReplyMatrix walks the reply endpoint's behavior-by-status table
// plus the uniform-404 and POST-only rules.
func TestCameraViewReplyMatrix(t *testing.T) {
	camResetSvcRateLimiter()
	cfg := newCamViewTestConfig(t)
	srv := newCamViewTestServer(t, cfg)
	db, err := openProxyDB(cfg)
	if err != nil {
		t.Fatalf("openProxyDB: %v", err)
	}
	defer db.Close()

	// Unknown token → uniform 404.
	if code, _ := camViewReplyPost(t, srv, randToken(16), "hello"); code != http.StatusNotFound {
		t.Errorf("unknown token reply: %d, want 404", code)
	}

	// GET on /reply → 404 (the write surface is POST-only).
	invID, viewTok, _ := seedCamViewInvestigation(t, cfg, "site_1", "", "settled question")
	if code, _ := camViewGet(t, srv, "/camera/investigations/"+viewTok+"/reply"); code != http.StatusNotFound {
		t.Errorf("GET on /reply: %d, want 404", code)
	}

	// Closed → ok:false (200, mirroring the admin reply handler's shape).
	if err := setCameraInvestigationStatus(db, invID, "closed"); err != nil {
		t.Fatalf("set closed: %v", err)
	}
	code, out := camViewReplyPost(t, srv, viewTok, "anyone there?")
	if code != http.StatusOK || out["ok"] != false || out["error"] != "investigation is closed" {
		t.Errorf("closed reply: %d %v, want ok=false closed error", code, out)
	}

	// Settled (answered): the operator row is appended FIRST, then the status
	// flips to queued — both observable after the call (append-before-queue).
	if err := setCameraInvestigationStatus(db, invID, "answered"); err != nil {
		t.Fatalf("set answered: %v", err)
	}
	before, _ := listCameraInvestigationMessages(db, invID)
	code, out = camViewReplyPost(t, srv, viewTok, "follow-up: which door?")
	if code != http.StatusOK || out["ok"] != true || out["status"] != "queued" {
		t.Fatalf("settled reply: %d %v, want ok=true status=queued", code, out)
	}
	after, _ := listCameraInvestigationMessages(db, invID)
	if len(after) != len(before)+1 {
		t.Fatalf("settled reply transcript rows = %d, want %d", len(after), len(before)+1)
	}
	lastMsg := after[len(after)-1]
	if lastMsg.Role != "operator" || lastMsg.Content != "follow-up: which door?" {
		t.Errorf("appended row = %+v, want the operator follow-up", lastMsg)
	}
	if inv, _ := getCameraInvestigation(db, invID); inv.Status != "queued" {
		t.Errorf("status after settled reply = %q, want queued", inv.Status)
	}
	if pend, _ := listCameraInvestigationPendingMessages(db, invID); len(pend) != 0 {
		t.Errorf("settled reply must not park pending rows: %+v", pend)
	}

	// Running → pending row inserted, transcript UNCHANGED.
	if err := setCameraInvestigationStatus(db, invID, "running"); err != nil {
		t.Fatalf("set running: %v", err)
	}
	before, _ = listCameraInvestigationMessages(db, invID)
	code, out = camViewReplyPost(t, srv, viewTok, "also check the yard")
	if code != http.StatusOK || out["ok"] != true || out["queued"] != true || out["status"] != "running" {
		t.Fatalf("running reply: %d %v, want ok=true queued=true status=running", code, out)
	}
	after, _ = listCameraInvestigationMessages(db, invID)
	if len(after) != len(before) {
		t.Errorf("running reply transcript rows = %d, want unchanged %d", len(after), len(before))
	}
	pend, _ := listCameraInvestigationPendingMessages(db, invID)
	if len(pend) != 1 || pend[0].Message != "also check the yard" {
		t.Errorf("pending rows = %+v, want the one parked message", pend)
	}
}

// TestCameraViewReplyRateLimit proves the tight per-IP reply ceiling: the 7th
// POST inside a minute is 429'd.
func TestCameraViewReplyRateLimit(t *testing.T) {
	camResetSvcRateLimiter()
	cfg := newCamViewTestConfig(t)
	srv := newCamViewTestServer(t, cfg)
	db, err := openProxyDB(cfg)
	if err != nil {
		t.Fatalf("openProxyDB: %v", err)
	}
	defer db.Close()
	invID, viewTok, _ := seedCamViewInvestigation(t, cfg, "site_1", "", "rate limited?")
	if err := setCameraInvestigationStatus(db, invID, "running"); err != nil {
		t.Fatalf("set running: %v", err)
	}
	for i := 0; i < 6; i++ {
		if code, out := camViewReplyPost(t, srv, viewTok, "ping"); code != http.StatusOK {
			t.Fatalf("reply %d: %d %v, want 200", i+1, code, out)
		}
	}
	if code, _ := camViewReplyPost(t, srv, viewTok, "ping"); code != http.StatusTooManyRequests {
		t.Errorf("7th reply: %d, want 429", code)
	}
}

// TestCameraViewReplyKillSwitch: with PROXY_CAM_VIEW_REPLY_DISABLED the endpoint
// 404s (indistinguishable from absent) and state.json reports can_reply=false so
// the shell hides the composer.
func TestCameraViewReplyKillSwitch(t *testing.T) {
	camResetSvcRateLimiter()
	cfg := newCamViewTestConfig(t)
	cfg.CameraViewReplyDisabled = true
	srv := newCamViewTestServer(t, cfg)
	_, viewTok, _ := seedCamViewInvestigation(t, cfg, "site_1", "", "switched off")

	if code, _ := camViewReplyPost(t, srv, viewTok, "hello?"); code != http.StatusNotFound {
		t.Errorf("kill-switch reply: %d, want 404", code)
	}
	code, body := camViewGet(t, srv, "/camera/investigations/"+viewTok+"/state.json")
	if code != http.StatusOK {
		t.Fatalf("state.json: %d %s", code, body)
	}
	var st map[string]any
	_ = json.Unmarshal(body, &st)
	if st["can_reply"] != false {
		t.Errorf("can_reply = %v, want false under the kill switch", st["can_reply"])
	}
}

// TestCameraViewReplyTruncatesRunes: an over-long message is capped at 2000
// RUNES server-side — rune-safe for Arabic, never a byte slice that could split
// a multibyte character.
func TestCameraViewReplyTruncatesRunes(t *testing.T) {
	camResetSvcRateLimiter()
	cfg := newCamViewTestConfig(t)
	srv := newCamViewTestServer(t, cfg)
	db, err := openProxyDB(cfg)
	if err != nil {
		t.Fatalf("openProxyDB: %v", err)
	}
	defer db.Close()
	invID, viewTok, _ := seedCamViewInvestigation(t, cfg, "site_1", "", "طويل جدا")
	if err := setCameraInvestigationStatus(db, invID, "running"); err != nil {
		t.Fatalf("set running: %v", err)
	}

	long := strings.Repeat("مرحبا", 500) // 2500 Arabic runes (5000 bytes)
	code, out := camViewReplyPost(t, srv, viewTok, long)
	if code != http.StatusOK || out["ok"] != true {
		t.Fatalf("long reply: %d %v", code, out)
	}
	pend, _ := listCameraInvestigationPendingMessages(db, invID)
	if len(pend) != 1 {
		t.Fatalf("pending rows = %d, want 1", len(pend))
	}
	got := []rune(pend[0].Message)
	if len(got) != 2000 {
		t.Errorf("stored message runes = %d, want exactly 2000", len(got))
	}
	if string(got) != string([]rune(long)[:2000]) {
		t.Error("stored message is not the rune-safe prefix of the original")
	}
}

// TestCameraViewReplyBodyCap: the reply body is byte-capped (http.MaxBytesReader)
// BEFORE it is decoded, so a huge JSON string can never buffer into RAM — the
// endpoint answers 400 and parks nothing.
func TestCameraViewReplyBodyCap(t *testing.T) {
	camResetSvcRateLimiter()
	cfg := newCamViewTestConfig(t)
	srv := newCamViewTestServer(t, cfg)
	db, err := openProxyDB(cfg)
	if err != nil {
		t.Fatalf("openProxyDB: %v", err)
	}
	defer db.Close()
	invID, viewTok, _ := seedCamViewInvestigation(t, cfg, "site_1", "", "big body")
	if err := setCameraInvestigationStatus(db, invID, "running"); err != nil {
		t.Fatalf("set running: %v", err)
	}

	huge := `{"message":"` + strings.Repeat("a", 64<<10) + `"}` // 64KB > camViewReplyMaxBody
	resp, err := srv.Client().Post(srv.URL+"/camera/investigations/"+viewTok+"/reply",
		"application/json", strings.NewReader(huge))
	if err != nil {
		t.Fatalf("POST reply: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("oversized body: %d, want 400", resp.StatusCode)
	}
	if pend, _ := listCameraInvestigationPendingMessages(db, invID); len(pend) != 0 {
		t.Errorf("oversized body must park nothing: %+v", pend)
	}
}

// TestCameraViewReplyGenericInternalError: a storage failure on the PUBLIC
// surface answers a generic {"error":"internal error"} — never the raw engine/
// schema text the authenticated handlers may echo.
func TestCameraViewReplyGenericInternalError(t *testing.T) {
	camResetSvcRateLimiter()
	cfg := newCamViewTestConfig(t)
	srv := newCamViewTestServer(t, cfg)
	db, err := openProxyDB(cfg)
	if err != nil {
		t.Fatalf("openProxyDB: %v", err)
	}
	defer db.Close()
	invID, viewTok, _ := seedCamViewInvestigation(t, cfg, "site_1", "", "leaky errors?")
	if err := setCameraInvestigationStatus(db, invID, "running"); err != nil {
		t.Fatalf("set running: %v", err)
	}
	// Force an internal failure the handler must not narrate.
	if _, err := db.Exec(`DROP TABLE camera_investigation_pending_messages`); err != nil {
		t.Fatalf("drop pending table: %v", err)
	}

	body, _ := json.Marshal(map[string]string{"message": "hello"})
	resp, err := srv.Client().Post(srv.URL+"/camera/investigations/"+viewTok+"/reply",
		"application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST reply: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d %s, want 500", resp.StatusCode, raw)
	}
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	if out["error"] != "internal error" {
		t.Errorf("error = %v, want the generic string", out["error"])
	}
	for _, leak := range []string{"sqlite", "SQL", "no such table", "camera_investigation_pending_messages"} {
		if strings.Contains(string(raw), leak) {
			t.Errorf("500 body leaks internals (%q): %s", leak, raw)
		}
	}
}

// TestCameraViewReplyChildRefused: a delegated sub-investigation's view token
// must not accept replies — a child has no drain point (it runs in-process
// inside the lead's delegate turn), so an accepted message would strand forever.
// Its state.json also reports can_reply=false so the shell hides the composer.
func TestCameraViewReplyChildRefused(t *testing.T) {
	camResetSvcRateLimiter()
	cfg := newCamViewTestConfig(t)
	srv := newCamViewTestServer(t, cfg)
	db, err := openProxyDB(cfg)
	if err != nil {
		t.Fatalf("openProxyDB: %v", err)
	}
	defer db.Close()
	parentID, _, _ := seedCamViewInvestigation(t, cfg, "site_1", "", "lead question")
	childID, childTok, _ := seedCamViewInvestigation(t, cfg, "site_1", parentID, "sub question")
	if err := setCameraInvestigationStatus(db, childID, "running"); err != nil {
		t.Fatalf("set child running: %v", err)
	}

	code, out := camViewReplyPost(t, srv, childTok, "hello child")
	if code != http.StatusOK || out["ok"] != false {
		t.Fatalf("child reply: %d %v, want 200 ok=false", code, out)
	}
	if msg, _ := out["error"].(string); !strings.Contains(msg, "lead investigation") {
		t.Errorf("child reply error = %q, want it to steer to the lead", msg)
	}
	if pend, _ := listCameraInvestigationPendingMessages(db, childID); len(pend) != 0 {
		t.Errorf("child reply must park nothing: %+v", pend)
	}

	// A settled child refuses too (a resume would misroute it into the queue).
	if err := setCameraInvestigationStatus(db, childID, "answered"); err != nil {
		t.Fatalf("set child answered: %v", err)
	}
	if code, out := camViewReplyPost(t, srv, childTok, "follow-up"); code != http.StatusOK || out["ok"] != false {
		t.Errorf("settled child reply: %d %v, want ok=false", code, out)
	}
	if inv, _ := getCameraInvestigation(db, childID); inv.Status != "answered" {
		t.Errorf("child status = %q, want answered (never requeued)", inv.Status)
	}

	code, body := camViewGet(t, srv, "/camera/investigations/"+childTok+"/state.json")
	if code != http.StatusOK {
		t.Fatalf("child state.json: %d %s", code, body)
	}
	var st map[string]any
	_ = json.Unmarshal(body, &st)
	if st["can_reply"] != false {
		t.Errorf("child can_reply = %v, want false", st["can_reply"])
	}
}

// TestCameraViewReplyCapPerInvestigation pins the lifetime share-link message
// cap: once camViewReplyMaxPerInvestigation view-originated messages exist
// (delivered markers + parked view pending rows), the endpoint refuses with
// ok:false and state.json flips can_reply to false — the unauthenticated
// view_token can never reset the run's budget an unbounded number of times.
func TestCameraViewReplyCapPerInvestigation(t *testing.T) {
	camResetSvcRateLimiter()
	cfg := newCamViewTestConfig(t)
	srv := newCamViewTestServer(t, cfg)
	db, err := openProxyDB(cfg)
	if err != nil {
		t.Fatalf("openProxyDB: %v", err)
	}
	defer db.Close()
	invID, viewTok, _ := seedCamViewInvestigation(t, cfg, "site_1", "", "capped?")
	if err := setCameraInvestigationStatus(db, invID, "running"); err != nil {
		t.Fatalf("set running: %v", err)
	}
	// Cap-1 delivered view messages + 1 still-parked one = at the cap.
	for i := 0; i < camViewReplyMaxPerInvestigation-1; i++ {
		camAppendInvestigateMessage(db, invID, "operator", "earlier view message", camViewReplySource, "", 0, nil)
	}
	if _, err := insertCameraInvestigationPendingMessage(db, invID, "parked view message", camViewReplySource); err != nil {
		t.Fatalf("insert pending: %v", err)
	}

	code, out := camViewReplyPost(t, srv, viewTok, "one too many")
	if code != http.StatusOK || out["ok"] != false {
		t.Fatalf("capped reply: %d %v, want 200 ok=false", code, out)
	}
	if msg, _ := out["error"].(string); !strings.Contains(msg, "reply limit") {
		t.Errorf("capped reply error = %q, want a reply-limit message", msg)
	}
	if pend, _ := listCameraInvestigationPendingMessages(db, invID); len(pend) != 1 {
		t.Errorf("capped reply must not park more rows: %+v", pend)
	}
	code, body := camViewGet(t, srv, "/camera/investigations/"+viewTok+"/state.json")
	if code != http.StatusOK {
		t.Fatalf("state.json: %d %s", code, body)
	}
	var st map[string]any
	_ = json.Unmarshal(body, &st)
	if st["can_reply"] != false {
		t.Errorf("can_reply at the cap = %v, want false", st["can_reply"])
	}

	// Authenticated ('' origin) operator rows never count against the view cap.
	fresh, freshTok, _ := seedCamViewInvestigation(t, cfg, "site_1", "", "uncapped admin traffic")
	if err := setCameraInvestigationStatus(db, fresh, "running"); err != nil {
		t.Fatalf("set running: %v", err)
	}
	for i := 0; i < camViewReplyMaxPerInvestigation+3; i++ {
		camAppendInvestigateMessage(db, fresh, "operator", "studio message", "", "", 0, nil)
	}
	if code, out := camViewReplyPost(t, srv, freshTok, "still fine"); code != http.StatusOK || out["ok"] != true {
		t.Errorf("reply with only authenticated rows: %d %v, want ok=true", code, out)
	}
}

// TestCameraViewReplyInlineDrainWorkerDisabled: with the background worker
// DISABLED, a settled view reply must drain the re-queued row inline (like the
// sibling admin/svc reply paths) instead of stranding it in "queued" forever.
func TestCameraViewReplyInlineDrainWorkerDisabled(t *testing.T) {
	camResetSvcRateLimiter()
	cfg := newCamViewTestConfig(t)
	cfg.CameraInvestigateWorkerEnabled = false
	srv := newCamViewTestServer(t, cfg)
	db, err := openProxyDB(cfg)
	if err != nil {
		t.Fatalf("openProxyDB: %v", err)
	}
	defer db.Close()
	siteID, serr := insertCameraSite(db, camSite{Name: "Inline Site"})
	if serr != nil {
		t.Fatalf("insertCameraSite: %v", serr)
	}
	invID, ierr := insertCameraInvestigation(db, camInvestigation{SiteID: siteID, Question: "anything overnight?", Status: "answered"})
	if ierr != nil {
		t.Fatalf("insert investigation: %v", ierr)
	}
	camAppendInvestigateMessage(db, invID, "operator", "anything overnight?", "", "", 0, nil)
	inv, _ := getCameraInvestigation(db, invID)

	orig := camInvestigateAnalyzeFn
	defer func() { camInvestigateAnalyzeFn = orig }()
	calls := 0
	camInvestigateAnalyzeFn = func(ctx context.Context, c config, alias, sys, user string, images []string) (string, []camAnalyzeAttempt, error) {
		calls++
		return `{"thought":"reviewed","action":{"type":"answer","answer":"all clear at the gate"}}`, nil, nil
	}

	code, out := camViewReplyPost(t, srv, inv.ViewToken, "please double-check")
	if code != http.StatusOK || out["ok"] != true {
		t.Fatalf("settled reply: %d %v, want ok=true", code, out)
	}
	if calls < 1 {
		t.Fatalf("inline drain never ran the model (calls = %d)", calls)
	}
	after, _ := getCameraInvestigation(db, invID)
	if after.Status != "answered" {
		t.Errorf("status = %q, want answered (inline-drained, not stranded in queued)", after.Status)
	}
	msgs, _ := listCameraInvestigationMessages(db, invID)
	last := msgs[len(msgs)-1]
	if last.Role != "ai" || !strings.Contains(last.Content, "all clear at the gate") {
		t.Errorf("final row = %+v, want the inline run's answer", last)
	}
}

// TestCamViewStateCanReplyMatrix pins the can_reply gate: true for every
// non-closed status while the kill switch is off, false when closed or disabled.
func TestCamViewStateCanReplyMatrix(t *testing.T) {
	cfg := newCamViewTestConfig(t)
	db, err := openProxyDB(cfg)
	if err != nil {
		t.Fatalf("openProxyDB: %v", err)
	}
	defer db.Close()

	for _, status := range []string{"queued", "running", "awaiting_operator", "answered", "exhausted"} {
		inv := camInvestigation{ID: "inv_x", SiteID: "site_1", ViewToken: "vt", Status: status}
		if st := camViewStateJSON(cfg, db, inv); st["can_reply"] != true {
			t.Errorf("can_reply(%s) = %v, want true", status, st["can_reply"])
		}
		off := cfg
		off.CameraViewReplyDisabled = true
		if st := camViewStateJSON(off, db, inv); st["can_reply"] != false {
			t.Errorf("can_reply(%s, disabled) = %v, want false", status, st["can_reply"])
		}
	}
	closed := camInvestigation{ID: "inv_x", SiteID: "site_1", ViewToken: "vt", Status: "closed"}
	if st := camViewStateJSON(cfg, db, closed); st["can_reply"] != false {
		t.Errorf("can_reply(closed) = %v, want false", st["can_reply"])
	}
}

// TestCamInvestigationViewHTMLComposer pins the shell's composer contract via
// string containment (the const is zero-interpolation): the markup ids, the
// 2000-char maxlength, the can_reply gating, and the /reply POST path.
func TestCamInvestigationViewHTMLComposer(t *testing.T) {
	for _, want := range []string{
		`id="composer"`,
		`id="composer-text"`,
		`id="composer-send"`,
		`maxlength="2000"`,
		`state.can_reply`,
		`path + "/reply"`,
		`The investigator is waiting for your reply`,
		`Send a note to the investigator`,
		`Too many messages`,
	} {
		if !strings.Contains(camInvestigationViewHTML, want) {
			t.Errorf("shell HTML missing %q", want)
		}
	}
}
