package main

// camera_describe.go — the setup/describe phase of the orchestration brain (part
// 2, after cameraorch.go's AI bridge). This is what turns "the operator added a
// DVR and wrote a description of the place" into per-camera AI descriptions,
// area grouping, and an inbox of clarifying questions.
//
// runSiteAnalysis(ctx,cfg,siteID) drives the whole flow:
//
//  1. Capture ONE low-bitrate (sub-stream) snapshot per ENABLED camera on the
//     site (captureSnapshot from camera_capture.go, bounded by captureSem).
//  2. Classify each snapshot with isBlankSnapshot: a black / no-signal / undecodable
//     frame means the DVR has that channel turned off, so the camera is
//     auto-disabled (enabled=0 + disabled_reason persisted, the reason camlog'd)
//     and EXCLUDED from the AI call — we never pay a vision model to look at a
//     black rectangle, and we never invent a description for a dead feed.
//  3. Persist each good snapshot as a served camera_captures artifact and link it
//     to the camera (snapshot_capture_id) so the UI grid can show a live thumb —
//     done immediately, so the thumbnail survives even if the AI step later fails.
//  4. Make exactly ONE analyzeWithAlias call (agy | claude | codex, per the site's
//     analysis_alias or cfg.CameraAnalysisAlias) with a strict structured-output
//     contract: {cameras:[…], areas:[…], questions:[…]}. Parse it tolerantly
//     (extractFirstJSONObject + a parseDecision-style normalizer that drops any
//     camera id the model hallucinated); on a parse failure, do ONE repair round
//     asking for JSON only before giving up.
//  5. Persist per-camera ai_description / ai_location / area, the whole grouping
//     into camera_sites.analysis_json, and open camera_questions rows.
//
// Re-analyze is the SAME function: it folds every ANSWERED question into the
// system prompt ("Operator clarifications: Q -> A") so the operator's replies
// sharpen the next pass, and supersedes stale still-open questions.
//
// The site's analysis_status is a running → idle/error state machine the UI polls
// (startSiteAnalysis flips it to running synchronously and runs the rest in a
// goroutine). OBSERVABILITY: every capture already logs itself in camera_capture.go;
// this file adds start/exclude/parse/done camlog lines (+ camera_events rows) so a
// stuck or empty analysis is diagnosable from the trail.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// ─────────────────────────── model output contract ───────────────────────────

// setupAnalysis is the strict single-object JSON the analysis model must emit for
// the describe phase. Field names mirror the plan exactly so the prompt and the
// decoder never drift.
type setupAnalysis struct {
	Cameras   []setupCamera   `json:"cameras"`
	Areas     []setupArea     `json:"areas"`
	Questions []setupQuestion `json:"questions"`
}

// setupCamera is the model's description of one camera it was shown. Confidence
// and looks_disabled use the tolerant flexFloat/flexBool decoders from
// cameraorch.go via the custom UnmarshalJSON below, so a model that emits
// "confidence":"0.8" or "looks_disabled":"yes" doesn't fail the whole parse.
type setupCamera struct {
	CameraID      string  `json:"camera_id"`
	Description   string  `json:"description"`
	LocationGuess string  `json:"location_guess"`
	Confidence    float64 `json:"confidence"`
	LooksDisabled bool    `json:"looks_disabled"`
}

// UnmarshalJSON decodes a setupCamera through an auxiliary shape whose numeric/
// boolean fields tolerate stringified encodings (mirrors decision.UnmarshalJSON).
func (c *setupCamera) UnmarshalJSON(b []byte) error {
	var aux struct {
		CameraID      string    `json:"camera_id"`
		Description   string    `json:"description"`
		LocationGuess string    `json:"location_guess"`
		Confidence    flexFloat `json:"confidence"`
		LooksDisabled flexBool  `json:"looks_disabled"`
	}
	if err := json.Unmarshal(b, &aux); err != nil {
		return err
	}
	c.CameraID = aux.CameraID
	c.Description = aux.Description
	c.LocationGuess = aux.LocationGuess
	c.Confidence = float64(aux.Confidence)
	c.LooksDisabled = bool(aux.LooksDisabled)
	return nil
}

