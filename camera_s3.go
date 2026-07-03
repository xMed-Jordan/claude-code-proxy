package main

// camera_s3.go — a minimal, dependency-free AWS Signature V4 client for archiving
// camera frames to S3-compatible object storage (Hetzner Object Storage). We speak
// SigV4 by hand (stdlib crypto/hmac + crypto/sha256 + net/http only) so the proxy
// keeps its tiny dependency footprint (modernc.org/sqlite + gorilla/websocket).
//
// Scope: single-object PUT / GET / HEAD against a virtual-hosted bucket — request
// host = <bucket>.<endpoint> (e.g. connect-cams.hel1.your-objectstorage.com),
// HTTPS only, no multipart, no query signing. Credentials come from config at
// runtime and are NEVER hardcoded (the secret is injected via .env at deploy).
//
// Callers: camera_archive.go (writer, WP2) and camera_investigate.go (read-through,
// WP1). The signing math is verified offline by camera_s3_test.go against AWS's
// published SigV4 test-suite "get-vanilla" vector — no network needed to test.

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// errS3NotFound is the sentinel returned by s3GetObject on HTTP 404 so callers can
// errors.Is it and fall back to a live capture instead of treating it as a failure.
var errS3NotFound = errors.New("s3: object not found")

const (
	s3Service      = "s3"
	s3SigAlgorithm = "AWS4-HMAC-SHA256"
	// s3SignedHeaders is the fixed, sorted set this client signs for every request.
	s3SignedHeaders = "host;x-amz-content-sha256;x-amz-date"
	// emptyPayloadSHA256 is hex(sha256("")) — the payload hash for a bodyless request.
	emptyPayloadSHA256 = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	s3HexUpper         = "0123456789ABCDEF"
)

// s3HTTPClient is a guard-free, short-timeout client dedicated to object storage.
// Unlike camHTTPClient (which accepts DVR self-signed certs) it verifies TLS —
// Hetzner presents a valid public cert — and is not wrapped by any SSRF/outbound
// guard because the endpoint is a fixed, trusted host.
var s3HTTPClient = &http.Client{
	Timeout: 20 * time.Second,
	Transport: &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		MaxIdleConns:          32,
		MaxIdleConnsPerHost:   8,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 15 * time.Second,
	},
}

// camS3Enabled reports whether object-storage archiving is configured: endpoint,
// bucket, access key and secret must all be non-empty.
func camS3Enabled(cfg config) bool {
	return strings.TrimSpace(cfg.CameraS3Endpoint) != "" &&
		strings.TrimSpace(cfg.CameraS3Bucket) != "" &&
		strings.TrimSpace(cfg.CameraS3AccessKey) != "" &&
		strings.TrimSpace(cfg.CameraS3Secret) != ""
}

// camS3FrameKey is the deterministic object key for one archived frame: one JPEG
// per second per camera per quality. quality is "main" or "sub". The timestamp is
// laid out as UTC yyyy/mm/dd/hh/mm/ss so keys sort chronologically and prefix-list
// cheaply. The optional prefix comes from the captured config (cameraCfg).
//
//	<prefix><cameraID>/<quality>/2006/01/02/15/04/05.jpg
func camS3FrameKey(cameraID, quality string, t time.Time) string {
	return cameraCfg.CameraS3Prefix + cameraID + "/" + quality + "/" +
		t.UTC().Format("2006/01/02/15/04/05") + ".jpg"
}

// s3Host is the virtual-hosted request host, <bucket>.<endpoint>.
func s3Host(cfg config) string {
	return strings.TrimSpace(cfg.CameraS3Bucket) + "." + strings.TrimSpace(cfg.CameraS3Endpoint)
}

// s3Region resolves the signing region (defaults to hel1).
func s3Region(cfg config) string {
	if r := strings.TrimSpace(cfg.CameraS3Region); r != "" {
		return r
	}
	return "hel1"
}

// s3PutObject uploads body under key with the given Content-Type (defaults to
// application/octet-stream). Succeeds on 200/201.
func s3PutObject(ctx context.Context, cfg config, key string, body []byte, contentType string) error {
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	resp, err := s3DoRequest(ctx, cfg, http.MethodPut, key, body, contentType, false)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusCreated {
		io.Copy(io.Discard, resp.Body)
		return nil
	}
	return s3RespError("PUT", key, resp)
}

// s3PutObjectPublic uploads body under key with a public-read canned ACL so the
// object is anonymously readable at its virtual-hosted URL (camS3PublicURL). Used
// for large evidence clips we want to hand out as a direct link. Succeeds on
// 200/201.
func s3PutObjectPublic(ctx context.Context, cfg config, key string, body []byte, contentType string) error {
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	resp, err := s3DoRequest(ctx, cfg, http.MethodPut, key, body, contentType, true)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusCreated {
		io.Copy(io.Discard, resp.Body)
		return nil
	}
	return s3RespError("PUT(public)", key, resp)
}

