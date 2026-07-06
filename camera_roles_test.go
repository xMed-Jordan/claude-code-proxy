package main

// camera_roles_test.go — the per-site person-role vocabulary (camera_roles.go),
// the role columns' write-path discipline (setCameraAvatarRole stamps
// role_confirmed_at; updateCameraAvatar never touches role/profile columns),
// the R0 service-API parity routes (media list/delete, scans, scan_id filter,
// role on create/update, site policy), and the NEUTRALITY invariant: the
// identity-layer schema and payloads must never grow severity/threat/alert
// vocabulary. Temp sqlite + httptest only.

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// ─────────────────────────── vocabulary ───────────────────────────

func TestCamSitePolicyNormalize(t *testing.T) {
	// Valid custom role, slug derived from the label when omitted.
	p := camSitePolicy{CustomRoles: []camRole{{Label: "Cleaning Crew", Description: "night shift"}}}
	if problem := camNormalizeSitePolicy(&p); problem != "" {
		t.Fatalf("normalize: %q", problem)
	}
	if len(p.CustomRoles) != 1 || p.CustomRoles[0].Slug != "cleaning_crew" || p.CustomRoles[0].Core {
		t.Fatalf("custom roles = %+v, want cleaning_crew (core=false)", p.CustomRoles)
	}

	// Core collisions and the reserved sentinels are rejected.
	for _, slug := range []string{"employee", "patient", "unknown", "none"} {
		p := camSitePolicy{CustomRoles: []camRole{{Slug: slug, Label: "X"}}}
		if problem := camNormalizeSitePolicy(&p); problem == "" {
			t.Errorf("slug %q must be rejected as a built-in collision", slug)
		}
	}

	// Bad slug characters are rejected; empty repeater rows are dropped.
	p = camSitePolicy{CustomRoles: []camRole{{Slug: "Not Valid!", Label: "x"}}}
	if problem := camNormalizeSitePolicy(&p); problem == "" {
		t.Error("invalid slug characters must be rejected")
	}
	p = camSitePolicy{CustomRoles: []camRole{{}, {Slug: "ok_role", Label: "OK"}}}
	if problem := camNormalizeSitePolicy(&p); problem != "" || len(p.CustomRoles) != 1 {
		t.Errorf("empty rows must be dropped, got (%q, %+v)", problem, p.CustomRoles)
	}

	// Duplicates rejected.
	p = camSitePolicy{CustomRoles: []camRole{{Slug: "dup", Label: "A"}, {Slug: "dup", Label: "B"}}}
	if problem := camNormalizeSitePolicy(&p); problem == "" {
		t.Error("duplicate slugs must be rejected")
	}

	// Non-Latin labels (the live sites label roles in Arabic) derive a stable
	// hash-fallback slug instead of failing the whole save with an empty-slug
	// 400 — and two different Arabic labels must not collide on "_".
	p = camSitePolicy{CustomRoles: []camRole{
		{Label: "موظف نظافة"},
		{Label: "حارس أمن"},
	}}
	if problem := camNormalizeSitePolicy(&p); problem != "" {
		t.Fatalf("arabic labels: %q", problem)
	}
	if len(p.CustomRoles) != 2 {
		t.Fatalf("arabic labels: %d roles, want 2", len(p.CustomRoles))
	}
	for _, r := range p.CustomRoles {
		if !camRoleSlugRe.MatchString(r.Slug) || !strings.HasPrefix(r.Slug, "role_") {
			t.Errorf("arabic label %q derived slug %q, want role_<hash>", r.Label, r.Slug)
		}
	}
	if p.CustomRoles[0].Slug == p.CustomRoles[1].Slug {
		t.Error("distinct arabic labels must derive distinct slugs")
	}

	// Truncation counts RUNES, not bytes — a long Arabic label must stay valid UTF-8.
	long := strings.Repeat("م", 70)
	p = camSitePolicy{CustomRoles: []camRole{{Slug: "ok", Label: long}}}
	if problem := camNormalizeSitePolicy(&p); problem != "" {
		t.Fatalf("long arabic label: %q", problem)
	}
	if got := []rune(p.CustomRoles[0].Label); len(got) != 64 {
		t.Errorf("label truncated to %d runes, want 64", len(got))
	}
	if !json.Valid([]byte(mustJSON(p))) {
		t.Error("truncated policy must still marshal to valid JSON")
	}
}

