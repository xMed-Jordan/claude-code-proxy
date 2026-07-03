package main

// camera_avatar_tools_test.go — focused unit tests for camera_avatar_tools.go's
// PURE surfaces: the tolerant verdict parser, the sighting timeline/cluster/
// instant helpers, the avatar_check prompt shape, and the tools' budget guards
// (no network, no DVR, no DB writes).

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestParseAvatarVerdictTolerant(t *testing.T) {
	cases := []struct {
		name        string
		raw         string
		wantPresent bool
		wantConf    float64
		wantBBox    bool
		wantReason  string
	}{
		{
			name:        "plain object",
			raw:         `{"present":true,"confidence":0.85,"bbox":{"x0":300,"y0":120,"x1":420,"y1":700},"reason":"white coat"}`,
			wantPresent: true, wantConf: 0.85, wantBBox: true, wantReason: "white coat",
		},
		{
			name:        "prose wrapped with fences",
			raw:         "Sure! Here is my verdict:\n```json\n{\"present\": true, \"confidence\": 0.7, \"bbox\": null, \"reason\": \"plausible\"}\n```\nHope this helps.",
			wantPresent: true, wantConf: 0.7, wantBBox: false, wantReason: "plausible",
		},
		{
			name:        "stringified whole reply",
			raw:         `"{\"present\":true,\"confidence\":0.6,\"bbox\":{\"x0\":1,\"y0\":1,\"x1\":900,\"y1\":900},\"reason\":\"ok\"}"`,
			wantPresent: true, wantConf: 0.6, wantBBox: true, wantReason: "ok",
		},
		{
			name:        "stringy scalars",
			raw:         `{"present":"true","confidence":"0.42","bbox":null,"reason":"stringy"}`,
			wantPresent: true, wantConf: 0.42, wantBBox: false, wantReason: "stringy",
		},
		{
			name:        "stringified bbox unwrapped",
			raw:         `{"present":true,"confidence":0.9,"bbox":"{\"x0\":10,\"y0\":10,\"x1\":500,\"y1\":500}","reason":"r"}`,
			wantPresent: true, wantConf: 0.9, wantBBox: true, wantReason: "r",
		},
		{
			name:        "confidence clamped high",
			raw:         `{"present":true,"confidence":7,"reason":"over"}`,
			wantPresent: true, wantConf: 1, wantBBox: false, wantReason: "over",
		},
		{
			name:        "confidence clamped low, absent",
			raw:         `{"present":false,"confidence":-2,"bbox":null,"reason":""}`,
			wantPresent: false, wantConf: 0, wantBBox: false, wantReason: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v, err := parseAvatarVerdict(tc.raw)
			if err != nil {
				t.Fatalf("parseAvatarVerdict(%q) error: %v", tc.raw, err)
			}
			if v.Present != tc.wantPresent {
				t.Errorf("Present = %v, want %v", v.Present, tc.wantPresent)
			}
			if v.Confidence != tc.wantConf {
				t.Errorf("Confidence = %v, want %v", v.Confidence, tc.wantConf)
			}
			if (len(v.BBox) > 0) != tc.wantBBox {
				t.Errorf("BBox presence = %v (%s), want %v", len(v.BBox) > 0, string(v.BBox), tc.wantBBox)
			}
			if tc.wantBBox && !strings.HasPrefix(strings.TrimSpace(string(v.BBox)), "{") {
				t.Errorf("BBox should be a raw object, got %s", string(v.BBox))
			}
			if v.Reason != tc.wantReason {
				t.Errorf("Reason = %q, want %q", v.Reason, tc.wantReason)
			}
		})
	}
}

func TestParseAvatarVerdictNoObject(t *testing.T) {
	for _, raw := range []string{"", "no json here", "just some { broken"} {
		if _, err := parseAvatarVerdict(raw); err == nil {
			t.Errorf("parseAvatarVerdict(%q) expected an error", raw)
		}
	}
}

func TestAvFindTimeline(t *testing.T) {
	base := time.Date(2026, 7, 2, 8, 2, 15, 0, time.UTC)
	s := []avFindSighting{
		{T: base.Add(90 * time.Minute), Confidence: 0.5, Reason: "leaves"},
		{T: base, Confidence: 0.9, Reason: "arrives via staff door"},
		{T: base.Add(10 * time.Minute), Confidence: 0.7}, // no reason -> no dash
	}
	got := avFindTimeline(s, time.UTC)
	want := "08:02:15 conf 0.90 — arrives via staff door\n08:12:15 conf 0.70\n09:32:15 conf 0.50 — leaves"
	if got != want {
		t.Errorf("timeline mismatch:\n got: %q\nwant: %q", got, want)
	}
	// nil loc falls back to UTC and must not panic (s[0] is the 09:32 sighting)
	if got := avFindTimeline(s[:1], nil); !strings.HasPrefix(got, "09:32:15 conf 0.50") {
		t.Errorf("nil-loc timeline = %q", got)
	}
	if got := avFindTimeline(nil, time.UTC); got != "" {
		t.Errorf("empty timeline = %q, want empty", got)
	}
}

