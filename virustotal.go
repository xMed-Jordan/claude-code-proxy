package main

// virustotal.go — optional malware scanning for media attachments.
//
// Every materialized file is scanned with the VirusTotal v3 API before agy is
// allowed to open it. Multiple API keys are load-balanced round-robin; a key
// that errors (rate limit / auth / network) is parked for one hour and skipped.
// Scanning is hash-first (instant verdict for files VT already knows) and falls
// back to upload+poll for unknown files when enabled. The key list is editable
// at runtime from the control panel (/virustotal), no restart required.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

func parseIntDefault(s string, def int) int {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil || n < 0 {
		return def
	}
	return n
}

const (
	vtBaseURL      = "https://www.virustotal.com/api/v3"
	vtCooldownDur  = time.Hour
	vtUploadLimit  = 32 << 20 // /files endpoint accepts up to 32 MB
	vtPollInterval = 5 * time.Second
)

var (
	vtMu        sync.Mutex
	vtCooldown  = map[string]time.Time{} // key → parked-until
	vtRotation  int
	vtRuntimeMu sync.RWMutex
	vtKeysRT    string
	vtEnabledRT bool
)

// initVT seeds the runtime key list/enabled flag from config at startup.
func initVT(cfg config) {
	setVTRuntime(cfg.VirusTotalKeys, cfg.VirusTotalEnabled)
}

func setVTRuntime(keys string, enabled bool) {
	vtRuntimeMu.Lock()
	vtKeysRT = keys
	vtEnabledRT = enabled
	vtRuntimeMu.Unlock()
}

func getVTRuntime() (string, bool) {
	vtRuntimeMu.RLock()
	defer vtRuntimeMu.RUnlock()
	return vtKeysRT, vtEnabledRT
}

// parseVTKeys splits a keys blob (newline / comma / whitespace separated),
// trims, de-dupes, and preserves order.
func parseVTKeys(raw string) []string {
	fields := strings.FieldsFunc(raw, func(r rune) bool {
		return r == '\n' || r == '\r' || r == ',' || r == ' ' || r == '\t' || r == ';'
	})
	seen := map[string]bool{}
	var keys []string
	for _, f := range fields {
		k := strings.TrimSpace(f)
		if k == "" || seen[k] {
			continue
		}
		seen[k] = true
		keys = append(keys, k)
	}
	return keys
}

// vtPickKey returns the next available key in round-robin order, skipping parked
// ones. ok=false means every key is currently cooling down.
func vtPickKey(keys []string) (string, bool) {
	if len(keys) == 0 {
		return "", false
	}
	vtMu.Lock()
	defer vtMu.Unlock()
	now := time.Now()
	n := len(keys)
	for i := 0; i < n; i++ {
		idx := (vtRotation + i) % n
		k := keys[idx]
		if until, parked := vtCooldown[k]; parked && now.Before(until) {
			continue
		}
		vtRotation = (idx + 1) % n
		return k, true
	}
	return "", false
}

func vtParkKey(key string) {
	vtMu.Lock()
	vtCooldown[key] = time.Now().Add(vtCooldownDur)
	vtMu.Unlock()
}

// vtKeyStatuses reports each key (masked) and whether it is active or cooling
// down, for the control-panel page.
func vtKeyStatuses(keys []string) []map[string]any {
	vtMu.Lock()
	defer vtMu.Unlock()
	now := time.Now()
	out := make([]map[string]any, 0, len(keys))
	for _, k := range keys {
		status := "active"
		var until string
		if t, parked := vtCooldown[k]; parked && now.Before(t) {
			status = "cooling"
			until = t.UTC().Format(time.RFC3339)
		}
		out = append(out, map[string]any{"masked": maskSecret(k), "status": status, "cooling_until": until})
	}
	return out
}

// ── verdict ──────────────────────────────────────────────────────────────────

type vtStats struct {
	Malicious  int
	Suspicious int
	Harmless   int
	Undetected int
}

type vtVerdict struct {
	Scanned   bool
	KnownToVT bool
	SHA256    string
	Stats     vtStats
}

func (v vtVerdict) isMalicious(threshold int) bool {
	if threshold < 1 {
		threshold = 1
	}
	return v.Stats.Malicious >= threshold
}

