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

const Dashboard = ({ proxyState, onAction, liveStatus }) => {
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
            <div style={{ display: "flex", alignItems: "baseline", gap: 14, marginBottom: 4 }}>
              <span className="mono" style={{ fontSize: 26, fontWeight: 500, letterSpacing: "-0.02em", color: "var(--fg)" }}>
                {liveStatus?.local_url || "http://127.0.0.1:4000"}
              </span>
              <CopyBtn text={liveStatus?.local_url || "http://127.0.0.1:4000"} label="Copy URL"/>
            </div>
            <p className="txt-2" style={{ margin: "2px 0 16px" }}>
              Anthropic-compatible API surface · forwarding to <span className="mono" style={{color:"var(--fg-1)"}}>codex chatgpt</span> backend
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
              <span className="hdr-bind"><Icon name="lock" size={11}/>Local-only · bound to 127.0.0.1</span>
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
              <div className="k">Active aliases</div>
              <div className="v mono"><span style={{color:"var(--fg)"}}>{liveStatus?.models?.length || 0}</span><span className="txt-3"> · opus[1m], sonnet[1m], gpt-5.3-codex</span></div>
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
              {["codex", "openai"].map(opt => (
                <button key={opt} aria-pressed={cfg.UPSTREAM === opt} onClick={() => update("UPSTREAM", opt)}>
                  {opt}
                </button>
              ))}
            </div>
            <div className="hint">
              <Icon name="info" size={11}/>
              {cfg.UPSTREAM === "codex"
                ? "Uses your Codex ChatGPT subscription via ~/.codex/auth.json."
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

// ─────────────────────────── MODELS ───────────────────────────

const Models = () => {
  const [filter, setFilter] = useState("all");
  const [models, setModels] = useState(SAMPLE_MODELS);
  const refresh = () => api.get("/ui/api/models").then(res => setModels((res.models || []).map(m => ({ alias: m.alias, real: m.real, status: m.status, context: m.context || "200k", desc: `${m.alias} maps to ${m.real}`, default: m.default, recommended: m.recommended })))).catch(() => {});
  useEffect(() => { refresh(); }, []);
  const filtered = models.filter(m => filter === "all" || m.status === filter);
  const counts = models.reduce((acc, m) => { acc[m.status] = (acc[m.status] || 0) + 1; return acc; }, {});

  return (
    <section data-screen-label="03 Models" className="col" style={{ gap: 16 }}>
      <SectionHd
        title="Models"
        sub="The model identifiers this proxy advertises to Claude Code, and what they map to upstream."
        actions={
          <>
            <button className="btn btn-sm btn-ghost" onClick={refresh}><Icon name="refresh" size={11}/>Refresh from upstream</button>
            <button className="btn btn-sm"><Icon name="plus" size={11}/>Add alias</button>
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
          <input className="inp inp-sans" placeholder="Filter by alias…" style={{ width: 220, height: 26 }}/>
        </div>
      </div>

      <Card flush>
        <table className="tbl">
          <thead>
            <tr>
              <th style={{width: "32%"}}>Public alias</th>
              <th style={{width: 16}}></th>
              <th style={{width: "20%"}}>Codex model</th>
              <th>Context</th>
              <th>Status</th>
              <th>Notes</th>
              <th style={{width: 40}}></th>
            </tr>
          </thead>
          <tbody>
            {filtered.map(m => (
              <tr key={m.alias}>
                <td className="mono">
                  <div className="row" style={{gap: 8}}>
                    <span style={{color:"var(--fg)"}}>{m.alias}</span>
                    {m.default && <Pill tone="info">default</Pill>}
                    {m.recommended && <Pill tone="ok">recommended</Pill>}
                  </div>
                </td>
                <td style={{color:"var(--fg-3)", textAlign:"center"}}><Icon name="arrowR" size={11}/></td>
                <td className="mono" style={{color: m.real ? "var(--fg)" : "var(--fg-3)"}}>{m.real || "—"}</td>
                <td>{m.context === "1m" ? <Pill tone="info">1M tokens</Pill> : <Pill tone="muted">200k</Pill>}</td>
                <td>
                  {m.status === "ok" && <Pill tone="ok">Available</Pill>}
                  {m.status === "warn" && <Pill tone="warn">Untested</Pill>}
                  {m.status === "untested" && <Pill tone="muted">Untested</Pill>}
                  {m.status === "unsupported" && <Pill tone="err">Unsupported</Pill>}
                </td>
                <td className="txt-2" style={{fontSize: 12}}>{m.desc}</td>
                <td>
                  <button className="btn btn-sm btn-ghost" title="More">
                    <Icon name="chevronR" size={11}/>
                  </button>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
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
window.Models = Models;