// camS3PublicURL is the anonymous, virtual-hosted URL of a public-read object.
func camS3PublicURL(cfg config, key string) string {
	return "https://" + s3Host(cfg) + "/" + s3EscapePath(key)
}

// camS3ClipKey is the object key for an evidence clip:
// <prefix>clips/<site>/<camera>/<UTC yyyy/mm/dd/HHMMSS>.mp4
func camS3ClipKey(siteID, cameraID string, t time.Time) string {
	return cameraCfg.CameraS3Prefix + "clips/" + siteID + "/" + cameraID + "/" +
		t.UTC().Format("2006/01/02/150405") + ".mp4"
}

// s3GetObject fetches key. Returns errS3NotFound on HTTP 404.
func s3GetObject(ctx context.Context, cfg config, key string) ([]byte, error) {
	resp, err := s3DoRequest(ctx, cfg, http.MethodGet, key, nil, "", false)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		return io.ReadAll(resp.Body)
	case http.StatusNotFound:
		io.Copy(io.Discard, resp.Body)
		return nil, errS3NotFound
	default:
		return nil, s3RespError("GET", key, resp)
	}
}

// s3HeadObject reports whether key exists (200 → true, 404 → false).
func s3HeadObject(ctx context.Context, cfg config, key string) (bool, error) {
	resp, err := s3DoRequest(ctx, cfg, http.MethodHead, key, nil, "", false)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	switch resp.StatusCode {
	case http.StatusOK:
		return true, nil
	case http.StatusNotFound:
		return false, nil
	default:
		return false, s3RespError("HEAD", key, resp)
	}
}

// s3DoRequest builds, signs and executes a single request. A nil body means no
// request body (GET/HEAD); its payload hash is that of the empty string.
func s3DoRequest(ctx context.Context, cfg config, method, key string, body []byte, contentType string, aclPublic bool) (*http.Response, error) {
	host := s3Host(cfg)
	rawURL := "https://" + host + "/" + s3EscapePath(key)
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, rawURL, rdr)
	if err != nil {
		return nil, err
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	// sha256(nil) == sha256("") == emptyPayloadSHA256, so this is correct for GET/HEAD.
	payloadHash := sha256Hex(body)
	s3SignRequest(cfg, req, payloadHash, time.Now(), aclPublic)
	return s3HTTPClient.Do(req)
}

// s3SignRequest sets the x-amz-* and Authorization headers on req per SigV4. The
// canonical URI is taken from req.URL.EscapedPath() so the signed path is byte-for-
// byte what goes on the wire.
func s3SignRequest(cfg config, req *http.Request, payloadHash string, t time.Time, aclPublic bool) {
	amzDate := t.UTC().Format("20060102T150405Z")
	host := req.URL.Host
	req.Header.Set("X-Amz-Date", amzDate)
	req.Header.Set("X-Amz-Content-Sha256", payloadHash)
	// Canonical headers, already sorted, each terminated with a newline.
	signedHeaders := s3SignedHeaders
	canonicalHeaders := "host:" + host + "\n" +
		"x-amz-content-sha256:" + payloadHash + "\n" +
		"x-amz-date:" + amzDate + "\n"
	if aclPublic {
		// x-amz-acl must be both sent AND signed; it sorts between host and
		// x-amz-content-sha256.
		req.Header.Set("X-Amz-Acl", "public-read")
		signedHeaders = "host;x-amz-acl;x-amz-content-sha256;x-amz-date"
		canonicalHeaders = "host:" + host + "\n" +
			"x-amz-acl:public-read\n" +
			"x-amz-content-sha256:" + payloadHash + "\n" +
			"x-amz-date:" + amzDate + "\n"
	}
	auth := s3Authorization(
		cfg.CameraS3AccessKey, cfg.CameraS3Secret, s3Region(cfg), s3Service,
		amzDate, req.Method, req.URL.EscapedPath(), req.URL.RawQuery,
		signedHeaders, canonicalHeaders, payloadHash,
	)
	req.Header.Set("Authorization", auth)
}

