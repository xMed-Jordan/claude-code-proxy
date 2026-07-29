package main

// camera_digest.go — hand-rolled HTTP Basic/Digest auth for DVR/NVR hosts.
//
// DVRs live on the private LAN and speak HTTP Digest (Hikvision ISAPI, Dahua CGI)
// or occasionally Basic; the Go stdlib has no Digest client, so we implement it.
//
// SECURITY NOTE: camHTTPClient() is DELIBERATELY guard-free — it must reach
// RFC1918/private addresses, so it does NOT use openMediaURL/isBlockedIP (the
// SSRF-guarded path used for outbound webhooks). What keeps this safe is that DVR
// CRUD is admin-only and we only ever fetch known camera paths. Never point this
// client at a user-supplied URL.

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// camHTTPClient returns a short-timeout client for camera/DVR hosts. It accepts
// self-signed TLS (DVRs ship default certs) and has NO SSRF hook (private IPs are
// the target). Reused across discovery + HTTP snapshots.
func camHTTPClient() *http.Client {
	return &http.Client{
		Timeout: 15 * time.Second,
		Transport: &http.Transport{
			Proxy:                 nil, // never route DVR traffic through an env proxy
			DialContext:           (&net.Dialer{Timeout: 5 * time.Second}).DialContext,
			TLSClientConfig:       &tls.Config{InsecureSkipVerify: true}, // DVRs use default self-signed certs
			ResponseHeaderTimeout: 12 * time.Second,
			MaxIdleConns:          8,
		},
	}
}

// httpDigestGet performs a GET with automatic auth. It sends an unauthenticated
// GET; on 401 it parses WWW-Authenticate and retries with Basic or Digest. For
// Digest it computes HA1=md5(user:realm:pass) (MD5-sess variant when the server
// asks), HA2=md5(GET:uri) with the uri INCLUDING the query string (a Hikvision
// snapshot requirement — the top cause of repeat 401s), and
// response=md5(HA1:nonce:nc:cnonce:qop:HA2) with nc=00000001 and a random cnonce.
// The returned *http.Response body is the caller's to close.
func httpDigestGet(ctx context.Context, client *http.Client, rawURL, user, pass string) (*http.Response, error) {
	return httpDigestDo(ctx, client, http.MethodGet, rawURL, nil, "", user, pass)
}

// httpDigestDo is httpDigestGet generalized over method and request body, for the
// ISAPI endpoints that are POST-only (ContentMgmt/search, ContentMgmt/download).
// body is buffered rather than streamed because the auth handshake replays the
// request after the 401 — ISAPI request bodies are a few hundred bytes of XML.
func httpDigestDo(ctx context.Context, client *http.Client, method, rawURL string, body []byte, contentType, user, pass string) (*http.Response, error) {
	if client == nil {
		client = camHTTPClient()
	}
	req, err := camNewRequest(ctx, method, rawURL, body, contentType)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusUnauthorized {
		return resp, nil
	}
	challenge := strings.TrimSpace(resp.Header.Get("WWW-Authenticate"))
	// Drain + close the 401 body before issuing the authenticated retry.
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()

	scheme := ""
	if challenge != "" {
		scheme = strings.ToLower(strings.Fields(challenge)[0])
	}
	switch {
	case strings.HasPrefix(scheme, "digest"):
		return camDoDigest(ctx, client, method, rawURL, body, contentType, user, pass, challenge)
	case strings.HasPrefix(scheme, "basic"), scheme == "":
		return camDoBasic(ctx, client, method, rawURL, body, contentType, user, pass)
	default:
		// Unknown/negotiate scheme: try Digest, then fall back to Basic.
		if r, derr := camDoDigest(ctx, client, method, rawURL, body, contentType, user, pass, challenge); derr == nil {
			return r, nil
		}
		return camDoBasic(ctx, client, method, rawURL, body, contentType, user, pass)
	}
}

// camNewRequest builds a request with an optional buffered body, so the same
// bytes can be replayed on the authenticated retry.
func camNewRequest(ctx context.Context, method, rawURL string, body []byte, contentType string) (*http.Request, error) {
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, rawURL, rdr)
	if err != nil {
		return nil, err
	}
	if body != nil && contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	return req, nil
}

// camDoBasic retries the request with an Authorization: Basic header.
func camDoBasic(ctx context.Context, client *http.Client, method, rawURL string, body []byte, contentType, user, pass string) (*http.Response, error) {
	req, err := camNewRequest(ctx, method, rawURL, body, contentType)
	if err != nil {
		return nil, err
	}
	req.SetBasicAuth(user, pass)
	return client.Do(req)
}

