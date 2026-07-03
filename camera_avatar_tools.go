package main

// camera_avatar_tools.go — the five avatar investigate tools (design-avatars.md
// §2.4) dispatched by camExecuteInvestigateTool (camera_investigate.go):
//
//   - avatars      — list the site's enabled avatars in scope (Fetches 0)
//   - avatar_info  — full description + reference images shown next turn (Fetches 0)
//   - avatar_check — focused identity comparison against ONE camera at ONE
//     instant: S3 frame w/ DVR-fallback read-through → FACE PATH first (cheap,
//     no VLM), VLM comparison (camAvatarCheckPrompt) when the face path is
//     unavailable/ambiguous → on match, annotate + mint kind "annotated"; Fetches 2
//   - avatar_find  — bounded mini-scan of ONE camera across [from,to] (face
//     path ≈ free; VLM capped at cfg.CameraAvatarFindMaxVLM batches): sighting
//     timeline text + one annotated contact sheet; Fetches = VLM batches used
//     (face-path finds cost 1)
//   - annotate     — draw the model's bbox on an archived frame and mint it as
//     evidence (kind "annotated"); Fetches 1, no VLM
//
// BUDGET RULE: the loop's single Fetches budget (cfg.CameraInvestigateMaxMedia)
// counts frame fetches + VLM sub-calls, keeping camCountInvestigateProgress's
// resume accounting correct for free (set PROXY_CAMERA_INVESTIGATE_MAX_MEDIA=60
// in prod for avatar-heavy sites).
//
// Internal helpers here use the avTool/avFind prefixes to stay clear of the
// sibling avatar files (camera_avatars.go / camera_avatar_scan.go own the
// camAvatar* store + scan namespaces).

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	// avFindMaxFaceFetch caps avatar_find's MAIN-frame S3 pulls for the face
	// path — faces need pixels, but a whole-day window must not turn into
	// hundreds of main GETs. Motion survivors beyond the cap are dropped
	// lowest-motion first.
	avFindMaxFaceFetch = 120
	// avFindMinConfidence is the VLM plausibility floor (design 3.1: "Include
	// ONLY frames where the target is plausibly present (confidence ≥ 0.4)") —
	// applied defensively to whatever the model returns anyway.
	avFindMinConfidence = 0.4
	// avFindClusterGap merges sightings closer than this into one (design §2.3:
	// "matches <20s apart in one camera = one cluster; keep highest-confidence").
	avFindClusterGap = 20 * time.Second
	// avFindSheet* size avatar_find's annotated contact sheet: fewer, larger
	// cells than the scanning contact_sheet so the drawn circles stay legible.
	avFindSheetCols     = 6
	avFindSheetCellW    = 320
	avFindSheetCellH    = 180
	avFindSheetMaxCells = 60
)

// avVerdict is the tolerant decode of an avatar_check VLM reply:
// {"present":true,"confidence":0.85,"bbox":{...}|null,"reason":"..."}.
type avVerdict struct {
	Present    bool            `json:"present"`
	Confidence float64         `json:"confidence"`
	BBox       json.RawMessage `json:"bbox"`
	Reason     string          `json:"reason"`
}

// ─────────────────────────── avatars ───────────────────────────

// camToolAvatars lists the enabled avatars in scope for the active cameras
// (camAvatarsForCameras + camAvatarRoster). Text-only, Fetches 0.
func camToolAvatars(db *sql.DB, site camSite, active []camera) investigateToolResult {
	avatars, err := listCameraAvatars(db, site.ID, false)
	if err != nil {
		return investigateToolResult{Summary: "avatars: could not load the avatar registry: " + err.Error()}
	}
	scoped := camAvatarsForCameras(avatars, active)
	if len(scoped) == 0 {
		return investigateToolResult{Summary: "avatars: no avatars registered for this site."}
	}
	return investigateToolResult{Summary: "Known avatars:\n" + camAvatarRoster(scoped)}
}

// ─────────────────────────── avatar_info ───────────────────────────

// camToolAvatarInfo returns args.AvatarID's full description + external_ref +
// per-reference notes, attaching up to cfg.CameraAvatarMaxRefs+1 newest
// reference images (read from the pinned local media root — Fetches 0).
func camToolAvatarInfo(cfg config, db *sql.DB, site camSite, args investigateArgs, scratch string) investigateToolResult {
	av, errSum, ok := avToolAvatarByID(db, "avatar_info", args.AvatarID, site.ID)
	if !ok {
		return investigateToolResult{Summary: errSum}
	}
	media, err := avToolRefs(db, av.ID)
	if err != nil {
		return investigateToolResult{Summary: "avatar_info: could not load references: " + err.Error()}
	}
	paths, used := avToolRefPaths(db, media, avToolMaxRefs(cfg)+1)

	var b strings.Builder
	fmt.Fprintf(&b, "Avatar %s — %q (%s", av.ID, av.Name, avToolType(av))
	if av.IsGroup {
		b.WriteString(", group")
	}
	b.WriteString(")\n")
	if ref := strings.TrimSpace(av.ExternalRef); ref != "" {
		fmt.Fprintf(&b, "external_ref: %s\n", ref)
	}
	if desc := strings.TrimSpace(av.Description); desc != "" {
		fmt.Fprintf(&b, "description: %s\n", desc)
	} else {
		b.WriteString("description: (none recorded)\n")
	}
	if len(paths) == 0 {
		b.WriteString("No approved reference images yet — any matching will use the description alone.")
	} else {
		fmt.Fprintf(&b, "References: %d approved, the %d newest attached as images for your next turn:\n", len(media), len(paths))
		for i, m := range used {
			fmt.Fprintf(&b, "  ref %d [%s", i+1, firstNonEmpty(strings.TrimSpace(m.Source), "scan"))
			if m.CameraID != "" {
				fmt.Fprintf(&b, " %s", m.CameraID)
			}
			if ts := firstNonEmpty(strings.TrimSpace(m.FrameTS), strings.TrimSpace(m.CreatedAt)); ts != "" {
				fmt.Fprintf(&b, " @ %s", ts)
			}
			b.WriteString("]")
			if note := strings.TrimSpace(m.Note); note != "" {
				b.WriteString(": " + note)
			}
			b.WriteString("\n")
		}
		b.WriteString("Study the references before judging any frame.")
	}
	return investigateToolResult{Summary: strings.TrimRight(b.String(), "\n"), Images: paths}
}