// s3Authorization builds the full SigV4 Authorization header value. It is a pure
// function (no I/O, no time) so it can be verified against AWS's published SigV4
// test vectors. canonicalHeaders must already be sorted and newline-terminated.
func s3Authorization(accessKey, secret, region, service, amzDate, method, canonicalURI, canonicalQuery, signedHeaders, canonicalHeaders, payloadHash string) string {
	dateStamp := amzDate[:8]
	canonicalRequest := s3CanonicalRequest(method, canonicalURI, canonicalQuery, canonicalHeaders, signedHeaders, payloadHash)
	scope := dateStamp + "/" + region + "/" + service + "/aws4_request"
	stringToSign := strings.Join([]string{
		s3SigAlgorithm,
		amzDate,
		scope,
		sha256Hex([]byte(canonicalRequest)),
	}, "\n")
	signature := hex.EncodeToString(hmacSHA256(s3SigningKey(secret, dateStamp, region, service), stringToSign))
	return s3SigAlgorithm + " Credential=" + accessKey + "/" + scope +
		", SignedHeaders=" + signedHeaders + ", Signature=" + signature
}

// s3CanonicalRequest assembles the SigV4 canonical request. canonicalHeaders is
// expected to end in a newline; the extra "\n" from the join then yields the blank
// line the spec requires between the header block and the signed-headers list.
func s3CanonicalRequest(method, canonicalURI, canonicalQuery, canonicalHeaders, signedHeaders, payloadHash string) string {
	return strings.Join([]string{
		method,
		canonicalURI,
		canonicalQuery,
		canonicalHeaders,
		signedHeaders,
		payloadHash,
	}, "\n")
}

// s3SigningKey derives the SigV4 signing key: HMAC chain over date → region →
// service → "aws4_request", seeded with "AWS4"+secret.
func s3SigningKey(secret, dateStamp, region, service string) []byte {
	kDate := hmacSHA256([]byte("AWS4"+secret), dateStamp)
	kRegion := hmacSHA256(kDate, region)
	kService := hmacSHA256(kRegion, service)
	return hmacSHA256(kService, "aws4_request")
}

// s3EscapePath URI-encodes each "/"-separated segment of key per RFC 3986 (AWS
// UriEncode with encodeSlash=false): unreserved chars pass through, everything else
// becomes %XX (uppercase), and the "/" separators are preserved. The result is the
// object path WITHOUT the leading slash.
func s3EscapePath(key string) string {
	segs := strings.Split(key, "/")
	for i, s := range segs {
		segs[i] = s3EncodeSegment(s)
	}
	return strings.Join(segs, "/")
}

func s3EncodeSegment(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') ||
			c == '-' || c == '.' || c == '_' || c == '~' {
			b.WriteByte(c)
			continue
		}
		b.WriteByte('%')
		b.WriteByte(s3HexUpper[c>>4])
		b.WriteByte(s3HexUpper[c&0x0f])
	}
	return b.String()
}

// s3RespError wraps a non-2xx S3 response for the log. It deliberately does NOT
// echo the raw XML fault body: S3 auth faults (SignatureDoesNotMatch /
// AuthorizationHeaderMalformed) reflect back the AWSAccessKeyId plus the
// StringToSign/CanonicalRequest (which embed the signing metadata). We extract
// only the safe, enum-valued <Code> element (e.g. "NoSuchKey",
// "SignatureDoesNotMatch", "AccessDenied") — never the credential/signing
// fields — and drop the rest so nothing sensitive reaches camlog.
func s3RespError(op, key string, resp *http.Response) error {
	snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
	if code := s3ErrorCode(snippet); code != "" {
		return fmt.Errorf("s3 %s %s: status %d: %s", op, key, resp.StatusCode, code)
	}
	return fmt.Errorf("s3 %s %s: status %d", op, key, resp.StatusCode)
}

// s3ErrorCode extracts the value of the <Code> element from an S3 XML error
// body. The Code is a fixed enum token and never contains credentials; any
// other element (AWSAccessKeyId, StringToSign, CanonicalRequest, …) is ignored.
// Returns "" when no Code is present. The result is length- and charset-capped
// defensively so a hostile/garbled body can't smuggle arbitrary text into logs.
func s3ErrorCode(body []byte) string {
	const openTag, closeTag = "<Code>", "</Code>"
	s := string(body)
	i := strings.Index(s, openTag)
	if i < 0 {
		return ""
	}
	i += len(openTag)
	j := strings.Index(s[i:], closeTag)
	if j < 0 {
		return ""
	}
	code := strings.TrimSpace(s[i : i+j])
	if len(code) > 64 {
		code = code[:64]
	}
	// S3 error codes are simple identifier tokens; reject anything else.
	for _, c := range code {
		if !((c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9')) {
			return ""
		}
	}
	return code
}

// hmacSHA256 returns HMAC-SHA256(key, data).
func hmacSHA256(key []byte, data string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(data))
	return m.Sum(nil)
}

// sha256Hex returns hex(sha256(b)). sha256Hex(nil) is the empty-string hash.
func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
