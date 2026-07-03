package main

// camera_motion.go — real-time DVR motion listeners + coalesced motion history.
//
// Hikvision DVRs expose a long-lived HTTP alert stream at
// /ISAPI/Event/notification/alertStream: a multipart/mixed response that stays
// open forever, streaming one <EventNotificationAlert> XML part per motion/smart
// event. This file holds ONE such connection per enabled Hikvision DVR (keyed by
// DVR id), reconnecting with capped exponential backoff on drop, and reconciled
// against the DVR list by a supervisor on a ticker — the same shape as the
// persistent-RTSP stream archiver (camera_stream_archive.go), but per-DVR.
//
// Each alert carries an eventType (VMD / linedetection / fielddetection / …),
// an eventState (active | inactive), and a channelID/dynChannelID. Hik repeats
// eventState=active heartbeats for the whole duration of sustained motion, so we
// COALESCE them into one EPISODE per (camera, eventType): the first active opens
// the episode (one camera_motion_events row), further actives keep it alive, and
// the episode closes on an explicit inactive OR after an idle gap with no
// heartbeat (many firmwares never send inactive — they just stop). Every episode
// is stored for every camera regardless of arming — that is the queryable history
// the motion_search investigate tool reads.
//
// On episode START, if the camera is armed (camera_motion_arm) for that event
// type and past its per-(camera,event) cooldown, a snapshot is captured and a
// signed camera_motion webhook fires (camNotifyMotion) — in a detached goroutine
// so a slow DVR/webhook never stalls the read loop.
//
// SECURITY NOTE: like camera_digest.go's camHTTPClient, camMotionStreamClient is
// deliberately guard-free (the target is a private-LAN DVR) — never point it at a
// user-supplied URL. The outbound webhook, by contrast, goes through the
// SSRF-guarded camPostSignedCallback.