func TestCamValidateRoleAndVocabulary(t *testing.T) {
	pol := camParseSitePolicy(`{"custom_roles":[{"slug":"courier","label":"Courier"}]}`)
	for _, slug := range []string{"", "employee", "patient", "visitor", "family", "stranger", "courier"} {
		if !camValidateRole(pol, slug) {
			t.Errorf("role %q must validate", slug)
		}
	}
	for _, slug := range []string{"boss", "unknown", "EMPLOYEE "} {
		if camValidateRole(pol, slug) {
			t.Errorf("role %q must NOT validate", slug)
		}
	}
	vocab := camRoleVocabulary(pol)
	if len(vocab) != len(camRoleCore)+1 || vocab[len(vocab)-1].Slug != "courier" {
		t.Errorf("vocabulary = %+v, want core+courier", vocab)
	}
	// Round-trip: serialize → parse.
	pol2 := camParseSitePolicy(camSitePolicyJSON(pol))
	if len(pol2.CustomRoles) != 1 || pol2.CustomRoles[0].Slug != "courier" {
		t.Errorf("policy JSON round-trip lost custom roles: %+v", pol2)
	}
	if camSitePolicyJSON(camSitePolicy{}) != "" {
		t.Error("empty policy must serialize to \"\" (column stays empty)")
	}
}

// ─────────────────────────── role write paths ───────────────────────────

func TestSetCameraAvatarRoleStampsConfirmation(t *testing.T) {
	cfg := newSvcTestConfig(t)
	db := svcTestDB(t, cfg)
	siteID, _ := insertCameraSite(db, camSite{Name: "Clinic"})
	avID, err := insertCameraAvatar(db, camAvatar{SiteID: siteID, Name: "Dr K", Enabled: true})
	if err != nil {
		t.Fatalf("insert avatar: %v", err)
	}

	av, _ := getCameraAvatar(db, avID)
	if av.Role != "" || av.RoleConfirmedAt != "" {
		t.Fatalf("new avatar must be unlabeled, got role=%q confirmed=%q", av.Role, av.RoleConfirmedAt)
	}

	if err := setCameraAvatarRole(db, avID, "employee", "front desk"); err != nil {
		t.Fatalf("setCameraAvatarRole: %v", err)
	}
	av, _ = getCameraAvatar(db, avID)
	if av.Role != "employee" || av.RoleNote != "front desk" || av.RoleConfirmedAt == "" {
		t.Fatalf("after set: role=%q note=%q confirmed=%q", av.Role, av.RoleNote, av.RoleConfirmedAt)
	}

	// Clearing is also a confirmed decision — the stamp updates, role empties.
	if err := setCameraAvatarRole(db, avID, "", ""); err != nil {
		t.Fatalf("clear role: %v", err)
	}
	av, _ = getCameraAvatar(db, avID)
	if av.Role != "" || av.RoleConfirmedAt == "" {
		t.Fatalf("after clear: role=%q confirmed=%q (stamp must survive)", av.Role, av.RoleConfirmedAt)
	}

	// updateCameraAvatar must never touch the role trio (disjoint write paths).
	if err := setCameraAvatarRole(db, avID, "visitor", "n"); err != nil {
		t.Fatal(err)
	}
	av, _ = getCameraAvatar(db, avID)
	av.Name = "Dr Khaled"
	if err := updateCameraAvatar(db, av); err != nil {
		t.Fatalf("updateCameraAvatar: %v", err)
	}
	got, _ := getCameraAvatar(db, avID)
	if got.Role != "visitor" || got.RoleNote != "n" || got.Name != "Dr Khaled" {
		t.Fatalf("updateCameraAvatar clobbered role fields: %+v", got)
	}

	// Creation with a role stamps the confirmation (create handlers validate first).
	avID2, _ := insertCameraAvatar(db, camAvatar{SiteID: siteID, Name: "R", Role: "patient", Enabled: true})
	av2, _ := getCameraAvatar(db, avID2)
	if av2.Role != "patient" || av2.RoleConfirmedAt == "" {
		t.Fatalf("create-with-role: role=%q confirmed=%q", av2.Role, av2.RoleConfirmedAt)
	}
}

// ─────────────────────────── service API parity (R0) ───────────────────────────

