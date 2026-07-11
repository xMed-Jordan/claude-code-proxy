package main

// cameramonitor.go (WP9 scheduler + WP10 action dispatch) — the last piece that
// turns a saved camera_watches row into an actually-running background monitor,
// and the registry that decides what happens when one fires.
//
// ─── Scheduler (WP9) ───
// startCameraMonitor is called once from runServe (guarded internally by
// cfg.CameraEnabled — the media reaper still runs even when the scheduler is
// off, since on-demand captures accumulate regardless). A ticker
// (PROXY_CAMERA_TICK_INTERVAL, default 15s) polls camera_watches for due rows
// (listDueCameraWatches, camera_store.go) and, for each one:
//   - honors active_hours (skip + advance next_run_at so a closed window is
//     never re-checked every single tick);
//   - atomically claims it (claimCameraWatch: UPDATE ... WHERE next_run_at=?,
//     RowsAffected==1) so two ticks — or two proxy instances — can never
//     double-run the same watch;
//   - runs it in a bounded goroutine (PROXY_CAMERA_WATCH_CONCURRENCY slots) via
//     runEscalation (camera_escalate.go), then dispatches its action if
//     rule_status=="met".
// A startup jitter staggers the very first tick so a fleet of proxy restarts
// doesn't all fire every due watch in the same instant. triggerWatchRun exposes
// the same execution path to the manual "run now" HTTP handler (camerahttp.go);
// an in-memory in-flight guard (camWatchRunMu) prevents the scheduler and a
// manual click from ever running the same watch concurrently.
//
// ─── Action dispatch (WP10) ───
// dispatchAction resolves camera_watches.action_json (parseCamActionConfig)
// to one of four handlers: "none" (explicit no-op), "log" (the v1 default —
// just an observability line + the run's fired_action column, which is what
// the UI badge reads), "connect_callback" and "webhook" (signed/bearer-authed
// POSTs carrying capability media URLs — NEVER DVR credentials — built with an
// SSRF-guarded client, the mirror image of camera_digest.go's deliberately
// guard-free camera-host client: these targets are operator-supplied and
// arbitrary, so they get the same isBlockedIP treatment as openMediaURL).
//
// OBSERVABILITY: every claim/skip/busy/run/dispatch outcome is camlog'd.
import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// ─────────────────────────────── scheduler ───────────────────────────────

// camWatchSem bounds concurrently RUNNING watches (across the whole scheduler,
// not per-tick — a run can take up to the run budget, far longer than the tick
// period). Sized by startCameraMonitor from cfg.CameraWatchConcurrency.
var camWatchSem chan struct{}

// camWatchRunMu/camWatchRunning is the in-flight guard: a watch id present in
// the map has a run in progress right now, whether started by the scheduler or
// a manual "run now" click, so the two paths can never collide.
var (
	camWatchRunMu   sync.Mutex
	camWatchRunning = map[string]bool{}
)

func camTryAcquireWatchRun(id string) bool {
	camWatchRunMu.Lock()
	defer camWatchRunMu.Unlock()
	if camWatchRunning[id] {
		return false
	}
	camWatchRunning[id] = true
	return true
}

func camReleaseWatchRun(id string) {
	camWatchRunMu.Lock()
	delete(camWatchRunning, id)
	camWatchRunMu.Unlock()
}

// startCameraMonitor is the runServe hook (called unconditionally; the
// scheduler itself is what respects cfg.CameraEnabled). Disk hygiene (the media
// reaper) runs regardless, since on-demand snapshots/clips from the setup phase
// or the camera grid's "Live" button accumulate independently of scheduled
// monitoring.
func startCameraMonitor(cfg config) {
	startCameraMediaReaper(cfg)

	if !cfg.CameraEnabled {
		camlog("info", "scheduler", map[string]any{"enabled": false})
		return
	}
	interval := cfg.CameraTickInterval
	if interval <= 0 {
		interval = 15 * time.Second
	}
	conc := cfg.CameraWatchConcurrency
	if conc < 1 {
		conc = 1
	}
	camWatchSem = make(chan struct{}, conc)
	camlog("info", "scheduler_start", map[string]any{"tick_ms": interval.Milliseconds(), "concurrency": conc})

	go func() {
		time.Sleep(camStartupJitter(interval))
		camMonitorTick(cfg)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for range ticker.C {
			camMonitorTick(cfg)
		}
	}()
}