// ─────────────────────────── avatar_check ───────────────────────────

// camToolAvatarCheck runs the focused identity comparison for args.AvatarID on
// ONE camera (args.CameraIDs[0]) at args.Time (snapped to the archive grid).
// alias is the run's analysis alias for the VLM fallback path.
func camToolAvatarCheck(ctx context.Context, cfg config, db *sql.DB, r *http.Request, site camSite, alias string, args investigateArgs, camByID map[string]camera, dvrByID map[string]CamDVR, allowed map[string]bool, scratch string, mediaLeft int) investigateToolResult {
	ids := camFilterAllowed(args.CameraIDs, allowed)
	if len(ids) == 0 {
		return investigateToolResult{Summary: "avatar_check: no valid camera_ids given (must be exactly ONE exact id from the roster)."}
	}
	id := ids[0]
	c, ok := camByID[id]
	if !ok {
		return investigateToolResult{Summary: "avatar_check: unknown camera " + id}
	}
	dv, ok := dvrByID[c.DVRID]
	if !ok || !dv.Enabled {
		return investigateToolResult{Summary: "avatar_check: camera " + id + " has no enabled DVR."}
	}
	t, terr := time.Parse(time.RFC3339, strings.TrimSpace(args.Time))
	if terr != nil {
		return investigateToolResult{Summary: `avatar_check: invalid "time" — pass one RFC3339 timestamp (e.g. 2026-07-02T08:12:45Z) for the archived instant to check.`}
	}
	if mediaLeft < 2 {
		return investigateToolResult{Summary: fmt.Sprintf("avatar_check: needs 2 media fetches but only %d remain — the investigation's media fetch budget is exhausted; answer with what you already have or ask the operator.", mediaLeft)}
	}
	av, errSum, ok := avToolAvatarByID(db, "avatar_check", args.AvatarID, site.ID)
	if !ok {
		return investigateToolResult{Summary: errSum}
	}

	q := StreamMain
	if strings.EqualFold(strings.TrimSpace(args.Quality), "sub") {
		q = StreamSub
	}
	ft := t
	if camS3Enabled(cfg) {
		ft = camS3SnapArchiveTime(t, avToolSnapInterval(cfg, q))
	}
	loc, tzName := avToolLoc(dv, dvrByID)
	disp := ft.In(loc).Format("Jan 2 15:04:05")

	frame, framePath, fw, fh, ok := avToolFetchFrame(ctx, cfg, dv, c, q, ft, scratch, "avcheck")
	if !ok {
		return investigateToolResult{Summary: fmt.Sprintf("avatar_check: no archived or recorded frame found for %s at %s (%s).", camDisplayName(c), disp, tzName), Fetches: 1}
	}

	media, merr := avToolRefs(db, av.ID)
	if merr != nil {
		return investigateToolResult{Summary: "avatar_check: could not load references: " + merr.Error(), Fetches: 1}
	}
	refPaths, _ := avToolRefPaths(db, media, avToolMaxRefs(cfg))
	refEmbeds := avToolRefEmbeddings(av, media)

	// FACE PATH first (design addendum): humans with embedded refs + sidecar up
	// get a cheap cosine verdict — no VLM call at all on a hit.
	faceNote := ""
	if avToolType(av) == "human" && camFaceEnabled(cfg) && len(refEmbeds) > 0 {
		faces, ferr := camFaceDetect(ctx, cfg, frame)
		switch {
		case ferr != nil:
			faceNote = " (face engine unavailable — used appearance matching.)"
		case len(faces) == 0:
			faceNote = " (no legible face in the frame — used appearance matching.)"
		default:
			bf, sim := avToolBestFace(faces, refEmbeds)
			thr := avToolFaceThreshold(cfg)
			if sim >= thr {
				reason := fmt.Sprintf("face similarity %.2f", sim)
				box := camBBoxFromPixels(bf.BBox, fw, fh)
				caption := fmt.Sprintf("%s at %s on %s (confidence %.2f)", av.Name, disp, camDisplayName(c), sim)
				item, annPath, minted := avToolMintAnnotated(db, cfg, r, site.ID, c.ID, q.String(), frame, box, ft, scratch, caption)
				res := investigateToolResult{
					Summary: fmt.Sprintf("avatar_check: verdict PRESENT (confidence %.2f) — %s [face match]. Annotated frame attached (the match is circled).", sim, reason),
					Fetches: 2,
				}
				if minted {
					res.Media = []evidenceItem{item}
					res.Images = []string{annPath}
				} else {
					res.Summary = fmt.Sprintf("avatar_check: verdict PRESENT (confidence %.2f) — %s [face match]. (Annotation could not be minted.)", sim, reason)
					res.Images = []string{framePath}
				}
				return res
			}
			faceNote = fmt.Sprintf(" (face path: best similarity %.2f below threshold %.2f — fell back to appearance matching.)", sim, thr)
		}
	}

	// VLM comparison (design 3.2): refs attached first, candidate frame last.
	callDir := filepath.Join(scratch, fmt.Sprintf("avcheck_%d", time.Now().UnixNano()))
	if err := os.MkdirAll(callDir, 0o700); err != nil {
		return investigateToolResult{Summary: "avatar_check: " + err.Error(), Fetches: 1}
	}
	attach := avToolCopyRefs(refPaths, callDir)
	nRefs := len(attach)
	candidate := filepath.Join(callDir, "candidate"+filepath.Ext(framePath))
	if copyFile(framePath, candidate, 0o600) != nil {
		candidate = framePath
	}
	attach = append(attach, candidate)

	tsLabel := ft.In(loc).Format(time.RFC3339)
	sys, user := camAvatarCheckPrompt(av, nRefs, camDisplayName(c), tsLabel, fw, fh)
	out, aerr := analyzeWithAlias(ctx, cfg, alias, sys, user, attach)
	if aerr != nil {
		return investigateToolResult{Summary: "avatar_check: comparison analysis failed: " + aerr.Error(), Fetches: 2}
	}
	v, perr := parseAvatarVerdict(out)
	if perr != nil {
		return investigateToolResult{Summary: "avatar_check: could not parse the comparison verdict (" + perr.Error() + ") — retry once or fall back to past_frames.", Fetches: 2}
	}
	if !v.Present {
		reason := firstNonEmpty(v.Reason, "no plausible match in the frame")
		return investigateToolResult{Summary: fmt.Sprintf("avatar_check: verdict ABSENT (confidence %.2f) — %s.%s", v.Confidence, reason, faceNote), Fetches: 2}
	}

	reason := firstNonEmpty(v.Reason, "appearance matches the references")
	summary := fmt.Sprintf("avatar_check: verdict PRESENT (confidence %.2f) — %s [appearance match].%s", v.Confidence, reason, faceNote)
	res := investigateToolResult{Summary: summary, Fetches: 2}
	if box, bok := camParseBBox(v.BBox); bok {
		caption := fmt.Sprintf("%s at %s on %s (confidence %.2f)", av.Name, disp, camDisplayName(c), v.Confidence)
		if item, annPath, minted := avToolMintAnnotated(db, cfg, r, site.ID, c.ID, q.String(), frame, box, ft, scratch, caption); minted {
			res.Summary = summary + " Annotated frame attached (the match is circled)."
			res.Media = []evidenceItem{item}
			res.Images = []string{annPath}
			return res
		}
	}
	res.Summary = summary + " (No usable bbox returned, so no annotated frame was minted — use the annotate tool if you can localize the match.)"
	res.Images = []string{framePath}
	return res
}

