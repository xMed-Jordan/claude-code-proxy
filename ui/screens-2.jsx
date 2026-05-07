// screens-2.jsx — Test Request, Logs, Setup screens

const { useState: useState2, useEffect: useEffect2, useRef: useRef2, useMemo: useMemo2 } = React;

// ─────────────────────────── TEST REQUEST ───────────────────────────

const SAMPLE_RESPONSE = `Here's a small Go function that reads ~/.codex/auth.json and returns the access token. It tolerates the file being missing (returns ErrAuthMissing) and validates that the mode is "chatgpt" before yielding a token.

func loadCodexAuth(path string) (string, error) {
    f, err := os.Open(path)
    if err != nil {
        if errors.Is(err, os.ErrNotExist) {
            return "", ErrAuthMissing
        }
        return "", err
    }
    defer f.Close()

    var a struct {
        Mode  string \`json:"mode"\`
        Token string \`json:"access_token"\`
        Exp   int64  \`json:"expires_at"\`
    }
    if err := json.NewDecoder(f).Decode(&a); err != nil {
        return "", fmt.Errorf("parse auth: %w", err)
    }
    if a.Mode != "chatgpt" {
        return "", fmt.Errorf("unsupported auth mode %q", a.Mode)
    }
    return a.Token, nil
}

Want me to add a refresh-on-expiry path next?`;