// camStartupJitter staggers the scheduler's first tick so multiple proxy
// instances (or a restart storm) don't all evaluate every due watch at once.
// Not a security-sensitive random value, so a cheap time-derived spread is
// enough — this only smooths startup load, it has no bearing on the atomic
// per-watch claim that actually prevents double-running.
func camStartupJitter(interval time.Duration) time.Duration {
	if interval <= 0 {
		return 0
	}
	n := time.Now().UnixNano()
	if n < 0 {
		n = -n
	}
	return time.Duration(n % int64(interval))
}

func camMonitorTick(cfg config) {
	// The local control panel's stop/start switch is a global pause on ALL
	// proxy activity (requireProxyEnabled rejects forwarded requests with 503
	// while it's off). Background camera monitoring is traffic too — it hits
	// DVRs and burns AI-backend calls — so a tick is a full no-op while the
	// operator has deliberately paused the proxy, exactly like everything else.
	if !proxyEnabled.Load() {
		camlog("debug", "scheduler_tick", map[string]any{"skipped": true, "reason": "proxy_disabled"})
		return
	}

	db, err := openProxyDB(cfg)
	if err != nil {
		camlog("error", "scheduler_tick", map[string]any{"ok": false, "error": err.Error()})
		return
	}
	defer db.Close()

	now := time.Now()
	due, err := listDueCameraWatches(db, now.Format(time.RFC3339))
	if err != nil {
		camlog("error", "scheduler_tick", map[string]any{"ok": false, "error": err.Error()})
		return
	}
	camlog("debug", "scheduler_tick", map[string]any{"due": len(due)})
	for _, w := range due {
		camConsiderWatch(cfg, db, w, now)
	}
}

// camConsiderWatch evaluates one due watch: active-hours gating, the atomic
// claim (the no-double-run guard), and — only once both pass — spawning the
// bounded run goroutine.
func camConsiderWatch(cfg config, db *sql.DB, w watch, now time.Time) {
	interval := camWatchIntervalDur(w)

	if !camWithinActiveHours(w, now) {
		next := now.Add(interval).Format(time.RFC3339)
		claimed, cerr := claimCameraWatch(db, w.ID, w.NextRunAt, next)
		if cerr != nil {
			camlog("warn", "scheduler_claim", map[string]any{"watch_id": w.ID, "site_id": w.SiteID, "ok": false, "error": cerr.Error()})
			return
		}
		if !claimed {
			return // another tick/instance already advanced this watch past the active-hours check
		}
		camRecordSkippedRun(db, w, now)
		camlog("info", "scheduler_skip_hours", map[string]any{"watch_id": w.ID, "site_id": w.SiteID, "next_run_at": next})
		return
	}

	select {
	case camWatchSem <- struct{}{}:
	default:
		camlog("debug", "scheduler_busy", map[string]any{"watch_id": w.ID}) // reconsidered next tick; next_run_at is untouched
		return
	}

	next := now.Add(interval).Format(time.RFC3339)
	claimed, cerr := claimCameraWatch(db, w.ID, w.NextRunAt, next)
	if cerr != nil {
		<-camWatchSem
		camlog("warn", "scheduler_claim", map[string]any{"watch_id": w.ID, "ok": false, "error": cerr.Error()})
		return
	}
	if !claimed {
		<-camWatchSem // another tick/instance already claimed it — not an error
		return
	}
	if !camTryAcquireWatchRun(w.ID) {
		<-camWatchSem // a manual "run now" is already in flight for this watch
		camlog("debug", "scheduler_already_running", map[string]any{"watch_id": w.ID})
		return
	}

	go func() {
		defer func() { <-camWatchSem }()
		defer camReleaseWatchRun(w.ID)
		camExecuteWatch(cfg, w, next)
	}()
}

