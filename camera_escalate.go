package main

// camera_escalate.go (WP8) — the SERVER-orchestrated ReAct escalation loop, the
// second half of the orchestration brain (after camera_describe.go's setup phase).
//
// runEscalation(ctx,cfg,w,cams) is what a scheduled watch actually runs. It drives
// the plan's escalation ladder, SERVER-SIDE (portable across agy/claude/codex — the
// model never fetches its own media; the loop does, so a text-only backend still
// works):
//
//   Round 0 (ALWAYS): a cheap low-resolution MOSAIC of the SUB-stream snapshots of
//     every monitored camera → analyzeWithAlias → parseDecision. If the model is
//     unsuspicious (suspicion==none && rule_status==not_met) the run early-exits
//     here, before paying for a single full image or clip. When a watch has MANY
//     cameras, an optional ROSTER-FIRST pre-selection step (opt-in via
//     PROXY_CAMERA_ROSTER_FIRST, see camSelectMosaicCameras) asks the model to pick
//     the relevant subset from the roster's TEXT (reusing camSelectCameras from
//     camera_investigate.go) and builds the round-0 mosaic from only those cameras,
//     falling back to every active camera whenever selection is disabled, the watch
//     is small, or the selection call errors/returns nothing — so default behavior
//     is unchanged unless explicitly opted into.
//   Escalation: the loop honors the model's need_more request each round —
//     "full_images" → fresh MAIN-stream snapshots of the flagged cameras;
//     "clip" → a past-playback clip for the requested window (agy is handed the
//     .mp4 directly; claude/codex get it sampled into frames first).
//
// Bounds (a misbehaving model can never run the proxy dry): max rounds (watch
// override → cfg default 4, hard cap 6), max clips per run (cfg, default 3), and a
// wall-clock run budget (cfg, default 300s) enforced via a context deadline.
//
// Reliability: a round whose JSON can't be parsed triggers ONE repair round ("reply
// ONLY the JSON object"); if that still fails, OR any backend call errors, the run
// is recorded status="error" and NO actions are ever returned/fired.
//
// Every round is camlog'd (backend, latency, parse ok/fail, repair used, raw JSON)
// and every capture is persisted via the store (kind mosaic/snapshot/clip/frame,
// quality, capability token) with its token folded into transcript_json, so the run
// is fully replayable from the Diagnostics / run-history views. No credentials flow
// through here — all device URLs are built and masked inside the capture layer.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// ─────────────────────────────── result + transcript ───────────────────────────────

// runResult is the outcome of one escalation run, returned to the scheduler (WP9)
// which persists nothing itself for the run body (runEscalation already recorded
// the camera_watch_runs row) and only decides — gated on RuleStatus=="met" — whether
// to dispatch TriggeredActions and record the outcome against RunID.
type runResult struct {
	Status           string            // done | error
	Suspicion        string            // none | low | medium | high
	RuleStatus       string            // not_met | possibly_met | met
	Rounds           int               // number of analysis rounds performed (incl. the mosaic)
	Summary          string            // one-line human summary (the last round's round_summary)
	Transcript       string            // transcript_json (escTranscript) already persisted on the run row
	TriggeredActions []triggeredAction // non-empty ONLY when RuleStatus=="met"
	RunID            string            // the persisted camera_watch_runs id (for the dispatcher)
}

// escTranscript is the full, replayable record of a run, serialized into
// camera_watch_runs.transcript_json. It carries every round's decision plus the
// capability tokens of the media shown that round.
type escTranscript struct {
	Alias     string     `json:"alias"`
	Backend   string     `json:"backend"`
	Mode      string     `json:"mode"`
	MaxRounds int        `json:"max_rounds"`
	MaxClips  int        `json:"max_clips"`
	StartedAt string     `json:"started_at"`
	Rounds    []escRound `json:"rounds"`
}

// escRound is one round in the transcript: what media was shown, the raw model
// reply, the parsed decision (nil on parse failure), and timing/parse diagnostics.
type escRound struct {
	Round       int        `json:"round"` // 0 = mosaic, 1.. = escalation
	Kind        string     `json:"kind"`  // mosaic | full_images | clip
	Backend     string     `json:"backend,omitempty"`
	Media       []escMedia `json:"media,omitempty"`
	Suspicion   string     `json:"suspicion,omitempty"`
	RuleStatus  string     `json:"rule_status,omitempty"`
	NeedMore    string     `json:"need_more,omitempty"` // this decision's need_more.type
	Decision    *decision  `json:"decision,omitempty"`
	RawResponse string     `json:"raw_response,omitempty"`
	ParseOK     bool       `json:"parse_ok"`
	RepairUsed  bool       `json:"repair_used,omitempty"`
	LatencyMs   int64      `json:"latency_ms,omitempty"`
	Error       string     `json:"error,omitempty"`
}

// escMedia references one persisted capture the model was shown this round (by its
// served capability token), so run-history/diagnostics can re-render it.
type escMedia struct {
	Kind     string `json:"kind"`              // mosaic | snapshot | clip | frame
	Quality  string `json:"quality,omitempty"` // main | sub
	Token    string `json:"token,omitempty"`   // /camera/media/<token>
	CameraID string `json:"camera_id,omitempty"`
	From     string `json:"from,omitempty"` // clip window start (RFC3339)
	To       string `json:"to,omitempty"`   // clip window end (RFC3339)
}

// ─────────────────────────────── mode selection ───────────────────────────────

// errCamOrchClaudeToolsUnimplemented signals the (scaffolded) native-MCP tool-loop
// mode isn't built yet, so runEscalation falls back to the server loop.
var errCamOrchClaudeToolsUnimplemented = errors.New("claude_tools orchestration mode is not implemented yet")

// resolveCamOrchMode normalizes PROXY_CAMERA_ORCH_MODE; anything but the explicit
// "claude_tools" opt-in resolves to the default portable "server" loop.
func resolveCamOrchMode(cfg config) string {
	if strings.ToLower(strings.TrimSpace(cfg.CameraOrchMode)) == "claude_tools" {
		return "claude_tools"
	}
	return "server"
}

// resolveWatchMode normalizes a watch's escalation mode, mirroring
// camValidateWatchMode: "" and any unrecognized value default to "escalate";
// "mosaic_only" means the run stops after the round-0 mosaic decision and never
// pays for full-resolution snapshots or playback clips (the operator's cheap
// "Mosaic only (no escalation)" choice in the UI).
func resolveWatchMode(w watch) string {
	if strings.ToLower(strings.TrimSpace(w.Mode)) == "mosaic_only" {
		return "mosaic_only"
	}
	return "escalate"
}

