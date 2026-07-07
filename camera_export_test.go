package main

// camera_export_test.go — unit coverage for the evidence-export subsystem that
// needs NO ffmpeg: the pure chunk/grid/label/overlay helpers, the window/layout
// parsing + clamps, the S3 key layout + URL escaping, the durable store round-
// trip with claim/requeue CAS, the crash-resume predicates, and the
// camera_export_ready payload shape.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestCamExportChunks(t *testing.T) {
	base := time.Date(2026, 7, 7, 10, 0, 0, 0, time.UTC)
	mk := func(secs int) time.Time { return base.Add(time.Duration(secs) * time.Second) }
	tests := []struct {
		name     string
		fromSec  int
		toSec    int
		chunkSec int
		want     [][2]int // [fromSec,toSec] pairs relative to base
	}{
		{"span_below_chunk", 0, 200, 300, [][2]int{{0, 200}}},
		{"span_below_min", 0, 20, 300, [][2]int{{0, 20}}},
		{"exact_multiple", 0, 600, 300, [][2]int{{0, 300}, {300, 600}}},
		{"tail_merges_below_30s", 0, 310, 300, [][2]int{{0, 310}}},
		{"tail_kept_at_50s", 0, 650, 300, [][2]int{{0, 300}, {300, 600}, {600, 650}}},
		{"tail_merges_into_prev", 0, 620, 300, [][2]int{{0, 300}, {300, 620}}},
		{"empty_range", 100, 100, 300, nil},
		{"reversed_range", 200, 100, 300, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := camExportChunks(mk(tc.fromSec), mk(tc.toSec), tc.chunkSec)
			if len(got) != len(tc.want) {
				t.Fatalf("got %d chunks, want %d (%v)", len(got), len(tc.want), got)
			}
			for i, w := range tc.want {
				if !got[i][0].Equal(mk(w[0])) || !got[i][1].Equal(mk(w[1])) {
					t.Errorf("chunk %d = [%v,%v], want [%v,%v]", i, got[i][0], got[i][1], mk(w[0]), mk(w[1]))
				}
			}
		})
	}
}

func TestCamExportGridLayout(t *testing.T) {
	tests := []struct {
		n          int
		cols, rows int
		layout     string
	}{
		{2, 2, 1, "0_0|w0_0"},
		{3, 2, 2, "0_0|w0_0|0_h0|w0_h0"},
		{4, 2, 2, "0_0|w0_0|0_h0|w0_h0"},
		{5, 3, 2, "0_0|w0_0|w0+w1_0|0_h0|w0_h0|w0+w1_h0"},
		{6, 3, 2, "0_0|w0_0|w0+w1_0|0_h0|w0_h0|w0+w1_h0"},
		{7, 3, 3, "0_0|w0_0|w0+w1_0|0_h0|w0_h0|w0+w1_h0|0_h0+h3|w0_h0+h3|w0+w1_h0+h3"},
		{9, 3, 3, "0_0|w0_0|w0+w1_0|0_h0|w0_h0|w0+w1_h0|0_h0+h3|w0_h0+h3|w0+w1_h0+h3"},
	}
	for _, tc := range tests {
		t.Run(strconv.Itoa(tc.n), func(t *testing.T) {
			cols, rows, layout := camExportGridLayout(tc.n)
			if cols != tc.cols || rows != tc.rows {
				t.Errorf("n=%d dims = %dx%d, want %dx%d", tc.n, cols, rows, tc.cols, tc.rows)
			}
			if layout != tc.layout {
				t.Errorf("n=%d layout = %q, want %q", tc.n, layout, tc.layout)
			}
			// The layout must always name exactly cols*rows panes.
			if got := strings.Count(layout, "|") + 1; got != cols*rows {
				t.Errorf("n=%d layout names %d panes, want %d", tc.n, got, cols*rows)
			}
		})
	}
}