// setupArea is one logical grouping of cameras (e.g. "Waiting room") with the
// model's rationale for the grouping.
type setupArea struct {
	Area      string   `json:"area"`
	CameraIDs []string `json:"camera_ids"`
	Rationale string   `json:"rationale"`
}

// setupQuestion is a clarifying question the model wants to ask the operator,
// scoped to the referenced camera ids (may be empty for a general question).
type setupQuestion struct {
	Question  string   `json:"question"`
	CameraIDs []string `json:"camera_ids"`
}

// ─────────────────────── persisted analysis (camera_sites.analysis_json) ───────────────────────

// siteAnalysisResult is what gets stored in camera_sites.analysis_json and read
// back by the UI. It echoes the per-camera descriptions with their resolved area
// and snapshot token (for thumbnails), the grouping, the ids of the questions
// opened this pass, and the cameras excluded from the AI (with why). On failure
// only AnalyzedAt + Error are populated.
type siteAnalysisResult struct {
	AnalyzedAt  string              `json:"analyzed_at"`
	Alias       string              `json:"alias,omitempty"`
	Backend     string              `json:"backend,omitempty"`
	Cameras     []setupCameraResult `json:"cameras,omitempty"`
	Areas       []setupArea         `json:"areas,omitempty"`
	QuestionIDs []string            `json:"question_ids,omitempty"`
	Excluded    []excludedCamera    `json:"excluded,omitempty"`
	Note        string              `json:"note,omitempty"`
	Error       string              `json:"error,omitempty"`
}

// setupCameraResult is one row of the persisted per-camera result (the model's
// description joined with the area it landed in and its served snapshot token).
type setupCameraResult struct {
	CameraID      string  `json:"camera_id"`
	Name          string  `json:"name,omitempty"`
	Channel       int     `json:"channel"`
	Description   string  `json:"description"`
	LocationGuess string  `json:"location_guess"`
	Area          string  `json:"area,omitempty"`
	Confidence    float64 `json:"confidence"`
	LooksDisabled bool    `json:"looks_disabled,omitempty"`
	SnapshotToken string  `json:"snapshot_token,omitempty"`
}

// excludedCamera records a camera left out of the AI call and why (a disabled or
// unreachable feed) so the UI can explain the gap.
type excludedCamera struct {
	CameraID string `json:"camera_id"`
	Name     string `json:"name,omitempty"`
	Channel  int    `json:"channel"`
	Reason   string `json:"reason"`
}

// ─────────────────────────── async entrypoint ───────────────────────────

// siteAnalysisInFlight guards against two concurrent analyses of the same site
// (e.g. an impatient operator double-clicking Analyze): the second call is a
// no-op while the first is running.
var (
	siteAnalysisMu       sync.Mutex
	siteAnalysisInFlight = map[string]bool{}
)

// camSiteAnalysisBudget is the wall-clock ceiling for one background analysis run
// (many sequential snapshot captures + one vision call + an optional repair
// round). Generous on purpose — the individual captures and the backend calls
// have their own tighter timeouts; this only stops a wedged run from leaking a
// goroutine forever.
const camSiteAnalysisBudget = 20 * time.Minute

