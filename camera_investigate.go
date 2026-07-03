package main

// camera_investigate.go — the "ask-AI investigation chat" feature (roster-first
// targeting + a server-orchestrated agentic loop with camera tools).
//
// TWO things live here:
//
//  1. ROSTER-FIRST TARGETING (camBuildRoster + camSelectCameras): instead of
//     blindly pulling imagery from every camera, the model first sees the site
//     ROSTER as TEXT (per enabled camera: id, name, area, ai_description) plus the
//     task, and picks WHICH cameras are worth pulling images from. The caller then
//     fetches imagery only for the returned ids. Selection is a single text-only
//     analyzeWithAlias call (no images) whose reply is parsed as strict JSON and
//     filtered down to ids that actually exist in the roster.
//
//  2. THE INVESTIGATION LOOP (runInvestigation + the four HTTP handlers): an
//     operator asks a freeform question about a site; the server drives a bounded
//     ReAct loop over camera TOOLS (roster / snapshot / mosaic / contact_sheet /
//     past_frames / past_clip, the knowledge tools playbook / call_api, and the
//     avatar tools avatars / avatar_info / avatar_check / avatar_find / annotate),
//     going back and forth across time, optionally pausing to ask the
//     operator, and finally answering with cited evidence. Each turn the model
//     emits ONE JSON action (investigateAction). The loop, NOT the model, executes
//     the tools and feeds results (images + text) back — so a text-only backend
//     still works, exactly like the escalation loop (camera_escalate.go).
//
// WP0 established the data model, config, roster-first helpers, and the
// tool-action JSON contract. WP1 (this commit) fills runInvestigation and the
// four HTTP handlers with the real agentic loop.
//
// BOUNDS: max turns (PROXY_CAMERA_INVESTIGATE_MAX_TURNS, default 12, hard cap 30),
// max media fetches (PROXY_CAMERA_INVESTIGATE_MAX_MEDIA, default 30, counted in
// DVR device fetches), and a wall-clock budget (PROXY_CAMERA_INVESTIGATE_BUDGET,
// default 360s) applied as a context deadline for THIS call only — time spent
// paused on ask_operator waiting for the operator never counts against it. Turn/
// media counts are cumulative within a single RUN, reconstructed from the persisted
// transcript each call (camCountInvestigateProgress). A run pauses only on
// ask_operator; resuming that pause carries the budget over (so a model can't reset
// it by looping on ask_operator), while a genuinely new operator-initiated question
// after the run terminated (answer / bound / error) begins a fresh, independently
// bounded run — human-gated, never model-driven, so it still can't run unbounded.
// The loop NEVER calls the model again once a bound is hit; it stops gracefully
// with a system note and status "exhausted". Every turn/tool call is camlog'd
// (op=investigate_turn / investigate_tool) with latency, tool, and parse ok/fail.
//
// RUN MODEL: synchronous per HTTP request, capped by the bounds above — POST
// /investigations and /investigations/reply both drive the loop to completion
// (ask_operator pause or answer) before responding, per the plan's "sane
// per-request cap" option. The UI polls/reloads via GET .../get for progress if
// a run is slow.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ─────────────────────────────── tool-action contract ───────────────────────────────

// investigateAction is the single per-turn JSON object the investigator model
// emits. Its nested Action carries the actual command:
//
//	{"thought":"...","action":{"type":"call_tool|ask_operator|answer",
//	  "tool":"roster|snapshot|mosaic|contact_sheet|past_frames|past_clip|motion_search|playbook|call_api|avatars|avatar_info|avatar_check|avatar_find|annotate",
//	  "args":{"camera_ids":[...],"quality":"sub|main","from":"RFC3339","to":"RFC3339","count":6,"event_type":"VMD",
//	    "name":"<playbook or api tool>","params":{"key":"value"},
//	    "avatar_id":"...","time":"RFC3339","bbox":{"x0":0,"y0":0,"x1":1000,"y1":1000}},
//	  "question":"...(ask_operator)","answer":"...(final narrative)",
//	  "evidence":[{"media_url":"...","caption":"..."}]}}
type investigateAction struct {
	Thought string             `json:"thought"`
	Action  investigateCommand `json:"action"`
}

// investigateCommand is the "action" node of an investigateAction: what the model
// wants the loop to do this turn.
type investigateCommand struct {
	Type     string          `json:"type"` // call_tool | ask_operator | answer
	Tool     string          `json:"tool"` // roster | snapshot | mosaic | past_frames | past_clip
	Args     investigateArgs `json:"args"`
	Question string          `json:"question"` // set when Type == "ask_operator"
	Answer   string          `json:"answer"`   // final narrative when Type == "answer"
	Evidence []evidenceItem  `json:"evidence"` // media cited with the final answer
}

// investigateArgs is the tool argument bag. Only the fields relevant to the chosen
// tool are read (e.g. past_clip uses a single camera + from/to + quality; snapshot
// uses camera_ids + quality; past_frames adds count). Count uses the tolerant
// flexInt decoder so a text model emitting "6" is still accepted.
type investigateArgs struct {
	CameraIDs []string        `json:"camera_ids"`
	Quality   string          `json:"quality"`    // sub | main
	From      string          `json:"from"`       // RFC3339 absolute time (DVR timezone honored by the loop)
	To        string          `json:"to"`         // RFC3339 absolute time
	Count     flexInt         `json:"count"`      // desired still count for past_frames
	Mode      string          `json:"mode"`       // contact_sheet: "motion" (changed frames) | "interval" (uniform time-lapse)
	Name      string          `json:"name"`       // playbook / call_api: the catalog entry to invoke (exact name)
	Params    map[string]any  `json:"params"`     // call_api: {{placeholder}} values (stringified tolerantly)
	AvatarID  string          `json:"avatar_id"`  // avatar_info / avatar_check / avatar_find: the avatar to inspect/match
	Time      string          `json:"time"`       // avatar_check / annotate: RFC3339 archive instant
	BBox      json.RawMessage `json:"bbox"`       // annotate: {"x0","y0","x1","y1"} normalized 0-1000, top-left origin
	EventType string          `json:"event_type"` // motion_search: optional exact event-type filter (VMD | linedetection | …)
}

// evidenceItem is one media artifact cited in a final answer: a served
// /camera/media/<token> URL plus a short caption describing what it shows.
type evidenceItem struct {
	MediaURL string `json:"media_url"`
	Caption  string `json:"caption"`
}

// investigateToolNames is the set of tools the loop can execute. Kept here so the
// prompt builder and the executor agree on exactly one list.
var investigateToolNames = []string{"roster", "snapshot", "mosaic", "contact_sheet", "past_frames", "past_clip", "motion_search", "playbook", "call_api", "avatars", "avatar_info", "avatar_check", "avatar_find", "annotate"}

// ─────────────────────────────── roster-first targeting ───────────────────────────────

// camBuildRoster renders the enabled cameras of a site as a plain-text roster the
// model reads to choose which cameras to inspect. One line per enabled camera:
// "- <id> — <name> [<area>]: <ai_description> | location: <ai_location> | operator notes: <notes>"
// (name/area/description/location/notes omitted when blank). Disabled cameras are
// skipped so the model never targets a camera the loop can't capture.
func camBuildRoster(cams []camera) string {
	var b strings.Builder
	n := 0
	for _, c := range cams {
		if !c.Enabled {
			continue
		}
		n++
		b.WriteString("- ")
		b.WriteString(c.ID)
		if name := strings.TrimSpace(c.Name); name != "" {
			b.WriteString(" — ")
			b.WriteString(name)
		}
		if area := strings.TrimSpace(c.Area); area != "" {
			b.WriteString(" [")
			b.WriteString(area)
			b.WriteString("]")
		}
		if d := strings.TrimSpace(c.AIDescription); d != "" {
			b.WriteString(": ")
			b.WriteString(d)
		}
		if loc := strings.TrimSpace(c.AILocation); loc != "" {
			b.WriteString(" | location: ")
			b.WriteString(loc)
		}
		if notes := strings.TrimSpace(c.Notes); notes != "" {
			b.WriteString(" | operator notes: ")
			b.WriteString(notes)
		}
		b.WriteString("\n")
	}
	if n == 0 {
		return "(no enabled cameras)"
	}
	return strings.TrimRight(b.String(), "\n")
}

// camSiteContextBlock renders the operator-authored knowledge shared by the
// investigation system prompt and the roster-first selector: the site's
// description plus one paragraph per DVR with non-blank AIInstructions (the
// `[DVR "<name-or-id>"]` prefix is omitted when the site has exactly one DVR).
// Returns "" when nothing is set, so callers can skip the section entirely.
func camSiteContextBlock(site camSite, dvrs []CamDVR) string {
	var b strings.Builder
	if desc := strings.TrimSpace(site.Description); desc != "" {
		b.WriteString("SITE PROFILE (operator-authored — trust this over your own guesses):\n")
		b.WriteString(desc)
		b.WriteString("\n")
	}
	var instr strings.Builder
	for _, d := range dvrs {
		text := strings.TrimSpace(d.AIInstructions)
		if text == "" {
			continue
		}
		if len(dvrs) > 1 {
			fmt.Fprintf(&instr, "[DVR %q] %s\n", firstNonEmpty(strings.TrimSpace(d.Name), d.ID), text)
		} else {
			instr.WriteString(text)
			instr.WriteString("\n")
		}
	}
	if instr.Len() > 0 {
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		b.WriteString("OPERATOR INSTRUCTIONS (authoritative facts about this site's layout and rules):\n")
		b.WriteString(instr.String())
	}
	return strings.TrimRight(b.String(), "\n")
}

// camSelection is the strict JSON the model returns from camSelectCameras.
type camSelection struct {
	CameraIDs []string `json:"camera_ids"`
	Reason    string   `json:"reason"`
}

// camSelectCameras is the roster-first selector: it shows the model the site roster
// as TEXT (NO images) plus the task, and asks it to return
// {"camera_ids":[...],"reason":"..."} — the subset of cameras worth pulling imagery
// from. siteContext is the operator-authored camSiteContextBlock ("" = none),
// prepended so selections honor the operator's layout knowledge. The reply is
// parsed with the shared JSON extractor and filtered down to ids
// that actually exist (enabled) in the roster, de-duplicated and order-preserving.
// An empty result is valid (the model judged no camera relevant); the caller
// decides how to proceed.
func camSelectCameras(ctx context.Context, cfg config, alias, task, siteContext string, cams []camera) ([]string, error) {
	allowed := make(map[string]bool, len(cams))
	for _, c := range cams {
		if c.Enabled {
			allowed[c.ID] = true
		}
	}

	sys := "You are a security-camera investigator for a physical premises (e.g. a clinic). " +
		"You are given a roster of cameras and a task. Choose ONLY the cameras whose imagery is worth " +
		"pulling to accomplish the task — prefer the smallest useful set. Use the EXACT camera ids from the roster. " +
		"Reply with EXACTLY ONE JSON object and nothing else (no prose, no markdown fences):\n" +
		`{"camera_ids":["<id>"],"reason":"..."}`

	var b strings.Builder
	if sc := strings.TrimSpace(siteContext); sc != "" {
		b.WriteString("SITE CONTEXT:\n")
		b.WriteString(sc)
		b.WriteString("\n\n")
	}
	b.WriteString("CAMERA ROSTER (id — name [area]: description):\n")
	b.WriteString(camBuildRoster(cams))
	b.WriteString("\n\nTASK:\n")
	b.WriteString(strings.TrimSpace(task))
	b.WriteString("\n\nReturn ONLY the JSON object with the camera_ids to inspect and a short reason.")

	raw, err := analyzeWithAlias(ctx, cfg, alias, sys, b.String(), nil)
	if err != nil {
		return nil, err
	}
	out, requested, perr := parseCameraSelection(raw, allowed)
	if perr != nil {
		return nil, perr
	}
	camlog("info", "investigate_select", map[string]any{
		"alias": alias, "roster": len(allowed), "requested": requested, "selected": len(out),
	})
	return out, nil
}

