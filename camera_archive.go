package main

// camera_archive.go — background archiver (WP2): continuously captures every
// enabled camera's sub stream (fast cadence) and main stream (slower cadence)
// and writes one JPEG per interval to S3 object storage (camera_s3.go owns the
// client + camS3FrameKey scheme).
//
// Guarded by cfg.CameraArchiveEnabled AND camS3Enabled(cfg) — with either off
// this file is entirely inert, so an operator can flip the feature flag on
// without yet having entered S3 credentials (or vice versa) without anything
// actually running.
//
// Design: sub and main are archived by TWO INDEPENDENT loops, each with its own
// time.Ticker and its own worker pool. The fast sub loop ticks at
// cfg.CameraArchiveInterval; the slow main loop ticks at
// cfg.CameraArchiveMainInterval. Running them separately means a batch of slow
// full-resolution main grabs can never delay or drop a sub tick (the previous
// single-pool design let a main sweep monopolize the shared workers). Each cycle
// runs to completion before the next iteration begins, so Go's time.Ticker —
// which drops ticks a slow receiver hasn't read yet rather than queuing them —
// naturally gives us "skip the next tick if the previous one is still running".
//
// Concurrency: both pools still acquire the shared captureSem (sized from
// cfg.CameraCaptureConcurrency, shared with every live/investigate/monitor
// capture), so the two pools together are budgeted at captureSem-1 slots — main
// takes one, sub takes the rest — leaving at least one slot always free for the
// live proxy. A forever-running archiver can therefore never hold every capture
// slot.
//
// Every camera on every enabled DVR across every site is archived — no
// per-camera opt-in/out beyond the existing enabled flags (camera.Enabled /
// dvr.Enabled), the same ones the investigate tools and watch scheduler already
// respect. The camera/DVR list is loaded fresh every cycle (not cached) so a
// newly added camera is picked up on its very next tick.
//
// SUB frames reuse captureSnapshot (camera_capture.go) — HTTP snapshot first,
// ffmpeg single-frame fallback. MAIN frames deliberately bypass captureSnapshot
// and call captureSnapshotFFmpeg directly against the brand's RTSP main LiveURL:
// Hikvision's ISAPI ".../picture" endpoint (what captureSnapshot's HTTP-first
// branch would hit) returns a small thumbnail regardless of which channel/stream
// number is requested, so it can never stand in for a true full-resolution
// archive frame. Every frame is captured into its own temp directory that is
// always removed, win or lose — this file does not touch camera_captures/served
// media at all; archived frames are read back directly from S3
// (camera_investigate.go read-through, WP1), never through the capability-token
// media store.
//
// Frames are never deleted here — retention is forever / handled by an S3
// lifecycle rule out of band.
import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// startCameraArchiver launches the background frame archiver. Called
// unconditionally from runServe (cli.go) — the checks below are what actually
// gate it, mirroring startCameraMonitor's own internal cfg.CameraEnabled gate.
func startCameraArchiver(cfg config) {
	if !cfg.CameraArchiveEnabled {
		camlog("info", "archiver", map[string]any{"enabled": false, "reason": "archive_disabled"})
		return
	}
	if !camS3Enabled(cfg) {
		camlog("warn", "archiver", map[string]any{"enabled": false, "reason": "s3_not_configured"})
		return
	}

	interval := cfg.CameraArchiveInterval
	if interval <= 0 {
		interval = 15 * time.Second
	}
	mainInterval := cfg.CameraArchiveMainInterval
	if mainInterval <= 0 {
		mainInterval = 30 * time.Second
	}

	captureSlots := cfg.CameraCaptureConcurrency
	if captureSlots < 1 {
		captureSlots = 1
	}
	// Sub and main run as INDEPENDENT loops, each with its OWN worker pool, so a
	// batch of slow full-res main grabs can never delay the fast sub cadence (the
	// previous single-pool design let a main sweep monopolize the workers and drop
	// sub ticks). Both pools still acquire the shared captureSem, so split a budget
	// of captureSem-1 slots between them (leaving ≥1 for live/investigate/monitor
	// captures): main gets one slot, sub — the high-frequency path that must keep
	// up — gets the rest. An explicit CameraArchiveConcurrency, if lower, caps the
	// total.
	budget := captureSlots - 1
	if budget < 1 {
		budget = 1
	}
	if c := cfg.CameraArchiveConcurrency; c >= 1 && c < budget {
		budget = c
	}
	mainWorkers := 1
	subWorkers := budget - mainWorkers
	if subWorkers < 1 {
		subWorkers = 1
	}

	camlog("info", "archiver_start", map[string]any{
		"sub_interval_ms": interval.Milliseconds(), "main_interval_ms": mainInterval.Milliseconds(),
		"sub_workers": subWorkers, "main_workers": mainWorkers, "capture_slots": captureSlots,
		"bucket": cfg.CameraS3Bucket, "endpoint": cfg.CameraS3Endpoint,
	})

	go camArchiveStreamLoop(cfg, "sub", StreamSub, interval, subWorkers)
	go camArchiveStreamLoop(cfg, "main", StreamMain, mainInterval, mainWorkers)
}

