// screens-images.jsx — Images page: generate images with agy (Antigravity)
// and get a shareable link. Exposes window.ImageStudio.
//
// Calls the admin endpoint POST /ui/api/images/generate, which runs agy in a
// scratch dir, collects the file(s) it wrote, and serves them back as capability
// URLs (/media/generated/<token>.<ext>). Generation is slow (~30–120s on a cold
// request), so the request uses a long client timeout.
(() => {
  const { useState, useCallback } = React;
  const { Card, Button, Select, Textarea, Input, Badge, PageHeader, EmptyState, Spinner, useToast } = window;

  // size value → what we POST. Presets the live server has been verified to honor.
  const SIZE_OPTIONS = [
    { value: "512", label: "½K — 512 × 512" },
    { value: "1024", label: "1K — 1024 × 1024" },
    { value: "1280x720", label: "HD 720p — 1280 × 720" },
    { value: "1920x1080", label: "FHD 1080p — 1920 × 1080" },
    { value: "2k", label: "2K — 2560 × 1440" },
    { value: "4k", label: "4K — 3840 × 2160" },
    { value: "custom", label: "Custom…" },
  ];

  // Antigravity models that can drive image generation. Gemini models carry the
  // image tool; they are the reliable choices here.
  const MODEL_OPTIONS = [
    "Gemini 3.5 Flash (Low)",
    "Gemini 3.5 Flash (Medium)",
    "Gemini 3.5 Flash (High)",
    "Gemini 3.1 Pro (Low)",
    "Gemini 3.1 Pro (High)",
  ];

  function fmtBytes(n) {
    if (!n || n <= 0) return "—";
    if (n < 1024) return n + " B";
    if (n < 1024 * 1024) return (n / 1024).toFixed(1) + " KB";
    return (n / (1024 * 1024)).toFixed(1) + " MB";
  }

  function ImageStudio({ pushToast }) {
    const toast = useToast();
    const notify = useCallback((msg, tone) => {
      if (toast) { tone === "error" ? toast.error(msg) : tone === "success" ? toast.success(msg) : toast.info(msg); }
      else if (pushToast) pushToast(msg, tone || "info");
    }, [toast, pushToast]);

    const [prompt, setPrompt] = useState("");
    const [size, setSize] = useState("1024");
    const [custom, setCustom] = useState("1024x1024");
    const [model, setModel] = useState(MODEL_OPTIONS[0]);
    const [count, setCount] = useState(1);
    const [busy, setBusy] = useState(false);
    const [result, setResult] = useState(null);
    const [error, setError] = useState("");

    const absUrl = (url) => (url && url.startsWith("http")) ? url : (window.location.origin + url);

    const generate = useCallback(async () => {
      const p = prompt.trim();
      if (!p) { notify("Enter a prompt first.", "error"); return; }
      const sizeVal = size === "custom" ? custom.trim() : size;
      setBusy(true); setError(""); setResult(null);
      try {
        const res = await api.post(
          "/ui/api/images/generate",
          { prompt: p, model, size: sizeVal, n: Number(count) || 1 },
          { timeoutMs: 300000 }
        );
        if (!res || !res.ok) {
          const msg = (res && res.error) || "Generation failed";
          setError(msg); notify(msg, "error"); return;
        }
        setResult(res);
        const k = (res.images || []).length;
        notify(`Generated ${k} image${k === 1 ? "" : "s"}.`, "success");
      } catch (e) {
        const msg = (e && e.message) || String(e);
        setError(msg); notify("Generation failed: " + msg, "error");
      } finally { setBusy(false); }
    }, [prompt, size, custom, model, count, notify]);

    const copyLink = useCallback((url) => {
      const abs = absUrl(url);
      if (navigator.clipboard && navigator.clipboard.writeText) {
        navigator.clipboard.writeText(abs).then(
          () => notify("Link copied to clipboard.", "success"),
          () => notify("Copy failed — select the link manually.", "error")
        );
      } else {
        notify("Clipboard unavailable — select the link manually.", "info");
      }
    }, [notify]);

    return (
      <div className="screen">
        <PageHeader
          title="Images"
          description="Generate images with Antigravity (agy). Pick a size, describe the image, and get a shareable link."
        />

        <Card title="Generate">
          <div className="cfg-row">
            <div className="cfg-label">
              <span className="cfg-label-name">Prompt</span>
              <span className="cfg-label-env">What to draw</span>
            </div>
            <div className="cfg-ctl">
              <Textarea
                rows={4}
                value={prompt}
                placeholder="A watercolor fox sitting in a snowy forest at dusk"
                onChange={e => setPrompt(e.target.value)}
                disabled={busy}
              />
            </div>
          </div>

          <div className="cfg-row">
            <div className="cfg-label">
              <span className="cfg-label-name">Size</span>
              <span className="cfg-label-env">Output resolution</span>
            </div>
            <div className="cfg-ctl" style={{ display: "flex", gap: 8, flexWrap: "wrap", alignItems: "center" }}>
              <Select options={SIZE_OPTIONS} value={size} onChange={e => setSize(e.target.value)} disabled={busy} />
              {size === "custom" && (
                <Input
                  mono
                  style={{ width: 140 }}
                  value={custom}
                  placeholder="1024x1024"
                  onChange={e => setCustom(e.target.value)}
                  disabled={busy}
                />
              )}
              <span className="cfg-hint" style={{ margin: 0 }}>16–4096 px per side. Exact pixel size is honored.</span>
            </div>
          </div>

          <div className="cfg-row">
            <div className="cfg-label">
              <span className="cfg-label-name">Model</span>
              <span className="cfg-label-env">Antigravity engine</span>
            </div>
            <div className="cfg-ctl">
              <Select options={MODEL_OPTIONS} value={model} onChange={e => setModel(e.target.value)} disabled={busy} />
              <p className="cfg-hint">Gemini models carry the image tool. Default: Gemini 3.5 Flash (Low).</p>
            </div>
          </div>

          <div className="cfg-row">
            <div className="cfg-label">
              <span className="cfg-label-name">Count</span>
              <span className="cfg-label-env">Images per request</span>
            </div>
            <div className="cfg-ctl" style={{ display: "flex", gap: 8, alignItems: "center" }}>
              <Input
                type="number" min={1} max={4} style={{ width: 80 }}
                value={count}
                onChange={e => setCount(Math.max(1, Math.min(4, Number(e.target.value) || 1)))}
                disabled={busy}
              />
              <span className="cfg-hint" style={{ margin: 0 }}>1–4. More images take proportionally longer.</span>
            </div>
          </div>

          <div style={{ display: "flex", gap: 10, alignItems: "center", marginTop: 4 }}>
            <Button variant="primary" icon={busy ? undefined : "bolt"} loading={busy} disabled={busy || !prompt.trim()} onClick={generate}>
              {busy ? "Generating…" : "Generate"}
            </Button>
            {busy && <span className="cfg-hint" style={{ margin: 0 }}>This can take 30–120 seconds while agy renders.</span>}
          </div>
        </Card>

        {error && !busy && (
          <Card title="Error" flush>
            <div style={{ padding: 16 }}>
              <Badge tone="error">Failed</Badge>
              <p className="cfg-hint" style={{ marginTop: 8 }}>{error}</p>
            </div>
          </Card>
        )}

        {busy && (
          <Card flush>
            <div style={{ display: "flex", gap: 10, alignItems: "center", justifyContent: "center", padding: 32 }}>
              <Spinner size={18} />
              <span className="muted">Rendering with {model}…</span>
            </div>
          </Card>
        )}

        {result && result.images && result.images.length > 0 && !busy && (
          <Card
            title="Result"
            description={`${result.model} · ${result.size}`}
            flush
          >
            <div style={{ display: "grid", gridTemplateColumns: "repeat(auto-fill, minmax(260px, 1fr))", gap: 16, padding: 16 }}>
              {result.images.map((img, i) => (
                <div key={i} style={{ border: "1px solid var(--border, #2a2a2a)", borderRadius: 10, overflow: "hidden", background: "var(--surface-2, #161616)" }}>
                  <a href={absUrl(img.url)} target="_blank" rel="noreferrer">
                    <img
                      src={absUrl(img.url)}
                      alt={`generated ${i + 1}`}
                      style={{ display: "block", width: "100%", height: "auto", background: "#fff" }}
                    />
                  </a>
                  <div style={{ padding: "10px 12px", display: "flex", flexDirection: "column", gap: 8 }}>
                    <div className="cfg-hint" style={{ margin: 0 }}>
                      {(img.width && img.height) ? `${img.width} × ${img.height}` : "image"} · {fmtBytes(img.bytes)}
                    </div>
                    <div style={{ display: "flex", gap: 6 }}>
                      <Input mono readOnly value={absUrl(img.url)} onFocus={e => e.target.select()} style={{ flex: 1, minWidth: 0 }} />
                      <Button size="sm" variant="ghost" icon="copy" onClick={() => copyLink(img.url)} title="Copy link" />
                      <a className="btn btn-sm" href={absUrl(img.url)} target="_blank" rel="noreferrer" title="Open in new tab">
                        <Icon name="external" size={12} />
                      </a>
                    </div>
                  </div>
                </div>
              ))}
            </div>
          </Card>
        )}

        {!result && !busy && !error && (
          <EmptyState
            icon="image"
            title="No images yet"
            description="Describe an image above, choose a size, and click Generate."
          />
        )}
      </div>
    );
  }

  window.ImageStudio = ImageStudio;
})();
