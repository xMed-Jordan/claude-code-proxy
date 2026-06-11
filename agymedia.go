package main

// agymedia.go — media support for the "agy" upstream.
//
// agy (Antigravity CLI) can read rich media (images, audio, video, PDFs, Office
// docs, archives) by shelling out to local tools. When an agy-routed request
// carries attachments, we decode/download them into a per-request scratch dir,
// hand agy the paths (via --add-dir + --dangerously-skip-permissions), and let
// it analyze them. Everything here is hardened: files are written non-executable
// (0644), archive extraction is zip-slip-safe with size/count caps, and remote
// downloads are SSRF-guarded. See the security notes in the plan.

import (
	"archive/tar"
	"archive/zip"
	"compress/bzip2"
	"compress/gzip"
	"context"
	"crypto/md5"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

const (
	maxArchiveEntries  = 2000
	defaultMediaPerMax = 1 << 30 // 1 GB per file (configurable)
	defaultMediaTotMax = 1 << 30 // 1 GB extracted total (configurable)
)

// mediaPart is a normalized attachment reference pulled from a request: exactly
// one of B64 (raw base64, no data: prefix) or URL is set. MD5 (hex), when the
// client supplies it, is verified after the bytes land so large uploads can be
// checked for completeness/corruption.
type mediaPart struct {
	B64       string
	URL       string
	MediaType string
	Filename  string
	MD5       string
}

// mediaItem is a materialized file (or extracted dir) on disk for agy to read.
type mediaItem struct {
	Path string
	Name string
	Kind string // image/audio/video/pdf/office/text/archive/other
}

// ── extraction from request content ─────────────────────────────────────────

func collectAnthropicMedia(in anthropicRequest) []mediaPart {
	var parts []mediaPart
	for _, msg := range in.Messages {
		parts = append(parts, collectMediaFromContent(msg.Content)...)
	}
	return parts
}

func collectOpenAIChatMedia(in openAIRequest) []mediaPart {
	var parts []mediaPart
	for _, msg := range in.Messages {
		parts = append(parts, collectMediaFromContent(msg.Content)...)
	}
	return parts
}

func collectResponsesMedia(in responsesRequest) []mediaPart {
	var parts []mediaPart
	for _, raw := range in.Input {
		if m, ok := raw.(map[string]any); ok {
			parts = append(parts, collectMediaFromContent(m["content"])...)
		}
	}
	return parts
}

func collectMediaFromContent(content any) []mediaPart {
	blocks, ok := content.([]any)
	if !ok {
		return nil
	}
	var parts []mediaPart
	for _, b := range blocks {
		if block, ok := b.(map[string]any); ok {
			if p, ok := mediaPartFromBlock(block); ok {
				parts = append(parts, p)
			}
		}
	}
	return parts
}

func mediaPartFromBlock(block map[string]any) (mediaPart, bool) {
	switch strings.ToLower(stringField(block, "type")) {
	case "text", "input_text", "output_text", "tool_use", "tool_result", "thinking", "redacted_thinking":
		return mediaPart{}, false
	}
	// Optional client-supplied checksum (verified after the bytes land).
	md5hex := stringField(block, "md5", "checksum", "content_md5", "hash")
	apply := func(p mediaPart) mediaPart {
		if p.MD5 == "" {
			p.MD5 = md5hex
		}
		return p
	}
	// OpenAI image_url (url may be a data: URL or http(s))
	if iu, ok := block["image_url"]; ok {
		if u := strings.TrimSpace(openAIImageURL(iu)); u != "" {
			return apply(mediaPartFromRef(u, "", "")), true
		}
	}
	// OpenAI input_audio
	if ia, ok := block["input_audio"].(map[string]any); ok {
		if data := strings.TrimSpace(stringField(ia, "data")); data != "" {
			format := strings.TrimSpace(stringField(ia, "format"))
			if format == "" {
				format = "mp3"
			}
			b64, mt := splitInlineData(data)
			return apply(mediaPart{B64: b64, MediaType: firstNonEmpty(mt, "audio/"+format), Filename: "audio." + format, MD5: stringField(ia, "md5")}), true
		}
	}
	// Anthropic image/document/file, or OpenAI file block — via source/file object.
	if _, hasSource := block["source"]; hasSource {
		if p, ok := mediaPartFromSource(block); ok {
			return apply(p), true
		}
	}
	if _, hasFile := block["file"]; hasFile {
		if p, ok := mediaPartFromSource(block); ok {
			return apply(p), true
		}
	}
	return mediaPart{}, false
}

func mediaPartFromSource(block map[string]any) (mediaPart, bool) {
	src := anthropicBlockSource(block)
	filename := anthropicFileName(block, src)
	mediaType := anthropicSourceMediaType(block, src)
	md5hex := stringField(src, "md5", "checksum", "content_md5", "hash")
	if data := strings.TrimSpace(stringField(src, "data", "file_data")); data != "" {
		b64, mt := splitInlineData(data)
		return mediaPart{B64: b64, MediaType: firstNonEmpty(mt, mediaType), Filename: filename, MD5: md5hex}, true
	}
	if u := strings.TrimSpace(stringField(src, "url", "file_uri", "uri")); u != "" {
		p := mediaPartFromRef(u, mediaType, filename)
		p.MD5 = md5hex
		return p, true
	}
	return mediaPart{}, false
}

func mediaPartFromRef(ref, mediaType, filename string) mediaPart {
	if strings.HasPrefix(ref, "data:") {
		b64, mt := splitInlineData(ref)
		return mediaPart{B64: b64, MediaType: firstNonEmpty(mt, mediaType), Filename: filename}
	}
	return mediaPart{URL: ref, MediaType: mediaType, Filename: filename}
}

// splitInlineData parses a possible data: URL into (base64, mediaType). A plain
// base64 string (no data: prefix) is returned unchanged with an empty mediaType.
func splitInlineData(s string) (string, string) {
	if !strings.HasPrefix(s, "data:") {
		return s, ""
	}
	comma := strings.Index(s, ",")
	if comma < 0 {
		return s, ""
	}
	header := s[len("data:"):comma]
	mt := header
	if i := strings.Index(header, ";"); i >= 0 {
		mt = header[:i]
	}
	return s[comma+1:], strings.TrimSpace(mt)
}

// ── extension / kind mapping ─────────────────────────────────────────────────

var mimeExt = map[string]string{
	"image/jpeg": ".jpg", "image/jpg": ".jpg", "image/png": ".png", "image/webp": ".webp",
	"image/gif": ".gif", "image/bmp": ".bmp", "image/tiff": ".tiff", "image/heic": ".heic",
	"audio/mpeg": ".mp3", "audio/mp3": ".mp3", "audio/mp4": ".m4a", "audio/x-m4a": ".m4a",
	"audio/aac": ".aac", "audio/wav": ".wav", "audio/x-wav": ".wav", "audio/ogg": ".ogg",
	"audio/flac": ".flac", "audio/opus": ".opus",
	"video/mp4": ".mp4", "video/quicktime": ".mov", "video/x-matroska": ".mkv",
	"video/webm": ".webm", "video/x-msvideo": ".avi",
	"application/pdf":    ".pdf",
	"application/msword": ".doc",
	"application/vnd.openxmlformats-officedocument.wordprocessingml.document": ".docx",
	"application/vnd.ms-excel": ".xls",
	"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet":         ".xlsx",
	"application/vnd.ms-powerpoint":                                             ".ppt",
	"application/vnd.openxmlformats-officedocument.presentationml.presentation": ".pptx",
	"text/csv": ".csv", "text/plain": ".txt", "text/markdown": ".md",
	"application/zip": ".zip", "application/x-zip-compressed": ".zip",
	"application/x-tar": ".tar", "application/gzip": ".gz", "application/x-gzip": ".gz",
	"application/x-bzip2": ".bz2", "application/x-xz": ".xz",
	"application/x-rar-compressed": ".rar", "application/vnd.rar": ".rar",
	"application/x-7z-compressed": ".7z",
}

func mediaExtFor(mediaType, filename string) string {
	if ext := strings.ToLower(filepath.Ext(strings.TrimSpace(filename))); ext != "" && len(ext) <= 6 {
		return ext
	}
	mt := strings.ToLower(strings.TrimSpace(mediaType))
	if i := strings.Index(mt, ";"); i >= 0 {
		mt = strings.TrimSpace(mt[:i])
	}
	if ext, ok := mimeExt[mt]; ok {
		return ext
	}
	switch {
	case strings.HasPrefix(mt, "image/"):
		return "." + strings.TrimPrefix(mt, "image/")
	case strings.HasPrefix(mt, "audio/"):
		return "." + strings.TrimPrefix(mt, "audio/")
	case strings.HasPrefix(mt, "video/"):
		return "." + strings.TrimPrefix(mt, "video/")
	case strings.HasPrefix(mt, "text/"):
		return ".txt"
	}
	return ".bin"
}

func isArchiveExt(ext string) bool {
	switch strings.ToLower(ext) {
	case ".zip", ".tar", ".gz", ".tgz", ".bz2", ".tbz", ".tbz2", ".xz", ".txz", ".rar", ".7z":
		return true
	}
	return false
}

func mediaKindFor(ext string) string {
	switch strings.ToLower(ext) {
	case ".png", ".jpg", ".jpeg", ".webp", ".gif", ".bmp", ".tiff", ".tif", ".heic":
		return "image"
	case ".mp3", ".m4a", ".wav", ".ogg", ".aac", ".flac", ".opus":
		return "audio"
	case ".mp4", ".mov", ".mkv", ".webm", ".avi", ".m4v":
		return "video"
	case ".pdf":
		return "pdf"
	case ".docx", ".doc", ".xlsx", ".xls", ".pptx", ".ppt", ".csv":
		return "office"
	case ".txt", ".md", ".json", ".xml", ".html":
		return "text"
	}
	if isArchiveExt(ext) {
		return "archive"
	}
	return "other"
}

// ── scratch lifecycle ────────────────────────────────────────────────────────

func agyMediaRoot(cfg config) string {
	if d := strings.TrimSpace(cfg.AgyMediaDir); d != "" {
		return d
	}
	return filepath.Join(os.TempDir(), "connect-ai-proxy-media")
}

func mediaPerFileMax(cfg config) int64 {
	if cfg.AgyMediaMaxBytes > 0 {
		return cfg.AgyMediaMaxBytes
	}
	return defaultMediaPerMax
}

func mediaTotalMax(cfg config) int64 {
	if cfg.AgyMediaMaxTotal > 0 {
		return cfg.AgyMediaMaxTotal
	}
	return defaultMediaTotMax
}

// ── materialization (content-addressed, retained for reuse) ──────────────────

// materializeMedia writes each attachment under root/f-<sha256>/ so an identical
// re-upload reuses the already-decoded / virus-scanned / extracted files (no
// repeat work) for follow-up questions. Files are retained (reaped after the
// retention window), not deleted per request. Returns the items plus the unique
// content dirs to hand agy via --add-dir (scoping it to just this request's files).
func materializeMedia(ctx context.Context, cfg config, root string, parts []mediaPart) ([]mediaItem, []string, error) {
	perMax := mediaPerFileMax(cfg)
	totMax := mediaTotalMax(cfg)
	dlTimeout := cfg.AgyMediaTimeout
	if dlTimeout <= 0 {
		dlTimeout = 300 * time.Second
	}
	var total int64
	var items []mediaItem
	var addDirs []string
	seenDir := map[string]bool{}
	now := time.Now()

	for i, p := range parts {
		ext := mediaExtFor(p.MediaType, p.Filename)
		if p.URL != "" && filepath.Ext(p.Filename) == "" {
			if u, e := url.Parse(p.URL); e == nil {
				if ue := strings.ToLower(filepath.Ext(u.Path)); ue != "" && len(ue) <= 6 {
					ext = ue
				}
			}
		}
		name := safeMediaName(p.Filename, i, ext)

		// Stream into a unique staging file so big files never buffer in RAM and
		// we can content-address by the resulting hash.
		tmp, err := os.CreateTemp(root, "staging-*.tmp")
		if err != nil {
			return nil, nil, err
		}
		staging := tmp.Name()
		tmp.Close()

		var src io.Reader
		var closer io.Closer
		var cancel context.CancelFunc
		switch {
		case p.URL != "":
			if !cfg.AgyMediaAllowURLs {
				_ = os.Remove(staging)
				return nil, nil, fmt.Errorf("remote media URLs are disabled")
			}
			dctx, c := context.WithTimeout(ctx, dlTimeout)
			body, derr := openMediaURL(dctx, p.URL, perMax)
			if derr != nil {
				c()
				_ = os.Remove(staging)
				return nil, nil, derr
			}
			src, closer, cancel = body, body, c
		case p.B64 != "":
			src = base64.NewDecoder(base64.StdEncoding, newB64CleanReader(p.B64))
		default:
			_ = os.Remove(staging)
			continue
		}

		sha, md5sum, werr := saveStreamCapped(staging, src, perMax, totMax, &total)
		if closer != nil {
			closer.Close()
		}
		if cancel != nil {
			cancel()
		}
		if werr != nil {
			_ = os.Remove(staging)
			if p.B64 != "" && strings.Contains(werr.Error(), "illegal base64") {
				return nil, nil, fmt.Errorf("invalid base64 media for %s", firstNonEmpty(p.Filename, name))
			}
			return nil, nil, werr
		}
		if exp := strings.TrimSpace(p.MD5); exp != "" && !strings.EqualFold(exp, md5sum) {
			_ = os.Remove(staging)
			return nil, nil, fmt.Errorf("upload incomplete: md5 mismatch for %s (expected %s, got %s)", firstNonEmpty(p.Filename, name), exp, md5sum)
		}

		cacheDir := filepath.Join(root, "f-"+sha)
		finalPath := filepath.Join(cacheDir, name)
		reused := false

		if fi, e := os.Stat(finalPath); e == nil && !fi.IsDir() {
			// Identical file already materialized (and previously scanned clean) —
			// reuse it and bump the dir's mtime so retention is extended.
			_ = os.Remove(staging)
			_ = os.Chtimes(cacheDir, now, now)
			reused = true
		} else {
			// Scan the staged bytes before committing them to the cache. A
			// malicious file is never committed and agy is never run on it.
			if verdict, serr := scanFileVT(ctx, cfg, staging, sha, md5sum); serr != nil {
				_ = os.Remove(staging)
				return nil, nil, fmt.Errorf("could not virus-scan %s: %v", firstNonEmpty(p.Filename, name), serr)
			} else if verdict.isMalicious(vtThreshold(cfg)) {
				_ = os.Remove(staging)
				return nil, nil, fmt.Errorf("attachment %s was flagged as malicious by VirusTotal (%d engines)", firstNonEmpty(p.Filename, name), verdict.Stats.Malicious)
			}
			if err := os.MkdirAll(cacheDir, 0o700); err != nil {
				_ = os.Remove(staging)
				return nil, nil, err
			}
			if err := os.Rename(staging, finalPath); err != nil {
				_ = os.Remove(staging)
				if _, e2 := os.Stat(finalPath); e2 != nil { // not a concurrent-create race
					return nil, nil, err
				}
			}
			_ = os.Chmod(finalPath, 0o644)
		}

		itemPath, itemKind := finalPath, mediaKindFor(ext)
		if itemKind == "archive" {
			exDir := filepath.Join(cacheDir, "extract")
			if _, e := os.Stat(exDir); e != nil {
				if err := os.MkdirAll(exDir, 0o755); err == nil {
					if eerr := extractArchive(finalPath, exDir, &total, totMax); eerr == nil {
						sanitizeTree(exDir)
						itemPath = exDir
					} else {
						_ = os.RemoveAll(exDir) // leave only the raw archive
					}
				}
			} else {
				itemPath = exDir // already extracted on a prior request
			}
		}

		// Write a markdown map so the file's location (and any extracted archive
		// tree) is always discoverable for follow-up questions.
		if !reused {
			writeMediaManifest(cacheDir, p, name, finalPath, itemKind, itemPath, sha)
		}

		items = append(items, mediaItem{Path: itemPath, Name: firstNonEmpty(p.Filename, name), Kind: itemKind})
		if !seenDir[cacheDir] {
			seenDir[cacheDir] = true
			addDirs = append(addDirs, cacheDir)
		}
	}
	return items, addDirs, nil
}

// writeMediaManifest writes <cacheDir>/_map.md describing the materialized file
// and, for archives, the full list of extracted files and their paths.
func writeMediaManifest(cacheDir string, p mediaPart, name, finalPath, kind, itemPath, sha string) {
	var b strings.Builder
	b.WriteString("# Attached file map\n\n")
	b.WriteString("- Original name: " + firstNonEmpty(p.Filename, name) + "\n")
	b.WriteString("- Type: " + kind + "\n")
	b.WriteString("- SHA-256: " + sha + "\n")
	b.WriteString("- File on disk: " + finalPath + "\n")
	if p.URL != "" {
		b.WriteString("- Source URL: " + p.URL + "\n")
	}
	if kind == "archive" && itemPath != finalPath {
		b.WriteString("\n## Extracted contents\n\nExtracted to: " + itemPath + "\n\n")
		count := 0
		_ = filepath.WalkDir(itemPath, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || count >= maxArchiveEntries {
				return nil
			}
			rel, rerr := filepath.Rel(itemPath, path)
			if rerr != nil {
				rel = path
			}
			b.WriteString("- " + rel + "  →  " + path + "\n")
			count++
			return nil
		})
	}
	_ = os.WriteFile(filepath.Join(cacheDir, "_map.md"), []byte(b.String()), 0o644)
}

