// app.jsx — application root: auth gate, login screen, shell
// (sidebar / topbar / connection banner / route outlet / ⌘K palette).
// Depends on globals from icons.jsx, lib.jsx and components.jsx.
(() => {
  const { useState, useEffect, useRef, useMemo, useCallback } = React;
  const {
    Icon, api, useLive, ROUTES, useRoute, fmtUptime, timeAgo,
    Button, IconButton, Badge, Field, Input, Checkbox, Spinner,
    EmptyState, ToastProvider, useToast, ErrorBoundary, CommandPalette,
  } = window;

  const THEME_KEY = "ccp.theme";

  /* ────────────────────────────────────────────
     Theme
     ──────────────────────────────────────────── */
  function useTheme() {
    const [theme, setTheme] = useState(() =>
      document.documentElement.dataset.theme === "dark" ? "dark" : "light"
    );
    const toggle = useCallback(() => {
      setTheme((prev) => {
        const next = prev === "dark" ? "light" : "dark";
        document.documentElement.dataset.theme = next;
        try { localStorage.setItem(THEME_KEY, next); } catch (e) {}
        return next;
      });
    }, []);
    return [theme, toggle];
  }

  function ThemeToggle({ theme, onToggle, bordered }) {
    return (
      <IconButton
        icon={theme === "dark" ? "sun" : "moon"}
        label={theme === "dark" ? "Switch to light theme" : "Switch to dark theme"}
        bordered={bordered}
        onClick={onToggle}
      />
    );
  }

  /* ────────────────────────────────────────────
     Splash (initial auth check)
     ──────────────────────────────────────────── */
  function Splash() {
    return (
      <div className="app-splash">
        <span className="brand-mark brand-mark-lg"><Icon name="bolt" size={18} /></span>
        <Spinner size={18} />
      </div>
    );
  }

  /* ────────────────────────────────────────────
     Login screen — login + first-run setup variants.
     URL never changes here; the bookmarked route is
     restored as soon as the user signs in.
     ──────────────────────────────────────────── */
  function LoginScreen({ auth, expired, onAuthenticated, theme, onToggleTheme }) {
    const isSetup = auth.configured === false;
    const [username, setUsername] = useState(auth.username || "admin");
    const [password, setPassword] = useState("");
    const [remember, setRemember] = useState(true); // default CHECKED
    const [busy, setBusy] = useState(false);
    const [error, setError] = useState("");

    const submit = async (e) => {
      e.preventDefault();
      if (busy) return;
      setBusy(true);
      setError("");
      try {
        if (isSetup) {
          await api.post("/ui/api/auth/setup", { username, password, remember: true });
        } else {
          await api.post("/ui/api/auth/login", { username, password, remember });
        }
        await onAuthenticated();
      } catch (err) {
        setError((err && err.message) || String(err));
      } finally {
        setBusy(false);
      }
    };

    return (
      <div className="login-screen">
        <div className="login-theme-toggle">
          <ThemeToggle theme={theme} onToggle={onToggleTheme} bordered />
        </div>
        <form className="login-card" onSubmit={submit}>
          <div className="login-brand">
            <span className="brand-mark brand-mark-lg"><Icon name="bolt" size={18} /></span>
          </div>
          <h1 className="login-title">{isSetup ? "Create admin access" : "Welcome back"}</h1>
          <p className="login-sub">
            {isSetup
              ? "Set the local admin credentials before opening the control panel."
              : "Sign in to the Connect AI Proxy control panel."}
          </p>

          {expired && !isSetup && (
            <div className="alert" data-tone="info">
              <Icon name="info" size={14} /> Your session expired — sign in again.
            </div>
          )}
          {error && (
            <div className="alert" data-tone="danger">
              <Icon name="alert-circle" size={14} /> {error}
            </div>
          )}

          <Field label="Username">
            <Input
              value={username}
              onChange={(e) => setUsername(e.target.value)}
              autoFocus
              autoComplete="username"
              name="username"
            />
          </Field>
          <Field
            label="Password"
            hint={isSetup ? "At least 8 characters. Stored as a hash — the raw password is never saved." : undefined}
          >
            <Input
              type="password"
              value={password}
              onChange={(e) => setPassword(e.target.value)}
              autoComplete={isSetup ? "new-password" : "current-password"}
              name="password"
            />
          </Field>

          {!isSetup && (
            <Checkbox checked={remember} onChange={setRemember} label="Remember me" />
          )}

          <Button
            variant="primary"
            type="submit"
            className="login-submit"
            block
            loading={busy}
            disabled={!username || password.length < (isSetup ? 8 : 1)}
          >
            {isSetup ? "Create admin account" : "Sign in"}
          </Button>
        </form>
        <div className="login-foot">Connect AI Proxy · local control panel</div>
      </div>
    );
  }

  /* ────────────────────────────────────────────
     Sidebar
     ──────────────────────────────────────────── */
  function Sidebar({ routeId, navigate, liveStatus, onNavigated }) {
    const groups = useMemo(() => {
      const map = new Map();
      ROUTES.forEach((r) => {
        if (!map.has(r.group)) map.set(r.group, []);
        map.get(r.group).push(r);
      });
      return Array.from(map.entries());
    }, []);

    const modelCount =
      liveStatus && Array.isArray(liveStatus.models) ? liveStatus.models.length : null;
    const version = liveStatus && liveStatus.version ? String(liveStatus.version) : null;
    const update = liveStatus && liveStatus.update;

    const go = (id) => {
      navigate(id);
      if (onNavigated) onNavigated();
    };

    return (
      <aside className="sidebar">
        <div className="side-brand">
          <span className="brand-mark"><Icon name="bolt" size={14} /></span>
          <div className="brand-text">
            <span className="brand-name">Connect AI Proxy</span>
            <span className="brand-sub">Control panel</span>
          </div>
        </div>

        <nav className="side-nav" aria-label="Main">
          {groups.map(([group, items]) => (
            <div className="nav-group" key={group}>
              <div className="nav-group-label">{group}</div>
              {items.map((r) => (
                <button
                  key={r.id}
                  type="button"
                  className={"nav-item" + (routeId === r.id ? " active" : "")}
                  aria-current={routeId === r.id ? "page" : undefined}
                  onClick={() => go(r.id)}
                >
                  <Icon name={r.icon} size={15} />
                  <span className="nav-label">{r.title}</span>
                  {r.id === "models" && modelCount != null && (
                    <span className="nav-badge tnum">{modelCount}</span>
                  )}
                  {r.id === "logs" && (
                    <span className="nav-badge nav-badge-live"><span className="live-dot" />live</span>
                  )}
                </button>
              ))}
            </div>
          ))}
        </nav>

        <div className="side-footer">
          <div className="side-footer-row">
            <span className="side-version mono">{version ? "v" + version.replace(/^v/, "") : "version —"}</span>
            {update && update.update_available && (
              <Badge tone="accent">v{String(update.latest_version || "").replace(/^v/, "")} available</Badge>
            )}
          </div>
          <a className="side-docs" href="/docs" target="_blank" rel="noreferrer">
            <Icon name="book" size={13} /> API documentation <Icon name="external-link" size={11} />
          </a>
        </div>
      </aside>
    );
  }

  /* ────────────────────────────────────────────
     Topbar
     ──────────────────────────────────────────── */
  function UserMenu({ username, onLogout }) {
    const [open, setOpen] = useState(false);
    const ref = useRef(null);
    useEffect(() => {
      if (!open) return undefined;
      const onDoc = (e) => { if (ref.current && !ref.current.contains(e.target)) setOpen(false); };
      const onKey = (e) => { if (e.key === "Escape") setOpen(false); };
      document.addEventListener("mousedown", onDoc);
      window.addEventListener("keydown", onKey);
      return () => {
        document.removeEventListener("mousedown", onDoc);
        window.removeEventListener("keydown", onKey);
      };
    }, [open]);
    return (
      <div className="user-menu" ref={ref}>
        <button
          type="button"
          className="user-btn"
          onClick={() => setOpen((o) => !o)}
          aria-haspopup="menu"
          aria-expanded={open}
        >
          <span className="user-avatar"><Icon name="user" size={13} /></span>
          <span className="user-name">{username || "admin"}</span>
          <Icon name="chevron" size={12} />
        </button>
        {open && (
          <div className="menu-pop" role="menu">
            <div className="menu-meta">Signed in as <strong>{username || "admin"}</strong></div>
            <div className="menu-sep" />
            <a className="menu-item" role="menuitem" href="/docs" target="_blank" rel="noreferrer">
              <Icon name="book" size={14} /> API docs <Icon name="external-link" size={11} />
            </a>
            <button type="button" className="menu-item menu-danger" role="menuitem" onClick={onLogout}>
              <Icon name="log-out" size={14} /> Log out
            </button>
          </div>
        )}
      </div>
    );
  }

  function Topbar({ liveStatus, onToggleDrawer, onOpenPalette, theme, onToggleTheme, username, onLogout }) {
    const toast = useToast();
    const hasStatus = !!liveStatus;
    const running = !!(liveStatus && liveStatus.proxy_running);
    const url =
      (liveStatus && (liveStatus.display_url || liveStatus.public_url || liveStatus.local_url)) ||
      "http://127.0.0.1:4000";
    const copyUrl = () => {
      if (navigator.clipboard) {
        navigator.clipboard.writeText(url)
          .then(() => toast.success("Endpoint URL copied"))
          .catch(() => toast.error("Could not copy URL"));
      }
    };
    return (
      <header className="topbar">
        <button type="button" className="icon-btn menu-btn" onClick={onToggleDrawer} aria-label="Open navigation">
          <Icon name="menu" size={16} />
        </button>

        <button
          type="button"
          className="endpoint-pill"
          data-state={hasStatus ? (running ? "ok" : "err") : "unknown"}
          onClick={copyUrl}
          data-tip="Copy endpoint URL"
        >
          <span className="status-dot" />
          <span className="endpoint-url">{url}</span>
          <Icon name="copy" size={12} />
        </button>

        {hasStatus && (
          <Badge tone={running ? "success" : "danger"} pulse={running}>
            {running ? "Running" : "Stopped"}
          </Badge>
        )}
        {hasStatus && running && liveStatus.uptime_seconds != null && (
          <span className="topbar-uptime tnum">up {fmtUptime(liveStatus.uptime_seconds)}</span>
        )}

        <div className="topbar-spacer" />

        <button type="button" className="search-btn" onClick={onOpenPalette}>
          <Icon name="search" size={14} />
          <span>Search</span>
          <span className="kbd">⌘K</span>
        </button>
        <ThemeToggle theme={theme} onToggle={onToggleTheme} />
        <UserMenu username={username} onLogout={onLogout} />
      </header>
    );
  }

  /* ────────────────────────────────────────────
     Connection banner
     ──────────────────────────────────────────── */
  function ConnectionBanner({ reconnecting, lastUpdated, updating }) {
    if (updating) {
      return (
        <div className="conn-banner" data-tone="info">
          <Spinner size={12} /> Service restarting for update… the panel will reconnect automatically.
        </div>
      );
    }
    if (reconnecting) {
      return (
        <div className="conn-banner">
          <Icon name="warning" size={13} /> Reconnecting… last updated {timeAgo(lastUpdated)}
        </div>
      );
    }
    return null;
  }

  /* ────────────────────────────────────────────
     Shell — owns the single 2s /ui/api/status poll
     ──────────────────────────────────────────── */
  function Shell({ username, onLoggedOut, theme, onToggleTheme }) {
    const { route, routeId, navigate } = useRoute();
    const toast = useToast();
    const [paletteOpen, setPaletteOpen] = useState(false);
    const [drawerOpen, setDrawerOpen] = useState(false);

    const {
      data: liveStatus,
      error: statusError,
      stale: statusStale,
      reconnecting,
      lastUpdated,
      refresh: refreshStatus,
    } = useLive("/ui/api/status", { interval: 2000, enabled: true });

    const liveStatusRef = useRef(liveStatus);
    liveStatusRef.current = liveStatus;

    const pushToast = useCallback((msg, tone = "info") => {
      toast.push(msg, { tone });
    }, [toast]);

    const onLogout = useCallback(async () => {
      try { await api.post("/ui/api/auth/logout"); } catch (e) {}
      onLoggedOut();
    }, [onLoggedOut]);

    // Ported proxy/update action handler (start/stop/restart/validate/
    // update/check-update/toggle-auto-update) with toasts.
    const onAction = useCallback(async (action) => {
      try {
        if (action === "start") {
          await api.post("/ui/api/proxy/start");
          toast.success("Proxy endpoints started");
        } else if (action === "stop") {
          await api.post("/ui/api/proxy/stop");
          toast.info("Proxy endpoints stopped — the control panel stays online");
        } else if (action === "restart") {
          await api.post("/ui/api/proxy/restart");
          toast.info("Proxy restarting");
        } else if (action === "validate") {
          const res = await api.get("/ui/api/validate");
          const steps = (res && res.steps) || [];
          const passed = steps.filter((s) => s.ok).length;
          const allOk = passed === steps.length && steps.length > 0;
          toast.push(`Validation complete — ${passed}/${steps.length} checks passed`, { tone: allOk ? "success" : "error" });
        } else if (action === "update") {
          await api.post("/ui/api/update/start");
          toast.info("Update started — waiting for reconnect");
        } else if (action === "check-update") {
          await api.post("/ui/api/update/check");
          toast.success("Update check complete");
        } else if (action === "toggle-auto-update") {
          const cur = liveStatusRef.current;
          const enabled = !(cur && cur.update && cur.update.auto_update);
          await api.post("/ui/api/update/settings", { auto_update: enabled });
          toast.success(enabled ? "Auto update enabled" : "Auto update disabled");
        }
        refreshStatus();
      } catch (e) {
        if (action === "update") {
          toast.info("Update is running — reconnecting soon");
          return;
        }
        toast.error(`Action failed — ${(e && e.message) || e}`);
      }
    }, [toast, refreshStatus]);

    // Keyboard: Ctrl/⌘+K palette; bare digits 1-9 navigate (guarded).
    useEffect(() => {
      const onKey = (e) => {
        if ((e.ctrlKey || e.metaKey) && !e.altKey && e.key.toLowerCase() === "k") {
          e.preventDefault();
          setPaletteOpen((o) => !o);
          return;
        }
        if (e.ctrlKey || e.metaKey || e.altKey) return;
        const t = e.target;
        if (
          t &&
          (t.tagName === "INPUT" || t.tagName === "TEXTAREA" || t.tagName === "SELECT" || t.isContentEditable)
        ) return;
        const idx = "123456789".indexOf(e.key);
        if (idx >= 0 && idx < ROUTES.length) navigate(ROUTES[idx].id);
      };
      window.addEventListener("keydown", onKey);
      return () => window.removeEventListener("keydown", onKey);
    }, [navigate]);

    const running = !!(liveStatus && liveStatus.proxy_running);
    const paletteActions = useMemo(() => [
      running
        ? { id: "stop", label: "Stop proxy endpoints", icon: "stop", run: () => onAction("stop") }
        : { id: "start", label: "Start proxy endpoints", icon: "play", run: () => onAction("start") },
      { id: "restart", label: "Restart proxy", icon: "restart", run: () => onAction("restart") },
      { id: "validate", label: "Run validation", icon: "shield", run: () => onAction("validate") },
      { id: "check-update", label: "Check for updates", icon: "download", run: () => onAction("check-update") },
      {
        id: "toggle-theme",
        label: theme === "dark" ? "Switch to light theme" : "Switch to dark theme",
        icon: theme === "dark" ? "sun" : "moon",
        run: onToggleTheme,
      },
      { id: "logout", label: "Log out", icon: "log-out", run: onLogout },
    ], [running, theme, onAction, onToggleTheme, onLogout]);

    const ScreenComponent = window[route.component];
    const updating = !!(liveStatus && liveStatus.update && liveStatus.update.running && statusError);

    return (
      <div className="app-shell" data-drawer={drawerOpen ? "open" : undefined}>
        <Sidebar
          routeId={routeId}
          navigate={navigate}
          liveStatus={liveStatus}
          onNavigated={() => setDrawerOpen(false)}
        />
        {drawerOpen && (
          <button
            type="button"
            className="drawer-backdrop"
            aria-label="Close navigation"
            onClick={() => setDrawerOpen(false)}
          />
        )}

        <Topbar
          liveStatus={liveStatus}
          onToggleDrawer={() => setDrawerOpen((o) => !o)}
          onOpenPalette={() => setPaletteOpen(true)}
          theme={theme}
          onToggleTheme={onToggleTheme}
          username={username}
          onLogout={onLogout}
        />

        <main className="app-main">
          <ConnectionBanner reconnecting={reconnecting} lastUpdated={lastUpdated} updating={updating} />
          <div className="main-inner">
            <ErrorBoundary key={routeId}>
              {ScreenComponent ? (
                <ScreenComponent
                  liveStatus={liveStatus}
                  statusError={statusError}
                  statusStale={statusStale}
                  refreshStatus={refreshStatus}
                  onAction={onAction}
                  navigate={navigate}
                  pushToast={pushToast}
                />
              ) : (
                <EmptyState
                  icon="warning"
                  title="Screen not loaded"
                  description={`Component "${route.component}" is missing — check the script tags in index.html.`}
                />
              )}
            </ErrorBoundary>
          </div>
        </main>

        <CommandPalette
          open={paletteOpen}
          onClose={() => setPaletteOpen(false)}
          actions={paletteActions}
          onNavigate={navigate}
        />
      </div>
    );
  }

  /* ────────────────────────────────────────────
     App root — auth gate
     ──────────────────────────────────────────── */
  function App() {
    const [theme, toggleTheme] = useTheme();
    const [auth, setAuth] = useState({
      loading: true,
      configured: true,
      authenticated: false,
      username: "",
      expired: false,
    });

    const refreshAuth = useCallback(async () => {
      try {
        const res = await api.get("/ui/api/auth/status");
        setAuth({ loading: false, expired: false, configured: true, authenticated: false, username: "", ...res });
      } catch (e) {
        setAuth((a) => ({ ...a, loading: false, authenticated: false }));
      }
    }, []);

    useEffect(() => {
      api.onAuthExpired = () => {
        setAuth((a) => (a.authenticated ? { ...a, authenticated: false, expired: true } : a));
      };
      refreshAuth();
      return () => { api.onAuthExpired = null; };
    }, [refreshAuth]);

    let body;
    if (auth.loading) {
      body = <Splash />;
    } else if (!auth.authenticated) {
      body = (
        <LoginScreen
          auth={auth}
          expired={auth.expired}
          onAuthenticated={refreshAuth}
          theme={theme}
          onToggleTheme={toggleTheme}
        />
      );
    } else {
      body = (
        <Shell
          username={auth.username}
          onLoggedOut={refreshAuth}
          theme={theme}
          onToggleTheme={toggleTheme}
        />
      );
    }

    return <ToastProvider>{body}</ToastProvider>;
  }

  const root = ReactDOM.createRoot(document.getElementById("root"));
  root.render(<App />);
})();
