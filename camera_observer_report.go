package main

// camera_observer_report.go — hourly/daily rollups over the activity journal
// (camera_observer.go) plus the daily-report webhook. A 60s ticker
// (camObserverRollupTick, started by startCameraObserver) walks every enabled
// log stream and:
//
//   - HOURLY: for each of the last PROXY_CAMERA_OBSERVER_ROLLUP_CATCHUP
//     completed site-local hours (ended ≥ 5min ago) with no camera_log_hours
//     row yet, claims the hour by pending INSERT (the unique (stream,hour)
//     index makes the claim atomic — the loser skips) and condenses that
//     hour's entries into ONE text-only summary in the site's report language
//     (camSitePolicy.ReportLanguage, default English — entries themselves stay
//     English, camera_observer_prompt.go). An hour with no entries settles as
//     status "empty" without a model call. The terminal UPDATE re-stamps the
//     sync seq so Connect re-ingests the finished row. Bounded catch-up: a
//     long outage loses old hour summaries — the daily report falls back to
//     raw entries, and Connect archived every entry anyway.
//
//   - DAILY: once the site-local clock passes the stream's daily_report_time
//     on day D (and only while it is still day D locally), the day's report is
//     claimed/generated from the hour summaries 00:00→report time (raw entry
//     headlines fill hours without a summary), then delivered ONCE through the
//     site callback (camNotifyDailyReport — the camNotifyExportReady shape).
//     Exactly-once delivery is the notified_at CAS (claimLogDayNotified): the
//     scheduled tick — never the on-demand generate — takes the stamp before
//     spawning the webhook, so a report generated early through days/generate
//     still goes out at report time, a regenerate after delivery never
//     re-notifies (the Connect-loop guard), and a report left undelivered by a
//     crash is picked up by the next tick while it is still day D.
//
// STALE-CLAIM RECOVERY: claimLogDay commits a `pending` row in its own tiny
// transaction; the compose (a model call) and finishLogDay run after it. A
// crash in that window — or a finishLogDay failure (a transient store error
// after the long call) — would strand the (stream,day) row `pending` forever:
// the sync route hides it, the webhook never fires, the public page shows
// "generating…" indefinitely. So the recovery paths do NOT treat `pending` as
// untouchable: camObserverDailyTick reclaims a pending row whose claim is older
// than a compose lease (camObserverDayReclaimLease) and rebuilds it in place,
// and camObserverGenerateLogDay lets force recompose a pending row too. A fresh
// pending row (a live generation on this or another instance) is still left
// alone until the lease elapses.
//
// camObserverGenerateLogDay is the on-demand path behind days/generate:
// build-if-missing (or rebuild with force — which also heals a stranded pending
// row) on the SAME row — id and view_token survive regeneration so a shared
// /camera/reports/<view_token> link keeps working — and it NEVER emits the
// webhook.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// camObserverNotifyDailyReportFn is the daily-report webhook seam — a package
// var so tests can observe the exactly-once spawn without network I/O.
var camObserverNotifyDailyReportFn = camNotifyDailyReport

// camObserverRollupInterval is the rollup ticker cadence.
const camObserverRollupInterval = time.Minute

// camObserverHourSettle is how long past a local hour boundary the rollup
// waits before claiming that hour, so late-finishing log cycles (observer lag
// + a slow analysis) have landed their entries first.
const camObserverHourSettle = 5 * time.Minute

// camObserverRollupCatchupFor resolves how many completed local hours each
// tick back-fills (default 4 — deliberately bounded, see the file comment).
func camObserverRollupCatchupFor(cfg config) int {
	if n := cfg.CameraObserverRollupCatchup; n > 0 {
		return n
	}
	return 4
}

// ─────────────────────────────── ticker ───────────────────────────────

