package main

// camera_observer_tool.go — the `activity_log` Ask-AI investigation tool
// (dispatched by camExecuteInvestigateTool, camera_investigate.go): search the
// site's continuous activity journal (camera_log_entries) and its rollups
// (camera_log_hours / camera_log_days) as TEXT. Fetches:0 by design — the
// journal already describes what happened every few minutes, so "what happened
// last Tuesday 3–5pm" is answered from cheap text logs instead of re-reading
// frames; the investigator then verifies only the pivotal moments with
// past_frames/avatar_check.
//
// Granularities (investigateArgs.Granularity):
//   - "entries" (default): raw journal entries, oldest first, capped at 200
//     with a truncation note; full reports are quoted only when the result is
//     small (≤ 10 rows) — otherwise headlines keep the transcript compact.
//   - "hours": the hourly rollup summaries, verbatim (they are written in the
//     site's report language, which may differ from English).
//   - "days": the daily reports, verbatim.
//
// Times render site-locally (camSiteTimezone) so the model reasons in the same
// clock the rest of the investigation uses. Empty results return a helpful
// pointer to the imagery tools instead of a bare "nothing found".

import (
	"database/sql"
	"fmt"
	"strings"
	"time"
)

const (
	camActivityToolMaxRows     = 200  // entries/hours/days rows per call (newest kept)
	camActivityToolVerbatimMax = 10   // entries: quote full reports only at/below this count
	camActivityToolReportCap   = 2000 // runes per quoted report (continuity-block cap)
)

// camToolActivityLog executes one activity_log call. camera_ids is optional and
// narrows ENTRIES to windows that sampled that camera (first valid id wins);
// hours/days are stream-level rollups, so the camera filter does not apply to
// them. Costs no media budget (Fetches:0) and returns text only.
func camToolActivityLog(cfg config, db *sql.DB, site camSite, args investigateArgs, allowed map[string]bool, camByID map[string]camera, dvrByID map[string]CamDVR) investigateToolResult {
	gran := strings.ToLower(strings.TrimSpace(args.Granularity))
	if gran == "" {
		gran = "entries"
	}
	if gran != "entries" && gran != "hours" && gran != "days" {
		return investigateToolResult{Summary: `activity_log: granularity must be "entries", "hours" or "days".`}
	}
	from, to, terr := camParseWindow(args.From, args.To, cfg, false)
	if terr != nil {
		return investigateToolResult{Summary: "activity_log: " + terr.Error()}
	}
	cameraID := ""
	if len(args.CameraIDs) > 0 {
		ids := camFilterAllowed(args.CameraIDs, allowed)
		if len(ids) == 0 {
			return investigateToolResult{Summary: "activity_log: no valid camera_ids given (must be exact ids from the roster; omit to search the whole log)."}
		}
		cameraID = ids[0]
	}

	dvrs := make([]CamDVR, 0, len(dvrByID))
	for _, d := range dvrByID {
		dvrs = append(dvrs, d)
	}
	loc, tzName := camSiteTimezone(dvrs)
	fromUTC := from.UTC().Format(time.RFC3339)
	toUTC := to.UTC().Format(time.RFC3339)
	window := fmt.Sprintf("%s – %s (%s)", from.In(loc).Format("2006-01-02 15:04"), to.In(loc).Format("2006-01-02 15:04"), tzName)

	switch gran {
	case "hours":
		return camActivityLogHours(db, site, fromUTC, toUTC, window, loc)
	case "days":
		return camActivityLogDays(db, site, from.In(loc).Format("2006-01-02"), to.In(loc).Format("2006-01-02"), window)
	}
	return camActivityLogEntries(db, site, cameraID, camByID, fromUTC, toUTC, window, loc)
}

