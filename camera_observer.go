package main

// camera_observer.go — the activity-log "observer" runner: a dedicated
// scheduler (cloned from the watch scheduler, cameramonitor.go) that executes
// camera_log_streams rows every interval_minutes, turning one [from,to) window
// of archived footage into ONE camera_log_entries row — the site's continuous
// AI journal ("the crew is STILL painting").
//
// ─── Window math ───
// camObserverWindow ends the window PROXY_CAMERA_OBSERVER_LAG behind now (so
// the sampled frames already exist in S3/on the DVR) and resumes from the
// stream's last_window_to continuity cursor when it is parseable and at most
// 2×interval old; anything staler skips to the latest window — catch-up NEVER
// backfills (Connect archives every entry; a gap is honest, a burst of stale
// analyses is not). A window shorter than interval/2 (e.g. right after a
// manual run) is skipped and re-armed. The cursor always advances and EVERY
// executed cycle writes an entry — failures get headline "Logging failed" and
// the error column — so the journal has no silent holes.
//
// ─── Sampling + composites (S3-first) ───
// Frames are sampled at from+k·step from the S3 1-fps archive in parallel
// (conc 16, the contact-sheet loop shape); the DVR playback fallback
// (captureFrameAtTime) runs ONLY for cameras the archive had nothing for,
// capped at PROXY_CAMERA_OBSERVER_MAX_DVR_FRAMES per cycle — degraded, never
// DVR-hammering. Frames are tiled via buildContactSheet: one roomy sheet per
// camera (cols 5, 320×180) when the stream watches ≤ MAX_SHEETS cameras, else
// everything packs into ≤ MAX_SHEETS sheets at contact-sheet geometry (cols
// 10, 160×90). Each sheet's legend line ("SHEET 2 — CELL=CAMERA@TIME …") binds
// cells to reality in the user prompt. Sheets persist as kind "log_sheet"
// captures (normal media retention: old entries lose thumbnails, keep text).
//
// ─── Motion gating + trust guard (fail open) ───
// A cycle is gated only when ALL hold: the stream opted in (motion_gate) ∧
// the deployment runs motion listeners (CameraMotionEnabled) ∧ EVERY selected
// camera is armed (camera_motion_arm) ∧ the site produced ≥1 motion event in
// the trailing 24h (the dead-listener guard). Store errors fail OPEN. Gated +
// zero events in the window → a cheap idle entry, no model call.
//
// ─── Analysis + drill-down ───
// ONE camObserverAnalyzeFn call (package-var seam over camResilientAnalyze —
// the never-die retry/failover ladder) reads the sheets against the observer
// prompt (camera_observer_prompt.go) and returns strict JSON; parse gets one
// repair round, then salvage. The model may request needs_detail full-quality
// frames ONCE (≤ MAX_DETAIL_FRAMES, fetched through the camObserverFetchFrameFn
// seam, persisted as kind "log_detail") and must then finalize — a second
// round's requests are ignored.
//
// Hourly/daily rollups + the daily-report webhook live in
// camera_observer_report.go (60s ticker started here); the public report share
// page lives in camera_observer_view.go. Retention: camObserverPruneOnce.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// ─────────────────────────────── seams ───────────────────────────────

// camObserverAnalyzeFn is the ONE model call a log cycle makes — a package var
// so tests can script replies (mirrors camInvestigateAnalyzeFn). Real runs go
// through camResilientAnalyze: per-attempt timeout floor, retry, alias failover.
var camObserverAnalyzeFn = camResilientAnalyze

// camObserverFetchFrameFn fetches ONE frame for cam at instant t into destPath
// — the seam both the window sampler ("sub") and the needs_detail drill-down
// ("main") go through, so tests can serve frames without S3 or a DVR. Returns
// ok=false with a nil error when no frame exists at t (an archive miss is not
// a fault). allowDVR permits the bounded playback fallback; the S3 read-through
// is always attempted first when the archive is configured.
var camObserverFetchFrameFn = camObserverFetchFrame

// ─────────────────────────────── scheduler ───────────────────────────────

// camObserverSem bounds concurrently RUNNING log cycles (across the scheduler,
// not per-tick). Sized by startCameraObserver from cfg.CameraObserverConcurrency.
var camObserverSem chan struct{}

// camObserverRunMu/camObserverRunning is the in-flight guard shared by the
// scheduler and the manual "run now" path (triggerLogStreamRun), so the two can
// never execute the same stream concurrently — the camWatchRunning shape.
var (
	camObserverRunMu   sync.Mutex
	camObserverRunning = map[string]bool{}
)

func camObserverTryAcquire(id string) bool {
	camObserverRunMu.Lock()
	defer camObserverRunMu.Unlock()
	if camObserverRunning[id] {
		return false
	}
	camObserverRunning[id] = true
	return true
}

func camObserverRelease(id string) {
	camObserverRunMu.Lock()
	delete(camObserverRunning, id)
	camObserverRunMu.Unlock()
}

