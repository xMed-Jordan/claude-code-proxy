package main

// camera_capture.go — ffmpeg/ffprobe capture wrappers: snapshots (HTTP-first with
// an ffmpeg single-frame fallback), live clips, past-playback clips, clip frame
// sampling (for vision backends that can't ingest video directly), an ffmpeg/
// ffprobe preflight check, an ffprobe video-stream check, and black/no-signal
// snapshot classification.
//
// Every capture function is bounded by captureSem (camCaptureAcquire/Release,
// camera.go) — the same bounded-subprocess pattern as agy.go's agySem — and every
// attempt is camlog'd with the masked FULL external command, exit code, duration,
// byte sizes, and (on failure) the complete, untruncated ffmpeg stderr, per the
// observability requirement. Context timeout is always the authoritative kill:
// ffmpeg's own -timeout stall guard (the RTSP demuxer's socket-I/O timeout, in µs;
// NOT -rw_timeout, which the rtsp demuxer rejects as "Option not found") is passed
// as defense-in-depth, but the Go context deadline (runCamCommand, with a
// process-tree kill on cancel) is what actually terminates a stuck process
// regardless of ffmpeg version/flag support.
//
// This file does NOT touch the database — classification/capture results are
// returned to the caller (setup/describe phase, scheduler, HTTP handlers in later
// WPs) which owns persisting camera_captures rows and any disabled_reason /
// re-enable decision.
import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"io"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// errNoRecording is returned by capturePlaybackClip when the DVR has nothing in
// the requested time window — a clean, expected outcome (never surfaced as a 500).
var errNoRecording = errors.New("no recording found in the requested time window")

// captureResult is the outcome of a successful snapshot/clip capture: enough for
// the caller to insert a camera_captures row (token/site/camera ids are theirs).
type captureResult struct {
	Path        string
	ContentType string
	Bytes       int64
	Width       int // 0 for clips (video dimensions aren't probed here)
	Height      int
	Method      string // "http" | "ffmpeg"
}

// ─────────────────────────── subprocess plumbing ───────────────────────────

// camExecResult is the outcome of one ffmpeg/ffprobe subprocess invocation, with
// stdout/stderr captured separately (unlike the swallow-on-error commandOutput in
// main.go, which the plan reserves for cheap yes/no probes only).
type camExecResult struct {
	ExitCode   int
	Stdout     string
	Stderr     string
	DurationMs int64
	TimedOut   bool
}

// camFFmpegDiag carries subprocess diagnostics — the exact masked command line,
// exit code, timed-out flag, wall-clock duration, and the FULL (never truncated)
// stderr — out of a capture helper so the caller can fold "log the exact external
// command with credentials masked, exit code, duration, and full stderr on
// failure" (a hard observability requirement) into its own camlog line, which
// already carries dvr/camera/watch context this file doesn't have.
type camFFmpegDiag struct {
	Cmd        string
	ExitCode   int
	TimedOut   bool
	Stderr     string
	DurationMs int64
}

// runCamCommand runs name+args bounded by timeout. The context deadline is the
// authoritative kill, enforced via cmd.Cancel calling killCamProcessTree
// (camera_proc_windows.go / camera_proc_other.go) rather than the default
// Process.Kill: on at least one real ffmpeg install (Chocolatey's Windows shim)
// the exec'd process is a launcher whose actual worker process has a DIFFERENT
// pid, so a plain Process.Kill only kills the shim and leaks the real ffmpeg
// process forever — which also hangs Wait() waiting for stdout/stderr pipe EOF
// that the orphan never sends. cmd.WaitDelay is a second line of defense that
// force-closes the pipes if some process nonetheless outlives the tree-kill. A
// non-nil returned error means the process could not even be run/awaited; a bad
// exit or timeout is reported via the returned camExecResult so callers can
// log/classify it (e.g. looksLikeNoRecording) instead of it being swallowed.
func runCamCommand(ctx context.Context, timeout time.Duration, name string, args ...string) (camExecResult, error) {
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(cctx, name, args...)
	configureCamProcAttr(cmd)
	cmd.Cancel = func() error {
		killCamProcessTree(cmd)
		return nil
	}
	cmd.WaitDelay = 5 * time.Second

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	start := time.Now()
	runErr := cmd.Run()
	dur := time.Since(start)

	res := camExecResult{Stdout: stdout.String(), Stderr: stderr.String(), DurationMs: dur.Milliseconds()}
	if cctx.Err() == context.DeadlineExceeded {
		res.TimedOut = true
		res.ExitCode = -1
		return res, nil
	}
	if runErr != nil {
		var exitErr *exec.ExitError
		if errors.As(runErr, &exitErr) {
			res.ExitCode = exitErr.ExitCode()
			return res, nil
		}
		return res, fmt.Errorf("exec %s: %w", name, runErr)
	}
	return res, nil
}