// camObserverRollupTick runs one rollup pass over every enabled log stream.
// Mirrors camObserverTick, including the global proxy pause (rollups burn AI
// calls like everything else).
func camObserverRollupTick(cfg config) {
	if !proxyEnabled.Load() {
		camlog("debug", "observer_rollup", map[string]any{"skipped": true, "reason": "proxy_disabled"})
		return
	}
	db, err := openProxyDB(cfg)
	if err != nil {
		camlog("error", "observer_rollup", map[string]any{"ok": false, "error": err.Error()})
		return
	}
	defer db.Close()

	streams, err := listLogStreams(db, "")
	if err != nil {
		camlog("error", "observer_rollup", map[string]any{"ok": false, "error": err.Error()})
		return
	}
	now := time.Now()
	for _, s := range streams {
		if !s.Enabled {
			continue
		}
		camObserverStreamRollups(cfg, db, s, now)
	}
	// Close presence intervals for anyone unseen past the absence grace (departures).
	camPresenceSweep(cfg, db)
}

// camObserverStreamRollups resolves one stream's report environment (site,
// timezone, report language) and runs its hourly catch-up and daily check for
// the given instant.
func camObserverStreamRollups(cfg config, db *sql.DB, s logStream, now time.Time) {
	site, loc, tzName, language, err := camObserverReportEnv(cfg, db, s)
	if err != nil {
		camlog("warn", "observer_rollup", map[string]any{"stream_id": s.ID, "site_id": s.SiteID, "ok": false, "error": err.Error()})
		return
	}
	camObserverHourlyRollups(cfg, db, s, site, now, loc, tzName, language)
	camObserverDailyTick(cfg, db, s, site, now, loc, tzName, language)
}

// camObserverReportEnv loads the rollup context shared by the hourly and daily
// paths: the site row, the site timezone (camSiteTimezone over its DVRs), and
// the report language (camSitePolicy.ReportLanguage, default English).
func camObserverReportEnv(cfg config, db *sql.DB, s logStream) (camSite, *time.Location, string, string, error) {
	site, err := getCameraSite(db, s.SiteID)
	if err != nil {
		return camSite{}, nil, "", "", fmt.Errorf("load site: %w", err)
	}
	dvrs, derr := listCameraDVRs(db, cfg, s.SiteID)
	if derr != nil {
		return camSite{}, nil, "", "", fmt.Errorf("load dvrs: %w", derr)
	}
	loc, tzName := camSiteTimezone(dvrs)
	language := firstNonEmpty(camParseSitePolicy(site.PolicyJSON).ReportLanguage, "English")
	return site, loc, tzName, language, nil
}

// ─────────────────────────────── hourly rollups ───────────────────────────────

// camObserverHourlyRollups claims-and-summarizes the last catch-up completed
// site-local hours that have no rollup row yet. The newest eligible hour is
// the one that ended at least camObserverHourSettle ago.
func camObserverHourlyRollups(cfg config, db *sql.DB, s logStream, site camSite, now time.Time, loc *time.Location, tzName, language string) {
	lc := now.Add(-camObserverHourSettle).In(loc)
	newestEnd := time.Date(lc.Year(), lc.Month(), lc.Day(), lc.Hour(), 0, 0, 0, loc)
	for i := 0; i < camObserverRollupCatchupFor(cfg); i++ {
		hourStart := newestEnd.Add(-time.Duration(i+1) * time.Hour)
		camObserverRollupHour(cfg, db, s, site, hourStart, loc, tzName, language)
	}
}