// scanFileVT scans the file at path. A zero verdict with Scanned=false means
// scanning was disabled, no keys were available (and not fail-closed), or the
// file is unknown to VT and uploads are off.
func scanFileVT(ctx context.Context, cfg config, path, sha, md5hex string) (vtVerdict, error) {
	rawKeys, enabled := getVTRuntime()
	keys := parseVTKeys(rawKeys)
	if !enabled || len(keys) == 0 {
		return vtVerdict{Scanned: false}, nil
	}
	var size int64
	if fi, err := os.Stat(path); err == nil {
		size = fi.Size()
	}
	v := vtVerdict{SHA256: sha}
	threshold := vtThreshold(cfg)

	// Local cache: a previously-seen file skips VirusTotal entirely. A malicious
	// verdict is honored forever; a clean one until it goes stale.
	if found, engines, atMS := scanCacheLookup(sha); found {
		if engines >= threshold || (atMS > 0 && time.Now().UnixMilli()-atMS < cleanCacheTTLms) {
			v.Scanned, v.KnownToVT, v.Stats.Malicious = true, true, engines
			return v, nil
		}
	}

	exhausted := true
	for attempt := 0; attempt < len(keys); attempt++ {
		key, ok := vtPickKey(keys)
		if !ok {
			exhausted = false // all parked, not "tried and failed"
			break
		}
		stats, status, lerr := vtLookupHash(ctx, key, sha)
		switch {
		case lerr == nil && status == http.StatusOK:
			v.Scanned, v.KnownToVT, v.Stats = true, true, stats
			scanCacheStore(sha, md5hex, stats.Malicious, time.Now().UnixMilli())
			return v, nil
		case status == http.StatusNotFound:
			if cfg.VirusTotalUpload && size <= vtUploadLimit {
				stats2, uerr := vtUploadAndWait(ctx, cfg, key, path)
				if uerr == nil {
					v.Scanned, v.KnownToVT, v.Stats = true, true, stats2
					scanCacheStore(sha, md5hex, stats2.Malicious, time.Now().UnixMilli())
					return v, nil
				}
				vtParkKey(key)
				continue
			}
			// Unknown to VT and not uploading → treat as scanned-but-unknown.
			v.Scanned, v.KnownToVT = true, false
			return v, nil
		default:
			// 401/403 (bad key), 429 (rate limit), 5xx, or transport error.
			vtParkKey(key)
			continue
		}
	}

	if cfg.VirusTotalFailClosed {
		if exhausted {
			return vtVerdict{}, fmt.Errorf("virus scan unavailable (all VirusTotal keys errored)")
		}
		return vtVerdict{}, fmt.Errorf("virus scan unavailable (all VirusTotal keys cooling down)")
	}
	return vtVerdict{Scanned: false}, nil
}

const cleanCacheTTLms = int64(7 * 24 * 60 * 60 * 1000) // re-scan "clean" files after 7 days

// scanCacheLookup returns a cached verdict for a file SHA-256, if present.
func scanCacheLookup(sha string) (found bool, engines int, scannedAtMS int64) {
	if metricsDB == nil {
		return false, 0, 0
	}
	row := metricsDB.QueryRow(`SELECT malicious_engines, scanned_at_ms FROM file_scan_cache WHERE sha256 = ?`, sha)
	if err := row.Scan(&engines, &scannedAtMS); err != nil {
		return false, 0, 0
	}
	return true, engines, scannedAtMS
}

// scanCacheStore records a verdict (malicious-engine count) for a file so a
// re-upload of the same bytes skips VirusTotal.
func scanCacheStore(sha, md5hex string, engines int, nowMS int64) {
	if metricsDB == nil || sha == "" {
		return
	}
	_, _ = metricsDB.Exec(
		`INSERT INTO file_scan_cache (sha256, md5, malicious_engines, scanned_at_ms) VALUES (?, ?, ?, ?)
		 ON CONFLICT(sha256) DO UPDATE SET md5=excluded.md5, malicious_engines=excluded.malicious_engines, scanned_at_ms=excluded.scanned_at_ms`,
		sha, md5hex, engines, nowMS,
	)
}

// ── VT API calls ─────────────────────────────────────────────────────────────

func vtClient() *http.Client { return &http.Client{Timeout: 30 * time.Second} }

