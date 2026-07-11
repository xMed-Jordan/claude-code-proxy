package main

// camera_observer_tool_test.go — the `activity_log` Ask-AI tool
// (camera_observer_tool.go): granularity routing (entries/hours/days + the
// invalid-granularity refusal), window/camera-filter validation, the
// entries-cap truncation note, the verbatim-vs-headlines threshold, the
// motion-gated marker, hour/day status rendering, and the helpful empty
// messages. Pure store + formatting — no DVRs, no model calls, no network,
// and Fetches must always be 0 (reading the journal costs no media budget).

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// obsToolSeed builds the canonical tool fixture: a site with one UTC DVR and
// two cameras, plus the allowed/camByID/dvrByID maps the dispatcher passes.
func obsToolSeed(t *testing.T) (site camSite, allowed map[string]bool, camByID map[string]camera, dvrByID map[string]CamDVR) {
	t.Helper()
	site = camSite{ID: "siteA", Name: "Clinic"}
	camA := camera{ID: "cam_a", SiteID: "siteA", DVRID: "dvr1", Name: "Gate", Enabled: true}
	camB := camera{ID: "cam_b", SiteID: "siteA", DVRID: "dvr1", Name: "Reception", Enabled: true}
	allowed = map[string]bool{"cam_a": true, "cam_b": true}
	camByID = map[string]camera{"cam_a": camA, "cam_b": camB}
	dvrByID = map[string]CamDVR{"dvr1": {ID: "dvr1", Timezone: "UTC"}} // deterministic clock rendering
	return site, allowed, camByID, dvrByID
}

