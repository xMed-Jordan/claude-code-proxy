package main

// camera_archive.go — background archiver: continuously captures every enabled
// camera's sub AND main stream and writes one JPEG per interval to S3 object
// storage (camera_s3.go owns the client + camS3FrameKey scheme).
//
// Guarded by cfg.CameraArchiveEnabled AND camS3Enabled(cfg) — with either off
// this file is entirely inert.
//
// Design: sub and main are archived by TWO INDEPENDENT loops, each with its own
// time.Ticker and its own worker pool, so neither cadence can delay the other.
// The sub loop ticks at cfg.CameraArchiveInterval, the main loop at
// cfg.CameraArchiveMainInterval. Each cycle runs to completion before the next
// begins, so Go's time.Ticker — which drops (not queues) ticks a slow receiver
// hasn't read — gives us "skip the next tick if the previous one is still
// running" for free.
//
// BOTH streams are captured via a CHEAP HTTP snapshot (camArchiveCapture →
// captureSnapshotHTTP), not ffmpeg: the DVR serves a full-resolution JPEG for the
// main stream directly over HTTP when asked (Hikvision honors
// ?videoResolutionWidth/Height on /ISAPI/Streaming/channels/<id>/picture), so a
// full-res main archive frame costs one HTTP GET instead of a full HEVC decode.
// That means the archiver is bounded only by network/HTTP, NOT by the
// ffmpeg-oriented captureSem, so it scales to many DVRs: each stream gets its own
// pool of cfg.CameraArchiveConcurrency HTTP workers. Only the rare fallback for a
// brand/camera without an HTTP snapshot drops to ffmpeg (camArchiveCaptureFFmpeg),
// which still takes a captureSem slot and is thereby naturally bounded.
//
// Every capture+upload is retried up to cfg.CameraArchiveRetries times with a
// small backoff, so a transient DVR/S3 timeout doesn't leave a hole in the
// archive. Even a frame that still can't be written is never lost footage — it
// stays on the DVR and the investigate read-through back-fills it from the DVR on
// demand.
//
// Every camera on every enabled DVR across every site is archived (respecting the
// existing camera.Enabled / dvr.Enabled flags); the list is reloaded fresh every
// cycle so newly added cameras are picked up on the next tick. Each frame is
// captured into its own temp dir that is always removed; archived frames never
// touch camera_captures/served media — they are read back directly from S3.
//
// Frames are never deleted here — retention is forever / handled by an S3
// lifecycle rule out of band.
import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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

	// Both streams are cheap HTTP snapshots, NOT ffmpeg, so the archiver isn't
	// bounded by the ffmpeg-oriented captureSem. Concurrency is bounded PER DVR
	// (cfg.CameraArchiveConcurrency) and DVRs are archived in PARALLEL, so a single
	// DVR is never hammered with too many simultaneous full-res snapshots (which
	// slows it down and triggers retries) while total throughput still scales with
	// the number of DVRs. (Only a brand without an HTTP snapshot drops to the
	// ffmpeg fallback, which still takes a captureSem slot and is thereby bounded.)
	perDVR := cfg.CameraArchiveConcurrency
	if perDVR < 1 {
		perDVR = 6
	}

	camlog("info", "archiver_start", map[string]any{
		"sub_interval_ms": interval.Milliseconds(), "main_interval_ms": mainInterval.Milliseconds(),
		"per_dvr_concurrency": perDVR, "retries": cfg.CameraArchiveRetries,
		"main_res": fmt.Sprintf("%dx%d", cfg.CameraArchiveMainWidth, cfg.CameraArchiveMainHeight),
		"bucket": cfg.CameraS3Bucket, "endpoint": cfg.CameraS3Endpoint,
	})

	go camArchiveStreamLoop(cfg, "sub", StreamSub, interval, perDVR)
	go camArchiveStreamLoop(cfg, "main", StreamMain, mainInterval, perDVR)
}

// camArchiveStreamLoop runs one stream's archiver forever: it ticks at `interval`
// and archives a frame of quality q for every enabled camera each tick. Sub and
// main run as SEPARATE loops (separate tickers + separate worker pools) so neither
// delays the other. A startup jitter staggers the first tick across a fleet
// restart. Because a cycle runs to completion before the next iteration, Go's
// time.Ticker (which drops, not queues, unread ticks) gives us "skip the next tick
// if this one is still running" for free.
func camArchiveStreamLoop(cfg config, label string, q StreamQuality, interval time.Duration, perDVR int) {
	if perDVR < 1 {
		perDVR = 1
	}
	time.Sleep(camStartupJitter(interval))
	camArchiveStreamCycle(cfg, label, q, interval, perDVR)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for range ticker.C {
		camArchiveStreamCycle(cfg, label, q, interval, perDVR)
	}
}

