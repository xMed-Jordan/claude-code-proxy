package main

// camera_strategies.go — built-in, environment-agnostic investigation METHODS
// for the Ask-AI camera loop. Unlike operator-authored playbooks (camera_playbooks.go,
// site-specific, stored in SQLite), these are static procedures that apply to ANY
// premises (clinic, supermarket, store, home, office, garage): "how-to: trace
// everything a person did", "how-to: find who took an object", etc.
//
// They are surfaced two ways, mirroring the playbook mechanism so the model sees a
// uniform interface:
//   - a compact one-liner catalog (camStrategyCatalog) is always present in the
//     investigation system prompt (camInvestigateSystemPrompt);
//   - the full step-by-step text loads on demand through the EXISTING `playbook`
//     tool — camToolPlaybook checks camFindBuiltinStrategy after it misses the DB,
//     so there is ZERO action-contract change and no new tool to teach the model.
//
// Precedence: an operator playbook always wins on an exact-name collision (the DB
// lookup runs first in camToolPlaybook); a built-in method is the fallback. This
// lets a site override a global method by authoring a playbook with the same name.

import (
	"fmt"
	"strings"
)

// camStrategyWhenToUseMax truncates a method's when_to_use in the prompt catalog
// (the full text loads via the playbook tool), matching camPlaybookOneLinerMax's
// role for operator playbooks.
const camStrategyWhenToUseMax = 110

// camDelegateToolEnabled gates the delegate-fan-out core principle: the delegate
// tool ships in a later workstream (WS3 sub-agent delegation). Until it exists the
// prompt must NOT advertise a tool the loop cannot execute, so the principle is
// suppressed. Flip to true when the delegate tool lands.
const camDelegateToolEnabled = false

// camStrategy is one built-in investigation method: a model-facing name (prefixed
// "how-to: "), a one-liner shown in the catalog, and the full procedure returned by
// the playbook tool. Mirrors the operator-facing camPlaybook shape minus DB fields.
type camStrategy struct {
	Name         string // model-facing identifier, e.g. "how-to: trace everything a person did"
	WhenToUse    string // one-liner shown in the prompt catalog
	Instructions string // full step-by-step procedure returned by the playbook tool
}