// runEscalation is the entrypoint the scheduler calls. It selects the orchestration
// strategy (PROXY_CAMERA_ORCH_MODE) and runs it. The server-orchestrated loop is the
// default and the only mode that must fully work now; the optional claude_tools mode
// (claude's native MCP tool-loop) is scaffolded here and transparently falls back to
// the server loop until it is implemented in a later phase.
func runEscalation(ctx context.Context, cfg config, w watch, cams []camera) (runResult, error) {
	if resolveCamOrchMode(cfg) == "claude_tools" {
		res, err := runEscalationClaudeTools(ctx, cfg, w, cams)
		if err == nil {
			return res, nil
		}
		camlog("info", "escalate_mode", map[string]any{
			"watch_id": w.ID, "mode": "claude_tools", "fallback": "server", "note": err.Error(),
		})
	}
	return runEscalationServer(ctx, cfg, w, cams)
}

// runEscalationClaudeTools is a scaffold for the optional native-MCP tool-loop mode
// (a second gateway whose get_camera_mosaic/get_camera_image/get_camera_clip tools
// execute inside the proxy). Not built yet — returns a sentinel so runEscalation
// falls back to the portable server loop, keeping the mode selectable without a
// dead configuration path.
func runEscalationClaudeTools(ctx context.Context, cfg config, w watch, cams []camera) (runResult, error) {
	return runResult{}, errCamOrchClaudeToolsUnimplemented
}

// ─────────────────────────────── the server loop ───────────────────────────────

