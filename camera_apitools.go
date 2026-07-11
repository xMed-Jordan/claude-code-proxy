package main

// camera_apitools.go — operator-defined external API tools for the Ask-AI
// investigation loop (design-knowledge.md Part C): e.g. "call the attendance
// system when an employee is seen in the kitchen".
//
// Each tool is a GET/POST template with {{param}} placeholders the model fills
// via the `call_api` investigate tool. Safety rails: the scheme+host portion of
// url_template may NOT contain "{{" (blocks Bearer-secret exfiltration to a
// model-chosen host), the Authorization header key is reserved for the stored
// secret, execution goes through the EXISTING SSRF-guarded outbound client
// (camGuardedHTTPClient, cameramonitor.go — post-DNS isBlockedIP dial hook),
// and responses are TEXT only (never citable evidence). auth_secret_enc is
// encryptSecret'd and has NO code path into JSON — handlers echo only
// has_secret (sole deliberate exception: the flag-gated DR secrets export,
// camera_serviceapi_sync.go).
//
// Schema lives in migrateCameraDB (camera_store.go): camera_api_tools with
// UNIQUE(site_id, name).

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// camAPIToolRespMax caps how many chars of a response body are folded into the
// transcript (and the /test endpoint's echo).
const camAPIToolRespMax = 4000

// camAPITool is an in-memory view of a camera_api_tools row. AuthSecretEnc is
// the ENCRYPTED secret — it is decrypted only inside camToolCallAPI / the test
// handler, never logged, never serialized.
type camAPITool struct {
	ID                   string
	SiteID               string
	Name                 string // model-facing identifier, unique per site
	Description          string // shown to the model in the catalog
	Method               string // GET | POST
	URLTemplate          string // {{param}} placeholders allowed in path/query ONLY
	HeadersJSON          string // static {"Header":"value"} map
	AuthSecretEnc        string // encryptSecret; sent as Authorization: Bearer <secret>
	BodyTemplate         string // POST body with {{param}} placeholders
	RequestInstructions  string // params the model must supply (shown in catalog)
	ResponseInstructions string // folded into the tool result after the response
	Enabled              bool
	CreatedAt            string
	UpdatedAt            string
}

// camAPIToolPlaceholderRe matches one {{placeholder}}; the key is the trimmed
// inner text. Deliberately loose (any non-brace content) so that malformed
// placeholders still surface as "unfilled" instead of passing through silently.
var camAPIToolPlaceholderRe = regexp.MustCompile(`\{\{([^{}]*)\}\}`)

// camAPIToolNormMethod uppercases/trims a method and defaults blank to GET.
func camAPIToolNormMethod(m string) string {
	m = strings.ToUpper(strings.TrimSpace(m))
	if m == "" {
		return http.MethodGet
	}
	return m
}

// ─────────────────────────── CRUD ───────────────────────────

const camAPIToolCols = `id, site_id, name, description, method, url_template, headers_json, auth_secret_enc,
	body_template, request_instructions, response_instructions, enabled, created_at, updated_at`

func scanCameraAPITool(sc interface{ Scan(...any) error }) (camAPITool, error) {
	var t camAPITool
	var enabled int
	err := sc.Scan(&t.ID, &t.SiteID, &t.Name, &t.Description, &t.Method, &t.URLTemplate, &t.HeadersJSON,
		&t.AuthSecretEnc, &t.BodyTemplate, &t.RequestInstructions, &t.ResponseInstructions,
		&enabled, &t.CreatedAt, &t.UpdatedAt)
	t.Enabled = enabled != 0
	return t, err
}

