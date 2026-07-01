package main

// camera_dahua.go — Dahua CGI adapter: channel discovery via the documented
// configManager.cgi ChannelTitle config (falling back to camera_rtspprobe.go's
// sequential RTSP probe when that CGI call fails or returns nothing), plus
// live/snapshot/playback URL builders for Dahua's cam/realmonitor + cam/playback
// RTSP conventions and the snapshot.cgi HTTP endpoint. Registers as brand "dahua".
//
// Time-window playback uses Dahua's LOCAL-time "YYYY_MM_DD_HH_MM_SS" convention
// (unlike Hikvision's GMT/Z), so it is formatted in the DVR's configured IANA
// timezone (camera_dvrs.timezone) — see dahuaLocation.
import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

func init() {
	registerBrand("dahua", dahuaBrandAdapter{})
}

// dahuaBrandAdapter is the CameraBrand implementation for Dahua DVR/NVRs.
type dahuaBrandAdapter struct{}

func (dahuaBrandAdapter) Name() string { return "dahua" }

// dahuaHTTPPort/dahuaRTSPPort apply the same hardcoded fallbacks as
// cameraDVRHTTPPort/cameraDVRPort (main.go) for the pure URL-builder methods,
// which — per the CameraBrand interface — don't receive cfg. Callers that DO have
// cfg (Discover, and the future capture layer) are expected to pre-resolve
// dvr.Port/dvr.HTTPPort via cameraDVRPort/cameraDVRHTTPPort so the configured
// defaults (not just these hardcoded ones) are honored; these are the last-resort
// floor when that hasn't happened (e.g. a DVR row loaded with Port/HTTPPort==0).
func dahuaHTTPPort(dvr CamDVR) int {
	if dvr.HTTPPort > 0 {
		return dvr.HTTPPort
	}
	return 80
}

func dahuaRTSPPort(dvr CamDVR) int {
	if dvr.Port > 0 {
		return dvr.Port
	}
	return 554
}

// dahuaLocation resolves the IANA timezone configured on the DVR for local-time
// playback formatting, defaulting to UTC (not the proxy host's local time, which
// would silently vary by deployment) when unset or unparseable. A bad timezone
// string is logged once here rather than failing playback outright.
func dahuaLocation(dvr CamDVR) *time.Location {
	tz := strings.TrimSpace(dvr.Timezone)
	if tz == "" {
		return time.UTC
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		camlog("warn", "dahua_timezone_invalid", map[string]any{
			"dvr_id": dvr.ID, "timezone": tz, "ok": false, "error": err.Error(),
		})
		return time.UTC
	}
	return loc
}

// dahuaRTSPBaseURL builds "rtsp://user:pass@host:port" — credentials always via
// url.UserPassword, never string-concatenated.
func dahuaRTSPBaseURL(dvr CamDVR) url.URL {
	return url.URL{
		Scheme: "rtsp",
		User:   url.UserPassword(dvr.Username, dvr.Password),
		Host:   net.JoinHostPort(dvr.Host, strconv.Itoa(dahuaRTSPPort(dvr))),
	}
}

func dahuaSubtype(q StreamQuality) string {
	if q == StreamSub {
		return "1"
	}
	return "0"
}

// LiveURL: rtsp://user:pass@host:port/cam/realmonitor?channel=N&subtype=0|1
func (dahuaBrandAdapter) LiveURL(dvr CamDVR, ch int, q StreamQuality) string {
	u := dahuaRTSPBaseURL(dvr)
	u.Path = "/cam/realmonitor"
	v := url.Values{}
	v.Set("channel", strconv.Itoa(ch))
	v.Set("subtype", dahuaSubtype(q))
	u.RawQuery = v.Encode()
	return u.String()
}

// SnapshotURL: http://host:httpPort/cgi-bin/snapshot.cgi?channel=N&subtype=0|1
// (digest/basic auth, handled by camera_digest.go's httpDigestGet — credentials
// are never put in this URL). Newer Dahua firmware honors "subtype" the same way
// realmonitor/playback do (0=main, 1=sub); older firmware without a sub selector
// simply ignores the extra query param and returns its one configured snapshot
// resolution, so it is always safe to send.
func (dahuaBrandAdapter) SnapshotURL(dvr CamDVR, ch int, q StreamQuality) (string, bool, bool) {
	u := url.URL{
		Scheme: "http",
		Host:   net.JoinHostPort(dvr.Host, strconv.Itoa(dahuaHTTPPort(dvr))),
		Path:   "/cgi-bin/snapshot.cgi",
	}
	v := url.Values{}
	v.Set("channel", strconv.Itoa(ch))
	v.Set("subtype", dahuaSubtype(q))
	u.RawQuery = v.Encode()
	return u.String(), true, true
}

// PlaybackURL: rtsp://user:pass@host:port/cam/playback?channel=N&subtype=0|1
// &starttime=YYYY_MM_DD_HH_MM_SS&endtime=YYYY_MM_DD_HH_MM_SS, both timestamps
// rendered in the DVR's local timezone (Dahua's documented convention — unlike
// Hikvision's GMT/Z ISAPI query params).
func (dahuaBrandAdapter) PlaybackURL(dvr CamDVR, ch int, q StreamQuality, start, end time.Time) (string, error) {
	if !end.After(start) {
		return "", fmt.Errorf("dahua playback: end (%s) must be after start (%s)", end, start)
	}
	loc := dahuaLocation(dvr)
	u := dahuaRTSPBaseURL(dvr)
	u.Path = "/cam/playback"
	v := url.Values{}
	v.Set("channel", strconv.Itoa(ch))
	v.Set("subtype", dahuaSubtype(q))
	v.Set("starttime", start.In(loc).Format("2006_01_02_15_04_05"))
	v.Set("endtime", end.In(loc).Format("2006_01_02_15_04_05"))
	u.RawQuery = v.Encode()
	return u.String(), nil
}

