// screens-cameras.jsx — Cameras page (RTSP/DVR monitoring), WP12.
//
// Mirrors ui/screens-images.jsx / ui/screens-tools.jsx for structure: plain
// window.* component library, window.api.get/post, useLive polling, no build
// step (Babel-in-browser). Exposes window.Cameras (registered by lib.jsx's
// ROUTES in WP0 — nothing to wire here).
//
// Sections: Sites (list + selector + create/edit), Add-DVR wizard, Camera
// grid (thumb/description/area/enable/disabled_reason + site Analyze button),
// Questions inbox, Watches editor, Run history (round-by-round transcript
// replay: mosaic -> full images -> clip <video> + fired-action result).
//
// All data lives behind /ui/api/cameras/* (camerahttp.go, WP11) and is
// admin-gated exactly like every other control-panel screen — the browser's
// session cookie already covers <img>/<video> requests to /camera/media/<token>.
(() => {
  const { useState, useEffect, useMemo, useCallback } = React;
  const {
    Icon, api, useLive, fmtMs, timeAgo,
    Button, IconButton, Card, StatCard, PageHeader, Field, Input, Textarea,
    Select, Switch, Checkbox, Badge, Tabs, Modal,
    useToast, EmptyState, Spinner, CodeBlock,
  } = window;

  /* ─────────────────────────────────────────────────────────────
     Shared helpers + small display atoms
     ───────────────────────────────────────────────────────────── */

  // mediaURL builds the served-capability path for a camera_captures token —
  // relative so it always hits the same origin/session as the rest of the panel.
  function mediaURL(token) {
    return token ? "/camera/media/" + token : "";
  }

  // ago renders an RFC3339 timestamp with the shared timeAgo() helper, which
  // expects milliseconds — Date.parse bridges the gap.
  function ago(ts) {
    if (!ts) return "—";
    const t = Date.parse(ts);
    return isNaN(t) ? ts : timeAgo(t);
  }

  const ANALYSIS_STATUS = {
    "": { tone: "neutral", label: "Not analyzed" },
    running: { tone: "info", label: "Analyzing…", pulse: true },
    idle: { tone: "success", label: "Analyzed" },
    error: { tone: "danger", label: "Analysis failed" },
  };
  function AnalysisStatusBadge({ status }) {
    const s = ANALYSIS_STATUS[status] || ANALYSIS_STATUS[""];
    return <Badge tone={s.tone} pulse={s.pulse}>{s.label}</Badge>;
  }

  const SUSPICION_TONE = { none: "neutral", low: "info", medium: "warning", high: "danger" };
  function SuspicionBadge({ value }) {
    if (!value) return null;
    return <Badge tone={SUSPICION_TONE[value] || "neutral"}>{value} suspicion</Badge>;
  }

  const RULE_STATUS_TONE = { not_met: "neutral", possibly_met: "warning", met: "danger" };
  function RuleStatusBadge({ value }) {
    if (!value) return null;
    return <Badge tone={RULE_STATUS_TONE[value] || "neutral"}>{value.replace(/_/g, " ")}</Badge>;
  }

  const DISABLED_REASON_LABEL = { black: "Black frame", "no-signal": "No signal", failed: "Capture failed" };
  function DisabledReasonBadge({ reason }) {
    if (!reason) return null;
    return <Badge tone="danger">{DISABLED_REASON_LABEL[reason] || reason}</Badge>;
  }

  // PasswordField — masked by default (the plan's "password shown masked"
  // requirement) with an eye toggle to reveal while typing.
  function PasswordField({ value, onChange, placeholder, disabled, autoComplete = "new-password" }) {
    const [show, setShow] = useState(false);
    return (
      <div className="cam-pw-row">
        <Input
          type={show ? "text" : "password"}
          mono
          value={value}
          onChange={onChange}
          placeholder={placeholder}
          disabled={disabled}
          autoComplete={autoComplete}
        />
        <IconButton
          icon={show ? "eye-off" : "eye"}
          label={show ? "Hide" : "Show"}
          size={13}
          onClick={() => setShow((s) => !s)}
        />
      </div>
    );
  }

  const BRAND_OPTIONS = [
    { value: "", label: "Auto-detect (RTSP probe)" },
    { value: "hikvision", label: "Hikvision (ISAPI)" },
    { value: "dahua", label: "Dahua (CGI)" },
    { value: "onvif", label: "ONVIF" },
  ];
  const WATCH_MODE_OPTIONS = [
    { value: "escalate", label: "Escalate (mosaic → full images → clip)" },
    { value: "mosaic_only", label: "Mosaic only (cheap, no escalation)" },
  ];
  const ACTION_TYPE_OPTIONS = [
    { value: "log", label: "Log (default) — record + badge" },
    { value: "none", label: "None — do nothing" },
    { value: "connect_callback", label: "Connect callback" },
    { value: "webhook", label: "Webhook" },
  ];

  /* ─────────────────────────────────────────────────────────────
     SITES — list + selector, create/edit modal
     ───────────────────────────────────────────────────────────── */

  function SiteFormModal({ open, mode, initial, aliasOptions, notify, onClose, onSaved }) {
    const [name, setName] = useState("");
    const [description, setDescription] = useState("");
    const [alias, setAlias] = useState("");
    const [saving, setSaving] = useState(false);

    useEffect(() => {
      if (!open) return;
      setName(initial ? initial.name || "" : "");
      setDescription(initial ? initial.description || "" : "");
      setAlias(initial ? initial.analysis_alias || "" : "");
    }, [open, initial]);

    const save = useCallback(async () => {
      const n = name.trim();
      if (!n) { notify("Name is required", "error"); return; }
      setSaving(true);
      try {
        const isEdit = mode === "edit";
        const path = isEdit ? "/ui/api/cameras/sites/update" : "/ui/api/cameras/sites";
        const body = { name: n, description, analysis_alias: alias };
        if (isEdit) body.id = initial.id;
        const res = await api.post(path, body);
        if (!res || res.ok === false) { notify((res && res.error) || "Save failed", "error"); return; }
        notify(isEdit ? "Site updated" : "Site created", "success");
        onSaved(res.site);
        onClose();
      } catch (e) {
        notify("Save failed: " + ((e && e.message) || e), "error");
      } finally { setSaving(false); }
    }, [mode, initial, name, description, alias, notify, onSaved, onClose]);

    return (
      <Modal
        open={open}
        onClose={onClose}
        title={mode === "edit" ? "Edit site" : "New site"}
        width={520}
        footer={
          <div className="cam-modal-footer">
            <Button variant="secondary" onClick={onClose} disabled={saving}>Cancel</Button>
            <Button variant="primary" icon="check" loading={saving} onClick={save}>
              {mode === "edit" ? "Save" : "Create site"}
            </Button>
          </div>
        }
      >
        <Field label="Name">
          <Input value={name} onChange={(e) => setName(e.target.value)} placeholder="Downtown Clinic" disabled={saving} />
        </Field>
        <Field
          label="Description"
          hint="Describe the place — rooms, layout, what's normal — so the AI can recognize areas and cameras."
        >
          <Textarea
            rows={4}
            value={description}
            onChange={(e) => setDescription(e.target.value)}
            placeholder="A small clinic with a waiting room, reception desk, two exam rooms, and a back entrance…"
            disabled={saving}
          />
        </Field>
        <Field label="Analysis backend" hint="Model alias used to describe/monitor this site. Leave blank to use the proxy default.">
          <Select
            options={[{ value: "", label: "Use default" }, ...aliasOptions]}
            value={alias}
            onChange={(e) => setAlias(e.target.value)}
            disabled={saving}
          />
        </Field>
      </Modal>
    );
  }

  function SitesPanel({ sites, siteId, site, setSiteId, aliasOptions, notify, refresh, onSiteDeleted }) {
    const [modalMode, setModalMode] = useState(""); // "" | "create" | "edit"
    const [confirmDelete, setConfirmDelete] = useState(false);
    const [analyzing, setAnalyzing] = useState(false);

    const openCreate = () => setModalMode("create");
    const openEdit = () => setModalMode("edit");

    const handleSaved = useCallback((savedSite) => {
      refresh();
      if (modalMode === "create" && savedSite && savedSite.id) setSiteId(savedSite.id);
    }, [modalMode, refresh, setSiteId]);

    const runAnalyze = useCallback(async () => {
      if (!site) return;
      setAnalyzing(true);
      try {
        const res = await api.post("/ui/api/cameras/sites/analyze", { id: site.id });
        notify(res && res.started ? "Analysis started" : "Analysis is already running for this site", "info");
        refresh();
      } catch (e) {
        notify("Failed to start analysis: " + ((e && e.message) || e), "error");
      } finally { setAnalyzing(false); }
    }, [site, notify, refresh]);

    const deleteSite = useCallback(async () => {
      if (!site) return;
      try {
        await api.post("/ui/api/cameras/sites/delete", { id: site.id });
        notify("Site deleted", "success");
        setConfirmDelete(false);
        onSiteDeleted();
        refresh();
      } catch (e) {
        notify("Delete failed: " + ((e && e.message) || e), "error");
      }
    }, [site, notify, refresh, onSiteDeleted]);

    return (
      <Card
        title="Sites"
        description="A monitored place (e.g. a clinic). Describe it once so the AI can recognize areas and cameras."
        actions={<Button size="sm" variant="primary" icon="plus" onClick={openCreate}>New site</Button>}
        flush
      >
        {sites.length === 0 ? (
          <EmptyState icon="server" title="No sites yet" description="Create a site to start adding DVRs and cameras." compact />
        ) : (
          <div className="cam-site-strip">
            {sites.map((s) => (
              <button
                key={s.id}
                type="button"
                className={"cam-site-chip" + (s.id === siteId ? " active" : "")}
                onClick={() => setSiteId(s.id)}
              >
                <span className="cam-site-chip-name">{s.name}</span>
                <AnalysisStatusBadge status={s.analysis_status} />
              </button>
            ))}
          </div>
        )}

        {site && (
          <div className="cam-site-detail">
            <div className="cam-site-detail-head">
              <div>
                <h4 className="cam-site-detail-name">{site.name}</h4>
                {site.description && <p className="cam-site-detail-desc">{site.description}</p>}
              </div>
              <div className="cam-site-detail-actions">
                <Button size="sm" variant="ghost" icon="edit" onClick={openEdit}>Edit</Button>
                <Button size="sm" variant="secondary" icon="bolt" loading={analyzing} onClick={runAnalyze}>
                  {site.analysis_status ? "Re-analyze" : "Analyze"}
                </Button>
                <Button size="sm" variant="ghost" icon="trash" onClick={() => setConfirmDelete(true)}>Delete</Button>
              </div>
            </div>

            {site.analysis && Array.isArray(site.analysis.areas) && site.analysis.areas.length > 0 && (
              <div className="cam-areas">
                {site.analysis.areas.map((a, i) => (
                  <div key={i} className="cam-area-chip" title={a.rationale || ""}>
                    <span className="cam-area-chip-name">{a.area}</span>
                    <span className="subtle">{(a.camera_ids || []).length} camera(s)</span>
                  </div>
                ))}
              </div>
            )}
            {site.analysis && Array.isArray(site.analysis.excluded) && site.analysis.excluded.length > 0 && (
              <p className="cam-hint-fail">
                {site.analysis.excluded.length} camera(s) excluded from analysis (blank/no signal):{" "}
                {site.analysis.excluded.map((e) => e.name || e.camera_id).join(", ")}
              </p>
            )}
            {site.analysis && site.analysis.error && (
              <div className="alert" data-tone="danger"><Icon name="error" size={13} /><span>{site.analysis.error}</span></div>
            )}
          </div>
        )}

        <SiteFormModal
          open={!!modalMode}
          mode={modalMode}
          initial={modalMode === "edit" ? site : null}
          aliasOptions={aliasOptions}
          notify={notify}
          onClose={() => setModalMode("")}
          onSaved={handleSaved}
        />
        <Modal
          open={confirmDelete}
          onClose={() => setConfirmDelete(false)}
          title="Delete this site?"
          description="This removes its DVRs, cameras, and watches. Past run history is left as a record."
          footer={
            <div className="cam-modal-footer">
              <Button variant="secondary" onClick={() => setConfirmDelete(false)}>Cancel</Button>
              <Button variant="danger" icon="trash" onClick={deleteSite}>Delete site</Button>
            </div>
          }
        />
      </Card>
    );
  }

  /* ─────────────────────────────────────────────────────────────
     ADD-DVR WIZARD + DVR list + camera grid ("Setup" tab)
     ───────────────────────────────────────────────────────────── */

  function AddDvrWizard({ siteId, notify, onAdded }) {
    const blank = { name: "", brand: "", host: "", port: 554, http_port: 80, username: "", password: "", timezone: "", ai_instructions: "" };
    const [form, setForm] = useState(blank);
    const [busy, setBusy] = useState(false);
    const [result, setResult] = useState(null);
    const set = (k) => (e) => setForm((f) => ({ ...f, [k]: e.target.value }));

    const submit = useCallback(async () => {
      const host = form.host.trim();
      if (!host) { notify("Host is required", "error"); return; }
      setBusy(true); setResult(null);
      try {
        const res = await api.post("/ui/api/cameras/dvrs", {
          site_id: siteId, name: form.name.trim(), brand: form.brand, host,
          port: Number(form.port) || 0, http_port: Number(form.http_port) || 0,
          username: form.username.trim(), password: form.password, timezone: form.timezone.trim(),
          ai_instructions: form.ai_instructions,
        });
        if (!res || !res.ok) { notify((res && res.error) || "Failed to add DVR", "error"); return; }
        notify("DVR added — discovering channels…", "info");
        const disc = await api.post("/ui/api/cameras/dvrs/discover", { id: res.id }, { timeoutMs: 45000 });
        if (!disc || !disc.ok) {
          notify("Added, but discovery failed: " + ((disc && disc.error) || "unknown error"), "error");
        } else {
          setResult({ channels_found: disc.channels_found, added: disc.added, updated: disc.updated });
          notify(`Discovered ${disc.channels_found} channel(s) — ${disc.added} added`, "success");
        }
        setForm(blank);
        onAdded();
      } catch (e) {
        notify("Failed to add DVR: " + ((e && e.message) || e), "error");
      } finally { setBusy(false); }
    }, [form, siteId, notify, onAdded]);

    return (
      <Card title="Add a DVR" description="Point at the DVR/NVR IP and credentials — the proxy auto-discovers every channel.">
        <div className="cfg-form">
          <div className="cfg-row">
            <div className="cfg-label"><span className="cfg-label-name">Brand</span><span className="cfg-label-env">Adapter</span></div>
            <div className="cfg-ctl"><Select options={BRAND_OPTIONS} value={form.brand} onChange={set("brand")} disabled={busy} /></div>
          </div>
          <div className="cfg-row">
            <div className="cfg-label"><span className="cfg-label-name">Name</span><span className="cfg-label-env">Optional</span></div>
            <div className="cfg-ctl"><Input value={form.name} onChange={set("name")} placeholder="Lobby NVR" disabled={busy} /></div>
          </div>
          <div className="cfg-row">
            <div className="cfg-label"><span className="cfg-label-name">Host</span><span className="cfg-label-env">IP or hostname</span></div>
            <div className="cfg-ctl"><Input mono value={form.host} onChange={set("host")} placeholder="192.168.1.10" disabled={busy} /></div>
          </div>
          <div className="cfg-row">
            <div className="cfg-label"><span className="cfg-label-name">Ports</span><span className="cfg-label-env">RTSP / HTTP</span></div>
            <div className="cfg-ctl cam-inline-fields">
              <Input type="number" mono value={form.port} onChange={set("port")} placeholder="554" disabled={busy} />
              <Input type="number" mono value={form.http_port} onChange={set("http_port")} placeholder="80" disabled={busy} />
            </div>
          </div>
          <div className="cfg-row">
            <div className="cfg-label"><span className="cfg-label-name">Username</span></div>
            <div className="cfg-ctl"><Input value={form.username} onChange={set("username")} placeholder="admin" disabled={busy} autoComplete="off" /></div>
          </div>
          <div className="cfg-row">
            <div className="cfg-label"><span className="cfg-label-name">Password</span><span className="cfg-label-env">Encrypted at rest</span></div>
            <div className="cfg-ctl"><PasswordField value={form.password} onChange={set("password")} placeholder="••••••••" disabled={busy} /></div>
          </div>
          <div className="cfg-row">
            <div className="cfg-label"><span className="cfg-label-name">Timezone</span><span className="cfg-label-env">IANA, for playback</span></div>
            <div className="cfg-ctl">
              <Input value={form.timezone} onChange={set("timezone")} placeholder="Asia/Amman" disabled={busy} />
              <p className="cfg-hint">Used to format past-playback time windows correctly (Dahua uses local time).</p>
            </div>
          </div>
          <div className="cfg-row">
            <div className="cfg-label"><span className="cfg-label-name">AI instructions</span><span className="cfg-label-env">Optional</span></div>
            <div className="cfg-ctl">
              <Textarea rows={4} value={form.ai_instructions} onChange={set("ai_instructions")} placeholder="Staff enter via camera 3's door; camera 16's closed door leads to the staff room…" disabled={busy} />
              <p className="cfg-hint">Operator knowledge injected into every investigation — doors, rooms, movement rules for this DVR's cameras.</p>
            </div>
          </div>
        </div>
        <div className="cam-wizard-actions">
          <Button variant="primary" icon={busy ? undefined : "bolt"} loading={busy} disabled={busy || !form.host.trim()} onClick={submit}>
            {busy ? "Adding…" : "Add & discover"}
          </Button>
          {result && <Badge tone="success">{result.channels_found} channel(s) found · {result.added} added · {result.updated} updated</Badge>}
        </div>
      </Card>
    );
  }

  function DvrFormModal({ open, dvr, notify, onClose, onSaved }) {
    const [form, setForm] = useState(null);
    const [saving, setSaving] = useState(false);

    useEffect(() => {
      if (open && dvr) {
        setForm({
          name: dvr.name || "", brand: dvr.brand || "", host: dvr.host || "",
          port: dvr.port || 554, http_port: dvr.http_port || 80,
          username: dvr.username || "", password: "", timezone: dvr.timezone || "",
          ai_instructions: dvr.ai_instructions || "",
        });
      }
    }, [open, dvr]);

    if (!form) return null;
    const set = (k) => (e) => setForm((f) => ({ ...f, [k]: e.target.value }));

    const save = async () => {
      setSaving(true);
      try {
        const body = {
          id: dvr.id, name: form.name, brand: form.brand, host: form.host,
          port: Number(form.port) || 0, http_port: Number(form.http_port) || 0,
          username: form.username, timezone: form.timezone,
          ai_instructions: form.ai_instructions,
        };
        if (form.password) body.password = form.password;
        const res = await api.post("/ui/api/cameras/dvrs/update", body);
        if (!res || !res.ok) { notify((res && res.error) || "Save failed", "error"); return; }
        notify("DVR updated", "success");
        onSaved();
      } catch (e) {
        notify("Save failed: " + ((e && e.message) || e), "error");
      } finally { setSaving(false); }
    };

    return (
      <Modal
        open={open}
        onClose={onClose}
        title="Edit DVR"
        width={520}
        footer={
          <div className="cam-modal-footer">
            <Button variant="secondary" onClick={onClose} disabled={saving}>Cancel</Button>
            <Button variant="primary" icon="check" loading={saving} onClick={save}>Save</Button>
          </div>
        }
      >
        <Field label="Brand"><Select options={BRAND_OPTIONS} value={form.brand} onChange={set("brand")} disabled={saving} /></Field>
        <Field label="Name"><Input value={form.name} onChange={set("name")} disabled={saving} /></Field>
        <Field label="Host"><Input mono value={form.host} onChange={set("host")} disabled={saving} /></Field>
        <Field label="RTSP port"><Input type="number" mono value={form.port} onChange={set("port")} disabled={saving} /></Field>
        <Field label="HTTP port"><Input type="number" mono value={form.http_port} onChange={set("http_port")} disabled={saving} /></Field>
        <Field label="Username"><Input value={form.username} onChange={set("username")} disabled={saving} autoComplete="off" /></Field>
        <Field label="Password" hint="Leave blank to keep the existing password.">
          <PasswordField value={form.password} onChange={set("password")} placeholder="Unchanged" disabled={saving} />
        </Field>
        <Field label="Timezone"><Input value={form.timezone} onChange={set("timezone")} placeholder="Asia/Amman" disabled={saving} /></Field>
        <Field label="AI instructions" hint="Operator knowledge injected into every investigation — doors, rooms, movement rules for this DVR's cameras.">
          <Textarea rows={4} value={form.ai_instructions} onChange={set("ai_instructions")} disabled={saving} />
        </Field>
      </Modal>
    );
  }

  function CameraEditModal({ open, camera, notify, onClose, onSaved }) {
    const [name, setName] = useState("");
    const [area, setArea] = useState("");
    const [notes, setNotes] = useState("");
    const [saving, setSaving] = useState(false);

    useEffect(() => {
      if (open && camera) { setName(camera.name || ""); setArea(camera.area || ""); setNotes(camera.notes || ""); }
    }, [open, camera]);

    if (!camera) return null;

    const save = async () => {
      setSaving(true);
      try {
        const res = await api.post("/ui/api/cameras/update", { id: camera.id, name, area, notes });
        if (!res || !res.ok) { notify((res && res.error) || "Save failed", "error"); return; }
        notify("Camera updated", "success");
        onSaved();
      } catch (e) {
        notify("Save failed: " + ((e && e.message) || e), "error");
      } finally { setSaving(false); }
    };

    return (
      <Modal
        open={open}
        onClose={onClose}
        title={"Edit " + camera.name}
        width={440}
        footer={
          <div className="cam-modal-footer">
            <Button variant="secondary" onClick={onClose} disabled={saving}>Cancel</Button>
            <Button variant="primary" icon="check" loading={saving} onClick={save}>Save</Button>
          </div>
        }
      >
        <Field label="Name"><Input value={name} onChange={(e) => setName(e.target.value)} disabled={saving} /></Field>
        <Field label="Area" hint="e.g. Waiting room, Reception, Back entrance">
          <Input value={area} onChange={(e) => setArea(e.target.value)} disabled={saving} />
        </Field>
        <Field label="Operator notes" hint="e.g. 'the closed door here leads to the staff room'">
          <Textarea rows={3} value={notes} onChange={(e) => setNotes(e.target.value)} disabled={saving} />
        </Field>
      </Modal>
    );
  }

  function CameraCard({ camera, onEdit, notify, refresh }) {
    const [snapBusy, setSnapBusy] = useState(false);
    const thumb = mediaURL(camera.snapshot_token);

    const takeSnapshot = useCallback(async () => {
      setSnapBusy(true);
      try {
        const res = await api.post("/ui/api/cameras/snapshot", { id: camera.id, quality: "sub" }, { timeoutMs: 45000 });
        if (!res || !res.ok) notify((res && res.error) || "Snapshot failed", "error");
        else refresh();
      } catch (e) {
        notify("Snapshot failed: " + ((e && e.message) || e), "error");
      } finally { setSnapBusy(false); }
    }, [camera.id, notify, refresh]);

    const toggleEnabled = useCallback(async () => {
      try {
        await api.post("/ui/api/cameras/update", { id: camera.id, enabled: !camera.enabled });
        refresh();
      } catch (e) {
        notify("Update failed: " + ((e && e.message) || e), "error");
      }
    }, [camera, notify, refresh]);

    return (
      <div className={"cam-card" + (camera.enabled ? "" : " disabled")}>
        <div className="cam-card-thumb">
          {thumb ? <img src={thumb} alt={camera.name} /> : (
            <div className="cam-card-thumb-empty"><Icon name="image" size={20} /></div>
          )}
          <button type="button" className="cam-card-thumb-btn" onClick={takeSnapshot} disabled={snapBusy} title="Capture a fresh snapshot">
            {snapBusy ? <Spinner size={13} /> : <Icon name="refresh" size={13} />}
          </button>
        </div>
        <div className="cam-card-body">
          <div className="cam-card-title-row">
            <span className="cam-card-name truncate">{camera.name}</span>
            <IconButton icon="edit" label="Edit camera" size={13} onClick={onEdit} />
          </div>
          <div className="cam-card-badges">
            {camera.area ? <Badge tone="accent">{camera.area}</Badge> : <span className="subtle cam-card-unassigned">Unassigned</span>}
            <DisabledReasonBadge reason={camera.disabled_reason} />
          </div>
          {camera.ai_description && <p className="cam-card-desc">{camera.ai_description}</p>}
          <div className="cam-card-foot">
            <Switch checked={!!camera.enabled} onChange={toggleEnabled} label={camera.enabled ? "Enabled" : "Disabled"} />
            <span className="muted subtle">ch {camera.channel}</span>
          </div>
        </div>
      </div>
    );
  }

  function SetupTab({ site, dvrs, cameras, notify, refreshDvrs, refreshCameras }) {
    const [editingDvr, setEditingDvr] = useState(null);
    const [deleteDvrId, setDeleteDvrId] = useState("");
    const [discoveringId, setDiscoveringId] = useState("");
    const [editCamera, setEditCamera] = useState(null);
    const [analyzing, setAnalyzing] = useState(false);

    const analysisRunning = site.analysis_status === "running";

    const runAnalyze = useCallback(async () => {
      setAnalyzing(true);
      try {
        const res = await api.post("/ui/api/cameras/sites/analyze", { id: site.id });
        notify(res && res.started ? "Analysis started" : "Analysis is already running for this site", "info");
      } catch (e) {
        notify("Failed to start analysis: " + ((e && e.message) || e), "error");
      } finally { setAnalyzing(false); }
    }, [site, notify]);

    const discoverDvr = useCallback(async (id) => {
      setDiscoveringId(id);
      try {
        const res = await api.post("/ui/api/cameras/dvrs/discover", { id }, { timeoutMs: 45000 });
        if (!res || !res.ok) notify("Discovery failed: " + ((res && res.error) || "unknown error"), "error");
        else notify(`Discovered ${res.channels_found} channel(s) — ${res.added} added, ${res.updated} updated`, "success");
        refreshDvrs(); refreshCameras();
      } catch (e) {
        notify("Discovery failed: " + ((e && e.message) || e), "error");
      } finally { setDiscoveringId(""); }
    }, [notify, refreshDvrs, refreshCameras]);

    const toggleDvr = useCallback(async (d) => {
      try {
        await api.post("/ui/api/cameras/dvrs/toggle", { id: d.id, enabled: !d.enabled });
        refreshDvrs();
      } catch (e) {
        notify("Update failed: " + ((e && e.message) || e), "error");
      }
    }, [notify, refreshDvrs]);

    const deleteDvr = useCallback(async () => {
      try {
        await api.post("/ui/api/cameras/dvrs/delete", { id: deleteDvrId });
        notify("DVR deleted", "success");
        setDeleteDvrId("");
        refreshDvrs(); refreshCameras();
      } catch (e) {
        notify("Delete failed: " + ((e && e.message) || e), "error");
      }
    }, [deleteDvrId, notify, refreshDvrs, refreshCameras]);

    return (
      <div className="cam-tab-body">
        <AddDvrWizard siteId={site.id} notify={notify} onAdded={() => { refreshDvrs(); refreshCameras(); }} />

        <Card title="DVRs" description="Every DVR/NVR added to this site." flush>
          {dvrs.length === 0 ? (
            <EmptyState icon="server" title="No DVRs yet" description="Use the wizard above to add one." compact />
          ) : (
            <div className="table-wrap">
              <table className="table">
                <thead>
                  <tr><th>Name</th><th>Brand</th><th>Host</th><th>Password</th><th>Channels</th><th>Last discovered</th><th>Enabled</th><th></th></tr>
                </thead>
                <tbody>
                  {dvrs.map((d) => (
                    <tr key={d.id}>
                      <td>{d.name || "—"}</td>
                      <td><Badge tone="neutral">{d.brand || "auto"}</Badge></td>
                      <td className="mono">{d.host}:{d.port || 554}</td>
                      <td className="mono muted">{d.password_preview || "—"}</td>
                      <td className="tnum">{d.channels_discovered}</td>
                      <td className="muted">{d.last_discovered_at ? ago(d.last_discovered_at) : "never"}</td>
                      <td><Switch checked={!!d.enabled} onChange={() => toggleDvr(d)} /></td>
                      <td className="cam-row-actions">
                        <Button size="sm" variant="ghost" icon="refresh" loading={discoveringId === d.id} onClick={() => discoverDvr(d.id)}>Discover</Button>
                        <IconButton icon="edit" label="Edit DVR" onClick={() => setEditingDvr(d)} />
                        <IconButton icon="trash" label="Delete DVR" onClick={() => setDeleteDvrId(d.id)} />
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}
        </Card>

        <Card
          title="Cameras"
          description={cameras.length + " camera(s) discovered on this site."}
          actions={
            <Button size="sm" variant="secondary" icon="bolt" loading={analyzing || analysisRunning} onClick={runAnalyze}>
              {site.analysis_status ? "Re-analyze" : "Analyze"}
            </Button>
          }
          flush
        >
          {cameras.length === 0 ? (
            <EmptyState icon="image" title="No cameras yet" description="Add a DVR and discover its channels to populate this grid." compact />
          ) : (
            <div className="cam-grid">
              {cameras.map((c) => (
                <CameraCard key={c.id} camera={c} onEdit={() => setEditCamera(c)} notify={notify} refresh={refreshCameras} />
              ))}
            </div>
          )}
        </Card>

        <DvrFormModal open={!!editingDvr} dvr={editingDvr} notify={notify} onClose={() => setEditingDvr(null)} onSaved={() => { setEditingDvr(null); refreshDvrs(); }} />
        <CameraEditModal open={!!editCamera} camera={editCamera} notify={notify} onClose={() => setEditCamera(null)} onSaved={() => { setEditCamera(null); refreshCameras(); }} />
        <Modal
          open={!!deleteDvrId}
          onClose={() => setDeleteDvrId("")}
          title="Delete this DVR?"
          description="This removes the DVR and all of its discovered cameras. Captured media already saved is left as-is."
          footer={
            <div className="cam-modal-footer">
              <Button variant="secondary" onClick={() => setDeleteDvrId("")}>Cancel</Button>
              <Button variant="danger" icon="trash" onClick={deleteDvr}>Delete DVR</Button>
            </div>
          }
        />
      </div>
    );
  }

  /* ─────────────────────────────────────────────────────────────
     QUESTIONS INBOX
     ───────────────────────────────────────────────────────────── */

  function QuestionCard({ question, camerasById, notify, refresh }) {
    const [answer, setAnswer] = useState(question.answer || "");
    const [busy, setBusy] = useState(false);
    const cams = (question.camera_ids || []).map((id) => camerasById[id]).filter(Boolean);

    const submit = async () => {
      const a = answer.trim();
      if (!a) { notify("Write an answer first", "error"); return; }
      setBusy(true);
      try {
        await api.post("/ui/api/cameras/questions/answer", { id: question.id, answer: a });
        notify("Answer saved", "success");
        refresh();
      } catch (e) {
        notify("Failed: " + ((e && e.message) || e), "error");
      } finally { setBusy(false); }
    };
    const dismiss = async () => {
      setBusy(true);
      try {
        await api.post("/ui/api/cameras/questions/dismiss", { id: question.id });
        refresh();
      } catch (e) {
        notify("Failed: " + ((e && e.message) || e), "error");
      } finally { setBusy(false); }
    };

    return (
      <div className="cam-question">
        <div className="cam-question-head">
          <p className="cam-question-text">{question.question}</p>
          <Badge tone={question.status === "open" ? "warning" : question.status === "answered" ? "success" : "neutral"}>{question.status}</Badge>
        </div>
        {cams.length > 0 && (
          <div className="cam-question-cams">
            {cams.map((c) => (
              <div key={c.id} className="cam-question-cam">
                {c.snapshot_token ? <img src={mediaURL(c.snapshot_token)} alt={c.name} /> : <div className="cam-card-thumb-empty small"><Icon name="image" size={13} /></div>}
                <span className="mono">{c.name}</span>
              </div>
            ))}
          </div>
        )}
        {question.status === "open" ? (
          <div className="cam-question-answer">
            <Textarea rows={2} value={answer} onChange={(e) => setAnswer(e.target.value)} placeholder="Type the answer the AI should use…" disabled={busy} />
            <div className="cam-question-actions">
              <Button size="sm" variant="primary" icon="check" loading={busy} onClick={submit}>Submit answer</Button>
              <Button size="sm" variant="ghost" onClick={dismiss} disabled={busy}>Dismiss</Button>
            </div>
          </div>
        ) : (
          question.answer && <p className="cam-question-answered muted">Answer: {question.answer}</p>
        )}
      </div>
    );
  }

  function QuestionsTab({ questions, camerasById, notify, refresh }) {
    const [statusFilter, setStatusFilter] = useState("open");
    const filtered = questions.filter((q) => statusFilter === "all" || q.status === statusFilter);

    return (
      <div className="cam-tab-body">
        <Card
          title="Questions inbox"
          description="Clarifying questions the AI asked while describing this site."
          actions={
            <Select
              value={statusFilter}
              onChange={(e) => setStatusFilter(e.target.value)}
              options={[
                { value: "open", label: "Open" },
                { value: "answered", label: "Answered" },
                { value: "dismissed", label: "Dismissed" },
                { value: "all", label: "All" },
              ]}
            />
          }
          flush
        >
          {filtered.length === 0 ? (
            <EmptyState icon="info" title="No questions" description="Nothing needs your input right now." compact />
          ) : (
            <div className="cam-questions">
              {filtered.map((q) => (
                <QuestionCard key={q.id} question={q} camerasById={camerasById} notify={notify} refresh={refresh} />
              ))}
            </div>
          )}
        </Card>
      </div>
    );
  }

  /* ─────────────────────────────────────────────────────────────
     WATCHES EDITOR
     ───────────────────────────────────────────────────────────── */

  function WatchFormModal({ open, watch, siteId, cameras, aliasOptions, notify, onClose, onSaved }) {
    const blank = {
      name: "", instruction: "", camera_ids: [], analysis_alias: "", mode: "escalate",
      interval_seconds: 300, active_hours: "", max_rounds: 4, enabled: false,
      action_type: "log", action_url: "", action_secret: "",
    };
    const [form, setForm] = useState(blank);
    const [saving, setSaving] = useState(false);

    useEffect(() => {
      if (!open) return;
      if (watch) {
        setForm({
          name: watch.name || "", instruction: watch.instruction || "",
          camera_ids: watch.camera_ids || [], analysis_alias: watch.analysis_alias || "",
          mode: watch.mode || "escalate", interval_seconds: watch.interval_seconds || 300,
          active_hours: watch.active_hours || "", max_rounds: watch.max_rounds || 4,
          enabled: !!watch.enabled,
          action_type: (watch.action && watch.action.type) || "log",
          action_url: (watch.action && watch.action.url) || "",
          action_secret: "",
          has_secret: !!(watch.action && watch.action.has_secret),
        });
      } else {
        setForm(blank);
      }
    }, [open, watch]);

    const toggleCamera = (id) => {
      setForm((f) => ({
        ...f,
        camera_ids: f.camera_ids.includes(id) ? f.camera_ids.filter((x) => x !== id) : [...f.camera_ids, id],
      }));
    };

    const needsUrl = form.action_type === "connect_callback" || form.action_type === "webhook";

    const save = useCallback(async () => {
      const name = form.name.trim();
      if (!name) { notify("Name is required", "error"); return; }

      // The update endpoint replaces the whole action config in one shot (there
      // is no per-field "keep existing secret" on the server) — so only send an
      // "action" payload when the operator actually touched it this session,
      // otherwise the previously-stored secret would be silently wiped.
      const originalAction = (watch && watch.action) || { type: "log", url: "" };
      const actionChanged = !watch
        || form.action_type !== originalAction.type
        || form.action_url !== (originalAction.url || "")
        || form.action_secret.trim() !== "";
      if (needsUrl && actionChanged && !form.action_url.trim()) {
        notify("Enter a target URL for this action type", "error");
        return;
      }

      setSaving(true);
      try {
        const body = {
          name, instruction: form.instruction, camera_ids: form.camera_ids,
          analysis_alias: form.analysis_alias, mode: form.mode,
          interval_seconds: Number(form.interval_seconds) || 300,
          active_hours: form.active_hours, max_rounds: Number(form.max_rounds) || 0,
        };
        let path;
        if (watch) {
          path = "/ui/api/cameras/watches/update";
          body.id = watch.id;
          if (actionChanged) body.action = { type: form.action_type, url: form.action_url, secret: form.action_secret };
        } else {
          path = "/ui/api/cameras/watches";
          body.site_id = siteId;
          body.enabled = form.enabled;
          body.action = { type: form.action_type, url: form.action_url, secret: form.action_secret };
        }
        const res = await api.post(path, body);
        if (!res || !res.ok) { notify((res && res.error) || "Save failed", "error"); return; }
        if (watch && form.enabled !== !!watch.enabled) {
          await api.post("/ui/api/cameras/watches/toggle", { id: watch.id, enabled: form.enabled });
        }
        notify(watch ? "Watch updated" : "Watch created", "success");
        onSaved();
      } catch (e) {
        notify("Save failed: " + ((e && e.message) || e), "error");
      } finally { setSaving(false); }
    }, [form, watch, siteId, needsUrl, notify, onSaved]);

    return (
      <Modal
        open={open}
        onClose={onClose}
        title={watch ? "Edit watch" : "New watch"}
        width={640}
        footer={
          <div className="cam-modal-footer">
            <Button variant="secondary" onClick={onClose} disabled={saving}>Cancel</Button>
            <Button variant="primary" icon="check" loading={saving} onClick={save}>{watch ? "Save" : "Create watch"}</Button>
          </div>
        }
      >
        <Field label="Name">
          <Input value={form.name} onChange={(e) => setForm((f) => ({ ...f, name: e.target.value }))} placeholder="After-hours entrance watch" disabled={saving} />
        </Field>
        <Field label="Instruction" hint="What should the AI look for? e.g. &quot;Alert if anyone enters after 8pm or a door is left open.&quot;">
          <Textarea rows={3} value={form.instruction} onChange={(e) => setForm((f) => ({ ...f, instruction: e.target.value }))} disabled={saving} />
        </Field>
        <Field label="Cameras" hint="Leave empty to include every enabled camera on this site.">
          <div className="cam-multiselect">
            {cameras.length === 0 ? (
              <p className="cfg-hint">No cameras discovered yet.</p>
            ) : cameras.map((c) => (
              <Checkbox
                key={c.id}
                checked={form.camera_ids.includes(c.id)}
                onChange={() => toggleCamera(c.id)}
                label={c.name + (c.area ? " · " + c.area : "")}
              />
            ))}
          </div>
        </Field>
        <div className="cam-form-grid-2">
          <Field label="Mode"><Select options={WATCH_MODE_OPTIONS} value={form.mode} onChange={(e) => setForm((f) => ({ ...f, mode: e.target.value }))} disabled={saving} /></Field>
          <Field label="Analysis backend">
            <Select options={[{ value: "", label: "Use site/default" }, ...aliasOptions]} value={form.analysis_alias} onChange={(e) => setForm((f) => ({ ...f, analysis_alias: e.target.value }))} disabled={saving} />
          </Field>
          <Field label="Interval (seconds)"><Input type="number" min={30} value={form.interval_seconds} onChange={(e) => setForm((f) => ({ ...f, interval_seconds: e.target.value }))} disabled={saving} /></Field>
          <Field label="Max rounds" hint="1–6, the mosaic round is included"><Input type="number" min={1} max={6} value={form.max_rounds} onChange={(e) => setForm((f) => ({ ...f, max_rounds: e.target.value }))} disabled={saving} /></Field>
        </div>
        <Field label="Active hours" hint={'Comma-separated "HH:MM-HH:MM" ranges in server-local time. Leave blank to always run — e.g. "22:00-06:00".'}>
          <Input mono value={form.active_hours} onChange={(e) => setForm((f) => ({ ...f, active_hours: e.target.value }))} placeholder="22:00-06:00" disabled={saving} />
        </Field>
        <Field label="Action">
          <Select options={ACTION_TYPE_OPTIONS} value={form.action_type} onChange={(e) => setForm((f) => ({ ...f, action_type: e.target.value }))} disabled={saving} />
        </Field>
        {needsUrl && (
          <>
            <Field label="Target URL">
              <Input mono value={form.action_url} onChange={(e) => setForm((f) => ({ ...f, action_url: e.target.value }))} placeholder="https://…" disabled={saving} />
            </Field>
            <Field
              label={form.action_type === "webhook" ? "Signing secret" : "Bearer token"}
              hint={watch
                ? "Leave the whole action section unchanged to keep the current URL and secret — entering a new secret saves the URL and secret together."
                : "Stored encrypted; never shown again."}
            >
              <PasswordField value={form.action_secret} onChange={(e) => setForm((f) => ({ ...f, action_secret: e.target.value }))} placeholder={form.has_secret ? "Unchanged" : ""} disabled={saving} />
            </Field>
          </>
        )}
        <Switch checked={form.enabled} onChange={(v) => setForm((f) => ({ ...f, enabled: v }))} label="Enabled" />
      </Modal>
    );
  }

  function WatchesTab({ site, watches, cameras, aliasOptions, notify, refresh, onSelectForHistory }) {
    const [formOpen, setFormOpen] = useState(false);
    const [editing, setEditing] = useState(null);
    const [deleteId, setDeleteId] = useState("");
    const [runningId, setRunningId] = useState("");

    const runNow = async (id) => {
      setRunningId(id);
      try {
        const res = await api.post("/ui/api/cameras/watches/run", { id });
        if (!res || !res.ok) notify((res && res.error) || "Could not start run", "error");
        else notify("Run started", "success");
      } catch (e) {
        notify("Failed: " + ((e && e.message) || e), "error");
      } finally { setRunningId(""); }
    };
    const toggleWatch = async (w) => {
      try {
        await api.post("/ui/api/cameras/watches/toggle", { id: w.id, enabled: !w.enabled });
        refresh();
      } catch (e) {
        notify("Update failed: " + ((e && e.message) || e), "error");
      }
    };
    const deleteWatch = async () => {
      try {
        await api.post("/ui/api/cameras/watches/delete", { id: deleteId });
        notify("Watch deleted", "success");
        setDeleteId("");
        refresh();
      } catch (e) {
        notify("Delete failed: " + ((e && e.message) || e), "error");
      }
    };

    return (
      <div className="cam-tab-body">
        <Card
          title="Watches"
          description="Scheduled monitoring rules — the AI checks in on an interval and escalates on its own."
          actions={<Button size="sm" variant="primary" icon="plus" onClick={() => { setEditing(null); setFormOpen(true); }}>New watch</Button>}
          flush
        >
          {watches.length === 0 ? (
            <EmptyState icon="activity" title="No watches yet" description="Create a watch to start scheduled monitoring." compact />
          ) : (
            <div className="table-wrap">
              <table className="table">
                <thead>
                  <tr><th>Name</th><th>Mode</th><th>Interval</th><th>Action</th><th>Next run</th><th>Enabled</th><th></th></tr>
                </thead>
                <tbody>
                  {watches.map((w) => (
                    <tr key={w.id}>
                      <td>{w.name || "Untitled watch"}</td>
                      <td><Badge tone="neutral">{w.mode === "mosaic_only" ? "mosaic only" : "escalate"}</Badge></td>
                      <td className="mono">{w.interval_seconds}s</td>
                      <td><Badge tone="neutral">{w.action.type}</Badge></td>
                      <td className="muted">{w.next_run_at ? ago(w.next_run_at) : "—"}</td>
                      <td><Switch checked={!!w.enabled} onChange={() => toggleWatch(w)} /></td>
                      <td className="cam-row-actions">
                        <Button size="sm" variant="ghost" icon="play" loading={runningId === w.id} onClick={() => runNow(w.id)}>Run now</Button>
                        <Button size="sm" variant="ghost" icon="clock" onClick={() => onSelectForHistory(w.id)}>History</Button>
                        <IconButton icon="edit" label="Edit watch" onClick={() => { setEditing(w); setFormOpen(true); }} />
                        <IconButton icon="trash" label="Delete watch" onClick={() => setDeleteId(w.id)} />
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}
        </Card>

        <WatchFormModal
          open={formOpen}
          watch={editing}
          siteId={site.id}
          cameras={cameras}
          aliasOptions={aliasOptions}
          notify={notify}
          onClose={() => setFormOpen(false)}
          onSaved={() => { setFormOpen(false); refresh(); }}
        />
        <Modal
          open={!!deleteId}
          onClose={() => setDeleteId("")}
          title="Delete this watch?"
          description="This stops scheduled monitoring for this watch. Past run history is kept."
          footer={
            <div className="cam-modal-footer">
              <Button variant="secondary" onClick={() => setDeleteId("")}>Cancel</Button>
              <Button variant="danger" icon="trash" onClick={deleteWatch}>Delete watch</Button>
            </div>
          }
        />
      </div>
    );
  }

  /* ─────────────────────────────────────────────────────────────
     RUN HISTORY — transcript replay
     ───────────────────────────────────────────────────────────── */

  function RoundView({ round, camLabel }) {
    const media = round.media || [];
    const mosaics = media.filter((m) => m.kind === "mosaic");
    const snapshots = media.filter((m) => m.kind === "snapshot");
    const clips = media.filter((m) => m.kind === "clip");
    const frames = media.filter((m) => m.kind === "frame");
    const decision = round.decision;

    return (
      <div className="cam-round">
        <div className="cam-round-head">
          <Badge tone="neutral">Round {round.round}</Badge>
          <Badge tone="accent">{round.kind}</Badge>
          <SuspicionBadge value={round.suspicion} />
          <RuleStatusBadge value={round.rule_status} />
          {round.parse_ok === false && <Badge tone="danger">parse failed</Badge>}
          {round.repair_used && <Badge tone="warning">repair used</Badge>}
          {round.latency_ms > 0 && <span className="muted subtle mono">{fmtMs(round.latency_ms)}</span>}
        </div>

        {mosaics.map((m, i) => (
          m.token ? <img key={i} className="cam-round-mosaic" src={mediaURL(m.token)} alt="mosaic" /> : null
        ))}

        {snapshots.length > 0 && (
          <div className="cam-round-grid">
            {snapshots.map((s, i) => (
              <div key={i} className="cam-round-tile">
                {s.token ? <img src={mediaURL(s.token)} alt={camLabel(s.camera_id)} /> : <div className="cam-card-thumb-empty small"><Icon name="error" size={13} /></div>}
                <span>{camLabel(s.camera_id)}</span>
              </div>
            ))}
          </div>
        )}

        {clips.map((c, i) => (
          c.token
            ? <video key={i} className="cam-round-clip" controls src={mediaURL(c.token)} />
            : <p key={i} className="cam-hint-fail">Clip capture failed for {camLabel(c.camera_id)}</p>
        ))}

        {frames.length > 0 && (
          <div className="cam-round-grid">
            {frames.map((f, i) => (
              <div key={i} className="cam-round-tile">
                {f.token ? <img src={mediaURL(f.token)} alt={camLabel(f.camera_id)} /> : <div className="cam-card-thumb-empty small"><Icon name="error" size={13} /></div>}
                <span>frame</span>
              </div>
            ))}
          </div>
        )}

        {decision && (
          <div className="cam-round-decision">
            {decision.round_summary && <p className="cam-round-summary">{decision.round_summary}</p>}
            {Array.isArray(decision.per_camera) && decision.per_camera.length > 0 && (
              <ul className="cam-round-obs">
                {decision.per_camera.map((p, i) => (
                  <li key={i}>
                    <span className="mono">{camLabel(p.camera_id)}</span>
                    {p.notable && <Badge tone="warning">notable</Badge>}
                    <span>{p.observation}</span>
                  </li>
                ))}
              </ul>
            )}
            {decision.need_more && decision.need_more.type && decision.need_more.type !== "none" && (
              <p className="cam-hint-fail">
                Requested: {decision.need_more.type}
                {Array.isArray(decision.need_more.camera_ids) && decision.need_more.camera_ids.length > 0
                  ? " for " + decision.need_more.camera_ids.map(camLabel).join(", ") : ""}
              </p>
            )}
            {Array.isArray(decision.triggered_actions) && decision.triggered_actions.length > 0 && (
              <div className="cam-round-triggers">
                {decision.triggered_actions.map((t, i) => (
                  <Badge key={i} tone={t.severity === "critical" ? "danger" : t.severity === "warning" ? "warning" : "info"}>
                    {t.action}: {t.reason}
                  </Badge>
                ))}
              </div>
            )}
          </div>
        )}

        {round.error && <div className="alert" data-tone="danger"><Icon name="error" size={13} /><span>{round.error}</span></div>}

        {round.raw_response && <CodeBlock code={round.raw_response} title="Raw model response" collapsible defaultCollapsed />}
      </div>
    );
  }

  function RunDetail({ run, camerasById }) {
    if (!run) {
      return (
        <Card title="Run detail">
          <div className="cam-run-loading"><Spinner size={18} /><span className="muted">Loading run…</span></div>
        </Card>
      );
    }
    const transcript = run.transcript || {};
    const rounds = transcript.rounds || [];
    const camLabel = (id) => (camerasById[id] ? camerasById[id].name : id);

    return (
      <Card
        title="Run detail"
        description={`${transcript.backend || "—"} via ${transcript.alias || "default"} · ${rounds.length} round(s)`}
        actions={<><SuspicionBadge value={run.suspicion} /><RuleStatusBadge value={run.rule_status} /></>}
      >
        {run.status === "running" && (
          <div className="cam-run-loading"><Spinner size={16} /><span className="muted">Run in progress — the transcript appears once it finishes.</span></div>
        )}
        {run.error && <div className="alert" data-tone="danger"><Icon name="error" size={13} /><span>{run.error}</span></div>}
        <div className="cam-rounds">
          {rounds.map((round, i) => <RoundView key={i} round={round} camLabel={camLabel} />)}
        </div>
        {run.fired_action && (
          <div className="cam-fired">
            <h4>Fired action: <Badge tone="warning">{run.fired_action}</Badge></h4>
            <CodeBlock code={run.action_result} mode="json" title="Action result" collapsible defaultCollapsed />
          </div>
        )}
      </Card>
    );
  }

  function RunHistoryTab({ watches, selectedWatchId, setSelectedWatchId, runs, selectedRunId, setSelectedRunId, runDetail, camerasById }) {
    const watchOptions = watches.map((w) => ({ value: w.id, label: w.name || "Untitled watch" }));

    return (
      <div className="cam-tab-body">
        <Card
          title="Run history"
          description="Every scheduled or manual run, replayable round by round."
          actions={watches.length > 0 && (
            <Select
              options={watchOptions}
              value={selectedWatchId}
              onChange={(e) => { setSelectedWatchId(e.target.value); setSelectedRunId(""); }}
            />
          )}
          flush
        >
          {watches.length === 0 ? (
            <EmptyState icon="activity" title="No watches yet" description="Create a watch first to see its run history here." compact />
          ) : runs.length === 0 ? (
            <EmptyState icon="clock" title="No runs yet" description="Runs appear here once the watch fires (on schedule or via Run now)." compact />
          ) : (
            <div className="table-wrap">
              <table className="table">
                <thead>
                  <tr><th>Started</th><th>Status</th><th>Suspicion</th><th>Rule</th><th>Rounds</th><th>Summary</th><th>Fired</th></tr>
                </thead>
                <tbody>
                  {runs.map((r) => (
                    <tr
                      key={r.id}
                      className={"cam-run-row" + (r.id === selectedRunId ? " active" : "")}
                      onClick={() => setSelectedRunId(r.id)}
                    >
                      <td className="mono">{ago(r.started_at)}</td>
                      <td><Badge tone={r.status === "error" ? "danger" : r.status === "running" ? "info" : "success"} pulse={r.status === "running"}>{r.status}</Badge></td>
                      <td><SuspicionBadge value={r.suspicion} /></td>
                      <td><RuleStatusBadge value={r.rule_status} /></td>
                      <td className="tnum">{r.rounds}</td>
                      <td className="truncate cam-run-summary">{r.summary || "—"}</td>
                      <td>{r.fired_action ? <Badge tone="warning">{r.fired_action}</Badge> : <span className="subtle">—</span>}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}
        </Card>

        {selectedRunId && <RunDetail run={runDetail} camerasById={camerasById} />}
      </div>
    );
  }

  /* ─────────────────────────────────────────────────────────────
     DIAGNOSTICS — health surface, events trail, re-run capture (WP13)
     ───────────────────────────────────────────────────────────── */

  const DVR_STATUS_TONE = { ok: "success", failing: "danger", unknown: "neutral" };
  function DVRStatusBadge({ status }) {
    return <Badge tone={DVR_STATUS_TONE[status] || "neutral"}>{status || "unknown"}</Badge>;
  }

  function HealthCard({ health }) {
    if (!health) {
      return (
        <Card title="System health">
          <div className="cam-run-loading"><Spinner size={16} /><span className="muted">Loading…</span></div>
        </Card>
      );
    }
    const ffmpeg = health.ffmpeg || {};
    const ffprobe = health.ffprobe || {};
    const dvrs = health.dvrs || [];
    return (
      <Card
        title="System health"
        description="Live ffmpeg/ffprobe presence and per-DVR reachability, derived from the diagnostics trail."
        actions={<Badge tone={health.enabled ? "success" : "neutral"}>{health.enabled ? "Scheduler on" : "Scheduler off"}</Badge>}
        flush
      >
        <div className="cam-health-bins">
          <div className="cam-health-bin">
            <span className="cam-health-bin-label">ffmpeg</span>
            <Badge tone={ffmpeg.ok ? "success" : "danger"}>{ffmpeg.ok ? "found" : "missing"}</Badge>
            <span className="mono muted subtle">{ffmpeg.version || ffmpeg.bin || "—"}</span>
          </div>
          <div className="cam-health-bin">
            <span className="cam-health-bin-label">ffprobe</span>
            <Badge tone={ffprobe.ok ? "success" : "danger"}>{ffprobe.ok ? "found" : "missing"}</Badge>
            <span className="mono muted subtle">{ffprobe.version || ffprobe.bin || "—"}</span>
          </div>
        </div>

        {dvrs.length === 0 ? (
          <EmptyState icon="server" title="No DVRs yet" description="Add a DVR to see its reachability here." compact />
        ) : (
          <div className="table-wrap">
            <table className="table">
              <thead>
                <tr><th>DVR</th><th>Brand</th><th>Host</th><th>Status</th><th>Last attempt</th><th>Last success</th><th>Last error</th></tr>
              </thead>
              <tbody>
                {dvrs.map((d) => (
                  <tr key={d.dvr_id}>
                    <td>{d.name || "—"}</td>
                    <td><Badge tone="neutral">{d.brand || "auto"}</Badge></td>
                    <td className="mono">{d.host}</td>
                    <td><DVRStatusBadge status={d.status} /></td>
                    <td className="muted">{d.last_attempt_at ? ago(d.last_attempt_at) : "—"}</td>
                    <td className="muted">{d.last_success_at ? ago(d.last_success_at) : "never"}</td>
                    <td className="truncate cam-run-summary">{d.last_error || "—"}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </Card>
    );
  }

  function EventDetailModal({ event, onClose }) {
    return (
      <Modal
        open={!!event}
        onClose={onClose}
        title={event ? `Event: ${event.op}` : "Event"}
        width={640}
        footer={<div className="cam-modal-footer"><Button variant="secondary" onClick={onClose}>Close</Button></div>}
      >
        {event && (
          <>
            <div className="cam-event-meta">
              <Badge tone={event.ok ? "success" : "danger"}>{event.ok ? "ok" : "failed"}</Badge>
              <span className="muted mono">{event.ts}</span>
              {event.latency_ms > 0 && <span className="muted subtle mono">{fmtMs(event.latency_ms)}</span>}
            </div>
            {event.error && <div className="alert" data-tone="danger"><Icon name="error" size={13} /><span>{event.error}</span></div>}
            {event.detail != null && <CodeBlock code={JSON.stringify(event.detail, null, 2)} mode="json" title="Detail" />}
          </>
        )}
      </Modal>
    );
  }

  // EventsCard is a filterable view over the camera_events trail (the plan's
  // "recent failures" diagnostics panel) — ok/dvr/op filters map directly onto
  // handleCameraEvents' query params (camera_diag.go).
  function EventsCard({ dvrs }) {
    const [okFilter, setOkFilter] = useState("failures"); // "all" | "failures" | "ok"
    const [dvrFilter, setDvrFilter] = useState("");
    const [opFilter, setOpFilter] = useState("");
    const [detailEvent, setDetailEvent] = useState(null);

    const dvrNameById = useMemo(() => {
      const m = {};
      dvrs.forEach((d) => { m[d.dvr_id] = d.name || d.host; });
      return m;
    }, [dvrs]);

    const qs = useMemo(() => {
      const p = new URLSearchParams();
      p.set("limit", "100");
      if (okFilter === "failures") p.set("ok", "false");
      else if (okFilter === "ok") p.set("ok", "true");
      if (dvrFilter) p.set("dvr_id", dvrFilter);
      if (opFilter.trim()) p.set("op", opFilter.trim());
      return p.toString();
    }, [okFilter, dvrFilter, opFilter]);

    const eventsLive = useLive(`/ui/api/cameras/events?${qs}`, { interval: 8000 });
    const events = (eventsLive.data && eventsLive.data.events) || [];
    const dvrOptions = useMemo(() => ([
      { value: "", label: "All DVRs" },
      ...dvrs.map((d) => ({ value: d.dvr_id, label: d.name || d.host })),
    ]), [dvrs]);

    return (
      <Card
        title="Recent events"
        description="The camera_events diagnostics trail — every discovery, capture, analysis round, scheduler tick, and action dispatch."
        actions={
          <div className="cam-event-filters">
            <Select
              options={[{ value: "failures", label: "Failures only" }, { value: "all", label: "All" }, { value: "ok", label: "OK only" }]}
              value={okFilter}
              onChange={(e) => setOkFilter(e.target.value)}
            />
            <Select options={dvrOptions} value={dvrFilter} onChange={(e) => setDvrFilter(e.target.value)} />
            <Input mono value={opFilter} onChange={(e) => setOpFilter(e.target.value)} placeholder="op (e.g. capture_snapshot)" />
            <IconButton icon="refresh" label="Refresh" onClick={eventsLive.refresh} />
          </div>
        }
        flush
      >
        {events.length === 0 ? (
          <EmptyState icon="activity" title="No events" description="Nothing matches this filter yet." compact />
        ) : (
          <div className="table-wrap">
            <table className="table">
              <thead>
                <tr><th>Time</th><th>Op</th><th>Status</th><th>DVR</th><th>Latency</th><th>Error</th><th></th></tr>
              </thead>
              <tbody>
                {events.map((e) => (
                  <tr key={e.id} className="cam-run-row" onClick={() => setDetailEvent(e)}>
                    <td className="mono">{ago(e.ts)}</td>
                    <td className="mono">{e.op}</td>
                    <td><Badge tone={e.ok ? "success" : "danger"}>{e.ok ? "ok" : "failed"}</Badge></td>
                    <td className="muted">{e.dvr_id ? (dvrNameById[e.dvr_id] || e.dvr_id) : "—"}</td>
                    <td className="tnum muted">{e.latency_ms > 0 ? fmtMs(e.latency_ms) : "—"}</td>
                    <td className="truncate cam-run-summary">{e.error || "—"}</td>
                    <td className="cam-row-actions">
                      <Button size="sm" variant="ghost" icon="external" onClick={(ev) => { ev.stopPropagation(); setDetailEvent(e); }}>View</Button>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
        <EventDetailModal event={detailEvent} onClose={() => setDetailEvent(null)} />
      </Card>
    );
  }

  // RecaptureForm is the "reproduce a failure live" diagnostics primitive —
  // it posts to the SAME capture functions the dashboard's Live/Clip buttons
  // use (handleCameraRecapture, camera_diag.go) and renders the exact masked
  // command + full ffmpeg stderr the plan requires.
  function RecaptureForm({ cameras, notify }) {
    const [cameraId, setCameraId] = useState("");
    const [kind, setKind] = useState("snapshot");
    const [quality, setQuality] = useState("main");
    const [clipMode, setClipMode] = useState("live"); // "live" | "playback"
    const [seconds, setSeconds] = useState(15);
    const [from, setFrom] = useState("");
    const [to, setTo] = useState("");
    const [busy, setBusy] = useState(false);
    const [result, setResult] = useState(null);

    useEffect(() => {
      if (!cameraId && cameras.length > 0) setCameraId(cameras[0].id);
    }, [cameras, cameraId]);

    const cameraOptions = useMemo(
      () => cameras.map((c) => ({ value: c.id, label: c.name + (c.area ? " · " + c.area : "") })),
      [cameras]
    );

    const run = useCallback(async () => {
      if (!cameraId) { notify("Pick a camera first", "error"); return; }
      if (kind === "clip" && clipMode === "playback" && (!from || !to)) {
        notify("From and To are required for a playback clip", "error");
        return;
      }
      setBusy(true); setResult(null);
      try {
        const body = { camera_id: cameraId, kind, quality };
        if (kind === "clip") {
          if (clipMode === "live") {
            body.seconds = Number(seconds) || 15;
          } else {
            body.from = new Date(from).toISOString();
            body.to = new Date(to).toISOString();
          }
        }
        const res = await api.post("/ui/api/cameras/recapture", body, { timeoutMs: 45000 });
        setResult(res);
        if (!res || res.ok === false) notify((res && res.error) || "Re-run failed", "error");
        else notify("Re-run succeeded", "success");
      } catch (e) {
        notify("Re-run failed: " + ((e && e.message) || e), "error");
      } finally { setBusy(false); }
    }, [cameraId, kind, quality, clipMode, seconds, from, to, notify]);

    return (
      <Card
        title="Re-run a single capture"
        description="Reproduce a failure live — runs the exact capture path the dashboard uses and shows the masked command and full ffmpeg stderr."
      >
        <div className="cam-form-grid-2">
          <Field label="Camera">
            <Select
              options={cameraOptions.length ? cameraOptions : [{ value: "", label: "No cameras on this site" }]}
              value={cameraId}
              onChange={(e) => setCameraId(e.target.value)}
              disabled={busy || cameraOptions.length === 0}
            />
          </Field>
          <Field label="Kind">
            <Select options={[{ value: "snapshot", label: "Snapshot" }, { value: "clip", label: "Clip" }]} value={kind} onChange={(e) => setKind(e.target.value)} disabled={busy} />
          </Field>
          <Field label="Quality">
            <Select options={[{ value: "main", label: "Main" }, { value: "sub", label: "Sub" }]} value={quality} onChange={(e) => setQuality(e.target.value)} disabled={busy} />
          </Field>
          {kind === "clip" && (
            <Field label="Window">
              <Select
                options={[{ value: "live", label: "Live (last N seconds)" }, { value: "playback", label: "Past playback" }]}
                value={clipMode}
                onChange={(e) => setClipMode(e.target.value)}
                disabled={busy}
              />
            </Field>
          )}
        </div>
        {kind === "clip" && clipMode === "live" && (
          <Field label="Seconds"><Input type="number" min={1} max={300} mono value={seconds} onChange={(e) => setSeconds(e.target.value)} disabled={busy} /></Field>
        )}
        {kind === "clip" && clipMode === "playback" && (
          <div className="cam-form-grid-2">
            <Field label="From"><Input type="datetime-local" mono value={from} onChange={(e) => setFrom(e.target.value)} disabled={busy} /></Field>
            <Field label="To"><Input type="datetime-local" mono value={to} onChange={(e) => setTo(e.target.value)} disabled={busy} /></Field>
          </div>
        )}
        <div className="cam-wizard-actions">
          <Button variant="primary" icon={busy ? undefined : "bolt"} loading={busy} disabled={busy || !cameraId} onClick={run}>
            {busy ? "Running…" : "Re-run capture"}
          </Button>
        </div>

        {result && (
          <div className="cam-recapture-result">
            <div className="cam-event-meta">
              <Badge tone={result.ok ? "success" : "danger"}>{result.ok ? "success" : "failed"}</Badge>
              {result.error && <span className="cam-hint-fail">{result.error}</span>}
            </div>
            {result.ok && result.url && (
              kind === "clip"
                ? <video className="cam-round-clip" controls src={result.url} />
                : <img className="cam-recapture-thumb" src={result.url} alt="capture" />
            )}
            {result.diagnostic && (result.diagnostic.cmd || result.diagnostic.stderr) && (
              <>
                {result.diagnostic.cmd && <CodeBlock code={result.diagnostic.cmd} mode="shell" title="Masked command" collapsible />}
                {result.diagnostic.stderr && (
                  <CodeBlock
                    code={result.diagnostic.stderr}
                    title={"ffmpeg stderr" + (result.diagnostic.exit_code ? ` (exit ${result.diagnostic.exit_code})` : "") + (result.diagnostic.timed_out ? " — timed out" : "")}
                    collapsible
                  />
                )}
              </>
            )}
          </div>
        )}
      </Card>
    );
  }

  function DiagnosticsTab({ cameras, notify }) {
    const healthLive = useLive("/ui/api/cameras/health", { interval: 10000 });
    const health = healthLive.data;
    const dvrs = (health && health.dvrs) || [];

    return (
      <div className="cam-tab-body">
        <HealthCard health={health} />
        <EventsCard dvrs={dvrs} />
        <RecaptureForm cameras={cameras} notify={notify} />
      </div>
    );
  }

  /* ─────────────────────────────────────────────────────────────
     ASK AI — investigation chat (WP2)

     Talks to camera_investigate.go's four endpoints:
       POST /ui/api/cameras/investigations {site_id, question} -> {ok, id, status, investigation, messages}
       POST /ui/api/cameras/investigations/reply {id, message}  -> same shape
       GET  /ui/api/cameras/investigations?site_id=             -> {investigations: [...]}
       GET  /ui/api/cameras/investigations/get?id=              -> {investigation, messages}
     Runs are synchronous per request (the loop drives to its next
     ask_operator/answer pause before responding), so Ask/Reply use a long
     timeoutMs; the transcript still polls via useLive so a second tab (or a
     slow/retried request) converges on the same state.
     ───────────────────────────────────────────────────────────── */

  const INVESTIGATION_STATUS_TONE = { queued: "info", running: "info", active: "info", awaiting_operator: "warning", answered: "success", exhausted: "warning", closed: "neutral" };
  const INVESTIGATION_STATUS_LABEL = { queued: "queued", running: "working" };
  function InvestigationStatusBadge({ status }) {
    const working = status === "queued" || status === "running" || status === "active";
    return (
      <Badge tone={INVESTIGATION_STATUS_TONE[status] || "neutral"} pulse={working}>
        {(INVESTIGATION_STATUS_LABEL[status] || status || "unknown").replace(/_/g, " ")}
      </Badge>
    );
  }

  // EvidenceMedia renders one cited {media_url, caption} — evidenceItem carries
  // no explicit kind and /camera/media/<token> URLs have no file extension, so
  // this tries an <img> first and falls back to a <video> if the browser can't
  // decode it as an image (a past_clip citation). When onAttach is provided the
  // tile grows an "Attach to playbook…" action (design B4 — the interactive
  // playbook-authoring loop).
  function EvidenceMedia({ url, caption, onAttach }) {
    const [isVideo, setIsVideo] = useState(false);
    if (!url) return null;
    return (
      <div className="cam-evidence-item">
        {isVideo ? (
          <video controls src={url} />
        ) : (
          <img src={url} alt={caption || "evidence"} onError={() => setIsVideo(true)} />
        )}
        {caption && <span className="cam-evidence-caption" title={caption}>{caption}</span>}
        {onAttach && (
          <Button size="sm" variant="ghost" icon="pin" onClick={() => onAttach(url)}>Attach to playbook…</Button>
        )}
      </div>
    );
  }

  function EvidenceGrid({ media, onAttach }) {
    if (!Array.isArray(media) || media.length === 0) return null;
    return (
      <div className="cam-evidence-grid">
        {media.map((m, i) => <EvidenceMedia key={i} url={m.media_url} caption={m.caption} onAttach={onAttach} />)}
      </div>
    );
  }

  // tokenFromMediaURL extracts the camera_captures token from a served media
  // URL ("/camera/media/<token>", relative or absolute) — the playbook media
  // attach endpoint wants the bare token, not the URL.
  function tokenFromMediaURL(url) {
    if (!url) return "";
    const path = String(url).split(/[?#]/)[0];
    const segs = path.split("/").filter(Boolean);
    return segs.length > 0 ? segs[segs.length - 1] : "";
  }

  // AttachToPlaybookModal turns an investigation evidence tile into a pinned
  // playbook reference image: pick a playbook, annotate it, POST the token.
  function AttachToPlaybookModal({ open, url, siteId, notify, onClose }) {
    const [playbooks, setPlaybooks] = useState([]);
    const [loading, setLoading] = useState(false);
    const [playbookId, setPlaybookId] = useState("");
    const [note, setNote] = useState("");
    const [saving, setSaving] = useState(false);

    useEffect(() => {
      if (!open) return;
      setNote(""); setSaving(false); setLoading(true);
      api.get(`/ui/api/cameras/playbooks?site_id=${encodeURIComponent(siteId)}`)
        .then((res) => {
          const list = (res && res.playbooks) || [];
          setPlaybooks(list);
          setPlaybookId(list.length > 0 ? list[0].id : "");
        })
        .catch((e) => notify("Failed to load playbooks: " + ((e && e.message) || e), "error"))
        .finally(() => setLoading(false));
    }, [open, siteId]);

    const attach = useCallback(async () => {
      const token = tokenFromMediaURL(url);
      if (!playbookId) { notify("Pick a playbook first", "error"); return; }
      if (!token) { notify("Could not extract a media token from this evidence URL", "error"); return; }
      setSaving(true);
      try {
        const res = await api.post("/ui/api/cameras/playbooks/media", { playbook_id: playbookId, token, note });
        if (!res || res.ok === false) { notify((res && res.error) || "Attach failed", "error"); return; }
        notify("Reference image attached to playbook", "success");
        if (res.warning) notify(res.warning, "info");
        onClose();
      } catch (e) {
        notify("Attach failed: " + ((e && e.message) || e), "error");
      } finally { setSaving(false); }
    }, [url, playbookId, note, notify, onClose]);

    return (
      <Modal
        open={open}
        onClose={onClose}
        title="Attach to playbook"
        description="Pins this evidence frame permanently as an operator-approved reference image for the playbook."
        width={480}
        footer={
          <div className="cam-modal-footer">
            <Button variant="secondary" onClick={onClose} disabled={saving}>Cancel</Button>
            <Button variant="primary" icon="pin" loading={saving} disabled={saving || loading || !playbookId} onClick={attach}>Attach</Button>
          </div>
        }
      >
        {loading ? (
          <div className="cam-run-loading"><Spinner size={16} /><span className="muted">Loading playbooks…</span></div>
        ) : playbooks.length === 0 ? (
          <EmptyState icon="book" title="No playbooks yet" description="Create a playbook in the Knowledge tab first, then attach evidence to it." compact />
        ) : (
          <>
            {url && <img className="cam-recapture-thumb" src={url} alt="evidence" />}
            <Field label="Playbook">
              <Select
                options={playbooks.map((p) => ({ value: p.id, label: p.name }))}
                value={playbookId}
                onChange={(e) => setPlaybookId(e.target.value)}
                disabled={saving}
              />
            </Field>
            <Field label="Note" hint='What this image shows, e.g. "correct standby position — handle UP".'>
              <Textarea rows={2} value={note} onChange={(e) => setNote(e.target.value)} disabled={saving} />
            </Field>
          </>
        )}
      </Modal>
    );
  }

  // formatToolArgs renders an investigateArgs object (camera_ids/quality/from/to/
  // count/mode/name/params/avatar_id/time, decoded server-side from JSON) as a
  // short mono summary line — empty fields omitted.
  function formatToolArgs(args) {
    if (!args || typeof args !== "object") return "";
    const parts = [];
    if (Array.isArray(args.camera_ids) && args.camera_ids.length > 0) parts.push("cameras: " + args.camera_ids.join(", "));
    if (args.quality) parts.push("quality: " + args.quality);
    if (args.from || args.to) parts.push("window: " + (args.from || "?") + " → " + (args.to || "?"));
    if (args.count) parts.push("count: " + args.count);
    if (args.mode) parts.push("mode: " + args.mode);
    if (args.name) parts.push("name: " + args.name);
    if (args.avatar_id) parts.push("avatar: " + args.avatar_id);
    if (args.time) parts.push("time: " + args.time);
    if (args.params && typeof args.params === "object" && !Array.isArray(args.params)) {
      const kv = Object.keys(args.params).map((k) => k + "=" + String(args.params[k])).join(" ");
      if (kv) parts.push("params: " + kv);
    }
    return parts.join(" · ");
  }

  const INVESTIGATE_TOOL_LABEL = {
    roster: "Roster", snapshot: "Snapshot", mosaic: "Mosaic", past_frames: "Past frames", past_clip: "Past clip",
    contact_sheet: "Contact sheet", playbook: "Playbook", call_api: "API call",
    avatars: "Avatars", avatar_info: "Avatar info", avatar_check: "Avatar check", avatar_find: "Avatar find",
    annotate: "Annotate",
  };

  // InvestigateMessage renders one camera_investigation_messages row as a chat
  // bubble, styled by role — operator (the human), ai (a thought + either a
  // tool call, an ask_operator question, or the final answer + evidence), tool
  // (the loop's own result summary + any media it fetched), system (a bound/
  // error notice from camStopInvestigation).
  function InvestigateMessage({ m, onAttach }) {
    if (m.role === "operator") {
      return (
        <div className="cam-msg cam-msg-operator">
          <div className="cam-msg-bubble">
            <span className="cam-msg-role">Operator</span>
            <p>{m.content}</p>
          </div>
        </div>
      );
    }
    if (m.role === "system") {
      return (
        <div className="cam-msg cam-msg-system">
          <Icon name="info" size={12} /><span>{m.content}</span>
        </div>
      );
    }
    if (m.role === "tool") {
      const args = formatToolArgs(m.tool_args);
      return (
        <div className="cam-msg cam-msg-tool">
          <div className="cam-msg-bubble">
            <span className="cam-msg-role">
              <Icon name="bolt" size={12} /> {INVESTIGATE_TOOL_LABEL[m.tool_name] || m.tool_name || "tool"} result
            </span>
            <p>{m.content}</p>
            {args && <span className="cam-msg-args mono muted subtle">{args}</span>}
            <EvidenceGrid media={m.media} onAttach={onAttach} />
          </div>
        </div>
      );
    }
    // role === "ai"
    const isAnswer = m.tool_name === "answer";
    const isQuestion = m.tool_name === "ask_operator";
    const args = formatToolArgs(m.tool_args);
    return (
      <div className="cam-msg cam-msg-ai">
        <div className="cam-msg-bubble">
          <span className="cam-msg-role">
            <Icon name="shield" size={12} /> AI
            {isAnswer && <Badge tone="success">answer</Badge>}
            {isQuestion && <Badge tone="warning">question</Badge>}
            {!isAnswer && !isQuestion && m.tool_name && <Badge tone="neutral">{INVESTIGATE_TOOL_LABEL[m.tool_name] || m.tool_name}</Badge>}
          </span>
          <p>{m.content}</p>
          {args && <span className="cam-msg-args mono muted subtle">{args}</span>}
          {isAnswer && <EvidenceGrid media={m.media} onAttach={onAttach} />}
        </div>
      </div>
    );
  }

  // InvestigationThread renders one investigation's transcript + (unless
  // closed) a reply box — always available, not just on awaiting_operator,
  // since an "answered" OR "exhausted" (bound-terminated) investigation accepts
  // follow-ups: the server reopens it to "active" and, because a non-ask_operator
  // operator message starts a FRESH run, the follow-up gets a fresh budget and
  // actually advances (per handleCameraInvestigationReply / camCountInvestigateProgress).
  function InvestigationThread({ investigation, messages, notify, onReplied, onAttach }) {
    const [replyText, setReplyText] = useState("");
    const [replying, setReplying] = useState(false);
    const invId = investigation.id;

    useEffect(() => { setReplyText(""); }, [invId]);

    const isAsk = investigation.status === "awaiting_operator";
    const working = investigation.status === "queued" || investigation.status === "running";
    const canReply = investigation.status !== "closed" && !working;

    const send = useCallback(async () => {
      const msg = replyText.trim();
      if (!msg) { notify("Type a message first", "error"); return; }
      setReplying(true);
      try {
        const res = await api.post("/ui/api/cameras/investigations/reply", { id: invId, message: msg }, { timeoutMs: 400000 });
        if (!res || res.ok === false) { notify((res && res.error) || "Failed to send", "error"); return; }
        setReplyText("");
        onReplied();
      } catch (e) {
        notify("Failed to send: " + ((e && e.message) || e), "error");
      } finally { setReplying(false); }
    }, [replyText, invId, notify, onReplied]);

    return (
      <div className="cam-investigate-thread">
        <div className="cam-investigate-thread-head">
          <h4 className="truncate">{investigation.title || investigation.question}</h4>
          <InvestigationStatusBadge status={investigation.status} />
        </div>

        <div className="cam-msg-list">
          {messages.length === 0 ? (
            <div className="cam-run-loading"><Spinner size={16} /><span className="muted">Investigating…</span></div>
          ) : messages.map((m) => <InvestigateMessage key={m.id} m={m} onAttach={onAttach} />)}
        </div>

        {working && (
          <div className="cam-investigate-reply">
            <div className="alert" data-tone="info"><Spinner size={13} /><span>The AI is working in the background — safe to leave or refresh this page; progress keeps updating here.</span></div>
          </div>
        )}
        {canReply && (
          <div className="cam-investigate-reply">
            {isAsk && (
              <div className="alert" data-tone="warning"><Icon name="info" size={13} /><span>The AI is waiting for your reply.</span></div>
            )}
            <Textarea
              rows={2}
              value={replyText}
              onChange={(e) => setReplyText(e.target.value)}
              placeholder={isAsk ? "Answer the AI's question…" : "Ask a follow-up…"}
              disabled={replying}
            />
            <Button variant="primary" icon={replying ? undefined : "send"} loading={replying} disabled={replying || !replyText.trim()} onClick={send}>
              {isAsk ? "Reply" : "Follow up"}
            </Button>
          </div>
        )}
      </div>
    );
  }

  function InvestigateTab({ site, investigations, listRefresh, notify }) {
    const [question, setQuestion] = useState("");
    const [asking, setAsking] = useState(false);
    const [selectedId, setSelectedId] = useState("");
    const [attachUrl, setAttachUrl] = useState("");

    useEffect(() => { setSelectedId(""); setQuestion(""); setAttachUrl(""); }, [site.id]);

    // Polls the selected investigation's full transcript regardless of status —
    // mirrors RunHistoryTab's runDetailLive — so a reply sent from another tab,
    // or a request that outlived the client's own timeout, still shows up here.
    const detailLive = useLive(
      selectedId ? `/ui/api/cameras/investigations/get?id=${encodeURIComponent(selectedId)}` : "",
      { interval: 4000, enabled: !!selectedId }
    );
    const investigation = detailLive.data && detailLive.data.investigation;
    const messages = (detailLive.data && detailLive.data.messages) || [];

    const ask = useCallback(async () => {
      const q = question.trim();
      if (!q) { notify("Type a question first", "error"); return; }
      setAsking(true);
      try {
        const res = await api.post("/ui/api/cameras/investigations", { site_id: site.id, question: q }, { timeoutMs: 400000 });
        if (!res || res.ok === false) { notify((res && res.error) || "Failed to start investigation", "error"); return; }
        setQuestion("");
        setSelectedId(res.id);
        listRefresh();
        notify(
          res.status === "answered" ? "Investigation answered"
            : res.status === "awaiting_operator" ? "The AI needs your input"
            : res.status === "exhausted" ? "Investigation stopped at a budget limit — send a follow-up to continue"
            : "Investigation started — working in the background",
          "success"
        );
      } catch (e) {
        notify("Failed to start investigation: " + ((e && e.message) || e), "error");
      } finally { setAsking(false); }
    }, [question, site, notify, listRefresh]);

    const onReplied = useCallback(() => { detailLive.refresh(); listRefresh(); }, [detailLive, listRefresh]);

    return (
      <div className="cam-tab-body">
        <Card title="Ask AI" description="Ask a freeform question about this site — the AI investigates across cameras and time, citing evidence.">
          <div className="cam-investigate-ask">
            <Textarea
              rows={2}
              value={question}
              onChange={(e) => setQuestion(e.target.value)}
              placeholder="What happened at reception around 15:00?"
              disabled={asking}
            />
            <Button variant="primary" icon={asking ? undefined : "search"} loading={asking} disabled={asking || !question.trim()} onClick={ask}>
              {asking ? "Investigating…" : "Ask"}
            </Button>
          </div>
        </Card>

        <div className="cam-investigate-layout">
          <Card title="History" flush className="cam-investigate-history">
            {investigations.length === 0 ? (
              <EmptyState icon="search" title="No investigations yet" description="Ask a question above to start one." compact />
            ) : (
              <div className="cam-investigate-list">
                {investigations.map((inv) => (
                  <button
                    key={inv.id}
                    type="button"
                    className={"cam-investigate-list-item" + (inv.id === selectedId ? " active" : "")}
                    onClick={() => setSelectedId(inv.id)}
                  >
                    <span className="truncate">{inv.title || inv.question}</span>
                    <InvestigationStatusBadge status={inv.status} />
                    <span className="muted subtle">{ago(inv.updated_at)}</span>
                  </button>
                ))}
              </div>
            )}
          </Card>

          <Card title="Transcript" className="cam-investigate-transcript">
            {!selectedId ? (
              <EmptyState icon="info" title="Select an investigation" description="Pick one from the history, or ask a new question above." compact />
            ) : !investigation ? (
              <div className="cam-run-loading"><Spinner size={16} /><span className="muted">Loading…</span></div>
            ) : (
              <InvestigationThread investigation={investigation} messages={messages} notify={notify} onReplied={onReplied} onAttach={setAttachUrl} />
            )}
          </Card>
        </div>

        <AttachToPlaybookModal
          open={!!attachUrl}
          url={attachUrl}
          siteId={site.id}
          notify={notify}
          onClose={() => setAttachUrl("")}
        />
      </div>
    );
  }

  /* ─────────────────────────────────────────────────────────────
     KNOWLEDGE — playbook library + external API tools (designs B5/C4)

     Playbooks: /ui/api/cameras/playbooks[…] — operator-authored procedures
     the AI fetches by name, plus pinned reference images attached from
     investigation evidence. API tools: /ui/api/cameras/apitools[…] —
     operator-configured HTTP lookups ({{param}} templates, Bearer secret
     never echoed, has_secret only) with a live test drawer.
     ───────────────────────────────────────────────────────────── */

  // PlaybookMediaStrip shows a playbook's pinned reference images (thumbnail +
  // note + per-item delete). Attaching happens from investigation evidence
  // tiles ("Attach to playbook…"), not here.
  function PlaybookMediaStrip({ playbookId, notify }) {
    const [media, setMedia] = useState(null); // null = loading

    const load = useCallback(() => {
      api.get(`/ui/api/cameras/playbooks/media?playbook_id=${encodeURIComponent(playbookId)}`)
        .then((res) => setMedia((res && res.media) || []))
        .catch((e) => { setMedia([]); notify("Failed to load reference images: " + ((e && e.message) || e), "error"); });
    }, [playbookId, notify]);

    useEffect(() => { setMedia(null); load(); }, [playbookId]);

    const remove = async (id) => {
      try {
        const res = await api.post("/ui/api/cameras/playbooks/media/delete", { id });
        if (!res || res.ok === false) { notify((res && res.error) || "Delete failed", "error"); return; }
        notify("Reference removed", "success");
        load();
      } catch (e) {
        notify("Delete failed: " + ((e && e.message) || e), "error");
      }
    };

    if (media === null) {
      return <div className="cam-run-loading"><Spinner size={14} /><span className="muted">Loading references…</span></div>;
    }
    if (media.length === 0) {
      return <p className="cfg-hint">No reference images yet — run a setup investigation and use "Attach to playbook…" on any evidence tile.</p>;
    }
    return (
      <div className="cam-evidence-grid">
        {media.map((m) => (
          <div key={m.id} className="cam-evidence-item">
            <img src={m.url || mediaURL(m.capture_token)} alt={m.note || "reference"} />
            {m.note && <span className="cam-evidence-caption" title={m.note}>{m.note}</span>}
            <Button size="sm" variant="ghost" icon="trash" onClick={() => remove(m.id)}>Remove</Button>
          </div>
        ))}
      </div>
    );
  }

  function PlaybookFormModal({ open, playbook, siteId, dvrs, notify, onClose, onSaved }) {
    const [form, setForm] = useState({ name: "", when_to_use: "", instructions: "", dvr_id: "", enabled: true });
    const [saving, setSaving] = useState(false);

    useEffect(() => {
      if (!open) return;
      setForm(playbook ? {
        name: playbook.name || "", when_to_use: playbook.when_to_use || "",
        instructions: playbook.instructions || "", dvr_id: playbook.dvr_id || "",
        enabled: playbook.enabled !== false,
      } : { name: "", when_to_use: "", instructions: "", dvr_id: "", enabled: true });
    }, [open, playbook]);

    const dvrOptions = [{ value: "", label: "Site-wide" }, ...dvrs.map((d) => ({ value: d.id, label: d.name || d.host }))];

    const save = useCallback(async () => {
      const name = form.name.trim();
      if (!name) { notify("Name is required", "error"); return; }
      setSaving(true);
      try {
        const body = { name, when_to_use: form.when_to_use, instructions: form.instructions, dvr_id: form.dvr_id, enabled: form.enabled };
        let path;
        if (playbook) { path = "/ui/api/cameras/playbooks/update"; body.id = playbook.id; }
        else { path = "/ui/api/cameras/playbooks"; body.site_id = siteId; }
        const res = await api.post(path, body);
        if (!res || res.ok === false) { notify((res && res.error) || "Save failed", "error"); return; }
        notify(playbook ? "Playbook updated" : "Playbook created", "success");
        onSaved();
      } catch (e) {
        notify("Save failed: " + ((e && e.message) || e), "error");
      } finally { setSaving(false); }
    }, [form, playbook, siteId, notify, onSaved]);

    return (
      <Modal
        open={open}
        onClose={onClose}
        title={playbook ? "Edit playbook" : "New playbook"}
        width={640}
        footer={
          <div className="cam-modal-footer">
            <Button variant="secondary" onClick={onClose} disabled={saving}>Cancel</Button>
            <Button variant="primary" icon="check" loading={saving} onClick={save}>{playbook ? "Save" : "Create playbook"}</Button>
          </div>
        }
      >
        <Field label="Name" hint="The exact name the AI calls this playbook by — short and unique, e.g. employee-workday.">
          <Input mono value={form.name} onChange={(e) => setForm((f) => ({ ...f, name: e.target.value }))} placeholder="device-standby-check" disabled={saving} />
        </Field>
        <Field label="When to use" hint="Shown to the AI on every turn — keep it short.">
          <Input value={form.when_to_use} onChange={(e) => setForm((f) => ({ ...f, when_to_use: e.target.value }))} placeholder="In room Y the device handle must point UP when in standby." disabled={saving} />
        </Field>
        <Field label="Instructions" hint="The full step-by-step procedure returned when the AI fetches this playbook.">
          <Textarea rows={8} value={form.instructions} onChange={(e) => setForm((f) => ({ ...f, instructions: e.target.value }))} disabled={saving} />
        </Field>
        <Field label="Scope" hint="Site-wide, or hint that it belongs to one DVR's cameras.">
          <Select options={dvrOptions} value={form.dvr_id} onChange={(e) => setForm((f) => ({ ...f, dvr_id: e.target.value }))} disabled={saving} />
        </Field>
        <Switch checked={form.enabled} onChange={(v) => setForm((f) => ({ ...f, enabled: v }))} label="Enabled" />
        {playbook && (
          <Field label="Reference images" hint="Operator-approved images the AI sees when it fetches this playbook.">
            <PlaybookMediaStrip playbookId={playbook.id} notify={notify} />
          </Field>
        )}
      </Modal>
    );
  }

  // ApiToolTestDrawer runs the real substitution + SSRF-guarded call against a
  // SAVED tool ({id, params} → {ok,status,response}) — an authoring aid.
  function ApiToolTestDrawer({ tool, notify }) {
    const [rows, setRows] = useState([{ k: "", v: "" }]);
    const [busy, setBusy] = useState(false);
    const [result, setResult] = useState(null);

    useEffect(() => { setRows([{ k: "", v: "" }]); setResult(null); }, [tool && tool.id]);

    const setRow = (i, key, val) => setRows((rs) => rs.map((r, j) => (j === i ? { ...r, [key]: val } : r)));
    const addRow = () => setRows((rs) => [...rs, { k: "", v: "" }]);
    const removeRow = (i) => setRows((rs) => (rs.length > 1 ? rs.filter((_, j) => j !== i) : rs));

    const run = async () => {
      const params = {};
      rows.forEach((r) => { if (r.k.trim()) params[r.k.trim()] = r.v; });
      setBusy(true); setResult(null);
      try {
        const res = await api.post("/ui/api/cameras/apitools/test", { id: tool.id, params }, { timeoutMs: 45000 });
        setResult(res);
        if (!res || res.ok === false) notify((res && res.error) || "Test failed", "error");
      } catch (e) {
        notify("Test failed: " + ((e && e.message) || e), "error");
      } finally { setBusy(false); }
    };

    return (
      <div className="cam-recapture-result">
        <p className="cfg-hint">Fill the {"{{param}}"} placeholders and run the real request (secret applied server-side, response truncated).</p>
        {rows.map((r, i) => (
          <div key={i} className="cam-inline-fields">
            <Input mono placeholder="param" value={r.k} onChange={(e) => setRow(i, "k", e.target.value)} disabled={busy} />
            <Input mono placeholder="value" value={r.v} onChange={(e) => setRow(i, "v", e.target.value)} disabled={busy} />
            <IconButton icon="x" label="Remove param" onClick={() => removeRow(i)} />
          </div>
        ))}
        <div className="cam-wizard-actions">
          <Button size="sm" variant="ghost" icon="plus" onClick={addRow} disabled={busy}>Add param</Button>
          <Button size="sm" variant="primary" icon={busy ? undefined : "bolt"} loading={busy} onClick={run}>Run test</Button>
        </div>
        {result && (
          <>
            <div className="cam-event-meta">
              <Badge tone={result.ok ? "success" : "danger"}>{result.ok ? "HTTP " + result.status : "failed"}</Badge>
              {result.error && <span className="cam-hint-fail">{result.error}</span>}
            </div>
            {result.response != null && result.response !== "" && (
              <CodeBlock code={String(result.response)} title="Response (truncated)" collapsible />
            )}
          </>
        )}
      </div>
    );
  }

  function ApiToolFormModal({ open, tool, siteId, notify, onClose, onSaved, initialTestOpen }) {
    const blank = {
      name: "", description: "", method: "GET", url_template: "", headers: "",
      auth_secret: "", clear_secret: false, body_template: "",
      request_instructions: "", response_instructions: "", enabled: true,
    };
    const [form, setForm] = useState(blank);
    const [saving, setSaving] = useState(false);
    const [testOpen, setTestOpen] = useState(false);

    useEffect(() => {
      if (!open) return;
      setTestOpen(!!initialTestOpen);
      setForm(tool ? {
        name: tool.name || "", description: tool.description || "", method: tool.method || "GET",
        url_template: tool.url_template || "", headers: tool.headers_json || tool.headers || "",
        auth_secret: "", clear_secret: false, body_template: tool.body_template || "",
        request_instructions: tool.request_instructions || "", response_instructions: tool.response_instructions || "",
        enabled: tool.enabled !== false,
      } : blank);
    }, [open, tool, initialTestOpen]);

    const set = (k) => (e) => setForm((f) => ({ ...f, [k]: e.target.value }));

    const save = useCallback(async () => {
      const name = form.name.trim();
      if (!name) { notify("Name is required", "error"); return; }
      if (!form.url_template.trim()) { notify("URL template is required", "error"); return; }
      setSaving(true);
      try {
        const body = {
          name, description: form.description, method: form.method,
          url_template: form.url_template.trim(), headers: form.headers,
          auth_secret: form.auth_secret, body_template: form.body_template,
          request_instructions: form.request_instructions, response_instructions: form.response_instructions,
          enabled: form.enabled,
        };
        let path;
        if (tool) {
          path = "/ui/api/cameras/apitools/update";
          body.id = tool.id;
          if (form.clear_secret) body.clear_secret = true;
        } else {
          path = "/ui/api/cameras/apitools";
          body.site_id = siteId;
        }
        const res = await api.post(path, body);
        if (!res || res.ok === false) { notify((res && res.error) || "Save failed", "error"); return; }
        notify(tool ? "API tool updated" : "API tool created", "success");
        onSaved();
      } catch (e) {
        notify("Save failed: " + ((e && e.message) || e), "error");
      } finally { setSaving(false); }
    }, [form, tool, siteId, notify, onSaved]);

    return (
      <Modal
        open={open}
        onClose={onClose}
        title={tool ? "Edit API tool" : "New API tool"}
        width={640}
        footer={
          <div className="cam-modal-footer">
            <Button variant="secondary" onClick={onClose} disabled={saving}>Cancel</Button>
            <Button variant="primary" icon="check" loading={saving} onClick={save}>{tool ? "Save" : "Create API tool"}</Button>
          </div>
        }
      >
        <Field label="Name" hint="The exact name the AI calls this tool by, e.g. attendance-check.">
          <Input mono value={form.name} onChange={set("name")} disabled={saving} />
        </Field>
        <Field label="Description" hint="Shown to the AI in the catalog — what the lookup does.">
          <Textarea rows={2} value={form.description} onChange={set("description")} placeholder="Checks the attendance system for whether an employee is clocked in or on break." disabled={saving} />
        </Field>
        <Field label="Method">
          <Select options={[{ value: "GET", label: "GET" }, { value: "POST", label: "POST" }]} value={form.method} onChange={set("method")} disabled={saving} />
        </Field>
        <Field label="URL template" hint={"Use {{param}} placeholders in path/query only — the scheme and host must be literal."}>
          <Input mono value={form.url_template} onChange={set("url_template")} placeholder="https://api.example.com/attendance?employee={{employee}}" disabled={saving} />
        </Field>
        <Field label="Headers (JSON)" hint='Static headers, e.g. {"X-Api-Key": "abc"}. "Authorization" is reserved for the secret.'>
          <Textarea rows={2} value={form.headers} onChange={set("headers")} placeholder='{"Accept": "application/json"}' disabled={saving} />
        </Field>
        <Field label="Secret" hint="Sent as Authorization: Bearer — leave blank to keep the existing secret; encrypted at rest, never shown again.">
          <PasswordField value={form.auth_secret} onChange={set("auth_secret")} placeholder={tool && tool.has_secret ? "Unchanged" : ""} disabled={saving} />
        </Field>
        {tool && tool.has_secret && (
          <Checkbox
            checked={form.clear_secret}
            onChange={(v) => setForm((f) => ({ ...f, clear_secret: v }))}
            label="Remove the stored secret"
            disabled={saving}
          />
        )}
        {form.method === "POST" && (
          <Field label="Body template" hint={"POST body with {{param}} placeholders (values are JSON-escaped)."}>
            <Textarea rows={3} value={form.body_template} onChange={set("body_template")} placeholder='{"employee": "{{employee}}"}' disabled={saving} />
          </Field>
        )}
        <Field label="Request instructions" hint="Shown to the AI in the catalog — which params it must supply.">
          <Textarea rows={2} value={form.request_instructions} onChange={set("request_instructions")} placeholder="params: employee (the employee id or full name as registered)" disabled={saving} />
        </Field>
        <Field label="Response instructions" hint="Folded into the tool result — how the AI should interpret the response.">
          <Textarea rows={2} value={form.response_instructions} onChange={set("response_instructions")} disabled={saving} />
        </Field>
        <Switch checked={form.enabled} onChange={(v) => setForm((f) => ({ ...f, enabled: v }))} label="Enabled" />
        {tool && (
          <>
            <div className="cam-wizard-actions">
              <Button size="sm" variant="ghost" icon={testOpen ? "chevron-up" : "chevron-down"} onClick={() => setTestOpen((t) => !t)}>
                {testOpen ? "Hide test" : "Test this tool"}
              </Button>
            </div>
            {testOpen && <ApiToolTestDrawer tool={tool} notify={notify} />}
          </>
        )}
      </Modal>
    );
  }

  // toolHost extracts the literal host from a url_template for the table (the
  // host may never contain {{placeholders}}, so this is safe to display).
  function toolHost(t) {
    const m = /^https?:\/\/([^\/?#]+)/i.exec(((t && t.url_template) || "").trim());
    return m ? m[1] : "—";
  }

  function KnowledgeTab({ site, dvrs, notify }) {
    const playbooksLive = useLive(`/ui/api/cameras/playbooks?site_id=${encodeURIComponent(site.id)}`, { interval: 8000, enabled: !!site.id });
    const playbooks = (playbooksLive.data && playbooksLive.data.playbooks) || [];
    const toolsLive = useLive(`/ui/api/cameras/apitools?site_id=${encodeURIComponent(site.id)}`, { interval: 8000, enabled: !!site.id });
    const toolsData = toolsLive.data || {};
    const apiTools = toolsData.tools || toolsData.apitools || toolsData.api_tools || [];

    const dvrNameById = useMemo(() => {
      const m = {};
      dvrs.forEach((d) => { m[d.id] = d.name || d.host; });
      return m;
    }, [dvrs]);

    const [pbFormOpen, setPbFormOpen] = useState(false);
    const [pbEditing, setPbEditing] = useState(null);
    const [pbDeleteId, setPbDeleteId] = useState("");
    const [toolFormOpen, setToolFormOpen] = useState(false);
    const [toolEditing, setToolEditing] = useState(null);
    const [toolTestOpen, setToolTestOpen] = useState(false);
    const [toolDeleteId, setToolDeleteId] = useState("");

    // Toggles resend the row's full editable fields alongside the flipped
    // enabled bit, so they are safe under both pointer-patch and full-replace
    // update semantics server-side.
    const togglePlaybook = async (p) => {
      try {
        const res = await api.post("/ui/api/cameras/playbooks/update", {
          id: p.id, name: p.name, when_to_use: p.when_to_use || "", instructions: p.instructions || "",
          dvr_id: p.dvr_id || "", enabled: !p.enabled,
        });
        if (!res || res.ok === false) { notify((res && res.error) || "Update failed", "error"); return; }
        playbooksLive.refresh();
      } catch (e) {
        notify("Update failed: " + ((e && e.message) || e), "error");
      }
    };

    const deletePlaybook = async () => {
      try {
        const res = await api.post("/ui/api/cameras/playbooks/delete", { id: pbDeleteId });
        if (!res || res.ok === false) { notify((res && res.error) || "Delete failed", "error"); return; }
        notify("Playbook deleted", "success");
        setPbDeleteId("");
        playbooksLive.refresh();
      } catch (e) {
        notify("Delete failed: " + ((e && e.message) || e), "error");
      }
    };

    const toggleTool = async (t) => {
      try {
        const res = await api.post("/ui/api/cameras/apitools/update", {
          id: t.id, name: t.name, description: t.description || "", method: t.method || "GET",
          url_template: t.url_template || "", headers: t.headers_json || t.headers || "",
          auth_secret: "", body_template: t.body_template || "",
          request_instructions: t.request_instructions || "", response_instructions: t.response_instructions || "",
          enabled: !t.enabled,
        });
        if (!res || res.ok === false) { notify((res && res.error) || "Update failed", "error"); return; }
        toolsLive.refresh();
      } catch (e) {
        notify("Update failed: " + ((e && e.message) || e), "error");
      }
    };

    const deleteTool = async () => {
      try {
        const res = await api.post("/ui/api/cameras/apitools/delete", { id: toolDeleteId });
        if (!res || res.ok === false) { notify((res && res.error) || "Delete failed", "error"); return; }
        notify("API tool deleted", "success");
        setToolDeleteId("");
        toolsLive.refresh();
      } catch (e) {
        notify("Delete failed: " + ((e && e.message) || e), "error");
      }
    };

    return (
      <div className="cam-tab-body">
        <Card
          title="Playbook library"
          description="Operator-authored procedures the AI fetches by name. Each enabled playbook adds one line to every AI turn — keep the catalog tight."
          actions={<Button size="sm" variant="primary" icon="plus" onClick={() => { setPbEditing(null); setPbFormOpen(true); }}>New playbook</Button>}
          flush
        >
          {playbooks.length === 0 ? (
            <EmptyState icon="book" title="No playbooks yet" description="Write a procedure once — the AI follows it every time the question matches." compact />
          ) : (
            <div className="table-wrap">
              <table className="table">
                <thead>
                  <tr><th>Name</th><th>When to use</th><th>Scope</th><th>References</th><th>Enabled</th><th></th></tr>
                </thead>
                <tbody>
                  {playbooks.map((p) => (
                    <tr key={p.id}>
                      <td className="mono">{p.name}</td>
                      <td className="truncate cam-run-summary">{p.when_to_use || "—"}</td>
                      <td>{p.dvr_id ? <Badge tone="neutral">{dvrNameById[p.dvr_id] || p.dvr_id}</Badge> : <Badge tone="accent">Site-wide</Badge>}</td>
                      <td className="tnum">{p.media_count || 0}</td>
                      <td><Switch checked={!!p.enabled} onChange={() => togglePlaybook(p)} /></td>
                      <td className="cam-row-actions">
                        <IconButton icon="edit" label="Edit playbook" onClick={() => { setPbEditing(p); setPbFormOpen(true); }} />
                        <IconButton icon="trash" label="Delete playbook" onClick={() => setPbDeleteId(p.id)} />
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}
        </Card>

        <Card
          title="External API tools"
          description="Operator-configured HTTP lookups the AI calls by name (attendance systems, registries…). Responses are text for reasoning — never citable evidence."
          actions={<Button size="sm" variant="primary" icon="plus" onClick={() => { setToolEditing(null); setToolTestOpen(false); setToolFormOpen(true); }}>New API tool</Button>}
          flush
        >
          {apiTools.length === 0 ? (
            <EmptyState icon="globe" title="No API tools yet" description="Point the AI at an internal system — e.g. an attendance or booking API." compact />
          ) : (
            <div className="table-wrap">
              <table className="table">
                <thead>
                  <tr><th>Name</th><th>Method</th><th>Host</th><th>Secret</th><th>Enabled</th><th></th></tr>
                </thead>
                <tbody>
                  {apiTools.map((t) => (
                    <tr key={t.id}>
                      <td className="mono">{t.name}</td>
                      <td><Badge tone="neutral">{t.method || "GET"}</Badge></td>
                      <td className="mono">{toolHost(t)}</td>
                      <td>{t.has_secret ? <Badge tone="success">secret set</Badge> : <span className="subtle">—</span>}</td>
                      <td><Switch checked={!!t.enabled} onChange={() => toggleTool(t)} /></td>
                      <td className="cam-row-actions">
                        <Button size="sm" variant="ghost" icon="bolt" onClick={() => { setToolEditing(t); setToolTestOpen(true); setToolFormOpen(true); }}>Test</Button>
                        <IconButton icon="edit" label="Edit API tool" onClick={() => { setToolEditing(t); setToolTestOpen(false); setToolFormOpen(true); }} />
                        <IconButton icon="trash" label="Delete API tool" onClick={() => setToolDeleteId(t.id)} />
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}
        </Card>

        <PlaybookFormModal
          open={pbFormOpen}
          playbook={pbEditing}
          siteId={site.id}
          dvrs={dvrs}
          notify={notify}
          onClose={() => setPbFormOpen(false)}
          onSaved={() => { setPbFormOpen(false); playbooksLive.refresh(); }}
        />
        <ApiToolFormModal
          open={toolFormOpen}
          tool={toolEditing}
          siteId={site.id}
          notify={notify}
          initialTestOpen={toolTestOpen}
          onClose={() => setToolFormOpen(false)}
          onSaved={() => { setToolFormOpen(false); toolsLive.refresh(); }}
        />
        <Modal
          open={!!pbDeleteId}
          onClose={() => setPbDeleteId("")}
          title="Delete this playbook?"
          description="Removes the procedure and detaches its reference images (they fall back to normal media retention)."
          footer={
            <div className="cam-modal-footer">
              <Button variant="secondary" onClick={() => setPbDeleteId("")}>Cancel</Button>
              <Button variant="danger" icon="trash" onClick={deletePlaybook}>Delete playbook</Button>
            </div>
          }
        />
        <Modal
          open={!!toolDeleteId}
          onClose={() => setToolDeleteId("")}
          title="Delete this API tool?"
          description="The AI immediately loses access to this lookup; the stored secret is deleted with it."
          footer={
            <div className="cam-modal-footer">
              <Button variant="secondary" onClick={() => setToolDeleteId("")}>Cancel</Button>
              <Button variant="danger" icon="trash" onClick={deleteTool}>Delete API tool</Button>
            </div>
          }
        />
      </div>
    );
  }

  /* ─────────────────────────────────────────────────────────────
     Connect integration — service tokens
     ───────────────────────────────────────────────────────────── */

  // ServiceTokenModal mints the bearer token the Connect platform pastes into
  // its System panel. The plaintext comes back from POST exactly once, so it
  // lives ONLY in local state (`minted`) surfaced in a copy-once reveal — it is
  // never re-fetched, logged, or persisted client-side.
  function ServiceTokenModal({ open, onClose, notify, onMinted }) {
    const [scope, setScope] = useState("management");
    const [label, setLabel] = useState("");
    const [siteId, setSiteId] = useState("");
    const [saving, setSaving] = useState(false);
    const [minted, setMinted] = useState("");
    const [copied, setCopied] = useState(false);

    useEffect(() => {
      if (!open) return;
      setScope("management"); setLabel(""); setSiteId("");
      setSaving(false); setMinted(""); setCopied(false);
    }, [open]);

    const mint = useCallback(async () => {
      const lbl = label.trim();
      if (!lbl) { notify("Label is required", "error"); return; }
      if (scope === "site" && !siteId.trim()) { notify("Site ID is required for a site token", "error"); return; }
      setSaving(true);
      try {
        const body = { scope, label: lbl };
        if (scope === "site") body.site_id = siteId.trim();
        const res = await api.post("/ui/api/cameras/service-tokens", body);
        if (!res || res.ok === false || !res.token) { notify((res && res.error) || "Mint failed", "error"); return; }
        setMinted(res.token);
        notify("Token minted", "success");
        onMinted();
      } catch (e) {
        notify("Mint failed: " + ((e && e.message) || e), "error");
      } finally { setSaving(false); }
    }, [scope, label, siteId, notify, onMinted]);

    const copyToken = useCallback(() => {
      if (navigator.clipboard) navigator.clipboard.writeText(minted).catch(() => {});
      setCopied(true);
    }, [minted]);

    return (
      <Modal
        open={open}
        onClose={onClose}
        title={minted ? "Copy your new token" : "Mint service token"}
        description={minted
          ? "Copy this now — it will not be shown again. The proxy keeps only a hash."
          : "Bearer token the Connect platform uses to reach this proxy's camera service API."}
        width={560}
        footer={minted ? (
          <div className="cam-modal-footer">
            <Button variant="primary" size="md" icon={copied ? "check" : "copy"} onClick={copyToken}>{copied ? "Copied" : "Copy token"}</Button>
            <Button variant="secondary" onClick={onClose}>Done</Button>
          </div>
        ) : (
          <div className="cam-modal-footer">
            <Button variant="secondary" onClick={onClose} disabled={saving}>Cancel</Button>
            <Button variant="primary" icon="key" loading={saving} onClick={mint}>Mint token</Button>
          </div>
        )}
      >
        {minted ? (
          <Field label="Bearer token" hint="Paste into Connect → System → Camera Proxy Servers.">
            <Input mono readOnly value={minted} onFocus={(e) => e.target.select()} />
          </Field>
        ) : (
          <>
            <Field label="Scope" hint="Management tokens administer every site; site tokens are scoped to one company's cameras.">
              <Select
                options={[{ value: "management", label: "Management (all sites)" }, { value: "site", label: "Site" }]}
                value={scope}
                onChange={(e) => setScope(e.target.value)}
                disabled={saving}
              />
            </Field>
            <Field label="Label" hint="A human name so you can recognise this token later.">
              <Input value={label} onChange={(e) => setLabel(e.target.value)} placeholder="connect-production" disabled={saving} />
            </Field>
            {scope === "site" && (
              <Field label="Site ID" hint="The camera site this token is limited to (optional — most operators mint a management token).">
                <Input mono value={siteId} onChange={(e) => setSiteId(e.target.value)} placeholder="site_…" disabled={saving} />
              </Field>
            )}
          </>
        )}
      </Modal>
    );
  }

  // IntegrationTab lists the global (not per-site) service tokens and mints new
  // ones. Accepts the shared `notify`; any site/context props are ignored.
  function IntegrationTab({ notify }) {
    const tokensLive = useLive("/ui/api/cameras/service-tokens", { interval: 15000 });
    const tokens = (tokensLive.data && tokensLive.data.tokens) || [];

    const [mintOpen, setMintOpen] = useState(false);
    const [revokeId, setRevokeId] = useState("");
    const [revoking, setRevoking] = useState(false);

    const revoke = useCallback(async () => {
      if (!revokeId) return;
      setRevoking(true);
      try {
        const res = await api.post("/ui/api/cameras/service-tokens/revoke", { id: revokeId });
        if (!res || res.ok === false) { notify((res && res.error) || "Revoke failed", "error"); return; }
        notify("Token revoked", "success");
        setRevokeId("");
        tokensLive.refresh();
      } catch (e) {
        notify("Revoke failed: " + ((e && e.message) || e), "error");
      } finally { setRevoking(false); }
    }, [revokeId, notify, tokensLive]);

    return (
      <div className="cam-tab-body">
        <Card
          title="Connect service tokens"
          description="Mint a management token and paste it into Connect → System → Camera Proxy Servers. Site tokens are created automatically when a company adds the camera plugin."
          actions={<Button size="sm" variant="primary" icon="plus" onClick={() => setMintOpen(true)}>Mint token</Button>}
          flush
        >
          {tokens.length === 0 ? (
            <EmptyState icon="key" title="No service tokens yet" description="Mint a management token so the Connect platform can reach this proxy's camera API." compact />
          ) : (
            <div className="table-wrap">
              <table className="table">
                <thead>
                  <tr><th>Scope</th><th>Label</th><th>Token</th><th>Site</th><th>Status</th><th>Last used</th><th>Created</th><th></th></tr>
                </thead>
                <tbody>
                  {tokens.map((t) => {
                    const disabled = t.enabled === false;
                    return (
                      <tr key={t.id} className={disabled ? "muted" : undefined}>
                        <td>{t.scope === "site" ? <Badge tone="neutral">Site</Badge> : <Badge tone="accent">Management</Badge>}</td>
                        <td>{t.label || "—"}</td>
                        <td className="mono">{t.token_preview || "—"}</td>
                        <td className="mono">{t.site_id || "—"}</td>
                        <td>{disabled ? <Badge tone="neutral">Revoked</Badge> : <Badge tone="success">Enabled</Badge>}</td>
                        <td>{ago(t.last_used_at)}</td>
                        <td>{ago(t.created_at)}</td>
                        <td className="cam-row-actions">
                          {!disabled && (
                            <Button size="sm" variant="ghost" icon="trash" onClick={() => setRevokeId(t.id)}>Revoke</Button>
                          )}
                        </td>
                      </tr>
                    );
                  })}
                </tbody>
              </table>
            </div>
          )}
        </Card>

        <ServiceTokenModal
          open={mintOpen}
          notify={notify}
          onClose={() => setMintOpen(false)}
          onMinted={() => tokensLive.refresh()}
        />
        <Modal
          open={!!revokeId}
          onClose={() => { if (!revoking) setRevokeId(""); }}
          title="Revoke this token?"
          description="Connect immediately loses access with this token. This cannot be undone — mint a new one to restore access."
          footer={
            <div className="cam-modal-footer">
              <Button variant="secondary" onClick={() => setRevokeId("")} disabled={revoking}>Cancel</Button>
              <Button variant="danger" icon="trash" loading={revoking} onClick={revoke}>Revoke token</Button>
            </div>
          }
        />
      </div>
    );
  }

  /* ─────────────────────────────────────────────────────────────
     Top-level screen
     ───────────────────────────────────────────────────────────── */

  function Cameras({ pushToast }) {
    const toast = useToast();
    const notify = useCallback((msg, tone) => {
      if (toast) { tone === "error" ? toast.error(msg) : tone === "success" ? toast.success(msg) : toast.info(msg); }
      else if (pushToast) pushToast(msg, tone || "info");
    }, [toast, pushToast]);

    const sitesLive = useLive("/ui/api/cameras/sites", { interval: 5000 });
    const sites = (sitesLive.data && sitesLive.data.sites) || [];

    const [siteId, setSiteId] = useState("");
    useEffect(() => {
      if (!siteId && sites.length > 0) setSiteId(sites[0].id);
    }, [sites, siteId]);
    const site = useMemo(() => sites.find((s) => s.id === siteId) || null, [sites, siteId]);

    // Model aliases for the analysis-backend pickers (mirrors TestRequest's shape handling).
    const modelsLive = useLive("/ui/api/models", { interval: 20000 });
    const models = useMemo(() => {
      const d = modelsLive.data;
      const list = d && Array.isArray(d.models) ? d.models : [];
      if (list.length === 0 && Array.isArray(d)) return d;
      return list;
    }, [modelsLive.data]);
    const aliasOptions = useMemo(() => models.map((m) => {
      if (typeof m === "string") return { value: m, label: m };
      const alias = m.alias || m.id || m.name || "";
      const real = m.real || m.upstream_model || "";
      return { value: alias, label: real ? alias + " → " + real : alias };
    }), [models]);

    const dvrsLive = useLive(`/ui/api/cameras/dvrs?site_id=${encodeURIComponent(siteId)}`, { interval: 6000, enabled: !!siteId });
    const dvrs = (dvrsLive.data && dvrsLive.data.dvrs) || [];

    const camerasLive = useLive(`/ui/api/cameras/list?site_id=${encodeURIComponent(siteId)}`, { interval: 5000, enabled: !!siteId });
    const cameras = (camerasLive.data && camerasLive.data.cameras) || [];
    const camerasById = useMemo(() => {
      const m = {};
      cameras.forEach((c) => { m[c.id] = c; });
      return m;
    }, [cameras]);

    const questionsLive = useLive(`/ui/api/cameras/questions?site_id=${encodeURIComponent(siteId)}`, { interval: 6000, enabled: !!siteId });
    const questions = (questionsLive.data && questionsLive.data.questions) || [];
    const openQuestions = useMemo(() => questions.filter((q) => q.status === "open"), [questions]);

    const watchesLive = useLive(`/ui/api/cameras/watches?site_id=${encodeURIComponent(siteId)}`, { interval: 6000, enabled: !!siteId });
    const watches = (watchesLive.data && watchesLive.data.watches) || [];

    const investigationsLive = useLive(`/ui/api/cameras/investigations?site_id=${encodeURIComponent(siteId)}`, { interval: 8000, enabled: !!siteId });
    const investigations = (investigationsLive.data && investigationsLive.data.investigations) || [];
    const awaitingInvestigations = useMemo(() => investigations.filter((i) => i.status === "awaiting_operator"), [investigations]);

    // Pending avatar candidates drive the Avatars tab badge; the tab body itself
    // (window.AvatarsTab, screens-cameras-avatars.jsx) does its own polling.
    const avatarsLive = useLive(`/ui/api/cameras/avatars?site_id=${encodeURIComponent(siteId)}`, { interval: 8000, enabled: !!siteId });
    const pendingCandidates = (avatarsLive.data && avatarsLive.data.pending_candidates) || 0;

    const [activeTab, setActiveTab] = useState("setup");
    const [selectedWatchId, setSelectedWatchId] = useState("");
    const [selectedRunId, setSelectedRunId] = useState("");
    useEffect(() => { setSelectedWatchId(""); setSelectedRunId(""); }, [siteId]);
    useEffect(() => {
      if (!selectedWatchId && watches.length > 0) setSelectedWatchId(watches[0].id);
    }, [watches, selectedWatchId]);

    const runsLive = useLive(`/ui/api/cameras/runs?watch_id=${encodeURIComponent(selectedWatchId)}&limit=50`, { interval: 5000, enabled: !!selectedWatchId });
    const runs = (runsLive.data && runsLive.data.runs) || [];

    const runDetailLive = useLive(`/ui/api/cameras/runs/get?id=${encodeURIComponent(selectedRunId)}`, { interval: 4000, enabled: !!selectedRunId });
    const runDetail = (runDetailLive.data && runDetailLive.data.run) || null;

    const refreshAll = useCallback(() => {
      sitesLive.refresh(); dvrsLive.refresh(); camerasLive.refresh();
      questionsLive.refresh(); watchesLive.refresh(); runsLive.refresh(); investigationsLive.refresh();
    }, [sitesLive, dvrsLive, camerasLive, questionsLive, watchesLive, runsLive, investigationsLive]);

    const tabDefs = useMemo(() => ([
      { id: "setup", label: "Cameras", icon: "server" },
      { id: "questions", label: "Questions", icon: "info", badge: openQuestions.length || undefined },
      { id: "investigate", label: "Ask AI", icon: "search", badge: awaitingInvestigations.length || undefined },
      { id: "avatars", label: "Avatars", icon: "user", badge: pendingCandidates || undefined },
      { id: "activity", label: "Activity", icon: "logs" },
      { id: "knowledge", label: "Knowledge", icon: "book" },
      { id: "watches", label: "Watches", icon: "activity" },
      { id: "runs", label: "Run history", icon: "clock" },
      { id: "diag", label: "Diagnostics", icon: "shield" },
      { id: "integration", label: "Connect", icon: "key" },
    ]), [openQuestions.length, awaitingInvestigations.length, pendingCandidates]);

    return (
      <div className="screen screen-cameras">
        <PageHeader
          title="Cameras"
          description="RTSP/DVR camera monitoring — discover devices, describe your site, and let the AI escalate on suspicious activity."
          actions={<Button variant="ghost" icon="refresh" onClick={refreshAll}>Refresh</Button>}
        />

        <div className="grid-stats">
          <StatCard label="Sites" value={String(sites.length)} />
          <StatCard label="DVRs (this site)" value={site ? String(dvrs.length) : "—"} />
          <StatCard label="Cameras" value={site ? `${cameras.filter((c) => c.enabled).length}/${cameras.length} enabled` : "—"} />
          <StatCard label="Open questions" value={site ? String(openQuestions.length) : "—"} />
        </div>

        <SitesPanel
          sites={sites}
          siteId={siteId}
          site={site}
          setSiteId={setSiteId}
          aliasOptions={aliasOptions}
          notify={notify}
          refresh={sitesLive.refresh}
          onSiteDeleted={() => setSiteId("")}
        />

        {!site ? (
          <EmptyState icon="server" title="No site selected" description="Create a site above to add DVRs, describe cameras, and start monitoring." />
        ) : (
          <>
            <Tabs tabs={tabDefs} active={activeTab} onChange={setActiveTab} variant="pill" />

            {activeTab === "setup" && (
              <SetupTab site={site} dvrs={dvrs} cameras={cameras} notify={notify} refreshDvrs={dvrsLive.refresh} refreshCameras={camerasLive.refresh} />
            )}
            {activeTab === "questions" && (
              <QuestionsTab questions={questions} camerasById={camerasById} notify={notify} refresh={questionsLive.refresh} />
            )}
            {activeTab === "investigate" && (
              <InvestigateTab site={site} investigations={investigations} listRefresh={investigationsLive.refresh} notify={notify} />
            )}
            {activeTab === "avatars" && window.AvatarsTab && (
              <window.AvatarsTab site={site} dvrs={dvrs} cameras={cameras} aliasOptions={aliasOptions} notify={notify} onChanged={avatarsLive.refresh} />
            )}
            {activeTab === "activity" && window.ActivityTab && (
              <window.ActivityTab site={site} cameras={cameras} aliasOptions={aliasOptions} notify={notify} />
            )}
            {activeTab === "knowledge" && (
              <KnowledgeTab site={site} dvrs={dvrs} notify={notify} />
            )}
            {activeTab === "watches" && (
              <WatchesTab
                site={site} watches={watches} cameras={cameras} aliasOptions={aliasOptions} notify={notify}
                refresh={watchesLive.refresh}
                onSelectForHistory={(id) => { setSelectedWatchId(id); setActiveTab("runs"); }}
              />
            )}
            {activeTab === "runs" && (
              <RunHistoryTab
                watches={watches} selectedWatchId={selectedWatchId} setSelectedWatchId={setSelectedWatchId}
                runs={runs} selectedRunId={selectedRunId} setSelectedRunId={setSelectedRunId}
                runDetail={runDetail} camerasById={camerasById}
              />
            )}
            {activeTab === "diag" && (
              <DiagnosticsTab cameras={cameras} notify={notify} />
            )}
            {activeTab === "integration" && (
              <IntegrationTab notify={notify} />
            )}
          </>
        )}
      </div>
    );
  }

  window.Cameras = Cameras;
})();
