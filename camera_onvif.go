package main

// camera_onvif.go — minimal, hand-rolled ONVIF adapter (brand "onvif"), no new
// dependency. It speaks just enough SOAP to (a) find devices on the LAN via
// WS-Discovery and (b) enumerate a device's media profiles and resolve their live
// RTSP + HTTP-snapshot URIs.
//
// Auth is WS-Security UsernameToken with a PasswordDigest —
// base64(sha1(nonce + created + password)) — carried in the SOAP header of every
// media request (onvifSecurityHeader). No credential ever appears in a URL or a
// log; device stream URIs are stored credentials-free and creds are injected only
// at LiveURL() time via url.UserPassword, exactly like the other brand adapters.
//
// ── How this maps onto the CameraBrand contract ──────────────────────────────
// CameraBrand.LiveURL/SnapshotURL are documented as I/O-free, pure URL builders,
// but an ONVIF stream/snapshot URI is only known after GetStreamUri/GetSnapshotUri
// (network I/O in Discover). Since CamDVR is passed by value and CamChannel has no
// slot for adapter-private state, the resolved URIs are kept in an in-memory,
// process-lifetime cache keyed by (dvr.ID, channel) — reading it is not network
// I/O, so the builder methods stay deterministic once Discover has run. This is the
// same trade-off camera_rtspprobe.go makes for its winning template. It does NOT
// survive a restart; onvifCapsJSON/onvifLoadCaps expose the (creds-free) URIs so a
// later WP's discovery handler can persist them in the camera row's `caps` column
// (the plan's "stream/snapshot URIs stored in Caps") and repopulate the cache on
// boot without re-running Discover.
//
// ── Profile → channel mapping ────────────────────────────────────────────────
// ONVIF has no "channel" concept; it has media profiles. Profiles that share a
// VideoSourceConfiguration SourceToken are the same physical input, so they are
// grouped into one CamChannel; within a group the highest-resolution profile is the
// main stream and the lowest is the sub stream — mapping cleanly onto StreamMain/
// StreamSub. (A device that omits SourceToken degrades to one channel per profile;
// that is honest and rare — real NVRs populate it.)
//
// SECURITY: like camera_digest.go, this reaches RFC1918/private DVR hosts through
// the deliberately guard-free camHTTPClient(); it is only ever pointed at
// admin-configured DVR hosts, never a user-supplied URL.