// startMediaReaper periodically removes content dirs (and stale staging files)
// older than the retention window so files persist for follow-up questions but
// don't accumulate forever.
func startMediaReaper(cfg config) {
	if !cfg.AgyMedia {
		return
	}
	root := agyMediaRoot(cfg)
	retention := cfg.AgyMediaRetention
	if retention <= 0 {
		retention = 24 * time.Hour
	}
	go func() {
		ticker := time.NewTicker(time.Hour)
		defer ticker.Stop()
		reapMediaRoot(root, retention)
		for range ticker.C {
			reapMediaRoot(root, retention)
		}
	}()
}

func reapMediaRoot(root string, retention time.Duration) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	now := time.Now()
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			continue
		}
		full := filepath.Join(root, e.Name())
		if !e.IsDir() {
			// abandoned staging temp files: clear after an hour
			if strings.HasPrefix(e.Name(), "staging-") && now.Sub(info.ModTime()) > time.Hour {
				_ = os.Remove(full)
			}
			continue
		}
		if strings.HasPrefix(e.Name(), "f-") && now.Sub(info.ModTime()) > retention {
			_ = os.RemoveAll(full)
		}
	}
}

// saveStreamCapped streams src to dest (0644), enforcing the per-file and
// remaining-total byte caps, and returns the hex SHA-256 (content-address key)
// and MD5 (completeness check) of the written bytes.
func saveStreamCapped(dest string, src io.Reader, perMax, totMax int64, total *int64) (sha string, md5hex string, err error) {
	remaining := totMax - *total
	limit := perMax
	if remaining < limit {
		limit = remaining
	}
	if limit < 0 {
		limit = 0
	}
	f, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return "", "", err
	}
	hs := sha256.New()
	hm := md5.New()
	n, copyErr := io.Copy(io.MultiWriter(f, hs, hm), io.LimitReader(src, limit+1))
	closeErr := f.Close()
	if copyErr != nil {
		return "", "", copyErr
	}
	if closeErr != nil {
		return "", "", closeErr
	}
	if n > limit {
		if remaining <= perMax {
			return "", "", fmt.Errorf("attached files exceed the %d MB total limit", totMax>>20)
		}
		return "", "", fmt.Errorf("attached file exceeds the %d MB limit", perMax>>20)
	}
	*total += n
	_ = os.Chmod(dest, 0o644)
	return hex.EncodeToString(hs.Sum(nil)), hex.EncodeToString(hm.Sum(nil)), nil
}

