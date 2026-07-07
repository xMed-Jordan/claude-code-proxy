package main

// camera_playbooks.go — operator-authored operational playbooks + approved
// reference media for the Ask-AI investigation loop (design-knowledge.md Part B).
//
// A playbook is a named procedure ("employee-workday", "device-standby-check")
// with a one-liner shown in every investigation prompt (camPlaybookCatalog) and
// full instructions + operator-approved reference images returned on demand by
// the `playbook` investigate tool (camToolPlaybook). Reference media rows point
// at PINNED camera_captures (expires_at='' — pinCameraCapture, camera_store.go);
// camReleaseCaptureIfUnreferenced restores normal retention when the last
// playbook reference to a token is removed.
//
// Schema lives in migrateCameraDB (camera_store.go): camera_playbooks +
// camera_playbook_media with UNIQUE(site_id, name).

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"
)

const (
	camPlaybookCatalogMax  = 150 // defensive cap on catalog entries per prompt
	camPlaybookOneLinerMax = 120 // when_to_use truncation in the catalog
	camPlaybookRefImgMax   = 6   // reference images returned per playbook tool call
)

// camPlaybook is an in-memory view of a camera_playbooks row.
type camPlaybook struct {
	ID           string
	SiteID       string
	DVRID        string // "" = site-wide; else a hint shown in the catalog line
	Name         string // model-facing identifier, unique per site
	WhenToUse    string // one-liner shown in the catalog
	Instructions string // full procedure returned by the playbook tool
	Enabled      bool
	CreatedAt    string
	UpdatedAt    string
}

// camPlaybookMedia is one operator-approved reference artifact attached to a
// playbook. capture_token references camera_captures.token (pinned).
type camPlaybookMedia struct {
	ID           string
	PlaybookID   string
	CaptureToken string
	Note         string // operator annotation, e.g. "correct standby position — handle UP"
	CreatedAt    string
}

// camPlaybookNameTakenError is the friendly UNIQUE(site_id, name) violation:
// handlers map it to a 409-style response; other callers get a readable message.
type camPlaybookNameTakenError struct{ Name string }

func (e camPlaybookNameTakenError) Error() string {
	return fmt.Sprintf("a playbook named %q already exists for this site", e.Name)
}

// camPlaybookDupErr reports whether err is SQLite's UNIQUE constraint violation
// (modernc.org/sqlite surfaces it only via the error text).
func camPlaybookDupErr(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}

// ─────────────────────────── CRUD ───────────────────────────

const camPlaybookCols = `id, site_id, dvr_id, name, when_to_use, instructions, enabled, created_at, updated_at`

func scanPlaybook(s rowScanner) (camPlaybook, error) {
	var p camPlaybook
	var enabled int
	err := s.Scan(&p.ID, &p.SiteID, &p.DVRID, &p.Name, &p.WhenToUse, &p.Instructions, &enabled, &p.CreatedAt, &p.UpdatedAt)
	p.Enabled = enabled != 0
	return p, err
}