// camDoDigest retries the request with a computed Digest Authorization header.
func camDoDigest(ctx context.Context, client *http.Client, method, rawURL string, body []byte, contentType, user, pass, challenge string) (*http.Response, error) {
	p := parseWWWAuthenticate(challenge)
	realm := p["realm"]
	nonce := p["nonce"]
	if nonce == "" {
		return nil, fmt.Errorf("digest challenge missing nonce")
	}
	opaque := p["opaque"]
	algorithm := strings.TrimSpace(p["algorithm"]) // preserve case for the echo
	algoLower := strings.ToLower(algorithm)

	// uri MUST include the query string (path?query) — Hik snapshot endpoints
	// reject a path-only uri with a repeat 401.
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, err
	}
	uri := u.RequestURI()

	// Pick a qop token, preferring "auth".
	qop := ""
	for _, cand := range strings.Split(p["qop"], ",") {
		cand = strings.ToLower(strings.TrimSpace(cand))
		if cand == "auth" {
			qop = "auth"
			break
		}
		if cand != "" && qop == "" {
			qop = cand
		}
	}

	nc := "00000001"
	cnonce := camCnonce()

	ha1 := md5Hex(user + ":" + realm + ":" + pass)
	if algoLower == "md5-sess" {
		ha1 = md5Hex(ha1 + ":" + nonce + ":" + cnonce)
	}
	// HA2 depends on qop. Plain "auth" (and RFC 2069) use md5(method:uri); "auth-int"
	// folds in a hash of the entity body (md5("") for a bodyless request). All real
	// Hikvision/Dahua firmware advertises "auth", so the auth-int branch only matters
	// for an auth-int-only device (which would otherwise keep returning 401 from a
	// wrong response digest).
	ha2 := md5Hex(method + ":" + uri)
	if qop == "auth-int" {
		ha2 = md5Hex(method + ":" + uri + ":" + md5Hex(string(body)))
	}

	var response string
	if qop == "auth" || qop == "auth-int" {
		response = md5Hex(strings.Join([]string{ha1, nonce, nc, cnonce, qop, ha2}, ":"))
	} else {
		// RFC 2069 (no qop) fallback for older firmware.
		response = md5Hex(strings.Join([]string{ha1, nonce, ha2}, ":"))
	}

	var h strings.Builder
	fmt.Fprintf(&h, `Digest username=%q, realm=%q, nonce=%q, uri=%q, response=%q`,
		user, realm, nonce, uri, response)
	if algorithm != "" {
		fmt.Fprintf(&h, ", algorithm=%s", algorithm)
	}
	if qop != "" {
		fmt.Fprintf(&h, ", qop=%s, nc=%s, cnonce=%q", qop, nc, cnonce)
	}
	if opaque != "" {
		fmt.Fprintf(&h, ", opaque=%q", opaque)
	}

	req, err := camNewRequest(ctx, method, rawURL, body, contentType)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", h.String())
	return client.Do(req)
}

// parseWWWAuthenticate parses the parameters of a WWW-Authenticate header into a
// lowercase-keyed map, tolerating quoted values, embedded commas inside quotes,
// and a leading scheme token (Digest/Basic). Values are unquoted.
func parseWWWAuthenticate(h string) map[string]string {
	out := map[string]string{}
	h = strings.TrimSpace(h)
	if h == "" {
		return out
	}
	// Strip a leading scheme token if present.
	if i := strings.IndexAny(h, " \t"); i >= 0 {
		first := strings.ToLower(strings.TrimSpace(h[:i]))
		if first == "digest" || first == "basic" {
			h = h[i+1:]
		}
	}
	// Split on commas that are not inside a quoted string.
	var parts []string
	var cur strings.Builder
	inQuote := false
	for i := 0; i < len(h); i++ {
		c := h[i]
		switch {
		case c == '"':
			inQuote = !inQuote
			cur.WriteByte(c)
		case c == ',' && !inQuote:
			parts = append(parts, cur.String())
			cur.Reset()
		default:
			cur.WriteByte(c)
		}
	}
	if cur.Len() > 0 {
		parts = append(parts, cur.String())
	}
	for _, part := range parts {
		part = strings.TrimSpace(part)
		eq := strings.Index(part, "=")
		if eq < 0 {
			continue
		}
		key := strings.ToLower(strings.TrimSpace(part[:eq]))
		val := strings.TrimSpace(part[eq+1:])
		val = strings.Trim(val, `"`)
		if key != "" {
			out[key] = val
		}
	}
	return out
}

func md5Hex(s string) string {
	sum := md5.Sum([]byte(s))
	return hex.EncodeToString(sum[:])
}

// camCnonce returns a random 16-hex-char client nonce (crypto source, time
// fallback if the RNG is unavailable).
func camCnonce() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%016x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}
