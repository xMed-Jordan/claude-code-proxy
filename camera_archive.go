package main

// camera_archive.go — background archiver (WP2): continuously captures every
// enabled camera's sub stream (every tick) and main stream (on a slower
// cadence) and writes one JPEG per interval to S3 object storage
// (camera_s3.go owns the client + camS3FrameKey scheme).
//
// Guarded by cfg.CameraArchiveEnabled AND camS3Enabled(cfg) — with either off
// this file is entirely inert, so an operator can flip the feature flag on
// without yet having entered S3 credentials (or vice versa) without anything
// actually running.
//
// Design: ONE forever ticker at cfg.CameraArchiveInterval (default 15s, the
// sub-stream cadence). Every tick archives the sub frame for every enabled
// camera on every enabled DVR; a separate due-time tracked across cycles
// (cfg.CameraArchiveMainInterval, default 30s) additionally archives the main
// frame on the ticks where it's due. A single cycle is handled synchronously
// by the loop goroutine and only returns once every dispatched capture has
// completed, so Go's time.Ticker — which drops ticks a slow receiver hasn't
// read yet rather than queuing them — naturally gives us "skip the next tick
// if the previous one is still running" for free, with no extra bookkeeping.
//
// Concurrency is bounded by a small worker pool (cfg.CameraArchiveConcurrency,
// default 4 goroutines) reading off a jobs channel: the dispatch loop blocks
// on a full pool rather than spawning unbounded goroutines, so a slow DVR or
// S3 upload can never let the archiver starve the live proxy of ffmpeg/HTTP
// capture slots (still further bounded underneath by captureSem, shared with
// every other capture path).
//
// Every camera on every enabled DVR across every site is archived — no
// per-camera opt-in/out beyond the existing enabled flags (camera.Enabled /
// dvr.Enabled), the same ones the investigate tools and watch scheduler
// already respect. The camera/DVR list is loaded fresh every cycle (not
// cached) so a newly added camera is picked up on its very next tick.
//
// SUB frames reuse captureSnapshot (camera_capture.go) — HTTP snapshot first,
// ffmpeg single-frame fallback — same as every other capture path. MAIN
// frames deliberately bypass captureSnapshot and call captureSnapshotFFmpeg
// directly against the brand's RTSP main LiveURL: Hikvision's ISAPI
// ".../picture" endpoint (what captureSnapshot's HTTP-first branch would hit)
// returns a small thumbnail regardless of which channel/stream number is
// requested, so it can never stand in for a true full-resolution archive
// frame. Every frame is captured into its own temp directory that is always
// removed, win or lose — this file does not touch camera_captures/served
// media at all, archived frames are read back directly from S3
// (camera_investigate.go read-through, WP1), never through the
// capability-token media store.
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
	conc := cfg.CameraArchiveConcurrency
	if conc < 1 {
		conc = 4
	}
	// The archiver must NEVER occupy every shared capture slot: both archive
	// capture paths (captureSnapshot for sub, camArchiveCaptureFullRes for
	// main) acquire captureSem, which is sized from cfg.CameraCaptureConcurrency
	// and shared with every live/investigate/monitor capture. Cap the worker
	// pool at captureSem-1 so at least one slot always stays free for the live
	// proxy — a forever-running archiver can otherwise hold all slots for up to
	// camArchiveFrameTimeout each cycle and inject latency into live captures.
	captureSlots := cfg.CameraCaptureConcurrency
	if captureSlots < 1 {
		captureSlots = 1
	}
	maxArchive := captureSlots - 1
	if maxArchive < 1 {
		maxArchive = 1 // degenerate 1-slot pool: nothing to reserve, run single-file
	}
	if conc > maxArchive {
		conc = maxArchive
	}

	camlog("info", "archiver_start", map[string]any{
		"interval_ms": interval.Milliseconds(), "main_interval_ms": mainInterval.Milliseconds(),
		"concurrency": conc, "capture_slots": captureSlots,
		"bucket": cfg.CameraS3Bucket, "endpoint": cfg.CameraS3Endpoint,
	})

	go camArchiveLoop(cfg, interval, mainInterval, conc)
}

