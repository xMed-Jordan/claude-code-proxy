package main

// camera_hikvision.go — Hikvision ISAPI adapter. Registers as brand "hikvision".
//
// Discovery asks the device for its real channel list over HTTP digest/basic auth
// (camera_digest.go's httpDigestGet): first GET /ISAPI/Streaming/channels, and if
// that fails or yields nothing, GET /ISAPI/ContentMgmt/InputProxy/channels. Both
// responses are parsed with encoding/xml into deliberately namespace-tolerant
// struct shapes (the XML carries an xmlns default namespace; Go matches by local
// element name when the field tag omits a namespace, so the tags below match
// regardless). Channel ids come from the DEVICE — never assumed contiguous 1..N —
// because NVRs renumber and skip.
//
//   - Streaming/channels reports one <StreamingChannel> per encoder stream with an
//     <id> that is streamId = channel*100 + streamType (…01 = main, …02 = sub), so
//     the physical channel is id/100. We fold main+sub back to one CamChannel.
//   - InputProxy/channels (typical on NVRs) reports one <InputProxyChannel> per
//     input with <id> = the actual channel number.
//
// URL builders are pure/deterministic (no I/O). Credentials are ALWAYS carried via
// url.UserPassword (RTSP) or the Authorization header (HTTP) — never concatenated —
// and only a maskCredsURL variant is ever logged. Time-window playback uses
// Hikvision's GMT "20060102T150405Z" convention (unlike Dahua's local underscore
// time), formatted from x.UTC().
import (
	"context"
	"encoding/xml"
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
	registerBrand("hikvision", hikvisionBrandAdapter{})
}

// hikvisionBrandAdapter is the CameraBrand implementation for Hikvision DVR/NVRs.
type hikvisionBrandAdapter struct{}

func (hikvisionBrandAdapter) Name() string { return "hikvision" }

// hikHTTPPort/hikRTSPPort apply the same hardcoded fallbacks as
// cameraDVRHTTPPort/cameraDVRPort (main.go) for the pure URL-builder methods,
// which — per the CameraBrand interface — don't receive cfg. Discover DOES have
// cfg and pre-resolves dvr.Port/dvr.HTTPPort via cameraDVRPort/cameraDVRHTTPPort so
// the configured defaults are honored; these are the last-resort floor when that
// hasn't happened (e.g. a DVR row loaded with Port/HTTPPort==0).
func hikHTTPPort(dvr CamDVR) int {
	if dvr.HTTPPort > 0 {
		return dvr.HTTPPort
	}
	return 80
}