// camBuiltinStrategies is the static method library. Each entry names the exact
// tools per step and carries 2-3 worked examples from DIFFERENT environments so the
// procedure reads as environment-agnostic. Order here is the catalog order.
var camBuiltinStrategies = []camStrategy{
	{
		Name:      "how-to: trace everything a person did",
		WhenToUse: `"what did employee/visitor X do", "where did they go", arrival & departure across a day`,
		Instructions: `Goal: a chronological timeline of everything ONE person did on site.

1. Identify the target. If they are a registered person, call avatars to find the id, then avatar_info to study their reference images. Otherwise take the operator's description (clothing, build) and find their FIRST appearance on the entrance camera (pick it from the roster) with motion_search over the day, then contact_sheet mode "motion" the flagged window.
2. Establish the entry instant from that first sighting; past_frames main quality to confirm it is the right person.
3. Follow them by TOPOLOGY HANDOFF: from the roster, list the areas adjacent to where they were last seen (corridors and doorways are chokepoints) and check those cameras in TIME ORDER. Use avatar_find when the target is registered, otherwise contact_sheet the next camera starting at the handoff time.
4. At each area note arrival and leave times plus what they did there; verify pivotal moments (entering a restricted room, an interaction) at main quality with past_frames or avatar_check.
5. Answer = a chronological timeline with ONE annotated frame per segment (annotate the person you cite).

Examples:
- Clinic: "what did the new assistant do this morning?" — reception first sighting, corridor handoff, per-room arrival/leave times.
- Office: "which rooms did the visitor enter?" — lobby entry, then meeting-room and corridor cams in order.
- Home: "when did my son get home and where did he go?" — driveway/front-door entry time, hallway, his room.`,
	},
	{
		Name:      "how-to: verify an object on a person",
		WhenToUse: `"did he still have his watch/bag/phone when he arrived/left?" — confirm an item is on a person`,
		Instructions: `Goal: confirm whether a specific object was on a person at a specific moment.

1. Find the arrival (or relevant) moment — method "how-to: trace everything a person did", abbreviated to just the entry instant.
2. Pick the camera pass CLOSEST to a close-up or overhead lens: reception-desk overheads and doorway close-ups are ideal (choose it from the roster, not the widest cam).
3. past_frames at MAIN quality at those EXACT instants, several stills — a single frame misleads because of occlusion, motion blur, and body angle.
4. Inspect the specific body part (wrist, shoulder, hand) across the stills. If the question is about LOSS, repeat at the departure moment and compare arrival vs departure.
5. annotate the clearest frame around the object/limb; state your confidence and be honest about resolution limits.

Examples:
- Clinic: "did the patient still have his watch when he left?" — arrival wrist frames vs departure wrist frames at the reception overhead.
- Store: "did she walk out holding merchandise?" — exit-door main-quality frames of the hands.
- Home: "did the courier actually leave the package?" — front-door frames before vs after the visit.`,
	},
	{
		Name:      "how-to: measure waiting or dwell time",
		WhenToUse: `"did X wait long?", "how long was the room occupied?" — a duration between two instants`,
		Instructions: `Goal: a duration, measured from two ENDPOINT instants — never scan the middle.

1. Find the ARRIVAL instant on the entrance or waiting-area camera: motion_search to bracket it cheaply, then contact_sheet (mode "interval" for a busy waiting area, "motion" for a door) to pin the exact moment.
2. Find the SERVICE-START (or departure) instant: the moment they approached the desk, entered the room, or staff engaged them — same tools on the relevant camera.
3. Wait/dwell = the difference between the two instants. Capture ONE evidence frame at EACH endpoint (past_frames), not the span between.
4. If presence is intermittent (they left and came back), motion_search the station camera across the window — the motion episodes give you the presence timeline directly.

Examples:
- Clinic: "did the patient wait too long?" — waiting-room arrival instant vs called-in instant.
- Supermarket: "how long was that man in the checkout queue?" — join-queue instant vs reach-till instant.
- Home/office: "how long was the cleaner in room 3?" — entered-room vs left-room instants.`,
	},
	{
		Name:      "how-to: find when something changed",
		WhenToUse: `"when did the damage appear / the item vanish / the door open?" — pinpoint a change-point`,
		Instructions: `Goal: the exact moment a state changed, found by BINARY SEARCH — never a linear scan.

1. Bracket the change: [last known GOOD time, first known BAD time]. If you lack a bracket, past_frames the two ends of the plausible window to establish one.
2. past_frames a single still at the MIDPOINT of the bracket. Decide which half still contains the change (good midpoint → change is later; bad midpoint → earlier). Halve again. Six frames locate a moment inside a ten-hour day.
3. Narrow to a window of about two minutes or less.
4. contact_sheet mode "motion" that narrow window to catch the ACTOR who caused the change, and identify who was present.
5. annotate the frame that shows the change and the person.

Examples:
- Store: "when did the display glass break?" — binary-search between opening (intact) and the report (broken).
- Garage: "when did that graffiti appear?" — bracket overnight, halve down to the tagging minute.
- Home: "when was the medicine cabinet opened?" — bracket the afternoon, halve to the exact frame.`,
	},
	{
		Name:      "how-to: find who took an object",
		WhenToUse: `a lost or stolen item — "who took the ...?"`,
		Instructions: `Goal: identify who removed an object, with evidence.

1. Establish the object's LAST-SEEN placement: the operator may give it, otherwise use "how-to: find when something changed" to find when it was last in place.
2. Binary-search the DISAPPEARANCE moment (past_frames midpoints) down to a tight window.
3. contact_sheet mode "motion" roughly ±5 minutes around that moment on the camera covering the object; identify EVERY person who came near it.
4. Follow the most likely person with "how-to: trace everything a person did" to see them leave with it.
5. past_clip the taking moment as citable evidence; annotate the person.
6. If occlusion or angle prevents certainty, say so plainly and report who was near it rather than guessing.

Examples:
- Clinic: "who took the phone left at reception?" — last-seen on the desk, disappearance minute, people at the desk.
- Office: "my wallet went missing from the break room" — placement, then who entered the room in that window.
- Garage: "a tool is gone from the bench" — last-seen, ±5 min contact sheet, follow the person out.`,
	},
	{
		Name:      "how-to: verify a workflow or checklist",
		WhenToUse: `"did the cleaner do all her jobs?", patrol rounds, closing procedure, hygiene compliance`,
		Instructions: `Goal: a per-task verdict (done / not done / not visible) for a routine.

1. Get the checklist. FIRST check the OPERATIONAL PLAYBOOKS catalog — if the operator authored one for this routine, fetch it with the playbook tool and follow it. Otherwise ask_operator ONCE for the task list, or infer the standard steps.
2. Map each task to the camera(s) covering its area, using the roster.
3. For EACH task, over the shift window: motion_search that camera to find presence episodes, then verify the ACTIVITY itself (not just presence) at an appropriate quality — contact_sheet or past_frames, main quality when the task needs fine detail (wiping a surface, locking a door).
4. Record a per-task verdict with the time and one evidence frame.
5. Answer = a checklist table: each item marked done/not-done/not-visible with its time and evidence.

Examples:
- Clinic: "did the cleaner do all her rounds?" — map each room to its cam, verify presence + wiping per room.
- Warehouse: "did the guard complete the patrol?" — each checkpoint cam, a presence episode per checkpoint in order.
- Store: "was the closing procedure followed?" — lights, till, back-door-lock cameras, each verified at close.`,
	},
	{
		Name:      "how-to: count entries or occupancy",
		WhenToUse: `"how many customers came?", "was it busy?", queue length over time`,
		Instructions: `Goal: a count of people (entries) or occupancy over time.

1. Pick the ONE best camera from the roster: the door/gate for ENTRIES, the widest view of the area for OCCUPANCY. Judge from the roster description whether it is a mostly-still DOOR or a BUSY area.
2. Door/entries → contact_sheet mode "motion" (drops static frames, keeps arrivals). Busy area → contact_sheet mode "interval" (uniform time-lapse; motion filtering would keep everything and help nothing).
3. Count DISTINCT people: adjacent cells seconds apart are the SAME person, not several. For occupancy over time, sample every N minutes and read how many are present per sample.
4. Cross-check any surprising count by past_frames on the specific cells.

Examples:
- Clinic: "how many patients came today?" — entrance door, motion contact sheet, dedupe.
- Supermarket: "how long was the queue at 6pm?" — checkout-area interval sheet, count per sample.
- Gym/office: "how busy was the floor this afternoon?" — wide floor cam, interval sampling.`,
	},
	{
		Name:      "how-to: review a transaction or dispute",
		WhenToUse: `"he says he paid cash", "was it scanned?", damage/refund disputes at a counter`,
		Instructions: `Goal: reconstruct a counter interaction to settle a dispute.

1. Pin the time: take the operator's estimate, then motion_search / contact_sheet the desk or till camera to find the EXACT interaction interval.
2. Use the OVERHEAD desk/POS camera at MAIN quality — hand-level detail needs the closest, highest-resolution angle (pick it from the roster).
3. past_frames DENSELY across the 1-3 minute interaction (high count) so no beat is missed.
4. past_clip the whole interaction as citable evidence.
5. Narrate the sequence with timestamps. Be explicit about what is and is NOT resolvable — bill denominations and small screen text usually are NOT.

Examples:
- Clinic reception: "the patient claims he paid cash" — desk cam, dense frames of the hand-off plus a clip.
- Supermarket: "were all items scanned?" — POS overhead, frame each item across the belt.
- Store: "he says the item was already broken" — counter cam at the moment of handling.`,
	},
	{
		Name:      "how-to: investigate vehicle activity",
		WhenToUse: `a car entered the garage/driveway, "did they take something from the car?", who drove off`,
		Instructions: `Goal: account for a vehicle and the people/objects around it.

1. motion_search the garage/driveway/gate camera to find the entry and exit episodes.
2. past_frames the arrival: who got out, which doors/trunk opened.
3. Watch for CARRIED OBJECTS at main quality — compare the hands and arms of each person ENTERING the vehicle vs LEAVING it (something taken out shows in the leaving frames).
4. Follow people between the vehicle and the building by topology handoff.
5. past_clip the loading/unloading moment as evidence; report per-person carried-object observations, honestly noting what the angle could not show.

Examples:
- Home garage: "did someone take something from my car?" — entry episode, trunk-open frames, carried-object comparison.
- Delivery: "what did the van driver pick up?" — dock cam arrival, hands empty in vs loaded out.
- Driveway: "who drove off in the grey car?" — exit episode, driver frames at main quality.`,
	},
	{
		Name:      "how-to: audit after-hours activity",
		WhenToUse: `"was anyone here last night?", loitering, lights/door left on/open overnight`,
		Instructions: `Goal: a timeline of everything that happened during a closed period.

1. motion_search ALL relevant cameras across the WHOLE closed window FIRST — it costs no media budget and instantly tells you which cameras saw anything and when.
2. Cluster the returned episodes by camera and time.
3. For each cluster, contact_sheet mode "motion" the involved camera over that episode; classify what it was: staff, intruder, animal, or just a light/shadow change.
4. For STATE questions (were the lights left on? was the door left open?), sample past_frames every 30-60 minutes instead — motion won't fire on a static state.
5. Answer = a timeline, with evidence per episode.

Examples:
- Office: "was anyone in the building last night?" — all cams motion-scanned 8pm-6am, episodes classified.
- Store: "someone's been loitering at the entrance after close" — entrance cam episodes clustered.
- Home: "did we leave the oven light / garage door on?" — periodic frame sampling of that view overnight.`,
	},
	{
		Name:      "how-to: read fine detail",
		WhenToUse: `phone screens, license plates, name tags, labels, small text on a screen`,
		Instructions: `Goal: read small detail — and be honest when it cannot be read.

1. Find the closest camera and the moment the detail FACES the lens (a plate square-on, a screen tilted up). Pick the camera from the roster by proximity, not by coverage.
2. past_frames at MAIN quality, several instants — motion blur and focus vary frame to frame, so one still is not enough.
3. Set expectations honestly: even 2560x1440 main streams rarely resolve phone-UI text or a license plate cleanly beyond ~4-6 metres.
4. If it is unreadable, say EXACTLY why (distance, angle, blur, resolution) and state what WOULD have captured it (a closer/overhead camera, a different moment). NEVER guess the content.

Examples:
- Office: "which app was she using on her phone?" — closest desk cam, main quality, several frames; often not resolvable — say so.
- Garage: "what's the plate of the grey car?" — plate-facing moment at main quality.
- Store: "does the price tag match?" — closest shelf cam, main quality, multiple frames.`,
	},
	{
		Name:      "how-to: package evidence for the operator",
		WhenToUse: `wrapping up ANY investigation, or "send me the video"`,
		// TODO(WS5): once the evidence_export tool ships, add it here as the mode
		// for LONG windows / multi-camera compilations (sequential/grid/separate)
		// delivered as permanent links; today only annotate + past_clip exist.
		Instructions: `Goal: deliver proof the operator can act on — one evidence item per claim.

Choose the right artifact:
- annotate — circle the person/object in a SINGLE frame. Best for WHO/WHERE claims ("this is the person at 14:03").
- past_clip — a continuous recording (up to ~5 minutes) of ONE camera. Best for ACTIONS the operator should watch unfold (the taking, the interaction, the arrival).

Rules:
- Every factual claim in the answer needs its OWN evidence item with the exact timestamp.
- Always cite the exact media_url values from the TOOL RESULT lines — that is how the operator actually sees the proof.
- One evidence item per claim; lead the answer with the CONCLUSION, then the timeline, then the evidence.

Examples:
- "Who took the phone?" — annotate the person plus past_clip the taking moment.
- "Did the cleaner finish?" — one annotated frame per completed/missed task.
- "What happened at the counter?" — past_clip the full interaction.`,
	},
}

