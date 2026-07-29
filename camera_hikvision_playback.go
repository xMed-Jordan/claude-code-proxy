package main

// camera_hikvision_playback.go — recorded-footage access over ISAPI ContentMgmt,
// for Hikvision devices whose RTSP playback endpoint does not work.
//
// Measured on the two live units (2026-07):
//   - The Amman DVR serves RTSP playback at /Streaming/tracks/<ch>01?starttime=…
//     exactly as PlaybackURL builds it.
//   - The Irbid NVR answers that same URL with "400 Bad Request" — and answers
//     /Streaming/channels/<ch>01?starttime=… with the LIVE stream, ignoring the
//     query entirely. That second behaviour is the dangerous one: a fallback to
//     the channels form returns today's footage with no error, so an
//     investigation asking about last Tuesday would be answered with video from
//     this morning. We therefore never use the channels form for playback.
//
// A device that rejects RTSP playback is served over ISAPI instead: search for
// the recording segment covering the instant (ContentMgmt/search), download the
// segment (ContentMgmt/download), and cut the frame out locally. Both devices
// ignore a narrowed starttime/endtime on the download and always stream from the
// SEGMENT start, so the transfer is capped at the byte offset the target instant
// maps to rather than by asking for a shorter clip.
//
// Every timestamp here is DVR-LOCAL. These devices label times with a trailing
// "Z" but mean their own wall clock — the same quirk hikLocation() handles for
// the RTSP URL builder. Treating them as UTC silently returns footage that is
// off by the DVR's offset (3h for Asia/Amman).

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// hikISAPITimeLayout is the shape ISAPI uses in both directions. The trailing Z
// is part of the format, not a UTC claim (see the file comment).
const hikISAPITimeLayout = "2006-01-02T15:04:05Z"

// hikSegmentSearchMax bounds how many segments we ask for in one search; a day of
// continuous recording is a handful of segments on both units.
const hikSegmentSearchMax = 40

// hikDownloadSlack pads the computed byte cap. Bitrate is not constant, so the
// linear time→byte estimate can land short of the instant we actually want.
const hikDownloadSlack = 0.12

// hikSegmentMaxBytes is a hard ceiling on one segment fetch, so a
// misconfigured/huge recording can never fill the disk. 1.5 GB comfortably
// covers a multi-hour 1080p segment on these units.
const hikSegmentMaxBytes int64 = 1536 << 20

// hikRecordSegment is one contiguous recording on a track, as reported by
// ContentMgmt/search. Name and Size are the device's own tokens: the download
// endpoint rejects a playbackURI that omits them (400/500 on the two live units).
type hikRecordSegment struct {
	Track int
	Start time.Time
	End   time.Time
	Name  string
	Size  int64
}

// Duration is the segment's wall-clock length (never zero, so it is safe as a
// divisor).
func (s hikRecordSegment) Duration() time.Duration {
	if d := s.End.Sub(s.Start); d > 0 {
		return d
	}
	return time.Second
}

// ─────────────────────────── search ───────────────────────────

type hikSearchResult struct {
	NumOfMatches int                  `xml:"numOfMatches"`
	Matches      []hikSearchMatchItem `xml:"matchList>searchMatchItem"`
}

type hikSearchMatchItem struct {
	TrackID   int    `xml:"trackID"`
	StartTime string `xml:"timeSpan>startTime"`
	EndTime   string `xml:"timeSpan>endTime"`
	URI       string `xml:"mediaSegmentDescriptor>playbackURI"`
}

// hikSearchSegments asks the device which recordings overlap [from, to] on a
// channel's main track. from/to are converted into the DVR's own local clock.
func hikSearchSegments(ctx context.Context, cfg config, dvr CamDVR, ch int, from, to time.Time) ([]hikRecordSegment, error) {
	loc := hikLocation(dvr)
	track := ch*100 + 1
	body := fmt.Sprintf(`<?xml version="1.0" encoding="utf-8"?>
<CMSearchDescription><searchID>%s</searchID>
<trackIDList><trackID>%d</trackID></trackIDList>
<timeSpanList><timeSpan><startTime>%s</startTime><endTime>%s</endTime></timeSpan></timeSpanList>
<maxResults>%d</maxResults><searchResultPostion>0</searchResultPostion>
<metadataList><metadataDescriptor>//recordType.meta.std-cgi.com</metadataDescriptor></metadataList>
</CMSearchDescription>`,
		hikSearchID(dvr, track), track,
		from.In(loc).Format(hikISAPITimeLayout), to.In(loc).Format(hikISAPITimeLayout),
		hikSegmentSearchMax)

	raw, err := hikISAPIPost(ctx, cfg, dvr, "/ISAPI/ContentMgmt/search", []byte(body), 1<<20)
	if err != nil {
		return nil, err
	}
	var res hikSearchResult
	if err := xml.Unmarshal(raw, &res); err != nil {
		return nil, fmt.Errorf("hik search: %w", err)
	}
	out := make([]hikRecordSegment, 0, len(res.Matches))
	for _, m := range res.Matches {
		st, serr := time.ParseInLocation(hikISAPITimeLayout, strings.TrimSpace(m.StartTime), loc)
		en, eerr := time.ParseInLocation(hikISAPITimeLayout, strings.TrimSpace(m.EndTime), loc)
		if serr != nil || eerr != nil || !en.After(st) {
			continue
		}
		seg := hikRecordSegment{Track: m.TrackID, Start: st, End: en}
		if seg.Track == 0 {
			seg.Track = track
		}
		seg.Name, seg.Size = hikURITokens(m.URI)
		out = append(out, seg)
	}
	sortSegments(out)
	return out, nil
}