// camDVRClock fetches the DVR's OWN wall-clock (Hikvision ISAPI /System/time) so
// the investigation prompt can tell the model both the real/operator time and the
// time the DVR actually stamps on its recordings — these drift apart when the DVR
// runs on manual (non-NTP) time. Best-effort: returns ok=false on any error or for
// brands without a known time endpoint, and the caller falls back to the server clock.
func camDVRClock(ctx context.Context, dvr CamDVR) (time.Time, bool) {
	if !strings.EqualFold(strings.TrimSpace(dvr.Brand), "hikvision") {
		return time.Time{}, false
	}
	cctx, cancel := context.WithTimeout(ctx, 6*time.Second)
	defer cancel()
	scheme := "http"
	if hikHTTPPort(dvr) == 443 {
		scheme = "https"
	}
	rawURL := fmt.Sprintf("%s://%s/ISAPI/System/time", scheme, net.JoinHostPort(dvr.Host, strconv.Itoa(hikHTTPPort(dvr))))
	resp, err := httpDigestGet(cctx, camHTTPClient(), rawURL, dvr.Username, dvr.Password)
	if err != nil {
		return time.Time{}, false
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
	var doc struct {
		LocalTime string `xml:"localTime"`
	}
	if xml.Unmarshal(body, &doc) != nil {
		return time.Time{}, false
	}
	t, perr := time.Parse(time.RFC3339, strings.TrimSpace(doc.LocalTime))
	if perr != nil {
		return time.Time{}, false
	}
	return t, true
}

func hikRTSPPort(dvr CamDVR) int {
	if dvr.Port > 0 {
		return dvr.Port
	}
	return 554
}

// hikScheme selects the HTTP scheme for ISAPI requests/snapshots. There is no TLS
// flag on CamDVR, so we heuristically upgrade to https only on the canonical HTTPS
// port; camHTTPClient tolerates the device's self-signed cert either way.
func hikScheme(dvr CamDVR) string {
	if hikHTTPPort(dvr) == 443 {
		return "https"
	}
	return "http"
}

// hikStreamNo maps (channel, quality) to Hikvision's streamId convention
// channel*100 + streamType (1 = main, 2 = sub): channel 1 main => 101, sub => 102.
func hikStreamNo(ch int, q StreamQuality) int {
	streamIdx := 1
	if q == StreamSub {
		streamIdx = 2
	}
	return ch*100 + streamIdx
}

// hikRTSPBaseURL builds "rtsp://user:pass@host:port" — credentials always via
// url.UserPassword, never string-concatenated (passwords may contain @ : / ?).
func hikRTSPBaseURL(dvr CamDVR) url.URL {
	return url.URL{
		Scheme: "rtsp",
		User:   url.UserPassword(dvr.Username, dvr.Password),
		Host:   net.JoinHostPort(dvr.Host, strconv.Itoa(hikRTSPPort(dvr))),
	}
}

// hikHTTPBaseURL builds "http(s)://host:httpPort<path>" with NO credentials
// embedded — HTTP auth is carried in the Authorization header (see httpDigestGet).
func hikHTTPBaseURL(dvr CamDVR, path string) string {
	u := url.URL{
		Scheme: hikScheme(dvr),
		Host:   net.JoinHostPort(dvr.Host, strconv.Itoa(hikHTTPPort(dvr))),
		Path:   path,
	}
	return u.String()
}

// LiveURL: rtsp://user:pass@host:port/Streaming/Channels/<channel*100+stream>
// (…01 = main, …02 = sub).
func (hikvisionBrandAdapter) LiveURL(dvr CamDVR, ch int, q StreamQuality) string {
	u := hikRTSPBaseURL(dvr)
	u.Path = fmt.Sprintf("/Streaming/Channels/%d", hikStreamNo(ch, q))
	return u.String()
}

// SnapshotURL: http(s)://host:httpPort/ISAPI/Streaming/channels/<channel*100+stream>/picture
// It is an HTTP (digest) still-image endpoint, so isHTTP=true and ok=true; the
// httpDigestGet layer supplies auth and no credentials appear in this URL. Both
// qualities are expressible (main => …01/picture, sub => …02/picture).
func (hikvisionBrandAdapter) SnapshotURL(dvr CamDVR, ch int, q StreamQuality) (string, bool, bool) {
	u := url.URL{
		Scheme: hikScheme(dvr),
		Host:   net.JoinHostPort(dvr.Host, strconv.Itoa(hikHTTPPort(dvr))),
		Path:   fmt.Sprintf("/ISAPI/Streaming/channels/%d/picture", hikStreamNo(ch, q)),
	}
	return u.String(), true, true
}

// hikLocation resolves the IANA timezone this DVR uses when it interprets playback
// starttime/endtime (see PlaybackURL — these devices seek against their OWN local
// clock, not GMT). Defaults to UTC when unset or unparseable, so a deployment with
// no configured timezone keeps the ISAPI-documented GMT behavior; a bad tz string
// is logged once rather than failing playback. Mirrors dahuaLocation.
func hikLocation(dvr CamDVR) *time.Location {
	tz := strings.TrimSpace(dvr.Timezone)
	if tz == "" {
		return time.UTC
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		camlog("warn", "hik_timezone_invalid", map[string]any{
			"dvr_id": dvr.ID, "timezone": tz, "ok": false, "error": err.Error(),
		})
		return time.UTC
	}
	return loc
}

// PlaybackURL: rtsp://user:pass@host:port/Streaming/tracks/<channel*100+1>
// ?starttime=<t>&endtime=<t> where t is "YYYYMMDDThhmmssZ".
//
// Time base: the Hikvision ISAPI spec documents starttime/endtime as GMT (the
// trailing "Z"), but the DVRs in the field interpret the digits against their OWN
// configured wall clock and IGNORE the Z — a request for "…T080907Z" returns the
// frame the DVR stamped 08:09:07 in ITS local time, not 08:09:07 UTC. Seeking in
// UTC therefore lands the tz offset off (e.g. 3h in Asia/Amman), silently returning
// stale footage. So we render the digits in the DVR's configured timezone
// (hikLocation) while keeping the literal "Z" token for URL-syntax compatibility,
// and fall back to UTC when no timezone is configured. The main track (…01) carries
// recorded footage. "No recording in the window" is detected at capture time (typed
// errNoRecording); here we only build a well-formed URL, rejecting only an inverted
// window with a clean (non-500) error.
func (hikvisionBrandAdapter) PlaybackURL(dvr CamDVR, ch int, q StreamQuality, start, end time.Time) (string, error) {
	if !end.After(start) {
		return "", fmt.Errorf("hikvision playback: end (%s) must be after start (%s)", end, start)
	}
	loc := hikLocation(dvr)
	u := hikRTSPBaseURL(dvr)
	u.Path = fmt.Sprintf("/Streaming/tracks/%d", ch*100+1)
	// The "Z" here is a literal (Go treats a lone trailing Z as a literal, not a
	// zone token), so the digits render in loc while the token shape stays intact.
	st := start.In(loc).Format("20060102T150405Z")
	et := end.In(loc).Format("20060102T150405Z")
	// Build the query manually: the values are ASCII-safe (digits/T/Z) and Hik
	// expects the raw starttime/endtime tokens; url.Values.Encode would reorder.
	u.RawQuery = fmt.Sprintf("starttime=%s&endtime=%s", st, et)
	return u.String(), nil
}

// hikStreamingChannel mirrors <StreamingChannel> (Streaming/channels). Fields are
// captured as strings and converted manually so a single malformed value can never
// abort the whole document parse. <id> is the streamId (channel*100 + streamType).
type hikStreamingChannel struct {
	ID          string `xml:"id"`
	ChannelName string `xml:"channelName"`
	Enabled     string `xml:"enabled"`
	Width       string `xml:"Video>videoResolutionWidth"`
	Height      string `xml:"Video>videoResolutionHeight"`
}

// hikStreamingChannelList mirrors <StreamingChannelList>. No XMLName is declared so
// Unmarshal ignores the (namespaced) root element name and simply collects the
// StreamingChannel children — the namespace-tolerant path.
type hikStreamingChannelList struct {
	Channels []hikStreamingChannel `xml:"StreamingChannel"`
}

// hikInputProxyChannel mirrors <InputProxyChannel> (InputProxy/channels). Here <id>
// is the real device channel number and <name> the operator-set title.
type hikInputProxyChannel struct {
	ID   string `xml:"id"`
	Name string `xml:"name"`
}

// hikInputProxyChannelList mirrors <InputProxyChannelList> (namespace-tolerant, no
// XMLName — see hikStreamingChannelList).
type hikInputProxyChannelList struct {
	Channels []hikInputProxyChannel `xml:"InputProxyChannel"`
}

// hikAtoi parses a trimmed integer, returning 0 (the "unknown" sentinel used by
// CamChannel.MainW/MainH) on any error.
func hikAtoi(s string) int {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return 0
	}
	return n
}

