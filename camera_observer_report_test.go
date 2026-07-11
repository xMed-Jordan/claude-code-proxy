package main

// camera_observer_report_test.go — the hourly/daily rollups + the daily-report
// webhook (camera_observer_report.go), driven through camObserverStreamRollups
// with a fixed `now` (the DVR's Asia/Amman timezone is the fake site clock)
// and the camObserverAnalyzeFn / camObserverNotifyDailyReportFn seams scripted
// (swap+defer-restore, the runner-test pattern):
//
//   - hourly idempotency: one non-empty hour → ONE pending-claimed row settled
//     "done" with ONE text-only analyze call; a second identical pass adds no
//     rows and no calls; an entry-less hour settles "empty" with NO call.
//   - the hourly prompt compresses idle runs, quotes active reports verbatim,
//     and carries the language directive — Arabic when the site policy sets
//     report_language, English by default — while the journal-entry system
//     prompt keeps its English-always mandate.
//   - the daily report fires only at/after daily_report_time SITE-LOCAL time,
//     and the scheduled tick takes the notified_at CAS exactly once — a report
//     generated early through the on-demand path (which never webhooks) is
//     still delivered at report time, and later ticks never re-deliver.
//   - hourly claim race: concurrent rollup passes produce one row + one call.
//   - camNotifyDailyReport posts the documented payload with the Bearer +
//     HMAC signature to the registered callback (httptest).
//   - the generate path keeps id + view_token across a forced rebuild (the
//     share link survives) while the sync seq moves, and an unforced generate
//     over an existing row is a free no-op.
//
// Temp sqlite only; the only network is the local httptest callback receiver.

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// obsRollupNow is the canonical fixed instant for hourly tests: 09:30 UTC =
// 12:30 in Asia/Amman, so the newest settled local hour is 11:00–12:00 local
// (08:00–09:00 UTC) — comfortably before the default 20:00 report time.
var obsRollupNow = time.Date(2026, 7, 11, 9, 30, 0, 0, time.UTC)

// obsSeedHourEntries inserts the canonical four-entry hour (active, idle ×2,
// active) into 08:00–09:00 UTC for the stream: 4 entries, 2 motion-gated.
func obsSeedHourEntries(t *testing.T, cfg config, siteID, streamID string) {
	t.Helper()
	db, err := openProxyDB(cfg)
	if err != nil {
		t.Fatalf("openProxyDB: %v", err)
	}
	defer db.Close()
	base := time.Date(2026, 7, 11, 8, 0, 0, 0, time.UTC)
	rows := []struct {
		offsetMin int
		headline  string
		report    string
		gated     bool
	}{
		{0, "Crew painting the corridor", "Two painters actively work in the corridor.", false},
		{5, camObserverIdleHeadline, "idle", true},
		{10, camObserverIdleHeadline, "idle", true},
		{15, "Visitor at the gate", "A visitor waits at the gate.", false},
	}
	for _, r := range rows {
		if _, err := insertLogEntry(db, logEntry{
			StreamID: streamID, SiteID: siteID,
			FromTS:      base.Add(time.Duration(r.offsetMin) * time.Minute).Format(time.RFC3339),
			ToTS:        base.Add(time.Duration(r.offsetMin+5) * time.Minute).Format(time.RFC3339),
			CameraIDs:   "[]",
			Headline:    r.headline,
			Report:      r.report,
			MotionGated: r.gated,
		}); err != nil {
			t.Fatalf("insertLogEntry: %v", err)
		}
	}
}

// obsRunStreamRollups runs one rollup pass for the stream at a fixed instant.
func obsRunStreamRollups(t *testing.T, cfg config, streamID string, now time.Time) {
	t.Helper()
	db, err := openProxyDB(cfg)
	if err != nil {
		t.Fatalf("openProxyDB: %v", err)
	}
	defer db.Close()
	s, err := getLogStream(db, streamID)
	if err != nil {
		t.Fatalf("getLogStream: %v", err)
	}
	camObserverStreamRollups(cfg, db, s, now)
}

// obsListHours lists the stream's hour rows (newest first).
func obsListHours(t *testing.T, cfg config, siteID, streamID string) []logHour {
	t.Helper()
	db, err := openProxyDB(cfg)
	if err != nil {
		t.Fatalf("openProxyDB: %v", err)
	}
	defer db.Close()
	hours, err := listLogHoursBySite(db, siteID, streamID, "", "", 0)
	if err != nil {
		t.Fatalf("listLogHoursBySite: %v", err)
	}
	return hours
}