func sortSegments(segs []hikRecordSegment) {
	for i := 1; i < len(segs); i++ {
		for j := i; j > 0 && segs[j].Start.Before(segs[j-1].Start); j-- {
			segs[j], segs[j-1] = segs[j-1], segs[j]
		}
	}
}

// hikURITokens pulls the device's own name=/size= tokens out of a playbackURI.
// The download endpoint needs them echoed back verbatim.
func hikURITokens(uri string) (string, int64) {
	uri = strings.ReplaceAll(uri, "&amp;", "&")
	i := strings.Index(uri, "?")
	if i < 0 {
		return "", 0
	}
	q, err := url.ParseQuery(uri[i+1:])
	if err != nil {
		return "", 0
	}
	size, _ := strconv.ParseInt(strings.TrimSpace(q.Get("size")), 10, 64)
	return strings.TrimSpace(q.Get("name")), size
}

// hikSearchID is a stable per-(dvr,track) search identifier. ISAPI wants a
// GUID-shaped string; it does not have to be unique across calls.
func hikSearchID(dvr CamDVR, track int) string {
	return fmt.Sprintf("C7B4A1E2-0000-1000-8000-%012d", track%1000000000000)
}

// ─────────────────────────── download ───────────────────────────

// hikSegmentByteOffset estimates how many bytes of a segment must be read to
// reach wall-clock `until`, plus slack. Returns 0 (meaning "no cap") when the
// segment size is unknown.
func hikSegmentByteOffset(seg hikRecordSegment, until time.Time) int64 {
	if seg.Size <= 0 {
		return 0
	}
	frac := until.Sub(seg.Start).Seconds() / seg.Duration().Seconds()
	if frac < 0 {
		frac = 0
	}
	frac = frac*(1+hikDownloadSlack) + 0.02
	if frac >= 1 {
		return seg.Size
	}
	n := int64(float64(seg.Size) * frac)
	// Always ask for enough bytes that ffmpeg has something decodable, but never
	// more than the segment actually holds.
	floor := int64(1 << 20)
	if seg.Size < floor {
		floor = seg.Size
	}
	if n < floor {
		n = floor
	}
	if n > seg.Size {
		n = seg.Size
	}
	return n
}

// hikDownloadSegment streams a segment to dest, stopping after maxBytes (0 =
// whole segment, still bounded by hikSegmentMaxBytes). Returns bytes written.
func hikDownloadSegment(ctx context.Context, cfg config, dvr CamDVR, seg hikRecordSegment, dest string, maxBytes int64) (int64, error) {
	loc := hikLocation(dvr)
	uri := fmt.Sprintf("rtsp://%s:%d/Streaming/tracks/%d/?starttime=%s&endtime=%s&name=%s&size=%d",
		dvr.Host, hikHTTPPort(dvr), seg.Track,
		seg.Start.In(loc).Format("20060102T150405Z"),
		seg.End.In(loc).Format("20060102T150405Z"),
		seg.Name, seg.Size)
	body := `<?xml version="1.0" encoding="utf-8"?>
<downloadRequest version="1.0" xmlns="http://www.hikvision.com/ver20/XMLSchema">` +
		"<playbackURI>" + xmlEscape(uri) + "</playbackURI></downloadRequest>"

	if maxBytes <= 0 || maxBytes > hikSegmentMaxBytes {
		maxBytes = hikSegmentMaxBytes
	}
	resp, err := hikISAPIRequest(ctx, cfg, dvr, "/ISAPI/ContentMgmt/download", []byte(body))
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("hik download: status %d", resp.StatusCode)
	}
	f, err := os.Create(dest)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	n, cerr := io.Copy(f, io.LimitReader(resp.Body, maxBytes))
	if cerr != nil && n == 0 {
		return 0, cerr
	}
	if n < 4096 {
		return n, errNoRecording
	}
	return n, nil
}

// ─────────────────────────── frame extraction ───────────────────────────