import (
	"context"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

func init() {
	registerBrand("onvif", onvifBrandAdapter{})
}

// ── namespaces / constants ───────────────────────────────────────────────────
const (
	onvifSOAP12NS = "http://www.w3.org/2003/05/soap-envelope"

	// WS-Security (UsernameToken) namespaces + token-profile URIs.
	onvifWSSENS        = "http://docs.oasis-open.org/wss/2004/01/oasis-200401-wss-wssecurity-secext-1.0.xsd"
	onvifWSUNS         = "http://docs.oasis-open.org/wss/2004/01/oasis-200401-wss-wssecurity-utility-1.0.xsd"
	onvifPassProfileNS = "http://docs.oasis-open.org/wss/2004/01/oasis-200401-wss-username-token-profile-1.0"
	onvifMsgSecNS      = "http://docs.oasis-open.org/wss/2004/01/oasis-200401-wss-soap-message-security-1.0"

	// ONVIF media service + common schema namespaces.
	onvifMediaWSDL = "http://www.onvif.org/ver10/media/wsdl"
	onvifSchemaNS  = "http://www.onvif.org/ver10/schema"

	// WS-Discovery (SOAP 1.2 over UDP multicast) namespaces + fixed values.
	onvifWSANS         = "http://schemas.xmlsoap.org/ws/2004/08/addressing"
	onvifWSDNS         = "http://schemas.xmlsoap.org/ws/2005/04/discovery"
	onvifNVTNS         = "http://www.onvif.org/ver10/network/wsdl"
	onvifProbeAction   = "http://schemas.xmlsoap.org/ws/2005/04/discovery/Probe"
	onvifDiscoveryTo   = "urn:schemas-xmlsoap-org:ws:2005:04:discovery"
	onvifMulticastAddr = "239.255.255.250:3702"
	onvifWSDiscPort    = "3702"
)

// onvifBrandAdapter is the CameraBrand implementation for generic ONVIF devices.
type onvifBrandAdapter struct{}

func (onvifBrandAdapter) Name() string { return "onvif" }

// ── in-memory, process-lifetime URI cache (see file header) ──────────────────

// onvifStreamInfo holds one resolved profile's URIs (credentials-free) + geometry.
type onvifStreamInfo struct {
	profileToken string
	streamURI    string // rtsp://… (no creds — injected at LiveURL time)
	snapshotURI  string // http(s)://… (no creds — auth via httpDigestGet)
	width        int
	height       int
}

// onvifChannelInfo is the cached resolution for one CamChannel.
type onvifChannelInfo struct {
	name     string
	endpoint string // media service endpoint that answered GetProfiles
	main     onvifStreamInfo
	sub      onvifStreamInfo // equals main when the device exposes no sub profile
	hasSub   bool
}

var (
	onvifCacheMu sync.RWMutex
	onvifCache   = map[string]map[int]onvifChannelInfo{} // dvr.ID → channel → info
)

func onvifRemember(dvrID string, ch int, info onvifChannelInfo) {
	if dvrID == "" || ch <= 0 {
		return
	}
	onvifCacheMu.Lock()
	defer onvifCacheMu.Unlock()
	if onvifCache[dvrID] == nil {
		onvifCache[dvrID] = map[int]onvifChannelInfo{}
	}
	onvifCache[dvrID][ch] = info
}

func onvifChannelFor(dvrID string, ch int) (onvifChannelInfo, bool) {
	onvifCacheMu.RLock()
	defer onvifCacheMu.RUnlock()
	if m := onvifCache[dvrID]; m != nil {
		info, ok := m[ch]
		return info, ok
	}
	return onvifChannelInfo{}, false
}

// ── URL builders (pure/deterministic given the cache; no network I/O) ─────────

// LiveURL replays the RTSP stream URI Discover resolved for (dvr,ch) at the
// requested quality, injecting credentials via url.UserPassword. On a cache miss
// (pre-discovery, or after a restart with no persisted caps) it returns "" so the
// capture layer's ffmpeg path fails and logs cleanly rather than pulling a wrong
// URL — an ONVIF stream path is device-specific and cannot be guessed.
func (onvifBrandAdapter) LiveURL(dvr CamDVR, ch int, q StreamQuality) string {
	info, ok := onvifChannelFor(dvr.ID, ch)
	if !ok {
		return ""
	}
	uri := info.main.streamURI
	if q == StreamSub && info.hasSub && info.sub.streamURI != "" {
		uri = info.sub.streamURI
	}
	return onvifInjectCreds(uri, dvr.Username, dvr.Password)
}

// SnapshotURL returns the device's HTTP snapshot URI (credentials-free — the
// capture layer authenticates via httpDigestGet's Authorization header, never via
// the URL). ok is false on a cache miss or when the device advertised no snapshot
// URI, so captureSnapshot falls through to the ffmpeg-from-RTSP path.
func (onvifBrandAdapter) SnapshotURL(dvr CamDVR, ch int, q StreamQuality) (string, bool, bool) {
	info, ok := onvifChannelFor(dvr.ID, ch)
	if !ok {
		return "", false, false
	}
	uri := info.main.snapshotURI
	if q == StreamSub && info.hasSub && info.sub.snapshotURI != "" {
		uri = info.sub.snapshotURI
	}
	if strings.TrimSpace(uri) == "" {
		return "", false, false
	}
	return uri, true, true
}

// PlaybackURL: ONVIF time-window playback is Profile G (GetRecordings/GetReplayUri),
// a separate service not implemented in this phase. Return a clean, actionable
// error (never a 500) so capturePlaybackClip surfaces it plainly.
func (onvifBrandAdapter) PlaybackURL(dvr CamDVR, ch int, q StreamQuality, start, end time.Time) (string, error) {
	return "", fmt.Errorf("onvif: time-window playback (Profile G replay) is not supported in this phase; capture a live clip, or set the DVR brand to hikvision/dahua for native playback")
}

// onvifInjectCreds parses an ONVIF-returned URL and injects user:pass via
// url.UserPassword (never string-concatenated — passwords contain @:/?). Returns
// the input unchanged if it cannot be parsed or there is no username to inject.
func onvifInjectCreds(rawURL, user, pass string) string {
	if strings.TrimSpace(rawURL) == "" || user == "" {
		return rawURL
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return rawURL
	}
	u.User = url.UserPassword(user, pass)
	return u.String()
}

// ── Discover (per-DVR channel enumeration) ───────────────────────────────────

// Discover resolves the device's media endpoint, enumerates its profiles, groups
// them into channels, and resolves each channel's main/sub stream + snapshot URIs
// (caching them for the URL builders). All ONVIF discovery is gated behind
// PROXY_CAM_ONVIF_DISCOVERY (default off) — when disabled it returns a clean error
// so the caller can fall back to the RTSP-probe brand. Any device that does not
// answer ONVIF likewise returns a clean error (never a panic/500).
func (a onvifBrandAdapter) Discover(ctx context.Context, cfg config, dvr CamDVR) ([]CamChannel, error) {
	start := time.Now()
	if !cfg.CameraONVIFDiscovery {
		err := fmt.Errorf("ONVIF discovery is disabled; set PROXY_CAM_ONVIF_DISCOVERY=true to enable ONVIF devices")
		camlog("warn", "discover", map[string]any{
			"dvr_id": dvr.ID, "brand": "onvif", "ok": false, "error": err.Error(),
		})
		return nil, err
	}

	timeout := cfg.CameraDiscoverTimeout
	if timeout <= 0 {
		timeout = 20 * time.Second
	}
	dctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	httpPort := cameraDVRHTTPPort(cfg, dvr)
	camlog("info", "discover_start", map[string]any{
		"dvr_id": dvr.ID, "brand": "onvif", "host": dvr.Host, "http_port": httpPort,
	})

	endpoint, profiles, err := onvifResolveProfiles(dctx, dvr, httpPort)
	if err != nil {
		camlog("error", "discover", map[string]any{
			"dvr_id": dvr.ID, "brand": "onvif", "ok": false, "error": err.Error(),
			"latency_ms": time.Since(start).Milliseconds(),
		})
		return nil, err
	}
	camlog("debug", "onvif_profiles", map[string]any{
		"dvr_id": dvr.ID, "endpoint": maskCredsURL(endpoint), "profiles": len(profiles),
	})

	groups := onvifGroupProfiles(profiles)
	var channels []CamChannel
	for i, g := range groups {
		if dctx.Err() != nil {
			break
		}
		ch := i + 1
		info := onvifChannelInfo{name: g.name, endpoint: endpoint}

		mStream, sErr := onvifGetStreamURI(dctx, endpoint, dvr, g.main.Token)
		mSnap, snErr := onvifGetSnapshotURI(dctx, endpoint, dvr, g.main.Token)
		info.main = onvifStreamInfo{
			profileToken: g.main.Token, streamURI: mStream, snapshotURI: mSnap,
			width: g.main.W, height: g.main.H,
		}
		if g.hasSub {
			subStream, _ := onvifGetStreamURI(dctx, endpoint, dvr, g.sub.Token)
			subSnap, _ := onvifGetSnapshotURI(dctx, endpoint, dvr, g.sub.Token)
			info.sub = onvifStreamInfo{
				profileToken: g.sub.Token, streamURI: subStream, snapshotURI: subSnap,
				width: g.sub.W, height: g.sub.H,
			}
			info.hasSub = true
		} else {
			info.sub = info.main
		}

		// A channel with no usable main stream URI is not worth tracking; skip it
		// (the snapshot URI alone can't drive live view / clips) but keep going.
		if strings.TrimSpace(info.main.streamURI) == "" {
			camlog("warn", "onvif_channel_skipped", map[string]any{
				"dvr_id": dvr.ID, "channel": ch, "profile_token": g.main.Token, "ok": false,
				"error": onvifFirstErr("no stream uri", sErr, snErr),
			})
			continue
		}

		onvifRemember(dvr.ID, ch, info)
		channels = append(channels, CamChannel{Channel: ch, Name: g.name, MainW: g.main.W, MainH: g.main.H})
		camlog("debug", "onvif_channel_mapped", map[string]any{
			"dvr_id": dvr.ID, "channel": ch, "profile_token": g.main.Token,
			"stream_uri": maskCredsURL(info.main.streamURI), "snapshot_uri": maskCredsURL(info.main.snapshotURI),
			"has_sub": info.hasSub, "width": g.main.W, "height": g.main.H,
		})
	}

	if len(channels) == 0 {
		err := fmt.Errorf("onvif device answered GetProfiles but no channel yielded a usable stream URI")
		camlog("error", "discover", map[string]any{
			"dvr_id": dvr.ID, "brand": "onvif", "ok": false, "error": err.Error(),
			"latency_ms": time.Since(start).Milliseconds(),
		})
		return nil, err
	}

	camlog("info", "discover", map[string]any{
		"dvr_id": dvr.ID, "brand": "onvif", "ok": true, "channels_found": len(channels),
		"endpoint": maskCredsURL(endpoint), "latency_ms": time.Since(start).Milliseconds(),
	})
	return channels, nil
}

// onvifFirstErr renders a short reason from up to two errors for a log field.
func onvifFirstErr(fallback string, errs ...error) string {
	for _, e := range errs {
		if e != nil {
			return e.Error()
		}
	}
	return fallback
}

// onvifEndpointCandidates lists the media-service paths to try, most-common first.
// ONVIF does not fix the media endpoint path across vendors: strict Profile-S
// devices expose /onvif/Media (or /onvif/media_service) while many consumer cameras
// route every operation through /onvif/device_service. We probe with GetProfiles
// and lock onto the first that answers with ≥1 profile.
func onvifEndpointCandidates(host string, port int) []string {
	base := "http://" + net.JoinHostPort(host, strconv.Itoa(port))
	return []string{
		base + "/onvif/Media",
		base + "/onvif/media_service",
		base + "/onvif/device_service",
		base + "/onvif/media",
		base + "/onvif/Media_service",
	}
}

// onvifResolveProfiles finds the working media endpoint by trying GetProfiles
// against each candidate, returning the first endpoint that yields profiles.
func onvifResolveProfiles(ctx context.Context, dvr CamDVR, httpPort int) (string, []onvifProfile, error) {
	var reasons []string
	for _, endpoint := range onvifEndpointCandidates(dvr.Host, httpPort) {
		if ctx.Err() != nil {
			break
		}
		profiles, err := onvifGetProfiles(ctx, endpoint, dvr)
		if err == nil && len(profiles) > 0 {
			return endpoint, profiles, nil
		}
		if err != nil {
			reasons = append(reasons, endpoint+" → "+err.Error())
		} else {
			reasons = append(reasons, endpoint+" → 0 profiles")
		}
	}
	return "", nil, fmt.Errorf("no ONVIF media endpoint answered GetProfiles on %s: %s",
		dvr.Host, truncateString(strings.Join(reasons, " | "), 400))
}

// ── profile grouping ─────────────────────────────────────────────────────────

type onvifProfileRef struct {
	Token string
	Name  string
	W, H  int
}

type onvifProfileGroup struct {
	name   string
	main   onvifProfileRef
	sub    onvifProfileRef
	hasSub bool
}

// onvifGroupProfiles groups profiles by shared VideoSourceConfiguration SourceToken
// (same physical input), preserving first-appearance order, and within each group
// picks the highest-resolution profile as main and the lowest as sub. Profiles that
// lack a SourceToken each become their own single-stream channel.
func onvifGroupProfiles(profiles []onvifProfile) []onvifProfileGroup {
	order := make([]string, 0, len(profiles))
	byKey := map[string][]onvifProfile{}
	for _, p := range profiles {
		key := strings.TrimSpace(p.VideoSource.SourceToken)
		if key == "" {
			key = "\x00" + p.Token // isolate profiles with no source token
		}
		if _, seen := byKey[key]; !seen {
			order = append(order, key)
		}
		byKey[key] = append(byKey[key], p)
	}

	groups := make([]onvifProfileGroup, 0, len(order))
	for _, key := range order {
		ps := byKey[key]
		// Highest resolution first (stable so equal-area profiles keep device order).
		sort.SliceStable(ps, func(i, j int) bool { return onvifArea(ps[i]) > onvifArea(ps[j]) })
		g := onvifProfileGroup{name: onvifGroupName(ps), main: onvifRef(ps[0])}
		if len(ps) > 1 {
			g.sub = onvifRef(ps[len(ps)-1])
			g.hasSub = true
		}
		groups = append(groups, g)
	}
	return groups
}

func onvifArea(p onvifProfile) int {
	return p.VideoEncoder.Resolution.Width * p.VideoEncoder.Resolution.Height
}

func onvifRef(p onvifProfile) onvifProfileRef {
	return onvifProfileRef{
		Token: p.Token, Name: p.Name,
		W: p.VideoEncoder.Resolution.Width, H: p.VideoEncoder.Resolution.Height,
	}
}

func onvifGroupName(ps []onvifProfile) string {
	for _, p := range ps {
		if n := strings.TrimSpace(p.Name); n != "" {
			return n
		}
	}
	return ""
}

// ── SOAP media operations ────────────────────────────────────────────────────

// onvifSecurityHeader builds a WS-Security UsernameToken header with a
// PasswordDigest = base64(sha1(nonce + created + password)). A fresh random nonce
// and current UTC Created are generated per call; the exact Created string that is
// hashed is the same one placed in the XML (byte-identical), which the digest
// requires. The password never leaves this function except as its SHA-1 digest.
func onvifSecurityHeader(user, pass string) string {
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		// Deterministic fallback keeps the request well-formed if the RNG fails.
		for i := range nonce {
			nonce[i] = byte(time.Now().UnixNano() >> (uint(i) % 8))
		}
	}
	created := time.Now().UTC().Format("2006-01-02T15:04:05.000Z")

	h := sha1.New()
	h.Write(nonce)
	h.Write([]byte(created))
	h.Write([]byte(pass))
	digest := base64.StdEncoding.EncodeToString(h.Sum(nil))
	nonceB64 := base64.StdEncoding.EncodeToString(nonce)

	return fmt.Sprintf(`<wsse:Security s:mustUnderstand="1" xmlns:wsse=%q xmlns:wsu=%q>`+
		`<wsse:UsernameToken>`+
		`<wsse:Username>%s</wsse:Username>`+
		`<wsse:Password Type="%s#PasswordDigest">%s</wsse:Password>`+
		`<wsse:Nonce EncodingType="%s#Base64Binary">%s</wsse:Nonce>`+
		`<wsu:Created>%s</wsu:Created>`+
		`</wsse:UsernameToken></wsse:Security>`,
		onvifWSSENS, onvifWSUNS, onvifXMLEscape(user), onvifPassProfileNS, digest,
		onvifMsgSecNS, nonceB64, created)
}

