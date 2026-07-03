package main

// camera.go — foundation for the RTSP/DVR camera-monitoring subsystem.
//
// This file owns the in-memory types, the brand-adapter contract, the capture
// concurrency semaphore, and the one-time initCameras() startup hook. It holds
// NO device I/O and NO SQL — brand adapters (camera_hikvision.go etc.), the
// capture layer (camera_capture.go), and the store (camera_store.go) build on
// the types defined here. Config fields live on the shared `config` struct in
// main.go (added by this work package); the getenv wiring is in loadConfig.
//
// Design mirrors the "agy" upstream: a bounded subprocess/IO semaphore
// (captureSem ~ agySem), context-aware acquire/release helpers, and a package
// level cfg captured at startup so short-lived helpers can reach settings.

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// parseCamMaxRounds parses the escalation round cap (default 4), clamped to 1..6
// so a misconfiguration can never make a monitoring run unbounded.
func parseCamMaxRounds(s string) int {
	n := parseIntDefault(s, 4)
	if n < 1 {
		n = 1
	}
	if n > 6 {
		n = 6
	}
	return n
}

// camParseFloat parses a float64 env value with a fallback default.
func camParseFloat(s string, def float64) float64 {
	f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return def
	}
	return f
}

// StreamQuality selects which encoder stream of a channel to use. DVRs publish a
// high-bitrate "main" stream and a low-bitrate "sub" stream; monitoring works from
// the cheap sub stream (mosaics), evidence clips prefer main.
type StreamQuality int

const (
	StreamMain StreamQuality = 0
	StreamSub  StreamQuality = 1
)

func (q StreamQuality) String() string {
	if q == StreamSub {
		return "sub"
	}
	return "main"
}

// CamDVR is a decrypted, in-memory view of a camera_dvrs row: everything a brand
// adapter needs to reach a device. Password is plaintext and populated only for
// the lifetime of an operation (via decryptSecret); it must NEVER be logged or
// serialized — build device URLs with url.UserPassword and log a masked variant.
type CamDVR struct {
	ID             string
	SiteID         string
	Name           string
	Brand          string
	Host           string
	Port           int // RTSP port (default cfg.CameraDefaultRTSPPort)
	HTTPPort       int // HTTP/ISAPI/CGI port (default cfg.CameraDefaultHTTPPort)
	Username       string
	Password       string // plaintext, in-memory only — never log or store as-is
	Timezone       string // IANA tz used for playback time formatting (Dahua local time)
	AIInstructions string // operator-authored site knowledge injected into every investigation (doors, rooms, movement rules)
	Enabled        bool
}

// CamChannel is one discovered input channel on a DVR. Channel numbers come from
// the device and are NOT assumed contiguous — NVRs renumber and skip.
type CamChannel struct {
	Channel int    // device-assigned channel id
	Name    string // device-provided title, if any
	MainW   int    // optional main-stream width  (0 if unknown)
	MainH   int    // optional main-stream height (0 if unknown)
}

// CameraBrand is the contract each device family (Hikvision, Dahua, ONVIF, and the
// RTSP-probe fallback) implements: discovery plus URL builders for live view,
// snapshot, and time-window playback. Implementations are pure/deterministic for
// the URL builders (no I/O) so they are trivially unit-testable; only Discover
// touches the network.
type CameraBrand interface {
	// Name is the lowercase brand key this adapter registers under.
	Name() string
	// Discover probes the DVR and returns its real channel list (never assume 1..N).
	Discover(ctx context.Context, cfg config, dvr CamDVR) ([]CamChannel, error)
	// LiveURL returns an RTSP URL for a live stream at the requested quality.
	LiveURL(dvr CamDVR, ch int, q StreamQuality) string
	// SnapshotURL returns a still-image URL. isHTTP reports whether it is an HTTP
	// (digest) snapshot endpoint (vs an RTSP url to grab one frame from); ok is
	// false when the brand has no snapshot URL for this channel/quality.
	SnapshotURL(dvr CamDVR, ch int, q StreamQuality) (url string, isHTTP bool, ok bool)
	// PlaybackURL returns an RTSP URL for recorded footage in [start,end]. It
	// returns an error when the brand cannot express the window.
	PlaybackURL(dvr CamDVR, ch int, q StreamQuality, start, end time.Time) (string, error)
}

// brandRegistry maps a lowercase brand key → adapter. Adapters register themselves
// from their own init() (registerBrand), so the flat package stays decoupled.
var brandRegistry = map[string]CameraBrand{}

// rtspProbeBrand is the fallback adapter key used when a DVR's brand is unknown.
const rtspProbeBrand = "rtsp"

// registerBrand records a brand adapter. Called from each adapter's init().
func registerBrand(name string, b CameraBrand) {
	key := strings.ToLower(strings.TrimSpace(name))
	if key == "" || b == nil {
		return
	}
	brandRegistry[key] = b
}

// brandFor returns the adapter for a brand name, falling back to the RTSP
// channel-probe adapter when the brand is unknown/unset. May return nil if even
// the fallback has not been registered (e.g. in a minimal unit-test build).
func brandFor(name string) CameraBrand {
	if b, ok := brandRegistry[strings.ToLower(strings.TrimSpace(name))]; ok {
		return b
	}
	return brandRegistry[rtspProbeBrand]
}

