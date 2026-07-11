package main

// camera_investigate_subagent.go (WS3) — the `delegate` tool: a lead investigation
// fans a long/multi-camera task out to several PARALLEL sub-investigators, each a
// restricted copy of the same agentic loop scoped to ONE subtask (its own question,
// cameras, and time window). Children are real rows in camera_investigations with
// parent_id = the lead run's id, but they run IN-PROCESS inside the parent's single
// delegate turn (a WaitGroup) — NEVER through the background queue. That is deliberate:
// the queue's slot semaphore (camInvestigateSem) is held by the parent run for the
// whole turn, so routing a child through the queue would deadlock when the pool is
// saturated. Children instead share their own global ceiling (camSubagentSem).
//
// WHY IN-PROCESS IS SAFE:
//   - Each child's context derives from the parent's budget context, so the parent
//     wall-clock deadline always wins and cancels every child with it.
//   - A child never touches camInvestigateSem or the queue, so no queue deadlock.
//   - Crash-safety: children are excluded from the worker requeue/stale-reaper, and an
//     orphaned running child is terminalized to "exhausted" at startup (camera_store.go);
//     a re-run parent simply re-delegates fresh children.
//   - Media accounting is resume-safe: the delegate tool row's Fetches = Σ child fetches,
//     which camCountInvestigateProgress reconstructs from the transcript like any tool.
//
// EVIDENCE CHAIN: each child reports a verdict + its kept evidence media_urls; the
// delegate tool aggregates those into its OWN tool-row media_json, so the lead model
// can cite a child's frame verbatim in its final answer through the exact same
// camCollectMintedMedia / camFilterEvidence path every other tool uses.

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// ─────────────────────────────── config resolvers ───────────────────────────────

// camSubagentMax resolves the max subtasks a single delegate call may fan out,
// clamped to 0..8. Unlike the other resolvers a value of 0 is MEANINGFUL — it
// disables delegation entirely (the prompt drops the tool, the dispatcher refuses) —
// so 0 is preserved rather than re-defaulted; only a negative misconfig or an
// over-cap is clamped. The default 4 lives in the env layer (main.go).
func camSubagentMax(cfg config) int {
	n := cfg.CameraSubagentMax
	if n < 0 {
		n = 0 // a negative cap can't mean "unlimited" — treat as disabled
	}
	if n > 8 {
		n = 8
	}
	return n
}

// camSubagentConcurrency resolves the global ceiling on concurrently-running
// sub-investigators, floored at 1 (0/negative → the default 3).
func camSubagentConcurrency(cfg config) int {
	n := cfg.CameraSubagentConcurrency
	if n < 1 {
		n = 3
	}
	return n
}

// camSubagentMaxTurns resolves a child's per-run turn cap, clamped to 1..12 so a
// misconfigured override can neither stall a child (0 turns) nor let one run as long
// as the lead.
func camSubagentMaxTurns(cfg config) int {
	n := cfg.CameraSubagentMaxTurns
	if n <= 0 {
		n = 6
	}
	if n > 12 {
		n = 12
	}
	return n
}

// camSubagentMaxMedia resolves the per-child device-fetch ceiling (0/negative →
// default 10). The actual per-child budget is the SMALLER of this and the parent's
// remaining pool split across the children (camSubagentPerChildMedia).
func camSubagentMaxMedia(cfg config) int {
	n := cfg.CameraSubagentMaxMedia
	if n <= 0 {
		n = 10
	}
	return n
}

// camSubagentBudget resolves a child's wall-clock cap (0/negative → default 600s).
// The child context is ALWAYS derived from the parent's budget context, so this only
// ever shortens a child relative to the lead — it can never outlive it.
func camSubagentBudget(cfg config) time.Duration {
	d := cfg.CameraSubagentBudget
	if d <= 0 {
		d = 600 * time.Second
	}
	return d
}

// camSubagentPerChildMedia splits the parent's REMAINING media pool across the
// children with a floor of 4 each, capped by the per-child ceiling:
// min(subagentMaxMedia, max(4, mediaLeft/children)). The floor guarantees even a
// heavily-spent parent still lets each child do useful work; the children's fetches
// count back against the parent pool (delegate tool-row Fetches), so the parent's own
// next-turn budget check absorbs any overshoot.
func camSubagentPerChildMedia(subagentMaxMedia, mediaLeft, children int) int {
	if children <= 0 {
		return 0
	}
	share := mediaLeft / children
	if share < 4 {
		share = 4
	}
	if share < subagentMaxMedia {
		return share
	}
	return subagentMaxMedia
}

// ─────────────────────────────── concurrency gate ───────────────────────────────

