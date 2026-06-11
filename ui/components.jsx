// components.jsx — primitive component library for the control panel.
// Style only with classes + tokens from base.css. No inline hex colors.
// Exposes (on window): Button, IconButton, Card, StatCard, PageHeader, Field,
// Input, Textarea, Select, Switch, Checkbox, SegmentedControl, Badge, Tabs,
// Modal, ToastProvider, useToast, EmptyState, Skeleton, Spinner, CopyButton,
// SecretField, KeyValue, CodeBlock, Sparkline, AreaChart, BarChart,
// ErrorBoundary, CommandPalette
(() => {
  const { useState, useEffect, useRef, useMemo, useCallback, useContext } = React;
  const Icon = window.Icon;
  const fmtCount = window.fmtCount;

  let uidCounter = 0;
  const nextUid = (prefix) => prefix + "-" + (++uidCounter);

  const cx = (...parts) => parts.filter(Boolean).join(" ");

  const TONE_COLORS = {
    accent: "var(--accent)",
    success: "var(--success)",
    warning: "var(--warning)",
    danger: "var(--danger)",
    info: "var(--info)",
    neutral: "var(--text-3)",
  };
  const toneColor = (tone) => TONE_COLORS[tone] || TONE_COLORS.accent;

  /* ────────────────────────────────────────────
     Buttons
     ──────────────────────────────────────────── */
  function Button({ variant = "secondary", size = "md", loading = false, icon, block, className, children, disabled, type = "button", ...props }) {
    return (
      <button
        type={type}
        className={cx("btn", "btn-" + variant, size === "sm" && "btn-sm", size === "lg" && "btn-lg", block && "btn-block", className)}
        disabled={disabled || loading}
        {...props}
      >
        {loading ? <Spinner size={14} /> : icon ? <Icon name={icon} size={size === "sm" ? 12 : 14} /> : null}
        {children}
      </button>
    );
  }

  function IconButton({ icon, label, size = 15, bordered, className, ...props }) {
    return (
      <button
        type="button"
        className={cx("icon-btn", bordered && "icon-btn-bordered", className)}
        aria-label={label}
        data-tip={label || undefined}
        {...props}
      >
        <Icon name={icon} size={size} />
      </button>
    );
  }

  /* ────────────────────────────────────────────
     Card + StatCard + PageHeader
     ──────────────────────────────────────────── */
  function Card({ title, description, actions, footer, flush, className, children }) {
    return (
      <section className={cx("card", className)}>
        {(title || description || actions) && (
          <header className="card-header">
            <div className="card-heading">
              {title && <h3 className="card-title">{title}</h3>}
              {description && <p className="card-desc">{description}</p>}
            </div>
            {actions && <div className="card-actions">{actions}</div>}
          </header>
        )}
        <div className={cx("card-body", flush && "flush")}>{children}</div>
        {footer && <footer className="card-footer">{footer}</footer>}
      </section>
    );
  }

  // delta: number → formatted as a % delta with tone inferred from sign,
  //        string → rendered as-is (pass deltaTone to color it).
  function StatCard({ label, value, delta, deltaTone, sub, spark, sparkTone = "accent", loading, className }) {
    let deltaNode = null;
    if (delta != null && delta !== "") {
      let text = delta;
      let tone = deltaTone;
      if (typeof delta === "number") {
        text = window.fmtDelta(delta, "%");
        if (!tone) tone = delta > 0 ? "success" : delta < 0 ? "danger" : "neutral";
      }
      deltaNode = <Badge tone={tone || "neutral"}>{text}</Badge>;
    }
    return (
      <div className={cx("card stat-card", className)}>
        <div className="stat-top">
          <span className="stat-label">{label}</span>
          {deltaNode}
        </div>
        <div className="stat-main">
          {loading ? <Skeleton width={88} height={30} /> : <span className="stat-value tnum">{value}</span>}
          {spark && spark.length > 0 && <Sparkline data={spark} tone={sparkTone} />}
        </div>
        {sub && <div className="stat-sub">{sub}</div>}
      </div>
    );
  }

  function PageHeader({ title, description, actions, children }) {
    return (
      <div className="page-header">
        <div className="page-header-row">
          <div className="page-header-text">
            <h1 className="page-title">{title}</h1>
            {description && <p className="page-desc">{description}</p>}
          </div>
          {actions && <div className="page-actions">{actions}</div>}
        </div>
        {children}
      </div>
    );
  }

  /* ────────────────────────────────────────────
     Form controls
     ──────────────────────────────────────────── */
  function Field({ label, hint, error, htmlFor, mono, className, children }) {
    return (
      <div className={cx("field", error && "field-invalid", className)}>
        {label && <label className="field-label" htmlFor={htmlFor}>{label}</label>}
        {children}
        {hint && !error && <div className={cx("field-hint", mono && "mono")}>{hint}</div>}
        {error && (
          <div className="field-error">
            <Icon name="alert-circle" size={12} /> {error}
          </div>
        )}
      </div>
    );
  }

  const Input = React.forwardRef(function Input({ className, mono, invalid, ...props }, ref) {
    return (
      <input
        ref={ref}
        className={cx("input", mono && "mono", invalid && "invalid", className)}
        aria-invalid={invalid || undefined}
        {...props}
      />
    );
  });

  const Textarea = React.forwardRef(function Textarea({ className, mono, invalid, ...props }, ref) {
    return (
      <textarea
        ref={ref}
        className={cx("textarea", mono && "mono", invalid && "invalid", className)}
        aria-invalid={invalid || undefined}
        {...props}
      />
    );
  });

  // options: ["a", "b"] or [{value, label, disabled}] — or pass <option> children.
  function Select({ className, options, children, ...props }) {
    return (
      <div className={cx("select-wrap", className)}>
        <select className="select" {...props}>
          {options
            ? options.map((o) =>
                typeof o === "object" ? (
                  <option key={o.value} value={o.value} disabled={o.disabled}>
                    {o.label != null ? o.label : o.value}
                  </option>
                ) : (
                  <option key={o} value={o}>{o}</option>
                )
              )
            : children}
        </select>
        <Icon name="chevron" size={12} className="select-caret" />
      </div>
    );
  }

  // Real role="switch" — keyboard accessible (button => Space/Enter native).
  function Switch({ checked, onChange, disabled, label, id }) {
    const btn = (
      <button
        type="button"
        role="switch"
        aria-checked={!!checked}
        className="switch"
        disabled={disabled}
        id={id}
        onClick={() => onChange && onChange(!checked)}
      >
        <span className="switch-thumb" />
      </button>
    );
    if (!label) return btn;
    return (
      <label className="switch-row">
        {btn}
        <span className="switch-label">{label}</span>
      </label>
    );
  }

  function Checkbox({ checked, onChange, disabled, label, id }) {
    return (
      <label className={cx("checkbox", disabled && "disabled")}>
        <input
          type="checkbox"
          id={id}
          checked={!!checked}
          disabled={disabled}
          onChange={(e) => onChange && onChange(e.target.checked)}
        />
        <span className="checkbox-box"><Icon name="check" size={10} /></span>
        {label && <span className="checkbox-label">{label}</span>}
      </label>
    );
  }

  // options: ["a"] or [{value, label, icon}]
  function SegmentedControl({ options, value, onChange, size }) {
    return (
      <div className={cx("segmented", size === "sm" && "segmented-sm")}>
        {(options || []).map((o) => {
          const opt = typeof o === "object" ? o : { value: o, label: o };
          const active = opt.value === value;
          return (
            <button
              key={opt.value}
              type="button"
              className={cx("segmented-item", active && "active")}
              aria-pressed={active}
              onClick={() => onChange && onChange(opt.value)}
            >
              {opt.icon && <Icon name={opt.icon} size={13} />}
              {opt.label != null ? opt.label : opt.value}
            </button>
          );
        })}
      </div>
    );
  }

  /* ────────────────────────────────────────────
     Badge + Tabs
     ──────────────────────────────────────────── */
  function Badge({ tone = "neutral", pulse, className, children }) {
    return (
      <span className={cx("badge", className)} data-tone={tone}>
        {pulse && <span className="badge-dot" />}
        {children}
      </span>
    );
  }

  // tabs: [{id, label, icon?, badge?}], variant: undefined | "pill"
  function Tabs({ tabs, active, onChange, variant }) {
    return (
      <div className={cx("tabs", variant === "pill" && "tabs-pill")} role="tablist">
        {(tabs || []).map((t) => {
          const tab = typeof t === "object" ? t : { id: t, label: t };
          const is = tab.id === active;
          return (
            <button
              key={tab.id}
              type="button"
              role="tab"
              aria-selected={is}
              className={cx("tab", is && "active")}
              onClick={() => onChange && onChange(tab.id)}
            >
              {tab.icon && <Icon name={tab.icon} size={13} />}
              {tab.label}
              {tab.badge != null && <span className="tab-badge tnum">{tab.badge}</span>}
            </button>
          );
        })}
      </div>
    );
  }

  /* ────────────────────────────────────────────
     Modal — Esc + backdrop close
     ──────────────────────────────────────────── */
  function Modal({ open, onClose, title, description, footer, width = 480, children }) {
    const dialogRef = useRef(null);
    useEffect(() => {
      if (!open) return undefined;
      const onKey = (e) => { if (e.key === "Escape") { e.stopPropagation(); onClose && onClose(); } };
      window.addEventListener("keydown", onKey);
      requestAnimationFrame(() => { if (dialogRef.current) dialogRef.current.focus(); });
      return () => window.removeEventListener("keydown", onKey);
    }, [open, onClose]);
    if (!open) return null;
    return ReactDOM.createPortal(
      <div
        className="modal-backdrop"
        onMouseDown={(e) => { if (e.target === e.currentTarget && onClose) onClose(); }}
      >
        <div className="modal" style={{ maxWidth: width }} role="dialog" aria-modal="true" aria-label={typeof title === "string" ? title : undefined} tabIndex={-1} ref={dialogRef}>
          {(title || description) && (
            <div className="modal-header">
              <div className="modal-heading">
                {title && <h2 className="modal-title">{title}</h2>}
                {description && <p className="modal-desc">{description}</p>}
              </div>
              <IconButton icon="x" label="Close" onClick={onClose} />
            </div>
          )}
          <div className="modal-body">{children}</div>
          {footer && <div className="modal-footer">{footer}</div>}
        </div>
      </div>,
      document.body
    );
  }

  /* ────────────────────────────────────────────
     Toasts — top-right, 4s, success/error/info
     ──────────────────────────────────────────── */
  const ToastContext = React.createContext(null);
  const TOAST_ICONS = { success: "check", error: "alert-circle", info: "info" };

  function ToastProvider({ children }) {
    const [toasts, setToasts] = useState([]);
    const idRef = useRef(0);

    const dismiss = useCallback((id) => {
      setToasts((ts) => ts.filter((t) => t.id !== id));
    }, []);

    const push = useCallback((message, opts = {}) => {
      const id = ++idRef.current;
      const tone = opts.tone && TOAST_ICONS[opts.tone] ? opts.tone : "info";
      setToasts((ts) => [...ts.slice(-4), { id, message, tone }]);
      setTimeout(() => dismiss(id), opts.duration || 4000);
      return id;
    }, [dismiss]);

    const value = useMemo(() => ({
      push,
      dismiss,
      success: (m, o) => push(m, { ...o, tone: "success" }),
      error: (m, o) => push(m, { ...o, tone: "error" }),
      info: (m, o) => push(m, { ...o, tone: "info" }),
    }), [push, dismiss]);

    return (
      <ToastContext.Provider value={value}>
        {children}
        <div className="toast-region" role="status" aria-live="polite">
          {toasts.map((t) => (
            <div key={t.id} className="toast" data-tone={t.tone}>
              <span className="toast-icon"><Icon name={TOAST_ICONS[t.tone]} size={15} /></span>
              <span className="toast-message">{t.message}</span>
              <button className="toast-close" onClick={() => dismiss(t.id)} aria-label="Dismiss">
                <Icon name="x" size={12} />
              </button>
            </div>
          ))}
        </div>
      </ToastContext.Provider>
    );
  }

  function useToast() {
    return useContext(ToastContext);
  }

  /* ────────────────────────────────────────────
     Empty state, skeleton, spinner
     ──────────────────────────────────────────── */
  function EmptyState({ icon = "info", title, description, action, compact }) {
    return (
      <div className={cx("empty-state", compact && "compact")}>
        <div className="empty-icon"><Icon name={icon} size={20} /></div>
        {title && <h3 className="empty-title">{title}</h3>}
        {description && <p className="empty-desc">{description}</p>}
        {action && <div className="empty-action">{action}</div>}
      </div>
    );
  }

  function Skeleton({ width = "100%", height = 14, className, style }) {
    return <span className={cx("skeleton", className)} style={{ width, height, ...style }} aria-hidden="true" />;
  }

  function Spinner({ size = 16, className }) {
    return <span className={cx("spinner", className)} style={{ width: size, height: size }} aria-hidden="true" />;
  }

  /* ────────────────────────────────────────────
     CopyButton + SecretField
     ──────────────────────────────────────────── */
  function CopyButton({ text, label, size = "sm", variant = "ghost", className }) {
    const [copied, setCopied] = useState(false);
    const timerRef = useRef(null);
    useEffect(() => () => clearTimeout(timerRef.current), []);
    const copy = () => {
      const value = typeof text === "function" ? text() : text;
      if (navigator.clipboard) navigator.clipboard.writeText(value == null ? "" : String(value)).catch(() => {});
      setCopied(true);
      clearTimeout(timerRef.current);
      timerRef.current = setTimeout(() => setCopied(false), 1400);
    };
    if (!label) {
      return (
        <button type="button" className={cx("icon-btn", className)} data-tip={copied ? "Copied" : "Copy"} aria-label="Copy" onClick={copy}>
          <Icon name={copied ? "check" : "copy"} size={13} />
        </button>
      );
    }
    return (
      <Button variant={variant} size={size} icon={copied ? "check" : "copy"} className={className} onClick={copy}>
        {copied ? "Copied" : label}
      </Button>
    );
  }

  function SecretField({ value, copyValue, placeholder = "Not set", className }) {
    const [shown, setShown] = useState(false);
    const text = value || "";
    const clip = copyValue || text;
    const hasValue = !!(text || clip);
    const masked = "•".repeat(Math.max(10, Math.min(26, (text || clip).length || 10)));
    return (
      <div className={cx("secret-field", className)}>
        <span className="secret-value mono">{hasValue ? (shown ? clip : masked) : placeholder}</span>
        <span className="secret-actions">
          <button
            type="button"
            className="icon-btn"
            data-tip={shown ? "Hide" : "Reveal"}
            aria-label={shown ? "Hide value" : "Reveal value"}
            onClick={() => setShown((s) => !s)}
          >
            <Icon name={shown ? "eye-off" : "eye"} size={13} />
          </button>
          <CopyButton text={clip} />
        </span>
      </div>
    );
  }

  /* ────────────────────────────────────────────
     KeyValue grid — items: [{label, value, mono?}]
     ──────────────────────────────────────────── */
  function KeyValue({ items, columns = 1, className }) {
    return (
      <dl className={cx("kv", columns === 2 && "kv-2", className)}>
        {(items || []).map((it, i) => (
          <React.Fragment key={(it.label != null ? it.label : it.key) + "-" + i}>
            <dt>{it.label != null ? it.label : it.key}</dt>
            <dd className={it.mono ? "mono" : undefined}>{it.value}</dd>
          </React.Fragment>
        ))}
      </dl>
    );
  }

  /* ────────────────────────────────────────────
     CodeBlock — SAFE highlighter (React elements,
     no dangerouslySetInnerHTML). modes: json | shell | text
     ──────────────────────────────────────────── */
  function highlightJson(src) {
    const out = [];
    let key = 0;
    const re = /("(?:\\.|[^"\\])*")(\s*:)?|(-?\b\d+(?:\.\d+)?(?:[eE][+-]?\d+)?\b)|(\btrue\b|\bfalse\b|\bnull\b)/g;
    let last = 0;
    let m;
    while ((m = re.exec(src))) {
      if (m.index > last) out.push(src.slice(last, m.index));
      if (m[1] !== undefined) {
        out.push(<span key={key++} className={m[2] ? "tok-key" : "tok-str"}>{m[1]}</span>);
        if (m[2]) out.push(m[2]);
      } else if (m[3] !== undefined) {
        out.push(<span key={key++} className="tok-num">{m[3]}</span>);
      } else if (m[4] !== undefined) {
        out.push(<span key={key++} className="tok-kw">{m[4]}</span>);
      }
      last = m.index + m[0].length;
    }
    if (last < src.length) out.push(src.slice(last));
    return out;
  }

  function highlightShell(src) {
    const out = [];
    let key = 0;
    src.split("\n").forEach((line, li) => {
      if (li > 0) out.push("\n");
      if (line.trimStart().startsWith("#")) {
        out.push(<span key={key++} className="tok-comment">{line}</span>);
        return;
      }
      const re = /("(?:\\.|[^"\\])*"|'[^']*')|(\$\{?[A-Za-z_][A-Za-z0-9_]*\}?)|(#.*$)|((?:^|\s)-{1,2}[A-Za-z0-9][\w.-]*)/g;
      let last = 0;
      let m;
      while ((m = re.exec(line))) {
        if (m.index > last) out.push(line.slice(last, m.index));
        if (m[1]) out.push(<span key={key++} className="tok-str">{m[1]}</span>);
        else if (m[2]) out.push(<span key={key++} className="tok-key">{m[2]}</span>);
        else if (m[3]) out.push(<span key={key++} className="tok-comment">{m[3]}</span>);
        else if (m[4]) out.push(<span key={key++} className="tok-num">{m[4]}</span>);
        last = m.index + m[0].length;
      }
      if (last < line.length) out.push(line.slice(last));
    });
    return out;
  }

  function CodeBlock({ code, mode = "text", title, collapsible = false, defaultCollapsed = false, maxHeight, className }) {
    const [collapsed, setCollapsed] = useState(collapsible ? defaultCollapsed : false);
    const text = code == null ? "" : typeof code === "string" ? code : JSON.stringify(code, null, 2);
    const content = useMemo(() => {
      if (mode === "json") return highlightJson(text);
      if (mode === "shell") return highlightShell(text);
      return text;
    }, [text, mode]);
    return (
      <div className={cx("code-block", collapsed && "collapsed", className)}>
        <div className="code-block-bar">
          <span className="code-block-title">{title || mode}</span>
          <div className="code-block-actions">
            {collapsible && (
              <button type="button" className="code-block-toggle" onClick={() => setCollapsed((c) => !c)}>
                <Icon name={collapsed ? "chevron-right" : "chevron-down"} size={12} />
                {collapsed ? "Expand" : "Collapse"}
              </button>
            )}
            <CopyButton text={text} />
          </div>
        </div>
        {!collapsed && (
          <pre style={maxHeight ? { maxHeight, overflow: "auto" } : undefined}>
            <code>{content}</code>
          </pre>
        )}
      </div>
    );
  }

  /* ────────────────────────────────────────────
     Charts — shared geometry helpers
     ──────────────────────────────────────────── */
  function normalizeSeries(data) {
    return (Array.isArray(data) ? data : []).map((d) =>
      typeof d === "number" ? d : d && typeof d.value === "number" ? d.value : 0
    );
  }

  // Monotone cubic (Fritsch–Carlson) — smooth without overshoot.
  function monotonePath(pts) {
    const n = pts.length;
    if (n === 0) return "";
    const f = (v) => Math.round(v * 100) / 100;
    if (n === 1) return `M${f(pts[0].x)},${f(pts[0].y)}`;
    const dx = [], slope = [];
    for (let i = 0; i < n - 1; i++) {
      const h = pts[i + 1].x - pts[i].x || 1e-6;
      dx.push(h);
      slope.push((pts[i + 1].y - pts[i].y) / h);
    }
    const tan = [slope[0]];
    for (let i = 1; i < n - 1; i++) {
      tan.push(slope[i - 1] * slope[i] <= 0 ? 0 : (slope[i - 1] + slope[i]) / 2);
    }
    tan.push(slope[n - 2]);
    for (let i = 0; i < n - 1; i++) {
      if (slope[i] === 0) { tan[i] = 0; tan[i + 1] = 0; continue; }
      const a = tan[i] / slope[i];
      const b = tan[i + 1] / slope[i];
      const s = a * a + b * b;
      if (s > 9) {
        const tau = 3 / Math.sqrt(s);
        tan[i] = tau * a * slope[i];
        tan[i + 1] = tau * b * slope[i];
      }
    }
    let d = `M${f(pts[0].x)},${f(pts[0].y)}`;
    for (let i = 0; i < n - 1; i++) {
      const c1x = pts[i].x + dx[i] / 3;
      const c1y = pts[i].y + (tan[i] * dx[i]) / 3;
      const c2x = pts[i + 1].x - dx[i] / 3;
      const c2y = pts[i + 1].y - (tan[i + 1] * dx[i]) / 3;
      d += `C${f(c1x)},${f(c1y)} ${f(c2x)},${f(c2y)} ${f(pts[i + 1].x)},${f(pts[i + 1].y)}`;
    }
    return d;
  }

  function niceTicks(min, max, count) {
    if (!(max > min)) return [min, max];
    const step0 = (max - min) / Math.max(1, count);
    const mag = Math.pow(10, Math.floor(Math.log10(step0)));
    const norm = step0 / mag;
    const step = (norm >= 5 ? 10 : norm >= 2 ? 5 : norm >= 1 ? 2 : 1) * mag;
    const lo = Math.floor(min / step) * step;
    const hi = Math.ceil(max / step) * step;
    const ticks = [];
    for (let v = lo; v <= hi + step * 1e-6; v += step) ticks.push(Math.round(v * 1e6) / 1e6);
    return ticks;
  }

  function useMeasuredWidth() {
    const ref = useRef(null);
    const [width, setWidth] = useState(0);
    useEffect(() => {
      const el = ref.current;
      if (!el) return undefined;
      setWidth(el.clientWidth);
      if (typeof ResizeObserver === "undefined") return undefined;
      const ro = new ResizeObserver((entries) => {
        const w = entries[0] && entries[0].contentRect.width;
        if (w != null) setWidth(w);
      });
      ro.observe(el);
      return () => ro.disconnect();
    }, []);
    return [ref, width];
  }

  /* ────────────────────────────────────────────
     Sparkline — smooth path + gradient fill (~100x28)
     ──────────────────────────────────────────── */
  function Sparkline({ data, width = 100, height = 28, tone = "accent", className }) {
    const gradId = useRef(nextUid("spark")).current;
    const values = normalizeSeries(data);
    const color = toneColor(tone);
    if (values.length === 0) {
      return <svg className={cx("sparkline", className)} width={width} height={height} aria-hidden="true" />;
    }
    let min = Math.min(...values);
    let max = Math.max(...values);
    if (min === max) { min -= 1; max += 1; }
    const padY = 2;
    const innerH = height - padY * 2;
    const pts = values.map((v, i) => ({
      x: values.length === 1 ? width / 2 : (i / (values.length - 1)) * width,
      y: padY + innerH - ((v - min) / (max - min)) * innerH,
    }));
    const line = monotonePath(pts);
    const area = `${line}L${Math.round(pts[pts.length - 1].x * 100) / 100},${height} L${Math.round(pts[0].x * 100) / 100},${height} Z`;
    return (
      <svg className={cx("sparkline", className)} width={width} height={height} viewBox={`0 0 ${width} ${height}`} aria-hidden="true">
        <defs>
          <linearGradient id={gradId} x1="0" y1="0" x2="0" y2="1">
            <stop offset="0%" style={{ stopColor: color, stopOpacity: 0.28 }} />
            <stop offset="100%" style={{ stopColor: color, stopOpacity: 0 }} />
          </linearGradient>
        </defs>
        <path d={area} fill={`url(#${gradId})`} stroke="none" />
        <path d={line} fill="none" style={{ stroke: color }} strokeWidth="1.5" strokeLinecap="round" strokeLinejoin="round" />
      </svg>
    );
  }

  /* ────────────────────────────────────────────
     AreaChart — measured width, monotone curve,
     gradient fill, gridlines, hover tooltip.
     data: [number] or [{value, label}]; labels: optional [string]
     ──────────────────────────────────────────── */
  function AreaChart({ data, labels, height = 220, tone = "accent", formatValue, formatLabel, emptyLabel = "No data yet", className }) {
    const [wrapRef, width] = useMeasuredWidth();
    const [hover, setHover] = useState(null);
    const gradId = useRef(nextUid("area")).current;

    const values = useMemo(() => normalizeSeries(data), [data]);
    const xLabels = useMemo(() => {
      if (labels) return labels;
      return (Array.isArray(data) ? data : []).map((d) => (d && d.label != null ? d.label : ""));
    }, [data, labels]);
    const fmtV = formatValue || ((v) => (fmtCount ? fmtCount(v) : String(v)));
    const fmtL = formatLabel || ((l) => l);
    const color = toneColor(tone);

    const pad = { top: 10, right: 12, bottom: 22, left: 46 };
    const w = Math.max(0, width);
    const innerW = Math.max(1, w - pad.left - pad.right);
    const innerH = Math.max(1, height - pad.top - pad.bottom);
    const n = values.length;
    const empty = n === 0;

    let yMin = 0;
    let yMax = 1;
    let ticks = [];
    if (!empty) {
      let min = Math.min(...values);
      let max = Math.max(...values);
      if (min === max) {
        // flat series → centered line
        const spread = Math.abs(min) * 0.5 || 1;
        yMin = min - spread;
        yMax = max + spread;
        ticks = [yMin, min, yMax];
      } else {
        const lo = min >= 0 ? 0 : min;
        ticks = niceTicks(lo, max, 3);
        yMin = ticks[0];
        yMax = ticks[ticks.length - 1];
      }
    }

    const xAt = (i) => pad.left + (n <= 1 ? innerW / 2 : (i / (n - 1)) * innerW);
    const yAt = (v) => pad.top + innerH - ((v - yMin) / (yMax - yMin || 1)) * innerH;

    const pts = values.map((v, i) => ({ x: xAt(i), y: yAt(v) }));
    const line = empty
      ? `M${pad.left},${pad.top + innerH} L${pad.left + innerW},${pad.top + innerH}` // flat baseline
      : monotonePath(pts);
    const area = empty || pts.length === 0
      ? ""
      : `${line}L${Math.round(pts[pts.length - 1].x * 100) / 100},${pad.top + innerH} L${Math.round(pts[0].x * 100) / 100},${pad.top + innerH} Z`;

    // pick ~4 x labels
    const xTickIdx = useMemo(() => {
      if (n === 0) return [];
      if (n <= 4) return values.map((_, i) => i);
      return [0, Math.round((n - 1) / 3), Math.round(((n - 1) * 2) / 3), n - 1].filter((v, i, a) => a.indexOf(v) === i);
    }, [n, values]);

    const onMove = (e) => {
      if (empty || n === 0 || w === 0) return;
      const rect = e.currentTarget.getBoundingClientRect();
      const px = e.clientX - rect.left;
      const t = n <= 1 ? 0 : (px - pad.left) / innerW;
      const i = Math.max(0, Math.min(n - 1, Math.round(t * (n - 1))));
      setHover(i);
    };

    const hoverPt = hover != null && pts[hover] ? pts[hover] : null;
    const tooltipLeft = hoverPt ? Math.max(4, Math.min(hoverPt.x + 12, w - 130)) : 0;
    const tooltipTop = hoverPt ? Math.max(4, hoverPt.y - 48) : 0;

    return (
      <div className={cx("chart-wrap", className)} ref={wrapRef} style={{ height }}>
        {w > 0 && (
          <svg width={w} height={height} viewBox={`0 0 ${w} ${height}`} onMouseMove={onMove} onMouseLeave={() => setHover(null)}>
            <defs>
              <linearGradient id={gradId} x1="0" y1="0" x2="0" y2="1">
                <stop offset="0%" style={{ stopColor: color, stopOpacity: 0.22 }} />
                <stop offset="100%" style={{ stopColor: color, stopOpacity: 0 }} />
              </linearGradient>
            </defs>

            {/* gridlines + y labels */}
            {ticks.map((t, i) => (
              <g key={"t" + i}>
                <line className="chart-gridline" x1={pad.left} x2={pad.left + innerW} y1={yAt(t)} y2={yAt(t)} />
                <text className="chart-axis-label" x={pad.left - 8} y={yAt(t) + 3.5} textAnchor="end">{fmtV(t)}</text>
              </g>
            ))}
            {empty && <line className="chart-gridline" x1={pad.left} x2={pad.left + innerW} y1={pad.top + innerH} y2={pad.top + innerH} />}

            {/* x labels */}
            {xTickIdx.map((i) =>
              xLabels[i] ? (
                <text
                  key={"x" + i}
                  className="chart-axis-label"
                  x={xAt(i)}
                  y={height - 6}
                  textAnchor={i === 0 ? "start" : i === n - 1 ? "end" : "middle"}
                >
                  {fmtL(xLabels[i])}
                </text>
              ) : null
            )}

            {/* area + line */}
            {area && <path d={area} fill={`url(#${gradId})`} stroke="none" />}
            <path d={line} fill="none" style={{ stroke: color }} strokeWidth="2" strokeLinecap="round" strokeLinejoin="round" />

            {/* hover guide + dot */}
            {hoverPt && (
              <g>
                <line className="chart-guide" x1={hoverPt.x} x2={hoverPt.x} y1={pad.top} y2={pad.top + innerH} />
                <circle cx={hoverPt.x} cy={hoverPt.y} r="4" style={{ fill: color, stroke: "var(--surface)" }} strokeWidth="2" />
              </g>
            )}
          </svg>
        )}
        {empty && <div className="chart-empty-label">{emptyLabel}</div>}
        {hoverPt && (
          <div className="chart-tooltip" style={{ left: tooltipLeft, top: tooltipTop }}>
            {xLabels[hover] ? <div className="chart-tooltip-label">{fmtL(xLabels[hover])}</div> : null}
            <div className="chart-tooltip-value">{fmtV(values[hover])}</div>
          </div>
        )}
      </div>
    );
  }

  /* ────────────────────────────────────────────
     BarChart — rounded bars + tooltip.
     data: [{label, value, tone?}]
     ──────────────────────────────────────────── */
  function BarChart({ data, height = 180, tone = "accent", formatValue, className }) {
    const [wrapRef, width] = useMeasuredWidth();
    const [hover, setHover] = useState(null);
    const items = Array.isArray(data) ? data : [];
    const fmtV = formatValue || ((v) => (fmtCount ? fmtCount(v) : String(v)));

    const pad = { top: 8, right: 8, bottom: 22, left: 8 };
    const w = Math.max(0, width);
    const innerW = Math.max(1, w - pad.left - pad.right);
    const innerH = Math.max(1, height - pad.top - pad.bottom);
    const n = items.length;
    const max = Math.max(1e-9, ...items.map((d) => d.value || 0));

    const slot = n > 0 ? innerW / n : innerW;
    const barW = Math.max(6, Math.min(48, slot * 0.6));

    const roundedBar = (x, y, bw, bh) => {
      if (bh <= 0) return "";
      const r = Math.min(5, bw / 2, bh);
      const f = (v) => Math.round(v * 100) / 100;
      return `M${f(x)},${f(y + bh)} L${f(x)},${f(y + r)} Q${f(x)},${f(y)} ${f(x + r)},${f(y)} L${f(x + bw - r)},${f(y)} Q${f(x + bw)},${f(y)} ${f(x + bw)},${f(y + r)} L${f(x + bw)},${f(y + bh)} Z`;
    };

    const hoverItem = hover != null ? items[hover] : null;
    const hoverX = hover != null ? pad.left + hover * slot + slot / 2 : 0;

    return (
      <div className={cx("chart-wrap", className)} ref={wrapRef} style={{ height }}>
        {w > 0 && (
          <svg width={w} height={height} viewBox={`0 0 ${w} ${height}`} onMouseLeave={() => setHover(null)}>
            <line className="chart-gridline" x1={pad.left} x2={pad.left + innerW} y1={pad.top + innerH} y2={pad.top + innerH} />
            {items.map((d, i) => {
              const v = Math.max(0, d.value || 0);
              const bh = (v / max) * (innerH - 2);
              const x = pad.left + i * slot + (slot - barW) / 2;
              const y = pad.top + innerH - bh;
              const color = toneColor(d.tone || tone);
              return (
                <g key={i} onMouseEnter={() => setHover(i)}>
                  {/* hit area */}
                  <rect x={pad.left + i * slot} y={pad.top} width={slot} height={innerH} fill="transparent" />
                  <path d={roundedBar(x, y, barW, Math.max(bh, v > 0 ? 2 : 0))} style={{ fill: color, opacity: hover == null || hover === i ? 1 : 0.45 }} />
                  {d.label != null && (
                    <text className="chart-axis-label" x={pad.left + i * slot + slot / 2} y={height - 6} textAnchor="middle">{d.label}</text>
                  )}
                </g>
              );
            })}
          </svg>
        )}
        {n === 0 && <div className="chart-empty-label">No data yet</div>}
        {hoverItem && (
          <div className="chart-tooltip" style={{ left: Math.max(4, Math.min(hoverX + 10, w - 110)), top: 6 }}>
            {hoverItem.label != null && <div className="chart-tooltip-label">{hoverItem.label}</div>}
            <div className="chart-tooltip-value">{fmtV(hoverItem.value || 0)}</div>
          </div>
        )}
      </div>
    );
  }

  /* ────────────────────────────────────────────
     ErrorBoundary
     ──────────────────────────────────────────── */
  class ErrorBoundary extends React.Component {
    constructor(props) {
      super(props);
      this.state = { error: null };
    }
    static getDerivedStateFromError(error) {
      return { error };
    }
    componentDidCatch(error, info) {
      console.error("Control panel screen crashed:", error, info);
    }
    render() {
      if (this.state.error) {
        const msg = (this.state.error && this.state.error.message) || String(this.state.error);
        return (
          <div className="card error-boundary">
            <div className="card-body">
              <div className="error-boundary-icon"><Icon name="alert-circle" size={20} /></div>
              <h3>Something went wrong on this screen</h3>
              <p className="mono">{msg}</p>
              <Button variant="secondary" icon="refresh" onClick={() => window.location.reload()}>
                Reload panel
              </Button>
            </div>
          </div>
        );
      }
      return this.props.children;
    }
  }

  /* ────────────────────────────────────────────
     CommandPalette — Ctrl/⌘+K
     items = ROUTES (pages) + actions [{id,label,icon,hint?,run}]
     ──────────────────────────────────────────── */
  function CommandPalette({ open, onClose, actions = [], onNavigate }) {
    const [query, setQuery] = useState("");
    const [sel, setSel] = useState(0);
    const inputRef = useRef(null);
    const listRef = useRef(null);

    const items = useMemo(() => {
      const routeItems = (window.ROUTES || []).map((r) => ({
        key: "route:" + r.id,
        group: "Pages",
        icon: r.icon,
        label: r.title,
        hint: r.path,
        run: () => { if (onNavigate) onNavigate(r.id); },
      }));
      const actionItems = actions.map((a) => ({
        key: "action:" + a.id,
        group: "Actions",
        icon: a.icon,
        label: a.label,
        hint: a.hint,
        run: a.run,
      }));
      const all = [...routeItems, ...actionItems];
      const q = query.trim().toLowerCase();
      if (!q) return all;
      return all.filter(
        (it) => it.label.toLowerCase().includes(q) || (it.hint || "").toLowerCase().includes(q)
      );
    }, [query, actions, onNavigate]);

    useEffect(() => {
      if (open) {
        setQuery("");
        setSel(0);
        requestAnimationFrame(() => { if (inputRef.current) inputRef.current.focus(); });
      }
    }, [open]);

    useEffect(() => { setSel(0); }, [query]);

    useEffect(() => {
      const el = listRef.current && listRef.current.querySelector(".cmdk-item.selected");
      if (el && el.scrollIntoView) el.scrollIntoView({ block: "nearest" });
    }, [sel, items.length]);

    // Close on Escape even when focus has left the input.
    useEffect(() => {
      if (!open) return undefined;
      const onKey = (e) => { if (e.key === "Escape") onClose(); };
      window.addEventListener("keydown", onKey);
      return () => window.removeEventListener("keydown", onKey);
    }, [open, onClose]);

    if (!open) return null;

    const runItem = (it) => {
      onClose();
      if (it && it.run) it.run();
    };

    const onKeyDown = (e) => {
      if (e.key === "ArrowDown") { e.preventDefault(); setSel((s) => Math.min(items.length - 1, s + 1)); }
      else if (e.key === "ArrowUp") { e.preventDefault(); setSel((s) => Math.max(0, s - 1)); }
      else if (e.key === "Enter") { e.preventDefault(); if (items[sel]) runItem(items[sel]); }
      else if (e.key === "Escape") { e.preventDefault(); onClose(); }
    };

    // group rendering preserving order
    const rendered = [];
    let lastGroup = null;
    items.forEach((it, i) => {
      if (it.group !== lastGroup) {
        lastGroup = it.group;
        rendered.push(<div key={"g:" + it.group} className="cmdk-group-label">{it.group}</div>);
      }
      rendered.push(
        <button
          key={it.key}
          type="button"
          className={cx("cmdk-item", i === sel && "selected")}
          onMouseEnter={() => setSel(i)}
          onClick={() => runItem(it)}
        >
          {it.icon && <Icon name={it.icon} size={15} />}
          <span className="cmdk-item-label">{it.label}</span>
          {it.hint && <span className="cmdk-item-hint">{it.hint}</span>}
        </button>
      );
    });

    return ReactDOM.createPortal(
      <div className="cmdk-backdrop" onMouseDown={(e) => { if (e.target === e.currentTarget) onClose(); }}>
        <div className="cmdk" role="dialog" aria-modal="true" aria-label="Command palette">
          <div className="cmdk-input-row">
            <Icon name="search" size={15} />
            <input
              ref={inputRef}
              className="cmdk-input"
              placeholder="Search pages and actions…"
              value={query}
              onChange={(e) => setQuery(e.target.value)}
              onKeyDown={onKeyDown}
              aria-label="Search pages and actions"
            />
            <span className="kbd">Esc</span>
          </div>
          <div className="cmdk-list" ref={listRef}>
            {items.length === 0 ? <div className="cmdk-empty">No matches for “{query}”</div> : rendered}
          </div>
          <div className="cmdk-foot">
            <span><span className="kbd">↑↓</span> Navigate</span>
            <span><span className="kbd">↵</span> Select</span>
          </div>
        </div>
      </div>,
      document.body
    );
  }

  /* ──────────────────────────────────────────── */
  window.Button = Button;
  window.IconButton = IconButton;
  window.Card = Card;
  window.StatCard = StatCard;
  window.PageHeader = PageHeader;
  window.Field = Field;
  window.Input = Input;
  window.Textarea = Textarea;
  window.Select = Select;
  window.Switch = Switch;
  window.Checkbox = Checkbox;
  window.SegmentedControl = SegmentedControl;
  window.Badge = Badge;
  window.Tabs = Tabs;
  window.Modal = Modal;
  window.ToastProvider = ToastProvider;
  window.useToast = useToast;
  window.EmptyState = EmptyState;
  window.Skeleton = Skeleton;
  window.Spinner = Spinner;
  window.CopyButton = CopyButton;
  window.SecretField = SecretField;
  window.KeyValue = KeyValue;
  window.CodeBlock = CodeBlock;
  window.Sparkline = Sparkline;
  window.AreaChart = AreaChart;
  window.BarChart = BarChart;
  window.ErrorBoundary = ErrorBoundary;
  window.CommandPalette = CommandPalette;
})();