func insertCameraPlaybook(db *sql.DB, p camPlaybook) (string, error) {
	if p.ID == "" {
		p.ID = randomID("pb")
	}
	now := nowRFC3339()
	if p.CreatedAt == "" {
		p.CreatedAt = now
	}
	if p.UpdatedAt == "" {
		p.UpdatedAt = now
	}
	enabled := 0
	if p.Enabled {
		enabled = 1
	}
	_, err := db.Exec(`INSERT INTO camera_playbooks
		(id, site_id, dvr_id, name, when_to_use, instructions, enabled, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		p.ID, p.SiteID, p.DVRID, p.Name, p.WhenToUse, p.Instructions, enabled, p.CreatedAt, p.UpdatedAt)
	if camPlaybookDupErr(err) {
		return "", camPlaybookNameTakenError{Name: p.Name}
	}
	if err != nil {
		return "", err
	}
	return p.ID, nil
}

func getCameraPlaybook(db *sql.DB, id string) (camPlaybook, error) {
	return scanPlaybook(db.QueryRow(`SELECT `+camPlaybookCols+` FROM camera_playbooks WHERE id = ?`, id))
}

// listCameraPlaybooks lists a site's playbooks (enabledOnly filters to enabled=1).
func listCameraPlaybooks(db *sql.DB, siteID string, enabledOnly bool) ([]camPlaybook, error) {
	q := `SELECT ` + camPlaybookCols + ` FROM camera_playbooks WHERE site_id = ?`
	if enabledOnly {
		q += ` AND enabled = 1`
	}
	q += ` ORDER BY name COLLATE NOCASE, created_at`
	rows, err := db.Query(q, siteID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []camPlaybook
	for rows.Next() {
		p, err := scanPlaybook(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func updateCameraPlaybook(db *sql.DB, p camPlaybook) error {
	enabled := 0
	if p.Enabled {
		enabled = 1
	}
	res, err := db.Exec(`UPDATE camera_playbooks
		SET dvr_id = ?, name = ?, when_to_use = ?, instructions = ?, enabled = ?, updated_at = ?
		WHERE id = ?`,
		p.DVRID, p.Name, p.WhenToUse, p.Instructions, enabled, nowRFC3339(), p.ID)
	if camPlaybookDupErr(err) {
		return camPlaybookNameTakenError{Name: p.Name}
	}
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// deleteCameraPlaybook removes a playbook, its media rows, and releases each
// referenced capture pin (camReleaseCaptureIfUnreferenced).
func deleteCameraPlaybook(db *sql.DB, cfg config, id string) error {
	media, err := listCameraPlaybookMedia(db, id)
	if err != nil {
		return err
	}
	if _, err := db.Exec(`DELETE FROM camera_playbook_media WHERE playbook_id = ?`, id); err != nil {
		return err
	}
	if _, err := db.Exec(`DELETE FROM camera_playbooks WHERE id = ?`, id); err != nil {
		return err
	}
	// Release AFTER the media rows are gone so the "still referenced?" check
	// sees only surviving references (e.g. the same token attached to another
	// playbook keeps its pin).
	seen := make(map[string]bool, len(media))
	for _, m := range media {
		if seen[m.CaptureToken] {
			continue
		}
		seen[m.CaptureToken] = true
		camReleaseCaptureIfUnreferenced(db, cfg, m.CaptureToken)
	}
	return nil
}

// findCameraPlaybookByName resolves a site's playbook by case-insensitive name
// match (in Go over the site's list — 100 rows is trivial). sql.ErrNoRows when
// nothing matches.
func findCameraPlaybookByName(db *sql.DB, siteID, name string) (camPlaybook, error) {
	want := strings.TrimSpace(name)
	if want == "" {
		return camPlaybook{}, sql.ErrNoRows
	}
	all, err := listCameraPlaybooks(db, siteID, false)
	if err != nil {
		return camPlaybook{}, err
	}
	for _, p := range all {
		if strings.EqualFold(p.Name, want) {
			return p, nil
		}
	}
	return camPlaybook{}, sql.ErrNoRows
}

func insertCameraPlaybookMedia(db *sql.DB, m camPlaybookMedia) (string, error) {
	if m.ID == "" {
		m.ID = randomID("pbm")
	}
	if m.CreatedAt == "" {
		m.CreatedAt = nowRFC3339()
	}
	_, err := db.Exec(`INSERT INTO camera_playbook_media
		(id, playbook_id, capture_token, note, created_at)
		VALUES (?, ?, ?, ?, ?)`,
		m.ID, m.PlaybookID, m.CaptureToken, m.Note, m.CreatedAt)
	if err != nil {
		return "", err
	}
	return m.ID, nil
}

const camPlaybookMediaCols = `id, playbook_id, capture_token, note, created_at`

func scanPlaybookMedia(s rowScanner) (camPlaybookMedia, error) {
	var m camPlaybookMedia
	err := s.Scan(&m.ID, &m.PlaybookID, &m.CaptureToken, &m.Note, &m.CreatedAt)
	return m, err
}

func listCameraPlaybookMedia(db *sql.DB, playbookID string) ([]camPlaybookMedia, error) {
	rows, err := db.Query(`SELECT `+camPlaybookMediaCols+` FROM camera_playbook_media
		WHERE playbook_id = ? ORDER BY created_at, id`, playbookID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []camPlaybookMedia
	for rows.Next() {
		m, err := scanPlaybookMedia(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func getCameraPlaybookMedia(db *sql.DB, id string) (camPlaybookMedia, error) {
	return scanPlaybookMedia(db.QueryRow(`SELECT `+camPlaybookMediaCols+` FROM camera_playbook_media WHERE id = ?`, id))
}

func deleteCameraPlaybookMedia(db *sql.DB, id string) error {
	_, err := db.Exec(`DELETE FROM camera_playbook_media WHERE id = ?`, id)
	return err
}

// camReleaseCaptureIfUnreferenced restores normal retention when the LAST
// camera_playbook_media row for a token is removed: if no camera_playbook_media
// row still references it, SET expires_at = now + cfg.CameraMediaRetention
// (default 24h). Best-effort — failures are camlog'd, never fatal.
func camReleaseCaptureIfUnreferenced(db *sql.DB, cfg config, token string) {
	token = strings.TrimSpace(token)
	if token == "" {
		return
	}
	// Consult BOTH pinned-reference tables: another playbook OR an approved avatar
	// reference may still need this capture pinned (they share the token namespace).
	if camCaptureReferenced(db, token) {
		return
	}
	retention := cfg.CameraMediaRetention
	if retention <= 0 {
		retention = 24 * time.Hour
	}
	expires := time.Now().Add(retention).Format(time.RFC3339)
	if _, err := db.Exec(`UPDATE camera_captures SET expires_at = ? WHERE token = ?`, expires, token); err != nil {
		camlog("warn", "playbook_release", map[string]any{"ok": false, "error": err.Error()})
	}
}

// ─────────────────────────── prompt catalog ───────────────────────────

// camPlaybookCatalog renders the OPERATIONAL PLAYBOOKS one-liner list for the
// investigation system prompt: `- "<name>" — [DVR "<name>"] <when_to_use>` per
// enabled playbook, when_to_use truncated to camPlaybookOneLinerMax chars,
// capped at camPlaybookCatalogMax entries. Returns "" when nothing to show.
// (The header line and the trailing "call tool playbook..." guidance are
// emitted by the caller, camInvestigateSystemPrompt.)
func camPlaybookCatalog(playbooks []camPlaybook, dvrByID map[string]CamDVR) string {
	var b strings.Builder
	count := 0
	for _, p := range playbooks {
		if !p.Enabled {
			continue // defensive: callers normally pass enabled-only lists
		}
		if count >= camPlaybookCatalogMax {
			break
		}
		count++
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		b.WriteString(`- "` + p.Name + `"`)
		oneLiner := strings.TrimSpace(p.WhenToUse)
		if r := []rune(oneLiner); len(r) > camPlaybookOneLinerMax {
			oneLiner = string(r[:camPlaybookOneLinerMax]) + "…"
		}
		var prefix string
		if p.DVRID != "" {
			label := p.DVRID
			if d, ok := dvrByID[p.DVRID]; ok && strings.TrimSpace(d.Name) != "" {
				label = strings.TrimSpace(d.Name)
			}
			prefix = `[DVR "` + label + `"]`
		}
		switch {
		case prefix != "" && oneLiner != "":
			b.WriteString(" — " + prefix + " " + oneLiner)
		case prefix != "":
			b.WriteString(" — " + prefix)
		case oneLiner != "":
			b.WriteString(" — " + oneLiner)
		}
	}
	return b.String()
}

// ─────────────────────────── investigate tool ───────────────────────────

// camToolPlaybook is the `playbook` investigate tool: looks up the named
// playbook (case-insensitive; unknown/disabled → helpful summary listing valid
// names), returns Summary = full instructions + reference-image notes, Images =
// up to camPlaybookRefImgMax on-disk reference paths (shown to the model next
// turn), Media = every reference artifact as a citable evidence item
// ("REFERENCE — <note>"). Fetches: 0 (no DVR I/O — references are on disk).
func camToolPlaybook(ctx context.Context, cfg config, db *sql.DB, r *http.Request, site camSite, args investigateArgs, scratch string) investigateToolResult {
	_ = ctx
	_ = scratch
	name := strings.TrimSpace(args.Name)
	pb, err := findCameraPlaybookByName(db, site.ID, name)
	if err != nil || !pb.Enabled {
		// No operator playbook matches (operator names win on an exact-name
		// collision because the DB lookup runs first). Fall back to the built-in,
		// environment-agnostic investigation methods before giving up.
		if s, ok := camFindBuiltinStrategy(name); ok {
			camlog("info", "investigate_playbook", map[string]any{"builtin": s.Name})
			return investigateToolResult{Summary: camStrategyToolSummary(s), Fetches: 0}
		}
		return investigateToolResult{Summary: camPlaybookUnknownSummary(db, site.ID, name)}
	}

	rowsMedia, merr := listCameraPlaybookMedia(db, pb.ID)
	if merr != nil {
		camlog("warn", "investigate_playbook", map[string]any{"ok": false, "playbook_id": pb.ID, "error": merr.Error()})
	}
	var (
		images []string
		media  []evidenceItem
		notes  []string
	)
	for _, m := range rowsMedia {
		cpt, cerr := getCameraCaptureByToken(db, m.CaptureToken)
		if cerr != nil {
			continue // capture row gone — skip rather than fail the whole tool
		}
		if _, serr := os.Stat(cpt.Path); serr != nil {
			continue // reference file vanished from disk — skip
		}
		if strings.HasPrefix(strings.TrimSpace(cpt.ContentType), "image/") && len(images) < camPlaybookRefImgMax {
			images = append(images, cpt.Path)
		}
		note := strings.TrimSpace(m.Note)
		caption := "REFERENCE"
		if note != "" {
			caption = "REFERENCE — " + note
		} else {
			note = "(no note)"
		}
		media = append(media, evidenceItem{MediaURL: camMediaURL(cfg, r, m.CaptureToken), Caption: caption})
		notes = append(notes, note)
	}

	var b strings.Builder
	if use := strings.TrimSpace(pb.WhenToUse); use != "" {
		fmt.Fprintf(&b, "PLAYBOOK %q (use when: %s):\n", pb.Name, use)
	} else {
		fmt.Fprintf(&b, "PLAYBOOK %q:\n", pb.Name)
	}
	b.WriteString(strings.TrimSpace(pb.Instructions))
	if len(notes) > 0 {
		b.WriteString("\n\nREFERENCE IMAGES (operator-approved, attached this turn):\n")
		for i, n := range notes {
			fmt.Fprintf(&b, "%d. %s\n", i+1, n)
		}
	}
	camlog("info", "investigate_playbook", map[string]any{
		"playbook_id": pb.ID, "name": pb.Name, "refs": len(media), "images": len(images),
	})
	return investigateToolResult{Summary: strings.TrimRight(b.String(), "\n"), Media: media, Images: images, Fetches: 0}
}

// camPlaybookUnknownSummary is the unknown/disabled-name tool reply: names the
// miss, lists up to 20 valid (enabled) operator playbook names, and always lists
// the built-in investigation-method names (which are environment-agnostic and thus
// valid on every site) so the next attempt can pick an EXACT name from either set.
func camPlaybookUnknownSummary(db *sql.DB, siteID, name string) string {
	msg := fmt.Sprintf("playbook: no playbook named %q — use the EXACT name from the OPERATIONAL PLAYBOOKS list, or a built-in INVESTIGATION METHOD name below.", name)
	all, err := listCameraPlaybooks(db, siteID, true)
	if err == nil && len(all) > 0 {
		names := make([]string, 0, 20)
		for i, p := range all {
			if i >= 20 {
				break
			}
			names = append(names, fmt.Sprintf("%q", p.Name))
		}
		extra := ""
		if len(all) > len(names) {
			extra = fmt.Sprintf(" (+%d more)", len(all)-len(names))
		}
		msg += " Valid playbooks: " + strings.Join(names, ", ") + extra + "."
	} else {
		msg += " This site has no enabled playbooks."
	}
	return msg + " Built-in methods: " + camStrategyNameList() + "."
}

// ─────────────────────────── HTTP handlers ───────────────────────────
// Registered in registerCameraRoutes (camerahttp.go), all noStore(requireAdmin(...)).

func cameraPlaybookJSON(p camPlaybook, mediaCount int) map[string]any {
	return map[string]any{
		"id":           p.ID,
		"site_id":      p.SiteID,
		"dvr_id":       p.DVRID,
		"name":         p.Name,
		"when_to_use":  p.WhenToUse,
		"instructions": p.Instructions,
		"enabled":      p.Enabled,
		"media_count":  mediaCount,
		"created_at":   p.CreatedAt,
		"updated_at":   p.UpdatedAt,
	}
}

// camPlaybookMediaCount counts one playbook's reference rows (best-effort).
func camPlaybookMediaCount(db *sql.DB, playbookID string) int {
	var n int
	_ = db.QueryRow(`SELECT COUNT(*) FROM camera_playbook_media WHERE playbook_id = ?`, playbookID).Scan(&n)
	return n
}

// camPlaybookMediaCounts maps playbook_id → reference count for a site's
// playbooks in one query (best-effort — list rendering must not fail on it).
func camPlaybookMediaCounts(db *sql.DB, siteID string) map[string]int {
	counts := map[string]int{}
	rows, err := db.Query(`SELECT m.playbook_id, COUNT(*)
		FROM camera_playbook_media m
		JOIN camera_playbooks p ON p.id = m.playbook_id
		WHERE p.site_id = ?
		GROUP BY m.playbook_id`, siteID)
	if err != nil {
		return counts
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var n int
		if rows.Scan(&id, &n) == nil {
			counts[id] = n
		}
	}
	return counts
}

// writePlaybookErr maps CRUD errors to friendly statuses (409 on name clash).
func writePlaybookErr(w http.ResponseWriter, err error) {
	var dup camPlaybookNameTakenError
	if errors.As(err, &dup) {
		writeJSON(w, http.StatusConflict, map[string]any{"error": dup.Error()})
		return
	}
	writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
}

// handleCameraPlaybooks serves /ui/api/cameras/playbooks:
// GET ?site_id= → {playbooks:[{id,site_id,dvr_id,name,when_to_use,instructions,enabled,media_count,created_at,updated_at}]}
// POST {site_id,dvr_id?,name,when_to_use,instructions,enabled?} → {ok,playbook} (friendly error on duplicate name).
func handleCameraPlaybooks(cfg config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		db, err := openProxyDB(cfg)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		defer db.Close()
		switch r.Method {
		case http.MethodGet:
			siteID := strings.TrimSpace(r.URL.Query().Get("site_id"))
			if siteID == "" {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": "site_id is required"})
				return
			}
			pbs, lerr := listCameraPlaybooks(db, siteID, false)
			if lerr != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]any{"error": lerr.Error()})
				return
			}
			counts := camPlaybookMediaCounts(db, siteID)
			out := make([]map[string]any, 0, len(pbs))
			for _, p := range pbs {
				out = append(out, cameraPlaybookJSON(p, counts[p.ID]))
			}
			writeJSON(w, http.StatusOK, map[string]any{"playbooks": out})
		case http.MethodPost:
			var body struct {
				SiteID       string `json:"site_id"`
				DVRID        string `json:"dvr_id"`
				Name         string `json:"name"`
				WhenToUse    string `json:"when_to_use"`
				Instructions string `json:"instructions"`
				Enabled      *bool  `json:"enabled"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid JSON"})
				return
			}
			siteID := strings.TrimSpace(body.SiteID)
			name := strings.TrimSpace(body.Name)
			if siteID == "" || name == "" {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": "site_id and name are required"})
				return
			}
			// Case-insensitive duplicate check: the SQL UNIQUE index is
			// case-sensitive, but tool-time lookup is case-insensitive — two
			// names differing only in case would collide when the model calls
			// the playbook tool, so reject both kinds up front.
			if _, ferr := findCameraPlaybookByName(db, siteID, name); ferr == nil {
				writeJSON(w, http.StatusConflict, map[string]any{"error": camPlaybookNameTakenError{Name: name}.Error()})
				return
			}
			enabled := true
			if body.Enabled != nil {
				enabled = *body.Enabled
			}
			p := camPlaybook{
				SiteID:       siteID,
				DVRID:        strings.TrimSpace(body.DVRID),
				Name:         name,
				WhenToUse:    strings.TrimSpace(body.WhenToUse),
				Instructions: strings.TrimSpace(body.Instructions),
				Enabled:      enabled,
			}
			id, ierr := insertCameraPlaybook(db, p)
			if ierr != nil {
				writePlaybookErr(w, ierr)
				return
			}
			pb, gerr := getCameraPlaybook(db, id)
			if gerr != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]any{"error": gerr.Error()})
				return
			}
			camlog("info", "playbook_create", map[string]any{"playbook_id": id, "site_id": siteID, "name": name})
			writeJSON(w, http.StatusOK, map[string]any{"ok": true, "playbook": cameraPlaybookJSON(pb, 0)})
		default:
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		}
	}
}

