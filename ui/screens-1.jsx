// screens.jsx — content for each screen of the dashboard
// Depends on: window.Icon, window.Sparkline, window.Bars, sample data globals

const { useState, useEffect, useRef, useMemo } = React;

// ─────────────────────────── shared bits ───────────────────────────

const Pill = ({ tone = "muted", pulse = false, children }) => (
  <span className={`pill ${pulse ? "pill-pulse" : ""}`} data-tone={tone}>
    <span className="dot"></span>{children}
  </span>
);

const SectionHd = ({ title, sub, actions }) => (
  <div className="sec-hd">
    <div>
      <h1>{title}</h1>
      {sub && <p>{sub}</p>}
    </div>
    {actions && <div className="sec-hd-actions">{actions}</div>}
  </div>
);

const Card = ({ title, actions, flush, children }) => (
  <div className="card">
    {title && (
      <div className="card-hd">
        <span>{title}</span>
        {actions && <><div className="spacer"></div><div className="actions">{actions}</div></>}
      </div>
    )}
    <div className={`card-bd ${flush ? "flush" : ""}`}>{children}</div>
  </div>
);

const TokenField = ({ value, copyValue }) => {
  const [shown, setShown] = useState(false);
  const [copied, setCopied] = useState(false);
  const text = value || "";
  const clipboardText = copyValue || text;
  const masked = "•".repeat(Math.max(8, Math.min(24, text.length || clipboardText.length || 8)));
  const copy = () => {
    navigator.clipboard?.writeText(clipboardText).catch(() => {});
    setCopied(true);
    setTimeout(() => setCopied(false), 1100);
  };
  return (
    <div className="token-fld" style={{ maxWidth: 360 }}>
      <span className="v">{shown ? clipboardText : masked}</span>
      <button onClick={() => setShown(s => !s)} title={shown ? "Hide" : "Reveal"}>
        <Icon name={shown ? "eyeOff" : "eye"} size={12}/>
      </button>
      <button onClick={copy} title="Copy">
        <Icon name={copied ? "check" : "copy"} size={12}/>
      </button>
    </div>
  );
};

const CopyBtn = ({ text, label = "Copy" }) => {
  const [copied, setCopied] = useState(false);
  const copy = () => {
    navigator.clipboard?.writeText(text).catch(() => {});
    setCopied(true);
    setTimeout(() => setCopied(false), 1100);
  };
  return (
    <button className="btn btn-sm btn-ghost" onClick={copy}>
      <Icon name={copied ? "check" : "copy"} size={11}/>
      {copied ? "Copied" : label}
    </button>
  );
};

const stateMeta = {
  running: { tone: "ok",   label: "Running",  pulse: true },
  warning: { tone: "warn", label: "Running",  pulse: true },
  stopped: { tone: "err",  label: "Stopped",  pulse: false },
};

// ─────────────────────────── DASHBOARD ───────────────────────────