// ─────────────────────────── avatar_find ───────────────────────────

// camToolAvatarFind runs the bounded sighting scan for args.AvatarID on ONE
// camera (args.CameraIDs[0]) across [args.From, args.To].
func camToolAvatarFind(ctx context.Context, cfg config, db *sql.DB, r *http.Request, site camSite, alias string, args investigateArgs, camByID map[string]camera, dvrByID map[string]CamDVR, allowed map[string]bool, scratch string, mediaLeft int) investigateToolResult {
	ids := camFilterAllowed(args.CameraIDs, allowed)
	if len(ids) == 0 {
		return investigateToolResult{Summary: "avatar_find: no valid camera_ids given (must be exactly ONE exact id from the roster)."}
	}
	id := ids[0]
	c, ok := camByID[id]
	if !ok {
		return investigateToolResult{Summary: "avatar_find: unknown camera " + id}
	}
	dv, ok := dvrByID[c.DVRID]
	if !ok || !dv.Enabled {
		return investigateToolResult{Summary: "avatar_find: camera " + id + " has no enabled DVR."}
	}
	from, to, terr := camParseWindow(args.From, args.To, cfg, false)
	if terr != nil {
		return investigateToolResult{Summary: "avatar_find: " + terr.Error()}
	}
	if mediaLeft < 1 {
		return investigateToolResult{Summary: "avatar_find: the investigation's media fetch budget is exhausted — answer with what you already have or ask the operator."}
	}
	if !camS3Enabled(cfg) {
		return investigateToolResult{Summary: "avatar_find: the frame archive (S3) is not configured; this tool needs the archive. Use avatar_check at specific instants instead."}
	}
	av, errSum, ok := avToolAvatarByID(db, "avatar_find", args.AvatarID, site.ID)
	if !ok {
		return investigateToolResult{Summary: errSum}
	}
	loc, tzName := avToolLoc(dv, dvrByID)

	// Candidate instants coarsened to ≤ camContactMaxScan, then parallel SUB
	// fetches + motion signatures — the contact_sheet scan pipeline.
	instants, step := avFindInstants(from, to, camContactMaxScan)
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
			data, err := s3GetObject(ctx, cfg, camS3FrameKey(id, "sub", t))
			if err != nil {
				return
			}
			sig, serr := camMotionSignature(data)
			if serr != nil {
				return
			}
			scans[i] = scanFrame{t: t, sig: sig, data: data, ok: true}
		}(i, t)
	}
	wg.Wait()

	scanned := 0
	type survivor struct {
		t      time.Time
		data   []byte
		motion float64
	}
	var survivors []survivor
	var prevSig []uint8
	for i := range scans {
		if !scans[i].ok {
			continue
		}
		scanned++
		if prevSig == nil {
			survivors = append(survivors, survivor{t: scans[i].t, data: scans[i].data})
			prevSig = scans[i].sig
			continue
		}
		if m := camMotionScore(scans[i].sig, prevSig); m >= camContactMotionMin {
			survivors = append(survivors, survivor{t: scans[i].t, data: scans[i].data, motion: m})
			prevSig = scans[i].sig
		}
	}
	sparse := ""
	if step > 20*time.Second {
		sparse = fmt.Sprintf(" NOTE: the window is long, so the archive was sampled every %s — brief appearances between samples can be missed; narrow the window (or run avatar_find per sub-window) for denser coverage.", step)
	}
	if scanned == 0 {
		return investigateToolResult{Summary: fmt.Sprintf("avatar_find: no archived frames found for %s across %s (the window likely predates the 1-fps archive).", camDisplayName(c), windowLabel(from, to))}
	}
	if len(survivors) == 0 {
		return investigateToolResult{Summary: fmt.Sprintf("avatar_find: scanned %d archived frames for %s across %s and detected NO motion — nobody moved through the view, so there are no sightings of %q.%s", scanned, camDisplayName(c), windowLabel(from, to), av.Name, sparse), Fetches: 1}
	}

	media, merr := avToolRefs(db, av.ID)
	if merr != nil {
		return investigateToolResult{Summary: "avatar_find: could not load references: " + merr.Error(), Fetches: 1}
	}
	refPaths, _ := avToolRefPaths(db, media, avToolMaxRefs(cfg))
	refEmbeds := avToolRefEmbeddings(av, media)

	var (
		sightings []avFindSighting
		vlmCalls  int
		matchNote string
		fetches   = 1
	)
	faceEligible := avToolType(av) == "human" && camFaceEnabled(cfg) && len(refEmbeds) > 0
	faceRan := false

	if faceEligible {
		// FACE PATH: per motion-active instant, pull the MAIN frame (faces need
		// pixels; sub bytes are the fallback on an archive miss) and cosine-match.
		subset := append([]survivor(nil), survivors...) // copy — keep survivors chronological for the VLM fallback
		if len(subset) > avFindMaxFaceFetch {
			sort.Slice(subset, func(a, b int) bool { return subset[a].motion > subset[b].motion })
			subset = subset[:avFindMaxFaceFetch]
			sort.Slice(subset, func(a, b int) bool { return subset[a].t.Before(subset[b].t) })
		}
		thr := avToolFaceThreshold(cfg)
		faceErrs := 0
		faceRan = true
		for _, s := range subset {
			// The streaming archiver keys MAIN frames on the 1-fps grid (the 30s
			// snapshot-interval snap belongs to the legacy archiver and would miss
			// almost every main frame); look it up at the exact second and fall
			// back to the sub bytes we already have.
			frame := s.data
			quality := "sub"
			if b, gerr := s3GetObject(ctx, cfg, camS3FrameKey(id, "main", s.t)); gerr == nil {
				frame, quality = b, "main"
			}
			faces, ferr := camFaceDetect(ctx, cfg, frame)
			if ferr != nil {
				faceErrs++
				if faceErrs >= 3 && len(sightings) == 0 {
					faceRan = false // sidecar down — fall back to the VLM path below
					break
				}
				continue
			}
			if len(faces) == 0 {
				continue
			}
			if bf, sim := avToolBestFace(faces, refEmbeds); sim >= thr {
				fw, fh := avToolImageDims(frame)
				sightings = append(sightings, avFindSighting{
					T:          s.t,
					Confidence: sim,
					Reason:     fmt.Sprintf("face similarity %.2f", sim),
					BBox:       camBBoxFromPixels(bf.BBox, fw, fh),
					HasBBox:    fw > 0 && fh > 0,
					Frame:      frame,
					Quality:    quality,
				})
			}
		}
		if faceRan {
			matchNote = "matched via the face engine"
		} else {
			matchNote = "face engine unavailable — used appearance matching"
			sightings = nil
		}
	}

	if !faceRan {
		// VLM PATH: batch survivors, capped at cfg.CameraAvatarFindMaxVLM calls
		// AND at the remaining media budget (each batch is one budgeted fetch).
		if matchNote == "" {
			matchNote = "matched on appearance (no face path for this avatar)"
		}
		batchSize := cfg.CameraAvatarScanBatch
		if batchSize <= 0 {
			batchSize = 8
		}
		maxBatches := cfg.CameraAvatarFindMaxVLM
		if maxBatches <= 0 {
			maxBatches = 6
		}
		if maxBatches > mediaLeft {
			maxBatches = mediaLeft
		}
		subset := append([]survivor(nil), survivors...)
		if capFrames := maxBatches * batchSize; len(subset) > capFrames {
			sort.Slice(subset, func(a, b int) bool { return subset[a].motion > subset[b].motion })
			subset = subset[:capFrames]
			sort.Slice(subset, func(a, b int) bool { return subset[a].t.Before(subset[b].t) })
			sparse += fmt.Sprintf(" %d motion-active moments exceeded the VLM budget — only the %d busiest were compared.", len(survivors), capFrames)
		}
		findDir := filepath.Join(scratch, fmt.Sprintf("avfind_%d", time.Now().UnixNano()))
		refCopies := avToolCopyRefs(refPaths, findDir)
		var analysisErr string
		for b := 0; b*batchSize < len(subset) && b < maxBatches; b++ {
			batch := subset[b*batchSize : min((b+1)*batchSize, len(subset))]
			dir := filepath.Join(findDir, fmt.Sprintf("b%d", b+1))
			if os.MkdirAll(dir, 0o700) != nil {
				break
			}
			var (
				attach = append([]string{}, refCopies...)
				lines  []string
				times  []time.Time
			)
			for i, s := range batch {
				p := filepath.Join(dir, fmt.Sprintf("frame_%02d.jpg", i+1))
				if os.WriteFile(p, s.data, 0o600) != nil {
					continue
				}
				attach = append(attach, p)
				lines = append(lines, fmt.Sprintf("frame %d = %s", len(times)+1, s.t.In(loc).Format(time.RFC3339)))
				times = append(times, s.t)
			}
			if len(times) == 0 {
				continue
			}
			fw, fh := avToolImageDims(batch[0].data)
			sys := camAvatarScanSystemPrompt(av, len(refCopies) > 0, fw, fh)
			user := avFindUserText(av, len(refCopies), lines)
			out, aerr := analyzeWithAlias(ctx, cfg, alias, sys, user, attach)
			vlmCalls++
			if aerr != nil {
				analysisErr = aerr.Error()
				break
			}
			matches, perr := parseAvatarScanMatches(out)
			if perr != nil {
				analysisErr = "unparseable batch reply: " + perr.Error()
				continue
			}
			for _, m := range matches {
				idx := int(m.Frame)
				if idx < 1 || idx > len(times) || m.Confidence < avFindMinConfidence {
					continue
				}
				s := avFindSighting{T: times[idx-1], Confidence: min(m.Confidence, 1), Reason: strings.TrimSpace(m.Reason), Frame: batch[idx-1].data, Quality: "sub"}
				if box, bok := camParseBBox(m.BBox); bok {
					s.BBox, s.HasBBox = box, true
				}
				sightings = append(sightings, s)
			}
		}
		if vlmCalls > fetches {
			fetches = vlmCalls
		}
		if analysisErr != "" {
			sparse += " WARNING: appearance analysis stopped early (" + analysisErr + ") — coverage is partial."
		}
	}

	sightings = avFindCluster(sightings, avFindClusterGap)
	head := fmt.Sprintf("avatar_find: %d sighting(s) of %q on %s across %s — scanned %d archived frames, %d motion-active; %s.",
		len(sightings), av.Name, camDisplayName(c), windowLabel(from, to), scanned, len(survivors), matchNote)
	if len(sightings) == 0 {
		return investigateToolResult{Summary: head + sparse, Fetches: fetches}
	}

	// One annotated contact sheet from the matched frames' annotated versions.
	summary := head + fmt.Sprintf("\nSIGHTINGS (%s):\n%s", tzName, avFindTimeline(sightings, loc))
	var items []mosaicItem
	for _, s := range sightings {
		if len(items) >= avFindSheetMaxCells {
			summary += fmt.Sprintf("\nNOTE: only the first %d sightings fit on the sheet.", avFindSheetMaxCells)
			break
		}
		cell := s.Frame
		if s.HasBBox {
			if ann, _, _, aerr := camAnnotateJPEG(s.Frame, []camBBox{s.BBox}); aerr == nil {
				cell = ann
			}
		}
		p := filepath.Join(scratch, fmt.Sprintf("avfind_cell_%d_%d.jpg", len(items), time.Now().UnixNano()))
		if os.WriteFile(p, cell, 0o600) != nil {
			continue
		}
		items = append(items, mosaicItem{Index: len(items) + 1, CameraID: id, Name: s.T.In(loc).Format("15:04:05"), Path: p})
	}
	res := investigateToolResult{Fetches: fetches}
	if sheet, berr := buildContactSheet(items, avFindSheetCols, avFindSheetCellW, avFindSheetCellH); berr == nil {
		sheetPath := filepath.Join(scratch, fmt.Sprintf("avfind_sheet_%s_%d.png", id, time.Now().UnixNano()))
		if os.WriteFile(sheetPath, sheet.PNG, 0o600) == nil {
			caption := fmt.Sprintf("%s — %d sighting(s) on %s, %s", av.Name, len(sightings), camDisplayName(c), windowLabel(from, to))
			if token, perr := camPersistCapture(db, cfg, "", site.ID, id, "annotated", "sub", sheetPath, "image/png", sheet.Width, sheet.Height, from.Format(time.RFC3339), to.Format(time.RFC3339)); perr == nil {
				res.Media = []evidenceItem{{MediaURL: camMediaURL(cfg, r, token), Caption: caption}}
			}
			res.Images = []string{sheetPath}
			summary += "\nAnnotated contact sheet attached — numbered cells follow the sighting order above (matches circled). Confirm pivotal moments with avatar_check."
		}
	}
	res.Summary = summary + sparse
	return res
}

