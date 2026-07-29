package main

// camera_stream_archive.go — HIGH-FREQUENCY archiver (~1 fps) built on PERSISTENT
// RTSP connections instead of snapshot polling.
//
// Why this exists: the DVR's HTTP snapshot endpoint tops out at ~2.7 JPEGs/sec
// TOTAL across all channels (measured), so snapshot polling can't do 1 fps for a
// fleet of cameras. Instead, this holds one long-lived ffmpeg per (camera, stream)
// that stays connected to the RTSP stream (which already carries 12–25 fps) and
// samples frames out of it:
//   - SUB  → full decode at cfg.CameraStreamFPS (light; 704x576).
//   - MAIN → by default "keyframe" mode (-skip_frame nokey): only I-frames are
//     decoded, so a full-res main frame costs almost nothing — the output rate is
//     the stream's keyframe (GOP) rate (~1 fps when the DVR GOP is ~1s). Set
//     PROXY_CAM_STREAM_MAIN_MODE=full for a guaranteed fps via full decode (heavy).
//
// ffmpeg emits frames as a multipart-MJPEG stream on stdout (-f mpjpeg), which we
// parse by Content-Length (exact, unambiguous frame boundaries) and upload each
// frame to S3 under the same camS3FrameKey scheme (snapped to the 1-second grid,
// one frame per second per camera per quality). A supervisor reconciles the set of
// workers with the enabled-camera list every cfg.CameraStreamReconcile and restarts
// any worker whose ffmpeg dies (network blip / DVR reset) with a short backoff.
//
// Resource note: this runs N = (#enabled cameras) × 2 persistent ffmpeg processes.
// cfg.CameraStreamMaxWorkers caps that so a large fleet can't exhaust the box — a
// heavy fleet wants a dedicated archiver host. Frames are never deleted here.

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// startCameraStreamArchiver launches the persistent-RTSP archiver supervisor.
// Gated by the same CameraArchiveEnabled + camS3Enabled checks as the snapshot
// archiver (checked by the caller in startCameraArchiver).
func startCameraStreamArchiver(cfg config) {
	fps := cfg.CameraStreamFPS
	if fps < 1 {
		fps = 1
	}
	maxWorkers := cfg.CameraStreamMaxWorkers
	if maxWorkers < 1 {
		maxWorkers = 64
	}
	mode := strings.TrimSpace(cfg.CameraStreamMainMode)
	if mode == "" {
		mode = "keyframe"
	}
	cameraFFmpegPreflight(cfg)
	camlog("info", "stream_archiver_start", map[string]any{
		"fps": fps, "max_workers": maxWorkers, "main_mode": mode,
		"bucket": cfg.CameraS3Bucket, "endpoint": cfg.CameraS3Endpoint,
	})
	go camStreamSupervisor(cfg, fps, maxWorkers)
}

// camStreamKey identifies one persistent stream worker.
type camStreamKey struct {
	camID string
	q     StreamQuality
}

// camStreamSupervisor owns the worker set (single goroutine → no locking on the
// map) and reconciles it with the enabled-camera list on a ticker.
func camStreamSupervisor(cfg config, fps, maxWorkers int) {
	interval := cfg.CameraStreamReconcile
	if interval <= 0 {
		interval = 30 * time.Second
	}
	workers := map[camStreamKey]context.CancelFunc{}
	camStreamReconcile(cfg, fps, maxWorkers, workers)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for range ticker.C {
		camStreamReconcile(cfg, fps, maxWorkers, workers)
	}
}