// maskedCamCmd renders bin+args as a single space-joined, copy-pasteable string
// for logging. rawURL (if it appears verbatim as one of args, which is how every
// caller in this file passes it) is replaced by its maskCredsURL rendering first
// so plaintext DVR credentials never reach a log line or the camera_events table;
// any argument containing whitespace/quotes is quoted. Logging just a bare "url"
// field (as earlier iterations of this file did) is not enough to reproduce a
// failure by hand — the plan's observability requirement is the *exact* command.
func maskedCamCmd(bin string, args []string, rawURL string) string {
	var masked string
	if rawURL != "" {
		masked = maskCredsURL(rawURL)
	}
	parts := make([]string, 0, len(args)+1)
	parts = append(parts, bin)
	for _, a := range args {
		if rawURL != "" && a == rawURL {
			a = masked
		}
		if strings.ContainsAny(a, " \t\"") {
			a = strconv.Quote(a)
		}
		parts = append(parts, a)
	}
	return strings.Join(parts, " ")
}

// ─────────────────────────── ffmpeg/ffprobe preflight ───────────────────────────

var ffmpegPreflightOnce sync.Once

// ffmpegAvailable reports, once per process (subsequent calls are free — the
// result is cached), whether both ffmpeg and ffprobe are usable, plus a
// human-readable summary suitable for a startup log line or a UI badge. Use the
// uncached cameraBinaryStatus instead when a live check is required (the /health
// endpoint and the Diagnostics "re-run a single capture" flow, later WPs) — a
// binary could be installed/removed while the process keeps running, which this
// cached view would never notice.
var (
	ffmpegAvailableOnce sync.Once
	ffmpegAvailOK       bool
	ffmpegAvailMsg      string
	ffmpegAvailFFOK     bool
	ffmpegAvailFFVer    string
	ffmpegAvailFPOK     bool
	ffmpegAvailFPVer    string
)

func ffmpegAvailable(cfg config) (bool, string) {
	ffmpegAvailableOnce.Do(func() {
		ffmpegAvailFFOK, ffmpegAvailFFVer, ffmpegAvailFPOK, ffmpegAvailFPVer = cameraBinaryStatus(cfg)
		ffmpegAvailOK = ffmpegAvailFFOK && ffmpegAvailFPOK
		switch {
		case ffmpegAvailOK:
			ffmpegAvailMsg = fmt.Sprintf("ffmpeg (%s) and ffprobe (%s) available", ffmpegAvailFFVer, ffmpegAvailFPVer)
		case !ffmpegAvailFFOK && !ffmpegAvailFPOK:
			ffmpegAvailMsg = "ffmpeg and ffprobe not found on PATH (set PROXY_FFMPEG_BIN/PROXY_FFPROBE_BIN); HTTP snapshots still work, clips/probing/frame-sampling are disabled"
		case !ffmpegAvailFFOK:
			ffmpegAvailMsg = "ffmpeg not found on PATH (set PROXY_FFMPEG_BIN); snapshots fall back to HTTP-only, clips/frame-sampling are disabled"
		default:
			ffmpegAvailMsg = "ffprobe not found on PATH (set PROXY_FFPROBE_BIN); RTSP-probe discovery is disabled"
		}
	})
	return ffmpegAvailOK, ffmpegAvailMsg
}

// cameraFFmpegPreflight logs ffmpeg/ffprobe presence+version exactly once
// (lazily, on first capture use — this file cannot hook runServe/initCameras
// directly without touching camera.go, which is out of scope here). Absence never
// blocks startup or a request; captures on a missing ffmpeg simply fail their own
// way (HTTP snapshots keep working) and that failure is logged individually too.
func cameraFFmpegPreflight(cfg config) {
	ffmpegPreflightOnce.Do(func() {
		ok, msg := ffmpegAvailable(cfg)
		level := "info"
		if !ok {
			level = "warn"
		}
		camlog(level, "ffmpeg_preflight", map[string]any{
			"ffmpeg_ok": ffmpegAvailFFOK, "ffmpeg_version": ffmpegAvailFFVer,
			"ffprobe_ok": ffmpegAvailFPOK, "ffprobe_version": ffmpegAvailFPVer,
			"ok": ok, "detail": msg,
		})
	})
}

// cameraBinaryStatus reports live ffmpeg/ffprobe presence + version, uncached (a
// fresh subprocess check every call) — intended for /health and the Diagnostics
// panel (later WPs) to surface current reachability, unlike the once-only
// ffmpegAvailable/cameraFFmpegPreflight above.
func cameraBinaryStatus(cfg config) (ffmpegOK bool, ffmpegVersion string, ffprobeOK bool, ffprobeVersion string) {
	ffmpegOK, ffmpegVersion = camCheckBinary(cameraFFmpegBin(cfg), "-version")
	ffprobeOK, ffprobeVersion = camCheckBinary(cameraFFprobeBin(cfg), "-version")
	return
}