// camObserverRollupHour produces the rollup row for ONE site-local hour:
// existence probe (cheap read — the common path), atomic pending-INSERT claim,
// then either the "empty" settle (no entries → no model call) or one text-only
// summary in the report language. Terminal states re-stamp the sync seq
// (finishLogHour); an "error" hour is terminal too — it is never re-claimed,
// so a persistently failing model can not burn a call per tick (the daily
// report falls back to the hour's raw headlines, and Connect archived the
// entries themselves).
func camObserverRollupHour(cfg config, db *sql.DB, s logStream, site camSite, hourStart time.Time, loc *time.Location, tzName, language string) {
	hsUTC := hourStart.UTC().Format(time.RFC3339)
	heUTC := hourStart.Add(time.Hour).UTC().Format(time.RFC3339)
	if _, err := getLogHourByStreamHour(db, s.ID, hsUTC); err == nil {
		return // already claimed/settled
	} else if !errors.Is(err, sql.ErrNoRows) {
		camlog("warn", "observer_hour", map[string]any{"stream_id": s.ID, "hour_start": hsUTC, "ok": false, "error": err.Error()})
		return
	}

	h := logHour{StreamID: s.ID, SiteID: s.SiteID, HourStart: hsUTC, TZ: tzName}
	id, won, err := claimLogHour(db, h)
	if err != nil {
		camlog("warn", "observer_hour", map[string]any{"stream_id": s.ID, "hour_start": hsUTC, "ok": false, "error": err.Error()})
		return
	}
	if !won {
		return // a concurrent tick/instance owns this hour
	}
	h.ID = id

	entries, eerr := listLogEntriesForRollup(db, s.ID, hsUTC, heUTC)
	if eerr != nil {
		h.Status, h.Error = "error", "load entries: "+eerr.Error()
		camObserverFinishHour(db, h)
		return
	}
	h.EntryCount = len(entries)
	for _, e := range entries {
		if e.MotionGated {
			h.GatedCount++
		}
	}
	if len(entries) == 0 {
		h.Status = "empty"
		camObserverFinishHour(db, h)
		return
	}

	sys := camObserverHourSystemPrompt(site, language)
	user := camObserverHourUserPrompt(hourStart, loc, tzName, camObserverRollupEntryLines(entries, loc, true))
	ctx, cancel := context.WithTimeout(context.Background(), camObserverBudget(cfg))
	defer cancel()
	raw, _, aerr := camObserverAnalyzeFn(ctx, cfg, camObserverAliasFor(cfg, s), sys, user, nil)
	if aerr != nil {
		h.Status, h.Error = "error", aerr.Error()
		camObserverFinishHour(db, h)
		return
	}
	summary := camObserverStripFences(raw)
	if summary == "" {
		h.Status, h.Error = "error", "model returned no usable text"
		camObserverFinishHour(db, h)
		return
	}
	h.Summary = camTruncateRunes(summary, 4000)
	h.Language = language
	h.Status = "done"
	camObserverFinishHour(db, h)
	camlog("info", "observer_hour", map[string]any{
		"stream_id": s.ID, "site_id": s.SiteID, "hour_start": hsUTC,
		"entries": h.EntryCount, "gated": h.GatedCount, "language": language, "ok": true,
	})
}

// camObserverFinishHour persists an hour's terminal state, logging (never
// propagating) store failures — the claim row stays pending and is simply
// never retried, which the next manifest/browse surfaces to the operator.
func camObserverFinishHour(db *sql.DB, h logHour) {
	if err := finishLogHour(db, h); err != nil {
		camlog("warn", "observer_hour", map[string]any{"stream_id": h.StreamID, "hour_start": h.HourStart, "ok": false, "error": err.Error()})
	}
}

// ─────────────────────────────── daily report ───────────────────────────────

// camObserverReportInstant resolves a stream's daily report instant on the
// day starting at dayStart (site-local midnight): dayStart + daily_report_time
// ("HH:MM", default 20:00 for blank/bad values — insertLogStream's default).
func camObserverReportInstant(s logStream, dayStart time.Time) time.Time {
	hh, mm := 20, 0
	if t, err := time.Parse("15:04", strings.TrimSpace(s.DailyReportTime)); err == nil {
		hh, mm = t.Hour(), t.Minute()
	}
	return time.Date(dayStart.Year(), dayStart.Month(), dayStart.Day(), hh, mm, 0, 0, dayStart.Location())
}

// camObserverDayReclaimLease is how long a daily-report row may sit `pending`
// before camObserverDailyTick treats the claim as stranded (a crash, or a
// finishLogDay failure after the compose) and rebuilds it in place. It exceeds
// the compose budget so a generation still legitimately in flight — on this or
// another instance — is never interrupted.
func camObserverDayReclaimLease(cfg config) time.Duration {
	if lease := 2 * camObserverBudget(cfg); lease > 10*time.Minute {
		return lease
	}
	return 10 * time.Minute
}

