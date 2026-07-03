// screens-cameras-avatars.jsx — the Avatars tab of the Cameras page.
//
// Mirrors ui/screens-cameras.jsx for structure: plain window.* component
// library, window.api.get/post, useLive polling, no build step
// (Babel-in-browser). Exposes window.AvatarsTab, rendered by
// screens-cameras.jsx's tab switcher with { site, dvrs, cameras, notify,
// onChanged } (only { site } is guaranteed — everything else is re-fetched
// here when absent).
//
// Sections (design-avatars §7): avatar registry cards + create/edit modal,
// reference gallery (pinned operator-approved images + photo upload),
// enrollment scan launcher + scans list with progress, and the pending
// candidate review grid (approve / reject / assign-to-group).
//
// Data lives behind /ui/api/cameras/avatars[…] (camera_avatars.go +
// camera_avatar_scan.go) — admin-gated like the rest of the panel; the
// session cookie covers <img> requests to /camera/media/<token>.
(() => {
  const { useState, useEffect, useMemo, useCallback, useRef } = React;
  const {
    Icon, api, useLive, timeAgo,
    Button, IconButton, Card, Field, Input, Textarea, Select, Switch, Checkbox,
    Badge, Modal, useToast, EmptyState, Spinner,
  } = window;

  /* ─────────────────────────────────────────────────────────────
     Helpers
     ───────────────────────────────────────────────────────────── */

  function mediaURL(token) {
    return token ? "/camera/media/" + token : "";
  }

  function ago(ts) {
    if (!ts) return "—";
    const t = Date.parse(ts);
    return isNaN(t) ? ts : timeAgo(t);
  }

  // localTime renders an RFC3339 UTC instant in the browser's local time —
  // frame_ts is stored UTC; operators think in wall-clock time.
  function localTime(ts) {
    if (!ts) return "—";
    const t = Date.parse(ts);
    return isNaN(t) ? ts : new Date(t).toLocaleString();
  }

  // parseDvrIds tolerates the two shapes avatar.dvr_ids arrives in — a JSON
  // array (typical) or the raw TEXT column value ("" / "[…]").
  function parseDvrIds(raw) {
    if (Array.isArray(raw)) return raw;
    if (typeof raw === "string" && raw.trim()) {
      try {
        const v = JSON.parse(raw);
        return Array.isArray(v) ? v : [];
      } catch (e) { return []; }
    }
    return [];
  }

  // parseProgress tolerates scan.progress as a JSON string or an object.
  function parseProgress(raw) {
    if (!raw) return null;
    if (typeof raw === "object") return raw;
    try { return JSON.parse(raw); } catch (e) { return null; }
  }

  function readFileAsDataURL(file) {
    return new Promise((resolve, reject) => {
      const fr = new FileReader();
      fr.onload = () => resolve(fr.result);
      fr.onerror = () => reject(fr.error || new Error("could not read the file"));
      fr.readAsDataURL(file);
    });
  }

  // defaultScanWindow: yesterday 06:00 → 20:00 local, as datetime-local values.
  function defaultScanWindow() {
    const d = new Date();
    d.setDate(d.getDate() - 1);
    const pad = (n) => String(n).padStart(2, "0");
    const ymd = d.getFullYear() + "-" + pad(d.getMonth() + 1) + "-" + pad(d.getDate());
    return { from: ymd + "T06:00", to: ymd + "T20:00" };
  }

  const AVATAR_TYPE_OPTIONS = [
    { value: "human", label: "Human" },
    { value: "vehicle", label: "Vehicle" },
    { value: "pet", label: "Pet" },
  ];
  const TYPE_TONE = { human: "accent", vehicle: "info", pet: "warning" };
  const SCAN_STATUS_TONE = { queued: "info", running: "info", done: "success", error: "danger", cancelled: "neutral" };

  /* ─────────────────────────────────────────────────────────────
     Avatar registry — list cards + create/edit modal
     ───────────────────────────────────────────────────────────── */

  function AvatarListCard({ avatar, active, onSelect, onEdit, onDelete, onScan, onToggle }) {
    const refs = avatar.ref_count || 0;
    return (
      <div
        className={"cam-card" + (avatar.enabled ? "" : " disabled")}
        onClick={onSelect}
        style={{ cursor: "pointer", ...(active ? { outline: "2px solid var(--accent)", outlineOffset: "-1px" } : {}) }}
      >
        <div className="cam-card-body">
          <div className="cam-card-title-row">
            <span className="cam-card-name truncate">{avatar.name}</span>
            <span className="cam-row-actions">
              <IconButton icon="edit" label="Edit avatar" size={13} onClick={(e) => { e.stopPropagation(); onEdit(); }} />
              <IconButton icon="trash" label="Delete avatar" size={13} onClick={(e) => { e.stopPropagation(); onDelete(); }} />
            </span>
          </div>
          <div className="cam-card-badges">
            <Badge tone={TYPE_TONE[avatar.type] || "neutral"}>{avatar.type || "human"}</Badge>
            {avatar.is_group ? <Badge tone="warning">group</Badge> : null}
            <Badge tone="neutral">{refs} ref{refs === 1 ? "" : "s"}</Badge>
          </div>
          {avatar.description && <p className="cam-card-desc">{avatar.description}</p>}
          <div className="cam-card-foot">
            <Switch checked={!!avatar.enabled} onChange={onToggle} label={avatar.enabled ? "Enabled" : "Disabled"} />
            <Button size="sm" variant="ghost" icon="search" onClick={(e) => { e.stopPropagation(); onScan(); }}>Scan footage</Button>
          </div>
        </div>
      </div>
    );
  }

  function AvatarFormModal({ open, avatar, siteId, dvrs, notify, onClose, onSaved }) {
    const blank = { name: "", type: "human", is_group: false, external_ref: "", description: "", dvr_ids: [] };
    const [form, setForm] = useState(blank);
    const [photo, setPhoto] = useState(null);
    const [saving, setSaving] = useState(false);
    const fileRef = useRef(null);

    useEffect(() => {
      if (!open) return;
      setPhoto(null);
      if (fileRef.current) fileRef.current.value = "";
      setForm(avatar ? {
        name: avatar.name || "", type: avatar.type || "human", is_group: !!avatar.is_group,
        external_ref: avatar.external_ref || "", description: avatar.description || "",
        dvr_ids: parseDvrIds(avatar.dvr_ids),
      } : blank);
    }, [open, avatar]);

    const toggleDvr = (id) => {
      setForm((f) => ({
        ...f,
        dvr_ids: f.dvr_ids.includes(id) ? f.dvr_ids.filter((x) => x !== id) : [...f.dvr_ids, id],
      }));
    };

    const save = useCallback(async () => {
      const name = form.name.trim();
      if (!name) { notify("Name is required", "error"); return; }
      setSaving(true);
      try {
        const fields = {
          name, type: form.type, is_group: form.is_group,
          external_ref: form.external_ref, description: form.description, dvr_ids: form.dvr_ids,
        };
        if (avatar) {
          const res = await api.post("/ui/api/cameras/avatars/update", { id: avatar.id, ...fields, enabled: avatar.enabled !== false });
          if (!res || res.ok === false) { notify((res && res.error) || "Save failed", "error"); return; }
          notify("Avatar updated", "success");
          onSaved(avatar.id);
        } else {
          const res = await api.post("/ui/api/cameras/avatars", { site_id: siteId, ...fields });
          if (!res || res.ok === false) { notify((res && res.error) || "Save failed", "error"); return; }
          const newId = res.id || "";
          if (photo && newId) {
            try {
              const dataURL = await readFileAsDataURL(photo);
              const up = await api.post("/ui/api/cameras/avatars/media/upload", { avatar_id: newId, image_b64: dataURL, note: "" }, { timeoutMs: 60000 });
              if (!up || up.ok === false) notify("Avatar created, but the photo upload failed: " + ((up && up.error) || "unknown error"), "error");
            } catch (e) {
              notify("Avatar created, but the photo upload failed: " + ((e && e.message) || e), "error");
            }
          }
          notify("Avatar created", "success");
          onSaved(newId);
        }
      } catch (e) {
        notify("Save failed: " + ((e && e.message) || e), "error");
      } finally { setSaving(false); }
    }, [form, photo, avatar, siteId, notify, onSaved]);

    return (
      <Modal
        open={open}
        onClose={onClose}
        title={avatar ? "Edit avatar" : "New avatar"}
        width={560}
        footer={
          <div className="cam-modal-footer">
            <Button variant="secondary" onClick={onClose} disabled={saving}>Cancel</Button>
            <Button variant="primary" icon="check" loading={saving} onClick={save}>{avatar ? "Save" : "Create avatar"}</Button>
          </div>
        }
      >
        <Field label="Name">
          <Input value={form.name} onChange={(e) => setForm((f) => ({ ...f, name: e.target.value }))} placeholder="Dr. Ahmad" disabled={saving} />
        </Field>
        <div className="cam-form-grid-2">
          <Field label="Type">
            <Select options={AVATAR_TYPE_OPTIONS} value={form.type} onChange={(e) => setForm((f) => ({ ...f, type: e.target.value }))} disabled={saving} />
          </Field>
          <Field label="External reference" hint="Employee id / plate number / chip id.">
            <Input mono value={form.external_ref} onChange={(e) => setForm((f) => ({ ...f, external_ref: e.target.value }))} disabled={saving} />
          </Field>
        </div>
        <Checkbox
          checked={form.is_group}
          onChange={(v) => setForm((f) => ({ ...f, is_group: v }))}
          label="Group — a class of people (e.g. patients, customers), not one individual"
          disabled={saving}
        />
        <Field label="Description" hint="Appearance, habits, schedule — the AI matches on this when reference images are scarce.">
          <Textarea
            rows={4}
            value={form.description}
            onChange={(e) => setForm((f) => ({ ...f, description: e.target.value }))}
            placeholder="Male, ~50, usually a white coat; arrives 8–9am via the staff door; drives a grey SUV…"
            disabled={saving}
          />
        </Field>
        <Field label="DVRs" hint="Leave empty to cover every DVR on this site.">
          <div className="cam-multiselect">
            {dvrs.length === 0 ? (
              <p className="cfg-hint">No DVRs on this site yet.</p>
            ) : dvrs.map((d) => (
              <Checkbox
                key={d.id}
                checked={form.dvr_ids.includes(d.id)}
                onChange={() => toggleDvr(d.id)}
                label={d.name || d.host}
                disabled={saving}
              />
            ))}
          </div>
        </Field>
        {!avatar && (
          <Field label="Initial photo (optional)" hint="A clear photo to seed the reference gallery — uploaded right after the avatar is created.">
            <input
              ref={fileRef}
              className="input"
              type="file"
              accept="image/*"
              onChange={(e) => setPhoto(e.target.files && e.target.files[0] ? e.target.files[0] : null)}
              disabled={saving}
            />
          </Field>
        )}
      </Modal>
    );
  }

  /* ─────────────────────────────────────────────────────────────
     Reference gallery — pinned operator-approved images + upload
     ───────────────────────────────────────────────────────────── */

  function ReferenceGallery({ avatar, notify, onChanged }) {
    const mediaLive = useLive(`/ui/api/cameras/avatars/media?avatar_id=${encodeURIComponent(avatar.id)}`, { interval: 8000 });
    const media = (mediaLive.data && mediaLive.data.media) || [];
    const [note, setNote] = useState("");
    const [uploading, setUploading] = useState(false);
    const fileRef = useRef(null);

    const onFile = async (e) => {
      const file = e.target.files && e.target.files[0];
      if (!file) return;
      setUploading(true);
      try {
        const dataURL = await readFileAsDataURL(file);
        const res = await api.post("/ui/api/cameras/avatars/media/upload", { avatar_id: avatar.id, image_b64: dataURL, note }, { timeoutMs: 60000 });
        if (!res || res.ok === false) { notify((res && res.error) || "Upload failed", "error"); return; }
        notify("Reference photo uploaded", "success");
        setNote("");
        mediaLive.refresh();
        if (onChanged) onChanged();
      } catch (err) {
        notify("Upload failed: " + ((err && err.message) || err), "error");
      } finally {
        setUploading(false);
        if (fileRef.current) fileRef.current.value = "";
      }
    };

    const remove = async (id) => {
      try {
        const res = await api.post("/ui/api/cameras/avatars/media/delete", { id });
        if (!res || res.ok === false) { notify((res && res.error) || "Delete failed", "error"); return; }
        notify("Reference removed", "success");
        mediaLive.refresh();
        if (onChanged) onChanged();
      } catch (e) {
        notify("Delete failed: " + ((e && e.message) || e), "error");
      }
    };

    const caption = (m) => {
      const bits = [m.source || "scan"];
      if (m.match_confidence) bits.push("conf " + Number(m.match_confidence).toFixed(2));
      if (m.note) bits.push(m.note);
      return bits.join(" · ");
    };

    return (
      <Card
        title={"References — " + avatar.name}
        description="Operator-approved reference images (pinned, never reaped) — the AI studies these before judging any frame."
        actions={
          <div className="cam-event-filters">
            <Input placeholder="note, e.g. winter coat" value={note} onChange={(e) => setNote(e.target.value)} disabled={uploading} />
            <Button size="sm" variant="secondary" icon={uploading ? undefined : "upload"} loading={uploading} onClick={() => fileRef.current && fileRef.current.click()}>
              Upload photo
            </Button>
            <input ref={fileRef} type="file" accept="image/*" style={{ display: "none" }} onChange={onFile} />
          </div>
        }
      >
        {media.length === 0 ? (
          <EmptyState icon="image" title="No references yet" description="Upload a photo, or scan footage below and approve the candidates." compact />
        ) : (
          <div className="cam-evidence-grid">
            {media.map((m) => (
              <div key={m.id} className="cam-evidence-item">
                <img src={m.url || mediaURL(m.token)} alt={m.note || "reference"} />
                <span className="cam-evidence-caption" title={caption(m)}>{caption(m)}</span>
                <Button size="sm" variant="ghost" icon="trash" onClick={() => remove(m.id)}>Remove</Button>
              </div>
            ))}
          </div>
        )}
      </Card>
    );
  }

  /* ─────────────────────────────────────────────────────────────
     Scan launcher + scans list (enrollment queue)
     ───────────────────────────────────────────────────────────── */

  function ScanProgress({ scan }) {
    const p = parseProgress(scan.progress) || {};
    const total = Number(p.cameras_total) || 0;
    // cameras_done is the resume ledger (a JSON array of camera ids), not a count.
    const done = Array.isArray(p.cameras_done) ? p.cameras_done.length : Number(p.cameras_done) || 0;
    const pct = scan.status === "done" ? 100 : total > 0 ? Math.min(100, Math.round((done / total) * 100)) : 0;
    return (
      <div style={{ minWidth: 120 }}>
        <div style={{ height: 4, borderRadius: 2, background: "var(--border)", overflow: "hidden", marginBottom: 4 }}>
          <div style={{ width: pct + "%", height: "100%", background: "var(--accent)" }} />
        </div>
        <span className="muted subtle">
          {total > 0 ? `${done}/${total} cameras` : "—"}
          {p.current_camera ? ` · ${p.current_camera}` : ""}
        </span>
      </div>
    );
  }

  function ScanPanel({ avatar, cameras, notify }) {
    const win = defaultScanWindow();
    const [cameraIds, setCameraIds] = useState([]);
    const [touched, setTouched] = useState(false);
    const [from, setFrom] = useState(win.from);
    const [to, setTo] = useState(win.to);
    const [busy, setBusy] = useState(false);

    // Default selection = enabled cameras on the avatar's DVRs (all DVRs when
    // unscoped) — recomputed until the operator touches the checkboxes.
    useEffect(() => {
      if (touched) return;
      const dvrIds = parseDvrIds(avatar.dvr_ids);
      const scoped = cameras.filter((c) => c.enabled && (dvrIds.length === 0 || dvrIds.indexOf(c.dvr_id) !== -1));
      setCameraIds(scoped.map((c) => c.id));
    }, [cameras.length, avatar.id, touched]);

    const toggleCamera = (id) => {
      setTouched(true);
      setCameraIds((ids) => (ids.includes(id) ? ids.filter((x) => x !== id) : [...ids, id]));
    };

    const scansLive = useLive(`/ui/api/cameras/avatars/scans?avatar_id=${encodeURIComponent(avatar.id)}`, { interval: 4000 });
    const scans = (scansLive.data && scansLive.data.scans) || [];

    const submit = useCallback(async () => {
      if (!from || !to) { notify("Pick a from/to window", "error"); return; }
      if (cameraIds.length === 0) { notify("Pick at least one camera", "error"); return; }
      setBusy(true);
      try {
        const res = await api.post("/ui/api/cameras/avatars/scan", {
          avatar_id: avatar.id,
          camera_ids: cameraIds,
          from: new Date(from).toISOString(),
          to: new Date(to).toISOString(),
        });
        if (!res || res.ok === false) { notify((res && res.error) || "Could not queue the scan", "error"); return; }
        notify("Scan queued — candidates appear below as they are found", "success");
        scansLive.refresh();
      } catch (e) {
        notify("Could not queue the scan: " + ((e && e.message) || e), "error");
      } finally { setBusy(false); }
    }, [avatar.id, cameraIds, from, to, notify, scansLive]);

    return (
      <Card
        title="Scan footage"
        description="Sweep the ~1fps archive for this avatar across a time window — matches land in the review grid below."
      >
        <Field label="Cameras" hint="Defaults to the enabled cameras on this avatar's DVRs.">
          <div className="cam-multiselect">
            {cameras.length === 0 ? (
              <p className="cfg-hint">No cameras discovered yet.</p>
            ) : cameras.map((c) => (
              <Checkbox
                key={c.id}
                checked={cameraIds.includes(c.id)}
                onChange={() => toggleCamera(c.id)}
                label={c.name + (c.area ? " · " + c.area : "")}
                disabled={busy}
              />
            ))}
          </div>
        </Field>
        <div className="cam-form-grid-2">
          <Field label="From">
            <Input type="datetime-local" mono value={from} onChange={(e) => setFrom(e.target.value)} disabled={busy} />
          </Field>
          <Field label="To">
            <Input type="datetime-local" mono value={to} onChange={(e) => setTo(e.target.value)} disabled={busy} />
          </Field>
        </div>
        <div className="cam-wizard-actions">
          <Button variant="primary" icon={busy ? undefined : "search"} loading={busy} disabled={busy || cameraIds.length === 0} onClick={submit}>
            {busy ? "Queuing…" : "Start scan"}
          </Button>
        </div>

        {scans.length > 0 && (
          <div className="table-wrap" style={{ marginTop: "var(--sp-3)" }}>
            <table className="table">
              <thead>
                <tr><th>Queued</th><th>Window</th><th>Status</th><th>Progress</th><th>Candidates</th></tr>
              </thead>
              <tbody>
                {scans.map((s) => {
                  const p = parseProgress(s.progress) || {};
                  return (
                    <tr key={s.id}>
                      <td className="muted">{ago(s.created_at)}</td>
                      <td className="mono subtle">{localTime(s.from_ts)} → {localTime(s.to_ts)}</td>
                      <td>
                        <Badge tone={SCAN_STATUS_TONE[s.status] || "neutral"} pulse={s.status === "running" || s.status === "queued"}>{s.status}</Badge>
                        {s.error && <div className="cam-hint-fail">{s.error}</div>}
                      </td>
                      <td><ScanProgress scan={s} /></td>
                      <td className="tnum">{p.candidates != null ? p.candidates : "—"}</td>
                    </tr>
                  );
                })}
              </tbody>
            </table>
          </div>
        )}
      </Card>
    );
  }

  /* ─────────────────────────────────────────────────────────────
     Candidate review grid — approve / reject / assign-to-group
     ───────────────────────────────────────────────────────────── */

  function CandidateCard({ cand, camName, groups, state, note, setNote, groupId, setGroupId, onReview }) {
    const conf = Number(cand.vlm_confidence) || 0;
    const thumb = cand.annotated_url || mediaURL(cand.annotated_token) || cand.crop_url || mediaURL(cand.crop_token);
    const isFace = cand.match_kind === "face";
    return (
      <div className="cam-card">
        <div className="cam-card-thumb">
          {thumb ? <img src={thumb} alt="candidate" /> : (
            <div className="cam-card-thumb-empty"><Icon name="image" size={20} /></div>
          )}
        </div>
        <div className="cam-card-body">
          <div className="cam-card-title-row">
            <span className="cam-card-name truncate">{camName(cand.camera_id)}</span>
            <Badge tone={isFace ? "accent" : "neutral"}>{isFace ? "face" : "appearance"}</Badge>
          </div>
          <div className="cam-card-badges">
            <span className="mono subtle">{localTime(cand.frame_ts)}</span>
            <Badge tone={conf >= 0.7 ? "success" : conf >= 0.5 ? "warning" : "neutral"}>conf {conf.toFixed(2)}</Badge>
          </div>
          {cand.vlm_reason && <p className="cam-card-desc">{cand.vlm_reason}</p>}
          {state ? (
            <div className="cam-card-foot">
              {state === "rejected" ? (
                <Badge tone="danger"><Icon name="x" size={11} /> rejected</Badge>
              ) : (
                <Badge tone="success"><Icon name="check" size={11} /> {state}</Badge>
              )}
            </div>
          ) : (
            <>
              <Input placeholder="note (optional)" value={note} onChange={(e) => setNote(e.target.value)} />
              <div className="cam-card-foot">
                <Button size="sm" variant="primary" icon="check" onClick={() => onReview("approve")}>Approve</Button>
                <Button size="sm" variant="ghost" icon="x" onClick={() => onReview("reject")}>Reject</Button>
              </div>
              {groups.length > 0 && (
                <div className="cam-inline-fields">
                  <Select
                    options={groups.map((g) => ({ value: g.id, label: g.name }))}
                    value={groupId}
                    onChange={(e) => setGroupId(e.target.value)}
                  />
                  <Button size="sm" variant="secondary" onClick={() => onReview("group")}>Assign to group</Button>
                </div>
              )}
            </>
          )}
        </div>
      </div>
    );
  }

  function CandidateReviewGrid({ avatar, avatars, camerasById, notify, onChanged }) {
    const candsLive = useLive(
      `/ui/api/cameras/avatars/candidates?avatar_id=${encodeURIComponent(avatar.id)}&status=pending`,
      { interval: 5000 }
    );
    const candidates = (candsLive.data && candsLive.data.candidates) || [];

    // Optimistic per-candidate review state: the poll eventually drops reviewed
    // rows from the pending list; until then the cell shows its check/cross.
    const [reviewed, setReviewed] = useState({});
    const [notes, setNotes] = useState({});
    const [groupSel, setGroupSel] = useState({});

    const groups = useMemo(() => avatars.filter((a) => a.is_group && a.id !== avatar.id), [avatars, avatar.id]);
    const camName = useCallback((id) => (camerasById[id] ? camerasById[id].name : id), [camerasById]);

    const review = useCallback(async (cand, action) => {
      const body = { id: cand.id, action, note: notes[cand.id] || "" };
      if (action === "group") {
        body.group_avatar_id = groupSel[cand.id] || (groups.length > 0 ? groups[0].id : "");
        if (!body.group_avatar_id) { notify("Create a group avatar first", "error"); return; }
      }
      const optimistic = action === "approve" ? "approved" : action === "reject" ? "rejected" : "grouped";
      setReviewed((r) => ({ ...r, [cand.id]: optimistic }));
      try {
        const res = await api.post("/ui/api/cameras/avatars/candidates/review", body);
        if (!res || res.ok === false) {
          setReviewed((r) => { const n = { ...r }; delete n[cand.id]; return n; });
          notify((res && res.error) || "Review failed", "error");
          return;
        }
        if (onChanged) onChanged();
      } catch (e) {
        setReviewed((r) => { const n = { ...r }; delete n[cand.id]; return n; });
        notify("Review failed: " + ((e && e.message) || e), "error");
      }
    }, [notes, groupSel, groups, notify, onChanged]);

    return (
      <Card
        title="Candidate review"
        description="Frames the scanner thinks show this avatar — nothing enters the reference set without your approval."
        flush
      >
        {candidates.length === 0 ? (
          <EmptyState icon="check" title="No pending candidates" description="Run a footage scan above to propose sightings for review." compact />
        ) : (
          <div className="cam-grid">
            {candidates.map((c) => (
              <CandidateCard
                key={c.id}
                cand={c}
                camName={camName}
                groups={groups}
                state={reviewed[c.id] || ""}
                note={notes[c.id] || ""}
                setNote={(v) => setNotes((n) => ({ ...n, [c.id]: v }))}
                groupId={groupSel[c.id] || (groups.length > 0 ? groups[0].id : "")}
                setGroupId={(v) => setGroupSel((g) => ({ ...g, [c.id]: v }))}
                onReview={(action) => review(c, action)}
              />
            ))}
          </div>
        )}
      </Card>
    );
  }

  /* ─────────────────────────────────────────────────────────────
     Tab root
     ───────────────────────────────────────────────────────────── */

  function AvatarsTab({ site, dvrs: dvrsProp, cameras: camerasProp, notify: notifyProp, onChanged: onChangedProp }) {
    const toast = useToast();
    const fallbackNotify = useCallback((msg, tone) => {
      if (!toast) return;
      if (tone === "error") toast.error(msg);
      else if (tone === "success") toast.success(msg);
      else toast.info(msg);
    }, [toast]);
    const notify = notifyProp || fallbackNotify;

    const avatarsLive = useLive(`/ui/api/cameras/avatars?site_id=${encodeURIComponent(site.id)}`, { interval: 6000, enabled: !!site.id });
    const avatars = (avatarsLive.data && avatarsLive.data.avatars) || [];
    const pendingCandidates = (avatarsLive.data && avatarsLive.data.pending_candidates) || 0;

    // dvrs/cameras usually arrive as props from the Cameras screen; fetch them
    // here only when this tab is mounted standalone.
    const needDvrs = !Array.isArray(dvrsProp);
    const dvrsLive = useLive(`/ui/api/cameras/dvrs?site_id=${encodeURIComponent(site.id)}`, { interval: 10000, enabled: !!site.id && needDvrs });
    const dvrs = needDvrs ? ((dvrsLive.data && dvrsLive.data.dvrs) || []) : dvrsProp;

    const needCameras = !Array.isArray(camerasProp);
    const camerasLive = useLive(`/ui/api/cameras/list?site_id=${encodeURIComponent(site.id)}`, { interval: 10000, enabled: !!site.id && needCameras });
    const cameras = needCameras ? ((camerasLive.data && camerasLive.data.cameras) || []) : camerasProp;

    const camerasById = useMemo(() => {
      const m = {};
      cameras.forEach((c) => { m[c.id] = c; });
      return m;
    }, [cameras]);

    const [selectedId, setSelectedId] = useState("");
    useEffect(() => { setSelectedId(""); }, [site.id]);
    useEffect(() => {
      if (!selectedId && avatars.length > 0) setSelectedId(avatars[0].id);
    }, [avatars, selectedId]);
    const selected = useMemo(() => avatars.find((a) => a.id === selectedId) || null, [avatars, selectedId]);

    const [formOpen, setFormOpen] = useState(false);
    const [editing, setEditing] = useState(null);
    const [deleteId, setDeleteId] = useState("");

    const onChanged = useCallback(() => {
      avatarsLive.refresh();
      if (onChangedProp) onChangedProp();
    }, [avatarsLive, onChangedProp]);

    // Toggle resends the row's full editable fields alongside the flipped
    // enabled bit — safe under both pointer-patch and full-replace update
    // semantics server-side.
    const toggleAvatar = useCallback(async (a) => {
      try {
        const res = await api.post("/ui/api/cameras/avatars/update", {
          id: a.id, name: a.name, type: a.type || "human", is_group: !!a.is_group,
          external_ref: a.external_ref || "", description: a.description || "",
          dvr_ids: parseDvrIds(a.dvr_ids), enabled: !a.enabled,
        });
        if (!res || res.ok === false) { notify((res && res.error) || "Update failed", "error"); return; }
        onChanged();
      } catch (e) {
        notify("Update failed: " + ((e && e.message) || e), "error");
      }
    }, [notify, onChanged]);

    const deleteAvatar = useCallback(async () => {
      try {
        const res = await api.post("/ui/api/cameras/avatars/delete", { id: deleteId });
        if (!res || res.ok === false) { notify((res && res.error) || "Delete failed", "error"); return; }
        notify("Avatar deleted", "success");
        if (deleteId === selectedId) setSelectedId("");
        setDeleteId("");
        onChanged();
      } catch (e) {
        notify("Delete failed: " + ((e && e.message) || e), "error");
      }
    }, [deleteId, selectedId, notify, onChanged]);

    return (
      <div className="cam-tab-body">
        <Card
          title="Avatars"
          description={
            "Known people, vehicles, and pets — the AI matches them against footage using operator-approved references." +
            (pendingCandidates > 0 ? ` ${pendingCandidates} candidate(s) awaiting review.` : "")
          }
          actions={<Button size="sm" variant="primary" icon="plus" onClick={() => { setEditing(null); setFormOpen(true); }}>New avatar</Button>}
          flush
        >
          {avatars.length === 0 ? (
            <EmptyState icon="user" title="No avatars yet" description="Register a person (e.g. an employee) to find and track them across cameras and time." compact />
          ) : (
            <div className="cam-grid">
              {avatars.map((a) => (
                <AvatarListCard
                  key={a.id}
                  avatar={a}
                  active={a.id === selectedId}
                  onSelect={() => setSelectedId(a.id)}
                  onEdit={() => { setEditing(a); setFormOpen(true); }}
                  onDelete={() => setDeleteId(a.id)}
                  onScan={() => setSelectedId(a.id)}
                  onToggle={() => toggleAvatar(a)}
                />
              ))}
            </div>
          )}
        </Card>

        {selected && (
          <>
            <ReferenceGallery key={"refs-" + selected.id} avatar={selected} notify={notify} onChanged={onChanged} />
            <ScanPanel key={"scan-" + selected.id} avatar={selected} cameras={cameras} notify={notify} />
            <CandidateReviewGrid
              key={"cands-" + selected.id}
              avatar={selected}
              avatars={avatars}
              camerasById={camerasById}
              notify={notify}
              onChanged={onChanged}
            />
          </>
        )}

        <AvatarFormModal
          open={formOpen}
          avatar={editing}
          siteId={site.id}
          dvrs={dvrs}
          notify={notify}
          onClose={() => setFormOpen(false)}
          onSaved={(id) => {
            setFormOpen(false);
            onChanged();
            if (id) setSelectedId(id);
          }}
        />
        <Modal
          open={!!deleteId}
          onClose={() => setDeleteId("")}
          title="Delete this avatar?"
          description="Removes the avatar, its pinned reference images, and any pending candidates."
          footer={
            <div className="cam-modal-footer">
              <Button variant="secondary" onClick={() => setDeleteId("")}>Cancel</Button>
              <Button variant="danger" icon="trash" onClick={deleteAvatar}>Delete avatar</Button>
            </div>
          }
        />
      </div>
    );
  }

  window.AvatarsTab = AvatarsTab;
})();