// newB64CleanReader strips whitespace so base64.NewDecoder tolerates wrapped
// payloads while still streaming (no full in-memory decode).
func newB64CleanReader(s string) io.Reader { return &b64CleanReader{s: s} }

type b64CleanReader struct {
	s string
	i int
}

func (r *b64CleanReader) Read(p []byte) (int, error) {
	n := 0
	for n < len(p) && r.i < len(r.s) {
		c := r.s[r.i]
		r.i++
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' {
			continue
		}
		p[n] = c
		n++
	}
	if n == 0 {
		return 0, io.EOF
	}
	return n, nil
}

func safeMediaName(filename string, i int, ext string) string {
	base := filepath.Base(strings.TrimSpace(strings.ReplaceAll(filename, "\\", "/")))
	base = strings.Map(func(r rune) rune {
		if r == '/' || r == '\\' || r < 0x20 {
			return '_'
		}
		return r
	}, base)
	base = strings.TrimSpace(base)
	if base == "" || base == "." || base == ".." {
		base = "file" + ext
	}
	if filepath.Ext(base) == "" {
		base += ext
	}
	return fmt.Sprintf("%d-%s", i, base)
}

func buildMediaPrompt(items []mediaItem, userText string) string {
	var b strings.Builder
	b.WriteString("Read and analyze the following local file(s) by opening them with your tools ")
	b.WriteString("(read the file directly; use python, ffmpeg/ffprobe, or document libraries for audio/video/PDF/Office as needed), ")
	b.WriteString("then answer the request below using their ACTUAL contents — do not guess:\n")
	for _, it := range items {
		b.WriteString("- ")
		b.WriteString(it.Path)
		if it.Kind != "" && it.Kind != "other" {
			b.WriteString(" (" + it.Kind + ")")
		}
		if it.Name != "" && it.Name != filepath.Base(it.Path) {
			b.WriteString(" — " + it.Name)
		}
		b.WriteString("\n")
	}
	b.WriteString("Each file's directory also contains a _map.md listing its path and (for archives) the extracted file tree.\n")
	b.WriteString("\nRequest: ")
	if strings.TrimSpace(userText) != "" {
		b.WriteString(userText)
	} else {
		b.WriteString("Describe the contents of the attached file(s).")
	}
	return b.String()
}

