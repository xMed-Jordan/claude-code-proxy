// data.jsx — sample data + helpers for the Connect AI Proxy dashboard

const SAMPLE_LOGS_RUNNING = [
  { ts: "14:42:08.124", lvl: "info",  meth: "GET",  path: "/anthropic/v1/models",                status: 200, dur: 4,    ip: "127.0.0.1", note: "" },
  { ts: "14:42:08.301", lvl: "info",  meth: "POST", path: "/anthropic/v1/messages/count_tokens", status: 200, dur: 12,   ip: "127.0.0.1", note: "tokens=842" },
  { ts: "14:42:08.418", lvl: "info",  meth: "POST", path: "/anthropic/v1/messages",              status: 200, dur: 1284, ip: "127.0.0.1", note: "model=gpt-5.5 stream=false" },
  { ts: "14:42:09.766", lvl: "debug", meth: "",     path: "alias resolved",            status: 0,   dur: 0,    ip: "",          note: "claude-3-7-sonnet-latest → gpt-5.5" },
  { ts: "14:42:11.009", lvl: "info",  meth: "POST", path: "/anthropic/v1/messages",              status: 200, dur: 6420, ip: "127.0.0.1", note: "model=gpt-5.3-codex stream=true frames=42" },
  { ts: "14:42:17.502", lvl: "info",  meth: "POST", path: "/anthropic/v1/messages/count_tokens", status: 200, dur: 8,    ip: "127.0.0.1", note: "tokens=212" },
  { ts: "14:42:18.044", lvl: "info",  meth: "POST", path: "/anthropic/v1/messages",              status: 200, dur: 944,  ip: "127.0.0.1", note: "model=gpt-5.4-mini stream=false" },
  { ts: "14:42:21.118", lvl: "warn",  meth: "POST", path: "/anthropic/v1/messages",              status: 200, dur: 4188, ip: "127.0.0.1", note: "upstream slow > 4s, codex region us-east-1" },
  { ts: "14:42:25.301", lvl: "info",  meth: "POST", path: "/anthropic/v1/messages",              status: 200, dur: 1822, ip: "127.0.0.1", note: "model=gpt-5.5 stream=true" },
  { ts: "14:42:28.510", lvl: "debug", meth: "",     path: "auth refresh",              status: 0,   dur: 0,    ip: "",          note: "codex chatgpt token refreshed (exp 90d)" },
  { ts: "14:42:31.402", lvl: "info",  meth: "GET",  path: "/anthropic/v1/models",                status: 200, dur: 3,    ip: "127.0.0.1", note: "" },
  { ts: "14:42:34.221", lvl: "info",  meth: "POST", path: "/anthropic/v1/messages",              status: 200, dur: 2104, ip: "127.0.0.1", note: "model=gpt-5.5 stream=true" },
  { ts: "14:42:36.802", lvl: "warn",  meth: "POST", path: "/anthropic/v1/messages",              status: 429, dur: 18,   ip: "127.0.0.1", note: "upstream rate limit, retry-after 1s" },
  { ts: "14:42:37.901", lvl: "info",  meth: "POST", path: "/anthropic/v1/messages",              status: 200, dur: 1605, ip: "127.0.0.1", note: "model=gpt-5.5 retry ok" },
  { ts: "14:42:40.118", lvl: "info",  meth: "POST", path: "/anthropic/v1/messages",              status: 200, dur: 982,  ip: "127.0.0.1", note: "model=gpt-5.4-mini stream=false" },
];

const SAMPLE_LOGS_WARN = [
  { ts: "14:38:02.001", lvl: "info",  meth: "",     path: "proxy started",   status: 0, dur: 0, ip: "", note: "listen 127.0.0.1:4000 pid=14324" },
  { ts: "14:38:02.142", lvl: "info",  meth: "",     path: "auth loaded",     status: 0, dur: 0, ip: "", note: "~/.codex/auth.json mode=chatgpt" },
  { ts: "14:38:02.180", lvl: "warn",  meth: "",     path: "auth advisory",   status: 0, dur: 0, ip: "", note: "codex chatgpt token expires in 7d 14h" },
  { ts: "14:38:14.221", lvl: "info",  meth: "GET",  path: "/anthropic/v1/models",      status: 200, dur: 4,  ip: "127.0.0.1", note: "" },
  { ts: "14:38:18.501", lvl: "info",  meth: "POST", path: "/anthropic/v1/messages",    status: 200, dur: 1422, ip: "127.0.0.1", note: "model=gpt-5.5" },
  { ts: "14:38:24.118", lvl: "warn",  meth: "POST", path: "/anthropic/v1/messages",    status: 200, dur: 4188, ip: "127.0.0.1", note: "upstream slow > 4s" },
  { ts: "14:38:30.401", lvl: "info",  meth: "POST", path: "/anthropic/v1/messages",    status: 200, dur: 1604, ip: "127.0.0.1", note: "model=gpt-5.5 stream=true" },
];

const SAMPLE_LOGS_STOPPED = [
  { ts: "14:31:55.221", lvl: "info",  meth: "",     path: "shutdown signal received", status: 0, dur: 0, ip: "", note: "SIGTERM from launcher" },
  { ts: "14:31:55.244", lvl: "info",  meth: "",     path: "draining connections",     status: 0, dur: 0, ip: "", note: "active=0 idle=2" },
  { ts: "14:31:55.260", lvl: "info",  meth: "",     path: "listener closed",          status: 0, dur: 0, ip: "", note: "127.0.0.1:4000" },
  { ts: "14:31:55.288", lvl: "info",  meth: "",     path: "proxy stopped",            status: 0, dur: 0, ip: "", note: "uptime 4h 12m" },
];