// camObserverDayPendingStale reports whether a pending day row's claim (stamped
// at created_at) is at least `lease` old at `now`. A blank/unparseable claim
// timestamp is treated as stale — a row that cannot prove it is fresh can only
// be a stranded claim.
func camObserverDayPendingStale(d logDay, now time.Time, lease time.Duration) bool {
	created, err := time.Parse(time.RFC3339, strings.TrimSpace(d.CreatedAt))
	if err != nil {
		return true
	}
	return now.Sub(created) >= lease
}

// camObserverDailyTick is the scheduled daily-report path for one stream: past
// daily_report_time on day D (site-local, and only while still day D) it
// generates the missing report (atomic claim — one winner across ticks and
// instances) and then takes the notified_at CAS to fire the webhook exactly
// once. A row STRANDED pending past the compose lease (a crash or a
// finishLogDay failure after an earlier claim) is reclaimed and rebuilt in
// place — otherwise it, and its delivery, would be lost forever; a fresh
// pending row (a live generation) is left to its owner. Error rows (a
// regenerate heals them; the NEXT tick then delivers) are skipped.
func camObserverDailyTick(cfg config, db *sql.DB, s logStream, site camSite, now time.Time, loc *time.Location, tzName, language string) {
	localNow := now.In(loc)
	dayStart := time.Date(localNow.Year(), localNow.Month(), localNow.Day(), 0, 0, 0, 0, loc)
	if localNow.Before(camObserverReportInstant(s, dayStart)) {
		return // today's report time hasn't arrived yet
	}
	day := localNow.Format("2006-01-02")

	d, err := getLogDayByStreamDay(db, s.ID, day)
	if errors.Is(err, sql.ErrNoRows) {
		id, won, cerr := claimLogDay(db, logDay{StreamID: s.ID, SiteID: s.SiteID, Day: day})
		if cerr != nil {
			camlog("warn", "observer_daily", map[string]any{"stream_id": s.ID, "day": day, "ok": false, "error": cerr.Error()})
			return
		}
		if !won {
			return // another tick/instance is generating it
		}
		claimed, gerr := getLogDay(db, id)
		if gerr != nil {
			camlog("warn", "observer_daily", map[string]any{"stream_id": s.ID, "day": day, "ok": false, "error": gerr.Error()})
			return
		}
		d = camObserverComposeDay(cfg, db, s, site, claimed, loc, tzName, language)
		if ferr := finishLogDay(db, d); ferr != nil {
			camlog("warn", "observer_daily", map[string]any{"stream_id": s.ID, "day": day, "ok": false, "error": ferr.Error()})
			return
		}
		camlog("info", "observer_daily", map[string]any{
			"stream_id": s.ID, "site_id": s.SiteID, "day": day, "status": d.Status,
			"hours_used": d.HoursUsed, "entries_used": d.EntriesUsed, "language": d.Language,
		})
	} else if err != nil {
		camlog("warn", "observer_daily", map[string]any{"stream_id": s.ID, "day": day, "ok": false, "error": err.Error()})
		return
	}

	if d.Status == "pending" {
		// A row still pending here was claimed by an earlier run that never
		// reached a terminal state (a crash mid-compose, or a finishLogDay
		// failure after the long model call). Left alone it is stuck forever —
		// the sync route hides it, this webhook never fires, the public page
		// shows "generating…" indefinitely. Once the claim is older than the
		// compose lease (no live generation on any tick/instance could still
		// hold it), reclaim and rebuild the SAME row in place (id + view_token
		// survive); a fresher claim is left to its in-flight owner.
		if !camObserverDayPendingStale(d, now, camObserverDayReclaimLease(cfg)) {
			return
		}
		d = camObserverComposeDay(cfg, db, s, site, d, loc, tzName, language)
		if ferr := finishLogDay(db, d); ferr != nil {
			camlog("warn", "observer_daily", map[string]any{"stream_id": s.ID, "day": day, "ok": false, "error": ferr.Error(), "reclaim": true})
			return
		}
		camlog("info", "observer_daily", map[string]any{
			"stream_id": s.ID, "site_id": s.SiteID, "day": day, "status": d.Status,
			"hours_used": d.HoursUsed, "entries_used": d.EntriesUsed, "language": d.Language, "reclaimed": true,
		})
	}
	if d.Status == "error" || strings.TrimSpace(d.NotifiedAt) != "" {
		return
	}
	won, nerr := claimLogDayNotified(db, d.ID, nowRFC3339())
	if nerr != nil {
		camlog("warn", "observer_daily_notify", map[string]any{"report_id": d.ID, "ok": false, "error": nerr.Error()})
		return
	}
	if !won {
		return // another tick/instance already delivered it
	}
	camlog("info", "observer_daily_notify", map[string]any{"report_id": d.ID, "stream_id": s.ID, "site_id": s.SiteID, "day": d.Day})
	go camObserverNotifyDailyReportFn(cfg, d.ID)
}

