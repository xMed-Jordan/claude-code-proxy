package main

// camera_mosaic_test.go — buildMosaic must always produce a decodable, non-empty
// PNG sized to the configured grid, regardless of how many/what-shaped source
// images it's fed (including a truncated final row and a broken source image).

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"os"
	"path/filepath"
	"testing"
)

// writeTestJPEG writes a solid-color w x h JPEG to dir/name and returns its path.
func writeTestJPEG(t *testing.T, dir, name string, w, h int, c color.RGBA) string {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, c)
		}
	}
	path := filepath.Join(dir, name)
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create %s: %v", path, err)
	}
	defer f.Close()
	if err := jpeg.Encode(f, img, &jpeg.Options{Quality: 90}); err != nil {
		t.Fatalf("encode %s: %v", path, err)
	}
	return path
}

func TestBuildMosaicProducesDecodableNonEmptyPNG(t *testing.T) {
	dir := t.TempDir()
	items := []mosaicItem{
		{CameraID: "cam1", Name: "Lobby", Path: writeTestJPEG(t, dir, "cam1.jpg", 640, 360, color.RGBA{200, 50, 50, 255})},
		{CameraID: "cam2", Name: "Back Door", Path: writeTestJPEG(t, dir, "cam2.jpg", 320, 180, color.RGBA{50, 200, 50, 255})},
		{CameraID: "cam3", Name: "Hallway", Path: writeTestJPEG(t, dir, "cam3.jpg", 800, 600, color.RGBA{50, 50, 200, 255})},
	}
	cfg := config{CameraMosaicCols: 2, CameraMosaicCellW: 160, CameraMosaicCellH: 90, CameraMosaicMaxCams: 16}

	res, err := buildMosaic(cfg, items)
	if err != nil {
		t.Fatalf("buildMosaic: %v", err)
	}
	if len(res.PNG) == 0 {
		t.Fatal("mosaic PNG is empty")
	}
	if !bytes.HasPrefix(res.PNG, []byte("\x89PNG\r\n\x1a\n")) {
		t.Fatal("mosaic output does not start with the PNG magic header")
	}

	// 3 items at 2 cols -> 2 rows; grid should be exactly cols*cellW x rows*cellH.
	wantW, wantH := 2*160, 2*90
	if res.Width != wantW || res.Height != wantH {
		t.Errorf("dims = %dx%d, want %dx%d", res.Width, res.Height, wantW, wantH)
	}

	decoded, err := png.Decode(bytes.NewReader(res.PNG))
	if err != nil {
		t.Fatalf("png.Decode failed on buildMosaic output: %v", err)
	}
	b := decoded.Bounds()
	if b.Dx() != wantW || b.Dy() != wantH {
		t.Errorf("decoded dims = %dx%d, want %dx%d", b.Dx(), b.Dy(), wantW, wantH)
	}

	if len(res.Legend) != 3 {
		t.Fatalf("legend len = %d, want 3", len(res.Legend))
	}
	for i, want := range []string{"cam1", "cam2", "cam3"} {
		if res.Legend[i].CameraID != want || res.Legend[i].Index != i+1 {
			t.Errorf("legend[%d] = %+v, want CameraID=%s Index=%d", i, res.Legend[i], want, i+1)
		}
	}
}

func TestBuildMosaicToleratesUnreadableCell(t *testing.T) {
	dir := t.TempDir()
	items := []mosaicItem{
		{CameraID: "cam1", Path: writeTestJPEG(t, dir, "cam1.jpg", 100, 100, color.RGBA{10, 10, 10, 255})},
		{CameraID: "cam2", Path: filepath.Join(dir, "does-not-exist.jpg")}, // broken/missing source
	}
	cfg := config{CameraMosaicCols: 2, CameraMosaicCellW: 64, CameraMosaicCellH: 64, CameraMosaicMaxCams: 16}

	res, err := buildMosaic(cfg, items)
	if err != nil {
		t.Fatalf("buildMosaic should tolerate one broken cell, got error: %v", err)
	}
	if len(res.PNG) == 0 {
		t.Fatal("mosaic PNG is empty")
	}
	if _, err := png.Decode(bytes.NewReader(res.PNG)); err != nil {
		t.Fatalf("png.Decode: %v", err)
	}
	if len(res.Legend) != 2 {
		t.Fatalf("legend len = %d, want 2 (missing cell still gets a legend entry)", len(res.Legend))
	}
}

func TestBuildMosaicUsesDefaultsWhenConfigZero(t *testing.T) {
	dir := t.TempDir()
	items := []mosaicItem{
		{CameraID: "cam1", Path: writeTestJPEG(t, dir, "cam1.jpg", 100, 100, color.RGBA{1, 2, 3, 255})},
	}
	res, err := buildMosaic(config{}, items) // all mosaic dims zero -> defaults (4 cols / 320x180 cells)
	if err != nil {
		t.Fatalf("buildMosaic: %v", err)
	}
	// Grid width is always cols*cellW (a full row), regardless of item count;
	// with the defaults (4 cols, 320x180 cells) and 1 row of items that's
	// 1280x180.
	if res.Width != 1280 || res.Height != 180 {
		t.Errorf("dims = %dx%d, want default 1280x180 (4 cols x 320w, 1 row x 180h)", res.Width, res.Height)
	}
}

func TestBuildMosaicTruncatesOverMaxCams(t *testing.T) {
	dir := t.TempDir()
	items := make([]mosaicItem, 0, 5)
	for i := 0; i < 5; i++ {
		items = append(items, mosaicItem{
			CameraID: string(rune('a' + i)),
			Path:     writeTestJPEG(t, dir, string(rune('a'+i))+".jpg", 50, 50, color.RGBA{0, 0, 0, 255}),
		})
	}
	cfg := config{CameraMosaicCols: 2, CameraMosaicCellW: 32, CameraMosaicCellH: 32, CameraMosaicMaxCams: 3}
	res, err := buildMosaic(cfg, items)
	if err != nil {
		t.Fatalf("buildMosaic: %v", err)
	}
	if len(res.Legend) != 3 {
		t.Fatalf("legend len = %d, want 3 (truncated to MosaicMaxCams)", len(res.Legend))
	}
}

func TestBuildMosaicEmptyItemsErrors(t *testing.T) {
	if _, err := buildMosaic(config{}, nil); err == nil {
		t.Fatal("expected an error for an empty item list, got nil")
	}
}