// ── SSRF-guarded download ────────────────────────────────────────────────────

// openMediaURL performs an SSRF-guarded GET and returns the streaming body for
// the caller to copy to disk under the size cap. The dialer's Control hook
// inspects the post-DNS resolved IP, so DNS-rebinding to internal hosts is
// blocked too.
func openMediaURL(ctx context.Context, rawurl string, max int64) (io.ReadCloser, error) {
	u, err := url.Parse(rawurl)
	if err != nil {
		return nil, fmt.Errorf("bad url")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("only http(s) media URLs are allowed")
	}
	dialer := &net.Dialer{
		Timeout: 10 * time.Second,
		Control: func(network, address string, c syscall.RawConn) error {
			host, _, _ := net.SplitHostPort(address)
			ip := net.ParseIP(host)
			if ip == nil || isBlockedIP(ip) {
				return fmt.Errorf("blocked address %s", host)
			}
			return nil
		},
	}
	client := &http.Client{
		Transport: &http.Transport{DialContext: dialer.DialContext, ResponseHeaderTimeout: 30 * time.Second},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 3 {
				return fmt.Errorf("too many redirects")
			}
			return nil
		},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawurl, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("download failed: %v", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		resp.Body.Close()
		return nil, fmt.Errorf("download returned status %d", resp.StatusCode)
	}
	if resp.ContentLength > max {
		resp.Body.Close()
		return nil, fmt.Errorf("remote file exceeds the %d MB limit", max>>20)
	}
	return resp.Body, nil
}