func TestCamExportSafeLabel(t *testing.T) {
	tests := []struct{ in, want string }{
		{"Reception", "Reception"},
		{"Cam-1 (Lobby) رقم", "Cam-1 Lobby"},
		{"back'door\"1", "backdoor1"},
		{"  spaced  ", "spaced"},
		{"a:b;c,d", "abcd"},
	}
	for _, tc := range tests {
		if got := camExportSafeLabel(tc.in); got != tc.want {
			t.Errorf("camExportSafeLabel(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestCamExportDrawtextFilters(t *testing.T) {
	// No font → overlay is skipped entirely.
	if got := camExportDrawtextFilters("Reception", 1_700_000_000, 720, ""); got != "" {
		t.Errorf("empty font should skip overlay, got %q", got)
	}
	// With a font → two drawtext filters carrying the label, the epoch, and the
	// escaped gmtime expression.
	got := camExportDrawtextFilters("Reception (main)", 1_700_000_000, 720, "/f.ttf")
	for _, want := range []string{"drawtext", "Reception main", "gmtime", "1700000000", "%F %T"} {
		if !strings.Contains(got, want) {
			t.Errorf("drawtext filter missing %q in %q", want, got)
		}
	}
	if strings.Count(got, "drawtext=") != 2 {
		t.Errorf("expected exactly two drawtext filters, got %q", got)
	}
}

func TestCamExportEpochFor(t *testing.T) {
	loc, err := time.LoadLocation("Asia/Amman")
	if err != nil {
		t.Skipf("tz db unavailable: %v", err)
	}
	// gmtime(epoch) must render the SITE wall clock, i.e. epoch = unix + offset.
	inst := time.Date(2026, 7, 7, 15, 0, 0, 0, loc)
	_, offset := inst.Zone()
	if got, want := camExportEpochFor(inst, loc), inst.Unix()+int64(offset); got != want {
		t.Errorf("camExportEpochFor = %d, want %d", got, want)
	}
}

func TestCamParseExportWindow(t *testing.T) {
	cfg := config{CameraExportMaxSeconds: 3600}
	now := time.Now().UTC()

	// A 2h span is clamped to the 1h export cap (from pulled forward).
	from := now.Add(-2 * time.Hour).Format(time.RFC3339)
	to := now.Add(-1 * time.Hour).Format(time.RFC3339)
	gf, gt, err := camParseExportWindow(from, to, cfg)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if span := gt.Sub(gf); span > time.Hour+time.Second {
		t.Errorf("span %v exceeds the 1h cap", span)
	}

	// Missing endpoints and reversed windows are rejected.
	if _, _, err := camParseExportWindow("", to, cfg); err == nil {
		t.Error("expected error for missing from")
	}
	if _, _, err := camParseExportWindow(to, from, cfg); err == nil {
		t.Error("expected error for reversed window")
	}

	// "to" in the future is clamped to now.
	future := now.Add(48 * time.Hour).Format(time.RFC3339)
	past := now.Add(-10 * time.Minute).Format(time.RFC3339)
	_, gt2, err := camParseExportWindow(past, future, cfg)
	if err != nil {
		t.Fatalf("parse future: %v", err)
	}
	if gt2.After(time.Now().Add(time.Minute)) {
		t.Errorf("to = %v not clamped to now", gt2)
	}
}

func TestCamNormalizeExportLayout(t *testing.T) {
	tests := []struct{ in, want string }{
		{"sequential", "sequential"},
		{"GRID", "grid"},
		{" separate ", "separate"},
		{"", "separate"},
		{"nonsense", "separate"},
	}
	for _, tc := range tests {
		if got := camNormalizeExportLayout(tc.in); got != tc.want {
			t.Errorf("camNormalizeExportLayout(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestCamS3ExportKeyAndEscaping(t *testing.T) {
	saved := cameraCfg.CameraS3Prefix
	defer func() { cameraCfg.CameraS3Prefix = saved }()
	cameraCfg.CameraS3Prefix = ""

	key := camS3ExportKey("site1", "export_abc", "grid.mp4")
	if want := "exports/site1/export_abc/grid.mp4"; key != want {
		t.Errorf("camS3ExportKey = %q, want %q", key, want)
	}

	// A space in a segment is percent-escaped in the public URL, and the "/"
	// separators are preserved.
	cfg := config{CameraS3Bucket: "bucket", CameraS3Endpoint: "hel1.example.com"}
	spaced := camS3ExportKey("site 1", "export_abc", "grid.mp4")
	url := camS3PublicURL(cfg, spaced)
	if !strings.Contains(url, "site%201") {
		t.Errorf("public URL did not escape the space: %q", url)
	}
	if !strings.HasPrefix(url, "https://bucket.hel1.example.com/exports/") {
		t.Errorf("unexpected public URL: %q", url)
	}
}

func TestCamExportRawUsable(t *testing.T) {
	dir := t.TempDir()
	big := filepath.Join(dir, "big.mp4")
	small := filepath.Join(dir, "small.mp4")
	if err := os.WriteFile(big, make([]byte, camMinValidClipBytes+10), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(small, make([]byte, 100), 0o600); err != nil {
		t.Fatal(err)
	}
	if !camExportRawUsable(big) {
		t.Error("big file should be usable")
	}
	if camExportRawUsable(small) {
		t.Error("small file should not be usable")
	}
	if camExportRawUsable(filepath.Join(dir, "missing.mp4")) {
		t.Error("missing file should not be usable")
	}
}

func TestCamExportProgressResume(t *testing.T) {
	// A checkpointed progress JSON round-trips so a crash-requeued run can skip
	// recorded gaps by their chunk key.
	base := time.Date(2026, 7, 7, 10, 0, 0, 0, time.UTC)
	key := camExportChunkKey(base, base.Add(5*time.Minute))
	prog := camExportProgress{
		Stage: "download",
		Cameras: map[string]*camExportCamProgress{
			"cam1": {ChunksDone: 2, ChunksTotal: 3, Gaps: []string{key}},
		},
	}
	raw := mustJSON(&prog)
	got := camExportParseProgress(raw)
	cp := got.Cameras["cam1"]
	if cp == nil || cp.ChunksDone != 2 || cp.ChunksTotal != 3 {
		t.Fatalf("progress did not round-trip: %+v", got)
	}
	gapSet := map[string]bool{}
	for _, g := range cp.Gaps {
		gapSet[g] = true
	}
	if !gapSet[key] {
		t.Errorf("recorded gap %q not resolvable on resume", key)
	}
}

func TestCamExportStoreRoundTripAndClaimCAS(t *testing.T) {
	cfg := config{DBPath: filepath.Join(t.TempDir(), ".proxy.db")}
	db, err := openProxyDB(cfg)
	if err != nil {
		t.Fatalf("openProxyDB: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if err := migrateCameraDB(db); err != nil {
		t.Fatalf("migrateCameraDB: %v", err)
	}

	id, err := insertCameraExport(db, camExport{
		SiteID: "s1", InvestigationID: "inv1", CameraIDs: mustJSON([]string{"c1", "c2"}),
		FromTS: "2026-07-07T10:00:00Z", ToTS: "2026-07-07T11:00:00Z",
		Layout: "grid", Quality: "main", Status: "queued",
	})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	got, err := getCameraExport(db, id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.SiteID != "s1" || got.Layout != "grid" || got.Status != "queued" {
		t.Errorf("round-trip mismatch: %+v", got)
	}
	if ids := camExportParseCameraIDs(got.CameraIDs); len(ids) != 2 || ids[0] != "c1" {
		t.Errorf("camera_ids did not round-trip: %v", ids)
	}

	// listQueued sees it; a site list sees it.
	if q, _ := listQueuedCameraExports(db); len(q) != 1 {
		t.Errorf("listQueued = %d, want 1", len(q))
	}
	if l, _ := listCameraExports(db, "s1"); len(l) != 1 {
		t.Errorf("listCameraExports = %d, want 1", len(l))
	}

	// Claim CAS: first wins, second loses (already running).
	won, err := claimCameraExport(db, id)
	if err != nil || !won {
		t.Fatalf("first claim = %v, %v; want true", won, err)
	}
	won2, _ := claimCameraExport(db, id)
	if won2 {
		t.Error("second claim should lose the CAS")
	}
	if r, _ := getCameraExport(db, id); r.Status != "running" {
		t.Errorf("status = %q, want running", r.Status)
	}

	// Startup requeue resets running → queued.
	n, err := requeueRunningCameraExports(db)
	if err != nil || n != 1 {
		t.Fatalf("requeueRunning = %d, %v; want 1", n, err)
	}
	if r, _ := getCameraExport(db, id); r.Status != "queued" {
		t.Errorf("status = %q, want queued after requeue", r.Status)
	}

	// Outputs + progress writers persist their JSON.
	if err := setCameraExportProgress(db, id, `{"stage":"upload"}`); err != nil {
		t.Fatalf("setProgress: %v", err)
	}
	if err := setCameraExportOutputs(db, id, `[{"label":"grid","s3_url":"https://x/grid.mp4"}]`); err != nil {
		t.Fatalf("setOutputs: %v", err)
	}
	if err := setCameraExportStatus(db, id, "done", ""); err != nil {
		t.Fatalf("setStatus: %v", err)
	}
	final, _ := getCameraExport(db, id)
	if final.Status != "done" || !strings.Contains(final.Outputs, "grid.mp4") || !strings.Contains(final.Progress, "upload") {
		t.Errorf("terminal row mismatch: %+v", final)
	}
}

func TestCamExportReadyPayload(t *testing.T) {
	outs := []camExportOutput{
		{CameraID: "c1", Label: "01_c1", Token: "tok1", S3URL: "https://s3/01.mp4", Bytes: 1234, ContentType: "video/mp4", Caption: "cam 1"},
		{CameraID: "c2", Label: "02_c2", Token: "tok2", S3URL: "https://s3/02.mp4", Bytes: 5678, ContentType: "video/mp4", Caption: "cam 2"},
	}
	prog := camExportProgress{Cameras: map[string]*camExportCamProgress{
		"c2": {Gaps: []string{"2026-07-07T10:00:00Z/2026-07-07T10:05:00Z"}},
	}}
	ex := camExport{
		ID: "export_x", SiteID: "s1", Layout: "separate", Status: "done",
		FromTS: "2026-07-07T10:00:00Z", ToTS: "2026-07-07T11:00:00Z",
		Outputs: mustJSON(outs), Progress: mustJSON(&prog),
	}
	at := time.Date(2026, 7, 7, 12, 0, 0, 0, time.UTC)
	p := camExportReadyPayload(ex, at)

	if p["event"] != "camera_export_ready" {
		t.Errorf("event = %v", p["event"])
	}
	if p["export_id"] != "export_x" || p["site_id"] != "s1" || p["status"] != "done" || p["layout"] != "separate" {
		t.Errorf("scalar fields mismatch: %+v", p)
	}
	links, ok := p["links"].([]map[string]any)
	if !ok || len(links) != 2 {
		t.Fatalf("links = %v, want 2", p["links"])
	}
	if links[0]["url"] != "https://s3/01.mp4" || links[0]["bytes"].(int64) != 1234 {
		t.Errorf("link 0 mismatch: %+v", links[0])
	}
	gaps, ok := p["gaps"].([]map[string]any)
	if !ok || len(gaps) != 1 {
		t.Fatalf("gaps = %v, want 1", p["gaps"])
	}
	if gaps[0]["camera_id"] != "c2" || gaps[0]["from"] != "2026-07-07T10:00:00Z" || gaps[0]["to"] != "2026-07-07T10:05:00Z" {
		t.Errorf("gap mismatch: %+v", gaps[0])
	}
	// The whole payload must marshal cleanly (webhook body).
	if _, err := json.Marshal(p); err != nil {
		t.Fatalf("payload not JSON-marshalable: %v", err)
	}
}

func TestCamExportResolverClamps(t *testing.T) {
	// Budget clamps to the 4h ceiling; max-seconds clamps to 4h; cameras clamp to
	// the grid hard cap.
	if got := camExportBudget(config{CameraExportBudget: 24 * time.Hour}); got != 4*time.Hour {
		t.Errorf("budget clamp = %v, want 4h", got)
	}
	if got := camExportBudget(config{}); got != 2*time.Hour {
		t.Errorf("budget default = %v, want 2h", got)
	}
	if got := camExportMaxSeconds(config{CameraExportMaxSeconds: 999999}); got != 4*3600 {
		t.Errorf("maxSeconds clamp = %d, want 14400", got)
	}
	if got := camExportMaxCameras(config{CameraExportMaxCameras: 50}); got != camExportGridHardCap {
		t.Errorf("maxCameras clamp = %d, want %d", got, camExportGridHardCap)
	}
}