const Dashboard = ({ proxyState, onAction, liveStatus, statusError }) => {
  const m = stateMeta[proxyState];
  const isUp = proxyState !== "stopped";
  const [validation, setValidation] = useState(VALIDATION_STEPS);
  const [validationMeta, setValidationMeta] = useState(null);
  const runValidation = () => api.get("/ui/api/validate").then(res => {
    setValidation((res.steps || []).map(s => ({
      name: s.name,
      expect: `${s.status || 0} ${s.ok ? "OK" : "FAILED"}`,
      tone: s.ok ? "ok" : "err",
      ms: s.duration_ms || 0,
    })));
    setValidationMeta({
      ok: !!res.ok,
      ran_at: res.ran_at || "",
      model: res.upstream_model || res.model || "",
    });
  }).catch(() => {});
  useEffect(() => { runValidation(); }, []);

  const dashboard = liveStatus?.dashboard || {};
  const traffic = dashboard.traffic || {};
  const sparks = dashboard.sparks || {};
  const lastValidation = validationMeta || dashboard.last_validation || {};
  const zeroSpark = Array.from({ length: 60 }, () => 0);
  const reqSpark = sparks.requests || zeroSpark;
  const latSpark = sparks.latency || zeroSpark;
  const tokSpark = sparks.tokens || zeroSpark;
  const errSpark = sparks.errors || zeroSpark;
  const fmtCount = (n) => {
    n = Number(n || 0);
    if (n >= 1000000) return `${(n / 1000000).toFixed(1)}m`;
    if (n >= 1000) return `${(n / 1000).toFixed(n >= 10000 ? 0 : 1)}k`;
    return `${n}`;
  };
  const fmtPct = (n) => `${Number(n || 0).toFixed(1)}%`;
  const fmtDelta = (n) => {
    n = Number(n || 0);
    if (!Number.isFinite(n) || Math.abs(n) < 0.1) return "no change";
    return `${n > 0 ? "▲" : "▼"} ${Math.abs(n).toFixed(0)}%`;
  };
  const validationOk = validation.length > 0 && validation.every(s => s.tone === "ok");
  const rootUrl = liveStatus?.display_url || liveStatus?.public_url || liveStatus?.local_url || "http://127.0.0.1:4000";
  const anthropicUrl = liveStatus?.anthropic_url || `${rootUrl}/anthropic`;
  const openaiUrl = liveStatus?.openai_url || `${rootUrl}/openai/v1`;
  const update = liveStatus?.update || {};
  const updateRunning = !!update.running;
  const updateFailed = update.state === "failed";
  const updateAvailable = !!update.update_available;
  const currentVersion = update.current_version || liveStatus?.version || "unknown";
  const latestVersion = update.latest_version || update.latest?.latest_version || "checking";
  const updateTone = updateRunning ? "info" : updateFailed ? "err" : updateAvailable ? "warn" : "ok";
  const updateLabel = updateRunning ? "Updating" : updateFailed ? "Failed" : updateAvailable ? "Available" : "Current";
  const updateMessage = statusError && updateRunning
    ? "Reconnecting while the service restarts"
    : update.message || (updateAvailable ? `Latest ${latestVersion}` : "No update pending");

  return (
    <section data-screen-label="01 Dashboard" className="col" style={{ gap: 16 }}>
      <SectionHd
        title="Dashboard"
        sub="Live status of the local proxy and the connection between Claude Code and your Codex subscription."
        actions={
          <>
            <button className="btn btn-sm btn-ghost"><Icon name="refresh" size={11}/>Refresh</button>
            <button className="btn btn-sm btn-ghost"><Icon name="logs" size={11}/>Open logs</button>
          </>
        }
      />

      {/* Hero state row */}
      <div className="card">
        <div style={{ display: "grid", gridTemplateColumns: "1.2fr 1fr", gap: 0 }}>
          <div style={{ padding: "18px 20px", borderRight: "1px solid var(--line)" }}>
            <div className="row" style={{ gap: 12, marginBottom: 14 }}>
              <Pill tone={m.tone} pulse={m.pulse}>{m.label}</Pill>
              {proxyState === "warning" && <Pill tone="warn">Codex auth · expires in 7d</Pill>}
              {proxyState === "stopped" && <Pill tone="muted">Endpoints paused</Pill>}
              <span className="txt-3 mono">uptime {isUp ? fmtUptime(liveStatus?.uptime_seconds || 0) : "—"}</span>
            </div>
            <div className="col" style={{ gap: 6, marginBottom: 10 }}>
              <div style={{ display: "flex", alignItems: "baseline", gap: 14 }}>
                <span className="txt-3 mono" style={{ width: 70 }}>Anthropic</span>
                <span className="mono" style={{ fontSize: 22, fontWeight: 500, color: "var(--fg)" }}>
                  {anthropicUrl}
                </span>
                <CopyBtn text={anthropicUrl} label="Copy"/>
              </div>
              <div style={{ display: "flex", alignItems: "baseline", gap: 14 }}>
                <span className="txt-3 mono" style={{ width: 70 }}>OpenAI</span>
                <span className="mono" style={{ fontSize: 16, fontWeight: 500, color: "var(--fg-1)" }}>
                  {openaiUrl}
                </span>
                <CopyBtn text={openaiUrl} label="Copy"/>
              </div>
            </div>
            <p className="txt-2" style={{ margin: "2px 0 16px" }}>
              Split Anthropic/OpenAI API surfaces · forwarding to <span className="mono" style={{color:"var(--fg-1)"}}>codex chatgpt</span> backend
            </p>
            <div className="row" style={{ gap: 6 }}>
              {isUp ? (
                <>
                  <button className="btn btn-err" onClick={() => onAction("stop")}>
                    <Icon name="stop" size={12}/>Stop endpoints
                  </button>
                  <button className="btn" onClick={() => onAction("restart")}>
                    <Icon name="restart" size={12}/>Restart
                  </button>
                </>
              ) : (
                <button className="btn btn-primary" onClick={() => onAction("start")}>
                  <Icon name="play" size={12}/>Start endpoints
                </button>
              )}
              <button className="btn" onClick={() => onAction("validate")}>
                <Icon name="bolt" size={12}/>Validate
              </button>
              <div className="spacer"></div>
              <span className="hdr-bind"><Icon name="lock" size={11}/>{liveStatus?.public_url ? "Public HTTPS · local app remains private" : "Local-only · bound to 127.0.0.1"}</span>
            </div>
          </div>
          <div style={{ padding: "18px 20px" }}>
            <div className="kv" style={{ gridTemplateColumns: "130px 1fr", rowGap: 8 }}>
              <div className="k">PID</div>
              <div className="v mono">{isUp ? (liveStatus?.pid || "—") : "—"}</div>
              <div className="k">Upstream</div>
              <div className="v mono">{liveStatus?.upstream || "codex"} <span className="txt-3">· chatgpt subscription</span></div>
              <div className="k">Codex auth</div>
              <div className="v">
                {liveStatus?.codex_auth?.exists
                  ? <Pill tone="ok">Found · {liveStatus?.codex_auth?.mode || "unknown"}</Pill>
                  : <Pill tone="err">Missing</Pill>}
                <span className="txt-3 mono">{liveStatus?.codex_auth?.path || "~/.codex/auth.json"}</span>
              </div>
              <div className="k">Local API key</div>
              <div className="v"><TokenField value={liveStatus?.proxy_key_masked || DEFAULT_CONFIG.PROXY_API_KEY} copyValue={liveStatus?.proxy_key}/></div>
              <div className="k">Claude settings</div>
              <div className="v mono">
                <span style={{color:"var(--fg)"}}>{liveStatus?.claude_settings?.mode || "none"}</span>
                <span className="txt-3"> · api key {liveStatus?.claude_settings?.api_key_present ? "present" : "absent"} · cache {liveStatus?.claude_settings?.gateway_cache_present ? "present" : "absent"}</span>
              </div>
              <div className="k">Codex sessions</div>
              <div className="v mono">
                <span style={{color:"var(--fg)"}}>{liveStatus?.codex_sessions?.count || 0}</span>
                <span className="txt-3"> · prompt key {liveStatus?.codex_sessions?.prompt_cache_key_enabled ? "on" : "off"} · one-shot {liveStatus?.codex_sessions?.side_thread_count || 0}</span>
              </div>
              <div className="k">Browser bridge</div>
              <div className="v">
                {liveStatus?.antigravity?.ready
                  ? <Pill tone="ok">Ready</Pill>
                  : <Pill tone="warn">Needs check</Pill>}
                <span className="txt-3 mono">
                  Code {liveStatus?.antigravity?.mcp?.present ? "MCP ready" : "MCP missing"} · Desktop {liveStatus?.antigravity?.mcp?.desktop?.present ? "MCP ready" : "MCP missing"}
                  {liveStatus?.antigravity?.extension?.manifest?.version ? ` · ext ${liveStatus.antigravity.extension.manifest.version}` : ""}
                </span>
              </div>
              <div className="k">Active aliases</div>
              <div className="v mono"><span style={{color:"var(--fg)"}}>{liveStatus?.models?.length || 0}</span><span className="txt-3"> · opus[1m], sonnet[1m], gpt-5.3-codex</span></div>
              <div className="k">Version</div>
              <div className="v update-line">
                <Pill tone={updateTone} pulse={updateRunning}>{updateLabel}</Pill>
                <span className="mono">{currentVersion}</span>
                <span className="txt-3 mono">latest {latestVersion || "checking"}</span>
              </div>
              <div className="k">Update</div>
              <div className="v update-line">
                <button className={`btn btn-sm ${updateAvailable ? "btn-warn" : ""}`} disabled={updateRunning} onClick={() => onAction("update")}>
                  <Icon name={updateRunning ? "refresh" : "download"} size={11}/>{updateRunning ? "Working" : "Update"}
                </button>
                <button className={`btn btn-sm ${update.auto_update ? "btn-ok" : "btn-ghost"}`} onClick={() => onAction("toggle-auto-update")}>
                  <Icon name={update.auto_update ? "check" : "refresh"} size={11}/>{update.auto_update ? "Auto on" : "Auto off"}
                </button>
                <span className="txt-3 mono update-message">{updateMessage}</span>
              </div>
              <div className="k">Last request</div>
              <div className="v mono">{liveStatus?.last_request ? `${liveStatus.last_request.meth} ${liveStatus.last_request.path} · ${liveStatus.last_request.status}` : "—"}</div>
            </div>
          </div>
        </div>
      </div>

      {/* Stats */}
      <div className="stats">
        <div className="stat">
          <div className="lbl">Requests / min</div>
          <div className="row sb">
            <span className="val">{isUp ? fmtCount(dashboard.requests_per_min) : "0"}</span>
            <span className={`sub ${Number(dashboard.requests_delta_pct || 0) >= 0 ? "delta-up" : "delta-down"}`}>{fmtDelta(dashboard.requests_delta_pct)}</span>
          </div>
          <div className="spark"><Sparkline data={reqSpark}/></div>
        </div>
        <div className="stat">
          <div className="lbl">Avg latency</div>
          <div className="row sb">
            <span className="val">{isUp ? (Number(dashboard.avg_latency_ms || 0) / 1000).toFixed(2) : "—"}<span style={{fontSize:13,color:"var(--fg-2)",marginLeft:4}}>s</span></span>
            <span className="sub">{fmtCount(dashboard.requests_per_min)} req · last 60s</span>
          </div>
          <div className="spark"><Sparkline data={latSpark}/></div>
        </div>
        <div className="stat">
          <div className="lbl">Tokens / min</div>
          <div className="row sb">
            <span className="val">{isUp ? fmtCount(dashboard.tokens_per_min) : "0"}</span>
            <span className="sub">in {fmtCount(dashboard.input_tokens)} · out {fmtCount(dashboard.output_tokens)}</span>
          </div>
          <div className="spark"><Sparkline data={tokSpark}/></div>
        </div>
        <div className="stat">
          <div className="lbl">Error rate</div>
          <div className="row sb">
            <span className="val">{Number(dashboard.error_rate || 0).toFixed(1)}<span style={{fontSize:13,color:"var(--fg-2)",marginLeft:4}}>%</span></span>
            <span className="sub">{dashboard.error_count ? `${dashboard.error_count} errors` : "no errors"}</span>
          </div>
          <div className="spark"><Sparkline data={errSpark}/></div>
        </div>
      </div>

      {/* Two-col validation + recent traffic */}
      <div className="col-2">
        <Card title="Last validation" actions={<button className="btn btn-sm" onClick={runValidation}><Icon name="bolt" size={11}/>Run again</button>} flush>
          <table className="tbl">
            <thead><tr><th>Check</th><th>Result</th><th style={{textAlign:"right"}}>Duration</th></tr></thead>
            <tbody>
              {validation.map(s => (
                <tr key={s.name}>
                  <td className="mono">{s.name}</td>
                  <td><Pill tone={s.tone}>{s.expect}</Pill></td>
                  <td className="mono" style={{textAlign:"right", color:"var(--fg-2)"}}>{s.ms < 1000 ? `${s.ms} ms` : `${(s.ms/1000).toFixed(2)} s`}</td>
                </tr>
              ))}
            </tbody>
          </table>
          <div style={{padding:"10px 14px", borderTop:"1px solid var(--line)", fontSize:11.5, color:"var(--fg-2)", display:"flex", alignItems:"center", gap:8}}>
            <Icon name={validationOk ? "check" : "warn"} size={11}/>
            {validationOk ? "All checks passed" : "Some checks failed"} · last run {lastValidation.ran_at || "not yet"} · model {lastValidation.upstream_model || lastValidation.model || "—"}
          </div>
        </Card>

        <Card title="Recent traffic (last 60s)" actions={<span className="txt-3 mono">window 1s</span>}>
          <div style={{ marginBottom: 14 }}>
            <Bars data={reqSpark.map(v => Math.max(0, v))}/>
            <div className="row sb txt-3 mono" style={{marginTop:6}}>
              <span>−60s</span><span>−30s</span><span>now</span>
            </div>
          </div>
          <div className="kv" style={{gridTemplateColumns:"1fr 1fr", rowGap:8}}>
            <div className="k">2xx</div><div className="v mono"><span style={{color:"var(--ok)"}}>{traffic.status_2xx || 0}</span> <span className="txt-3">{fmtPct(traffic.status_2xx_pct)}</span></div>
            <div className="k">4xx</div><div className="v mono"><span style={{color: traffic.status_4xx ? "var(--warn)" : "var(--fg)"}}>{traffic.status_4xx || 0}</span> <span className="txt-3">{fmtPct(traffic.status_4xx_pct)}</span></div>
            <div className="k">5xx</div><div className="v mono"><span style={{color: traffic.status_5xx ? "var(--err)" : "var(--fg)"}}>{traffic.status_5xx || 0}</span> <span className="txt-3">{fmtPct(traffic.status_5xx_pct)}</span></div>
            <div className="k">streamed</div><div className="v mono">{traffic.streamed || 0} <span className="txt-3">{fmtPct(traffic.streamed_pct)}</span></div>
          </div>
        </Card>
      </div>
    </section>
  );
};

