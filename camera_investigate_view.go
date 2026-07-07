package main

// camera_investigate_view.go (WS4) — the PUBLIC, no-admin-session live timeline
// page for an investigation, addressed by its unguessable view_token capability id:
//
//   GET /camera/investigations/<view_token>             → static HTML shell
//   GET /camera/investigations/<view_token>/state.json  → transcript + children JSON
//
// The shell is a single zero-interpolation Go const (its JS reads the token from
// location.pathname and polls state.json); serving it never touches the DB, so a
// bad token just renders "not found" client-side. Every response carries
// noindex/no-referrer/no-store + a tight CSP, and both the page and its media are
// rate-limited per view-token and per client IP.
//
// Media on the page loads through the SAME /camera/media/<token> route the admin UI
// uses, but with a ?v=<view_token> gate (handleCameraMediaGated): the requested
// capture token must be one this investigation (or one of its delegated children)
// actually minted, verified via camCollectMintedMedia — deliberately reusing the
// transcript-derived allow-set instead of adding an investigation_id column to the
// ~14 capture persist sites.

import (
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"strings"
)

// Rate ceilings (requests/min) for the public view surface. The page bucket sizes
// for a 4s state.json poll (~15/min) plus the shell load with headroom; the media
// bucket is higher so a long transcript's thumbnails can all render on first paint.
const (
	camViewPageRatePerMin  = 60.0
	camViewMediaRatePerMin = 300.0
)

// camInvestigationViewURL builds the public timeline URL for an investigation, or
// "" when the row has no view_token (nothing to link). r may be nil (background
// settle/progress callers) — imageBaseURL then falls back to cfg.PublicURL.
func camInvestigationViewURL(cfg config, r *http.Request, inv camInvestigation) string {
	if strings.TrimSpace(inv.ViewToken) == "" {
		return ""
	}
	return strings.TrimRight(imageBaseURL(cfg, r), "/") + "/camera/investigations/" + inv.ViewToken
}

// camSetViewSecurityHeaders stamps the hardening headers shared by the shell, the
// state.json, and the gated media responses: keep the page out of search indexes,
// leak no referrer to the DVR/site, never cache, and lock the CSP down to
// same-origin media + inline script/style (the shell is self-contained).
func camSetViewSecurityHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("X-Robots-Tag", "noindex, nofollow")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Cache-Control", "no-store")
	h.Set("Content-Security-Policy", "default-src 'none'; img-src 'self'; media-src 'self'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; connect-src 'self'")
}

// handleCameraInvestigationView dispatches the public timeline routes: the bare
// token serves the HTML shell, a "/state.json" suffix serves the transcript JSON.
// Rate-limited FIRST (per token + per IP); unknown tokens 404 uniformly on the JSON
// path.
func handleCameraInvestigationView(cfg config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.NotFound(w, r)
			return
		}
		rest := strings.TrimPrefix(r.URL.Path, "/camera/investigations/")
		if rest == "" {
			http.NotFound(w, r)
			return
		}
		wantState := false
		token := rest
		if strings.HasSuffix(rest, "/state.json") {
			wantState = true
			token = strings.TrimSuffix(rest, "/state.json")
		}
		if token == "" || strings.ContainsAny(token, "/\\") {
			http.NotFound(w, r)
			return
		}
		ip := clientIP(r)
		// Per-IP FIRST: clientIP is the real TCP peer (unspoofable), so a flooding IP
		// is 429'd before the `||` ever evaluates the token bucket — that short-circuit
		// keeps an attacker's unique-token spray from minting a fresh bucket per request.
		if !camSvcRateAllow("viewip:"+ip, camViewPageRatePerMin) || !camSvcRateAllow("view:"+token, camViewPageRatePerMin) {
			http.Error(w, "rate limited", http.StatusTooManyRequests)
			return
		}
		camSetViewSecurityHeaders(w)

		if !wantState {
			// Static shell: leaks nothing, so it renders for any well-formed token —
			// an unknown one just shows "not found" once its state.json 404s.
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = io.WriteString(w, camInvestigationViewHTML)
			return
		}

		db, err := openProxyDB(cfg)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		defer db.Close()
		inv, ierr := getCameraInvestigationByViewToken(db, token)
		if ierr != nil {
			http.NotFound(w, r)
			return
		}
		writeJSON(w, http.StatusOK, camViewStateJSON(db, inv))
	}
}

