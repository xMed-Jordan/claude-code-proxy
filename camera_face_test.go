package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"image"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCamCosine(t *testing.T) {
	a := []float32{1, 2, 3}
	if got := camCosine(a, a); math.Abs(got-1) > 1e-6 {
		t.Errorf("cosine(a,a) = %f want 1", got)
	}
	if got := camCosine([]float32{1, 0}, []float32{0, 1}); math.Abs(got) > 1e-6 {
		t.Errorf("orthogonal = %f want 0", got)
	}
	if got := camCosine([]float32{1, 0}, []float32{-1, 0}); math.Abs(got+1) > 1e-6 {
		t.Errorf("opposite = %f want -1", got)
	}
	if got := camCosine([]float32{1, 2}, []float32{1, 2, 3}); got != 0 {
		t.Errorf("length mismatch = %f want 0", got)
	}
	if got := camCosine([]float32{0, 0}, []float32{1, 2}); got != 0 {
		t.Errorf("zero vector = %f want 0", got)
	}
	if got := camCosine(nil, nil); got != 0 {
		t.Errorf("nil vectors = %f want 0", got)
	}
}

func TestCamEmbeddingBlobRoundTrip(t *testing.T) {
	v := []float32{0.5, -1.25, 3.0625e-3, 0, 123456.78}
	b := camEmbeddingBlob(v)
	if len(b) != 4*len(v) {
		t.Fatalf("blob len %d want %d", len(b), 4*len(v))
	}
	got := camEmbeddingFromBlob(b)
	if len(got) != len(v) {
		t.Fatalf("round trip len %d want %d", len(got), len(v))
	}
	for i := range v {
		if got[i] != v[i] {
			t.Errorf("v[%d] = %v want %v", i, got[i], v[i])
		}
	}
	if camEmbeddingBlob(nil) != nil {
		t.Error("blob of empty vector should be nil")
	}
	if camEmbeddingFromBlob(nil) != nil {
		t.Error("nil blob should decode to nil")
	}
	if camEmbeddingFromBlob([]byte{1, 2, 3}) != nil {
		t.Error("misaligned blob should decode to nil")
	}
}

func TestCamAvatarCentroid(t *testing.T) {
	c := camAvatarCentroid([][]float32{{1, 0}, {0, 1}})
	if len(c) != 2 {
		t.Fatalf("centroid len %d want 2", len(c))
	}
	want := float32(1 / math.Sqrt2)
	if math.Abs(float64(c[0]-want)) > 1e-6 || math.Abs(float64(c[1]-want)) > 1e-6 {
		t.Errorf("centroid = %v want [%f %f]", c, want, want)
	}
	var norm float64
	for _, f := range c {
		norm += float64(f) * float64(f)
	}
	if math.Abs(norm-1) > 1e-6 {
		t.Errorf("centroid norm^2 = %f want 1", norm)
	}

	if camAvatarCentroid(nil) != nil {
		t.Error("empty refs should give nil")
	}
	if camAvatarCentroid([][]float32{{}, nil}) != nil {
		t.Error("all-empty refs should give nil")
	}
	if camAvatarCentroid([][]float32{{0, 0}}) != nil {
		t.Error("zero-norm mean should give nil")
	}
	// Mismatched dimensionality is skipped, not mixed in.
	c = camAvatarCentroid([][]float32{{2, 0}, {1, 2, 3}})
	if len(c) != 2 || math.Abs(float64(c[0])-1) > 1e-6 || c[1] != 0 {
		t.Errorf("mismatched refs centroid = %v want [1 0]", c)
	}
}

func TestCamFaceEnabled(t *testing.T) {
	if !camFaceEnabled(config{FaceEnabled: true, FaceAPIURL: "http://127.0.0.1:8799"}) {
		t.Error("enabled + URL should be true")
	}
	if camFaceEnabled(config{FaceEnabled: false, FaceAPIURL: "http://127.0.0.1:8799"}) {
		t.Error("flag off should be false")
	}
	if camFaceEnabled(config{FaceEnabled: true, FaceAPIURL: ""}) {
		t.Error("empty URL should be false")
	}
}

func TestCamFaceDetect(t *testing.T) {
	frame := []byte{0xff, 0xd8, 0xff, 0xe0, 1, 2, 3}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/embed" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		var req struct {
			ImageB64 string `json:"image_b64"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode request: %v", err)
		}
		if got, _ := base64.StdEncoding.DecodeString(req.ImageB64); string(got) != string(frame) {
			t.Error("image_b64 does not round-trip the frame")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"faces":[
			{"bbox":[10.4,20.6,110.2,220.9],"det_score":0.91,"embedding":[1,0,0]},
			{"bbox":[5,5,20,20],"det_score":0.30,"embedding":[0,1,0]},
			{"bbox":[1,2,3],"det_score":0.95,"embedding":[0,0,1]}
		]}`))
	}))
	defer srv.Close()

	cfg := config{FaceEnabled: true, FaceAPIURL: srv.URL, FaceMinScore: 0.5}
	faces, err := camFaceDetect(context.Background(), cfg, frame)
	if err != nil {
		t.Fatalf("camFaceDetect: %v", err)
	}
	if len(faces) != 1 {
		t.Fatalf("faces = %d want 1 (low score + malformed bbox filtered)", len(faces))
	}
	f := faces[0]
	if want := image.Rect(10, 21, 110, 221); f.BBox != want {
		t.Errorf("bbox = %v want %v", f.BBox, want)
	}
	if f.Score != 0.91 {
		t.Errorf("score = %f want 0.91", f.Score)
	}
	if len(f.Embedding) != 3 || f.Embedding[0] != 1 {
		t.Errorf("embedding = %v", f.Embedding)
	}

	// Non-200 surfaces as an error.
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"detail":"boom"}`, http.StatusServiceUnavailable)
	}))
	defer bad.Close()
	if _, err := camFaceDetect(context.Background(), config{FaceEnabled: true, FaceAPIURL: bad.URL, FaceMinScore: 0.5}, frame); err == nil {
		t.Error("expected error on 503")
	}

	// Disabled config short-circuits.
	if _, err := camFaceDetect(context.Background(), config{}, frame); err == nil {
		t.Error("expected error when face engine disabled")
	}
	// Empty frame rejected before any network use.
	if _, err := camFaceDetect(context.Background(), cfg, nil); err == nil {
		t.Error("expected error on empty image")
	}
}
