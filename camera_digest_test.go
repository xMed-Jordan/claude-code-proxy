package main

// camera_digest_test.go — table-driven tests for the hand-rolled HTTP Basic/Digest
// client (camera_digest.go). The centerpiece is httpDigestGet's Digest response
// computation checked against the well-known RFC 2617 §3.5 worked example
// (Mufasa/testrealm@host.com/Circle Of Life), end-to-end through a real
// httptest.Server 401-challenge round trip — not just a reimplementation of the
// hashing in the test. Because httpDigestGet generates a fresh random cnonce per
// call, the response digest can't be pinned to the RFC vector's fixed cnonce; the
// test instead captures the actual Authorization header's cnonce and recomputes
// the expected response with it, while independently asserting the HA1/HA2
// intermediates against the RFC's fixed values (which don't depend on cnonce).

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestHTTPDigestGetRFC2617Vector drives httpDigestGet against a server that
// issues the exact WWW-Authenticate challenge from RFC 2617's worked example and
// checks the resulting Authorization header is a correct Digest response for
// that vector.
func TestHTTPDigestGetRFC2617Vector(t *testing.T) {
	const (
		user  = "Mufasa"
		pass  = "Circle Of Life"
		realm = "testrealm@host.com"
		nonce = "dcd98b7102dd2f0e8b11d0f600bfb0c093"
		path  = "/dir/index.html" // RFC 2617's own example path (no query)

		wantHA1 = "939e7578ed9e3c518a452acee763bce9"
		wantHA2 = "39aff3a2bab6126f332b942af96d3366"
	)

	var authHeader string
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if requests == 1 {
			if r.Header.Get("Authorization") != "" {
				t.Errorf("first request should be unauthenticated, got Authorization=%q", r.Header.Get("Authorization"))
			}
			w.Header().Set("WWW-Authenticate", `Digest realm="`+realm+`", qop="auth", nonce="`+nonce+`", opaque="5ccc069c403ebaf9f0171e9517f40e41"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		authHeader = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	resp, err := httpDigestGet(context.Background(), &http.Client{}, srv.URL+path, user, pass)
	if err != nil {
		t.Fatalf("httpDigestGet: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("final status = %d, want 200", resp.StatusCode)
	}
	if requests != 2 {
		t.Fatalf("requests = %d, want 2 (unauthenticated probe + digest retry)", requests)
	}
	if !strings.HasPrefix(authHeader, "Digest ") {
		t.Fatalf("Authorization header = %q, want Digest scheme", authHeader)
	}

	fields := parseWWWAuthenticate(authHeader)
	if fields["username"] != user {
		t.Errorf("username = %q, want %q", fields["username"], user)
	}
	if fields["realm"] != realm {
		t.Errorf("realm = %q, want %q", fields["realm"], realm)
	}
	if fields["nonce"] != nonce {
		t.Errorf("nonce = %q, want %q", fields["nonce"], nonce)
	}
	if fields["uri"] != path {
		t.Errorf("uri = %q, want %q (no query string in this vector)", fields["uri"], path)
	}
	if fields["nc"] != "00000001" {
		t.Errorf("nc = %q, want 00000001", fields["nc"])
	}
	if fields["qop"] != "auth" {
		t.Errorf("qop = %q, want auth", fields["qop"])
	}
	cnonce := fields["cnonce"]
	if cnonce == "" {
		t.Fatal("cnonce missing from Authorization header")
	}
	gotResponse := fields["response"]
	if gotResponse == "" {
		t.Fatal("response missing from Authorization header")
	}

	// HA1/HA2 don't depend on cnonce — check them against the RFC's fixed values.
	ha1 := md5Hex(user + ":" + realm + ":" + pass)
	if ha1 != wantHA1 {
		t.Fatalf("HA1 = %s, want %s (RFC 2617 vector)", ha1, wantHA1)
	}
	ha2 := md5Hex("GET:" + fields["uri"])
	if ha2 != wantHA2 {
		t.Fatalf("HA2 = %s, want %s (RFC 2617 vector)", ha2, wantHA2)
	}
	// Final response DOES depend on the random cnonce; recompute with the one the
	// client actually sent and confirm it matches what was transmitted.
	wantResponse := md5Hex(strings.Join([]string{ha1, nonce, "00000001", cnonce, "auth", ha2}, ":"))
	if gotResponse != wantResponse {
		t.Errorf("response = %s, want %s (recomputed with captured cnonce %s)", gotResponse, wantResponse, cnonce)
	}
}

// TestHTTPDigestGetURIIncludesQueryString pins the Hikvision-snapshot requirement
// (see camera_digest.go's doc comment): the Digest "uri" field must be the path
// PLUS query string, not just the path, or Hik devices repeat-401.
func TestHTTPDigestGetURIIncludesQueryString(t *testing.T) {
	var authHeader string
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if requests == 1 {
			w.Header().Set("WWW-Authenticate", `Digest realm="cam", qop="auth", nonce="abc123"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		authHeader = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	resp, err := httpDigestGet(context.Background(), &http.Client{}, srv.URL+"/ISAPI/Streaming/channels/101/picture?foo=bar", "admin", "pw")
	if err != nil {
		t.Fatalf("httpDigestGet: %v", err)
	}
	resp.Body.Close()

	fields := parseWWWAuthenticate(authHeader)
	if fields["uri"] != "/ISAPI/Streaming/channels/101/picture?foo=bar" {
		t.Errorf("uri = %q, want path+query string included", fields["uri"])
	}
}

// TestHTTPDigestGetNoQopFallback checks the RFC 2069 (no qop) response formula —
// older Dahua/Hik firmware still issues bare challenges without a qop directive.
func TestHTTPDigestGetNoQopFallback(t *testing.T) {
	const (
		user  = "admin"
		pass  = "pw123"
		realm = "cam"
		nonce = "n0nce-value"
		path  = "/snap.cgi"
	)
	var authHeader string
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if requests == 1 {
			w.Header().Set("WWW-Authenticate", `Digest realm="`+realm+`", nonce="`+nonce+`"`) // no qop
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		authHeader = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	resp, err := httpDigestGet(context.Background(), &http.Client{}, srv.URL+path, user, pass)
	if err != nil {
		t.Fatalf("httpDigestGet: %v", err)
	}
	resp.Body.Close()

	fields := parseWWWAuthenticate(authHeader)
	if fields["qop"] != "" {
		t.Fatalf("qop = %q, want absent for a no-qop challenge", fields["qop"])
	}
	ha1 := md5Hex(user + ":" + realm + ":" + pass)
	ha2 := md5Hex("GET:" + path)
	want := md5Hex(strings.Join([]string{ha1, nonce, ha2}, ":")) // RFC2069: HA1:nonce:HA2 (no nc/cnonce/qop)
	if fields["response"] != want {
		t.Errorf("response = %s, want %s (RFC2069 no-qop formula)", fields["response"], want)
	}
}

// TestHTTPDigestGetBasicFallback checks the Basic-auth path (older Dahua
// firmware, or a bare/absent WWW-Authenticate scheme token).
func TestHTTPDigestGetBasicFallback(t *testing.T) {
	requests := 0
	var authHeader string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if requests == 1 {
			w.Header().Set("WWW-Authenticate", `Basic realm="cam"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		authHeader = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	resp, err := httpDigestGet(context.Background(), &http.Client{}, srv.URL+"/x", "admin", "hunter2")
	if err != nil {
		t.Fatalf("httpDigestGet: %v", err)
	}
	resp.Body.Close()

	want := "Basic " + base64.StdEncoding.EncodeToString([]byte("admin:hunter2"))
	if authHeader != want {
		t.Errorf("Authorization = %q, want %q", authHeader, want)
	}
}

// TestHTTPDigestGetNoChallengeReturnsFirstResponse checks the (common) case where
// the device answers 200 immediately with no auth required at all.
func TestHTTPDigestGetNoChallengeReturnsFirstResponse(t *testing.T) {
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	resp, err := httpDigestGet(context.Background(), &http.Client{}, srv.URL+"/open", "admin", "pw")
	if err != nil {
		t.Fatalf("httpDigestGet: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if requests != 1 {
		t.Errorf("requests = %d, want 1 (no 401 challenge to react to)", requests)
	}
}

// ─────────────────────── parseWWWAuthenticate ───────────────────────

func TestParseWWWAuthenticate(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want map[string]string
	}{
		{
			"digest_full",
			`Digest realm="testrealm@host.com", qop="auth,auth-int", nonce="dcd98b7102dd2f0e8b11d0f600bfb0c093", opaque="5ccc069c403ebaf9f0171e9517f40e41"`,
			map[string]string{"realm": "testrealm@host.com", "qop": "auth,auth-int", "nonce": "dcd98b7102dd2f0e8b11d0f600bfb0c093", "opaque": "5ccc069c403ebaf9f0171e9517f40e41"},
		},
		{
			"basic",
			`Basic realm="cam"`,
			map[string]string{"realm": "cam"},
		},
		{
			"lowercase_scheme",
			`digest realm="x", nonce="y"`,
			map[string]string{"realm": "x", "nonce": "y"},
		},
		{
			"unquoted_values",
			`Digest realm=x, algorithm=MD5, qop=auth`,
			map[string]string{"realm": "x", "algorithm": "MD5", "qop": "auth"},
		},
		{"empty", "", map[string]string{}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := parseWWWAuthenticate(c.in)
			for k, want := range c.want {
				if got[k] != want {
					t.Errorf("[%s] = %q, want %q (full map=%v)", k, got[k], want, got)
				}
			}
		})
	}
}
