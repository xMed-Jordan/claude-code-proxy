package main

// agyimage.go — image *generation* via the "agy" upstream.
//
// agy (Antigravity CLI) can generate images: when handed a writable directory
// (via --add-dir + --dangerously-skip-permissions) and asked to "generate an
// image and save it as a PNG at <path>", the model invokes its image tool and
// writes a real raster file to disk (proven on the live server: 512x512 / 2K /
// 4K all honored, exact pixel dimensions). This mirrors the media-read path in
// agymedia.go but in reverse — agy produces a file, and we serve it back over
// HTTP as a link.
//
// Flow: parse the requested size → resolve an Antigravity model → run agy in a
// per-request scratch dir → collect whatever image file(s) it wrote (by content
// sniff, not by name, since agy may pick its own filename/format) → move them
// into a flat served root as <random-token>.<ext> → return capability URLs of
// the form <base>/media/generated/<token>.<ext>. Served files are retained for
// a window (reaped afterward) so links keep working.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	maxImageDim  = 4096 // hard per-side cap to bound output file size
	minImageDim  = 16
	maxImageGenN = 8 // most images one request may ask for
)

// imageSize is a validated width/height plus a human label for logging/UI.
type imageSize struct {
	W, H  int
	Label string
}

// generatedImage is one image agy produced, after we copy it into the served root.
type generatedImage struct {
	ID          string // served basename without extension (random hex token)
	File        string // served basename with extension, e.g. "ab12….png"
	Path        string // absolute path in the served root
	Ext         string // ".png" / ".jpg" / ".webp" …
	ContentType string // sniffed MIME, e.g. "image/png"
	Width       int    // decoded pixel width (0 if undecodable)
	Height      int    // decoded pixel height (0 if undecodable)
	Bytes       int64
}

// generatedImageNameRe matches the served filenames we mint: a hex token plus a
// known image extension. Used to validate the public serve path (no traversal).
var generatedImageNameRe = regexp.MustCompile(`^[a-f0-9]{8,64}\.(?:png|jpe?g|gif|webp|bmp)$`)

func validGeneratedImageName(name string) bool {
	return generatedImageNameRe.MatchString(name)
}

// parseImageSize accepts named presets (½K/1K/HD/2K/4K …) or an explicit "WxH".
// An empty/auto value defaults to 1024x1024 (the model's native square output).
func parseImageSize(s string) (imageSize, error) {
	key := strings.ToLower(strings.TrimSpace(s))
	key = strings.ReplaceAll(key, " ", "")
	switch key {
	case "", "auto", "default", "1k", "1024", "1024x1024":
		return imageSize{1024, 1024, "1K (1024×1024)"}, nil
	case "256", "256x256":
		return imageSize{256, 256, "256×256"}, nil
	case "512", "half", "halfk", "½k", "512x512":
		return imageSize{512, 512, "½K (512×512)"}, nil
	case "720p", "hd", "1280x720":
		return imageSize{1280, 720, "720p (1280×720)"}, nil
	case "1080p", "fhd", "1920x1080":
		return imageSize{1920, 1080, "1080p (1920×1080)"}, nil
	case "2k", "2560x1440":
		return imageSize{2560, 1440, "2K (2560×1440)"}, nil
	case "4k", "uhd", "3840x2160":
		return imageSize{3840, 2160, "4K (3840×2160)"}, nil
	}
	// Generic WxH (accept x, *, or ×).
	norm := strings.ReplaceAll(key, "×", "x")
	norm = strings.ReplaceAll(norm, "*", "x")
	if a, b, ok := strings.Cut(norm, "x"); ok {
		w, e1 := strconv.Atoi(a)
		h, e2 := strconv.Atoi(b)
		if e1 == nil && e2 == nil {
			if w < minImageDim || h < minImageDim || w > maxImageDim || h > maxImageDim {
				return imageSize{}, fmt.Errorf("size %dx%d out of range (%d..%d per side)", w, h, minImageDim, maxImageDim)
			}
			return imageSize{w, h, fmt.Sprintf("%d×%d", w, h)}, nil
		}
	}
	return imageSize{}, fmt.Errorf("unrecognized size %q (use e.g. 512x512, 1024x1024, or a preset: 512/1k/hd/2k/4k)", s)
}