func camCheckBinary(bin, versionFlag string) (bool, string) {
	out := strings.TrimSpace(commandOutput(5*time.Second, bin, versionFlag))
	if out == "" {
		return false, ""
	}
	firstLine := out
	if i := strings.IndexByte(out, '\n'); i >= 0 {
		firstLine = strings.TrimSpace(out[:i])
	}
	return true, firstLine
}

// ─────────────────────────── snapshots ───────────────────────────

// captureSnapshot writes a still image for (dvr,ch,q) to a path derived from
// destPath (the extension is corrected to match the actual sniffed image type).
// When cfg.CameraSnapshotHTTPFirst and the brand exposes an HTTP snapshot
// endpoint, that is tried first (cheap, no transcode, no ffmpeg dependency); on
// any failure it falls back to grabbing one frame via ffmpeg from the live RTSP
// URL. Every attempt (both paths) is camlog'd with a masked URL/command.
func captureSnapshot(ctx context.Context, cfg config, dvr CamDVR, ch int, q StreamQuality, destPath string) (captureResult, error) {
	cameraFFmpegPreflight(cfg)
	if err := camCaptureAcquire(ctx); err != nil {
		return captureResult{}, fmt.Errorf("capture queue wait cancelled: %w", err)
	}
	defer camCaptureRelease()

	brand := brandFor(dvr.Brand)
	start := time.Now()

	if cfg.CameraSnapshotHTTPFirst && brand != nil {
		if rawURL, isHTTP, ok := brand.SnapshotURL(dvr, ch, q); ok && isHTTP {
			res, err := captureSnapshotHTTP(ctx, dvr, rawURL, destPath)
			if err == nil {
				camlog("info", "capture_snapshot", map[string]any{
					"dvr_id": dvr.ID, "channel": ch, "quality": q.String(), "method": "http",
					"ok": true, "bytes": res.Bytes, "latency_ms": time.Since(start).Milliseconds(),
					"url": maskCredsURL(rawURL),
				})
				return res, nil
			}
			camlog("warn", "capture_snapshot_http_fallback", map[string]any{
				"dvr_id": dvr.ID, "channel": ch, "quality": q.String(), "ok": false,
				"error": err.Error(), "url": maskCredsURL(rawURL),
			})
		}
	}

	if brand == nil {
		err := fmt.Errorf("no brand adapter available for %q", dvr.Brand)
		camlog("error", "capture_snapshot", map[string]any{"dvr_id": dvr.ID, "channel": ch, "ok": false, "error": err.Error()})
		return captureResult{}, err
	}
	liveURL := brand.LiveURL(dvr, ch, q)
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
		camlog("error", "capture_snapshot", fields)
		return captureResult{}, err
	}
	camlog("info", "capture_snapshot", map[string]any{
		"dvr_id": dvr.ID, "channel": ch, "quality": q.String(), "method": "ffmpeg",
		"ok": true, "bytes": res.Bytes, "latency_ms": time.Since(start).Milliseconds(),
		"url": maskCredsURL(liveURL), "cmd": diag.Cmd, "exit_code": diag.ExitCode,
	})
	return res, nil
}

// captureSnapshotHTTP performs the digest/basic-authenticated GET (credentials
// never appear in rawURL itself — see camera_digest.go) and verifies the body is
// really an image (sniffImage, which sniffs via http.DetectContentType) before
// accepting it (DVRs return HTML login/error pages on auth or path mistakes, and
// those must never be saved/served as a "snapshot").
func captureSnapshotHTTP(ctx context.Context, dvr CamDVR, rawURL, destPath string) (captureResult, error) {
	resp, err := httpDigestGet(ctx, camHTTPClient(), rawURL, dvr.Username, dvr.Password)
	if err != nil {
		return captureResult{}, fmt.Errorf("http snapshot request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return captureResult{}, fmt.Errorf("http snapshot HTTP %d: %s", resp.StatusCode, truncateString(string(body), 200))
	}

	tmp := destPath + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return captureResult{}, fmt.Errorf("create temp file: %w", err)
	}
	n, copyErr := io.Copy(f, io.LimitReader(resp.Body, 32<<20)) // 32MB sanity cap for a still image
	closeErr := f.Close()
	if copyErr != nil {
		os.Remove(tmp)
		return captureResult{}, fmt.Errorf("write snapshot: %w", copyErr)
	}
	if closeErr != nil {
		os.Remove(tmp)
		return captureResult{}, fmt.Errorf("close snapshot file: %w", closeErr)
	}

	ct, ext := sniffImage(tmp)
	if ct == "" {
		os.Remove(tmp)
		return captureResult{}, fmt.Errorf("http snapshot response was not a recognizable image (%d bytes)", n)
	}
	finalPath := strings.TrimSuffix(destPath, filepath.Ext(destPath)) + ext
	if err := os.Rename(tmp, finalPath); err != nil {
		os.Remove(tmp)
		return captureResult{}, fmt.Errorf("finalize snapshot: %w", err)
	}
	w, h := imageDimensions(finalPath)
	return captureResult{Path: finalPath, ContentType: ct, Bytes: n, Width: w, Height: h, Method: "http"}, nil
}

