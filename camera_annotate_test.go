package main

import (
	"bytes"
	"encoding/json"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"strings"
	"testing"
)

func TestCamParseBBox(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want camBBox
		ok   bool
	}{
		{"valid", `{"x0":100,"y0":100,"x1":400,"y1":900}`, camBBox{100, 100, 400, 900}, true},
		{"clamped", `{"x0":-50,"y0":0,"x1":1200,"y1":500}`, camBBox{0, 0, 1000, 500}, true},
		{"swapped corners", `{"x0":400,"y0":900,"x1":100,"y1":100}`, camBBox{100, 100, 400, 900}, true},
		{"string numbers", `{"x0":"100","y0":"100.7","x1":"400","y1":"900"}`, camBBox{100, 100, 400, 900}, true},
		{"float numbers", `{"x0":100.6,"y0":100,"x1":400,"y1":900}`, camBBox{100, 100, 400, 900}, true},
		{"stringified object", `"{\"x0\":100,\"y0\":100,\"x1\":400,\"y1\":900}"`, camBBox{100, 100, 400, 900}, true},
		{"too narrow", `{"x0":100,"y0":100,"x1":105,"y1":900}`, camBBox{}, false},
		{"too short", `{"x0":100,"y0":100,"x1":900,"y1":104}`, camBBox{}, false},
		{"null", `null`, camBBox{}, false},
		{"empty", ``, camBBox{}, false},
		{"garbage", `wat`, camBBox{}, false},
		{"empty object", `{}`, camBBox{}, false},
	}
	for _, tc := range cases {
		got, ok := camParseBBox(json.RawMessage(tc.raw))
		if ok != tc.ok {
			t.Errorf("%s: ok=%v want %v", tc.name, ok, tc.ok)
			continue
		}
		if ok && got != tc.want {
			t.Errorf("%s: got %+v want %+v", tc.name, got, tc.want)
		}
	}
}

func TestCamBBoxPixelRoundTrip(t *testing.T) {
	b := camBBox{250, 250, 750, 750}
	r := camBBoxToPixels(b, 200, 100)
	if want := image.Rect(50, 25, 150, 75); r != want {
		t.Fatalf("camBBoxToPixels = %v want %v", r, want)
	}
	back := camBBoxFromPixels(r, 200, 100)
	if back != b {
		t.Fatalf("round trip = %+v want %+v", back, b)
	}
	if got := camBBoxFromPixels(r, 0, 0); got != (camBBox{}) {
		t.Fatalf("zero-dim frame should give zero box, got %+v", got)
	}
	if r := camBBoxToPixels(b, 0, 0); !r.Empty() {
		t.Fatalf("zero-dim frame should give empty rect, got %v", r)
	}
	// Out-of-frame pixels clamp onto the grid.
	c := camBBoxFromPixels(image.Rect(-20, -10, 400, 300), 200, 100)
	if c != (camBBox{0, 0, 1000, 1000}) {
		t.Fatalf("clamped pixel box = %+v", c)
	}
}

func TestCamDrawEllipseBandOnly(t *testing.T) {
	dst := image.NewRGBA(image.Rect(0, 0, 100, 100))
	col := color.RGBA{255, 200, 0, 255}
	r := image.Rect(20, 20, 80, 80)
	const thick = 3
	camDrawEllipse(dst, r, col, thick)

	cx, cy := 50.0, 50.0
	rx, ry := 30.0, 30.0
	painted := 0
	for y := 0; y < 100; y++ {
		for x := 0; x < 100; x++ {
			if dst.RGBAAt(x, y) != col {
				continue
			}
			painted++
			dx, dy := float64(x)+0.5-cx, float64(y)+0.5-cy
			in := dx*dx/(rx*rx) + dy*dy/(ry*ry)
			out := dx*dx/((rx+thick)*(rx+thick)) + dy*dy/((ry+thick)*(ry+thick))
			if in < 1 || out > 1 {
				t.Fatalf("pixel (%d,%d) painted outside the ellipse band (in=%.3f out=%.3f)", x, y, in, out)
			}
		}
	}
	if painted == 0 {
		t.Fatal("no pixels painted")
	}
	// Spot probes: band point painted, center and far corner untouched.
	if dst.RGBAAt(50, 18) != col {
		t.Error("expected band pixel (50,18) painted")
	}
	if dst.RGBAAt(50, 50) == col {
		t.Error("center must not be painted")
	}
	if dst.RGBAAt(0, 0) == col {
		t.Error("far corner must not be painted")
	}

	// Bounds safety: rect hanging off the canvas must not panic.
	camDrawEllipse(dst, image.Rect(-30, -30, 130, 130), col, 5)
	// Degenerate rect: no-op.
	before := *dst
	camDrawEllipse(&before, image.Rect(10, 10, 11, 11), col, 3)
}

// camTestJPEG builds a w×h JPEG: left half red, right half blue when split,
// or a solid fill.
func camTestJPEG(t *testing.T, w, h int, split bool) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	if split {
		draw.Draw(img, image.Rect(0, 0, w/2, h), &image.Uniform{color.RGBA{200, 0, 0, 255}}, image.Point{}, draw.Src)
		draw.Draw(img, image.Rect(w/2, 0, w, h), &image.Uniform{color.RGBA{0, 0, 200, 255}}, image.Point{}, draw.Src)
	} else {
		draw.Draw(img, img.Bounds(), &image.Uniform{color.RGBA{255, 255, 255, 255}}, image.Point{}, draw.Src)
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 95}); err != nil {
		t.Fatalf("encode test jpeg: %v", err)
	}
	return buf.Bytes()
}

