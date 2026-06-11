// screens-monitor.jsx — Worker A: Dashboard + Logs (the data-viz centerpiece).
// Built on Phase-2 primitives (window.*) + tokens from base.css only.
// Per-screen CSS lives in screens-monitor.css (.dash- / .logs- prefixes).
// Screens receive props: { liveStatus, statusError, statusStale,
//   refreshStatus, onAction, navigate, pushToast }
(() => {
  const { useState, useEffect, useRef, useMemo, useCallback } = React;
  const {
    Icon, api, useLive, fmtUptime, fmtCount, fmtPct, fmtMs, timeAgo,
    PageHeader, Card, StatCard, Button, IconButton, Badge, Tabs, Switch,
    Input, CopyButton, SecretField, KeyValue, Sparkline, AreaChart,
    Skeleton, Spinner, EmptyState,
  } = window;

  const cx = (...p) => p.filter(Boolean).join(" ");
  const ZERO60 = Array.from({ length: 60 }, () => 0);
  const num = (v) => (v == null || isNaN(Number(v)) ? 0 : Number(v));

  /* ════════════════════════════════════════════════════════════════
     DASHBOARD
     ════════════════════════════════════════════════════════════════ */
  function Dashboard({ liveStatus, statusStale, onAction, navigate }) {
    const [chartTab, setChartTab] = useState("requests");

    // On-demand validation (GET /ui/api/validate) — same contract as old code.
    const [validation, setValidation] = useState(null); // {ok, ran_at, model, upstream_model, steps, duration_total}
    const [validating, setValidating] = useState(false);
    const runValidation = useCallback(() => {
      setValidating(true);
      api.get("/ui/api/validate")
        .then((res) => setValidation(res || null))
        .catch(() => {})
        .finally(() => setValidating(false));
    }, []);
    useEffect(() => { runValidation(); }, [runValidation]);

    const loading = !liveStatus;
    const running = !!(liveStatus && liveStatus.proxy_running);
    const dashboard = (liveStatus && liveStatus.dashboard) || {};
    const traffic = dashboard.traffic || {};
    const sparks = dashboard.sparks || {};
    const update = (liveStatus && liveStatus.update) || {};

    // Endpoint URLs (replicates old derivation, prefers server-provided fields).
    const rootUrl =
      (liveStatus && (liveStatus.display_url || liveStatus.public_url || liveStatus.local_url)) ||
      "http://127.0.0.1:4000";
    const anthropicUrl = (liveStatus && liveStatus.anthropic_url) || rootUrl + "/anthropic";
    const openaiUrl = (liveStatus && liveStatus.openai_url) || rootUrl + "/openai/v1";
    const isPublic = !!(liveStatus && liveStatus.public_url);

    // Sparklines.
    const reqSpark = sparks.requests || ZERO60;
    const latSpark = sparks.latency || ZERO60;
    const tokSpark = sparks.tokens || ZERO60;
    const errSpark = sparks.errors || ZERO60;

    // StatCard values.
    const reqPerMin = num(dashboard.requests_per_min);
    const reqDelta = dashboard.requests_delta_pct;
    const avgLatency = num(dashboard.avg_latency_ms);
    const tokPerMin = num(dashboard.tokens_per_min);
    const inTok = num(dashboard.input_tokens);
    const outTok = num(dashboard.output_tokens);
    const errRate = num(dashboard.error_rate);
    const errCount = num(dashboard.error_count);

    // Chart series — map the active spark to labeled points (−60s … now).
    const chartConf = {
      requests: { data: reqSpark, tone: "accent", fmt: (v) => fmtCount(v), unit: "requests" },
      latency: { data: latSpark, tone: "info", fmt: (v) => fmtMs(v), unit: "avg latency" },
      tokens: { data: tokSpark, tone: "success", fmt: (v) => fmtCount(v), unit: "tokens" },
      errors: { data: errSpark, tone: "danger", fmt: (v) => fmtCount(v), unit: "errors" },
    }[chartTab];
    const chartData = useMemo(() => {
      const arr = chartConf.data || ZERO60;
      const n = arr.length;
      const points = arr.map((v, i) => {
        const secsAgo = n - 1 - i;
        return { value: num(v), label: secsAgo === 0 ? "now" : "−" + secsAgo + "s" };
      });
      // All-zero window → render the chart's empty state instead of a flat line.
      const hasSignal = points.some((p) => p.value > 0);
      return hasSignal ? points : [];
    }, [chartConf, chartTab]);

    // Status breakdown bars.
    const breakdown = [
      { key: "2xx", label: "2xx success", tone: "success", count: num(traffic.status_2xx), pct: num(traffic.status_2xx_pct) },
      { key: "4xx", label: "4xx client", tone: "warning", count: num(traffic.status_4xx), pct: num(traffic.status_4xx_pct) },
      { key: "5xx", label: "5xx server", tone: "danger", count: num(traffic.status_5xx), pct: num(traffic.status_5xx_pct) },
      { key: "streamed", label: "Streamed (SSE)", tone: "info", count: num(traffic.streamed), pct: num(traffic.streamed_pct) },
    ];
    const breakdownTotal = num(traffic.total) || (num(traffic.status_2xx) + num(traffic.status_4xx) + num(traffic.status_5xx));

    // System card.
    const codexAuth = (liveStatus && liveStatus.codex_auth) || {};
    const updateAvailable = !!update.update_available;
    const updateRunning = !!update.running;
    const currentVersion = update.current_version || (liveStatus && liveStatus.version) || "—";
    const latestVersion = update.latest_version || "";
    const autoUpdate = !!update.auto_update;

    const headerActions = (
      <>
        <Button variant="secondary" icon="shield" loading={validating} onClick={runValidation}>Validate</Button>
        {running ? (
          <>
            <Button variant="secondary" icon="restart" onClick={() => onAction("restart")}>Restart</Button>
            <Button variant="danger" icon="stop" onClick={() => onAction("stop")}>Stop</Button>
          </>
        ) : (
          <Button variant="primary" icon="play" onClick={() => onAction("start")}>Start endpoints</Button>
        )}
      </>
    );

    return (
      <div className="screen" data-stale={statusStale ? "true" : undefined}>
        <PageHeader
          title={
            <span className="dash-title-row">
              Dashboard
              {loading ? (
                <Skeleton width={78} height={22} />
              ) : (
                <Badge tone={running ? "success" : "danger"} pulse={running}>
                  {running ? "Running" : "Stopped"}
                </Badge>
              )}
              {running && liveStatus && liveStatus.uptime_seconds != null && (
                <span className="dash-uptime tnum">up {fmtUptime(liveStatus.uptime_seconds)}</span>
              )}
            </span>
          }
          description="Live overview of proxy health, traffic and the Claude Code → Codex round-trip."
          actions={headerActions}
        />

        {/* Stat cards */}
        <div className="grid-stats">
          <StatCard
            label="Requests / min"
            value={loading ? "" : fmtCount(reqPerMin)}
            delta={loading ? undefined : (reqDelta == null ? undefined : num(reqDelta))}
            sub="last 60 seconds"
            spark={reqSpark}
            sparkTone="accent"
            loading={loading}
          />
          <StatCard
            label="Avg latency"
            value={loading ? "" : fmtMs(avgLatency)}
            sub={reqPerMin > 0 ? fmtCount(reqPerMin) + " req sampled" : "no traffic yet"}
            spark={latSpark}
            sparkTone="info"
            loading={loading}
          />
          <StatCard
            label="Tokens / min"
            value={loading ? "" : fmtCount(tokPerMin)}
            sub={"in " + fmtCount(inTok) + " · out " + fmtCount(outTok)}
            spark={tokSpark}
            sparkTone="success"
            loading={loading}
          />
          <StatCard
            label="Error rate"
            value={loading ? "" : fmtPct(errRate)}
            delta={loading ? undefined : (errCount > 0 ? fmtCount(errCount) + " err" : "clean")}
            deltaTone={errCount > 0 ? "danger" : "success"}
            sub={errCount > 0 ? fmtCount(errCount) + " in window" : "no errors"}
            spark={errSpark}
            sparkTone="danger"
            loading={loading}
          />
        </div>

        {/* Traffic chart */}
        <Card
          title="Traffic — last 60 seconds"
          description="Per-second activity through the Anthropic and OpenAI surfaces."
          actions={
            <Tabs
              variant="pill"
              active={chartTab}
              onChange={setChartTab}
              tabs={[
                { id: "requests", label: "Requests" },
                { id: "latency", label: "Latency" },
                { id: "tokens", label: "Tokens" },
                { id: "errors", label: "Errors" },
              ]}
            />
          }
        >
          {loading ? (
            <Skeleton width="100%" height={220} style={{ borderRadius: "var(--r-md)" }} />
          ) : (
            <AreaChart
              key={chartTab}
              data={chartData}
              height={224}
              tone={chartConf.tone}
              formatValue={chartConf.fmt}
              emptyLabel="No traffic in the last minute"
            />
          )}
        </Card>

        {/* Endpoints + System */}
        <div className="grid-2">
          <Card title="Endpoints" description="Point your clients at these base URLs.">
            <div className="dash-endpoints">
              <div className="dash-endpoint">
                <span className="dash-endpoint-label">Anthropic</span>
                {loading ? <Skeleton width="60%" height={16} /> : <code className="dash-endpoint-url mono truncate">{anthropicUrl}</code>}
                <CopyButton text={anthropicUrl} />
              </div>
              <div className="dash-endpoint">
                <span className="dash-endpoint-label">OpenAI</span>
                {loading ? <Skeleton width="60%" height={16} /> : <code className="dash-endpoint-url mono truncate">{openaiUrl}</code>}
                <CopyButton text={openaiUrl} />
              </div>
              <div className="dash-endpoint dash-endpoint-secret">
                <span className="dash-endpoint-label">Local API key</span>
                {loading ? (
                  <Skeleton width="60%" height={32} />
                ) : (
                  <SecretField
                    value={(liveStatus && liveStatus.proxy_key_masked) || ""}
                    copyValue={(liveStatus && liveStatus.proxy_key) || ""}
                    placeholder="Not set"
                  />
                )}
              </div>
            </div>
            <div className="dash-bind">
              <Badge tone={isPublic ? "info" : "neutral"}>
                <Icon name={isPublic ? "globe" : "lock"} size={11} />
                {isPublic ? "Public HTTPS" : "Local-only · 127.0.0.1"}
              </Badge>
              <span className="subtle">
                {isPublic ? "The local app stays private; clients use the public URL." : "Bound to loopback for local clients."}
              </span>
            </div>
          </Card>

          <Card title="System">
            {loading ? (
              <div className="dash-sys-loading">
                {[0, 1, 2, 3, 4].map((i) => <Skeleton key={i} width={i % 2 ? "70%" : "85%"} height={16} />)}
              </div>
            ) : (
              <>
                <KeyValue
                  items={[
                    { label: "PID", value: running ? (liveStatus.pid || "—") : "—", mono: true },
                    {
                      label: "Upstream",
                      value: (
                        <>
                          <span className="mono">{liveStatus.upstream || "codex"}</span>
                          <span className="subtle">chatgpt subscription</span>
                        </>
                      ),
                    },
                    {
                      label: "Codex auth",
                      value: (
                        <>
                          {codexAuth.exists ? (
                            <Badge tone="success">Found · {codexAuth.mode || "unknown"}</Badge>
                          ) : (
                            <Badge tone="danger">Missing</Badge>
                          )}
                          <span className="subtle mono truncate dash-auth-path">{codexAuth.path || "~/.codex/auth.json"}</span>
                        </>
                      ),
                    },
                    {
                      label: "Version",
                      value: (
                        <>
                          <span className="mono">{String(currentVersion).replace(/^v/, "v")}</span>
                          {updateRunning ? (
                            <Badge tone="info" pulse>Updating</Badge>
                          ) : updateAvailable ? (
                            <Badge tone="warning">v{String(latestVersion).replace(/^v/, "")} available</Badge>
                          ) : (
                            <Badge tone="success">Up to date</Badge>
                          )}
                        </>
                      ),
                    },
                  ]}
                />
                <div className="dash-update-row">
                  <Button
                    variant={updateAvailable ? "primary" : "secondary"}
                    size="sm"
                    icon={updateRunning ? "refresh" : "download"}
                    loading={updateRunning}
                    disabled={updateRunning}
                    onClick={() => onAction("update")}
                  >
                    {updateRunning ? "Updating…" : "Update now"}
                  </Button>
                  <Switch
                    checked={autoUpdate}
                    onChange={() => onAction("toggle-auto-update")}
                    label="Auto-update"
                  />
                  <button type="button" className="dash-update-link" onClick={() => navigate("updates")}>
                    Manage <Icon name="arrow-right" size={11} />
                  </button>
                </div>
              </>
            )}
          </Card>
        </div>

        {/* Validation + Status breakdown */}
        <div className="grid-2">
          <Card
            title="Validation"
            description="End-to-end round-trip checks against the proxy."
            actions={
              <Button variant="secondary" size="sm" icon="refresh" loading={validating} onClick={runValidation}>
                Run again
              </Button>
            }
            flush
          >
            {!validation && validating ? (
              <div className="dash-validation-loading">
                <Spinner size={16} /> <span className="muted">Running validation…</span>
              </div>
            ) : !validation || !(validation.steps && validation.steps.length) ? (
              <EmptyState
                compact
                icon="shield"
                title="No validation run yet"
                description="Run the checks to confirm Claude Code can reach Codex."
                action={<Button variant="primary" size="sm" icon="shield" loading={validating} onClick={runValidation}>Run validation</Button>}
              />
            ) : (
              <>
                <table className="table dash-validation-table">
                  <thead>
                    <tr>
                      <th>Check</th>
                      <th>Result</th>
                      <th className="dash-col-right">Duration</th>
                    </tr>
                  </thead>
                  <tbody>
                    {validation.steps.map((s, i) => (
                      <tr key={i} className={s.ok ? undefined : "tr-danger"}>
                        <td className="mono dash-check-name">{s.name}</td>
                        <td>
                          <Badge tone={s.ok ? "success" : "danger"}>
                            {num(s.status) || (s.ok ? 200 : 0)} {s.ok ? "OK" : "FAIL"}
                          </Badge>
                        </td>
                        <td className="mono tnum dash-col-right">{fmtMs(num(s.duration_ms))}</td>
                      </tr>
                    ))}
                  </tbody>
                </table>
                <div className="dash-validation-foot">
                  <Icon name={validation.ok ? "check" : "warning"} size={13} className={validation.ok ? "dash-ok" : "dash-warn"} />
                  <span>{validation.ok ? "All checks passed" : "Some checks failed"}</span>
                  <span className="subtle">·</span>
                  <span className="subtle">ran {validation.ran_at || "—"}</span>
                  <span className="subtle">·</span>
                  <span className="subtle mono truncate">{validation.upstream_model || validation.model || "—"}</span>
                </div>
              </>
            )}
          </Card>

          <Card title="Status breakdown" description="Response classes over the last 60 seconds.">
            {loading ? (
              <div className="dash-bars-loading">
                {[0, 1, 2, 3].map((i) => <Skeleton key={i} width="100%" height={28} />)}
              </div>
            ) : breakdownTotal === 0 ? (
              <EmptyState compact icon="activity" title="No traffic yet" description="Send a request through the proxy to see the response mix." />
            ) : (
              <div className="dash-bars">
                {breakdown.map((b) => (
                  <div className="dash-bar-row" key={b.key}>
                    <div className="dash-bar-head">
                      <span className="dash-bar-label">{b.label}</span>
                      <span className="dash-bar-count tnum mono">
                        {b.count}
                        <span className="subtle"> · {fmtPct(b.pct)}</span>
                      </span>
                    </div>
                    <div className="dash-bar-track">
                      <span
                        className="dash-bar-fill"
                        data-tone={b.tone}
                        style={{ width: Math.max(b.count > 0 ? 2 : 0, Math.min(100, b.pct)) + "%" }}
                      />
                    </div>
                  </div>
                ))}
              </div>
            )}
          </Card>
        </div>
      </div>
    );
  }

  /* ════════════════════════════════════════════════════════════════
     LOGS
     ════════════════════════════════════════════════════════════════ */
  const SOURCE_TABS = [
    { id: "requests", label: "Requests", icon: "logs" },
    { id: "stdout", label: "stdout", icon: "terminal" },
    { id: "stderr", label: "stderr", icon: "terminal" },
    { id: "trace", label: "trace", icon: "activity" },
  ];
  const LEVELS = [
    { id: "info", label: "info", tone: "info" },
    { id: "warn", label: "warn", tone: "warning" },
    { id: "error", label: "error", tone: "danger" },
  ];
  // Normalize a row level to one of the three filter buckets.
  const levelBucket = (lvl) => {
    const l = String(lvl || "").toLowerCase();
    if (l === "error" || l === "err" || l === "fatal") return "error";
    if (l === "warn" || l === "warning") return "warn";
    return "info"; // info, debug, trace, "" → info bucket
  };
  const statusTone = (status) => {
    const s = num(status);
    if (s === 0) return "subtle";
    if (s >= 500) return "danger";
    if (s >= 400) return "warning";
    if (s >= 300) return "info";
    return "success";
  };

  function Logs() {
    const { data, stale, reconnecting, lastUpdated } = useLive("/ui/api/logs", { interval: 1200 });

    const [source, setSource] = useState("requests");
    const [levelOn, setLevelOn] = useState({ info: true, warn: true, error: true });
    const [query, setQuery] = useState("");
    const [autoScroll, setAutoScroll] = useState(true);
    const [paused, setPaused] = useState(false);

    // Frozen snapshot while paused; "cleared" hides everything up to a watermark.
    const [frozen, setFrozen] = useState(null);
    const [clearedAt, setClearedAt] = useState(0); // ignore rows with .at <= this
    const scrollRef = useRef(null);
    const pinnedRef = useRef(true); // user is pinned to the bottom

    const live = data || {};
    const snapshot = paused && frozen ? frozen : live;
    const allRows = Array.isArray(snapshot.rows) ? snapshot.rows : [];

    // Drop rows cleared locally (watermark on row .at timestamp).
    const visibleRows = useMemo(
      () => (clearedAt > 0 ? allRows.filter((r) => num(r.at) > clearedAt) : allRows),
      [allRows, clearedAt]
    );

    // Live level counts (over the request rows, post-clear).
    const counts = useMemo(() => {
      const c = { info: 0, warn: 0, error: 0 };
      visibleRows.forEach((r) => { c[levelBucket(r.lvl)]++; });
      return c;
    }, [visibleRows]);

    // Wired search + level filtering for the structured (requests) view.
    const q = query.trim().toLowerCase();
    const filteredRows = useMemo(() => {
      return visibleRows.filter((r) => {
        if (!levelOn[levelBucket(r.lvl)]) return false;
        if (!q) return true;
        const hay = [r.path, r.note, r.meth, r.model, r.upstream, r.status != null ? String(r.status) : ""]
          .filter(Boolean).join(" ").toLowerCase();
        return hay.includes(q);
      });
    }, [visibleRows, levelOn, q]);

    // Raw text views (stdout/stderr/trace), greppable by the same search box.
    const rawText = useMemo(() => {
      if (source === "requests") return "";
      const txt = snapshot[source] || "";
      if (!q) return txt;
      const matched = txt.split("\n").filter((line) => line.toLowerCase().includes(q));
      return matched.join("\n");
    }, [source, snapshot, q]);
    const rawLineCount = useMemo(() => {
      if (source === "requests") return 0;
      const txt = (paused && frozen ? frozen : live)[source] || "";
      return txt ? txt.split("\n").filter((l) => l.length > 0).length : 0;
    }, [source, live, frozen, paused]);

    // Smart pin: track whether the user is near the bottom; re-pin within 40px.
    const onScroll = useCallback(() => {
      const el = scrollRef.current;
      if (!el) return;
      const dist = el.scrollHeight - el.scrollTop - el.clientHeight;
      pinnedRef.current = dist < 40;
      // Disable auto-scroll when the user scrolls up; re-enable when re-pinned.
      setAutoScroll((cur) => (pinnedRef.current ? true : cur && false));
    }, []);

    // Auto-scroll to bottom on new content when pinned + enabled.
    useEffect(() => {
      const el = scrollRef.current;
      if (!el || !autoScroll || !pinnedRef.current) return;
      el.scrollTop = el.scrollHeight;
    }, [filteredRows, rawText, autoScroll, source]);

    const togglePause = () => {
      setPaused((p) => {
        const next = !p;
        if (next) setFrozen(live); // freeze the current snapshot
        else setFrozen(null);
        return next;
      });
    };
    const onToggleAutoScroll = (val) => {
      setAutoScroll(val);
      if (val) {
        pinnedRef.current = true;
        const el = scrollRef.current;
        if (el) el.scrollTop = el.scrollHeight;
      }
    };
    const clearView = () => {
      // Local-only clear: watermark the latest row time + remember raw lengths.
      const latest = allRows.reduce((mx, r) => Math.max(mx, num(r.at)), 0);
      setClearedAt(latest || Date.now());
    };

    // Copy current view as text.
    const copyText = source === "requests"
      ? filteredRows.map((r) => {
          const st = r.status ? " " + r.status : "";
          const du = r.dur ? " " + r.dur + "ms" : "";
          const meth = r.meth ? r.meth.padEnd(5) : "     ";
          return (r.ts || "") + " " + levelBucket(r.lvl).toUpperCase().padEnd(5) + " " + meth + (r.path || "") + st + du + (r.note ? "  " + r.note : "");
        }).join("\n")
      : rawText;

    const totalLines = source === "requests" ? visibleRows.length : rawLineCount;
    const shownLines = source === "requests" ? filteredRows.length : (rawText ? rawText.split("\n").filter((l) => l.length).length : 0);
    const liveState = paused ? "paused" : reconnecting ? "reconnecting" : "live";

    const isRaw = source !== "requests";
    const emptyStructured = filteredRows.length === 0;
    const emptyRaw = !rawText;

    return (
      <div className="screen logs-screen">
        <PageHeader
          title={
            <span className="dash-title-row">
              Logs
              <Badge tone={paused ? "neutral" : "success"} pulse={!paused && !reconnecting}>
                {paused ? "Paused" : reconnecting ? "Reconnecting" : "Live"}
              </Badge>
            </span>
          }
          description="Request stream, process stdout/stderr and structured trace — updated every 1.2s."
        />

        <Card className="logs-card" flush>
          {/* Toolbar */}
          <div className="logs-toolbar">
            <div className="logs-toolbar-row logs-toolbar-top">
              <Tabs tabs={SOURCE_TABS} active={source} onChange={setSource} />
              <div className="logs-toolbar-spacer" />
              <div className="logs-search">
                <Icon name="search" size={13} />
                <Input
                  className="logs-search-input"
                  placeholder={isRaw ? "Grep this stream…" : "Filter by path, method, status, note…"}
                  value={query}
                  onChange={(e) => setQuery(e.target.value)}
                  aria-label="Filter log lines"
                />
                {query && (
                  <IconButton icon="x" label="Clear filter" size={12} onClick={() => setQuery("")} />
                )}
              </div>
            </div>

            <div className="logs-toolbar-row logs-toolbar-bottom">
              <div className="logs-levels" role="group" aria-label="Filter by level">
                {LEVELS.map((lv) => (
                  <button
                    key={lv.id}
                    type="button"
                    className={cx("logs-chip", levelOn[lv.id] && "active")}
                    data-tone={lv.tone}
                    aria-pressed={levelOn[lv.id]}
                    disabled={isRaw}
                    onClick={() => setLevelOn((s) => ({ ...s, [lv.id]: !s[lv.id] }))}
                  >
                    <span className="logs-chip-dot" />
                    {lv.label}
                    <span className="logs-chip-count tnum">{counts[lv.id]}</span>
                  </button>
                ))}
              </div>

              <div className="logs-toolbar-spacer" />

              <Switch checked={autoScroll} onChange={onToggleAutoScroll} label="Auto-scroll" />
              <Button variant="ghost" size="sm" icon={paused ? "play" : "stop"} onClick={togglePause}>
                {paused ? "Resume" : "Pause"}
              </Button>
              <CopyButton text={() => copyText} label="Copy" size="sm" />
              <Button variant="ghost" size="sm" icon="trash" onClick={clearView}>Clear</Button>
            </div>
          </div>

          {/* Log body */}
          <div className="logs-body" ref={scrollRef} onScroll={onScroll}>
            {isRaw ? (
              emptyRaw ? (
                <EmptyState
                  icon="terminal"
                  title={query ? "No matching lines" : "No " + source + " output"}
                  description={query ? "No lines in this stream match your filter." : "This stream has not emitted any output yet."}
                />
              ) : (
                <pre className="logs-raw mono">{rawText}</pre>
              )
            ) : emptyStructured ? (
              <EmptyState
                icon="logs"
                title={query || !(levelOn.info && levelOn.warn && levelOn.error) ? "No matching log lines" : "No requests yet"}
                description={
                  query || !(levelOn.info && levelOn.warn && levelOn.error)
                    ? "No request rows match the current filters."
                    : "Requests through the proxy will appear here in real time."
                }
              />
            ) : (
              <div className="logs-rows">
                {filteredRows.map((r, i) => {
                  const bucket = levelBucket(r.lvl);
                  return (
                    <div key={(r.at || "") + "-" + i} className="logs-row" data-level={bucket}>
                      <span className="logs-ts mono">{r.ts || ""}</span>
                      <span className="logs-level" data-level={bucket}>{bucket}</span>
                      <span className="logs-msg">
                        {r.meth && <span className="logs-method" data-method={r.meth}>{r.meth}</span>}
                        {r.path && <span className="logs-path mono">{r.path}</span>}
                        {r.status != null && r.status !== 0 && (
                          <span className="logs-status mono" data-tone={statusTone(r.status)}>{r.status}</span>
                        )}
                        {r.dur != null && r.dur !== 0 && <span className="logs-dur mono tnum">{r.dur}ms</span>}
                        {r.model && <span className="logs-meta mono">{r.model}</span>}
                        {r.upstream && <span className="logs-meta mono">→{r.upstream}</span>}
                        {(num(r.input_tokens) > 0 || num(r.output_tokens) > 0) && (
                          <span className="logs-meta mono tnum">{num(r.input_tokens)}↓ {num(r.output_tokens)}↑</span>
                        )}
                        {r.stream && <span className="logs-meta logs-stream">SSE</span>}
                        {r.note && <span className="logs-note">{r.note}</span>}
                      </span>
                    </div>
                  );
                })}
              </div>
            )}
          </div>

          {/* Footer */}
          <div className="logs-footer">
            <span className="logs-footer-stat tnum">
              {totalLines} {totalLines === 1 ? "line" : "lines"}
              {shownLines !== totalLines && <span className="subtle"> · showing {shownLines}</span>}
            </span>
            <div className="logs-toolbar-spacer" />
            <span className={cx("logs-livedot", "logs-livedot-" + liveState)} aria-hidden="true" />
            <span className="logs-footer-state">{liveState}</span>
            {stale && lastUpdated && <span className="subtle">· updated {timeAgo(lastUpdated)}</span>}
          </div>
        </Card>
      </div>
    );
  }

  window.Dashboard = Dashboard;
  window.Logs = Logs;
})();