const SAMPLE_MODELS = [
  { alias: "opus[1m]",                     real: "gpt-5.5",         status: "ok",     desc: "Claude Code Opus alias with 1M context.", default: false, context: "1m" },
  { alias: "claude-opus-4-7[1m]",          real: "gpt-5.5",         status: "ok",     desc: "Latest Opus full model name with 1M context.", default: false, context: "1m" },
  { alias: "sonnet[1m]",                   real: "gpt-5.5",         status: "ok",     desc: "Claude Code Sonnet alias with 1M context.", default: true, context: "1m" },
  { alias: "claude-sonnet-4-6[1m]",        real: "gpt-5.5",         status: "ok",     desc: "Latest Sonnet full model name with 1M context.", default: false, context: "1m" },
  { alias: "haiku",                        real: "gpt-5.4-mini",    status: "ok",     desc: "Fast Haiku alias. Claude Haiku 4.5 is 200k context.", default: false, context: "200k" },
  { alias: "claude-haiku-4-5",             real: "gpt-5.4-mini",    status: "ok",     desc: "Latest Haiku model alias.", default: false, context: "200k" },
  { alias: "gpt-5.3-codex",                real: "gpt-5.3-codex",   status: "ok",     desc: "Direct passthrough. Recommended for Claude Code.", default: false, recommended: true },
];

const DEFAULT_CONFIG = {
  UPSTREAM: "codex",
  CODEX_BASE_URL: "https://chatgpt.com/backend-api/codex",
  CODEX_AUTH_FILE: "~/.codex/auth.json",
  PROXY_HOST: "127.0.0.1",
  PROXY_PORT: "4000",
  PROXY_PUBLIC_URL: "",
  PROXY_API_KEY: "ccp_local_4f8e29a6c1b7d3e5_71a9",
  PROXY_BIND: "127.0.0.1",
  ANTHROPIC_DEFAULT_OPUS_MODEL: "claude-opus-4-7[1m]",
  ANTHROPIC_DEFAULT_SONNET_MODEL: "claude-sonnet-4-6[1m]",
  ANTHROPIC_DEFAULT_HAIKU_MODEL: "claude-haiku-4-5",
  REQUEST_TIMEOUT_MS: "120000",
  LOG_LEVEL: "info",
};

const ALIAS_DEFAULTS = [
  { from: "opus",          to: "gpt-5.5",       context: "1m" },
  { from: "sonnet",        to: "gpt-5.5",       context: "1m" },
  { from: "haiku",         to: "gpt-5.4-mini",  context: "200k" },
  { from: "gpt-5.3-codex", to: "gpt-5.3-codex", context: "1m" },
];

const VALIDATION_STEPS = [
  { name: "GET /anthropic/v1/models",                expect: "200 OK · 4 models",      tone: "ok",   ms: 4 },
  { name: "POST /anthropic/v1/messages/count_tokens", expect: "200 OK · 842 tokens",    tone: "ok",   ms: 12 },
  { name: "POST /anthropic/v1/messages",              expect: "200 OK · non-streaming", tone: "ok",   ms: 1284 },
  { name: "POST /anthropic/v1/messages (stream)",     expect: "200 OK · 42 SSE frames", tone: "ok",   ms: 6420 },
];

// Sparkline generator: deterministic pseudo-random
function genSpark(seed, n = 24, base = 50, range = 30) {
  let s = seed;
  const rng = () => { s = (s * 9301 + 49297) % 233280; return s / 233280; };
  return Array.from({ length: n }, () => base + (rng() - 0.5) * range);
}

function pathFromPoints(pts, w, h, pad = 2) {
  const min = Math.min(...pts), max = Math.max(...pts);
  const span = max - min || 1;
  const stepX = (w - pad * 2) / (pts.length - 1);
  return pts.map((p, i) => {
    const x = pad + i * stepX;
    const y = h - pad - ((p - min) / span) * (h - pad * 2);
    return `${i === 0 ? "M" : "L"}${x.toFixed(1)} ${y.toFixed(1)}`;
  }).join(" ");
}

const Sparkline = ({ data, w = 120, h = 22 }) => {
  const line = pathFromPoints(data, w, h);
  const area = `${line} L${w - 2} ${h - 2} L2 ${h - 2} Z`;
  return (
    <svg className="spark-svg" viewBox={`0 0 ${w} ${h}`} preserveAspectRatio="none">
      <path className="area" d={area} />
      <path className="line" d={line} />
    </svg>
  );
};

const Bars = ({ data }) => (
  <div className="bars">
    {data.map((v, i) => {
      const max = Math.max(1, ...data);
      return <div key={i} className="bar" style={{ height: `${(v / max) * 100}%` }} />;
    })}
  </div>
);

window.SAMPLE_LOGS_RUNNING = SAMPLE_LOGS_RUNNING;
window.SAMPLE_LOGS_WARN = SAMPLE_LOGS_WARN;
window.SAMPLE_LOGS_STOPPED = SAMPLE_LOGS_STOPPED;
window.SAMPLE_MODELS = SAMPLE_MODELS;
window.DEFAULT_CONFIG = DEFAULT_CONFIG;
window.ALIAS_DEFAULTS = ALIAS_DEFAULTS;
window.VALIDATION_STEPS = VALIDATION_STEPS;
window.genSpark = genSpark;
window.Sparkline = Sparkline;
window.Bars = Bars;
