#!/usr/bin/env python3
"""connect-face-api — localhost face-embedding sidecar for connect-ai-proxy.

FastAPI wrapper around insightface buffalo_l (SCRFD detection + ArcFace-R100
512-d embeddings) on onnxruntime CPU. Binds 127.0.0.1:8799 ONLY — the Go proxy
(camera_face.go) is the sole consumer.

API:
  GET  /health -> {"ok": true, "model": "buffalo_l"}
                  ({"ok": false, "error": ...} before the model has loaded)
  POST /embed  {"image_b64": "<base64 jpeg/png>"} ->
               {"faces": [{"bbox": [x0, y0, x1, y1],   # pixels, float
                           "det_score": 0.87,
                           "embedding": [512 floats]}]}

The model (~330MB) auto-downloads to ~/.insightface on first load; init is
lazy/global with a startup warm thread so /health flips to ok soon after boot.
"""

import base64
import binascii
import threading

import cv2
import numpy as np
from fastapi import FastAPI, HTTPException
from pydantic import BaseModel

MODEL_NAME = "buffalo_l"

app = FastAPI(title="connect-face-api", docs_url=None, redoc_url=None)

_engine = None
_engine_err = ""
_engine_lock = threading.Lock()


def _get_engine():
    """Lazy global insightface init; returns the engine or raises RuntimeError."""
    global _engine, _engine_err
    if _engine is not None:
        return _engine
    with _engine_lock:
        if _engine is not None:
            return _engine
        try:
            from insightface.app import FaceAnalysis

            eng = FaceAnalysis(name=MODEL_NAME, providers=["CPUExecutionProvider"])
            eng.prepare(ctx_id=-1, det_size=(640, 640))
            _engine = eng
            _engine_err = ""
        except Exception as e:  # noqa: BLE001 — surface any init failure via /health
            _engine_err = f"{type(e).__name__}: {e}"
            raise RuntimeError(_engine_err) from e
    return _engine


@app.on_event("startup")
def _warm():
    """Kick model load in the background so /health goes ok without blocking bind."""

    def load():
        try:
            _get_engine()
        except RuntimeError:
            pass  # recorded in _engine_err; retried on next /embed

    threading.Thread(target=load, name="model-warm", daemon=True).start()


@app.get("/health")
def health():
    if _engine is not None:
        return {"ok": True, "model": MODEL_NAME}
    return {"ok": False, "model": MODEL_NAME,
            "error": _engine_err or "model not loaded yet"}


class EmbedRequest(BaseModel):
    image_b64: str = ""


@app.post("/embed")
def embed(req: EmbedRequest):
    raw_b64 = (req.image_b64 or "").strip()
    if not raw_b64:
        raise HTTPException(status_code=400, detail="image_b64 is required")
    # Tolerate data-URI prefixed payloads ("data:image/jpeg;base64,....").
    if raw_b64.startswith("data:") and "," in raw_b64:
        raw_b64 = raw_b64.split(",", 1)[1]
    try:
        raw = base64.b64decode(raw_b64, validate=True)
    except (binascii.Error, ValueError):
        raise HTTPException(status_code=400, detail="image_b64 is not valid base64")
    if not raw:
        raise HTTPException(status_code=400, detail="decoded image is empty")

    buf = np.frombuffer(raw, dtype=np.uint8)
    img = cv2.imdecode(buf, cv2.IMREAD_COLOR)
    if img is None or img.size == 0:
        raise HTTPException(status_code=400, detail="could not decode image (expected jpeg/png)")

    try:
        engine = _get_engine()
    except RuntimeError as e:
        raise HTTPException(status_code=503, detail=f"face model unavailable: {e}")

    faces = []
    for f in engine.get(img):
        emb = getattr(f, "normed_embedding", None)
        if emb is None:
            emb = getattr(f, "embedding", None)
        if emb is None:
            continue
        faces.append({
            "bbox": [float(v) for v in f.bbox],
            "det_score": float(f.det_score),
            "embedding": [float(v) for v in emb],
        })
    return {"faces": faces}


if __name__ == "__main__":
    import uvicorn

    uvicorn.run(app, host="127.0.0.1", port=8799, log_level="info")
