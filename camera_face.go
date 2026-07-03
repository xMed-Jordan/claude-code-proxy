package main

// camera_face.go — Go client for the localhost face-embedding sidecar
// (faceapi/: FastAPI + insightface buffalo_l — SCRFD detect + ArcFace-R100
// 512-d embed, onnxruntime CPU, bound to 127.0.0.1:8799).
//
// The sidecar API: GET /health → {ok, model}; POST /embed {image_b64} →
// {faces:[{bbox:[x0,y0,x1,y1] px, det_score, embedding:[512]}]}. Embeddings are
// stored in SQLite as float32 little-endian BLOBs (camEmbeddingBlob /
// camEmbeddingFromBlob); matching is cosine similarity in Go against
// cfg.FaceMatchThreshold; detections below cfg.FaceMinScore are dropped.
// The face path is the PRIMARY match path for humans; VLM appearance matching
// (camera_avatar_scan.go / camera_avatar_tools.go) is the fallback when the
// sidecar is down, no face is visible, or the avatar isn't human.

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"math"
	"net/http"
	"strings"
	"time"
)

// camFace is one detected face: its pixel bounding box in the source frame, the
// sidecar's detection score, and the 512-d ArcFace embedding.
type camFace struct {
	BBox      image.Rectangle
	Score     float64
	Embedding []float32
}

// camFaceEnabled reports whether the face sidecar should be used: the
// cfg.FaceEnabled flag plus a configured sidecar URL. Reachability is
// discovered per call — camFaceDetect errors make callers fall back to the
// VLM appearance path.
func camFaceEnabled(cfg config) bool {
	return cfg.FaceEnabled && cfg.FaceAPIURL != ""
}

// camFaceRespMax caps the sidecar response we are willing to buffer: a frame
// crowded with dozens of faces at 512 floats each stays well under 8MB.
const camFaceRespMax = 8 << 20

// camFaceDetect posts a JPEG to the sidecar's /embed (10s timeout, plain
// localhost client — the sidecar binds 127.0.0.1 only) and returns every
// detected face with Score >= cfg.FaceMinScore.
func camFaceDetect(ctx context.Context, cfg config, jpeg []byte) ([]camFace, error) {
	if !camFaceEnabled(cfg) {
		return nil, errors.New("face sidecar disabled")
	}
	if len(jpeg) == 0 {
		return nil, errors.New("empty image")
	}
	payload, err := json.Marshal(map[string]string{
		"image_b64": base64.StdEncoding.EncodeToString(jpeg),
	})
	if err != nil {
		return nil, err
	}
	url := strings.TrimRight(cfg.FaceAPIURL, "/") + "/embed"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("face sidecar: %w", err)
	}
	defer resp.Body.Close()
	body := readAllLimited(resp.Body, camFaceRespMax)
	if resp.StatusCode != http.StatusOK {
		snippet := strings.TrimSpace(string(body))
		if len(snippet) > 200 {
			snippet = snippet[:200]
		}
		return nil, fmt.Errorf("face sidecar status %d: %s", resp.StatusCode, snippet)
	}
	var out struct {
		Faces []struct {
			BBox      []float64 `json:"bbox"`
			DetScore  float64   `json:"det_score"`
			Embedding []float32 `json:"embedding"`
		} `json:"faces"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("face sidecar response: %w", err)
	}
	faces := make([]camFace, 0, len(out.Faces))
	for _, f := range out.Faces {
		if f.DetScore < cfg.FaceMinScore {
			continue
		}
		if len(f.BBox) != 4 || len(f.Embedding) == 0 {
			continue
		}
		r := image.Rect(
			int(math.Round(f.BBox[0])), int(math.Round(f.BBox[1])),
			int(math.Round(f.BBox[2])), int(math.Round(f.BBox[3])),
		).Canon()
		faces = append(faces, camFace{BBox: r, Score: f.DetScore, Embedding: f.Embedding})
	}
	return faces, nil
}

// camCosine is the cosine similarity of two embeddings (0 on length mismatch
// or zero vectors).
func camCosine(a, b []float32) float64 {
	if len(a) == 0 || len(a) != len(b) {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		x, y := float64(a[i]), float64(b[i])
		dot += x * y
		na += x * x
		nb += y * y
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

// camEmbeddingBlob serializes an embedding as float32 little-endian bytes for
// BLOB storage.
func camEmbeddingBlob(v []float32) []byte {
	if len(v) == 0 {
		return nil
	}
	b := make([]byte, 4*len(v))
	for i, f := range v {
		binary.LittleEndian.PutUint32(b[4*i:], math.Float32bits(f))
	}
	return b
}

// camEmbeddingFromBlob deserializes a float32-LE BLOB (nil on malformed input).
func camEmbeddingFromBlob(b []byte) []float32 {
	if len(b) == 0 || len(b)%4 != 0 {
		return nil
	}
	v := make([]float32, len(b)/4)
	for i := range v {
		v[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[4*i:]))
	}
	return v
}

// camAvatarCentroid is the L2-normalized mean of an avatar's reference
// embeddings — the single vector cached on camera_avatars.embedding and
// recomputed on every reference change. nil when refs is empty (or no ref
// has a usable vector).
func camAvatarCentroid(refs [][]float32) []float32 {
	var sum []float64
	n := 0
	for _, r := range refs {
		if len(r) == 0 {
			continue
		}
		if sum == nil {
			sum = make([]float64, len(r))
		}
		if len(r) != len(sum) {
			continue // mismatched dimensionality — skip rather than corrupt
		}
		for i, f := range r {
			sum[i] += float64(f)
		}
		n++
	}
	if n == 0 {
		return nil
	}
	var norm float64
	for i := range sum {
		sum[i] /= float64(n)
		norm += sum[i] * sum[i]
	}
	norm = math.Sqrt(norm)
	if norm == 0 {
		return nil
	}
	out := make([]float32, len(sum))
	for i := range sum {
		out[i] = float32(sum[i] / norm)
	}
	return out
}
