package main

// camera_hikvision_playback_test.go — the ISAPI recorded-footage transport.
//
// The behaviours worth pinning down are the ones that were expensive to
// discover on the live units: the device's "Z"-suffixed timestamps are its OWN
// local clock (not UTC), the download endpoint needs the device's name=/size=
// tokens echoed back, and it always streams from the segment start so the caller
// must cap by byte offset rather than by asking for a shorter window.

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// hikTestServer wires a CamDVR to an httptest server standing in for the device.
func hikTestServer(t *testing.T, h http.HandlerFunc) (CamDVR, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse test server url: %v", err)
	}
	port, _ := strconv.Atoi(u.Port())
	return CamDVR{
		ID: "dvr_test", Brand: "hikvision", Host: u.Hostname(), HTTPPort: port,
		Username: "admin", Password: "pw", Timezone: "Asia/Amman",
	}, srv
}

const hikSearchBody = `<?xml version="1.0" encoding="UTF-8"?>
<CMSearchResult version="2.0" xmlns="http://www.hikvision.com/ver20/XMLSchema">
<searchID>C7B4A1E2-0000-1000-8000-000000000101</searchID>
<responseStatus>true</responseStatus>
<numOfMatches>1</numOfMatches>
<matchList><searchMatchItem>
<trackID>101</trackID>
<timeSpan><startTime>2026-07-01T08:32:14Z</startTime><endTime>2026-07-01T15:23:41Z</endTime></timeSpan>
<mediaSegmentDescriptor><contentType>video</contentType><codecType>H.265</codecType>
<playbackURI>rtsp://10.0.0.5:80/Streaming/tracks/101/?starttime=20260701T083214Z&amp;endtime=20260701T152341Z&amp;name=00000004650000000&amp;size=1063889248</playbackURI>
</mediaSegmentDescriptor>
</searchMatchItem></matchList>
</CMSearchResult>`

func TestHikSearchSegmentsReadsDeviceLocalTimes(t *testing.T) {
	var gotBody string
	dvr, _ := hikTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/ISAPI/ContentMgmt/search" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		_, _ = w.Write([]byte(hikSearchBody))
	})

	from := time.Date(2026, 7, 1, 7, 0, 0, 0, time.UTC)  // 10:00 Amman
	to := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)   // 15:00 Amman
	segs, err := hikSearchSegments(t.Context(), config{}, dvr, 1, from, to)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	// The request must be phrased in the DVR's clock, not ours.
	if !strings.Contains(gotBody, "<startTime>2026-07-01T10:00:00Z</startTime>") {
		t.Errorf("request start not in DVR-local time:\n%s", gotBody)
	}
	if !strings.Contains(gotBody, "<trackID>101</trackID>") {
		t.Errorf("request track = channel*100+1 expected:\n%s", gotBody)
	}
	if len(segs) != 1 {
		t.Fatalf("got %d segments, want 1", len(segs))
	}
	s := segs[0]
	loc, _ := time.LoadLocation("Asia/Amman")
	wantStart := time.Date(2026, 7, 1, 8, 32, 14, 0, loc)
	if !s.Start.Equal(wantStart) {
		t.Errorf("Start = %s, want %s (the Z is the device's local clock, not UTC)",
			s.Start, wantStart)
	}
	if s.Track != 101 {
		t.Errorf("Track = %d, want 101", s.Track)
	}
	if s.Name != "00000004650000000" || s.Size != 1063889248 {
		t.Errorf("name/size = %q/%d, want the device's own tokens", s.Name, s.Size)
	}
}

func TestHikURITokensToleratesEscapedAmpersands(t *testing.T) {
	name, size := hikURITokens(
		"rtsp://h/Streaming/tracks/101/?starttime=x&amp;endtime=y&amp;name=N1&amp;size=42")
	if name != "N1" || size != 42 {
		t.Errorf("got %q/%d, want N1/42", name, size)
	}
	if n, s := hikURITokens("rtsp://h/no/query"); n != "" || s != 0 {
		t.Errorf("query-less URI = %q/%d, want empty", n, s)
	}
}

