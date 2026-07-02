package main

// camera_s3_test.go — offline (no-network) verification of the hand-rolled SigV4
// signer. The load-bearing test drives s3CanonicalRequest + s3Authorization with
// AWS's published Signature V4 test-suite "get-vanilla" vector and asserts the
// exact canonical request and Authorization header AWS documents, so the signing
// math (canonicalization → string-to-sign → HMAC signing-key chain → signature) is
// proven correct without touching the network. The rest cover key formatting,
// path escaping and the enabled predicate.

import (
	"testing"
	"time"
)

// TestS3AuthorizationGetVanilla verifies the full SigV4 pipeline against the
// canonical "get-vanilla" case from AWS's official aws-sig-v4-test-suite.
//
//	Access key: AKIDEXAMPLE
//	Secret:     wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY
//	Region:     us-east-1   Service: service
//	Request:    GET / (host example.amazonaws.com, x-amz-date 20150830T123600Z)
func TestS3AuthorizationGetVanilla(t *testing.T) {
	const (
		accessKey = "AKIDEXAMPLE"
		secret    = "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY"
		region    = "us-east-1"
		service   = "service"
		amzDate   = "20150830T123600Z"
	)
	// get-vanilla signs only host + x-amz-date (no x-amz-content-sha256), with the
	// empty-payload hash.
	signedHeaders := "host;x-amz-date"
	canonicalHeaders := "host:example.amazonaws.com\nx-amz-date:20150830T123600Z\n"

	// Assert the canonical request byte-for-byte against AWS's published .creq.
	wantCanonical := "GET\n" +
		"/\n" +
		"\n" +
		"host:example.amazonaws.com\n" +
		"x-amz-date:20150830T123600Z\n" +
		"\n" +
		"host;x-amz-date\n" +
		emptyPayloadSHA256
	gotCanonical := s3CanonicalRequest("GET", "/", "", canonicalHeaders, signedHeaders, emptyPayloadSHA256)
	if gotCanonical != wantCanonical {
		t.Fatalf("canonical request mismatch:\n got: %q\nwant: %q", gotCanonical, wantCanonical)
	}

	// Assert the full Authorization header against AWS's published .authz.
	wantAuth := "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20150830/us-east-1/service/aws4_request, " +
		"SignedHeaders=host;x-amz-date, " +
		"Signature=5fa00fa31553b73ebf1942676e86291e8372ff2a2260956d9b8aae1d763fbf31"
	gotAuth := s3Authorization(accessKey, secret, region, service, amzDate, "GET", "/", "",
		signedHeaders, canonicalHeaders, emptyPayloadSHA256)
	if gotAuth != wantAuth {
		t.Fatalf("authorization mismatch:\n got: %s\nwant: %s", gotAuth, wantAuth)
	}
}

// TestS3SigningKeyChain verifies the HMAC signing-key derivation is stable and
// non-empty for the get-vanilla inputs (the full-auth test above pins its use).
func TestS3SigningKeyChain(t *testing.T) {
	k := s3SigningKey("wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY", "20150830", "us-east-1", "service")
	if len(k) != 32 {
		t.Fatalf("signing key length = %d, want 32 (sha256)", len(k))
	}
}