// camSubagentSem is the process-wide ceiling on concurrently-running
// sub-investigators, sized once by startCameraInvestigationWorker. It stays nil when
// the background worker is disabled (inline-drain deploys), which camSubagentAcquire
// treats as "no limit" so delegation still works synchronously.
var camSubagentSem chan struct{}

// camSubagentAcquire takes a sub-investigator slot, honoring cancellation while it
// waits so a parent whose budget dies mid-wait bails instead of blocking. A nil sem
// (worker off) grants immediately. The returned release is always safe to call.
func camSubagentAcquire(ctx context.Context) (release func(), ok bool) {
	if camSubagentSem == nil {
		return func() {}, true
	}
	select {
	case camSubagentSem <- struct{}{}:
		return func() { <-camSubagentSem }, true
	case <-ctx.Done():
		return func() {}, false
	}
}

// ─────────────────────────────── restricted toolset ───────────────────────────────

// camSubagentAllowedTools is the HARD ceiling of tools a sub-investigator may run.
// Deliberately excluded: delegate (no recursion — a child can't fan out further),
// past_clip / evidence_export (operator-facing deliverables the LEAD packages),
// playbook / call_api (lead-level orchestration), mosaic (whole-site overview), and
// there is no ask_operator action at all. A subtask's tools_allowed can only NARROW
// this set, never widen it.
var camSubagentAllowedTools = map[string]bool{
	"roster":        true,
	"snapshot":      true,
	"past_frames":   true,
	"contact_sheet": true,
	"motion_search": true,
	"activity_log":  true,
	"avatars":       true,
	"avatar_info":   true,
	"avatar_check":  true,
	"avatar_find":   true,
	"annotate":      true,
}

// camIsAvatarTool reports whether a tool is one of the avatar-registry tools, which
// are dropped from a child's toolset when the site has no enabled avatars.
func camIsAvatarTool(tool string) bool {
	switch tool {
	case "avatars", "avatar_info", "avatar_check", "avatar_find":
		return true
	}
	return false
}

// camSubagentEffectiveTools computes a child's usable toolset: the allowed ceiling
// (minus avatar tools when the site has none), further intersected with the subtask's
// tools_allowed when it names any VALID tools. A tools_allowed that lists only
// unknown/forbidden tools is ignored (falls back to the full ceiling) rather than
// leaving the child with nothing to do. Returns the tools in canonical prompt order
// plus a membership set for the loop's per-turn gate.
func camSubagentEffectiveTools(requested []string, hasAvatars bool) (ordered []string, set map[string]bool) {
	base := make(map[string]bool, len(camSubagentAllowedTools))
	for t := range camSubagentAllowedTools {
		if !hasAvatars && camIsAvatarTool(t) {
			continue
		}
		base[t] = true
	}
	set = base
	if len(requested) > 0 {
		narrowed := make(map[string]bool, len(requested))
		for _, t := range requested {
			t = strings.ToLower(strings.TrimSpace(t))
			if base[t] {
				narrowed[t] = true
			}
		}
		if len(narrowed) > 0 {
			set = narrowed // never widens: intersection with the base ceiling only
		}
	}
	for _, t := range investigateToolNames { // canonical order, subset of the global list
		if set[t] {
			ordered = append(ordered, t)
		}
	}
	return ordered, set
}

// camSubagentToolList renders a child's allowed tools as a comma list for the
// correction message shown when it requests a forbidden tool.
func camSubagentToolList(set map[string]bool) string {
	var names []string
	for _, t := range investigateToolNames {
		if set[t] {
			names = append(names, t)
		}
	}
	return strings.Join(names, ", ")
}

// ─────────────────────────────── sub-investigator prompt ───────────────────────────────