// camArchiveStreamLoop runs one stream's archiver forever: it ticks at `interval`
// and archives a frame of quality q for every enabled camera each tick. Sub and
// main run as SEPARATE loops (separate tickers + separate worker pools) so the
// slow main sweep never delays the fast sub cadence. A startup jitter staggers the
// first tick across a fleet restart. Because a cycle runs to completion before the
// next iteration, Go's time.Ticker (which drops, not queues, unread ticks) gives
// us "skip the next tick if this one is still running" for free.
func camArchiveStreamLoop(cfg config, label string, q StreamQuality, interval time.Duration, workers int) {
	if workers < 1 {
		workers = 1
	}
	time.Sleep(camStartupJitter(interval))
	camArchiveStreamCycle(cfg, label, q, interval, workers)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for range ticker.C {
		camArchiveStreamCycle(cfg, label, q, interval, workers)
	}
}

// camArchiveStreamCycle archives one frame of quality q for every enabled camera
// on every enabled DVR across all sites (listCameraDVRs with an empty siteID),
// through a fixed-size worker pool, returning only once every dispatched job has
// finished. The write instant is snapped to THIS stream's grid
// (camS3SnapArchiveTime) so the object key matches what the read-through looks up.
// Skipped entirely while the operator has paused the proxy (same as the watch
// scheduler) — archiving is background traffic too.
func camArchiveStreamCycle(cfg config, label string, q StreamQuality, interval time.Duration, workers int) {
	if !proxyEnabled.Load() {
		return
	}
	start := time.Now()
	t := camS3SnapArchiveTime(start, interval)

	db, err := openProxyDB(cfg)
	if err != nil {
		camlog("error", "archive_cycle", map[string]any{"stream": label, "ok": false, "error": err.Error()})
		return
	}
	defer db.Close()

	dvrs, err := listCameraDVRs(db, cfg, "")
	if err != nil {
		camlog("error", "archive_cycle", map[string]any{"stream": label, "ok": false, "error": err.Error()})
		return
	}

	type archiveJob struct {
		dvr CamDVR
		cam camera
	}
	jobs := make(chan archiveJob)
	var uploaded, failed int
	var mu sync.Mutex
	var wg sync.WaitGroup
	if workers < 1 {
		workers = 1
	}
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobs {
				ok := camArchiveFrame(cfg, j.dvr, j.cam, q, t)
				mu.Lock()
				if ok {
					uploaded++
				} else {
					failed++
				}
				mu.Unlock()
			}
		}()
	}

	var cameras int
	for _, dvr := range dvrs {
		if !dvr.Enabled {
			continue
		}
		cams, cerr := listCamerasByDVR(db, dvr.ID)
		if cerr != nil {
			camlog("warn", "archive_cycle", map[string]any{"stream": label, "dvr_id": dvr.ID, "ok": false, "error": cerr.Error()})
			continue
		}
		for _, cam := range cams {
			if !cam.Enabled {
				continue
			}
			cameras++
			jobs <- archiveJob{dvr: dvr, cam: cam}
		}
	}
	close(jobs)
	wg.Wait()

	dur := time.Since(start)
	overran := dur > interval
	level := "info"
	if overran {
		level = "warn"
	}
	camlog(level, "archive_cycle", map[string]any{
		"stream": label, "ok": failed == 0, "cameras": cameras,
		"uploaded": uploaded, "failed": failed,
		"duration_ms": dur.Milliseconds(), "interval_ms": interval.Milliseconds(), "overran": overran,
	})
}

// camArchiveFrameTimeout bounds a single capture+upload pipeline so a stuck
// DVR/S3 call can never hold a worker-pool slot forever.
const camArchiveFrameTimeout = 20 * time.Second