// captureSnapshotFFmpeg grabs a single frame from a live RTSP URL:
//
//	ffmpeg -nostdin -loglevel error -rtsp_transport <tcp|udp> -timeout <µs>
//	  -i <url> -frames:v 1 -q:v 3 -y <dest>.jpg
//
// The returned camFFmpegDiag always carries the masked command line (even on a
// pre-exec failure) so the caller can log it regardless of outcome.
func captureSnapshotFFmpeg(ctx context.Context, cfg config, liveURL, destPath string) (captureResult, camFFmpegDiag, error) {
	timeout := cfg.CameraCaptureTimeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	rwTimeoutUs := stallGuardMicros(timeout)
	transport := rtspTransport(cfg)

	finalPath := strings.TrimSuffix(destPath, filepath.Ext(destPath)) + ".jpg"
	args := []string{
		"-nostdin", "-loglevel", "error",
		"-rtsp_transport", transport,
		"-timeout", strconv.FormatInt(rwTimeoutUs, 10),
		"-i", liveURL,
		"-frames:v", "1", "-q:v", "3",
		"-y", finalPath,
	}
	bin := cameraFFmpegBin(cfg)
	diag := camFFmpegDiag{Cmd: maskedCamCmd(bin, args, liveURL)}

	res, err := runCamCommand(ctx, timeout, bin, args...)
	if err != nil {
		return captureResult{}, diag, fmt.Errorf("ffmpeg exec failed: %w", err)
	}
	diag.ExitCode, diag.TimedOut, diag.Stderr, diag.DurationMs = res.ExitCode, res.TimedOut, res.Stderr, res.DurationMs

	if res.TimedOut {
		return captureResult{}, diag, fmt.Errorf("ffmpeg snapshot timed out after %s: %s", timeout, truncateString(res.Stderr, 500))
	}
	if res.ExitCode != 0 {
		return captureResult{}, diag, fmt.Errorf("ffmpeg snapshot exited %d: %s", res.ExitCode, truncateString(res.Stderr, 500))
	}
	ct, _ := sniffImage(finalPath)
	if ct == "" {
		return captureResult{}, diag, fmt.Errorf("ffmpeg produced no recognizable image: %s", truncateString(res.Stderr, 500))
	}
	var size int64
	if fi, e := os.Stat(finalPath); e == nil {
		size = fi.Size()
	}
	w, h := imageDimensions(finalPath)
	return captureResult{Path: finalPath, ContentType: ct, Bytes: size, Width: w, Height: h, Method: "ffmpeg"}, diag, nil
}

// ─────────────────────────── clips (live + playback) ───────────────────────────

// captureLiveClip records `seconds` of live video (clamped to
// cfg.CameraClipMaxSeconds) starting now, to a ".mp4" derived from destPath.
func captureLiveClip(ctx context.Context, cfg config, dvr CamDVR, ch int, q StreamQuality, seconds int, destPath string) (captureResult, error) {
	cameraFFmpegPreflight(cfg)
	if err := camCaptureAcquire(ctx); err != nil {
		return captureResult{}, fmt.Errorf("capture queue wait cancelled: %w", err)
	}
	defer camCaptureRelease()

	brand := brandFor(dvr.Brand)
	if brand == nil {
		err := fmt.Errorf("no brand adapter available for %q", dvr.Brand)
		camlog("error", "capture_clip", map[string]any{"dvr_id": dvr.ID, "channel": ch, "kind": "live", "ok": false, "error": err.Error()})
		return captureResult{}, err
	}
	seconds = clampClipSeconds(cfg, seconds)
	liveURL := brand.LiveURL(dvr, ch, q)

	start := time.Now()
	res, diag, err := runClipFFmpeg(ctx, cfg, liveURL, seconds, destPath)
	logClipAttempt(dvr.ID, ch, q, "live", liveURL, seconds, start, res, diag, err)
	return res, err
}