// onvifMediaEnvelope wraps a security header + body element into a SOAP 1.2 envelope.
func onvifMediaEnvelope(security, body string) string {
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>`+
		`<s:Envelope xmlns:s=%q><s:Header>%s</s:Header><s:Body>%s</s:Body></s:Envelope>`,
		onvifSOAP12NS, security, body)
}

// onvifSOAPCall POSTs a SOAP 1.2 request to endpoint and returns the response body.
// It uses the guard-free camHTTPClient (private DVR hosts are the target).
func onvifSOAPCall(ctx context.Context, endpoint, action, xmlBody string) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(xmlBody))
	if err != nil {
		return nil, 0, err
	}
	ct := "application/soap+xml; charset=utf-8"
	if action != "" {
		ct += `; action="` + action + `"`
	}
	req.Header.Set("Content-Type", ct)
	resp, err := camHTTPClient().Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	body, rerr := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if rerr != nil {
		return body, resp.StatusCode, fmt.Errorf("read onvif response: %w", rerr)
	}
	return body, resp.StatusCode, nil
}

// onvifGetProfiles issues Media GetProfiles and returns the parsed profile list.
func onvifGetProfiles(ctx context.Context, endpoint string, dvr CamDVR) ([]onvifProfile, error) {
	start := time.Now()
	inner := fmt.Sprintf(`<GetProfiles xmlns=%q/>`, onvifMediaWSDL)
	env := onvifMediaEnvelope(onvifSecurityHeader(dvr.Username, dvr.Password), inner)

	body, status, err := onvifSOAPCall(ctx, endpoint, onvifMediaWSDL+"/GetProfiles", env)
	if err != nil {
		camlog("debug", "onvif_get_profiles", map[string]any{
			"dvr_id": dvr.ID, "endpoint": maskCredsURL(endpoint), "ok": false,
			"error": err.Error(), "latency_ms": time.Since(start).Milliseconds(),
		})
		return nil, err
	}

	var e onvifProfilesEnvelope
	if uerr := xml.Unmarshal(body, &e); uerr != nil {
		return nil, fmt.Errorf("parse GetProfiles (HTTP %d): %w; body: %s",
			status, uerr, truncateString(string(body), 160))
	}
	if msg := e.Body.Fault.message(); msg != "" {
		return nil, fmt.Errorf("GetProfiles SOAP fault (HTTP %d): %s", status, msg)
	}
	profiles := e.Body.Resp.Profiles
	if len(profiles) == 0 {
		return nil, fmt.Errorf("GetProfiles returned no profiles (HTTP %d)", status)
	}
	camlog("debug", "onvif_get_profiles", map[string]any{
		"dvr_id": dvr.ID, "endpoint": maskCredsURL(endpoint), "ok": true,
		"profiles": len(profiles), "http_status": status, "latency_ms": time.Since(start).Milliseconds(),
	})
	return profiles, nil
}

// onvifGetStreamURI issues Media GetStreamUri (RTP-Unicast / RTSP) for a profile and
// returns the credentials-free RTSP URI the device advertises.
func onvifGetStreamURI(ctx context.Context, endpoint string, dvr CamDVR, profileToken string) (string, error) {
	start := time.Now()
	inner := fmt.Sprintf(`<GetStreamUri xmlns=%q>`+
		`<StreamSetup>`+
		`<Stream xmlns=%q>RTP-Unicast</Stream>`+
		`<Transport xmlns=%q><Protocol>RTSP</Protocol></Transport>`+
		`</StreamSetup>`+
		`<ProfileToken>%s</ProfileToken>`+
		`</GetStreamUri>`,
		onvifMediaWSDL, onvifSchemaNS, onvifSchemaNS, onvifXMLEscape(profileToken))
	env := onvifMediaEnvelope(onvifSecurityHeader(dvr.Username, dvr.Password), inner)

	body, status, err := onvifSOAPCall(ctx, endpoint, onvifMediaWSDL+"/GetStreamUri", env)
	if err != nil {
		camlog("debug", "onvif_get_stream_uri", map[string]any{
			"dvr_id": dvr.ID, "profile_token": profileToken, "ok": false,
			"error": err.Error(), "latency_ms": time.Since(start).Milliseconds(),
		})
		return "", err
	}
	uri, perr := onvifParseMediaURI(body, status, "GetStreamUri")
	camlog("debug", "onvif_get_stream_uri", map[string]any{
		"dvr_id": dvr.ID, "profile_token": profileToken, "ok": perr == nil,
		"uri": maskCredsURL(uri), "http_status": status, "latency_ms": time.Since(start).Milliseconds(),
		"error": onvifErrStr(perr),
	})
	return uri, perr
}

// onvifGetSnapshotURI issues Media GetSnapshotUri for a profile and returns the
// credentials-free HTTP snapshot URI (auth is applied later by httpDigestGet).
func onvifGetSnapshotURI(ctx context.Context, endpoint string, dvr CamDVR, profileToken string) (string, error) {
	start := time.Now()
	inner := fmt.Sprintf(`<GetSnapshotUri xmlns=%q><ProfileToken>%s</ProfileToken></GetSnapshotUri>`,
		onvifMediaWSDL, onvifXMLEscape(profileToken))
	env := onvifMediaEnvelope(onvifSecurityHeader(dvr.Username, dvr.Password), inner)

	body, status, err := onvifSOAPCall(ctx, endpoint, onvifMediaWSDL+"/GetSnapshotUri", env)
	if err != nil {
		camlog("debug", "onvif_get_snapshot_uri", map[string]any{
			"dvr_id": dvr.ID, "profile_token": profileToken, "ok": false,
			"error": err.Error(), "latency_ms": time.Since(start).Milliseconds(),
		})
		return "", err
	}
	uri, perr := onvifParseMediaURI(body, status, "GetSnapshotUri")
	camlog("debug", "onvif_get_snapshot_uri", map[string]any{
		"dvr_id": dvr.ID, "profile_token": profileToken, "ok": perr == nil,
		"uri": maskCredsURL(uri), "http_status": status, "latency_ms": time.Since(start).Milliseconds(),
		"error": onvifErrStr(perr),
	})
	return uri, perr
}

// onvifParseMediaURI extracts MediaUri>Uri from a GetStreamUri/GetSnapshotUri
// response (both share the same MediaUri shape), turning a SOAP fault or an empty
// URI into a descriptive error.
func onvifParseMediaURI(body []byte, status int, op string) (string, error) {
	var e onvifMediaURIEnvelope
	if uerr := xml.Unmarshal(body, &e); uerr != nil {
		return "", fmt.Errorf("parse %s (HTTP %d): %w; body: %s",
			op, status, uerr, truncateString(string(body), 160))
	}
	if msg := e.Body.Fault.message(); msg != "" {
		return "", fmt.Errorf("%s SOAP fault (HTTP %d): %s", op, status, msg)
	}
	uri := strings.TrimSpace(e.Body.Stream.MediaUri.Uri)
	if uri == "" {
		uri = strings.TrimSpace(e.Body.Snapshot.MediaUri.Uri)
	}
	if uri == "" {
		return "", fmt.Errorf("%s returned an empty URI (HTTP %d)", op, status)
	}
	return uri, nil
}

func onvifErrStr(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// ── WS-Discovery (LAN device scan) ───────────────────────────────────────────

// onvifDevice is one WS-Discovery ProbeMatch: a device's endpoint reference and
// its service addresses (XAddrs — typically the device_service URL).
type onvifDevice struct {
	Address string
	Types   string
	Scopes  string
	XAddrs  []string
}

// onvifDiscover performs WS-Discovery: it sends a Probe to the 239.255.255.250:3702
// multicast group AND a unicast Probe to each provided host (multicast is commonly
// firewalled/blocked across VLANs, so the unicast probe to a known host is the
// reliable path), then collects ProbeMatch responses for a short window. Gated
// behind PROXY_CAM_ONVIF_DISCOVERY; returns a clean error when disabled.
func onvifDiscover(ctx context.Context, cfg config, unicastHosts ...string) ([]onvifDevice, error) {
	if !cfg.CameraONVIFDiscovery {
		return nil, fmt.Errorf("ONVIF discovery is disabled; set PROXY_CAM_ONVIF_DISCOVERY=true to scan the LAN")
	}
	start := time.Now()

	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero, Port: 0})
	if err != nil {
		camlog("error", "onvif_wsdiscovery", map[string]any{"ok": false, "error": err.Error()})
		return nil, fmt.Errorf("ws-discovery listen: %w", err)
	}
	defer conn.Close()

	probe := []byte(onvifProbeEnvelope(onvifUUID()))
	var sentMulticast bool
	if maddr, merr := net.ResolveUDPAddr("udp4", onvifMulticastAddr); merr == nil {
		if _, werr := conn.WriteToUDP(probe, maddr); werr == nil {
			sentMulticast = true
		} else {
			camlog("debug", "onvif_wsdiscovery_send", map[string]any{
				"target": "multicast", "ok": false, "error": werr.Error(),
			})
		}
	}
	var unicastSent []string
	for _, h := range unicastHosts {
		h = strings.TrimSpace(h)
		if h == "" {
			continue
		}
		ua, uerr := net.ResolveUDPAddr("udp4", net.JoinHostPort(h, onvifWSDiscPort))
		if uerr != nil {
			continue
		}
		if _, werr := conn.WriteToUDP(probe, ua); werr == nil {
			unicastSent = append(unicastSent, h)
		} else {
			camlog("debug", "onvif_wsdiscovery_send", map[string]any{
				"target": h, "ok": false, "error": werr.Error(),
			})
		}
	}

	camlog("info", "onvif_wsdiscovery_start", map[string]any{
		"multicast": sentMulticast, "unicast_hosts": unicastSent, "multicast_addr": onvifMulticastAddr,
	})

	// Collection window: bounded, short, and clamped independent of the (larger)
	// per-device discover timeout — WS-Discovery replies arrive within ~1–2s.
	window := cfg.CameraDiscoverTimeout
	if window <= 0 || window > 6*time.Second {
		window = 4 * time.Second
	}
	if window < 2*time.Second {
		window = 2 * time.Second
	}
	overall := time.Now().Add(window)

	buf := make([]byte, 65535)
	seen := map[string]bool{}
	var devices []onvifDevice
	for {
		if ctx.Err() != nil {
			break
		}
		now := time.Now()
		if !now.Before(overall) {
			break
		}
		readWindow := overall.Sub(now)
		if readWindow > 800*time.Millisecond {
			readWindow = 800 * time.Millisecond
		}
		_ = conn.SetReadDeadline(now.Add(readWindow))
		n, src, rerr := conn.ReadFromUDP(buf)
		if rerr != nil {
			if ne, ok := rerr.(net.Error); ok && ne.Timeout() {
				continue // window slice elapsed; loop re-checks overall/ctx
			}
			break
		}
		for _, d := range parseProbeMatches(buf[:n]) {
			key := d.Address
			if key == "" {
				key = strings.Join(d.XAddrs, ",")
			}
			if key == "" || seen[key] {
				continue
			}
			seen[key] = true
			devices = append(devices, d)
			maskedX := make([]string, len(d.XAddrs))
			for i, x := range d.XAddrs {
				maskedX[i] = maskCredsURL(x)
			}
			camlog("debug", "onvif_wsdiscovery_match", map[string]any{
				"address": d.Address, "xaddrs": maskedX, "src": src.String(),
			})
		}
	}

	camlog("info", "onvif_wsdiscovery", map[string]any{
		"ok": true, "devices": len(devices), "latency_ms": time.Since(start).Milliseconds(),
	})
	return devices, nil
}

// onvifProbeEnvelope builds a WS-Discovery Probe for NetworkVideoTransmitter devices.
func onvifProbeEnvelope(messageID string) string {
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>`+
		`<e:Envelope xmlns:e=%q xmlns:w=%q xmlns:d=%q xmlns:dn=%q>`+
		`<e:Header>`+
		`<w:MessageID>%s</w:MessageID>`+
		`<w:To e:mustUnderstand="true">%s</w:To>`+
		`<w:Action e:mustUnderstand="true">%s</w:Action>`+
		`</e:Header>`+
		`<e:Body><d:Probe><d:Types>dn:NetworkVideoTransmitter</d:Types></d:Probe></e:Body>`+
		`</e:Envelope>`,
		onvifSOAP12NS, onvifWSANS, onvifWSDNS, onvifNVTNS,
		messageID, onvifDiscoveryTo, onvifProbeAction)
}