// camSubagentToolLine is the one-line tool description shown to a sub-investigator.
// Compact variants of the lead prompt's lines — a child never needs the lead-only
// tools, so its prompt stays short and focused on its scoped task.
func camSubagentToolLine(tool string) string {
	switch tool {
	case "roster":
		return "- roster: re-read YOUR cameras as text (no images; double-check exact ids).\n"
	case "snapshot":
		return "- snapshot: args.camera_ids, args.quality \"sub\"|\"main\" — a fresh CURRENT still per camera.\n"
	case "contact_sheet":
		return "- contact_sheet: args.camera_ids (ONE id), args.from + args.to (RFC3339), args.mode \"motion\"|\"interval\" — one composite frame scanning that camera's archive across the window (mode \"motion\" for doors/entrances, \"interval\" for busy areas). Keep each window under ~45 min.\n"
	case "past_frames":
		return "- past_frames: args.camera_ids, args.from + args.to (RFC3339), args.count, args.quality — stills sampled from recorded footage; use to SEE a window or zoom into flagged times.\n"
	case "motion_search":
		return "- motion_search: args.camera_ids (optional), args.from + args.to (RFC3339), args.event_type (optional) — the DVR motion log's exact episode times, no footage pulled (no media cost). Use FIRST to find WHEN.\n"
	case "activity_log":
		return "- activity_log: args.from + args.to (RFC3339), args.granularity \"entries\"|\"hours\"|\"days\" (default entries), args.camera_ids (optional, ONE id) — the site's continuous AI activity JOURNAL as text, no footage pulled (no media cost). PREFER it over re-scanning footage for \"what happened <when>\"; verify only pivotal moments with frames.\n"
	case "avatars":
		return "- avatars: (no args) — list the site's known people/vehicles/pets (id, name, type).\n"
	case "avatar_info":
		return "- avatar_info: args.avatar_id — that avatar's description + reference images (shown next turn).\n"
	case "avatar_check":
		return "- avatar_check: args.avatar_id, args.camera_ids (ONE id), args.time (RFC3339) — focused identity comparison at that instant; verdict + annotated frame on a match.\n"
	case "avatar_find":
		return "- avatar_find: args.avatar_id, args.camera_ids (ONE id), args.from + args.to (RFC3339) — scan a camera's archive for that avatar; sighting times + annotated sheet.\n"
	case "annotate":
		return "- annotate: args.camera_ids (ONE id), args.time (RFC3339), args.bbox {\"x0\",\"y0\",\"x1\",\"y1\"} on a 0-1000 grid — mint an evidence frame with your box circled.\n"
	}
	return ""
}

// camSubagentSystemPrompt builds a sub-investigator's per-call system prompt: a
// trimmed lead prompt scoped to ONE subtask. It reuses the shared operator-context
// (camSiteContextBlock) and clock (camPromptTimeBlock) blocks, shows ONLY the
// subtask's cameras (with a one-line "others are out of scope" note), lists only the
// child's allowed tool lines, and offers exactly two actions — call_tool and answer
// (never ask_operator). The role paragraph makes the relay contract explicit: cite
// exact media_url values because the LEAD investigator forwards them to the operator.
func camSubagentSystemPrompt(site camSite, dvrs []CamDVR, childCams []camera, avatars []camAvatar,
	orderedTools []string, now, dvrClock time.Time, dvrClockOK bool, tzName string) string {
	var b strings.Builder
	b.WriteString("You are a SUB-INVESTIGATOR handling ONE scoped subtask for a lead security investigator")
	if n := strings.TrimSpace(site.Name); n != "" {
		b.WriteString(" at ")
		b.WriteString(n)
	}
	b.WriteString(". You investigate ONLY the cameras and time window in your task by calling TOOLS the loop executes for you — you never fetch media yourself. You CANNOT ask the operator and you CANNOT delegate further: answer as soon as you can with what the cameras show. Cite the exact media_url values from TOOL RESULT lines — the lead investigator relays YOUR evidence to the operator, so an uncited finding is invisible.\n\n")

	if block := camSiteContextBlock(site, dvrs); block != "" {
		b.WriteString(block)
		b.WriteString("\n\n")
	}
	camPromptTimeBlock(&b, now, dvrClock, dvrClockOK, tzName)

	b.WriteString("YOUR CAMERAS (id — name [area]: description) — you may inspect ONLY these:\n")
	b.WriteString(camBuildRoster(childCams))
	b.WriteString("\nOther cameras exist at this site but are OUT OF SCOPE for your subtask; do not reference them.\n\n")

	b.WriteString("TOOLS you may call (action.tool):\n")
	for _, t := range orderedTools {
		b.WriteString(camSubagentToolLine(t))
	}
	b.WriteString("\n")

	b.WriteString("ACTIONS (action.type — exactly one per turn):\n")
	b.WriteString("- call_tool: run ONE of the TOOLS above; you see its result next turn.\n")
	b.WriteString("- answer: finish with action.answer (your verdict) plus action.evidence citing exact media_url values from TOOL RESULT lines.\n")
	b.WriteString("There is NO ask_operator action for a sub-investigator — if you truly cannot determine something, answer and state the limitation.\n\n")

	b.WriteString("QUALITY (args.quality): \"main\" = full-resolution for FINE detail (text, faces, plates, condition); \"sub\" = cheaper, fine for presence/counting/movement. Default \"sub\"; switch to \"main\" only when detail matters.\n\n")

	b.WriteString("PROTOCOL:\n")
	b.WriteString("- motion_search FIRST (no media cost) to find WHEN activity happened, then past_frames/contact_sheet those exact windows — don't brute-scan.\n")
	b.WriteString("- Binary-search change points; sample endpoints for durations; escalate to \"main\" on the closest camera for fine detail.\n")
	b.WriteString("- Cite an annotated frame (or the frame you relied on) for every claim; be economical and answer as soon as you can confidently.\n\n")

	toolEnum := strings.Join(orderedTools, "|")
	b.WriteString("OUTPUT: reply with EXACTLY ONE JSON object and nothing else (no prose, no markdown fences):\n")
	b.WriteString(`{"thought":"...","action":{"type":"call_tool|answer",`)
	b.WriteString("\n")
	b.WriteString(`  "tool":"` + toolEnum + `","args":{"camera_ids":["<id>"],"quality":"sub|main","from":"RFC3339","to":"RFC3339","count":6,"event_type":"<motion type>","granularity":"entries|hours|days","avatar_id":"<avatar id>","time":"RFC3339","bbox":{"x0":0,"y0":0,"x1":1000,"y1":1000}},`)
	b.WriteString("\n")
	b.WriteString(`  "answer":"...(your verdict)","evidence":[{"media_url":"...","caption":"..."}]}}`)
	b.WriteString("\nUse ONLY the EXACT camera ids listed above. Reply with ONLY the JSON object.")
	return b.String()
}