// startSiteAnalysis kicks off runSiteAnalysis in the background and returns
// immediately (started=false when an analysis for this site is already running).
// It flips analysis_status to "running" SYNCHRONOUSLY before returning so an
// immediate UI poll never races the goroutine and sees a stale status. This is
// the entrypoint the HTTP layer (WP11) calls for the Analyze / Re-analyze button.
func startSiteAnalysis(cfg config, siteID string) (started bool, err error) {
	siteID = strings.TrimSpace(siteID)
	if siteID == "" {
		return false, errors.New("site id is required")
	}

	siteAnalysisMu.Lock()
	if siteAnalysisInFlight[siteID] {
		siteAnalysisMu.Unlock()
		return false, nil
	}
	siteAnalysisInFlight[siteID] = true
	siteAnalysisMu.Unlock()

	// Mark running now so the very next poll observes it, independent of goroutine
	// scheduling. Best-effort: a DB hiccup here still lets the run proceed.
	if db, derr := openProxyDB(cfg); derr == nil {
		_ = setSiteAnalysisStatus(db, siteID, "running")
		_ = db.Close()
	}

	go func() {
		defer func() {
			siteAnalysisMu.Lock()
			delete(siteAnalysisInFlight, siteID)
			siteAnalysisMu.Unlock()
		}()
		ctx, cancel := context.WithTimeout(context.Background(), camSiteAnalysisBudget)
		defer cancel()
		if rerr := runSiteAnalysis(ctx, cfg, siteID); rerr != nil {
			camlog("error", "site_analyze", map[string]any{"site_id": siteID, "ok": false, "error": rerr.Error()})
		}
	}()
	return true, nil
}

// ─────────────────────────── the describe flow ───────────────────────────