// dahuaChannelTitleURL builds the getConfig&name=ChannelTitle CGI URL (no
// credentials embedded — auth is via the Authorization header, see Discover).
func dahuaChannelTitleURL(dvr CamDVR) string {
	u := url.URL{
		Scheme: "http",
		Host:   net.JoinHostPort(dvr.Host, strconv.Itoa(dahuaHTTPPort(dvr))),
		Path:   "/cgi-bin/configManager.cgi",
	}
	v := url.Values{}
	v.Set("action", "getConfig")
	v.Set("name", "ChannelTitle")
	u.RawQuery = v.Encode()
	return u.String()
}

// dahuaParseChannelTitles parses lines of the form
// `table.ChannelTitle[0]=Front Door` (0-based index in the CGI response; Dahua's
// channel/subtype query params are 1-based, so the returned CamChannel.Channel is
// idx+1). Tolerant of a leading "table." or not, surrounding quotes on the value,
// and CRLF line endings. Unparseable lines are skipped, not fatal.
func dahuaParseChannelTitles(body string) []CamChannel {
	var out []CamChannel
	for _, raw := range strings.Split(body, "\n") {
		line := strings.TrimSpace(strings.TrimRight(raw, "\r"))
		if line == "" {
			continue
		}
		low := strings.ToLower(line)
		marker := "channeltitle["
		mi := strings.Index(low, marker)
		if mi < 0 {
			continue
		}
		rest := line[mi+len(marker):]
		closeIdx := strings.Index(rest, "]")
		if closeIdx < 0 {
			continue
		}
		idx, err := strconv.Atoi(strings.TrimSpace(rest[:closeIdx]))
		if err != nil {
			continue
		}
		eq := strings.Index(rest, "=")
		if eq < 0 || eq < closeIdx {
			continue
		}
		name := strings.Trim(strings.TrimSpace(rest[eq+1:]), `"`)
		out = append(out, CamChannel{Channel: idx + 1, Name: name})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Channel < out[j].Channel })
	return out
}

// Discover fetches ChannelTitle over digest/basic auth (camera_digest.go) and
// line-parses it; if that request fails, returns a non-2xx, or yields zero
// channels, it falls back to camera_rtspprobe.go's sequential RTSP probe using
// Dahua's own cam/realmonitor URL scheme. Channel ids come from the device (or the
// probe's discovered sequence) — never assumed contiguous.
func (a dahuaBrandAdapter) Discover(ctx context.Context, cfg config, dvr CamDVR) ([]CamChannel, error) {
	start := time.Now()
	timeout := cfg.CameraDiscoverTimeout
	if timeout <= 0 {
		timeout = 20 * time.Second
	}
	dctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	rawURL := dahuaChannelTitleURL(dvr)
	camlog("debug", "discover_request", map[string]any{
		"dvr_id": dvr.ID, "brand": "dahua", "url": maskCredsURL(rawURL),
	})

	channels, cgiErr := dahuaFetchChannelTitles(dctx, rawURL, dvr.Username, dvr.Password)
	if cgiErr != nil || len(channels) == 0 {
		reason := "ChannelTitle returned no channels"
		if cgiErr != nil {
			reason = cgiErr.Error()
		}
		camlog("warn", "discover_cgi_fallback", map[string]any{
			"dvr_id": dvr.ID, "brand": "dahua", "ok": false,
			"error": reason, "channels_from_cgi": len(channels),
		})

		maxCh := cfg.CameraDiscoverMaxChannels
		if maxCh <= 0 {
			maxCh = 64
		}
		perProbe := timeout
		if perProbe <= 0 || perProbe > 5*time.Second {
			perProbe = 5 * time.Second
		}
		probed := rtspProbeChannels(dctx, cfg, dvr.ID, maxCh, perProbe, func(ch int) string {
			return a.LiveURL(dvr, ch, StreamMain)
		})
		if len(probed) == 0 {
			finalErr := cgiErr
			if finalErr == nil {
				finalErr = fmt.Errorf("ChannelTitle returned no channels and RTSP probe found none")
			}
			camlog("error", "discover", map[string]any{
				"dvr_id": dvr.ID, "brand": "dahua", "ok": false, "error": finalErr.Error(),
				"latency_ms": time.Since(start).Milliseconds(),
			})
			return nil, finalErr
		}
		channels = probed
	}

	camlog("info", "discover", map[string]any{
		"dvr_id": dvr.ID, "brand": "dahua", "ok": true, "channels_found": len(channels),
		"latency_ms": time.Since(start).Milliseconds(),
	})
	return channels, nil
}

// dahuaFetchChannelTitles performs the digest-authenticated GET and parses the
// body. Split out from Discover so the HTTP/parse error paths are simple to
// reason about (and unit test with a fake server if WP15 wants to).
func dahuaFetchChannelTitles(ctx context.Context, rawURL, user, pass string) ([]CamChannel, error) {
	resp, err := httpDigestGet(ctx, camHTTPClient(), rawURL, user, pass)
	if err != nil {
		return nil, fmt.Errorf("ChannelTitle request failed: %w", err)
	}
	defer resp.Body.Close()
	body, rerr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("ChannelTitle HTTP %d: %s", resp.StatusCode, truncateString(string(body), 200))
	}
	if rerr != nil {
		return nil, fmt.Errorf("ChannelTitle read failed: %w", rerr)
	}
	return dahuaParseChannelTitles(string(body)), nil
}