// startCameraObserver is the runServe hook for the activity-log feature:
// the due-stream runner plus the hourly retention pruner, gated by
// CameraEnabled && CameraObserverEnabled (the cameras feature must be on AND
// the observer not explicitly disabled).
func startCameraObserver(cfg config) {
	if !cfg.CameraEnabled || !cfg.CameraObserverEnabled {
		camlog("info", "observer", map[string]any{"enabled": false})
		return
	}
	interval := cfg.CameraObserverTickInterval
	if interval <= 0 {
		interval = 15 * time.Second
	}
	conc := cfg.CameraObserverConcurrency
	if conc < 1 {
		conc = 1
	}
	camObserverSem = make(chan struct{}, conc)
	camlog("info", "observer_start", map[string]any{"tick_ms": interval.Milliseconds(), "concurrency": conc})

	go func() {
		time.Sleep(camStartupJitter(interval))
		camObserverTick(cfg)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for range ticker.C {
			camObserverTick(cfg)
		}
	}()

	// 60s rollup cadence (camera_observer_report.go): hourly summaries, the
	// scheduled daily report and its exactly-once webhook, per enabled stream.
	go func() {
		time.Sleep(camStartupJitter(camObserverRollupInterval))
		camObserverRollupTick(cfg)
		ticker := time.NewTicker(camObserverRollupInterval)
		defer ticker.Stop()
		for range ticker.C {
			camObserverRollupTick(cfg)
		}
	}()

	go camObserverPruner(cfg)
}

// camObserverTick evaluates every due log stream once. Mirrors camMonitorTick,
// including the global proxy pause: background journaling hits DVRs/S3 and
// burns AI calls, so it is traffic like everything else.
func camObserverTick(cfg config) {
	if !proxyEnabled.Load() {
		camlog("debug", "observer_tick", map[string]any{"skipped": true, "reason": "proxy_disabled"})
		return
	}
	db, err := openProxyDB(cfg)
	if err != nil {
		camlog("error", "observer_tick", map[string]any{"ok": false, "error": err.Error()})
		return
	}
	defer db.Close()

	now := time.Now()
	due, err := listDueLogStreams(db, now.Format(time.RFC3339))
	if err != nil {
		camlog("error", "observer_tick", map[string]any{"ok": false, "error": err.Error()})
		return
	}
	camlog("debug", "observer_tick", map[string]any{"due": len(due)})
	for _, s := range due {
		camConsiderLogStream(cfg, db, s, now)
	}
}

// camConsiderLogStream evaluates one due stream: active-hours gating, the
// atomic claim (claimLogStream — the no-double-run guard), and — only once
// both pass — spawning the bounded cycle goroutine. The camConsiderWatch shape.
func camConsiderLogStream(cfg config, db *sql.DB, s logStream, now time.Time) {
	interval := camLogStreamInterval(s)

	if !camWithinActiveHoursSpec(s.ActiveHours, now) {
		next := now.Add(interval).Format(time.RFC3339)
		claimed, cerr := claimLogStream(db, s.ID, s.NextRunAt, next)
		if cerr != nil {
			camlog("warn", "observer_claim", map[string]any{"stream_id": s.ID, "site_id": s.SiteID, "ok": false, "error": cerr.Error()})
			return
		}
		if !claimed {
			return // another tick/instance already advanced this stream past the check
		}
		camlog("info", "observer_skip_hours", map[string]any{"stream_id": s.ID, "site_id": s.SiteID, "next_run_at": next})
		return
	}

	select {
	case camObserverSem <- struct{}{}:
	default:
		camlog("debug", "observer_busy", map[string]any{"stream_id": s.ID}) // reconsidered next tick; next_run_at untouched
		return
	}

	next := now.Add(interval).Format(time.RFC3339)
	claimed, cerr := claimLogStream(db, s.ID, s.NextRunAt, next)
	if cerr != nil {
		<-camObserverSem
		camlog("warn", "observer_claim", map[string]any{"stream_id": s.ID, "ok": false, "error": cerr.Error()})
		return
	}
	if !claimed {
		<-camObserverSem // another tick/instance already claimed it — not an error
		return
	}
	if !camObserverTryAcquire(s.ID) {
		<-camObserverSem // a manual "run now" is already in flight for this stream
		camlog("debug", "observer_already_running", map[string]any{"stream_id": s.ID})
		return
	}

	go func() {
		defer func() { <-camObserverSem }()
		defer camObserverRelease(s.ID)
		camExecuteLogCycle(cfg, s)
	}()
}

// triggerLogStreamRun starts stream s in the background immediately, outside
// the scheduler's due/claim cycle — the entrypoint for the manual "run now"
// API. started is false when this stream already has a cycle in flight.
// next_run_at is never touched (the cycle's completion writes only the
// continuity cursor), so a manual run cannot disturb the schedule.
func triggerLogStreamRun(cfg config, s logStream) (started bool) {
	if !camObserverTryAcquire(s.ID) {
		return false
	}
	go func() {
		defer camObserverRelease(s.ID)
		camExecuteLogCycle(cfg, s)
	}()
	return true
}

// camLogStreamInterval resolves a stream's journaling interval, clamped to the
// schema's 1..15 minutes (default 5) so a bad row can never hammer or stall.
func camLogStreamInterval(s logStream) time.Duration {
	m := s.IntervalMinutes
	if m <= 0 {
		m = 5
	}
	if m > 15 {
		m = 15
	}
	return time.Duration(m) * time.Minute
}

// camLogStreamStep resolves the frame-sampling step, clamped to 10s..interval
// (default 30s) — never finer than the archive can sensibly serve, never so
// wide a window yields zero instants.
func camLogStreamStep(s logStream) time.Duration {
	step := time.Duration(s.FrameStepSeconds) * time.Second
	if step <= 0 {
		step = 30 * time.Second
	}
	if step < 10*time.Second {
		step = 10 * time.Second
	}
	if iv := camLogStreamInterval(s); step > iv {
		step = iv
	}
	return step
}

