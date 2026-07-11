// screens-cameras-activity.jsx — the Activity tab of the Cameras page.
//
// Mirrors ui/screens-cameras-avatars.jsx for structure: plain window.*
// component library, window.api.get/post, useLive polling, no build step
// (Babel-in-browser). Exposes window.ActivityTab, rendered by
// screens-cameras.jsx's tab switcher with { site, cameras, aliasOptions,
// notify } (only { site } is guaranteed — cameras are re-fetched here when
// absent).
//
// Sections (plan P10): StreamsPanel (activity-log stream table + create/edit
// modal with the WatchesTab camera picker), LogBrowser (stream + day selector,
// expandable journal entries with composite-sheet thumbnails that hide
// themselves once media retention reaps the file), HoursPanel (hourly
// rollups), DaysPanel (daily reports: status, share-link copy, generate /
// regenerate).
//
// Data lives behind /ui/api/cameras/activity/* (camera_observer_http.go) —
// admin-gated like the rest of the panel; the session cookie covers <img>
// requests to /camera/media/<token>. Everything polls at 30s — the journal
// only moves every 1–15 minutes.
(() => {
  const { useState, useEffect, useMemo, useCallback } = React;
  const {
    Icon, api, useLive, timeAgo,
    Button, IconButton, Card, Field, Input, Select, Switch, Checkbox,
    Badge, Modal, useToast, EmptyState, CopyButton,
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

  // clock renders an RFC3339 UTC instant as browser-local "HH:MM" — journal
  // windows are stored UTC; operators think in wall-clock time.
  function clock(ts) {
    if (!ts) return "—";
    const t = Date.parse(ts);
    return isNaN(t) ? ts : new Date(t).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" });
  }

  // hourLabel renders an hour boundary with its date ("Jul 11, 14:00").
  function hourLabel(ts) {
    if (!ts) return "—";
    const t = Date.parse(ts);
    if (isNaN(t)) return ts;
    const d = new Date(t);
    return d.toLocaleDateString([], { month: "short", day: "numeric" }) + ", " +
      d.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" });
  }

  // todayLocalDay: the browser-local calendar date as "YYYY-MM-DD" — the day
  // picker's default and the generate button's default date.
  function todayLocalDay() {
    const d = new Date();
    const pad = (n) => String(n).padStart(2, "0");
    return d.getFullYear() + "-" + pad(d.getMonth() + 1) + "-" + pad(d.getDate());
  }

  // dayWindowUTC maps a local "YYYY-MM-DD" day onto the UTC RFC3339 window the
  // entries route filters from_ts with (seconds precision, matching the store's
  // stored format so the string comparisons line up).
  function dayWindowUTC(day) {
    const start = new Date(day + "T00:00:00");
    if (isNaN(start.getTime())) return null;
    const iso = (d) => d.toISOString().replace(/\.\d+Z$/, "Z");
    return { from: iso(start), to: iso(new Date(start.getTime() + 86399000)) };
  }

  const ROLLUP_STATUS_TONE = { pending: "info", done: "success", empty: "neutral", error: "danger" };
  function RollupStatusBadge({ status }) {
    return <Badge tone={ROLLUP_STATUS_TONE[status] || "neutral"} pulse={status === "pending"}>{status || "—"}</Badge>;
  }

  /* ─────────────────────────────────────────────────────────────
     STREAMS — table + create/edit modal (WatchFormModal clone)
     ───────────────────────────────────────────────────────────── */

  function StreamFormModal({ open, stream, siteId, cameras, aliasOptions, notify, onClose, onSaved }) {
    const blank = {
      name: "", camera_ids: [], interval_minutes: 5, frame_step_seconds: 30,
      analysis_alias: "", active_hours: "", motion_gate: true,
      daily_report_time: "20:00", enabled: false,
    };
    const [form, setForm] = useState(blank);
    const [saving, setSaving] = useState(false);

    useEffect(() => {
      if (!open) return;
      if (stream) {
        setForm({
          name: stream.name || "", camera_ids: stream.camera_ids || [],
          interval_minutes: stream.interval_minutes || 5,
          frame_step_seconds: stream.frame_step_seconds || 30,
          analysis_alias: stream.analysis_alias || "", active_hours: stream.active_hours || "",
          motion_gate: stream.motion_gate !== false,
          daily_report_time: stream.daily_report_time || "20:00",
          enabled: !!stream.enabled,
        });
      } else {
        setForm(blank);
      }
    }, [open, stream]);

    const toggleCamera = (id) => {
      setForm((f) => ({
        ...f,
        camera_ids: f.camera_ids.includes(id) ? f.camera_ids.filter((x) => x !== id) : [...f.camera_ids, id],
      }));
    };

    const save = useCallback(async () => {
      const name = form.name.trim();
      if (!name) { notify("Name is required", "error"); return; }
      setSaving(true);
      try {
        const body = {
          name, camera_ids: form.camera_ids,
          interval_minutes: Number(form.interval_minutes) || 5,
          frame_step_seconds: Number(form.frame_step_seconds) || 30,
          analysis_alias: form.analysis_alias, active_hours: form.active_hours,
          motion_gate: !!form.motion_gate, daily_report_time: form.daily_report_time,
        };
        let path;
        if (stream) {
          path = "/ui/api/cameras/activity/streams/update";
          body.id = stream.id;
        } else {
          path = "/ui/api/cameras/activity/streams";
          body.site_id = siteId;
          body.enabled = form.enabled;
        }
        const res = await api.post(path, body);
        if (!res || !res.ok) { notify((res && res.error) || "Save failed", "error"); return; }
        // enabled is toggle-only on the update route (the watch semantics) —
        // flip it separately when the switch changed this session.
        if (stream && form.enabled !== !!stream.enabled) {
          await api.post("/ui/api/cameras/activity/streams/toggle", { id: stream.id, enabled: form.enabled });
        }
        notify(stream ? "Stream updated" : "Stream created", "success");
        onSaved();
      } catch (e) {
        notify("Save failed: " + ((e && e.message) || e), "error");
      } finally { setSaving(false); }
    }, [form, stream, siteId, notify, onSaved]);

    return (
      <Modal
        open={open}
        onClose={onClose}
        title={stream ? "Edit activity stream" : "New activity stream"}
        width={640}
        footer={
          <div className="cam-modal-footer">
            <Button variant="secondary" onClick={onClose} disabled={saving}>Cancel</Button>
            <Button variant="primary" icon="check" loading={saving} onClick={save}>{stream ? "Save" : "Create stream"}</Button>
          </div>
        }
      >
        <Field label="Name">
          <Input value={form.name} onChange={(e) => setForm((f) => ({ ...f, name: e.target.value }))} placeholder="Reception journal" disabled={saving} />
        </Field>
        <Field label="Cameras" hint="Leave empty to journal every enabled camera on this site.">
          <div className="cam-multiselect">
            {cameras.length === 0 ? (
              <p className="cfg-hint">No cameras discovered yet.</p>
            ) : cameras.map((c) => (
              <Checkbox
                key={c.id}
                checked={form.camera_ids.includes(c.id)}
                onChange={() => toggleCamera(c.id)}
                label={c.name + (c.area ? " · " + c.area : "")}
                disabled={saving}
              />
            ))}
          </div>
        </Field>
        <div className="cam-form-grid-2">
          <Field label="Interval (minutes)" hint="1–15 — how often the AI writes an entry.">
            <Input type="number" min={1} max={15} value={form.interval_minutes} onChange={(e) => setForm((f) => ({ ...f, interval_minutes: e.target.value }))} disabled={saving} />
          </Field>
          <Field label="Frame step (seconds)" hint="10..interval×60 — one frame per step per camera.">
            <Input type="number" min={10} value={form.frame_step_seconds} onChange={(e) => setForm((f) => ({ ...f, frame_step_seconds: e.target.value }))} disabled={saving} />
          </Field>
          <Field label="Analysis backend">
            <Select options={[{ value: "", label: "Use site/default" }, ...aliasOptions]} value={form.analysis_alias} onChange={(e) => setForm((f) => ({ ...f, analysis_alias: e.target.value }))} disabled={saving} />
          </Field>
          <Field label="Daily report time" hint="HH:MM site-local — when the daily report goes out.">
            <Input mono value={form.daily_report_time} onChange={(e) => setForm((f) => ({ ...f, daily_report_time: e.target.value }))} placeholder="20:00" disabled={saving} />
          </Field>
        </div>
        <Field label="Active hours" hint={'Comma-separated "HH:MM-HH:MM" ranges in server-local time. Leave blank to journal around the clock — e.g. "07:00-22:00".'}>
          <Input mono value={form.active_hours} onChange={(e) => setForm((f) => ({ ...f, active_hours: e.target.value }))} placeholder="07:00-22:00" disabled={saving} />
        </Field>
        <Switch checked={form.motion_gate} onChange={(v) => setForm((f) => ({ ...f, motion_gate: v }))} label="Motion gate — write a cheap idle entry (no AI call) when armed cameras saw no motion" />
        <Switch checked={form.enabled} onChange={(v) => setForm((f) => ({ ...f, enabled: v }))} label="Enabled" />
      </Modal>
    );
  }

  function StreamsPanel({ site, streams, cameras, aliasOptions, notify, refresh }) {
    const [formOpen, setFormOpen] = useState(false);
    const [editing, setEditing] = useState(null);
    const [deleteId, setDeleteId] = useState("");
    const [runningId, setRunningId] = useState("");

    const runNow = async (id) => {
      setRunningId(id);
      try {
        const res = await api.post("/ui/api/cameras/activity/streams/run", { id });
        if (!res || !res.ok) notify((res && res.error) || "Could not start the cycle", "error");
        else notify("Cycle started — the entry lands in the journal below", "success");
      } catch (e) {
        notify("Failed: " + ((e && e.message) || e), "error");
      } finally { setRunningId(""); }
    };
    const toggleStream = async (s) => {
      try {
        await api.post("/ui/api/cameras/activity/streams/toggle", { id: s.id, enabled: !s.enabled });
        refresh();
      } catch (e) {
        notify("Update failed: " + ((e && e.message) || e), "error");
      }
    };
    const deleteStream = async () => {
      try {
        await api.post("/ui/api/cameras/activity/streams/delete", { id: deleteId });
        notify("Stream deleted", "success");
        setDeleteId("");
        refresh();
      } catch (e) {
        notify("Delete failed: " + ((e && e.message) || e), "error");
      }
    };

    return (
      <>
        <Card
          title="Activity streams"
          description="Continuous AI journals — every few minutes the AI reads sampled frames and writes one log entry."
          actions={<Button size="sm" variant="primary" icon="plus" onClick={() => { setEditing(null); setFormOpen(true); }}>New stream</Button>}
          flush
        >
          {streams.length === 0 ? (
            <EmptyState icon="logs" title="No activity streams yet" description="Create a stream to start the continuous activity journal." compact />
          ) : (
            <div className="table-wrap">
              <table className="table">
                <thead>
                  <tr><th>Name</th><th>Cameras</th><th>Interval</th><th>Gate</th><th>Report</th><th>Last run</th><th>Enabled</th><th></th></tr>
                </thead>
                <tbody>
                  {streams.map((s) => (
                    <tr key={s.id}>
                      <td>
                        {s.name || "Untitled stream"}
                        {s.last_error && <div className="cam-hint-fail">{s.last_error}</div>}
                      </td>
                      <td className="tnum">{(s.camera_ids || []).length || "all"}</td>
                      <td className="mono">{s.interval_minutes}m / {s.frame_step_seconds}s</td>
                      <td><Badge tone={s.motion_gate ? "info" : "neutral"}>{s.motion_gate ? "motion" : "always"}</Badge></td>
                      <td className="mono">{s.daily_report_time || "20:00"}</td>
                      <td className="muted">{s.last_run_at ? ago(s.last_run_at) : "—"}</td>
                      <td><Switch checked={!!s.enabled} onChange={() => toggleStream(s)} /></td>
                      <td className="cam-row-actions">
                        <Button size="sm" variant="ghost" icon="play" loading={runningId === s.id} onClick={() => runNow(s.id)}>Run now</Button>
                        <IconButton icon="edit" label="Edit stream" onClick={() => { setEditing(s); setFormOpen(true); }} />
                        <IconButton icon="trash" label="Delete stream" onClick={() => setDeleteId(s.id)} />
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}
        </Card>

        <StreamFormModal
          open={formOpen}
          stream={editing}
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
          title="Delete this stream?"
          description="This stops the journal for this stream. Logged entries and reports are kept."
          footer={
            <div className="cam-modal-footer">
              <Button variant="secondary" onClick={() => setDeleteId("")}>Cancel</Button>
              <Button variant="danger" icon="trash" onClick={deleteStream}>Delete stream</Button>
            </div>
          }
        />
      </>
    );
  }

  /* ─────────────────────────────────────────────────────────────
     LOG BROWSER — stream + day selector, expandable entries
     ───────────────────────────────────────────────────────────── */

  // SheetThumb hides itself when the capture behind the token has been reaped
  // by media retention — old entries keep their text, not their thumbnails.
  function SheetThumb({ token, label }) {
    const [dead, setDead] = useState(false);
    if (dead) return null;
    return (
      <div className="cam-evidence-item">
        <img src={mediaURL(token)} alt={label} onError={() => setDead(true)} />
        <span className="cam-evidence-caption">{label}</span>
      </div>
    );
  }

  function EntryRow({ entry, camerasById }) {
    const [open, setOpen] = useState(false);
    const sheets = Array.isArray(entry.media_tokens) ? entry.media_tokens : [];
    const details = Array.isArray(entry.detail_tokens) ? entry.detail_tokens : [];
    const cams = (entry.camera_ids || []).map((id) => (camerasById[id] ? camerasById[id].name : id));
    return (
      <div className="cam-question">
        <div className="cam-question-head" style={{ cursor: "pointer" }} onClick={() => setOpen((o) => !o)}>
          <div>
            <p className="cam-question-text">{entry.headline || "(no headline)"}</p>
            <div className="cam-event-meta">
              <span className="mono subtle">{clock(entry.from_ts)} – {clock(entry.to_ts)}</span>
              {entry.motion_gated ? <Badge tone="neutral">idle</Badge> : <Badge tone="info">{entry.frames || 0} frames</Badge>}
              {cams.length > 0 && <span className="muted subtle">{cams.join(", ")}</span>}
              {entry.error && <Badge tone="warning">{entry.error}</Badge>}
            </div>
          </div>
          <Icon name={open ? "chevron-up" : "chevron-down"} size={14} />
        </div>
        {open && (
          <>
            <p className="cam-question-answered" style={{ whiteSpace: "pre-wrap" }}>{entry.report || "(no report)"}</p>
            {(sheets.length > 0 || details.length > 0) && (
              <div className="cam-evidence-grid">
                {sheets.map((t, i) => <SheetThumb key={t} token={t} label={"sheet " + (i + 1)} />)}
                {details.map((t, i) => <SheetThumb key={t} token={t} label={"detail " + (i + 1)} />)}
              </div>
            )}
          </>
        )}
      </div>
    );
  }

  function LogBrowser({ site, streams, camerasById, streamId, setStreamId }) {
    const [day, setDay] = useState(todayLocalDay());
    useEffect(() => { setDay(todayLocalDay()); }, [site.id]);

    const win = dayWindowUTC(day);
    const entriesLive = useLive(
      win
        ? `/ui/api/cameras/activity/entries?site_id=${encodeURIComponent(site.id)}&stream_id=${encodeURIComponent(streamId)}&from=${encodeURIComponent(win.from)}&to=${encodeURIComponent(win.to)}&limit=300`
        : "",
      { interval: 30000, enabled: !!site.id && !!win }
    );
    const entries = (entriesLive.data && entriesLive.data.entries) || [];

    const streamOptions = [
      { value: "", label: "All streams" },
      ...streams.map((s) => ({ value: s.id, label: s.name || s.id })),
    ];

    return (
      <Card
        title="Journal"
        description="The AI's continuous activity log — one entry per cycle, always English, newest first. Click an entry for the full report and its frame sheets."
        actions={
          <div className="cam-event-filters">
            <Select options={streamOptions} value={streamId} onChange={(e) => setStreamId(e.target.value)} />
            <Input type="date" mono value={day} onChange={(e) => setDay(e.target.value)} />
          </div>
        }
        flush
      >
        {entries.length === 0 ? (
          <EmptyState icon="logs" title="No entries for this day" description="Pick another day above, or enable an activity stream to start journaling." compact />
        ) : (
          <div className="cam-questions">
            {entries.map((e) => <EntryRow key={e.id} entry={e} camerasById={camerasById} />)}
          </div>
        )}
      </Card>
    );
  }

  /* ─────────────────────────────────────────────────────────────
     HOURLY SUMMARIES + DAILY REPORTS
     ───────────────────────────────────────────────────────────── */

  function HoursPanel({ site, streamId }) {
    const hoursLive = useLive(
      `/ui/api/cameras/activity/hours?site_id=${encodeURIComponent(site.id)}&stream_id=${encodeURIComponent(streamId)}&limit=48`,
      { interval: 30000, enabled: !!site.id }
    );
    const hours = (hoursLive.data && hoursLive.data.hours) || [];

    return (
      <Card title="Hourly summaries" description="Rolled up on the hour in the site's report language." flush>
        {hours.length === 0 ? (
          <EmptyState icon="clock" title="No hourly summaries yet" description="Summaries appear after an enabled stream crosses an hour boundary." compact />
        ) : (
          <div className="table-wrap">
            <table className="table">
              <thead>
                <tr><th>Hour</th><th>Status</th><th>Entries</th><th>Summary</th></tr>
              </thead>
              <tbody>
                {hours.map((h) => (
                  <tr key={h.id}>
                    <td className="mono subtle">{hourLabel(h.hour_start)}</td>
                    <td>
                      <RollupStatusBadge status={h.status} />
                      {h.error && <div className="cam-hint-fail">{h.error}</div>}
                    </td>
                    <td className="tnum">{h.entry_count}{h.gated_count ? ` (${h.gated_count} idle)` : ""}</td>
                    <td style={{ maxWidth: 480, whiteSpace: "pre-wrap" }}>{h.summary || <span className="muted">—</span>}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </Card>
    );
  }

  function DaysPanel({ site, streams, streamId, notify }) {
    const daysLive = useLive(
      `/ui/api/cameras/activity/days?site_id=${encodeURIComponent(site.id)}&stream_id=${encodeURIComponent(streamId)}&limit=60`,
      { interval: 30000, enabled: !!site.id }
    );
    const days = (daysLive.data && daysLive.data.days) || [];
    const [genDate, setGenDate] = useState(todayLocalDay());
    const [genBusy, setGenBusy] = useState("");

    const streamName = useCallback((id) => {
      const s = streams.find((x) => x.id === id);
      return s ? (s.name || s.id) : id;
    }, [streams]);

    // The header "Generate now" needs a concrete stream: the journal filter
    // selection, or the only stream when the site has exactly one.
    const genStreamId = streamId || (streams.length === 1 ? streams[0].id : "");

    // Generation runs a text-only model call inside the request — give it a
    // generous timeout instead of the 10s default.
    const generate = async (sid, date, force) => {
      setGenBusy(sid + "|" + date);
      try {
        const res = await api.post("/ui/api/cameras/activity/days/generate", { stream_id: sid, date, force }, { timeoutMs: 180000 });
        if (!res || !res.ok) { notify((res && res.error) || "Generate failed", "error"); return; }
        const rep = res.report || {};
        notify(rep.status === "empty" ? "Report generated — no entries that day" : "Daily report generated", "success");
        daysLive.refresh();
      } catch (e) {
        notify("Generate failed: " + ((e && e.message) || e), "error");
      } finally { setGenBusy(""); }
    };

    return (
      <Card
        title="Daily reports"
        description="One report per stream per site-local day — stored, shareable via the public view link, and pushed to Connect on the scheduled run."
        actions={
          <div className="cam-event-filters">
            <Input type="date" mono value={genDate} onChange={(e) => setGenDate(e.target.value)} />
            {genStreamId ? (
              <Button size="sm" variant="secondary" icon="bolt" loading={genBusy === genStreamId + "|" + genDate} onClick={() => generate(genStreamId, genDate, false)}>
                Generate now
              </Button>
            ) : (
              <span className="muted subtle">Pick a stream in the journal to generate</span>
            )}
          </div>
        }
        flush
      >
        {days.length === 0 ? (
          <EmptyState icon="book" title="No daily reports yet" description="Reports are generated at each stream's daily report time — or on demand above." compact />
        ) : (
          <div className="table-wrap">
            <table className="table">
              <thead>
                <tr><th>Day</th><th>Stream</th><th>Status</th><th>Report</th><th>Share</th><th></th></tr>
              </thead>
              <tbody>
                {days.map((d) => (
                  <tr key={d.id}>
                    <td className="mono">{d.day}</td>
                    <td className="muted">{streamName(d.stream_id)}</td>
                    <td>
                      <RollupStatusBadge status={d.status} />
                      {d.error && <div className="cam-hint-fail">{d.error}</div>}
                    </td>
                    <td style={{ maxWidth: 420 }}>
                      {d.headline || <span className="muted">—</span>}
                      {d.report && (
                        <details>
                          <summary className="muted subtle" style={{ cursor: "pointer" }}>full report</summary>
                          <p style={{ whiteSpace: "pre-wrap", margin: "6px 0 0" }}>{d.report}</p>
                        </details>
                      )}
                    </td>
                    <td className="cam-row-actions">
                      {d.view_url ? (
                        <>
                          <CopyButton text={d.view_url} label="Copy link" />
                          <IconButton icon="external" label="Open share page" onClick={() => window.open(d.view_url, "_blank", "noopener")} />
                        </>
                      ) : (
                        <span className="muted">—</span>
                      )}
                    </td>
                    <td className="cam-row-actions">
                      <Button size="sm" variant="ghost" icon="refresh" loading={genBusy === d.stream_id + "|" + d.day} onClick={() => generate(d.stream_id, d.day, true)}>Regenerate</Button>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </Card>
    );
  }

  /* ─────────────────────────────────────────────────────────────
     Tab root
     ───────────────────────────────────────────────────────────── */

  function ActivityTab({ site, cameras: camerasProp, aliasOptions: aliasProp, notify: notifyProp }) {
    const toast = useToast();
    const fallbackNotify = useCallback((msg, tone) => {
      if (!toast) return;
      if (tone === "error") toast.error(msg);
      else if (tone === "success") toast.success(msg);
      else toast.info(msg);
    }, [toast]);
    const notify = notifyProp || fallbackNotify;

    const streamsLive = useLive(`/ui/api/cameras/activity/streams?site_id=${encodeURIComponent(site.id)}`, { interval: 30000, enabled: !!site.id });
    const streams = (streamsLive.data && streamsLive.data.streams) || [];

    // cameras usually arrive as a prop from the Cameras screen; fetch them
    // here only when this tab is mounted standalone (the AvatarsTab pattern).
    const needCameras = !Array.isArray(camerasProp);
    const camerasLive = useLive(`/ui/api/cameras/list?site_id=${encodeURIComponent(site.id)}`, { interval: 30000, enabled: !!site.id && needCameras });
    const cameras = needCameras ? ((camerasLive.data && camerasLive.data.cameras) || []) : camerasProp;
    const camerasById = useMemo(() => {
      const m = {};
      cameras.forEach((c) => { m[c.id] = c; });
      return m;
    }, [cameras]);
    const aliasOptions = Array.isArray(aliasProp) ? aliasProp : [];

    // The journal's stream filter also scopes the hours/days panels and the
    // header "Generate now" target.
    const [streamId, setStreamId] = useState("");
    useEffect(() => { setStreamId(""); }, [site.id]);

    return (
      <div className="cam-tab-body">
        <StreamsPanel site={site} streams={streams} cameras={cameras} aliasOptions={aliasOptions} notify={notify} refresh={streamsLive.refresh} />
        <LogBrowser site={site} streams={streams} camerasById={camerasById} streamId={streamId} setStreamId={setStreamId} />
        <HoursPanel site={site} streamId={streamId} />
        <DaysPanel site={site} streams={streams} streamId={streamId} notify={notify} />
      </div>
    );
  }

  window.ActivityTab = ActivityTab;
})();
