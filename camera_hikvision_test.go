package main

// camera_hikvision_test.go — table-driven tests for the pure/deterministic
// Hikvision URL builders (LiveURL/SnapshotURL/PlaybackURL). These never touch the
// network; PlaybackURL's GMT/"Z" time formatting (unlike Dahua's local-underscore
// convention, see camera_dahua_test.go) is the main thing worth pinning down.

import (
	"strings"
	"testing"
	"time"
)

func hikTestDVR() CamDVR {
	return CamDVR{
		ID:       "dvr1",
		Host:     "192.168.1.50",
		Port:     554,
		HTTPPort: 80,
		Username: "admin",
		Password: "s3cr3t!@#",
	}
}

func TestHikLiveURL(t *testing.T) {
	dvr := hikTestDVR()
	cases := []struct {
		name string
		ch   int
		q    StreamQuality
		want string
	}{
		{"main_ch1", 1, StreamMain, "rtsp://admin:s3cr3t%21%40%23@192.168.1.50:554/Streaming/Channels/101"},
		{"sub_ch1", 1, StreamSub, "rtsp://admin:s3cr3t%21%40%23@192.168.1.50:554/Streaming/Channels/102"},
		{"main_ch7", 7, StreamMain, "rtsp://admin:s3cr3t%21%40%23@192.168.1.50:554/Streaming/Channels/701"},
		{"sub_ch32", 32, StreamSub, "rtsp://admin:s3cr3t%21%40%23@192.168.1.50:554/Streaming/Channels/3202"},
	}
	adapter := hikvisionBrandAdapter{}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := adapter.LiveURL(dvr, c.ch, c.q)
			if got != c.want {
				t.Errorf("LiveURL(%d,%v) = %q, want %q", c.ch, c.q, got, c.want)
			}
			// Credentials must always be carried via url.UserPassword (percent
			// encoded), never string-concatenated raw into the URL.
			if strings.Contains(got, "s3cr3t!@#") {
				t.Errorf("LiveURL leaked raw (unencoded) password: %q", got)
			}
		})
	}
}

func TestHikSnapshotURL(t *testing.T) {
	dvr := hikTestDVR()
	adapter := hikvisionBrandAdapter{}
	cases := []struct {
		name string
		ch   int
		q    StreamQuality
		want string
	}{
		{"main_ch1", 1, StreamMain, "http://192.168.1.50:80/ISAPI/Streaming/channels/101/picture"},
		{"sub_ch1", 1, StreamSub, "http://192.168.1.50:80/ISAPI/Streaming/channels/102/picture"},
		{"main_ch12", 12, StreamMain, "http://192.168.1.50:80/ISAPI/Streaming/channels/1201/picture"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, isHTTP, ok := adapter.SnapshotURL(dvr, c.ch, c.q)
			if !ok || !isHTTP {
				t.Fatalf("SnapshotURL ok=%v isHTTP=%v, want true/true", ok, isHTTP)
			}
			if got != c.want {
				t.Errorf("SnapshotURL(%d,%v) = %q, want %q", c.ch, c.q, got, c.want)
			}
			// The HTTP snapshot endpoint never embeds credentials — auth rides in
			// the Authorization header via httpDigestGet.
			if strings.Contains(got, "admin") || strings.Contains(got, "@") {
				t.Errorf("SnapshotURL must not embed credentials: %q", got)
			}
		})
	}

	// https upgrade only on the canonical HTTPS port.
	tlsDVR := dvr
	tlsDVR.HTTPPort = 443
	got, _, _ := adapter.SnapshotURL(tlsDVR, 1, StreamMain)
	if !strings.HasPrefix(got, "https://") {
		t.Errorf("SnapshotURL on port 443 = %q, want https:// scheme", got)
	}
}