func TestHikSegmentByteOffsetScalesWithTime(t *testing.T) {
	loc, _ := time.LoadLocation("Asia/Amman")
	const size = int64(400 << 20)
	seg := hikRecordSegment{
		Start: time.Date(2026, 7, 1, 8, 0, 0, 0, loc),
		End:   time.Date(2026, 7, 1, 12, 0, 0, 0, loc), // 4h
		Size:  size,
	}
	// Halfway through the segment => about half the bytes, plus slack, never more
	// than the whole segment.
	mid := hikSegmentByteOffset(seg, time.Date(2026, 7, 1, 10, 0, 0, 0, loc))
	if mid < size/2 || mid > size*13/20 {
		t.Errorf("halfway offset = %d, want ~half of %d plus slack", mid, size)
	}
	// A segment smaller than the 1MB decode floor must never be over-requested.
	tiny := hikRecordSegment{Start: seg.Start, End: seg.End, Size: 4000}
	if got := hikSegmentByteOffset(tiny, seg.Start.Add(time.Minute)); got > tiny.Size {
		t.Errorf("tiny-segment offset = %d, want <= %d", got, tiny.Size)
	}
	if got := hikSegmentByteOffset(seg, seg.End.Add(time.Hour)); got != seg.Size {
		t.Errorf("past-end offset = %d, want the full size %d", got, seg.Size)
	}
	if got := hikSegmentByteOffset(seg, seg.Start.Add(-time.Hour)); got <= 0 {
		t.Errorf("before-start offset = %d, want a positive floor", got)
	}
	// Unknown size means "no cap" so the caller falls back to the hard ceiling.
	if got := hikSegmentByteOffset(hikRecordSegment{Start: seg.Start, End: seg.End}, seg.End); got != 0 {
		t.Errorf("unknown-size offset = %d, want 0 (no cap)", got)
	}
}

func TestHikDownloadSegmentStopsAtByteCap(t *testing.T) {
	payload := strings.Repeat("v", 5<<20)
	var gotURI string
	dvr, _ := hikTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotURI = string(b)
		_, _ = io.WriteString(w, payload)
	})
	dest := filepath.Join(t.TempDir(), "seg.mp4")
	seg := hikRecordSegment{Track: 101, Name: "N1", Size: 5 << 20,
		Start: time.Now().Add(-time.Hour), End: time.Now()}

	n, err := hikDownloadSegment(t.Context(), config{}, dvr, seg, dest, 1<<20)
	if err != nil {
		t.Fatalf("download: %v", err)
	}
	if n != 1<<20 {
		t.Errorf("wrote %d bytes, want the 1MB cap", n)
	}
	fi, serr := os.Stat(dest)
	if serr != nil || fi.Size() != 1<<20 {
		t.Errorf("file size = %v, want 1MB", fi)
	}
	// The device rejects a playbackURI without its own name/size tokens.
	if !strings.Contains(gotURI, "name=N1") || !strings.Contains(gotURI, "size=5242880") {
		t.Errorf("download URI missing device tokens:\n%s", gotURI)
	}
	if !strings.Contains(gotURI, "&amp;") {
		t.Errorf("playbackURI must be XML-escaped inside the request body:\n%s", gotURI)
	}
}

func TestHikDownloadSegmentTinyPayloadIsNoRecording(t *testing.T) {
	dvr, _ := hikTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "nope")
	})
	dest := filepath.Join(t.TempDir(), "seg.mp4")
	seg := hikRecordSegment{Track: 101, Name: "N", Size: 100,
		Start: time.Now().Add(-time.Hour), End: time.Now()}
	if _, err := hikDownloadSegment(t.Context(), config{}, dvr, seg, dest, 0); !errors.Is(err, errNoRecording) {
		t.Errorf("err = %v, want errNoRecording for a truncated payload", err)
	}
}

func TestHikDownloadSegmentSurfacesHTTPStatus(t *testing.T) {
	dvr, _ := hikTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
	})
	dest := filepath.Join(t.TempDir(), "seg.mp4")
	seg := hikRecordSegment{Track: 101, Start: time.Now().Add(-time.Hour), End: time.Now()}
	_, err := hikDownloadSegment(t.Context(), config{}, dvr, seg, dest, 0)
	if err == nil || !strings.Contains(err.Error(), "400") {
		t.Errorf("err = %v, want the 400 surfaced", err)
	}
}