// runSiteAnalysis runs (or re-runs) the setup/describe analysis for one site:
// snapshot every enabled camera, auto-disable the blank ones, ask the analysis
// alias to describe + group + question the rest, and persist all of it. It owns
// the analysis_status state machine (running → idle on success, error on
// failure) so it is safe to call directly (synchronously) OR via startSiteAnalysis.
func runSiteAnalysis(ctx context.Context, cfg config, siteID string) (err error) {
	siteID = strings.TrimSpace(siteID)
	if siteID == "" {
		return errors.New("site id is required")
	}
	start := time.Now()

	db, derr := openProxyDB(cfg)
	if derr != nil {
		return fmt.Errorf("open proxy db: %w", derr)
	}
	defer db.Close()

	// Any error return persists an error result + status so the UI stops polling
	// "running". Registered before the site load so a bad id also resolves cleanly.
	defer func() {
		if err != nil {
			_ = setSiteAnalysis(db, siteID, camAnalysisErrorJSON(err), "error", nowRFC3339())
			camlog("error", "site_analyze_done", map[string]any{
				"site_id": siteID, "ok": false, "error": err.Error(),
				"latency_ms": time.Since(start).Milliseconds(),
			})
		}
	}()

	site, gerr := getCameraSite(db, siteID)
	if gerr != nil {
		return fmt.Errorf("load site %s: %w", siteID, gerr)
	}
	_ = setSiteAnalysisStatus(db, siteID, "running")

	alias := firstNonEmpty(strings.TrimSpace(site.AnalysisAlias), strings.TrimSpace(cfg.CameraAnalysisAlias))
	modelConfigMu.RLock()
	backend := forwardForAlias(cfg, alias)
	modelConfigMu.RUnlock()

	cams, cerr := listCamerasBySite(db, siteID)
	if cerr != nil {
		return fmt.Errorf("load cameras: %w", cerr)
	}
	dvrs, verr := listCameraDVRs(db, cfg, siteID)
	if verr != nil {
		return fmt.Errorf("load dvrs: %w", verr)
	}
	dvrByID := make(map[string]CamDVR, len(dvrs))
	for _, d := range dvrs {
		dvrByID[d.ID] = d
	}

	camlog("info", "site_analyze_start", map[string]any{
		"site_id": siteID, "alias": alias, "backend": backend, "cameras": len(cams), "dvrs": len(dvrs),
	})

	// Scratch dir for the snapshots we hand to the AI. Each file is named after its
	// camera id so analyzeWithAlias derives a clean "camera <id>" label the model
	// can cite. Served copies live separately in the media root and outlive this dir.
	workDir, werr := os.MkdirTemp("", "camdescribe-")
	if werr != nil {
		return fmt.Errorf("create scratch dir: %w", werr)
	}
	defer os.RemoveAll(workDir)

	var (
		imagePaths []string
		allowed    = map[string]bool{}
		shown      = make(map[string]*camera) // camera id -> in-memory camera we AI'd
		tokenByID  = make(map[string]string)  // camera id -> served snapshot token
		excluded   []excludedCamera
	)

	for i := range cams {
		c := cams[i]
		if !c.Enabled {
			continue // already disabled — excluded by definition, and never captured
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}

		dvr, ok := dvrByID[c.DVRID]
		if !ok || !dvr.Enabled {
			// The DVR is off/removed — not the camera's fault, so don't flip the
			// camera's own disabled_reason (that flag means "the DVR turned THIS
			// channel off"). Just leave it out of this pass.
			reason := "dvr-disabled"
			if !ok {
				reason = "dvr-missing"
			}
			excluded = append(excluded, excludedCamera{CameraID: c.ID, Name: c.Name, Channel: c.Channel, Reason: reason})
			camlog("warn", "site_analyze_skip", map[string]any{
				"site_id": siteID, "camera_id": c.ID, "dvr_id": c.DVRID, "reason": reason, "ok": false,
			})
			continue
		}

		destPath := filepath.Join(workDir, c.ID+".jpg")
		res, capErr := captureSnapshot(ctx, cfg, dvr, c.Channel, StreamSub, destPath)
		if capErr != nil {
			// No usable frame at all — treat as a soft "failed" disable (re-enabled
			// on a later successful capture). captureSnapshot already logged the
			// masked command + stderr; record the disable decision here.
			_ = setCameraEnabled(db, c.ID, false, "failed")
			excluded = append(excluded, excludedCamera{CameraID: c.ID, Name: c.Name, Channel: c.Channel, Reason: "failed"})
			camlog("warn", "site_analyze_disable", map[string]any{
				"site_id": siteID, "camera_id": c.ID, "reason": "failed", "ok": false, "error": capErr.Error(),
			})
			continue
		}

		if blank, reason := isBlankSnapshot(res.Path); blank {
			// The DVR has this channel turned off (black / no-signal) or the frame
			// was undecodable — auto-disable and exclude it from the AI.
			_ = setCameraEnabled(db, c.ID, false, reason)
			excluded = append(excluded, excludedCamera{CameraID: c.ID, Name: c.Name, Channel: c.Channel, Reason: reason})
			camlog("info", "site_analyze_disable", map[string]any{
				"site_id": siteID, "camera_id": c.ID, "reason": reason, "ok": true,
			})
			continue
		}

		// Good frame. Persist a served copy + link it now (so the thumbnail works
		// even if the AI step fails) and clear any stale soft-disable reason.
		capID, token, perr := persistSnapshotCapture(db, cfg, siteID, c.ID, res, StreamSub)
		if perr != nil {
			camlog("warn", "site_analyze_persist", map[string]any{
				"site_id": siteID, "camera_id": c.ID, "ok": false, "error": perr.Error(),
			})
		} else {
			_ = setCameraSnapshot(db, c.ID, capID)
			c.SnapshotCaptureID = capID
			tokenByID[c.ID] = token
		}
		if c.DisabledReason != "" {
			_ = setCameraEnabled(db, c.ID, true, "")
			c.DisabledReason = ""
		}

		imagePaths = append(imagePaths, res.Path)
		allowed[c.ID] = true
		cc := c
		shown[c.ID] = &cc
	}

	// Nothing analyzable (fresh site, or every camera disabled). That is a valid
	// resting state, not an error — record it and go idle.
	if len(imagePaths) == 0 {
		result := siteAnalysisResult{
			AnalyzedAt: nowRFC3339(), Alias: alias, Backend: backend,
			Excluded: excluded,
			Note:     "No active cameras were available to analyze (all disabled or unreachable).",
		}
		if serr := setSiteAnalysis(db, siteID, mustJSON(result), "idle", nowRFC3339()); serr != nil {
			return fmt.Errorf("persist empty analysis: %w", serr)
		}
		camlog("warn", "site_analyze_done", map[string]any{
			"site_id": siteID, "ok": true, "cameras": 0, "excluded": len(excluded),
			"latency_ms": time.Since(start).Milliseconds(),
		})
		return nil
	}

	// Fold answered clarifications from earlier passes into the prompt; the still
	// -open ones are superseded once THIS analysis succeeds (dismissed below).
	priorQuestions, _ := listCameraQuestions(db, siteID, "")
	clarifications := camClarificationsBlock(priorQuestions)

	sysPrompt := siteSetupSystemPrompt(site, clarifications, len(imagePaths))
	userText := camSetupUserText(len(imagePaths))

	parsed, perr := camAnalyzeSetup(ctx, cfg, alias, sysPrompt, userText, imagePaths, allowed)
	if perr != nil {
		return perr
	}

	// ── persist results ──
	areaByID := camAreaAssignments(parsed.Areas)

	var camResults []setupCameraResult
	descByID := make(map[string]setupCamera, len(parsed.Cameras))
	for _, sc := range parsed.Cameras {
		descByID[sc.CameraID] = sc
	}
	// Persist a row for every camera we actually showed the model, in capture
	// order, whether or not the model returned a description for it.
	for _, path := range imagePaths {
		id := camImageLabel(path)
		cam := shown[id]
		if cam == nil {
			continue
		}
		sc := descByID[id]
		area := areaByID[id]
		cam.Area = area
		cam.AIDescription = strings.TrimSpace(sc.Description)
		cam.AILocation = strings.TrimSpace(sc.LocationGuess)
		if uerr := updateCamera(db, *cam); uerr != nil {
			camlog("warn", "site_analyze_update", map[string]any{
				"site_id": siteID, "camera_id": id, "ok": false, "error": uerr.Error(),
			})
		}
		camResults = append(camResults, setupCameraResult{
			CameraID: id, Name: cam.Name, Channel: cam.Channel,
			Description: cam.AIDescription, LocationGuess: cam.AILocation, Area: area,
			Confidence: sc.Confidence, LooksDisabled: sc.LooksDisabled, SnapshotToken: tokenByID[id],
		})
	}

	// Supersede stale open questions, then open the fresh ones. Doing this only on
	// a successful parse means a failed analysis never wipes the operator's inbox.
	for _, q := range priorQuestions {
		if q.Status == "open" {
			_ = setCameraQuestionStatus(db, q.ID, "dismissed")
		}
	}
	var questionIDs []string
	for _, q := range parsed.Questions {
		qIDs := q.CameraIDs
		if qIDs == nil {
			qIDs = []string{} // store "[]" not "null" for the empty case
		}
		qrow := camQuestion{
			SiteID:    siteID,
			CameraIDs: mustJSON(qIDs),
			Question:  q.Question,
			Status:    "open",
		}
		if qid, qerr := insertCameraQuestion(db, qrow); qerr == nil {
			questionIDs = append(questionIDs, qid)
		} else {
			camlog("warn", "site_analyze_question", map[string]any{
				"site_id": siteID, "ok": false, "error": qerr.Error(),
			})
		}
	}

	result := siteAnalysisResult{
		AnalyzedAt:  nowRFC3339(),
		Alias:       alias,
		Backend:     backend,
		Cameras:     camResults,
		Areas:       parsed.Areas,
		QuestionIDs: questionIDs,
		Excluded:    excluded,
	}
	if serr := setSiteAnalysis(db, siteID, mustJSON(result), "idle", nowRFC3339()); serr != nil {
		return fmt.Errorf("persist analysis: %w", serr)
	}

	camlog("info", "site_analyze_done", map[string]any{
		"site_id": siteID, "alias": alias, "backend": backend, "ok": true,
		"cameras": len(camResults), "areas": len(parsed.Areas), "questions": len(questionIDs),
		"excluded": len(excluded), "latency_ms": time.Since(start).Milliseconds(),
	})
	return nil
}