// camObserverGenerateLogDay is the on-demand days/generate path: build the
// report for day (site-local "2006-01-02", "" = today) when it is missing,
// return the existing row untouched when it exists and force is off (idempotent
// button-mash, and the hands-off-a-live-claim case), or rebuild it in place
// with force — the SAME row id and view_token survive (finishLogDay), only the
// sync seq is re-stamped. force rebuilds REGARDLESS of status, so it is also
// the manual escape hatch that heals a row stranded `pending` by a crash /
// finishLogDay failure (the status test must not precede the force branch, or
// force could never override a pending row). This path NEVER emits the
// camera_daily_report webhook: only the scheduled tick takes the notified_at
// CAS, so a Connect-initiated regenerate can never trigger a webhook loop.
func camObserverGenerateLogDay(cfg config, db *sql.DB, s logStream, day string, force bool) (logDay, error) {
	site, loc, tzName, language, err := camObserverReportEnv(cfg, db, s)
	if err != nil {
		return logDay{}, err
	}
	day = strings.TrimSpace(day)
	if day == "" {
		day = time.Now().In(loc).Format("2006-01-02")
	}
	if _, perr := time.ParseInLocation("2006-01-02", day, loc); perr != nil {
		return logDay{}, errors.New("date must be YYYY-MM-DD")
	}

	existing, gerr := getLogDayByStreamDay(db, s.ID, day)
	if gerr == nil {
		if !force {
			return existing, nil // idempotent button-mash, or another claimant mid-generation
		}
		// force recomposes on the SAME row whatever its status — including a
		// pending row stranded by a crash/finishLogDay failure after its claim.
		rebuilt := camObserverComposeDay(cfg, db, s, site, existing, loc, tzName, language)
		if ferr := finishLogDay(db, rebuilt); ferr != nil {
			return rebuilt, ferr
		}
		camlog("info", "observer_daily_generate", map[string]any{"stream_id": s.ID, "day": day, "status": rebuilt.Status, "forced": true})
		return getLogDay(db, rebuilt.ID)
	}
	if !errors.Is(gerr, sql.ErrNoRows) {
		return logDay{}, gerr
	}

	id, won, cerr := claimLogDay(db, logDay{StreamID: s.ID, SiteID: s.SiteID, Day: day})
	if cerr != nil {
		return logDay{}, cerr
	}
	if !won {
		return getLogDayByStreamDay(db, s.ID, day) // lost a concurrent claim — hand back whoever won
	}
	claimed, derr := getLogDay(db, id)
	if derr != nil {
		return logDay{}, derr
	}
	built := camObserverComposeDay(cfg, db, s, site, claimed, loc, tzName, language)
	if ferr := finishLogDay(db, built); ferr != nil {
		return built, ferr
	}
	camlog("info", "observer_daily_generate", map[string]any{"stream_id": s.ID, "day": day, "status": built.Status, "forced": false})
	return getLogDay(db, id)
}