// TestObserverHourlyRollup: the non-empty settled hour is claimed once and
// summarized with ONE text-only call (idle runs compressed, active reports
// verbatim, English directive by default); the entry-less hour settles
// "empty" with no call; an identical second pass is a no-op (idempotency);
// and the daily report does NOT fire at 12:30 local.
func TestObserverHourlyRollup(t *testing.T) {
	cfg := newCamObserverTestConfig(t)
	cfg.CameraObserverRollupCatchup = 2 // hours 11:00–12:00 (entries) + 10:00–11:00 (empty) local
	siteID, _ := seedObserverSite(t, cfg)
	s := seedObserverStream(t, cfg, logStream{SiteID: siteID, Name: "Journal", Enabled: true})
	obsSeedHourEntries(t, cfg, siteID, s.ID)

	origAnalyze := camObserverAnalyzeFn
	defer func() { camObserverAnalyzeFn = origAnalyze }()
	analyzeCalls := 0
	var sysSeen, userSeen string
	var imagesSeen []string
	camObserverAnalyzeFn = func(ctx context.Context, c config, alias, sys, user string, images []string) (string, []camAnalyzeAttempt, error) {
		analyzeCalls++
		sysSeen, userSeen, imagesSeen = sys, user, images
		return "A quiet late morning: the painters kept working, one visitor at the gate.", nil, nil
	}

	obsRunStreamRollups(t, cfg, s.ID, obsRollupNow)

	if analyzeCalls != 1 {
		t.Fatalf("analyze calls = %d, want exactly 1 (only the non-empty hour)", analyzeCalls)
	}
	if len(imagesSeen) != 0 {
		t.Errorf("rollup attached %d images, want text-only", len(imagesSeen))
	}
	hours := obsListHours(t, cfg, siteID, s.ID)
	if len(hours) != 2 {
		t.Fatalf("hour rows = %d, want 2 (done + empty)", len(hours))
	}
	byStart := map[string]logHour{}
	for _, h := range hours {
		byStart[h.HourStart] = h
	}
	done, ok := byStart["2026-07-11T08:00:00Z"]
	if !ok || done.Status != "done" {
		t.Fatalf("entry hour = %+v, want status done at 08:00Z", byStart)
	}
	if done.EntryCount != 4 || done.GatedCount != 2 {
		t.Errorf("entry hour counts = %d/%d, want 4 entries / 2 gated", done.EntryCount, done.GatedCount)
	}
	if done.Summary != "A quiet late morning: the painters kept working, one visitor at the gate." {
		t.Errorf("summary = %q, want the model's text", done.Summary)
	}
	if done.Language != "English" || done.TZ != "Asia/Amman" {
		t.Errorf("language/tz = %q/%q, want English/Asia/Amman", done.Language, done.TZ)
	}
	empty, ok := byStart["2026-07-11T07:00:00Z"]
	if !ok || empty.Status != "empty" || empty.Summary != "" || empty.EntryCount != 0 {
		t.Fatalf("empty hour = %+v, want a settled empty row", empty)
	}

	if !strings.Contains(sysSeen, "in English") {
		t.Errorf("system prompt missing the default English directive:\n%s", sysSeen)
	}
	if !strings.Contains(userSeen, "HOUR: 2026-07-11 11:00–12:00 (Asia/Amman)") {
		t.Errorf("user prompt missing the site-local hour label:\n%s", userSeen)
	}
	if !strings.Contains(userSeen, "(2 idle cycles)") {
		t.Errorf("user prompt missing the compressed idle run:\n%s", userSeen)
	}
	if strings.Count(userSeen, camObserverIdleHeadline) != 1 {
		t.Errorf("idle headline should appear exactly once (compressed):\n%s", userSeen)
	}
	for _, want := range []string{"Crew painting the corridor", "Two painters actively work in the corridor.", "A visitor waits at the gate."} {
		if !strings.Contains(userSeen, want) {
			t.Errorf("user prompt missing %q (active entries ride verbatim)", want)
		}
	}

	// Idempotency: the same pass again adds no rows and burns no calls.
	obsRunStreamRollups(t, cfg, s.ID, obsRollupNow)
	if analyzeCalls != 1 {
		t.Errorf("analyze calls after re-run = %d, want still 1", analyzeCalls)
	}
	if again := obsListHours(t, cfg, siteID, s.ID); len(again) != 2 {
		t.Errorf("hour rows after re-run = %d, want still 2", len(again))
	}

	// 12:30 local is before the 20:00 report time — no daily row yet.
	db, err := openProxyDB(cfg)
	if err != nil {
		t.Fatalf("openProxyDB: %v", err)
	}
	defer db.Close()
	if _, derr := getLogDayByStreamDay(db, s.ID, "2026-07-11"); !errors.Is(derr, sql.ErrNoRows) {
		t.Errorf("daily row before report time: err = %v, want ErrNoRows", derr)
	}
}