func runEscalationServer(ctx context.Context, cfg config, w watch, cams []camera) (runResult, error) {
	start := time.Now()

	alias := firstNonEmpty(strings.TrimSpace(w.AnalysisAlias), strings.TrimSpace(cfg.CameraAnalysisAlias))
	modelConfigMu.RLock()
	backend := forwardForAlias(cfg, alias)
	modelConfigMu.RUnlock()

	maxRounds := w.MaxRounds
	if maxRounds <= 0 {
		maxRounds = cfg.CameraMaxRounds
	}
	if maxRounds < 1 {
		maxRounds = 1
	}
	if maxRounds > 6 { // hard cap — never let a per-watch override run the loop unbounded
		maxRounds = 6
	}
	maxClips := cfg.CameraMaxClips
	if maxClips <= 0 {
		maxClips = 3
	}
	runBudget := cfg.CameraRunBudget
	if runBudget <= 0 {
		runBudget = 300 * time.Second
	}
	cctx, cancel := context.WithTimeout(ctx, runBudget)
	defer cancel()

	db, err := openProxyDB(cfg)
	if err != nil {
		return runResult{Status: "error", Suspicion: "none", RuleStatus: "not_met", Summary: "could not open the database"},
			fmt.Errorf("open proxy db: %w", err)
	}
	defer db.Close()

	// Load DVRs (with decrypted passwords) so captures can reach the devices, and
	// build the active-camera set: enabled cameras whose DVR is enabled.
	dvrs, _ := listCameraDVRs(db, cfg, w.SiteID)
	dvrByID := make(map[string]CamDVR, len(dvrs))
	for _, d := range dvrs {
		dvrByID[d.ID] = d
	}
	var active []camera
	camByID := make(map[string]camera, len(cams))
	allowed := make(map[string]bool, len(cams))
	for _, c := range cams {
		if !c.Enabled {
			continue
		}
		if dv, ok := dvrByID[c.DVRID]; !ok || !dv.Enabled {
			continue
		}
		active = append(active, c)
		camByID[c.ID] = c
		allowed[c.ID] = true
	}

	transcript := escTranscript{
		Alias: alias, Backend: backend, Mode: "server",
		MaxRounds: maxRounds, MaxClips: maxClips, StartedAt: nowRFC3339(),
	}

	// Create the run row up-front (status "running") so captures link to it and the
	// run is durably recorded even if the process dies mid-loop.
	runID, rerr := insertCameraWatchRun(db, camWatchRun{
		WatchID: w.ID, SiteID: w.SiteID, Status: "running", StartedAt: nowRFC3339(),
	})
	if rerr != nil {
		return runResult{Status: "error", Suspicion: "none", RuleStatus: "not_met", Summary: "could not create the run record"},
			fmt.Errorf("create run: %w", rerr)
	}

	camlog("info", "escalate_start", map[string]any{
		"run_id": runID, "watch_id": w.ID, "site_id": w.SiteID,
		"alias": alias, "backend": backend, "cameras": len(active),
		"max_rounds": maxRounds, "max_clips": maxClips, "budget_ms": runBudget.Milliseconds(),
	})

	// finalize persists the terminal run state (+ transcript) and logs the summary.
	// fired_action is left empty here — the dispatcher (WP9/WP10) fills it after it
	// acts on the returned RuleStatus/TriggeredActions.
	finalize := func(res runResult, runErr error) (runResult, error) {
		res.RunID = runID
		res.Transcript = mustJSON(transcript)
		errText := ""
		if runErr != nil {
			errText = runErr.Error()
		} else if res.Status == "error" {
			errText = res.Summary
		}
		if ferr := finishCameraWatchRun(db, camWatchRun{
			ID: runID, WatchID: w.ID, SiteID: w.SiteID, FinishedAt: nowRFC3339(),
			Status: res.Status, Suspicion: res.Suspicion, RuleStatus: res.RuleStatus,
			Rounds: res.Rounds, Summary: res.Summary, TranscriptJSON: res.Transcript, Error: errText,
		}); ferr != nil {
			camlog("warn", "escalate_persist", map[string]any{"run_id": runID, "ok": false, "error": ferr.Error()})
		}
		camlog("info", "escalate_done", map[string]any{
			"run_id": runID, "watch_id": w.ID, "ok": res.Status != "error",
			"status": res.Status, "suspicion": res.Suspicion, "rule_status": res.RuleStatus,
			"rounds": res.Rounds, "actions": len(res.TriggeredActions),
			"latency_ms": time.Since(start).Milliseconds(),
		})
		return res, runErr
	}

	if len(active) == 0 {
		return finalize(runResult{Status: "error", Suspicion: "none", RuleStatus: "not_met",
			Summary: "no active cameras to monitor for this watch"}, nil)
	}

	scratch, serr := os.MkdirTemp("", "camwatch-")
	if serr != nil {
		return finalize(runResult{Status: "error", Suspicion: "none", RuleStatus: "not_met",
			Summary: "could not create a scratch directory"}, fmt.Errorf("scratch dir: %w", serr))
	}
	defer os.RemoveAll(scratch)

	sys := camMonitorSystemPrompt(w, active)

	// ── Round 0: the low-quality mosaic (ALWAYS) ──
	// Roster-first pre-selection (opt-in, WP3): when there are many watched cameras,
	// let the model pick the relevant subset from the roster text first, and build
	// the mosaic from just that subset. Always falls back to `active` (every
	// escalation round downstream, e.g. full_images/clip fallback, still considers
	// the full `active` set — only the round-0 mosaic composition is narrowed).
	// Operator-authored site knowledge (site description + per-DVR instructions)
	// is shown to the roster-first selector so its picks honor the layout rules.
	// Built once per run; "" (no knowledge / site load failure) degrades cleanly.
	siteCtx := ""
	if site, serr := getCameraSite(db, w.SiteID); serr == nil {
		siteCtx = camSiteContextBlock(site, dvrs)
	}
	mosaicCams := camSelectMosaicCameras(cctx, cfg, runID, alias, w, active, siteCtx)
	mosaicPath, legend, mosaicToken, merr := camBuildRoundMosaic(cctx, cfg, db, runID, w.SiteID, mosaicCams, dvrByID, scratch)
	if merr != nil {
		return finalize(runResult{Status: "error", Suspicion: "none", RuleStatus: "not_met", Rounds: 0,
			Summary: "could not build the camera mosaic: " + merr.Error()}, nil)
	}
	mosaicUser := camMosaicUserText(legend, w)
	r0 := escRound{Round: 0, Kind: "mosaic", Backend: backend, Media: []escMedia{{Kind: "mosaic", Quality: "sub", Token: mosaicToken}}}
	t0 := time.Now()
	d, raw, rep, aerr := camAnalyzeDecision(cctx, cfg, alias, sys, mosaicUser, []string{mosaicPath}, allowed)
	r0.LatencyMs = time.Since(t0).Milliseconds()
	r0.RawResponse, r0.RepairUsed, r0.ParseOK = raw, rep, aerr == nil
	if aerr != nil {
		r0.Error = aerr.Error()
		transcript.Rounds = append(transcript.Rounds, r0)
		camlogEscRound(runID, w.ID, r0, len(sys), len(mosaicUser))
		return finalize(runResult{Status: "error", Suspicion: "none", RuleStatus: "not_met",
			Rounds: len(transcript.Rounds), Summary: "analysis failed at the mosaic round: " + aerr.Error()}, nil)
	}
	r0.Decision, r0.Suspicion, r0.RuleStatus, r0.NeedMore = &d, d.Suspicion, d.RuleStatus, d.NeedMore.Type
	transcript.Rounds = append(transcript.Rounds, r0)
	camlogEscRound(runID, w.ID, r0, len(sys), len(mosaicUser))
	last := d

	// Early exit after the mosaic round when nothing looks off — the cheap-first
	// contract that keeps background monitoring from paying for evidence it doesn't need.
	if last.Suspicion == "none" && last.RuleStatus == "not_met" {
		camlog("info", "escalate_early_exit", map[string]any{"run_id": runID, "watch_id": w.ID})
		return finalize(camBuildResult("done", last, transcript), nil)
	}

	// A "mosaic_only" watch is enforced HERE: the operator picked the cheap
	// "Mosaic only (no escalation)" mode, so the round-0 mosaic decision is final —
	// the run never escalates to full-resolution snapshots or playback clips even
	// if the model is suspicious. Actions still fire when the mosaic round already
	// judged rule_status=="met" (camBuildResult gates on that).
	if resolveWatchMode(w) != "escalate" {
		camlog("info", "escalate_mosaic_only", map[string]any{
			"run_id": runID, "watch_id": w.ID,
			"suspicion": last.Suspicion, "rule_status": last.RuleStatus,
		})
		return finalize(camBuildResult("done", last, transcript), nil)
	}

	// ── Escalation rounds ──
	clipsUsed := 0
	for len(transcript.Rounds) < maxRounds {
		if cctx.Err() != nil {
			camlog("warn", "escalate_budget", map[string]any{"run_id": runID, "watch_id": w.ID, "rounds": len(transcript.Rounds)})
			break
		}
		if last.Final || last.NeedMore.Type == "none" {
			break
		}
		media, meta, kind, userText, n, stop := camGatherEvidence(cctx, cfg, db, runID, w, backend, last, active, camByID, dvrByID, allowed, scratch, maxClips-clipsUsed)
		clipsUsed += n
		if stop || len(media) == 0 {
			break
		}
		rd := escRound{Round: len(transcript.Rounds), Kind: kind, Backend: backend, Media: meta}
		ts := time.Now()
		d2, raw2, rep2, aerr2 := camAnalyzeDecision(cctx, cfg, alias, sys, userText, media, allowed)
		rd.LatencyMs = time.Since(ts).Milliseconds()
		rd.RawResponse, rd.RepairUsed, rd.ParseOK = raw2, rep2, aerr2 == nil
		if aerr2 != nil {
			rd.Error = aerr2.Error()
			transcript.Rounds = append(transcript.Rounds, rd)
			camlogEscRound(runID, w.ID, rd, len(sys), len(userText))
			// A parse/backend failure mid-ladder → error the run and fire nothing.
			return finalize(runResult{Status: "error", Suspicion: last.Suspicion, RuleStatus: "not_met",
				Rounds: len(transcript.Rounds), Summary: "analysis failed during escalation: " + aerr2.Error()}, nil)
		}
		rd.Decision, rd.Suspicion, rd.RuleStatus, rd.NeedMore = &d2, d2.Suspicion, d2.RuleStatus, d2.NeedMore.Type
		transcript.Rounds = append(transcript.Rounds, rd)
		camlogEscRound(runID, w.ID, rd, len(sys), len(userText))
		last = d2
		if last.Final {
			break
		}
	}

	return finalize(camBuildResult("done", last, transcript), nil)
}