// camObserverWindow computes the [from,to) window one cycle covers.
// to = now − lag (frames must already exist in S3/on the DVR); from resumes
// from the last_window_to continuity cursor iff it parses AND to−from ≤
// 2×interval — anything staler skips to the latest window (catch-up NEVER
// backfills). ok=false means the window is too small (< interval/2) to be
// worth an entry — skip this cycle, the claim already re-armed the schedule.
func camObserverWindow(s logStream, now time.Time, lag time.Duration) (from, to time.Time, ok bool) {
	if lag <= 0 {
		lag = 60 * time.Second
	}
	interval := camLogStreamInterval(s)
	to = now.Add(-lag).Truncate(time.Second)
	from = to.Add(-interval)
	if cur, err := time.Parse(time.RFC3339, strings.TrimSpace(s.LastWindowTo)); err == nil {
		if d := to.Sub(cur); d > 0 && d <= 2*interval {
			from = cur
		}
	}
	if to.Sub(from) < interval/2 {
		return from, to, false
	}
	return from, to, true
}

// ─────────────────────────────── the cycle ───────────────────────────────

// camObserverFailedHeadline marks entries a cycle could not genuinely produce —
// the journal's "no silent holes" contract (the error column has the cause).
const camObserverFailedHeadline = "Logging failed"

// camObserverIdleHeadline marks motion-gated idle entries (no events in the
// window → no model call).
const camObserverIdleHeadline = "No activity"

// camObserverBudget resolves the wall-clock cap for one log cycle.
func camObserverBudget(cfg config) time.Duration {
	if b := cfg.CameraObserverBudget; b > 0 {
		return b
	}
	return 300 * time.Second
}

// camExecuteLogCycle runs one stream cycle end to end: window math, the cycle
// body (camRunLogCycle — which ALWAYS yields an entry), persisting the entry,
// and advancing the continuity cursor. Both the scheduler and the manual "run
// now" land here; neither path touches next_run_at (the scheduler's claim owns
// it — the setWatchLastRunOnly lesson).
func camExecuteLogCycle(cfg config, s logStream) {
	start := time.Now()
	camlog("info", "observer_run_start", map[string]any{"stream_id": s.ID, "site_id": s.SiteID, "name": s.Name})

	db, err := openProxyDB(cfg)
	if err != nil {
		camlog("error", "observer_run", map[string]any{"stream_id": s.ID, "ok": false, "error": err.Error()})
		return
	}
	defer db.Close()

	from, to, ok := camObserverWindow(s, time.Now(), cfg.CameraObserverLag)
	if !ok {
		camlog("info", "observer_skip_window", map[string]any{
			"stream_id": s.ID, "site_id": s.SiteID,
			"from": from.UTC().Format(time.RFC3339), "to": to.UTC().Format(time.RFC3339),
		})
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), camObserverBudget(cfg))
	defer cancel()

	entry := camRunLogCycle(ctx, cfg, db, s, from, to)
	if _, ierr := insertLogEntry(db, entry); ierr != nil {
		camlog("error", "observer_entry", map[string]any{"stream_id": s.ID, "ok": false, "error": ierr.Error()})
	}
	lastErr := ""
	if entry.Headline == camObserverFailedHeadline {
		lastErr = entry.Error
	}
	if cerr := setLogStreamCursor(db, s.ID, nowRFC3339(), entry.ToTS, lastErr); cerr != nil {
		camlog("warn", "observer_cursor", map[string]any{"stream_id": s.ID, "ok": false, "error": cerr.Error()})
	}
	camlog("info", "observer_run_done", map[string]any{
		"stream_id": s.ID, "site_id": s.SiteID, "ok": entry.Headline != camObserverFailedHeadline,
		"gated": entry.MotionGated, "frames": entry.Frames, "error": entry.Error,
		"latency_ms": time.Since(start).Milliseconds(),
	})
}

