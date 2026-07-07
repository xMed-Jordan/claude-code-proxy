package main

// camera_investigate_view_test.go (WS4) — the public timeline surface: the
// view-token media auth matrix (valid / not-minted / cross-investigation / wrong
// view-token), the state.json shape (gated relative media URLs + expiry flag), and
// the progress notifier (opt-out silence, nil-safety, key-tool filtering, and the
// coalesced "media" flush). Temp sqlite + httptest only — no DVRs, no model calls.

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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