func TestCamAnnotateJPEGRoundTrip(t *testing.T) {
	src := camTestJPEG(t, 200, 100, false)
	out, w, h, err := camAnnotateJPEG(src, []camBBox{{250, 250, 750, 750}})
	if err != nil {
		t.Fatalf("camAnnotateJPEG: %v", err)
	}
	if w != 200 || h != 100 {
		t.Fatalf("dims %dx%d want 200x100", w, h)
	}
	img, _, err := image.Decode(bytes.NewReader(out))
	if err != nil {
		t.Fatalf("decode annotated: %v", err)
	}
	if b := img.Bounds(); b.Dx() != 200 || b.Dy() != 100 {
		t.Fatalf("annotated dims %v", b)
	}
	// Band pixel above the box center should be amber (white background).
	pr, pg, pb, _ := img.At(100, 23).RGBA()
	r8, g8, b8 := pr>>8, pg>>8, pb>>8
	if r8 < 180 || g8 < 120 || b8 > 140 {
		t.Errorf("pixel (100,23) = %d,%d,%d — expected amber ellipse band", r8, g8, b8)
	}
	// Center stays white-ish.
	pr, pg, pb, _ = img.At(100, 50).RGBA()
	if pr>>8 < 200 || pg>>8 < 200 || pb>>8 < 200 {
		t.Errorf("center pixel repainted: %d,%d,%d", pr>>8, pg>>8, pb>>8)
	}

	// Multi-box: index badges drawn at each box's top-left (dark over white).
	out, _, _, err = camAnnotateJPEG(src, []camBBox{{100, 100, 400, 900}, {600, 100, 900, 900}})
	if err != nil {
		t.Fatalf("camAnnotateJPEG multi: %v", err)
	}
	img, _, err = image.Decode(bytes.NewReader(out))
	if err != nil {
		t.Fatalf("decode multi: %v", err)
	}
	pr, pg, pb, _ = img.At(22, 12).RGBA() // inside badge of box 1 (pixel top-left 20,10)
	if pr>>8 > 120 && pg>>8 > 120 && pb>>8 > 120 {
		t.Errorf("expected dark index badge at (22,12), got %d,%d,%d", pr>>8, pg>>8, pb>>8)
	}

	if _, _, _, err := camAnnotateJPEG([]byte("not an image"), nil); err == nil {
		t.Error("expected decode error for garbage input")
	}
}

func TestCamCropJPEG(t *testing.T) {
	src := camTestJPEG(t, 200, 100, true)

	// Right half → 100x100 blue crop.
	out, w, h, err := camCropJPEG(src, camBBox{500, 0, 1000, 1000}, 0)
	if err != nil {
		t.Fatalf("camCropJPEG: %v", err)
	}
	if w != 100 || h != 100 {
		t.Fatalf("crop dims %dx%d want 100x100", w, h)
	}
	img, _, err := image.Decode(bytes.NewReader(out))
	if err != nil {
		t.Fatalf("decode crop: %v", err)
	}
	pr, _, pb, _ := img.At(50, 50).RGBA()
	if pb>>8 < 130 || pr>>8 > 100 {
		t.Errorf("crop center = r%d b%d — expected blue right half", pr>>8, pb>>8)
	}

	// 15% padding each side: (50,25)-(150,75) → (35,18)-(165,82) = 130x64.
	_, w, h, err = camCropJPEG(src, camBBox{250, 250, 750, 750}, 15)
	if err != nil {
		t.Fatalf("padded crop: %v", err)
	}
	if w != 130 || h != 64 {
		t.Errorf("padded dims %dx%d want 130x64", w, h)
	}

	// Tiny box grows to the 48px floor.
	_, w, h, err = camCropJPEG(src, camBBox{500, 500, 520, 530}, 15)
	if err != nil {
		t.Fatalf("tiny crop: %v", err)
	}
	if w != 48 || h != 48 {
		t.Errorf("tiny crop dims %dx%d want 48x48", w, h)
	}

	// Box at the frame edge stays inside the frame.
	_, w, h, err = camCropJPEG(src, camBBox{980, 980, 1000, 1000}, 15)
	if err != nil {
		t.Fatalf("edge crop: %v", err)
	}
	if w != 48 || h != 48 {
		t.Errorf("edge crop dims %dx%d want 48x48", w, h)
	}

	if _, _, _, err := camCropJPEG([]byte("junk"), camBBox{0, 0, 1000, 1000}, 15); err == nil {
		t.Error("expected decode error for garbage input")
	}
}

func TestCamFitSpan(t *testing.T) {
	cases := []struct {
		lo, hi, min, limit int
		wantLo, wantHi     int
	}{
		{10, 90, 48, 200, 10, 90},     // already big enough
		{100, 104, 48, 200, 78, 126},  // grow around center
		{0, 4, 48, 200, 0, 48},        // clamp at low edge
		{196, 200, 48, 200, 152, 200}, // clamp at high edge
		{10, 90, 48, 30, 0, 30},       // frame smaller than min
	}
	for _, tc := range cases {
		lo, hi := camFitSpan(tc.lo, tc.hi, tc.min, tc.limit)
		if lo != tc.wantLo || hi != tc.wantHi {
			t.Errorf("camFitSpan(%d,%d,%d,%d) = %d,%d want %d,%d",
				tc.lo, tc.hi, tc.min, tc.limit, lo, hi, tc.wantLo, tc.wantHi)
		}
	}
}

func TestCamBBoxConvention(t *testing.T) {
	s := camBBoxConvention(704, 576)
	for _, want := range []string{"0-1000", "704x576 pixels", "TOP-LEFT", `{"x0":int,"y0":int,"x1":int,"y1":int}`} {
		if !strings.Contains(s, want) {
			t.Errorf("camBBoxConvention missing %q in %q", want, s)
		}
	}
}