// camStreamReconcile starts a worker for every enabled (camera, quality) that
// isn't running, and stops any running worker no longer wanted. While the proxy is
// paused it tears every worker down (background traffic stops, like the scheduler).
// camStreamWantSub / camStreamWantMain read PROXY_CAM_STREAM_QUALITIES
// ("both" | "sub" | "main", default both). Archiving both qualities for every
// camera means TWO persistent ffmpeg processes per camera, which on a 32-camera
// fleet is 64 of them and by far the largest CPU consumer on the host. Dropping
// to "sub" halves that at the cost of full-resolution archived frames (the
// observer samples sub anyway; main only matters for face work).
func camStreamWantSub(cfg config) bool {
	return strings.ToLower(strings.TrimSpace(cfg.CameraStreamQualities)) != "main"
}

func camStreamWantMain(cfg config) bool {
	return strings.ToLower(strings.TrimSpace(cfg.CameraStreamQualities)) != "sub"
}

func camStreamReconcile(cfg config, fps, maxWorkers int, workers map[camStreamKey]context.CancelFunc) {
	if !proxyEnabled.Load() {
		if len(workers) > 0 {
			for k, cancel := range workers {
				cancel()
				delete(workers, k)
			}
			camlog("info", "stream_reconcile", map[string]any{"paused": true, "stopped_all": true})
		}
		return
	}

	db, err := openProxyDB(cfg)
	if err != nil {
		camlog("error", "stream_reconcile", map[string]any{"ok": false, "error": err.Error()})
		return
	}
	defer db.Close()

	dvrs, err := listCameraDVRs(db, cfg, "")
	if err != nil {
		camlog("error", "stream_reconcile", map[string]any{"ok": false, "error": err.Error()})
		return
	}

	type target struct {
		dvr CamDVR
		cam camera
	}
	desired := map[camStreamKey]target{}
	for _, dvr := range dvrs {
		if !dvr.Enabled {
			continue
		}
		cams, cerr := listCamerasByDVR(db, dvr.ID)
		if cerr != nil {
			camlog("warn", "stream_reconcile", map[string]any{"dvr_id": dvr.ID, "ok": false, "error": cerr.Error()})
			continue
		}
		for _, cam := range cams {
			if !cam.Enabled {
				continue
			}
			if camStreamWantSub(cfg) {
				desired[camStreamKey{cam.ID, StreamSub}] = target{dvr, cam}
			}
			if camStreamWantMain(cfg) {
				desired[camStreamKey{cam.ID, StreamMain}] = target{dvr, cam}
			}
		}
	}

	// Stop workers whose camera was disabled/removed.
	for k, cancel := range workers {
		if _, ok := desired[k]; !ok {
			cancel()
			delete(workers, k)
			camlog("info", "stream_worker_stop", map[string]any{"camera_id": k.camID, "quality": k.q.String()})
		}
	}

	// Start missing workers, bounded by maxWorkers. Iterate in a DETERMINISTIC
	// order — sub streams first, then main, each by camera id — because Go map
	// order is random and the cap would otherwise land on an arbitrary half of the
	// fleet, giving some cameras only a main stream and others only a sub, with
	// the split reshuffling on every reconcile. Sub goes first because it is the
	// cheap stream the observer actually samples; main is the one worth dropping
	// when the box cannot afford everything.
	order := make([]camStreamKey, 0, len(desired))
	for k := range desired {
		order = append(order, k)
	}
	sort.Slice(order, func(i, j int) bool {
		if (order[i].q == StreamSub) != (order[j].q == StreamSub) {
			return order[i].q == StreamSub
		}
		return order[i].camID < order[j].camID
	})

	started := 0
	capped := false
	for _, k := range order {
		tg := desired[k]
		if _, ok := workers[k]; ok {
			continue
		}
		if len(workers) >= maxWorkers {
			capped = true
			break
		}
		ctx, cancel := context.WithCancel(context.Background())
		workers[k] = cancel
		go camStreamWorker(ctx, cfg, tg.dvr, tg.cam, k.q, fps)
		started++
	}

	fields := map[string]any{"running": len(workers), "desired": len(desired), "started": started}
	if capped {
		fields["worker_cap_reached"] = true
		fields["max_workers"] = maxWorkers
	}
	camlog("info", "stream_reconcile", fields)
}

