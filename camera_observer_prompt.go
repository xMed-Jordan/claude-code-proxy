package main

// camera_observer_prompt.go — prompts + strict-JSON parsing for the
// activity-log observer (camera_observer.go).
//
// The system prompt reuses the investigation loop's shared knowledge blocks
// (camSiteContextBlock, camPromptTimeBlock, camBuildRoster) and adds three
// observer-specific pieces: the avatar block (enrolled people/vehicles/pets
// with their confirmed-or-suggested roles rendered through the site's role
// vocabulary), the continuity block (the stream's own recent entries — the
// "the crew is STILL painting" memory: compressed idle runs + a few verbatim
// reports), and the English-only mandate — log ENTRIES are the AI's own
// continuity memory and stay English regardless of the site's report_language
// (rollups translate later, camera_observer_report.go).
//
// The contract is strict single-object JSON (headline/report/needs_detail);
// parseObserverReport reuses the investigate-style balanced-brace extractor,
// callers get ONE repair round and then salvage (camera_observer.go).

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// observerReport is the strict JSON the observer model returns for one window.
type observerReport struct {
	Headline    string              `json:"headline"`
	Report      string              `json:"report"`
	NeedsDetail []observerDetailReq `json:"needs_detail"`
}

// observerDetailReq is one drill-down request: a full-quality frame of one
// camera at one instant (clamped by camObserverClampDetails).
type observerDetailReq struct {
	CameraID string `json:"camera_id"`
	Time     string `json:"time"`
}

// camObserverContract is the JSON contract appended to every system prompt.
const camObserverContract = "OUTPUT — reply with EXACTLY ONE JSON object and nothing else (no prose, no markdown fences):\n" +
	`{"headline":"...","report":"...","needs_detail":[{"camera_id":"...","time":"..."}]}` + "\n" +
	"- headline: ONE factual English line (max 140 chars) summarizing the window.\n" +
	"- report: English, max 300 words — WHAT is going on, WHO is present (name enrolled people you actually recognize; describe everyone else neutrally), WHERE (camera/area). Neutral and factual; note continuity with your earlier entries.\n" +
	"- needs_detail: OPTIONAL (omit or []). ONLY when the sheets are genuinely too small/unclear to tell what is happening, list up to 4 requests {camera_id: exact roster id, time: RFC3339 inside the window} for full-quality frames. You receive them ONCE and must then write the final entry.\n"

// camObserverReplyLine closes the user prompt with the reply-format reminder.
const camObserverReplyLine = "Reply with EXACTLY ONE JSON object as specified in the contract."

// camObserverRepairSuffix is appended to the user prompt for the single
// repair round after an unparseable reply.
const camObserverRepairSuffix = "\n\nYour previous reply could not be parsed. Reply with ONLY the single JSON object described in the contract — no prose, no markdown fences, nothing else."

// camObserverSystemPrompt builds the observer's per-cycle system prompt: the
// logger role, the operator-authored site context, the clock preamble
// (dvrClockOK=false in v1 — entries are cut on the server clock, not DVR
// playback windows), the selected-camera roster, the avatar and continuity
// blocks, the English mandate, and the JSON contract.
func camObserverSystemPrompt(site camSite, dvrs []CamDVR, cams []camera, avatars []camAvatar,
	policy camSitePolicy, recent []logEntry, now time.Time, loc *time.Location, tzName string, fullContext int) string {
	var b strings.Builder
	b.WriteString("You are the continuous activity logger for a physical premises")
	if n := strings.TrimSpace(site.Name); n != "" {
		fmt.Fprintf(&b, " (%s)", n)
	}
	b.WriteString(". Every few minutes you are shown composite contact sheets of frames sampled from the site's cameras ")
	b.WriteString("across ONE time window, and you write ONE log entry recording what is going on, who is present, and where — ")
	b.WriteString("a neutral, factual, CONTINUOUS narrative of the site's life. You are a journal, not an alarm: record normal ")
	b.WriteString("activity plainly, and note anything unusual without dramatizing.\n\n")

	if block := camSiteContextBlock(site, dvrs); block != "" {
		b.WriteString(block)
		b.WriteString("\n\n")
	}

	camPromptTimeBlock(&b, now, time.Time{}, false, tzName)

	b.WriteString("CAMERA ROSTER (id — name [area]: description) — the cameras this log stream watches:\n")
	b.WriteString(camBuildRoster(cams))
	b.WriteString("\n\n")

	if block := camObserverAvatarBlock(avatars, policy); block != "" {
		b.WriteString(block)
		b.WriteString("\n\n")
	}

	if block := camObserverContinuityBlock(recent, loc, fullContext); block != "" {
		b.WriteString(block)
		b.WriteString("\n\n")
	}

	b.WriteString("LANGUAGE: write the headline and report in ENGLISH, always — the log is the system's own memory and must ")
	b.WriteString("stay uniform, even when the site profile, notes or names above are in another language.\n\n")
	b.WriteString(camObserverContract)
	return b.String()
}

