package main

// camera_rtspprobe.go — fallback discovery adapter for an unrecognized/unset DVR
// brand (registered under rtspProbeBrand == "rtsp", see camera.go's brandFor).
// It has no vendor CGI/ONVIF endpoint to ask, so it guesses: it tries a short list
// of well-known RTSP channel-path templates (Hikvision ISAPI-style, Dahua
// realmonitor-style, and a bare generic form) against channel 1, locks onto the
// first template that yields a live video stream (verified with ffprobe), then
// walks channels 1..N with that template until a run of consecutive misses says
// there are no more.
//
// rtspProbeChannels (the sequential probe loop) is exported at package scope so
// brand adapters that DO have a primary discovery method (e.g. Dahua's
// ChannelTitle CGI call) can reuse it as their own last-resort fallback when the
// primary method comes back empty — see camera_dahua.go.
//
// CameraBrand.LiveURL/SnapshotURL/PlaybackURL are documented as I/O-free, pure
// URL builders, but this adapter's channel-path "template" is only known after a
// network probe (Discover). Since CamDVR is passed by value and CamChannel has no
// slot for adapter-private state, the winning template is remembered in an
// in-memory, process-lifetime cache keyed by dvr.ID (rtspRememberTemplate /
// rtspTemplateFor below) — reading that cache is not network I/O, so the builder
// methods stay deterministic and testable once Discover has run. This does not
// survive a process restart; a future WP could persist the template name in a
// camera's `caps` JSON blob (explicitly reserved for discovered capabilities) if
// that turns out to matter in practice.
import (
	"context"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

func init() {
	registerBrand(rtspProbeBrand, rtspProbeBrandAdapter{})
}

// rtspProbeBrandAdapter is the CameraBrand implementation for unidentified DVRs.
type rtspProbeBrandAdapter struct{}

func (rtspProbeBrandAdapter) Name() string { return rtspProbeBrand }

// rtspProbeCandidate is one guessable channel-URL scheme.
type rtspProbeCandidate struct {
	name  string
	build func(dvr CamDVR, ch int, q StreamQuality) string
}

// rtspProbeCandidates is tried, in order, against channel 1 until one responds
// with a real video stream. Order matters: Hikvision and Dahua are the two most
// common brands a misconfigured/blank "brand" field would actually be.
var rtspProbeCandidates = []rtspProbeCandidate{
	{name: "hikvision-isapi", build: rtspHikvisionStyleURL},
	{name: "dahua-realmonitor", build: rtspDahuaStyleURL},
	{name: "generic-ch", build: rtspGenericChannelURL},
}

// rtspBaseURL builds "rtsp://user:pass@host:port" with the credentials encoded via
// url.UserPassword (never string-concatenated — passwords may contain @:/?).
func rtspBaseURL(dvr CamDVR) string {
	port := dvr.Port
	if port <= 0 {
		port = 554
	}
	u := url.URL{
		Scheme: "rtsp",
		User:   url.UserPassword(dvr.Username, dvr.Password),
		Host:   net.JoinHostPort(dvr.Host, strconv.Itoa(port)),
	}
	return u.String()
}

// rtspHikvisionStyleURL guesses Hikvision's ISAPI streaming path convention:
// channel*100 + stream-index (1=main, 2=sub), e.g. channel 1 main => ".../101".
func rtspHikvisionStyleURL(dvr CamDVR, ch int, q StreamQuality) string {
	streamIdx := 1
	if q == StreamSub {
		streamIdx = 2
	}
	return rtspBaseURL(dvr) + fmt.Sprintf("/Streaming/Channels/%d%02d", ch, streamIdx)
}

// rtspDahuaStyleURL guesses Dahua's cam/realmonitor convention.
func rtspDahuaStyleURL(dvr CamDVR, ch int, q StreamQuality) string {
	sub := 0
	if q == StreamSub {
		sub = 1
	}
	return rtspBaseURL(dvr) + fmt.Sprintf("/cam/realmonitor?channel=%d&subtype=%d", ch, sub)
}

// rtspGenericChannelURL is a last-resort bare "/chN[/sub]" guess used by a long
// tail of cheap/white-label DVRs.
func rtspGenericChannelURL(dvr CamDVR, ch int, q StreamQuality) string {
	suffix := ""
	if q == StreamSub {
		suffix = "/sub"
	}
	return rtspBaseURL(dvr) + fmt.Sprintf("/ch%d%s", ch, suffix)
}

// rtspProbeHasVideo shells out to ffprobe (via the shared commandOutput helper,
// which is intentionally used for cheap yes/no probes only — it swallows exit
// errors) and reports whether the URL yielded a video stream. Credentials are
// embedded in rawURL for ffprobe's -i flag (there is no header-based auth for
// RTSP); callers must log only the maskCredsURL variant.
//
// Bounded by captureSem (camCaptureAcquire/Release, camera.go) — the same
// bounded-subprocess pattern camera_capture.go uses for real captures — so a
// discovery scan can't pile up RTSP sessions against a DVR (or steal ffmpeg
// capacity from) concurrently running snapshot/clip captures. If ctx is
// cancelled while waiting for a slot, the probe reports "no video" rather than
// blocking past the caller's deadline.
func rtspProbeHasVideo(ctx context.Context, cfg config, rawURL string, timeout time.Duration) bool {
	if err := camCaptureAcquire(ctx); err != nil {
		return false
	}
	defer camCaptureRelease()

	transport := strings.ToLower(strings.TrimSpace(cfg.CameraRTSPTransport))
	if transport == "" {
		transport = "tcp"
	}
	out := commandOutput(timeout, cameraFFprobeBin(cfg),
		"-v", "error",
		"-rtsp_transport", transport,
		"-i", rawURL,
		"-select_streams", "v:0",
		"-show_entries", "stream=codec_type",
		"-of", "csv=p=0",
	)
	return strings.Contains(strings.ToLower(out), "video")
}

// camRTSPProbeMaxMisses bounds how many consecutive channel misses are tolerated
// before a sequential probe gives up (once at least one channel has been found —
// NVRs commonly have gaps but not long ones). If nothing has been found yet, the
// loop runs the full requested range once (it may just be a slow/high channel-1
// device) — cfg.CameraDiscoverMaxChannels already puts a hard ceiling on that.
const camRTSPProbeMaxMisses = 6

// rtspProbeChannels sequentially probes channel ids 1..maxCh using buildURL(ch)
// (which must return a full, credential-bearing RTSP URL) and returns every
// channel where ffprobe reports a live video stream. ctx is checked between
// iterations so a cancelled/expired parent context stops the scan promptly (the
// individual ffprobe subprocess calls are still bounded by perProbeTimeout
// regardless, per commandOutput). dvrIDForLog is attached to every camlog line so
// probe activity for a given DVR is queryable in camera_events.
func rtspProbeChannels(ctx context.Context, cfg config, dvrIDForLog string, maxCh int, perProbeTimeout time.Duration, buildURL func(ch int) string) []CamChannel {
	if maxCh <= 0 {
		maxCh = 64
	}
	if perProbeTimeout <= 0 {
		perProbeTimeout = 5 * time.Second
	}
	var out []CamChannel
	misses := 0
	for ch := 1; ch <= maxCh; ch++ {
		select {
		case <-ctx.Done():
			camlog("warn", "rtsp_probe_cancelled", map[string]any{
				"dvr_id": dvrIDForLog, "channel": ch, "ok": false, "error": ctx.Err().Error(),
			})
			return out
		default:
		}
		rawURL := buildURL(ch)
		start := time.Now()
		ok := rtspProbeHasVideo(ctx, cfg, rawURL, perProbeTimeout)
		camlog("debug", "rtsp_probe_channel", map[string]any{
			"dvr_id": dvrIDForLog, "channel": ch, "ok": ok,
			"latency_ms": time.Since(start).Milliseconds(), "url": maskCredsURL(rawURL),
		})
		if ok {
			out = append(out, CamChannel{Channel: ch})
			misses = 0
			continue
		}
		misses++
		if len(out) > 0 && misses >= camRTSPProbeMaxMisses {
			break
		}
	}
	return out
}

// rtspTemplateCache remembers, per DVR id, which candidate template Discover
// locked onto — see the file-level doc comment for why this is process-lifetime
// only and why that is an acceptable trade-off here.
var (
	rtspTemplateCacheMu sync.RWMutex
	rtspTemplateCache   = map[string]string{}
)

func rtspRememberTemplate(dvrID, name string) {
	if dvrID == "" || name == "" {
		return
	}
	rtspTemplateCacheMu.Lock()
	rtspTemplateCache[dvrID] = name
	rtspTemplateCacheMu.Unlock()
}

// rtspTemplateFor resolves the candidate template for a DVR: the in-memory cache
// first, then an "rtsp+<template>" hint smuggled into dvr.Brand (accepted for
// forward-compatibility, though no current caller writes one), then the first
// (Hikvision-style) candidate as a deterministic default. No network I/O.
func rtspTemplateFor(dvr CamDVR) rtspProbeCandidate {
	rtspTemplateCacheMu.RLock()
	name := rtspTemplateCache[dvr.ID]
	rtspTemplateCacheMu.RUnlock()
	if name == "" {
		if _, suffix, ok := strings.Cut(dvr.Brand, "+"); ok {
			name = strings.TrimSpace(suffix)
		}
	}
	if name != "" {
		for _, c := range rtspProbeCandidates {
			if c.name == name {
				return c
			}
		}
	}
	return rtspProbeCandidates[0]
}

// Discover tries each candidate template against channel 1, locks onto the first
// one that yields video, then walks channels with rtspProbeChannels. Never assumes
// contiguous ids beyond that walk; returns an error (never a device panic/500) if
// no template and no channel produced a stream.
func (rtspProbeBrandAdapter) Discover(ctx context.Context, cfg config, dvr CamDVR) ([]CamChannel, error) {
	start := time.Now()
	timeout := cfg.CameraDiscoverTimeout
	if timeout <= 0 {
		timeout = 20 * time.Second
	}
	dctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	perProbe := timeout
	if perProbe <= 0 || perProbe > 5*time.Second {
		perProbe = 5 * time.Second
	}
	maxCh := cfg.CameraDiscoverMaxChannels
	if maxCh <= 0 {
		maxCh = 64
	}

	camlog("info", "discover_start", map[string]any{
		"dvr_id": dvr.ID, "brand": "rtsp-probe", "host": dvr.Host, "max_channels": maxCh,
	})

	var chosen rtspProbeCandidate
	found := false
	for _, cand := range rtspProbeCandidates {
		rawURL := cand.build(dvr, 1, StreamMain)
		probeStart := time.Now()
		ok := rtspProbeHasVideo(dctx, cfg, rawURL, perProbe)
		camlog("debug", "rtsp_probe_template", map[string]any{
			"dvr_id": dvr.ID, "template": cand.name, "ok": ok,
			"latency_ms": time.Since(probeStart).Milliseconds(), "url": maskCredsURL(rawURL),
		})
		if ok {
			chosen = cand
			found = true
			break
		}
		if dctx.Err() != nil {
			break
		}
	}
	if !found {
		err := fmt.Errorf("no RTSP channel template matched channel 1 (tried %d templates)", len(rtspProbeCandidates))
		camlog("error", "discover", map[string]any{
			"dvr_id": dvr.ID, "brand": "rtsp-probe", "ok": false, "error": err.Error(),
			"latency_ms": time.Since(start).Milliseconds(),
		})
		return nil, err
	}

	channels := rtspProbeChannels(dctx, cfg, dvr.ID, maxCh, perProbe, func(ch int) string {
		return chosen.build(dvr, ch, StreamMain)
	})
	if len(channels) == 0 {
		err := fmt.Errorf("rtsp probe matched template %q on channel 1 but no channels responded on rescan", chosen.name)
		camlog("error", "discover", map[string]any{
			"dvr_id": dvr.ID, "brand": "rtsp-probe", "ok": false, "error": err.Error(),
			"latency_ms": time.Since(start).Milliseconds(),
		})
		return nil, err
	}
	rtspRememberTemplate(dvr.ID, chosen.name)

	camlog("info", "discover", map[string]any{
		"dvr_id": dvr.ID, "brand": "rtsp-probe", "ok": true, "template": chosen.name,
		"channels_found": len(channels), "latency_ms": time.Since(start).Milliseconds(),
	})
	return channels, nil
}

// LiveURL replays the template Discover locked onto for this DVR (see
// rtspTemplateFor). Pure/deterministic given the in-memory cache; no I/O.
func (rtspProbeBrandAdapter) LiveURL(dvr CamDVR, ch int, q StreamQuality) string {
	return rtspTemplateFor(dvr).build(dvr, ch, q)
}

// SnapshotURL: the generic fallback brand has no known HTTP snapshot endpoint (we
// don't know the vendor), so callers must grab a still frame from the RTSP live
// URL via ffmpeg instead (camera_capture.go's HTTP-first path simply skips ahead
// to the ffmpeg fallback when ok is false).
func (rtspProbeBrandAdapter) SnapshotURL(dvr CamDVR, ch int, q StreamQuality) (string, bool, bool) {
	return "", false, false
}

// PlaybackURL: there is no generic RTSP playback URL convention across vendors
// (Hikvision uses ISAPI GMT-Z query params, Dahua uses cam/playback with local
// underscore-time — both vendor-specific), so an unidentified DVR cannot express a
// time-window request. Operators should set the correct brand once known.
func (rtspProbeBrandAdapter) PlaybackURL(dvr CamDVR, ch int, q StreamQuality, start, end time.Time) (string, error) {
	return "", fmt.Errorf("rtsp-probe fallback brand has no playback support for an unidentified DVR; set the correct brand (hikvision/dahua/onvif) to enable time-window playback")
}