func TestSvcSitePolicyAndAvatarRole(t *testing.T) {
	cfg := newSvcTestConfig(t)
	db := svcTestDB(t, cfg)
	srv := newSvcTestServer(t, cfg)
	siteID, _ := insertCameraSite(db, camSite{Name: "Clinic"})
	_, tok, _ := mintCameraServiceToken(db, "site", siteID, "t")

	// GET site: policy + resolved role_vocabulary (core set).
	code, out, _ := svcCall(t, srv, http.MethodGet, "/api/cameras/site", tok, nil)
	if code != http.StatusOK {
		t.Fatalf("get site: %d %v", code, out)
	}
	site := out["site"].(map[string]any)
	vocab, _ := site["role_vocabulary"].([]any)
	if len(vocab) != len(camRoleCore) {
		t.Fatalf("role_vocabulary = %v, want %d core roles", vocab, len(camRoleCore))
	}

	// POST a custom role; vocabulary grows.
	code, out, _ = svcCall(t, srv, http.MethodPost, "/api/cameras/site", tok, map[string]any{
		"policy": map[string]any{"custom_roles": []map[string]any{{"slug": "cleaning_crew", "label": "Cleaning Crew"}}},
	})
	if code != http.StatusOK {
		t.Fatalf("post policy: %d %v", code, out)
	}
	site = out["site"].(map[string]any)
	if vocab, _ := site["role_vocabulary"].([]any); len(vocab) != len(camRoleCore)+1 {
		t.Fatalf("vocabulary after custom role = %v", vocab)
	}

	// Invalid policy → 400.
	code, _, _ = svcCall(t, srv, http.MethodPost, "/api/cameras/site", tok, map[string]any{
		"policy": map[string]any{"custom_roles": []map[string]any{{"slug": "employee", "label": "X"}}},
	})
	if code != http.StatusBadRequest {
		t.Fatalf("core-collision policy: %d, want 400", code)
	}

	// Create an avatar with a valid custom role.
	code, out, _ = svcCall(t, srv, http.MethodPost, "/api/cameras/avatars", tok, map[string]any{
		"name": "Sam", "role": "cleaning_crew", "role_note": "nights",
	})
	if code != http.StatusOK {
		t.Fatalf("create avatar: %d %v", code, out)
	}
	avID := out["id"].(string)
	av, _ := getCameraAvatar(db, avID)
	if av.Role != "cleaning_crew" || av.RoleConfirmedAt == "" {
		t.Fatalf("created avatar role=%q confirmed=%q", av.Role, av.RoleConfirmedAt)
	}

	// Create with an unknown role → 400.
	code, _, _ = svcCall(t, srv, http.MethodPost, "/api/cameras/avatars", tok, map[string]any{
		"name": "X", "role": "boss",
	})
	if code != http.StatusBadRequest {
		t.Fatalf("create with unknown role: %d, want 400", code)
	}

	// Update role to a core slug; response carries the new fields.
	code, out, _ = svcCall(t, srv, http.MethodPost, "/api/cameras/avatars/update", tok, map[string]any{
		"id": avID, "role": "employee",
	})
	if code != http.StatusOK {
		t.Fatalf("update role: %d %v", code, out)
	}
	avatar := out["avatar"].(map[string]any)
	if avatar["role"] != "employee" || avatar["role_confirmed_at"] == "" {
		t.Fatalf("update response avatar = %v", avatar)
	}
	if _, ok := avatar["suggested_role"]; !ok {
		t.Error("avatar JSON must carry suggested_role (empty until the profile builder runs)")
	}

	// Unknown role on update → 400 and the stored role is untouched.
	code, _, _ = svcCall(t, srv, http.MethodPost, "/api/cameras/avatars/update", tok, map[string]any{
		"id": avID, "role": "intruder",
	})
	if code != http.StatusBadRequest {
		t.Fatalf("update with unknown role: %d, want 400", code)
	}
	av, _ = getCameraAvatar(db, avID)
	if av.Role != "employee" {
		t.Fatalf("role after rejected update = %q, want employee", av.Role)
	}

	// role_note alone updates the note without re-stamping the confirmation.
	before, _ := getCameraAvatar(db, avID)
	code, _, _ = svcCall(t, srv, http.MethodPost, "/api/cameras/avatars/update", tok, map[string]any{
		"id": avID, "role_note": "reception desk",
	})
	if code != http.StatusOK {
		t.Fatalf("note-only update: %d", code)
	}
	after, _ := getCameraAvatar(db, avID)
	if after.RoleNote != "reception desk" || after.RoleConfirmedAt != before.RoleConfirmedAt {
		t.Fatalf("note-only update: note=%q stamp %q→%q (stamp must not change)",
			after.RoleNote, before.RoleConfirmedAt, after.RoleConfirmedAt)
	}
}