// camObserverComposeDay fills a claimed/existing day row with the generated
// report content (the caller persists via finishLogDay): the window is
// site-local 00:00 → the stream's report time; the model input is that span's
// hour summaries where they exist ("done" rows) with raw entry headlines
// filling the gaps; ONE text-only call returns {"headline","report"} in the
// report language, with an unparseable-but-nonempty reply salvaged rather
// than dropped. A day with no entries and no summaries settles as "empty"
// without a model call (fixed English text — there is nothing to translate).
func camObserverComposeDay(cfg config, db *sql.DB, s logStream, site camSite, d logDay, loc *time.Location, tzName, language string) logDay {
	dayStart, err := time.ParseInLocation("2006-01-02", d.Day, loc)
	if err != nil {
		d.Status, d.Error = "error", "bad day: "+err.Error()
		return d
	}
	to := camObserverReportInstant(s, dayStart)
	d.FromTS = dayStart.UTC().Format(time.RFC3339)
	d.ToTS = to.UTC().Format(time.RFC3339)
	d.Language = language
	d.Error = ""

	entries, eerr := listLogEntriesForRollup(db, s.ID, d.FromTS, d.ToTS)
	if eerr != nil {
		d.Status, d.Error = "error", "load entries: "+eerr.Error()
		return d
	}
	doneByStart := map[string]logHour{}
	if hours, herr := listLogHoursBySite(db, s.SiteID, s.ID, d.FromTS, d.ToTS, 0); herr == nil {
		for _, h := range hours {
			if h.Status == "done" {
				doneByStart[h.HourStart] = h
			}
		}
	} // summaries are an optimization — on error the raw entries still carry the day

	d.EntriesUsed = len(entries)
	if len(entries) == 0 && len(doneByStart) == 0 {
		d.Status = "empty"
		d.Headline = camObserverIdleHeadline
		d.Language = "English" // the fixed idle text below, not the report language
		d.Report = fmt.Sprintf("No activity was recorded by log stream %q on %s between 00:00 and %s (%s).",
			s.Name, d.Day, to.In(loc).Format("15:04"), tzName)
		return d
	}

	block, hoursUsed := camObserverDayInputBlock(entries, doneByStart, dayStart, to, loc)
	d.HoursUsed = hoursUsed
	sys := camObserverDaySystemPrompt(site, language)
	user := camObserverDayUserPrompt(d.Day, dayStart, to, loc, tzName, block)
	ctx, cancel := context.WithTimeout(context.Background(), camObserverBudget(cfg))
	defer cancel()
	raw, _, aerr := camObserverAnalyzeFn(ctx, cfg, camObserverAliasFor(cfg, s), sys, user, nil)
	if aerr != nil {
		d.Status, d.Error = "error", aerr.Error()
		return d
	}
	rep, perr := parseObserverReport(raw)
	if perr != nil {
		text := strings.TrimSpace(raw)
		if text == "" {
			d.Status, d.Error = "error", "model returned no usable text"
			return d
		}
		rep = observerReport{Headline: camTruncateRunes(camFirstLine(text), 200), Report: camTruncateRunes(text, 8000)}
		d.Error = "salvaged_raw"
	}
	d.Headline = rep.Headline
	d.Report = rep.Report
	d.Status = "done"
	return d
}