// avFindUserText renders the per-batch user message (design 3.1): target line,
// then the attachment-order map — refs first, then "frame N = <time>" lines.
func avFindUserText(av camAvatar, nRefs int, frameLines []string) string {
	var u strings.Builder
	fmt.Fprintf(&u, "TARGET: %s (%s).", av.Name, avToolType(av))
	if ref := strings.TrimSpace(av.ExternalRef); ref != "" {
		fmt.Fprintf(&u, " external_ref: %s.", ref)
	}
	fmt.Fprintf(&u, " DESCRIPTION: %s\n", firstNonEmpty(strings.TrimSpace(av.Description), "(no description recorded)"))
	if nRefs > 0 {
		fmt.Fprintf(&u, "Attachment order: ref_1..ref_%d = approved reference image(s) of the target (attached first), then the candidate frames:\n", nRefs)
	} else {
		u.WriteString("There are no reference images yet — the attachments are the candidate frames:\n")
	}
	u.WriteString(strings.Join(frameLines, "\n"))
	return u.String()
}

// ─────────────────────────── annotate ───────────────────────────

// camToolAnnotate fetches the archived frame for args.CameraIDs[0] at args.Time
// (S3 with DVR fallback), draws camParseBBox(args.BBox) via camAnnotateJPEG,
// and mints it as evidence (kind "annotated", normal retention). Fetches 1.
func camToolAnnotate(ctx context.Context, cfg config, db *sql.DB, r *http.Request, site camSite, args investigateArgs, camByID map[string]camera, allowed map[string]bool, scratch string) investigateToolResult {
	ids := camFilterAllowed(args.CameraIDs, allowed)
	if len(ids) == 0 {
		return investigateToolResult{Summary: "annotate: no valid camera_ids given (must be exactly ONE exact id from the roster)."}
	}
	id := ids[0]
	c, ok := camByID[id]
	if !ok {
		return investigateToolResult{Summary: "annotate: unknown camera " + id}
	}
	t, terr := time.Parse(time.RFC3339, strings.TrimSpace(args.Time))
	if terr != nil {
		return investigateToolResult{Summary: `annotate: invalid "time" — pass one RFC3339 timestamp (e.g. 2026-07-02T08:12:45Z) for the archived instant to annotate.`}
	}
	box, bok := camParseBBox(args.BBox)
	if !bok {
		return investigateToolResult{Summary: `annotate: invalid bbox — pass {"x0":int,"y0":int,"x1":int,"y1":int} on the normalized 0-1000 grid: (0,0) is the frame's TOP-LEFT corner and (1000,1000) its BOTTOM-RIGHT, x0<x1 and y0<y1, and the box must span at least 8/1000 in each axis.`}
	}
	// This tool's signature carries no dvrByID, so the DVR fallback loads the
	// device row itself (decrypted) — S3 stays the primary path.
	dv, derr := getCameraDVR(db, cfg, c.DVRID)
	if derr != nil || !dv.Enabled {
		return investigateToolResult{Summary: "annotate: camera " + id + " has no enabled DVR."}
	}
	q := StreamMain
	if strings.EqualFold(strings.TrimSpace(args.Quality), "sub") {
		q = StreamSub
	}
	ft := t
	if camS3Enabled(cfg) {
		ft = camS3SnapArchiveTime(t, avToolSnapInterval(cfg, q))
	}
	loc, tzName := avToolLoc(dv, nil)
	disp := ft.In(loc).Format("Jan 2 15:04:05")

	frame, _, _, _, ok := avToolFetchFrame(ctx, cfg, dv, c, q, ft, scratch, "annotate")
	if !ok {
		return investigateToolResult{Summary: fmt.Sprintf("annotate: no archived or recorded frame found for %s at %s (%s).", camDisplayName(c), disp, tzName), Fetches: 1}
	}
	caption := fmt.Sprintf("annotated — %s @ %s", camDisplayName(c), disp)
	item, annPath, minted := avToolMintAnnotated(db, cfg, r, site.ID, c.ID, q.String(), frame, box, ft, scratch, caption)
	if !minted {
		return investigateToolResult{Summary: "annotate: the frame was fetched but the annotation could not be drawn/saved.", Fetches: 1}
	}
	return investigateToolResult{
		Summary: fmt.Sprintf("annotate: circled the box on %s's frame at %s (%s) and saved it as evidence: %s", camDisplayName(c), disp, tzName, item.MediaURL),
		Media:   []evidenceItem{item},
		Images:  []string{annPath},
		Fetches: 1,
	}
}