// camActivityLogEntries renders the raw journal entries for the window, oldest
// first: "[date HH:MM–HH:MM] headline" lines (motion-gated idle windows
// marked), with full reports quoted only when the result set is small.
func camActivityLogEntries(db *sql.DB, site camSite, cameraID string, camByID map[string]camera, fromUTC, toUTC, window string, loc *time.Location) investigateToolResult {
	entries, err := listLogEntriesBySite(db, site.ID, "", cameraID, fromUTC, toUTC, camActivityToolMaxRows+1)
	if err != nil {
		return investigateToolResult{Summary: "activity_log: " + err.Error()}
	}
	scope := "all cameras"
	if cameraID != "" {
		scope = cameraID
		if c, ok := camByID[cameraID]; ok {
			scope = camDisplayName(c)
		}
	}
	if len(entries) == 0 {
		return investigateToolResult{Summary: fmt.Sprintf(
			"activity_log: no journal entries recorded for %s in %s. The activity log may not have been running (or watching these cameras) then — investigate the window directly with motion_search / contact_sheet / past_frames instead.",
			scope, window)}
	}
	truncated := len(entries) > camActivityToolMaxRows
	if truncated {
		entries = entries[:camActivityToolMaxRows]
	}
	// listLogEntriesBySite returns newest-first; the narrative reads oldest-first.
	for i, j := 0, len(entries)-1; i < j; i, j = i+1, j-1 {
		entries[i], entries[j] = entries[j], entries[i]
	}

	var b strings.Builder
	fmt.Fprintf(&b, "activity_log: %d journal entr%s for %s in %s, oldest first.\n",
		len(entries), camPluralIES(len(entries)), scope, window)
	if truncated {
		fmt.Fprintf(&b, "NOTE: only the most recent %d entries of this window are shown — narrow the window (or use granularity \"hours\") for full coverage.\n", camActivityToolMaxRows)
	}
	verbatim := len(entries) <= camActivityToolVerbatimMax
	for _, e := range entries {
		fmt.Fprintf(&b, "- [%s] %s", camActivityToolEntryWindow(e.FromTS, e.ToTS, loc),
			firstNonEmpty(strings.TrimSpace(e.Headline), "(no headline)"))
		if e.MotionGated {
			b.WriteString(" (motion-gated)")
		}
		b.WriteString("\n")
		if verbatim {
			if rep := strings.TrimSpace(e.Report); rep != "" {
				b.WriteString("  ")
				b.WriteString(camTruncateRunes(rep, camActivityToolReportCap))
				b.WriteString("\n")
			}
		}
	}
	if !verbatim {
		b.WriteString("(Headlines only — call activity_log again with a narrower window to read the full entry reports.)")
	}
	return investigateToolResult{Summary: strings.TrimRight(b.String(), "\n")}
}

// camActivityLogHours renders the hourly rollup summaries covering the window,
// oldest first and verbatim — they are already operator-facing prose (written
// in the site's report language, which may differ from English).
func camActivityLogHours(db *sql.DB, site camSite, fromUTC, toUTC, window string, loc *time.Location) investigateToolResult {
	hours, err := listLogHoursBySite(db, site.ID, "", fromUTC, toUTC, camActivityToolMaxRows)
	if err != nil {
		return investigateToolResult{Summary: "activity_log: " + err.Error()}
	}
	if len(hours) == 0 {
		return investigateToolResult{Summary: fmt.Sprintf(
			"activity_log: no hourly summaries exist for %s — rollups only exist where the journal ran. Try granularity \"entries\", or investigate the window directly with motion_search / past_frames.",
			window)}
	}
	for i, j := 0, len(hours)-1; i < j; i, j = i+1, j-1 {
		hours[i], hours[j] = hours[j], hours[i]
	}
	var b strings.Builder
	fmt.Fprintf(&b, "activity_log: %d hourly summar%s in %s, oldest first (summaries may be written in the site's report language).\n",
		len(hours), camPluralIES(len(hours)), window)
	for _, h := range hours {
		label := camActivityToolHourLabel(h.HourStart, loc)
		switch h.Status {
		case "done":
			fmt.Fprintf(&b, "- [%s] %s\n", label, strings.TrimSpace(h.Summary))
		case "empty":
			fmt.Fprintf(&b, "- [%s] (no journal entries — quiet hour)\n", label)
		case "pending":
			fmt.Fprintf(&b, "- [%s] (summary still being generated)\n", label)
		default: // error
			fmt.Fprintf(&b, "- [%s] (rollup failed: %s — read this hour with granularity \"entries\")\n", label, firstNonEmpty(strings.TrimSpace(h.Error), "unknown error"))
		}
	}
	return investigateToolResult{Summary: strings.TrimRight(b.String(), "\n")}
}