import (
	"context"
	"crypto/tls"
	"database/sql"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// camMotionIdleGap closes an episode when no heartbeat has arrived for this long
// (wall-clock) — the primary close path for firmware that never sends inactive.
const camMotionIdleGap = 8 * time.Second

// camMotionSweepInterval is how often the read loop checks for idle-expired
// episodes (well below camMotionIdleGap so closes are timely).
const camMotionSweepInterval = 2 * time.Second

// camMotionMaxBackoff caps the reconnect backoff so a dead DVR never storms.
const camMotionMaxBackoff = 2 * time.Minute

// hikEventAlert mirrors the fields of a Hikvision <EventNotificationAlert> we
// care about. Numeric channels are captured as strings and parsed tolerantly so a
// malformed value can never abort the whole loop (same discipline as
// camera_hikvision.go's channel-list parse).
type hikEventAlert struct {
	XMLName         xml.Name `xml:"EventNotificationAlert"`
	EventType       string   `xml:"eventType"`
	EventState      string   `xml:"eventState"`
	DateTime        string   `xml:"dateTime"`
	ActivePostCount string   `xml:"activePostCount"`
	ChannelID       string   `xml:"channelID"`
	DynChannelID    string   `xml:"dynChannelID"`
}

// camMotionChannel resolves the physical device channel for an alert, preferring
// channelID and falling back to dynChannelID (0 = unresolved → the caller skips).
// cameras.channel is the raw 1..N device channel (NOT ch*100+stream), matching
// what discovery stores (camera_hikvision.go).
func camMotionChannel(a hikEventAlert) int {
	if ch := atoiDefault(strings.TrimSpace(a.ChannelID), 0); ch > 0 {
		return ch
	}
	return atoiDefault(strings.TrimSpace(a.DynChannelID), 0)
}

// camMotionEventType normalizes an alert's eventType, defaulting a blank value to
// "motion" so an episode always has a stable, queryable type key.
func camMotionEventType(a hikEventAlert) string {
	if t := strings.TrimSpace(a.EventType); t != "" {
		return t
	}
	return "motion"
}

// camMotionStartUTC renders an alert's DVR dateTime as UTC RFC3339 (so stored
// started_at/ended_at range-filter correctly across DVRs on different offsets),
// falling back to server-now when the stamp is missing/unparseable.
func camMotionStartUTC(a hikEventAlert) string {
	if t, err := time.Parse(time.RFC3339, strings.TrimSpace(a.DateTime)); err == nil {
		return t.UTC().Format(time.RFC3339)
	}
	return time.Now().UTC().Format(time.RFC3339)
}

// camMotionParseAlert decodes one alert-stream part body into a hikEventAlert,
// reporting ok=false on an XML parse failure (a heartbeat/garbage part is simply
// skipped by the caller).
func camMotionParseAlert(data []byte) (hikEventAlert, bool) {
	var a hikEventAlert
	if xml.Unmarshal(data, &a) != nil {
		return hikEventAlert{}, false
	}
	return a, true
}

// ─────────────────────────── streaming client ───────────────────────────

// camMotionStreamClient is a no-timeout clone of camHTTPClient (camera_digest.go):
// same guard-free Transport/TLS/Proxy:nil, but Timeout:0 and NO
// ResponseHeaderTimeout — a whole-request or header timeout would kill the
// long-lived alert stream. Connect still bounds via the dialer's 5s timeout.
func camMotionStreamClient() *http.Client {
	return &http.Client{
		Timeout: 0,
		Transport: &http.Transport{
			Proxy:           nil,
			DialContext:     (&net.Dialer{Timeout: 5 * time.Second}).DialContext,
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
			MaxIdleConns:    8,
		},
	}
}

// ─────────────────────────── cooldown ───────────────────────────

// camMotionCooldownAt tracks the last-fired instant per (site,camera,event) so
// repeat episodes within the cooldown are suppressed. Shared across DVR workers,
// so it is mutex-guarded (each worker's read loop is single-goroutine, but there
// is one read loop per DVR).
var camMotionCooldownAt = struct {
	sync.Mutex
	at map[string]time.Time
}{at: map[string]time.Time{}}

// camMotionCooldownAllow spends the cooldown for key, returning true (and stamping
// now) when the last fire was at least ttl ago, false otherwise. ttl <= 0 always
// allows.
func camMotionCooldownAllow(key string, ttl time.Duration) bool {
	if ttl <= 0 {
		return true
	}
	now := time.Now()
	camMotionCooldownAt.Lock()
	defer camMotionCooldownAt.Unlock()
	if last, ok := camMotionCooldownAt.at[key]; ok && now.Sub(last) < ttl {
		return false
	}
	camMotionCooldownAt.at[key] = now
	return true
}

// ─────────────────────────── supervisor ───────────────────────────

// startCameraMotionListeners launches the per-DVR alert-stream supervisor and the
// motion-history retention pruner. Inert unless cfg.CameraMotionEnabled.
func startCameraMotionListeners(cfg config) {
	if !cfg.CameraMotionEnabled {
		camlog("info", "motion_listener", map[string]any{"enabled": false})
		return
	}
	camlog("info", "motion_listener_start", map[string]any{
		"reconcile_ms": camMotionReconcileInterval(cfg).Milliseconds(),
		"cooldown_ms":  camMotionCooldown(cfg).Milliseconds(),
		"retention_h":  camMotionRetention(cfg).Hours(),
	})
	go camMotionSupervisor(cfg)
	go camMotionRetentionPruner(cfg)
}

func camMotionReconcileInterval(cfg config) time.Duration {
	if cfg.CameraMotionReconcile > 0 {
		return cfg.CameraMotionReconcile
	}
	return 30 * time.Second
}

func camMotionReconnectBackoff(cfg config) time.Duration {
	if cfg.CameraMotionReconnect > 0 {
		return cfg.CameraMotionReconnect
	}
	return 5 * time.Second
}

func camMotionCooldown(cfg config) time.Duration {
	if cfg.CameraMotionCooldown > 0 {
		return cfg.CameraMotionCooldown
	}
	return 60 * time.Second
}

func camMotionRetention(cfg config) time.Duration {
	if cfg.CameraMotionRetention > 0 {
		return cfg.CameraMotionRetention
	}
	return 720 * time.Hour
}

// camMotionSupervisor owns the per-DVR worker set (single goroutine → no locking
// on the map) and reconciles it with the enabled Hikvision DVR list on a ticker.
func camMotionSupervisor(cfg config) {
	workers := map[string]context.CancelFunc{} // dvrID -> cancel
	camMotionReconcile(cfg, workers)
	ticker := time.NewTicker(camMotionReconcileInterval(cfg))
	defer ticker.Stop()
	for range ticker.C {
		camMotionReconcile(cfg, workers)
	}
}

// camMotionReconcile starts a listener for every enabled Hikvision DVR that isn't
// running and stops any whose DVR was disabled/removed. While the proxy is paused
// it tears every listener down (background traffic stops, like the scheduler).
func camMotionReconcile(cfg config, workers map[string]context.CancelFunc) {
	if !proxyEnabled.Load() {
		if len(workers) > 0 {
			for id, cancel := range workers {
				cancel()
				delete(workers, id)
			}
			camlog("info", "motion_reconcile", map[string]any{"paused": true, "stopped_all": true})
		}
		return
	}

	db, err := openProxyDB(cfg)
	if err != nil {
		camlog("error", "motion_reconcile", map[string]any{"ok": false, "error": err.Error()})
		return
	}
	dvrs, lerr := listCameraDVRs(db, cfg, "")
	db.Close()
	if lerr != nil {
		camlog("error", "motion_reconcile", map[string]any{"ok": false, "error": lerr.Error()})
		return
	}

	desired := map[string]CamDVR{}
	for _, dvr := range dvrs {
		if !dvr.Enabled || !strings.EqualFold(strings.TrimSpace(dvr.Brand), "hikvision") {
			continue // alertStream is a Hikvision ISAPI endpoint
		}
		desired[dvr.ID] = dvr
	}

	// Stop listeners whose DVR was disabled/removed.
	for id, cancel := range workers {
		if _, ok := desired[id]; !ok {
			cancel()
			delete(workers, id)
			camlog("info", "motion_worker_stop", map[string]any{"dvr_id": id})
		}
	}

	// Start missing listeners.
	started := 0
	for id, dvr := range desired {
		if _, ok := workers[id]; ok {
			continue
		}
		ctx, cancel := context.WithCancel(context.Background())
		workers[id] = cancel
		go camMotionWorker(ctx, cfg, dvr)
		started++
	}
	camlog("info", "motion_reconcile", map[string]any{"running": len(workers), "desired": len(desired), "started": started})
}

// camMotionWorker keeps one DVR's alert stream connected forever: it runs one
// connection, and when that drops it reconnects after a capped exponential
// backoff — until the supervisor cancels ctx (DVR disabled / proxy paused /
// shutdown). Wrapped in recover so a parse panic can never take the process down.
func camMotionWorker(ctx context.Context, cfg config, dvr CamDVR) {
	defer func() {
		if rec := recover(); rec != nil {
			camlog("error", "motion_worker", map[string]any{"dvr_id": dvr.ID, "panic": fmt.Sprintf("%v", rec)})
		}
	}()
	base := camMotionReconnectBackoff(cfg)
	cur := base
	for ctx.Err() == nil {
		start := time.Now()
		err := camMotionRunOnce(ctx, cfg, dvr)
		if ctx.Err() != nil {
			return
		}
		// A connection that lived a while then dropped is a transient blip — reset
		// the backoff so we don't slow-walk reconnects after a long healthy run.
		if time.Since(start) > time.Minute {
			cur = base
		}
		camlog("warn", "motion_worker", map[string]any{
			"dvr_id": dvr.ID, "host": dvr.Host, "restart_in_ms": cur.Milliseconds(), "error": errString(err),
		})
		select {
		case <-ctx.Done():
			return
		case <-time.After(cur):
		}
		if cur *= 2; cur > camMotionMaxBackoff {
			cur = camMotionMaxBackoff
		}
	}
}

// camMotionEpisode is the in-memory state of one open motion episode. All episode
// state is owned by camMotionRunOnce's single goroutine (events + sweeps arrive on
// channels), so no locking is needed.
type camMotionEpisode struct {
	eventID     string
	siteID      string
	cameraID    string
	cameraName  string
	eventType   string
	startedAt   string    // UTC RFC3339 (DVR clock)
	lastEventAt string    // UTC RFC3339 of the last active heartbeat
	lastSeen    time.Time // wall clock, for the idle-gap sweep
}

// camMotionRunOnce opens ONE alert-stream connection, reads its multipart parts,
// coalesces alerts into episodes, and returns when the stream drops or ctx is
// cancelled. All open episodes are closed (ended_at = last heartbeat) on exit so a
// mid-motion disconnect never leaves a dangling open row.
func camMotionRunOnce(ctx context.Context, cfg config, dvr CamDVR) error {
	rawURL := hikHTTPBaseURL(dvr, "/ISAPI/Event/notification/alertStream")
	resp, err := httpDigestGet(ctx, camMotionStreamClient(), rawURL, dvr.Username, dvr.Password)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("alertStream HTTP %d", resp.StatusCode)
	}
	mediaType, params, perr := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if perr != nil || !strings.HasPrefix(mediaType, "multipart/") {
		return fmt.Errorf("unexpected alertStream content-type %q", resp.Header.Get("Content-Type"))
	}
	boundary := strings.TrimSpace(params["boundary"])
	if boundary == "" {
		return fmt.Errorf("alertStream multipart missing boundary")
	}

	db, derr := openProxyDB(cfg)
	if derr != nil {
		return derr
	}
	defer db.Close()

	episodes := map[string]*camMotionEpisode{}
	defer camMotionCloseAll(db, episodes) // runs before db.Close (LIFO): close dangling episodes

	camlog("info", "motion_connected", map[string]any{"dvr_id": dvr.ID, "host": dvr.Host})

	events := make(chan hikEventAlert, 16)
	errc := make(chan error, 1)
	go camMotionReadLoop(ctx, multipart.NewReader(resp.Body, boundary), events, errc)

	sweep := time.NewTicker(camMotionSweepInterval)
	defer sweep.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case rerr := <-errc:
			return rerr
		case ev := <-events:
			camMotionHandleEvent(cfg, db, dvr, ev, episodes)
		case now := <-sweep.C:
			camMotionSweep(db, episodes, now)
		}
	}
}

