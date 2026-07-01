package main

// camera_capture_test.go — table-driven tests for the deterministic, I/O-free
// (aside from decoding a local file) black/no-signal classifier isBlankSnapshot.
// Synthetic images are written to a temp dir rather than shelling out to ffmpeg,
// keeping this test hermetic (no ffmpeg binary required to run `go test`).

import (
	"image"
	"image/color"
	"image/png"
	"math/rand"
	"os"
	"path/filepath"
	"testing"
)

// writeTestPNG writes img as a PNG to dir/name and returns its path.
func writeTestPNG(t *testing.T, dir, name string, img image.Image) string {
	t.Helper()
	path := filepath.Join(dir, name)
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create %s: %v", path, err)
	}
	defer f.Close()
	if err := png.Encode(f, img); err != nil {
		t.Fatalf("encode %s: %v", path, err)
	}
	return path
}

func solidImage(w, h int, c color.RGBA) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, c)
		}
	}
	return img
}

// noisyImage fills w x h with deterministically-seeded random gray noise — a
// stand-in for a real, busy camera scene (high stddev, i.e. NOT blank).
func noisyImage(w, h int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	rng := rand.New(rand.NewSource(42))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			v := uint8(rng.Intn(256))
			img.Set(x, y, color.RGBA{v, v, v, 255})
		}
	}
	return img
}

func TestIsBlankSnapshotBlackImage(t *testing.T) {
	dir := t.TempDir()
	path := writeTestPNG(t, dir, "black.png", solidImage(64, 64, color.RGBA{0, 0, 0, 255}))

	blank, reason := isBlankSnapshot(path)
	if !blank {
		t.Fatal("solid black image should be classified as blank")
	}
	if reason != "black" {
		t.Errorf("reason = %q, want \"black\"", reason)
	}
}

func TestIsBlankSnapshotNoSignalCard(t *testing.T) {
	dir := t.TempDir()
	// A mid-gray solid frame: low variance but not dark -> "no-signal", not "black".
	path := writeTestPNG(t, dir, "gray.png", solidImage(64, 64, color.RGBA{128, 128, 128, 255}))

	blank, reason := isBlankSnapshot(path)
	if !blank {
		t.Fatal("solid mid-gray image should be classified as blank (no-signal)")
	}
	if reason != "no-signal" {
		t.Errorf("reason = %q, want \"no-signal\"", reason)
	}
}

func TestIsBlankSnapshotNoisyImageNotBlank(t *testing.T) {
	dir := t.TempDir()
	path := writeTestPNG(t, dir, "noisy.png", noisyImage(64, 64))

	blank, reason := isBlankSnapshot(path)
	if blank {
		t.Fatalf("high-variance noisy image should NOT be classified as blank, got reason=%q", reason)
	}
	if reason != "" {
		t.Errorf("reason = %q, want empty", reason)
	}
}

func TestIsBlankSnapshotUndecodableFileFails(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "not-an-image.png")
	if err := os.WriteFile(path, []byte("this is not an image"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	blank, reason := isBlankSnapshot(path)
	if !blank {
		t.Fatal("an undecodable file must be classified as blank (failed), never treated as a healthy frame")
	}
	if reason != "failed" {
		t.Errorf("reason = %q, want \"failed\"", reason)
	}
}

func TestIsBlankSnapshotMissingFileFails(t *testing.T) {
	blank, reason := isBlankSnapshot(filepath.Join(t.TempDir(), "does-not-exist.png"))
	if !blank || reason != "failed" {
		t.Errorf("blank=%v reason=%q, want true/\"failed\" for a missing file", blank, reason)
	}
}

// TestAnalyzeImageLuma pins the luma/stddev math itself against hand-computed
// expectations for two trivial fixed images (a uniform image has stddev 0; the
// mean luma of a uniform image is the ITU-R BT.601 weighted value).
func TestAnalyzeImageLuma(t *testing.T) {
	dir := t.TempDir()

	whitePath := writeTestPNG(t, dir, "white.png", solidImage(16, 16, color.RGBA{255, 255, 255, 255}))
	mean, std, ok := analyzeImageLuma(whitePath)
	if !ok {
		t.Fatal("analyzeImageLuma failed to decode a valid PNG")
	}
	if std != 0 {
		t.Errorf("stddev of a uniform image = %v, want 0", std)
	}
	if mean < 254 || mean > 255 { // 0.299+0.587+0.114 == 1.0 at 255,255,255 -> 255
		t.Errorf("mean luma of pure white = %v, want ~255", mean)
	}

	blackPath := writeTestPNG(t, dir, "black2.png", solidImage(16, 16, color.RGBA{0, 0, 0, 255}))
	mean, std, ok = analyzeImageLuma(blackPath)
	if !ok {
		t.Fatal("analyzeImageLuma failed to decode a valid PNG")
	}
	if mean != 0 || std != 0 {
		t.Errorf("mean/std of pure black = %v/%v, want 0/0", mean, std)
	}
}
