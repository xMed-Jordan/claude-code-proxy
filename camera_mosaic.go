package main

// camera_mosaic.go — pure-Go tiling of per-camera snapshots into one low-cost
// composite image the AI scans first before escalating to full-resolution images
// or clips (see the plan's escalation ladder). Zero new dependencies: downscaling
// is a hand-rolled box filter, and palette reduction uses the Go STANDARD LIBRARY's
// image/color/palette + image/draw.FloydSteinberg (not golang.org/x/image — that
// upgrade is explicitly reserved for later, only if label legibility review
// demands crisper fonts/scaling).
//
// Callers are expected to pass SUB-stream snapshots (cheap, low-res) per the
// plan's "monitoring works from the cheap sub stream" guidance — this file has no
// opinion on stream quality, it just composes whatever image paths it's given.
//
// On-image labels are a small hand-rolled 3x5 pixel digit font (no text/font
// dependency). The pixel label is only a visual cross-reference for a human
// glancing at the mosaic; the authoritative index -> camera mapping is the
// returned Legend, which callers (cameraorch.go) fold into the analysis prompt
// text so the model can refer to "camera 3" unambiguously.
import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/color/palette"
	"image/draw"
	_ "image/gif"
	_ "image/jpeg"
	"image/png"
	"os"
	"strconv"
)

// mosaicItem is one camera's already-captured snapshot to place in the mosaic.
// Index, when 0, is assigned sequentially (1-based) in the order items appear.
type mosaicItem struct {
	Index    int
	CameraID string
	Name     string
	Path     string // decodable image file (jpeg/png/gif)
}

// mosaicLegendEntry maps an on-image numeric label back to the real camera.
type mosaicLegendEntry struct {
	Index    int
	CameraID string
	Name     string
}

// mosaicResult is the encoded PNG plus the legend callers must include (as text)
// alongside the image when handing it to an analysis backend.
type mosaicResult struct {
	PNG    []byte
	Width  int
	Height int
	Legend []mosaicLegendEntry
}

// buildMosaic tiles items into a cfg.CameraMosaicCols-wide grid of
// cfg.CameraMosaicCellW x cfg.CameraMosaicCellH cells, box-downscaling each source
// image to fill its cell, labeling each cell with its 1-based index, dithering the
// composite to a standard-library palette, and PNG-encoding it. Items beyond
// cfg.CameraMosaicMaxCams are dropped (with a warn log) — the caller decides which
// subset matters most if a watch covers more cameras than that.
func buildMosaic(cfg config, items []mosaicItem) (mosaicResult, error) {
	if len(items) == 0 {
		return mosaicResult{}, fmt.Errorf("mosaic: no items")
	}
	maxCams := cfg.CameraMosaicMaxCams
	if maxCams <= 0 {
		maxCams = 16
	}
	if len(items) > maxCams {
		camlog("warn", "mosaic_truncated", map[string]any{
			"requested": len(items), "max_cams": maxCams, "ok": true,
		})
		items = items[:maxCams]
	}
	cols := cfg.CameraMosaicCols
	if cols <= 0 {
		cols = 4
	}
	cellW := cfg.CameraMosaicCellW
	if cellW <= 0 {
		cellW = 320
	}
	cellH := cfg.CameraMosaicCellH
	if cellH <= 0 {
		cellH = 180
	}

	rows := (len(items) + cols - 1) / cols
	gridW, gridH := cols*cellW, rows*cellH
	canvas := image.NewRGBA(image.Rect(0, 0, gridW, gridH))
	// Dark-gray background so an empty trailing cell (last row, when the count
	// doesn't evenly divide cols) or a failed capture doesn't leave a jarring
	// black/transparent hole.
	draw.Draw(canvas, canvas.Bounds(), &image.Uniform{color.RGBA{32, 32, 32, 255}}, image.Point{}, draw.Src)

	legend := make([]mosaicLegendEntry, 0, len(items))
	for i, item := range items {
		col, row := i%cols, i/cols
		ox, oy := col*cellW, row*cellH
		cellRect := image.Rect(ox, oy, ox+cellW, oy+cellH)

		if cellImg, err := loadAndFitCell(item.Path, cellW, cellH); err != nil {
			camlog("warn", "mosaic_cell_failed", map[string]any{
				"camera_id": item.CameraID, "path": item.Path, "ok": false, "error": err.Error(),
			})
		} else {
			draw.Draw(canvas, cellRect, cellImg, image.Point{}, draw.Src)
		}

		idx := item.Index
		if idx <= 0 {
			idx = i + 1
		}
		drawMosaicLabel(canvas, ox, oy, idx)
		legend = append(legend, mosaicLegendEntry{Index: idx, CameraID: item.CameraID, Name: item.Name})
	}

	dst := image.NewPaletted(canvas.Bounds(), palette.Plan9)
	draw.FloydSteinberg.Draw(dst, canvas.Bounds(), canvas, image.Point{})

	var buf bytes.Buffer
	if err := png.Encode(&buf, dst); err != nil {
		return mosaicResult{}, fmt.Errorf("encode mosaic png: %w", err)
	}
	camlog("info", "mosaic_build", map[string]any{
		"ok": true, "cameras": len(items), "cols": cols, "rows": rows,
		"width": gridW, "height": gridH, "bytes": buf.Len(),
	})
	return mosaicResult{PNG: buf.Bytes(), Width: gridW, Height: gridH, Legend: legend}, nil
}