// ─────────────────────────────── the delegate tool ───────────────────────────────

// camSubagentVerdict is one child's outcome, collected by camToolDelegate and folded
// into the delegate tool result (summary block + citable evidence) and the "subagents"
// progress event.
type camSubagentVerdict struct {
	ChildID   string
	Question  string
	CamIDs    []string
	Window    string
	Status    string // answered | exhausted
	Answer    string
	Evidence  []evidenceItem
	KeyImages []string
	Fetches   int
}

// Aggregation caps: how much of each child's evidence/imagery the delegate tool row
// carries back to the lead. Bounded so a fan-out can't flood the parent transcript.
const (
	camSubagentEvidencePerChild = 6
	camSubagentEvidenceTotal    = 20
	camSubagentKeyImagePerChild = 4
	camSubagentKeyImageTotal    = 8
)

// camSubagentMinRemaining is the wall-clock headroom a delegate call requires: with
// less than this left in the pass there is no point spawning children (they'd be
// cancelled before doing useful work), so the tool refuses and lets the lead answer.
const camSubagentMinRemaining = 90 * time.Second

// camToolDelegate executes the `delegate` tool: it validates and caps the requested
// subtasks, inserts a child investigation row per valid subtask (status "running",
// parent_id = the lead run), and runs them CONCURRENTLY in-process under camSubagentSem,
// each on a context derived from the parent's budget context. It blocks until every
// child finishes (safe — this is the background worker's own goroutine, or the inline
// drain), then aggregates the verdicts into one tool result whose media_json makes the
// children's evidence citable by the lead. Media fetches spent by the children are
// summed into Fetches so the parent's media budget stays accurate across a resume.
func camToolDelegate(ctx context.Context, cfg config, db *sql.DB, r *http.Request, site camSite, alias string, inv camInvestigation, args investigateArgs, camByID map[string]camera, dvrByID map[string]CamDVR, allowed map[string]bool, active []camera, scratch string, mediaLeft int, prog *camProgressNotifier) investigateToolResult {
	maxSub := camSubagentMax(cfg)
	if maxSub <= 0 {
		return investigateToolResult{Summary: "delegate: sub-agent delegation is disabled on this deployment — investigate the cameras yourself with the other tools."}
	}
	if dl, ok := ctx.Deadline(); ok && time.Until(dl) < camSubagentMinRemaining {
		return investigateToolResult{Summary: "delegate: not enough time budget remains this pass to fan out sub-investigators — answer with the evidence you already have (the run auto-resumes if it needs another pass)."}
	}

	// Sub-investigations analyze frame-heavy multi-camera slices — the same "many
	// frames per call" profile the operator routed OFF agy for avatar scans. When
	// PROXY_CAMERA_SUBAGENT_ALIAS is set it overrides the parent's alias for EVERY
	// child (its stored row + its analysis + nested-tool calls), so the frame-heavy
	// children can move to a reliable backend while the parent stays on the cheap
	// alias it was invoked with. Unset → children run on the parent alias, unchanged.
	childAlias := firstNonEmpty(strings.TrimSpace(cfg.CameraSubagentAlias), alias)

	// Operator context for the children's prompts, loaded once. The DVR clock is
	// fetched once here and shared by every child so a fan-out makes at most one clock
	// probe (the deadline check above guarantees there is time for it).
	dvrs, _ := listCameraDVRs(db, cfg, site.ID)
	avatars, _ := listCameraAvatars(db, site.ID, false)
	hasAvatars := len(avatars) > 0
	loc, tzName := camSiteTimezone(dvrs)
	srvNow := time.Now().In(loc)
	var dvrClock time.Time
	dvrClockOK := false
	if len(dvrs) > 0 {
		if t, ok := camDVRClock(ctx, dvrs[0]); ok {
			dvrClock, dvrClockOK = t.In(loc), true
		}
	}

	// Validate + normalize each subtask; cap at maxSub. A subtask is valid only when it
	// names ≥1 of the LEAD's allowed cameras and carries a parseable window.
	type childPlan struct {
		sub      investigateSubtask
		childID  string
		cams     []camera
		camIDs   []string
		allowed  map[string]bool
		tools    map[string]bool
		toolList []string
		window   string
	}
	var plans []childPlan
	skipped := 0
	for _, st := range args.Subtasks {
		if len(plans) >= maxSub {
			skipped++
			continue
		}
		ids := camFilterAllowed(st.CameraIDs, allowed)
		if len(ids) == 0 {
			skipped++
			continue
		}
		from, to, werr := camParseWindow(st.From, st.To, cfg, false)
		if werr != nil {
			skipped++
			continue
		}
		var cams []camera
		for _, id := range ids {
			if c, ok := camByID[id]; ok {
				cams = append(cams, c)
			}
		}
		if len(cams) == 0 {
			skipped++
			continue
		}
		childAllowed := make(map[string]bool, len(ids))
		for _, id := range ids {
			childAllowed[id] = true
		}
		toolList, toolSet := camSubagentEffectiveTools(st.Tools, hasAvatars)
		plans = append(plans, childPlan{
			sub: st, cams: cams, camIDs: ids, allowed: childAllowed,
			tools: toolSet, toolList: toolList, window: windowLabel(from, to),
		})
	}
	if len(plans) == 0 {
		return investigateToolResult{Summary: "delegate: no valid subtasks — each subtask needs at least one of YOUR cameras (exact roster ids) and a parseable from/to window. Investigate directly instead."}
	}

	// Persist a child row + its opening operator turn (the question) per plan. Children
	// are inserted "running" — they NEVER enter the queue, so the worker never claims
	// one; a child that fails to persist is dropped from the fan-out (best-effort).
	live := plans[:0]
	for i := range plans {
		childID, ierr := insertCameraInvestigation(db, camInvestigation{
			SiteID:   site.ID,
			ParentID: inv.ID,
			Title:    "sub: " + truncateString(strings.TrimSpace(plans[i].sub.Question), 70),
			Question: plans[i].sub.Question,
			Alias:    childAlias,
			Status:   "running",
		})
		if ierr != nil {
			camlog("warn", "investigate_subagent_insert", map[string]any{"parent_id": inv.ID, "ok": false, "error": ierr.Error()})
			continue
		}
		plans[i].childID = childID
		camAppendInvestigateMessage(db, childID, "operator", plans[i].sub.Question, "", "", 0, nil)
		live = append(live, plans[i])
	}
	plans = live
	if len(plans) == 0 {
		return investigateToolResult{Summary: "delegate: could not create any sub-investigations (database error). Investigate directly instead."}
	}

	perChildMedia := camSubagentPerChildMedia(camSubagentMaxMedia(cfg), mediaLeft, len(plans))
	camlog("info", "investigate_delegate", map[string]any{
		"investigation_id": inv.ID, "children": len(plans), "skipped": skipped,
		"per_child_media": perChildMedia, "media_left": mediaLeft,
	})

	// Run the children concurrently. Each verdict slot is pre-seeded with an exhausted
	// default so a panic or a slot-acquire failure still yields a reportable outcome;
	// distinct indices are written by distinct goroutines (no shared-slot race), and
	// wg.Wait() establishes the happens-before for the aggregation read below.
	verdicts := make([]camSubagentVerdict, len(plans))
	var wg sync.WaitGroup
	for i := range plans {
		p := plans[i]
		verdicts[i] = camSubagentVerdict{ChildID: p.childID, Question: p.sub.Question, CamIDs: p.camIDs, Window: p.window, Status: "exhausted"}
		wg.Add(1)
		go func(i int, p childPlan) {
			defer wg.Done()
			// A panic in one child fails only that child, never the whole fan-out.
			defer func() {
				if rec := recover(); rec != nil {
					camlog("error", "investigate_subagent_panic", map[string]any{"child_id": p.childID, "panic": fmt.Sprint(rec)})
					camAppendInvestigateMessage(db, p.childID, "system", "Sub-investigation stopped: an internal error occurred.", "", "", 0, nil)
					_ = setCameraInvestigationStatus(db, p.childID, "exhausted")
				}
			}()
			release, ok := camSubagentAcquire(ctx)
			if !ok {
				camAppendInvestigateMessage(db, p.childID, "system", "Sub-investigation could not start before the time budget elapsed.", "", "", 0, nil)
				_ = setCameraInvestigationStatus(db, p.childID, "exhausted")
				return
			}
			defer release()
			// Derive the child context from the PARENT's budget context so the lead
			// deadline always wins; camSubagentBudget only ever shortens it further.
			cctx, ccancel := context.WithTimeout(ctx, camSubagentBudget(cfg))
			defer ccancel()
			// Child scratch nested UNDER the parent scratch, so the child's key frames
			// outlive the delegate turn (parent removes the tree at run end) and remain
			// attachable to the lead's next analysis call.
			childScratch := filepath.Join(scratch, "sub_"+p.childID)
			if err := os.MkdirAll(childScratch, 0o700); err != nil {
				camAppendInvestigateMessage(db, p.childID, "system", "Sub-investigation could not start: "+err.Error(), "", "", 0, nil)
				_ = setCameraInvestigationStatus(db, p.childID, "exhausted")
				return
			}
			sys := camSubagentSystemPrompt(site, dvrs, p.cams, avatars, p.toolList, srvNow, dvrClock, dvrClockOK, tzName)
			status, answer, evidence, keyImages, fetches := camRunSubagentLoop(
				cctx, cfg, db, r, site, childAlias, p.childID, sys, camByID, dvrByID, p.allowed, p.cams, p.tools, childScratch, perChildMedia, prog)
			verdicts[i] = camSubagentVerdict{
				ChildID: p.childID, Question: p.sub.Question, CamIDs: p.camIDs, Window: p.window,
				Status: status, Answer: answer, Evidence: evidence, KeyImages: keyImages, Fetches: fetches,
			}
		}(i, p)
	}
	wg.Wait()

	return camAggregateSubagentVerdicts(verdicts, prog)
}

