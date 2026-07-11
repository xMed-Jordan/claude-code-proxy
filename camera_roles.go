package main

// camera_roles.go — per-site person-role vocabulary for the identity layer.
//
// A role is a DESCRIPTIVE label the operator confirms on an avatar ("employee",
// "patient", …) — never a behavior. The camera layer stores and echoes roles; it
// never reasons about them, never ranks them, and never lets an AI write the
// confirmed value (camera_avatars.role is written only by the role-update
// handlers, which stamp role_confirmed_at — the profile builder writes only
// suggested_role/profile_json). All alerting/automation built on roles lives in
// Connect.
//
// The vocabulary = a fixed core set (below) plus per-site custom roles stored in
// camera_sites.policy_json: {"custom_roles":[{slug,label,description}],"notes":"",
// "report_language":""} (report_language is the activity-log rollup language,
// camera_observer_report.go — it rides along in this policy object).
// The vocabulary lives proxy-side because the person-profile VLM prompt
// (camera_profile.go, R1) is built here and must constrain its role hypothesis
// to the site's labels at call time.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"regexp"
	"strings"
)

// camRole is one selectable person-role label.
type camRole struct {
	Slug        string `json:"slug"`
	Label       string `json:"label"`
	Description string `json:"description"`
	Core        bool   `json:"core"`
}

// camRoleCore is the built-in vocabulary every site starts with. Slugs are
// stable API values; labels are display defaults (Connect may localize).
var camRoleCore = []camRole{
	{Slug: "employee", Label: "Employee", Description: "works here (staff, clinicians, cleaners)", Core: true},
	{Slug: "patient", Label: "Patient", Description: "receives care or treatment here (a case)", Core: true},
	{Slug: "visitor", Label: "Visitor", Description: "short-term guest", Core: true},
	{Slug: "family", Label: "Family", Description: "accompanies a patient or resident regularly", Core: true},
	{Slug: "stranger", Label: "Stranger", Description: "no established relationship with this place yet", Core: true},
}

// camSitePolicy is the decoded camera_sites.policy_json. Descriptive context
// only — labels, notes and presentation preferences, no behavior.
type camSitePolicy struct {
	CustomRoles []camRole `json:"custom_roles,omitempty"`
	Notes       string    `json:"notes,omitempty"`
	// ReportLanguage is the language activity-log ROLLUPS (hourly summaries +
	// the daily report, camera_observer_report.go) are written in — a free-text
	// language name ("Arabic", "ar", …); "" means English. Log ENTRIES stay
	// English regardless (they are the AI's own continuity memory). It rides
	// the existing site-policy routes; POSTs replace the whole policy object,
	// so writers must round-trip a GET first (existing semantics).
	ReportLanguage string `json:"report_language,omitempty"`
}

var camRoleSlugRe = regexp.MustCompile(`^[a-z0-9_-]{1,32}$`)

// camSitePolicyMaxCustomRoles bounds operator input (a vocabulary, not a table).
const camSitePolicyMaxCustomRoles = 32

// camParseSitePolicy tolerantly decodes a stored policy_json ("" → zero value).
func camParseSitePolicy(raw string) camSitePolicy {
	var p camSitePolicy
	if s := strings.TrimSpace(raw); s != "" {
		_ = json.Unmarshal([]byte(s), &p)
	}
	return p
}

// camNormalizeSitePolicy validates+cleans an operator-supplied policy: slug
// format, no core collisions, no duplicates, capped counts/lengths. Returns a
// human-readable problem ("" = ok).
func camNormalizeSitePolicy(p *camSitePolicy) string {
	if len(p.CustomRoles) > camSitePolicyMaxCustomRoles {
		return "too many custom roles (max 32)"
	}
	core := make(map[string]bool, len(camRoleCore))
	for _, r := range camRoleCore {
		core[r.Slug] = true
	}
	seen := map[string]bool{}
	clean := make([]camRole, 0, len(p.CustomRoles))
	for _, r := range p.CustomRoles {
		r.Slug = strings.ToLower(strings.TrimSpace(r.Slug))
		r.Label = strings.TrimSpace(r.Label)
		r.Description = strings.TrimSpace(r.Description)
		r.Core = false
		if r.Slug == "" && r.Label == "" {
			continue // empty row from a UI repeater
		}
		if r.Slug == "" {
			r.Slug = camRoleSlugFromLabel(r.Label)
		}
		if !camRoleSlugRe.MatchString(r.Slug) {
			return "invalid role slug " + camQuote(r.Slug) + " (want [a-z0-9_-]{1,32})"
		}
		if core[r.Slug] || r.Slug == "unknown" || r.Slug == "none" {
			return "role slug " + camQuote(r.Slug) + " collides with a built-in role"
		}
		if seen[r.Slug] {
			return "duplicate role slug " + camQuote(r.Slug)
		}
		seen[r.Slug] = true
		if r.Label == "" {
			r.Label = r.Slug
		}
		r.Label = camTruncateRunes(r.Label, 64)
		r.Description = camTruncateRunes(r.Description, 500)
		clean = append(clean, r)
	}
	p.CustomRoles = clean
	p.Notes = camTruncateRunes(p.Notes, 4000)
	p.ReportLanguage = camTruncateRunes(strings.TrimSpace(p.ReportLanguage), 32)
	return ""
}