// camBuildResult assembles the success runResult from the final decision. Actions
// are surfaced ONLY when rule_status=="met" — the loop (not the model) is the final
// gate on whether anything fires.
func camBuildResult(status string, last decision, tr escTranscript) runResult {
	res := runResult{
		Status:     status,
		Suspicion:  last.Suspicion,
		RuleStatus: last.RuleStatus,
		Rounds:     len(tr.Rounds),
		Summary:    camDecisionSummary(last),
	}
	if last.RuleStatus == "met" {
		res.TriggeredActions = last.TriggeredActions
	}
	return res
}

// ─────────────────────────────── analysis (one round + repair) ───────────────────────────────

// camAnalyzeDecision runs one analysis round through analyzeWithAlias and parses the
// result, doing exactly ONE repair round on a parse failure ("reply ONLY the JSON
// object"). It returns the parsed decision, the raw model text of the last attempt,
// whether the repair round was used, and an error (backend failure, or an
// unparseable reply even after repair) — on which the caller errors the run without
// firing any action.
func camAnalyzeDecision(ctx context.Context, cfg config, alias, sys, user string, media []string, allowed map[string]bool) (decision, string, bool, error) {
	raw, err := analyzeWithAlias(ctx, cfg, alias, sys, user, media)
	if err != nil {
		return decision{}, raw, false, err
	}
	d, perr := parseDecision(raw, allowed)
	if perr == nil {
		return d, raw, false, nil
	}
	camlog("warn", "escalate_parse", map[string]any{"ok": false, "attempt": 1, "error": perr.Error()})

	repair := user + "\n\nYour previous reply could not be parsed. Reply with ONLY the single JSON object " +
		"described above — no prose, no markdown fences, nothing else."
	raw2, err2 := analyzeWithAlias(ctx, cfg, alias, sys, repair, media)
	if err2 != nil {
		return decision{}, raw2, true, err2
	}
	d2, perr2 := parseDecision(raw2, allowed)
	if perr2 != nil {
		camlog("warn", "escalate_parse", map[string]any{"ok": false, "attempt": 2, "error": perr2.Error()})
		return decision{}, raw2, true, fmt.Errorf("could not parse decision JSON after repair: %w", perr2)
	}
	return d2, raw2, true, nil
}

// ─────────────────────────────── evidence gathering ───────────────────────────────

// camGatherEvidence fulfills the model's need_more request for one escalation round:
// captures the requested evidence, persists each artifact, and returns the local
// file paths to feed the next analysis plus the transcript media metadata. stop is
// true when the ladder should end this round (unknown/none need_more type, clip
// budget exhausted, or nothing could be captured). clipsCaptured is added to the
// caller's running clip tally even when stop is true.
func camGatherEvidence(ctx context.Context, cfg config, db *sql.DB, runID string, w watch, backend string, last decision, active []camera, camByID map[string]camera, dvrByID map[string]CamDVR, allowed map[string]bool, scratch string, remainingClips int) (media []string, meta []escMedia, kind, userText string, clipsCaptured int, stop bool) {
	switch last.NeedMore.Type {
	case "full_images":
		ids := camCapIDs(camPickCameras(last.NeedMore.CameraIDs, last.PerCamera, active, allowed), camMosaicMax(cfg))
		paths, m, cerr := camCaptureFullImages(ctx, cfg, db, runID, w.SiteID, ids, camByID, dvrByID, scratch)
		if cerr != nil || len(paths) == 0 {
			camlog("warn", "escalate_no_media", map[string]any{"run_id": runID, "watch_id": w.ID, "kind": "full_images", "ok": false, "error": camErrStr(cerr)})
			return nil, nil, "full_images", "", 0, true
		}
		return paths, m, "full_images", camFullImagesUserText(ids, camByID), 0, false
	case "clip":
		if remainingClips <= 0 {
			camlog("info", "escalate_clip_budget", map[string]any{"run_id": runID, "watch_id": w.ID})
			return nil, nil, "clip", "", 0, true
		}
		ids := camPickCameras(last.NeedMore.CameraIDs, last.PerCamera, active, allowed)
		from, to := camClipWindow(last.NeedMore.Clip, cfg)
		paths, m, n, cerr := camCaptureClips(ctx, cfg, db, runID, w.SiteID, backend, ids, from, to, camByID, dvrByID, scratch, remainingClips)
		if cerr != nil || len(paths) == 0 {
			camlog("info", "escalate_no_clip", map[string]any{"run_id": runID, "watch_id": w.ID, "ok": false, "error": camErrStr(cerr)})
			return nil, nil, "clip", "", n, true
		}
		return paths, m, "clip", camClipUserText(ids, camByID, from, to), n, false
	default:
		return nil, nil, "", "", 0, true
	}
}

// camBuildRoundMosaic captures one SUB-stream snapshot per active camera, tiles them
// into a single low-cost mosaic (buildMosaic), writes+persists it as a served
// capture, and returns the on-disk mosaic path, the index→camera legend, and the
// capture token. Individual snapshot failures are logged and skipped; an error is
// returned only when NOTHING could be captured (so the whole run errors cleanly).
func camBuildRoundMosaic(ctx context.Context, cfg config, db *sql.DB, runID, siteID string, active []camera, dvrByID map[string]CamDVR, scratch string) (mosaicPath string, legend []mosaicLegendEntry, token string, err error) {
	items := make([]mosaicItem, 0, len(active))
	for _, c := range active {
		if ctx.Err() != nil {
			return "", nil, "", ctx.Err()
		}
		dv, ok := dvrByID[c.DVRID]
		if !ok || !dv.Enabled {
			continue
		}
		dest := filepath.Join(scratch, c.ID+"_sub.jpg")
		res, cerr := captureSnapshot(ctx, cfg, dv, c.Channel, StreamSub, dest)
		if cerr != nil {
			camlog("warn", "escalate_mosaic_skip", map[string]any{"run_id": runID, "camera_id": c.ID, "ok": false, "error": cerr.Error()})
			continue
		}
		items = append(items, mosaicItem{Index: len(items) + 1, CameraID: c.ID, Name: camDisplayName(c), Path: res.Path})
	}
	if len(items) == 0 {
		return "", nil, "", errors.New("no camera snapshots could be captured for the mosaic")
	}
	m, merr := buildMosaic(cfg, items)
	if merr != nil {
		return "", nil, "", merr
	}
	mosaicPath = filepath.Join(scratch, "mosaic.png")
	if werr := os.WriteFile(mosaicPath, m.PNG, 0o600); werr != nil {
		return "", nil, "", fmt.Errorf("write mosaic png: %w", werr)
	}
	if tok, perr := camPersistCapture(db, cfg, runID, siteID, "", "mosaic", "sub", mosaicPath, "image/png", m.Width, m.Height, "", ""); perr == nil {
		token = tok
	} else {
		// A failed served-copy shouldn't abort the analysis — we still have the
		// on-disk mosaic to hand the model, just without a UI-replayable token.
		camlog("warn", "escalate_persist_mosaic", map[string]any{"run_id": runID, "ok": false, "error": perr.Error()})
	}
	return mosaicPath, m.Legend, token, nil
}