// ─────────────────────────── prompt + verdict parsing ───────────────────────────

// camAvatarCheckPrompt builds avatar_check's comparison prompt pair
// (design-avatars.md 3.2: calibrated confidence tiers, camBBoxConvention(w,h),
// strict {"present","confidence","bbox","reason"} JSON) for an avatar with
// nRefs attached references, a candidate frame from camName at ts.
func camAvatarCheckPrompt(av camAvatar, nRefs int, camName, ts string, w, h int) (sys, user string) {
	typ := avToolType(av)
	var b strings.Builder
	if nRefs > 0 {
		fmt.Fprintf(&b, "You are a security-camera identification assistant. Decide whether the specific known %s described below appears in the CANDIDATE frame. The reference images (ref 1..%d, attached first) are operator-APPROVED prior sightings of them; the CANDIDATE frame (attached last) is from camera %q at %s.\n\n", typ, nRefs, camName, ts)
	} else {
		fmt.Fprintf(&b, "You are a security-camera identification assistant. Decide whether the specific known %s described below appears in the CANDIDATE frame. There are no reference images yet — match on the DESCRIPTION alone, and be extra conservative. The CANDIDATE frame (attached last) is from camera %q at %s.\n\n", typ, camName, ts)
	}
	b.WriteString("This is assistive matching reviewed by a human — not biometric proof. Calibrate confidence: 0.9+ only when clothing AND build AND context all agree with the references; 0.5–0.7 = plausible; below 0.4 = probably someone else. If several people are visible, evaluate each and report the best match only.\n\n")
	b.WriteString(camBBoxConvention(w, h))
	b.WriteString(" The bbox is for the CANDIDATE frame.\n\n")
	b.WriteString("Reply with EXACTLY ONE JSON object and nothing else:\n")
	b.WriteString(`{"present":true,"confidence":0.85,"bbox":{"x0":300,"y0":120,"x1":420,"y1":700},"reason":"same white coat and build as refs 1-2, at his usual arrival time"}` + "\n")
	b.WriteString(`bbox locates the matched person IN THE CANDIDATE FRAME; use "bbox":null when present=false.`)
	sys = b.String()

	var u strings.Builder
	fmt.Fprintf(&u, "TARGET: %s (%s).", av.Name, typ)
	if ref := strings.TrimSpace(av.ExternalRef); ref != "" {
		fmt.Fprintf(&u, " external_ref: %s.", ref)
	}
	fmt.Fprintf(&u, " DESCRIPTION: %s\n", firstNonEmpty(strings.TrimSpace(av.Description), "(no description recorded)"))
	if nRefs > 0 {
		fmt.Fprintf(&u, "Attachment order: ref_1..ref_%d = approved reference sightings (newest first); the LAST image is the CANDIDATE frame from %q at %s.", nRefs, camName, ts)
	} else {
		fmt.Fprintf(&u, "Attachment order: the only image is the CANDIDATE frame from %q at %s.", camName, ts)
	}
	user = u.String()
	return sys, user
}