// camArchiveLoop runs forever, ticking at interval (the sub-stream cadence).
// A startup jitter (camStartupJitter, cameramonitor.go) staggers the very
// first tick so a fleet of proxy restarts doesn't all burst-capture every
// camera in the same instant. nextMain — the time the next main-stream sweep
// is due — is plain loop-local state: only this one goroutine ever reads or
// advances it, since camArchiveCycle runs to completion before the next
// iteration begins.
func camArchiveLoop(cfg config, interval, mainInterval time.Duration, conc int) {
	time.Sleep(camStartupJitter(interval))
	var nextMain time.Time // zero value: the very first cycle is always due for main too
	nextMain = camArchiveCycle(cfg, interval, mainInterval, conc, nextMain)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for range ticker.C {
		nextMain = camArchiveCycle(cfg, interval, mainInterval, conc, nextMain)
	}
}

// camArchiveCycle loads every enabled camera on every enabled DVR (across all
// sites — listCameraDVRs(db, cfg, "") with an empty siteID lists every site's
// DVRs, mirroring handleCameraHealth's use of listCameraDVRViews(db, "")) and
// archives a sub frame for each, plus a main frame too when doMain (main is
// due this cycle). Dispatch runs through a fixed-size worker pool so overall
// capture+upload concurrency this cycle never exceeds conc, and the function
// does not return until every dispatched job has finished — which is what
// lets the ticker's own drop-on-backpressure behavior stand in for "skip the
// next tick if this one is still running". Respects the same global pause
// switch as the watch scheduler (camMonitorTick): archiving is background
// traffic too, and stays off while the operator has paused the proxy.
func camArchiveCycle(cfg config, interval, mainInterval time.Duration, conc int, nextMain time.Time) time.Time {
	if !proxyEnabled.Load() {
		return nextMain
	}
	start := time.Now()
	doMain := !start.Before(nextMain)
	if doMain {
		nextMain = start.Add(mainInterval)
	}
	// Snap the write instant to the SAME grid the read-through looks keys up on
	// (camS3SnapArchiveTime), so writer and reader agree on the object key —
	// otherwise the archiver keys frames at an arbitrary wall-clock second and
	// the reader (which snaps to Round(interval)) never finds them. Sub frames
	// use the sub cadence grid; main frames use their own (slower) grid.
	subT := camS3SnapArchiveTime(start, interval)
	mainT := camS3SnapArchiveTime(start, mainInterval)

	db, err := openProxyDB(cfg)
	if err != nil {
		camlog("error", "archive_cycle", map[string]any{"ok": false, "error": err.Error()})
		return nextMain
	}
	defer db.Close()

	dvrs, err := listCameraDVRs(db, cfg, "")
	if err != nil {
		camlog("error", "archive_cycle", map[string]any{"ok": false, "error": err.Error()})
		return nextMain
	}

	type archiveJob struct {
		dvr CamDVR
		cam camera
		q   StreamQuality
		t   time.Time // grid-snapped write instant (matches the read-through key)
	}
	jobs := make(chan archiveJob)
	workers := conc
	if workers < 1 {
		workers = 1
	}
	uploaded := map[string]int{"main": 0, "sub": 0}
	failed := map[string]int{"main": 0, "sub": 0}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobs {
				ok := camArchiveFrame(cfg, j.dvr, j.cam, j.q, j.t)
				mu.Lock()
				if ok {
					uploaded[j.q.String()]++
				} else {
					failed[j.q.String()]++
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
			camlog("warn", "archive_cycle", map[string]any{"dvr_id": dvr.ID, "ok": false, "error": cerr.Error()})
			continue
		}
		for _, cam := range cams {
			if !cam.Enabled {
				continue
			}
			cameras++
			jobs <- archiveJob{dvr: dvr, cam: cam, q: StreamSub, t: subT}
			if doMain {
				jobs <- archiveJob{dvr: dvr, cam: cam, q: StreamMain, t: mainT}
			}
		}
	}
	close(jobs)
	wg.Wait()

	dur := time.Since(start)
	uploadedTotal := uploaded["main"] + uploaded["sub"]
	failedTotal := failed["main"] + failed["sub"]
	overran := dur > interval
	level := "info"
	if overran {
		level = "warn"
	}
	camlog(level, "archive_cycle", map[string]any{
		"ok": failedTotal == 0, "cameras": cameras, "main_due": doMain,
		"uploaded_main": uploaded["main"], "uploaded_sub": uploaded["sub"], "uploaded_total": uploadedTotal,
		"failed_main": failed["main"], "failed_sub": failed["sub"], "failed_total": failedTotal,
		"duration_ms": dur.Milliseconds(), "interval_ms": interval.Milliseconds(), "overran": overran,
	})
	return nextMain
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
