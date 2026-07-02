package main

// camera_contact_sheet.go — the investigate `contact_sheet` tool.
//
// Problem it solves: answering "how many people entered door X between T1 and T2,
// and when" by sampling a handful of frames across a long window is both too sparse
// (a person is on-camera for only ~2s) and too slow, and feeding the model hundreds
// of individual frames blows its time/token budget (it literally timed out at 300s).
//
// Instead: scan ONE camera's archived 1-fps frames across the window, DROP the
// static ones via frame-differencing, and tile only the moments where the scene
// CHANGED into a single NUMBERED contact sheet (reusing camera_mosaic.go). The model
// reviews one composite image, reads the "cell number -> time" map, and then drills
// into the specific cells that show a person via past_frames (main quality) to
// confirm and read the exact entry time.
//
// Motion detection (why not MD5): the DVR burns a timestamp into every frame and
// JPEG/sensor noise perturbs the bytes every second, so an exact hash flags EVERY
// frame as "different" even on a dead-static scene. We instead crop the timestamp
// band out and compare a small grayscale signature with a fuzzy mean-absolute-diff:
// a static scene sits at the ~0.2 noise floor, a moving person spikes well above it.

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	camContactMaxScan   = 600  // max candidate frames pulled+diffed from S3 per call
	camContactMaxCells  = 100  // max cells (kept, motion-filtered frames) per sheet
	camContactCols      = 10   // sheet grid width
	camContactCellW     = 160  // sheet cell width
	camContactCellH     = 90   // sheet cell height
	camContactSigW      = 48   // motion signature grid width
	camContactSigH      = 27   // motion signature grid height
	camContactMotionMin = 2.0  // mean-abs-gray-diff threshold (static ~0.2; motion spikes above)
	camContactFetchConc = 16   // parallel S3 GETs while scanning
)