// camRunLogCycle is the cycle body: resolve cameras → motion gate → sample →
// sheets → ONE analysis call (+ optional single drill-down round) → the built
// logEntry. It NEVER returns without an entry — every failure path yields the
// "Logging failed" entry with the cause in the error column, so the caller
// unconditionally persists and advances the cursor.
func camRunLogCycle(ctx context.Context, cfg config, db *sql.DB, s logStream, from, to time.Time) logEntry {
	entry := logEntry{
		StreamID: s.ID, SiteID: s.SiteID,
		FromTS: from.UTC().Format(time.RFC3339), ToTS: to.UTC().Format(time.RFC3339),
		CameraIDs: "[]", MediaTokens: "[]", DetailTokens: "[]",
	}
	fail := func(msg string) logEntry {
		entry.Headline = camObserverFailedHeadline
		entry.Report = "The activity logger could not produce an entry for this window: " + msg
		entry.Error = msg
		return entry
	}

	site, err := getCameraSite(db, s.SiteID)
	if err != nil {
		return fail("load site: " + err.Error())
	}
	dvrs, err := listCameraDVRs(db, cfg, s.SiteID)
	if err != nil {
		return fail("load dvrs: " + err.Error())
	}
	cams, err := camObserverStreamCameras(db, s)
	if err != nil {
		return fail("load cameras: " + err.Error())
	}
	if len(cams) == 0 {
		return fail("no enabled cameras selected")
	}

	ids := make([]string, 0, len(cams))
	camByID := make(map[string]camera, len(cams))
	for _, c := range cams {
		ids = append(ids, c.ID)
		camByID[c.ID] = c
	}
	entry.CameraIDs = mustJSON(ids)
	dvrByID := make(map[string]CamDVR, len(dvrs))
	for _, d := range dvrs {
		dvrByID[d.ID] = d
	}
	loc, tzName := camSiteTimezone(dvrs)

	// Motion digest + gate. The events query serves both: the idle short-circuit
	// when the gate applies, and the "MOTION EVENTS" digest lines in the user
	// prompt when it doesn't (or when events exist). A store error fails OPEN —
	// the cycle analyzes rather than trusting a broken motion table.
	var motionEvents []camMotionEvent
	motionOK := false
	if cfg.CameraMotionEnabled {
		evs, _, merr := listCameraMotionEvents(db, s.SiteID, ids, entry.FromTS, entry.ToTS, "")
		if merr == nil {
			motionEvents, motionOK = evs, true
		} else {
			camlog("warn", "observer_motion", map[string]any{"stream_id": s.ID, "ok": false, "error": merr.Error()})
		}
	}
	if motionOK && len(motionEvents) == 0 && camObserverGateApplies(cfg, db, s, ids) {
		entry.Headline = camObserverIdleHeadline
		entry.Report = camObserverIdleReport(cams, from, to, loc, tzName)
		entry.MotionGated = true
		return entry
	}

	alias := camObserverAliasFor(cfg, s)
	entry.Alias = alias

	scratch, serr := os.MkdirTemp("", "camobserver-")
	if serr != nil {
		return fail("scratch dir: " + serr.Error())
	}
	defer os.RemoveAll(scratch)

	frames, misses, total := camObserverSampleFrames(ctx, cfg, cams, dvrByID, from, to,
		camLogStreamStep(s), camObserverMaxSheetsFor(cfg), camObserverMaxDVRFramesFor(cfg), scratch)
	entry.Frames = total
	var notes []string
	if misses > 0 {
		notes = append(notes, fmt.Sprintf("s3 misses: %d", misses))
	}
	if total == 0 {
		entry.Error = strings.Join(append(notes, "no frames sampled"), "; ")
		entry.Headline = camObserverFailedHeadline
		entry.Report = "No frames could be sampled for this window — the archive had nothing and the bounded DVR fallback found no recording."
		return entry
	}

	sheets := camObserverBuildSheets(cams, frames, loc, tzName, camObserverMaxSheetsFor(cfg), scratch)
	if len(sheets) == 0 {
		entry.Error = strings.Join(append(notes, "no sheets assembled"), "; ")
		entry.Headline = camObserverFailedHeadline
		entry.Report = "Frames were sampled but no composite sheet could be assembled."
		return entry
	}
	sheetPaths := make([]string, 0, len(sheets))
	legends := make([]string, 0, len(sheets))
	tokens := make([]string, 0, len(sheets))
	for _, sh := range sheets {
		sheetPaths = append(sheetPaths, sh.Path)
		legends = append(legends, sh.Legend)
		if token, perr := camPersistCapture(db, cfg, "", s.SiteID, sh.CameraID, "log_sheet", "sub",
			sh.Path, "image/png", sh.Width, sh.Height, entry.FromTS, entry.ToTS); perr == nil {
			tokens = append(tokens, token)
		} else {
			camlog("warn", "observer_persist_sheet", map[string]any{"stream_id": s.ID, "ok": false, "error": perr.Error()})
		}
	}
	if len(tokens) > 0 {
		entry.MediaTokens = mustJSON(tokens)
	}

	// Prompt context: enrolled avatars scoped to these cameras' DVRs, the site
	// role vocabulary, and the stream's own recent entries (continuity memory).
	avatars, aerr := listCameraAvatars(db, s.SiteID, false)
	if aerr != nil {
		avatars = nil // optional context — never fails the cycle
	}
	policy := camParseSitePolicy(site.PolicyJSON)
	recent, rerr := listRecentLogEntries(db, s.ID, camObserverHeadlineContextFor(cfg))
	if rerr != nil {
		recent = nil
	}

	scoped := camAvatarsForCameras(avatars, cams)

	// Face recognition (bounded): match detected faces in the sampled frames
	// against enrolled human avatars so the model names people reliably instead
	// of guessing from appearance. Additive — a miss/absent sidecar just omits
	// the block and the description-based roster below still applies.
	var identityBlock string
	if cfg.CameraObserverFaceRec && s.FaceRec {
		identities := camObserverRecognizeFaces(ctx, cfg, db, cams, frames, scoped, loc)
		identityBlock = camObserverIdentityBlock(identities, loc, tzName)
	}

	sys := camObserverSystemPrompt(site, dvrs, cams, scoped, policy,
		recent, time.Now().In(loc), loc, tzName, camObserverFullContextFor(cfg))
	user := camObserverUserPrompt(from, to, loc, tzName, legends,
		camObserverMotionDigest(motionEvents, camByID, loc, tzName), identityBlock)

	raw, _, aerr2 := camObserverAnalyzeFn(ctx, cfg, alias, sys, user, sheetPaths)
	if aerr2 != nil {
		entry.Error = strings.Join(append(notes, aerr2.Error()), "; ")
		entry.Headline = camObserverFailedHeadline
		entry.Report = "Analysis failed for this window: " + aerr2.Error()
		return entry
	}
	rep, perr := parseObserverReport(raw)
	if perr != nil {
		// ONE repair round, then salvage — never a parse-crash hole in the journal.
		camlog("warn", "observer_parse", map[string]any{"stream_id": s.ID, "ok": false, "attempt": 1, "error": perr.Error()})
		raw2, _, err2 := camObserverAnalyzeFn(ctx, cfg, alias, sys, user+camObserverRepairSuffix, sheetPaths)
		if err2 == nil {
			if rep2, perr2 := parseObserverReport(raw2); perr2 == nil {
				rep, perr = rep2, nil
			} else if strings.TrimSpace(raw2) != "" {
				raw = raw2 // salvage the repair text — it is the model's latest word
			}
		}
	}
	salvaged := false
	if perr != nil {
		text := strings.TrimSpace(raw)
		if text == "" {
			entry.Error = strings.Join(append(notes, "model returned no usable text"), "; ")
			entry.Headline = camObserverFailedHeadline
			entry.Report = "The model returned no usable text for this window."
			return entry
		}
		rep = observerReport{Headline: camTruncateRunes(camFirstLine(text), 200), Report: camTruncateRunes(text, 8000)}
		notes = append(notes, "salvaged_raw")
		salvaged = true
	}

	// ONE drill-down round: fetch the requested full-quality frames (clamped),
	// show them alongside the sheets, and take the model's FINAL entry. A second
	// round's needs_detail is ignored; an unparseable final reply keeps the draft.
	if !salvaged && len(rep.NeedsDetail) > 0 {
		reqs := camObserverClampDetails(rep.NeedsDetail, camByID, from, to, camObserverMaxDetailFramesFor(cfg))
		if len(reqs) > 0 {
			detailPaths, detailTokens, detailLegend := camObserverFetchDetails(ctx, cfg, db, s, reqs, camByID, dvrByID, loc, tzName, scratch)
			if len(detailTokens) > 0 {
				entry.DetailTokens = mustJSON(detailTokens)
			}
			if len(detailPaths) > 0 {
				user2 := camObserverDetailPrompt(user, rep, detailLegend)
				images := append(append([]string{}, sheetPaths...), detailPaths...)
				raw2, _, err2 := camObserverAnalyzeFn(ctx, cfg, alias, sys, user2, images)
				if err2 == nil {
					if rep2, perr2 := parseObserverReport(raw2); perr2 == nil {
						rep2.NeedsDetail = nil // one round only — further requests are ignored
						rep = rep2
					} else {
						camlog("warn", "observer_parse", map[string]any{"stream_id": s.ID, "ok": false, "attempt": 2, "error": perr2.Error(), "fallback": "draft_kept"})
					}
				} else {
					camlog("warn", "observer_detail_round", map[string]any{"stream_id": s.ID, "ok": false, "error": err2.Error(), "fallback": "draft_kept"})
				}
			}
		}
	}

	entry.Headline = rep.Headline
	entry.Report = rep.Report
	entry.Error = strings.Join(notes, "; ")
	return entry
}