// parseAvatarVerdict extracts and tolerantly decodes the single verdict object
// from a (possibly prose-wrapped or JSON-stringified) VLM reply via
// extractFirstJSONObject: present/confidence go through the flex decoders,
// confidence is clamped to [0,1], and a null/stringified bbox is normalized.
func parseAvatarVerdict(raw string) (avVerdict, error) {
	obj, ok := extractFirstJSONObject(raw)
	if !ok {
		// Whole reply may itself be a JSON-quoted string — unquote and rescan.
		var s string
		if json.Unmarshal([]byte(strings.TrimSpace(raw)), &s) == nil {
			obj, ok = extractFirstJSONObject(s)
		}
	}
	if !ok {
		return avVerdict{}, errors.New("no JSON verdict object found in model output")
	}
	var t struct {
		Present    flexBool        `json:"present"`
		Confidence flexFloat       `json:"confidence"`
		BBox       json.RawMessage `json:"bbox"`
		Reason     string          `json:"reason"`
	}
	if err := json.Unmarshal(obj, &t); err != nil {
		return avVerdict{}, fmt.Errorf("decode verdict JSON: %w", err)
	}
	v := avVerdict{Present: bool(t.Present), Confidence: float64(t.Confidence), Reason: strings.TrimSpace(t.Reason)}
	if v.Confidence < 0 {
		v.Confidence = 0
	}
	if v.Confidence > 1 {
		v.Confidence = 1
	}
	if bb := bytes.TrimSpace(t.BBox); len(bb) > 0 && string(bb) != "null" {
		if bb[0] == '"' { // stringified bbox: unwrap so camParseBBox sees the object
			var s string
			if json.Unmarshal(bb, &s) == nil {
				bb = []byte(s)
			}
		}
		v.BBox = json.RawMessage(bb)
	}
	return v, nil
}

// ─────────────────────────── sightings (pure) ───────────────────────────

// avFindSighting is one matched instant produced by avatar_find's mini-scan.
type avFindSighting struct {
	T          time.Time
	Confidence float64
	Reason     string
	BBox       camBBox
	HasBBox    bool
	Frame      []byte // source frame bytes (annotated copy goes on the sheet)
	Quality    string // sub | main
}

// avFindTimeline renders the sighting list as "HH:MM:SS conf 0.90 — reason"
// lines in loc, sorted chronologically. Pure — unit-tested directly.
func avFindTimeline(sightings []avFindSighting, loc *time.Location) string {
	if loc == nil {
		loc = time.UTC
	}
	sorted := append([]avFindSighting(nil), sightings...)
	sort.Slice(sorted, func(a, b int) bool { return sorted[a].T.Before(sorted[b].T) })
	var b strings.Builder
	for i, s := range sorted {
		if i > 0 {
			b.WriteString("\n")
		}
		fmt.Fprintf(&b, "%s conf %.2f", s.T.In(loc).Format("15:04:05"), s.Confidence)
		if reason := strings.TrimSpace(s.Reason); reason != "" {
			b.WriteString(" — " + reason)
		}
	}
	return b.String()
}