// triggerWatchRun starts watch w in the background immediately, outside the
// scheduler's due/claim cycle — the entrypoint for the manual "run now" API
// (camerahttp.go). started is false when this watch already has a run in
// flight (a double-click, or a race with the scheduler). next_run_at is left
// exactly as it was (camExecuteWatch's "" sentinel), so a manual run never
// disturbs the watch's own schedule.
func triggerWatchRun(cfg config, w watch) (started bool) {
	if !camTryAcquireWatchRun(w.ID) {
		return false
	}
	go func() {
		defer camReleaseWatchRun(w.ID)
		camExecuteWatch(cfg, w, "")
	}()
	return true
}

// camWatchIntervalDur resolves a watch's tick interval, floored at 30s so a
// misconfigured 0/1-second interval can never hammer a DVR or the analysis
// backend.
func camWatchIntervalDur(w watch) time.Duration {
	sec := w.IntervalSeconds
	if sec <= 0 {
		sec = 300
	}
	if sec < 30 {
		sec = 30
	}
	return time.Duration(sec) * time.Second
}

// camWithinActiveHours reports whether now falls inside the watch's configured
// active window — the watch-typed wrapper over camWithinActiveHoursSpec.
func camWithinActiveHours(w watch, now time.Time) bool {
	return camWithinActiveHoursSpec(w.ActiveHours, now)
}

// camWithinActiveHoursSpec reports whether now falls inside an active-hours
// spec ("" → always active), shared by the watch scheduler and the activity-log
// observer (camera_observer.go). Format: comma-separated "HH:MM-HH:MM" ranges
// in server-local time; a range whose end is before its start wraps past
// midnight (e.g. "22:00-06:00"). Any value that fails to parse at all is
// treated as always-active (fail open) so a typo can never silently stop
// monitoring.
func camWithinActiveHoursSpec(spec string, now time.Time) bool {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return true
	}
	minutes := now.Hour()*60 + now.Minute()
	parsedAny := false
	for _, part := range strings.Split(spec, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		from, to, ok := camParseHourRange(part)
		if !ok {
			continue
		}
		parsedAny = true
		if from <= to {
			if minutes >= from && minutes < to {
				return true
			}
		} else if minutes >= from || minutes < to { // wraps past midnight
			return true
		}
	}
	return !parsedAny
}

func camParseHourRange(s string) (fromMin, toMin int, ok bool) {
	a, b, found := strings.Cut(s, "-")
	if !found {
		return 0, 0, false
	}
	fm, ok1 := camParseHHMM(a)
	tm, ok2 := camParseHHMM(b)
	if !ok1 || !ok2 {
		return 0, 0, false
	}
	return fm, tm, true
}

func camParseHHMM(s string) (int, bool) {
	h, m, found := strings.Cut(strings.TrimSpace(s), ":")
	if !found {
		return 0, false
	}
	hh, err1 := strconv.Atoi(strings.TrimSpace(h))
	mm, err2 := strconv.Atoi(strings.TrimSpace(m))
	if err1 != nil || err2 != nil || hh < 0 || hh > 23 || mm < 0 || mm > 59 {
		return 0, false
	}
	return hh*60 + mm, true
}

// camWatchRunBudget bounds how long the scheduler waits for one watch's
// runEscalation call, a little beyond the loop's own internal run-budget
// context so runEscalation is always the one that times the run out cleanly.
func camWatchRunBudget(cfg config) time.Duration {
	b := cfg.CameraRunBudget
	if b <= 0 {
		b = 300 * time.Second
	}
	return b + 30*time.Second
}