func TestAvFindCluster(t *testing.T) {
	base := time.Date(2026, 7, 2, 8, 0, 0, 0, time.UTC)
	in := []avFindSighting{
		{T: base.Add(15 * time.Second), Confidence: 0.9, Reason: "best of cluster 1"},
		{T: base, Confidence: 0.5},
		{T: base.Add(28 * time.Second), Confidence: 0.6}, // chains via the 15s member (<20s apart)
		{T: base.Add(5 * time.Minute), Confidence: 0.4, Reason: "cluster 2"},
	}
	out := avFindCluster(in, 20*time.Second)
	if len(out) != 2 {
		t.Fatalf("clusters = %d, want 2 (%+v)", len(out), out)
	}
	if out[0].Confidence != 0.9 || out[0].Reason != "best of cluster 1" {
		t.Errorf("cluster 1 keeper = %+v, want the 0.9 member", out[0])
	}
	if out[1].Reason != "cluster 2" {
		t.Errorf("cluster 2 keeper = %+v", out[1])
	}
	if !out[0].T.Before(out[1].T) {
		t.Errorf("clusters not chronological: %v then %v", out[0].T, out[1].T)
	}
	if got := avFindCluster(nil, 20*time.Second); len(got) != 0 {
		t.Errorf("nil cluster input should stay empty, got %d", len(got))
	}
}

func TestAvFindInstants(t *testing.T) {
	from := time.Date(2026, 7, 2, 6, 0, 0, 0, time.UTC)

	// Short window: 1s step, inclusive endpoints.
	got, step := avFindInstants(from, from.Add(10*time.Second), 600)
	if step != time.Second || len(got) != 11 {
		t.Errorf("short window: step=%v len=%d, want 1s/11", step, len(got))
	}

	// 2h window coarsened to <=600 instants: step 12s.
	got, step = avFindInstants(from, from.Add(2*time.Hour), 600)
	if step != 12*time.Second {
		t.Errorf("2h window step = %v, want 12s", step)
	}
	if len(got) > 600 {
		t.Errorf("2h window instants = %d, want <= 600", len(got))
	}
	for i := 1; i < len(got); i++ {
		if !got[i].After(got[i-1]) {
			t.Fatalf("instants not strictly increasing at %d", i)
		}
	}

	// Degenerate window still yields the start instant.
	got, _ = avFindInstants(from, from, 600)
	if len(got) != 1 || !got[0].Equal(from) {
		t.Errorf("degenerate window = %v", got)
	}
}

// TestAvatarToolsBudgetGuard exercises the mediaLeft guards: the guards fire
// BEFORE any DB/DVR/S3 work, so a nil *sql.DB proves the ordering.
func TestAvatarToolsBudgetGuard(t *testing.T) {
	ctx := context.Background()
	cfg := config{}
	site := camSite{ID: "site1"}
	camByID := map[string]camera{"cam1": {ID: "cam1", SiteID: "site1", DVRID: "d1", Channel: 1, Enabled: true}}
	dvrByID := map[string]CamDVR{"d1": {ID: "d1", SiteID: "site1", Enabled: true}}
	allowed := map[string]bool{"cam1": true}
	scratch := t.TempDir()

	// avatar_check needs 2 fetches: mediaLeft 0 and 1 both refuse.
	for _, left := range []int{0, 1} {
		res := camToolAvatarCheck(ctx, cfg, nil, nil, site, "alias", investigateArgs{
			AvatarID: "avatar_x", CameraIDs: []string{"cam1"}, Time: "2026-07-01T10:00:00Z",
		}, camByID, dvrByID, allowed, scratch, left)
		if !strings.Contains(res.Summary, "budget") {
			t.Errorf("avatar_check mediaLeft=%d: summary %q should mention the budget", left, res.Summary)
		}
		if res.Fetches != 0 || len(res.Images) != 0 || len(res.Media) != 0 {
			t.Errorf("avatar_check mediaLeft=%d: refused call must cost nothing, got %+v", left, res)
		}
	}

	// avatar_find refuses at mediaLeft=0.
	now := time.Now().UTC()
	res := camToolAvatarFind(ctx, cfg, nil, nil, site, "alias", investigateArgs{
		AvatarID:  "avatar_x",
		CameraIDs: []string{"cam1"},
		From:      now.Add(-2 * time.Hour).Format(time.RFC3339),
		To:        now.Add(-1 * time.Hour).Format(time.RFC3339),
	}, camByID, dvrByID, allowed, scratch, 0)
	if !strings.Contains(res.Summary, "budget") {
		t.Errorf("avatar_find mediaLeft=0: summary %q should mention the budget", res.Summary)
	}
	if res.Fetches != 0 {
		t.Errorf("avatar_find mediaLeft=0: Fetches = %d, want 0", res.Fetches)
	}

	// Bad camera ids fail before the budget guard (and before any DB use).
	res = camToolAvatarCheck(ctx, cfg, nil, nil, site, "alias", investigateArgs{AvatarID: "avatar_x", CameraIDs: []string{"nope"}, Time: "2026-07-01T10:00:00Z"}, camByID, dvrByID, allowed, scratch, 10)
	if !strings.Contains(res.Summary, "camera_ids") {
		t.Errorf("avatar_check bad camera: summary %q", res.Summary)
	}
}

