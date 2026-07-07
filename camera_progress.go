package main

// camera_progress.go (WS4) — the opt-in live-progress webhook for a running
// investigation. A site whose callback has stream_progress=1 receives, ALONGSIDE
// the reliable settle webhook, a best-effort stream of camera_investigation_progress
// events: "started" (once, carrying the public view_url), coalesced "media" bursts
// (only the operator-facing key tools, throttled to CameraProgressMinInterval so a
// chatty run can't spam WhatsApp), and "subagents" verdict summaries (WS3). None of
// this is load-bearing: delivery uses a short {0,15s} retry ladder and every method
// is a no-op on a nil receiver, so a run with no opted-in callback pays nothing and
// a slow Connect never wedges the loop.
//
// The notifier is created ONCE per runInvestigation pass (newCamProgressNotifier
// returns nil unless the site opted in) and threaded through the loop; the settle
// webhook (camera_serviceapi.go) remains the single authoritative "run ended" event.

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"
)

// camProgressEvidenceMax caps evidence items in one coalesced "media" event (the
// same ceiling as the settle payload).
const camProgressEvidenceMax = 10

// camProgressRetryDelays is the best-effort delivery ladder for progress events:
// try now, retry once after 15s, then give up — short so a stream event never ties
// up a delivery goroutine for the settle ladder's minutes.
var camProgressRetryDelays = []time.Duration{0, 15 * time.Second}

// camProgressKeyTools is the allowlist of tools whose media is worth streaming to
// the operator mid-run: the ones that produce a human-facing artifact (an annotated
// frame, an avatar match, a contact sheet, a sub-agent's evidence). High-frequency
// scaffolding fetches (roster/snapshot/past_frames/…) are intentionally excluded so
// the stream stays signal, not noise.
var camProgressKeyTools = map[string]bool{
	"annotate":      true,
	"avatar_check":  true,
	"avatar_find":   true,
	"contact_sheet": true,
	"delegate":      true,
}

// camSubagentSummary is the compact per-child verdict the "subagents" progress
// phase relays. WS3's camToolDelegate maps its richer verdicts onto these so the
// notifier stays decoupled from the delegation internals.
type camSubagentSummary struct {
	ID            string
	Question      string
	Status        string
	Summary       string
	EvidenceCount int
}

// camProgressNotifier buffers and coalesces progress events for one run. All fields
// are guarded by mu (the timer callback and the loop's ObserveMedia calls race).
type camProgressNotifier struct {
	mu       sync.Mutex
	cfg      config
	cb       camSiteCallback
	inv      camInvestigation
	viewURL  string
	status   string         // carried into every event; "running" for the whole active pass
	pending  []evidenceItem // buffered media awaiting the next flush
	lastSent time.Time      // last event delivery, so "media" bursts respect the min interval
	timer    *time.Timer    // armed when a burst arrives inside the interval; fires the deferred flush
}

// camProgressMinInterval resolves the minimum gap between coalesced "media" events,
// defaulting to 20s. A non-positive override falls back to the default rather than
// disabling throttling (an unthrottled stream would flood the operator's chat).
func camProgressMinInterval(cfg config) time.Duration {
	if cfg.CameraProgressMinInterval > 0 {
		return cfg.CameraProgressMinInterval
	}
	return 20 * time.Second
}

// newCamProgressNotifier returns a live notifier only when the run's site has a
// callback that is enabled, has a URL, AND opted into stream_progress; otherwise it
// returns nil (every notifier method is nil-safe, so callers never branch). db is
// used synchronously here and not retained — the notifier opens its own short-lived
// connections for enrichment at flush time.
func newCamProgressNotifier(cfg config, db *sql.DB, inv camInvestigation, r *http.Request) *camProgressNotifier {
	cb, err := getCameraSiteCallback(db, inv.SiteID)
	if err != nil || !cb.Enabled || !cb.StreamProgress || strings.TrimSpace(cb.URL) == "" {
		return nil
	}
	return &camProgressNotifier{
		cfg:     cfg,
		cb:      cb,
		inv:     inv,
		viewURL: camInvestigationViewURL(cfg, r, inv),
		status:  "running",
	}
}

// basePayload builds the fields every camera_investigation_progress event shares.
func (n *camProgressNotifier) basePayload(phase string) map[string]any {
	return map[string]any{
		"event":            "camera_investigation_progress",
		"phase":            phase,
		"investigation_id": n.inv.ID,
		"site_id":          n.inv.SiteID,
		"status":           n.status,
		"view_url":         n.viewURL,
		"at":               time.Now().UTC().Format(time.RFC3339),
	}
}

// deliver marshals a payload and dispatches it on its own goroutine with the short
// progress ladder — best-effort, so a marshal failure or an unreachable Connect only
// drops the one event.
func (n *camProgressNotifier) deliver(payload map[string]any) {
	body, err := json.Marshal(payload)
	if err != nil {
		camlog("warn", "investigation_progress", map[string]any{"investigation_id": n.inv.ID, "ok": false, "error": err.Error()})
		return
	}
	go camPostSignedCallbackDelays(n.cfg, n.cb, body, camProgressRetryDelays, "investigation_progress", map[string]any{
		"investigation_id": n.inv.ID, "site_id": n.inv.SiteID, "phase": payload["phase"],
	})
}