// camMotionReadLoop reads multipart parts off the alert stream, decodes each as an
// <EventNotificationAlert>, and forwards the decoded alerts on events. It reports
// the terminating read error on errc (buffered) so the run loop can reconnect;
// undecodable/heartbeat parts are skipped.
func camMotionReadLoop(ctx context.Context, mr *multipart.Reader, events chan<- hikEventAlert, errc chan<- error) {
	for {
		part, err := mr.NextPart()
		if err != nil {
			errc <- err
			return
		}
		data, rerr := io.ReadAll(io.LimitReader(part, 1<<20))
		part.Close()
		if rerr != nil {
			errc <- rerr
			return
		}
		if alert, ok := camMotionParseAlert(data); ok {
			select {
			case events <- alert:
			case <-ctx.Done():
				return
			}
		}
	}
}

// camMotionHandleEvent applies one alert to the episode map: an active heartbeat
// opens a new episode (and checks real-time firing) or keeps an existing one
// alive; an inactive closes the matching episode. Unmatched channels are skipped.
func camMotionHandleEvent(cfg config, db *sql.DB, dvr CamDVR, ev hikEventAlert, episodes map[string]*camMotionEpisode) {
	channel := camMotionChannel(ev)
	if channel <= 0 {
		return
	}
	cam, err := findCameraByChannel(db, dvr.ID, channel)
	if err != nil {
		return // no camera mapped to this physical channel — never guess
	}
	etype := camMotionEventType(ev)
	key := cam.ID + "|" + etype
	at := camMotionStartUTC(ev)

	if strings.EqualFold(strings.TrimSpace(ev.EventState), "inactive") {
		if ep, ok := episodes[key]; ok {
			_ = updateMotionEventEnd(db, ep.eventID, at)
			delete(episodes, key)
		}
		return
	}

	// Any non-inactive alert (active, or a stateless event) is treated as a
	// heartbeat for the (camera, eventType) episode.
	if ep, ok := episodes[key]; ok {
		ep.lastEventAt = at
		ep.lastSeen = time.Now()
		return
	}
	// START a new episode.
	id, ierr := insertCameraMotionEvent(db, cam.SiteID, cam.ID, etype, at)
	if ierr != nil {
		camlog("warn", "motion_event", map[string]any{"dvr_id": dvr.ID, "camera_id": cam.ID, "ok": false, "error": ierr.Error()})
		return
	}
	episodes[key] = &camMotionEpisode{
		eventID: id, siteID: cam.SiteID, cameraID: cam.ID, cameraName: camDisplayName(cam),
		eventType: etype, startedAt: at, lastEventAt: at, lastSeen: time.Now(),
	}
	camlog("info", "motion_event", map[string]any{
		"dvr_id": dvr.ID, "site_id": cam.SiteID, "camera_id": cam.ID, "event_type": etype, "started_at": at,
	})
	camMotionMaybeFire(cfg, db, dvr, cam, etype, at)
}