// parseProbeMatches decodes a WS-Discovery ProbeMatches datagram into devices.
// Namespace-tolerant (matches by local element name), and splits the
// space-separated XAddrs list into individual service URLs.
func parseProbeMatches(raw []byte) []onvifDevice {
	var env onvifProbeMatchesEnvelope
	if err := xml.Unmarshal(raw, &env); err != nil {
		return nil
	}
	var out []onvifDevice
	for _, m := range env.Body.ProbeMatches.Match {
		d := onvifDevice{
			Address: strings.TrimSpace(m.EndpointReference.Address),
			Types:   strings.TrimSpace(m.Types),
			Scopes:  strings.TrimSpace(m.Scopes),
		}
		for _, x := range strings.Fields(m.XAddrs) {
			if x = strings.TrimSpace(x); x != "" {
				d.XAddrs = append(d.XAddrs, x)
			}
		}
		out = append(out, d)
	}
	return out
}

// onvifUUID returns a random RFC-4122 v4 "uuid:…" string for the Probe MessageID.
func onvifUUID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("uuid:%016x", time.Now().UnixNano())
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("uuid:%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// ── caps (de)serialization: forward-compat with the camera row's `caps` column ──

type onvifCapsStream struct {
	Profile  string `json:"profile,omitempty"`
	Stream   string `json:"stream,omitempty"`
	Snapshot string `json:"snapshot,omitempty"`
	W        int    `json:"w,omitempty"`
	H        int    `json:"h,omitempty"`
}

type onvifCaps struct {
	Brand    string          `json:"brand"`
	Endpoint string          `json:"endpoint,omitempty"`
	Name     string          `json:"name,omitempty"`
	Main     onvifCapsStream `json:"main"`
	Sub      onvifCapsStream `json:"sub,omitempty"`
	HasSub   bool            `json:"has_sub"`
}

// onvifCapsJSON serializes the discovered, credentials-free URIs for (dvr,ch) so a
// later WP's discovery handler can persist them in the camera row's `caps` column.
// Returns "" on a cache miss. Storing these is safe — no password is present.
func onvifCapsJSON(dvrID string, ch int) string {
	info, ok := onvifChannelFor(dvrID, ch)
	if !ok {
		return ""
	}
	c := onvifCaps{
		Brand: "onvif", Endpoint: info.endpoint, Name: info.name, HasSub: info.hasSub,
		Main: onvifCapsStream{
			Profile: info.main.profileToken, Stream: info.main.streamURI,
			Snapshot: info.main.snapshotURI, W: info.main.width, H: info.main.height,
		},
	}
	if info.hasSub {
		c.Sub = onvifCapsStream{
			Profile: info.sub.profileToken, Stream: info.sub.streamURI,
			Snapshot: info.sub.snapshotURI, W: info.sub.width, H: info.sub.height,
		}
	}
	b, err := json.Marshal(c)
	if err != nil {
		return ""
	}
	return string(b)
}

// onvifLoadCaps repopulates the in-memory URI cache from a persisted `caps` blob,
// so LiveURL/SnapshotURL work after a restart without re-running Discover. A later
// WP can call this when loading cameras whose brand is "onvif". Unknown/foreign
// caps JSON is ignored.
func onvifLoadCaps(dvrID string, ch int, capsJSON string) {
	capsJSON = strings.TrimSpace(capsJSON)
	if dvrID == "" || ch <= 0 || capsJSON == "" {
		return
	}
	var c onvifCaps
	if err := json.Unmarshal([]byte(capsJSON), &c); err != nil || c.Brand != "onvif" {
		return
	}
	if strings.TrimSpace(c.Main.Stream) == "" {
		return
	}
	info := onvifChannelInfo{
		name: c.Name, endpoint: c.Endpoint, hasSub: c.HasSub,
		main: onvifStreamInfo{
			profileToken: c.Main.Profile, streamURI: c.Main.Stream,
			snapshotURI: c.Main.Snapshot, width: c.Main.W, height: c.Main.H,
		},
	}
	if c.HasSub && strings.TrimSpace(c.Sub.Stream) != "" {
		info.sub = onvifStreamInfo{
			profileToken: c.Sub.Profile, streamURI: c.Sub.Stream,
			snapshotURI: c.Sub.Snapshot, width: c.Sub.W, height: c.Sub.H,
		}
	} else {
		info.sub = info.main
		info.hasSub = false
	}
	onvifRemember(dvrID, ch, info)
}

// ── XML structs (namespace-tolerant: matched by local element name) ───────────

// onvifProfile is one <Profiles> element from a GetProfilesResponse.
type onvifProfile struct {
	Token       string `xml:"token,attr"`
	Name        string `xml:"Name"`
	VideoSource struct {
		Token       string `xml:"token,attr"`
		SourceToken string `xml:"SourceToken"`
	} `xml:"VideoSourceConfiguration"`
	VideoEncoder struct {
		Token      string `xml:"token,attr"`
		Resolution struct {
			Width  int `xml:"Width"`
			Height int `xml:"Height"`
		} `xml:"Resolution"`
	} `xml:"VideoEncoderConfiguration"`
}

type onvifProfilesEnvelope struct {
	Body struct {
		Resp struct {
			Profiles []onvifProfile `xml:"Profiles"`
		} `xml:"GetProfilesResponse"`
		Fault onvifFault `xml:"Fault"`
	} `xml:"Body"`
}

type onvifMediaUri struct {
	Uri string `xml:"Uri"`
}

// onvifMediaURIEnvelope covers both GetStreamUriResponse and GetSnapshotUriResponse
// (identical MediaUri shape) — whichever is present carries a non-empty Uri.
type onvifMediaURIEnvelope struct {
	Body struct {
		Stream struct {
			MediaUri onvifMediaUri `xml:"MediaUri"`
		} `xml:"GetStreamUriResponse"`
		Snapshot struct {
			MediaUri onvifMediaUri `xml:"MediaUri"`
		} `xml:"GetSnapshotUriResponse"`
		Fault onvifFault `xml:"Fault"`
	} `xml:"Body"`
}

// onvifFault parses both SOAP 1.2 (<Code><Value>/<Subcode>, <Reason><Text>) and
// SOAP 1.1 (<faultstring>) fault shapes.
type onvifFault struct {
	Code struct {
		Value   string `xml:"Value"`
		Subcode struct {
			Value string `xml:"Value"`
		} `xml:"Subcode"`
	} `xml:"Code"`
	Reason struct {
		Text string `xml:"Text"`
	} `xml:"Reason"`
	FaultString string `xml:"faultstring"`
}

// message returns a human-readable fault string, or "" when no fault is present.
func (f onvifFault) message() string {
	switch {
	case strings.TrimSpace(f.Reason.Text) != "":
		return strings.TrimSpace(f.Reason.Text)
	case strings.TrimSpace(f.Code.Subcode.Value) != "":
		return strings.TrimSpace(f.Code.Subcode.Value)
	case strings.TrimSpace(f.Code.Value) != "":
		return strings.TrimSpace(f.Code.Value)
	case strings.TrimSpace(f.FaultString) != "":
		return strings.TrimSpace(f.FaultString)
	}
	return ""
}

type onvifProbeMatchesEnvelope struct {
	Body struct {
		ProbeMatches struct {
			Match []struct {
				EndpointReference struct {
					Address string `xml:"Address"`
				} `xml:"EndpointReference"`
				Types  string `xml:"Types"`
				Scopes string `xml:"Scopes"`
				XAddrs string `xml:"XAddrs"`
			} `xml:"ProbeMatch"`
		} `xml:"ProbeMatches"`
	} `xml:"Body"`
}

// onvifXMLEscape escapes text/attribute content placed into the hand-built SOAP
// (Username, ProfileToken). Passwords are never emitted (only their SHA-1 digest).
var onvifXMLEscaper = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&apos;")

func onvifXMLEscape(s string) string { return onvifXMLEscaper.Replace(s) }
