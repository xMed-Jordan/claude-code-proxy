// screens-tools.jsx — Worker C: TestRequest + Updates + BrowserBridge + Setup.
// Exposes: window.TestRequest, window.Updates, window.BrowserBridge, window.Setup.
// Per-screen CSS: screens-tools.css (prefixes: .test- .upd- .browser- .setup-)
(() => {
  const { useState, useEffect, useRef, useMemo, useCallback } = React;
  const {
    Icon, api, useLive,
    Button, IconButton, Card, StatCard, PageHeader, Field, Input, Textarea,
    Select, Switch, Checkbox, SegmentedControl, Badge, Tabs, Modal,
    useToast, EmptyState, Skeleton, Spinner, CopyButton, SecretField,
    KeyValue, CodeBlock,
  } = window;

  /* ─────────────────────────────────────────────────────────────
     TEST REQUEST
     ───────────────────────────────────────────────────────────── */
  function TestRequest({ liveStatus, navigate, pushToast }) {
    // Live models list — no hard-coded list.
    const { data: modelsData } = useLive("/ui/api/models", { interval: 10000 });
    const models = useMemo(() => {
      const list = modelsData && Array.isArray(modelsData.models) ? modelsData.models : [];
      if (list.length === 0 && modelsData && Array.isArray(modelsData)) return modelsData;
      return list;
    }, [modelsData]);

    const firstModel = useMemo(() => {
      if (models.length === 0) return "";
      const m = models[0];
      return (typeof m === "string") ? m : (m.alias || m.id || m.name || "");
    }, [models]);

    const [model, setModel] = useState("");
    const [stream, setStream] = useState(true);
    const [prompt, setPrompt] = useState(
      "Read ~/.codex/auth.json in Go and return the access token. Handle missing file and wrong auth mode."
    );
    const [phase, setPhase] = useState("idle"); // idle|sending|streaming|done|error
    const [output, setOutput] = useState("");
    const [rawResponse, setRawResponse] = useState(null);
    const [metrics, setMetrics] = useState(null); // {inputTokens, outputTokens, durationMs}
    const ctrlRef = useRef(null);

    // Populate model selector once models load.
    useEffect(() => {
      if (model === "" && firstModel) setModel(firstModel);
    }, [firstModel, model]);

    const modelOptions = useMemo(() => {
      return models.map((m) => {
        if (typeof m === "string") return { value: m, label: m };
        const alias = m.alias || m.id || m.name || "";
        const real = m.real || m.upstream_model || "";
        return { value: alias, label: real ? alias + " → " + real : alias };
      });
    }, [models]);

    const reqPayload = useMemo(() => ({
      model: model || "—",
      max_tokens: 1024,
      stream,
      messages: [{ role: "user", content: prompt }],
    }), [model, prompt, stream]);

    const send = useCallback(async () => {
      if (phase === "sending" || phase === "streaming") return;
      setOutput("");
      setRawResponse(null);
      setMetrics(null);
      const ctrl = new AbortController();
      ctrlRef.current = ctrl;
      setPhase(stream ? "streaming" : "sending");
      const start = Date.now();
      try {
        const res = await api.post(
          "/ui/api/test",
          { model, prompt, stream },
          { timeoutMs: 180000, signal: ctrl.signal }
        );
        if (ctrl.signal.aborted) return;
        const durationMs = (res && res.duration_ms) ? res.duration_ms : (Date.now() - start);
        const usage = (res && res.usage) || {};
        setMetrics({
          inputTokens: usage.input_tokens != null ? usage.input_tokens : null,
          outputTokens: usage.output_tokens != null ? usage.output_tokens : null,
          durationMs,
        });
        setOutput((res && (res.text || res.raw || res.content)) || "");
        setRawResponse(res);
        setPhase("done");
      } catch (e) {
        if (e && e.name === "AbortError") return; // cancelled
        setOutput((e && e.message) || String(e));
        setMetrics({ durationMs: Date.now() - start });
        setPhase("error");
      }
    }, [model, prompt, stream, phase]);

    const cancel = useCallback(() => {
      if (ctrlRef.current) ctrlRef.current.abort();
      setPhase("idle");
      setOutput("");
      setMetrics(null);
    }, []);

    useEffect(() => () => { if (ctrlRef.current) ctrlRef.current.abort(); }, []);

    const phaseBadge = useMemo(() => {
      if (phase === "idle") return { tone: "neutral", label: "Idle" };
      if (phase === "sending") return { tone: "info", label: "Connecting" };
      if (phase === "streaming") return { tone: "info", label: "Streaming", pulse: true };
      if (phase === "done") return { tone: "success", label: "200 OK" };
      return { tone: "danger", label: "Error" };
    }, [phase]);

    const inFlight = phase === "sending" || phase === "streaming";

    return (
      <div className="screen">
        <PageHeader
          title="Test Request"
          description="Send a request through the proxy to verify the Claude Code → Codex round-trip."
        />

        <div className="grid-2">
          {/* ── Request card ── */}
          <Card title="Request">
            <div className="test-req-body">
              <Field label="Model">
                {models.length === 0 ? (
                  <Skeleton height={32} />
                ) : (
                  <Select
                    options={modelOptions}
                    value={model}
                    onChange={(e) => setModel(e.target.value)}
                    disabled={inFlight}
                  />
                )}
              </Field>

              <div className="test-switch-row">
                <span className="test-switch-label">Stream (SSE)</span>
                <Switch
                  checked={stream}
                  onChange={setStream}
                  disabled={inFlight}
                />
              </div>

              <Field label="Prompt">
                <Textarea
                  value={prompt}
                  onChange={(e) => setPrompt(e.target.value)}
                  disabled={inFlight}
                  rows={5}
                  placeholder="Enter a prompt…"
                />
              </Field>

              <div className="test-req-actions">
                <Button
                  variant="primary"
                  icon={inFlight ? undefined : "send"}
                  loading={inFlight}
                  onClick={send}
                  disabled={inFlight || !model}
                >
                  {inFlight ? (stream ? "Streaming…" : "Sending…") : "Send"}
                </Button>
                {inFlight && (
                  <Button variant="secondary" icon="x" onClick={cancel}>
                    Cancel
                  </Button>
                )}
                <span className="test-endpoint-hint mono muted">POST /ui/api/test</span>
              </div>

              <CodeBlock
                code={reqPayload}
                mode="json"
                title="Request payload"
                collapsible
                defaultCollapsed
              />
            </div>
          </Card>

          {/* ── Response card ── */}
          <Card
            title="Response"
            actions={
              <Badge tone={phaseBadge.tone} pulse={phaseBadge.pulse}>
                {phaseBadge.label}
              </Badge>
            }
          >
            <div className="test-resp-body">
              {phase === "idle" && (
                <EmptyState
                  icon="send"
                  title="No response yet"
                  description="Press Send to dispatch a request through the proxy."
                  compact
                />
              )}
              {(phase === "sending" || phase === "streaming" || phase === "done" || phase === "error") && (
                <div className="test-output-wrap">
                  <div className={"test-output" + (inFlight ? " test-output-live" : "")}>
                    {phase === "sending" && !output && (
                      <span className="muted">Connecting to proxy…</span>
                    )}
                    {output && <span>{output}</span>}
                    {inFlight && <span className="test-cursor" aria-hidden="true" />}
                  </div>
                </div>
              )}

              {(phase === "done" || phase === "error") && metrics && (
                <div className="test-metrics">
                  {metrics.durationMs != null && (
                    <span className="test-metric">
                      <Icon name="clock" size={12} />
                      <span className="mono tnum">
                        {metrics.durationMs < 1000
                          ? metrics.durationMs + " ms"
                          : (metrics.durationMs / 1000).toFixed(2) + " s"}
                      </span>
                    </span>
                  )}
                  {metrics.inputTokens != null && (
                    <span className="test-metric">
                      <span className="muted">in</span>
                      <span className="mono tnum">{metrics.inputTokens}</span>
                    </span>
                  )}
                  {metrics.outputTokens != null && (
                    <span className="test-metric">
                      <span className="muted">out</span>
                      <span className="mono tnum">{metrics.outputTokens}</span>
                    </span>
                  )}
                  {metrics.inputTokens != null && metrics.outputTokens != null && (
                    <span className="test-metric">
                      <span className="muted">total</span>
                      <span className="mono tnum">{metrics.inputTokens + metrics.outputTokens}</span>
                    </span>
                  )}
                  <span className="test-metrics-spacer" />
                  {output && phase === "done" && <CopyButton text={output} label="Copy text" />}
                </div>
              )}
            </div>
          </Card>
        </div>

        {/* Raw response — full width, only when response exists */}
        {rawResponse ? (
          <CodeBlock
            code={rawResponse}
            mode="json"
            title={stream ? "Raw response · SSE frames" : "Raw response · JSON"}
            collapsible
            defaultCollapsed
          />
        ) : (
          <Card title="Raw response">
            <EmptyState
              icon="info"
              title="No response yet"
              description="Send a test request to see the parsed JSON envelope and proxy metadata."
              compact
            />
          </Card>
        )}
      </div>
    );
  }

  /* ─────────────────────────────────────────────────────────────
     UPDATES
     ───────────────────────────────────────────────────────────── */
  const DEFAULT_VERSION_URL =
    "https://raw.githubusercontent.com/xMed-Jordan/claude-code-proxy/main/VERSION";

  function Updates({ liveStatus, statusError, refreshStatus, onAction, pushToast }) {
    const update = (liveStatus && liveStatus.update) || {};
    const toast = useToast();

    const [form, setForm] = useState({
      auto_update: false,
      full_system_update: true,
      branch: "main",
      version_url: DEFAULT_VERSION_URL,
      repo_dir: "",
      status_file: "",
    });
    const [busy, setBusy] = useState(""); // "save"|"check"|"update"
    const [confirmOpen, setConfirmOpen] = useState(false);

    // Sync form with live status whenever update fields change.
    useEffect(() => {
      setForm({
        auto_update: !!update.auto_update,
        full_system_update: update.full_system_update !== false,
        branch: update.branch || "main",
        version_url: update.version_url || DEFAULT_VERSION_URL,
        repo_dir: update.repo_dir || "",
        status_file: update.status_file || "",
      });
    }, [
      update.auto_update,
      update.full_system_update,
      update.branch,
      update.version_url,
      update.repo_dir,
      update.status_file,
    ]);

    const setField = useCallback((key, value) => {
      setForm((f) => ({ ...f, [key]: value }));
    }, []);

    const save = useCallback(async () => {
      setBusy("save");
      try {
        await api.post("/ui/api/update/settings", form);
        toast.success("Update settings saved");
        refreshStatus();
      } catch (err) {
        toast.error("Save failed — " + ((err && err.message) || err));
      } finally {
        setBusy("");
      }
    }, [form, refreshStatus, toast]);

    const checkNow = useCallback(async () => {
      setBusy("check");
      try {
        await api.post("/ui/api/update/check");
        toast.success("Update check complete");
        refreshStatus();
      } catch (err) {
        toast.error("Check failed — " + ((err && err.message) || err));
      } finally {
        setBusy("");
      }
    }, [refreshStatus, toast]);

    const runUpdate = useCallback(async () => {
      setConfirmOpen(false);
      setBusy("update");
      try {
        await onAction("update");
      } finally {
        setBusy("");
      }
    }, [onAction]);

    const latest = update.latest || {};
    const updateRunning = !!update.running;
    const stateTone = updateRunning ? "info"
      : update.state === "failed" ? "danger"
      : update.update_available ? "warning"
      : update.state === "succeeded" ? "success"
      : "neutral";
    const stateLabel = updateRunning
      ? (update.phase || "running")
      : update.update_available
      ? "update available"
      : (update.state || "idle");

    const modeTone = form.full_system_update ? "accent" : "neutral";

    const kvItems = [
      { label: "Message", value: update.message || "No update has run yet." },
      { label: "Phase", value: update.phase || "idle" },
      { label: "Source", value: update.source || "manual" },
      { label: "Checked", value: latest.checked_at || "not checked" },
      { label: "Started", value: update.started_at || "—" },
      { label: "Finished", value: update.finished_at || "—" },
      { label: "Status file", value: update.status_file || form.status_file || "—" },
      { label: "Public URL", value: update.public_url || (liveStatus && liveStatus.public_url) || "—" },
      ...(latest.error ? [{ label: "Check error", value: latest.error }] : []),
    ];

    return (
      <div className="screen">
        <PageHeader
          title="Updates"
          description="Version checks, automatic update behavior, and live-server updater paths."
          actions={
            <div className="upd-header-actions">
              <Button
                variant="ghost"
                icon="refresh"
                size="sm"
                loading={busy === "check"}
                disabled={!!busy || updateRunning}
                onClick={checkNow}
              >
                {busy === "check" ? "Checking…" : "Check now"}
              </Button>
              <Button
                variant="primary"
                icon="download"
                size="sm"
                loading={busy === "update"}
                disabled={!!busy || updateRunning}
                onClick={() => setConfirmOpen(true)}
              >
                {busy === "update" || updateRunning ? "Updating…" : "Run update"}
              </Button>
            </div>
          }
        />

        {/* 4 stat cards */}
        <div className="grid-stats">
          <StatCard
            label="Current version"
            value={update.current_version || (liveStatus && liveStatus.version) || "—"}
          />
          <StatCard
            label="Latest version"
            value={update.latest_version || "not checked"}
          />
          <StatCard
            label="State"
            value={
              <Badge tone={stateTone} pulse={updateRunning}>
                {stateLabel}
              </Badge>
            }
          />
          <StatCard
            label="Mode"
            value={
              <Badge tone={modeTone}>
                {form.full_system_update ? "full updater" : "app only"}
              </Badge>
            }
          />
        </div>

        <div className="grid-2">
          {/* Options card */}
          <Card
            title="Update Options"
            actions={
              <Button
                variant="primary"
                size="sm"
                icon="check"
                loading={busy === "save"}
                disabled={!!busy || updateRunning}
                onClick={save}
              >
                Save
              </Button>
            }
          >
            <div className="upd-options">
              <div className="upd-option-row">
                <div className="upd-option-copy">
                  <span className="upd-option-label">Automatic updates</span>
                  <span className="upd-option-sub">
                    Checks the VERSION URL and runs an app-scoped update when a newer version appears.
                  </span>
                </div>
                <Switch
                  checked={form.auto_update}
                  onChange={(v) => setField("auto_update", v)}
                  disabled={updateRunning}
                />
              </div>

              <div className="upd-option-row">
                <div className="upd-option-copy">
                  <span className="upd-option-label">Full system update</span>
                  <span className="upd-option-sub">
                    Manual updates may update OS packages, Go, Codex, Claude CLI, rebuild, and restart.
                  </span>
                </div>
                <Switch
                  checked={form.full_system_update}
                  onChange={(v) => setField("full_system_update", v)}
                  disabled={updateRunning}
                />
              </div>

              <Field label="Branch" hint="Git branch used by update-linux.sh.">
                <Input
                  value={form.branch}
                  onChange={(e) => setField("branch", e.target.value)}
                  placeholder="main"
                  disabled={updateRunning}
                />
              </Field>

              <Field label="VERSION URL" hint="Used by Check now and auto update.">
                <div className="upd-url-row">
                  <Input
                    value={form.version_url}
                    onChange={(e) => setField("version_url", e.target.value)}
                    placeholder={DEFAULT_VERSION_URL}
                    mono
                    disabled={updateRunning}
                  />
                  <CopyButton text={form.version_url || DEFAULT_VERSION_URL} />
                </div>
              </Field>

              <Field label="Repository path" hint="Where update-linux.sh is run on the server.">
                <Input
                  value={form.repo_dir}
                  onChange={(e) => setField("repo_dir", e.target.value)}
                  placeholder="/root/claude-code-proxy"
                  mono
                  disabled={updateRunning}
                />
              </Field>

              <Field label="Status file" hint="Progress file read while the service reconnects.">
                <Input
                  value={form.status_file}
                  onChange={(e) => setField("status_file", e.target.value)}
                  placeholder=".proxy.update.json"
                  mono
                  disabled={updateRunning}
                />
              </Field>
            </div>
          </Card>

          {/* Last update status */}
          <Card
            title="Last Update Status"
            actions={
              <CopyButton
                text={JSON.stringify(update, null, 2)}
                label="Copy JSON"
                variant="ghost"
              />
            }
          >
            <KeyValue items={kvItems} />
          </Card>
        </div>

        {/* Confirm Run Update modal */}
        <Modal
          open={confirmOpen}
          onClose={() => setConfirmOpen(false)}
          title="Run update?"
          description="The proxy service will restart during the update. The control panel will reconnect automatically."
          footer={
            <div className="upd-confirm-footer">
              <Button variant="secondary" onClick={() => setConfirmOpen(false)}>
                Cancel
              </Button>
              <Button variant="primary" icon="download" onClick={runUpdate}>
                Run update
              </Button>
            </div>
          }
        />
      </div>
    );
  }

  /* ─────────────────────────────────────────────────────────────
     BROWSER BRIDGE
     ───────────────────────────────────────────────────────────── */
  function BrowserBridge({ navigate, pushToast }) {
    const { data: status, refresh } = useLive("/ui/api/antigravity", { interval: 3000 });
    const toast = useToast();
    const [probing, setProbing] = useState(false);

    const ext = (status && status.extension) || {};
    const manifest = ext.manifest || {};
    const profile = (status && status.profile) || {};
    const mcp = (status && status.mcp) || {};
    const desktopMcp = mcp.desktop || {};
    const probe = (status && status.last_probe) || null;
    const bridgeState = (status && status.bridge_state) || {};
    const ready = !!(status && status.ready);
    const launcher = (status && status.launcher && (status.launcher.command || status.launcher.path)) ||
      "connect-ai-proxy browser-mcp";

    const doProbe = useCallback(async () => {
      setProbing(true);
      try {
        await api.post("/ui/api/antigravity/probe");
        toast.success("Bridge probe sent");
        refresh();
      } catch (err) {
        toast.error("Probe failed — " + ((err && err.message) || err));
      } finally {
        setProbing(false);
      }
    }, [refresh, toast]);

    const openBridge = useCallback(() => {
      if (status && status.bridge_url) {
        window.open(status.bridge_url, "_blank", "noopener,noreferrer");
      }
    }, [status]);

    // Checklist rows — match old screen field for field.
    const rows = [
      {
        ok: ext.exists,
        label: "Extension",
        meta: ext.exists
          ? (manifest.name || "Antigravity") + " " + (manifest.version || "")
          : "not found",
      },
      {
        ok: !!manifest.externally_connectable,
        label: "Manifest bridge",
        meta: manifest.service_worker || "service worker not detected",
      },
      {
        ok: true,
        label: "Browser mode",
        meta: (status && status.chrome && status.chrome.mode || "default") +
          " · " + (status && status.chrome && status.chrome.browser_url || "debug port pending"),
      },
      {
        ok: status && status.chrome && status.chrome.exists,
        label: "Chrome",
        meta: (status && status.chrome && status.chrome.path) || "not found",
      },
      {
        ok: status && status.chrome && !!status.chrome.debug_running,
        label: "DevTools port",
        meta: (status && status.chrome && status.chrome.debug_running)
          ? "connected"
          : (status && status.chrome && status.chrome.default_cdp_forced)
          ? (status.chrome.can_relaunch_default
              ? "forced Default relaunch available"
              : "forced Default mode waiting")
          : "Default profile blocked by Chrome; controlled profile will be used",
      },
      {
        ok: true,
        label: "Chrome windows",
        meta: ((status && status.chrome && status.chrome.process_count) || 0) + " processes · " +
          ((status && status.chrome && status.chrome.visible_count) || 0) + " visible",
      },
      {
        ok: mcp.present,
        label: "Claude Code MCP",
        meta: mcp.present
          ? (mcp.command || "connect-ai-proxy") + " " + (Array.isArray(mcp.args) ? mcp.args.join(" ") : "")
          : "antigravity-browser not injected yet",
      },
      {
        ok: desktopMcp.present,
        label: "Claude Desktop MCP",
        meta: desktopMcp.present
          ? (desktopMcp.command || "connect-ai-proxy") + " " + (Array.isArray(desktopMcp.args) ? desktopMcp.args.join(" ") : "")
          : desktopMcp.exists
          ? (desktopMcp.server_count || 0) + " local servers · antigravity-browser not injected"
          : "desktop config not found",
      },
      {
        ok: !!status && !!status.visible_overlay,
        label: "Visible control",
        meta: bridgeState.connected_now
          ? "connected · " + (bridgeState.last_action || "ready")
          : "cursor overlay tools ready when Claude starts MCP",
      },
    ];

    const bridgeKvItems = [
      {
        label: "Connection",
        value: bridgeState.connected_now
          ? <Badge tone="success">Chrome control connected</Badge>
          : <Badge tone="warning">waiting for controlled Chrome</Badge>,
      },
      { label: "Current page", value: bridgeState.current_url || "—", mono: true },
      { label: "Title", value: bridgeState.current_title || "—" },
      { label: "Last action", value: bridgeState.last_action || "—", mono: true },
      { label: "Screenshot", value: bridgeState.last_screenshot || "—", mono: true },
      { label: "Last error", value: bridgeState.last_error || "—", mono: true },
    ];

    const probeKvItems = probe ? [
      { label: "Received", value: probe.received_at || "—", mono: true },
      {
        label: "Runtime",
        value: probe.runtime_available
          ? <Badge tone="success">available</Badge>
          : <Badge tone="warning">unavailable</Badge>,
      },
      { label: "Wake", value: probe.wake ? JSON.stringify(probe.wake, null, 2) : "—", mono: true },
      { label: "Connection", value: probe.connection ? JSON.stringify(probe.connection, null, 2) : "—", mono: true },
    ] : [];

    const mcpKvItems = [
      { label: "Mode", value: (status && status.mode) || "visible_overlay_cdp", mono: true },
      { label: "Extension ID", value: (status && status.extension_id) || "—", mono: true },
      { label: "Extension path", value: ext.path || "—", mono: true },
      { label: "Profile path", value: profile.path || "—", mono: true },
      { label: "Bridge URL", value: (status && status.bridge_url) || "—", mono: true },
    ];

    return (
      <div className="screen">
        <PageHeader
          title="Antigravity Browser"
          description="Claude Code browser tools use a visible cursor overlay for move, click, type, page reads, and screenshots."
          actions={
            <div className="browser-header-actions">
              <Badge tone={ready ? "success" : "warning"}>
                {ready ? "Ready" : "Needs attention"}
              </Badge>
              <Button variant="ghost" size="sm" icon="refresh" onClick={refresh}>
                Refresh
              </Button>
              <Button
                variant="secondary"
                size="sm"
                icon="bolt"
                loading={probing}
                onClick={doProbe}
              >
                Probe bridge
              </Button>
            </div>
          }
        />

        <div className="grid-2">
          {/* Checklist card */}
          <Card title="Bridge checklist" flush>
            <ul className="browser-checklist">
              {rows.map((row, i) => (
                <li key={i} className="browser-checklist-row">
                  <span className={"browser-check-icon" + (row.ok ? " ok" : " warn")}>
                    <Icon name={row.ok ? "check" : "warning"} size={11} />
                  </span>
                  <span className="browser-check-label">{row.label}</span>
                  <span className="browser-check-meta mono">{row.meta}</span>
                </li>
              ))}
            </ul>
          </Card>

          {/* Bridge probe card */}
          <Card title="Bridge probe">
            {probe ? (
              <KeyValue items={probeKvItems} />
            ) : (
              <EmptyState
                icon="activity"
                title="No probe data yet"
                description="No extension probe has reported yet. Open the bridge probe from Chrome to check the Antigravity extension messaging wake path."
                compact
              />
            )}
          </Card>
        </div>

        {/* Visible browser control */}
        <Card title="Visible browser control">
          <KeyValue items={bridgeKvItems} columns={2} />
        </Card>

        {/* MCP launcher */}
        <Card title="MCP launcher">
          <div className="grid-2">
            <div>
              <CodeBlock
                code={launcher}
                mode="shell"
                title="Launch command"
              />
            </div>
            <KeyValue items={mcpKvItems} />
          </div>
        </Card>
      </div>
    );
  }

  /* ─────────────────────────────────────────────────────────────
     SETUP
     ───────────────────────────────────────────────────────────── */
  function Setup({ liveStatus, onAction, navigate }) {
    const toast = useToast();
    const [shellTab, setShellTab] = useState("powershell");

    // Derive real values from liveStatus (same as old Setup screen).
    const rootUrl = (liveStatus && (
      liveStatus.display_url || liveStatus.public_url || liveStatus.local_url
    )) || "http://127.0.0.1:4000";
    const baseUrl = (liveStatus && liveStatus.anthropic_url) || (rootUrl + "/anthropic");
    const openAIBaseUrl = (liveStatus && liveStatus.openai_url) || (rootUrl + "/openai/v1");
    const apiKey = (liveStatus && liveStatus.proxy_key) || "YOUR_LOCAL_PROXY_TOKEN";
    const launcher = (liveStatus && liveStatus.launcher_commands && liveStatus.launcher_commands.launch_claude) ||
      "connect-ai-proxy launch-claude";

    // Detect OS for default tab.
    useEffect(() => {
      if (liveStatus && liveStatus.goos) {
        setShellTab(liveStatus.goos === "windows" ? "powershell" : "bash");
      }
    }, [liveStatus && liveStatus.goos]);

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

    const envFile = `ANTHROPIC_BASE_URL=${baseUrl}
ANTHROPIC_AUTH_TOKEN=${apiKey}
CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY=1
CLAUDE_CODE_DISABLE_EXPERIMENTAL_BETAS=1
CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1
API_TIMEOUT_MS=3000000`;

    const shellCode = shellTab === "powershell" ? psh : shellTab === "bash" ? bash : envFile;

    // Connection checklist — replicate old checks with liveStatus.
    const antigravity = (liveStatus && liveStatus.antigravity) || {};
    const codexAuth = (liveStatus && liveStatus.codex_auth) || {};
    const claudeSettings = (liveStatus && liveStatus.claude_settings) || {};
    const desktopSupported = !liveStatus || liveStatus.desktop_supported !== false;
    const desktopOk = desktopSupported
      ? !!(antigravity.mcp && antigravity.mcp.desktop && antigravity.mcp.desktop.present)
      : true;

    const checks = [
      {
        ok: !!(liveStatus && liveStatus.proxy_running),
        warn: false,
        label: "Proxy is running and bound to " + baseUrl.replace("http://", ""),
        meta: "PID " + ((liveStatus && liveStatus.pid) || "—"),
      },
      {
        ok: !!codexAuth.exists,
        warn: !codexAuth.exists,
        label: "Codex auth file found",
        meta: (codexAuth.path || "~/.codex/auth.json") + " · mode " + (codexAuth.mode || "unknown"),
      },
      {
        ok: !!(claudeSettings.mode === "anthropic_auth_token" && !claudeSettings.api_key_present),
        warn: true,
        label: "Claude settings use ANTHROPIC_AUTH_TOKEN",
        meta: "api key " + (claudeSettings.api_key_present ? "present" : "absent") +
          " · cache " + (claudeSettings.gateway_cache_present ? "present" : "absent"),
      },
      {
        ok: !!(antigravity.mcp && antigravity.mcp.present && desktopOk),
        warn: true,
        label: "Antigravity browser MCP configured",
        meta: "Code " + (antigravity.mcp && antigravity.mcp.present ? "ready" : "missing") +
          " · Desktop " + (desktopSupported
            ? (antigravity.mcp && antigravity.mcp.desktop && antigravity.mcp.desktop.present ? "ready" : "missing")
            : "not supported on this platform"),
      },
      {
        ok: !!antigravity.ready,
        warn: true,
        label: "Antigravity browser bridge ready",
        meta: antigravity.extension && antigravity.extension.exists
          ? "extension " + (antigravity.extension.manifest && antigravity.extension.manifest.version || "installed")
          : "extension not found",
      },
      {
        ok: !!(liveStatus && liveStatus.claude_version),
        warn: true,
        label: "Claude Code CLI detected on PATH",
        meta: (liveStatus && liveStatus.claude_version) || "not detected",
      },
    ];

    return (
      <div className="screen">
        <PageHeader
          title="Claude Code Setup"
          description="Point Claude Code at this proxy. Use the launcher for the smoothest experience, or set environment variables manually."
        />

        <div className="grid-2">
          {/* Quick start */}
          <Card title="Quick start · launcher script">
            <ol className="setup-steps">
              <li className="setup-step">
                <span className="setup-step-num">1</span>
                <div className="setup-step-body">
                  <strong>Run the launcher</strong>
                  <p className="setup-step-desc">
                    The launcher exports the right environment variables and starts Claude Code with the recommended model.
                  </p>
                  <CodeBlock
                    code={launcher}
                    mode="shell"
                    title="Launch command"
                  />
                </div>
              </li>
              <li className="setup-step">
                <span className="setup-step-num">2</span>
                <div className="setup-step-body">
                  <strong>Verify the connection</strong>
                  <p className="setup-step-desc">
                    From inside Claude Code, ask <em>"What model are you?"</em> The response should reference the recommended Codex model.
                  </p>
                  <div className="setup-model-row">
                    <Badge tone="success">Recommended</Badge>
                    <span className="mono">gpt-5.3-codex</span>
                    <span className="muted subtle">→ codex direct passthrough</span>
                  </div>
                </div>
              </li>
            </ol>
          </Card>

          {/* Manual setup */}
          <Card
            title="Manual setup · environment variables"
            actions={
              <SegmentedControl
                size="sm"
                options={[
                  { value: "powershell", label: "PowerShell" },
                  { value: "bash", label: "bash · zsh" },
                  { value: "env", label: ".env" },
                ]}
                value={shellTab}
                onChange={setShellTab}
              />
            }
          >
            <CodeBlock
              code={shellCode}
              mode="shell"
              title={shellTab === "powershell" ? "PowerShell" : shellTab === "bash" ? "bash / zsh" : ".env"}
            />
            <p className="setup-env-note">
              <Icon name="info" size={12} />
              <span>
                <span className="mono">ANTHROPIC_AUTH_TOKEN</span> is the local proxy token — it never leaves your machine.
                OpenAI-compatible clients use <span className="mono">{openAIBaseUrl}</span>.
              </span>
            </p>
          </Card>
        </div>

        {/* Live connection checklist */}
        <Card title="Connection checklist" flush>
          <ul className="browser-checklist">
            {checks.map((c, i) => {
              const iconOk = c.ok;
              const tone = c.ok ? "ok" : c.warn ? "warn" : "err";
              return (
                <li key={i} className="browser-checklist-row">
                  <span className={"browser-check-icon" + (iconOk ? " ok" : " warn")}>
                    <Icon name={iconOk ? "check" : (c.warn ? "warning" : "x")} size={11} />
                  </span>
                  <span className="browser-check-label">{c.label}</span>
                  <span className="browser-check-meta mono">{c.meta}</span>
                </li>
              );
            })}
          </ul>
        </Card>

        <div className="grid-2">
          {/* Recommended model */}
          <Card title="Recommended model">
            <div className="setup-rec-model">
              <span className="mono setup-rec-model-name">gpt-5.3-codex</span>
              <Badge tone="success">Recommended</Badge>
            </div>
            <p className="setup-rec-model-desc subtle">
              Best balance of latency, code quality, and tool-use compatibility for this proxy.
              Ask Claude Code to switch with <span className="mono">/model gpt-5.3-codex</span>.
            </p>
          </Card>

          {/* Resources */}
          <Card title="Need help?">
            <ul className="setup-resources">
              <li>
                <a
                  className="setup-resource-link"
                  href="/docs"
                  target="_blank"
                  rel="noreferrer"
                >
                  <Icon name="book" size={13} />
                  <span>README · troubleshooting</span>
                  <Icon name="external-link" size={11} />
                </a>
              </li>
              <li>
                <button
                  type="button"
                  className="setup-resource-link"
                  onClick={() => navigate("logs")}
                >
                  <Icon name="logs" size={13} />
                  <span>Open proxy logs</span>
                  <Icon name="arrow-right" size={11} />
                </button>
              </li>
              <li>
                <button
                  type="button"
                  className="setup-resource-link"
                  onClick={() => onAction("validate")}
                >
                  <Icon name="shield" size={13} />
                  <span>Run validation</span>
                  <Icon name="arrow-right" size={11} />
                </button>
              </li>
              <li>
                <button
                  type="button"
                  className="setup-resource-link"
                  onClick={() => navigate("test")}
                >
                  <Icon name="terminal" size={13} />
                  <span>Open a test request</span>
                  <Icon name="arrow-right" size={11} />
                </button>
              </li>
            </ul>
          </Card>
        </div>
      </div>
    );
  }

  /* ─────────────────────────────────────────────────────────────
     Exports
     ───────────────────────────────────────────────────────────── */
  window.TestRequest = TestRequest;
  window.Updates = Updates;
  window.BrowserBridge = BrowserBridge;
  window.Setup = Setup;
})();