// camObserverDayInputBlock renders the daily model input, walking the local
// hour boundaries from midnight to the report time: an hour with a "done"
// summary contributes that summary line; an hour without one contributes its
// raw entry headlines (idle runs compressed); a silent hour contributes
// nothing. Returns the block and how many hour summaries were used.
func camObserverDayInputBlock(entries []logEntry, doneByStart map[string]logHour, from, to time.Time, loc *time.Location) (string, int) {
	type stamped struct {
		at time.Time
		e  logEntry
	}
	ordered := make([]stamped, 0, len(entries))
	for _, e := range entries {
		if at, err := time.Parse(time.RFC3339, e.FromTS); err == nil {
			ordered = append(ordered, stamped{at: at, e: e})
		}
	}

	var b strings.Builder
	hoursUsed := 0
	idx := 0
	for hs := from; hs.Before(to); hs = hs.Add(time.Hour) {
		he := hs.Add(time.Hour)
		if he.After(to) {
			he = to
		}
		var hourEntries []logEntry
		for idx < len(ordered) && ordered[idx].at.Before(he) {
			if !ordered[idx].at.Before(hs) {
				hourEntries = append(hourEntries, ordered[idx].e)
			}
			idx++
		}
		if h, ok := doneByStart[hs.UTC().Format(time.RFC3339)]; ok {
			fmt.Fprintf(&b, "[%s–%s] %s\n", hs.In(loc).Format("15:04"), he.In(loc).Format("15:04"), strings.TrimSpace(h.Summary))
			hoursUsed++
			continue
		}
		if len(hourEntries) == 0 {
			continue
		}
		b.WriteString(camObserverRollupEntryLines(hourEntries, loc, false))
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n"), hoursUsed
}

// ─────────────────────────────── rollup prompts ───────────────────────────────

// camObserverRollupEntryLines renders journal entries (oldest first) for a
// rollup prompt: "[from–to] headline" lines with consecutive motion-gated idle
// entries compressed into one "(N idle cycles)" line; verbatim additionally
// quotes each active entry's report (truncated) — the hourly rollup reads full
// reports, the daily raw-hour fallback reads headlines only.
func camObserverRollupEntryLines(entries []logEntry, loc *time.Location, verbatim bool) string {
	var b strings.Builder
	i := 0
	for i < len(entries) {
		e := entries[i]
		if camObserverEntryIdle(e) {
			j := i
			for j+1 < len(entries) && camObserverEntryIdle(entries[j+1]) {
				j++
			}
			if j > i {
				fmt.Fprintf(&b, "[%s–%s] %s (%d idle cycles)\n",
					camObserverClock(entries[i].FromTS, loc), camObserverClock(entries[j].ToTS, loc),
					camObserverIdleHeadline, j-i+1)
				i = j + 1
				continue
			}
		}
		fmt.Fprintf(&b, "[%s–%s] %s\n", camObserverClock(e.FromTS, loc), camObserverClock(e.ToTS, loc),
			firstNonEmpty(strings.TrimSpace(e.Headline), "(no headline)"))
		if verbatim {
			if rep := strings.TrimSpace(e.Report); rep != "" {
				b.WriteString(camTruncateRunes(rep, 2000))
				b.WriteString("\n")
			}
		}
		i++
	}
	return strings.TrimRight(b.String(), "\n")
}

// camObserverHourSystemPrompt is the hourly rollup's system prompt: condense
// ONE hour of the site's own journal into one operator-facing paragraph, in
// the site's report language (this is where translation happens — the entries
// themselves are always English).
func camObserverHourSystemPrompt(site camSite, language string) string {
	var b strings.Builder
	b.WriteString("You are the activity-report writer for a physical premises")
	if n := strings.TrimSpace(site.Name); n != "" {
		fmt.Fprintf(&b, " (%s)", n)
	}
	b.WriteString(". Below are the site's own activity-log entries for ONE hour. Condense them into ONE summary ")
	b.WriteString("paragraph for the site operator: what happened, who was present, what continued from earlier, ")
	b.WriteString("anything unusual. Neutral and factual — never invent details the entries do not contain.\n\n")
	fmt.Fprintf(&b, "LANGUAGE: write the summary in %s.\n\n", language)
	b.WriteString("OUTPUT: reply with the summary text ONLY — no JSON, no markdown fences, no headings; at most 150 words.")
	return b.String()
}

// camObserverHourUserPrompt is the hourly rollup's user message: the exact
// hour (site-local + UTC) and the rendered entry lines.
func camObserverHourUserPrompt(hourStart time.Time, loc *time.Location, tzName, block string) string {
	he := hourStart.Add(time.Hour)
	return fmt.Sprintf("HOUR: %s %s–%s (%s) = %s – %s UTC.\n\nLOG ENTRIES (oldest first):\n%s\n\nWrite the hourly summary now.",
		hourStart.In(loc).Format("2006-01-02"), hourStart.In(loc).Format("15:04"), he.In(loc).Format("15:04"), tzName,
		hourStart.UTC().Format(time.RFC3339), he.UTC().Format(time.RFC3339), block)
}

// camObserverDayContract is the daily report's strict JSON contract.
const camObserverDayContract = "OUTPUT — reply with EXACTLY ONE JSON object and nothing else (no prose, no markdown fences):\n" +
	`{"headline":"...","report":"..."}` + "\n" +
	"- headline: ONE line (max 140 chars) capturing the day.\n" +
	"- report: max 400 words — the day's activity in chronological order: what happened, who was present, what stood out.\n"

// camObserverDaySystemPrompt is the daily report's system prompt: the report
// writer role, the language directive, and the JSON contract.
func camObserverDaySystemPrompt(site camSite, language string) string {
	var b strings.Builder
	b.WriteString("You are the daily activity-report writer for a physical premises")
	if n := strings.TrimSpace(site.Name); n != "" {
		fmt.Fprintf(&b, " (%s)", n)
	}
	b.WriteString(". Below is the site's own activity journal for ONE day — hourly summaries where available, raw log ")
	b.WriteString("lines elsewhere. Write the day's report for the site operator. Neutral and factual — never invent ")
	b.WriteString("details the journal does not contain.\n\n")
	fmt.Fprintf(&b, "LANGUAGE: write the headline and report in %s.\n\n", language)
	b.WriteString(camObserverDayContract)
	return b.String()
}

// camObserverDayUserPrompt is the daily report's user message: the covered
// window (site-local) and the composed journal block.
func camObserverDayUserPrompt(day string, from, to time.Time, loc *time.Location, tzName, block string) string {
	return fmt.Sprintf("DAILY REPORT WINDOW: %s 00:00–%s (%s) = %s – %s UTC.\n\nACTIVITY JOURNAL (oldest first — hourly summaries where available, raw log lines elsewhere):\n%s\n\nWrite the daily report now. Reply with EXACTLY ONE JSON object.",
		day, to.In(loc).Format("15:04"), tzName, from.UTC().Format(time.RFC3339), to.UTC().Format(time.RFC3339), block)
}

// camObserverStripFences trims a plain-text model reply, unwrapping one
// markdown code fence when the model ignored the "no fences" instruction.
func camObserverStripFences(raw string) string {
	t := strings.TrimSpace(raw)
	if !strings.HasPrefix(t, "```") {
		return t
	}
	if i := strings.IndexByte(t, '\n'); i >= 0 {
		t = t[i+1:]
	} else {
		return ""
	}
	t = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(t), "```"))
	return t
}