// camAnalyzeSetup makes the ONE analysis call and parses it, with a single repair
// round on a parse failure (mirrors the loop's "reply ONLY the JSON object"
// recovery in the plan). A backend error is returned as-is; an unrecoverable
// parse failure is returned so the run is marked error and NO descriptions/
// questions are written from garbage.
func camAnalyzeSetup(ctx context.Context, cfg config, alias, sysPrompt, userText string, imagePaths []string, allowed map[string]bool) (setupAnalysis, error) {
	raw, err := analyzeWithAlias(ctx, cfg, alias, sysPrompt, userText, imagePaths)
	if err != nil {
		return setupAnalysis{}, fmt.Errorf("analysis backend: %w", err)
	}
	parsed, perr := parseSetupAnalysis(raw, allowed)
	if perr == nil {
		return parsed, nil
	}
	camlog("warn", "site_analyze_parse", map[string]any{"ok": false, "attempt": 1, "error": perr.Error()})

	repair := userText + "\n\nYour previous reply could not be parsed. Reply with ONLY the single JSON object " +
		"described above — no explanation, no markdown code fences, nothing else."
	raw2, err2 := analyzeWithAlias(ctx, cfg, alias, sysPrompt, repair, imagePaths)
	if err2 != nil {
		return setupAnalysis{}, fmt.Errorf("analysis repair backend: %w", err2)
	}
	parsed, perr = parseSetupAnalysis(raw2, allowed)
	if perr != nil {
		camlog("warn", "site_analyze_parse", map[string]any{"ok": false, "attempt": 2, "error": perr.Error()})
		return setupAnalysis{}, fmt.Errorf("could not parse analysis JSON after repair: %w", perr)
	}
	return parsed, nil
}