// avFindCluster collapses sightings whose consecutive gaps are under gap into
// one sighting each (the highest-confidence member wins — design §2.3's "<20s
// apart = one cluster"). Output is chronological. Pure — unit-tested directly.
func avFindCluster(sightings []avFindSighting, gap time.Duration) []avFindSighting {
	if len(sightings) <= 1 {
		return sightings
	}
	sorted := append([]avFindSighting(nil), sightings...)
	sort.Slice(sorted, func(a, b int) bool { return sorted[a].T.Before(sorted[b].T) })
	out := []avFindSighting{sorted[0]}
	lastT := sorted[0].T
	for _, s := range sorted[1:] {
		if s.T.Sub(lastT) < gap {
			if s.Confidence > out[len(out)-1].Confidence {
				out[len(out)-1] = s
			}
		} else {
			out = append(out, s)
		}
		lastT = s.T
	}
	return out
}

// avFindInstants returns the candidate instants across [from,to] on a step
// coarsened so at most max are produced (the contact_sheet stepping), plus the
// step used. Pure — unit-tested directly.
func avFindInstants(from, to time.Time, maxN int) ([]time.Time, time.Duration) {
	if maxN < 1 {
		maxN = 1
	}
	step := time.Second
	if span := to.Sub(from); span > 0 {
		if secs := int64(span / time.Second); secs > int64(maxN) {
			step = time.Duration(secs/int64(maxN)) * time.Second
			if step < time.Second {
				step = time.Second
			}
		}
	}
	var out []time.Time
	for t := from; !t.After(to); t = t.Add(step) {
		out = append(out, t.Truncate(time.Second))
		if len(out) >= maxN {
			break
		}
	}
	return out, step
}

// ─────────────────────────── shared plumbing ───────────────────────────

// avToolAvatarByID resolves a tool's avatar_id, returning an instructive
// summary (listing the valid ids) when it is blank, unknown, disabled, or
// scoped to a different site. siteID "" skips the site check (avatar_info's
// signature carries no site).
func avToolAvatarByID(db *sql.DB, tool, avatarID, siteID string) (camAvatar, string, bool) {
	id := strings.TrimSpace(avatarID)
	if id == "" {
		return camAvatar{}, tool + `: "avatar_id" is required — ` + avToolValidIDs(db, siteID), false
	}
	av, err := getCameraAvatar(db, id)
	if err != nil || (siteID != "" && av.SiteID != siteID) {
		return camAvatar{}, fmt.Sprintf("%s: unknown avatar_id %q — %s", tool, id, avToolValidIDs(db, siteID)), false
	}
	if !av.Enabled {
		return camAvatar{}, fmt.Sprintf("%s: avatar %s (%q) is disabled — an operator must re-enable it first.", tool, id, av.Name), false
	}
	return av, "", true
}

// avToolValidIDs renders the enabled avatar ids (site-scoped when siteID != "")
// for the helpful unknown-id tool errors.
func avToolValidIDs(db *sql.DB, siteID string) string {
	q := `SELECT id, name FROM camera_avatars WHERE enabled = 1`
	var params []any
	if siteID != "" {
		q += ` AND site_id = ?`
		params = append(params, siteID)
	}
	q += ` ORDER BY created_at LIMIT 30`
	rows, err := db.Query(q, params...)
	if err != nil {
		return "call the avatars tool to list the registered avatars."
	}
	defer rows.Close()
	var parts []string
	for rows.Next() {
		var id, name string
		if rows.Scan(&id, &name) == nil {
			parts = append(parts, fmt.Sprintf("%s (%s)", id, name))
		}
	}
	if len(parts) == 0 {
		return "no avatars are registered — there is nothing to match against."
	}
	return "valid ids: " + strings.Join(parts, ", ")
}

// avToolType normalizes an avatar's type for prompts/eligibility ("" → human).
func avToolType(av camAvatar) string {
	if t := strings.ToLower(strings.TrimSpace(av.Type)); t != "" {
		return t
	}
	return "human"
}

// avToolMaxRefs resolves cfg.CameraAvatarMaxRefs (default 3).
func avToolMaxRefs(cfg config) int {
	if cfg.CameraAvatarMaxRefs > 0 {
		return cfg.CameraAvatarMaxRefs
	}
	return 3
}

// avToolFaceThreshold resolves cfg.FaceMatchThreshold (default 0.40 cosine).
func avToolFaceThreshold(cfg config) float64 {
	if cfg.FaceMatchThreshold > 0 {
		return cfg.FaceMatchThreshold
	}
	return 0.40
}

// avToolSnapInterval is the archive-grid interval read-through lookups snap to,
// per quality (mirrors camToolPastFrames: 15s sub / 30s main defaults).
func avToolSnapInterval(cfg config, q StreamQuality) time.Duration {
	if q == StreamMain {
		if cfg.CameraArchiveMainInterval > 0 {
			return cfg.CameraArchiveMainInterval
		}
		return 30 * time.Second
	}
	if cfg.CameraArchiveInterval > 0 {
		return cfg.CameraArchiveInterval
	}
	return 15 * time.Second
}

// avToolLoc resolves the display timezone: the camera's own DVR first, then the
// site's other DVRs (camSiteTimezone falls back to time.Local).
func avToolLoc(dv CamDVR, dvrByID map[string]CamDVR) (*time.Location, string) {
	dvrs := []CamDVR{dv}
	for _, d := range dvrByID {
		if d.ID != dv.ID {
			dvrs = append(dvrs, d)
		}
	}
	return camSiteTimezone(dvrs)
}