// camFindBuiltinStrategy resolves a built-in method by case-insensitive name
// (trimmed). The bool is false for an unknown name, so callers can fall through to
// the operator-playbook path or the unknown-name summary.
func camFindBuiltinStrategy(name string) (camStrategy, bool) {
	want := strings.TrimSpace(name)
	if want == "" {
		return camStrategy{}, false
	}
	for _, s := range camBuiltinStrategies {
		if strings.EqualFold(s.Name, want) {
			return s, true
		}
	}
	return camStrategy{}, false
}

// camStrategyCatalog renders the INVESTIGATION METHODS one-liner list for the
// system prompt: `- "<name>" — <when_to_use>` per method, when_to_use truncated to
// camStrategyWhenToUseMax runes (full text loads via the playbook tool). Mirrors
// camPlaybookCatalog formatting. The header and trailing guidance are emitted by
// the caller (camInvestigateSystemPrompt).
func camStrategyCatalog() string {
	var b strings.Builder
	for _, s := range camBuiltinStrategies {
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		b.WriteString(`- "` + s.Name + `"`)
		when := strings.TrimSpace(s.WhenToUse)
		if r := []rune(when); len(r) > camStrategyWhenToUseMax {
			when = string(r[:camStrategyWhenToUseMax]) + "…"
		}
		if when != "" {
			b.WriteString(" — " + when)
		}
	}
	return b.String()
}

// camStrategyToolSummary renders a built-in method for the playbook tool result,
// mirroring camToolPlaybook's "PLAYBOOK ..." header so the model sees a uniform
// "here is the procedure" shape whichever kind it fetched.
func camStrategyToolSummary(s camStrategy) string {
	if when := strings.TrimSpace(s.WhenToUse); when != "" {
		return fmt.Sprintf("INVESTIGATION METHOD %q (use when: %s):\n%s", s.Name, when, strings.TrimSpace(s.Instructions))
	}
	return fmt.Sprintf("INVESTIGATION METHOD %q:\n%s", s.Name, strings.TrimSpace(s.Instructions))
}

// camStrategyNameList returns the built-in method names as a quoted, comma-joined
// list for the playbook tool's unknown-name reply (camPlaybookUnknownSummary).
func camStrategyNameList() string {
	names := make([]string, 0, len(camBuiltinStrategies))
	for _, s := range camBuiltinStrategies {
		names = append(names, fmt.Sprintf("%q", s.Name))
	}
	return strings.Join(names, ", ")
}