// capturePlaybackClip pulls recorded footage for [from,to] to a ".mp4" derived
// from destPath. Returns errNoRecording (never a generic error) when the DVR has
// nothing in that window, so callers can show a clean message instead of a 500.
func capturePlaybackClip(ctx context.Context, cfg config, dvr CamDVR, ch int, q StreamQuality, from, to time.Time, destPath string) (captureResult, error) {
	cameraFFmpegPreflight(cfg)
	if err := camCaptureAcquire(ctx); err != nil {
		return captureResult{}, fmt.Errorf("capture queue wait cancelled: %w", err)
	}
	defer camCaptureRelease()

	brand := brandFor(dvr.Brand)
	if brand == nil {
		err := fmt.Errorf("no brand adapter available for %q", dvr.Brand)
		camlog("error", "capture_clip", map[string]any{"dvr_id": dvr.ID, "channel": ch, "kind": "playback", "ok": false, "error": err.Error()})
		return captureResult{}, err
	}
	rawURL, err := brand.PlaybackURL(dvr, ch, q, from, to)
	if err != nil {
		camlog("error", "capture_clip", map[string]any{
			"dvr_id": dvr.ID, "channel": ch, "kind": "playback", "ok": false, "error": err.Error(),
			"from": from.Format(time.RFC3339), "to": to.Format(time.RFC3339),
		})
		return captureResult{}, err
	}
	seconds := clampClipSeconds(cfg, int(to.Sub(from).Seconds()))

	start := time.Now()
	res, diag, cerr := runClipFFmpeg(ctx, cfg, rawURL, seconds, destPath)
	logClipAttempt(dvr.ID, ch, q, "playback", rawURL, seconds, start, res, diag, cerr)
	return res, cerr
}

// clampClipSeconds enforces a sane minimum and the configured maximum clip length.
func clampClipSeconds(cfg config, seconds int) int {
	if seconds <= 0 {
		seconds = 30
	}
	max := cfg.CameraClipMaxSeconds
	if max <= 0 {
		max = 300
	}
	if seconds > max {
		seconds = max
	}
	return seconds
}

// runClipFFmpeg streams `seconds` from rawURL into a ".mp4" derived from destPath
// via -c copy (no transcode) with a byte cap and a stall guard:
//
//	ffmpeg -nostdin -loglevel error -rtsp_transport <t> -timeout <µs> -i <url>
//	  -t <seconds> -c copy -movflags +faststart -fs <maxBytes> -y out.mp4
//
// The process deadline is seconds+cfg.CameraCaptureTimeout (a start/stop grace
// window), not cfg.CameraCaptureTimeout alone — otherwise a multi-minute clip
// request would always be killed by the (snapshot-oriented) default timeout. The
// returned camFFmpegDiag always carries the masked command line, even when the
// outcome classifies as errNoRecording.
func runClipFFmpeg(ctx context.Context, cfg config, rawURL string, seconds int, destPath string) (captureResult, camFFmpegDiag, error) {
	grace := cfg.CameraCaptureTimeout
	if grace <= 0 {
		grace = 30 * time.Second
	}
	timeout := time.Duration(seconds)*time.Second + grace
	rwTimeoutUs := stallGuardMicros(grace)
	transport := rtspTransport(cfg)

	finalPath := strings.TrimSuffix(destPath, filepath.Ext(destPath)) + ".mp4"
	maxBytes := cfg.CameraClipMaxBytes
	if maxBytes <= 0 {
		maxBytes = 200 << 20
	}

	args := []string{
		"-nostdin", "-loglevel", "error",
		"-rtsp_transport", transport,
		"-timeout", strconv.FormatInt(rwTimeoutUs, 10),
		"-i", rawURL,
		"-t", strconv.Itoa(seconds),
		"-c", "copy",
		"-movflags", "+faststart",
		"-fs", strconv.FormatInt(maxBytes, 10),
		"-y", finalPath,
	}
	bin := cameraFFmpegBin(cfg)
	diag := camFFmpegDiag{Cmd: maskedCamCmd(bin, args, rawURL)}

	res, err := runCamCommand(ctx, timeout, bin, args...)
	if err != nil {
		return captureResult{}, diag, fmt.Errorf("ffmpeg exec failed: %w", err)
	}
	diag.ExitCode, diag.TimedOut, diag.Stderr, diag.DurationMs = res.ExitCode, res.TimedOut, res.Stderr, res.DurationMs

	var size int64
	if fi, e := os.Stat(finalPath); e == nil {
		size = fi.Size()
	}

	if res.TimedOut {
		if size < camMinValidClipBytes {
			return captureResult{}, diag, errNoRecording
		}
		return captureResult{}, diag, fmt.Errorf("ffmpeg clip timed out after %s: %s", timeout, truncateString(res.Stderr, 500))
	}
	if looksLikeNoRecording(res.Stderr, size) {
		return captureResult{}, diag, errNoRecording
	}
	if res.ExitCode != 0 {
		return captureResult{}, diag, fmt.Errorf("ffmpeg clip exited %d: %s", res.ExitCode, truncateString(res.Stderr, 500))
	}
	return captureResult{Path: finalPath, ContentType: "video/mp4", Bytes: size, Method: "ffmpeg"}, diag, nil
}