func TestSvcAvatarMediaScansAndCandidateFilter(t *testing.T) {
	cfg := newSvcTestConfig(t)
	db := svcTestDB(t, cfg)
	srv := newSvcTestServer(t, cfg)
	siteID, _ := insertCameraSite(db, camSite{Name: "A"})
	otherSiteID, _ := insertCameraSite(db, camSite{Name: "B"})
	_, tok, _ := mintCameraServiceToken(db, "site", siteID, "a")

	avID, _ := insertCameraAvatar(db, camAvatar{SiteID: siteID, Name: "P", Enabled: true})
	foreignAvID, _ := insertCameraAvatar(db, camAvatar{SiteID: otherSiteID, Name: "F", Enabled: true})

	// Media list/delete (capture row absent — camDeleteAvatarCapture tolerates it).
	mediaID, _ := insertAvatarMedia(db, camAvatarMedia{AvatarID: avID, CaptureID: "cap_x", Token: "tok_x", Source: "upload"})
	code, out, _ := svcCall(t, srv, http.MethodGet, "/api/cameras/avatars/media?avatar_id="+avID, tok, nil)
	if code != http.StatusOK {
		t.Fatalf("media list: %d %v", code, out)
	}
	if media, _ := out["media"].([]any); len(media) != 1 {
		t.Fatalf("media = %v, want 1 row", out["media"])
	}
	// Foreign avatar → identical 404.
	code, _, _ = svcCall(t, srv, http.MethodGet, "/api/cameras/avatars/media?avatar_id="+foreignAvID, tok, nil)
	if code != http.StatusNotFound {
		t.Fatalf("foreign media list: %d, want 404", code)
	}
	code, _, _ = svcCall(t, srv, http.MethodPost, "/api/cameras/avatars/media/delete", tok, map[string]any{"id": mediaID})
	if code != http.StatusOK {
		t.Fatalf("media delete: %d", code)
	}
	if media, _ := listAvatarMedia(db, avID); len(media) != 0 {
		t.Fatalf("media rows after delete = %d, want 0", len(media))
	}

	// Scan history: by avatar and site-wide; foreign avatar → 404.
	scanA, _ := insertCameraAvatarScan(db, camAvatarScan{AvatarID: avID, SiteID: siteID, FromTS: "2026-07-01T00:00:00Z", ToTS: "2026-07-02T00:00:00Z", Status: "done"})
	scanB, _ := insertCameraAvatarScan(db, camAvatarScan{AvatarID: avID, SiteID: siteID, FromTS: "2026-07-02T00:00:00Z", ToTS: "2026-07-03T00:00:00Z", Status: "done"})
	_, _ = insertCameraAvatarScan(db, camAvatarScan{AvatarID: foreignAvID, SiteID: otherSiteID, FromTS: "2026-07-01T00:00:00Z", ToTS: "2026-07-02T00:00:00Z", Status: "done"})
	code, out, _ = svcCall(t, srv, http.MethodGet, "/api/cameras/avatars/scans?avatar_id="+avID, tok, nil)
	if code != http.StatusOK {
		t.Fatalf("scans by avatar: %d %v", code, out)
	}
	if scans, _ := out["scans"].([]any); len(scans) != 2 {
		t.Fatalf("scans = %v, want 2", out["scans"])
	}
	code, out, _ = svcCall(t, srv, http.MethodGet, "/api/cameras/avatars/scans", tok, nil)
	if scans, _ := out["scans"].([]any); code != http.StatusOK || len(scans) != 2 {
		t.Fatalf("site scans: %d, %v — the other site's scan must not leak", code, out["scans"])
	}
	code, _, _ = svcCall(t, srv, http.MethodGet, "/api/cameras/avatars/scans?avatar_id="+foreignAvID, tok, nil)
	if code != http.StatusNotFound {
		t.Fatalf("foreign scans: %d, want 404", code)
	}

	// Candidates: scan_id filter ANDs with avatar_id.
	if _, err := insertCameraAvatarCandidate(db, camAvatarCandidate{
		ScanID: scanA, AvatarID: avID, CameraID: "cam1", FrameTS: "2026-07-01T10:00:00Z", Status: "pending",
	}); err != nil {
		t.Fatalf("insert candidate: %v", err)
	}
	if _, err := insertCameraAvatarCandidate(db, camAvatarCandidate{
		ScanID: scanB, AvatarID: avID, CameraID: "cam1", FrameTS: "2026-07-02T10:00:00Z", Status: "pending",
	}); err != nil {
		t.Fatalf("insert candidate: %v", err)
	}
	code, out, _ = svcCall(t, srv, http.MethodGet, "/api/cameras/avatars/candidates?avatar_id="+avID+"&scan_id="+scanA, tok, nil)
	if code != http.StatusOK {
		t.Fatalf("candidates by scan: %d %v", code, out)
	}
	cands, _ := out["candidates"].([]any)
	if len(cands) != 1 {
		t.Fatalf("scan-filtered candidates = %d, want 1", len(cands))
	}
	if c := cands[0].(map[string]any); c["scan_id"] != scanA {
		t.Fatalf("candidate scan_id = %v, want %s", c["scan_id"], scanA)
	}
}