// camExecuteWatch runs one watch end to end: load its cameras, run the
// escalation loop, dispatch the action if the rule fired, and record
// last_run_at. nextRunAt is the value the atomic claim already reserved on
// camera_watches; "" (from a manual trigger) means "leave the schedule as is".
func camExecuteWatch(cfg config, w watch, nextRunAt string) {
	start := time.Now()
	camlog("info", "watch_run_start", map[string]any{"watch_id": w.ID, "site_id": w.SiteID, "name": w.Name, "mode": w.Mode})

	cams, err := camLoadWatchCameras(cfg, w)
	if err != nil {
		camlog("error", "watch_run", map[string]any{"watch_id": w.ID, "ok": false, "error": err.Error()})
		camRecordWatchCompletion(cfg, w, nextRunAt)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), camWatchRunBudget(cfg))
	res, rerr := runEscalation(ctx, cfg, w, cams)
	cancel()
	if rerr != nil {
		camlog("error", "watch_run", map[string]any{"watch_id": w.ID, "run_id": res.RunID, "ok": false, "error": rerr.Error()})
	}

	camFireAndRecord(cfg, w, res)
	camRecordWatchCompletion(cfg, w, nextRunAt)

	camlog("info", "watch_run_done", map[string]any{
		"watch_id": w.ID, "run_id": res.RunID, "ok": rerr == nil, "status": res.Status,
		"rule_status": res.RuleStatus, "fired": len(res.TriggeredActions) > 0,
		"latency_ms": time.Since(start).Milliseconds(),
	})
}

// camLoadWatchCameras resolves the concrete camera rows a watch monitors: every
// camera on the site when camera_ids is empty (a site-wide watch), else the
// subset named in camera_ids. Disabled cameras/DVRs are filtered later, inside
// runEscalationServer, which needs to log/skip them individually.
func camLoadWatchCameras(cfg config, w watch) ([]camera, error) {
	db, err := openProxyDB(cfg)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	all, err := listCamerasBySite(db, w.SiteID)
	if err != nil {
		return nil, err
	}
	ids := camParseIDArray(w.CameraIDs)
	if len(ids) == 0 {
		return all, nil
	}
	want := make(map[string]bool, len(ids))
	for _, id := range ids {
		want[id] = true
	}
	out := make([]camera, 0, len(ids))
	for _, c := range all {
		if want[c.ID] {
			out = append(out, c)
		}
	}
	return out, nil
}

// camParseIDArray decodes a JSON string array column ("" or invalid → nil).
func camParseIDArray(s string) []string {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	var ids []string
	if err := json.Unmarshal([]byte(s), &ids); err != nil {
		return nil
	}
	return ids
}

// camRecordWatchCompletion stamps last_run_at on the watch. For a scheduled run
// it also re-affirms next_run_at with the EXACT value the scheduler's atomic
// claim already reserved, so recording completion never clobbers that claim. For
// a manual trigger (the "" sentinel) it stamps ONLY last_run_at and leaves
// next_run_at untouched in the DB: the in-memory w.NextRunAt is a snapshot from
// trigger time, and a concurrent scheduler tick may have already advanced the
// persisted next_run_at — re-writing the stale snapshot would revert that claim
// to a past timestamp and cause an unintended duplicate scheduled run.
func camRecordWatchCompletion(cfg config, w watch, nextRunAt string) {
	db, err := openProxyDB(cfg)
	if err != nil {
		camlog("warn", "watch_last_run", map[string]any{"watch_id": w.ID, "ok": false, "error": err.Error()})
		return
	}
	defer db.Close()
	if nextRunAt == "" {
		// Manual "run now": never touch the schedule the scheduler owns.
		if err := setWatchLastRunOnly(db, w.ID, nowRFC3339()); err != nil {
			camlog("warn", "watch_last_run", map[string]any{"watch_id": w.ID, "ok": false, "error": err.Error()})
		}
		return
	}
	if err := setWatchLastRun(db, w.ID, nowRFC3339(), nextRunAt); err != nil {
		camlog("warn", "watch_last_run", map[string]any{"watch_id": w.ID, "ok": false, "error": err.Error()})
	}
}