// camActivityLogDays renders the daily reports whose site-local day falls in
// [fromDay,toDay], oldest first and verbatim.
func camActivityLogDays(db *sql.DB, site camSite, fromDay, toDay, window string) investigateToolResult {
	days, err := listLogDaysBySite(db, site.ID, "", fromDay, toDay, camActivityToolMaxRows)
	if err != nil {
		return investigateToolResult{Summary: "activity_log: " + err.Error()}
	}
	if len(days) == 0 {
		return investigateToolResult{Summary: fmt.Sprintf(
			"activity_log: no daily reports exist for %s — reports only exist where the journal ran. Try granularity \"entries\" or \"hours\", or investigate the window directly with motion_search / past_frames.",
			window)}
	}
	for i, j := 0, len(days)-1; i < j; i, j = i+1, j-1 {
		days[i], days[j] = days[j], days[i]
	}
	var b strings.Builder
	fmt.Fprintf(&b, "activity_log: %d daily report%s for %s–%s, oldest first (reports may be written in the site's report language).\n",
		len(days), camPluralS(len(days)), fromDay, toDay)
	for _, d := range days {
		switch d.Status {
		case "done":
			fmt.Fprintf(&b, "- %s: %s\n  %s\n", d.Day, firstNonEmpty(strings.TrimSpace(d.Headline), "(no headline)"),
				camTruncateRunes(strings.TrimSpace(d.Report), 8000))
		case "empty":
			fmt.Fprintf(&b, "- %s: no activity recorded.\n", d.Day)
		case "pending":
			fmt.Fprintf(&b, "- %s: (report still being generated)\n", d.Day)
		default: // error
			fmt.Fprintf(&b, "- %s: (report generation failed: %s — read this day with granularity \"entries\" or \"hours\")\n", d.Day, firstNonEmpty(strings.TrimSpace(d.Error), "unknown error"))
		}
	}
	return investigateToolResult{Summary: strings.TrimRight(b.String(), "\n")}
}

// camActivityToolEntryWindow renders an entry's [from,to) UTC window as a
// site-local "2006-01-02 15:04–15:04" label (raw timestamps on parse failure,
// so a corrupt row still renders something greppable).
func camActivityToolEntryWindow(fromTS, toTS string, loc *time.Location) string {
	f, ferr := time.Parse(time.RFC3339, fromTS)
	t, terr := time.Parse(time.RFC3339, toTS)
	if ferr != nil || terr != nil {
		return fromTS + "–" + toTS
	}
	return f.In(loc).Format("2006-01-02 15:04") + "–" + t.In(loc).Format("15:04")
}

// camActivityToolHourLabel renders an hour row's UTC hour_start as the
// site-local "2006-01-02 15:00–16:00" hour label.
func camActivityToolHourLabel(hourStartUTC string, loc *time.Location) string {
	t, err := time.Parse(time.RFC3339, hourStartUTC)
	if err != nil {
		return hourStartUTC
	}
	return t.In(loc).Format("2006-01-02 15:04") + "–" + t.Add(time.Hour).In(loc).Format("15:04")
}

// camPluralIES returns "ies"/"y" for entry/summary-style pluralization.
func camPluralIES(n int) string {
	if n == 1 {
		return "y"
	}
	return "ies"
}

// camPluralS returns "s" except for exactly one.
func camPluralS(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