// The digest handshake replays the request after the 401; a POST must resend its
// body, or the device sees an empty search/download request.
func TestHikISAPIPostReplaysBodyThroughDigestAuth(t *testing.T) {
	var bodies []string
	dvr, _ := hikTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(b))
		if r.Header.Get("Authorization") == "" {
			w.Header().Set("WWW-Authenticate",
				`Digest realm="DVR", nonce="abc123", qop="auth", algorithm=MD5`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(hikSearchBody))
	})
	segs, err := hikSearchSegments(t.Context(), config{}, dvr, 1,
		time.Now().Add(-time.Hour), time.Now())
	if err != nil {
		t.Fatalf("search through digest: %v", err)
	}
	if len(segs) != 1 {
		t.Fatalf("got %d segments, want 1", len(segs))
	}
	if len(bodies) != 2 {
		t.Fatalf("saw %d requests, want 2 (challenge + authenticated retry)", len(bodies))
	}
	if bodies[1] == "" || bodies[0] != bodies[1] {
		t.Errorf("authenticated retry body = %q, want the original request replayed", bodies[1])
	}
}

func TestHikPlaybackModeCacheRoundTripAndReset(t *testing.T) {
	hikPlaybackModeReset()
	t.Cleanup(hikPlaybackModeReset)
	if got := hikPlaybackModeGet("dvr_x"); got != "" {
		t.Errorf("unknown dvr = %q, want empty", got)
	}
	hikPlaybackModeSet("dvr_x", hikPlaybackISAPI)
	if got := hikPlaybackModeGet("dvr_x"); got != hikPlaybackISAPI {
		t.Errorf("mode = %q, want %q", got, hikPlaybackISAPI)
	}
	// An entry older than the TTL must not be trusted — a device can be swapped
	// behind the same DVR id.
	hikPlaybackModeMu.Lock()
	hikPlaybackModes["dvr_x"] = hikPlaybackModeEntry{
		mode: hikPlaybackISAPI, at: time.Now().Add(-hikPlaybackModeTTL - time.Minute)}
	hikPlaybackModeMu.Unlock()
	if got := hikPlaybackModeGet("dvr_x"); got != "" {
		t.Errorf("expired mode = %q, want empty", got)
	}
	hikPlaybackModeSet("dvr_y", hikPlaybackRTSP)
	hikPlaybackModeReset()
	if got := hikPlaybackModeGet("dvr_y"); got != "" {
		t.Errorf("after reset = %q, want empty", got)
	}
}

func TestCamPlaybackSupportsISAPIOnlyForHikvisionWithHost(t *testing.T) {
	cases := []struct {
		name string
		dvr  CamDVR
		want bool
	}{
		{"hikvision", CamDVR{Brand: "hikvision", Host: "10.0.0.1"}, true},
		{"mixed case", CamDVR{Brand: "HikVision", Host: "10.0.0.1"}, true},
		{"dahua", CamDVR{Brand: "dahua", Host: "10.0.0.1"}, false},
		{"no host", CamDVR{Brand: "hikvision"}, false},
	}
	for _, c := range cases {
		if got := camPlaybackSupportsISAPI(c.dvr); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

func TestHikSearchSegmentsSortsAndSkipsBadSpans(t *testing.T) {
	body := `<CMSearchResult><matchList>` +
		fmt.Sprintf(`<searchMatchItem><trackID>101</trackID><timeSpan>
<startTime>2026-07-01T12:00:00Z</startTime><endTime>2026-07-01T13:00:00Z</endTime></timeSpan>
<mediaSegmentDescriptor><playbackURI>rtsp://h/?name=b&amp;size=2</playbackURI></mediaSegmentDescriptor></searchMatchItem>`) +
		`<searchMatchItem><trackID>101</trackID><timeSpan>
<startTime>2026-07-01T08:00:00Z</startTime><endTime>2026-07-01T09:00:00Z</endTime></timeSpan>
<mediaSegmentDescriptor><playbackURI>rtsp://h/?name=a&amp;size=1</playbackURI></mediaSegmentDescriptor></searchMatchItem>` +
		// inverted span — must be dropped rather than produce a negative duration
		`<searchMatchItem><trackID>101</trackID><timeSpan>
<startTime>2026-07-01T15:00:00Z</startTime><endTime>2026-07-01T14:00:00Z</endTime></timeSpan></searchMatchItem>` +
		`</matchList></CMSearchResult>`
	dvr, _ := hikTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, body)
	})
	segs, err := hikSearchSegments(t.Context(), config{}, dvr, 1,
		time.Now().Add(-time.Hour), time.Now())
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(segs) != 2 {
		t.Fatalf("got %d segments, want 2 (inverted span dropped)", len(segs))
	}
	if !segs[0].Start.Before(segs[1].Start) {
		t.Errorf("segments not sorted by start: %v then %v", segs[0].Start, segs[1].Start)
	}
	if segs[0].Name != "a" {
		t.Errorf("first segment name = %q, want the 08:00 one", segs[0].Name)
	}
}