// camRecordSkippedRun persists a terminal camera_watch_runs row (status
// "skipped") so a watch that never fires during a closed active_hours window
// still shows up in run history / diagnostics instead of silently vanishing.
// Best-effort: a failure here only costs observability, never the schedule
// advancement the caller already committed via claimCameraWatch. Reuses the
// tick's own db handle (camMonitorTick) rather than opening a new one.
func camRecordSkippedRun(db *sql.DB, w watch, now time.Time) {
	ts := now.Format(time.RFC3339)
	if _, err := insertCameraWatchRun(db, camWatchRun{
		WatchID: w.ID, SiteID: w.SiteID, StartedAt: ts, FinishedAt: ts,
		Status: "skipped", Suspicion: "none", RuleStatus: "not_met",
		Summary: "outside active_hours window (" + w.ActiveHours + ")",
	}); err != nil {
		camlog("warn", "scheduler_skip_hours", map[string]any{"watch_id": w.ID, "ok": false, "error": err.Error()})
	}
}

// camFireAndRecord dispatches the run's configured action — ONLY when
// rule_status=="met", the loop's own final gate — and folds the outcome back
// onto the persisted camera_watch_runs row's fired_action/action_result
// columns (runEscalation already wrote every other field via finishCameraWatchRun).
func camFireAndRecord(cfg config, w watch, res runResult) {
	if res.RuleStatus != "met" || res.RunID == "" {
		return
	}
	db, err := openProxyDB(cfg)
	if err != nil {
		camlog("error", "action_dispatch", map[string]any{"watch_id": w.ID, "run_id": res.RunID, "ok": false, "error": err.Error()})
		return
	}
	defer db.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	firedType, resultJSON := dispatchAction(ctx, cfg, db, w, res)

	if err := finishCameraWatchRun(db, camWatchRun{
		ID: res.RunID, WatchID: w.ID, SiteID: w.SiteID, FinishedAt: nowRFC3339(),
		Status: res.Status, Suspicion: res.Suspicion, RuleStatus: res.RuleStatus,
		Rounds: res.Rounds, Summary: res.Summary, TranscriptJSON: res.Transcript,
		FiredAction: firedType, ActionResult: resultJSON,
	}); err != nil {
		camlog("warn", "action_persist", map[string]any{"watch_id": w.ID, "run_id": res.RunID, "ok": false, "error": err.Error()})
	}
}

// ─────────────────────────────── action dispatch (WP10) ───────────────────────────────

// camActionConfig is the decoded shape of camera_watches.action_json. Type ""
// resolves to "log" (the safe v1 default: record + UI badge, no external
// call). token_enc is the connect_callback bearer token or the webhook's HMAC
// signing secret, encrypted at rest with encryptSecret exactly like a DVR
// password — never stored or echoed back in plaintext.
type camActionConfig struct {
	Type     string `json:"type"`
	URL      string `json:"url,omitempty"`
	TokenEnc string `json:"token_enc,omitempty"`
}

// parseCamActionConfig decodes action_json, defaulting to "log" on an empty or
// unparseable value so a watch never silently loses its firing behavior.
func parseCamActionConfig(raw string) camActionConfig {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return camActionConfig{Type: "log"}
	}
	var ac camActionConfig
	if err := json.Unmarshal([]byte(raw), &ac); err != nil {
		return camActionConfig{Type: "log"}
	}
	ac.Type = strings.ToLower(strings.TrimSpace(ac.Type))
	if ac.Type == "" {
		ac.Type = "log"
	}
	return ac
}

type camActionFunc func(ctx context.Context, cfg config, db *sql.DB, w watch, res runResult, ac camActionConfig) (map[string]any, error)

var camActionRegistry = map[string]camActionFunc{
	"none":             camActionNone,
	"log":              camActionLog,
	"connect_callback": camActionConnectCallback,
	"webhook":          camActionWebhook,
}