func insertCameraAPITool(db *sql.DB, cfg config, t camAPITool, secret string) (string, error) {
	if t.ID == "" {
		t.ID = randomID("apt")
	}
	enc := ""
	if secret != "" {
		var err error
		enc, err = encryptSecret(cfg, secret)
		if err != nil {
			return "", err
		}
	}
	now := nowRFC3339()
	_, err := db.Exec(`INSERT INTO camera_api_tools
		(id, site_id, name, description, method, url_template, headers_json, auth_secret_enc,
		 body_template, request_instructions, response_instructions, enabled, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		t.ID, t.SiteID, t.Name, t.Description, camAPIToolNormMethod(t.Method), t.URLTemplate,
		t.HeadersJSON, enc, t.BodyTemplate, t.RequestInstructions, t.ResponseInstructions,
		boolToInt(t.Enabled), now, now)
	return t.ID, err
}

func getCameraAPITool(db *sql.DB, id string) (camAPITool, error) {
	return scanCameraAPITool(db.QueryRow(`SELECT `+camAPIToolCols+` FROM camera_api_tools WHERE id = ?`, id))
}

// listCameraAPITools lists a site's API tools (enabledOnly filters to
// enabled=1). Never decrypts secrets. Name order keeps the prompt catalog
// deterministic across runs.
func listCameraAPITools(db *sql.DB, siteID string, enabledOnly bool) ([]camAPITool, error) {
	q := `SELECT ` + camAPIToolCols + ` FROM camera_api_tools WHERE site_id = ?`
	if enabledOnly {
		q += ` AND enabled = 1`
	}
	q += ` ORDER BY name COLLATE NOCASE ASC, id ASC`
	rows, err := db.Query(q, siteID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []camAPITool
	for rows.Next() {
		t, err := scanCameraAPITool(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// updateCameraAPITool mirrors updateCameraDVR's two-branch shape: when
// changeSecret is true auth_secret_enc is rewritten from secret (plaintext;
// "" clears it), otherwise it is left untouched.
func updateCameraAPITool(db *sql.DB, cfg config, t camAPITool, changeSecret bool, secret string) error {
	now := nowRFC3339()
	if changeSecret {
		enc := ""
		if secret != "" {
			var err error
			enc, err = encryptSecret(cfg, secret)
			if err != nil {
				return err
			}
		}
		_, err := db.Exec(`UPDATE camera_api_tools SET name = ?, description = ?, method = ?, url_template = ?,
			headers_json = ?, auth_secret_enc = ?, body_template = ?, request_instructions = ?,
			response_instructions = ?, enabled = ?, updated_at = ? WHERE id = ?`,
			t.Name, t.Description, camAPIToolNormMethod(t.Method), t.URLTemplate, t.HeadersJSON, enc,
			t.BodyTemplate, t.RequestInstructions, t.ResponseInstructions, boolToInt(t.Enabled), now, t.ID)
		return err
	}
	_, err := db.Exec(`UPDATE camera_api_tools SET name = ?, description = ?, method = ?, url_template = ?,
		headers_json = ?, body_template = ?, request_instructions = ?,
		response_instructions = ?, enabled = ?, updated_at = ? WHERE id = ?`,
		t.Name, t.Description, camAPIToolNormMethod(t.Method), t.URLTemplate, t.HeadersJSON,
		t.BodyTemplate, t.RequestInstructions, t.ResponseInstructions, boolToInt(t.Enabled), now, t.ID)
	return err
}

func deleteCameraAPITool(db *sql.DB, id string) error {
	_, err := db.Exec(`DELETE FROM camera_api_tools WHERE id = ?`, id)
	return err
}

// findCameraAPIToolByName resolves a site's tool by case-insensitive name match
// (in Go over the site's list — the catalogs are tiny). sql.ErrNoRows when absent.
func findCameraAPIToolByName(db *sql.DB, siteID, name string) (camAPITool, error) {
	tools, err := listCameraAPITools(db, siteID, false)
	if err != nil {
		return camAPITool{}, err
	}
	want := strings.TrimSpace(name)
	for _, t := range tools {
		if strings.EqualFold(strings.TrimSpace(t.Name), want) {
			return t, nil
		}
	}
	return camAPITool{}, sql.ErrNoRows
}

// ─────────────────────────── prompt catalog ───────────────────────────

// camAPIToolOneLine collapses whitespace/newlines so operator prose stays a
// single catalog line.
func camAPIToolOneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// camAPIToolCatalog renders the EXTERNAL API TOOLS list for the investigation
// system prompt: `- "<name>" — <description>` plus an indented
// `params: <request_instructions>` line per enabled tool. "" when empty.
// The surrounding header and "Call tool call_api ..." guidance are emitted by
// camInvestigateSystemPrompt.
func camAPIToolCatalog(tools []camAPITool) string {
	var b strings.Builder
	for _, t := range tools {
		if !t.Enabled {
			continue
		}
		name := strings.TrimSpace(t.Name)
		if name == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		b.WriteString("- \"" + name + "\"")
		if d := camAPIToolOneLine(t.Description); d != "" {
			b.WriteString(" — " + d)
		}
		if p := camAPIToolOneLine(t.RequestInstructions); p != "" {
			b.WriteString("\n    params: " + p)
		}
	}
	return b.String()
}

// ─────────────────────────── template engine ───────────────────────────

// camAPIToolValidateTemplate is enforced at create/update time:
//   - url_template must parse (with {{x}} → "x" sentinel substitution) as
//     http/https with a host
//   - the scheme+host portion (everything before the first "/" after "://")
//     must NOT contain "{{" — a model-controlled host would let params redirect
//     the Bearer secret to an attacker-chosen server (SSRF/credential exfil)
//   - headers_json must decode to map[string]string; the "Authorization" key is
//     rejected (the stored secret owns that header)
//   - method must be GET or POST
func camAPIToolValidateTemplate(t camAPITool) error {
	method := camAPIToolNormMethod(t.Method)
	if method != http.MethodGet && method != http.MethodPost {
		return fmt.Errorf("method must be GET or POST")
	}
	raw := strings.TrimSpace(t.URLTemplate)
	if raw == "" {
		return fmt.Errorf("url_template is required")
	}
	// Scheme+host portion: everything before the first "/" after "://" (the
	// whole template when there is no path). A placeholder there would let the
	// model pick the destination host for the Bearer secret.
	hostPart := raw
	if idx := strings.Index(raw, "://"); idx >= 0 {
		if slash := strings.Index(raw[idx+3:], "/"); slash >= 0 {
			hostPart = raw[:idx+3+slash]
		}
	}
	if strings.Contains(hostPart, "{{") {
		return fmt.Errorf("url_template may not use {{placeholders}} in the scheme or host — path/query only")
	}
	sentinel := camAPIToolPlaceholderRe.ReplaceAllStringFunc(raw, func(m string) string {
		return strings.TrimSpace(m[2 : len(m)-2])
	})
	u, err := url.Parse(sentinel)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("url_template must be an absolute http(s) URL")
	}
	if hj := strings.TrimSpace(t.HeadersJSON); hj != "" {
		var hdrs map[string]string
		if err := json.Unmarshal([]byte(hj), &hdrs); err != nil {
			return fmt.Errorf("headers must be a JSON object of string values")
		}
		for k := range hdrs {
			if strings.EqualFold(strings.TrimSpace(k), "Authorization") {
				return fmt.Errorf("the Authorization header is reserved for the stored secret")
			}
		}
	}
	return nil
}

// camAPIToolSubstitute is PURE (unit-testable): replaces every {{key}} with the
// param value — url.QueryEscape'd when jsonEscape is false (url_template),
// JSON-string-escaped when true (body_template) — and errors listing any
// placeholder left unfilled.
func camAPIToolSubstitute(tmpl string, params map[string]string, jsonEscape bool) (string, error) {
	var missing []string
	seen := map[string]bool{}
	out := camAPIToolPlaceholderRe.ReplaceAllStringFunc(tmpl, func(m string) string {
		key := strings.TrimSpace(m[2 : len(m)-2])
		v, ok := params[key]
		if !ok {
			if !seen[key] {
				seen[key] = true
				missing = append(missing, "{{"+key+"}}")
			}
			return m
		}
		if jsonEscape {
			b, _ := json.Marshal(v)
			return string(b[1 : len(b)-1])
		}
		return url.QueryEscape(v)
	})
	if len(missing) > 0 {
		return "", fmt.Errorf("missing values for %s — supply them in args.params", strings.Join(missing, ", "))
	}
	return out, nil
}

// camAPIToolDo executes GET/POST through camGuardedHTTPClient (the EXISTING
// SSRF-guarded outbound path — post-DNS isBlockedIP dial hook, 20s timeout,
// ≤3 redirects), reads via readAllLimited(1<<20), and returns (status,
// body truncated to camAPIToolRespMax). A sibling of camDispatchPOSTBody
// generalized to GET.
func camAPIToolDo(ctx context.Context, method, rawURL string, headers map[string]string, body []byte) (int, string, error) {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return 0, "", fmt.Errorf("invalid url")
	}
	var rd io.Reader
	if method == http.MethodPost && len(body) > 0 {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), rd)
	if err != nil {
		return 0, "", err
	}
	if rd != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := camGuardedHTTPClient().Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	respBody := bytes.TrimSpace(readAllLimited(resp.Body, 1<<20))
	return resp.StatusCode, truncateString(string(respBody), camAPIToolRespMax), nil
}

// ─────────────────────────── investigate tool ───────────────────────────

// camAPIToolUnknownSummary is the helpful failure line when the model names a
// tool that does not exist (or is disabled): repeats the valid catalog names.
func camAPIToolUnknownSummary(db *sql.DB, siteID, name string) string {
	tools, _ := listCameraAPITools(db, siteID, true)
	if len(tools) == 0 {
		return fmt.Sprintf("call_api: no API tool named %q — this site has no enabled API tools.", name)
	}
	names := make([]string, 0, len(tools))
	for _, t := range tools {
		if len(names) >= 20 {
			break
		}
		names = append(names, fmt.Sprintf("%q", t.Name))
	}
	return fmt.Sprintf("call_api: no API tool named %q — use the EXACT name from the EXTERNAL API TOOLS list. Valid names: %s.",
		name, strings.Join(names, ", "))
}

// camToolCallAPI is the `call_api` investigate tool: looks up the named tool
// (enabled only; unknown → helpful summary with valid names), stringifies
// args.Params tolerantly, substitutes URL + body (missing-param errors become
// the Summary so the model can retry), decrypts the secret ONLY here, executes
// via camAPIToolDo, and returns a TEXT-only result:
//
//	API "<name>" → HTTP <status>:
//	<body, truncated>
//
//	HOW TO INTERPRET (operator instructions):
//	<response_instructions>
//
// Fetches: 0, Media: nil, Images: nil (API responses are never citable
// evidence — camFilterEvidence correctly drops any API URL). camlog's
// {tool, host, status, latency_ms, resp_bytes} — never the substituted
// URL/headers/secret.
func camToolCallAPI(ctx context.Context, cfg config, db *sql.DB, site camSite, args investigateArgs) investigateToolResult {
	name := strings.TrimSpace(args.Name)
	t, err := findCameraAPIToolByName(db, site.ID, name)
	if name == "" || err != nil || !t.Enabled {
		return investigateToolResult{Summary: camAPIToolUnknownSummary(db, site.ID, name)}
	}

	// Tolerant stringification of the model-supplied params.
	params := make(map[string]string, len(args.Params))
	for k, v := range args.Params {
		params[strings.TrimSpace(k)] = strings.TrimSpace(fmt.Sprint(v))
	}

	rawURL, serr := camAPIToolSubstitute(t.URLTemplate, params, false)
	if serr != nil {
		return investigateToolResult{Summary: fmt.Sprintf("call_api %q: %v", t.Name, serr)}
	}
	method := camAPIToolNormMethod(t.Method)
	var body []byte
	if method == http.MethodPost && strings.TrimSpace(t.BodyTemplate) != "" {
		b, berr := camAPIToolSubstitute(t.BodyTemplate, params, true)
		if berr != nil {
			return investigateToolResult{Summary: fmt.Sprintf("call_api %q: %v", t.Name, berr)}
		}
		body = []byte(b)
	}

	headers := map[string]string{}
	if hj := strings.TrimSpace(t.HeadersJSON); hj != "" {
		if uerr := json.Unmarshal([]byte(hj), &headers); uerr != nil {
			return investigateToolResult{Summary: fmt.Sprintf("call_api %q: this tool's headers are misconfigured — ask the operator to fix them.", t.Name)}
		}
	}
	// The stored secret is decrypted ONLY here, sent ONLY as Authorization: Bearer,
	// and never appears in summaries or logs.
	if strings.TrimSpace(t.AuthSecretEnc) != "" {
		secret, derr := decryptSecret(cfg, t.AuthSecretEnc)
		if derr != nil {
			return investigateToolResult{Summary: fmt.Sprintf("call_api %q: this tool's stored secret cannot be decrypted — ask the operator to re-save it.", t.Name)}
		}
		headers["Authorization"] = "Bearer " + secret
	}

	host := ""
	if u, perr := url.Parse(rawURL); perr == nil {
		host = u.Host
	}
	start := time.Now()
	status, respBody, derr := camAPIToolDo(ctx, method, rawURL, headers, body)
	latency := time.Since(start).Milliseconds()
	if derr != nil {
		// Never log the substituted URL/headers (derr may embed the URL —
		// keep it out of the log; the transcript Summary is the model's copy).
		camlog("warn", "investigate_api", map[string]any{
			"tool": t.Name, "host": host, "status": 0, "latency_ms": latency,
		})
		return investigateToolResult{Summary: fmt.Sprintf("call_api: request failed: %v", derr)}
	}
	camlog("info", "investigate_api", map[string]any{
		"tool": t.Name, "host": host, "status": status, "latency_ms": latency, "resp_bytes": len(respBody),
	})

	if respBody == "" {
		respBody = "(empty response)"
	}
	sum := fmt.Sprintf("API %q → HTTP %d:\n%s", t.Name, status, respBody)
	if ri := strings.TrimSpace(t.ResponseInstructions); ri != "" {
		sum += "\n\nHOW TO INTERPRET (operator instructions):\n" + ri
	}
	return investigateToolResult{Summary: sum}
}

// ─────────────────────────── HTTP handlers ───────────────────────────
// Registered in registerCameraRoutes (camerahttp.go), all noStore(requireAdmin(...)).

// apiToolJSON is the credential-safe wire shape: auth_secret_enc has NO code
// path into JSON — only has_secret (watch-action precedent).
func apiToolJSON(t camAPITool) map[string]any {
	return map[string]any{
		"id": t.ID, "site_id": t.SiteID, "name": t.Name, "description": t.Description,
		"method": t.Method, "url_template": t.URLTemplate, "headers_json": t.HeadersJSON,
		"body_template": t.BodyTemplate, "request_instructions": t.RequestInstructions,
		"response_instructions": t.ResponseInstructions,
		"has_secret":            strings.TrimSpace(t.AuthSecretEnc) != "",
		"enabled":               t.Enabled, "created_at": t.CreatedAt, "updated_at": t.UpdatedAt,
	}
}

// camAPIToolHeadersString normalizes the request "headers" field: the UI may
// send either a JSON object or the raw textarea string containing one.
func camAPIToolHeadersString(raw json.RawMessage) string {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" {
		return ""
	}
	var asString string
	if err := json.Unmarshal(raw, &asString); err == nil {
		return strings.TrimSpace(asString)
	}
	return s
}

// handleCameraAPITools serves /ui/api/cameras/apitools:
// GET ?site_id= → rows with has_secret (auth_secret_enc has NO JSON code path);
// POST {site_id,name,description,method,url_template,headers,auth_secret,
// body_template,request_instructions,response_instructions,enabled?} —
// validates the template, encrypts the secret → {ok,tool}.
func handleCameraAPITools(cfg config) http.HandlerFunc {
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
			tools, err := listCameraAPITools(db, siteID, false)
			if err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
				return
			}
			out := make([]map[string]any, 0, len(tools))
			for _, t := range tools {
				out = append(out, apiToolJSON(t))
			}
			writeJSON(w, http.StatusOK, map[string]any{"tools": out})
		case http.MethodPost:
			var body struct {
				SiteID               string          `json:"site_id"`
				Name                 string          `json:"name"`
				Description          string          `json:"description"`
				Method               string          `json:"method"`
				URLTemplate          string          `json:"url_template"`
				Headers              json.RawMessage `json:"headers"`
				AuthSecret           string          `json:"auth_secret"`
				BodyTemplate         string          `json:"body_template"`
				RequestInstructions  string          `json:"request_instructions"`
				ResponseInstructions string          `json:"response_instructions"`
				Enabled              *bool           `json:"enabled"`
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
			if _, err := getCameraSite(db, siteID); err != nil {
				writeJSON(w, http.StatusNotFound, map[string]any{"error": "site not found"})
				return
			}
			enabled := true
			if body.Enabled != nil {
				enabled = *body.Enabled
			}
			t := camAPITool{
				SiteID:               siteID,
				Name:                 name,
				Description:          strings.TrimSpace(body.Description),
				Method:               camAPIToolNormMethod(body.Method),
				URLTemplate:          strings.TrimSpace(body.URLTemplate),
				HeadersJSON:          camAPIToolHeadersString(body.Headers),
				BodyTemplate:         body.BodyTemplate,
				RequestInstructions:  strings.TrimSpace(body.RequestInstructions),
				ResponseInstructions: strings.TrimSpace(body.ResponseInstructions),
				Enabled:              enabled,
			}
			if verr := camAPIToolValidateTemplate(t); verr != nil {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": verr.Error()})
				return
			}
			if _, ferr := findCameraAPIToolByName(db, siteID, name); ferr == nil {
				writeJSON(w, http.StatusConflict, map[string]any{"error": "an API tool with that name already exists for this site"})
				return
			}
			id, err := insertCameraAPITool(db, cfg, t, body.AuthSecret)
			if err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
				return
			}
			camlog("info", "apitool_create", map[string]any{"tool_id": id, "site_id": siteID, "name": name})
			created, _ := getCameraAPITool(db, id)
			writeJSON(w, http.StatusOK, map[string]any{"ok": true, "tool": apiToolJSON(created)})
		default:
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		}
	}
}

// handleCameraAPIToolUpdate serves POST /ui/api/cameras/apitools/update: same
// fields + {id}; blank auth_secret = keep existing (DVR-password idiom);
// clear_secret:true removes it.
func handleCameraAPIToolUpdate(cfg config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
			return
		}
		var body struct {
			ID                   string          `json:"id"`
			Name                 string          `json:"name"`
			Description          *string         `json:"description"`
			Method               string          `json:"method"`
			URLTemplate          string          `json:"url_template"`
			Headers              json.RawMessage `json:"headers"`
			AuthSecret           string          `json:"auth_secret"`
			ClearSecret          bool            `json:"clear_secret"`
			BodyTemplate         *string         `json:"body_template"`
			RequestInstructions  *string         `json:"request_instructions"`
			ResponseInstructions *string         `json:"response_instructions"`
			Enabled              *bool           `json:"enabled"`
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
		t, gerr := getCameraAPITool(db, id)
		if gerr != nil {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "api tool not found"})
			return
		}
		if n := strings.TrimSpace(body.Name); n != "" {
			if other, ferr := findCameraAPIToolByName(db, t.SiteID, n); ferr == nil && other.ID != t.ID {
				writeJSON(w, http.StatusConflict, map[string]any{"error": "an API tool with that name already exists for this site"})
				return
			}
			t.Name = n
		}
		if body.Description != nil {
			t.Description = strings.TrimSpace(*body.Description)
		}
		if m := strings.TrimSpace(body.Method); m != "" {
			t.Method = camAPIToolNormMethod(m)
		}
		if u := strings.TrimSpace(body.URLTemplate); u != "" {
			t.URLTemplate = u
		}
		if len(body.Headers) > 0 {
			t.HeadersJSON = camAPIToolHeadersString(body.Headers)
		}
		if body.BodyTemplate != nil {
			t.BodyTemplate = *body.BodyTemplate
		}
		if body.RequestInstructions != nil {
			t.RequestInstructions = strings.TrimSpace(*body.RequestInstructions)
		}
		if body.ResponseInstructions != nil {
			t.ResponseInstructions = strings.TrimSpace(*body.ResponseInstructions)
		}
		if body.Enabled != nil {
			t.Enabled = *body.Enabled
		}
		if verr := camAPIToolValidateTemplate(t); verr != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": verr.Error()})
			return
		}
		// Blank secret keeps the stored one; clear_secret removes it;
		// non-blank replaces it (DVR-password idiom).
		changeSecret := body.ClearSecret || strings.TrimSpace(body.AuthSecret) != ""
		secret := ""
		if !body.ClearSecret {
			secret = body.AuthSecret
		}
		if err := updateCameraAPITool(db, cfg, t, changeSecret, secret); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		camlog("info", "apitool_update", map[string]any{"tool_id": t.ID, "site_id": t.SiteID, "name": t.Name})
		updated, _ := getCameraAPITool(db, t.ID)
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "tool": apiToolJSON(updated)})
	}
}

// handleCameraAPIToolDelete serves POST /ui/api/cameras/apitools/delete: {id} → {ok}.
func handleCameraAPIToolDelete(cfg config) http.HandlerFunc {
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
		t, gerr := getCameraAPITool(db, id)
		if gerr != nil {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "api tool not found"})
			return
		}
		if err := deleteCameraAPITool(db, id); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		camlog("info", "apitool_delete", map[string]any{"tool_id": t.ID, "site_id": t.SiteID, "name": t.Name})
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	}
}

// handleCameraAPIToolTest serves POST /ui/api/cameras/apitools/test:
// {id, params:{...}} — runs the full substitution + guarded call and returns
// {ok,status,response} (truncated). Authoring aid, admin-gated; works on
// disabled tools too so operators can test before enabling.
func handleCameraAPIToolTest(cfg config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
			return
		}
		var body struct {
			ID     string         `json:"id"`
			Params map[string]any `json:"params"`
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
		t, gerr := getCameraAPITool(db, id)
		if gerr != nil {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "api tool not found"})
			return
		}
		params := make(map[string]string, len(body.Params))
		for k, v := range body.Params {
			params[strings.TrimSpace(k)] = strings.TrimSpace(fmt.Sprint(v))
		}
		rawURL, serr := camAPIToolSubstitute(t.URLTemplate, params, false)
		if serr != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": serr.Error()})
			return
		}
		method := camAPIToolNormMethod(t.Method)
		var reqBody []byte
		if method == http.MethodPost && strings.TrimSpace(t.BodyTemplate) != "" {
			b, berr := camAPIToolSubstitute(t.BodyTemplate, params, true)
			if berr != nil {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": berr.Error()})
				return
			}
			reqBody = []byte(b)
		}
		headers := map[string]string{}
		if hj := strings.TrimSpace(t.HeadersJSON); hj != "" {
			if uerr := json.Unmarshal([]byte(hj), &headers); uerr != nil {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": "headers must be a JSON object of string values"})
				return
			}
		}
		if strings.TrimSpace(t.AuthSecretEnc) != "" {
			secret, derr := decryptSecret(cfg, t.AuthSecretEnc)
			if derr != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "stored secret cannot be decrypted — re-save it"})
				return
			}
			headers["Authorization"] = "Bearer " + secret
		}
		host := ""
		if u, perr := url.Parse(rawURL); perr == nil {
			host = u.Host
		}
		start := time.Now()
		status, respBody, derr := camAPIToolDo(r.Context(), method, rawURL, headers, reqBody)
		latency := time.Since(start).Milliseconds()
		if derr != nil {
			// Never log the substituted URL/headers; the error is echoed to the
			// admin only.
			camlog("warn", "apitool_test", map[string]any{
				"tool_id": t.ID, "tool": t.Name, "host": host, "status": 0, "latency_ms": latency,
			})
			writeJSON(w, http.StatusBadGateway, map[string]any{"error": "request failed: " + derr.Error()})
			return
		}
		camlog("info", "apitool_test", map[string]any{
			"tool_id": t.ID, "tool": t.Name, "host": host, "status": status, "latency_ms": latency, "resp_bytes": len(respBody),
		})
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "status": status, "response": respBody})
	}
}