// camCaptureFullImages grabs a fresh MAIN-stream snapshot of each requested camera,
// persists each as a served snapshot capture, and returns the scratch paths (named
// after the camera id so analyzeWithAlias derives a clean "camera <id>" label) plus
// transcript metadata. An error is returned only when none could be captured.
func camCaptureFullImages(ctx context.Context, cfg config, db *sql.DB, runID, siteID string, ids []string, camByID map[string]camera, dvrByID map[string]CamDVR, scratch string) ([]string, []escMedia, error) {
	var (
		paths []string
		meta  []escMedia
	)
	for _, id := range ids {
		if ctx.Err() != nil {
			break
		}
		c, ok := camByID[id]
		if !ok {
			continue
		}
		dv, ok := dvrByID[c.DVRID]
		if !ok || !dv.Enabled {
			continue
		}
		dest := filepath.Join(scratch, id+".jpg")
		res, cerr := captureSnapshot(ctx, cfg, dv, c.Channel, StreamMain, dest)
		if cerr != nil {
			continue // captureSnapshot already logged the masked command + stderr
		}
		if token, perr := camPersistCapture(db, cfg, runID, siteID, id, "snapshot", "main", res.Path, res.ContentType, res.Width, res.Height, "", ""); perr == nil {
			meta = append(meta, escMedia{Kind: "snapshot", Quality: "main", Token: token, CameraID: id})
		} else {
			camlog("warn", "escalate_persist_snapshot", map[string]any{"run_id": runID, "camera_id": id, "ok": false, "error": perr.Error()})
			meta = append(meta, escMedia{Kind: "snapshot", Quality: "main", CameraID: id})
		}
		paths = append(paths, res.Path)
	}
	if len(paths) == 0 {
		return nil, nil, errors.New("no full-resolution snapshots could be captured")
	}
	return paths, meta, nil
}

// camCaptureClips pulls a past-playback clip for each requested camera (bounded by
// the remaining clip budget), persists each clip, then prepares it for the backend:
// agy is handed the .mp4 directly (it reads video off disk), while claude/codex get
// the clip sampled into a few JPEG frames (also persisted as frame captures). A
// camera with no footage in the window (errNoRecording) is silently skipped. Returns
// errNoRecording only when NOTHING usable was produced for any requested camera.
func camCaptureClips(ctx context.Context, cfg config, db *sql.DB, runID, siteID, backend string, ids []string, from, to time.Time, camByID map[string]camera, dvrByID map[string]CamDVR, scratch string, budget int) (paths []string, meta []escMedia, captured int, err error) {
	fromTS, toTS := from.Format(time.RFC3339), to.Format(time.RFC3339)
	for _, id := range ids {
		if captured >= budget || ctx.Err() != nil {
			break
		}
		c, ok := camByID[id]
		if !ok {
			continue
		}
		dv, ok := dvrByID[c.DVRID]
		if !ok || !dv.Enabled {
			continue
		}
		dest := filepath.Join(scratch, id+".mp4")
		res, cerr := capturePlaybackClip(ctx, cfg, dv, c.Channel, StreamMain, from, to, dest)
		if cerr != nil {
			// errNoRecording or a capture failure — capturePlaybackClip already logged
			// the masked command + stderr; skip this camera for the round.
			continue
		}
		captured++
		if token, perr := camPersistCapture(db, cfg, runID, siteID, id, "clip", "main", res.Path, "video/mp4", 0, 0, fromTS, toTS); perr == nil {
			meta = append(meta, escMedia{Kind: "clip", Quality: "main", Token: token, CameraID: id, From: fromTS, To: toTS})
		} else {
			camlog("warn", "escalate_persist_clip", map[string]any{"run_id": runID, "camera_id": id, "ok": false, "error": perr.Error()})
		}

		if backend == "agy" {
			paths = append(paths, res.Path) // agy reads the .mp4 itself
			continue
		}
		// claude/codex can't ingest video: sample frames and hand those over instead.
		framePaths, frameMeta, ferr := camSampleAndPersistFrames(ctx, cfg, db, runID, siteID, id, res.Path, scratch)
		if ferr != nil || len(framePaths) == 0 {
			// Sampling failed — fall back to handing analyzeWithAlias the clip, which
			// will attempt its own internal frame sampling.
			camlog("warn", "escalate_frames_fallback", map[string]any{"run_id": runID, "camera_id": id, "ok": false, "error": camErrStr(ferr)})
			paths = append(paths, res.Path)
			continue
		}
		paths = append(paths, framePaths...)
		meta = append(meta, frameMeta...)
	}
	if len(paths) == 0 {
		return nil, meta, captured, errNoRecording
	}
	return paths, meta, captured, nil
}

// camSampleAndPersistFrames samples a handful of frames from a captured clip (via
// sampleClipFrames, default 2s/8-frames/768px), renames each into the run scratch
// dir with a camera-id-tagged name so the analysis label stays meaningful, persists
// each as a served frame capture, and returns the frame paths + transcript metadata.
func camSampleAndPersistFrames(ctx context.Context, cfg config, db *sql.DB, runID, siteID, camID, clipPath, scratch string) ([]string, []escMedia, error) {
	frameDir := filepath.Join(scratch, camID+"_frames")
	frames, err := sampleClipFrames(ctx, cfg, clipPath, frameDir, 0, 0, 0)
	if err != nil {
		return nil, nil, err
	}
	var (
		paths []string
		meta  []escMedia
	)
	for i, f := range frames {
		named := filepath.Join(scratch, fmt.Sprintf("%s_f%02d.jpg", camID, i+1))
		if rerr := os.Rename(f, named); rerr != nil {
			named = f // rename can fail across some filesystems; keep the original path
		}
		if token, perr := camPersistCapture(db, cfg, runID, siteID, camID, "frame", "main", named, "image/jpeg", 0, 0, "", ""); perr == nil {
			meta = append(meta, escMedia{Kind: "frame", Quality: "main", Token: token, CameraID: camID})
		} else {
			camlog("warn", "escalate_persist_frame", map[string]any{"run_id": runID, "camera_id": camID, "ok": false, "error": perr.Error()})
			meta = append(meta, escMedia{Kind: "frame", Quality: "main", CameraID: camID})
		}
		paths = append(paths, named)
	}
	if len(paths) == 0 {
		return nil, nil, errors.New("no frames sampled from clip")
	}
	return paths, meta, nil
}