// camTruncateRunes caps s at n RUNES (never bytes — a byte slice could split a
// multibyte character, e.g. Arabic labels, into invalid UTF-8 that json.Marshal
// mangles to U+FFFD).
func camTruncateRunes(s string, n int) string {
	if utf8len := len([]rune(s)); utf8len > n {
		return string([]rune(s)[:n])
	}
	return s
}

// camQuote quotes a value for an error message.
func camQuote(s string) string { return `"` + s + `"` }

// camRoleSlugFromLabel derives a slug from a display label ("Cleaning Crew" →
// "cleaning_crew"). Non-conforming characters are dropped — so a label written
// entirely in another script (Arabic labels are the default at the live sites)
// would derive to nothing; those fall back to a stable hash slug ("role_1a2b3c4d")
// instead of failing the whole policy save with an empty-slug 400.
func camRoleSlugFromLabel(label string) string {
	label = strings.TrimSpace(label)
	var b strings.Builder
	for _, c := range strings.ToLower(label) {
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '-', c == '_':
			b.WriteRune(c)
		case c == ' ':
			b.WriteByte('_')
		}
	}
	// Trim filler so "موظف نظافة" doesn't degenerate to "_" (its only ASCII
	// character is the space) and collide with the next such label.
	s := strings.Trim(b.String(), "_-")
	if s == "" && label != "" {
		sum := sha256.Sum256([]byte(label))
		s = "role_" + hex.EncodeToString(sum[:4])
	}
	if len(s) > 32 {
		s = s[:32]
	}
	return s
}

// camSitePolicyJSON serializes a policy for storage ("" when empty so untouched
// sites keep an empty column).
func camSitePolicyJSON(p camSitePolicy) string {
	if len(p.CustomRoles) == 0 && strings.TrimSpace(p.Notes) == "" && strings.TrimSpace(p.ReportLanguage) == "" {
		return ""
	}
	return mustJSON(p)
}

// camRoleVocabulary resolves the full selectable vocabulary for a site: core
// roles first, then the site's custom roles.
func camRoleVocabulary(p camSitePolicy) []camRole {
	out := make([]camRole, 0, len(camRoleCore)+len(p.CustomRoles))
	out = append(out, camRoleCore...)
	out = append(out, p.CustomRoles...)
	return out
}

// camValidateRole reports whether slug is assignable at this site. "" is always
// valid (clearing the label).
func camValidateRole(p camSitePolicy, slug string) bool {
	if slug == "" {
		return true
	}
	for _, r := range camRoleVocabulary(p) {
		if r.Slug == slug {
			return true
		}
	}
	return false
}

// camRoleVocabularyJSON renders the vocabulary for API responses.
func camRoleVocabularyJSON(p camSitePolicy) []map[string]any {
	roles := camRoleVocabulary(p)
	out := make([]map[string]any, 0, len(roles))
	for _, r := range roles {
		out = append(out, map[string]any{
			"slug": r.Slug, "label": r.Label, "description": r.Description, "core": r.Core,
		})
	}
	return out
}

// camSitePolicyResponseJSON renders the decoded policy for API responses.
func camSitePolicyResponseJSON(p camSitePolicy) map[string]any {
	custom := make([]map[string]any, 0, len(p.CustomRoles))
	for _, r := range p.CustomRoles {
		custom = append(custom, map[string]any{
			"slug": r.Slug, "label": r.Label, "description": r.Description,
		})
	}
	return map[string]any{"custom_roles": custom, "notes": p.Notes, "report_language": p.ReportLanguage}
}
