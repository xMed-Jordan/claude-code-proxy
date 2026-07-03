package main

// camera_playbooks_test.go — focused unit tests for the pure/DB-local playbook
// functions: catalog rendering (truncation, caps, DVR prefix), case-insensitive
// name lookup, pin/release retention semantics, and the playbook investigate
// tool (unknown-name summary + reference loading). No network, no DVR.

import (
	"database/sql"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ─────────────────────────── catalog ───────────────────────────

func TestCamPlaybookCatalogEmpty(t *testing.T) {
	if got := camPlaybookCatalog(nil, nil); got != "" {
		t.Errorf("empty catalog = %q, want \"\"", got)
	}
	disabled := []camPlaybook{{Name: "off", WhenToUse: "never", Enabled: false}}
	if got := camPlaybookCatalog(disabled, nil); got != "" {
		t.Errorf("all-disabled catalog = %q, want \"\"", got)
	}
}

func TestCamPlaybookCatalogEntriesAndPrefix(t *testing.T) {
	dvrByID := map[string]CamDVR{
		"dvr1": {ID: "dvr1", Name: "Clinic NVR"},
		"dvr2": {ID: "dvr2", Name: "   "}, // blank name → fall back to id
	}
	pbs := []camPlaybook{
		{Name: "employee-workday", WhenToUse: "Track a named employee's full day.", Enabled: true},
		{Name: "device-standby-check", WhenToUse: "Handle must point UP.", DVRID: "dvr1", Enabled: true},
		{Name: "blank-dvr-name", WhenToUse: "x", DVRID: "dvr2", Enabled: true},
		{Name: "unknown-dvr", WhenToUse: "y", DVRID: "dvr9", Enabled: true},
		{Name: "no-one-liner", Enabled: true},
		{Name: "disabled-one", WhenToUse: "should not appear", Enabled: false},
	}
	got := camPlaybookCatalog(pbs, dvrByID)
	lines := strings.Split(got, "\n")
	if len(lines) != 5 {
		t.Fatalf("catalog has %d lines, want 5:\n%s", len(lines), got)
	}
	if lines[0] != `- "employee-workday" — Track a named employee's full day.` {
		t.Errorf("plain entry = %q", lines[0])
	}
	if lines[1] != `- "device-standby-check" — [DVR "Clinic NVR"] Handle must point UP.` {
		t.Errorf("dvr entry = %q", lines[1])
	}
	if !strings.Contains(lines[2], `[DVR "dvr2"]`) {
		t.Errorf("blank dvr name should fall back to id, got %q", lines[2])
	}
	if !strings.Contains(lines[3], `[DVR "dvr9"]`) {
		t.Errorf("unknown dvr should fall back to id, got %q", lines[3])
	}
	if lines[4] != `- "no-one-liner"` {
		t.Errorf("no-one-liner entry = %q", lines[4])
	}
	if strings.Contains(got, "disabled-one") {
		t.Errorf("disabled playbook leaked into catalog:\n%s", got)
	}
	// The header + guidance lines are emitted by camInvestigateSystemPrompt,
	// not by the catalog builder.
	if strings.Contains(got, "OPERATIONAL PLAYBOOKS") || strings.Contains(got, "Never improvise") {
		t.Errorf("catalog must not duplicate the caller's header/guidance:\n%s", got)
	}
}

func TestCamPlaybookCatalogTruncation(t *testing.T) {
	long := strings.Repeat("é", camPlaybookOneLinerMax+40) // multibyte: rune-safe cut required
	got := camPlaybookCatalog([]camPlaybook{{Name: "long", WhenToUse: long, Enabled: true}}, nil)
	wantLine := `- "long" — ` + strings.Repeat("é", camPlaybookOneLinerMax) + "…"
	if got != wantLine {
		t.Errorf("truncated entry:\n got %q\nwant %q", got, wantLine)
	}
	// Exactly-at-limit strings are left untouched (no stray ellipsis).
	exact := strings.Repeat("a", camPlaybookOneLinerMax)
	got = camPlaybookCatalog([]camPlaybook{{Name: "exact", WhenToUse: exact, Enabled: true}}, nil)
	if strings.Contains(got, "…") {
		t.Errorf("at-limit one-liner must not be truncated: %q", got)
	}
}

func TestCamPlaybookCatalogCap(t *testing.T) {
	var pbs []camPlaybook
	for i := 0; i < camPlaybookCatalogMax+10; i++ {
		pbs = append(pbs, camPlaybook{Name: "pb-" + strings.Repeat("x", i%3), WhenToUse: "w", Enabled: true})
	}
	got := camPlaybookCatalog(pbs, nil)
	if n := len(strings.Split(got, "\n")); n != camPlaybookCatalogMax {
		t.Errorf("catalog entries = %d, want cap %d", n, camPlaybookCatalogMax)
	}
}

// ─────────────────────────── DB fixtures ───────────────────────────

func newPlaybookTestDB(t *testing.T) (*sql.DB, string) {
	t.Helper()
	db := openCamTestDB(t)
	if err := migrateCameraDB(db); err != nil {
		t.Fatalf("migrateCameraDB: %v", err)
	}
	siteID, err := insertCameraSite(db, camSite{Name: "Test Clinic"})
	if err != nil {
		t.Fatalf("insertCameraSite: %v", err)
	}
	return db, siteID
}

func insertTestCapture(t *testing.T, db *sql.DB, path, contentType, expiresAt string) string {
	t.Helper()
	token := randToken(16)
	if _, err := insertCameraCapture(db, camCapture{
		SiteID: "site_x", Kind: "snapshot", Quality: "sub",
		Token: token, Path: path, ContentType: contentType, ExpiresAt: expiresAt,
	}); err != nil {
		t.Fatalf("insertCameraCapture: %v", err)
	}
	return token
}

func captureExpiry(t *testing.T, db *sql.DB, token string) string {
	t.Helper()
	var exp string
	if err := db.QueryRow(`SELECT expires_at FROM camera_captures WHERE token = ?`, token).Scan(&exp); err != nil {
		t.Fatalf("select expires_at: %v", err)
	}
	return exp
}

// ─────────────────────────── name lookup ───────────────────────────

func TestFindCameraPlaybookByNameCaseInsensitive(t *testing.T) {
	db, siteID := newPlaybookTestDB(t)
	id, err := insertCameraPlaybook(db, camPlaybook{SiteID: siteID, Name: "Employee-Workday", Enabled: true})
	if err != nil {
		t.Fatalf("insertCameraPlaybook: %v", err)
	}
	for _, name := range []string{"Employee-Workday", "employee-workday", "EMPLOYEE-WORKDAY", "  employee-workday  "} {
		pb, ferr := findCameraPlaybookByName(db, siteID, name)
		if ferr != nil {
			t.Errorf("findCameraPlaybookByName(%q): %v", name, ferr)
			continue
		}
		if pb.ID != id {
			t.Errorf("findCameraPlaybookByName(%q) = %q, want %q", name, pb.ID, id)
		}
	}
	if _, ferr := findCameraPlaybookByName(db, siteID, "nope"); !errors.Is(ferr, sql.ErrNoRows) {
		t.Errorf("unknown name err = %v, want sql.ErrNoRows", ferr)
	}
	if _, ferr := findCameraPlaybookByName(db, "other_site", "Employee-Workday"); !errors.Is(ferr, sql.ErrNoRows) {
		t.Errorf("wrong-site lookup err = %v, want sql.ErrNoRows", ferr)
	}
	if _, ferr := findCameraPlaybookByName(db, siteID, "   "); !errors.Is(ferr, sql.ErrNoRows) {
		t.Errorf("blank name err = %v, want sql.ErrNoRows", ferr)
	}
}

func TestInsertCameraPlaybookDuplicateName(t *testing.T) {
	db, siteID := newPlaybookTestDB(t)
	if _, err := insertCameraPlaybook(db, camPlaybook{SiteID: siteID, Name: "dup", Enabled: true}); err != nil {
		t.Fatalf("first insert: %v", err)
	}
	_, err := insertCameraPlaybook(db, camPlaybook{SiteID: siteID, Name: "dup", Enabled: true})
	var dup camPlaybookNameTakenError
	if !errors.As(err, &dup) {
		t.Fatalf("second insert err = %v, want camPlaybookNameTakenError", err)
	}
	if !strings.Contains(err.Error(), `"dup"`) {
		t.Errorf("friendly error should name the clash: %q", err.Error())
	}
	// Same name on ANOTHER site is fine (uniqueness is per site).
	otherSite, _ := insertCameraSite(db, camSite{Name: "Second Site"})
	if _, err := insertCameraPlaybook(db, camPlaybook{SiteID: otherSite, Name: "dup", Enabled: true}); err != nil {
		t.Errorf("same name on another site should insert: %v", err)
	}
}

// ─────────────────────────── pin / release ───────────────────────────

func TestCamPlaybookPinReleaseSemantics(t *testing.T) {
	db, siteID := newPlaybookTestDB(t)
	cfg := config{CameraMediaRetention: time.Hour}
	origExpiry := time.Now().Add(30 * time.Minute).Format(time.RFC3339)
	token := insertTestCapture(t, db, filepath.Join(t.TempDir(), "ref.jpg"), "image/jpeg", origExpiry)

	if err := pinCameraCapture(db, token); err != nil {
		t.Fatalf("pinCameraCapture: %v", err)
	}
	if exp := captureExpiry(t, db, token); exp != "" {
		t.Fatalf("pinned expires_at = %q, want \"\"", exp)
	}

	// Two playbooks referencing the SAME token: removing one reference must
	// keep the pin; removing the last must restore a real expiry.
	pb1, _ := insertCameraPlaybook(db, camPlaybook{SiteID: siteID, Name: "pb-one", Enabled: true})
	pb2, _ := insertCameraPlaybook(db, camPlaybook{SiteID: siteID, Name: "pb-two", Enabled: true})
	m1, err := insertCameraPlaybookMedia(db, camPlaybookMedia{PlaybookID: pb1, CaptureToken: token, Note: "n1"})
	if err != nil {
		t.Fatalf("insertCameraPlaybookMedia: %v", err)
	}
	m2, err := insertCameraPlaybookMedia(db, camPlaybookMedia{PlaybookID: pb2, CaptureToken: token, Note: "n2"})
	if err != nil {
		t.Fatalf("insertCameraPlaybookMedia: %v", err)
	}

	if err := deleteCameraPlaybookMedia(db, m1); err != nil {
		t.Fatalf("deleteCameraPlaybookMedia(m1): %v", err)
	}
	camReleaseCaptureIfUnreferenced(db, cfg, token)
	if exp := captureExpiry(t, db, token); exp != "" {
		t.Fatalf("token still referenced by pb2 but expires_at = %q, want \"\" (pin kept)", exp)
	}

	before := time.Now()
	if err := deleteCameraPlaybookMedia(db, m2); err != nil {
		t.Fatalf("deleteCameraPlaybookMedia(m2): %v", err)
	}
	camReleaseCaptureIfUnreferenced(db, cfg, token)
	exp := captureExpiry(t, db, token)
	if exp == "" {
		t.Fatal("last reference removed but capture is still pinned (expires_at empty)")
	}
	parsed, perr := time.Parse(time.RFC3339, exp)
	if perr != nil {
		t.Fatalf("restored expires_at %q is not RFC3339: %v", exp, perr)
	}
	if parsed.Before(before.Add(50*time.Minute)) || parsed.After(before.Add(80*time.Minute)) {
		t.Errorf("restored expiry %s not ~now+1h (retention)", exp)
	}
}

func TestDeleteCameraPlaybookReleasesPins(t *testing.T) {
	db, siteID := newPlaybookTestDB(t)
	cfg := config{CameraMediaRetention: time.Hour}
	token := insertTestCapture(t, db, filepath.Join(t.TempDir(), "ref.jpg"), "image/jpeg", "")
	pbID, _ := insertCameraPlaybook(db, camPlaybook{SiteID: siteID, Name: "to-delete", Enabled: true})
	if _, err := insertCameraPlaybookMedia(db, camPlaybookMedia{PlaybookID: pbID, CaptureToken: token}); err != nil {
		t.Fatalf("insertCameraPlaybookMedia: %v", err)
	}

	if err := deleteCameraPlaybook(db, cfg, pbID); err != nil {
		t.Fatalf("deleteCameraPlaybook: %v", err)
	}
	if _, err := getCameraPlaybook(db, pbID); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("playbook row should be gone, got err %v", err)
	}
	if rows, _ := listCameraPlaybookMedia(db, pbID); len(rows) != 0 {
		t.Errorf("media rows should be gone, got %d", len(rows))
	}
	if exp := captureExpiry(t, db, token); exp == "" {
		t.Errorf("capture should be released after playbook delete, expires_at still \"\"")
	}
}