func TestCamAvatarCheckPromptShape(t *testing.T) {
	av := camAvatar{Name: "Dr. Ahmad", Type: "human", ExternalRef: "EMP-042", Description: "white coat, arrives 08:00"}

	sys, user := camAvatarCheckPrompt(av, 3, "Entrance", "2026-07-02T08:12:45+03:00", 1920, 1080)
	for _, want := range []string{
		"security-camera identification assistant",
		"ref 1..3",
		`"Entrance"`,
		"2026-07-02T08:12:45+03:00",
		"not biometric proof",
		"0.9+ only when clothing AND build AND context",
		`"bbox":null when present=false`,
	} {
		if !strings.Contains(sys, want) {
			t.Errorf("sys prompt missing %q:\n%s", want, sys)
		}
	}
	for _, want := range []string{"TARGET: Dr. Ahmad (human)", "EMP-042", "DESCRIPTION: white coat", "ref_1..ref_3", "CANDIDATE frame"} {
		if !strings.Contains(user, want) {
			t.Errorf("user prompt missing %q:\n%s", want, user)
		}
	}

	// Zero-refs bootstrap variant is extra conservative and never mentions refs.
	sys0, user0 := camAvatarCheckPrompt(camAvatar{Name: "X"}, 0, "Cam", "t", 640, 480)
	if !strings.Contains(sys0, "no reference images yet") || !strings.Contains(sys0, "extra conservative") {
		t.Errorf("zero-ref sys prompt missing bootstrap wording:\n%s", sys0)
	}
	if strings.Contains(sys0, "ref 1..") || strings.Contains(user0, "ref_1") {
		t.Errorf("zero-ref prompts must not promise reference attachments")
	}
	if !strings.Contains(user0, "TARGET: X (human)") {
		t.Errorf("blank type should normalize to human: %q", user0)
	}
}

func TestCamToolAnnotateInvalidBBox(t *testing.T) {
	ctx := context.Background()
	site := camSite{ID: "site1"}
	camByID := map[string]camera{"cam1": {ID: "cam1", SiteID: "site1", DVRID: "d1", Channel: 1, Enabled: true}}
	allowed := map[string]bool{"cam1": true}

	// nil bbox is always invalid (pre- and post-W3 camParseBBox), and the
	// instructive summary fires before any DB/DVR access (nil db proves it).
	res := camToolAnnotate(ctx, config{}, nil, nil, site, investigateArgs{
		CameraIDs: []string{"cam1"}, Time: "2026-07-01T10:00:00Z",
	}, camByID, allowed, t.TempDir())
	if !strings.Contains(res.Summary, "0-1000") || !strings.Contains(res.Summary, "TOP-LEFT") {
		t.Errorf("annotate invalid-bbox summary should restate the convention, got %q", res.Summary)
	}
	if res.Fetches != 0 {
		t.Errorf("invalid bbox must cost nothing, Fetches = %d", res.Fetches)
	}

	// Bad time gets its own instructive message before the bbox check.
	res = camToolAnnotate(ctx, config{}, nil, nil, site, investigateArgs{
		CameraIDs: []string{"cam1"}, Time: "yesterday at nine",
	}, camByID, allowed, t.TempDir())
	if !strings.Contains(res.Summary, "RFC3339") {
		t.Errorf("annotate invalid-time summary = %q", res.Summary)
	}
}