// buildContactSheet tiles items into an explicit cols-wide grid of cellW x cellH
// cells — like buildMosaic but with caller-chosen geometry and NO max-cams cap —
// for the investigate contact_sheet tool: many time-sampled frames of ONE camera
// laid out as a numbered grid so the model can scan a whole window in a single image
// and then drill into the cells that show activity. Each cell is labeled with its
// 1-based index; the returned Legend maps index -> the frame's timestamp (carried in
// each item's Name).
func buildContactSheet(items []mosaicItem, cols, cellW, cellH int) (mosaicResult, error) {
	if len(items) == 0 {
		return mosaicResult{}, fmt.Errorf("contact sheet: no items")
	}
	if cols <= 0 {
		cols = 10
	}
	if cellW <= 0 {
		cellW = 160
	}
	if cellH <= 0 {
		cellH = 90
	}
	rows := (len(items) + cols - 1) / cols
	gridW, gridH := cols*cellW, rows*cellH
	canvas := image.NewRGBA(image.Rect(0, 0, gridW, gridH))
	draw.Draw(canvas, canvas.Bounds(), &image.Uniform{color.RGBA{32, 32, 32, 255}}, image.Point{}, draw.Src)

	legend := make([]mosaicLegendEntry, 0, len(items))
	for i, item := range items {
		col, row := i%cols, i/cols
		ox, oy := col*cellW, row*cellH
		cellRect := image.Rect(ox, oy, ox+cellW, oy+cellH)
		if cellImg, err := loadAndFitCell(item.Path, cellW, cellH); err != nil {
			camlog("warn", "contact_cell_failed", map[string]any{"path": item.Path, "ok": false, "error": err.Error()})
		} else {
			draw.Draw(canvas, cellRect, cellImg, image.Point{}, draw.Src)
		}
		idx := item.Index
		if idx <= 0 {
			idx = i + 1
		}
		drawMosaicLabel(canvas, ox, oy, idx)
		legend = append(legend, mosaicLegendEntry{Index: idx, CameraID: item.CameraID, Name: item.Name})
	}

	dst := image.NewPaletted(canvas.Bounds(), palette.Plan9)
	draw.FloydSteinberg.Draw(dst, canvas.Bounds(), canvas, image.Point{})
	var buf bytes.Buffer
	if err := png.Encode(&buf, dst); err != nil {
		return mosaicResult{}, fmt.Errorf("encode contact sheet png: %w", err)
	}
	return mosaicResult{PNG: buf.Bytes(), Width: gridW, Height: gridH, Legend: legend}, nil
}

// loadAndFitCell decodes path and box-downscales it to exactly w x h (stretch-fit
// — the plan calls for cellW x cellH cells, not aspect-preserved letterboxing).
func loadAndFitCell(path string, w, h int) (image.Image, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	src, _, err := image.Decode(f)
	if err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	return boxDownscale(src, w, h), nil
}