func TestCamToolActivityLogEntries(t *testing.T) {
	db := openCamTestDB(t)
	if err := migrateCameraDB(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	site, allowed, camByID, dvrByID := obsToolSeed(t)

	base := time.Date(2020, 1, 15, 5, 0, 0, 0, time.UTC)
	seed := []struct {
		offsetMin int
		camIDs    string
		headline  string
		report    string
		gated     bool
	}{
		{0, `["cam_a"]`, "Courier at the gate", "A courier hands a package to the guard at the gate.", false},
		{5, `["cam_a","cam_b"]`, camObserverIdleHeadline, "idle window", true},
		{10, `["cam_b"]`, "Two visitors in reception", "Two visitors wait on the reception sofas.", false},
	}
	for _, s := range seed {
		if _, err := insertLogEntry(db, logEntry{
			StreamID: "logstream_x", SiteID: site.ID,
			FromTS:    base.Add(time.Duration(s.offsetMin) * time.Minute).Format(time.RFC3339),
			ToTS:      base.Add(time.Duration(s.offsetMin+5) * time.Minute).Format(time.RFC3339),
			CameraIDs: s.camIDs, Headline: s.headline, Report: s.report, MotionGated: s.gated,
		}); err != nil {
			t.Fatalf("insertLogEntry: %v", err)
		}
	}

	args := investigateArgs{From: "2020-01-15T00:00:00Z", To: "2020-01-15T23:00:00Z"}
	res := camToolActivityLog(config{}, db, site, args, allowed, camByID, dvrByID)
	if res.Fetches != 0 || len(res.Media) != 0 || len(res.Images) != 0 {
		t.Fatalf("activity_log must be text-only with zero media cost: %+v", res)
	}
	// 3 rows ≤ verbatim threshold → headlines AND full reports, oldest first,
	// idle row marked, site-local (UTC) window labels.
	for _, want := range []string{
		"3 journal entries", "oldest first",
		"[2020-01-15 05:00–05:05] Courier at the gate",
		"A courier hands a package to the guard at the gate.",
		camObserverIdleHeadline + " (motion-gated)",
		"Two visitors in reception",
		"Two visitors wait on the reception sofas.",
	} {
		if !strings.Contains(res.Summary, want) {
			t.Errorf("summary missing %q:\n%s", want, res.Summary)
		}
	}
	if strings.Index(res.Summary, "Courier at the gate") > strings.Index(res.Summary, "Two visitors in reception") {
		t.Errorf("entries must render oldest first:\n%s", res.Summary)
	}

	// Camera filter narrows to windows that sampled that camera.
	res = camToolActivityLog(config{}, db, site, investigateArgs{
		From: "2020-01-15T00:00:00Z", To: "2020-01-15T23:00:00Z", CameraIDs: []string{"cam_a"},
	}, allowed, camByID, dvrByID)
	if !strings.Contains(res.Summary, "2 journal entries") || !strings.Contains(res.Summary, "Gate") {
		t.Errorf("cam_a filter should keep 2 entries scoped to Gate:\n%s", res.Summary)
	}
	if strings.Contains(res.Summary, "Two visitors in reception") {
		t.Errorf("cam_a filter leaked a cam_b-only entry:\n%s", res.Summary)
	}

	// An explicit but unknown camera id is refused (roster discipline).
	res = camToolActivityLog(config{}, db, site, investigateArgs{
		From: "2020-01-15T00:00:00Z", To: "2020-01-15T23:00:00Z", CameraIDs: []string{"cam_evil"},
	}, allowed, camByID, dvrByID)
	if !strings.Contains(res.Summary, "no valid camera_ids") {
		t.Errorf("unknown camera id should be refused:\n%s", res.Summary)
	}

	// Empty window → the helpful pointer, not a bare miss.
	res = camToolActivityLog(config{}, db, site, investigateArgs{
		From: "2019-01-01T00:00:00Z", To: "2019-01-02T00:00:00Z",
	}, allowed, camByID, dvrByID)
	if !strings.Contains(res.Summary, "no journal entries recorded") || !strings.Contains(res.Summary, "motion_search") {
		t.Errorf("empty window needs the helpful fallback message:\n%s", res.Summary)
	}

	// Bad granularity and bad window are clean refusals.
	res = camToolActivityLog(config{}, db, site, investigateArgs{
		From: "2020-01-15T00:00:00Z", To: "2020-01-15T23:00:00Z", Granularity: "weeks",
	}, allowed, camByID, dvrByID)
	if !strings.Contains(res.Summary, "granularity must be") {
		t.Errorf("bad granularity should be refused:\n%s", res.Summary)
	}
	res = camToolActivityLog(config{}, db, site, investigateArgs{From: "not-a-time", To: "2020-01-15T23:00:00Z"}, allowed, camByID, dvrByID)
	if !strings.Contains(res.Summary, "activity_log:") || !strings.Contains(res.Summary, "from") {
		t.Errorf("bad window should surface the camParseWindow error:\n%s", res.Summary)
	}
}

// TestCamToolActivityLogEntriesCapAndHeadlines: above the verbatim threshold
// reports are not quoted, and past the 200-row cap the truncation note appears
// while the NEWEST rows of the window are kept.
func TestCamToolActivityLogEntriesCapAndHeadlines(t *testing.T) {
	db := openCamTestDB(t)
	if err := migrateCameraDB(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	site, allowed, camByID, dvrByID := obsToolSeed(t)

	base := time.Date(2020, 1, 15, 0, 0, 0, 0, time.UTC)
	for i := 0; i < camActivityToolMaxRows+5; i++ {
		if _, err := insertLogEntry(db, logEntry{
			StreamID: "logstream_x", SiteID: site.ID,
			FromTS:    base.Add(time.Duration(i) * time.Minute).Format(time.RFC3339),
			ToTS:      base.Add(time.Duration(i+1) * time.Minute).Format(time.RFC3339),
			CameraIDs: `["cam_a"]`, Headline: fmt.Sprintf("window %03d", i), Report: fmt.Sprintf("report body %03d", i),
		}); err != nil {
			t.Fatalf("insertLogEntry %d: %v", i, err)
		}
	}

	res := camToolActivityLog(config{}, db, site, investigateArgs{
		From: "2020-01-15T00:00:00Z", To: "2020-01-15T23:00:00Z",
	}, allowed, camByID, dvrByID)
	if !strings.Contains(res.Summary, fmt.Sprintf("%d journal entries", camActivityToolMaxRows)) {
		t.Errorf("summary should carry the capped count:\n%.400s", res.Summary)
	}
	if !strings.Contains(res.Summary, "only the most recent") {
		t.Errorf("summary missing the truncation note:\n%.400s", res.Summary)
	}
	// The newest rows survive the cap (the oldest 5 are dropped).
	if strings.Contains(res.Summary, "window 000") || !strings.Contains(res.Summary, fmt.Sprintf("window %03d", camActivityToolMaxRows+4)) {
		t.Errorf("cap must keep the newest rows of the window:\n%.400s", res.Summary)
	}
	// >10 rows → headlines only, with the how-to-read-more hint.
	if strings.Contains(res.Summary, "report body") || !strings.Contains(res.Summary, "Headlines only") {
		t.Errorf("large result must not quote reports:\n%.400s", res.Summary)
	}
}

func TestCamToolActivityLogHours(t *testing.T) {
	db := openCamTestDB(t)
	if err := migrateCameraDB(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	site, allowed, camByID, dvrByID := obsToolSeed(t)

	mkHour := func(hourStart, status, summary, errMsg string) {
		t.Helper()
		h := logHour{StreamID: "logstream_x", SiteID: site.ID, HourStart: hourStart, TZ: "UTC"}
		id, won, err := claimLogHour(db, h)
		if err != nil || !won {
			t.Fatalf("claimLogHour(%s): won=%v err=%v", hourStart, won, err)
		}
		h.ID, h.Status, h.Summary, h.Language, h.Error = id, status, summary, "Arabic", errMsg
		if err := finishLogHour(db, h); err != nil {
			t.Fatalf("finishLogHour: %v", err)
		}
	}
	mkHour("2020-01-15T05:00:00Z", "done", "ملخص الساعة الخامسة", "")
	mkHour("2020-01-15T06:00:00Z", "empty", "", "")
	mkHour("2020-01-15T07:00:00Z", "error", "", "model exploded")

	res := camToolActivityLog(config{}, db, site, investigateArgs{
		From: "2020-01-15T00:00:00Z", To: "2020-01-15T23:00:00Z", Granularity: "hours",
	}, allowed, camByID, dvrByID)
	if res.Fetches != 0 {
		t.Fatalf("hours granularity must cost no media budget: %d", res.Fetches)
	}
	for _, want := range []string{
		"3 hourly summaries",
		"[2020-01-15 05:00–06:00] ملخص الساعة الخامسة", // verbatim, report language preserved
		"[2020-01-15 06:00–07:00] (no journal entries — quiet hour)",
		"rollup failed: model exploded",
	} {
		if !strings.Contains(res.Summary, want) {
			t.Errorf("summary missing %q:\n%s", want, res.Summary)
		}
	}

	// No rollups in range → helpful pointer to entries.
	res = camToolActivityLog(config{}, db, site, investigateArgs{
		From: "2019-01-01T00:00:00Z", To: "2019-01-02T00:00:00Z", Granularity: "hours",
	}, allowed, camByID, dvrByID)
	if !strings.Contains(res.Summary, "no hourly summaries exist") || !strings.Contains(res.Summary, `"entries"`) {
		t.Errorf("empty hours needs the fallback hint:\n%s", res.Summary)
	}
}

func TestCamToolActivityLogDays(t *testing.T) {
	db := openCamTestDB(t)
	if err := migrateCameraDB(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	site, allowed, camByID, dvrByID := obsToolSeed(t)

	mkDay := func(day, status, headline, report string) {
		t.Helper()
		d := logDay{StreamID: "logstream_x", SiteID: site.ID, Day: day}
		id, won, err := claimLogDay(db, d)
		if err != nil || !won {
			t.Fatalf("claimLogDay(%s): won=%v err=%v", day, won, err)
		}
		d.ID, d.Status, d.Headline, d.Report, d.Language = id, status, headline, report, "Arabic"
		if err := finishLogDay(db, d); err != nil {
			t.Fatalf("finishLogDay: %v", err)
		}
	}
	mkDay("2020-01-14", "done", "يوم هادئ", "تقرير اليوم الكامل هنا.")
	mkDay("2020-01-15", "empty", "", "")

	res := camToolActivityLog(config{}, db, site, investigateArgs{
		From: "2020-01-14T00:00:00Z", To: "2020-01-15T23:00:00Z", Granularity: "days",
	}, allowed, camByID, dvrByID)
	for _, want := range []string{
		"2 daily reports",
		"- 2020-01-14: يوم هادئ",
		"تقرير اليوم الكامل هنا.", // verbatim
		"- 2020-01-15: no activity recorded.",
	} {
		if !strings.Contains(res.Summary, want) {
			t.Errorf("summary missing %q:\n%s", want, res.Summary)
		}
	}
	if strings.Index(res.Summary, "2020-01-14") > strings.Index(res.Summary, "2020-01-15") {
		t.Errorf("days must render oldest first:\n%s", res.Summary)
	}

	res = camToolActivityLog(config{}, db, site, investigateArgs{
		From: "2019-01-01T00:00:00Z", To: "2019-01-02T00:00:00Z", Granularity: "days",
	}, allowed, camByID, dvrByID)
	if !strings.Contains(res.Summary, "no daily reports exist") {
		t.Errorf("empty days needs the fallback hint:\n%s", res.Summary)
	}
}

// TestActivityLogToolWiring pins the five investigate-side touchpoints: the
// tool-name list (parse validation), the lead prompt's tool line + OUTPUT enum
// + granularity arg, the dispatcher, and the sub-agent ceiling + tool line.
func TestActivityLogToolWiring(t *testing.T) {
	found := false
	for _, n := range investigateToolNames {
		if n == "activity_log" {
			found = true
		}
	}
	if !found {
		t.Fatal("activity_log missing from investigateToolNames")
	}

	act, err := parseInvestigateAction(`{"thought":"x","action":{"type":"call_tool","tool":"activity_log","args":{"from":"2020-01-01T00:00:00Z","to":"2020-01-01T01:00:00Z","granularity":"hours"}}}`)
	if err != nil {
		t.Fatalf("parseInvestigateAction: %v", err)
	}
	if act.Action.Tool != "activity_log" || act.Action.Args.Granularity != "hours" {
		t.Fatalf("parsed action = %+v, want activity_log with granularity hours", act.Action)
	}

	active := []camera{{ID: "cam1", Name: "Front Door", Enabled: true}}
	prompt := camInvestigateSystemPrompt(camSite{Name: "Clinic"}, nil, active, nil, nil, nil, time.Now(), time.Time{}, false, "UTC", 0)
	for _, want := range []string{"- activity_log:", "motion_search|activity_log|playbook", `"granularity":"entries|hours|days"`} {
		if !strings.Contains(prompt, want) {
			t.Errorf("lead prompt missing %q", want)
		}
	}

	if !camSubagentAllowedTools["activity_log"] {
		t.Error("activity_log must be in the sub-agent tool ceiling")
	}
	if line := camSubagentToolLine("activity_log"); !strings.Contains(line, "activity_log") || !strings.Contains(line, "granularity") {
		t.Errorf("sub-agent tool line wrong: %q", line)
	}
}