func TestCamS3FrameKey(t *testing.T) {
	// Isolate the package-global prefix and restore it afterward.
	saved := cameraCfg.CameraS3Prefix
	defer func() { cameraCfg.CameraS3Prefix = saved }()

	// A non-UTC instant must be laid out in UTC.
	loc := time.FixedZone("UTC+2", 2*3600)
	ts := time.Date(2026, 3, 7, 7, 9, 2, 0, loc) // 05:09:02 UTC

	tests := []struct {
		name    string
		prefix  string
		camera  string
		quality string
		t       time.Time
		want    string
	}{
		{"no prefix main", "", "cam-42", "main", ts, "cam-42/main/2026/03/07/05/09/02.jpg"},
		{"no prefix sub", "", "cam-42", "sub", ts, "cam-42/sub/2026/03/07/05/09/02.jpg"},
		{"with prefix", "cams/", "abc123", "main", ts,
			"cams/abc123/main/2026/03/07/05/09/02.jpg"},
		{"utc input", "", "x", "sub",
			time.Date(2025, 12, 31, 23, 59, 59, 0, time.UTC),
			"x/sub/2025/12/31/23/59/59.jpg"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cameraCfg.CameraS3Prefix = tc.prefix
			if got := camS3FrameKey(tc.camera, tc.quality, tc.t); got != tc.want {
				t.Fatalf("camS3FrameKey = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestS3EscapePath(t *testing.T) {
	tests := []struct{ in, want string }{
		{"cam-42/main/2026/03/07/05/09/02.jpg", "cam-42/main/2026/03/07/05/09/02.jpg"}, // unreserved: unchanged
		{"a b/c.jpg", "a%20b/c.jpg"},   // space escaped, slash preserved
		{"x+y/z", "x%2By/z"},           // plus escaped
		{"a~b_c-d.e/f", "a~b_c-d.e/f"}, // tilde/underscore/hyphen/dot are unreserved
		{"p&q=r/s", "p%26q%3Dr/s"},     // ampersand + equals escaped
	}
	for _, tc := range tests {
		if got := s3EscapePath(tc.in); got != tc.want {
			t.Errorf("s3EscapePath(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestCamS3SnapArchiveTime exercises the read-through grid-snapping helper
// added alongside the archiver (camera_investigate.go, WP1): a sample time
// must round to the NEAREST multiple of the archiver's interval so a
// read-through lookup lands on the exact key camS3FrameKey/the archiver wrote.
func TestCamS3SnapArchiveTime(t *testing.T) {
	base := time.Date(2026, 3, 7, 5, 9, 0, 0, time.UTC)
	tests := []struct {
		name     string
		t        time.Time
		interval time.Duration
		want     time.Time
	}{
		{"exact multiple unchanged", base, 15 * time.Second, base},
		{"rounds down within half interval", base.Add(6 * time.Second), 15 * time.Second, base},
		{"rounds up within half interval", base.Add(9 * time.Second), 15 * time.Second, base.Add(15 * time.Second)},
		{"exact half rounds up (time.Round tie-break)", base.Add(7500 * time.Millisecond), 15 * time.Second, base.Add(15 * time.Second)},
		{"zero interval falls back to 15s grid", base.Add(6 * time.Second), 0, base},
		{"negative interval falls back to 15s grid", base.Add(9 * time.Second), -time.Second, base.Add(15 * time.Second)},
		{"30s main-stream grid", base.Add(14 * time.Second), 30 * time.Second, base},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := camS3SnapArchiveTime(tc.t, tc.interval); !got.Equal(tc.want) {
				t.Fatalf("camS3SnapArchiveTime(%v, %v) = %v, want %v", tc.t, tc.interval, got, tc.want)
			}
		})
	}
}

func TestCamS3Enabled(t *testing.T) {
	base := func() config {
		return config{
			CameraS3Endpoint:  "hel1.your-objectstorage.com",
			CameraS3Bucket:    "connect-cams",
			CameraS3AccessKey: "AK",
			CameraS3Secret:    "SK",
		}
	}
	if !camS3Enabled(base()) {
		t.Fatal("fully-configured cfg should be enabled")
	}
	for _, drop := range []string{"endpoint", "bucket", "access", "secret"} {
		cfg := base()
		switch drop {
		case "endpoint":
			cfg.CameraS3Endpoint = "  "
		case "bucket":
			cfg.CameraS3Bucket = ""
		case "access":
			cfg.CameraS3AccessKey = ""
		case "secret":
			cfg.CameraS3Secret = ""
		}
		if camS3Enabled(cfg) {
			t.Errorf("missing %s should disable S3", drop)
		}
	}
}