// avToolFetchFrame is the single-instant S3 read-through the avatar tools share
// (the camToolPastFrames shape): try the archive at the grid-snapped instant t,
// fall back to a live DVR playback pull on a genuine miss and back-fill the
// archive, and return the frame BYTES (face path and annotator both need
// pixels) alongside the on-disk path and dimensions.
func avToolFetchFrame(ctx context.Context, cfg config, dv CamDVR, c camera, q StreamQuality, t time.Time, scratch, prefix string) (data []byte, path string, w, h int, ok bool) {
	qStr := q.String()
	dest := filepath.Join(scratch, fmt.Sprintf("%s_%s_%d.jpg", prefix, c.ID, time.Now().UnixNano()))
	s3Miss := false
	if camS3Enabled(cfg) {
		key := camS3FrameKey(c.ID, qStr, t)
		b, gerr := s3GetObject(ctx, cfg, key)
		switch {
		case gerr == nil:
			if os.WriteFile(dest, b, 0o600) == nil {
				w, h = imageDimensions(dest)
				camlog("debug", "avatar_frame", map[string]any{"camera_id": c.ID, "quality": qStr, "source": "s3", "time": t.Format(time.RFC3339)})
				return b, dest, w, h, true
			}
		case errors.Is(gerr, errS3NotFound):
			s3Miss = true // genuine miss — fall back to the DVR and back-fill below
		default:
			camlog("warn", "avatar_frame_s3", map[string]any{"camera_id": c.ID, "quality": qStr, "key": key, "op": "get", "error": gerr.Error()})
		}
	}
	res, cerr := captureFrameAtTime(ctx, cfg, dv, c.Channel, q, t, dest)
	if cerr != nil || res.Path == "" {
		return nil, "", 0, 0, false
	}
	b, rerr := os.ReadFile(res.Path)
	if rerr != nil {
		return nil, "", 0, 0, false
	}
	if s3Miss {
		if perr := s3PutObject(ctx, cfg, camS3FrameKey(c.ID, qStr, t), b, "image/jpeg"); perr != nil {
			camlog("warn", "avatar_frame_s3", map[string]any{"camera_id": c.ID, "quality": qStr, "op": "put", "error": perr.Error()})
		}
	}
	w, h = res.Width, res.Height
	if w == 0 || h == 0 {
		w, h = imageDimensions(res.Path)
	}
	camlog("debug", "avatar_frame", map[string]any{"camera_id": c.ID, "quality": qStr, "source": "dvr", "time": t.Format(time.RFC3339)})
	return b, res.Path, w, h, true
}

// avToolRefs loads an avatar's reference media sorted newest-first.
func avToolRefs(db *sql.DB, avatarID string) ([]camAvatarMedia, error) {
	media, err := listAvatarMedia(db, avatarID)
	if err != nil {
		return nil, err
	}
	sort.SliceStable(media, func(i, j int) bool { return media[i].CreatedAt > media[j].CreatedAt })
	return media, nil
}

// avToolRefPaths resolves up to max newest refs to their pinned local files
// (rows whose capture row or on-disk file is missing are skipped), returning
// the paths plus the media rows they belong to, in matching order.
func avToolRefPaths(db *sql.DB, media []camAvatarMedia, max int) ([]string, []camAvatarMedia) {
	var (
		paths []string
		used  []camAvatarMedia
	)
	for _, m := range media {
		if max > 0 && len(paths) >= max {
			break
		}
		cp, err := getCameraCaptureByToken(db, m.Token)
		if err != nil || strings.TrimSpace(cp.Path) == "" {
			continue
		}
		if _, serr := os.Stat(cp.Path); serr != nil {
			continue
		}
		paths = append(paths, cp.Path)
		used = append(used, m)
	}
	return paths, used
}

// avToolRefEmbeddings collects the usable face embeddings for the face path:
// each reference's own embedding plus the avatar's cached centroid.
func avToolRefEmbeddings(av camAvatar, media []camAvatarMedia) [][]float32 {
	var out [][]float32
	for _, m := range media {
		if v := camEmbeddingFromBlob(m.Embedding); len(v) > 0 {
			out = append(out, v)
		}
	}
	if v := camEmbeddingFromBlob(av.Embedding); len(v) > 0 {
		out = append(out, v)
	}
	return out
}

// avToolBestFace returns the detected face with the best cosine similarity
// against any reference embedding, and that similarity (-1 when nothing to
// compare).
func avToolBestFace(faces []camFace, refs [][]float32) (camFace, float64) {
	best := -1.0
	var bf camFace
	for _, f := range faces {
		for _, ref := range refs {
			if s := camCosine(f.Embedding, ref); s > best {
				best, bf = s, f
			}
		}
	}
	return bf, best
}

// avToolCopyRefs copies reference files into dir as ref_1..ref_N (keeping each
// source extension) so the VLM sees the labels the prompts promise. Failed
// copies are skipped — the prompt's nRefs is derived from the returned slice.
func avToolCopyRefs(refPaths []string, dir string) []string {
	if len(refPaths) == 0 {
		return nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil
	}
	var out []string
	for _, src := range refPaths {
		ext := filepath.Ext(src)
		if ext == "" {
			ext = ".jpg"
		}
		dst := filepath.Join(dir, fmt.Sprintf("ref_%d%s", len(out)+1, ext))
		if copyFile(src, dst, 0o600) != nil {
			continue
		}
		out = append(out, dst)
	}
	return out
}

// avToolImageDims decodes just the header of an in-memory image (0,0 on failure).
func avToolImageDims(b []byte) (int, int) {
	c, _, err := image.DecodeConfig(bytes.NewReader(b))
	if err != nil {
		return 0, 0
	}
	return c.Width, c.Height
}

// avToolMintAnnotated draws box on frame (camAnnotateJPEG's amber ellipse),
// persists the result as a served capture (kind "annotated", normal retention),
// and returns the citable media item plus the local path for the model's next
// turn. ok=false when drawing or persisting failed (already camlog'd upstream).
func avToolMintAnnotated(db *sql.DB, cfg config, r *http.Request, siteID, cameraID, quality string, frame []byte, box camBBox, ts time.Time, scratch, caption string) (evidenceItem, string, bool) {
	ann, w, h, err := camAnnotateJPEG(frame, []camBBox{box})
	if err != nil {
		camlog("warn", "avatar_annotate", map[string]any{"camera_id": cameraID, "ok": false, "error": err.Error()})
		return evidenceItem{}, "", false
	}
	path := filepath.Join(scratch, fmt.Sprintf("annotated_%s_%d.jpg", cameraID, time.Now().UnixNano()))
	if werr := os.WriteFile(path, ann, 0o600); werr != nil {
		return evidenceItem{}, "", false
	}
	tsStr := ts.Format(time.RFC3339)
	token, perr := camPersistCapture(db, cfg, "", siteID, cameraID, "annotated", quality, path, "image/jpeg", w, h, tsStr, tsStr)
	if perr != nil {
		camlog("warn", "avatar_annotate", map[string]any{"camera_id": cameraID, "ok": false, "error": perr.Error()})
		return evidenceItem{}, "", false
	}
	return evidenceItem{MediaURL: camMediaURL(cfg, r, token), Caption: caption}, path, true
}