// TestObserverRollupLanguageDirective: report_language=Arabic reaches the
// hourly AND daily rollup prompts, while the journal-entry system prompt keeps
// its English-always mandate — entries are memory, rollups are for people.
func TestObserverRollupLanguageDirective(t *testing.T) {
	cfg := newCamObserverTestConfig(t)
	cfg.CameraObserverRollupCatchup = 1
	siteID, _ := seedObserverSite(t, cfg)
	s := seedObserverStream(t, cfg, logStream{SiteID: siteID, Name: "Journal", Enabled: true})
	obsSeedHourEntries(t, cfg, siteID, s.ID)

	db, err := openProxyDB(cfg)
	if err != nil {
		t.Fatalf("openProxyDB: %v", err)
	}
	defer db.Close()
	if err := updateCameraSitePolicy(db, siteID, camSitePolicyJSON(camSitePolicy{ReportLanguage: "Arabic"})); err != nil {
		t.Fatalf("updateCameraSitePolicy: %v", err)
	}

	origAnalyze := camObserverAnalyzeFn
	defer func() { camObserverAnalyzeFn = origAnalyze }()
	var hourSys, daySys string
	camObserverAnalyzeFn = func(ctx context.Context, c config, alias, sys, user string, images []string) (string, []camAnalyzeAttempt, error) {
		if strings.Contains(user, "DAILY REPORT WINDOW") {
			daySys = sys
			return `{"headline":"يوم هادئ","report":"مر اليوم بهدوء."}`, nil, nil
		}
		hourSys = sys
		return "ساعة هادئة في الموقع.", nil, nil
	}
	origNotify := camObserverNotifyDailyReportFn
	defer func() { camObserverNotifyDailyReportFn = origNotify }()
	camObserverNotifyDailyReportFn = func(c config, dayID string) {} // delivery has its own tests

	obsRunStreamRollups(t, cfg, s.ID, obsRollupNow)
	// 20:30 local: the daily fires too (same seeded entries feed it).
	obsRunStreamRollups(t, cfg, s.ID, time.Date(2026, 7, 11, 17, 30, 0, 0, time.UTC))

	if !strings.Contains(hourSys, "in Arabic") {
		t.Errorf("hourly system prompt missing the Arabic directive:\n%s", hourSys)
	}
	if !strings.Contains(daySys, "in Arabic") {
		t.Errorf("daily system prompt missing the Arabic directive:\n%s", daySys)
	}
	var doneHours []logHour
	for _, h := range obsListHours(t, cfg, siteID, s.ID) {
		if h.Status == "done" {
			doneHours = append(doneHours, h)
		}
	}
	if len(doneHours) != 1 || doneHours[0].Language != "Arabic" || doneHours[0].Summary != "ساعة هادئة في الموقع." {
		t.Fatalf("done hour rows = %+v, want the one Arabic summary", doneHours)
	}
	d, derr := getLogDayByStreamDay(db, s.ID, "2026-07-11")
	if derr != nil || d.Language != "Arabic" || d.Headline != "يوم هادئ" {
		t.Fatalf("day row = %+v, %v; want the Arabic daily report", d, derr)
	}

	// The journal ENTRY prompt is untouched by report_language: still English.
	site, serr := getCameraSite(db, siteID)
	if serr != nil {
		t.Fatalf("getCameraSite: %v", serr)
	}
	entrySys := camObserverSystemPrompt(site, nil, nil, nil, camParseSitePolicy(site.PolicyJSON), nil, obsRollupNow, time.UTC, "UTC", 3)
	if !strings.Contains(entrySys, "ENGLISH") {
		t.Errorf("entry system prompt lost its English mandate:\n%s", entrySys)
	}
	if strings.Contains(entrySys, "in Arabic") {
		t.Errorf("entry system prompt must not carry the rollup language directive:\n%s", entrySys)
	}
}

