package main

// camera_observer_view.go — the PUBLIC, no-admin-session share page for a
// daily activity report, addressed by its unguessable view_token capability id
// (minted at claim, preserved across regeneration — camera_observer_store.go):
//
//   GET /camera/reports/<view_token>             → static HTML shell
//   GET /camera/reports/<view_token>/state.json  → report JSON
//
// A clone of the investigation timeline page (camera_investigate_view.go) with
// a strictly smaller surface: text-only in v1 (no media gate), read-only (no
// reply/write endpoint), no polling (a finished report is static — the shell
// fetches state.json once). The same hardening applies: per-IP-then-per-token
// rate limits, noindex/no-referrer/no-store + tight CSP
// (camSetViewSecurityHeaders), a zero-interpolation static shell, and uniform
// 404s on the JSON path so an unknown token leaks nothing.

import (
	"database/sql"
	"io"
	"net/http"
	"strings"
)

// camLogDayViewURL builds the public share URL for a daily report, or "" when
// the row has no view_token (nothing to link). Background callers only (the
// webhook payload), so the base URL comes from cfg.PublicURL via imageBaseURL.
func camLogDayViewURL(cfg config, d logDay) string {
	if strings.TrimSpace(d.ViewToken) == "" {
		return ""
	}
	return strings.TrimRight(imageBaseURL(cfg, nil), "/") + "/camera/reports/" + d.ViewToken
}

// handleCameraReportView dispatches the public report routes: the bare token
// serves the HTML shell (static — leaks nothing, so it renders for any
// well-formed token and lets an unknown one show "not found" once state.json
// 404s), and a "/state.json" suffix serves the report JSON. Rate-limited FIRST
// (per IP, then per token — the short-circuit order that keeps a unique-token
// spray from minting fresh buckets, see handleCameraInvestigationView).
func handleCameraReportView(cfg config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rest := strings.TrimPrefix(r.URL.Path, "/camera/reports/")
		if rest == "" || r.Method != http.MethodGet && r.Method != http.MethodHead {
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
		if !camSvcRateAllow("reportip:"+ip, camViewPageRatePerMin) || !camSvcRateAllow("report:"+token, camViewPageRatePerMin) {
			http.Error(w, "rate limited", http.StatusTooManyRequests)
			return
		}
		camSetViewSecurityHeaders(w)

		if !wantState {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = io.WriteString(w, camReportViewHTML)
			return
		}

		db, err := openProxyDB(cfg)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		defer db.Close()
		d, derr := getLogDayByViewToken(db, token)
		if derr != nil {
			http.NotFound(w, r) // unknown token → uniform 404 (no token oracle)
			return
		}
		writeJSON(w, http.StatusOK, camReportViewStateJSON(db, d))
	}
}

// camReportViewStateJSON renders one daily report for the share page. Names
// resolve best-effort (a deleted stream still shows the report). The window's
// from/to are the stored UTC instants — the page renders the date and the raw
// report text, so no site-local math happens server-side.
func camReportViewStateJSON(db *sql.DB, d logDay) map[string]any {
	siteName, streamName := "", ""
	if s, err := getCameraSite(db, d.SiteID); err == nil {
		siteName = s.Name
	}
	if st, err := getLogStream(db, d.StreamID); err == nil {
		streamName = st.Name
	}
	return map[string]any{
		"date":         d.Day,
		"site_name":    siteName,
		"stream_name":  streamName,
		"headline":     d.Headline,
		"report":       d.Report,
		"language":     d.Language,
		"status":       d.Status,
		"from":         d.FromTS,
		"to":           d.ToTS,
		"hours_used":   d.HoursUsed,
		"entries_used": d.EntriesUsed,
		"generated_at": d.CreatedAt,
	}
}