// camArchiveFrame captures ONE frame from (dvr,cam) at quality q and uploads
// it to S3 under camS3FrameKey(cam.ID, q, t), reporting ok so the caller's
// tally stays accurate. The local temp file always lives in its own
// directory that is removed afterward, success or failure — mirrors
// camExecuteWatch's per-run os.MkdirTemp("camwatch-")/os.RemoveAll pattern
// (camera_escalate.go), just scoped to a single capture here since many of
// these run concurrently across cameras/qualities within one cycle.
func camArchiveFrame(cfg config, dvr CamDVR, cam camera, q StreamQuality, t time.Time) bool {
	ctx, cancel := context.WithTimeout(context.Background(), camArchiveFrameTimeout)
	defer cancel()

	scratch, err := os.MkdirTemp("", "camarchive-")
	if err != nil {
		camlog("error", "archive_frame", map[string]any{
			"camera_id": cam.ID, "dvr_id": dvr.ID, "quality": q.String(), "ok": false, "error": err.Error(),
		})
		return false
	}
	defer os.RemoveAll(scratch)

	dest := filepath.Join(scratch, fmt.Sprintf("%s_%s.jpg", cam.ID, q.String()))
	start := time.Now()

	var res captureResult
	var cerr error
	if q == StreamMain {
		// Full-resolution main frames MUST come straight off the RTSP main
		// stream, never the generic HTTP-first snapshot dispatch — see the
		// file-level doc comment for why.
		res, cerr = camArchiveCaptureFullRes(ctx, cfg, dvr, cam.Channel, dest)
	} else {
		res, cerr = captureSnapshot(ctx, cfg, dvr, cam.Channel, StreamSub, dest)
	}
	if cerr != nil {
		// Both capture paths already camlog their own masked command/URL + full
		// stderr on failure; this line just adds the archive-specific outcome.
		camlog("warn", "archive_frame", map[string]any{
			"camera_id": cam.ID, "dvr_id": dvr.ID, "quality": q.String(), "ok": false, "error": cerr.Error(),
			"latency_ms": time.Since(start).Milliseconds(),
		})
		return false
	}

	body, rerr := os.ReadFile(res.Path)
	if rerr != nil {
		camlog("error", "archive_frame", map[string]any{
			"camera_id": cam.ID, "dvr_id": dvr.ID, "quality": q.String(), "ok": false, "error": rerr.Error(),
		})
		return false
	}

	key := camS3FrameKey(cam.ID, q.String(), t)
	if perr := s3PutObject(ctx, cfg, key, body, "image/jpeg"); perr != nil {
		camlog("error", "archive_frame", map[string]any{
			"camera_id": cam.ID, "dvr_id": dvr.ID, "quality": q.String(), "ok": false, "error": perr.Error(),
			"key": key, "bytes": len(body), "latency_ms": time.Since(start).Milliseconds(),
		})
		return false
	}

	camlog("debug", "archive_frame", map[string]any{
		"camera_id": cam.ID, "dvr_id": dvr.ID, "quality": q.String(), "ok": true,
		"key": key, "bytes": len(body), "latency_ms": time.Since(start).Milliseconds(),
	})
	return true
}

// camArchiveCaptureFullRes grabs ONE full-resolution frame straight off the
// RTSP main stream via ffmpeg, deliberately bypassing captureSnapshot's
// HTTP-first branch: Hikvision's ISAPI ".../picture" endpoint (and the
// equivalent on other brands) returns a small thumbnail regardless of which
// channel/stream number is requested, so it can never stand in for a true
// archived main frame. Mirrors captureSnapshot's own acquire/release +
// preflight + failure-logging shape (camera_capture.go) so this path gets the
// same capture-semaphore bounding and observability as every other capture.
func camArchiveCaptureFullRes(ctx context.Context, cfg config, dvr CamDVR, ch int, destPath string) (captureResult, error) {
	cameraFFmpegPreflight(cfg)
	if err := camCaptureAcquire(ctx); err != nil {
		return captureResult{}, fmt.Errorf("capture queue wait cancelled: %w", err)
	}
	defer camCaptureRelease()

	brand := brandFor(dvr.Brand)
	if brand == nil {
		err := fmt.Errorf("no brand adapter available for %q", dvr.Brand)
		camlog("error", "archive_capture_main", map[string]any{"dvr_id": dvr.ID, "channel": ch, "ok": false, "error": err.Error()})
		return captureResult{}, err
	}
	liveURL := brand.LiveURL(dvr, ch, StreamMain)
	start := time.Now()
	res, diag, err := captureSnapshotFFmpeg(ctx, cfg, liveURL, destPath)
	if err != nil {
		fields := map[string]any{
			"dvr_id": dvr.ID, "channel": ch, "quality": "main", "method": "ffmpeg",
			"ok": false, "error": err.Error(), "latency_ms": time.Since(start).Milliseconds(),
			"url": maskCredsURL(liveURL), "cmd": diag.Cmd, "exit_code": diag.ExitCode, "timed_out": diag.TimedOut,
		}
		if diag.Stderr != "" {
			fields["stderr"] = diag.Stderr // FULL, never truncated — hard observability requirement
		}
		camlog("error", "archive_capture_main", fields)
		return captureResult{}, err
	}
	camlog("info", "archive_capture_main", map[string]any{
		"dvr_id": dvr.ID, "channel": ch, "quality": "main", "method": "ffmpeg",
		"ok": true, "bytes": res.Bytes, "latency_ms": time.Since(start).Milliseconds(),
		"url": maskCredsURL(liveURL), "cmd": diag.Cmd, "exit_code": diag.ExitCode,
	})
	return res, nil
}