// TestObserverHourlyClaimRace: concurrent rollup passes over the same missing
// hour produce exactly ONE row and ONE analyze call — the pending-INSERT claim
// (unique (stream,hour) index) picks a single winner.
func TestObserverHourlyClaimRace(t *testing.T) {
	cfg := newCamObserverTestConfig(t)
	cfg.CameraObserverRollupCatchup = 1
	siteID, _ := seedObserverSite(t, cfg)
	s := seedObserverStream(t, cfg, logStream{SiteID: siteID, Name: "Raced", Enabled: true})
	obsSeedHourEntries(t, cfg, siteID, s.ID)

	origAnalyze := camObserverAnalyzeFn
	defer func() { camObserverAnalyzeFn = origAnalyze }()
	var analyzeCalls atomic.Int32
	camObserverAnalyzeFn = func(ctx context.Context, c config, alias, sys, user string, images []string) (string, []camAnalyzeAttempt, error) {
		analyzeCalls.Add(1)
		time.Sleep(50 * time.Millisecond) // widen the race window
		return "One hour, one summary.", nil, nil
	}

	const racers = 4
	var wg sync.WaitGroup
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			obsRunStreamRollups(t, cfg, s.ID, obsRollupNow)
		}()
	}
	wg.Wait()

	if got := analyzeCalls.Load(); got != 1 {
		t.Errorf("analyze calls = %d, want exactly 1 across %d racers", got, racers)
	}
	if hours := obsListHours(t, cfg, siteID, s.ID); len(hours) != 1 || hours[0].Status != "done" {
		t.Errorf("hour rows = %+v, want one done row", hours)
	}
}