func isBlockedIP(ip net.IP) bool {
	if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified() || ip.IsPrivate() {
		return true
	}
	if ip4 := ip.To4(); ip4 != nil {
		// CGNAT 100.64.0.0/10
		if ip4[0] == 100 && ip4[1] >= 64 && ip4[1] <= 127 {
			return true
		}
	} else if len(ip) == net.IPv6len && (ip[0]&0xfe) == 0xfc {
		// ULA fc00::/7
		return true
	}
	return false
}

// ── safe archive extraction ──────────────────────────────────────────────────

func extractArchive(path, destDir string, total *int64, totMax int64) error {
	lower := strings.ToLower(path)
	switch {
	case strings.HasSuffix(lower, ".zip"):
		return unzipSafe(path, destDir, total, totMax)
	case strings.HasSuffix(lower, ".tar"):
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		return untarSafe(f, destDir, total, totMax)
	case strings.HasSuffix(lower, ".tar.gz"), strings.HasSuffix(lower, ".tgz"):
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		gz, err := gzip.NewReader(f)
		if err != nil {
			return err
		}
		defer gz.Close()
		return untarSafe(gz, destDir, total, totMax)
	case strings.HasSuffix(lower, ".tar.bz2"), strings.HasSuffix(lower, ".tbz2"), strings.HasSuffix(lower, ".tbz"):
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		return untarSafe(bzip2.NewReader(f), destDir, total, totMax)
	case strings.HasSuffix(lower, ".gz"):
		return gunzipSingle(path, destDir, total, totMax)
	case strings.HasSuffix(lower, ".tar.xz"), strings.HasSuffix(lower, ".txz"),
		strings.HasSuffix(lower, ".xz"), strings.HasSuffix(lower, ".rar"), strings.HasSuffix(lower, ".7z"):
		return extractWithTool(path, destDir)
	}
	return fmt.Errorf("unsupported archive type")
}