func TestSvcCandidateReviewExpiredCropWithoutS3(t *testing.T) {
	// With the crop capture gone and no S3 archive configured, the shared
	// re-hydrate helper must fail with a REAL 404 (admin parity) — not the old
	// 200 {ok:false, "run a new scan"} soft failure.
	cfg := newSvcTestConfig(t)
	db := svcTestDB(t, cfg)
	srv := newSvcTestServer(t, cfg)
	siteID, _ := insertCameraSite(db, camSite{Name: "A"})
	_, tok, _ := mintCameraServiceToken(db, "site", siteID, "a")
	avID, _ := insertCameraAvatar(db, camAvatar{SiteID: siteID, Name: "P", Enabled: true})
	scanID, _ := insertCameraAvatarScan(db, camAvatarScan{AvatarID: avID, SiteID: siteID, FromTS: "2026-07-01T00:00:00Z", ToTS: "2026-07-02T00:00:00Z", Status: "done"})
	candID, _ := insertCameraAvatarCandidate(db, camAvatarCandidate{
		ScanID: scanID, AvatarID: avID, CameraID: "cam1", FrameTS: "2026-07-01T10:00:00Z",
		CropToken: "tok_gone", Status: "pending",
	})

	code, out, _ := svcCall(t, srv, http.MethodPost, "/api/cameras/avatars/candidates/review", tok, map[string]any{
		"id": candID, "decision": "approved",
	})
	if code != http.StatusNotFound {
		t.Fatalf("review with expired crop, no S3: %d %v, want 404", code, out)
	}
	if msg, _ := out["error"].(string); !strings.Contains(msg, "S3 frame archive is not configured") {
		t.Fatalf("error = %q, want the S3-not-configured message", msg)
	}
	// The candidate must stay pending (nothing was pinned or inserted).
	cand, _ := getCameraAvatarCandidate(db, candID)
	if cand.Status != "pending" {
		t.Fatalf("candidate status = %q, want pending", cand.Status)
	}
}

// ─────────────────────────── neutrality invariant ───────────────────────────

// TestCameraIdentityNeutrality asserts the identity layer stays observation-only:
// no severity/threat/alert vocabulary may appear in the identity tables' column
// names or the avatar/site service payload keys. The camera layer records who/
// where/when and user-assigned labels; deciding what MATTERS lives in Connect.
// (camera_watch_runs.suspicion predates this layer and is a different feature.)
func TestCameraIdentityNeutrality(t *testing.T) {
	forbidden := []string{
		"severity", "priority", "risk", "threat", "suspicion",
		"should_alert", "notify", "escalate", "watchlist", "allowlist", "blocklist", "blacklist", "whitelist",
	}
	cfg := newSvcTestConfig(t)
	db := svcTestDB(t, cfg)

	for _, table := range []string{"camera_sightings", "camera_sighting_clusters", "camera_avatars", "camera_sites"} {
		rows, err := db.Query(`SELECT name FROM pragma_table_info(?)`, table)
		if err != nil {
			t.Fatalf("table_info(%s): %v", table, err)
		}
		var cols []string
		for rows.Next() {
			var name string
			if err := rows.Scan(&name); err != nil {
				t.Fatal(err)
			}
			cols = append(cols, strings.ToLower(name))
		}
		rows.Close()
		if len(cols) == 0 {
			t.Fatalf("table %s does not exist", table)
		}
		for _, col := range cols {
			for _, bad := range forbidden {
				if strings.Contains(col, bad) {
					t.Errorf("NEUTRALITY: %s.%s contains forbidden token %q", table, col, bad)
				}
			}
		}
	}

	// Payload keys: the avatar and site service JSON views.
	siteID, _ := insertCameraSite(db, camSite{Name: "N"})
	avID, _ := insertCameraAvatar(db, camAvatar{SiteID: siteID, Name: "P", Enabled: true})
	av, _ := getCameraAvatar(db, avID)
	site, _ := getCameraSite(db, siteID)
	for name, payload := range map[string]map[string]any{
		"svcAvatarJSON": svcAvatarJSON(db, av),
		"siteJSON":      siteJSON(site),
	} {
		for key := range payload {
			for _, bad := range forbidden {
				if strings.Contains(strings.ToLower(key), bad) {
					t.Errorf("NEUTRALITY: %s key %q contains forbidden token %q", name, key, bad)
				}
			}
		}
	}
}