// camObserverStreamCameras resolves the enabled cameras a stream journals:
// every enabled camera on the site when camera_ids is empty (watch semantics),
// else the enabled subset named in camera_ids.
func camObserverStreamCameras(db *sql.DB, s logStream) ([]camera, error) {
	all, err := listCamerasBySite(db, s.SiteID)
	if err != nil {
		return nil, err
	}
	ids := camParseIDArray(s.CameraIDs)
	want := make(map[string]bool, len(ids))
	for _, id := range ids {
		want[id] = true
	}
	out := make([]camera, 0, len(all))
	for _, c := range all {
		if !c.Enabled {
			continue
		}
		if len(ids) > 0 && !want[c.ID] {
			continue
		}
		out = append(out, c)
	}
	return out, nil
}

// camObserverAliasFor resolves the vision alias for a cycle: the stream's own
// alias, then the observer/analysis/investigate ladder from config.
func camObserverAliasFor(cfg config, s logStream) string {
	return firstNonEmpty(strings.TrimSpace(s.AnalysisAlias), cfg.CameraObserverAlias,
		cfg.CameraAnalysisAlias, cfg.CameraInvestigateAlias)
}

// ─────────────────────────────── config resolvers ───────────────────────────────

func camObserverMaxSheetsFor(cfg config) int {
	if n := cfg.CameraObserverMaxSheets; n > 0 {
		return n
	}
	return 3
}

func camObserverMaxDetailFramesFor(cfg config) int {
	if n := cfg.CameraObserverMaxDetailFrames; n > 0 {
		return n
	}
	return 4
}

func camObserverMaxDVRFramesFor(cfg config) int {
	if n := cfg.CameraObserverMaxDVRFrames; n > 0 {
		return n
	}
	return 4
}

func camObserverFullContextFor(cfg config) int {
	if n := cfg.CameraObserverFullContext; n > 0 {
		return n
	}
	return 3
}

// camObserverHeadlineContextFor resolves the continuity-block headline count
// (default 50), capped at 100 so a misconfigured override can't bloat every
// prompt.
func camObserverHeadlineContextFor(cfg config) int {
	n := cfg.CameraObserverHeadlineContext
	if n <= 0 {
		n = 50
	}
	if n > 100 {
		n = 100
	}
	return n
}

// ─────────────────────────────── motion gate (trust guard) ───────────────────────────────

// camObserverGateApplies reports whether this cycle may be motion-gated: the
// stream opted in AND the deployment runs motion listeners AND every selected
// camera is armed AND the site produced at least one motion event in the
// trailing 24h (the dead-listener guard — a listener that stopped feeding
// events must not silently blind the journal). Store errors fail OPEN (no
// gating). Documented residual: a genuinely 24h-quiet site loses gating — the
// safe direction (extra analysis, never lost coverage).
func camObserverGateApplies(cfg config, db *sql.DB, s logStream, ids []string) bool {
	if !s.MotionGate || !cfg.CameraMotionEnabled {
		return false
	}
	armed, err := camerasArmed(db, s.SiteID, ids)
	if err != nil {
		camlog("warn", "observer_gate", map[string]any{"stream_id": s.ID, "ok": false, "error": err.Error()})
		return false // fail open
	}
	if !armed {
		return false
	}
	since := time.Now().Add(-24 * time.Hour).UTC().Format(time.RFC3339)
	has, err := siteHasMotionEventSince(db, s.SiteID, since)
	if err != nil {
		camlog("warn", "observer_gate", map[string]any{"stream_id": s.ID, "ok": false, "error": err.Error()})
		return false // fail open
	}
	return has
}