// hikISAPIFrameAt grabs one still at (approximately) t by fetching the enclosing
// recording segment over ISAPI and cutting locally. The segment file is removed
// before returning — only the JPEG survives.
//
// -skip_frame nokey decodes I-frames only: these units key every 1-2s, which is
// well inside the tolerance of any caller sampling recorded footage, and it turns
// a multi-minute decode into a couple of seconds.
func hikISAPIFrameAt(ctx context.Context, cfg config, dvr CamDVR, ch int, t time.Time, destPath string) (captureResult, error) {
	segs, err := hikSearchSegments(ctx, cfg, dvr, ch, t.Add(-2*time.Minute), t.Add(2*time.Minute))
	if err != nil {
		return captureResult{}, err
	}
	var seg hikRecordSegment
	found := false
	for _, s := range segs {
		if !t.Before(s.Start) && t.Before(s.End) {
			seg, found = s, true
			break
		}
	}
	if !found {
		return captureResult{}, errNoRecording
	}

	dir := cfg.CameraMediaDir
	if dir == "" {
		dir = os.TempDir()
	}
	tmp, err := os.CreateTemp(dir, "hikseg-*.mp4")
	if err != nil {
		return captureResult{}, err
	}
	tmpPath := tmp.Name()
	_ = tmp.Close()
	defer os.Remove(tmpPath)

	if _, err := hikDownloadSegment(ctx, cfg, dvr, seg, tmpPath,
		hikSegmentByteOffset(seg, t.Add(pastFrameWindow))); err != nil {
		return captureResult{}, err
	}

	offset := t.Sub(seg.Start).Seconds()
	if offset < 0 {
		offset = 0
	}
	finalPath := strings.TrimSuffix(destPath, filepath.Ext(destPath)) + ".jpg"
	timeout := cfg.CameraCaptureTimeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	res, rerr := runCamCommand(ctx, timeout+60*time.Second, cameraFFmpegBin(cfg),
		"-nostdin", "-loglevel", "error", "-skip_frame", "nokey",
		"-ss", strconv.FormatFloat(offset, 'f', 2, 64), "-i", tmpPath,
		"-frames:v", "1", "-q:v", "3", "-y", finalPath)
	if rerr != nil {
		return captureResult{}, fmt.Errorf("hik isapi frame: %w", rerr)
	}
	st, serr := os.Stat(finalPath)
	if serr != nil || st.Size() == 0 {
		return captureResult{}, fmt.Errorf("hik isapi frame: no image decoded (%s)",
			strings.TrimSpace(res.Stderr))
	}
	return captureResult{Path: finalPath, ContentType: "image/jpeg",
		Bytes: st.Size(), Method: "ffmpeg"}, nil
}

// ─────────────────────────── transport ───────────────────────────

func hikISAPIRequest(ctx context.Context, cfg config, dvr CamDVR, path string, body []byte) (*http.Response, error) {
	scheme := "http"
	port := hikHTTPPort(dvr)
	if port == 443 {
		scheme = "https"
	}
	raw := fmt.Sprintf("%s://%s:%d%s", scheme, dvr.Host, port, path)
	client := camHTTPClient()
	// Segment downloads are hundreds of MB; the shared 15s client timeout would
	// abort mid-transfer.
	client.Timeout = 10 * time.Minute
	return httpDigestDo(ctx, client, http.MethodPost, raw, body, "application/xml",
		dvr.Username, dvr.Password)
}

func hikISAPIPost(ctx context.Context, cfg config, dvr CamDVR, path string, body []byte, limit int64) ([]byte, error) {
	resp, err := hikISAPIRequest(ctx, cfg, dvr, path, body)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw := readAllLimited(resp.Body, limit)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("isapi %s: status %d", path, resp.StatusCode)
	}
	return raw, nil
}

// ─────────────────────────── capability cache ───────────────────────────

// Playback transport per DVR. Probing costs a failed RTSP attempt, so the answer
// is remembered: without this every recorded-frame request on an ISAPI-only unit
// pays the RTSP timeout first.
const (
	hikPlaybackRTSP  = "rtsp"
	hikPlaybackISAPI = "isapi"
)

const hikPlaybackModeTTL = 30 * time.Minute

type hikPlaybackModeEntry struct {
	mode string
	at   time.Time
}

var (
	hikPlaybackModeMu sync.Mutex
	hikPlaybackModes  = map[string]hikPlaybackModeEntry{}
)

func hikPlaybackModeGet(dvrID string) string {
	hikPlaybackModeMu.Lock()
	defer hikPlaybackModeMu.Unlock()
	e, ok := hikPlaybackModes[dvrID]
	if !ok || time.Since(e.at) > hikPlaybackModeTTL {
		return ""
	}
	return e.mode
}

func hikPlaybackModeSet(dvrID, mode string) {
	hikPlaybackModeMu.Lock()
	defer hikPlaybackModeMu.Unlock()
	hikPlaybackModes[dvrID] = hikPlaybackModeEntry{mode: mode, at: time.Now()}
}

// hikPlaybackModeReset clears the cache (tests, and DVR edits that may change the
// device behind an id).
func hikPlaybackModeReset() {
	hikPlaybackModeMu.Lock()
	defer hikPlaybackModeMu.Unlock()
	hikPlaybackModes = map[string]hikPlaybackModeEntry{}
}

// camPlaybackSupportsISAPI reports whether the ISAPI recorded-footage path is
// worth trying for this DVR.
func camPlaybackSupportsISAPI(dvr CamDVR) bool {
	return strings.EqualFold(strings.TrimSpace(dvr.Brand), "hikvision") &&
		strings.TrimSpace(dvr.Host) != ""
}

var errNoISAPIPlayback = errors.New("isapi playback unavailable for this dvr")