// camToolContactSheet scans one camera's archived frames over [from,to], keeps only
// the frames where the scene changed (motion), and returns a numbered contact sheet
// plus a cell->time legend so the model can locate activity cheaply and then drill in.
func camToolContactSheet(ctx context.Context, cfg config, db *sql.DB, r *http.Request, site camSite, args investigateArgs, camByID map[string]camera, dvrByID map[string]CamDVR, allowed map[string]bool, scratch string, mediaLeft int) investigateToolResult {
	ids := camFilterAllowed(args.CameraIDs, allowed)
	if len(ids) == 0 {
		return investigateToolResult{Summary: "contact_sheet: no valid camera_ids given (must be an exact id from the roster)."}
	}
	if !camS3Enabled(cfg) {
		return investigateToolResult{Summary: "contact_sheet: the frame archive (S3) is not configured; this tool needs the archive. Use past_frames instead."}
	}
	id := ids[0] // one camera per call — this tool is for scanning a single door/area
	c, ok := camByID[id]
	if !ok {
		return investigateToolResult{Summary: "contact_sheet: unknown camera " + id}
	}
	dv := dvrByID[c.DVRID]
	from, to, terr := camParseWindow(args.From, args.To, cfg, false)
	if terr != nil {
		return investigateToolResult{Summary: "contact_sheet: " + terr.Error()}
	}
	q := StreamSub
	if strings.EqualFold(strings.TrimSpace(args.Quality), "main") {
		q = StreamMain
	}
	qStr := q.String()

	// mode: "motion" (default) keeps only frames where the scene CHANGED — right for
	// a door/quiet scene. "interval" is a uniform time-lapse (no motion filter) —
	// right for a BUSY area (reception) where constant staff movement makes motion
	// filtering useless; the model counts occupancy per cell / spots empty cells.
	mode := strings.ToLower(strings.TrimSpace(args.Mode))
	if mode == "uniform" {
		mode = "interval"
	}
	if mode != "interval" {
		mode = "motion"
	}
	needMotion := mode == "motion"

	loc := time.Local
	if dv.Timezone != "" {
		if l, e := time.LoadLocation(dv.Timezone); e == nil {
			loc = l
		}
	}

	// Candidate instants on the 1-second archive grid, coarsened so we scan at most
	// camContactMaxScan frames (long windows get a wider step).
	step := time.Second
	if span := to.Sub(from); span > 0 {
		if secs := int64(span / time.Second); secs > camContactMaxScan {
			step = time.Duration(secs/camContactMaxScan) * time.Second
			if step < time.Second {
				step = time.Second
			}
		}
	}
	var instants []time.Time
	for t := from; !t.After(to); t = t.Add(step) {
		instants = append(instants, t.Truncate(time.Second))
		if len(instants) >= camContactMaxScan {
			break
		}
	}

	// Fetch + fingerprint each candidate frame from S3 in parallel (archive only;
	// misses are simply skipped — a window predating the archive yields nothing).
	type scanFrame struct {
		t    time.Time
		sig  []uint8
		data []byte
		ok   bool
	}
	scans := make([]scanFrame, len(instants))
	sem := make(chan struct{}, camContactFetchConc)
	var wg sync.WaitGroup
	for i, t := range instants {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, t time.Time) {
			defer wg.Done()
			defer func() { <-sem }()
			data, err := s3GetObject(ctx, cfg, camS3FrameKey(id, qStr, t))
			if err != nil {
				return
			}
			var sig []uint8
			if needMotion {
				sg, serr := camMotionSignature(data)
				if serr != nil {
					return
				}
				sig = sg
			}
			scans[i] = scanFrame{t: t, sig: sig, data: data, ok: true}
		}(i, t)
	}
	wg.Wait()

	// Frames that actually came back from S3, already in time order (scans indexed
	// by the ascending instant list).
	var okIdx []int
	for i := range scans {
		if scans[i].ok {
			okIdx = append(okIdx, i)
		}
	}
	scanned := len(okIdx)
	if scanned == 0 {
		return investigateToolResult{Summary: fmt.Sprintf("contact_sheet: no archived frames found for %s across %s (the window likely predates the 1-fps archive).", camDisplayName(c), windowLabel(from, to))}
	}

	type keptFrame struct {
		idx    int
		motion float64
	}
	var keeps []keptFrame
	truncated := 0

	if needMotion {
		// MOTION mode: keep a frame only when it differs enough from the previous
		// KEPT frame (first frame is the baseline). Right for doors/quiet scenes.
		var prevSig []uint8
		for _, i := range okIdx {
			if prevSig == nil {
				keeps = append(keeps, keptFrame{idx: i, motion: 0})
				prevSig = scans[i].sig
				continue
			}
			m := camMotionScore(scans[i].sig, prevSig)
			if m >= camContactMotionMin {
				keeps = append(keeps, keptFrame{idx: i, motion: m})
				prevSig = scans[i].sig
			}
		}
		if len(keeps) == 0 {
			return investigateToolResult{Summary: fmt.Sprintf("contact_sheet: scanned %d archived frames for %s across %s and detected NO motion — the scene was static the whole time (nobody entered/moved). For a busy area, retry with mode \"interval\".", scanned, camDisplayName(c), windowLabel(from, to))}
		}
		if len(keeps) > camContactMaxCells {
			// keep the busiest cells, then restore chronological order below
			sort.Slice(keeps, func(a, b int) bool { return keeps[a].motion > keeps[b].motion })
			truncated = len(keeps) - camContactMaxCells
			keeps = keeps[:camContactMaxCells]
		}
	} else {
		// INTERVAL mode: uniform time-lapse — evenly pick up to camContactMaxCells
		// frames across the window regardless of motion. Right for busy areas
		// (occupancy over time / when-empty).
		sel := okIdx
		if len(sel) > camContactMaxCells {
			picked := make([]int, camContactMaxCells)
			for k := 0; k < camContactMaxCells; k++ {
				picked[k] = sel[k*(len(sel)-1)/(camContactMaxCells-1)]
			}
			truncated = len(sel) - camContactMaxCells
			sel = picked
		}
		for _, i := range sel {
			keeps = append(keeps, keptFrame{idx: i, motion: 0})
		}
	}
	sort.Slice(keeps, func(a, b int) bool { return scans[keeps[a].idx].t.Before(scans[keeps[b].idx].t) })

	// Write kept frames to disk and build the numbered sheet.
	var items []mosaicItem
	var legendLines []string
	for _, k := range keeps {
		s := scans[k.idx]
		p := filepath.Join(scratch, fmt.Sprintf("cs_%s_%02d.jpg", id, len(items)))
		if os.WriteFile(p, s.data, 0o600) != nil {
			continue
		}
		n := len(items) + 1
		items = append(items, mosaicItem{Index: n, CameraID: id, Name: s.t.In(loc).Format("15:04:05"), Path: p})
		legendLines = append(legendLines, fmt.Sprintf("%d=%s", n, s.t.In(loc).Format("15:04:05")))
	}
	if len(items) == 0 {
		return investigateToolResult{Summary: "contact_sheet: detected activity but could not assemble the frames."}
	}

	sheet, berr := buildContactSheet(items, camContactCols, camContactCellW, camContactCellH)
	if berr != nil {
		return investigateToolResult{Summary: "contact_sheet: " + berr.Error()}
	}
	sheetPath := filepath.Join(scratch, fmt.Sprintf("contact_%s_%d.png", id, time.Now().UnixNano()))
	if werr := os.WriteFile(sheetPath, sheet.PNG, 0o600); werr != nil {
		return investigateToolResult{Summary: "contact_sheet: could not write sheet: " + werr.Error()}
	}

	var media []evidenceItem
	if token, perr := camPersistCapture(db, cfg, "", site.ID, id, "mosaic", qStr, sheetPath, "image/png", sheet.Width, sheet.Height, "", ""); perr == nil {
		media = append(media, evidenceItem{MediaURL: camMediaURL(cfg, r, token), Caption: fmt.Sprintf("contact sheet — %s %s", camDisplayName(c), windowLabel(from, to))})
	}

	var lead string
	if needMotion {
		lead = fmt.Sprintf("Contact sheet (MOTION mode) for %s across %s: scanned %d archived %s frames, kept %d NUMBERED cells where the scene CHANGED. Each cell is a moment of activity — pick the cells showing a person and past_frames those exact times (quality \"main\") to confirm and read each time. The SAME person in cells a few seconds apart is ONE entry, not several.",
			camDisplayName(c), windowLabel(from, to), scanned, qStr, len(items))
	} else {
		lead = fmt.Sprintf("Contact sheet (INTERVAL time-lapse) for %s across %s: %d NUMBERED cells sampled evenly from %d archived %s frames (motion NOT filtered — right for a busy area). Count the people visible in each cell over time; use the roster to tell staff from visitors; cells with nobody show when it was empty. past_frames a cell's time (quality \"main\") for detail.",
			camDisplayName(c), windowLabel(from, to), len(items), scanned, qStr)
	}
	summary := lead + fmt.Sprintf("\nCELL=TIME (%s): %s", loc.String(), strings.Join(legendLines, "  "))
	if truncated > 0 {
		if needMotion {
			summary += fmt.Sprintf("\nNOTE: %d more active moments existed than fit on one sheet — narrow the window and scan again for the rest.", truncated)
		} else {
			summary += "\nNOTE: sampled evenly to fit one sheet — narrow the window for finer time resolution."
		}
	}
	return investigateToolResult{Summary: summary, Media: media, Images: []string{sheetPath}, Fetches: 1}
}