// camerasArmed reports whether EVERY camera in ids has an enabled
// camera_motion_arm row for the site — the precondition for trusting "zero
// motion events" to mean "nothing happened".
func camerasArmed(db *sql.DB, siteID string, ids []string) (bool, error) {
	arms, err := listCameraMotionArm(db, siteID)
	if err != nil {
		return false, err
	}
	armed := make(map[string]bool, len(arms))
	for _, a := range arms {
		if a.Enabled {
			armed[a.CameraID] = true
		}
	}
	for _, id := range ids {
		if !armed[id] {
			return false, nil
		}
	}
	return len(ids) > 0, nil
}

// siteHasMotionEventSince is the LIMIT-1 dead-listener probe: did the site
// record ANY motion event with started_at >= since?
func siteHasMotionEventSince(db *sql.DB, siteID, sinceUTC string) (bool, error) {
	var one int
	err := db.QueryRow(`SELECT 1 FROM camera_motion_events WHERE site_id = ? AND started_at >= ? LIMIT 1`,
		siteID, sinceUTC).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// ─────────────────────────────── sampling ───────────────────────────────

const (
	camObserverFetchConc      = 16  // parallel S3 GETs while sampling (contact-sheet shape)
	camObserverMaxTotalFrames = 300 // total sampled frames per cycle (3 packed sheets × 100 cells)
	camObserverMaxSheetCells  = 100 // cells per composite sheet (contact-sheet cap)
	camObserverPerCamCols     = 5   // roomy per-camera sheet geometry (≤ MAX_SHEETS cameras)
	camObserverPerCamCellW    = 320
	camObserverPerCamCellH    = 180
	camObserverPackedCols     = 10 // packed multi-camera geometry (contact-sheet numbers)
	camObserverPackedCellW    = 160
	camObserverPackedCellH    = 90
)

// camObserverFrame is one sampled frame: which camera, which instant, and the
// scratch file it was written to.
type camObserverFrame struct {
	Cam  camera
	T    time.Time
	Path string
}

// camObserverFetchFrame is the real camObserverFetchFrameFn: S3 read at the
// archive-grid instant first (when the archive is configured), then — only
// when allowDVR — a single-frame playback grab from the camera's enabled DVR.
// ok=false with nil error means "no frame exists there" (skip, not a fault).
func camObserverFetchFrame(ctx context.Context, cfg config, cam camera, dvr CamDVR, quality string, t time.Time, destPath string, allowDVR bool) (bool, error) {
	q := StreamSub
	if strings.EqualFold(strings.TrimSpace(quality), "main") {
		q = StreamMain
	}
	if camS3Enabled(cfg) {
		ft := camS3SnapArchiveTime(t, avToolSnapInterval(cfg, q))
		if data, err := s3GetObject(ctx, cfg, camS3FrameKey(cam.ID, q.String(), ft)); err == nil {
			if werr := os.WriteFile(destPath, data, 0o600); werr != nil {
				return false, werr
			}
			return true, nil
		}
	}
	if !allowDVR || dvr.ID == "" || !dvr.Enabled {
		return false, nil
	}
	if _, err := captureFrameAtTime(ctx, cfg, dvr, cam.Channel, q, t, destPath); err != nil {
		return false, nil // "no footage at t" — a miss, not a cycle fault
	}
	return true, nil
}

// camObserverInstants returns the sampling instants from + k·step < to.
func camObserverInstants(from, to time.Time, step time.Duration) []time.Time {
	if step <= 0 {
		step = 30 * time.Second
	}
	var out []time.Time
	for t := from; t.Before(to); t = t.Add(step) {
		out = append(out, t.Truncate(time.Second))
	}
	return out
}

// camObserverThinTimes evenly downsamples times to at most max entries,
// preserving order and both endpoints — camCapImages for instants.
func camObserverThinTimes(times []time.Time, max int) []time.Time {
	if max <= 0 || len(times) <= max {
		return times
	}
	if max == 1 {
		return times[len(times)-1:]
	}
	out := make([]time.Time, 0, max)
	last := len(times) - 1
	prev := -1
	for i := 0; i < max; i++ {
		idx := i * last / (max - 1)
		if idx <= prev {
			idx = prev + 1
		}
		if idx > last {
			idx = last
		}
		out = append(out, times[idx])
		prev = idx
	}
	return out
}

// camObserverSampleFrames samples the window's frames for every camera:
// a parallel S3 pass (conc 16) over the per-camera instants, then the bounded
// DVR playback fallback for cameras the archive had NOTHING for, capped at
// dvrBudget fetch ATTEMPTS per cycle. Returns frames grouped per camera (time
// order), the S3 miss count, and the total frames obtained.
func camObserverSampleFrames(ctx context.Context, cfg config, cams []camera, dvrByID map[string]CamDVR,
	from, to time.Time, step time.Duration, maxSheets, dvrBudget int, scratch string) (map[string][]camObserverFrame, int, int) {
	instants := camObserverInstants(from, to, step)
	perCam := camObserverMaxSheetCells
	if len(cams) > maxSheets && len(cams) > 0 {
		perCam = camObserverMaxTotalFrames / len(cams)
		if perCam < 1 {
			perCam = 1
		}
	}
	times := camObserverThinTimes(instants, perCam)

	type slot struct {
		cam  camera
		t    time.Time
		path string
		ok   bool
	}
	slots := make([]slot, 0, len(cams)*len(times))
	for _, c := range cams {
		for _, t := range times {
			slots = append(slots, slot{cam: c, t: t, path: filepath.Join(scratch, fmt.Sprintf("obs_%s_%d.jpg", c.ID, t.Unix()))})
		}
	}
	sem := make(chan struct{}, camObserverFetchConc)
	var wg sync.WaitGroup
	for i := range slots {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()
			sl := &slots[i]
			ok, err := camObserverFetchFrameFn(ctx, cfg, sl.cam, dvrByID[sl.cam.DVRID], "sub", sl.t, sl.path, false)
			if err != nil {
				camlog("debug", "observer_frame", map[string]any{"camera_id": sl.cam.ID, "ok": false, "error": err.Error()})
			}
			sl.ok = ok
		}(i)
	}
	wg.Wait()

	frames := make(map[string][]camObserverFrame, len(cams))
	misses, total := 0, 0
	for _, sl := range slots {
		if sl.ok {
			frames[sl.cam.ID] = append(frames[sl.cam.ID], camObserverFrame{Cam: sl.cam, T: sl.t, Path: sl.path})
			total++
		} else {
			misses++
		}
	}

	// Bounded DVR fallback: only cameras the archive served ZERO frames for
	// (which is every camera when S3 is off), never more than dvrBudget fetch
	// attempts across the whole cycle — degraded coverage beats a hammered DVR.
	var zero []camera
	for _, c := range cams {
		if len(frames[c.ID]) == 0 {
			zero = append(zero, c)
		}
	}
	if len(zero) > 0 && dvrBudget > 0 && len(times) > 0 {
		per := dvrBudget / len(zero)
		if per < 1 {
			per = 1
		}
		remaining := dvrBudget
		for _, c := range zero {
			if remaining <= 0 {
				break
			}
			n := per
			if n > remaining {
				n = remaining
			}
			for _, t := range camObserverThinTimes(times, n) {
				if remaining <= 0 {
					break
				}
				remaining-- // an attempt costs budget whether or not footage exists
				p := filepath.Join(scratch, fmt.Sprintf("obsdvr_%s_%d.jpg", c.ID, t.Unix()))
				ok, err := camObserverFetchFrameFn(ctx, cfg, c, dvrByID[c.DVRID], "sub", t, p, true)
				if err != nil || !ok {
					continue
				}
				frames[c.ID] = append(frames[c.ID], camObserverFrame{Cam: c, T: t, Path: p})
				total++
			}
		}
	}
	return frames, misses, total
}