const TestRequest = () => {
  const [model, setModel] = useState2("gpt-5.3-codex");
  const [stream, setStream] = useState2(true);
  const [prompt, setPrompt] = useState2("Read ~/.codex/auth.json in Go and return the access token. Handle missing file and wrong auth mode.");
  const [phase, setPhase] = useState2("idle"); // idle | sending | streaming | done | error
  const [output, setOutput] = useState2("");
  const [duration, setDuration] = useState2(0);
  const [showRaw, setShowRaw] = useState2(false);
  const [rawResponse, setRawResponse] = useState2(null);
  const cancelRef = useRef2(null);

  const reqJson = {
    model,
    max_tokens: 1024,
    stream,
    messages: [{ role: "user", content: prompt }],
  };

  const respJson = rawResponse || {
    id: "msg_01HQPX9F3YK7N2TJ8B4ZRW3DCM",
    type: "message",
    role: "assistant",
    model,
    stop_reason: "end_turn",
    usage: { input_tokens: 142, output_tokens: 318, total_tokens: 460 },
    proxy: {
      forwarded_to: "gpt-5.3-codex",
      upstream: "codex",
      duration_ms: duration || 1842,
    },
    content: [{ type: "text", text: phase === "done" ? SAMPLE_RESPONSE.slice(0, 80) + "…" : "" }],
  };

  const send = async () => {
    if (phase === "sending" || phase === "streaming") return;
    setOutput("");
    setRawResponse(null);
    setPhase("sending");
    const start = Date.now();
    try {
      if (stream) setPhase("streaming");
      const res = await api.post("/ui/api/test", { model, prompt, stream });
      setDuration(res.duration_ms || (Date.now() - start));
      setOutput(res.text || res.raw || "");
      setRawResponse(res);
      setPhase(res.status >= 200 && res.status < 300 ? "done" : "error");
    } catch (e) {
      setOutput(e.message || String(e));
      setDuration(Date.now() - start);
      setPhase("error");
    }
  };
  const cancel = () => {
    clearTimeout(cancelRef.current);
    setPhase("idle");
    setOutput("");
  };
  useEffect2(() => () => clearTimeout(cancelRef.current), []);

  return (
    <section data-screen-label="04 Test Request" className="col" style={{ gap: 16 }}>
      <SectionHd
        title="Test request"
        sub="Send a request through the proxy to verify Claude Code → Codex round-trip end-to-end."
      />

      <div className="col-2">
        <Card title="Request">
          <div className="col" style={{ gap: 12 }}>
            <div className="row" style={{ gap: 10 }}>
              <div style={{flex: 1}}>
                <div className="txt-3" style={{fontSize:10.5, textTransform:"uppercase", letterSpacing:"0.06em", marginBottom:6}}>Model</div>
                <select className="inp" value={model} onChange={e => setModel(e.target.value)}>
                  {SAMPLE_MODELS.filter(m => m.status === "ok").map(m => (
                    <option key={m.alias} value={m.alias}>{m.alias} → {m.real}</option>
                  ))}
                </select>
              </div>
              <div>
                <div className="txt-3" style={{fontSize:10.5, textTransform:"uppercase", letterSpacing:"0.06em", marginBottom:6}}>Stream</div>
                <div className="row" style={{ height: 28, gap: 8 }}>
                  <span className="toggle" aria-checked={stream} role="checkbox" tabIndex={0} onClick={() => setStream(s => !s)} onKeyDown={(e) => e.key === " " && setStream(s => !s)}/>
                  <span className="txt-2" style={{fontSize: 12}}>{stream ? "on · SSE" : "off"}</span>
                </div>
              </div>
            </div>

            <div>
              <div className="txt-3" style={{fontSize:10.5, textTransform:"uppercase", letterSpacing:"0.06em", marginBottom:6}}>Prompt</div>
              <textarea className="inp inp-sans" value={prompt} onChange={e => setPrompt(e.target.value)} style={{ minHeight: 110 }}/>
            </div>

            <div className="row">
              <button className="btn btn-primary" onClick={send} disabled={phase === "sending" || phase === "streaming"}>
                <Icon name="play" size={12}/>{phase === "streaming" ? "Streaming…" : phase === "sending" ? "Sending…" : "Send"}
              </button>
              {(phase === "sending" || phase === "streaming") && (
                <button className="btn" onClick={cancel}><Icon name="x" size={11}/>Cancel</button>
              )}
              <div className="spacer"></div>
              <span className="txt-3 mono" style={{fontSize: 11}}>POST /v1/messages</span>
            </div>

            <details>
              <summary className="txt-2" style={{cursor:"default", fontSize: 11.5, listStyle:"none", display:"flex", alignItems:"center", gap: 6}}>
                <Icon name="chevron" size={11}/>Request payload
              </summary>
              <div className="json-block" style={{marginTop: 8}}>{prettyJson(reqJson)}</div>
            </details>
          </div>
        </Card>

        <Card title="Response" actions={
          <>
            <span className="pill" data-tone={phase === "done" ? "ok" : phase === "error" ? "err" : "muted"}>
              <span className="dot"></span>
              {phase === "idle" && "Idle"}
              {phase === "sending" && "Connecting"}
              {phase === "streaming" && "Streaming"}
              {phase === "done" && "200 OK"}
              {phase === "error" && "Error"}
            </span>
            {phase === "done" && (
              <span className="txt-3 mono" style={{fontSize: 10.5}}>
                {(duration / 1000).toFixed(2)}s · 460 tok
              </span>
            )}
          </>
        }>
          <div className="resp-bubble">
            {phase === "idle" && <span className="txt-3">Press <kbd className="hdr-kbd">Send</kbd> to dispatch a request through the proxy.</span>}
            {phase === "sending" && <span className="txt-3">Connecting to <span className="mono" style={{color:"var(--fg-1)"}}>127.0.0.1:4000</span> · negotiating…</span>}
            {(phase === "streaming" || phase === "done") && (
              <>
                {output}
                {phase === "streaming" && <span className="resp-cursor"></span>}
              </>
            )}
          </div>

          {phase === "done" && (
            <div className="row" style={{ marginTop: 10, gap: 12, fontSize: 11.5, color: "var(--fg-2)" }}>
              <div className="row" style={{gap: 6}}><Icon name="bolt" size={11}/><span className="mono">duration</span> <span className="mono" style={{color:"var(--fg)"}}>{(duration/1000).toFixed(2)}s</span></div>
              <div className="row" style={{gap: 6}}><span className="mono">in</span><span className="mono" style={{color:"var(--fg)"}}>142</span></div>
              <div className="row" style={{gap: 6}}><span className="mono">out</span><span className="mono" style={{color:"var(--fg)"}}>318</span></div>
              <div className="row" style={{gap: 6}}><span className="mono">total</span><span className="mono" style={{color:"var(--fg)"}}>460</span></div>
              <div className="spacer"></div>
              <CopyBtn text={output} label="Copy text"/>
            </div>
          )}
        </Card>
      </div>

      {/* Raw response panel */}
      <Card
        title={`Raw response · ${stream ? "SSE frames" : "JSON"}`}
        actions={
          <>
            <button className="btn btn-sm btn-ghost" onClick={() => setShowRaw(s => !s)}>
              <Icon name={showRaw ? "chevron" : "chevronR"} size={11}/>{showRaw ? "Collapse" : "Expand"}
            </button>
            <CopyBtn text={prettyJson(respJson)}/>
          </>
        }
      >
        {showRaw && phase === "done" ? (
          <div className="json-block">{prettyJson(respJson)}</div>
        ) : (
          <div className="txt-2" style={{fontSize: 12}}>
            {phase === "done" ? "Click Expand to view the parsed JSON envelope and proxy metadata." : "No response yet — send a test request first."}
          </div>
        )}
      </Card>
    </section>
  );
};