// camArchiveStreamCycle archives one frame of quality q for every enabled camera
// on every enabled DVR across all sites (listCameraDVRs with an empty siteID),
// through a fixed-size worker pool, returning only once every dispatched job has
// finished. The write instant is snapped to THIS stream's grid
// (camS3SnapArchiveTime) so the object key matches what the read-through looks up.
// Skipped entirely while the operator has paused the proxy.
func camArchiveStreamCycle(cfg config, label string, q StreamQuality, interval time.Duration, perDVR int) {
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

	var uploaded, failed, cameras int
	var mu sync.Mutex
	var wg sync.WaitGroup
	if perDVR < 1 {
		perDVR = 1
	}
	// Archive every DVR in PARALLEL; within a DVR, bound concurrent snapshots to
	// perDVR so one DVR is never overwhelmed by simultaneous full-res grabs (which
	// slows it down and triggers retries). Total concurrency therefore scales with
	// the number of DVRs, not the camera count on any single one.
	for _, dvr := range dvrs {
		if !dvr.Enabled {
			continue
		}
		cams, cerr := listCamerasByDVR(db, dvr.ID)
		if cerr != nil {
			camlog("warn", "archive_cycle", map[string]any{"stream": label, "dvr_id": dvr.ID, "ok": false, "error": cerr.Error()})
			continue
		}
		wg.Add(1)
		go func(dvr CamDVR, cams []camera) {
			defer wg.Done()
			sem := make(chan struct{}, perDVR)
			var dwg sync.WaitGroup
			for _, cam := range cams {
				if !cam.Enabled {
					continue
				}
				mu.Lock()
				cameras++
				mu.Unlock()
				sem <- struct{}{}
				dwg.Add(1)
				go func(cam camera) {
					defer dwg.Done()
					defer func() { <-sem }()
					ok := camArchiveFrame(cfg, dvr, cam, q, t)
					mu.Lock()
					if ok {
						uploaded++
					} else {
						failed++
					}
					mu.Unlock()
				}(cam)
			}
			dwg.Wait()
		}(dvr, cams)
	}
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

// camArchiveFrameTimeout bounds a single capture+upload attempt so a stuck DVR/S3
// call can never hold a worker-pool slot forever.
const camArchiveFrameTimeout = 20 * time.Second

// camArchiveFrame captures ONE frame from (dvr,cam) at quality q and uploads it
// to S3 under camS3FrameKey(cam.ID, q, t), retrying up to cfg.CameraArchiveRetries
// times (with a small linear backoff) on any transient capture/upload failure so a
// blip doesn't leave a hole in the archive. Returns whether it ultimately
// succeeded so the caller's tally stays accurate.
func camArchiveFrame(cfg config, dvr CamDVR, cam camera, q StreamQuality, t time.Time) bool {
	retries := cfg.CameraArchiveRetries
	if retries < 0 {
		retries = 0
	}
	var lastErr error
	for attempt := 0; attempt <= retries; attempt++ {
		if attempt > 0 {
			time.Sleep(time.Duration(attempt) * 300 * time.Millisecond)
		}
		ok, err := camArchiveFrameAttempt(cfg, dvr, cam, q, t)
		if ok {
			return true
		}
		lastErr = err
	}
	fields := map[string]any{
		"camera_id": cam.ID, "dvr_id": dvr.ID, "quality": q.String(), "ok": false, "attempts": retries + 1,
	}
	if lastErr != nil {
		fields["error"] = lastErr.Error()
	}
	camlog("warn", "archive_frame", fields)
	return false
}

// camArchiveFrameAttempt runs ONE capture+upload. The local temp file always lives
// in its own directory that is removed afterward, success or failure.
func camArchiveFrameAttempt(cfg config, dvr CamDVR, cam camera, q StreamQuality, t time.Time) (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), camArchiveFrameTimeout)
	defer cancel()

	scratch, err := os.MkdirTemp("", "camarchive-")
	if err != nil {
		return false, err
	}
	defer os.RemoveAll(scratch)

	dest := filepath.Join(scratch, fmt.Sprintf("%s_%s.jpg", cam.ID, q.String()))
	res, cerr := camArchiveCapture(ctx, cfg, dvr, cam.Channel, q, dest)
	if cerr != nil {
		return false, cerr
	}

	body, rerr := os.ReadFile(res.Path)
	if rerr != nil {
		return false, rerr
	}

	key := camS3FrameKey(cam.ID, q.String(), t)
	if perr := s3PutObject(ctx, cfg, key, body, "image/jpeg"); perr != nil {
		return false, perr
	}

	camlog("debug", "archive_frame", map[string]any{
		"camera_id": cam.ID, "dvr_id": dvr.ID, "quality": q.String(), "ok": true,
		"key": key, "bytes": len(body),
	})
	return true, nil
}