// ─────────────────────────── CONFIGURATION ───────────────────────────

const Configuration = ({ pushToast }) => {
  const [cfg, setCfg] = useState(DEFAULT_CONFIG);
  const [secrets, setSecrets] = useState({});
  const [aliases, setAliases] = useState(ALIAS_DEFAULTS);
  const [dirty, setDirty] = useState(false);
  const [authMeta, setAuthMeta] = useState(null);

  useEffect(() => {
    api.get("/ui/api/config").then(res => {
      setCfg(c => ({ ...c, ...res.config }));
      setSecrets(res.secrets || {});
      setAliases((res.aliases || []).map(a => ({ from: a.from, to: a.to, context: a.context || "200k" })));
      setDirty(false);
    }).catch(() => {});
    api.get("/ui/api/status").then(res => setAuthMeta(res.codex_auth)).catch(() => {});
  }, []);

  const update = (k, v) => { setCfg(c => ({ ...c, [k]: v })); setDirty(true); };
  const updateAlias = (i, k, v) => {
    setAliases(a => a.map((row, idx) => idx === i ? { ...row, [k]: v } : row));
    setDirty(true);
  };
  const removeAlias = (i) => { setAliases(a => a.filter((_, idx) => idx !== i)); setDirty(true); };
  const addAlias = () => { setAliases(a => [...a, { from: "", to: "", context: "200k" }]); setDirty(true); };

  const portValid = /^\d{2,5}$/.test(cfg.PROXY_PORT) && +cfg.PROXY_PORT >= 1024;
  const urlValid = /^https?:\/\//.test(cfg.CODEX_BASE_URL);
  const canSave = dirty && portValid && urlValid;

  const save = () => {
    if (!canSave) return;
    api.post("/ui/api/config", { config: cfg, aliases }).then(res => {
      setDirty(false);
      pushToast(res.message || "Configuration saved · restart proxy to apply");
    }).catch(e => pushToast(`Save failed · ${e.message || e}`));
  };
  const reset = () => { setCfg(DEFAULT_CONFIG); setAliases(ALIAS_DEFAULTS); setDirty(true); };

  return (
    <section data-screen-label="02 Configuration" className="col" style={{ gap: 16 }}>
      <SectionHd
        title="Configuration"
        sub="Environment for the local proxy. Changes are written to .env and applied on restart."
        actions={
          <>
            {dirty && <Pill tone="warn">Unsaved changes</Pill>}
            <button className="btn btn-sm btn-ghost" onClick={reset}>
              <Icon name="restart" size={11}/>Reset to defaults
            </button>
            <button className={`btn btn-sm ${canSave ? "btn-primary" : ""}`} disabled={!canSave} onClick={save}>
              <Icon name="check" size={11}/>Save
            </button>
          </>
        }
      />

      <Card title="Upstream" flush>
        <div className="frm-row">
          <div className="lbl">
            <span className="name">UPSTREAM</span>
            <span className="desc">Where this proxy forwards requests.</span>
          </div>
          <div className="ctl">
            <div className="seg">
              {["codex", "antigravity", "openai"].map(opt => (
                <button key={opt} aria-pressed={cfg.UPSTREAM === opt} onClick={() => update("UPSTREAM", opt)}>
                  {opt}
                </button>
              ))}
            </div>
            <div className="hint">
              <Icon name="info" size={11}/>
              {cfg.UPSTREAM === "codex"
                ? "Uses your Codex ChatGPT subscription via ~/.codex/auth.json."
                : cfg.UPSTREAM === "antigravity"
                ? "Uses Google Antigravity/Gemini API via your ANTIGRAVITY_API_KEY."
                : "Uses a raw OpenAI API key. Codex-only models will be unavailable."}
            </div>
          </div>
        </div>

        <div className="frm-row">
          <div className="lbl">
            <span className="name">CODEX_BASE_URL</span>
            <span className="desc">Endpoint of the Codex backend.</span>
          </div>
          <div className="ctl">
            <input className={`inp ${urlValid ? "" : "invalid"}`} value={cfg.CODEX_BASE_URL} onChange={e => update("CODEX_BASE_URL", e.target.value)}/>
            {!urlValid && <div className="hint err"><Icon name="err" size={11}/>Must start with http:// or https://</div>}
          </div>
        </div>

        <div className="frm-row">
          <div className="lbl">
            <span className="name">CODEX_AUTH_FILE</span>
            <span className="desc">Path to the local Codex auth cache.</span>
          </div>
          <div className="ctl">
            <div className="row" style={{ gap: 6 }}>
              <input className="inp" value={cfg.CODEX_AUTH_FILE} onChange={e => update("CODEX_AUTH_FILE", e.target.value)}/>
              <button className="btn btn-sm"><Icon name="folder" size={11}/>Browse</button>
            </div>
            <div className={`hint ${authMeta?.exists ? "ok" : "err"}`}><Icon name={authMeta?.exists ? "check" : "err"} size={11}/>{authMeta?.exists ? `File exists · mode ${authMeta.mode || "unknown"}` : "File missing or unavailable"}</div>
          </div>
        </div>
      </Card>

      <Card title="Local proxy" flush>
        <div className="frm-row">
          <div className="lbl">
            <span className="name">PROXY_PUBLIC_URL</span>
            <span className="desc">Public HTTPS/domain URL shown to clients. Standard ports stay hidden.</span>
          </div>
          <div className="ctl">
            <input className="inp" value={cfg.PROXY_PUBLIC_URL || ""} onChange={e => update("PROXY_PUBLIC_URL", e.target.value)} placeholder="https://ai-api1.cus.cx"/>
            <div className="hint"><Icon name="info" size={11}/>Leave empty for local-only installs.</div>
          </div>
        </div>
        <div className="frm-row">
          <div className="lbl">
            <span className="name">PROXY_PORT</span>
            <span className="desc">Port to bind on 127.0.0.1.</span>
          </div>
          <div className="ctl">
            <input className={`inp ${portValid ? "" : "invalid"}`} value={cfg.PROXY_PORT} onChange={e => update("PROXY_PORT", e.target.value)} style={{maxWidth: 120}}/>
            {portValid
              ? <div className="hint"><Icon name="info" size={11}/>Port available · proxy will bind to <span className="mono" style={{color:"var(--fg-1)"}}>127.0.0.1:{cfg.PROXY_PORT}</span></div>
              : <div className="hint err"><Icon name="err" size={11}/>Use a numeric port ≥ 1024.</div>}
          </div>
        </div>

        <div className="frm-row">
          <div className="lbl">
            <span className="name">PROXY_API_KEY</span>
            <span className="desc">Token Claude Code presents to this proxy. Never sent upstream.</span>
          </div>
          <div className="ctl">
            <div className="row" style={{ gap: 6 }}>
              <TokenField value={cfg.PROXY_API_KEY} copyValue={secrets.PROXY_API_KEY}/>
              <button className="btn btn-sm"><Icon name="refresh" size={11}/>Rotate</button>
            </div>
            <div className="hint"><Icon name="lock" size={11}/>This is the local-proxy authentication, not your Codex ChatGPT auth.</div>
          </div>
        </div>

        <div className="frm-row">
          <div className="lbl">
            <span className="name">REQUEST_TIMEOUT_MS</span>
            <span className="desc">Per-request timeout against the upstream.</span>
          </div>
          <div className="ctl">
            <input className="inp" value={cfg.REQUEST_TIMEOUT_MS} onChange={e => update("REQUEST_TIMEOUT_MS", e.target.value)} style={{maxWidth: 160}}/>
          </div>
        </div>

        <div className="frm-row">
          <div className="lbl">
            <span className="name">LOG_LEVEL</span>
            <span className="desc">Verbosity of the proxy log stream.</span>
          </div>
          <div className="ctl">
            <div className="seg">
              {["debug", "info", "warn", "error"].map(opt => (
                <button key={opt} aria-pressed={cfg.LOG_LEVEL === opt} onClick={() => update("LOG_LEVEL", opt)}>{opt}</button>
              ))}
            </div>
          </div>
        </div>
      </Card>

      <Card
        title="Model aliases"
        actions={<button className="btn btn-sm" onClick={addAlias}><Icon name="plus" size={11}/>Add alias</button>}
        flush
      >
        <div style={{padding:"10px 14px 6px", fontSize: 11.5, color: "var(--fg-2)", display:"flex", alignItems:"center", gap: 8, borderBottom:"1px solid var(--line)"}}>
          <Icon name="info" size={11}/>
          Claude Code asks for Anthropic model names; the proxy rewrites the <span className="mono" style={{color:"var(--fg-1)"}}>model</span> field before forwarding.
        </div>
        <div style={{padding:"12px 14px"}}>
          <div className="alias-row" style={{marginBottom: 6}}>
            <span className="txt-3" style={{fontSize:10.5, textTransform:"uppercase", letterSpacing:"0.06em"}}>Public alias (Claude Code)</span>
            <span></span>
            <span className="txt-3" style={{fontSize:10.5, textTransform:"uppercase", letterSpacing:"0.06em"}}>Codex model</span>
            <span className="txt-3" style={{fontSize:10.5, textTransform:"uppercase", letterSpacing:"0.06em"}}>Context</span>
            <span></span>
          </div>
          {aliases.map((a, i) => (
            <div className="alias-row" key={i} style={{ marginBottom: 6 }}>
              <input className="inp" value={a.from} onChange={e => updateAlias(i, "from", e.target.value)}/>
              <span className="arrow"><Icon name="arrowR" size={12}/></span>
              <input className="inp" value={a.to} onChange={e => updateAlias(i, "to", e.target.value)}/>
              <select className="inp" value={a.context || "200k"} onChange={e => updateAlias(i, "context", e.target.value)} style={{ height: 30 }}>
                <option value="200k">200k</option>
                <option value="1m">1M</option>
              </select>
              <button className="btn btn-sm btn-ghost" onClick={() => removeAlias(i)} title="Remove">
                <Icon name="trash" size={11}/>
              </button>
            </div>
          ))}
        </div>
      </Card>
    </section>
  );
};

// ─────────────────────────── API KEYS ───────────────────────────

const ApiKeys = ({ pushToast = () => {}, liveStatus }) => {
  const [data, setData] = useState({ providers: [], clients: [], defaults: {} });
  const [providerForm, setProviderForm] = useState({ id: "", provider: "gemini", label: "Google AI Studio", base_url: "", api_key: "" });
  const [clientForm, setClientForm] = useState({ label: "Claude Code", schema: "both", provider: "default", provider_key_id: "" });
  const [createdKey, setCreatedKey] = useState("");
  const load = () => api.get("/ui/api/keys").then(setData).catch(e => pushToast(`Load failed · ${e.message || e}`));
  useEffect(() => { load(); }, []);
  const defaults = data.defaults || {};
  const providerOptions = data.providers || [];
  const selectedProviderKeys = providerOptions.filter(p => p.provider === clientForm.provider && p.enabled);
  const rootUrl = liveStatus?.display_url || liveStatus?.public_url || liveStatus?.local_url || "http://127.0.0.1:4000";
  const anthropicUrl = liveStatus?.anthropic_url || defaults.anthropic_base || `${rootUrl}/anthropic`;
  const openaiUrl = liveStatus?.openai_url || defaults.openai_local_url || `${rootUrl}/openai/v1`;
  const providerBaseDefault = providerForm.provider === "gemini" ? defaults.gemini_base_url : defaults.openai_base_url;

  const saveProvider = () => {
    api.post("/ui/api/keys/provider", { ...providerForm, base_url: providerForm.base_url || providerBaseDefault })
      .then(res => {
        setData(d => ({ ...d, providers: res.providers || d.providers, clients: res.clients || d.clients }));
        setProviderForm(f => ({ ...f, id: "", api_key: "" }));
        pushToast(res.message || "Provider key saved");
      })
      .catch(e => pushToast(`Save failed · ${e.message || e}`));
  };
  const renewProvider = (p) => {
    setProviderForm({
      id: p.id,
      provider: p.provider,
      label: p.label,
      base_url: p.base_url,
      api_key: "",
    });
    pushToast(`Renewing ${p.label} · paste the new provider key`);
  };
  const createClient = () => {
    api.post("/ui/api/keys/client", clientForm)
      .then(res => {
        setCreatedKey(res.api_key || "");
        setData(d => ({ ...d, providers: res.providers || d.providers, clients: res.clients || d.clients }));
        pushToast(res.message || "Client key created");
      })
      .catch(e => pushToast(`Create failed · ${e.message || e}`));
  };
  const toggle = (kind, row) => {
    api.post("/ui/api/keys/toggle", { kind, id: row.id, enabled: !row.enabled })
      .then(res => setData(d => ({ ...d, providers: res.providers || d.providers, clients: res.clients || d.clients })))
      .catch(e => pushToast(`Update failed · ${e.message || e}`));
  };

  return (
    <section data-screen-label="04 API Keys" className="col" style={{ gap: 16 }}>
      <SectionHd
        title="API keys"
        sub="Manage provider keys for OpenAI and Google AI Studio, plus local client keys for Claude Code or OpenAI-compatible clients."
        actions={<button className="btn btn-sm btn-ghost" onClick={load}><Icon name="refresh" size={11}/>Refresh</button>}
      />

      <div className="stats">
        <div className="stat">
          <div className="lbl">Anthropic base URL</div>
          <div className="row sb">
            <span className="mono" style={{ color: "var(--fg)", fontSize: 13 }}>{anthropicUrl}</span>
            <CopyBtn text={anthropicUrl} label="Copy"/>
          </div>
          <div className="sub">Use this with Claude Code.</div>
        </div>
        <div className="stat">
          <div className="lbl">OpenAI-compatible base URL</div>
          <div className="row sb">
            <span className="mono" style={{ color: "var(--fg)", fontSize: 13 }}>{openaiUrl}</span>
            <CopyBtn text={openaiUrl} label="Copy"/>
          </div>
          <div className="sub">Use this for OpenAI SDK-compatible clients.</div>
        </div>
        <div className="stat">
          <div className="lbl">Provider keys</div>
          <div className="val">{providerOptions.length}</div>
          <div className="sub">OpenAI · Google AI Studio</div>
        </div>
        <div className="stat">
          <div className="lbl">Client keys</div>
          <div className="val">{(data.clients || []).length}</div>
          <div className="sub">Local proxy authentication</div>
        </div>
      </div>

      <div className="col-2">
        <Card title={providerForm.id ? "Renew provider key" : "Add provider key"}>
          <div className="col" style={{ gap: 10 }}>
            <div className="seg">
              {[
                { value: "gemini", label: "Google AI Studio" },
                { value: "openai", label: "OpenAI" },
              ].map(opt => (
                <button key={opt.value} aria-pressed={providerForm.provider === opt.value} onClick={() => setProviderForm(f => ({ ...f, id: "", provider: opt.value, label: opt.label, base_url: "" }))}>{opt.label}</button>
              ))}
            </div>
            <input className="inp inp-sans" placeholder="Label" value={providerForm.label} onChange={e => setProviderForm(f => ({ ...f, label: e.target.value }))}/>
            <input className="inp inp-sans mono" placeholder={providerBaseDefault || "Provider base URL"} value={providerForm.base_url} onChange={e => setProviderForm(f => ({ ...f, base_url: e.target.value }))}/>
            <input className="inp inp-sans mono" placeholder="Provider API key" value={providerForm.api_key} onChange={e => setProviderForm(f => ({ ...f, api_key: e.target.value }))} type="password"/>
            <button className="btn btn-primary" disabled={!providerForm.api_key} onClick={saveProvider}><Icon name="check" size={12}/>{providerForm.id ? "Renew provider key" : "Save provider key"}</button>
            {providerForm.id && <button className="btn btn-sm btn-ghost" onClick={() => setProviderForm({ id: "", provider: "gemini", label: "Google AI Studio", base_url: "", api_key: "" })}>Cancel renewal</button>}
            <div className="hint"><Icon name="lock" size={11}/>Schema: OpenAI-compatible. Gemini uses the Google AI Studio OpenAI-compatible base URL; keys are encrypted in SQLite.</div>
          </div>
        </Card>

        <Card title="Create client key">
          <div className="col" style={{ gap: 10 }}>
            <input className="inp inp-sans" placeholder="Label" value={clientForm.label} onChange={e => setClientForm(f => ({ ...f, label: e.target.value }))}/>
            <select className="inp inp-sans" value={clientForm.schema} onChange={e => setClientForm(f => ({ ...f, schema: e.target.value }))}>
              <option value="both">Both schemas</option>
              <option value="anthropic">Anthropic only</option>
              <option value="openai">OpenAI-compatible only</option>
            </select>
            <select className="inp inp-sans" value={clientForm.provider} onChange={e => setClientForm(f => ({ ...f, provider: e.target.value, provider_key_id: "" }))}>
              <option value="default">Default proxy route</option>
              <option value="gemini">Google AI Studio provider key</option>
              <option value="openai">OpenAI provider key</option>
            </select>
            {clientForm.provider !== "default" && (
              <select className="inp inp-sans" value={clientForm.provider_key_id} onChange={e => setClientForm(f => ({ ...f, provider_key_id: e.target.value }))}>
                <option value="">Use first enabled {clientForm.provider} key</option>
                {selectedProviderKeys.map(p => <option key={p.id} value={p.id}>{p.label} · {p.key_preview}</option>)}
              </select>
            )}
            <button className="btn btn-primary" onClick={createClient}><Icon name="plus" size={12}/>Create client key</button>
            {createdKey && (
              <div className="created-key">
                <div className="txt-3">Copy this key now. It will not be shown again.</div>
                <TokenField value={createdKey} copyValue={createdKey}/>
              </div>
            )}
          </div>
        </Card>
      </div>

      <div className="col-2">
        <Card title="Provider keys" flush>
          <table className="tbl">
            <thead><tr><th>Provider</th><th>Schema</th><th>Label</th><th>Base URL</th><th>Key</th><th>Status</th><th></th></tr></thead>
            <tbody>
              {(data.providers || []).map(p => (
                <tr key={p.id}>
                  <td className="mono">{p.provider}</td>
                  <td className="mono">{p.schema || "openai-compatible"}</td>
                  <td>{p.label}</td>
                  <td className="mono">{p.base_url}</td>
                  <td className="mono">{p.key_preview}</td>
                  <td>{p.enabled ? <Pill tone="ok">Enabled</Pill> : <Pill tone="muted">Disabled</Pill>}</td>
                  <td className="row" style={{ justifyContent: "flex-end", gap: 6 }}>
                    <button className="btn btn-sm btn-ghost" onClick={() => renewProvider(p)}>Renew</button>
                    <button className="btn btn-sm btn-ghost" onClick={() => toggle("provider", p)}>{p.enabled ? "Disable" : "Enable"}</button>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
          {!(data.providers || []).length && <div className="empty-table">No provider keys yet.</div>}
        </Card>

        <Card title="Client keys" flush>
          <table className="tbl">
            <thead><tr><th>Label</th><th>Key</th><th>Schema</th><th>Route</th><th>Status</th><th></th></tr></thead>
            <tbody>
              {(data.clients || []).map(k => (
                <tr key={k.id}>
                  <td>{k.label}</td>
                  <td className="mono">{k.key_preview}</td>
                  <td className="mono">{k.schema || "both"}</td>
                  <td className="mono">{k.provider ? `${k.provider}${k.provider_label ? " · " + k.provider_label : ""}` : "default"}</td>
                  <td>{k.enabled ? <Pill tone="ok">Enabled</Pill> : <Pill tone="muted">Disabled</Pill>}</td>
                  <td><button className="btn btn-sm btn-ghost" onClick={() => toggle("client", k)}>{k.enabled ? "Disable" : "Enable"}</button></td>
                </tr>
              ))}
            </tbody>
          </table>
          {!(data.clients || []).length && <div className="empty-table">No client keys yet.</div>}
        </Card>
      </div>
    </section>
  );
};

// ─────────────────────────── MODELS ───────────────────────────

const CODEX_MODEL_OPTIONS = ["gpt-5.5", "gpt-5.4", "gpt-5.4-mini", "gpt-5.3-codex"];

function modelStatusFor(real) {
  if (!real) return "unsupported";
  return CODEX_MODEL_OPTIONS.includes(real) ? "ok" : "untested";
}

function normalizeModelDraft(m, idx = 0) {
  const alias = m.alias || "";
  const real = m.real || "";
  const status = m.status || modelStatusFor(real);
  return {
    id: m.id || alias || `model-${idx}`,
    alias,
    real,
    status,
    context: (m.context || "200k").toLowerCase() === "1m" ? "1m" : "200k",
    desc: m.desc || `${alias || "New alias"} maps to ${real || "no upstream model"}`,
    default: !!m.default,
    recommended: !!m.recommended,
  };
}

const Models = ({ pushToast = () => {} }) => {
  const [filter, setFilter] = useState("all");
  const [query, setQuery] = useState("");
  const [models, setModels] = useState(SAMPLE_MODELS.map(normalizeModelDraft));
  const [editing, setEditing] = useState(null);
  const [dirty, setDirty] = useState(false);
  const [saving, setSaving] = useState(false);
  const refresh = () => api.get("/ui/api/models").then(res => {
    setModels((res.models || []).map((m, i) => normalizeModelDraft({
      alias: m.alias,
      real: m.real,
      status: m.status,
      context: m.context || "200k",
      desc: `${m.alias} maps to ${m.real}`,
      default: m.default,
      recommended: m.recommended,
    }, i)));
    setDirty(false);
    setEditing(null);
  }).catch(() => {});
  useEffect(() => { refresh(); }, []);
  const aliasCounts = useMemo(() => models.reduce((acc, m) => {
    const key = m.alias.trim().toLowerCase();
    if (key) acc[key] = (acc[key] || 0) + 1;
    return acc;
  }, {}), [models]);
  const invalidIds = useMemo(() => new Set(models.filter(m => {
    const alias = m.alias.trim();
    const real = m.real.trim();
    return !alias || !real || aliasCounts[alias.toLowerCase()] > 1 || /\s/.test(alias);
  }).map(m => m.id)), [models, aliasCounts]);
  const filtered = models.filter(m => {
    const statusMatch = filter === "all"
      || m.status === filter
      || (filter === "warn" && (m.status === "warn" || m.status === "untested"));
    const q = query.trim().toLowerCase();
    const queryMatch = !q || m.alias.toLowerCase().includes(q) || m.real.toLowerCase().includes(q);
    return statusMatch && queryMatch;
  });
  const counts = models.reduce((acc, m) => { acc[m.status] = (acc[m.status] || 0) + 1; return acc; }, {});
  const canSave = dirty && !saving && invalidIds.size === 0;

  const updateModel = (id, key, value) => {
    setModels(rows => rows.map(row => {
      if (row.id !== id) return row;
      const next = normalizeModelDraft({ ...row, [key]: value, status: key === "real" ? modelStatusFor(value) : row.status });
      if (key === "real") next.status = modelStatusFor(value.trim());
      next.desc = `${next.alias || "New alias"} maps to ${next.real || "no upstream model"}`;
      return next;
    }));
    setDirty(true);
  };

  const addAlias = () => {
    const id = `new-${Date.now()}`;
    setModels(rows => [...rows, normalizeModelDraft({ id, alias: "", real: "gpt-5.5", context: "200k", status: "ok", desc: "New alias maps to gpt-5.5" })]);
    setEditing(id);
    setFilter("all");
    setDirty(true);
  };

  const removeModel = (id) => {
    setModels(rows => rows.filter(row => row.id !== id));
    if (editing === id) setEditing(null);
    setDirty(true);
  };

  const save = () => {
    if (!canSave) {
      if (invalidIds.size > 0) pushToast("Fix blank or duplicate model aliases first");
      return;
    }
    setSaving(true);
    api.post("/ui/api/models", {
      models: models.map(m => ({ alias: m.alias.trim(), real: m.real.trim(), context: m.context })),
    }).then(res => {
      setModels((res.models || []).map((m, i) => normalizeModelDraft({
        alias: m.alias,
        real: m.real,
        status: m.status,
        context: m.context || "200k",
        desc: `${m.alias} maps to ${m.real}`,
        default: m.default,
        recommended: m.recommended,
      }, i)));
      setDirty(false);
      setEditing(null);
      pushToast(res.message || "Model aliases saved");
    }).catch(e => pushToast(`Save failed · ${e.message || e}`))
      .finally(() => setSaving(false));
  };

  return (
    <section data-screen-label="03 Models" className="col" style={{ gap: 16 }}>
      <SectionHd
        title="Models"
        sub="The model identifiers this proxy advertises to Claude Code, and what they map to upstream."
        actions={
          <>
            {dirty && <Pill tone={invalidIds.size ? "err" : "warn"}>{invalidIds.size ? "Fix rows" : "Unsaved changes"}</Pill>}
            <button className="btn btn-sm btn-ghost" onClick={refresh}><Icon name="refresh" size={11}/>Refresh from upstream</button>
            <button className="btn btn-sm" onClick={addAlias}><Icon name="plus" size={11}/>Add alias</button>
            <button className={`btn btn-sm ${canSave ? "btn-primary" : ""}`} disabled={!canSave} onClick={save}>
              <Icon name="check" size={11}/>{saving ? "Saving" : "Save"}
            </button>
          </>
        }
      />

      <div className="row" style={{ gap: 8, alignItems: "center" }}>
        <div className="seg">
          <button aria-pressed={filter === "all"} onClick={() => setFilter("all")}>All · {models.length}</button>
          <button aria-pressed={filter === "ok"} onClick={() => setFilter("ok")}>Available · {counts.ok || 0}</button>
          <button aria-pressed={filter === "warn"} onClick={() => setFilter("warn")}>Untested · {(counts.warn || 0) + (counts.untested || 0)}</button>
          <button aria-pressed={filter === "unsupported"} onClick={() => setFilter("unsupported")}>Unsupported · {counts.unsupported || 0}</button>
        </div>
        <div className="spacer"></div>
        <div className="row" style={{ gap: 6 }}>
          <Icon name="search" size={12}/>
          <input className="inp inp-sans" placeholder="Filter by alias…" value={query} onChange={e => setQuery(e.target.value)} style={{ width: 220, height: 26 }}/>
        </div>
      </div>

      <Card flush>
        <datalist id="codex-model-options">
          {CODEX_MODEL_OPTIONS.map(model => <option key={model} value={model}/>)}
        </datalist>
        <table className="tbl model-table">
          <thead>
            <tr>
              <th style={{width: "28%"}}>Public alias</th>
              <th style={{width: 16}}></th>
              <th style={{width: "22%"}}>Codex model</th>
              <th style={{width: 118}}>Context</th>
              <th style={{width: 118}}>Status</th>
              <th>Notes</th>
              <th style={{width: 78}}></th>
            </tr>
          </thead>
          <tbody>
            {filtered.map(m => (
              <tr key={m.id} data-editing={editing === m.id ? "true" : undefined}>
                <td className="mono">
                  {editing === m.id ? (
                    <input className={`inp ${invalidIds.has(m.id) ? "invalid" : ""}`} value={m.alias} onChange={e => updateModel(m.id, "alias", e.target.value)} placeholder="claude-sonnet-4-6"/>
                  ) : (
                    <div className="row" style={{gap: 8}}>
                      <span style={{color:"var(--fg)"}}>{m.alias}</span>
                      {m.default && <Pill tone="info">default</Pill>}
                      {m.recommended && <Pill tone="ok">recommended</Pill>}
                    </div>
                  )}
                </td>
                <td style={{color:"var(--fg-3)", textAlign:"center"}}><Icon name="arrowR" size={11}/></td>
                <td className="mono" style={{color: m.real ? "var(--fg)" : "var(--fg-3)"}}>
                  {editing === m.id ? (
                    <input className={`inp ${invalidIds.has(m.id) ? "invalid" : ""}`} list="codex-model-options" value={m.real} onChange={e => updateModel(m.id, "real", e.target.value)} placeholder="gpt-5.5"/>
                  ) : (m.real || "—")}
                </td>
                <td>
                  {editing === m.id ? (
                    <select className="inp model-context-select" value={m.context} onChange={e => updateModel(m.id, "context", e.target.value)}>
                      <option value="200k">200k</option>
                      <option value="1m">1M tokens</option>
                    </select>
                  ) : (m.context === "1m" ? <Pill tone="info">1M tokens</Pill> : <Pill tone="muted">200k</Pill>)}
                </td>
                <td>
                  {m.status === "ok" && <Pill tone="ok">Available</Pill>}
                  {m.status === "warn" && <Pill tone="warn">Untested</Pill>}
                  {m.status === "untested" && <Pill tone="muted">Untested</Pill>}
                  {m.status === "unsupported" && <Pill tone="err">Unsupported</Pill>}
                </td>
                <td className="txt-2" style={{fontSize: 12}}>
                  {invalidIds.has(m.id)
                    ? "Alias and Codex model are required; aliases must be unique."
                    : `${m.alias || "New alias"} maps to ${m.real || "no upstream model"}`}
                </td>
                <td>
                  <div className="model-actions">
                    <button className="btn btn-sm btn-ghost" title={editing === m.id ? "Done editing" : "Edit alias"} onClick={() => setEditing(editing === m.id ? null : m.id)}>
                      <Icon name={editing === m.id ? "check" : "edit"} size={11}/>
                    </button>
                    <button className="btn btn-sm btn-ghost btn-err" title="Delete alias" onClick={() => removeModel(m.id)}>
                      <Icon name="trash" size={11}/>
                    </button>
                  </div>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
        {filtered.length === 0 && (
          <div className="empty-table">No model aliases match this filter.</div>
        )}
      </Card>

      <div style={{ display:"flex", gap: 10, alignItems: "flex-start", padding: "10px 14px", border: "1px solid var(--line)", borderRadius: "var(--radius)", background: "var(--bg-1)", color: "var(--fg-2)", fontSize: 12 }}>
        <Icon name="info" size={13} style={{ flexShrink: 0, marginTop: 2, color: "var(--accent)" }}/>
        <div>
          <strong style={{color: "var(--fg)"}}>How aliasing works.</strong> Claude Code requests Anthropic-style model names like
          <span className="mono" style={{color: "var(--fg)"}}> opus[1m]</span> or
          <span className="mono" style={{color: "var(--fg)"}}> claude-opus-4-7[1m]</span>. This proxy rewrites the
          <span className="mono" style={{color: "var(--fg)"}}> model</span> field at request time and forwards the call to the configured Codex model.
          Capabilities (tool use, streaming, vision) are mapped per-alias — see the configuration screen to override.
        </div>
      </div>
    </section>
  );
};

window.Pill = Pill;
window.SectionHd = SectionHd;
window.Card = Card;
window.TokenField = TokenField;
window.CopyBtn = CopyBtn;
window.Dashboard = Dashboard;
window.Configuration = Configuration;
window.ApiKeys = ApiKeys;
window.Models = Models;