// camMinValidClipBytes is the floor below which an ".mp4" output is treated as
// "no footage" rather than a real (if short) clip: a valid faststart MP4 header
// alone is a few hundred bytes, so anything under this is essentially empty.
const camMinValidClipBytes = 4096

// looksLikeNoRecording heuristically classifies an ffmpeg clip failure as "the
// DVR has no recording in the requested window" from its stderr and the resulting
// file size. DVR/ffmpeg error text is not standardized across vendors, so this is
// deliberately narrow (only phrases specific to an empty/absent stream) — genuine
// connectivity/auth errors must NOT be silently reclassified as "no recording".
func looksLikeNoRecording(stderr string, outBytes int64) bool {
	if outBytes < camMinValidClipBytes {
		return true
	}
	low := strings.ToLower(stderr)
	for _, needle := range []string{
		"404", "session not found", "no such file", "no data found",
		"invalid data found when processing input",
	} {
		if strings.Contains(low, needle) {
			return true
		}
	}
	return false
}

// logClipAttempt records one live/playback clip capture attempt: the masked
// command, exit code, duration, and — on any non-"no recording" failure — the
// FULL (never truncated) ffmpeg stderr, per the observability requirement. A
// classification of errNoRecording is logged at "info" (an expected, clean
// outcome) rather than "error" so the Diagnostics failures view isn't spammed by
// operators legitimately asking for footage that doesn't exist.
func logClipAttempt(dvrID string, ch int, q StreamQuality, kind, rawURL string, seconds int, start time.Time, res captureResult, diag camFFmpegDiag, err error) {
	fields := map[string]any{
		"dvr_id": dvrID, "channel": ch, "quality": q.String(), "kind": kind,
		"seconds": seconds, "latency_ms": time.Since(start).Milliseconds(), "url": maskCredsURL(rawURL),
		"cmd": diag.Cmd,
	}
	if err != nil {
		fields["ok"] = false
		fields["error"] = err.Error()
		level := "error"
		if errors.Is(err, errNoRecording) {
			level = "info"
			fields["no_recording"] = true
		} else {
			fields["exit_code"] = diag.ExitCode
			fields["timed_out"] = diag.TimedOut
			if diag.Stderr != "" {
				fields["stderr"] = diag.Stderr // FULL, never truncated
			}
		}
		camlog(level, "capture_clip", fields)
		return
	}
	fields["ok"] = true
	fields["bytes"] = res.Bytes
	fields["exit_code"] = diag.ExitCode
	camlog("info", "capture_clip", fields)
}

// ─────────────────────────── RTSP video-stream probe ───────────────────────────

// ffprobeHasVideo shells out to ffprobe to check whether rawURL yields a video
// stream, distinguishing "ffprobe ran cleanly and found no video stream" (false,
// nil) from "ffprobe could not even be run, was cancelled, or exited non-zero"
// (false, non-nil error) — the caller decides how to treat that difference (e.g.
// the RTSP channel-probe fallback's own commandOutput-based walk deliberately
// swallows exec errors as "no channel here"; a diagnostics re-run instead wants
// to surface a real connectivity/auth failure distinctly from "empty channel").
// Every attempt is camlog'd with the masked command, exit code, and — on any
// non-nil error — the full ffprobe stderr.
func ffprobeHasVideo(ctx context.Context, cfg config, rawURL string) (bool, error) {
	timeout := cfg.CameraDiscoverTimeout
	if timeout <= 0 || timeout > 10*time.Second {
		timeout = 10 * time.Second
	}
	transport := rtspTransport(cfg)
	args := []string{
		"-v", "error",
		"-rtsp_transport", transport,
		"-i", rawURL,
		"-select_streams", "v:0",
		"-show_entries", "stream=codec_type",
		"-of", "csv=p=0",
	}
	bin := cameraFFprobeBin(cfg)
	cmdLine := maskedCamCmd(bin, args, rawURL)

	res, err := runCamCommand(ctx, timeout, bin, args...)
	if err != nil {
		camlog("error", "ffprobe_has_video", map[string]any{
			"ok": false, "cmd": cmdLine, "url": maskCredsURL(rawURL), "error": err.Error(),
		})
		return false, fmt.Errorf("ffprobe exec failed: %w", err)
	}
	if res.TimedOut {
		camlog("warn", "ffprobe_has_video", map[string]any{
			"ok": false, "cmd": cmdLine, "url": maskCredsURL(rawURL), "timed_out": true,
			"duration_ms": res.DurationMs, "stderr": res.Stderr,
		})
		return false, fmt.Errorf("ffprobe timed out after %s", timeout)
	}
	if res.ExitCode != 0 {
		camlog("warn", "ffprobe_has_video", map[string]any{
			"ok": false, "cmd": cmdLine, "url": maskCredsURL(rawURL), "exit_code": res.ExitCode,
			"duration_ms": res.DurationMs, "stderr": res.Stderr,
		})
		return false, fmt.Errorf("ffprobe exited %d: %s", res.ExitCode, res.Stderr)
	}
	hasVideo := strings.Contains(strings.ToLower(res.Stdout), "video")
	camlog("debug", "ffprobe_has_video", map[string]any{
		"ok": true, "cmd": cmdLine, "url": maskCredsURL(rawURL), "has_video": hasVideo,
		"duration_ms": res.DurationMs,
	})
	return hasVideo, nil
}

