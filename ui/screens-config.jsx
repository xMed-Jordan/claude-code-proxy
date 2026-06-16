// screens-config.jsx — Worker B: Configuration + Models + ApiKeys.
// Owns: window.Configuration, window.Models, window.ApiKeys.
// Phase-2 primitives only; CSS in screens-config.css (.cfg- .models- .keys- prefixes).
// Props: { liveStatus, statusError, statusStale, refreshStatus, onAction, navigate, pushToast }
(() => {
  const { useState, useEffect, useRef, useMemo, useCallback } = React;
  const {
    Icon,
    api, useLive, useToast,
    Button, IconButton, Card, StatCard, PageHeader,
    Field, Input, Textarea, Select, Switch, Checkbox, SegmentedControl,
    Badge, Tabs, Modal, EmptyState, Skeleton, Spinner, CopyButton,
    SecretField, KeyValue, CodeBlock, ErrorBoundary,
  } = window;

  /* ─────────────────────────────────────────────────────────────────
     Helpers
  ───────────────────────────────────────────────────────────────── */
  const cx = (...parts) => parts.filter(Boolean).join(" ");

  const CODEX_MODEL_OPTIONS = [
    "gpt-5.5",
    "gpt-5.4",
    "gpt-5.4-mini",
    "gpt-5.3-codex",
    "gpt-4o",
    "gpt-4.1",
    "gpt-4.1-mini",
    "o3",
    "o4-mini",
  ];

  // Antigravity models served by the agy CLI (from `agy models`). Used as the
  // upstream-model options when a row's "Forwarded to" = Agy. agy accepts these
  // display names verbatim via --model. First entry is the default on switch.
  const AGY_MODEL_OPTIONS = [
    "Gemini 3.1 Pro (High)",
    "Gemini 3.1 Pro (Low)",
    "Gemini 3.5 Flash (High)",
    "Gemini 3.5 Flash (Medium)",
    "Gemini 3.5 Flash (Low)",
    "Claude Sonnet 4.6 (Thinking)",
    "Claude Opus 4.6 (Thinking)",
    "GPT-OSS 120B (Medium)",
  ];

  // Models the Claude Code CLI accepts via --model when a row's "Forwarded to"
  // = Claude: short aliases or full claude-* ids. First entry is the default on
  // switch. The "[1m]" alias suffix (if any) is stripped server-side.
  const CLAUDE_MODEL_OPTIONS = [
    "sonnet",
    "opus",
    "haiku",
    "claude-opus-4-8",
    "claude-sonnet-4-6",
    "claude-haiku-4-5",
  ];

  function modelStatusFor(real) {
    if (!real) return "unsupported";
    return (CODEX_MODEL_OPTIONS.includes(real) || AGY_MODEL_OPTIONS.includes(real) || CLAUDE_MODEL_OPTIONS.includes(real)) ? "available" : "untested";
  }

  // Where each alias forwards to. Codex, Agy and Claude route today (Agy and
  // Claude are text-only: no caller tool calls). Alphabetical.
  const FORWARD_OPTIONS = [
    { value: "agy", label: "Agy" },
    { value: "claude", label: "Claude" },
    { value: "codex", label: "Codex" },
  ];
  const FORWARD_VALUES = FORWARD_OPTIONS.map(o => o.value);
  function normalizeForward(v) {
    const s = (v == null ? "" : String(v)).trim().toLowerCase();
    return FORWARD_VALUES.includes(s) ? s : "codex";
  }

  function normalizeModelDraft(m, idx) {
    const alias = (m.alias || "").trim();
    const real = (m.real || "").trim();
    const status = m.status || modelStatusFor(real);
    return {
      id: m.id || alias || ("model-" + (idx || 0)),
      alias,
      real,
      status,
      context: ((m.context || "200k").toLowerCase() === "1m" ? "1m" : "200k"),
      forward_to: normalizeForward(m.forward_to),
      default: !!m.default,
      recommended: !!m.recommended,
    };
  }

  const STATUS_TONE = {
    available: "success",
    ok: "success",
    untested: "warning",
    warn: "warning",
    unsupported: "danger",
  };
  const STATUS_LABEL = {
    available: "Available",
    ok: "Available",
    untested: "Untested",
    warn: "Untested",
    unsupported: "Unsupported",
  };

  /* ─────────────────────────────────────────────────────────────────
     CONFIGURATION
  ───────────────────────────────────────────────────────────────── */
  function Configuration({ liveStatus, pushToast }) {
    const toast = useToast();
    const notify = useCallback((msg, tone) => {
      if (toast) { tone === "error" ? toast.error(msg) : tone === "success" ? toast.success(msg) : toast.info(msg); }
      else if (pushToast) pushToast(msg, tone || "info");
    }, [toast, pushToast]);

    const DEFAULT_CFG = {
      UPSTREAM: "codex",
      CODEX_BASE_URL: "https://chatgpt.com",
      CODEX_AUTH_FILE: "~/.codex/auth.json",
      PROXY_PUBLIC_URL: "",
      PROXY_PORT: "4000",
      PROXY_API_KEY: "",
      REQUEST_TIMEOUT_MS: "120000",
      LOG_LEVEL: "info",
    };

    const [cfg, setCfg] = useState(DEFAULT_CFG);
    const [secrets, setSecrets] = useState({});
    const [dirty, setDirty] = useState(false);
    const [loading, setLoading] = useState(true);
    const [saving, setSaving] = useState(false);

    // Use liveStatus for codex_auth if already available
    const authMeta = (liveStatus && liveStatus.codex_auth) || null;

    useEffect(() => {
      setLoading(true);
      api.get("/ui/api/config").then(res => {
        setCfg(c => ({ ...c, ...(res.config || {}) }));
        setSecrets(res.secrets || {});
        setDirty(false);
      }).catch(e => notify("Failed to load config: " + (e.message || e), "error"))
        .finally(() => setLoading(false));
    }, []);

    const update = useCallback((k, v) => {
      setCfg(c => ({ ...c, [k]: v }));
      setDirty(true);
    }, []);

    const portValid = /^\d{2,5}$/.test(cfg.PROXY_PORT) && Number(cfg.PROXY_PORT) >= 1024;
    const urlValid = cfg.CODEX_BASE_URL ? /^https?:\/\//.test(cfg.CODEX_BASE_URL) : true;
    const canSave = dirty && portValid && urlValid && !saving;

    const save = useCallback(async () => {
      if (!canSave) return;
      setSaving(true);
      try {
        const res = await api.post("/ui/api/config", {
          config: cfg,
        });
        setDirty(false);
        notify(res.message || "Configuration saved — restart proxy to apply", "success");
      } catch (e) {
        notify("Save failed: " + (e.message || e), "error");
      } finally {
        setSaving(false);
      }
    }, [canSave, cfg, notify]);

    const reset = useCallback(() => {
      setCfg(DEFAULT_CFG);
      setDirty(true);
    }, []);

    const upstreamHint = {
      codex: "Uses your Codex ChatGPT subscription via the auth file below.",
      openai: "Uses a raw OpenAI API key. Codex-only models will be unavailable.",
    };

    const headerActions = (
      <div className="cfg-header-actions">
        {dirty && <Badge tone={!portValid || !urlValid ? "danger" : "warning"}>Unsaved changes</Badge>}
        <Button variant="ghost" size="sm" icon="restart" onClick={reset} disabled={saving}>
          Reset
        </Button>
        <Button
          variant="primary"
          size="sm"
          icon={saving ? undefined : "check"}
          loading={saving}
          disabled={!canSave}
          onClick={save}
        >
          Save
        </Button>
      </div>
    );

    if (loading) {
      return (
        <div className="screen">
          <PageHeader title="Configuration" description="Upstream, server and model alias settings." />
          <Skeleton height={180} />
          <Skeleton height={220} />
          <Skeleton height={160} />
        </div>
      );
    }

    return (
      <div className="screen">
        <PageHeader
          title="Configuration"
          description="Environment variables for the local proxy. Changes are written to .env and applied on restart."
          actions={headerActions}
        />

        {/* Upstream */}
        <Card title="Upstream connection">
          <div className="cfg-form">

            <div className="cfg-row">
              <div className="cfg-label">
                <span className="cfg-label-name">Upstream</span>
                <span className="cfg-label-env mono">UPSTREAM</span>
              </div>
              <div className="cfg-ctl">
                <SegmentedControl
                  options={[
                    { value: "codex", label: "Codex" },
                    { value: "openai", label: "OpenAI" },
                  ]}
                  value={cfg.UPSTREAM}
                  onChange={v => update("UPSTREAM", v)}
                />
                <p className="cfg-hint">
                  {upstreamHint[cfg.UPSTREAM] || upstreamHint.codex}
                </p>
              </div>
            </div>

            <div className="cfg-row">
              <div className="cfg-label">
                <span className="cfg-label-name">Codex base URL</span>
                <span className="cfg-label-env mono">CODEX_BASE_URL</span>
              </div>
              <div className="cfg-ctl">
                <Input
                  value={cfg.CODEX_BASE_URL}
                  onChange={e => update("CODEX_BASE_URL", e.target.value)}
                  invalid={!urlValid}
                  placeholder="https://chatgpt.com"
                />
                {!urlValid && (
                  <p className="cfg-hint cfg-hint-error">Must start with http:// or https://</p>
                )}
              </div>
            </div>

            <div className="cfg-row">
              <div className="cfg-label">
                <span className="cfg-label-name">Codex auth file</span>
                <span className="cfg-label-env mono">CODEX_AUTH_FILE</span>
              </div>
              <div className="cfg-ctl">
                <Input
                  value={cfg.CODEX_AUTH_FILE}
                  onChange={e => update("CODEX_AUTH_FILE", e.target.value)}
                  placeholder="~/.codex/auth.json"
                />
                <p className={cx("cfg-hint", authMeta && (authMeta.exists ? "cfg-hint-success" : "cfg-hint-error"))}>
                  {authMeta
                    ? authMeta.exists
                      ? "File exists — mode: " + (authMeta.mode || "unknown")
                      : "File missing or unavailable"
                    : "Path to the local Codex auth cache."}
                </p>
              </div>
            </div>

          </div>
        </Card>

        {/* Upstream services — enable/disable each backend completely */}
        <Card title="Upstream services">
          <div className="cfg-form">
            {[
              { key: "PROXY_CODEX_ENABLED", name: "Codex (ChatGPT)", hint: "OpenAI Codex via your ChatGPT subscription." },
              { key: "PROXY_CLAUDE_ENABLED", name: "Claude Code", hint: "Anthropic Claude via the local Claude Code CLI." },
              { key: "PROXY_AGY_ENABLED", name: "Antigravity (Gemini)", hint: "Google Antigravity via the local agy CLI." },
            ].map(svc => {
              const on = (cfg[svc.key] || "1") !== "0";
              return (
                <div className="cfg-row" key={svc.key}>
                  <div className="cfg-label">
                    <span className="cfg-label-name">{svc.name}</span>
                    <span className="cfg-label-env mono">{svc.key}</span>
                  </div>
                  <div className="cfg-ctl">
                    <Switch checked={on} onChange={v => update(svc.key, v ? "1" : "0")} label={on ? "Enabled" : "Disabled"} />
                    <p className="cfg-hint">{svc.hint}{on ? "" : " Requests routed here return a clear “disabled” error."}</p>
                  </div>
                </div>
              );
            })}
            <p className="cfg-hint">Disabling a service takes effect after a proxy restart.</p>
          </div>
        </Card>

        {/* Server */}
        <Card title="Server settings">
          <div className="cfg-form">

            <div className="cfg-row">
              <div className="cfg-label">
                <span className="cfg-label-name">Public URL</span>
                <span className="cfg-label-env mono">PROXY_PUBLIC_URL</span>
              </div>
              <div className="cfg-ctl">
                <Input
                  value={cfg.PROXY_PUBLIC_URL || ""}
                  onChange={e => update("PROXY_PUBLIC_URL", e.target.value)}
                  placeholder="https://ai-api1.example.com"
                />
                <p className="cfg-hint">Public HTTPS URL shown to clients. Leave empty for local-only installs.</p>
              </div>
            </div>

            <div className="cfg-row">
              <div className="cfg-label">
                <span className="cfg-label-name">Port</span>
                <span className="cfg-label-env mono">PROXY_PORT</span>
              </div>
              <div className="cfg-ctl cfg-ctl-narrow">
                <Input
                  value={cfg.PROXY_PORT}
                  onChange={e => update("PROXY_PORT", e.target.value)}
                  invalid={!portValid}
                  placeholder="4000"
                />
                {portValid
                  ? <p className="cfg-hint">Binds to <span className="mono">127.0.0.1:{cfg.PROXY_PORT}</span></p>
                  : <p className="cfg-hint cfg-hint-error">Use a numeric port ≥ 1024</p>
                }
              </div>
            </div>

            <div className="cfg-row">
              <div className="cfg-label">
                <span className="cfg-label-name">API key</span>
                <span className="cfg-label-env mono">PROXY_API_KEY</span>
              </div>
              <div className="cfg-ctl">
                <SecretField
                  value={cfg.PROXY_API_KEY}
                  copyValue={secrets.PROXY_API_KEY || cfg.PROXY_API_KEY}
                  placeholder="Not set"
                />
                <p className="cfg-hint">Token Claude Code presents to this proxy. Never sent upstream.</p>
              </div>
            </div>

            <div className="cfg-row">
              <div className="cfg-label">
                <span className="cfg-label-name">Request timeout</span>
                <span className="cfg-label-env mono">REQUEST_TIMEOUT_MS</span>
              </div>
              <div className="cfg-ctl cfg-ctl-narrow">
                <Input
                  value={cfg.REQUEST_TIMEOUT_MS}
                  onChange={e => update("REQUEST_TIMEOUT_MS", e.target.value)}
                  placeholder="120000"
                />
                <p className="cfg-hint">Per-request timeout against the upstream (milliseconds).</p>
              </div>
            </div>

            <div className="cfg-row">
              <div className="cfg-label">
                <span className="cfg-label-name">Log level</span>
                <span className="cfg-label-env mono">LOG_LEVEL</span>
              </div>
              <div className="cfg-ctl">
                <SegmentedControl
                  options={["debug", "info", "warn", "error"]}
                  value={cfg.LOG_LEVEL}
                  onChange={v => update("LOG_LEVEL", v)}
                />
              </div>
            </div>

          </div>
        </Card>

        {/* Model aliases are managed on the dedicated Models page (single source). */}
        <div className="alert" data-tone="info" style={{ marginTop: "var(--sp-4)" }}>
          <Icon name="info" size={14} style={{ flexShrink: 0, marginTop: 1 }} />
          <span>
            <strong>Model aliases moved.</strong> Manage what each public model maps to —
            upstream model, context, forwarded-to backend, default and recommended — on the{" "}
            <a href="/models" className="cfg-inline-link">Models</a> page.
          </span>
        </div>

        <div className="cfg-footer-actions">
          {dirty && <Badge tone={!portValid || !urlValid ? "danger" : "warning"}>Unsaved changes</Badge>}
          <Button variant="ghost" size="sm" icon="restart" onClick={reset} disabled={saving}>
            Reset to defaults
          </Button>
          <Button
            variant="primary"
            size="sm"
            icon={saving ? undefined : "check"}
            loading={saving}
            disabled={!canSave}
            onClick={save}
          >
            Save configuration
          </Button>
        </div>
      </div>
    );
  }

  /* ─────────────────────────────────────────────────────────────────
     MODELS
  ───────────────────────────────────────────────────────────────── */
  function Models({ pushToast }) {
    const toast = useToast();
    const notify = useCallback((msg, tone) => {
      if (toast) { tone === "error" ? toast.error(msg) : tone === "success" ? toast.success(msg) : toast.info(msg); }
      else if (pushToast) pushToast(msg, tone || "info");
    }, [toast, pushToast]);

    const [filter, setFilter] = useState("all");
    const [query, setQuery] = useState("");
    const [models, setModels] = useState([]);
    const [loading, setLoading] = useState(true);
    const [saving, setSaving] = useState(false);
    const [dirty, setDirty] = useState(false);
    const [deleteConfirm, setDeleteConfirm] = useState(null); // id to delete

    const load = useCallback(() => {
      setLoading(true);
      api.get("/ui/api/models").then(res => {
        setModels((res.models || []).map((m, i) => normalizeModelDraft(m, i)));
        setDirty(false);
      }).catch(e => notify("Failed to load models: " + (e.message || e), "error"))
        .finally(() => setLoading(false));
    }, [notify]);

    useEffect(() => { load(); }, []);

    const aliasCounts = useMemo(() =>
      models.reduce((acc, m) => {
        const k = m.alias.toLowerCase();
        if (k) acc[k] = (acc[k] || 0) + 1;
        return acc;
      }, {}),
    [models]);

    const invalidIds = useMemo(() =>
      new Set(models.filter(m => {
        const a = m.alias.trim(), r = m.real.trim();
        return !a || !r || aliasCounts[a.toLowerCase()] > 1 || /\s/.test(a);
      }).map(m => m.id)),
    [models, aliasCounts]);

    const counts = useMemo(() => {
      const c = { all: models.length, available: 0, untested: 0, unsupported: 0 };
      models.forEach(m => {
        const s = m.status;
        if (s === "available" || s === "ok") c.available++;
        else if (s === "untested" || s === "warn") c.untested++;
        else c.unsupported++;
      });
      return c;
    }, [models]);

    const filtered = useMemo(() => {
      const q = query.trim().toLowerCase();
      return models.filter(m => {
        const statusMatch =
          filter === "all" ||
          (filter === "available" && (m.status === "available" || m.status === "ok")) ||
          (filter === "untested" && (m.status === "untested" || m.status === "warn")) ||
          (filter === "unsupported" && m.status === "unsupported");
        const queryMatch = !q || m.alias.toLowerCase().includes(q) || m.real.toLowerCase().includes(q);
        return statusMatch && queryMatch;
      });
    }, [models, filter, query]);

    const updateModel = useCallback((id, key, val) => {
      setModels(rows => rows.map(row => {
        if (row.id !== id) return row;
        const next = { ...row, [key]: val };
        // Switching a row to Agy: if its upstream model isn't an Antigravity
        // model yet, prefill a sensible default so it routes to a real agy model.
        if (key === "forward_to" && val === "agy" && !AGY_MODEL_OPTIONS.includes((next.real || "").trim())) {
          next.real = AGY_MODEL_OPTIONS[0];
        }
        // Switching a row to Claude: if its upstream model isn't a known Claude
        // name, prefill a sensible default so the CLI gets a valid --model.
        if (key === "forward_to" && val === "claude" && !CLAUDE_MODEL_OPTIONS.includes((next.real || "").trim())) {
          next.real = CLAUDE_MODEL_OPTIONS[0];
        }
        if (key === "real" || key === "forward_to") next.status = modelStatusFor((next.real || "").trim());
        return next;
      }));
      setDirty(true);
    }, []);

    // default / recommended are single-choice: turning one on clears the others.
    const toggleFlag = useCallback((id, key) => {
      setModels(rows => {
        const turningOn = !rows.find(r => r.id === id)?.[key];
        return rows.map(row => {
          if (row.id === id) return { ...row, [key]: turningOn };
          return turningOn && row[key] ? { ...row, [key]: false } : row;
        });
      });
      setDirty(true);
    }, []);

    const addAlias = useCallback(() => {
      const id = "new-" + Date.now();
      setModels(rows => [...rows, normalizeModelDraft({ id, alias: "", real: CODEX_MODEL_OPTIONS[0], context: "200k" }, rows.length)]);
      setFilter("all");
      setDirty(true);
    }, []);

    const confirmDelete = useCallback((id) => setDeleteConfirm(id), []);
    const doDelete = useCallback(() => {
      if (!deleteConfirm) return;
      setModels(rows => rows.filter(r => r.id !== deleteConfirm));
      setDeleteConfirm(null);
      setDirty(true);
    }, [deleteConfirm]);

    const canSave = dirty && !saving && invalidIds.size === 0;

    const save = useCallback(async () => {
      if (!canSave) {
        if (invalidIds.size > 0) notify("Fix blank or duplicate aliases before saving", "error");
        return;
      }
      setSaving(true);
      try {
        const res = await api.post("/ui/api/models", {
          models: models.map(m => ({ alias: m.alias.trim(), real: m.real.trim(), context: m.context, forward_to: m.forward_to, default: !!m.default, recommended: !!m.recommended })),
        });
        setModels((res.models || []).map((m, i) => normalizeModelDraft(m, i)));
        setDirty(false);
        notify(res.message || "Model aliases saved", "success");
      } catch (e) {
        notify("Save failed: " + (e.message || e), "error");
      } finally {
        setSaving(false);
      }
    }, [canSave, models, invalidIds, notify]);

    // ── Import / Export ──────────────────────────────────────────────
    const fileInputRef = useRef(null);

    const exportModels = useCallback(() => {
      const payload = {
        version: 1,
        exported_at: new Date().toISOString(),
        models: models.map(m => ({
          alias: m.alias.trim(),
          real: m.real.trim(),
          context: m.context,
          forward_to: m.forward_to,
          default: !!m.default,
          recommended: !!m.recommended,
        })),
      };
      const blob = new Blob([JSON.stringify(payload, null, 2)], { type: "application/json" });
      const url = URL.createObjectURL(blob);
      const a = document.createElement("a");
      a.href = url;
      a.download = "connect-ai-proxy-models.json";
      document.body.appendChild(a);
      a.click();
      document.body.removeChild(a);
      URL.revokeObjectURL(url);
      notify("Exported " + payload.models.length + " model" + (payload.models.length === 1 ? "" : "s"), "success");
    }, [models, notify]);

    const importModels = useCallback((file) => {
      if (!file) return;
      const reader = new FileReader();
      reader.onload = () => {
        try {
          const parsed = JSON.parse(String(reader.result));
          const list = Array.isArray(parsed) ? parsed : (parsed && Array.isArray(parsed.models) ? parsed.models : null);
          if (!list) throw new Error("expected a JSON array or an object with a \"models\" array");
          const rows = list.map((m, i) => normalizeModelDraft({
            id: "imp-" + Date.now() + "-" + i,
            alias: m.alias || m.from || "",
            real: m.real || m.to || "",
            context: m.context || "200k",
            forward_to: m.forward_to,
            default: !!m.default,
            recommended: !!m.recommended,
          }, i));
          if (rows.length === 0) throw new Error("no models found in file");
          setModels(rows);
          setFilter("all");
          setQuery("");
          setDirty(true);
          notify("Imported " + rows.length + " model" + (rows.length === 1 ? "" : "s") + " — review and Save", "success");
        } catch (e) {
          notify("Import failed: " + (e.message || e), "error");
        }
      };
      reader.onerror = () => notify("Could not read file", "error");
      reader.readAsText(file);
    }, [notify]);

    const onImportFile = useCallback((e) => {
      const file = e.target.files && e.target.files[0];
      importModels(file);
      e.target.value = ""; // allow re-importing the same file
    }, [importModels]);

    const filterTabs = [
      { id: "all", label: "All", badge: counts.all },
      { id: "available", label: "Available", badge: counts.available },
      { id: "untested", label: "Untested", badge: counts.untested },
      { id: "unsupported", label: "Unsupported", badge: counts.unsupported },
    ];

    return (
      <div className="screen">
        <PageHeader
          title="Models"
          description="Identifiers this proxy advertises to Claude Code, and what they map to upstream."
          actions={
            <div className="models-header-actions">
              {dirty && (
                <Badge tone={invalidIds.size ? "danger" : "warning"}>
                  {invalidIds.size ? "Fix " + invalidIds.size + " row" + (invalidIds.size > 1 ? "s" : "") : "Unsaved"}
                </Badge>
              )}
              <input
                ref={fileInputRef}
                type="file"
                accept="application/json,.json"
                style={{ display: "none" }}
                onChange={onImportFile}
              />
              <Button size="sm" variant="ghost" icon="refresh" onClick={load} disabled={loading}>
                Refresh
              </Button>
              <Button size="sm" variant="ghost" icon="upload" onClick={() => fileInputRef.current && fileInputRef.current.click()}>
                Import
              </Button>
              <Button size="sm" variant="ghost" icon="download" onClick={exportModels} disabled={!models.length}>
                Export
              </Button>
              <Button size="sm" variant="secondary" icon="plus" onClick={addAlias}>
                Add alias
              </Button>
              <Button
                size="sm"
                variant="primary"
                icon={saving ? undefined : "check"}
                loading={saving}
                disabled={!canSave}
                onClick={save}
              >
                Save
              </Button>
            </div>
          }
        />

        {/* Toolbar */}
        <div className="models-toolbar">
          <Tabs tabs={filterTabs} active={filter} onChange={setFilter} variant="pill" />
          <div className="models-search">
            <Icon name="search" size={13} />
            <input
              className="input models-search-input"
              placeholder="Search aliases…"
              value={query}
              onChange={e => setQuery(e.target.value)}
            />
            {query && (
              <IconButton icon="x" label="Clear search" size={12} onClick={() => setQuery("")} />
            )}
          </div>
        </div>

        {/* Table */}
        {loading ? (
          <Skeleton height={220} />
        ) : (
          <Card flush>
            <datalist id="models-codex-options">
              {CODEX_MODEL_OPTIONS.map(m => <option key={m} value={m} />)}
            </datalist>
            <datalist id="models-agy-options">
              {AGY_MODEL_OPTIONS.map(m => <option key={m} value={m} />)}
            </datalist>
            <datalist id="models-claude-options">
              {CLAUDE_MODEL_OPTIONS.map(m => <option key={m} value={m} />)}
            </datalist>
            {filtered.length === 0 ? (
              <EmptyState
                icon="models"
                title={query ? "No matches" : "No models in this filter"}
                description={query ? 'No aliases match "' + query + '".' : "Try a different filter."}
                compact
              />
            ) : (
              <div className="table-wrap">
                <table className="table">
                  <thead>
                    <tr>
                      <th>Public alias</th>
                      <th></th>
                      <th>Upstream model</th>
                      <th>Forwarded to</th>
                      <th>Context</th>
                      <th>Status</th>
                      <th className="models-notes-col">Notes</th>
                      <th style={{ width: 72 }}></th>
                    </tr>
                  </thead>
                  <tbody>
                    {filtered.map(m => {
                      const invalid = invalidIds.has(m.id);
                      return (
                        <tr key={m.id} className={invalid ? "tr-danger" : ""}>
                          <td>
                            <div className="models-alias-cell">
                              <input
                                className={cx("input models-inline-input mono", invalid && "invalid")}
                                value={m.alias}
                                onChange={e => updateModel(m.id, "alias", e.target.value)}
                                placeholder="claude-sonnet-4-5"
                              />
                              <div className="models-flags">
                                <button
                                  type="button"
                                  className={cx("models-flag", m.default && "on")}
                                  data-flag="default"
                                  onClick={() => toggleFlag(m.id, "default")}
                                  title="Mark this alias as the default choice"
                                >
                                  {m.default ? "★ default" : "☆ default"}
                                </button>
                                <button
                                  type="button"
                                  className={cx("models-flag", m.recommended && "on")}
                                  data-flag="recommended"
                                  onClick={() => toggleFlag(m.id, "recommended")}
                                  title="Mark this alias as the recommended choice"
                                >
                                  {m.recommended ? "★ recommended" : "☆ recommended"}
                                </button>
                              </div>
                            </div>
                          </td>
                          <td className="models-arrow subtle">→</td>
                          <td>
                            <input
                              className={cx("input models-inline-input mono", invalid && !m.real.trim() && "invalid")}
                              list={m.forward_to === "agy" ? "models-agy-options" : m.forward_to === "claude" ? "models-claude-options" : "models-codex-options"}
                              value={m.real}
                              onChange={e => updateModel(m.id, "real", e.target.value)}
                              placeholder={m.forward_to === "agy" ? "Gemini 3.1 Pro (High)" : "gpt-5.5"}
                            />
                          </td>
                          <td>
                            <Select
                              value={m.forward_to}
                              onChange={e => updateModel(m.id, "forward_to", e.target.value)}
                              options={FORWARD_OPTIONS}
                              className="models-forward-select"
                            />
                          </td>
                          <td>
                            <Select
                              value={m.context}
                              onChange={e => updateModel(m.id, "context", e.target.value)}
                              options={[
                                { value: "200k", label: "200k" },
                                { value: "1m", label: "1M" },
                              ]}
                              className="models-context-select"
                            />
                          </td>
                          <td>
                            <Badge tone={STATUS_TONE[m.status] || "neutral"}>
                              {STATUS_LABEL[m.status] || m.status}
                            </Badge>
                          </td>
                          <td className="models-notes-col muted" style={{ fontSize: "var(--fs-xs)" }}>
                            {invalid
                              ? "Alias and model required; aliases must be unique and have no spaces."
                              : m.alias
                                ? (m.alias + " → " + (m.real || "no model"))
                                : "New alias — fill in both fields."}
                          </td>
                          <td>
                            <div className="models-row-actions">
                              <IconButton
                                icon="trash"
                                label="Delete alias"
                                size={13}
                                onClick={() => confirmDelete(m.id)}
                              />
                            </div>
                          </td>
                        </tr>
                      );
                    })}
                  </tbody>
                </table>
              </div>
            )}
          </Card>
        )}

        {/* Info callout */}
        <div className="alert models-info-callout" data-tone="info">
          <Icon name="info" size={14} style={{ flexShrink: 0, marginTop: 1 }} />
          <span>
            <strong>How aliasing works.</strong> Claude Code requests Anthropic-style model names like{" "}
            <span className="mono">claude-sonnet-4-5</span>. This proxy rewrites the{" "}
            <span className="mono">model</span> field and forwards it to the upstream model. Context
            window size is advertised per alias. <strong>Forwarded to</strong> selects the backend:{" "}
            <span className="mono">Codex</span> and <span className="mono">Agy</span> route today —{" "}
            <span className="mono">Agy</span> (Google Antigravity) is text-only, so it serves plain
            completions but can't drive Claude Code's tool loop. <span className="mono">Claude</span> is
            reserved and takes effect once its backend is enabled.
          </span>
        </div>

        {/* Delete confirm modal */}
        <Modal
          open={!!deleteConfirm}
          onClose={() => setDeleteConfirm(null)}
          title="Delete alias"
          description={"Remove this model alias? This cannot be undone without saving a new alias."}
          footer={
            <div className="models-modal-footer">
              <Button variant="ghost" onClick={() => setDeleteConfirm(null)}>Cancel</Button>
              <Button variant="danger" icon="trash" onClick={doDelete}>Delete</Button>
            </div>
          }
        />
      </div>
    );
  }

  /* ─────────────────────────────────────────────────────────────────
     API KEYS
  ───────────────────────────────────────────────────────────────── */
  function ApiKeys({ liveStatus, pushToast }) {
    const toast = useToast();
    const notify = useCallback((msg, tone) => {
      if (toast) { tone === "error" ? toast.error(msg) : tone === "success" ? toast.success(msg) : toast.info(msg); }
      else if (pushToast) pushToast(msg, tone || "info");
    }, [toast, pushToast]);

    const [keysData, setKeysData] = useState({ providers: [], clients: [], defaults: {} });
    const [loading, setLoading] = useState(true);

    // Provider form
    const [providerForm, setProviderForm] = useState({
      id: "",
      provider: "openai",
      label: "OpenAI",
      base_url: "",
      api_key: "",
    });
    const [providerSaving, setProviderSaving] = useState(false);
    const isRenewing = !!providerForm.id;

    // Client form
    const [clientForm, setClientForm] = useState({
      label: "Claude Code",
      schema: "both",
      provider: "default",
      provider_key_id: "",
    });
    const [clientSaving, setClientSaving] = useState(false);

    // Created key modal
    const [createdKey, setCreatedKey] = useState("");

    const rootUrl = (liveStatus && (liveStatus.display_url || liveStatus.public_url || liveStatus.local_url)) || "http://127.0.0.1:4000";
    const defaults = keysData.defaults || {};
    const anthropicUrl = (liveStatus && liveStatus.anthropic_url) || defaults.anthropic_base || (rootUrl + "/anthropic");
    const openaiUrl = (liveStatus && liveStatus.openai_url) || defaults.openai_local_url || (rootUrl + "/openai/v1");

    const load = useCallback(() => {
      setLoading(true);
      api.get("/ui/api/keys")
        .then(res => setKeysData(res || { providers: [], clients: [], defaults: {} }))
        .catch(e => notify("Failed to load keys: " + (e.message || e), "error"))
        .finally(() => setLoading(false));
    }, [notify]);

    useEffect(() => { load(); }, []);

    const providerBaseDefault =
      providerForm.provider === "openai"
        ? (defaults.openai_base_url || "https://api.openai.com/v1")
        : (defaults.gemini_base_url || "https://generativelanguage.googleapis.com/v1beta/openai");

    const selectedProviderKeys = (keysData.providers || []).filter(
      p => p.provider === clientForm.provider && p.enabled
    );

    const saveProvider = useCallback(async () => {
      setProviderSaving(true);
      try {
        const res = await api.post("/ui/api/keys/provider", {
          ...providerForm,
          base_url: providerForm.base_url || providerBaseDefault,
        });
        setKeysData(d => ({
          ...d,
          providers: res.providers || d.providers,
          clients: res.clients || d.clients,
        }));
        setProviderForm(f => ({ ...f, id: "", api_key: "" }));
        notify(res.message || "Provider key saved", "success");
      } catch (e) {
        notify("Save failed: " + (e.message || e), "error");
      } finally {
        setProviderSaving(false);
      }
    }, [providerForm, providerBaseDefault, notify]);

    const renewProvider = useCallback((p) => {
      setProviderForm({
        id: p.id,
        provider: p.provider,
        label: p.label,
        base_url: p.base_url || "",
        api_key: "",
      });
      notify("Renewing " + p.label + " — paste the new API key", "info");
    }, [notify]);

    const cancelRenew = useCallback(() => {
      setProviderForm({ id: "", provider: "openai", label: "OpenAI", base_url: "", api_key: "" });
    }, []);

    const createClient = useCallback(async () => {
      setClientSaving(true);
      try {
        const res = await api.post("/ui/api/keys/client", clientForm);
        setKeysData(d => ({
          ...d,
          providers: res.providers || d.providers,
          clients: res.clients || d.clients,
        }));
        setCreatedKey(res.api_key || "");
        notify(res.message || "Client key created", "success");
      } catch (e) {
        notify("Create failed: " + (e.message || e), "error");
      } finally {
        setClientSaving(false);
      }
    }, [clientForm, notify]);

    const toggleKey = useCallback(async (kind, row) => {
      try {
        const res = await api.post("/ui/api/keys/toggle", { kind, id: row.id, enabled: !row.enabled });
        setKeysData(d => ({
          ...d,
          providers: res.providers || d.providers,
          clients: res.clients || d.clients,
        }));
      } catch (e) {
        notify("Update failed: " + (e.message || e), "error");
      }
    }, [notify]);

    const providerCount = (keysData.providers || []).length;
    const clientCount = (keysData.clients || []).length;

    return (
      <div className="screen">
        <PageHeader
          title="API Keys"
          description="Provider keys for upstream APIs, and local client keys for Claude Code or OpenAI-compatible clients."
          actions={
            <Button size="sm" variant="ghost" icon="refresh" onClick={load} disabled={loading}>
              Refresh
            </Button>
          }
        />

        {/* Stat cards */}
        <div className="grid-stats">
          <StatCard
            label="Anthropic base URL"
            value={<span className="mono keys-url-value truncate">{anthropicUrl}</span>}
            sub={<CopyButton text={anthropicUrl} label="Copy URL" size="sm" />}
          />
          <StatCard
            label="OpenAI base URL"
            value={<span className="mono keys-url-value truncate">{openaiUrl}</span>}
            sub={<CopyButton text={openaiUrl} label="Copy URL" size="sm" />}
          />
          <StatCard
            label="Provider keys"
            value={loading ? <Skeleton width={40} height={26} /> : String(providerCount)}
            sub="OpenAI · Google AI Studio"
          />
          <StatCard
            label="Client keys"
            value={loading ? <Skeleton width={40} height={26} /> : String(clientCount)}
            sub="Local proxy authentication"
          />
        </div>

        {/* Add/create forms */}
        <div className="grid-2">

          {/* Add provider key */}
          <Card title={isRenewing ? "Renew provider key" : "Add provider key"}>
            {isRenewing && (
              <div className="alert keys-renew-banner" data-tone="warning">
                <Icon name="warning" size={14} />
                <span>Renewing <strong>{providerForm.label}</strong> — paste the new API key below.</span>
                <Button size="sm" variant="ghost" onClick={cancelRenew}>Cancel</Button>
              </div>
            )}
            <div className="keys-form">
              {!isRenewing && (
                <Field label="Provider">
                  <SegmentedControl
                    options={[
                      { value: "openai", label: "OpenAI" },
                      { value: "gemini", label: "Google AI Studio" },
                    ]}
                    value={providerForm.provider}
                    onChange={v => setProviderForm(f => ({ ...f, id: "", provider: v, label: v === "openai" ? "OpenAI" : "Google AI Studio", base_url: "" }))}
                  />
                </Field>
              )}
              <Field label="Label">
                <Input
                  value={providerForm.label}
                  onChange={e => setProviderForm(f => ({ ...f, label: e.target.value }))}
                  placeholder="My OpenAI key"
                />
              </Field>
              <Field label="Base URL" hint={"Default: " + providerBaseDefault}>
                <Input
                  className="mono"
                  value={providerForm.base_url}
                  onChange={e => setProviderForm(f => ({ ...f, base_url: e.target.value }))}
                  placeholder={providerBaseDefault}
                />
              </Field>
              <Field label="API key">
                <Input
                  type="password"
                  className="mono"
                  value={providerForm.api_key}
                  onChange={e => setProviderForm(f => ({ ...f, api_key: e.target.value }))}
                  placeholder="sk-…"
                  autoComplete="off"
                />
              </Field>
              <Button
                variant="primary"
                icon="check"
                loading={providerSaving}
                disabled={!providerForm.api_key || providerSaving}
                onClick={saveProvider}
              >
                {isRenewing ? "Renew provider key" : "Save provider key"}
              </Button>
            </div>
          </Card>

          {/* Create client key */}
          <Card title="Create client key">
            <div className="keys-form">
              <Field label="Label">
                <Input
                  value={clientForm.label}
                  onChange={e => setClientForm(f => ({ ...f, label: e.target.value }))}
                  placeholder="Claude Code"
                />
              </Field>
              <Field label="API schema">
                <Select
                  value={clientForm.schema}
                  onChange={e => setClientForm(f => ({ ...f, schema: e.target.value }))}
                  options={[
                    { value: "both", label: "Both schemas" },
                    { value: "anthropic", label: "Anthropic only" },
                    { value: "openai", label: "OpenAI-compatible only" },
                  ]}
                />
              </Field>
              <Field label="Route to">
                <Select
                  value={clientForm.provider}
                  onChange={e => setClientForm(f => ({ ...f, provider: e.target.value, provider_key_id: "" }))}
                  options={[
                    { value: "default", label: "Default proxy route" },
                    { value: "openai", label: "OpenAI provider key" },
                    { value: "gemini", label: "Google AI Studio key" },
                  ]}
                />
              </Field>
              {clientForm.provider !== "default" && (
                <Field label="Specific key">
                  <Select
                    value={clientForm.provider_key_id}
                    onChange={e => setClientForm(f => ({ ...f, provider_key_id: e.target.value }))}
                    options={[
                      { value: "", label: "Use first enabled " + clientForm.provider + " key" },
                      ...selectedProviderKeys.map(p => ({ value: p.id, label: p.label + " · " + (p.key_preview || "") })),
                    ]}
                  />
                </Field>
              )}
              <Button
                variant="primary"
                icon="plus"
                loading={clientSaving}
                disabled={clientSaving}
                onClick={createClient}
              >
                Create client key
              </Button>
            </div>
          </Card>

        </div>

        {/* Provider keys table */}
        <Card title="Provider keys" flush>
          {loading ? (
            <div style={{ padding: "var(--sp-5)" }}><Skeleton height={80} /></div>
          ) : (keysData.providers || []).length === 0 ? (
            <EmptyState
              icon="key"
              title="No provider keys"
              description="Add a provider key above to route client requests to OpenAI or Google AI Studio."
              compact
            />
          ) : (
            <div className="table-wrap">
              <table className="table">
                <thead>
                  <tr>
                    <th>Provider</th>
                    <th>Label</th>
                    <th>Base URL</th>
                    <th>Key</th>
                    <th>Status</th>
                    <th>Enabled</th>
                    <th style={{ width: 72 }}></th>
                  </tr>
                </thead>
                <tbody>
                  {(keysData.providers || []).map(p => (
                    <tr key={p.id}>
                      <td><Badge tone="neutral">{p.provider}</Badge></td>
                      <td>{p.label}</td>
                      <td className="mono truncate" style={{ maxWidth: 200 }}>{p.base_url}</td>
                      <td className="mono">{p.key_preview}</td>
                      <td>
                        <Badge tone={p.enabled ? "success" : "neutral"}>
                          {p.enabled ? "Enabled" : "Disabled"}
                        </Badge>
                      </td>
                      <td>
                        <Switch
                          checked={!!p.enabled}
                          onChange={() => toggleKey("provider", p)}
                        />
                      </td>
                      <td>
                        <Button size="sm" variant="ghost" onClick={() => renewProvider(p)}>
                          Renew
                        </Button>
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}
        </Card>

        {/* Client keys table */}
        <Card title="Client keys" flush>
          {loading ? (
            <div style={{ padding: "var(--sp-5)" }}><Skeleton height={80} /></div>
          ) : (keysData.clients || []).length === 0 ? (
            <EmptyState
              icon="lock"
              title="No client keys"
              description="Create a client key above to authenticate Claude Code or other OpenAI-compatible clients."
              compact
            />
          ) : (
            <div className="table-wrap">
              <table className="table">
                <thead>
                  <tr>
                    <th>Label</th>
                    <th>Key</th>
                    <th>Schema</th>
                    <th>Route</th>
                    <th>Status</th>
                    <th>Enabled</th>
                  </tr>
                </thead>
                <tbody>
                  {(keysData.clients || []).map(k => (
                    <tr key={k.id}>
                      <td>{k.label}</td>
                      <td className="mono">{k.key_preview}</td>
                      <td><Badge tone="neutral">{k.schema || "both"}</Badge></td>
                      <td className="mono muted">
                        {k.provider
                          ? k.provider + (k.provider_label ? " · " + k.provider_label : "")
                          : "default"}
                      </td>
                      <td>
                        <Badge tone={k.enabled ? "success" : "neutral"}>
                          {k.enabled ? "Enabled" : "Disabled"}
                        </Badge>
                      </td>
                      <td>
                        <Switch
                          checked={!!k.enabled}
                          onChange={() => toggleKey("client", k)}
                        />
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}
        </Card>

        {/* Created key modal */}
        <Modal
          open={!!createdKey}
          onClose={() => setCreatedKey("")}
          title="Save your new client key"
          description="This key will not be shown again. Copy it now and store it somewhere safe."
          width={520}
          footer={
            <div className="keys-modal-footer">
              <CopyButton text={createdKey} label="Copy key" variant="primary" size="md" />
              <Button variant="secondary" onClick={() => setCreatedKey("")}>Done</Button>
            </div>
          }
        >
          <SecretField value={createdKey} copyValue={createdKey} />
        </Modal>
      </div>
    );
  }

  /* ─────────────────────────────────────────────────────────────────
     VIRUSTOTAL
  ───────────────────────────────────────────────────────────────── */
  function VirusTotal({ pushToast }) {
    const toast = useToast();
    const notify = useCallback((msg, tone) => {
      if (toast) { tone === "error" ? toast.error(msg) : tone === "success" ? toast.success(msg) : toast.info(msg); }
      else if (pushToast) pushToast(msg, tone || "info");
    }, [toast, pushToast]);

    const [enabled, setEnabled] = useState(true);
    const [keysText, setKeysText] = useState("");
    const [statuses, setStatuses] = useState([]);
    const [meta, setMeta] = useState({ threshold: 1, upload: true, fail_closed: false });
    const [loading, setLoading] = useState(true);
    const [saving, setSaving] = useState(false);
    const [dirty, setDirty] = useState(false);

    const load = useCallback(() => {
      setLoading(true);
      api.get("/ui/api/virustotal").then(res => {
        setEnabled(!!res.enabled);
        setKeysText(res.keys_raw || "");
        setStatuses(res.statuses || []);
        setMeta({ threshold: res.threshold, upload: res.upload, fail_closed: res.fail_closed });
        setDirty(false);
      }).catch(e => notify("Failed to load VirusTotal settings: " + (e.message || e), "error"))
        .finally(() => setLoading(false));
    }, [notify]);
    useEffect(() => { load(); }, []);

    const keyCount = keysText.split(/[\n,;]+/).map(s => s.trim()).filter(Boolean).length;

    const save = useCallback(async () => {
      setSaving(true);
      try {
        const res = await api.post("/ui/api/virustotal", { enabled, keys: keysText });
        setStatuses(res.statuses || []);
        setDirty(false);
        notify(res.message || "VirusTotal settings saved", "success");
      } catch (e) {
        notify("Save failed: " + (e.message || e), "error");
      } finally { setSaving(false); }
    }, [enabled, keysText, notify]);

    if (loading) {
      return (
        <div className="screen">
          <PageHeader title="VirusTotal" description="Scan media attachments for malware before agy opens them." />
          <Skeleton height={120} />
          <Skeleton height={180} />
        </div>
      );
    }

    return (
      <div className="screen">
        <PageHeader
          title="VirusTotal"
          description="Scan media attachments for malware before agy opens them."
          actions={
            <div className="models-header-actions">
              {dirty && <Badge tone="warning">Unsaved</Badge>}
              <Button size="sm" variant="ghost" icon="refresh" onClick={load} disabled={loading}>Refresh</Button>
              <Button size="sm" variant="primary" icon={saving ? undefined : "check"} loading={saving} disabled={saving} onClick={save}>Save</Button>
            </div>
          }
        />

        <Card title="Scanning">
          <div className="cfg-row">
            <div className="cfg-label">
              <span className="cfg-label-name">Enable scanning</span>
              <span className="cfg-label-env mono">PROXY_VIRUSTOTAL_ENABLED</span>
            </div>
            <div className="cfg-ctl">
              <Switch checked={enabled} onChange={v => { setEnabled(v); setDirty(true); }} />
              <p className="cfg-hint">
                Every attached file is checked against VirusTotal before agy reads it. A file flagged
                malicious is deleted and the request is rejected — agy is never run on it. Verdicts are
                cached locally by file hash, so re-uploading the same file skips VirusTotal.
              </p>
            </div>
          </div>
        </Card>

        <Card
          title="API keys"
          description="One key per line. Keys are load-balanced round-robin; a key that errors (rate limit / auth) is parked for 1 hour and skipped, then resumes automatically."
        >
          <Textarea
            rows={6}
            className="mono"
            value={keysText}
            placeholder={"key1\nkey2\nkey3"}
            onChange={e => { setKeysText(e.target.value); setDirty(true); }}
          />
          <p className="cfg-hint">
            {keyCount} key{keyCount === 1 ? "" : "s"} configured · reject threshold {meta.threshold} engine{meta.threshold === 1 ? "" : "s"}
            {meta.fail_closed ? " · fail-closed (reject when unavailable)" : ""}.
          </p>
        </Card>

        <Card title="Key status" flush>
          {statuses.length === 0 ? (
            <EmptyState icon="shield" title="No keys yet" description="Add one or more VirusTotal API keys above and Save." compact />
          ) : (
            <div className="table-wrap">
              <table className="table">
                <thead><tr><th>Key</th><th>Status</th><th>Available again</th></tr></thead>
                <tbody>
                  {statuses.map((s, i) => (
                    <tr key={i}>
                      <td className="mono">{s.masked}</td>
                      <td><Badge tone={s.status === "active" ? "success" : "warning"}>{s.status === "active" ? "Active" : "Cooling down"}</Badge></td>
                      <td className="muted">{s.cooling_until ? new Date(s.cooling_until).toLocaleString() : "—"}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}
        </Card>
      </div>
    );
  }

  /* ─────────────────────────────────────────────────────────────────
     Exports
  ───────────────────────────────────────────────────────────────── */
  window.Configuration = Configuration;
  window.Models = Models;
  window.VirusTotal = VirusTotal;
  window.ApiKeys = ApiKeys;
})();