// camAggregateSubagentVerdicts folds the finished children into ONE delegate tool
// result: a per-subtask verdict summary, the children's kept evidence (≤6/child,
// ≤20 total — this is what makes a child's frame citable by the lead), up to
// camSubagentKeyImageTotal key image paths for the lead's next analysis attachment,
// and the summed media spend. It also fires the "subagents" progress event.
func camAggregateSubagentVerdicts(verdicts []camSubagentVerdict, prog *camProgressNotifier) investigateToolResult {
	var sum strings.Builder
	fmt.Fprintf(&sum, "Delegated %d sub-investigation(s); their evidence frames/clips are listed below and are citable in your final answer.\n", len(verdicts))

	var (
		media   []evidenceItem
		images  []string
		fetches int
		seen    = map[string]bool{}
		subs    = make([]camSubagentSummary, 0, len(verdicts))
	)
	for i, v := range verdicts {
		fetches += v.Fetches
		answer := firstNonEmpty(strings.TrimSpace(v.Answer), "(no conclusion reached)")
		fmt.Fprintf(&sum, "SUBTASK %d [%s | %s] %s — %s\n",
			i+1, strings.Join(v.CamIDs, ","), v.Window, strings.ToUpper(v.Status), truncateString(answer, 600))

		kept := 0
		for _, e := range v.Evidence {
			if len(media) >= camSubagentEvidenceTotal || kept >= camSubagentEvidencePerChild {
				break
			}
			u := strings.TrimSpace(e.MediaURL)
			if u == "" || seen[u] {
				continue
			}
			seen[u] = true
			media = append(media, e)
			kept++
		}
		for j, p := range v.KeyImages {
			if len(images) >= camSubagentKeyImageTotal || j >= camSubagentKeyImagePerChild {
				break
			}
			images = append(images, p)
		}
		subs = append(subs, camSubagentSummary{
			ID: v.ChildID, Question: v.Question, Status: v.Status,
			Summary: answer, EvidenceCount: len(v.Evidence),
		})
	}
	prog.NotifySubagents(subs)

	return investigateToolResult{
		Summary: strings.TrimRight(sum.String(), "\n"),
		Media:   media,
		Images:  images,
		Fetches: fetches,
	}
}