// TestObserverDailyTimingAndExactlyOnceNotify: the daily report fires only
// once the SITE-LOCAL clock (fake: the DVR's Asia/Amman timezone) passes
// daily_report_time; a report generated EARLY through the on-demand path is
// not webhooked then, but the scheduled tick delivers it at report time via
// the notified_at CAS — and never a second time.
func TestObserverDailyTimingAndExactlyOnceNotify(t *testing.T) {
	cfg := newCamObserverTestConfig(t)
	cfg.CameraObserverRollupCatchup = 1
	siteID, _ := seedObserverSite(t, cfg)
	s := seedObserverStream(t, cfg, logStream{SiteID: siteID, Name: "Daily", Enabled: true})
	obsSeedHourEntries(t, cfg, siteID, s.ID)

	origAnalyze := camObserverAnalyzeFn
	defer func() { camObserverAnalyzeFn = origAnalyze }()
	camObserverAnalyzeFn = func(ctx context.Context, c config, alias, sys, user string, images []string) (string, []camAnalyzeAttempt, error) {
		if strings.Contains(user, "DAILY REPORT WINDOW") {
			return `{"headline":"Painters and one visitor","report":"The crew painted through the morning; a visitor called at the gate."}`, nil, nil
		}
		return "Morning summary.", nil, nil
	}
	origNotify := camObserverNotifyDailyReportFn
	defer func() { camObserverNotifyDailyReportFn = origNotify }()
	var notifies atomic.Int32
	notified := make(chan string, 8)
	camObserverNotifyDailyReportFn = func(c config, dayID string) {
		notifies.Add(1)
		notified <- dayID
	}

	db, err := openProxyDB(cfg)
	if err != nil {
		t.Fatalf("openProxyDB: %v", err)
	}
	defer db.Close()

	// Generate the report EARLY through the on-demand path (the operator's
	// afternoon button-press): the row is built, but nothing is webhooked —
	// only the scheduled path notifies.
	early, gerr := camObserverGenerateLogDay(cfg, db, s, "2026-07-11", false)
	if gerr != nil {
		t.Fatalf("camObserverGenerateLogDay: %v", gerr)
	}
	if early.Status != "done" || early.Headline != "Painters and one visitor" {
		t.Fatalf("early generate = %+v, want the built report", early)
	}
	if early.NotifiedAt != "" || notifies.Load() != 0 {
		t.Fatalf("on-demand generate must never webhook (notified_at=%q, notifies=%d)", early.NotifiedAt, notifies.Load())
	}

	// 19:00 local (16:00Z): before the 20:00 report time — no delivery.
	obsRunStreamRollups(t, cfg, s.ID, time.Date(2026, 7, 11, 16, 0, 0, 0, time.UTC))
	if notifies.Load() != 0 {
		t.Fatalf("notified before report time: %d", notifies.Load())
	}

	// 20:30 local (17:30Z): the scheduled tick takes the CAS and delivers ONCE.
	obsRunStreamRollups(t, cfg, s.ID, time.Date(2026, 7, 11, 17, 30, 0, 0, time.UTC))
	select {
	case id := <-notified:
		if id != early.ID {
			t.Errorf("notified report id = %q, want the early-generated row %q", id, early.ID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("scheduled tick did not deliver the daily report")
	}
	d, derr := getLogDayByStreamDay(db, s.ID, "2026-07-11")
	if derr != nil || d.ID != early.ID || d.NotifiedAt == "" {
		t.Fatalf("day row after delivery = %+v, %v; want same row with notified_at stamped", d, derr)
	}

	// Later ticks never re-deliver — the CAS already settled.
	obsRunStreamRollups(t, cfg, s.ID, time.Date(2026, 7, 11, 18, 0, 0, 0, time.UTC))
	obsRunStreamRollups(t, cfg, s.ID, time.Date(2026, 7, 11, 18, 1, 0, 0, time.UTC))
	time.Sleep(50 * time.Millisecond)
	if got := notifies.Load(); got != 1 {
		t.Errorf("notifies = %d, want exactly 1", got)
	}

	// The window covers site-local 00:00 → 20:00.
	if d.FromTS != "2026-07-10T21:00:00Z" || d.ToTS != "2026-07-11T17:00:00Z" {
		t.Errorf("day window = %s – %s, want local midnight → 20:00 (Asia/Amman)", d.FromTS, d.ToTS)
	}
}

// TestObserverGenerateKeepsIdentity: an unforced generate over an existing row
// is a free no-op; a forced regenerate rebuilds content on the SAME row — id
// and view_token survive (the share link keeps working) while the sync seq
// moves so Connect re-ingests it.
func TestObserverGenerateKeepsIdentity(t *testing.T) {
	cfg := newCamObserverTestConfig(t)
	siteID, _ := seedObserverSite(t, cfg)
	s := seedObserverStream(t, cfg, logStream{SiteID: siteID, Name: "Regen", Enabled: true})
	obsSeedHourEntries(t, cfg, siteID, s.ID)

	origAnalyze := camObserverAnalyzeFn
	defer func() { camObserverAnalyzeFn = origAnalyze }()
	analyzeCalls := 0
	camObserverAnalyzeFn = func(ctx context.Context, c config, alias, sys, user string, images []string) (string, []camAnalyzeAttempt, error) {
		analyzeCalls++
		return fmt.Sprintf(`{"headline":"Version %d","report":"Report build %d."}`, analyzeCalls, analyzeCalls), nil, nil
	}
	origNotify := camObserverNotifyDailyReportFn
	defer func() { camObserverNotifyDailyReportFn = origNotify }()
	camObserverNotifyDailyReportFn = func(c config, dayID string) {
		t.Error("the generate path must never webhook")
	}

	db, err := openProxyDB(cfg)
	if err != nil {
		t.Fatalf("openProxyDB: %v", err)
	}
	defer db.Close()

	v1, gerr := camObserverGenerateLogDay(cfg, db, s, "2026-07-11", false)
	if gerr != nil || v1.Status != "done" || v1.Headline != "Version 1" {
		t.Fatalf("first generate = %+v, %v", v1, gerr)
	}
	if v1.ViewToken == "" {
		t.Fatal("first generate minted no view_token")
	}

	// Unforced re-generate: the existing row comes back untouched, no call.
	same, gerr := camObserverGenerateLogDay(cfg, db, s, "2026-07-11", false)
	if gerr != nil || same.ID != v1.ID || same.Seq != v1.Seq || same.Headline != "Version 1" {
		t.Fatalf("unforced regenerate = %+v, %v; want the untouched v1 row", same, gerr)
	}
	if analyzeCalls != 1 {
		t.Fatalf("analyze calls after unforced regenerate = %d, want still 1", analyzeCalls)
	}

	// Forced: fresh content + fresh seq on the SAME id + view_token.
	v2, gerr := camObserverGenerateLogDay(cfg, db, s, "2026-07-11", true)
	if gerr != nil {
		t.Fatalf("forced regenerate: %v", gerr)
	}
	if v2.ID != v1.ID || v2.ViewToken != v1.ViewToken {
		t.Errorf("regenerate changed identity: id %q→%q token %q→%q", v1.ID, v2.ID, v1.ViewToken, v2.ViewToken)
	}
	if v2.Headline != "Version 2" || v2.Seq <= v1.Seq {
		t.Errorf("regenerated row = headline %q seq %d (v1 seq %d), want fresh content + moved seq", v2.Headline, v2.Seq, v1.Seq)
	}
	if analyzeCalls != 2 {
		t.Errorf("analyze calls = %d, want 2", analyzeCalls)
	}

	// A bad date is rejected before any claim.
	if _, err := camObserverGenerateLogDay(cfg, db, s, "11/07/2026", false); err == nil {
		t.Error("bad date must be rejected")
	}
}

// TestCamNotifyDailyReportWebhook: the webhook posts the documented
// camera_daily_report payload to the registered site callback, signed with
// the callback secret (Bearer + X-Camera-Signature HMAC over the exact body).
func TestCamNotifyDailyReportWebhook(t *testing.T) {
	cfg := newCamObserverTestConfig(t)
	cfg.CameraCallbackAllowPrivate = true // httptest is loopback
	cfg.PublicURL = "https://proxy.example.com"
	siteID, _ := seedObserverSite(t, cfg)
	s := seedObserverStream(t, cfg, logStream{SiteID: siteID, Name: "Hooked", Enabled: true})

	type hit struct {
		body      []byte
		signature string
		auth      string
	}
	hits := make(chan hit, 4)
	rcv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		hits <- hit{body: body, signature: r.Header.Get("X-Camera-Signature"), auth: r.Header.Get("Authorization")}
		w.WriteHeader(http.StatusOK)
	}))
	defer rcv.Close()

	db, err := openProxyDB(cfg)
	if err != nil {
		t.Fatalf("openProxyDB: %v", err)
	}
	defer db.Close()
	if err := setCameraSiteCallback(db, cfg, siteID, rcv.URL, "hook-secret", false); err != nil {
		t.Fatalf("setCameraSiteCallback: %v", err)
	}
	id, won, err := claimLogDay(db, logDay{StreamID: s.ID, SiteID: siteID, Day: "2026-07-11"})
	if err != nil || !won {
		t.Fatalf("claimLogDay = %v, %v", won, err)
	}
	if err := finishLogDay(db, logDay{ID: id, FromTS: "2026-07-10T21:00:00Z", ToTS: "2026-07-11T17:00:00Z",
		Headline: "يوم هادئ", Report: "مر اليوم دون أحداث تُذكر.", Language: "Arabic",
		HoursUsed: 20, EntriesUsed: 240, Status: "done"}); err != nil {
		t.Fatalf("finishLogDay: %v", err)
	}
	d, _ := getLogDay(db, id)

	camNotifyDailyReport(cfg, id) // synchronous here; production spawns it

	var got hit
	select {
	case got = <-hits:
	case <-time.After(2 * time.Second):
		t.Fatal("callback never received the webhook")
	}

	var payload map[string]any
	if err := json.Unmarshal(got.body, &payload); err != nil {
		t.Fatalf("decode payload: %v (%s)", err, got.body)
	}
	want := map[string]any{
		"event":     "camera_daily_report",
		"site_id":   siteID,
		"stream_id": s.ID,
		"report_id": id,
		"date":      "2026-07-11",
		"headline":  "يوم هادئ",
		"report":    "مر اليوم دون أحداث تُذكر.",
		"language":  "Arabic",
		"view_url":  "https://proxy.example.com/camera/reports/" + d.ViewToken,
	}
	for k, v := range want {
		if payload[k] != v {
			t.Errorf("payload[%s] = %v, want %v", k, payload[k], v)
		}
	}
	if at, _ := payload["at"].(string); at == "" {
		t.Error("payload missing the at timestamp")
	}
	mac := hmac.New(sha256.New, []byte("hook-secret"))
	mac.Write(got.body)
	if wantSig := "sha256=" + hex.EncodeToString(mac.Sum(nil)); got.signature != wantSig {
		t.Errorf("signature = %q, want %q (HMAC over the exact body)", got.signature, wantSig)
	}
	if got.auth != "Bearer hook-secret" {
		t.Errorf("authorization = %q, want the bearer secret", got.auth)
	}

	// A site with no registered callback is a cheap no-op, never a panic.
	if err := deleteCameraSiteCallback(db, siteID); err != nil {
		t.Fatalf("deleteCameraSiteCallback: %v", err)
	}
	camNotifyDailyReport(cfg, id)
	select {
	case <-hits:
		t.Error("webhook fired without a registered callback")
	case <-time.After(100 * time.Millisecond):
	}
}