// ─────────────────────────── clip frame sampling ───────────────────────────

// sampleClipFrames extracts up to maxFrames still frames from an already-captured
// clip (roughly one every everySec seconds, downscaled to at most maxWidth wide —
// never upscaled, via ffmpeg's scale=min(maxWidth,iw)) for vision backends that
// can't ingest video directly (claude's inline-base64 path — see cameraorch.go).
// everySec<=0, maxFrames<=0, and maxWidth<=0 each fall back to the plan's default
// (2s / 8 frames / 768px). Frames are written to destDir as frame_NN.jpg and
// returned in order.
func sampleClipFrames(ctx context.Context, cfg config, clipPath, destDir string, everySec, maxFrames, maxWidth int) ([]string, error) {
	cameraFFmpegPreflight(cfg)
	if everySec <= 0 {
		everySec = 2
	}
	if maxFrames <= 0 {
		maxFrames = 8
	}
	if maxWidth <= 0 {
		maxWidth = 768
	}
	if err := os.MkdirAll(destDir, 0o700); err != nil {
		return nil, fmt.Errorf("create frame dir: %w", err)
	}
	pattern := filepath.Join(destDir, "frame_%02d.jpg")
	timeout := cfg.CameraCaptureTimeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	// Frame extraction is CPU/decode-bound, not network-bound like a live capture,
	// but it can still take a while for a long clip — give it a generous multiple
	// of the base timeout rather than reusing the network stall guard.
	timeout *= 4

	// scale=min(maxWidth,iw):-2 never upscales a source narrower than maxWidth
	// (common for CCTV sub-streams); -2 keeps the output height even (required by
	// most encoders). The comma inside min(...) must be backslash-escaped because
	// ffmpeg's filtergraph syntax already uses "," to separate filters.
	vf := fmt.Sprintf(`fps=1/%d,scale=min(%d\,iw):-2`, everySec, maxWidth)
	args := []string{
		"-nostdin", "-loglevel", "error",
		"-i", clipPath,
		"-vf", vf,
		"-frames:v", strconv.Itoa(maxFrames),
		"-q:v", "3",
		"-y", pattern,
	}
	bin := cameraFFmpegBin(cfg)
	cmdLine := maskedCamCmd(bin, args, "") // clipPath is a local temp file, never credential-bearing

	start := time.Now()
	res, err := runCamCommand(ctx, timeout, bin, args...)
	if err != nil {
		camlog("error", "sample_clip_frames", map[string]any{"clip": clipPath, "cmd": cmdLine, "ok": false, "error": err.Error()})
		return nil, fmt.Errorf("ffmpeg exec failed: %w", err)
	}
	if res.TimedOut || res.ExitCode != 0 {
		fields := map[string]any{
			"clip": clipPath, "cmd": cmdLine, "ok": false, "exit_code": res.ExitCode, "timed_out": res.TimedOut,
			"latency_ms": time.Since(start).Milliseconds(),
		}
		if res.Stderr != "" {
			fields["stderr"] = res.Stderr // FULL, never truncated
		}
		camlog("error", "sample_clip_frames", fields)
		return nil, fmt.Errorf("ffmpeg frame sampling failed (exit %d): %s", res.ExitCode, truncateString(res.Stderr, 500))
	}
	matches, _ := filepath.Glob(filepath.Join(destDir, "frame_*.jpg"))
	sort.Strings(matches)
	camlog("info", "sample_clip_frames", map[string]any{
		"clip": clipPath, "cmd": cmdLine, "ok": true, "frames": len(matches), "latency_ms": time.Since(start).Milliseconds(),
	})
	return matches, nil
}