// handleCameraMediaGated is the /camera/media/<token> entry point. A request
// carrying ?v=<view_token> is a PUBLIC timeline-page fetch: the requested capture
// must belong to that investigation's (or a child's) minted media, checked here
// without any admin session. Every other request stays ADMIN-gated, unchanged.
func handleCameraMediaGated(cfg config) http.HandlerFunc {
	admin := requireAdmin(cfg, handleCameraMedia(cfg))
	return func(w http.ResponseWriter, r *http.Request) {
		viewToken := strings.TrimSpace(r.URL.Query().Get("v"))
		if viewToken == "" {
			admin(w, r)
			return
		}
		camServeViewMedia(cfg, w, r, viewToken)
	}
}

// camServeViewMedia serves a capture to the public timeline page after proving the
// requested token is one the page's investigation (or a delegated child) minted.
// Rate-limited FIRST (per view-token + per IP); any failure is a uniform 404 so the
// gate never distinguishes "wrong token" from "not yours".
func camServeViewMedia(cfg config, w http.ResponseWriter, r *http.Request, viewToken string) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.NotFound(w, r)
		return
	}
	mediaToken := strings.TrimSpace(strings.TrimPrefix(r.URL.Path, "/camera/media/"))
	if mediaToken == "" || strings.ContainsAny(mediaToken, "/\\") {
		http.NotFound(w, r)
		return
	}
	ip := clientIP(r)
	// Per-IP FIRST so a flooding IP short-circuits before the token bucket is touched
	// (see handleCameraInvestigationView) — bounds attacker-controlled map growth.
	if !camSvcRateAllow("viewmediaip:"+ip, camViewMediaRatePerMin) || !camSvcRateAllow("viewmedia:"+viewToken, camViewMediaRatePerMin) {
		http.Error(w, "rate limited", http.StatusTooManyRequests)
		return
	}
	db, err := openProxyDB(cfg)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer db.Close()
	inv, ierr := getCameraInvestigationByViewToken(db, viewToken)
	if ierr != nil {
		http.NotFound(w, r)
		return
	}
	if !camViewMediaAllowed(db, inv, mediaToken) {
		http.NotFound(w, r)
		return
	}
	camSetViewSecurityHeaders(w)
	camServeCapture(w, r, cfg, db, mediaToken, func() { http.NotFound(w, r) })
}

// camViewMediaAllowed reports whether mediaToken was minted by inv or any of its
// delegated children — the authorization for a ?v= media fetch. The allow-set is
// derived from the transcripts (camCollectMintedMedia), the same mechanism the
// final-answer evidence filter uses, so no capture-side schema is needed.
func camViewMediaAllowed(db *sql.DB, inv camInvestigation, mediaToken string) bool {
	msgs, _ := listCameraInvestigationMessages(db, inv.ID)
	if _, byToken := camCollectMintedMedia(msgs); byToken[mediaToken] != "" {
		return true
	}
	children, _ := listCameraInvestigationChildren(db, inv.ID)
	for _, ch := range children {
		cmsgs, _ := listCameraInvestigationMessages(db, ch.ID)
		if _, byToken := camCollectMintedMedia(cmsgs); byToken[mediaToken] != "" {
			return true
		}
	}
	return false
}

// ─────────────────────────── state.json builder ───────────────────────────

// camViewStateJSON renders an investigation (and its delegated children) into the
// timeline page's poll payload. Media URLs are rewritten to the gated relative form
// /camera/media/<token>?v=<view_token>, and each is enriched with its capture's
// content_type/kind plus an `expired` flag (capture reaped or past its TTL) so the
// page can show an "expired" placeholder instead of a broken thumbnail. A single
// capture-lookup cache spans parent + children so a token cited twice is fetched
// once.
func camViewStateJSON(db *sql.DB, inv camInvestigation) map[string]any {
	capCache := map[string]*camCapture{}
	msgs, _ := listCameraInvestigationMessages(db, inv.ID)
	siteName := ""
	if s, err := getCameraSite(db, inv.SiteID); err == nil {
		siteName = s.Name
	}
	state := map[string]any{
		"status":     inv.Status,
		"question":   inv.Question,
		"site_name":  siteName,
		"created_at": inv.CreatedAt,
		"updated_at": inv.UpdatedAt,
		"turns":      camViewTurnsJSON(db, msgs, inv.ViewToken, capCache),
	}
	// Children carry the PARENT's view_token in their media URLs: the media gate
	// authorizes the union of parent + children media under that one token.
	if children, _ := listCameraInvestigationChildren(db, inv.ID); len(children) > 0 {
		carr := make([]map[string]any, 0, len(children))
		for _, ch := range children {
			cmsgs, _ := listCameraInvestigationMessages(db, ch.ID)
			carr = append(carr, map[string]any{
				"id": ch.ID, "question": ch.Question, "status": ch.Status,
				"turns": camViewTurnsJSON(db, cmsgs, inv.ViewToken, capCache),
			})
		}
		state["children"] = carr
	}
	return state
}