// hikChannelsFromStreaming folds the per-stream Streaming/channels rows into one
// CamChannel per physical channel (id/100), preferring the main stream (lowest
// streamType) for the name and resolution. Malformed ids are skipped. The device's
// own channel numbers are preserved — never renumbered to 1..N.
func hikChannelsFromStreaming(list hikStreamingChannelList) []CamChannel {
	byCh := map[int]*CamChannel{}
	bestStream := map[int]int{} // channel -> lowest streamType folded so far
	for _, sc := range list.Channels {
		id := hikAtoi(sc.ID)
		if id <= 0 {
			continue
		}
		ch := id / 100
		streamType := id % 100
		if ch == 0 { // firmware that reports a bare channel id (< 100)
			ch = id
			streamType = 1
		}
		c, ok := byCh[ch]
		if !ok {
			c = &CamChannel{Channel: ch}
			byCh[ch] = c
			bestStream[ch] = 1 << 30
		}
		if streamType < bestStream[ch] {
			bestStream[ch] = streamType
			if name := strings.TrimSpace(sc.ChannelName); name != "" {
				c.Name = name
			}
			if w := hikAtoi(sc.Width); w > 0 {
				c.MainW = w
			}
			if h := hikAtoi(sc.Height); h > 0 {
				c.MainH = h
			}
		}
	}
	out := make([]CamChannel, 0, len(byCh))
	for _, c := range byCh {
		out = append(out, *c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Channel < out[j].Channel })
	return out
}