// ─────────────────────────── investigate tool ───────────────────────────

func TestCamToolPlaybookUnknownName(t *testing.T) {
	db, siteID := newPlaybookTestDB(t)
	if _, err := insertCameraPlaybook(db, camPlaybook{SiteID: siteID, Name: "employee-workday", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := insertCameraPlaybook(db, camPlaybook{SiteID: siteID, Name: "device-standby-check", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := insertCameraPlaybook(db, camPlaybook{SiteID: siteID, Name: "hidden", Enabled: false}); err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("GET", "http://proxy.test/", nil)
	site := camSite{ID: siteID}

	res := camToolPlaybook(t.Context(), config{}, db, r, site, investigateArgs{Name: "nope"}, t.TempDir())
	if !strings.Contains(res.Summary, `no playbook named "nope"`) {
		t.Errorf("unknown-name summary missing the miss: %q", res.Summary)
	}
	if !strings.Contains(res.Summary, `"employee-workday"`) || !strings.Contains(res.Summary, `"device-standby-check"`) {
		t.Errorf("unknown-name summary should list valid names: %q", res.Summary)
	}
	if strings.Contains(res.Summary, `"hidden"`) {
		t.Errorf("disabled playbook must not be listed as valid: %q", res.Summary)
	}
	if res.Fetches != 0 || len(res.Media) != 0 || len(res.Images) != 0 {
		t.Errorf("unknown-name result must be text-only: %+v", res)
	}

	// A DISABLED playbook behaves like an unknown one.
	res = camToolPlaybook(t.Context(), config{}, db, r, site, investigateArgs{Name: "hidden"}, t.TempDir())
	if !strings.Contains(res.Summary, `no playbook named "hidden"`) {
		t.Errorf("disabled playbook should read as unknown: %q", res.Summary)
	}
}

func TestCamToolPlaybookLoadsReferences(t *testing.T) {
	db, siteID := newPlaybookTestDB(t)
	dir := t.TempDir()

	refPath := filepath.Join(dir, "ref.jpg")
	if err := os.WriteFile(refPath, []byte("\xff\xd8\xff fake jpeg"), 0o600); err != nil {
		t.Fatalf("write ref: %v", err)
	}
	goodToken := insertTestCapture(t, db, refPath, "image/jpeg", "")
	missingToken := insertTestCapture(t, db, filepath.Join(dir, "vanished.jpg"), "image/jpeg", "")
	clipPath := filepath.Join(dir, "ref.mp4")
	if err := os.WriteFile(clipPath, []byte("fake mp4"), 0o600); err != nil {
		t.Fatalf("write clip: %v", err)
	}
	clipToken := insertTestCapture(t, db, clipPath, "video/mp4", "")

	pbID, err := insertCameraPlaybook(db, camPlaybook{
		SiteID: siteID, Name: "Device-Standby-Check",
		WhenToUse:    "the device handle question",
		Instructions: "1. Snapshot camera 7 main.\n2. Compare handle angle to the reference.",
		Enabled:      true,
	})
	if err != nil {
		t.Fatal(err)
	}
	mustAttach := func(token, note string) {
		t.Helper()
		if _, err := insertCameraPlaybookMedia(db, camPlaybookMedia{PlaybookID: pbID, CaptureToken: token, Note: note}); err != nil {
			t.Fatalf("attach %s: %v", note, err)
		}
	}
	mustAttach(goodToken, "correct standby position — handle UP")
	mustAttach(missingToken, "gone from disk")
	mustAttach(clipToken, "walkthrough clip")

	r := httptest.NewRequest("GET", "http://proxy.test/", nil)
	// Case-insensitive tool-time lookup: stored "Device-Standby-Check".
	res := camToolPlaybook(t.Context(), config{}, db, r, camSite{ID: siteID}, investigateArgs{Name: "device-standby-check"}, dir)

	if !strings.Contains(res.Summary, `PLAYBOOK "Device-Standby-Check" (use when: the device handle question):`) {
		t.Errorf("summary header wrong:\n%s", res.Summary)
	}
	if !strings.Contains(res.Summary, "2. Compare handle angle to the reference.") {
		t.Errorf("summary missing instructions:\n%s", res.Summary)
	}
	// Reference notes are listed in a non-contractual order (created_at ties break
	// on a random id), so assert the note text is present without a fixed number.
	if !strings.Contains(res.Summary, "REFERENCE IMAGES (operator-approved, attached this turn):") ||
		!strings.Contains(res.Summary, "correct standby position — handle UP") {
		t.Errorf("summary missing reference notes:\n%s", res.Summary)
	}
	if strings.Contains(res.Summary, "gone from disk") {
		t.Errorf("missing-file reference should be skipped entirely:\n%s", res.Summary)
	}
	if len(res.Images) != 1 || res.Images[0] != refPath {
		t.Errorf("Images = %v, want [%s] (clip is not an image; missing file skipped)", res.Images, refPath)
	}
	if len(res.Media) != 2 {
		t.Fatalf("Media count = %d, want 2 (image + clip)", len(res.Media))
	}
	// Media order is not contractual (both are references attached the same turn,
	// and listCameraPlaybookMedia ties-breaks on a random id), so assert by token.
	byToken := map[string]evidenceItem{}
	for _, m := range res.Media {
		if strings.Contains(m.MediaURL, "/camera/media/"+goodToken) {
			byToken[goodToken] = m
		}
		if strings.Contains(m.MediaURL, "/camera/media/"+clipToken) {
			byToken[clipToken] = m
		}
	}
	if _, ok := byToken[goodToken]; !ok {
		t.Errorf("image reference (token %s) missing from Media: %+v", goodToken, res.Media)
	} else if byToken[goodToken].Caption != "REFERENCE — correct standby position — handle UP" {
		t.Errorf("image media caption = %q", byToken[goodToken].Caption)
	}
	if _, ok := byToken[clipToken]; !ok {
		t.Errorf("clip reference (token %s) missing from Media: %+v", clipToken, res.Media)
	}
	if res.Fetches != 0 {
		t.Errorf("Fetches = %d, want 0 (no DVR I/O)", res.Fetches)
	}
}

func TestCamToolPlaybookRefImageCap(t *testing.T) {
	db, siteID := newPlaybookTestDB(t)
	dir := t.TempDir()
	pbID, err := insertCameraPlaybook(db, camPlaybook{SiteID: siteID, Name: "many-refs", Instructions: "look", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < camPlaybookRefImgMax+3; i++ {
		p := filepath.Join(dir, "ref"+string(rune('a'+i))+".jpg")
		if err := os.WriteFile(p, []byte("jpg"), 0o600); err != nil {
			t.Fatal(err)
		}
		token := insertTestCapture(t, db, p, "image/jpeg", "")
		if _, err := insertCameraPlaybookMedia(db, camPlaybookMedia{PlaybookID: pbID, CaptureToken: token, Note: "ref"}); err != nil {
			t.Fatal(err)
		}
	}
	r := httptest.NewRequest("GET", "http://proxy.test/", nil)
	res := camToolPlaybook(t.Context(), config{}, db, r, camSite{ID: siteID}, investigateArgs{Name: "many-refs"}, dir)
	if len(res.Images) != camPlaybookRefImgMax {
		t.Errorf("Images = %d, want cap %d", len(res.Images), camPlaybookRefImgMax)
	}
	if len(res.Media) != camPlaybookRefImgMax+3 {
		t.Errorf("Media = %d, want %d (all artifacts stay citable beyond the image cap)", len(res.Media), camPlaybookRefImgMax+3)
	}
}