// ─────────────────────────────── sub-investigator loop ───────────────────────────────

// camSubagentCoerceAnswer converts an ask_operator action into an answer: a
// sub-investigator has no operator to pause for, so it must report what it has. The
// operator-directed question text becomes the verdict body (falling back to the
// turn's thought). Any other action type is returned unchanged.
func camSubagentCoerceAnswer(act investigateAction) investigateAction {
	if act.Action.Type != "ask_operator" {
		return act
	}
	act.Action.Type = "answer"
	if strings.TrimSpace(act.Action.Answer) == "" {
		act.Action.Answer = firstNonEmpty(
			strings.TrimSpace(act.Action.Question),
			strings.TrimSpace(act.Thought),
			"A sub-investigator cannot ask the operator; reporting the findings gathered so far.")
	}
	return act
}

// camRunSubagentLoop drives ONE child's bounded agentic loop, reusing every lead-loop
// building block (camAnalyzeInvestigateAction → WS1 resilience, camExecuteInvestigateTool,
// the transcript/persist helpers). It is scoped hard: only childAllowedCams may be
// touched (a call to any other tool is corrected in-band, no fetch), tools outside
// allowedTools are refused (this is where recursion is denied — delegate is never in
// the set), turns are capped at camSubagentMaxTurns, media at perChildMedia, and the
// context deadline (already derived from the parent budget) bounds wall-clock. On a
// clean answer it returns "answered" with the model's filtered evidence; on any bound
// (turns/budget/analysis wall) it drops a system note, marks the child "exhausted",
// and returns a PARTIAL verdict (last thought + everything minted so far) so the lead
// still gets whatever the child found. prog receives the child's key-tool media
// tagged with the child id (nil-safe).
func camRunSubagentLoop(cctx context.Context, cfg config, db *sql.DB, r *http.Request, site camSite, alias, childID, sys string, camByID map[string]camera, dvrByID map[string]CamDVR, childAllowedCams map[string]bool, childActive []camera, allowedTools map[string]bool, childScratch string, perChildMedia int, prog *camProgressNotifier) (status, answer string, evidence []evidenceItem, keyImages []string, fetches int) {
	maxTurns := camSubagentMaxTurns(cfg)
	childInv := camInvestigation{ID: childID, SiteID: site.ID, Alias: alias}
	msgs, _ := listCameraInvestigationMessages(db, childID)
	var lastImages []string
	turns, mediaUsed := 0, 0

	for {
		if cctx.Err() != nil {
			return camSubagentBail(db, childID, msgs, lastImages, mediaUsed, "Sub-investigation time budget elapsed before a conclusion.")
		}
		if turns >= maxTurns {
			return camSubagentBail(db, childID, msgs, lastImages, mediaUsed, "Sub-investigation reached its turn limit before a conclusion.")
		}

		userText := camInvestigateTranscriptText(msgs) + "\n\nReply with ONLY the JSON action object now."
		tstart := time.Now()
		act, raw, repaired, attempts, aerr := camAnalyzeInvestigateAction(cctx, cfg, alias, sys, userText, lastImages)
		latency := time.Since(tstart).Milliseconds()
		if aerr != nil {
			camlog("warn", "investigate_subagent_turn", map[string]any{
				"child_id": childID, "turn": turns, "ok": false, "error": aerr.Error(), "repair_used": repaired, "latency_ms": latency,
			})
			return camSubagentBail(db, childID, msgs, lastImages, mediaUsed, "Sub-investigation analysis failed after all retries ("+truncateString(aerr.Error(), 160)+").")
		}
		if camAnalyzeRetried(attempts) {
			last := attempts[len(attempts)-1]
			rm := camAppendInvestigateMessage(db, childID, "system",
				fmt.Sprintf("Analysis retried: succeeded on %s after %d attempt(s).", last.Alias, len(attempts)), "analysis_retry", "", 0, nil)
			msgs = append(msgs, rm)
		}
		act = camSubagentCoerceAnswer(act) // a sub-investigator cannot ask the operator
		camlog("info", "investigate_subagent_turn", map[string]any{
			"child_id": childID, "turn": turns, "ok": true, "action": act.Action.Type, "tool": act.Action.Tool, "latency_ms": latency, "resp_bytes": len(raw),
		})

		if act.Action.Type == "answer" {
			mintedSet, mintedByToken := camCollectMintedMedia(msgs)
			ev, dropped := camFilterEvidence(act.Action.Evidence, mintedSet, mintedByToken)
			camAppendInvestigateMessage(db, childID, "ai", camAIMessageContent(act.Thought, act.Action.Answer), "answer", "", 0, ev)
			_ = setCameraInvestigationStatus(db, childID, "answered")
			camlog("info", "investigate_subagent_done", map[string]any{
				"child_id": childID, "status": "answered", "turns": turns + 1, "cited": len(act.Action.Evidence), "kept": len(ev), "dropped": dropped,
			})
			return "answered", strings.TrimSpace(act.Action.Answer), ev, camCapImages(lastImages, camSubagentKeyImagePerChild), mediaUsed
		}

		// call_tool
		tool := act.Action.Tool
		toolArgsJSON := mustJSON(act.Action.Args)
		aiMsg := camAppendInvestigateMessage(db, childID, "ai", act.Thought, tool, toolArgsJSON, 0, nil)
		msgs = append(msgs, aiMsg)
		turns++

		if !allowedTools[tool] {
			// Restricted/forbidden tool (this is the recursion refusal for "delegate"):
			// correct in-band with zero fetch so the child spends a turn but keeps going.
			note := "tool \"" + tool + "\" is not available to a sub-investigator. Available tools: " + camSubagentToolList(allowedTools) + ". Pick one of those, or answer."
			tm := camAppendInvestigateMessage(db, childID, "tool", note, tool, toolArgsJSON, 0, nil)
			msgs = append(msgs, tm)
			lastImages = nil
			continue
		}

		mediaLeft := perChildMedia - mediaUsed
		var tr investigateToolResult
		ttStart := time.Now()
		if mediaLeft <= 0 {
			tr = investigateToolResult{Summary: "Sub-investigation media budget is exhausted — answer with the evidence already gathered."}
		} else {
			// nil prog into the dispatcher: only delegate forwards it, and a child can't
			// delegate; the child's own key-tool media is streamed explicitly below.
			tr = camExecuteInvestigateTool(cctx, cfg, db, r, site, alias, childInv, tool, act.Action.Args, camByID, dvrByID, childAllowedCams, childActive, childScratch, mediaLeft, nil)
		}
		mediaUsed += tr.Fetches
		camlog("info", "investigate_subagent_tool", map[string]any{
			"child_id": childID, "tool": tool, "fetches": tr.Fetches, "media_used": mediaUsed, "images": len(tr.Images), "latency_ms": time.Since(ttStart).Milliseconds(),
		})

		attached := tr.Images
		if maxImg := camAnalyzeMaxImages(cfg); len(tr.Images) > maxImg {
			attached = camCapImages(tr.Images, maxImg)
			tr.Summary += fmt.Sprintf(" (Attaching %d of %d images, evenly spaced — re-run on a narrower window to see the rest.)", len(attached), len(tr.Images))
		}
		tm := camAppendInvestigateMessage(db, childID, "tool", tr.Summary, tool, toolArgsJSON, tr.Fetches, tr.Media)
		msgs = append(msgs, tm)
		lastImages = attached
		prog.ObserveMedia(childID, tool, tr.Media) // key tools only; coalesced; nil-safe
	}
}