// parseCameraSelection extracts and decodes camSelectCameras' single JSON reply
// and filters its camera_ids down to ones present in allowed, de-duplicated and
// order-preserving. Kept network-free (pure text in, ids out) so it is unit
// testable independent of analyzeWithAlias — mirrors parseDecision's separation
// from camAnalyzeDecision in cameraorch.go/camera_escalate.go.
func parseCameraSelection(raw string, allowed map[string]bool) (ids []string, requested int, err error) {
	obj, ok := extractFirstJSONObject(raw)
	if !ok {
		return nil, 0, errors.New("no JSON object found in camera-selection reply")
	}
	var sel camSelection
	if err := json.Unmarshal(obj, &sel); err != nil {
		return nil, 0, fmt.Errorf("decode camera selection JSON: %w", err)
	}

	seen := make(map[string]bool, len(sel.CameraIDs))
	out := make([]string, 0, len(sel.CameraIDs))
	for _, id := range sel.CameraIDs {
		id = strings.TrimSpace(id)
		if id != "" && allowed[id] && !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out, len(sel.CameraIDs), nil
}

// ─────────────────────────────── loop bounds ───────────────────────────────

// camInvestigateMaxTurns resolves the per-model-call turn cap, clamped to 1..30
// so a misconfigured override can never make the loop unbounded.
func camInvestigateMaxTurns(cfg config) int {
	n := cfg.CameraInvestigateMaxTurns
	if n <= 0 {
		n = 12
	}
	if n > 30 {
		n = 30
	}
	return n
}

// camInvestigateMaxMedia resolves the per-investigation device-fetch budget.
func camInvestigateMaxMedia(cfg config) int {
	n := cfg.CameraInvestigateMaxMedia
	if n <= 0 {
		n = 30
	}
	return n
}

// camInvestigateBudget resolves the wall-clock budget for ONE runInvestigation
// call (applied as a context deadline) — time spent paused on ask_operator
// between calls never counts against it.
func camInvestigateBudget(cfg config) time.Duration {
	d := cfg.CameraInvestigateBudget
	if d <= 0 {
		d = 30 * time.Minute // detached background run — no human is blocked on it
	}
	if d > time.Hour {
		d = time.Hour // hard ceiling for a single detached run
	}
	return d
}

// ─────────────────────────────── system prompt ───────────────────────────────

// camInvestigateSystemPrompt builds the loop's per-call system prompt: the
// investigator role, the operator-authored site context (camSiteContextBlock),
// the current time in the site's DVR timezone (so the model can construct
// absolute RFC3339 windows), the camera roster (reused from camBuildRoster —
// the roster-first contract), the operational-playbook and external-API-tool
// catalogs (only when the site has any), the tool list + ACTIONS + JSON
// contract, and the investigation protocol. The avatar tool lines/strategy are
// emitted only when the site has enabled avatars.
func camInvestigateSystemPrompt(site camSite, dvrs []CamDVR, active []camera,
	playbooks []camPlaybook, apiTools []camAPITool, avatars []camAvatar,
	now, dvrClock time.Time, dvrClockOK bool, tzName string) string {
	var b strings.Builder
	b.WriteString("You are a security investigator for a physical premises")
	if n := strings.TrimSpace(site.Name); n != "" {
		b.WriteString(" (")
		b.WriteString(n)
		b.WriteString(")")
	}
	b.WriteString(". An operator has asked you a freeform question about what happened on camera. You investigate by ")
	b.WriteString("calling TOOLS that the loop executes for you — you never fetch media yourself, only request it, and every ")
	b.WriteString("tool result (images and/or text) is shown to you on your NEXT turn.\n\n")

	// Operator-authored knowledge: site profile + per-DVR instructions.
	if block := camSiteContextBlock(site, dvrs); block != "" {
		b.WriteString(block)
		b.WriteString("\n\n")
	}

	fmt.Fprintf(&b, "CURRENT TIME (real / operator now): %s — today is %s, timezone %s.\n",
		now.Format(time.RFC3339), now.Format("Monday 2 January 2006"), tzName)
	if dvrClockOK {
		skew := dvrClock.Sub(now)
		mag := skew
		if mag < 0 {
			mag = -mag
		}
		if mag >= 45*time.Second {
			dir := "AHEAD of"
			if skew < 0 {
				dir = "BEHIND"
			}
			fmt.Fprintf(&b, "DVR CLOCK: %s — the DVR stamps its RECORDINGS with this clock, which is currently ~%s %s the real time. When you request past_frames/past_clip, express \"from\"/\"to\" against the DVR CLOCK so the window lines up with the recordings.\n",
				dvrClock.Format(time.RFC3339), mag.Round(time.Second), dir)
		} else {
			fmt.Fprintf(&b, "DVR CLOCK: %s (in sync with real time).\n", dvrClock.Format(time.RFC3339))
		}
	}
	b.WriteString("Use RFC3339 timestamps with this exact UTC offset for every \"from\"/\"to\" argument; e.g. \"yesterday at 15:00\" = yesterday's date at 15:00 with this offset.\n\n")

	b.WriteString("CAMERA ROSTER (id — name [area]: description):\n")
	b.WriteString(camBuildRoster(active))
	b.WriteString("\n\n")

	// Operational playbooks catalog — one line per enabled playbook (full text
	// only via the playbook tool), emitted only when the site has any.
	if len(playbooks) > 0 {
		dvrByID := make(map[string]CamDVR, len(dvrs))
		for _, d := range dvrs {
			dvrByID[d.ID] = d
		}
		b.WriteString("OPERATIONAL PLAYBOOKS (operator-authored procedures — name — when to use):\n")
		if cat := camPlaybookCatalog(playbooks, dvrByID); cat != "" {
			b.WriteString(cat)
			b.WriteString("\n")
		}
		b.WriteString("When the operator's question matches a playbook, call tool \"playbook\" with args.name set to the EXACT\n")
		b.WriteString("name FIRST — it returns the full step-by-step instructions and any operator-approved reference images.\n")
		b.WriteString("Never improvise a procedure the catalog already covers.\n\n")
	}

	// External API tools catalog — emitted only when the site has any.
	if len(apiTools) > 0 {
		b.WriteString("EXTERNAL API TOOLS (operator-configured HTTP lookups — name — what it does):\n")
		if cat := camAPIToolCatalog(apiTools); cat != "" {
			b.WriteString(cat)
			b.WriteString("\n")
		}
		b.WriteString("Call tool \"call_api\" with args.name and args.params. The response is TEXT for your reasoning —\n")
		b.WriteString("you cannot cite API responses or their URLs as evidence; cite camera media instead.\n\n")
	}

	hasAvatars := len(avatars) > 0

	b.WriteString("TOOLS you may call (action.tool):\n")
	b.WriteString("- roster: re-read the camera roster above as text (no images; use this to double-check exact camera ids).\n")
	b.WriteString("- snapshot: args.camera_ids (required), args.quality \"sub\"|\"main\" (see QUALITY below; default main) — a fresh CURRENT still per camera.\n")
	b.WriteString("- mosaic: args.camera_ids (optional, default = every enabled camera) — one low-res grid of CURRENT sub-stream stills (always sub quality; args.quality is ignored).\n")
	b.WriteString("- contact_sheet: args.camera_ids (ONE id), args.from + args.to (RFC3339, required), args.quality \"sub\"|\"main\" (default sub), args.mode \"motion\"|\"interval\" — scans that ONE camera's 1-fps archive across the window and returns a SINGLE numbered composite image plus a \"cell=time\" map (one image instead of hundreds). CHOOSE THE MODE from the camera's roster description:\n")
	b.WriteString("    * mode \"motion\" (default) DROPS static frames, keeping only moments the scene CHANGED — use for a DOOR/gate/entrance/exit or any mostly-still view: count who entered/passed and when.\n")
	b.WriteString("    * mode \"interval\" is a uniform time-lapse (no motion filter) — use for a BUSY area where people are always moving (reception, waiting room, office) so motion filtering would keep everything and help nothing: count how many people are present over time and see when it was empty.\n")
	b.WriteString("  Then past_frames the specific numbered cells (quality \"main\") to confirm/read detail. Frames of the same person seconds apart are ONE entry, not several.\n")
	b.WriteString("- past_frames: args.camera_ids (required), args.from + args.to (RFC3339, required), args.count (stills per camera, default 6), args.quality \"sub\"|\"main\" (see QUALITY below; default sub) — stills sampled from RECORDED footage; use to SEE a window directly, or to zoom into the exact times a contact_sheet flagged.\n")
	b.WriteString("- past_clip: args.camera_ids (one id used), args.from + args.to (RFC3339, required), args.quality \"sub\"|\"main\" (see QUALITY below) — saves a recorded clip as citable EVIDENCE. You are NOT shown its frames (use past_frames first if you need to see the footage yourself).\n")
	b.WriteString("- motion_search: args.camera_ids (optional, default = all cameras), args.from + args.to (RFC3339, required), args.event_type (optional exact type) — queries the DVR MOTION LOG and returns the exact times motion/line-cross/intrusion episodes occurred, WITHOUT pulling any footage (costs no media budget). Use FIRST to find WHEN activity happened, then past_frames/contact_sheet/avatar_check those exact windows — don't brute-scan.\n")
	b.WriteString("- playbook: args.name (EXACT name from OPERATIONAL PLAYBOOKS) — returns that procedure's full\n")
	b.WriteString("  instructions plus operator-approved reference images (shown to you next turn). Costs no media budget.\n")
	b.WriteString("- call_api: args.name (EXACT name from EXTERNAL API TOOLS), args.params {\"key\":\"value\"} — the loop\n")
	b.WriteString("  performs the operator-configured HTTP request with your params substituted and shows you the text\n")
	b.WriteString("  response next turn. Costs no media budget.\n")
	if hasAvatars {
		b.WriteString("- avatars: (no args) — list the KNOWN AVATARS registered for this site (people/vehicles/pets: id, name, type, description). Use FIRST whenever the question names a specific person (\"employee X\", \"the owner\", \"Dr. Ahmad\").\n")
		b.WriteString("- avatar_info: args.avatar_id — that avatar's full description plus its approved reference images (you SEE them next turn). Always look at the references before judging any frame.\n")
		b.WriteString("- avatar_check: args.avatar_id, args.camera_ids (ONE id), args.time (RFC3339) — pulls the archived frame at that instant and runs a focused identity comparison against the avatar's references; returns the verdict (present/confidence/reason) and, on a match, an ANNOTATED evidence frame with the person circled.\n")
		b.WriteString("- avatar_find: args.avatar_id, args.camera_ids (ONE id), args.from + args.to (RFC3339) — scans that ONE camera's 1-fps archive across the window (motion-filtered) and compares each active moment against the avatar; returns sighting times with confidence plus an annotated sheet. THE tool for \"what did X do today\": call it per relevant camera, then confirm pivotal moments with avatar_check.\n")
		b.WriteString("- annotate: args.camera_ids (ONE id), args.time (RFC3339), args.bbox = {\"x0\",\"y0\",\"x1\",\"y1\"} on a 0-1000 grid (top-left origin) — mints an evidence frame with an ellipse drawn around your box so the operator sees exactly WHO you mean. Annotate the person you cite before answering.\n")
	}
	b.WriteString("\n")

	b.WriteString("ACTIONS (action.type — exactly one per turn):\n")
	b.WriteString("- call_tool: run ONE of the TOOLS above; you see its result next turn.\n")
	b.WriteString("- ask_operator: pause and ask the operator action.question — ONLY for information no tool or playbook can provide.\n")
	b.WriteString("- answer: finish with action.answer plus action.evidence citing exact media_url values from TOOL RESULT lines.\n\n")

	b.WriteString("STRATEGY for counting people/occupancy over a time window: (1) use the ROSTER descriptions to pick the ONE relevant camera AND to judge whether it is a DOOR (mostly still) or a BUSY area (constant movement); (2) contact_sheet that camera with mode \"motion\" for a door or mode \"interval\" for a busy area; (3) read the numbered sheet, count DISTINCT people, and past_frames specific cells for detail/time. Do NOT blindly past_frames a wide multi-camera window — it is slow and misses people.\n\n")
	b.WriteString("STRATEGY for finding WHEN something happened (\"first person to arrive\", \"any activity after hours\"): call motion_search FIRST on the relevant camera(s) and window — it returns the exact episode times cheaply from the motion log — then drill into those precise windows with past_frames/contact_sheet/avatar_check instead of scanning the whole day.\n\n")
	if hasAvatars {
		b.WriteString("STRATEGY for tracking a NAMED person across a day: (1) avatars → find their id; (2) avatar_info to study their references; (3) from the roster pick the entry/exit cameras and their usual areas; (4) avatar_find each such camera across the window to collect sightings; (5) avatar_check the pivotal ones (arrival, departure, anything unusual); (6) answer with a chronological timeline citing the annotated frames.\n\n")
	}

	b.WriteString("QUALITY (args.quality) — choose per request:\n")
	b.WriteString("- \"main\" = full-resolution: use when FINE DETAIL matters — reading text/labels/signage, checking cleanliness or condition, or identifying small objects, faces, or license plates.\n")
	b.WriteString("- \"sub\" = lower-resolution and cheaper: fine for counting people, presence/absence, or general activity/movement.\n")
	b.WriteString("- Default to \"sub\" unless the task needs fine detail; switch to \"main\" only when it does.\n\n")

	b.WriteString("PROTOCOL:\n")
	if len(playbooks) > 0 {
		b.WriteString("- Playbook-first: if an OPERATIONAL PLAYBOOK matches the task, fetch it with the playbook tool before pulling any imagery, and follow its steps.\n")
	}
	b.WriteString("- Roster-first: read the roster above and pick the SMALLEST set of cameras worth pulling imagery from before fetching anything.\n")
	b.WriteString("- Investigate step by step: snapshot/mosaic for \"right now\", past_frames/past_clip for \"what happened at/around <time>\".\n")
	b.WriteString("- Use ask_operator ONLY when you genuinely need information only the operator has (an exact time, a name, which entrance, etc).\n")
	b.WriteString("- Cite evidence in your final answer by COPYING the exact media_url= values shown in the TOOL RESULT lines above — that is how the operator sees the proof frames/clips. Always attach the frames your answer relies on (e.g. one per person you counted).\n")
	b.WriteString("- Be economical: stop investigating and answer as soon as you can do so confidently.\n\n")

	b.WriteString("OUTPUT: reply with EXACTLY ONE JSON object and nothing else (no prose, no markdown fences):\n")
	b.WriteString(`{"thought":"...","action":{"type":"call_tool|ask_operator|answer",`)
	b.WriteString("\n")
	b.WriteString(`  "tool":"roster|snapshot|mosaic|contact_sheet|past_frames|past_clip|motion_search|playbook|call_api|avatars|avatar_info|avatar_check|avatar_find|annotate","args":{"camera_ids":["<id>"],"quality":"sub|main","from":"RFC3339","to":"RFC3339","count":6,"event_type":"<motion type>","name":"<playbook or api tool name>","params":{"key":"value"},"avatar_id":"<avatar id>","time":"RFC3339","bbox":{"x0":0,"y0":0,"x1":1000,"y1":1000}},`)
	b.WriteString("\n")
	b.WriteString(`  "question":"...(ask_operator)","answer":"...(final narrative)","evidence":[{"media_url":"...","caption":"..."}]}}`)
	b.WriteString("\nUse ONLY the EXACT camera ids from the roster above. Reply with ONLY the JSON object.")
	return b.String()
}

// ─────────────────────────────── action parsing ───────────────────────────────

// camInvestigateActionTypes is the set of valid investigateCommand.Type values.
var camInvestigateActionTypes = map[string]bool{"call_tool": true, "ask_operator": true, "answer": true}

// parseInvestigateAction extracts and decodes one investigateAction from a
// model's raw text, then normalizes it so a garbled reply can never crash or
// stall the loop: an unrecognized action type falls back to "call_tool roster"
// (cheap, no device I/O, gives the model another turn to recover) rather than
// silently ending the investigation; an unrecognized tool name for an otherwise
// valid call_tool also falls back to "roster"; an "answer" with no answer text
// falls back to the turn's thought (or a generic notice) so the transcript is
// never blank.
func parseInvestigateAction(raw string) (investigateAction, error) {
	obj, ok := extractFirstJSONObject(raw)
	if !ok {
		return investigateAction{}, errors.New("no JSON object found in model output")
	}
	var act investigateAction
	if err := json.Unmarshal(obj, &act); err != nil {
		return investigateAction{}, fmt.Errorf("decode investigation action JSON: %w", err)
	}
	act.Action.Type = camNormalizeEnum(act.Action.Type, camInvestigateActionTypes, "call_tool")
	switch act.Action.Type {
	case "call_tool":
		tool := strings.ToLower(strings.TrimSpace(act.Action.Tool))
		valid := false
		for _, t := range investigateToolNames {
			if t == tool {
				valid = true
				break
			}
		}
		if !valid {
			tool = "roster"
		}
		act.Action.Tool = tool
	case "answer":
		if strings.TrimSpace(act.Action.Answer) == "" {
			act.Action.Answer = firstNonEmpty(strings.TrimSpace(act.Thought), "The investigation could not produce a conclusive answer.")
		}
	}
	return act, nil
}

// camAnalyzeInvestigateAction runs one investigation turn through analyzeWithAlias
// and parses the single JSON action, doing exactly ONE repair round on a parse
// failure — mirrors camAnalyzeDecision (camera_escalate.go).
func camAnalyzeInvestigateAction(ctx context.Context, cfg config, alias, sys, user string, images []string) (investigateAction, string, bool, error) {
	raw, err := analyzeWithAlias(ctx, cfg, alias, sys, user, images)
	if err != nil {
		return investigateAction{}, raw, false, err
	}
	act, perr := parseInvestigateAction(raw)
	if perr == nil {
		return act, raw, false, nil
	}
	camlog("warn", "investigate_parse", map[string]any{"ok": false, "attempt": 1, "error": perr.Error()})

	repair := user + "\n\nYour previous reply could not be parsed. Reply with ONLY the single JSON object " +
		"described above — no prose, no markdown fences, nothing else."
	raw2, err2 := analyzeWithAlias(ctx, cfg, alias, sys, repair, images)
	if err2 != nil {
		return investigateAction{}, raw2, true, err2
	}
	act2, perr2 := parseInvestigateAction(raw2)
	if perr2 != nil {
		camlog("warn", "investigate_parse", map[string]any{"ok": false, "attempt": 2, "error": perr2.Error()})
		return investigateAction{}, raw2, true, fmt.Errorf("could not parse investigation action JSON after repair: %w", perr2)
	}
	return act2, raw2, true, nil
}

// ─────────────────────────────── transcript ───────────────────────────────

// camInvestigateTranscriptText renders the persisted message history into the
// compact running log the model reads as its memory each turn: one line per
// message. Only the LAST tool's images are ever attached alongside this text
// (see runInvestigation) — resending every prior image every turn would make
// the loop unboundedly expensive, exactly the "compact running transcript"
// the plan calls for.
func camInvestigateTranscriptText(msgs []camInvestigationMessage) string {
	var b strings.Builder
	for _, m := range msgs {
		switch m.Role {
		case "operator":
			b.WriteString("OPERATOR: ")
			b.WriteString(strings.TrimSpace(m.Content))
			b.WriteString("\n")
		case "ai":
			b.WriteString("AI: ")
			b.WriteString(strings.TrimSpace(m.Content))
			b.WriteString("\n")
			if m.ToolName != "" {
				b.WriteString("  action=")
				b.WriteString(m.ToolName)
				if args := strings.TrimSpace(m.ToolArgs); args != "" && args != "{}" {
					b.WriteString(" args=")
					b.WriteString(args)
				}
				b.WriteString("\n")
			}
		case "tool":
			b.WriteString("TOOL RESULT (")
			b.WriteString(m.ToolName)
			b.WriteString("): ")
			b.WriteString(strings.TrimSpace(m.Content))
			b.WriteString("\n")
			// Expose the EXACT media_url of every artifact this tool minted so the
			// model can cite it verbatim as evidence. The final answer's evidence is
			// validated against these exact strings, so if we never show them here the
			// model can only guess URLs and every citation gets dropped (kept=0).
			if raw := strings.TrimSpace(m.MediaJSON); raw != "" && raw != "null" && raw != "[]" {
				var media []evidenceItem
				if json.Unmarshal([]byte(raw), &media) == nil {
					for _, it := range media {
						if u := strings.TrimSpace(it.MediaURL); u != "" {
							b.WriteString("    evidence media_url=")
							b.WriteString(u)
							if capt := strings.TrimSpace(it.Caption); capt != "" {
								b.WriteString("  (")
								b.WriteString(capt)
								b.WriteString(")")
							}
							b.WriteString("\n")
						}
					}
				}
			}
		case "system":
			b.WriteString("SYSTEM: ")
			b.WriteString(strings.TrimSpace(m.Content))
			b.WriteString("\n")
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// camAIMessageContent joins a turn's reasoning with its user-facing text (a
// question, a final answer, ...) for a single readable transcript row.
func camAIMessageContent(thought, extra string) string {
	thought = strings.TrimSpace(thought)
	extra = strings.TrimSpace(extra)
	switch {
	case thought == "":
		return extra
	case extra == "":
		return thought
	default:
		return thought + "\n\n" + extra
	}
}

// camAppendInvestigateMessage persists one transcript row (auto-assigning seq)
// and returns it (with its new id) so the caller can append it to its in-memory
// transcript slice without re-querying the database. fetches records the DVR
// device-fetch count for tool rows (0 elsewhere) so the media budget survives a
// pause/resume in the same unit the live loop spends it. A persistence failure is
// logged but never aborts the loop — the model still gets to see the row via
// the in-memory slice this call.
func camAppendInvestigateMessage(db *sql.DB, invID, role, content, toolName, toolArgs string, fetches int, media []evidenceItem) camInvestigationMessage {
	m := camInvestigationMessage{
		InvestigationID: invID, Role: role, Content: content,
		ToolName: toolName, ToolArgs: toolArgs, MediaJSON: mustJSON(media), Fetches: fetches,
	}
	id, err := appendCameraInvestigationMessage(db, m)
	if err != nil {
		camlog("warn", "investigate_persist", map[string]any{"investigation_id": invID, "role": role, "ok": false, "error": err.Error()})
		return m
	}
	m.ID = id
	return m
}

// camCountInvestigateProgress reconstructs the turn/media counts for the CURRENT
// run from an investigation's persisted transcript, so bounds hold across resumed
// calls in the exact same units the live loop spends them.
//
// Budget is cumulative WITHIN one run, and a run pauses ONLY on ask_operator. An
// operator reply that answers an ask_operator pause therefore continues the same
// run — its prior turns/media carry over, so a model can NEVER reset its budget
// by looping on ask_operator. Any OTHER operator message (the opening question,
// or a follow-up sent after the run already terminated via answer / bound / error)
// begins a FRESH run with a fresh budget: those are human-initiated — the model
// can't emit operator messages — and each new run is still independently bounded,
// so this never permits an unbounded model loop. This is what lets a follow-up
// after a bound-terminated ("exhausted") run actually run the model again instead
// of instantly re-tripping the cumulative turn cap.
//
// Media is summed from each tool row's persisted device-fetch count (identical to
// the live mediaUsed += tr.Fetches), NOT from the count of persisted media
// artifacts — the latter diverges from the live spend (past_frames: one clip
// fetch -> many frames; mosaic: many snapshot fetches -> one composite), which
// would make a resumed investigation enforce the cap against a different number.
func camCountInvestigateProgress(msgs []camInvestigationMessage) (turns, mediaUsed int) {
	lastAITool := ""
	for _, m := range msgs {
		switch m.Role {
		case "operator":
			if lastAITool != "ask_operator" {
				turns, mediaUsed = 0, 0
			}
			lastAITool = ""
		case "ai":
			turns++
			lastAITool = strings.TrimSpace(m.ToolName)
		case "tool":
			mediaUsed += m.Fetches
		}
	}
	return turns, mediaUsed
}

// camStopInvestigation persists a system note explaining why the loop ended
// without a model-produced answer (a turn/media/time bound, or an unrecoverable
// analysis error) and marks the investigation terminal as "exhausted" — a status
// distinct from a clean model "answer" so the UI can label it honestly and so a
// later operator follow-up is treated as a fresh run (camCountInvestigateProgress
// resets the cumulative budget on a non-ask_operator operator message) rather than
// instantly re-tripping the same cumulative turn cap and re-stopping. Still
// re-openable: handleCameraInvestigationReply reopens anything that isn't "closed".
// Every exhausted transition (incl. the panic-terminalize path, which funnels
// through here) fires the settle webhook so Connect learns the run ended.
func camStopInvestigation(db *sql.DB, cfg config, invID, note string) error {
	camAppendInvestigateMessage(db, invID, "system", note, "", "", 0, nil)
	err := setCameraInvestigationStatus(db, invID, "exhausted")
	go camNotifyInvestigationSettled(cfg, invID, "exhausted")
	return err
}

// camCollectMintedMedia returns the /camera/media/<token> URLs the loop's tools
// actually minted across an investigation's WHOLE transcript (every tool row's
// persisted media_json, so evidence from a pre-ask_operator pause or an earlier
// follow-up round still counts): both the exact-string set and a token->canonical-URL
// index. The final answer's cited evidence is validated against these so a
// hallucinated media_url is dropped, while one that differs only in host/formatting
// but carries a real token is rewritten to the canonical served URL.
func camCollectMintedMedia(msgs []camInvestigationMessage) (set map[string]bool, byToken map[string]string) {
	set = make(map[string]bool)
	byToken = make(map[string]string)
	for _, m := range msgs {
		if m.Role != "tool" || strings.TrimSpace(m.MediaJSON) == "" {
			continue
		}
		var media []evidenceItem
		if json.Unmarshal([]byte(m.MediaJSON), &media) != nil {
			continue
		}
		for _, it := range media {
			if u := strings.TrimSpace(it.MediaURL); u != "" {
				set[u] = true
				if tok := camMediaToken(u); tok != "" {
					byToken[tok] = u
				}
			}
		}
	}
	return set, byToken
}

// camMediaToken extracts the capability token (last path segment, minus any query)
// from a /camera/media/<token> URL, so a cited evidence URL that differs only in
// scheme/host/formatting can still be matched back to the exact artifact minted.
func camMediaToken(u string) string {
	u = strings.TrimSpace(u)
	if i := strings.IndexAny(u, "?#"); i >= 0 {
		u = u[:i]
	}
	if i := strings.LastIndex(u, "/"); i >= 0 {
		u = u[i+1:]
	}
	return u
}

// camFilterEvidence keeps only cited evidence whose media_url was actually minted
// by a tool this investigation (present in allowed), dropping any the model
// invented or mistyped. Order and captions are preserved; the drop count is
// returned for logging. Enforces the "cite the exact media_url values already
// returned to you" contract without ever fabricating a URL of our own.
func camFilterEvidence(evidence []evidenceItem, allowed map[string]bool, byToken map[string]string) (kept []evidenceItem, dropped int) {
	seen := make(map[string]bool)
	for _, e := range evidence {
		u := strings.TrimSpace(e.MediaURL)
		canonical := ""
		switch {
		case allowed[u]:
			canonical = u
		default:
			// Cited URL isn't an exact minted string — accept it only if its
			// capability token matches one we minted, and rewrite to the canonical
			// served URL so the UI always renders a working link.
			if c, ok := byToken[camMediaToken(u)]; ok {
				canonical = c
			}
		}
		if canonical == "" {
			dropped++
			continue
		}
		if seen[canonical] {
			continue // de-dup repeated citations of the same artifact
		}
		seen[canonical] = true
		kept = append(kept, evidenceItem{MediaURL: canonical, Caption: e.Caption})
	}
	return kept, dropped
}

// camSiteTimezone resolves a representative timezone for the investigation
// prompt from the site's DVRs (the store's per-DVR Timezone field — e.g.
// Asia/Amman), falling back to the server's local zone when none is set or
// loadable. Sites with multiple DVRs in different timezones are rare (CCTV
// hardware is physically on-prem), so the first usable zone wins.
func camSiteTimezone(dvrs []CamDVR) (*time.Location, string) {
	for _, d := range dvrs {
		tz := strings.TrimSpace(d.Timezone)
		if tz == "" {
			continue
		}
		if loc, err := time.LoadLocation(tz); err == nil {
			return loc, tz
		}
	}
	return time.Local, "Local"
}

// ─────────────────────────────── tools ───────────────────────────────

// investigateToolResult is what one tool execution produced: a compact text
// summary folded into the transcript, the served-media refs to persist in the
// tool message's media_json (and for the model to cite as evidence later), the
// local image paths to feed the model on its NEXT turn (nil for roster and
// past_clip, which are never themselves visually analyzed), and how many DVR
// device fetches it cost (toward cfg.CameraInvestigateMaxMedia).
type investigateToolResult struct {
	Summary string
	Media   []evidenceItem
	Images  []string
	Fetches int
}

// camExecuteInvestigateTool dispatches one call_tool action to its
// implementation. tool is assumed already normalized by parseInvestigateAction
// (one of investigateToolNames); the default case is defensive. alias is the
// run's analysis alias, threaded to the tools that make their own VLM sub-calls
// (avatar_check / avatar_find).
func camExecuteInvestigateTool(ctx context.Context, cfg config, db *sql.DB, r *http.Request, site camSite, alias, tool string, args investigateArgs, camByID map[string]camera, dvrByID map[string]CamDVR, allowed map[string]bool, active []camera, scratch string, mediaLeft int) investigateToolResult {
	switch tool {
	case "roster":
		return investigateToolResult{Summary: "Camera roster:\n" + camBuildRoster(active)}
	case "snapshot":
		return camToolSnapshot(ctx, cfg, db, r, site, args, camByID, dvrByID, allowed, scratch, mediaLeft)
	case "mosaic":
		return camToolMosaic(ctx, cfg, db, r, site, args, camByID, dvrByID, allowed, active, scratch, mediaLeft)
	case "contact_sheet":
		return camToolContactSheet(ctx, cfg, db, r, site, args, camByID, dvrByID, allowed, scratch, mediaLeft)
	case "past_frames":
		return camToolPastFrames(ctx, cfg, db, r, site, args, camByID, dvrByID, allowed, scratch, mediaLeft)
	case "past_clip":
		return camToolPastClip(ctx, cfg, db, r, site, args, camByID, dvrByID, allowed, scratch, mediaLeft)
	case "motion_search":
		return camToolMotionSearch(cfg, db, site, args, allowed, camByID, dvrByID)
	case "playbook":
		return camToolPlaybook(ctx, cfg, db, r, site, args, scratch)
	case "call_api":
		return camToolCallAPI(ctx, cfg, db, site, args)
	case "avatars":
		return camToolAvatars(db, site, active)
	case "avatar_info":
		return camToolAvatarInfo(cfg, db, site, args, scratch)
	case "avatar_check":
		return camToolAvatarCheck(ctx, cfg, db, r, site, alias, args, camByID, dvrByID, allowed, scratch, mediaLeft)
	case "avatar_find":
		return camToolAvatarFind(ctx, cfg, db, r, site, alias, args, camByID, dvrByID, allowed, scratch, mediaLeft)
	case "annotate":
		return camToolAnnotate(ctx, cfg, db, r, site, args, camByID, allowed, scratch)
	default:
		return investigateToolResult{Summary: fmt.Sprintf("unknown tool %q — no action taken.", tool)}
	}
}

// camToolSnapshot captures a fresh CURRENT still per requested camera (default
// MAIN stream; "sub" opts into the cheaper stream), persists each as a served
// snapshot capture, and returns the local paths for the model's next turn.
func camToolSnapshot(ctx context.Context, cfg config, db *sql.DB, r *http.Request, site camSite, args investigateArgs, camByID map[string]camera, dvrByID map[string]CamDVR, allowed map[string]bool, scratch string, mediaLeft int) investigateToolResult {
	ids := camFilterAllowed(args.CameraIDs, allowed)
	if len(ids) == 0 {
		return investigateToolResult{Summary: "snapshot: no valid camera_ids given (must be exact ids from the roster)."}
	}
	if max := camMosaicMax(cfg); len(ids) > max {
		ids = ids[:max]
	}
	q := StreamMain
	if strings.EqualFold(strings.TrimSpace(args.Quality), "sub") {
		q = StreamSub
	}
	var (
		images  []string
		media   []evidenceItem
		fetches int
	)
	for _, id := range ids {
		if fetches >= mediaLeft {
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
		dest := filepath.Join(scratch, fmt.Sprintf("snap_%s_%d.jpg", id, time.Now().UnixNano()))
		res, cerr := captureSnapshot(ctx, cfg, dv, c.Channel, q, dest)
		fetches++
		if cerr != nil {
			continue // captureSnapshot already camlog'd the masked command + stderr
		}
		if token, perr := camPersistCapture(db, cfg, "", site.ID, id, "snapshot", q.String(), res.Path, res.ContentType, res.Width, res.Height, "", ""); perr == nil {
			media = append(media, evidenceItem{MediaURL: camMediaURL(cfg, r, token), Caption: "current " + q.String() + " snapshot — " + camDisplayName(c)})
		}
		images = append(images, res.Path)
	}
	if len(images) == 0 {
		return investigateToolResult{Summary: "snapshot: no image could be captured for " + camIDsWithNames(ids, camByID) + ".", Fetches: fetches}
	}
	return investigateToolResult{
		Summary: fmt.Sprintf("Captured %d current %s snapshot(s) for %s.", len(images), q.String(), camIDsWithNames(ids, camByID)),
		Media:   media, Images: images, Fetches: fetches,
	}
}

// camToolMosaic captures a SUB-stream snapshot per requested camera (or every
// enabled camera when camera_ids is empty), tiles them via buildMosaic (the
// same low-cost composite the escalation loop uses), persists the composite,
// and returns its single image path for the model's next turn.
func camToolMosaic(ctx context.Context, cfg config, db *sql.DB, r *http.Request, site camSite, args investigateArgs, camByID map[string]camera, dvrByID map[string]CamDVR, allowed map[string]bool, active []camera, scratch string, mediaLeft int) investigateToolResult {
	ids := camFilterAllowed(args.CameraIDs, allowed)
	var targets []camera
	if len(ids) > 0 {
		for _, id := range ids {
			if c, ok := camByID[id]; ok {
				targets = append(targets, c)
			}
		}
	} else {
		targets = active
	}
	if max := camMosaicMax(cfg); len(targets) > max {
		targets = targets[:max]
	}

	items := make([]mosaicItem, 0, len(targets))
	fetches := 0
	for _, c := range targets {
		if fetches >= mediaLeft {
			break
		}
		dv, ok := dvrByID[c.DVRID]
		if !ok || !dv.Enabled {
			continue
		}
		dest := filepath.Join(scratch, fmt.Sprintf("mosaic_%s_%d.jpg", c.ID, time.Now().UnixNano()))
		res, cerr := captureSnapshot(ctx, cfg, dv, c.Channel, StreamSub, dest)
		fetches++
		if cerr != nil {
			continue
		}
		items = append(items, mosaicItem{Index: len(items) + 1, CameraID: c.ID, Name: camDisplayName(c), Path: res.Path})
	}
	if len(items) == 0 {
		return investigateToolResult{Summary: "mosaic: no camera snapshots could be captured.", Fetches: fetches}
	}
	m, merr := buildMosaic(cfg, items)
	if merr != nil {
		return investigateToolResult{Summary: "mosaic: " + merr.Error(), Fetches: fetches}
	}
	mosaicPath := filepath.Join(scratch, fmt.Sprintf("mosaic_%d.png", time.Now().UnixNano()))
	if err := os.WriteFile(mosaicPath, m.PNG, 0o600); err != nil {
		return investigateToolResult{Summary: "mosaic: " + err.Error(), Fetches: fetches}
	}
	var media []evidenceItem
	if token, perr := camPersistCapture(db, cfg, "", site.ID, "", "mosaic", "sub", mosaicPath, "image/png", m.Width, m.Height, "", ""); perr == nil {
		media = append(media, evidenceItem{MediaURL: camMediaURL(cfg, r, token), Caption: fmt.Sprintf("current low-res mosaic (%d cameras)", len(items))})
	}
	var legend strings.Builder
	for _, e := range m.Legend {
		fmt.Fprintf(&legend, "  %d => %s", e.Index, e.CameraID)
		if e.Name != "" {
			fmt.Fprintf(&legend, " (%s)", e.Name)
		}
		legend.WriteString("\n")
	}
	return investigateToolResult{
		Summary: fmt.Sprintf("Captured a current low-res mosaic of %d camera(s). Tile legend:\n%s", len(items), strings.TrimRight(legend.String(), "\n")),
		Media:   media, Images: []string{mosaicPath}, Fetches: fetches,
	}
}

// camToolPastFrames samples up to args.Count recorded stills spread across
// [from,to] for each requested camera (preferring the SUB stream — main is slow
// for retrospective pulls, per the plan; "main" can be requested explicitly),
// persists each as a served frame capture, and returns all sampled paths for the
// model's next turn.
//
// S3 READ-THROUGH (WP1): when camS3Enabled(cfg), each sample timestamp is first
// snapped to the archive grid (camS3SnapArchiveTime — nearest multiple of the
// background archiver's interval) and looked up in object storage
// (s3GetObject at camS3FrameKey(id, quality, t)). A HIT is written to a local
// temp .jpg and used directly — fast, no DVR round-trip. A MISS (errS3NotFound)
// falls back to the live DVR pull (captureFrameAtTime) exactly as before, and on
// success the frame is ALSO uploaded (s3PutObject) under the same key so the next
// query for that instant is a hit — the read-through's own back-fill, plus grid
// snapping, makes repeated/overlapping investigations converge on the same keys.
// With S3 disabled the behaviour is byte-for-byte the pre-WP1 DVR path. Every
// frame is camlog'd with its source (s3|dvr). An S3 hit still counts as one media
// artifact against the fetch budget (logged as a cheap fetch).
func camToolPastFrames(ctx context.Context, cfg config, db *sql.DB, r *http.Request, site camSite, args investigateArgs, camByID map[string]camera, dvrByID map[string]CamDVR, allowed map[string]bool, scratch string, mediaLeft int) investigateToolResult {
	ids := camFilterAllowed(args.CameraIDs, allowed)
	if len(ids) == 0 {
		return investigateToolResult{Summary: "past_frames: no valid camera_ids given (must be exact ids from the roster)."}
	}
	from, to, terr := camParseWindow(args.From, args.To, cfg, false)
	if terr != nil {
		return investigateToolResult{Summary: "past_frames: " + terr.Error()}
	}
	count := int(args.Count)
	if count <= 0 {
		count = 6
	}
	if count > 12 {
		count = 12
	}
	if max := camMosaicMax(cfg); len(ids) > max {
		ids = ids[:max]
	}
	q := StreamSub
	if strings.EqualFold(strings.TrimSpace(args.Quality), "main") {
		q = StreamMain
	}
	qStr := q.String()
	s3Enabled := camS3Enabled(cfg)
	// Snap read-through lookups to the SAME grid the archiver wrote on. Main
	// frames are archived on the slower main cadence, so main lookups must snap
	// to the main grid — snapping them to the sub grid would target instants
	// where no main frame was ever written (defaults mirror the archiver: 15s
	// sub / 30s main).
	snapInterval := cfg.CameraArchiveInterval
	if snapInterval <= 0 {
		snapInterval = 15 * time.Second
	}
	if q == StreamMain {
		snapInterval = cfg.CameraArchiveMainInterval
		if snapInterval <= 0 {
			snapInterval = 30 * time.Second
		}
	}
	// s3Down is a per-request circuit breaker: the first transient S3 error
	// (timeout/blackhole/5xx) trips it and every remaining sample skips S3 and
	// goes straight to the DVR, so an S3 outage degrades to plain DVR latency
	// instead of paying the client timeout once per sampled instant per camera.
	s3Down := false

	// Sample `count` INSTANTS spread across [from,to] and grab one fast frame at
	// each (captureFrameAtTime) instead of downloading the whole window as a
	// real-time clip. This keeps a wide "what happened between X and Y" pull quick,
	// and — because every still is captioned with its exact time — makes it
	// actually useful for locating WHEN something happened. Instants with no
	// recording are skipped, so a partial-coverage window still returns what exists.
	times := camSampleTimes(from, to, count)

	var (
		images  []string
		media   []evidenceItem
		fetches int
		usedIDs []string
		missing int
	)
	for _, id := range ids {
		c, ok := camByID[id]
		if !ok {
			continue
		}
		dv, ok := dvrByID[c.DVRID]
		if !ok || !dv.Enabled {
			continue
		}
		loc := time.Local
		if dv.Timezone != "" {
			if l, e := time.LoadLocation(dv.Timezone); e == nil {
				loc = l
			}
		}
		got := 0
		for i, t := range times {
			if fetches >= mediaLeft {
				break
			}
			// Snap to the archive grid when S3 is on so the lookup key (and any
			// back-fill) lines up with the archiver and with other investigations.
			ft := t
			if s3Enabled {
				ft = camS3SnapArchiveTime(t, snapInterval)
			}
			dest := filepath.Join(scratch, fmt.Sprintf("pf_%s_%02d_%d.jpg", id, i, time.Now().UnixNano()))

			var (
				res    captureResult
				source string
				ok     bool
				s3Miss bool
				s3Key  string
			)
			// 1) S3 read-through: try object storage first (fast, no DVR). Skip
			// entirely once the per-request circuit breaker has tripped.
			if s3Enabled && !s3Down {
				s3Key = camS3FrameKey(id, qStr, ft)
				data, gerr := s3GetObject(ctx, cfg, s3Key)
				switch {
				case gerr == nil:
					if werr := os.WriteFile(dest, data, 0o600); werr == nil {
						w, h := imageDimensions(dest)
						res = captureResult{Path: dest, ContentType: "image/jpeg", Bytes: int64(len(data)), Width: w, Height: h, Method: "s3"}
						source, ok = "s3", true
					}
				case errors.Is(gerr, errS3NotFound):
					s3Miss = true // genuine miss — fall back to the DVR and back-fill below
				default:
					// Transient S3 error (timeout/blackhole/5xx): trip the circuit
					// breaker so the rest of this request skips S3, then fall back
					// to the DVR for this sample (don't re-upload).
					s3Down = true
					camlog("warn", "investigate_frame_s3", map[string]any{
						"camera_id": id, "quality": qStr, "key": s3Key, "op": "get",
						"error": gerr.Error(), "circuit_open": true,
					})
				}
			}
			// 2) MISS (or S3 off): pull the frame live from the DVR as before.
			if !ok {
				r2, cerr := captureFrameAtTime(ctx, cfg, dv, c.Channel, q, ft, dest)
				if cerr == nil && r2.Path != "" {
					res, source, ok = r2, "dvr", true
					// Back-fill S3 on a genuine miss so the next query is a hit.
					if s3Enabled && s3Miss {
						if body, rerr := os.ReadFile(r2.Path); rerr == nil {
							if perr := s3PutObject(ctx, cfg, s3Key, body, "image/jpeg"); perr != nil {
								camlog("warn", "investigate_frame_s3", map[string]any{
									"camera_id": id, "quality": qStr, "key": s3Key, "op": "put", "error": perr.Error(),
								})
							}
						}
					}
				}
			}
			fetches++ // one media artifact per sampled instant (an S3 hit is a cheap fetch)
			if !ok {
				missing++
				continue // no footage at this instant — skip it, keep sampling the rest
			}
			got++
			camlog("debug", "investigate_frame", map[string]any{
				"camera_id": id, "quality": qStr, "source": source,
				"time": ft.Format(time.RFC3339), "s3": s3Enabled,
			})
			ts := ft.Format(time.RFC3339)
			if token, perr := camPersistCapture(db, cfg, "", site.ID, id, "frame", qStr, res.Path, "image/jpeg", res.Width, res.Height, ts, ts); perr == nil {
				media = append(media, evidenceItem{MediaURL: camMediaURL(cfg, r, token), Caption: fmt.Sprintf("%s @ %s", camDisplayName(c), ft.In(loc).Format("Jan 2 15:04:05"))})
			}
			images = append(images, res.Path)
		}
		if got > 0 {
			usedIDs = append(usedIDs, id)
		}
	}
	if len(images) == 0 {
		return investigateToolResult{Summary: fmt.Sprintf("past_frames: no recorded footage found for %s at any of %d instants across %s.", camIDsWithNames(ids, camByID), len(times), windowLabel(from, to)), Fetches: fetches}
	}
	summary := fmt.Sprintf("Sampled %d still(s) at instants spread across %s for %s — each image caption gives its exact time.", len(images), windowLabel(from, to), camIDsWithNames(usedIDs, camByID))
	if missing > 0 {
		summary += fmt.Sprintf(" %d sampled instant(s) had no recording.", missing)
	}
	return investigateToolResult{
		Summary: summary,
		Media:   media, Images: images, Fetches: fetches,
	}
}

// camSampleTimes returns count timestamps spread evenly across [from,to] inclusive
// (both endpoints included when count>1), used by past_frames to sample instants
// across an operator's search window. Degenerate windows collapse to a single time.
func camSampleTimes(from, to time.Time, count int) []time.Time {
	if count < 1 {
		count = 1
	}
	if count == 1 || !to.After(from) {
		return []time.Time{from}
	}
	span := int64(to.Sub(from))
	out := make([]time.Time, count)
	for i := 0; i < count; i++ {
		out[i] = from.Add(time.Duration(span * int64(i) / int64(count-1)))
	}
	return out
}

// camS3SnapArchiveTime rounds t to the nearest instant on the background
// archiver's grid (a multiple of its sub-stream interval, camS3FrameKey being
// second-granular) so a read-through lookup lands on the SAME object key the
// archiver wrote — and so two investigations sampling the same window snap to
// identical keys, letting each other's put-on-miss back-fills hit. Rounding is
// on the absolute (UTC) instant, matching camS3FrameKey's t.UTC() layout; it
// falls back to a 15s grid when the interval is unset. Only used when S3 is on.
func camS3SnapArchiveTime(t time.Time, interval time.Duration) time.Time {
	if interval <= 0 {
		interval = 15 * time.Second
	}
	return t.Round(interval)
}

// camToolPastClip saves a single recorded-footage clip as citable evidence
// (persisted via camera_captures, served at /camera/media/<token>) WITHOUT
// sampling/analyzing its frames — use past_frames first if the model needs to
// actually see the footage. Only the first requested camera_id is used.
func camToolPastClip(ctx context.Context, cfg config, db *sql.DB, r *http.Request, site camSite, args investigateArgs, camByID map[string]camera, dvrByID map[string]CamDVR, allowed map[string]bool, scratch string, mediaLeft int) investigateToolResult {
	ids := camFilterAllowed(args.CameraIDs, allowed)
	if len(ids) == 0 {
		return investigateToolResult{Summary: "past_clip: no valid camera_ids given (must be an exact id from the roster)."}
	}
	if mediaLeft <= 0 {
		return investigateToolResult{Summary: "past_clip: media fetch budget for this investigation is exhausted."}
	}
	id := ids[0]
	c, ok := camByID[id]
	if !ok {
		return investigateToolResult{Summary: "past_clip: unknown camera " + id}
	}
	dv, ok := dvrByID[c.DVRID]
	if !ok || !dv.Enabled {
		return investigateToolResult{Summary: "past_clip: camera " + id + " has no enabled DVR."}
	}
	from, to, terr := camParseWindow(args.From, args.To, cfg, true)
	if terr != nil {
		return investigateToolResult{Summary: "past_clip: " + terr.Error()}
	}
	q := StreamMain
	if strings.EqualFold(strings.TrimSpace(args.Quality), "sub") {
		q = StreamSub
	}
	dest := filepath.Join(scratch, fmt.Sprintf("pastclip_%s_%d.mp4", id, time.Now().UnixNano()))
	res, cerr := capturePlaybackClip(ctx, cfg, dv, c.Channel, q, from, to, dest)
	if cerr != nil {
		if errors.Is(cerr, errNoRecording) {
			return investigateToolResult{Summary: fmt.Sprintf("past_clip: no recording found for %s in %s.", camDisplayName(c), windowLabel(from, to)), Fetches: 1}
		}
		return investigateToolResult{Summary: "past_clip: " + cerr.Error(), Fetches: 1}
	}
	token, perr := camPersistCapture(db, cfg, "", site.ID, id, "clip", q.String(), res.Path, "video/mp4", 0, 0, from.Format(time.RFC3339), to.Format(time.RFC3339))
	if perr != nil {
		return investigateToolResult{Summary: "past_clip: captured but could not be saved: " + perr.Error(), Fetches: 1}
	}
	// Best-effort: also upload the clip to public object storage so it can be
	// delivered as a direct link (large videos exceed chat inline-media limits).
	s3url := camUploadClipS3(ctx, cfg, db, token, site.ID, id, res.Path)
	// The evidence media_url stays the token URL so settle can resolve the capture;
	// the cited link prefers the direct S3 URL when the upload succeeded.
	mediaURL := camMediaURL(cfg, r, token)
	cite := mediaURL
	if s3url != "" {
		cite = s3url
	}
	return investigateToolResult{
		Summary: fmt.Sprintf("Saved a recorded clip for %s covering %s as evidence (direct video link): %s", camDisplayName(c), windowLabel(from, to), cite),
		Media:   []evidenceItem{{MediaURL: mediaURL, Caption: camDisplayName(c) + " — " + windowLabel(from, to)}},
		Fetches: 1,
	}
}

// camUploadClipS3 best-effort uploads a recorded clip to public object storage and
// records the resulting public URL on the capture, so evidence can be delivered as
// a direct link. Returns the public URL, or "" when S3 is disabled or the upload
// failed (the proxy-served URL then remains the only link). Never fatal.
func camUploadClipS3(ctx context.Context, cfg config, db *sql.DB, token, siteID, cameraID, localPath string) string {
	if !camS3Enabled(cfg) {
		return ""
	}
	data, err := os.ReadFile(localPath)
	if err != nil {
		camlog("warn", "clip_s3", map[string]any{"token": token, "ok": false, "error": err.Error()})
		return ""
	}
	key := camS3ClipKey(siteID, cameraID, time.Now())
	if err := s3PutObjectPublic(ctx, cfg, key, data, "video/mp4"); err != nil {
		camlog("warn", "clip_s3", map[string]any{"token": token, "ok": false, "error": err.Error()})
		return ""
	}
	url := camS3PublicURL(cfg, key)
	if err := setCameraCaptureS3URL(db, token, url); err != nil {
		// The object is already public; still return the URL so this run can cite it.
		camlog("warn", "clip_s3", map[string]any{"token": token, "ok": false, "error": "persist url: " + err.Error()})
	}
	camlog("info", "clip_s3", map[string]any{"site_id": siteID, "camera_id": cameraID, "ok": true, "bytes": len(data)})
	return url
}

// camToolMotionSearch queries the coalesced DVR motion history
// (camera_motion_events) for a [from,to] window and returns episode times as
// TEXT — no imagery, no media budget (Fetches:0). It is the cheap "when did
// activity happen" lookup the model runs BEFORE brute-scanning footage: the
// results tell it exactly which windows to drill into with
// past_frames/contact_sheet/avatar_check. camera_ids is optional (default = all
// enabled cameras); event_type optionally narrows to one type. Episode times are
// rendered in the site timezone.
func camToolMotionSearch(cfg config, db *sql.DB, site camSite, args investigateArgs, allowed map[string]bool, camByID map[string]camera, dvrByID map[string]CamDVR) investigateToolResult {
	explicit := len(args.CameraIDs) > 0
	ids := camFilterAllowed(args.CameraIDs, allowed)
	if explicit && len(ids) == 0 {
		return investigateToolResult{Summary: "motion_search: no valid camera_ids given (must be exact ids from the roster; omit to search all cameras)."}
	}
	if len(ids) == 0 {
		// Default = the cameras THIS investigation may see (enabled cameras on
		// enabled DVRs), never every row in the site — mirror the allowed-set
		// discipline every other tool enforces so a disabled camera can't leak.
		for id := range allowed {
			ids = append(ids, id)
		}
		if len(ids) == 0 {
			return investigateToolResult{Summary: "motion_search: no enabled cameras available to search."}
		}
	}
	from, to, terr := camParseWindow(args.From, args.To, cfg, false)
	if terr != nil {
		return investigateToolResult{Summary: "motion_search: " + terr.Error()}
	}
	fromUTC := from.UTC().Format(time.RFC3339)
	toUTC := to.UTC().Format(time.RFC3339)
	eventType := strings.TrimSpace(args.EventType)
	events, truncated, err := listCameraMotionEvents(db, site.ID, ids, fromUTC, toUTC, eventType)
	if err != nil {
		return investigateToolResult{Summary: "motion_search: " + err.Error()}
	}

	dvrs := make([]CamDVR, 0, len(dvrByID))
	for _, d := range dvrByID {
		dvrs = append(dvrs, d)
	}
	loc, tzName := camSiteTimezone(dvrs)

	scope := "all cameras"
	if explicit {
		scope = camIDsWithNames(ids, camByID)
	}
	typeNote := ""
	if eventType != "" {
		typeNote = " of type " + eventType
	}
	if len(events) == 0 {
		return investigateToolResult{Summary: fmt.Sprintf("motion_search: no motion episodes%s recorded for %s in %s.", typeNote, scope, windowLabel(from, to))}
	}

	// Group episodes per camera in first-seen order (events arrive oldest-first).
	order := []string{}
	byCam := map[string][]camMotionEvent{}
	var earliest, latest string
	for _, e := range events {
		if _, ok := byCam[e.CameraID]; !ok {
			order = append(order, e.CameraID)
		}
		byCam[e.CameraID] = append(byCam[e.CameraID], e)
		if earliest == "" || e.StartedAt < earliest {
			earliest = e.StartedAt
		}
		if e.StartedAt > latest {
			latest = e.StartedAt
		}
	}

	const perCamCap = 25
	var b strings.Builder
	fmt.Fprintf(&b, "motion_search: %d motion episode(s)%s across %d camera(s) in %s (times in %s). Earliest %s, latest %s.\n",
		len(events), typeNote, len(order), windowLabel(from, to), tzName,
		camMotionClock(earliest, loc), camMotionClock(latest, loc))
	if truncated {
		fmt.Fprintf(&b, "NOTE: only the earliest %d episodes are shown — more matched this window. Narrow the time range or specify fewer cameras for complete coverage.\n", camMotionEventQueryLimit)
	}
	for _, camID := range order {
		list := byCam[camID]
		name := camID
		if c, ok := camByID[camID]; ok {
			name = camDisplayName(c)
		}
		fmt.Fprintf(&b, "- %s (%s): %d episode(s); ", camID, name, len(list))
		parts := make([]string, 0, perCamCap)
		for i, e := range list {
			if i >= perCamCap {
				parts = append(parts, fmt.Sprintf("…(+%d more)", len(list)-perCamCap))
				break
			}
			parts = append(parts, camMotionEpisodeLabel(e, loc))
		}
		b.WriteString(strings.Join(parts, "; "))
		b.WriteString("\n")
	}
	return investigateToolResult{Summary: strings.TrimRight(b.String(), "\n")}
}

// camMotionClock renders a stored UTC RFC3339 instant as HH:MM:SS in loc.
func camMotionClock(utcRFC3339 string, loc *time.Location) string {
	t, err := time.Parse(time.RFC3339, utcRFC3339)
	if err != nil {
		return utcRFC3339
	}
	return t.In(loc).Format("15:04:05 Jan 2")
}

// camMotionEpisodeLabel renders one episode as "HH:MM:SS–HH:MM:SS type" (or just
// "HH:MM:SS type" while the episode is still open / had no distinct end).
func camMotionEpisodeLabel(e camMotionEvent, loc *time.Location) string {
	start, serr := time.Parse(time.RFC3339, e.StartedAt)
	if serr != nil {
		return e.StartedAt + " " + e.EventType
	}
	label := start.In(loc).Format("15:04:05")
	if strings.TrimSpace(e.EndedAt) != "" {
		if end, eerr := time.Parse(time.RFC3339, e.EndedAt); eerr == nil && end.After(start) {
			label += "–" + end.In(loc).Format("15:04:05")
		}
	}
	return label + " " + e.EventType
}

// camParseWindow validates a tool's [from,to] time-window args: both must be
// present and RFC3339-parseable, a future "to" is clamped to now, "from" must
// precede "to", and the span is capped at cfg.CameraClipMaxSeconds (the same
// ceiling clip captures already enforce) so a runaway request can't pull hours
// of footage.
func camParseWindow(fromRaw, toRaw string, cfg config, clampToClipMax bool) (from, to time.Time, err error) {
	fromRaw, toRaw = strings.TrimSpace(fromRaw), strings.TrimSpace(toRaw)
	if fromRaw == "" || toRaw == "" {
		return time.Time{}, time.Time{}, errors.New(`"from" and "to" must both be RFC3339 timestamps`)
	}
	from, ferr := time.Parse(time.RFC3339, fromRaw)
	if ferr != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("invalid \"from\": %w", ferr)
	}
	to, terr := time.Parse(time.RFC3339, toRaw)
	if terr != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("invalid \"to\": %w", terr)
	}
	now := time.Now()
	if to.After(now) {
		to = now
	}
	if !from.Before(to) {
		return time.Time{}, time.Time{}, errors.New(`"from" must be before "to"`)
	}
	// past_clip pulls a REAL-TIME clip, so its window must be bounded to the clip
	// max; past_frames samples instants (cost bounded by the frame count, not
	// duration) and passes clampToClipMax=false so it can span the whole day.
	if clampToClipMax {
		maxSec := cfg.CameraClipMaxSeconds
		if maxSec <= 0 {
			maxSec = 300
		}
		if span := to.Sub(from); span > time.Duration(maxSec)*time.Second {
			from = to.Add(-time.Duration(maxSec) * time.Second)
		}
	}
	return from, to, nil
}

// windowLabel renders a [from,to) window for a prompt/summary line.
func windowLabel(from, to time.Time) string {
	return from.Format("15:04:05") + "–" + to.Format("15:04:05 Jan 2")
}

// ─────────────────────────────── the loop ───────────────────────────────

// runInvestigation drives the server-orchestrated agentic loop for one
// investigation: each turn it builds the model input (system prompt + compact
// transcript + last tool's images), calls analyzeWithAlias, parses ONE
// investigateAction (with one repair round on bad JSON), executes the tool, and
// loops — bounded by turns/media/wall-clock — until the model asks the operator
// or answers. It persists every turn/tool result as a
// camera_investigation_messages row and updates the investigation status
// (active → awaiting_operator | answered). r is threaded through to build
// served /camera/media/ URLs (camMediaURL) and may be nil (falls back to
// cfg.PublicURL / the process's own local base URL).
//
// A non-nil return is reserved for failures that prevented ANY progress (the
// database couldn't be opened, or the investigation/site couldn't be loaded) —
// per-turn analysis/parse failures and bound violations are handled gracefully
// via camStopInvestigation instead, so the transcript always ends in a
// terminal, inspectable state.
func runInvestigation(ctx context.Context, cfg config, r *http.Request, invID string) error {
	start := time.Now()
	db, err := openProxyDB(cfg)
	if err != nil {
		return fmt.Errorf("open proxy db: %w", err)
	}
	defer db.Close()

	inv, err := getCameraInvestigation(db, invID)
	if err != nil {
		return fmt.Errorf("load investigation: %w", err)
	}
	if inv.Status != "running" {
		return nil // only a worker-claimed (running) row executes — the queue owns queued→running
	}
	site, err := getCameraSite(db, inv.SiteID)
	if err != nil {
		return fmt.Errorf("load site: %w", err)
	}

	cams, _ := listCamerasBySite(db, inv.SiteID)
	dvrs, _ := listCameraDVRs(db, cfg, inv.SiteID)
	dvrByID := make(map[string]CamDVR, len(dvrs))
	for _, d := range dvrs {
		dvrByID[d.ID] = d
	}
	camByID := make(map[string]camera, len(cams))
	allowed := make(map[string]bool, len(cams))
	var active []camera
	for _, c := range cams {
		camByID[c.ID] = c
		if !c.Enabled {
			continue
		}
		if dv, ok := dvrByID[c.DVRID]; !ok || !dv.Enabled {
			continue
		}
		allowed[c.ID] = true
		active = append(active, c)
	}

	alias := firstNonEmpty(strings.TrimSpace(inv.Alias), strings.TrimSpace(cfg.CameraInvestigateAlias))
	loc, tzName := camSiteTimezone(dvrs)
	srvNow := time.Now().In(loc)
	var dvrClock time.Time
	dvrClockOK := false
	if len(dvrs) > 0 {
		if t, ok := camDVRClock(ctx, dvrs[0]); ok {
			dvrClock, dvrClockOK = t.In(loc), true
		}
	}
	// Operator knowledge + avatar registry, loaded ONCE per run (enabled rows
	// only) — the prompt catalogs and the avatar tool gating both key off these.
	playbooks, _ := listCameraPlaybooks(db, inv.SiteID, true)
	apiTools, _ := listCameraAPITools(db, inv.SiteID, true)
	avatars, _ := listCameraAvatars(db, inv.SiteID, false)
	sys := camInvestigateSystemPrompt(site, dvrs, active, playbooks, apiTools, avatars, srvNow, dvrClock, dvrClockOK, tzName)

	maxTurns := camInvestigateMaxTurns(cfg)
	maxMedia := camInvestigateMaxMedia(cfg)
	cctx, cancel := context.WithTimeout(ctx, camInvestigateBudget(cfg))
	defer cancel()

	scratch, serr := os.MkdirTemp("", "caminvestigate-")
	if serr != nil {
		return fmt.Errorf("scratch dir: %w", serr)
	}
	defer os.RemoveAll(scratch)

	msgs, err := listCameraInvestigationMessages(db, invID)
	if err != nil {
		return fmt.Errorf("load transcript: %w", err)
	}
	turnsSoFar, mediaUsed := camCountInvestigateProgress(msgs)

	camlog("info", "investigate_start", map[string]any{
		"investigation_id": invID, "site_id": inv.SiteID, "alias": alias, "cameras": len(active),
		"turns_so_far": turnsSoFar, "media_used": mediaUsed, "max_turns": maxTurns, "max_media": maxMedia,
	})

	var lastImages []string
	for {
		if cctx.Err() != nil {
			camlog("warn", "investigate_budget", map[string]any{"investigation_id": invID, "turns": turnsSoFar})
			return camStopInvestigation(db, cfg, invID, "Investigation stopped: time budget exhausted before reaching a final answer.")
		}
		if turnsSoFar >= maxTurns {
			camlog("warn", "investigate_max_turns", map[string]any{"investigation_id": invID, "turns": turnsSoFar})
			return camStopInvestigation(db, cfg, invID, "Investigation stopped: reached the maximum number of turns before a final answer.")
		}

		userText := camInvestigateTranscriptText(msgs) + "\n\nReply with ONLY the JSON action object now."
		tstart := time.Now()
		act, raw, repaired, aerr := camAnalyzeInvestigateAction(cctx, cfg, alias, sys, userText, lastImages)
		latency := time.Since(tstart).Milliseconds()
		if aerr != nil {
			camlog("error", "investigate_turn", map[string]any{
				"investigation_id": invID, "turn": turnsSoFar, "ok": false, "error": aerr.Error(),
				"repair_used": repaired, "latency_ms": latency,
			})
			return camStopInvestigation(db, cfg, invID, "Investigation stopped due to an analysis error: "+aerr.Error())
		}
		camlog("info", "investigate_turn", map[string]any{
			"investigation_id": invID, "turn": turnsSoFar, "ok": true, "action": act.Action.Type,
			"tool": act.Action.Tool, "repair_used": repaired, "latency_ms": latency, "resp_bytes": len(raw),
		})

		switch act.Action.Type {
		case "answer":
			// Only cite evidence the tools actually minted this investigation —
			// drop any hallucinated/mistyped media_url so the UI never renders a
			// broken evidence link (finding: "cite real evidence" contract).
			mintedSet, mintedByToken := camCollectMintedMedia(msgs)
			evidence, dropped := camFilterEvidence(act.Action.Evidence, mintedSet, mintedByToken)
			camlog("info", "investigate_evidence", map[string]any{
				"investigation_id": invID, "cited": len(act.Action.Evidence), "kept": len(evidence), "dropped": dropped,
			})
			m := camAppendInvestigateMessage(db, invID, "ai", camAIMessageContent(act.Thought, act.Action.Answer), "answer", "", 0, evidence)
			msgs = append(msgs, m)
			if serr := setCameraInvestigationStatus(db, invID, "answered"); serr != nil {
				return serr
			}
			go camNotifyInvestigationSettled(cfg, invID, "answered") // settle webhook (camera_serviceapi.go)
			camlog("info", "investigate_done", map[string]any{
				"investigation_id": invID, "status": "answered", "turns": turnsSoFar + 1, "latency_ms": time.Since(start).Milliseconds(),
			})
			return nil

		case "ask_operator":
			m := camAppendInvestigateMessage(db, invID, "ai", camAIMessageContent(act.Thought, act.Action.Question), "ask_operator", "", 0, nil)
			msgs = append(msgs, m)
			if serr := setCameraInvestigationStatus(db, invID, "awaiting_operator"); serr != nil {
				return serr
			}
			go camNotifyInvestigationSettled(cfg, invID, "awaiting_operator") // settle webhook (camera_serviceapi.go)
			camlog("info", "investigate_done", map[string]any{
				"investigation_id": invID, "status": "awaiting_operator", "turns": turnsSoFar + 1, "latency_ms": time.Since(start).Milliseconds(),
			})
			return nil

		default: // "call_tool" (parseInvestigateAction normalizes anything else here too)
			args := act.Action.Args
			toolArgsJSON := mustJSON(args)
			aiMsg := camAppendInvestigateMessage(db, invID, "ai", act.Thought, act.Action.Tool, toolArgsJSON, 0, nil)
			msgs = append(msgs, aiMsg)
			turnsSoFar++

			mediaLeft := maxMedia - mediaUsed
			var tr investigateToolResult
			ttStart := time.Now()
			if mediaLeft <= 0 {
				tr = investigateToolResult{Summary: "Media fetch budget for this investigation is exhausted — answer with the evidence already gathered, or ask the operator."}
			} else {
				tr = camExecuteInvestigateTool(cctx, cfg, db, r, site, alias, act.Action.Tool, args, camByID, dvrByID, allowed, active, scratch, mediaLeft)
			}
			mediaUsed += tr.Fetches
			camlog("info", "investigate_tool", map[string]any{
				"investigation_id": invID, "tool": act.Action.Tool, "fetches": tr.Fetches,
				"media_used": mediaUsed, "images": len(tr.Images), "latency_ms": time.Since(ttStart).Milliseconds(),
			})
			toolMsg := camAppendInvestigateMessage(db, invID, "tool", tr.Summary, act.Action.Tool, toolArgsJSON, tr.Fetches, tr.Media)
			msgs = append(msgs, toolMsg)
			lastImages = tr.Images
		}
	}
}

// ─────────────────────── background investigation queue ───────────────────────

// camInvestigateSem bounds concurrently RUNNING investigations across the whole
// worker; one run can take up to the investigate budget (far longer than a tick).
// Sized once by startCameraInvestigationWorker; stays nil when the worker is off.
var camInvestigateSem chan struct{}

// startCameraInvestigationWorker is the runServe hook for the DURABLE Ask-AI
// investigation queue. The HTTP handlers ENQUEUE investigations (status "queued")
// and this worker runs them detached from any request, so a page refresh/close
// never kills a run. On startup it recovers rows stranded in "running" by a
// crash/restart (resets them to "queued"), then polls for "queued" rows and runs
// each to completion on a background context bounded by the wall-clock budget.
func startCameraInvestigationWorker(cfg config) {
	if !cfg.CameraInvestigateWorkerEnabled {
		camlog("info", "investigate_worker", map[string]any{"enabled": false})
		return
	}
	if strings.TrimSpace(cfg.PublicURL) == "" {
		camlog("warn", "investigate_worker", map[string]any{"public_url": "unset",
			"note": "background-run evidence links fall back to http://127.0.0.1 — set PROXY_PUBLIC_URL for externally reachable proof URLs"})
	}
	// Crash recovery: a "running" row is orphaned (its goroutine died with the
	// process) — requeue for retry. runInvestigation is resumable, so this loses at
	// most the single in-flight turn. Done once, before the ticker starts.
	if db, err := openProxyDB(cfg); err == nil {
		if n, rerr := requeueRunningCameraInvestigations(db); rerr != nil {
			camlog("warn", "investigate_recover", map[string]any{"ok": false, "error": rerr.Error()})
		} else if n > 0 {
			camlog("info", "investigate_recover", map[string]any{"requeued": n})
		}
		db.Close()
	}

	interval := cfg.CameraInvestigateTickInterval
	if interval <= 0 {
		interval = 5 * time.Second
	}
	conc := cfg.CameraInvestigateConcurrency
	if conc < 1 {
		conc = 1
	}
	camInvestigateSem = make(chan struct{}, conc)
	camlog("info", "investigate_worker_start", map[string]any{"tick_ms": interval.Milliseconds(), "concurrency": conc})

	go func() {
		time.Sleep(camStartupJitter(interval))
		camInvestigationTick(cfg)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for range ticker.C {
			camInvestigationTick(cfg)
		}
	}()
}

// camInvestigationTick claims and dispatches queued investigations. It no-ops while
// the proxy is globally paused (in-flight detached runs finish naturally).
func camInvestigationTick(cfg config) {
	if !proxyEnabled.Load() {
		return
	}
	db, err := openProxyDB(cfg)
	if err != nil {
		camlog("error", "investigate_tick", map[string]any{"ok": false, "error": err.Error()})
		return
	}
	defer db.Close()

	// Runtime reaper: requeue "running" rows stranded by a crashed run or a lost
	// terminalization write (idle longer than a whole run could take). Self-heals the
	// stuck-"running" case without waiting for a process restart.
	if n, rerr := requeueStaleRunningCameraInvestigations(db, camInvestigateBudget(cfg)+2*time.Minute); rerr != nil {
		camlog("warn", "investigate_reap", map[string]any{"ok": false, "error": rerr.Error()})
	} else if n > 0 {
		camlog("info", "investigate_reap", map[string]any{"requeued_stale_running": n})
	}

	queued, err := listQueuedCameraInvestigations(db)
	if err != nil {
		camlog("error", "investigate_tick", map[string]any{"ok": false, "error": err.Error()})
		return
	}
	for _, inv := range queued {
		camConsiderInvestigation(cfg, db, inv)
	}
}

// camConsiderInvestigation acquires a concurrency slot, atomically claims the row
// (queued→running), and spawns the bounded, DETACHED run — the same acquire-then-
// claim, release-on-lost-claim shape as camConsiderWatch.
func camConsiderInvestigation(cfg config, db *sql.DB, inv camInvestigation) {
	select {
	case camInvestigateSem <- struct{}{}:
	default:
		camlog("debug", "investigate_busy", map[string]any{"investigation_id": inv.ID})
		return // reconsidered next tick; row stays "queued"
	}
	claimed, cerr := claimCameraInvestigation(db, inv.ID)
	if cerr != nil {
		<-camInvestigateSem
		camlog("warn", "investigate_claim", map[string]any{"investigation_id": inv.ID, "ok": false, "error": cerr.Error()})
		return
	}
	if !claimed {
		<-camInvestigateSem // another tick/instance won the claim — not an error
		return
	}
	go func() {
		defer func() { <-camInvestigateSem }()
		// recover() so a panic anywhere in the run (image decode / ffmpeg / model-output
		// parsing) fails just THIS investigation instead of crashing the whole shared
		// proxy — and terminalize the row so a deterministic panic can't become a
		// restart crash-loop poison pill.
		defer func() {
			if rec := recover(); rec != nil {
				camlog("error", "investigate_panic", map[string]any{"investigation_id": inv.ID, "panic": fmt.Sprint(rec)})
				camTerminalizeInvestigation(cfg, inv.ID, "Investigation stopped: an internal error occurred.")
			}
		}()
		// Detached context (NOT an HTTP request); runInvestigation applies the
		// wall-clock budget internally. r == nil — media URLs come from cfg.PublicURL.
		if rerr := runInvestigation(context.Background(), cfg, nil, inv.ID); rerr != nil {
			// Non-nil == a setup failure that left the row "running" with no progress;
			// mark it terminal so the poll shows a final state (in-loop failures already
			// self-terminate to "exhausted"; if this write is lost, the runtime reaper
			// requeueStaleRunningCameraInvestigations picks the row back up).
			camlog("error", "investigate_run", map[string]any{"investigation_id": inv.ID, "ok": false, "error": rerr.Error()})
			camTerminalizeInvestigation(cfg, inv.ID, "Investigation could not start: "+rerr.Error())
		}
	}()
}

// camTerminalizeInvestigation best-effort marks an investigation exhausted with a
// note (the worker's error/panic paths). Best-effort by design: if the DB can't be
// opened or the write fails, the runtime reaper (requeueStaleRunningCameraInvestigations)
// eventually requeues the still-"running" row, so nothing strands permanently.
func camTerminalizeInvestigation(cfg config, id, note string) {
	if db, err := openProxyDB(cfg); err == nil {
		_ = camStopInvestigation(db, cfg, id, note)
		db.Close()
	}
}

// camRunInvestigationInline drains a just-enqueued investigation synchronously on
// the request goroutine — the fallback for when the background worker is DISABLED
// (PROXY_CAMERA_INVESTIGATE_WORKER=false), so disabling it degrades to the legacy
// synchronous behavior instead of stranding rows in "queued". It claims the row
// (queued→running) first (a no-op if the worker somehow already grabbed it) and runs
// with the live request r, so media URLs resolve even without PROXY_PUBLIC_URL.
func camRunInvestigationInline(db *sql.DB, cfg config, r *http.Request, id string) {
	if claimed, cerr := claimCameraInvestigation(db, id); cerr != nil || !claimed {
		return
	}
	if rerr := runInvestigation(r.Context(), cfg, r, id); rerr != nil {
		camlog("error", "investigate_run", map[string]any{"investigation_id": id, "ok": false, "error": rerr.Error()})
		_ = camStopInvestigation(db, cfg, id, "Investigation could not start: "+rerr.Error())
	}
}

// ─────────────────────────────── JSON row mappers ───────────────────────────────
// Mirrors camerahttp.go's convention: store rows have no json tags, so every
// response is built as an explicit snake_case map here (camDecodeJSON is
// camerahttp.go's best-effort stored-JSON-column decoder, reused as-is).

func investigationJSON(inv camInvestigation) map[string]any {
	return map[string]any{
		"id": inv.ID, "site_id": inv.SiteID, "title": inv.Title, "question": inv.Question,
		"alias": inv.Alias, "status": inv.Status, "created_at": inv.CreatedAt, "updated_at": inv.UpdatedAt,
	}
}

func investigateMessageJSON(m camInvestigationMessage) map[string]any {
	return map[string]any{
		"id": m.ID, "seq": m.Seq, "role": m.Role, "content": m.Content,
		"tool_name": m.ToolName, "tool_args": camDecodeJSON(m.ToolArgs),
		"media": camDecodeJSON(m.MediaJSON), "created_at": m.CreatedAt,
	}
}

func investigateMessagesJSON(msgs []camInvestigationMessage) []map[string]any {
	out := make([]map[string]any, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, investigateMessageJSON(m))
	}
	return out
}

// ─────────────────────────────── HTTP handlers ───────────────────────────────

// handleCameraInvestigations serves the /ui/api/cameras/investigations path:
// POST starts a new investigation ({site_id, question, alias?}) and runs the
// loop until it pauses on ask_operator or answers, returning {id, messages};
// GET lists a site's investigations (delegated to handleCameraInvestigationList).
func handleCameraInvestigations(cfg config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			handleCameraInvestigationList(cfg)(w, r)
		case http.MethodPost:
			handleCameraInvestigationStart(cfg)(w, r)
		default:
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		}
	}
}