// ─────────────────────────── parsing / normalization ───────────────────────────

// parseSetupAnalysis pulls the setup JSON out of the model's raw text (tolerating
// prose/fences via extractFirstJSONObject), decodes it with the tolerant scalar
// helpers, and normalizes it: camera ids the model made up (not in allowed) are
// dropped from every list, confidences are clamped to [0,1], and empty entries
// are removed. A non-nil allowed map enables the id filter.
func parseSetupAnalysis(raw string, allowed map[string]bool) (setupAnalysis, error) {
	obj, ok := extractFirstJSONObject(raw)
	if !ok {
		return setupAnalysis{}, errors.New("no JSON object found in model output")
	}
	var a setupAnalysis
	if err := json.Unmarshal(obj, &a); err != nil {
		return setupAnalysis{}, fmt.Errorf("decode setup JSON: %w", err)
	}

	cams := make([]setupCamera, 0, len(a.Cameras))
	for _, c := range a.Cameras {
		id := camNormalizeShownID(c.CameraID, allowed)
		if id == "" {
			continue
		}
		c.CameraID = id
		if c.Confidence < 0 {
			c.Confidence = 0
		}
		if c.Confidence > 1 {
			c.Confidence = 1
		}
		c.Description = strings.TrimSpace(c.Description)
		c.LocationGuess = strings.TrimSpace(c.LocationGuess)
		cams = append(cams, c)
	}
	a.Cameras = cams

	areas := make([]setupArea, 0, len(a.Areas))
	for _, ar := range a.Areas {
		ar.Area = strings.TrimSpace(ar.Area)
		ar.Rationale = strings.TrimSpace(ar.Rationale)
		ar.CameraIDs = camNormalizeShownIDs(ar.CameraIDs, allowed)
		if ar.Area == "" && len(ar.CameraIDs) == 0 {
			continue
		}
		areas = append(areas, ar)
	}
	a.Areas = areas

	qs := make([]setupQuestion, 0, len(a.Questions))
	for _, q := range a.Questions {
		q.Question = strings.TrimSpace(q.Question)
		if q.Question == "" {
			continue
		}
		q.CameraIDs = camNormalizeShownIDs(q.CameraIDs, allowed)
		qs = append(qs, q)
	}
	a.Questions = qs

	return a, nil
}