// safeJoin resolves an archive entry name against dest, rejecting (not silently
// sanitizing) any absolute path or ".." traversal so a tampered archive fails loudly.
func safeJoin(dest, name string) (string, bool) {
	name = strings.TrimSpace(strings.ReplaceAll(name, "\\", "/"))
	if name == "" || strings.HasPrefix(name, "/") {
		return "", false
	}
	for _, seg := range strings.Split(name, "/") {
		if seg == ".." {
			return "", false
		}
	}
	target := filepath.Join(dest, filepath.FromSlash(name))
	rel, err := filepath.Rel(dest, target)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return "", false
	}
	return target, true
}

func writeCapped(target string, r io.Reader, total *int64, totMax int64) error {
	remaining := totMax - *total
	if remaining <= 0 {
		return fmt.Errorf("archive exceeds the total size limit")
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	n, err := io.Copy(f, io.LimitReader(r, remaining+1))
	_ = f.Close()
	*total += n
	if err != nil {
		return err
	}
	if n > remaining {
		return fmt.Errorf("archive exceeds the total size limit")
	}
	_ = os.Chmod(target, 0o644)
	return nil
}

func untarSafe(r io.Reader, dest string, total *int64, totMax int64) error {
	tr := tar.NewReader(r)
	entries := 0
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		entries++
		if entries > maxArchiveEntries {
			return fmt.Errorf("archive has too many entries")
		}
		target, ok := safeJoin(dest, hdr.Name)
		if !ok {
			return fmt.Errorf("unsafe path in archive: %s", hdr.Name)
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			_ = os.MkdirAll(target, 0o755)
		case tar.TypeReg:
			if err := writeCapped(target, tr, total, totMax); err != nil {
				return err
			}
		default:
			// skip symlinks, hardlinks, devices, fifos — never created
		}
	}
	return nil
}