func TestHikPlaybackURLGMTZFormat(t *testing.T) {
	dvr := hikTestDVR()
	adapter := hikvisionBrandAdapter{}
	// Hikvision playback time is GMT/"Z", formatted from start.UTC()/end.UTC() —
	// this must hold even when the input times are constructed in a non-UTC
	// location (a client in +03:00 asking for "10:00-10:05 local").
	loc := time.FixedZone("TestTZ", 3*3600)
	start := time.Date(2026, 7, 1, 10, 0, 0, 0, loc)
	end := time.Date(2026, 7, 1, 10, 5, 0, 0, loc)

	got, err := adapter.PlaybackURL(dvr, 3, StreamMain, start, end)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.HasPrefix(got, "rtsp://admin:") {
		t.Fatalf("PlaybackURL = %q, want rtsp://admin:... prefix", got)
	}
	if !strings.Contains(got, "/Streaming/tracks/301") {
		t.Errorf("PlaybackURL = %q, want /Streaming/tracks/301 (channel*100+1)", got)
	}
	// start/end must be rendered in UTC (07:00Z, not 10:00 local) with the
	// trailing "Z" GMT marker — this is the top cause of playback drift bugs if
	// someone accidentally formats in local time (Dahua's convention).
	if !strings.Contains(got, "starttime=20260701T070000Z") {
		t.Errorf("PlaybackURL = %q, want starttime=20260701T070000Z (GMT)", got)
	}
	if !strings.Contains(got, "endtime=20260701T070500Z") {
		t.Errorf("PlaybackURL = %q, want endtime=20260701T070500Z (GMT)", got)
	}
	if strings.Contains(got, "_") {
		t.Errorf("PlaybackURL = %q, must not use Dahua's underscore local-time format", got)
	}
}

func TestHikPlaybackURLRejectsInvertedWindow(t *testing.T) {
	dvr := hikTestDVR()
	adapter := hikvisionBrandAdapter{}
	start := time.Date(2026, 7, 1, 10, 5, 0, 0, time.UTC)
	end := time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC) // before start
	if _, err := adapter.PlaybackURL(dvr, 1, StreamMain, start, end); err == nil {
		t.Fatal("expected error for inverted playback window, got nil")
	}
	// Equal start/end is also invalid (zero-length window).
	if _, err := adapter.PlaybackURL(dvr, 1, StreamMain, start, start); err == nil {
		t.Fatal("expected error for zero-length playback window, got nil")
	}
}

// ─────────────────────── channel-list folding (Streaming vs InputProxy) ───────────────────────

func TestHikChannelsFromStreamingFoldsMainSubAndPreservesDeviceIDs(t *testing.T) {
	list := hikStreamingChannelList{Channels: []hikStreamingChannel{
		{ID: "101", ChannelName: "Lobby", Width: "1920", Height: "1080"}, // ch1 main
		{ID: "102", ChannelName: "Lobby-sub"},                            // ch1 sub (lower priority, ignored for name)
		{ID: "3201", ChannelName: "Back Door", Width: "1280", Height: "720"}, // ch32 main (non-contiguous)
		{ID: "bogus"}, // unparseable, must be skipped
	}}
	got := hikChannelsFromStreaming(list)
	if len(got) != 2 {
		t.Fatalf("got %d channels, want 2: %+v", len(got), got)
	}
	if got[0].Channel != 1 || got[0].Name != "Lobby" || got[0].MainW != 1920 || got[0].MainH != 1080 {
		t.Errorf("channel 1 = %+v", got[0])
	}
	if got[1].Channel != 32 || got[1].Name != "Back Door" {
		t.Errorf("channel 32 = %+v", got[1])
	}
}

func TestHikChannelsFromInputProxyPreservesNonContiguousIDs(t *testing.T) {
	list := hikInputProxyChannelList{Channels: []hikInputProxyChannel{
		{ID: "1", Name: "Front"},
		{ID: "5", Name: "Rear"},
		{ID: "5", Name: "Rear-dup"}, // duplicate id, must be skipped
		{ID: "0"},                   // invalid, must be skipped
	}}
	got := hikChannelsFromInputProxy(list)
	if len(got) != 2 {
		t.Fatalf("got %d channels, want 2: %+v", len(got), got)
	}
	if got[0].Channel != 1 || got[0].Name != "Front" {
		t.Errorf("channel[0] = %+v", got[0])
	}
	if got[1].Channel != 5 || got[1].Name != "Rear" {
		t.Errorf("channel[1] = %+v (dup should have kept the FIRST name)", got[1])
	}
}