// handleCameraPlaybookUpdate serves POST /ui/api/cameras/playbooks/update:
// {id, name?, when_to_use?, instructions?, dvr_id?, enabled?} (pointers for
// clearable text fields) → {ok,playbook}.
func handleCameraPlaybookUpdate(cfg config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
			return
		}
		var body struct {
			ID           string  `json:"id"`
			Name         *string `json:"name"`
			WhenToUse    *string `json:"when_to_use"`
			Instructions *string `json:"instructions"`
			DVRID        *string `json:"dvr_id"`
			Enabled      *bool   `json:"enabled"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid JSON"})
			return
		}
		id := strings.TrimSpace(body.ID)
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
		pb, gerr := getCameraPlaybook(db, id)
		if gerr != nil {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "playbook not found"})
			return
		}
		if body.Name != nil {
			n := strings.TrimSpace(*body.Name)
			if n == "" {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": "name cannot be blank"})
				return
			}
			if other, ferr := findCameraPlaybookByName(db, pb.SiteID, n); ferr == nil && other.ID != pb.ID {
				writeJSON(w, http.StatusConflict, map[string]any{"error": camPlaybookNameTakenError{Name: n}.Error()})
				return
			}
			pb.Name = n
		}
		if body.WhenToUse != nil {
			pb.WhenToUse = strings.TrimSpace(*body.WhenToUse)
		}
		if body.Instructions != nil {
			pb.Instructions = strings.TrimSpace(*body.Instructions)
		}
		if body.DVRID != nil {
			pb.DVRID = strings.TrimSpace(*body.DVRID)
		}
		if body.Enabled != nil {
			pb.Enabled = *body.Enabled
		}
		if uerr := updateCameraPlaybook(db, pb); uerr != nil {
			writePlaybookErr(w, uerr)
			return
		}
		pb, gerr = getCameraPlaybook(db, id) // re-read for the fresh updated_at
		if gerr != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": gerr.Error()})
			return
		}
		camlog("info", "playbook_update", map[string]any{"playbook_id": id, "name": pb.Name, "enabled": pb.Enabled})
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "playbook": cameraPlaybookJSON(pb, camPlaybookMediaCount(db, id))})
	}
}

// handleCameraPlaybookDelete serves POST /ui/api/cameras/playbooks/delete:
// {id} → deletes playbook + media rows, releases pins → {ok}.
func handleCameraPlaybookDelete(cfg config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
			return
		}
		var body struct {
			ID string `json:"id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid JSON"})
			return
		}
		id := strings.TrimSpace(body.ID)
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
		pb, gerr := getCameraPlaybook(db, id)
		if gerr != nil {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "playbook not found"})
			return
		}
		if derr := deleteCameraPlaybook(db, cfg, id); derr != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": derr.Error()})
			return
		}
		camlog("info", "playbook_delete", map[string]any{"playbook_id": id, "site_id": pb.SiteID, "name": pb.Name})
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	}
}