// camObserverAvatarBlock renders the enrolled avatars the observer may
// recognize, one line each: "- <name> (<type>, role: <label>): <first line of
// description>". The role label prefers the operator-CONFIRMED role, then the
// AI-suggested one (marked), then "unlabeled"; slugs resolve to display labels
// through the site's role vocabulary (camRoleVocabulary). Returns "" when the
// site has no avatars in scope.
func camObserverAvatarBlock(avatars []camAvatar, policy camSitePolicy) string {
	if len(avatars) == 0 {
		return ""
	}
	labels := make(map[string]string)
	for _, r := range camRoleVocabulary(policy) {
		labels[r.Slug] = r.Label
	}
	var b strings.Builder
	b.WriteString("KNOWN PEOPLE & ENTITIES (operator-enrolled; name them when you recognize them — NEVER invent an identity for anyone else):\n")
	for _, a := range avatars {
		kind := strings.TrimSpace(a.Type)
		if kind == "" {
			kind = "human"
		}
		role := "unlabeled"
		if a.Role != "" {
			role = firstNonEmpty(labels[a.Role], a.Role)
		} else if sr := strings.TrimSpace(a.SuggestedRole); sr != "" && sr != "unknown" {
			role = firstNonEmpty(labels[sr], sr) + " (suggested)"
		}
		b.WriteString("- " + a.Name + " (" + kind + ", role: " + role + ")")
		if d := camFirstLine(a.Description); d != "" {
			b.WriteString(": " + d)
		}
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// camObserverContinuityBlock renders the stream's recent entries as the
// model's own memory, oldest first: one-line "[15:00–15:05] headline" rows
// (consecutive motion-gated idle entries compressed into a single "[from–to]
// No activity (N idle cycles)" line) with the LAST fullContext entries quoted
// verbatim (reports truncated to 2000 runes). recent arrives newest-first
// (listRecentLogEntries) and is already capped by the caller's headline
// context. Returns "" when the stream has no history yet.
func camObserverContinuityBlock(recent []logEntry, loc *time.Location, fullContext int) string {
	if len(recent) == 0 {
		return ""
	}
	ordered := make([]logEntry, len(recent))
	for i, e := range recent {
		ordered[len(recent)-1-i] = e
	}
	if fullContext < 0 {
		fullContext = 0
	}
	if fullContext > len(ordered) {
		fullContext = len(ordered)
	}
	headlines := ordered[:len(ordered)-fullContext]
	full := ordered[len(ordered)-fullContext:]

	var b strings.Builder
	b.WriteString("RECENT LOG (your OWN earlier entries, oldest first). CONTINUE this narrative: when an activity is unchanged ")
	b.WriteString("say it is STILL happening — do not re-describe it from scratch; note when something previously logged has ended.\n")
	i := 0
	for i < len(headlines) {
		e := headlines[i]
		if camObserverEntryIdle(e) {
			j := i
			for j+1 < len(headlines) && camObserverEntryIdle(headlines[j+1]) {
				j++
			}
			if j > i {
				fmt.Fprintf(&b, "[%s–%s] %s (%d idle cycles)\n",
					camObserverClock(headlines[i].FromTS, loc), camObserverClock(headlines[j].ToTS, loc),
					camObserverIdleHeadline, j-i+1)
				i = j + 1
				continue
			}
		}
		fmt.Fprintf(&b, "[%s–%s] %s\n", camObserverClock(e.FromTS, loc), camObserverClock(e.ToTS, loc),
			firstNonEmpty(strings.TrimSpace(e.Headline), "(no headline)"))
		i++
	}
	for _, e := range full {
		fmt.Fprintf(&b, "\n[%s–%s] %s\n%s\n", camObserverClock(e.FromTS, loc), camObserverClock(e.ToTS, loc),
			firstNonEmpty(strings.TrimSpace(e.Headline), "(no headline)"),
			camTruncateRunes(strings.TrimSpace(e.Report), 2000))
	}
	return strings.TrimRight(b.String(), "\n")
}

// camObserverEntryIdle reports whether an entry is a motion-gated idle row
// (the kind the continuity block compresses).
func camObserverEntryIdle(e logEntry) bool {
	return e.MotionGated && e.Headline == camObserverIdleHeadline
}

// camObserverClock renders a stored UTC RFC3339 instant as HH:MM in loc.
func camObserverClock(utcRFC3339 string, loc *time.Location) string {
	t, err := time.Parse(time.RFC3339, utcRFC3339)
	if err != nil {
		return utcRFC3339
	}
	return t.In(loc).Format("15:04")
}

// camObserverUserPrompt builds the per-cycle user message: the exact window
// (site-local + UTC), one legend line per attached sheet (the legend binds
// cells to camera+time — without it the composites are anonymous thumbnails),
// and the DVR motion digest when events exist.
func camObserverUserPrompt(from, to time.Time, loc *time.Location, tzName string, legends []string, motionDigest, identities string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "LOG WINDOW: %s – %s (%s) = %s – %s UTC. Describe THIS window only.\n\n",
		from.In(loc).Format("2006-01-02 15:04:05"), to.In(loc).Format("15:04:05"), tzName,
		from.UTC().Format(time.RFC3339), to.UTC().Format(time.RFC3339))
	if len(legends) == 0 {
		b.WriteString("No composite sheets could be assembled for this window.\n")
	}
	for _, l := range legends {
		b.WriteString(l)
		b.WriteString("\n")
	}
	b.WriteString("\n")
	if motionDigest != "" {
		b.WriteString(motionDigest)
		b.WriteString("\n\n")
	}
	if identities != "" {
		b.WriteString(identities)
		b.WriteString("\n\n")
	}
	b.WriteString("Write the log entry for this window now. ")
	b.WriteString(camObserverReplyLine)
	return b.String()
}

// camObserverMotionDigest renders the window's DVR motion episodes as digest
// lines ("- Gate: 14:01:03–14:01:20 VMD"), capped so a busy window can't bloat
// the prompt. Returns "" when there are no events.
func camObserverMotionDigest(events []camMotionEvent, camByID map[string]camera, loc *time.Location, tzName string) string {
	if len(events) == 0 {
		return ""
	}
	const maxLines = 30
	var b strings.Builder
	fmt.Fprintf(&b, "MOTION EVENTS the DVR detected in this window (times in %s):\n", tzName)
	for i, e := range events {
		if i >= maxLines {
			fmt.Fprintf(&b, "…(+%d more)\n", len(events)-maxLines)
			break
		}
		name := e.CameraID
		if c, ok := camByID[e.CameraID]; ok {
			name = camDisplayName(c)
		}
		fmt.Fprintf(&b, "- %s: %s\n", name, camMotionEpisodeLabel(e, loc))
	}
	return strings.TrimRight(b.String(), "\n")
}

// camObserverIdleReport is the English report body for a motion-gated idle
// entry — it names the cameras, the exact window, and the timezone so the
// entry stands alone in the archive.
func camObserverIdleReport(cams []camera, from, to time.Time, loc *time.Location, tzName string) string {
	names := make([]string, 0, len(cams))
	for _, c := range cams {
		names = append(names, camDisplayName(c))
	}
	return fmt.Sprintf("No motion was detected on %s between %s and %s (%s); the window was recorded as idle without analysis (motion-gated).",
		strings.Join(names, ", "), from.In(loc).Format("15:04"), to.In(loc).Format("15:04"), tzName)
}

// camObserverDetailPrompt builds the second-round user message for the single
// drill-down: the original context, the model's own draft, and the legend for
// the attached full-quality frames — with the explicit "finalize now" mandate
// (a second round of needs_detail is ignored by the caller).
func camObserverDetailPrompt(user string, draft observerReport, detailLegend string) string {
	var b strings.Builder
	b.WriteString(user)
	b.WriteString("\n\nYOUR DRAFT ENTRY was:\n")
	b.WriteString(mustJSON(map[string]any{"headline": draft.Headline, "report": draft.Report}))
	b.WriteString("\n\nThe full-quality frames you requested are attached after the sheets.\n")
	if detailLegend != "" {
		b.WriteString(detailLegend)
		b.WriteString("\n")
	}
	b.WriteString("Write the FINAL JSON entry now — same contract, and leave needs_detail empty (no further frames will be provided).")
	return b.String()
}

// parseObserverReport extracts and decodes the observer's single JSON reply
// (investigate-style: tolerate fences/prose via the balanced-brace extractor).
// A reply whose headline AND report are both empty is a parse failure — the
// caller's repair/salvage ladder handles it. Headline/report are trimmed and
// rune-capped so a runaway reply can never bloat the row.
func parseObserverReport(raw string) (observerReport, error) {
	obj, ok := extractFirstJSONObject(raw)
	if !ok {
		return observerReport{}, errors.New("no JSON object found in observer reply")
	}
	var rep observerReport
	if err := json.Unmarshal(obj, &rep); err != nil {
		return observerReport{}, fmt.Errorf("decode observer report JSON: %w", err)
	}
	rep.Headline = strings.TrimSpace(rep.Headline)
	rep.Report = strings.TrimSpace(rep.Report)
	if rep.Headline == "" && rep.Report == "" {
		return observerReport{}, errors.New("observer report JSON has empty headline and report")
	}
	if rep.Headline == "" {
		rep.Headline = camFirstLine(rep.Report)
	}
	if rep.Report == "" {
		rep.Report = rep.Headline
	}
	rep.Headline = camTruncateRunes(rep.Headline, 200)
	rep.Report = camTruncateRunes(rep.Report, 8000)
	return rep, nil
}