// camViewTurnsJSON maps a transcript to the page's turn shape {seq, role, content,
// tool_name, media?}.
func camViewTurnsJSON(db *sql.DB, msgs []camInvestigationMessage, viewToken string, capCache map[string]*camCapture) []map[string]any {
	out := make([]map[string]any, 0, len(msgs))
	for _, m := range msgs {
		turn := map[string]any{
			"seq": m.Seq, "role": m.Role, "content": m.Content, "tool_name": m.ToolName,
		}
		if media := camViewMediaJSON(db, m.MediaJSON, viewToken, capCache); len(media) > 0 {
			turn["media"] = media
		}
		out = append(out, turn)
	}
	return out
}

// camViewMediaJSON parses a row's media_json into the page's media shape, rewriting
// each URL to the gated relative form and attaching content_type/kind/expired.
func camViewMediaJSON(db *sql.DB, mediaJSON, viewToken string, capCache map[string]*camCapture) []map[string]any {
	if strings.TrimSpace(mediaJSON) == "" {
		return nil
	}
	var items []evidenceItem
	if json.Unmarshal([]byte(mediaJSON), &items) != nil {
		return nil
	}
	out := make([]map[string]any, 0, len(items))
	for _, it := range items {
		tok := camMediaToken(it.MediaURL)
		if tok == "" {
			continue
		}
		entry := map[string]any{
			"url":     "/camera/media/" + tok + "?v=" + viewToken,
			"caption": it.Caption,
		}
		if cap := camViewCaptureLookup(db, capCache, tok); cap == nil {
			entry["expired"] = true // row reaped/gone — not fetchable
		} else {
			entry["content_type"] = cap.ContentType
			entry["kind"] = cap.Kind
			entry["expired"] = cap.ExpiresAt != "" && cap.ExpiresAt <= nowRFC3339()
		}
		out = append(out, entry)
	}
	return out
}

// camViewCaptureLookup memoizes getCameraCaptureByToken across the state build; a
// cached nil records a miss so a repeated token never re-queries.
func camViewCaptureLookup(db *sql.DB, cache map[string]*camCapture, tok string) *camCapture {
	if c, ok := cache[tok]; ok {
		return c
	}
	if c, err := getCameraCaptureByToken(db, tok); err == nil {
		cc := c
		cache[tok] = &cc
		return &cc
	}
	cache[tok] = nil
	return nil
}

// ─────────────────────────── static shell ───────────────────────────