func cameraPlaybookMediaJSON(cfg config, r *http.Request, m camPlaybookMedia, contentType string) map[string]any {
	return map[string]any{
		"id":            m.ID,
		"capture_token": m.CaptureToken,
		"note":          m.Note,
		"url":           camMediaURL(cfg, r, m.CaptureToken),
		"content_type":  contentType,
		"created_at":    m.CreatedAt,
	}
}

// handleCameraPlaybookMedia serves /ui/api/cameras/playbooks/media:
// GET ?playbook_id= → {media:[{id,capture_token,note,url,content_type,created_at}]}
// POST {playbook_id, token, note} → validates the capture (row + file + not
// expired), pinCameraCapture, insert → {ok,media} (+"warning" when
// cfg.CameraMediaDir == "" — pinned refs would live in a temp dir).
func handleCameraPlaybookMedia(cfg config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		db, err := openProxyDB(cfg)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		defer db.Close()
		switch r.Method {
		case http.MethodGet:
			pbID := strings.TrimSpace(r.URL.Query().Get("playbook_id"))
			if pbID == "" {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": "playbook_id is required"})
				return
			}
			rowsM, lerr := listCameraPlaybookMedia(db, pbID)
			if lerr != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]any{"error": lerr.Error()})
				return
			}
			out := make([]map[string]any, 0, len(rowsM))
			for _, m := range rowsM {
				ct := ""
				if cpt, cerr := getCameraCaptureByToken(db, m.CaptureToken); cerr == nil {
					ct = cpt.ContentType
				}
				out = append(out, cameraPlaybookMediaJSON(cfg, r, m, ct))
			}
			writeJSON(w, http.StatusOK, map[string]any{"media": out})
		case http.MethodPost:
			var body struct {
				PlaybookID string `json:"playbook_id"`
				Token      string `json:"token"`
				Note       string `json:"note"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid JSON"})
				return
			}
			pbID := strings.TrimSpace(body.PlaybookID)
			token := strings.TrimSpace(body.Token)
			if pbID == "" || token == "" {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": "playbook_id and token are required"})
				return
			}
			pb, gerr := getCameraPlaybook(db, pbID)
			if gerr != nil {
				writeJSON(w, http.StatusNotFound, map[string]any{"error": "playbook not found"})
				return
			}
			// Attach validation: the capture row must exist, its file must
			// still be on disk, and it must not already be expired.
			cpt, cerr := getCameraCaptureByToken(db, token)
			if cerr != nil {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": "capture no longer available — re-run the setup investigation"})
				return
			}
			if _, serr := os.Stat(cpt.Path); serr != nil {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": "capture no longer available — re-run the setup investigation"})
				return
			}
			if cpt.ExpiresAt != "" && cpt.ExpiresAt <= nowRFC3339() {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": "capture no longer available — re-run the setup investigation"})
				return
			}
			if perr := pinCameraCapture(db, token); perr != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]any{"error": perr.Error()})
				return
			}
			m := camPlaybookMedia{
				PlaybookID:   pb.ID,
				CaptureToken: token,
				Note:         strings.TrimSpace(body.Note),
				CreatedAt:    nowRFC3339(),
			}
			id, ierr := insertCameraPlaybookMedia(db, m)
			if ierr != nil {
				// Undo the pin we just took if the row could not be recorded.
				camReleaseCaptureIfUnreferenced(db, cfg, token)
				writeJSON(w, http.StatusInternalServerError, map[string]any{"error": ierr.Error()})
				return
			}
			m.ID = id
			camlog("info", "playbook_media_attach", map[string]any{"playbook_id": pb.ID, "media_id": id, "kind": cpt.Kind})
			resp := map[string]any{"ok": true, "media": cameraPlaybookMediaJSON(cfg, r, m, cpt.ContentType)}
			if cfg.CameraMediaDir == "" {
				resp["warning"] = "PROXY_CAM_MEDIA_DIR is not set — reference images live in a temp directory"
			}
			writeJSON(w, http.StatusOK, resp)
		default:
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		}
	}
}

// handleCameraPlaybookMediaDelete serves POST /ui/api/cameras/playbooks/media/delete:
// {id} → delete row + camReleaseCaptureIfUnreferenced → {ok}.
func handleCameraPlaybookMediaDelete(cfg config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
			return
		}
		var body struct {
			ID string `json:"id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid JSON"})
			return
		}
		id := strings.TrimSpace(body.ID)
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
		m, gerr := getCameraPlaybookMedia(db, id)
		if gerr != nil {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "media not found"})
			return
		}
		if derr := deleteCameraPlaybookMedia(db, id); derr != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": derr.Error()})
			return
		}
		camReleaseCaptureIfUnreferenced(db, cfg, m.CaptureToken)
		camlog("info", "playbook_media_delete", map[string]any{"playbook_id": m.PlaybookID, "media_id": id})
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	}
}
