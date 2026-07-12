package main

// camera_presence.go — proactive known-person presence / attendance.
//
// The observer already recognizes enrolled people every cycle
// (camObserverRecognizeFaces → []camObserverIdentity, camera_observer_faces.go).
// Instead of discarding that structure into a prompt string, we persist it as
// presence INTERVALS in the (previously inert) camera_sightings table: one row
// per known person per building-visit — opened at first sighting (arrival =
// started_at), extended each cycle they're still seen, and closed after an
// absence grace (departure = ended_at). Arrival and departure are pushed once to
// the site's callback (→ WhatsApp, camNotifyPresence) and pull-synced to Connect
// via the shared seq cursor, exactly like the activity journal.
//
// NEUTRALITY INVARIANT (camera_neutrality_test.go): camera_sightings holds
// OBSERVATIONS only — no severity/threat/alert semantics. Arrival/departure are
// neutral facts; roles are never stored here (they JOIN from camera_avatars at
// read time, so re-labeling applies retroactively).

import (
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// camPresenceNotifyFn is the push seam (mirrors camObserverAnalyzeFn) so tests
// can observe arrival/departure emissions without a live callback.
var camPresenceNotifyFn = camNotifyPresence

// camPresenceDispatch runs a push off the cycle's critical path (the webhook
// retry ladder must never block a journal cycle). Tests override it to run
// synchronously so emissions can be asserted deterministically.
var camPresenceDispatch = func(fn func()) { go fn() }

func camPresenceEnabled(cfg config) bool { return cfg.CameraPresenceEnabled }

// camPresenceAbsenceGrace is how long a person must be UNSEEN before their open
// interval is closed as a departure. Default 10 min ≈ two 5-min observer cycles,
// so a single missed cycle never splits one visit into two.
func camPresenceAbsenceGrace(cfg config) time.Duration {
	if cfg.CameraPresenceAbsenceGrace > 0 {
		return cfg.CameraPresenceAbsenceGrace
	}
	return 10 * time.Minute
}

// ─────────────────────────── store (camera_sightings) ───────────────────────────

// camSighting is the subset of camera_sightings the presence layer reads/writes.
// (The table carries more Person-Identity-v2 columns — cluster_id, embedding,
// bbox — left at their defaults here.)
type camSighting struct {
	ID          string
	Seq         int64
	SiteID      string
	CameraID    string
	AvatarID    string
	Area        string
	StartedAt   string // arrival, RFC3339 UTC
	EndedAt     string // departure ('' = still present)
	LastSeenAt  string
	FrameCount  int
	BestScore   float64
	BestFrameTS string
	CreatedAt   string
}

const camSightingCols = `id, seq, site_id, camera_id, avatar_id, area, started_at, ended_at, last_seen_at, frame_count, best_score, best_frame_ts, created_at`

func scanSighting(sc rowScanner) (camSighting, error) {
	var s camSighting
	err := sc.Scan(&s.ID, &s.Seq, &s.SiteID, &s.CameraID, &s.AvatarID, &s.Area,
		&s.StartedAt, &s.EndedAt, &s.LastSeenAt, &s.FrameCount, &s.BestScore, &s.BestFrameTS, &s.CreatedAt)
	return s, err
}

func querySightings(db *sql.DB, query string, args ...any) ([]camSighting, error) {
	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []camSighting
	for rows.Next() {
		s, err := scanSighting(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// insertSighting opens a new presence interval, stamping a fresh sync seq in-tx
// so Connect pulls the arrival. Returns the new id + seq.
func insertSighting(db *sql.DB, s camSighting) (string, int64, error) {
	if s.ID == "" {
		s.ID = randomID("sight")
	}
	if s.CreatedAt == "" {
		s.CreatedAt = nowRFC3339()
	}
	if s.LastSeenAt == "" {
		s.LastSeenAt = s.StartedAt
	}
	if s.FrameCount <= 0 {
		s.FrameCount = 1
	}
	tx, err := db.Begin()
	if err != nil {
		return "", 0, err
	}
	seq, err := nextCamSyncSeq(tx)
	if err != nil {
		tx.Rollback()
		return "", 0, err
	}
	if _, err := tx.Exec(`INSERT INTO camera_sightings
		(id, seq, site_id, camera_id, subject_kind, avatar_id, area, started_at, ended_at, last_seen_at, frame_count, best_score, best_frame_ts, created_at)
		VALUES (?, ?, ?, ?, 'known', ?, ?, ?, '', ?, ?, ?, ?, ?)`,
		s.ID, seq, s.SiteID, s.CameraID, s.AvatarID, s.Area, s.StartedAt, s.LastSeenAt,
		s.FrameCount, s.BestScore, s.BestFrameTS, s.CreatedAt); err != nil {
		tx.Rollback()
		return "", 0, err
	}
	if err := tx.Commit(); err != nil {
		return "", 0, err
	}
	return s.ID, seq, nil
}

// updateSightingSeen extends an OPEN interval — advance last_seen_at, bump the
// frame count. Deliberately does NOT re-stamp seq: mid-interval extends are not
// synced (only the arrival open and the departure close carry a fresh seq).
func updateSightingSeen(db *sql.DB, id, lastSeenAt string) error {
	_, err := db.Exec(`UPDATE camera_sightings SET last_seen_at = ?, frame_count = frame_count + 1
		WHERE id = ? AND ended_at = ''`, lastSeenAt, id)
	return err
}

// closeSighting closes an OPEN interval (departure), re-stamping seq so the
// now-terminal row reappears past any cursor Connect holds. Returns whether THIS
// call performed the close (RowsAffected==1) so the caller pushes the departure
// exactly once — a double-close never double-notifies.
func closeSighting(db *sql.DB, id, endedAt string) (bool, error) {
	tx, err := db.Begin()
	if err != nil {
		return false, err
	}
	seq, err := nextCamSyncSeq(tx)
	if err != nil {
		tx.Rollback()
		return false, err
	}
	res, err := tx.Exec(`UPDATE camera_sightings SET ended_at = ?, seq = ?
		WHERE id = ? AND ended_at = ''`, endedAt, seq, id)
	if err != nil {
		tx.Rollback()
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		tx.Rollback()
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return n == 1, nil
}

// getOpenSighting returns the person's currently-open interval (most recent), or
// sql.ErrNoRows when they have none open.
func getOpenSighting(db *sql.DB, siteID, avatarID string) (camSighting, error) {
	return scanSighting(db.QueryRow(`SELECT `+camSightingCols+` FROM camera_sightings
		WHERE site_id = ? AND avatar_id = ? AND ended_at = '' ORDER BY started_at DESC LIMIT 1`, siteID, avatarID))
}

// listStaleOpenSightings finds open intervals last seen before cutoff (across all
// sites) — the absence-grace close sweep's input.
func listStaleOpenSightings(db *sql.DB, cutoffUTC string) ([]camSighting, error) {
	return querySightings(db, `SELECT `+camSightingCols+` FROM camera_sightings
		WHERE ended_at = '' AND last_seen_at != '' AND last_seen_at < ? ORDER BY last_seen_at`, cutoffUTC)
}

// listSightingsAfterSeq is the Connect pull-sync cursor query (arrivals + closed
// departures; open extends carry no new seq so they don't churn the sync).
func listSightingsAfterSeq(db *sql.DB, siteID string, afterSeq int64, limit int) ([]camSighting, error) {
	if limit <= 0 {
		limit = 200
	}
	return querySightings(db, `SELECT `+camSightingCols+` FROM camera_sightings
		WHERE site_id = ? AND seq > ? ORDER BY seq LIMIT ?`, siteID, afterSeq, limit)
}

// camSightingSyncJSON is the wire shape for the pull-sync + Connect archive.
func camSightingSyncJSON(s camSighting) map[string]any {
	return map[string]any{
		"id": s.ID, "seq": s.Seq, "site_id": s.SiteID, "camera_id": s.CameraID,
		"avatar_id": s.AvatarID, "area": s.Area, "arrived_at": s.StartedAt,
		"left_at": s.EndedAt, "last_seen_at": s.LastSeenAt, "frame_count": s.FrameCount,
		"best_score": s.BestScore, "created_at": s.CreatedAt,
	}
}

// ─────────────────────────── detector (per observer cycle) ───────────────────────────

// camPresenceObserve turns this cycle's recognized identities into presence
// interval transitions. Runs inside camRunLogCycle right after face recognition,
// scoped to the cycle's site/cameras/window for free. Errors never fail the
// cycle — a presence hiccup must not cost a journal entry.
//
// Coverage note: motion-gated cycles skip face recognition entirely (no
// identities reach here), so a genuinely zero-motion arrival is invisible in v1.
// In practice an arrival IS motion, so this rarely misses a real one.
func camPresenceObserve(cfg config, db *sql.DB, siteID string, identities []camObserverIdentity, cams []camera, windowTo time.Time, loc *time.Location) {
	if !camPresenceEnabled(cfg) || strings.TrimSpace(siteID) == "" || len(identities) == 0 {
		return
	}
	camByID := make(map[string]camera, len(cams))
	for _, c := range cams {
		camByID[c.ID] = c
	}
	seenAt := windowTo.UTC().Format(time.RFC3339)
	today := windowTo.In(loc).Format("2006-01-02")
	dayOf := func(ts string) string {
		if t, err := time.Parse(time.RFC3339, ts); err == nil {
			return t.In(loc).Format("2006-01-02")
		}
		return ""
	}

	for _, id := range identities {
		avatarID := strings.TrimSpace(id.AvatarID)
		if avatarID == "" {
			continue
		}
		// Arrival = earliest sighting THIS cycle (min f.T), never after the window.
		arrival := id.FirstT
		if arrival.IsZero() {
			arrival = id.T
		}
		if arrival.IsZero() || arrival.After(windowTo) {
			arrival = windowTo
		}
		arrivalTS := arrival.UTC().Format(time.RFC3339)
		camID := id.FirstCameraID
		if camID == "" {
			camID = id.CameraID
		}
		area := ""
		if c, ok := camByID[camID]; ok {
			area = strings.TrimSpace(c.Area)
		}

		open, err := getOpenSighting(db, siteID, avatarID)
		if err == nil && dayOf(open.StartedAt) == today {
			// Same-day interval already open → extend, no push.
			if uerr := updateSightingSeen(db, open.ID, seenAt); uerr != nil {
				camlog("warn", "presence", map[string]any{"site_id": siteID, "avatar_id": avatarID, "op": "extend", "error": uerr.Error()})
			}
			continue
		}
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			camlog("warn", "presence", map[string]any{"site_id": siteID, "avatar_id": avatarID, "op": "open_lookup", "error": err.Error()})
			continue
		}
		// A stale open interval from a previous day → close it (departure) before
		// opening today's (e.g. an overnight open the sweep hasn't reached).
		if err == nil && open.ID != "" {
			if closed, cerr := closeSighting(db, open.ID, open.LastSeenAt); cerr == nil && closed {
				dep := open
				dep.EndedAt = dep.LastSeenAt
				camPresenceDispatch(func() { camPresenceNotifyFn(cfg, "departure", dep) })
			}
		}
		// Open today's interval — the arrival.
		newID, seq, ierr := insertSighting(db, camSighting{
			SiteID: siteID, CameraID: camID, AvatarID: avatarID, Area: area,
			StartedAt: arrivalTS, LastSeenAt: seenAt,
			BestScore: id.Similarity, BestFrameTS: arrivalTS,
		})
		if ierr != nil {
			camlog("warn", "presence", map[string]any{"site_id": siteID, "avatar_id": avatarID, "op": "open", "error": ierr.Error()})
			continue
		}
		camlog("info", "presence", map[string]any{
			"site_id": siteID, "avatar_id": avatarID, "person": id.Name,
			"op": "arrival", "at": arrivalTS, "camera": camID,
		})
		arr := camSighting{
			ID: newID, Seq: seq, SiteID: siteID, CameraID: camID, AvatarID: avatarID,
			Area: area, StartedAt: arrivalTS, LastSeenAt: seenAt, BestScore: id.Similarity,
		}
		camPresenceDispatch(func() { camPresenceNotifyFn(cfg, "arrival", arr) })
	}
}

// camPresenceSweep closes every open interval unseen for longer than the absence
// grace and pushes each departure once. Runs on the observer's 60s rollup ticker
// (reuses that pass's db handle).
func camPresenceSweep(cfg config, db *sql.DB) {
	if !camPresenceEnabled(cfg) {
		return
	}
	cutoff := time.Now().Add(-camPresenceAbsenceGrace(cfg)).UTC().Format(time.RFC3339)
	stale, err := listStaleOpenSightings(db, cutoff)
	if err != nil {
		camlog("warn", "presence", map[string]any{"op": "sweep", "error": err.Error()})
		return
	}
	for _, s := range stale {
		endedAt := s.LastSeenAt
		if endedAt == "" {
			endedAt = cutoff
		}
		closed, cerr := closeSighting(db, s.ID, endedAt)
		if cerr != nil {
			camlog("warn", "presence", map[string]any{"op": "close", "sighting_id": s.ID, "error": cerr.Error()})
			continue
		}
		if closed {
			s.EndedAt = endedAt
			camlog("info", "presence", map[string]any{"site_id": s.SiteID, "avatar_id": s.AvatarID, "op": "departure", "at": endedAt})
			dep := s
			camPresenceDispatch(func() { camPresenceNotifyFn(cfg, "departure", dep) })
		}
	}
}

// ─────────────────────────── push (→ Connect → WhatsApp) ───────────────────────────

// camNotifyPresence delivers the real-time camera_presence webhook (arrival |
// departure) to the site's registered callback. A site with no enabled callback
// is a cheap no-op. Clone of camNotifyMotion; always spawned `go ...`.
func camNotifyPresence(cfg config, kind string, s camSighting) {
	db, err := openProxyDB(cfg)
	if err != nil {
		camlog("warn", "presence_callback", map[string]any{"site_id": s.SiteID, "ok": false, "error": err.Error()})
		return
	}
	cb, cerr := getCameraSiteCallback(db, s.SiteID)
	name := s.AvatarID
	if av, aerr := getCameraAvatar(db, s.AvatarID); aerr == nil && strings.TrimSpace(av.Name) != "" {
		name = strings.TrimSpace(av.Name)
	}
	db.Close()
	if cerr != nil || !cb.Enabled || strings.TrimSpace(cb.URL) == "" {
		return // no registered callback — cheap no-op
	}
	at := s.StartedAt
	if kind == "departure" {
		if at = s.EndedAt; at == "" {
			at = s.LastSeenAt
		}
	}
	payload := map[string]any{
		"event":       "camera_presence",
		"kind":        kind, // arrival | departure
		"site_id":     s.SiteID,
		"sighting_id": s.ID,
		"person":      name,
		"avatar_id":   s.AvatarID,
		"camera_id":   s.CameraID,
		"area":        s.Area,
		"arrived_at":  s.StartedAt,
		"left_at":     s.EndedAt,
		"at":          at,
		"similarity":  s.BestScore,
	}
	body, merr := json.Marshal(payload)
	if merr != nil {
		camlog("error", "presence_callback", map[string]any{"site_id": s.SiteID, "ok": false, "error": merr.Error()})
		return
	}
	camPostSignedCallback(cfg, cb, body, "presence_callback", map[string]any{
		"site_id": s.SiteID, "avatar_id": s.AvatarID, "kind": kind,
	})
}