func vtParseStats(body []byte) (vtStats, bool) {
	var resp struct {
		Data struct {
			Attributes struct {
				LastAnalysisStats vtStats `json:"-"`
				Stats             struct {
					Malicious  int `json:"malicious"`
					Suspicious int `json:"suspicious"`
					Harmless   int `json:"harmless"`
					Undetected int `json:"undetected"`
				} `json:"last_analysis_stats"`
			} `json:"attributes"`
		} `json:"data"`
	}
	if json.Unmarshal(body, &resp) != nil {
		return vtStats{}, false
	}
	s := resp.Data.Attributes.Stats
	return vtStats{Malicious: s.Malicious, Suspicious: s.Suspicious, Harmless: s.Harmless, Undetected: s.Undetected}, true
}

func vtLookupHash(ctx context.Context, key, sha string) (vtStats, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, vtBaseURL+"/files/"+sha, nil)
	if err != nil {
		return vtStats{}, 0, err
	}
	req.Header.Set("x-apikey", key)
	resp, err := vtClient().Do(req)
	if err != nil {
		return vtStats{}, 0, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return vtStats{}, resp.StatusCode, nil
	}
	stats, _ := vtParseStats(body)
	return stats, resp.StatusCode, nil
}

func vtUpload(ctx context.Context, key, path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile("file", "upload.bin")
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(fw, f); err != nil {
		return "", err
	}
	mw.Close()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, vtBaseURL+"/files", &buf)
	if err != nil {
		return "", err
	}
	req.Header.Set("x-apikey", key)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := (&http.Client{Timeout: 120 * time.Second}).Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("vt upload status %d", resp.StatusCode)
	}
	var r struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if json.Unmarshal(body, &r) != nil || r.Data.ID == "" {
		return "", fmt.Errorf("vt upload: no analysis id")
	}
	return r.Data.ID, nil
}

func vtUploadAndWait(ctx context.Context, cfg config, key, path string) (vtStats, error) {
	id, err := vtUpload(ctx, key, path)
	if err != nil {
		return vtStats{}, err
	}
	maxWait := cfg.VirusTotalMaxWait
	if maxWait <= 0 {
		maxWait = 90 * time.Second
	}
	deadline := time.Now().Add(maxWait)
	for {
		stats, done, err := vtAnalysis(ctx, key, id)
		if err != nil {
			return vtStats{}, err
		}
		if done {
			return stats, nil
		}
		if time.Now().After(deadline) {
			return vtStats{}, fmt.Errorf("vt analysis timed out")
		}
		select {
		case <-ctx.Done():
			return vtStats{}, ctx.Err()
		case <-time.After(vtPollInterval):
		}
	}
}

func vtAnalysis(ctx context.Context, key, id string) (vtStats, bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, vtBaseURL+"/analyses/"+id, nil)
	if err != nil {
		return vtStats{}, false, err
	}
	req.Header.Set("x-apikey", key)
	resp, err := vtClient().Do(req)
	if err != nil {
		return vtStats{}, false, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return vtStats{}, false, fmt.Errorf("vt analysis status %d", resp.StatusCode)
	}
	var r struct {
		Data struct {
			Attributes struct {
				Status string `json:"status"`
				Stats  struct {
					Malicious  int `json:"malicious"`
					Suspicious int `json:"suspicious"`
					Harmless   int `json:"harmless"`
					Undetected int `json:"undetected"`
				} `json:"stats"`
			} `json:"attributes"`
		} `json:"data"`
	}
	if json.Unmarshal(body, &r) != nil {
		return vtStats{}, false, fmt.Errorf("vt analysis: bad json")
	}
	s := r.Data.Attributes.Stats
	stats := vtStats{Malicious: s.Malicious, Suspicious: s.Suspicious, Harmless: s.Harmless, Undetected: s.Undetected}
	return stats, r.Data.Attributes.Status == "completed", nil
}

// vtThreshold returns the malicious-engine count at/above which a file is
// rejected (default 1).
func vtThreshold(cfg config) int {
	if cfg.VirusTotalThreshold > 0 {
		return cfg.VirusTotalThreshold
	}
	return 1
}

// sortedVTKeys is a deterministic view for tests/UI.
func sortedVTKeys(raw string) []string {
	keys := parseVTKeys(raw)
	sort.Strings(keys)
	return keys
}