func unzipSafe(path, dest string, total *int64, totMax int64) error {
	zr, err := zip.OpenReader(path)
	if err != nil {
		return err
	}
	defer zr.Close()
	if len(zr.File) > maxArchiveEntries {
		return fmt.Errorf("archive has too many entries")
	}
	for _, zf := range zr.File {
		target, ok := safeJoin(dest, zf.Name)
		if !ok {
			return fmt.Errorf("unsafe path in archive: %s", zf.Name)
		}
		if zf.FileInfo().IsDir() {
			_ = os.MkdirAll(target, 0o755)
			continue
		}
		if zf.Mode()&os.ModeSymlink != 0 {
			continue // never create symlinks
		}
		rc, err := zf.Open()
		if err != nil {
			return err
		}
		err = writeCapped(target, rc, total, totMax)
		rc.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

func gunzipSingle(path, dest string, total *int64, totMax int64) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gz.Close()
	name := strings.TrimSuffix(filepath.Base(path), ".gz")
	if gz.Name != "" {
		if n, ok := safeJoin(dest, filepath.Base(gz.Name)); ok {
			_ = n
			name = filepath.Base(gz.Name)
		}
	}
	if name == "" {
		name = "content"
	}
	target, ok := safeJoin(dest, name)
	if !ok {
		return fmt.Errorf("unsafe gzip name")
	}
	return writeCapped(target, gz, total, totMax)
}

// extractWithTool handles xz/rar/7z via an installed binary, best-effort. The
// destination is a fresh empty dir; sanitizeTree() runs afterward.
func extractWithTool(path, dest string) error {
	lower := strings.ToLower(path)
	var candidates [][]string
	switch {
	case strings.HasSuffix(lower, ".tar.xz"), strings.HasSuffix(lower, ".txz"):
		candidates = [][]string{{"tar", "-xJf", path, "-C", dest}}
	case strings.HasSuffix(lower, ".xz"):
		candidates = [][]string{{"sh", "-c", "xz -dc " + shellQuote(path) + " > " + shellQuote(filepath.Join(dest, "content"))}}
	case strings.HasSuffix(lower, ".rar"):
		candidates = [][]string{{"unar", "-quiet", "-output-directory", dest, path}, {"unrar", "x", "-y", path, dest + string(os.PathSeparator)}}
	case strings.HasSuffix(lower, ".7z"):
		candidates = [][]string{{"7z", "x", "-y", "-o" + dest, path}, {"7za", "x", "-y", "-o" + dest, path}}
	default:
		return fmt.Errorf("unsupported archive type")
	}
	for _, c := range candidates {
		if _, err := exec.LookPath(c[0]); err != nil {
			continue
		}
		cmd := exec.Command(c[0], c[1:]...)
		if err := cmd.Run(); err == nil {
			return nil
		}
	}
	return fmt.Errorf("no extractor available for %s", filepath.Ext(path))
}

// sanitizeTree forces non-executable files, removes symlinks, and keeps dirs
// traversable. Backstop after tool-based extraction.
func sanitizeTree(dest string) {
	_ = filepath.WalkDir(dest, func(p string, d fs.DirEntry, err error) error {
		if err != nil || p == dest {
			return nil
		}
		info, e := d.Info()
		if e != nil {
			return nil
		}
		if info.Mode()&os.ModeSymlink != 0 {
			_ = os.Remove(p)
			return nil
		}
		if d.IsDir() {
			_ = os.Chmod(p, 0o755)
		} else {
			_ = os.Chmod(p, 0o644)
		}
		return nil
	})
}