// camSubagentBail terminalizes a child that ran out of turns/budget or hit an
// analysis wall: it drops a system note, marks the row "exhausted", and returns a
// PARTIAL verdict — the child's last reasoning as the answer and every media artifact
// it minted so far as evidence, so a bounded child still contributes to the lead.
func camSubagentBail(db *sql.DB, childID string, msgs []camInvestigationMessage, lastImages []string, fetches int, note string) (status, answer string, evidence []evidenceItem, keyImages []string, out int) {
	camAppendInvestigateMessage(db, childID, "system", note, "", "", 0, nil)
	_ = setCameraInvestigationStatus(db, childID, "exhausted")
	camlog("info", "investigate_subagent_done", map[string]any{"child_id": childID, "status": "exhausted"})
	return "exhausted", camLastThought(msgs), camGatherMintedEvidence(msgs, camSubagentEvidencePerChild), camCapImages(lastImages, camSubagentKeyImagePerChild), fetches
}

// camLastThought returns the content of the last "ai" transcript row (the child's
// most recent reasoning), used as the answer body of a partial verdict. Empty when
// the child never took a model turn.
func camLastThought(msgs []camInvestigationMessage) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "ai" {
			return strings.TrimSpace(msgs[i].Content)
		}
	}
	return ""
}

// camGatherMintedEvidence collects the evidence artifacts a child actually minted
// (every tool row's media_json), de-duplicated by media_url and capped at max. Used to
// build a bounded child's partial-verdict evidence — all of it is real minted media,
// so it needs no further validation.
func camGatherMintedEvidence(msgs []camInvestigationMessage, max int) []evidenceItem {
	var out []evidenceItem
	seen := map[string]bool{}
	for _, m := range msgs {
		if m.Role != "tool" || strings.TrimSpace(m.MediaJSON) == "" {
			continue
		}
		var media []evidenceItem
		if json.Unmarshal([]byte(m.MediaJSON), &media) != nil {
			continue
		}
		for _, e := range media {
			u := strings.TrimSpace(e.MediaURL)
			if u == "" || seen[u] {
				continue
			}
			seen[u] = true
			out = append(out, e)
			if len(out) >= max {
				return out
			}
		}
	}
	return out
}