// camArchiveCapture grabs one frame at quality q, preferring the CHEAP HTTP
// snapshot (no captureSem, no ffmpeg): for a main frame the brand's snapshot URL
// is asked for full resolution (camArchiveMainSnapshotURL) since the raw endpoint
// returns a small thumbnail by default. Only if the brand exposes no HTTP snapshot,
// or the HTTP grab fails, does it fall back to a full ffmpeg RTSP decode
// (camArchiveCaptureFFmpeg), which is captureSem-bounded.
func camArchiveCapture(ctx context.Context, cfg config, dvr CamDVR, ch int, q StreamQuality, dest string) (captureResult, error) {
	brand := brandFor(dvr.Brand)
	if brand != nil {
		if rawURL, isHTTP, ok := brand.SnapshotURL(dvr, ch, q); ok && isHTTP {
			if q == StreamMain {
				rawURL = camArchiveMainSnapshotURL(cfg, rawURL)
			}
			if res, err := captureSnapshotHTTP(ctx, dvr, rawURL, dest); err == nil {
				return res, nil
			}
			// HTTP snapshot failed — fall through to the ffmpeg fallback below.
		}
	}
	return camArchiveCaptureFFmpeg(ctx, cfg, dvr, ch, q, dest)
}

// camArchiveMainSnapshotURL appends the requested capture resolution to a main
// snapshot URL so the DVR returns a full-res JPEG instead of its default small
// thumbnail (Hikvision honors videoResolutionWidth/Height on the .../picture
// endpoint; brands that ignore the params simply return their native snapshot).
// Zero width/height disables the override.
func camArchiveMainSnapshotURL(cfg config, rawURL string) string {
	w, h := cfg.CameraArchiveMainWidth, cfg.CameraArchiveMainHeight
	if w <= 0 || h <= 0 {
		return rawURL
	}
	sep := "?"
	if strings.Contains(rawURL, "?") {
		sep = "&"
	}
	return fmt.Sprintf("%s%svideoResolutionWidth=%d&videoResolutionHeight=%d", rawURL, sep, w, h)
}

// camArchiveCaptureFFmpeg is the fallback for a brand/camera with no HTTP snapshot:
// it grabs one frame straight off the RTSP stream (quality q) via ffmpeg, bounded
// by the shared captureSem like every other ffmpeg capture. Mirrors captureSnapshot's
// acquire/release + preflight + failure-logging shape (camera_capture.go).
func camArchiveCaptureFFmpeg(ctx context.Context, cfg config, dvr CamDVR, ch int, q StreamQuality, destPath string) (captureResult, error) {
	cameraFFmpegPreflight(cfg)
	if err := camCaptureAcquire(ctx); err != nil {
		return captureResult{}, fmt.Errorf("capture queue wait cancelled: %w", err)
	}
	defer camCaptureRelease()

	brand := brandFor(dvr.Brand)
	if brand == nil {
		err := fmt.Errorf("no brand adapter available for %q", dvr.Brand)
		camlog("error", "archive_capture_ffmpeg", map[string]any{"dvr_id": dvr.ID, "channel": ch, "quality": q.String(), "ok": false, "error": err.Error()})
		return captureResult{}, err
	}
	liveURL := brand.LiveURL(dvr, ch, q)
	start := time.Now()
	res, diag, err := captureSnapshotFFmpeg(ctx, cfg, liveURL, destPath)
	if err != nil {
		fields := map[string]any{
			"dvr_id": dvr.ID, "channel": ch, "quality": q.String(), "method": "ffmpeg",
			"ok": false, "error": err.Error(), "latency_ms": time.Since(start).Milliseconds(),
			"url": maskCredsURL(liveURL), "cmd": diag.Cmd, "exit_code": diag.ExitCode, "timed_out": diag.TimedOut,
		}
		if diag.Stderr != "" {
			fields["stderr"] = diag.Stderr // FULL, never truncated — hard observability requirement
		}
		camlog("error", "archive_capture_ffmpeg", fields)
		return captureResult{}, err
	}
	return res, nil
}