// TestObserverGenerateForceHealsPending: a row stranded `pending` (crash after
// claimLogDay, before finishLogDay) is NOT healed by an unforced generate
// (idempotent no-op, no model call) but IS rebuilt in place by force — the same
// id + view_token survive, the seq re-stamps, and the status becomes terminal.
// Guards the exact regression where the `pending` test preceded the force
// branch, so force could never override a stranded row.
func TestObserverGenerateForceHealsPending(t *testing.T) {
	cfg := newCamObserverTestConfig(t)
	siteID, _ := seedObserverSite(t, cfg)
	s := seedObserverStream(t, cfg, logStream{SiteID: siteID, Name: "Stranded", Enabled: true})
	obsSeedHourEntries(t, cfg, siteID, s.ID)

	origAnalyze := camObserverAnalyzeFn
	defer func() { camObserverAnalyzeFn = origAnalyze }()
	var calls atomic.Int32
	camObserverAnalyzeFn = func(ctx context.Context, c config, alias, sys, user string, images []string) (string, []camAnalyzeAttempt, error) {
		calls.Add(1)
		return `{"headline":"Recovered day","report":"Rebuilt after a stranded claim."}`, nil, nil
	}

	db, err := openProxyDB(cfg)
	if err != nil {
		t.Fatalf("openProxyDB: %v", err)
	}
	defer db.Close()

	// Simulate the crash window: a bare pending row for the day, no content.
	id, won, cerr := claimLogDay(db, logDay{StreamID: s.ID, SiteID: siteID, Day: "2026-07-11"})
	if cerr != nil || !won {
		t.Fatalf("claimLogDay = %v, %v", won, cerr)
	}
	stranded, _ := getLogDay(db, id)
	if stranded.Status != "pending" {
		t.Fatalf("seed status = %q, want pending", stranded.Status)
	}

	// Unforced generate over a pending row: returns it untouched, no model call.
	noop, gerr := camObserverGenerateLogDay(cfg, db, s, "2026-07-11", false)
	if gerr != nil || noop.Status != "pending" || calls.Load() != 0 {
		t.Fatalf("unforced generate over pending = %+v, %v, calls=%d; want the untouched pending row and no call", noop, gerr, calls.Load())
	}

	// Forced generate heals it IN PLACE: same identity, terminal status, content.
	healed, gerr := camObserverGenerateLogDay(cfg, db, s, "2026-07-11", true)
	if gerr != nil {
		t.Fatalf("forced generate over pending: %v", gerr)
	}
	if healed.ID != id || healed.ViewToken != stranded.ViewToken {
		t.Errorf("force changed identity: id %q→%q token %q→%q", id, healed.ID, stranded.ViewToken, healed.ViewToken)
	}
	if healed.Status != "done" || healed.Headline != "Recovered day" {
		t.Errorf("healed row = %+v, want a done rebuild", healed)
	}
	if calls.Load() != 1 {
		t.Errorf("analyze calls = %d, want exactly 1 (only the forced rebuild)", calls.Load())
	}
	if healed.Seq <= stranded.Seq {
		t.Errorf("healed seq = %d, want > claim seq %d (re-stamped so Connect re-ingests)", healed.Seq, stranded.Seq)
	}
}