// camNormalizeShownID trims a model-supplied camera id, strips a leading "camera "
// the model may echo back from the image labels, and (when allowed is non-nil)
// returns it only if it is a real id the model was actually shown — otherwise ""
// so a hallucinated id is dropped.
func camNormalizeShownID(raw string, allowed map[string]bool) string {
	id := strings.TrimSpace(raw)
	if len(id) >= 7 && strings.EqualFold(id[:7], "camera ") {
		id = strings.TrimSpace(id[7:])
	}
	if id == "" {
		return ""
	}
	if allowed == nil {
		return id
	}
	if allowed[id] {
		return id
	}
	return ""
}

// camNormalizeShownIDs normalizes + de-duplicates a list of model-supplied ids,
// dropping blanks and (against a non-nil allowed set) hallucinated ones.
func camNormalizeShownIDs(ids []string, allowed map[string]bool) []string {
	if len(ids) == 0 {
		return nil
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(ids))
	for _, raw := range ids {
		id := camNormalizeShownID(raw, allowed)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}

// camAreaAssignments maps each camera id to the name of the FIRST area it appears
// in (a camera should belong to one group; ties resolve to the earliest listed).
func camAreaAssignments(areas []setupArea) map[string]string {
	out := map[string]string{}
	for _, ar := range areas {
		if ar.Area == "" {
			continue
		}
		for _, id := range ar.CameraIDs {
			if _, exists := out[id]; !exists {
				out[id] = ar.Area
			}
		}
	}
	return out
}

// ─────────────────────────── prompt building ───────────────────────────

// siteSetupSystemPrompt builds the describe-phase system prompt from the operator's
// site description plus any answered-question clarifications (for re-analyze). It
// pins the model to the exact JSON contract parseSetupAnalysis expects.
func siteSetupSystemPrompt(site camSite, clarifications string, camCount int) string {
	var b strings.Builder
	b.WriteString("You are a security-camera setup assistant for a physical premises (for example a clinic, shop, or office). ")
	b.WriteString("You are given ONE recent snapshot from each active camera. Using the operator's description of the place ")
	b.WriteString("plus the images, describe each camera and organize them.\n\n")

	if name := strings.TrimSpace(site.Name); name != "" {
		b.WriteString("Place name: ")
		b.WriteString(name)
		b.WriteString("\n")
	}
	desc := strings.TrimSpace(site.Description)
	if desc == "" {
		desc = "(the operator has not written a description yet — infer the setting from the images.)"
	}
	b.WriteString("Place description (operator's own words):\n")
	b.WriteString(desc)
	b.WriteString("\n")

	if c := strings.TrimSpace(clarifications); c != "" {
		b.WriteString("\n")
		b.WriteString(c)
		b.WriteString("\n")
	}

	b.WriteString("\nEach image is labeled \"camera <id>\". For EVERY camera you are shown:\n")
	b.WriteString("- description: what the camera sees — the fixed scene (which room/area), notable objects, doors/entrances, and typical activity.\n")
	b.WriteString("- location_guess: your best guess at where this camera is physically mounted within the place.\n")
	b.WriteString("- confidence: a number from 0 to 1 for how sure you are.\n")
	b.WriteString("- looks_disabled: true only if the frame is blank, covered, or shows a no-signal card.\n")
	b.WriteString("Then GROUP the cameras into logical areas (e.g. \"Entrance\", \"Waiting room\", \"Pharmacy\"). ")
	b.WriteString("Cameras that clearly cover the same physical area go in the same group; give a short rationale per group.\n")
	b.WriteString("If something is genuinely ambiguous and an operator's answer would materially improve a description or the grouping, ")
	b.WriteString("ask a concise clarifying question and reference the relevant camera id(s). Do not ask questions you can answer from the images.\n\n")

	b.WriteString("Reply with EXACTLY ONE JSON object and nothing else (no prose, no markdown fences), in this shape:\n")
	b.WriteString(`{"cameras":[{"camera_id":"<id>","description":"...","location_guess":"...","confidence":0.0,"looks_disabled":false}],`)
	b.WriteString(`"areas":[{"area":"...","camera_ids":["<id>"],"rationale":"..."}],`)
	b.WriteString(`"questions":[{"question":"...","camera_ids":["<id>"]}]}` + "\n")
	b.WriteString("Use ONLY the camera ids that were shown to you, exactly as labeled. If you have no questions, return \"questions\":[].")
	return b.String()
}

// camSetupUserText is the short user-turn instruction; analyzeWithAlias appends the
// ordered "N. camera <id>" attachment map for the inline backends automatically.
func camSetupUserText(camCount int) string {
	if camCount == 1 {
		return "Analyze the attached camera snapshot and return the JSON object described above."
	}
	return fmt.Sprintf("Analyze the %d attached camera snapshots and return the JSON object described above.", camCount)
}

// camClarificationsBlock renders answered questions from earlier passes as an
// "Operator clarifications: Q -> A" block for the re-analyze prompt. Returns "" if
// there are no answered questions (a first-time analysis).
func camClarificationsBlock(questions []camQuestion) string {
	var lines []string
	for _, q := range questions {
		if q.Status != "answered" {
			continue
		}
		qt := strings.TrimSpace(q.Question)
		at := strings.TrimSpace(q.Answer)
		if qt == "" || at == "" {
			continue
		}
		lines = append(lines, fmt.Sprintf("- Q: %s -> A: %s", qt, at))
	}
	if len(lines) == 0 {
		return ""
	}
	return "Operator clarifications (use these to refine your descriptions and grouping):\n" + strings.Join(lines, "\n")
}

// ─────────────────────────── snapshot persistence ───────────────────────────

// persistSnapshotCapture copies a freshly captured snapshot into the served media
// root under a fresh capability token and records a camera_captures row, so the
// UI can display the camera's current frame via the admin-gated /camera/media/
// endpoint (WP11). Returns the capture id and its token. The original file (in the
// caller's scratch dir, named after the camera id for the AI call) is left in place.
func persistSnapshotCapture(db *sql.DB, cfg config, siteID, cameraID string, src captureResult, q StreamQuality) (captureID, token string, err error) {
	root := cameraMediaRoot(cfg)
	if mkErr := os.MkdirAll(root, 0o700); mkErr != nil {
		return "", "", fmt.Errorf("create media root: %w", mkErr)
	}
	ct, ext := sniffImage(src.Path)
	if ext == "" {
		ext = filepath.Ext(src.Path)
	}
	if ct == "" {
		ct = firstNonEmpty(src.ContentType, "image/jpeg")
	}
	token = randToken(16)
	dest := filepath.Join(root, token+ext)
	// CCTV frames are sensitive; keep them owner-only (the proxy reads+serves them).
	if cpErr := copyFile(src.Path, dest, 0o600); cpErr != nil {
		return "", "", fmt.Errorf("copy snapshot to media root: %w", cpErr)
	}

	var expires string
	if ret := cfg.CameraMediaRetention; ret > 0 {
		expires = time.Now().Add(ret).Format(time.RFC3339)
	}
	capRow := camCapture{
		SiteID:      siteID,
		CameraID:    cameraID,
		Kind:        "snapshot",
		Quality:     q.String(),
		Token:       token,
		Path:        dest,
		ContentType: ct,
		Width:       src.Width,
		Height:      src.Height,
		Bytes:       src.Bytes,
		CreatedAt:   nowRFC3339(),
		ExpiresAt:   expires,
	}
	id, insErr := insertCameraCapture(db, capRow)
	if insErr != nil {
		_ = os.Remove(dest) // don't leak an orphaned file with no row
		return "", "", fmt.Errorf("insert capture row: %w", insErr)
	}
	return id, token, nil
}

// ─────────────────────────── small helpers ───────────────────────────

// mustJSON marshals v to a JSON string, falling back to "{}" on the (practically
// impossible) marshal error so a stored column is always valid JSON.
func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(b)
}

// camAnalysisErrorJSON renders the analysis_json stored on a failed run: just the
// timestamp and the error message for the UI to surface.
func camAnalysisErrorJSON(err error) string {
	return mustJSON(siteAnalysisResult{AnalyzedAt: nowRFC3339(), Error: err.Error()})
}