// agyImageRoot is the flat directory generated images are served from.
func agyImageRoot(cfg config) string {
	if d := strings.TrimSpace(cfg.AgyImageDir); d != "" {
		return d
	}
	return filepath.Join(os.TempDir(), "connect-ai-proxy-images")
}

// agyImageModelForRequest resolves which Antigravity model orchestrates the
// generation. Only genuine agy display names (which always contain a space, e.g.
// "Gemini 3.5 Flash (Low)") are forwarded; anything else falls back to the
// configured default image model so we never hand agy an id it would reject.
func agyImageModelForRequest(cfg config, requested string) string {
	requested = strings.TrimSpace(requested)
	if strings.Contains(requested, " ") {
		return requested
	}
	if requested != "" {
		if m := agyModelFor(cfg, requested); strings.Contains(m, " ") {
			return m
		}
	}
	if m := strings.TrimSpace(cfg.AgyImageModel); m != "" {
		return m
	}
	return strings.TrimSpace(cfg.AgyMediaModel)
}

// buildImageGenPrompt instructs agy to generate N images at an exact size and
// save them as PNGs into the scratch dir. We collect by content sniff afterward,
// so the exact filenames are a hint, not a contract.
func buildImageGenPrompt(userPrompt string, n int, sz imageSize, dir string) string {
	var b strings.Builder
	b.WriteString("You are an image generation assistant. Generate ")
	if n == 1 {
		b.WriteString("one image")
	} else {
		fmt.Fprintf(&b, "%d distinct images", n)
	}
	b.WriteString(" from the description below and SAVE ")
	if n == 1 {
		b.WriteString("it")
	} else {
		b.WriteString("them")
	}
	b.WriteString(" to disk as PNG file(s). Do not ask any questions; just create the file(s).\n\n")
	b.WriteString("Description: ")
	b.WriteString(strings.TrimSpace(userPrompt))
	b.WriteString("\n\nRequirements:\n")
	fmt.Fprintf(&b, "- Save the file(s) into this directory: %s\n", dir)
	if n == 1 {
		b.WriteString("- Name the file: image-1.png\n")
	} else {
		fmt.Fprintf(&b, "- Name the files image-1.png, image-2.png, … image-%d.png\n", n)
	}
	fmt.Fprintf(&b, "- Each image must be exactly %d × %d pixels.\n", sz.W, sz.H)
	b.WriteString("- Use the .png extension and PNG format.\n")
	b.WriteString("- Do not save anywhere else, and do not delete or modify any existing files.\n")
	b.WriteString("\nAfter creating the file(s), reply with the absolute path of each file you created, one per line.")
	return b.String()
}

// agyGenerateImages runs agy to produce n image(s) of the requested size and
// returns them after copying into the served root. The per-request scratch dir
// (where agy writes) is removed before returning; only the served copies persist.
func agyGenerateImages(ctx context.Context, cfg config, userPrompt, model string, n int, sz imageSize) ([]generatedImage, error) {
	userPrompt = strings.TrimSpace(userPrompt)
	if userPrompt == "" {
		return nil, fmt.Errorf("prompt is required")
	}
	if n < 1 {
		n = 1
	}
	if n > maxImageGenN {
		n = maxImageGenN
	}
	root := agyImageRoot(cfg)
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, err
	}
	scratch := filepath.Join(root, ".gen-"+randToken(8))
	if err := os.MkdirAll(scratch, 0o700); err != nil {
		return nil, err
	}
	defer os.RemoveAll(scratch)

	prompt := buildImageGenPrompt(userPrompt, n, sz, scratch)
	res, err := runAgyj(ctx, cfg, prompt, model, []string{scratch})
	if err != nil {
		return nil, err
	}
	if !res.Ok {
		return nil, fmt.Errorf("%s", firstNonEmpty(res.Error, "agy returned no result"))
	}
	imgs, err := collectGeneratedImages(scratch, root)
	if err != nil {
		return nil, err
	}
	if len(imgs) == 0 {
		return nil, fmt.Errorf("agy produced no image file: %s", truncateString(strings.TrimSpace(res.Response), 200))
	}
	return imgs, nil
}