// dispatchAction resolves a watch's configured action and runs it, returning
// the fired action type plus a JSON-encoded result for camera_watch_runs.
// Called ONLY when rule_status=="met". No DVR credentials ever flow through
// here — only capability media URLs and text summaries.
func dispatchAction(ctx context.Context, cfg config, db *sql.DB, w watch, res runResult) (firedType, resultJSON string) {
	ac := parseCamActionConfig(w.ActionJSON)
	fn, ok := camActionRegistry[ac.Type]
	if !ok {
		camlog("warn", "action_dispatch", map[string]any{"watch_id": w.ID, "run_id": res.RunID, "type": ac.Type, "ok": false, "error": "unknown action type"})
		return ac.Type, mustJSON(map[string]any{"ok": false, "error": "unknown action type " + ac.Type})
	}
	start := time.Now()
	result, err := fn(ctx, cfg, db, w, res, ac)
	ok = err == nil
	level := "info"
	if !ok {
		level = "error"
	}
	camlog(level, "action_dispatch", map[string]any{
		"watch_id": w.ID, "run_id": res.RunID, "site_id": w.SiteID, "type": ac.Type, "ok": ok,
		"latency_ms": time.Since(start).Milliseconds(), "error": camErrStr(err),
	})
	if err != nil {
		result = map[string]any{"ok": false, "error": err.Error()}
	}
	return ac.Type, mustJSON(result)
}

func camActionNone(ctx context.Context, cfg config, db *sql.DB, w watch, res runResult, ac camActionConfig) (map[string]any, error) {
	return map[string]any{"ok": true, "note": "no action configured (type=none)"}, nil
}

// camActionLog is the v1 default: the alert is already fully durable in
// camera_watch_runs (and camlog'd here again at "warn" so it stands out in
// .proxy.log) — the "UI badge" the plan calls for is simply the run's
// fired_action/rule_status, which the run-history view already renders.
func camActionLog(ctx context.Context, cfg config, db *sql.DB, w watch, res runResult, ac camActionConfig) (map[string]any, error) {
	camlog("warn", "action_fired", map[string]any{
		"watch_id": w.ID, "run_id": res.RunID, "site_id": w.SiteID,
		"suspicion": res.Suspicion, "rule_status": res.RuleStatus, "summary": res.Summary,
		"triggered_actions": len(res.TriggeredActions),
	})
	return map[string]any{"ok": true, "note": "recorded in the run history and diagnostics log"}, nil
}

func camActionConnectCallback(ctx context.Context, cfg config, db *sql.DB, w watch, res runResult, ac camActionConfig) (map[string]any, error) {
	rawURL := strings.TrimSpace(ac.URL)
	if rawURL == "" {
		return nil, errors.New("connect_callback: no callback url configured on this watch")
	}
	token := camDecryptActionSecret(cfg, ac.TokenEnc)
	return camDispatchPOST(ctx, rawURL, token, camActionPayload(cfg, db, w, res), nil)
}

func camActionWebhook(ctx context.Context, cfg config, db *sql.DB, w watch, res runResult, ac camActionConfig) (map[string]any, error) {
	rawURL := strings.TrimSpace(ac.URL)
	if rawURL == "" {
		return nil, errors.New("webhook: no url configured on this watch")
	}
	secret := camDecryptActionSecret(cfg, ac.TokenEnc)
	body, err := json.Marshal(camActionPayload(cfg, db, w, res))
	if err != nil {
		return nil, err
	}
	headers := map[string]string{}
	if secret != "" {
		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write(body)
		headers["X-Camera-Signature"] = "sha256=" + hex.EncodeToString(mac.Sum(nil))
	}
	return camDispatchPOSTBody(ctx, rawURL, body, headers)
}

func camDecryptActionSecret(cfg config, enc string) string {
	enc = strings.TrimSpace(enc)
	if enc == "" {
		return ""
	}
	if s, err := decryptSecret(cfg, enc); err == nil {
		return s
	}
	return ""
}

