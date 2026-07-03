package main

// camera_annotate.go — bbox parsing + image annotation primitives for the
// avatar system (design-avatars.md §2.1): normalized bounding boxes, an amber
// ellipse "circle the person" drawer, and padded person crops.
//
// CONVENTION: every bbox is a normalized 0–1000 INTEGER grid, origin TOP-LEFT,
// x right / y down, JSON {"x0","y0","x1","y1"} (Gemini-native; integers survive
// flexInt; scale-invariant across sub/main frames). The face sidecar returns
// PIXELS — camBBoxFromPixels converts. camAnnotateJPEG re-encodes straight JPEG
// quality 85 (NO Plan9 palette dither — that is mosaic-only).

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	_ "image/png"
	"math"
)

// camBBox is a normalized bounding box on the 0-1000 grid (top-left origin).
type camBBox struct {
	X0, Y0, X1, Y1 int
}

// camBBoxMinSpan is the smallest accepted box edge on the 0-1000 grid; anything
// tighter is almost certainly a hallucinated point, not a person/vehicle.
const camBBoxMinSpan = 8

// camClamp1000 clamps a normalized coordinate to the [0,1000] grid.
func camClamp1000(v int) int {
	if v < 0 {
		return 0
	}
	if v > 1000 {
		return 1000
	}
	return v
}

// camParseBBox tolerantly decodes a model-emitted bbox: accepts {"x0":..} via
// flexInt fields, clamps to [0,1000], swaps inverted corners, and rejects boxes
// smaller than 8/1000 in either axis (ok=false).
func camParseBBox(raw json.RawMessage) (camBBox, bool) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return camBBox{}, false
	}
	// Models occasionally stringify the whole object; unquote once and retry.
	if trimmed[0] == '"' {
		var s string
		if err := json.Unmarshal(trimmed, &s); err != nil {
			return camBBox{}, false
		}
		trimmed = bytes.TrimSpace([]byte(s))
		if len(trimmed) == 0 || trimmed[0] == '"' {
			return camBBox{}, false
		}
	}
	var in struct {
		X0 flexInt `json:"x0"`
		Y0 flexInt `json:"y0"`
		X1 flexInt `json:"x1"`
		Y1 flexInt `json:"y1"`
	}
	if err := json.Unmarshal(trimmed, &in); err != nil {
		return camBBox{}, false
	}
	b := camBBox{
		X0: camClamp1000(int(in.X0)),
		Y0: camClamp1000(int(in.Y0)),
		X1: camClamp1000(int(in.X1)),
		Y1: camClamp1000(int(in.Y1)),
	}
	if b.X0 > b.X1 {
		b.X0, b.X1 = b.X1, b.X0
	}
	if b.Y0 > b.Y1 {
		b.Y0, b.Y1 = b.Y1, b.Y0
	}
	if b.X1-b.X0 < camBBoxMinSpan || b.Y1-b.Y0 < camBBoxMinSpan {
		return camBBox{}, false
	}
	return b, true
}

// camBBoxToPixels maps a normalized box onto a w×h pixel frame.
func camBBoxToPixels(b camBBox, w, h int) image.Rectangle {
	if w <= 0 || h <= 0 {
		return image.Rectangle{}
	}
	scale := func(v, size int) int {
		return int(math.Round(float64(v) * float64(size) / 1000))
	}
	r := image.Rect(scale(b.X0, w), scale(b.Y0, h), scale(b.X1, w), scale(b.Y1, h))
	return r.Intersect(image.Rect(0, 0, w, h))
}

// camBBoxFromPixels maps a pixel rectangle (e.g. from the face sidecar) on a
// w×h frame back to the normalized 0-1000 grid.
func camBBoxFromPixels(r image.Rectangle, w, h int) camBBox {
	if w <= 0 || h <= 0 {
		return camBBox{}
	}
	r = r.Canon()
	scale := func(v, size int) int {
		return camClamp1000(int(math.Round(float64(v) * 1000 / float64(size))))
	}
	return camBBox{
		X0: scale(r.Min.X, w),
		Y0: scale(r.Min.Y, h),
		X1: scale(r.Max.X, w),
		Y1: scale(r.Max.Y, h),
	}
}

// camAnnotateJPEG decodes src (jpeg/png), converts to RGBA, draws one amber
// ellipse outline per box (thickness max(3, imageWidth/300); plus a
// drawMosaicLabel index badge at each box's top-left when len(boxes)>1), and
// encodes JPEG quality 85. Returns the encoded bytes and the frame dimensions.
func camAnnotateJPEG(src []byte, boxes []camBBox) (jpg []byte, w, h int, err error) {
	img, _, err := image.Decode(bytes.NewReader(src))
	if err != nil {
		return nil, 0, 0, fmt.Errorf("decode frame: %w", err)
	}
	fb := img.Bounds()
	w, h = fb.Dx(), fb.Dy()
	if w <= 0 || h <= 0 {
		return nil, 0, 0, errors.New("empty frame")
	}
	canvas := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.Draw(canvas, canvas.Bounds(), img, fb.Min, draw.Src)

	thick := w / 300
	if thick < 3 {
		thick = 3
	}
	amber := color.RGBA{255, 200, 0, 255}
	for i, b := range boxes {
		r := camBBoxToPixels(b, w, h)
		if r.Empty() {
			continue
		}
		camDrawEllipse(canvas, r, amber, thick)
		if len(boxes) > 1 {
			drawMosaicLabel(canvas, r.Min.X, r.Min.Y, i+1)
		}
	}

	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, canvas, &jpeg.Options{Quality: 85}); err != nil {
		return nil, 0, 0, fmt.Errorf("encode jpeg: %w", err)
	}
	return buf.Bytes(), w, h, nil
}

