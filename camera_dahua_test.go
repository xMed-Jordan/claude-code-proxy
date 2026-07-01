package main

// camera_dahua_test.go — table-driven tests for the pure/deterministic Dahua URL
// builders (LiveURL/SnapshotURL/PlaybackURL) and the ChannelTitle line parser.
// PlaybackURL's LOCAL-underscore time convention (unlike Hikvision's GMT/"Z", see
// camera_hikvision_test.go) is the main thing worth pinning down, including that
// it actually converts into the DVR's configured IANA timezone.

import (
	"strings"
	"testing"
	"time"
)

func dahuaTestDVR() CamDVR {
	return CamDVR{
		ID:       "dvr2",
		Host:     "192.168.1.60",
		Port:     554,
		HTTPPort: 80,
		Username: "admin",
		Password: "p@ss/w:rd",
	}
}

func TestDahuaLiveURLRealmonitor(t *testing.T) {
	dvr := dahuaTestDVR()
	adapter := dahuaBrandAdapter{}
	cases := []struct {
		name        string
		ch          int
		q           StreamQuality
		wantChannel string
		wantSubtype string
	}{
		{"main_ch1", 1, StreamMain, "channel=1", "subtype=0"},
		{"sub_ch1", 1, StreamSub, "channel=1", "subtype=1"},
		{"main_ch9", 9, StreamMain, "channel=9", "subtype=0"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := adapter.LiveURL(dvr, c.ch, c.q)
			if !strings.HasPrefix(got, "rtsp://admin:") {
				t.Fatalf("LiveURL = %q, want rtsp://admin:... prefix", got)
			}
			if !strings.Contains(got, "/cam/realmonitor?") {
				t.Errorf("LiveURL = %q, want /cam/realmonitor path", got)
			}
			if !strings.Contains(got, c.wantChannel) {
				t.Errorf("LiveURL = %q, want to contain %q", got, c.wantChannel)
			}
			if !strings.Contains(got, c.wantSubtype) {
				t.Errorf("LiveURL = %q, want to contain %q", got, c.wantSubtype)
			}
			// Credentials must be percent-encoded via url.UserPassword, never the
			// raw password string (which contains reserved URL characters).
			if strings.Contains(got, "p@ss/w:rd") {
				t.Errorf("LiveURL leaked raw (unencoded) password: %q", got)
			}
		})
	}
}

func TestDahuaSnapshotURL(t *testing.T) {
	dvr := dahuaTestDVR()
	adapter := dahuaBrandAdapter{}
	got, isHTTP, ok := adapter.SnapshotURL(dvr, 4, StreamSub)
	if !ok || !isHTTP {
		t.Fatalf("SnapshotURL ok=%v isHTTP=%v, want true/true", ok, isHTTP)
	}
	if !strings.HasPrefix(got, "http://192.168.1.60:80/cgi-bin/snapshot.cgi?") {
		t.Fatalf("SnapshotURL = %q, want snapshot.cgi prefix", got)
	}
	if !strings.Contains(got, "channel=4") || !strings.Contains(got, "subtype=1") {
		t.Errorf("SnapshotURL = %q, want channel=4&subtype=1", got)
	}
	// No embedded credentials — auth rides in the Authorization header.
	if strings.Contains(got, "admin") || strings.Contains(got, "@") {
		t.Errorf("SnapshotURL must not embed credentials: %q", got)
	}
}

func TestDahuaPlaybackURLLocalUnderscoreFormat(t *testing.T) {
	adapter := dahuaBrandAdapter{}
	dvr := dahuaTestDVR()
	dvr.Timezone = "Asia/Amman" // UTC+3 in July 2026 (no DST)

	start := time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)
	end := time.Date(2026, 7, 1, 10, 3, 0, 0, time.UTC)

	got, err := adapter.PlaybackURL(dvr, 2, StreamMain, start, end)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(got, "/cam/playback?") {
		t.Errorf("PlaybackURL = %q, want /cam/playback path", got)
	}
	if !strings.Contains(got, "channel=2") || !strings.Contains(got, "subtype=0") {
		t.Errorf("PlaybackURL = %q, want channel=2&subtype=0", got)
	}
	// 10:00 UTC -> 13:00 local (Asia/Amman, UTC+3); local-underscore format, no "Z".
	if !strings.Contains(got, "starttime=2026_07_01_13_00_00") {
		t.Errorf("PlaybackURL = %q, want starttime=2026_07_01_13_00_00 (local, underscore)", got)
	}
	if !strings.Contains(got, "endtime=2026_07_01_13_03_00") {
		t.Errorf("PlaybackURL = %q, want endtime=2026_07_01_13_03_00 (local, underscore)", got)
	}
	if strings.Contains(got, "Z") {
		t.Errorf("PlaybackURL = %q, must not use Hikvision's GMT/Z format", got)
	}
}

func TestDahuaPlaybackURLDefaultsToUTCWithoutTimezone(t *testing.T) {
	adapter := dahuaBrandAdapter{}
	dvr := dahuaTestDVR() // Timezone == ""
	start := time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)
	end := time.Date(2026, 7, 1, 10, 1, 0, 0, time.UTC)
	got, err := adapter.PlaybackURL(dvr, 1, StreamMain, start, end)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(got, "starttime=2026_07_01_10_00_00") {
		t.Errorf("PlaybackURL (no tz) = %q, want UTC fallback 2026_07_01_10_00_00", got)
	}
}

func TestDahuaPlaybackURLRejectsInvertedWindow(t *testing.T) {
	adapter := dahuaBrandAdapter{}
	dvr := dahuaTestDVR()
	start := time.Date(2026, 7, 1, 10, 5, 0, 0, time.UTC)
	end := time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)
	if _, err := adapter.PlaybackURL(dvr, 1, StreamMain, start, end); err == nil {
		t.Fatal("expected error for inverted playback window, got nil")
	}
}

// ─────────────────────── ChannelTitle line parser ───────────────────────

func TestDahuaParseChannelTitles(t *testing.T) {
	body := "table.ChannelTitle[0]=Front Door\r\n" +
		"table.ChannelTitle[1]=\"Back Yard\"\r\n" +
		"garbage line without a marker\r\n" +
		"table.ChannelTitle[bad]=Oops\n" +
		"table.ChannelTitle[3]=Warehouse\n"
	got := dahuaParseChannelTitles(body)
	if len(got) != 3 {
		t.Fatalf("got %d channels, want 3: %+v", len(got), got)
	}
	// 0-based CGI index -> 1-based channel/subtype query param.
	if got[0].Channel != 1 || got[0].Name != "Front Door" {
		t.Errorf("channel[0] = %+v", got[0])
	}
	if got[1].Channel != 2 || got[1].Name != "Back Yard" {
		t.Errorf("channel[1] = %+v (quotes should be stripped)", got[1])
	}
	if got[2].Channel != 4 || got[2].Name != "Warehouse" {
		t.Errorf("channel[2] = %+v (non-contiguous index preserved)", got[2])
	}
}

func TestDahuaParseChannelTitlesEmpty(t *testing.T) {
	if got := dahuaParseChannelTitles(""); len(got) != 0 {
		t.Errorf("empty body -> %+v, want empty", got)
	}
	if got := dahuaParseChannelTitles("no markers here\nor here\n"); len(got) != 0 {
		t.Errorf("no markers -> %+v, want empty", got)
	}
}