// camReportViewHTML is the whole public report page: a self-contained dark
// document view whose JS reads the view_token from location.pathname and
// fetches state.json ONCE (a report is static; a pending one shows its status
// and offers a manual reload). Zero server-side interpolation — every dynamic
// string lands via textContent, and dir="auto" on the text nodes lets Arabic
// (or any RTL) reports lay out correctly. The CSP allows only inline
// script/style and same-origin connect (the state fetch).
const camReportViewHTML = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="robots" content="noindex, nofollow">
<title>Daily report</title>
<style>
  :root { color-scheme: dark; }
  * { box-sizing: border-box; }
  body {
    margin: 0; background: #0d1117; color: #e6edf3;
    font: 15px/1.6 -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, Helvetica, Arial, sans-serif;
  }
  .wrap { max-width: 720px; margin: 0 auto; padding: 24px 16px 64px; }
  header { border-bottom: 1px solid #21262d; padding-bottom: 16px; margin-bottom: 20px; }
  .site { font-size: 12px; letter-spacing: .04em; text-transform: uppercase; color: #7d8590; }
  .date { font-size: 13px; color: #adbac7; margin-top: 4px; }
  h1 { font-size: 20px; font-weight: 600; margin: 10px 0 12px; word-wrap: break-word; }
  .badge {
    display: inline-block; font-size: 12px; font-weight: 600; padding: 3px 10px;
    border-radius: 999px; background: #21262d; color: #adbac7; border: 1px solid #30363d;
  }
  .s-done { background: #0f2a17; color: #3fb950; border-color: #23863644; }
  .s-empty { background: #21262d; color: #adbac7; border-color: #30363d; }
  .s-pending { background: #123; color: #58a6ff; border-color: #1f6feb44; }
  .s-error { background: #2d1216; color: #f85149; border-color: #da363344; }
  #report {
    border: 1px solid #21262d; border-radius: 10px; padding: 16px 18px; background: #161b22;
    white-space: pre-wrap; word-wrap: break-word;
  }
  #foot { margin-top: 24px; color: #7d8590; font-size: 13px; text-align: center; }
</style>
</head>
<body>
<div class="wrap">
  <header>
    <div class="site" id="site"></div>
    <div class="date" id="date"></div>
    <h1 id="headline" dir="auto">Daily report</h1>
    <span class="badge" id="status" hidden></span>
  </header>
  <article id="report" dir="auto"></article>
  <div id="foot">Loading…</div>
</div>
<script>
(function () {
  var LABELS = { done: "Report", empty: "No activity", pending: "Generating", error: "Failed" };
  var path = location.pathname.replace(/\/+$/, "");
  var el = {
    site: document.getElementById("site"),
    date: document.getElementById("date"),
    headline: document.getElementById("headline"),
    status: document.getElementById("status"),
    report: document.getElementById("report"),
    foot: document.getElementById("foot")
  };

  function render(st) {
    el.site.textContent = st.site_name || "";
    var when = st.date || "";
    if (st.stream_name) when += (when ? " — " : "") + st.stream_name;
    el.date.textContent = when;
    el.headline.textContent = st.headline || "Daily report";
    el.status.textContent = LABELS[st.status] || st.status || "";
    el.status.className = "badge s-" + (st.status || "unknown");
    el.status.hidden = false;
    el.report.textContent = st.report || "";
    el.foot.textContent = st.status === "pending" ? "This report is still being generated — reload in a minute." : "";
  }

  function notFound() {
    el.headline.textContent = "Report not found";
    el.report.textContent = "";
    el.foot.textContent = "This link is invalid or has expired.";
  }

  fetch(path + "/state.json", { headers: { "Accept": "application/json" } }).then(function (res) {
    if (res.status === 404) { notFound(); return null; }
    if (!res.ok) throw new Error("http " + res.status);
    return res.json();
  }).then(function (st) {
    if (st) render(st);
  }).catch(function () {
    el.foot.textContent = "Temporarily unavailable — reload to retry.";
  });
})();
</script>
</body>
</html>`