// ─────────────────────────────── capture persistence ───────────────────────────────

// camPersistCapture copies a freshly captured artifact into the served media root
// under a fresh capability token, records a camera_captures row (linked to the run),
// and returns the token used to serve it via the admin-gated /camera/media/ endpoint.
// It handles both images (content type sniffed) and clips (video/mp4, kept by
// extension). Mirrors camera_describe.go's persistSnapshotCapture but is general over
// kind/quality/from/to. Expiry is the standard cfg.CameraMediaRetention window —
// callers that need a pinned (blank = never reaped) or custom deadline use
// camPersistCaptureExpires directly.
func camPersistCapture(db *sql.DB, cfg config, runID, siteID, cameraID, kind, quality, srcPath, ctHint string, width, height int, fromTS, toTS string) (string, error) {
	var expires string
	if ret := cfg.CameraMediaRetention; ret > 0 {
		expires = time.Now().Add(ret).Format(time.RFC3339)
	}
	return camPersistCaptureExpires(db, cfg, runID, siteID, cameraID, kind, quality, srcPath, ctHint, width, height, fromTS, toTS, expires)
}

// camPersistCaptureExpires is camPersistCapture with an EXPLICIT expires_at:
// "" = PINNED (never reaped — every reaper path filters on a non-blank expires_at);
// otherwise an RFC3339 deadline (avatar candidates use now+CameraAvatarCandidateRetention).
// Capture kinds in use: "avatar_ref" (pinned), "avatar_candidate" (retention
// deadline), "annotated" (normal retention via camPersistCapture).
func camPersistCaptureExpires(db *sql.DB, cfg config, runID, siteID, cameraID, kind, quality, srcPath, ctHint string, width, height int, fromTS, toTS, expiresAt string) (string, error) {
	root := cameraMediaRoot(cfg)
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", fmt.Errorf("create media root: %w", err)
	}
	ct, ext := sniffImage(srcPath)
	if ct == "" {
		// Not a recognizable image (e.g. an .mp4 clip) — use the caller's hint and
		// keep the source extension.
		ct = firstNonEmpty(ctHint, "application/octet-stream")
		ext = filepath.Ext(srcPath)
	}
	token := randToken(16)
	dest := filepath.Join(root, token+ext)
	// CCTV media is sensitive; keep it owner-only (the proxy reads + serves it).
	if err := copyFile(srcPath, dest, 0o600); err != nil {
		return "", fmt.Errorf("copy capture to media root: %w", err)
	}
	var size int64
	if fi, e := os.Stat(dest); e == nil {
		size = fi.Size()
	}
	row := camCapture{
		SiteID: siteID, CameraID: cameraID, WatchRunID: runID,
		Kind: kind, Quality: quality, Token: token, Path: dest,
		ContentType: ct, Width: width, Height: height, Bytes: size,
		FromTS: fromTS, ToTS: toTS, CreatedAt: nowRFC3339(), ExpiresAt: expiresAt,
	}
	if _, err := insertCameraCapture(db, row); err != nil {
		_ = os.Remove(dest) // don't leak an orphaned file with no row
		return "", fmt.Errorf("insert capture row: %w", err)
	}
	return token, nil
}

// ─────────────────────────────── camera selection helpers ───────────────────────────────

// camRosterFirstEnabled reports whether the optional roster-first mosaic
// pre-selection step (camSelectMosaicCameras) is turned on. Off by default so
// existing mosaic_only/escalate behavior is unchanged unless an operator opts in
// (PROXY_CAMERA_ROSTER_FIRST=1/true/yes/on). This package has no config-struct
// field for it — WP3 is scoped to camera_escalate.go only — so the env var is read
// directly here, mirroring camera_log.go's PROXY_CAMERA_LOG_LEVEL.
func camRosterFirstEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("PROXY_CAMERA_ROSTER_FIRST"))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