// ─────────────────────────────── composite sheets ───────────────────────────────

// camObserverSheet is one built composite: the PNG on disk, the camera it
// covers ("" for a packed multi-camera sheet), and the legend line that binds
// its numbered cells to camera+time in the user prompt.
type camObserverSheet struct {
	Path     string
	CameraID string
	Legend   string
	Width    int
	Height   int
}

// camObserverBuildSheets tiles the sampled frames into composite sheets: one
// roomy sheet per camera (cols 5, 320×180) when at most maxSheets cameras have
// frames, else ALL frames packed into ≤ maxSheets sheets at contact-sheet
// geometry (cols 10, 160×90). Cells are numbered per sheet; each legend reads
// "SHEET n — CELL=CAMERA@TIME (tz): 1=Gate 14:00:30 …".
func camObserverBuildSheets(cams []camera, frames map[string][]camObserverFrame, loc *time.Location, tzName string, maxSheets int, scratch string) []camObserverSheet {
	if maxSheets < 1 {
		maxSheets = 1
	}
	var withFrames []camera
	for _, c := range cams {
		if len(frames[c.ID]) > 0 {
			fs := frames[c.ID]
			sort.Slice(fs, func(a, b int) bool { return fs[a].T.Before(fs[b].T) })
			withFrames = append(withFrames, c)
		}
	}
	if len(withFrames) == 0 {
		return nil
	}

	build := func(chunk []camObserverFrame, cameraID string, cols, cellW, cellH int, sheetNo int) (camObserverSheet, bool) {
		items := make([]mosaicItem, 0, len(chunk))
		legendParts := make([]string, 0, len(chunk))
		for i, f := range chunk {
			clock := f.T.In(loc).Format("15:04:05")
			items = append(items, mosaicItem{Index: i + 1, CameraID: f.Cam.ID, Name: clock, Path: f.Path})
			legendParts = append(legendParts, fmt.Sprintf("%d=%s %s", i+1, camDisplayName(f.Cam), clock))
		}
		res, err := buildContactSheet(items, cols, cellW, cellH)
		if err != nil {
			camlog("warn", "observer_sheet", map[string]any{"ok": false, "error": err.Error()})
			return camObserverSheet{}, false
		}
		path := filepath.Join(scratch, fmt.Sprintf("logsheet_%d.png", sheetNo))
		if werr := os.WriteFile(path, res.PNG, 0o600); werr != nil {
			camlog("warn", "observer_sheet", map[string]any{"ok": false, "error": werr.Error()})
			return camObserverSheet{}, false
		}
		return camObserverSheet{
			Path: path, CameraID: cameraID, Width: res.Width, Height: res.Height,
			Legend: fmt.Sprintf("SHEET %d — CELL=CAMERA@TIME (%s): %s", sheetNo, tzName, strings.Join(legendParts, "  ")),
		}, true
	}

	var out []camObserverSheet
	if len(withFrames) <= maxSheets {
		for _, c := range withFrames {
			if sh, ok := build(frames[c.ID], c.ID, camObserverPerCamCols, camObserverPerCamCellW, camObserverPerCamCellH, len(out)+1); ok {
				out = append(out, sh)
			}
		}
		return out
	}

	var all []camObserverFrame
	for _, c := range withFrames {
		all = append(all, frames[c.ID]...)
	}
	perSheet := (len(all) + maxSheets - 1) / maxSheets
	if perSheet > camObserverMaxSheetCells {
		perSheet = camObserverMaxSheetCells
	}
	for start := 0; start < len(all) && len(out) < maxSheets; start += perSheet {
		end := start + perSheet
		if end > len(all) {
			end = len(all)
		}
		if sh, ok := build(all[start:end], "", camObserverPackedCols, camObserverPackedCellW, camObserverPackedCellH, len(out)+1); ok {
			out = append(out, sh)
		}
	}
	return out
}