// collectGeneratedImages walks the scratch dir for files whose bytes sniff as a
// supported image type and moves each into the served root as <token>.<ext>.
func collectGeneratedImages(scratch, root string) ([]generatedImage, error) {
	var found []string
	_ = filepath.WalkDir(scratch, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if ct, _ := sniffImage(p); ct != "" {
			found = append(found, p)
		}
		return nil
	})
	sort.Strings(found) // stable order: image-1, image-2, …
	var out []generatedImage
	for _, src := range found {
		ct, ext := sniffImage(src)
		if ct == "" {
			continue
		}
		id := randToken(16)
		dest := filepath.Join(root, id+ext)
		if err := os.Rename(src, dest); err != nil {
			if cerr := copyFile(src, dest, 0o644); cerr != nil {
				continue
			}
		}
		_ = os.Chmod(dest, 0o644)
		w, h := imageDimensions(dest)
		var size int64
		if fi, e := os.Stat(dest); e == nil {
			size = fi.Size()
		}
		out = append(out, generatedImage{
			ID: id, File: id + ext, Path: dest, Ext: ext,
			ContentType: ct, Width: w, Height: h, Bytes: size,
		})
	}
	return out, nil
}

// sniffImage reads the leading bytes and returns the MIME + canonical extension
// for supported raster types, or ("","") if the file is not a known image.
// agy sometimes writes JPEG bytes into a .png-named file, so we never trust the
// name — only the content.
func sniffImage(path string) (contentType, ext string) {
	f, err := os.Open(path)
	if err != nil {
		return "", ""
	}
	defer f.Close()
	buf := make([]byte, 512)
	n, _ := io.ReadFull(f, buf)
	ct := http.DetectContentType(buf[:n])
	switch {
	case strings.HasPrefix(ct, "image/png"):
		return "image/png", ".png"
	case strings.HasPrefix(ct, "image/jpeg"):
		return "image/jpeg", ".jpg"
	case strings.HasPrefix(ct, "image/gif"):
		return "image/gif", ".gif"
	case strings.HasPrefix(ct, "image/webp"):
		return "image/webp", ".webp"
	case strings.HasPrefix(ct, "image/bmp"):
		return "image/bmp", ".bmp"
	}
	return "", ""
}

// imageDimensions decodes just the header for width/height (best-effort; webp/bmp
// aren't in the stdlib decoder set, so they return 0,0).
func imageDimensions(path string) (int, int) {
	f, err := os.Open(path)
	if err != nil {
		return 0, 0
	}
	defer f.Close()
	cfg, _, err := image.DecodeConfig(f)
	if err != nil {
		return 0, 0
	}
	return cfg.Width, cfg.Height
}

// randToken returns 2*n lowercase hex chars from a crypto source (capability id).
func randToken(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand failure is exceptional; fall back to a time-based token.
		return strconv.FormatInt(time.Now().UnixNano(), 16)
	}
	return hex.EncodeToString(b)
}

// imageBaseURL returns the public base for image links: the configured public
// URL if set, else the scheme+host derived from the request, else the local URL.
func imageBaseURL(cfg config, r *http.Request) string {
	if cfg.PublicURL != "" {
		return cfg.PublicURL
	}
	if r != nil && r.Host != "" {
		scheme := "http"
		if r.TLS != nil {
			scheme = "https"
		}
		if xf := strings.TrimSpace(r.Header.Get("X-Forwarded-Proto")); xf != "" {
			scheme = xf
		}
		return scheme + "://" + r.Host
	}
	return localProxyBaseURL(cfg)
}

func generatedImageURL(cfg config, r *http.Request, file string) string {
	return strings.TrimRight(imageBaseURL(cfg, r), "/") + "/media/generated/" + file
}

// startImageReaper deletes served images (and abandoned scratch dirs) older than
// the retention window, hourly, so generated-image links persist for reuse but
// don't accumulate forever.
func startImageReaper(cfg config) {
	if !cfg.AgyImage {
		return
	}
	root := agyImageRoot(cfg)
	retention := cfg.AgyImageRetention
	if retention <= 0 {
		retention = 24 * time.Hour
	}
	go func() {
		ticker := time.NewTicker(time.Hour)
		defer ticker.Stop()
		reapImageRoot(root, retention)
		for range ticker.C {
			reapImageRoot(root, retention)
		}
	}()
}

func reapImageRoot(root string, retention time.Duration) {
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
		if e.IsDir() {
			// abandoned per-request scratch dirs: clear after an hour
			if strings.HasPrefix(e.Name(), ".gen-") && now.Sub(info.ModTime()) > time.Hour {
				_ = os.RemoveAll(full)
			}
			continue
		}
		if now.Sub(info.ModTime()) > retention {
			_ = os.Remove(full)
		}
	}
}