// camMotionSignature decodes a JPEG frame and returns a small grayscale signature
// (camContactSigW x camContactSigH) over the CENTER band only — the top and bottom
// ~12% are cropped to exclude the DVR's burned-in timestamp OSD, which changes every
// second and would otherwise register as constant "motion".
func camMotionSignature(data []byte) ([]uint8, error) {
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	b := img.Bounds()
	if b.Dx() <= 0 || b.Dy() <= 0 {
		return nil, fmt.Errorf("empty image")
	}
	y0 := b.Min.Y + b.Dy()*12/100
	y1 := b.Max.Y - b.Dy()*12/100
	if y1 <= y0 {
		y0, y1 = b.Min.Y, b.Max.Y
	}
	ch, cw := y1-y0, b.Dx()
	sig := make([]uint8, camContactSigW*camContactSigH)
	for gy := 0; gy < camContactSigH; gy++ {
		sy := y0 + gy*ch/camContactSigH
		for gx := 0; gx < camContactSigW; gx++ {
			sx := b.Min.X + gx*cw/camContactSigW
			rr, gg, bb, _ := img.At(sx, sy).RGBA()
			luma := (299*(rr>>8) + 587*(gg>>8) + 114*(bb>>8)) / 1000
			sig[gy*camContactSigW+gx] = uint8(luma)
		}
	}
	return sig, nil
}

// camMotionScore is the mean absolute per-pixel difference between two signatures
// (0 = identical). Static scenes sit near the sensor/JPEG noise floor (~0.2); a
// moving person pushes it well above camContactMotionMin.
func camMotionScore(a, b []uint8) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 1e9
	}
	var s int
	for i := range a {
		d := int(a[i]) - int(b[i])
		if d < 0 {
			d = -d
		}
		s += d
	}
	return float64(s) / float64(len(a))
}
