// screens-analytics.jsx — Analytics: usage / latency / tokens / errors over
// selectable periods, with a flight-style dual-month custom range picker.
// Exports: window.Analytics. Props: { navigate, pushToast }
// Per-screen CSS in screens-analytics.css (.an- prefix).
(() => {
  const { useState, useEffect, useMemo, useCallback } = React;
  const {
    Icon, api, useToast,
    PageHeader, Card, StatCard, Button, IconButton, Tabs,
    AreaChart, BarChart, EmptyState, Skeleton,
    fmtCount, fmtMs, fmtPct,
  } = window;

  const cx = (...p) => p.filter(Boolean).join(" ");
  const DAY = 86400000;

  const PERIODS = [
    { id: "today", label: "Today" },
    { id: "this_week", label: "This week" },
    { id: "this_month", label: "This month" },
    { id: "last_month", label: "Last month" },
    { id: "last_30_days", label: "30 days" },
    { id: "last_6_months", label: "6 months" },
    { id: "last_year", label: "1 year" },
    { id: "custom", label: "Custom" },
  ];
  const LIVE_PERIODS = new Set(["today", "this_week", "this_month", "last_30_days"]);

  const METRICS = [
    { id: "requests", label: "Requests", tone: "accent", fmt: (v) => fmtCount(v) },
    { id: "latency", label: "Latency", tone: "info", fmt: (v) => fmtMs(v) },
    { id: "tokens", label: "Tokens", tone: "success", fmt: (v) => fmtCount(v) },
    { id: "errors", label: "Errors", tone: "danger", fmt: (v) => fmtCount(v) },
  ];

  /* ── date helpers (local time) ── */
  function startOfDay(ms) { const d = new Date(ms); d.setHours(0, 0, 0, 0); return d.getTime(); }
  function fmtDay(ms) { return new Date(ms).toLocaleDateString(undefined, { month: "short", day: "numeric" }); }
  function fmtFull(ms) { return new Date(ms).toLocaleDateString(undefined, { month: "short", day: "numeric", year: "numeric" }); }
  function fmtHour(ms) { return new Date(ms).toLocaleTimeString(undefined, { hour: "numeric" }); }
  function fmtMonth(ms) { return new Date(ms).toLocaleDateString(undefined, { month: "short", year: "2-digit" }); }
  function bucketLabel(ms, bucketMs) {
    if (bucketMs <= 3600000) return fmtHour(ms);
    if (bucketMs >= 28 * DAY) return fmtMonth(ms);
    return fmtDay(ms);
  }
  function addMonth(y, m, delta) {
    const t = m + delta;
    return { y: y + Math.floor(t / 12), m: ((t % 12) + 12) % 12 };
  }
  function periodLabel(period, range) {
    if (period === "custom" && range.from != null && range.to != null) {
      return fmtDay(range.from) + " – " + fmtDay(range.to);
    }
    const p = PERIODS.find((x) => x.id === period);
    return p ? p.label : "";
  }

  /* ════════════════════════════════════════════════════════════════
     ANALYTICS SCREEN
     ════════════════════════════════════════════════════════════════ */
  function Analytics({ pushToast }) {
    const toast = useToast();
    const notify = useCallback((m, t) => {
      if (toast) { t === "error" ? toast.error(m) : toast.info(m); }
      else if (pushToast) pushToast(m, t || "info");
    }, [toast, pushToast]);

    const [period, setPeriod] = useState("today");
    const [metric, setMetric] = useState("requests");
    const [range, setRange] = useState({ from: null, to: null }); // custom (ms, local midnight)
    const [calOpen, setCalOpen] = useState(false);
    const [data, setData] = useState(null);
    const [loading, setLoading] = useState(true);
    const [error, setError] = useState(null);

    const buildUrl = useCallback(() => {
      let url = "/ui/api/analytics?period=" + period;
      if (period === "custom") {
        if (range.from == null || range.to == null) return null;
        url += "&from=" + range.from + "&to=" + (range.to + DAY); // end day inclusive
      }
      return url;
    }, [period, range]);

    const load = useCallback(() => {
      const url = buildUrl();
      if (!url) { setData(null); setLoading(false); return; }
      setLoading(true);
      api.get(url)
        .then((res) => { setData(res); setError(null); })
        .catch((e) => { setError(e); notify("Failed to load analytics", "error"); })
        .finally(() => setLoading(false));
    }, [buildUrl, notify]);

    useEffect(() => { load(); }, [load]);

    // Light 60s poll for periods that include "now".
    useEffect(() => {
      if (!LIVE_PERIODS.has(period)) return undefined;
      const id = setInterval(() => { if (!document.hidden) load(); }, 60000);
      return () => clearInterval(id);
    }, [period, load]);

    const buckets = (data && data.buckets) || [];
    const totals = (data && data.totals) || {};
    const bucketMs = (data && data.bucket_ms) || DAY;
    const unavailable = data && data.available === false;

    const series = useMemo(() => buckets.map((b) => {
      switch (metric) {
        case "latency": return b.avg_latency_ms || 0;
        case "tokens": return (b.tokens_in || 0) + (b.tokens_out || 0);
        case "errors": return b.errors || 0;
        default: return b.requests || 0;
      }
    }), [buckets, metric]);
    const labels = useMemo(() => buckets.map((b) => bucketLabel(b.t, bucketMs)), [buckets, bucketMs]);

    const metricConf = METRICS.find((m) => m.id === metric) || METRICS[0];
    const hasData = buckets.some((b) => (b.requests || 0) > 0);
    const needDates = period === "custom" && (range.from == null || range.to == null);

    const periodTabs = PERIODS.map((p) => ({
      id: p.id,
      label: p.id === "custom" && range.from != null && range.to != null ? periodLabel("custom", range) : p.label,
    }));

    return (
      <div className="screen">
        <PageHeader
          title="Analytics"
          description="Usage, latency, tokens and errors over a period you choose."
          actions={<Button size="sm" variant="ghost" icon="refresh" onClick={load} disabled={loading}>Refresh</Button>}
        />

        <div className="an-periodbar">
          <Tabs variant="pill" active={period} onChange={setPeriod} tabs={periodTabs} />
          {period === "custom" && (
            <Button size="sm" variant="secondary" icon="clock" onClick={() => setCalOpen(true)}>
              {range.from != null ? "Change dates" : "Choose dates"}
            </Button>
          )}
        </div>

        {error && (
          <div className="alert" data-tone="danger">
            <Icon name="alert-circle" size={14} style={{ flexShrink: 0 }} />
            <span>Failed to load analytics: {error.message || String(error)}</span>
          </div>
        )}

        <div className="grid-stats">
          <StatCard label="Requests" value={loading ? "" : fmtCount(totals.requests || 0)} sub={periodLabel(period, range)} loading={loading} />
          <StatCard label="Avg latency" value={loading ? "" : fmtMs(totals.avg_latency_ms || 0)} sub="per request" loading={loading} />
          <StatCard label="Tokens" value={loading ? "" : fmtCount(totals.tokens_total || 0)} sub={"in " + fmtCount(totals.tokens_in || 0) + " · out " + fmtCount(totals.tokens_out || 0)} loading={loading} />
          <StatCard
            label="Error rate"
            value={loading ? "" : fmtPct(totals.error_rate || 0)}
            sub={(totals.errors || 0) + " error" + ((totals.errors || 0) === 1 ? "" : "s")}
            deltaTone={(totals.errors || 0) > 0 ? "danger" : "success"}
            loading={loading}
          />
        </div>

        <Card
          title="Over time"
          description={unavailable ? "Metrics store unavailable on this server." : null}
          actions={<Tabs variant="pill" active={metric} onChange={setMetric} tabs={METRICS.map((m) => ({ id: m.id, label: m.label }))} />}
        >
          {needDates ? (
            <EmptyState icon="clock" title="Pick a date range" description="Choose a start and end date to see analytics." compact />
          ) : loading ? (
            <Skeleton width="100%" height={260} style={{ borderRadius: "var(--r-md)" }} />
          ) : !hasData ? (
            <EmptyState
              icon="activity"
              title="No data for this period"
              description={unavailable ? "The metrics database isn't available on this server." : "Requests will appear here as traffic flows through the proxy. History accrues from when analytics was enabled."}
            />
          ) : (
            <AreaChart key={metric} data={series} labels={labels} height={264} tone={metricConf.tone} formatValue={metricConf.fmt} emptyLabel="No data" />
          )}
        </Card>

        {hasData && !loading && !needDates && (
          <Card title="Request volume" description="Requests per bucket.">
            <BarChart data={buckets.map((b) => ({ label: bucketLabel(b.t, bucketMs), value: b.requests || 0 }))} height={180} formatValue={(v) => fmtCount(v)} />
          </Card>
        )}

        {calOpen && (
          <RangeCalendar
            initial={range}
            onClose={() => setCalOpen(false)}
            onApply={(r) => { setRange(r); setCalOpen(false); }}
          />
        )}
      </div>
    );
  }

  /* ════════════════════════════════════════════════════════════════
     RANGE CALENDAR — flight-style dual month range picker
     ════════════════════════════════════════════════════════════════ */
  function RangeCalendar({ initial, onClose, onApply }) {
    const today = startOfDay(Date.now());
    const [start, setStart] = useState(initial.from);
    const [end, setEnd] = useState(initial.to);
    const [view, setView] = useState(() => {
      const base = initial.from != null ? new Date(initial.from) : new Date();
      return { y: base.getFullYear(), m: base.getMonth() };
    });

    useEffect(() => {
      const onKey = (e) => { if (e.key === "Escape") onClose(); };
      window.addEventListener("keydown", onKey);
      return () => window.removeEventListener("keydown", onKey);
    }, [onClose]);

    const pick = (ms) => {
      if (start == null || end != null) { setStart(ms); setEnd(null); }
      else if (ms < start) { setEnd(start); setStart(ms); }
      else { setEnd(ms); }
    };

    const right = addMonth(view.y, view.m, 1);
    const shift = (delta) => setView(addMonth(view.y, view.m, delta));

    return (
      <div className="an-cal-overlay" onMouseDown={(e) => { if (e.target === e.currentTarget) onClose(); }}>
        <div className="an-cal" role="dialog" aria-label="Select date range" aria-modal="true">
          <div className="an-cal-head">
            <IconButton icon="chevron-left" label="Previous months" onClick={() => shift(-1)} />
            <div className="an-cal-title">Select a date range</div>
            <IconButton icon="chevron-right" label="Next months" onClick={() => shift(1)} />
          </div>
          <div className="an-cal-months">
            <MonthGrid y={view.y} m={view.m} start={start} end={end} today={today} onPick={pick} />
            <MonthGrid y={right.y} m={right.m} start={start} end={end} today={today} onPick={pick} />
          </div>
          <div className="an-cal-foot">
            <div className="an-cal-range">
              <span className={cx("an-cal-pill", start != null && "set")}>{start != null ? fmtFull(start) : "Start date"}</span>
              <Icon name="arrow-right" size={13} className="subtle" />
              <span className={cx("an-cal-pill", end != null && "set")}>{end != null ? fmtFull(end) : "End date"}</span>
            </div>
            <div className="an-cal-actions">
              <Button variant="ghost" size="sm" onClick={onClose}>Cancel</Button>
              <Button variant="primary" size="sm" disabled={start == null || end == null} onClick={() => onApply({ from: start, to: end })}>Apply</Button>
            </div>
          </div>
        </div>
      </div>
    );
  }

  const DOW = ["Mo", "Tu", "We", "Th", "Fr", "Sa", "Su"];

  function MonthGrid({ y, m, start, end, today, onPick }) {
    const first = new Date(y, m, 1);
    const monthName = first.toLocaleDateString(undefined, { month: "long", year: "numeric" });
    const daysInMonth = new Date(y, m + 1, 0).getDate();
    const lead = (first.getDay() + 6) % 7; // Monday-first

    const cells = [];
    for (let i = 0; i < lead; i++) cells.push(null);
    for (let d = 1; d <= daysInMonth; d++) cells.push(new Date(y, m, d).getTime());

    return (
      <div className="an-month">
        <div className="an-month-name">{monthName}</div>
        <div className="an-dow">{DOW.map((d) => <span key={d}>{d}</span>)}</div>
        <div className="an-days">
          {cells.map((ms, i) => {
            if (ms == null) return <span key={"e" + i} className="an-day empty" />;
            const future = ms > today;
            const isEdge = (start != null && ms === start) || (end != null && ms === end);
            const inRange = start != null && end != null && ms > start && ms < end;
            return (
              <button
                key={ms}
                type="button"
                className={cx("an-day", future && "disabled", isEdge && "sel", inRange && "in-range")}
                disabled={future}
                onClick={() => onPick(ms)}
              >
                {new Date(ms).getDate()}
              </button>
            );
          })}
        </div>
      </div>
    );
  }

  window.Analytics = Analytics;
})();