// handleCameraInvestigationStart implements the POST side of
// handleCameraInvestigations: validates the request, creates the investigation row
// (status "queued") + its opening operator message, and returns immediately — the
// background worker runs the loop detached from this request (or, if the worker is
// disabled, it is drained inline). The UI polls /investigations/get for progress.
func handleCameraInvestigationStart(cfg config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			SiteID   string `json:"site_id"`
			Question string `json:"question"`
			Alias    string `json:"alias"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid JSON"})
			return
		}
		siteID := strings.TrimSpace(body.SiteID)
		question := strings.TrimSpace(body.Question)
		if siteID == "" || question == "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "site_id and question are required"})
			return
		}
		db, err := openProxyDB(cfg)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		defer db.Close()
		if _, serr := getCameraSite(db, siteID); serr != nil {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "site not found"})
			return
		}
		alias := firstNonEmpty(strings.TrimSpace(body.Alias), strings.TrimSpace(cfg.CameraInvestigateAlias))
		id, ierr := insertCameraInvestigation(db, camInvestigation{
			SiteID: siteID, Title: truncateString(question, 80), Question: question, Alias: alias, Status: "queued",
		})
		if ierr != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": ierr.Error()})
			return
		}
		camAppendInvestigateMessage(db, id, "operator", question, "", "", 0, nil)
		camlog("info", "investigate_create", map[string]any{"investigation_id": id, "site_id": siteID, "alias": alias})

		// The investigation runs in the BACKGROUND queue (startCameraInvestigationWorker),
		// detached from this request — a page refresh/close never kills it; the UI's 4s
		// poll on /investigations/get shows progress. If the worker is DISABLED, drain it
		// synchronously here so disabling it degrades to legacy sync mode, not stranding.
		if !cfg.CameraInvestigateWorkerEnabled {
			camRunInvestigationInline(db, cfg, r, id)
		}
		inv, msgs, gerr := getCameraInvestigationWithMessages(db, id)
		if gerr != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": gerr.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"ok": true, "id": inv.ID, "status": inv.Status,
			"investigation": investigationJSON(inv), "messages": investigateMessagesJSON(msgs),
		})
	}
}

// handleCameraInvestigationReply resumes an investigation: POST {id, message}
// records the operator's message and RE-ENQUEUES the row (status "queued") for the
// background worker to continue, returning immediately. Works both to answer an
// ask_operator pause and to follow up on an already-answered/exhausted investigation.
// Rejects a closed investigation, and rejects a reply while the row is still
// queued/running (a worker owns it — wait for it to pause/finish). If the worker is
// disabled the re-queued row is drained inline.
func handleCameraInvestigationReply(cfg config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
			return
		}
		var body struct {
			ID      string `json:"id"`
			Message string `json:"message"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid JSON"})
			return
		}
		id := strings.TrimSpace(body.ID)
		message := strings.TrimSpace(body.Message)
		if id == "" || message == "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "id and message are required"})
			return
		}
		db, err := openProxyDB(cfg)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		defer db.Close()
		inv, gerr := getCameraInvestigation(db, id)
		if gerr != nil {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "investigation not found"})
			return
		}
		switch inv.Status {
		case "closed":
			writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": "investigation is closed"})
			return
		case "queued", "running":
			// A worker owns this row right now; refusing to mutate it is what prevents
			// the worker-vs-request lost-update race. Operator waits, then replies.
			writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": "investigation is still working — wait for it to pause or finish, then reply"})
			return
		}
		// Settled (awaiting_operator | answered | exhausted): append the operator message
		// FIRST (row still settled, so no worker touches it), THEN flip to "queued" so the
		// background worker only ever claims a row whose transcript already includes this
		// reply — this ordering closes the resume-on-stale-transcript race.
		camAppendInvestigateMessage(db, id, "operator", message, "", "", 0, nil)
		if serr := setCameraInvestigationStatus(db, id, "queued"); serr != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": serr.Error()})
			return
		}
		camlog("info", "investigate_reply", map[string]any{"investigation_id": id, "prev_status": inv.Status})

		// Background worker resumes the re-queued row; drain inline if it's disabled.
		if !cfg.CameraInvestigateWorkerEnabled {
			camRunInvestigationInline(db, cfg, r, id)
		}
		outInv, msgs, gerr2 := getCameraInvestigationWithMessages(db, id)
		if gerr2 != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": gerr2.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"ok": true, "id": outInv.ID, "status": outInv.Status,
			"investigation": investigationJSON(outInv), "messages": investigateMessagesJSON(msgs),
		})
	}
}

// handleCameraInvestigationList lists a site's investigations: GET ?site_id=
// ("" lists across every site).
func handleCameraInvestigationList(cfg config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
			return
		}
		siteID := strings.TrimSpace(r.URL.Query().Get("site_id"))
		db, err := openProxyDB(cfg)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		defer db.Close()
		invs, lerr := listCameraInvestigations(db, siteID)
		if lerr != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": lerr.Error()})
			return
		}
		out := make([]map[string]any, 0, len(invs))
		for _, inv := range invs {
			out = append(out, investigationJSON(inv))
		}
		writeJSON(w, http.StatusOK, map[string]any{"investigations": out})
	}
}

// handleCameraInvestigationGet returns one investigation's full transcript: GET ?id=.
func handleCameraInvestigationGet(cfg config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
			return
		}
		id := strings.TrimSpace(r.URL.Query().Get("id"))
		if id == "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "id is required"})
			return
		}
		db, err := openProxyDB(cfg)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		defer db.Close()
		inv, msgs, gerr := getCameraInvestigationWithMessages(db, id)
		if gerr != nil {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "investigation not found"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"investigation": investigationJSON(inv), "messages": investigateMessagesJSON(msgs)})
	}
}