// camMotionSweep closes episodes whose last heartbeat is older than the idle gap
// (the primary close path for firmware that never sends inactive).
func camMotionSweep(db *sql.DB, episodes map[string]*camMotionEpisode, now time.Time) {
	for key, ep := range episodes {
		if now.Sub(ep.lastSeen) >= camMotionIdleGap {
			_ = updateMotionEventEnd(db, ep.eventID, ep.lastEventAt)
			delete(episodes, key)
		}
	}
}

// camMotionCloseAll closes every still-open episode (ended_at = last heartbeat) —
// called on stream drop / cancel so no row is left dangling with a blank ended_at.
func camMotionCloseAll(db *sql.DB, episodes map[string]*camMotionEpisode) {
	for key, ep := range episodes {
		_ = updateMotionEventEnd(db, ep.eventID, ep.lastEventAt)
		delete(episodes, key)
	}
}

// camMotionMaybeFire fires the real-time camera_motion webhook for a just-started
// episode when the camera is armed for this event type and past its cooldown. The
// snapshot capture + webhook run in a DETACHED goroutine (with recover) so a slow
// DVR/webhook never stalls the read loop.
func camMotionMaybeFire(cfg config, db *sql.DB, dvr CamDVR, cam camera, eventType, at string) {
	arm, err := getCameraMotionArm(db, cam.SiteID, cam.ID)
	if err != nil || !arm.Enabled {
		return // not armed
	}
	if !camMotionArmMatches(arm, eventType) {
		return
	}
	ttl := camMotionCooldown(cfg)
	if arm.CooldownSeconds > 0 {
		ttl = time.Duration(arm.CooldownSeconds) * time.Second
	}
	if !camMotionCooldownAllow(cam.SiteID+"|"+cam.ID+"|"+eventType, ttl) {
		camlog("info", "motion_fire", map[string]any{"site_id": cam.SiteID, "camera_id": cam.ID, "event_type": eventType, "suppressed": "cooldown"})
		return
	}
	go func() {
		defer func() {
			if rec := recover(); rec != nil {
				camlog("error", "motion_fire", map[string]any{"camera_id": cam.ID, "panic": fmt.Sprintf("%v", rec)})
			}
		}()
		camMotionFire(cfg, dvr, cam, eventType, at)
	}()
}