// hikChannelsFromInputProxy maps InputProxy/channels rows straight to CamChannels
// (id is the device channel). Malformed/duplicate ids are skipped; ids preserved.
func hikChannelsFromInputProxy(list hikInputProxyChannelList) []CamChannel {
	seen := map[int]bool{}
	var out []CamChannel
	for _, ic := range list.Channels {
		id := hikAtoi(ic.ID)
		if id <= 0 || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, CamChannel{Channel: id, Name: strings.TrimSpace(ic.Name)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Channel < out[j].Channel })
	return out
}

// hikGet performs the digest/basic-authenticated GET and returns the body, capping
// the read (a 64-ch NVR channel list stays well under 4MB) and turning any non-200
// into a descriptive (credential-free) error.
func hikGet(ctx context.Context, rawURL, user, pass string) ([]byte, error) {
	resp, err := httpDigestGet(ctx, camHTTPClient(), rawURL, user, pass)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, rerr := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncateString(string(body), 200))
	}
	if rerr != nil {
		return nil, fmt.Errorf("read failed: %w", rerr)
	}
	return body, nil
}

// hikFetchStreamingChannels fetches + parses /ISAPI/Streaming/channels.
func hikFetchStreamingChannels(ctx context.Context, rawURL, user, pass string) ([]CamChannel, error) {
	body, err := hikGet(ctx, rawURL, user, pass)
	if err != nil {
		return nil, err
	}
	var list hikStreamingChannelList
	if uerr := xml.Unmarshal(body, &list); uerr != nil {
		return nil, fmt.Errorf("Streaming/channels XML parse failed: %w", uerr)
	}
	return hikChannelsFromStreaming(list), nil
}

// hikFetchInputProxyChannels fetches + parses /ISAPI/ContentMgmt/InputProxy/channels.
func hikFetchInputProxyChannels(ctx context.Context, rawURL, user, pass string) ([]CamChannel, error) {
	body, err := hikGet(ctx, rawURL, user, pass)
	if err != nil {
		return nil, err
	}
	var list hikInputProxyChannelList
	if uerr := xml.Unmarshal(body, &list); uerr != nil {
		return nil, fmt.Errorf("InputProxy/channels XML parse failed: %w", uerr)
	}
	return hikChannelsFromInputProxy(list), nil
}