// ─────────────────────────── shared ffmpeg arg helpers ───────────────────────────

// rtspTransport resolves cfg.CameraRTSPTransport, defaulting to "tcp" (the safe
// choice through NAT/firewalls; "udp" is lower-latency but frequently blocked).
func rtspTransport(cfg config) string {
	t := strings.ToLower(strings.TrimSpace(cfg.CameraRTSPTransport))
	if t == "" {
		return "tcp"
	}
	return t
}

// stallGuardMicros converts a Go duration to microseconds for ffmpeg's RTSP
// -timeout, capped at 15s: this flag is a best-effort stall guard, not the
// authoritative kill (the context deadline is), so it never needs to be as long
// as a multi-minute clip's overall timeout.
func stallGuardMicros(d time.Duration) int64 {
	if d <= 0 {
		d = 15 * time.Second
	}
	if d > 15*time.Second {
		d = 15 * time.Second
	}
	return int64(d / time.Microsecond)
}

// ─────────────────────────── black/no-signal classification ───────────────────────────

// analyzeImageLuma decodes path (any format sniffImage/image.Decode support) and
// computes the mean luma (0.299R+0.587G+0.114B) and its standard deviation over a
// subsampled pixel grid (every other pixel in each dimension — plenty for this
// heuristic, and much cheaper than a full scan on a multi-megapixel snapshot). ok
// is false when the file could not be opened or decoded as an image (a corrupt,
// truncated, or non-image capture) — callers must treat that as a capture
// failure, not as "mean=0, mission accomplished".
func analyzeImageLuma(path string) (mean, std float64, ok bool) {
	f, err := os.Open(path)
	if err != nil {
		return 0, 0, false
	}
	defer f.Close()
	img, _, err := image.Decode(f)
	if err != nil {
		return 0, 0, false
	}

	const step = 2
	bounds := img.Bounds()
	var sum, sumSq float64
	var n int
	for y := bounds.Min.Y; y < bounds.Max.Y; y += step {
		for x := bounds.Min.X; x < bounds.Max.X; x += step {
			r, g, b, _ := img.At(x, y).RGBA()
			// RGBA() returns 16-bit-scaled channel values (0..65535); take the
			// high byte to get the conventional 0..255 range before weighting.
			luma := 0.299*float64(r>>8) + 0.587*float64(g>>8) + 0.114*float64(b>>8)
			sum += luma
			sumSq += luma * luma
			n++
		}
	}
	if n == 0 {
		return 0, 0, false
	}
	mean = sum / float64(n)
	variance := sumSq/float64(n) - mean*mean
	if variance < 0 {
		variance = 0 // guard against float rounding producing a tiny negative
	}
	return mean, math.Sqrt(variance), true
}

// isBlankSnapshot classifies an already-captured snapshot for the "auto-disable
// cameras the DVR has turned off" requirement. Its signature is deliberately
// path-only (no ctx/cfg) — callers such as the setup/describe phase and the
// scheduler classify a file they just wrote and don't otherwise carry a config
// value at that call site — so thresholds are read from the package-level
// cameraCfg captured once by initCameras (camera.go), the same mechanism that
// file's doc comment reserves for exactly this kind of short-lived helper; a
// zero-value cameraCfg (e.g. in a unit test that never calls initCameras) simply
// falls back to the same defaults classifyCamSnapshot's cfg-taking predecessor
// used (16 / 6).
//
// reason is "" when the image looks fine, "black" when it is near-black with low
// variance (the DVR has visibly turned the feed off), "no-signal" when it has low
// variance but isn't dark (a solid gray/blue "no signal" card, common on
// disconnected analog-over-coax inputs), or "failed" when the file could not even
// be decoded as an image (a broken/empty capture). blank is true whenever reason
// is non-empty.
func isBlankSnapshot(path string) (blank bool, reason string) {
	mean, stddev, ok := analyzeImageLuma(path)
	if !ok {
		camlog("warn", "classify_snapshot", map[string]any{"path": path, "reason": "failed", "ok": false})
		return true, "failed"
	}

	lumaThresh := cameraCfg.CameraBlackLumaThreshold
	if lumaThresh <= 0 {
		lumaThresh = 16
	}
	sdThresh := cameraCfg.CameraBlackStddevThreshold
	if sdThresh <= 0 {
		sdThresh = 6
	}

	switch {
	case mean <= lumaThresh && stddev <= sdThresh:
		reason = "black"
	case stddev <= sdThresh:
		reason = "no-signal"
	}

	camlog("debug", "classify_snapshot", map[string]any{
		"path": path, "mean_luma": mean, "stddev": stddev, "reason": reason,
	})
	return reason != "", reason
}