// camInvestigationViewHTML is the whole public timeline page: a self-contained dark
// chat UI whose JS reads the view_token from location.pathname, polls state.json
// (4s while queued/running, 15s while awaiting_operator, stops when terminal), and
// renders operator/ai/tool/system bubbles with inline media and collapsible child
// sections. Zero server-side interpolation — the CSP allows only inline script/style
// and same-origin media.
const camInvestigationViewHTML = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="robots" content="noindex, nofollow">
<title>Investigation</title>
<style>
  :root { color-scheme: dark; }
  * { box-sizing: border-box; }
  body {
    margin: 0; background: #0d1117; color: #e6edf3;
    font: 15px/1.5 -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, Helvetica, Arial, sans-serif;
  }
  .wrap { max-width: 760px; margin: 0 auto; padding: 20px 16px 64px; }
  header { border-bottom: 1px solid #21262d; padding-bottom: 16px; margin-bottom: 16px; }
  .site { font-size: 12px; letter-spacing: .04em; text-transform: uppercase; color: #7d8590; }
  h1 { font-size: 20px; font-weight: 600; margin: 6px 0 12px; word-wrap: break-word; }
  .badge {
    display: inline-block; font-size: 12px; font-weight: 600; padding: 3px 10px;
    border-radius: 999px; background: #21262d; color: #adbac7; border: 1px solid #30363d;
  }
  .s-running, .s-active, .s-queued { background: #123; color: #58a6ff; border-color: #1f6feb44; }
  .s-awaiting_operator { background: #2d2205; color: #e3b341; border-color: #bb800944; }
  .s-answered { background: #0f2a17; color: #3fb950; border-color: #23863644; }
  .s-exhausted, .s-closed { background: #2d1216; color: #f85149; border-color: #da363344; }
  .turn { margin: 12px 0; }
  .turn .meta {
    font-size: 11px; letter-spacing: .05em; text-transform: uppercase; color: #7d8590; margin-bottom: 4px;
  }
  .bubble {
    border: 1px solid #21262d; border-radius: 10px; padding: 10px 12px; white-space: pre-wrap;
    word-wrap: break-word; background: #161b22;
  }
  .role-operator .bubble { background: #132030; border-color: #1f3a5f; }
  .role-ai .bubble { background: #161b22; }
  .role-tool .bubble { background: #12171e; color: #adbac7; font-size: 14px; }
  .role-system .bubble { background: transparent; border-style: dashed; color: #7d8590; font-size: 13px; }
  .media-grid { display: grid; grid-template-columns: repeat(auto-fill, minmax(150px, 1fr)); gap: 8px; margin-top: 10px; }
  .media { border: 1px solid #21262d; border-radius: 8px; overflow: hidden; background: #0d1117; }
  .media img, .media video { display: block; width: 100%; height: auto; }
  .media .cap { font-size: 12px; color: #7d8590; padding: 5px 7px; }
  .media-expired {
    padding: 24px 10px; text-align: center; color: #7d8590; font-size: 12px; background: #12171e;
  }
  h2 { font-size: 14px; font-weight: 600; color: #adbac7; margin: 24px 0 8px; }
  details.child { border: 1px solid #21262d; border-radius: 10px; margin: 8px 0; overflow: hidden; }
  details.child > summary { cursor: pointer; padding: 10px 12px; background: #12171e; list-style: none; }
  details.child > summary::-webkit-details-marker { display: none; }
  .child-q { color: #adbac7; }
  .child-body { padding: 4px 12px 12px; border-top: 1px solid #21262d; }
  #foot { margin-top: 24px; color: #7d8590; font-size: 13px; text-align: center; }
  .spin { display: inline-block; width: 8px; height: 8px; border-radius: 50%; background: #58a6ff; margin-right: 6px; animation: pulse 1.2s ease-in-out infinite; }
  @keyframes pulse { 0%,100% { opacity: .3; } 50% { opacity: 1; } }
  .media-click { position: relative; cursor: zoom-in; }
  .media-click:hover { border-color: #388bfd; }
  .play-badge {
    position: absolute; inset: 0; display: flex; align-items: center; justify-content: center;
    font-size: 34px; color: #fff; text-shadow: 0 1px 10px #000; pointer-events: none;
  }
  #lb { position: fixed; inset: 0; z-index: 50; background: rgba(4,8,14,.93); display: flex; flex-direction: column; }
  #lb[hidden] { display: none; }
  #lb-stage { flex: 1; min-height: 0; display: flex; align-items: center; justify-content: center; padding: 14px; }
  #lb-stage img, #lb-stage video { max-width: 100%; max-height: 100%; border-radius: 8px; box-shadow: 0 8px 40px rgba(0,0,0,.6); }
  .lb-btn {
    position: absolute; z-index: 51; width: 42px; height: 42px; border-radius: 999px;
    background: #161b22cc; border: 1px solid #30363d; color: #e6edf3; font-size: 22px;
    line-height: 1; cursor: pointer; display: flex; align-items: center; justify-content: center;
  }
  .lb-btn:hover { background: #21262d; border-color: #388bfd; }
  #lb-close { top: 12px; right: 12px; }
  #lb-prev { left: 10px; top: 50%; transform: translateY(-50%); }
  #lb-next { right: 10px; top: 50%; transform: translateY(-50%); }
  #lb-bar { padding: 8px 60px 18px; text-align: center; color: #adbac7; font-size: 13px; }
  #lb-count { color: #7d8590; margin-right: 10px; }
  #lb-open { color: #58a6ff; margin-left: 10px; text-decoration: none; }
  #lb-open:hover { text-decoration: underline; }
</style>
</head>
<body>
<div class="wrap">
  <header>
    <div class="site" id="site"></div>
    <h1 id="question">Investigation</h1>
    <span class="badge" id="status"></span>
  </header>
  <main id="transcript"></main>
  <section id="children"></section>
  <div id="foot"></div>
</div>
<div id="lb" hidden>
  <button class="lb-btn" id="lb-close" aria-label="Close">&#10005;</button>
  <button class="lb-btn" id="lb-prev" aria-label="Previous">&#8249;</button>
  <div id="lb-stage"></div>
  <button class="lb-btn" id="lb-next" aria-label="Next">&#8250;</button>
  <div id="lb-bar"><span id="lb-count"></span><span id="lb-cap"></span><a id="lb-open" target="_blank" rel="noopener">open file &#8599;</a></div>
</div>
<script>
(function () {
  var LABELS = {
    queued: "Queued", running: "Working", active: "Working",
    awaiting_operator: "Needs your reply", answered: "Answered",
    exhausted: "Stopped", closed: "Closed"
  };
  var path = location.pathname.replace(/\/+$/, "");
  var stateUrl = path + "/state.json";
  var el = {
    site: document.getElementById("site"),
    question: document.getElementById("question"),
    status: document.getElementById("status"),
    transcript: document.getElementById("transcript"),
    children: document.getElementById("children"),
    foot: document.getElementById("foot")
  };
  var timer = null, missing = false;

  function label(s) { return LABELS[s] || s || ""; }
  function badge(node, s) { node.className = "badge s-" + (s || "unknown"); node.textContent = label(s); }
  function pollDelay(s) {
    if (s === "queued" || s === "running" || s === "active") return 4000;
    if (s === "awaiting_operator") return 15000;
    return 0;
  }
  function who(role, tool) {
    if (role === "operator") return "Operator";
    if (role === "ai") return "Investigator";
    if (role === "tool") return tool ? tool : "tool";
    return "System";
  }

  // Every non-expired media tile joins the page-wide gallery (parent + children,
  // DOM order) so the viewer can step through the whole investigation's evidence.
  var gallery = [];

  function makeMedia(m) {
    var box = document.createElement("div");
    box.className = "media";
    if (m.expired) {
      var ph = document.createElement("div");
      ph.className = "media-expired";
      ph.textContent = "evidence expired";
      box.appendChild(ph);
      return box;
    }
    var ct = m.content_type || "";
    var isVideo = ct.indexOf("video/") === 0;
    var node;
    if (isVideo) {
      // The tile shows the first frame; playback happens in the viewer where
      // there is room for it — a play badge signals the click.
      node = document.createElement("video");
      node.preload = "metadata"; node.muted = true; node.playsInline = true;
    } else {
      node = document.createElement("img");
      node.loading = "lazy"; node.alt = m.caption || "evidence";
    }
    node.src = m.url;
    node.addEventListener("error", function () {
      var ph = document.createElement("div");
      ph.className = "media-expired";
      ph.textContent = "evidence unavailable";
      if (node.parentNode) node.parentNode.replaceChild(ph, node);
      box.className = "media";
    });
    box.appendChild(node);
    if (isVideo) {
      var play = document.createElement("div");
      play.className = "play-badge";
      play.textContent = "▶";
      box.appendChild(play);
    }
    if (m.caption) {
      var cap = document.createElement("div");
      cap.className = "cap";
      cap.textContent = m.caption;
      box.appendChild(cap);
    }
    var idx = gallery.length;
    gallery.push({ url: m.url, content_type: ct, caption: m.caption || "" });
    box.className = "media media-click";
    box.addEventListener("click", function () { lbShow(idx); });
    return box;
  }

  // ── Media viewer (lightbox) ──
  var lbIndex = -1;
  var lbEl = {
    root: document.getElementById("lb"),
    stage: document.getElementById("lb-stage"),
    cap: document.getElementById("lb-cap"),
    count: document.getElementById("lb-count"),
    open: document.getElementById("lb-open"),
    close: document.getElementById("lb-close"),
    prev: document.getElementById("lb-prev"),
    next: document.getElementById("lb-next")
  };

  function lbShow(i) {
    if (!gallery.length) return;
    lbIndex = ((i % gallery.length) + gallery.length) % gallery.length;
    var m = gallery[lbIndex];
    lbEl.stage.textContent = "";
    var node;
    if ((m.content_type || "").indexOf("video/") === 0) {
      node = document.createElement("video");
      node.controls = true; node.autoplay = true; node.playsInline = true;
    } else {
      node = document.createElement("img");
      node.alt = m.caption || "evidence";
    }
    node.src = m.url;
    lbEl.stage.appendChild(node);
    lbEl.cap.textContent = m.caption;
    lbEl.count.textContent = (lbIndex + 1) + " / " + gallery.length;
    lbEl.open.href = m.url;
    lbEl.prev.style.display = gallery.length > 1 ? "" : "none";
    lbEl.next.style.display = gallery.length > 1 ? "" : "none";
    lbEl.root.hidden = false;
    document.body.style.overflow = "hidden";
  }

  function lbClose() {
    lbEl.root.hidden = true;
    lbEl.stage.textContent = ""; // detaches any playing video
    document.body.style.overflow = "";
    lbIndex = -1;
  }

  lbEl.close.addEventListener("click", lbClose);
  lbEl.prev.addEventListener("click", function () { lbShow(lbIndex - 1); });
  lbEl.next.addEventListener("click", function () { lbShow(lbIndex + 1); });
  lbEl.root.addEventListener("click", function (e) {
    if (e.target === lbEl.root || e.target === lbEl.stage) lbClose();
  });
  document.addEventListener("keydown", function (e) {
    if (lbEl.root.hidden) return;
    if (e.key === "Escape") lbClose();
    else if (e.key === "ArrowLeft") lbShow(lbIndex - 1);
    else if (e.key === "ArrowRight") lbShow(lbIndex + 1);
  });

  function makeTurn(t) {
    var row = document.createElement("div");
    row.className = "turn role-" + (t.role || "system");
    var meta = document.createElement("div");
    meta.className = "meta";
    meta.textContent = who(t.role, t.tool_name);
    row.appendChild(meta);
    var bubble = document.createElement("div");
    bubble.className = "bubble";
    if (t.content) {
      var body = document.createElement("span");
      body.textContent = t.content;
      bubble.appendChild(body);
    }
    if (t.media && t.media.length) {
      var grid = document.createElement("div");
      grid.className = "media-grid";
      for (var i = 0; i < t.media.length; i++) grid.appendChild(makeMedia(t.media[i]));
      bubble.appendChild(grid);
    }
    row.appendChild(bubble);
    return row;
  }

  function renderTurns(container, turns) {
    container.textContent = "";
    turns = turns || [];
    for (var i = 0; i < turns.length; i++) container.appendChild(makeTurn(turns[i]));
  }

  function renderChildren(children) {
    el.children.textContent = "";
    if (!children || !children.length) return;
    var h = document.createElement("h2");
    h.textContent = "Sub-investigations (" + children.length + ")";
    el.children.appendChild(h);
    for (var i = 0; i < children.length; i++) {
      var c = children[i];
      var det = document.createElement("details");
      det.className = "child";
      if (i === 0) det.open = true;
      var sum = document.createElement("summary");
      var st = document.createElement("span");
      badge(st, c.status);
      sum.appendChild(st);
      var q = document.createElement("span");
      q.className = "child-q";
      q.textContent = " " + (c.question || "");
      sum.appendChild(q);
      det.appendChild(sum);
      var body = document.createElement("div");
      body.className = "child-body";
      renderTurns(body, c.turns);
      det.appendChild(body);
      el.children.appendChild(det);
    }
  }

  function render(state) {
    // Rebuild the gallery alongside the DOM: the transcript is append-only, so
    // indexes already handed to open tiles (and the open viewer) stay valid.
    gallery = [];
    el.site.textContent = state.site_name || "";
    el.question.textContent = state.question || "Investigation";
    badge(el.status, state.status);
    renderTurns(el.transcript, state.turns);
    renderChildren(state.children);
    if (lbIndex >= 0) lbEl.count.textContent = (lbIndex + 1) + " / " + gallery.length;
  }

  function schedule(status) {
    var d = pollDelay(status);
    if (d > 0) {
      el.foot.innerHTML = "";
      var s = document.createElement("span"); s.className = "spin"; el.foot.appendChild(s);
      el.foot.appendChild(document.createTextNode("Live — updating"));
      timer = setTimeout(tick, d);
    } else {
      el.foot.textContent = "This investigation has finished.";
    }
  }

  function notFound() {
    el.question.textContent = "Investigation not found";
    badge(el.status, "closed");
    el.transcript.textContent = "";
    el.children.textContent = "";
    el.foot.textContent = "This link is invalid or has expired.";
  }

  function tick() {
    fetch(stateUrl, { headers: { "Accept": "application/json" } }).then(function (res) {
      if (res.status === 404) { missing = true; return null; }
      if (!res.ok) throw new Error("http " + res.status);
      return res.json();
    }).then(function (state) {
      if (missing) { notFound(); return; }
      render(state);
      schedule(state.status);
    }).catch(function () {
      el.foot.textContent = "Temporarily unavailable — retrying…";
      timer = setTimeout(tick, 8000);
    });
  }

  tick();
})();
</script>
</body>
</html>`