// boxDownscale resizes src to exactly dstW x dstH by averaging each destination
// pixel's corresponding source box (a standard "area"/box filter) — good enough
// quality for a monitoring mosaic without pulling in a resize library. Nearly
// always downscaling in practice (a sub-stream frame into a small cell), but the
// box-mapping math works for upscaling too (each destination pixel just maps to a
// 0- or 1-pixel source box).
func boxDownscale(src image.Image, dstW, dstH int) *image.RGBA {
	dst := image.NewRGBA(image.Rect(0, 0, dstW, dstH))
	sb := src.Bounds()
	sw, sh := sb.Dx(), sb.Dy()
	if sw <= 0 || sh <= 0 || dstW <= 0 || dstH <= 0 {
		return dst
	}
	for dy := 0; dy < dstH; dy++ {
		sy0 := sb.Min.Y + dy*sh/dstH
		sy1 := sb.Min.Y + (dy+1)*sh/dstH
		if sy1 <= sy0 {
			sy1 = sy0 + 1
		}
		for dx := 0; dx < dstW; dx++ {
			sx0 := sb.Min.X + dx*sw/dstW
			sx1 := sb.Min.X + (dx+1)*sw/dstW
			if sx1 <= sx0 {
				sx1 = sx0 + 1
			}
			var rs, gs, bs, as, n uint64
			for y := sy0; y < sy1 && y < sb.Max.Y; y++ {
				for x := sx0; x < sx1 && x < sb.Max.X; x++ {
					r, g, b, a := src.At(x, y).RGBA()
					rs += uint64(r)
					gs += uint64(g)
					bs += uint64(b)
					as += uint64(a)
					n++
				}
			}
			if n == 0 {
				n = 1
			}
			dst.SetRGBA(dx, dy, color.RGBA{
				R: uint8((rs / n) >> 8),
				G: uint8((gs / n) >> 8),
				B: uint8((bs / n) >> 8),
				A: uint8((as / n) >> 8),
			})
		}
	}
	return dst
}

// ─────────────────────────── on-image numeric label ───────────────────────────

// mosaicDigitFont is a hand-rolled 3-column x 5-row bitmap font for '0'-'9', each
// row a 3-bit mask (bit 2 = leftmost column). Written as binary literals so the
// shape is visible directly in the source (top-to-bottom row order).
var mosaicDigitFont = map[byte][5]uint8{
	'0': {0b111, 0b101, 0b101, 0b101, 0b111},
	'1': {0b010, 0b110, 0b010, 0b010, 0b111},
	'2': {0b111, 0b001, 0b111, 0b100, 0b111},
	'3': {0b111, 0b001, 0b111, 0b001, 0b111},
	'4': {0b101, 0b101, 0b111, 0b001, 0b001},
	'5': {0b111, 0b100, 0b111, 0b001, 0b111},
	'6': {0b111, 0b100, 0b111, 0b101, 0b111},
	'7': {0b111, 0b001, 0b010, 0b010, 0b010},
	'8': {0b111, 0b101, 0b111, 0b101, 0b111},
	'9': {0b111, 0b101, 0b111, 0b001, 0b111},
}

// drawMosaicLabel paints a small dark badge with idx (zero-padded to 2 digits —
// PROXY_CAM_MOSAIC_MAX_CAMS defaults to 16 and is rarely raised much past that) in
// the top-left corner of the cell at (ox,oy).
func drawMosaicLabel(canvas *image.RGBA, ox, oy, idx int) {
	label := strconv.Itoa(idx)
	if len(label) < 2 {
		label = "0" + label
	}
	const scale = 3
	const glyphW, glyphH = 3, 5
	const pad = 4
	badgeW := pad*2 + len(label)*(glyphW+1)*scale
	badgeH := pad*2 + glyphH*scale
	draw.Draw(canvas, image.Rect(ox, oy, ox+badgeW, oy+badgeH),
		&image.Uniform{color.RGBA{0, 0, 0, 210}}, image.Point{}, draw.Over)

	x, y := ox+pad, oy+pad
	amber := color.RGBA{255, 200, 0, 255}
	for i := 0; i < len(label); i++ {
		if glyph, ok := mosaicDigitFont[label[i]]; ok {
			drawDigitGlyph(canvas, x, y, glyph, scale, amber)
		}
		x += (glyphW + 1) * scale
	}
}

// drawDigitGlyph draws one 3x5 glyph at (x,y), each "on" bit rendered as a
// scale x scale filled square.
func drawDigitGlyph(canvas *image.RGBA, x, y int, glyph [5]uint8, scale int, col color.RGBA) {
	for row := 0; row < glyphRows; row++ {
		bits := glyph[row]
		for c := 0; c < glyphCols; c++ {
			if bits&(1<<(glyphCols-1-c)) == 0 {
				continue
			}
			r := image.Rect(x+c*scale, y+row*scale, x+(c+1)*scale, y+(row+1)*scale)
			draw.Draw(canvas, r, &image.Uniform{col}, image.Point{}, draw.Src)
		}
	}
}

const (
	glyphCols = 3
	glyphRows = 5
)