// Start fires the one-time "started" event carrying the question and view_url.
// Called from runInvestigation only when the pass begins fresh (turnsSoFar==0), so a
// resumed run never re-announces itself.
func (n *camProgressNotifier) Start(question string) {
	if n == nil {
		return
	}
	n.mu.Lock()
	n.lastSent = time.Now()
	n.mu.Unlock()
	p := n.basePayload("started")
	p["question"] = question
	n.deliver(p)
}

// ObserveMedia records a tool row's minted media for the progress stream. Only the
// key tools' output is buffered; a burst arriving after the min interval flushes
// immediately, otherwise a one-shot timer flushes the coalesced batch when the
// interval elapses. sourceInvID identifies which (parent or child) run produced the
// media — WS3 passes a child id; WS4 ignores it beyond intent documentation.
func (n *camProgressNotifier) ObserveMedia(sourceInvID, tool string, media []evidenceItem) {
	if n == nil || len(media) == 0 || !camProgressKeyTools[strings.TrimSpace(tool)] {
		return
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	n.pending = append(n.pending, media...)
	interval := camProgressMinInterval(n.cfg)
	if since := time.Since(n.lastSent); since >= interval {
		n.flushLocked()
	} else {
		n.armTimerLocked(interval - since)
	}
}

// NotifySubagents fires the "subagents" event with each delegated child's verdict
// summary. Called by WS3's camToolDelegate once its children finish.
func (n *camProgressNotifier) NotifySubagents(subs []camSubagentSummary) {
	if n == nil || len(subs) == 0 {
		return
	}
	arr := make([]map[string]any, 0, len(subs))
	for _, s := range subs {
		arr = append(arr, map[string]any{
			"id": s.ID, "question": s.Question, "status": s.Status,
			"summary": truncateString(s.Summary, 400), "evidence_count": s.EvidenceCount,
		})
	}
	p := n.basePayload("subagents")
	p["subagents"] = arr
	n.deliver(p)
	n.mu.Lock()
	n.lastSent = time.Now()
	n.mu.Unlock()
}

// Flush delivers any buffered media now. force sends regardless of the min interval
// (the exit-path flush, so no evidence is stranded before settle); force==false only
// flushes if the interval has elapsed. Called on every runInvestigation exit path.
func (n *camProgressNotifier) Flush(force bool) {
	if n == nil {
		return
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if force || time.Since(n.lastSent) >= camProgressMinInterval(n.cfg) {
		n.flushLocked()
	}
}

// armTimerLocked schedules a single deferred flush after d, unless one is already
// pending. Caller holds mu.
func (n *camProgressNotifier) armTimerLocked(d time.Duration) {
	if n.timer != nil {
		return
	}
	n.timer = time.AfterFunc(d, func() {
		n.mu.Lock()
		n.timer = nil
		n.flushLocked()
		n.mu.Unlock()
	})
}

// flushLocked emits one coalesced "media" event for the buffered items (enriching
// each with its capture's content_type/kind/camera_id) and resets the buffer/timer.
// A no-op when nothing is pending. Caller holds mu.
func (n *camProgressNotifier) flushLocked() {
	if n.timer != nil {
		n.timer.Stop()
		n.timer = nil
	}
	if len(n.pending) == 0 {
		return
	}
	items := n.pending
	n.pending = nil
	n.lastSent = time.Now()

	var evidence []map[string]any
	if db, err := openProxyDB(n.cfg); err == nil {
		evidence = camProgressEvidence(n.cfg, db, items, camProgressEvidenceMax)
		db.Close()
	} else {
		evidence = camProgressEvidence(n.cfg, nil, items, camProgressEvidenceMax)
	}
	p := n.basePayload("media")
	p["evidence"] = evidence
	n.deliver(p)
}

// camProgressEvidence maps buffered media items to settle-shaped evidence entries
// (token, media_url, caption, and — when db is non-nil and the capture row still
// exists — content_type/kind/camera_id/public_url), de-duped by token and capped at
// max. A nil db (open failed) yields the bare token/url/caption fields.
func camProgressEvidence(cfg config, db *sql.DB, items []evidenceItem, max int) []map[string]any {
	out := make([]map[string]any, 0, max)
	seen := map[string]bool{}
	for _, it := range items {
		tok := camMediaToken(it.MediaURL)
		if tok == "" || seen[tok] {
			continue
		}
		seen[tok] = true
		entry := map[string]any{"token": tok, "media_url": camMediaURL(cfg, nil, tok), "caption": it.Caption}
		if db != nil {
			if cap, err := getCameraCaptureByToken(db, tok); err == nil {
				entry["content_type"] = cap.ContentType
				entry["kind"] = cap.Kind
				entry["camera_id"] = cap.CameraID
				if strings.TrimSpace(cap.S3URL) != "" {
					entry["public_url"] = cap.S3URL
				}
			}
		}
		out = append(out, entry)
		if len(out) >= max {
			break
		}
	}
	return out
}