// ─────────────────────────────── drill-down ───────────────────────────────

// camObserverClampDetails validates and clamps the model's needs_detail
// requests: unknown camera ids and unparseable times are dropped, times are
// clamped into the analyzed window, and at most max requests survive.
func camObserverClampDetails(reqs []observerDetailReq, camByID map[string]camera, from, to time.Time, max int) []observerDetailReq {
	out := make([]observerDetailReq, 0, max)
	for _, r := range reqs {
		if len(out) >= max {
			break
		}
		id := strings.TrimSpace(r.CameraID)
		if _, ok := camByID[id]; !ok {
			continue
		}
		t, err := time.Parse(time.RFC3339, strings.TrimSpace(r.Time))
		if err != nil {
			continue
		}
		if t.Before(from) {
			t = from
		}
		if !t.Before(to) {
			t = to.Add(-time.Second)
		}
		out = append(out, observerDetailReq{CameraID: id, Time: t.UTC().Format(time.RFC3339)})
	}
	return out
}

// camObserverFetchDetails fetches the clamped drill-down frames at main
// quality (S3 first, playback fallback allowed — already bounded by the
// clamp), persists each as a kind "log_detail" capture, and returns the local
// paths for the final model call, the capture tokens, and the legend line.
func camObserverFetchDetails(ctx context.Context, cfg config, db *sql.DB, s logStream, reqs []observerDetailReq,
	camByID map[string]camera, dvrByID map[string]CamDVR, loc *time.Location, tzName, scratch string) (paths, tokens []string, legend string) {
	var lines []string
	for i, r := range reqs {
		cam := camByID[r.CameraID]
		t, err := time.Parse(time.RFC3339, r.Time)
		if err != nil {
			continue
		}
		dest := filepath.Join(scratch, fmt.Sprintf("logdetail_%02d.jpg", i))
		ok, ferr := camObserverFetchFrameFn(ctx, cfg, cam, dvrByID[cam.DVRID], "main", t, dest, true)
		if ferr != nil || !ok {
			continue
		}
		paths = append(paths, dest)
		lines = append(lines, fmt.Sprintf("%d=%s %s", len(paths), camDisplayName(cam), t.In(loc).Format("15:04:05")))
		if token, perr := camPersistCapture(db, cfg, "", s.SiteID, cam.ID, "log_detail", "main",
			dest, "image/jpeg", 0, 0, r.Time, r.Time); perr == nil {
			tokens = append(tokens, token)
		} else {
			camlog("warn", "observer_persist_detail", map[string]any{"stream_id": s.ID, "ok": false, "error": perr.Error()})
		}
	}
	if len(lines) > 0 {
		legend = fmt.Sprintf("DETAIL FRAMES — main quality, IMAGE=CAMERA@TIME (%s): %s", tzName, strings.Join(lines, "  "))
	}
	return paths, tokens, legend
}

// ─────────────────────────────── retention ───────────────────────────────

// camObserverPruner prunes activity-log retention hourly (the standard reaper
// cadence — camMotionRetentionPruner's shape).
func camObserverPruner(cfg config) {
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	camObserverPruneOnce(cfg)
	for range ticker.C {
		camObserverPruneOnce(cfg)
	}
}

// camObserverPruneOnce deletes log entries + hourly rollups older than
// PROXY_CAMERA_ACTIVITY_RETENTION (90d) and daily reports older than
// PROXY_CAMERA_ACTIVITY_REPORT_RETENTION (365d). Connect archives everything
// forever via the pull-sync, so no sync bookkeeping is needed here — and the
// sync counter is never touched, so seq never regresses across deletes.
func camObserverPruneOnce(cfg config) {
	db, err := openProxyDB(cfg)
	if err != nil {
		camlog("warn", "observer_prune", map[string]any{"ok": false, "error": err.Error()})
		return
	}
	defer db.Close()

	retention := cfg.CameraActivityRetention
	if retention <= 0 {
		retention = 90 * 24 * time.Hour
	}
	reportRetention := cfg.CameraActivityReportRetention
	if reportRetention <= 0 {
		reportRetention = 365 * 24 * time.Hour
	}
	cutoff := time.Now().Add(-retention).Format(time.RFC3339)
	reportCutoff := time.Now().Add(-reportRetention).Format(time.RFC3339)

	var pruned int64
	if n, derr := deleteOldLogEntries(db, cutoff); derr != nil {
		camlog("warn", "observer_prune", map[string]any{"ok": false, "error": derr.Error()})
	} else {
		pruned += n
	}
	if n, derr := deleteOldLogHours(db, cutoff); derr != nil {
		camlog("warn", "observer_prune", map[string]any{"ok": false, "error": derr.Error()})
	} else {
		pruned += n
	}
	if n, derr := deleteOldLogDays(db, reportCutoff); derr != nil {
		camlog("warn", "observer_prune", map[string]any{"ok": false, "error": derr.Error()})
	} else {
		pruned += n
	}
	if pruned > 0 {
		camlog("info", "observer_prune", map[string]any{"ok": true, "pruned": pruned, "cutoff": cutoff})
	}
}
