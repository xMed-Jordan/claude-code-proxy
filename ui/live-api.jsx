// live-api.jsx - small client for the local Go UI API.
const api = {
  async get(path) {
    const res = await fetch(path, { headers: { "Accept": "application/json" } });
    if (!res.ok) throw new Error(await res.text());
    return res.json();
  },
  async post(path, body = {}) {
    const res = await fetch(path, {
      method: "POST",
      headers: { "Content-Type": "application/json", "Accept": "application/json" },
      body: JSON.stringify(body),
    });
    if (!res.ok) throw new Error(await res.text());
    return res.json();
  }
};

function usePolling(path, intervalMs = 2000) {
  const [data, setData] = React.useState(null);
  const [error, setError] = React.useState(null);
  const refresh = React.useCallback(async () => {
    try {
      setData(await api.get(path));
      setError(null);
    } catch (e) {
      setError(e.message || String(e));
    }
  }, [path]);
  React.useEffect(() => {
    refresh();
    const id = setInterval(refresh, intervalMs);
    return () => clearInterval(id);
  }, [refresh, intervalMs]);
  return { data, error, refresh };
}

function fmtUptime(seconds) {
  if (!seconds && seconds !== 0) return "-";
  const h = Math.floor(seconds / 3600);
  const m = Math.floor((seconds % 3600) / 60);
  const s = seconds % 60;
  return `${String(h).padStart(2,"0")}:${String(m).padStart(2,"0")}:${String(s).padStart(2,"0")}`;
}

window.api = api;
window.usePolling = usePolling;
window.fmtUptime = fmtUptime;