// camMotionArmMatches reports whether an arm's event-type filter covers eventType
// (an empty/"[]" filter means "any type"; matching is case-insensitive).
func camMotionArmMatches(arm camMotionArm, eventType string) bool {
	raw := strings.TrimSpace(arm.EventTypes)
	if raw == "" || raw == "[]" {
		return true
	}
	var types []string
	if json.Unmarshal([]byte(raw), &types) != nil {
		return true // unparseable filter → don't silently drop; treat as "any"
	}
	if len(types) == 0 {
		return true
	}
	for _, t := range types {
		if strings.EqualFold(strings.TrimSpace(t), eventType) {
			return true
		}
	}
	return false
}

// camMotionFire captures a fresh sub-stream snapshot for the fired camera and
// sends the camera_motion webhook. Snapshot failures are non-fatal — the webhook
// still fires (with a blank snapshot token) so Connect always learns of the event.
func camMotionFire(cfg config, dvr CamDVR, cam camera, eventType, at string) {
	token := ""
	ctx, cancel := context.WithTimeout(context.Background(), camCaptureTimeout(cfg))
	defer cancel()
	if scratch, serr := os.MkdirTemp("", "cammotionsnap-"); serr == nil {
		defer os.RemoveAll(scratch)
		res, cerr := captureSnapshot(ctx, cfg, dvr, cam.Channel, StreamSub, filepath.Join(scratch, "snap.jpg"))
		if cerr == nil {
			db, derr := openProxyDB(cfg)
			if derr == nil {
				if _, tok, perr := persistSnapshotCapture(db, cfg, cam.SiteID, cam.ID, res, StreamSub); perr == nil {
					token = tok
				}
				db.Close()
			}
		} else {
			camlog("warn", "motion_fire", map[string]any{"camera_id": cam.ID, "ok": false, "error": cerr.Error()})
		}
	}
	camlog("info", "motion_fire", map[string]any{
		"site_id": cam.SiteID, "camera_id": cam.ID, "event_type": eventType, "snapshot": token != "",
	})
	camNotifyMotion(cfg, cam.SiteID, cam.ID, camDisplayName(cam), eventType, at, token)
}

// ─────────────────────────── retention ───────────────────────────

// camMotionRetentionPruner prunes motion-event history older than the retention
// window, hourly (matches the other media/image reapers' cadence).
func camMotionRetentionPruner(cfg config) {
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	camMotionPruneOnce(cfg)
	for range ticker.C {
		camMotionPruneOnce(cfg)
	}
}

func camMotionPruneOnce(cfg config) {
	db, err := openProxyDB(cfg)
	if err != nil {
		camlog("warn", "motion_prune", map[string]any{"ok": false, "error": err.Error()})
		return
	}
	defer db.Close()
	cutoff := time.Now().Add(-camMotionRetention(cfg)).Format(time.RFC3339)
	n, derr := deleteOldCameraMotionEvents(db, cutoff)
	if derr != nil {
		camlog("warn", "motion_prune", map[string]any{"ok": false, "error": derr.Error()})
		return
	}
	if n > 0 {
		camlog("info", "motion_prune", map[string]any{"ok": true, "pruned": n, "cutoff": cutoff})
	}
}