// camCropJPEG crops b padded by padPct (15) each side, clamped to the frame,
// minimum 48px per axis; JPEG out. Used to mint avatar candidate/reference crops.
func camCropJPEG(src []byte, b camBBox, padPct int) (jpg []byte, w, h int, err error) {
	img, _, err := image.Decode(bytes.NewReader(src))
	if err != nil {
		return nil, 0, 0, fmt.Errorf("decode frame: %w", err)
	}
	fb := img.Bounds()
	fw, fh := fb.Dx(), fb.Dy()
	r := camBBoxToPixels(b, fw, fh)
	if r.Empty() {
		return nil, 0, 0, errors.New("crop box empty after clamping")
	}
	if padPct > 0 {
		px := r.Dx() * padPct / 100
		py := r.Dy() * padPct / 100
		r = image.Rect(r.Min.X-px, r.Min.Y-py, r.Max.X+px, r.Max.Y+py)
	}
	x0, x1 := camFitSpan(r.Min.X, r.Max.X, 48, fw)
	y0, y1 := camFitSpan(r.Min.Y, r.Max.Y, 48, fh)
	r = image.Rect(x0, y0, x1, y1)
	if r.Empty() {
		return nil, 0, 0, errors.New("crop box empty after clamping")
	}

	out := image.NewRGBA(image.Rect(0, 0, r.Dx(), r.Dy()))
	draw.Draw(out, out.Bounds(), img, fb.Min.Add(r.Min), draw.Src)
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, out, &jpeg.Options{Quality: 85}); err != nil {
		return nil, 0, 0, fmt.Errorf("encode jpeg: %w", err)
	}
	return buf.Bytes(), r.Dx(), r.Dy(), nil
}

// camFitSpan grows [lo,hi) to at least minSide (expanding around its center),
// then shifts/clamps it into [0,limit). The span only shrinks below minSide
// when the frame itself is smaller than minSide.
func camFitSpan(lo, hi, minSide, limit int) (int, int) {
	if hi < lo {
		lo, hi = hi, lo
	}
	if hi-lo < minSide {
		c := (lo + hi) / 2
		lo = c - minSide/2
		hi = lo + minSide
	}
	if lo < 0 {
		hi -= lo
		lo = 0
	}
	if hi > limit {
		lo -= hi - limit
		hi = limit
	}
	if lo < 0 {
		lo = 0
	}
	return lo, hi
}

// camDrawEllipse hand-rolls an ellipse outline inscribed in r: for each pixel in
// the (r grown by thick) region, paint it when it lies between the inner ellipse
// (inscribed in r) and the outer ellipse (r grown by thick) — the classic
// two-ellipse band test. Amber (255,200,0) matches the mosaic badge color.
func camDrawEllipse(dst *image.RGBA, r image.Rectangle, col color.RGBA, thick int) {
	cx, cy := float64(r.Min.X+r.Max.X)/2, float64(r.Min.Y+r.Max.Y)/2
	rx, ry := float64(r.Dx())/2, float64(r.Dy())/2
	if rx < 1 || ry < 1 {
		return
	}
	t := float64(thick)
	bounds := dst.Bounds()
	for y := r.Min.Y - thick; y <= r.Max.Y+thick; y++ {
		for x := r.Min.X - thick; x <= r.Max.X+thick; x++ {
			if !(image.Pt(x, y).In(bounds)) {
				continue
			}
			dx, dy := float64(x)+0.5-cx, float64(y)+0.5-cy
			in := dx*dx/(rx*rx) + dy*dy/(ry*ry)
			out := dx*dx/((rx+t)*(rx+t)) + dy*dy/((ry+t)*(ry+t))
			if in >= 1 && out <= 1 {
				dst.SetRGBA(x, y, col)
			}
		}
	}
}

// camBBoxConvention is the shared prompt snippet (design-avatars.md 3.0) that
// teaches a VLM the 0-1000 top-left bbox convention for a w×h-pixel frame.
func camBBoxConvention(w, h int) string {
	return fmt.Sprintf("COORDINATES: report any position as a bounding box "+
		"{\"x0\":int,\"y0\":int,\"x1\":int,\"y1\":int} on a normalized 0-1000 grid "+
		"where (0,0) is the image's TOP-LEFT corner and (1000,1000) its BOTTOM-RIGHT, "+
		"regardless of pixel size (this frame is %dx%d pixels). x0<x1 and y0<y1. "+
		"Make the box tight around the WHOLE person (head to feet), vehicle, or animal.", w, h)
}