// camStreamWorker keeps one stream archived forever: it runs ffmpeg, and when that
// exits (stream dropped, DVR reset) it restarts after a short backoff — until the
// supervisor cancels ctx (camera disabled / proxy paused / shutdown).
func camStreamWorker(ctx context.Context, cfg config, dvr CamDVR, cam camera, q StreamQuality, fps int) {
	const backoff = 3 * time.Second
	for ctx.Err() == nil {
		err := camStreamRun(ctx, cfg, dvr, cam, q, fps)
		if ctx.Err() != nil {
			return
		}
		camlog("warn", "stream_worker", map[string]any{
			"camera_id": cam.ID, "dvr_id": dvr.ID, "quality": q.String(),
			"restart_in_ms": backoff.Milliseconds(), "error": errString(err),
		})
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
	}
}

// camStreamRun runs one ffmpeg pulling the RTSP stream and pumps its multipart-MJPEG
// stdout into S3, one frame per second. Returns when ffmpeg exits or ctx is cancelled.
func camStreamRun(ctx context.Context, cfg config, dvr CamDVR, cam camera, q StreamQuality, fps int) error {
	brand := brandFor(dvr.Brand)
	if brand == nil {
		return fmt.Errorf("no brand adapter available for %q", dvr.Brand)
	}
	liveURL := brand.LiveURL(dvr, cam.Channel, q)
	args := camStreamFFmpegArgs(cfg, liveURL, q, fps)
	bin := cameraFFmpegBin(cfg)

	cmd := exec.CommandContext(ctx, bin, args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("stdout pipe: %w", err)
	}
	errBuf := &camCapWriter{max: 4096}
	cmd.Stderr = errBuf
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("ffmpeg start: %w", err)
	}
	// These workers are persistent — one per camera per quality — so they are the
	// single biggest CPU consumer on the box. Renice them below the interactive AI
	// path immediately after start.
	camDeprioritizeProcess(cmd, cfg.CameraProcessNice)
	camlog("info", "stream_run_start", map[string]any{
		"camera_id": cam.ID, "dvr_id": dvr.ID, "quality": q.String(), "cmd": maskedCamCmd(bin, args, liveURL),
	})

	frames := make(chan []byte, 8)
	var uploaded int
	var uwg sync.WaitGroup
	uwg.Add(1)
	go func() {
		defer uwg.Done()
		uploaded = camStreamUpload(ctx, cfg, cam, q, frames)
	}()

	parseErr := camStreamParseMPJPEG(ctx, stdout, frames)
	close(frames)
	uwg.Wait()
	waitErr := cmd.Wait()

	camlog("info", "stream_run_end", map[string]any{
		"camera_id": cam.ID, "dvr_id": dvr.ID, "quality": q.String(), "frames_uploaded": uploaded,
		"stderr": strings.TrimSpace(errBuf.String()),
	})
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if waitErr != nil {
		return fmt.Errorf("ffmpeg exited: %v: %s", waitErr, truncateString(strings.TrimSpace(errBuf.String()), 300))
	}
	return parseErr
}

// camStreamFFmpegArgs builds the ffmpeg command for a persistent stream grab. Main
// defaults to keyframe-only decode (-skip_frame nokey) so full-res frames are cheap;
// sub (and main in "full" mode) resample to fps via the fps filter.
func camStreamFFmpegArgs(cfg config, liveURL string, q StreamQuality, fps int) []string {
	if fps < 1 {
		fps = 1
	}
	stall := stallGuardMicros(15 * time.Second)
	keyframe := q == StreamMain && !strings.EqualFold(strings.TrimSpace(cfg.CameraStreamMainMode), "full")

	args := []string{"-nostdin", "-loglevel", "error"}
	if keyframe {
		// Input-side: decode ONLY keyframes — near-zero CPU for full-res frames.
		args = append(args, "-skip_frame", "nokey")
	}
	args = append(args,
		"-rtsp_transport", rtspTransport(cfg),
		"-timeout", strconv.FormatInt(stall, 10),
		"-i", liveURL,
		"-an",
	)
	if keyframe {
		args = append(args, "-fps_mode", "passthrough") // emit every decoded (key)frame
	} else {
		args = append(args, "-vf", fmt.Sprintf("fps=%d", fps))
	}
	args = append(args, "-q:v", "4", "-f", "mpjpeg", "-")
	return args
}