// camera is an in-memory view of a cameras row (one discovered channel we track).
type camera struct {
	ID                string
	SiteID            string
	DVRID             string
	Channel           int
	Name              string
	Area              string
	AIDescription     string
	AILocation        string
	Notes             string // operator-owned notes (ai_location is AI-owned and clobbered by re-describe)
	Enabled           bool
	DisabledReason    string // "", "black", "no-signal", "failed"
	SnapshotCaptureID string
	Caps              string // JSON blob of discovered capabilities
	CreatedAt         string
	UpdatedAt         string
}

// watch is an in-memory view of a camera_watches row (a scheduled monitoring rule).
type watch struct {
	ID              string
	SiteID          string
	Name            string
	Instruction     string
	CameraIDs       string // JSON array of camera ids
	AnalysisAlias   string // "" → cfg.CameraAnalysisAlias
	Mode            string // "mosaic_only" | "escalate"
	IntervalSeconds int
	ActiveHours     string // "" (always) or a serialized window
	MaxRounds       int
	ActionJSON      string
	Enabled         bool
	NextRunAt       string
	LastRunAt       string
	CreatedAt       string
	UpdatedAt       string
}

// captureSem bounds concurrent ffmpeg/HTTP capture operations. Each RTSP pull is a
// heavy process and DVRs cap simultaneous sessions, so this is kept small (ideally
// per-DVR). Sized at startup by initCameras from cfg.CameraCaptureConcurrency.
var captureSem chan struct{}

// cameraCfg is the config captured at startup so short-lived helpers (camlog's
// camera_events writer, the media root, ffmpeg paths) can reach settings without
// threading cfg through every call. Written once by initCameras.
var cameraCfg config

// initCameras sizes the capture semaphore, ensures the served media dir exists,
// captures cfg for package helpers, and emits a startup line. Safe to call once
// from runServe regardless of cfg.CameraEnabled (the scheduler is what respects
// the enable flag).
func initCameras(cfg config) {
	cameraCfg = cfg
	n := cfg.CameraCaptureConcurrency
	if n < 1 {
		n = 1
	}
	captureSem = make(chan struct{}, n)
	root := cameraMediaRoot(cfg)
	if err := os.MkdirAll(root, 0o700); err != nil {
		camlog("error", "init", map[string]any{"error": err.Error(), "media_dir": root, "ok": false})
	}
	camlog("info", "init", map[string]any{
		"enabled":             cfg.CameraEnabled,
		"analysis_alias":      cfg.CameraAnalysisAlias,
		"orch_mode":           cfg.CameraOrchMode,
		"capture_concurrency": n,
		"media_dir":           root,
		"ffmpeg":              cameraFFmpegBin(cfg),
		"ffprobe":             cameraFFprobeBin(cfg),
	})
}

// camCaptureAcquire blocks for a capture slot, honoring ctx cancellation/timeout so
// a queued capture can give up cleanly. Mirrors agyAcquire. Returns nil on success
// or ctx.Err() if cancelled while waiting.
func camCaptureAcquire(ctx context.Context) error {
	if captureSem == nil {
		return nil // not initialized (e.g. unit tests) → run unbounded
	}
	select {
	case captureSem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// camCaptureRelease frees a capture slot. Mirrors agyRelease (safe if never acquired).
func camCaptureRelease() {
	if captureSem == nil {
		return
	}
	select {
	case <-captureSem:
	default:
	}
}

// cameraMediaRoot is the flat directory captured snapshots/clips/mosaics are served
// from (mirrors agyImageRoot). Reaped hourly (cameramedia.go, WP11).
func cameraMediaRoot(cfg config) string {
	if d := strings.TrimSpace(cfg.CameraMediaDir); d != "" {
		return d
	}
	return filepath.Join(os.TempDir(), "connect-ai-proxy-cameras")
}

// cameraFFmpegBin resolves the ffmpeg binary ("" → "ffmpeg" on PATH).
func cameraFFmpegBin(cfg config) string {
	if b := strings.TrimSpace(cfg.CameraFFmpegBin); b != "" {
		return b
	}
	return "ffmpeg"
}

// cameraFFprobeBin resolves the ffprobe binary ("" → "ffprobe" on PATH).
func cameraFFprobeBin(cfg config) string {
	if b := strings.TrimSpace(cfg.CameraFFprobeBin); b != "" {
		return b
	}
	return "ffprobe"
}

// cameraDVRPort returns the DVR's RTSP port, applying the configured default when unset.
func cameraDVRPort(cfg config, dvr CamDVR) int {
	if dvr.Port > 0 {
		return dvr.Port
	}
	if cfg.CameraDefaultRTSPPort > 0 {
		return cfg.CameraDefaultRTSPPort
	}
	return 554
}

// cameraDVRHTTPPort returns the DVR's HTTP port, applying the configured default when unset.
func cameraDVRHTTPPort(cfg config, dvr CamDVR) int {
	if dvr.HTTPPort > 0 {
		return dvr.HTTPPort
	}
	if cfg.CameraDefaultHTTPPort > 0 {
		return cfg.CameraDefaultHTTPPort
	}
	return 80
}
