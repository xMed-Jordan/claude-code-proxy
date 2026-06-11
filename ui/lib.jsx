// lib.jsx — data + routing foundation for the control panel.
// Exposes: window.api, window.ApiError, window.useLive, window.ROUTES,
// window.useRoute, window.fmtUptime, window.fmtCount, window.fmtPct,
// window.fmtDelta, window.fmtMs, window.timeAgo
(() => {
  const { useState, useEffect, useRef, useCallback } = React;

  /* ────────────────────────────────────────────
     API client
     ──────────────────────────────────────────── */
  class ApiError extends Error {
    constructor(status, message) {
      super(message);
      this.name = "ApiError";
      this.status = status;
    }
  }

  const api = {
    // App registers this; called on any 401 outside /ui/api/auth/*.
    onAuthExpired: null,

    async request(path, { method = "GET", body, timeoutMs = 10000, signal } = {}) {
      const ctrl = new AbortController();
      const onExternalAbort = () => ctrl.abort();
      if (signal) {
        if (signal.aborted) ctrl.abort();
        else signal.addEventListener("abort", onExternalAbort);
      }
      const timer = setTimeout(() => ctrl.abort(), timeoutMs);
      let res;
      try {
        res = await fetch(path, {
          method,
          headers: {
            Accept: "application/json",
            ...(body !== undefined ? { "Content-Type": "application/json" } : {}),
          },
          body: body !== undefined ? JSON.stringify(body) : undefined,
          signal: ctrl.signal,
        });
      } catch (e) {
        if (e && e.name === "AbortError") {
          if (signal && signal.aborted) throw new ApiError(0, "Request cancelled");
          throw new ApiError(0, `Request timed out after ${Math.round(timeoutMs / 1000)}s`);
        }
        throw new ApiError(0, (e && e.message) || "Network error");
      } finally {
        clearTimeout(timer);
        if (signal) signal.removeEventListener("abort", onExternalAbort);
      }

      if (!res.ok) {
        let message = `HTTP ${res.status}`;
        const text = await res.text().catch(() => "");
        if (text) {
          try {
            const parsed = JSON.parse(text);
            message = parsed.error || parsed.message || text;
          } catch (e) {
            message = text;
          }
        }
        if (res.status === 401 && !path.startsWith("/ui/api/auth/")) {
          if (typeof api.onAuthExpired === "function") api.onAuthExpired();
        }
        throw new ApiError(res.status, message);
      }

      if (res.status === 204) return null;
      const text = await res.text();
      if (!text) return null;
      try { return JSON.parse(text); } catch (e) { return text; }
    },

    get(path, opts) {
      return api.request(path, opts);
    },

    post(path, body = {}, opts = {}) {
      return api.request(path, { ...opts, method: "POST", body });
    },
  };

  /* ────────────────────────────────────────────
     useLive — resilient polling hook
     - self-chaining setTimeout (next poll scheduled AFTER previous settles)
     - keeps last good data on error (stale: true)
     - reconnecting: true after >= 2 consecutive failures
     - instant refetch on tab visible / window focus / online
     - skips polls while document.hidden
     - aborts in-flight request on unmount; `enabled` gate stops everything
     ──────────────────────────────────────────── */
  function useLive(path, options = {}) {
    const { interval = 2000, enabled = true, timeoutMs } = options;
    const [data, setData] = useState(null);
    const [error, setError] = useState(null);
    const [stale, setStale] = useState(false);
    const [reconnecting, setReconnecting] = useState(false);
    const [lastUpdated, setLastUpdated] = useState(null);
    const handleRef = useRef(null);

    useEffect(() => {
      if (!enabled) return undefined;

      const st = { alive: true, fetching: false, failures: 0, hasData: false, timer: null, ctrl: null };

      const schedule = () => {
        if (!st.alive) return;
        st.timer = setTimeout(() => run(false), interval);
      };

      const run = async (force) => {
        if (!st.alive || st.fetching) return;
        if (document.hidden && !force) { schedule(); return; }
        st.fetching = true;
        st.ctrl = new AbortController();
        try {
          const result = await api.request(path, { timeoutMs, signal: st.ctrl.signal });
          if (!st.alive) return;
          st.failures = 0;
          st.hasData = true;
          setData(result);
          setError(null);
          setStale(false);
          setReconnecting(false);
          setLastUpdated(Date.now());
        } catch (e) {
          if (!st.alive) return;
          st.failures += 1;
          setError(e);
          setStale(st.hasData); // keep last good data, just mark it stale
          if (st.failures >= 2) setReconnecting(true);
        } finally {
          if (st.alive) {
            st.fetching = false;
            schedule();
          }
        }
      };

      const instant = () => {
        if (!st.alive || st.fetching) return;
        if (st.timer) clearTimeout(st.timer);
        run(true);
      };
      const onVisibility = () => { if (!document.hidden) instant(); };

      handleRef.current = { instant };
      run(true);
      document.addEventListener("visibilitychange", onVisibility);
      window.addEventListener("focus", instant);
      window.addEventListener("online", instant);

      return () => {
        st.alive = false;
        if (st.timer) clearTimeout(st.timer);
        if (st.ctrl) st.ctrl.abort();
        handleRef.current = null;
        document.removeEventListener("visibilitychange", onVisibility);
        window.removeEventListener("focus", instant);
        window.removeEventListener("online", instant);
      };
    }, [path, interval, enabled, timeoutMs]);

    const refresh = useCallback(() => {
      if (handleRef.current) handleRef.current.instant();
    }, []);

    return { data, error, stale, reconnecting, lastUpdated, refresh };
  }

  /* ────────────────────────────────────────────
     Routing — single source of truth
     ──────────────────────────────────────────── */
  const ROUTES = [
    { id: "dashboard", path: "/dashboard", title: "Dashboard",          icon: "dashboard", group: "Overview",     component: "Dashboard" },
    { id: "analytics", path: "/analytics", title: "Analytics",          icon: "activity",  group: "Overview",     component: "Analytics" },
    { id: "config",    path: "/config",    title: "Configuration",      icon: "config",    group: "Manage",       component: "Configuration" },
    { id: "models",    path: "/models",    title: "Models",             icon: "models",    group: "Manage",       component: "Models" },
    { id: "virustotal",path: "/virustotal",title: "VirusTotal",         icon: "shield",    group: "Manage",       component: "VirusTotal" },
    { id: "keys",      path: "/keys",      title: "API Keys",           icon: "key",       group: "Manage",       component: "ApiKeys" },
    { id: "test",      path: "/test",      title: "Test Request",       icon: "test",      group: "Operate",      component: "TestRequest" },
    { id: "logs",      path: "/logs",      title: "Logs",               icon: "logs",      group: "Operate",      component: "Logs" },
    { id: "updates",   path: "/updates",   title: "Updates",            icon: "download",  group: "Operate",      component: "Updates" },
    { id: "browser",   path: "/browser",   title: "Antigravity Browser", icon: "globe",    group: "Integrations", component: "BrowserBridge" },
    { id: "setup",     path: "/setup",     title: "Claude Code Setup",  icon: "terminal",  group: "Integrations", component: "Setup" },
  ];

  function routeFromLocation() {
    let p = window.location.pathname.replace(/\/+$/, "");
    if (p === "") p = "/";
    return ROUTES.find((r) => r.path === p) || null;
  }

  function useRoute() {
    const [routeId, setRouteId] = useState(() => {
      const r = routeFromLocation();
      if (!r) {
        window.history.replaceState(null, "", "/dashboard");
        return "dashboard";
      }
      return r.id;
    });
    const routeIdRef = useRef(routeId);
    routeIdRef.current = routeId;

    // Back/forward: re-parse only — never push.
    useEffect(() => {
      const onPop = () => {
        const r = routeFromLocation();
        if (!r) {
          window.history.replaceState(null, "", "/dashboard");
          setRouteId("dashboard");
        } else {
          setRouteId(r.id);
        }
      };
      window.addEventListener("popstate", onPop);
      return () => window.removeEventListener("popstate", onPop);
    }, []);

    // Title must keep the "Connect AI Proxy" substring (Chrome window-title allowlist).
    useEffect(() => {
      const r = ROUTES.find((x) => x.id === routeId);
      if (r) document.title = r.title + " · Connect AI Proxy";
    }, [routeId]);

    const navigate = useCallback((id) => {
      if (id === routeIdRef.current) return; // no-op if already current
      const r = ROUTES.find((x) => x.id === id);
      if (!r) return;
      window.history.pushState(null, "", r.path);
      setRouteId(id);
      requestAnimationFrame(() => {
        const main = document.querySelector(".app-main");
        if (main) main.scrollTop = 0;
      });
    }, []);

    const route = ROUTES.find((r) => r.id === routeId) || ROUTES[0];
    return { route, routeId, navigate };
  }

  /* ────────────────────────────────────────────
     Formatters
     ──────────────────────────────────────────── */
  function fmtUptime(seconds) {
    if (seconds == null || isNaN(seconds)) return "—";
    const s = Math.max(0, Math.floor(seconds));
    const d = Math.floor(s / 86400);
    const h = Math.floor((s % 86400) / 3600);
    const m = Math.floor((s % 3600) / 60);
    const sec = s % 60;
    if (d > 0) return `${d}d ${h}h`;
    if (h > 0) return `${h}h ${m}m`;
    if (m > 0) return `${m}m ${sec}s`;
    return `${sec}s`;
  }

  function fmtCount(n) {
    if (n == null || isNaN(n)) return "—";
    const abs = Math.abs(n);
    const r1 = (x) => String(Math.round(x * 10) / 10);
    if (abs >= 1e9) return r1(n / 1e9) + "B";
    if (abs >= 1e6) return r1(n / 1e6) + "M";
    if (abs >= 1e3) return r1(n / 1e3) + "k";
    return r1(n);
  }

  function fmtPct(n, digits = 1) {
    if (n == null || isNaN(n)) return "—";
    const f = Math.pow(10, digits);
    return String(Math.round(n * f) / f) + "%";
  }

  function fmtDelta(n, suffix = "") {
    if (n == null || isNaN(n)) return "—";
    if (n === 0) return "0" + suffix;
    const sign = n > 0 ? "+" : "−";
    return sign + fmtCount(Math.abs(n)) + suffix;
  }

  function fmtMs(ms) {
    if (ms == null || isNaN(ms)) return "—";
    if (ms < 1000) return Math.round(ms) + " ms";
    if (ms < 60000) return String(Math.round(ms / 100) / 10) + " s";
    return String(Math.round(ms / 6000) / 10) + " min";
  }

  function timeAgo(ts) {
    if (!ts) return "—";
    const time = ts instanceof Date ? ts.getTime() : ts;
    const s = Math.max(0, Math.floor((Date.now() - time) / 1000));
    if (s < 5) return "just now";
    if (s < 60) return s + "s ago";
    const m = Math.floor(s / 60);
    if (m < 60) return m + "m ago";
    const h = Math.floor(m / 60);
    if (h < 24) return h + "h ago";
    return Math.floor(h / 24) + "d ago";
  }

  /* ──────────────────────────────────────────── */
  window.api = api;
  window.ApiError = ApiError;
  window.useLive = useLive;
  window.ROUTES = ROUTES;
  window.useRoute = useRoute;
  window.fmtUptime = fmtUptime;
  window.fmtCount = fmtCount;
  window.fmtPct = fmtPct;
  window.fmtDelta = fmtDelta;
  window.fmtMs = fmtMs;
  window.timeAgo = timeAgo;

  // Back-compat for the legacy HTML shell while it still exists.
  window.usePolling = (path, intervalMs = 2000) => useLive(path, { interval: intervalMs });
})();