// camStreamParseMPJPEG reads ffmpeg's -f mpjpeg output (multipart/x-mixed-replace)
// and sends each complete JPEG frame on frames. It relies on the muxer's
// Content-Length header for exact, unambiguous frame boundaries (no JPEG marker
// scanning). A full frames channel drops the frame rather than stalling the reader
// (which would back-pressure ffmpeg and add latency).
func camStreamParseMPJPEG(ctx context.Context, r io.Reader, frames chan<- []byte) error {
	br := bufio.NewReaderSize(r, 128*1024)
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		clen := -1
		for { // read the boundary + header block, ending at the blank line after Content-Length
			line, err := br.ReadString('\n')
			if err != nil {
				return err
			}
			t := strings.TrimSpace(line)
			if t == "" {
				if clen >= 0 {
					break
				}
				continue // stray trailing CRLF before the next part
			}
			if strings.HasPrefix(strings.ToLower(t), "content-length:") {
				clen = atoiDefault(strings.TrimSpace(t[len("content-length:"):]), -1)
			}
		}
		if clen <= 0 || clen > 32*1024*1024 {
			continue // implausible; skip
		}
		frame := make([]byte, clen)
		if _, err := io.ReadFull(br, frame); err != nil {
			return err
		}
		select {
		case frames <- frame:
		default:
			// queue full — drop to keep reading (never block ffmpeg)
		}
	}
}

// camStreamUpload consumes frames and writes at most one per second per (camera,
// quality) to S3 under the 1-second-grid key, matching what the investigate
// read-through looks up. Returns the number of frames uploaded.
func camStreamUpload(ctx context.Context, cfg config, cam camera, q StreamQuality, frames <-chan []byte) int {
	var lastSlot time.Time
	n := 0
	for body := range frames {
		if ctx.Err() != nil {
			continue // drain without uploading during shutdown
		}
		if len(body) < 512 {
			continue // guard against a truncated/garbage frame
		}
		slot := time.Now().Truncate(time.Second)
		if slot.Equal(lastSlot) {
			continue // already have this second
		}
		lastSlot = slot
		key := camS3FrameKey(cam.ID, q.String(), slot)
		uctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		err := s3PutObject(uctx, cfg, key, body, "image/jpeg")
		cancel()
		if err != nil {
			camlog("warn", "stream_upload", map[string]any{
				"camera_id": cam.ID, "quality": q.String(), "ok": false, "error": err.Error(), "key": key,
			})
			continue
		}
		n++
		camlog("debug", "stream_upload", map[string]any{
			"camera_id": cam.ID, "quality": q.String(), "ok": true, "key": key, "bytes": len(body),
		})
	}
	return n
}

// camCapWriter is a thread-safe writer that retains only the last `max` bytes —
// used to keep a bounded tail of a long-lived process's stderr for diagnostics.
type camCapWriter struct {
	mu  sync.Mutex
	buf []byte
	max int
}

func (w *camCapWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf = append(w.buf, p...)
	if w.max > 0 && len(w.buf) > w.max {
		w.buf = w.buf[len(w.buf)-w.max:]
	}
	return len(p), nil
}

func (w *camCapWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return string(w.buf)
}

// atoiDefault parses n or returns def.
func atoiDefault(s string, def int) int {
	if v, err := strconv.Atoi(strings.TrimSpace(s)); err == nil {
		return v
	}
	return def
}

// errString is "" for a nil error, else err.Error().
func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