// ─────────────────────────────── webhook ───────────────────────────────

// camDailyReportPayload builds the camera_daily_report webhook body — pure
// over its inputs so it is unit-testable (the camExportReadyPayload shape).
func camDailyReportPayload(cfg config, d logDay, at time.Time) map[string]any {
	return map[string]any{
		"event":     "camera_daily_report",
		"site_id":   d.SiteID,
		"stream_id": d.StreamID,
		"report_id": d.ID,
		"date":      d.Day,
		"headline":  d.Headline,
		"report":    d.Report,
		"language":  d.Language,
		"view_url":  camLogDayViewURL(cfg, d),
		"at":        at.UTC().Format(time.RFC3339),
	}
}

// camNotifyDailyReport delivers the camera_daily_report webhook for a finished
// daily report to the site's registered callback, with the standard signed
// retry ladder. Always spawned `go ...`; a site with no callback is a cheap
// no-op. Exactly-once is the CALLER's notified_at CAS (camObserverDailyTick) —
// this function just delivers. Mirrors camNotifyExportReady.
func camNotifyDailyReport(cfg config, dayID string) {
	db, err := openProxyDB(cfg)
	if err != nil {
		camlog("warn", "daily_report_callback", map[string]any{"report_id": dayID, "ok": false, "error": err.Error()})
		return
	}
	d, gerr := getLogDay(db, dayID)
	if gerr != nil {
		db.Close()
		camlog("warn", "daily_report_callback", map[string]any{"report_id": dayID, "ok": false, "error": gerr.Error()})
		return
	}
	cb, cerr := getCameraSiteCallback(db, d.SiteID)
	db.Close()
	if cerr != nil || !cb.Enabled || strings.TrimSpace(cb.URL) == "" {
		return // no registered callback — cheap no-op
	}
	body, merr := json.Marshal(camDailyReportPayload(cfg, d, time.Now()))
	if merr != nil {
		camlog("error", "daily_report_callback", map[string]any{"report_id": dayID, "ok": false, "error": merr.Error()})
		return
	}
	camPostSignedCallback(cfg, cb, body, "daily_report_callback", map[string]any{
		"report_id": dayID, "site_id": d.SiteID, "stream_id": d.StreamID, "day": d.Day,
	})
}