// camActionPayload is the event body sent to connect_callback/webhook targets:
// enough for the receiver to render/relay an alert (e.g. to Connect's
// agents/WhatsApp surface) without ever seeing a DVR credential — only
// capability media URLs under /camera/media/<token>.
func camActionPayload(cfg config, db *sql.DB, w watch, res runResult) map[string]any {
	return map[string]any{
		"event":             "camera_watch_alert",
		"watch_id":          w.ID,
		"watch_name":        w.Name,
		"site_id":           w.SiteID,
		"run_id":            res.RunID,
		"summary":           res.Summary,
		"suspicion":         res.Suspicion,
		"rule_status":       res.RuleStatus,
		"rounds":            res.Rounds,
		"triggered_actions": res.TriggeredActions,
		"media":             camRunMediaURLs(cfg, db, res.RunID),
		"at":                nowRFC3339(),
	}
}

// camRunMediaURLs lists a run's persisted captures as capability URLs (no
// tokens are ever embedded in DVR-facing code; these are already-served files).
func camRunMediaURLs(cfg config, db *sql.DB, runID string) []map[string]any {
	if runID == "" {
		return nil
	}
	caps, err := listCameraCaptures(db, runID)
	if err != nil {
		return nil
	}
	base := strings.TrimRight(clientFacingBaseURL(cfg), "/")
	out := make([]map[string]any, 0, len(caps))
	for _, c := range caps {
		out = append(out, map[string]any{
			"kind": c.Kind, "quality": c.Quality, "camera_id": c.CameraID,
			"content_type": c.ContentType, "url": base + "/camera/media/" + c.Token,
		})
	}
	return out
}

// ─────────────────────────────── outbound dispatch (SSRF-guarded) ───────────────────────────────

// camGuardedHTTPClient is the outbound client for action targets. Unlike
// camera_digest.go's deliberately guard-free camera-host client (the target
// there — a DVR — is a private LAN address on purpose), connect_callback/
// webhook URLs are operator-supplied and arbitrary, so this blocks
// private/loopback/link-local/CGNAT destinations exactly like agymedia.go's
// openMediaURL downloader — a malicious or mistyped action URL can never be
// used to probe the proxy's own host or LAN.
func camGuardedHTTPClient() *http.Client {
	dialer := &net.Dialer{
		Timeout: 10 * time.Second,
		Control: func(network, address string, c syscall.RawConn) error {
			host, _, _ := net.SplitHostPort(address)
			ip := net.ParseIP(host)
			if ip == nil || isBlockedIP(ip) {
				return fmt.Errorf("blocked address %s", host)
			}
			return nil
		},
	}
	return &http.Client{
		Timeout:   20 * time.Second,
		Transport: &http.Transport{DialContext: dialer.DialContext, ResponseHeaderTimeout: 15 * time.Second},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 3 {
				return fmt.Errorf("too many redirects")
			}
			return nil
		},
	}
}

// camDispatchPOST JSON-encodes payload and POSTs it, adding a Bearer
// Authorization header when bearer is non-empty.
func camDispatchPOST(ctx context.Context, rawURL, bearer string, payload any, extraHeaders map[string]string) (map[string]any, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	if extraHeaders == nil {
		extraHeaders = map[string]string{}
	}
	if bearer != "" {
		extraHeaders["Authorization"] = "Bearer " + bearer
	}
	return camDispatchPOSTBody(ctx, rawURL, body, extraHeaders)
}

// camDispatchPOSTBody sends a pre-built JSON body through the SSRF-guarded
// client and classifies the response, truncating whatever the endpoint sends
// back so a chatty/misbehaving receiver can never bloat camera_watch_runs.
func camDispatchPOSTBody(ctx context.Context, rawURL string, body []byte, headers map[string]string) (map[string]any, error) {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("invalid action url")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rawURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := camGuardedHTTPClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()
	respBody := bytes.TrimSpace(readAllLimited(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("endpoint returned status %d: %s", resp.StatusCode, truncateString(string(respBody), 300))
	}
	return map[string]any{"ok": true, "status": resp.StatusCode, "response": truncateString(string(respBody), 2000)}, nil
}

func readAllLimited(r io.Reader, max int64) []byte {
	b, _ := io.ReadAll(io.LimitReader(r, max))
	return b
}