// TestObserverDailyTickReclaimsStalePending: the SCHEDULED tick recovers a day
// row stranded `pending` past the compose lease — it rebuilds the row in place
// and delivers it exactly once — while a FRESH pending claim (within the lease)
// is left to its in-flight owner. This is the crash-AFTER-claim recovery the
// on-demand force path can only fix manually.
func TestObserverDailyTickReclaimsStalePending(t *testing.T) {
	cfg := newCamObserverTestConfig(t)
	cfg.CameraObserverRollupCatchup = 1
	siteID, _ := seedObserverSite(t, cfg)
	s := seedObserverStream(t, cfg, logStream{SiteID: siteID, Name: "Reclaim", Enabled: true})
	obsSeedHourEntries(t, cfg, siteID, s.ID)

	origAnalyze := camObserverAnalyzeFn
	defer func() { camObserverAnalyzeFn = origAnalyze }()
	camObserverAnalyzeFn = func(ctx context.Context, c config, alias, sys, user string, images []string) (string, []camAnalyzeAttempt, error) {
		if strings.Contains(user, "DAILY REPORT WINDOW") {
			return `{"headline":"Reclaimed report","report":"Rebuilt by the scheduled tick after a stranded claim."}`, nil, nil
		}
		return "Morning summary.", nil, nil
	}
	origNotify := camObserverNotifyDailyReportFn
	defer func() { camObserverNotifyDailyReportFn = origNotify }()
	var notifies atomic.Int32
	notified := make(chan string, 4)
	camObserverNotifyDailyReportFn = func(c config, dayID string) {
		notifies.Add(1)
		notified <- dayID
	}

	db, err := openProxyDB(cfg)
	if err != nil {
		t.Fatalf("openProxyDB: %v", err)
	}
	defer db.Close()

	// A claim stranded pending long ago (created_at well before the tick, so it
	// is past the reclaim lease).
	id, won, cerr := claimLogDay(db, logDay{StreamID: s.ID, SiteID: siteID, Day: "2026-07-11", CreatedAt: "2026-07-11T00:00:00Z"})
	if cerr != nil || !won {
		t.Fatalf("claimLogDay = %v, %v", won, cerr)
	}
	stranded, _ := getLogDay(db, id)
	if stranded.Status != "pending" {
		t.Fatalf("seed status = %q, want pending", stranded.Status)
	}

	// 20:30 local (17:30Z), past the 20:00 report time: reclaim + deliver once.
	obsRunStreamRollups(t, cfg, s.ID, time.Date(2026, 7, 11, 17, 30, 0, 0, time.UTC))
	select {
	case gotID := <-notified:
		if gotID != id {
			t.Errorf("delivered id = %q, want the reclaimed row %q", gotID, id)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("stale pending row was never reclaimed/delivered")
	}
	d, derr := getLogDayByStreamDay(db, s.ID, "2026-07-11")
	if derr != nil {
		t.Fatalf("getLogDayByStreamDay: %v", derr)
	}
	if d.ID != id || d.ViewToken != stranded.ViewToken {
		t.Errorf("reclaim changed identity: id %q→%q token %q→%q", id, d.ID, stranded.ViewToken, d.ViewToken)
	}
	if d.Status != "done" || d.Headline != "Reclaimed report" || d.NotifiedAt == "" {
		t.Errorf("reclaimed row = %+v, want done + delivered", d)
	}

	// Guard: a FRESH pending claim (within the lease) is left to its owner — the
	// tick must not steal and rebuild it.
	s2 := seedObserverStream(t, cfg, logStream{SiteID: siteID, Name: "Fresh", Enabled: true})
	fid, won2, cerr2 := claimLogDay(db, logDay{StreamID: s2.ID, SiteID: siteID, Day: "2026-07-11", CreatedAt: "2026-07-11T17:29:30Z"})
	if cerr2 != nil || !won2 {
		t.Fatalf("claimLogDay(fresh) = %v, %v", won2, cerr2)
	}
	notifiesBefore := notifies.Load()
	obsRunStreamRollups(t, cfg, s2.ID, time.Date(2026, 7, 11, 17, 30, 0, 0, time.UTC))
	fresh, _ := getLogDay(db, fid)
	if fresh.Status != "pending" {
		t.Errorf("fresh pending row status = %q, want still pending (owner keeps it)", fresh.Status)
	}
	time.Sleep(50 * time.Millisecond)
	if notifies.Load() != notifiesBefore {
		t.Errorf("fresh pending row triggered a delivery; the lease guard failed")
	}
}