// camRosterFirstMinCams is the minimum number of active cameras a watch needs
// before roster-first pre-selection is worth its extra text-only model call —
// below this the full mosaic is already cheap. Overridable via
// PROXY_CAMERA_ROSTER_FIRST_MIN_CAMS; defaults to 6.
func camRosterFirstMinCams() int {
	if v := strings.TrimSpace(os.Getenv("PROXY_CAMERA_ROSTER_FIRST_MIN_CAMS")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return 6
}

// camSelectMosaicCameras narrows the round-0 mosaic's camera set via the
// roster-first selector (camSelectCameras, camera_investigate.go) when the feature
// is enabled and the watch has "many" active cameras (camRosterFirstMinCams). It
// asks the model to pick the subset relevant to the watch's instruction from the
// roster TEXT alone (no images — cheap), then filters `active` down to that subset.
// siteContext is the operator-authored camSiteContextBlock ("" = none) shown to
// the selector so its picks honor the operator's layout knowledge.
// It ALWAYS falls back to returning `active` unchanged — feature disabled, watch
// too small, the selection call errors, or the model selects nothing — so this is
// purely additive and every non-opted-in watch behaves exactly as before. Every
// attempt is camlog'd (op=escalate_roster_select) with the outcome and latency.
func camSelectMosaicCameras(ctx context.Context, cfg config, runID, alias string, w watch, active []camera, siteContext string) []camera {
	if !camRosterFirstEnabled() || len(active) <= camRosterFirstMinCams() {
		return active
	}
	start := time.Now()
	ids, err := camSelectCameras(ctx, cfg, alias, w.Instruction, siteContext, active)
	fields := map[string]any{
		"run_id": runID, "watch_id": w.ID, "cameras_total": len(active),
		"latency_ms": time.Since(start).Milliseconds(),
	}
	if err != nil {
		fields["ok"] = false
		fields["error"] = err.Error()
		camlog("warn", "escalate_roster_select", fields)
		return active
	}
	subset := camFilterCamerasByID(active, ids)
	fields["ok"] = true
	fields["selected"] = len(subset)
	if len(subset) == 0 {
		camlog("info", "escalate_roster_select", fields)
		return active
	}
	fields["camera_ids"] = ids
	camlog("info", "escalate_roster_select", fields)
	return subset
}

// camFilterCamerasByID returns the cameras in cams whose id appears in ids,
// preserving ids' order (camSelectCameras already de-duplicates and validates
// roster membership, so no further checks are needed here).
func camFilterCamerasByID(cams []camera, ids []string) []camera {
	byID := make(map[string]camera, len(cams))
	for _, c := range cams {
		byID[c.ID] = c
	}
	out := make([]camera, 0, len(ids))
	for _, id := range ids {
		if c, ok := byID[id]; ok {
			out = append(out, c)
		}
	}
	return out
}

// camPickCameras resolves which cameras an escalation round should fetch: the ids
// the model flagged (already allowed-filtered by parseDecision), else the cameras it
// marked notable this round, else — as a last resort — every active camera.
func camPickCameras(flagged []string, per []perCameraObs, active []camera, allowed map[string]bool) []string {
	if len(flagged) > 0 {
		return flagged
	}
	seen := map[string]bool{}
	var out []string
	for _, p := range per {
		id := strings.TrimSpace(p.CameraID)
		if p.Notable && allowed[id] && !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	if len(out) > 0 {
		return out
	}
	for _, c := range active {
		if !seen[c.ID] {
			seen[c.ID] = true
			out = append(out, c.ID)
		}
	}
	return out
}

// camCapIDs caps a camera-id list at max (0 = uncapped) so a "full_images" request
// with no specific ids can't fan out to an unbounded number of MAIN-stream pulls.
func camCapIDs(ids []string, max int) []string {
	if max > 0 && len(ids) > max {
		return ids[:max]
	}
	return ids
}

// camMosaicMax resolves the per-mosaic camera cap (also reused as the full_images
// fan-out cap), defaulting to 16.
func camMosaicMax(cfg config) int {
	if n := cfg.CameraMosaicMaxCams; n > 0 {
		return n
	}
	return 16
}

// camDisplayName is a camera's human label for the mosaic legend (name → area → id).
func camDisplayName(c camera) string {
	if n := strings.TrimSpace(c.Name); n != "" {
		return n
	}
	if a := strings.TrimSpace(c.Area); a != "" {
		return a
	}
	return c.ID
}

// camIDsWithNames renders a comma-separated "id (Name)" list for a prompt line.
func camIDsWithNames(ids []string, camByID map[string]camera) string {
	parts := make([]string, 0, len(ids))
	for _, id := range ids {
		s := id
		if c, ok := camByID[id]; ok {
			if n := strings.TrimSpace(c.Name); n != "" {
				s = id + " (" + n + ")"
			}
		}
		parts = append(parts, s)
	}
	return strings.Join(parts, ", ")
}

// camDecisionSummary is the one-line run summary: the model's round_summary if it
// gave one, otherwise a compact suspicion/rule_status line.
func camDecisionSummary(d decision) string {
	if s := strings.TrimSpace(d.RoundSummary); s != "" {
		return s
	}
	return fmt.Sprintf("suspicion=%s rule_status=%s", d.Suspicion, d.RuleStatus)
}

// camClipWindow converts a model clip request (offsets in seconds relative to now,
// negative = past) into a concrete [from,to] window, defensively: a nil/degenerate
// window falls back to the last 60s, a future "to" is clamped to now, and the span
// is capped at cfg.CameraClipMaxSeconds so a model can't ask for an hour of footage.
func camClipWindow(cw *clipWindow, cfg config) (from, to time.Time) {
	now := time.Now()
	if cw == nil {
		return now.Add(-60 * time.Second), now
	}
	to = now.Add(time.Duration(cw.ToOffsetSec) * time.Second)
	if to.After(now) {
		to = now
	}
	from = now.Add(time.Duration(cw.FromOffsetSec) * time.Second)
	if !from.Before(to) {
		from = to.Add(-60 * time.Second)
	}
	maxSec := cfg.CameraClipMaxSeconds
	if maxSec <= 0 {
		maxSec = 300
	}
	if span := to.Sub(from); span > time.Duration(maxSec)*time.Second {
		from = to.Add(-time.Duration(maxSec) * time.Second)
	}
	return from, to
}

// ─────────────────────────────── observability ───────────────────────────────

// camlogEscRound emits the per-round diagnostics line (backend, latency, parse
// ok/fail, repair used, response size, need_more, and a truncated raw reply — the
// full raw lives in transcript_json). Levels escalate to warn on a parse miss and
// error when the round itself errored.
func camlogEscRound(runID, watchID string, r escRound, sysBytes, userBytes int) {
	level := "info"
	if !r.ParseOK {
		level = "warn"
	}
	fields := map[string]any{
		"run_id": runID, "watch_id": watchID, "round": r.Round, "kind": r.Kind,
		"backend": r.Backend, "parse_ok": r.ParseOK, "repair_used": r.RepairUsed,
		"latency_ms": r.LatencyMs, "sys_bytes": sysBytes, "user_bytes": userBytes,
		"resp_bytes": len(r.RawResponse), "media": len(r.Media),
	}
	if r.Suspicion != "" {
		fields["suspicion"] = r.Suspicion
	}
	if r.RuleStatus != "" {
		fields["rule_status"] = r.RuleStatus
	}
	if r.NeedMore != "" {
		fields["need_more"] = r.NeedMore
	}
	if r.RawResponse != "" {
		fields["raw"] = truncateString(r.RawResponse, 1200)
	}
	if r.Error != "" {
		level = "error"
		fields["ok"] = false
		fields["error"] = r.Error
	}
	camlog(level, "escalate_round", fields)
}

// ─────────────────────────────── prompts ───────────────────────────────

// camMonitorSystemPrompt builds the loop's system prompt: the operator's monitoring
// rule, the roster of monitored cameras (ids the model MUST use), the escalation
// protocol, and the strict decision-JSON contract parseDecision expects.
func camMonitorSystemPrompt(w watch, active []camera) string {
	var b strings.Builder
	b.WriteString("You are an autonomous security-camera monitoring agent for a physical premises. ")
	b.WriteString("You evaluate a specific monitoring rule against camera imagery and decide, step by step, whether the ")
	b.WriteString("rule is met — escalating from a cheap low-resolution overview to full images and then recorded clips ")
	b.WriteString("ONLY when you need more evidence.\n\n")

	b.WriteString("MONITORING RULE (evaluate strictly against this):\n")
	instr := strings.TrimSpace(w.Instruction)
	if instr == "" {
		instr = "Report anything unusual or noteworthy in the monitored cameras."
	}
	b.WriteString(instr)
	b.WriteString("\n\n")

	b.WriteString("MONITORED CAMERAS (use these EXACT camera ids in every camera_id field):\n")
	for _, c := range active {
		b.WriteString("- ")
		b.WriteString(c.ID)
		if n := strings.TrimSpace(c.Name); n != "" {
			b.WriteString(" — ")
			b.WriteString(n)
		}
		if a := strings.TrimSpace(c.Area); a != "" {
			b.WriteString(" [")
			b.WriteString(a)
			b.WriteString("]")
		}
		if d := strings.TrimSpace(c.AIDescription); d != "" {
			b.WriteString(": ")
			b.WriteString(d)
		}
		b.WriteString("\n")
	}
	b.WriteString("\n")

	b.WriteString("ESCALATION PROTOCOL:\n")
	b.WriteString("- Round 1 shows a single low-resolution MOSAIC (the tile legend tells you exactly which monitored ")
	b.WriteString("cameras it includes — this may be all of them or a relevant subset); later rounds show what you request.\n")
	b.WriteString("- need_more.type \"full_images\": request full-resolution CURRENT snapshots for specific camera_ids.\n")
	b.WriteString("- need_more.type \"clip\": request recorded footage; set clip.from_offset_sec and clip.to_offset_sec as ")
	b.WriteString("seconds relative to NOW (negative = the past; e.g. from_offset_sec -180, to_offset_sec 0 = the last 3 minutes).\n")
	b.WriteString("- need_more.type \"none\": you have enough evidence — set final true.\n")
	b.WriteString("- Set rule_status \"met\" ONLY when you are confident the rule is satisfied; triggered_actions are ")
	b.WriteString("dispatched ONLY when rule_status is \"met\".\n")
	b.WriteString("- Be economical: do not escalate unless the extra evidence could change your decision.\n\n")

	b.WriteString("OUTPUT: reply with EXACTLY ONE JSON object and nothing else (no prose, no markdown fences):\n")
	b.WriteString(`{"round_summary":"...","per_camera":[{"camera_id":"<id>","observation":"...","notable":false}],`)
	b.WriteString(`"suspicion":"none|low|medium|high","rule_status":"not_met|possibly_met|met",`)
	b.WriteString(`"need_more":{"type":"none|full_images|clip","camera_ids":["<id>"],"clip":{"from_offset_sec":0,"to_offset_sec":0}},`)
	b.WriteString(`"triggered_actions":[{"action":"...","severity":"info|warning|critical","reason":"...","camera_ids":["<id>"]}],`)
	b.WriteString(`"final":false,"confidence":0.0}`)
	b.WriteString("\nUse ONLY the camera ids listed above. If you need nothing more, set need_more.type \"none\". ")
	b.WriteString("If you have no actions, use an empty triggered_actions array.")
	return b.String()
}

// camMosaicUserText is the round-0 user turn: the tile legend (number → real camera
// id) so the model can cite cameras unambiguously, plus how to escalate.
func camMosaicUserText(legend []mosaicLegendEntry, w watch) string {
	var b strings.Builder
	b.WriteString("You are shown ONE low-resolution mosaic combining the current view of the monitored cameras. ")
	b.WriteString("Each tile is stamped with a number in its top-left corner. Tile legend (number => camera id):\n")
	for _, e := range legend {
		fmt.Fprintf(&b, "  %d => %s", e.Index, e.CameraID)
		if n := strings.TrimSpace(e.Name); n != "" {
			b.WriteString(" (")
			b.WriteString(n)
			b.WriteString(")")
		}
		b.WriteString("\n")
	}
	b.WriteString("\nEvaluate the monitoring rule against what you can see. The mosaic is deliberately low quality: if a ")
	b.WriteString("tile might satisfy or approach the rule, request a clearer view via need_more (type \"full_images\" with ")
	b.WriteString("the relevant camera_id(s)), or request recorded footage (type \"clip\" with camera_id(s) and a clip window) ")
	b.WriteString("if you need to see what happened over time. Use the REAL camera id from the legend in every camera_id field. ")
	b.WriteString("Reply with ONLY the JSON object.")
	return b.String()
}

// camFullImagesUserText is the user turn accompanying full-resolution snapshots.
func camFullImagesUserText(ids []string, camByID map[string]camera) string {
	var b strings.Builder
	b.WriteString("You are now shown full-resolution CURRENT snapshots for the camera(s) you asked to inspect: ")
	b.WriteString(camIDsWithNames(ids, camByID))
	b.WriteString(".\nRe-evaluate the monitoring rule with this clearer view. If you still need to see motion or what ")
	b.WriteString("happened over a time window, escalate with need_more type \"clip\". If you can decide now, set ")
	b.WriteString("need_more.type to \"none\" and final to true. When the rule is satisfied set rule_status to \"met\" ")
	b.WriteString("and list the actions to trigger. Reply with ONLY the JSON object.")
	return b.String()
}

// camClipUserText is the user turn accompanying a recorded clip (or its frames).
func camClipUserText(ids []string, camByID map[string]camera, from, to time.Time) string {
	var b strings.Builder
	b.WriteString("You are shown recorded footage (a video clip or frames sampled from it) from camera(s) ")
	b.WriteString(camIDsWithNames(ids, camByID))
	fmt.Fprintf(&b, " covering the window %s to %s. ", from.Format("15:04:05"), to.Format("15:04:05"))
	b.WriteString("Review what happens over this window and re-evaluate the rule. You may request one more clip or more ")
	b.WriteString("images only if strictly necessary, otherwise set need_more.type to \"none\" and final to true. When the ")
	b.WriteString("rule is satisfied set rule_status to \"met\" and list the actions. Reply with ONLY the JSON object.")
	return b.String()
}