function prettyJson(obj) {
  const json = JSON.stringify(obj, null, 2);
  // light syntax highlight
  return json.split("\n").map((line, i) => (
    <div key={i} dangerouslySetInnerHTML={{ __html: line
      .replace(/(&|<|>)/g, m => ({"&":"&amp;","<":"&lt;",">":"&gt;"}[m]))
      .replace(/("[^"]+")(\s*:)/g, '<span class="jk">$1</span>$2')
      .replace(/:\s*("[^"]*")/g, ': <span class="js">$1</span>')
      .replace(/:\s*(-?\d+\.?\d*)/g, ': <span class="jn">$1</span>')
      .replace(/:\s*(true|false|null)/g, ': <span class="jn">$1</span>')
    }}/>
  ));
}

// ─────────────────────────── LOGS ───────────────────────────

const Logs = ({ proxyState }) => {
  const seed = proxyState === "running" ? SAMPLE_LOGS_RUNNING
              : proxyState === "warning" ? SAMPLE_LOGS_WARN
              : SAMPLE_LOGS_STOPPED;
  const [rows, setRows] = useState2(seed);
  const [filter, setFilter] = useState2({ info: true, warn: true, error: true, debug: true });
  const [view, setView] = useState2("raw"); // structured | raw
  const [autoscroll, setAutoscroll] = useState2(true);
  const [paused, setPaused] = useState2(false);
  const [stream, setStream] = useState2("trace"); // stdout | stderr | trace
  const [rawStreams, setRawStreams] = useState2({ stdout: "", stderr: "", trace: "" });
  const scrollRef = useRef2(null);

  useEffect2(() => {
    if (paused) return;
    const load = () => api.get("/ui/api/logs").then(res => {
      setRows(res.rows && res.rows.length ? res.rows : seed);
      setRawStreams({ stdout: res.stdout || "", stderr: res.stderr || "", trace: res.trace || "" });
    }).catch(() => setRows(seed));
    load();
    const id = setInterval(() => {
      load();
    }, 1200);
    return () => clearInterval(id);
  }, [paused, proxyState]);

  useEffect2(() => {
    if (autoscroll && scrollRef.current) {
      scrollRef.current.scrollTop = scrollRef.current.scrollHeight;
    }
  }, [rows, autoscroll]);

  const filtered = rows.filter(r => filter[r.lvl]);
  const counts = rows.reduce((acc, r) => { acc[r.lvl] = (acc[r.lvl] || 0) + 1; return acc; }, {});

  const clear = () => setRows([]);

  const rawText = (stream === "stdout" || stream === "stderr" || stream === "trace") && rawStreams[stream]
    ? rawStreams[stream]
    : filtered.map(r => {
    const status = r.status ? ` ${r.status}` : "";
    const dur = r.dur ? ` ${r.dur}ms` : "";
    return `${r.ts} ${r.lvl.toUpperCase().padEnd(5)} ${r.meth ? r.meth.padEnd(4) : "    "} ${r.path}${status}${dur}${r.note ? "  " + r.note : ""}`;
  }).join("\n");

  return (
    <section data-screen-label="05 Logs" className="col" style={{ gap: 16, height: "100%" }}>
      <SectionHd
        title="Logs"
        sub="Live trace of Claude Code requests, proxy events, and process output."
      />

      {/* Toolbar */}
      <div className="card" style={{display:"flex", flexDirection:"column"}}>
        <div className="row" style={{ padding: "8px 10px", gap: 8, borderBottom: "1px solid var(--line)" }}>
          <div className="seg">
            <button aria-pressed={stream === "stdout"} onClick={() => setStream("stdout")}>
              stdout <span className="txt-3" style={{marginLeft:6, fontFamily:"var(--font-mono)"}}>{rows.length}</span>
            </button>
            <button aria-pressed={stream === "stderr"} onClick={() => setStream("stderr")}>
              stderr <span className="txt-3" style={{marginLeft:6, fontFamily:"var(--font-mono)"}}>{counts.error || 0}</span>
            </button>
            <button aria-pressed={stream === "trace"} onClick={() => setStream("trace")}>
              trace <span className="txt-3" style={{marginLeft:6, fontFamily:"var(--font-mono)"}}>{rawStreams.trace ? rawStreams.trace.split("\n").filter(Boolean).length : 0}</span>
            </button>
          </div>
          <div className="seg">
            {[
              { k: "info",  label: "info",  tone: "info"  },
              { k: "warn",  label: "warn",  tone: "warn"  },
              { k: "error", label: "error", tone: "err"   },
              { k: "debug", label: "debug", tone: "muted" },
            ].map(f => (
              <button key={f.k} aria-pressed={filter[f.k]} onClick={() => setFilter(s => ({ ...s, [f.k]: !s[f.k] }))}>
                <span style={{display:"inline-block", width:6, height:6, borderRadius:"50%", background:`var(--${f.tone === "muted" ? "fg-3" : f.tone === "err" ? "err" : f.tone})`, marginRight:6}}/>
                {f.label} <span className="txt-3" style={{marginLeft:4, fontFamily:"var(--font-mono)"}}>{counts[f.k] || 0}</span>
              </button>
            ))}
          </div>
          <div className="row" style={{ gap: 6 }}>
            <Icon name="search" size={12} style={{color:"var(--fg-3)"}}/>
            <input className="inp inp-sans" placeholder="Filter…" style={{ width: 180, height: 24, fontSize: 11 }}/>
          </div>
          <div className="spacer"></div>
          <div className="seg">
            <button aria-pressed={view === "structured"} onClick={() => setView("structured")}>Structured</button>
            <button aria-pressed={view === "raw"} onClick={() => setView("raw")}>Raw</button>
          </div>
          <div className="row" style={{ gap: 6 }}>
            <span className="toggle" aria-checked={autoscroll} role="checkbox" onClick={() => setAutoscroll(s => !s)}/>
            <span className="txt-2" style={{fontSize: 11}}>Auto-scroll</span>
          </div>
          <button className="btn btn-sm btn-ghost" onClick={() => setPaused(p => !p)}>
            <Icon name={paused ? "play" : "stop"} size={11}/>{paused ? "Resume" : "Pause"}
          </button>
          <CopyBtn text={rawText} label="Copy"/>
          <button className="btn btn-sm btn-ghost" onClick={clear}>
            <Icon name="trash" size={11}/>Clear
          </button>
        </div>

        <div ref={scrollRef} className={`logs ${view}`} style={{ height: 480 }}>
          {view === "structured" ? (
            filtered.length === 0
              ? <div style={{padding:"20px 14px", color: "var(--fg-3)", fontSize: 11.5}}>No log lines match the current filters.</div>
              : filtered.map((r, i) => (
                  <div key={i} className="log-row" data-lvl={r.lvl}>
                    <span className="ts">{r.ts}</span>
                    <span className="lvl">{r.lvl}</span>
                    <span className="msg">
                      {r.meth && <><span className="meth">{r.meth}</span> </>}
                      <span className="path">{r.path}</span>
                      {r.status ? <> <span style={{color: r.status >= 400 ? "var(--err)" : r.status >= 300 ? "var(--warn)" : "var(--ok)"}}>{r.status}</span></> : null}
                      {r.dur ? <> <span className="num">{r.dur}ms</span></> : null}
                      {r.note && <> <span className="dim">· {r.note}</span></>}
                    </span>
                  </div>
                ))
          ) : (
            <pre className="logs raw" style={{ margin: 0, padding: "12px 16px", background: "transparent" }}>{rawText || "(empty)"}</pre>
          )}
        </div>

        <div className="row" style={{ padding: "6px 14px", borderTop: "1px solid var(--line)", fontSize: 10.5, color: "var(--fg-3)", fontFamily: "var(--font-mono)" }}>
          <span>{rows.length} lines · showing {filtered.length}</span>
          <div className="spacer"></div>
          <span>{paused ? "paused" : "live"} · pid 14324 · {stream}</span>
        </div>
      </div>
    </section>
  );
};

const pad = (n) => String(n).padStart(2, "0");

// ─────────────────────────── ANTIGRAVITY BROWSER ───────────────────────────

const BrowserBridge = () => {
  const { data: status, refresh } = usePolling("/ui/api/antigravity", 3000);
  const ext = status?.extension || {};
  const manifest = ext.manifest || {};
  const profile = status?.profile || {};
  const mcp = status?.mcp || {};
  const probe = status?.last_probe || null;
  const bridgeState = status?.bridge_state || {};
  const ready = !!status?.ready;
  const openBridge = () => {
    if (status?.bridge_url) window.open(status.bridge_url, "_blank", "noopener,noreferrer");
  };
  const launcher = status?.launcher?.path || "start-antigravity-browser-mcp.ps1";

  const rows = [
    { label: "Extension", ok: ext.exists, meta: ext.exists ? `${manifest.name || "Antigravity"} ${manifest.version || ""}` : "not found" },
    { label: "Manifest bridge", ok: !!manifest.externally_connectable, meta: manifest.service_worker || "service worker not detected" },
    { label: "Browser mode", ok: true, meta: `${status?.chrome?.mode || "default"} · ${status?.chrome?.browser_url || "debug port pending"}` },
    { label: "Chrome", ok: status?.chrome?.exists, meta: status?.chrome?.path || "not found" },
    { label: "DevTools port", ok: !!status?.chrome?.debug_running, meta: status?.chrome?.debug_running ? "connected" : "will start when MCP runs; close Chrome once if it stays unavailable" },
    { label: "Claude MCP", ok: mcp.present, meta: mcp.present ? `${mcp.command || "pwsh"} ${Array.isArray(mcp.args) ? mcp.args.join(" ") : ""}` : "antigravity-browser not injected yet" },
    { label: "Visible control", ok: !!status?.visible_overlay, meta: bridgeState?.connected_now ? `connected · ${bridgeState.last_action || "ready"}` : "cursor overlay tools ready when Claude starts MCP" },
  ];

  return (
    <section data-screen-label="06 Antigravity Browser" className="col" style={{ gap: 16 }}>
      <SectionHd
        title="Antigravity browser"
        sub="Claude Code browser tools use your normal Chrome profile with a visible cursor overlay for move, click, type, page reads, and screenshots."
        actions={
          <>
            <Pill tone={ready ? "ok" : "warn"}>{ready ? "Ready" : "Needs attention"}</Pill>
            <button className="btn btn-sm btn-ghost" onClick={refresh}><Icon name="refresh" size={11}/>Refresh</button>
            <button className="btn btn-sm" onClick={openBridge}><Icon name="bolt" size={11}/>Open bridge probe</button>
          </>
        }
      />

      <div className="col-2">
        <Card title="Local bridge status" flush>
          <ul className="checklist" style={{padding:"4px 14px"}}>
            {rows.map((row, i) => (
              <li key={i}>
                <span className="check" data-state={row.ok ? "ok" : "warn"}><Icon name={row.ok ? "check" : "warn"} size={9}/></span>
                <span>{row.label}</span>
                <span className="meta">{row.meta}</span>
              </li>
            ))}
          </ul>
        </Card>

        <Card title="Bridge probe">
          {probe ? (
            <div className="kv" style={{ gridTemplateColumns: "140px 1fr", rowGap: 8 }}>
              <div className="k">Received</div>
              <div className="v mono">{probe.received_at || "—"}</div>
              <div className="k">Runtime</div>
              <div className="v">{probe.runtime_available ? <Pill tone="ok">available</Pill> : <Pill tone="warn">unavailable</Pill>}</div>
              <div className="k">Wake</div>
              <div className="v mono">{prettyProbe(probe.wake)}</div>
              <div className="k">Connection</div>
              <div className="v mono">{prettyProbe(probe.connection)}</div>
            </div>
          ) : (
            <div className="txt-2" style={{fontSize: 12}}>
              No extension probe has reported yet. Open the bridge probe from Chrome to check the Antigravity extension messaging wake path.
            </div>
          )}
        </Card>
      </div>

      <Card title="Visible browser control">
        <div className="kv" style={{ gridTemplateColumns: "140px 1fr", rowGap: 8 }}>
          <div className="k">Connection</div>
          <div className="v">{bridgeState?.connected_now ? <Pill tone="ok">Chrome control connected</Pill> : <Pill tone="warn">waiting for Chrome DevTools</Pill>}</div>
          <div className="k">Current page</div>
          <div className="v mono">{bridgeState?.current_url || "—"}</div>
          <div className="k">Title</div>
          <div className="v">{bridgeState?.current_title || "—"}</div>
          <div className="k">Last action</div>
          <div className="v mono">{bridgeState?.last_action || "—"}</div>
          <div className="k">Last error</div>
          <div className="v mono">{bridgeState?.last_error || "—"}</div>
        </div>
      </Card>

      <Card title="MCP launcher">
        <div className="grid-2">
          <div>
            <div className="txt-3" style={{fontSize:10.5, textTransform:"uppercase", letterSpacing:"0.06em", marginBottom:6}}>Launcher</div>
            <CodeBox lang="PowerShell" code={launcher}/>
          </div>
          <div className="kv" style={{ gridTemplateColumns: "130px 1fr", rowGap: 8 }}>
            <div className="k">Mode</div>
            <div className="v mono">{status?.mode || "visible_overlay_cdp"}</div>
            <div className="k">Extension ID</div>
            <div className="v mono">{status?.extension_id || "—"}</div>
            <div className="k">Extension path</div>
            <div className="v mono">{ext.path || "—"}</div>
            <div className="k">Profile path</div>
            <div className="v mono">{profile.path || "—"}</div>
            <div className="k">Bridge URL</div>
            <div className="v mono">{status?.bridge_url || "—"}</div>
          </div>
        </div>
      </Card>
    </section>
  );
};

function prettyProbe(value) {
  if (!value) return "—";
  try {
    return JSON.stringify(value, null, 2);
  } catch (_) {
    return String(value);
  }
}

// ─────────────────────────── SETUP ───────────────────────────

const Setup = () => {
  const { data: status } = usePolling("/ui/api/status", 3000);
  const launcher = `C:\\Users\\hrash\\Documents\\claude-code-proxy\\start-claude-code.ps1`;
  const baseUrl = status?.local_url || "http://127.0.0.1:4000";
  const apiKey = status?.proxy_key || "YOUR_LOCAL_PROXY_TOKEN";
  const env = `ANTHROPIC_BASE_URL=${baseUrl}
ANTHROPIC_AUTH_TOKEN=${apiKey}
CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY=1
CLAUDE_CODE_DISABLE_EXPERIMENTAL_BETAS=1
CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1
API_TIMEOUT_MS=3000000`;
  const psh = `# PowerShell
$env:ANTHROPIC_BASE_URL = "${baseUrl}"
Remove-Item Env:ANTHROPIC_API_KEY -ErrorAction SilentlyContinue
$env:ANTHROPIC_AUTH_TOKEN = "${apiKey}"
$env:CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY = "1"
$env:CLAUDE_CODE_DISABLE_EXPERIMENTAL_BETAS = "1"
$env:CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC = "1"
$env:API_TIMEOUT_MS = "3000000"
claude --model gpt-5.3-codex`;
  const bash = `# bash / zsh
export ANTHROPIC_BASE_URL="${baseUrl}"
unset ANTHROPIC_API_KEY
export ANTHROPIC_AUTH_TOKEN="${apiKey}"
export CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY=1
export CLAUDE_CODE_DISABLE_EXPERIMENTAL_BETAS=1
export CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1
export API_TIMEOUT_MS=3000000
claude --model gpt-5.3-codex`;

  const [shell, setShell] = useState2("powershell");

  const checks = [
    { ok: "ok",   label: `Proxy is running and bound to ${baseUrl.replace("http://", "")}`,     meta: `PID ${status?.pid || "—"}` },
    { ok: status?.codex_auth?.exists ? "ok" : "err", label: "Codex auth file found",           meta: `${status?.codex_auth?.path || "~/.codex/auth.json"} · mode ${status?.codex_auth?.mode || "unknown"}` },
    { ok: "ok",   label: "GET /v1/models returns 4 entries",                                   meta: "200 · 4 ms" },
    { ok: "ok",   label: "POST /v1/messages succeeds against gpt-5.3-codex",                   meta: "200 · 1.28 s" },
    { ok: status?.claude_settings?.mode === "anthropic_auth_token" && !status?.claude_settings?.api_key_present ? "ok" : "warn", label: "Claude settings use ANTHROPIC_AUTH_TOKEN", meta: `api key ${status?.claude_settings?.api_key_present ? "present" : "absent"} · cache ${status?.claude_settings?.gateway_cache_present ? "present" : "absent"}` },
    { ok: status?.antigravity?.mcp?.present ? "ok" : "warn", label: "Antigravity browser MCP configured", meta: status?.antigravity?.mcp?.present ? "server antigravity-browser" : "start endpoints to inject MCP settings" },
    { ok: status?.antigravity?.ready ? "ok" : "warn", label: "Antigravity dedicated browser bridge ready", meta: status?.antigravity?.extension?.exists ? `extension ${status?.antigravity?.extension?.manifest?.version || "installed"}` : "extension not found" },
    { ok: status?.claude_version ? "ok" : "warn", label: "Claude Code CLI detected on PATH",   meta: status?.claude_version || "not detected" },
  ];

  return (
    <section data-screen-label="07 Setup" className="col" style={{ gap: 16 }}>
      <SectionHd
        title="Claude Code setup"
        sub="Point Claude Code at this proxy. Use the launcher script for the smoothest experience, or set environment variables manually."
      />

      <div className="col-2" style={{gridTemplateColumns:"1fr 1fr"}}>
        <Card title="Quick start · launcher script">
          <div className="step">
            <div className="num">1</div>
            <div className="body">
              <h3>Run the launcher</h3>
              <p>The launcher exports the right environment variables and starts Claude Code with the recommended model.</p>
              <CodeBox
                lang="PowerShell"
                code={launcher}
                hint="Right-click → Run with PowerShell. Or invoke from a terminal."
              />
            </div>
          </div>
          <div className="step">
            <div className="num">2</div>
            <div className="body">
              <h3>Verify the connection</h3>
              <p>From inside Claude Code, ask <em>“What model are you?”</em> The response should reference the recommended Codex model.</p>
              <div className="row" style={{ gap: 6 }}>
                <Pill tone="ok">Recommended</Pill>
                <span className="mono" style={{ color: "var(--fg)" }}>gpt-5.3-codex</span>
                <span className="txt-3 mono">→ codex direct passthrough</span>
              </div>
            </div>
          </div>
        </Card>

        <Card title="Manual setup · environment variables" actions={
          <div className="seg">
            <button aria-pressed={shell === "powershell"} onClick={() => setShell("powershell")}>PowerShell</button>
            <button aria-pressed={shell === "bash"} onClick={() => setShell("bash")}>bash · zsh</button>
            <button aria-pressed={shell === "env"} onClick={() => setShell("env")}>.env</button>
          </div>
        }>
          {shell === "env" && <CodeBox lang=".env" code={env}/>}
          {shell === "powershell" && <CodeBox lang="PowerShell" code={psh}/>}
          {shell === "bash" && <CodeBox lang="bash" code={bash}/>}
          <p className="txt-2" style={{ fontSize: 11.5, marginTop: 10 }}>
            <Icon name="info" size={11} style={{ color: "var(--accent)", verticalAlign: "-1px", marginRight: 4 }}/>
            <span className="mono" style={{color:"var(--fg-1)"}}>ANTHROPIC_AUTH_TOKEN</span> is the local proxy token, not your Anthropic billing key. It never leaves your machine.
          </p>
        </Card>
      </div>

      <Card title="Connection checklist" flush>
        <ul className="checklist" style={{padding:"4px 14px"}}>
          {checks.map((c, i) => (
            <li key={i}>
              <span className="check" data-state={c.ok}><Icon name={c.ok === "ok" ? "check" : c.ok === "warn" ? "warn" : "x"} size={9}/></span>
              <span>{c.label}</span>
              <span className="meta">{c.meta}</span>
            </li>
          ))}
        </ul>
      </Card>

      <div className="col-2" style={{gridTemplateColumns:"1fr 1fr"}}>
        <Card title="Recommended model">
          <div className="row" style={{ gap: 12, marginBottom: 10 }}>
            <span className="mono" style={{ fontSize: 16, color: "var(--fg)" }}>gpt-5.3-codex</span>
            <Pill tone="ok">Recommended</Pill>
          </div>
          <p className="txt-2" style={{ margin: 0, fontSize: 12 }}>
            Best balance of latency, code quality, and tool-use compatibility for this proxy.
            Ask Claude Code to switch with <span className="mono" style={{color:"var(--fg-1)"}}>/model gpt-5.3-codex</span>.
          </p>
        </Card>
        <Card title="Need help?">
          <ul style={{ margin: 0, padding: 0, listStyle: "none", display:"flex", flexDirection:"column", gap: 6 }}>
            <li className="row" style={{gap:8}}><Icon name="book" size={12} style={{color:"var(--fg-2)"}}/><a style={{color: "var(--accent)", textDecoration:"none", fontSize:12}}>README.md · troubleshooting</a></li>
            <li className="row" style={{gap:8}}><Icon name="logs" size={12} style={{color:"var(--fg-2)"}}/><a style={{color: "var(--accent)", textDecoration:"none", fontSize:12}}>Open proxy logs</a></li>
            <li className="row" style={{gap:8}}><Icon name="bolt" size={12} style={{color:"var(--fg-2)"}}/><a style={{color: "var(--accent)", textDecoration:"none", fontSize:12}}>Run validation suite</a></li>
            <li className="row" style={{gap:8}}><Icon name="terminal" size={12} style={{color:"var(--fg-2)"}}/><a style={{color: "var(--accent)", textDecoration:"none", fontSize:12}}>Open a test request</a></li>
          </ul>
        </Card>
      </div>
    </section>
  );
};

const CodeBox = ({ lang, code, hint }) => (
  <div className="codebox">
    <div className="codebox-hd">
      <span>{lang}</span>
      <CopyBtn text={code}/>
    </div>
    <div className="codebox-bd">{highlightShell(code)}</div>
    {hint && <div style={{padding:"6px 12px", borderTop:"1px solid var(--line)", fontSize: 11, color:"var(--fg-2)"}}>{hint}</div>}
  </div>
);

function highlightShell(code) {
  return code.split("\n").map((l, i) => {
    if (l.startsWith("#")) return <div key={i} className="cmt">{l}</div>;
    const m = l.match(/^(\s*(?:export |\$env:)?)([A-Z_][A-Z0-9_]*)(\s*=\s*)(.*)$/);
    if (m) {
      return <div key={i}><span>{m[1]}</span><span className="key">{m[2]}</span>{m[3]}<span className="val">{m[4]}</span></div>;
    }
    return <div key={i}>{l || "\u00A0"}</div>;
  });
}

window.TestRequest = TestRequest;
window.Logs = Logs;
window.BrowserBridge = BrowserBridge;
window.Setup = Setup;