// hikLogDigestChallenge emits, at debug level only, the WWW-Authenticate challenge
// the device presents (realm/nonce/algorithm/qop — no credentials, safe to log).
// It costs one extra unauthenticated round-trip, paid only during first-phase
// bring-up (PROXY_CAMERA_LOG_LEVEL=debug); production info/warn levels skip it.
func hikLogDigestChallenge(ctx context.Context, rawURL, dvrID string) {
	if camLogThreshold() > camLogLevelOrder["debug"] {
		return
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return
	}
	resp, err := camHTTPClient().Do(req)
	if err != nil {
		camlog("debug", "discover_challenge", map[string]any{
			"dvr_id": dvrID, "brand": "hikvision", "ok": false, "error": err.Error(),
		})
		return
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	camlog("debug", "discover_challenge", map[string]any{
		"dvr_id": dvrID, "brand": "hikvision", "status": resp.StatusCode,
		"www_authenticate": strings.TrimSpace(resp.Header.Get("WWW-Authenticate")),
	})
}

// Discover reads the DVR's real channel list. It tries Streaming/channels first and
// falls back to InputProxy/channels when the former errors or is empty (NVRs often
// only populate the InputProxy list). Every step is logged (brand matched, which
// endpoint answered, channel count, latency); the digest challenge is logged at
// debug. Returns a clean error (never a device panic) when neither endpoint yields
// a channel.
func (a hikvisionBrandAdapter) Discover(ctx context.Context, cfg config, dvr CamDVR) ([]CamChannel, error) {
	start := time.Now()
	timeout := cfg.CameraDiscoverTimeout
	if timeout <= 0 {
		timeout = 20 * time.Second
	}
	dctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// Honor configured port defaults for discovery (URL builders fall back on their
	// own hardcoded floors, but Discover has cfg so it can respect the operator's).
	dvr.Port = cameraDVRPort(cfg, dvr)
	dvr.HTTPPort = cameraDVRHTTPPort(cfg, dvr)

	camlog("info", "discover_start", map[string]any{
		"dvr_id": dvr.ID, "brand": "hikvision", "host": dvr.Host, "http_port": hikHTTPPort(dvr),
	})

	streamingURL := hikHTTPBaseURL(dvr, "/ISAPI/Streaming/channels")
	hikLogDigestChallenge(dctx, streamingURL, dvr.ID)

	// Primary: /ISAPI/Streaming/channels
	camlog("debug", "discover_request", map[string]any{
		"dvr_id": dvr.ID, "brand": "hikvision", "endpoint": "streaming", "url": maskCredsURL(streamingURL),
	})
	channels, err1 := hikFetchStreamingChannels(dctx, streamingURL, dvr.Username, dvr.Password)
	if err1 == nil && len(channels) > 0 {
		camlog("info", "discover", map[string]any{
			"dvr_id": dvr.ID, "brand": "hikvision", "ok": true, "endpoint": "streaming",
			"channels_found": len(channels), "channel_ids": hikChannelIDs(channels),
			"latency_ms": time.Since(start).Milliseconds(),
		})
		return channels, nil
	}
	camlog("warn", "discover_streaming_fallback", map[string]any{
		"dvr_id": dvr.ID, "brand": "hikvision", "ok": false,
		"error":                   hikErrText(err1, "Streaming/channels returned no channels"),
		"channels_from_streaming": len(channels),
	})

	// Fallback: /ISAPI/ContentMgmt/InputProxy/channels
	inputURL := hikHTTPBaseURL(dvr, "/ISAPI/ContentMgmt/InputProxy/channels")
	camlog("debug", "discover_request", map[string]any{
		"dvr_id": dvr.ID, "brand": "hikvision", "endpoint": "inputproxy", "url": maskCredsURL(inputURL),
	})
	channels2, err2 := hikFetchInputProxyChannels(dctx, inputURL, dvr.Username, dvr.Password)
	if err2 == nil && len(channels2) > 0 {
		camlog("info", "discover", map[string]any{
			"dvr_id": dvr.ID, "brand": "hikvision", "ok": true, "endpoint": "inputproxy",
			"channels_found": len(channels2), "channel_ids": hikChannelIDs(channels2),
			"latency_ms": time.Since(start).Milliseconds(),
		})
		return channels2, nil
	}

	finalErr := fmt.Errorf("hikvision discovery found no channels: streaming[%s], inputproxy[%s]",
		hikErrText(err1, "empty"), hikErrText(err2, "empty"))
	camlog("error", "discover", map[string]any{
		"dvr_id": dvr.ID, "brand": "hikvision", "ok": false, "error": finalErr.Error(),
		"latency_ms": time.Since(start).Milliseconds(),
	})
	return nil, finalErr
}

// hikChannelIDs extracts the discovered channel numbers for a compact log field.
func hikChannelIDs(chs []CamChannel) []int {
	ids := make([]int, 0, len(chs))
	for _, c := range chs {
		ids = append(ids, c.Channel)
	}
	return ids
}

// hikErrText renders an error for a log/message, substituting a fallback when nil.
func hikErrText(err error, ifNil string) string {
	if err != nil {
		return err.Error()
	}
	return ifNil
}
