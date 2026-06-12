package main

// groqstt.go — Groq Whisper speech-to-text for the agy media path.
//
// agy (the Antigravity CLI) has no native audio understanding: its CLI only takes
// workspace files (--add-dir), so given a voice note the agent improvises with
// shell tools (pip-installing an STT lib, calling a free web endpoint) — slow,
// non-deterministic, and weak on dialectal Arabic. Instead, when an agy-routed
// request carries ONLY audio/video and a Groq key is configured, we transcribe it
// directly with Groq's whisper-large-v3 (free tier, ~1s, strong on Arabic) and
// return the transcript — no agy round-trip. If Groq errors, the caller surfaces
// an error so the client's own fallback chain (e.g. Connect → Vertex) takes over.

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const groqTranscribeURL = "https://api.groq.com/openai/v1/audio/transcriptions"

// groqSTTEnabled reports whether Groq transcription is configured.
func groqSTTEnabled(cfg config) bool {
	return strings.TrimSpace(cfg.GroqAPIKey) != ""
}

// isAudioVideoPart classifies a media part as audio/video by media type or extension.
func isAudioVideoPart(p mediaPart) bool {
	mt := strings.ToLower(strings.TrimSpace(p.MediaType))
	if i := strings.Index(mt, ";"); i >= 0 {
		mt = strings.TrimSpace(mt[:i])
	}
	if strings.HasPrefix(mt, "audio/") || strings.HasPrefix(mt, "video/") {
		return true
	}
	if mt == "" {
		switch mediaKindFor(mediaExtFor(p.MediaType, p.Filename)) {
		case "audio", "video":
			return true
		}
	}
	return false
}

// agyAudioTranscript transcribes audio/video attachments via Groq. It returns
// (transcript, true, nil) only when EVERY part is audio/video and Groq is enabled —
// the audio-only case Connect's media_extractor produces. Mixed media (audio +
// image/doc) returns ("", false, nil) so the normal agy flow handles it. A Groq/API
// failure returns ("", false, err) so the caller can error out and let the client's
// fallback (Vertex) take over.
func agyAudioTranscript(ctx context.Context, cfg config, parts []mediaPart) (string, bool, error) {
	if !groqSTTEnabled(cfg) || len(parts) == 0 {
		return "", false, nil
	}
	for _, p := range parts {
		if !isAudioVideoPart(p) {
			return "", false, nil // mixed → defer to normal agy media flow
		}
	}
	var transcripts []string
	for _, p := range parts {
		path, cleanup, err := stageMediaToTemp(ctx, cfg, p)
		if err != nil {
			return "", false, err
		}
		text, err := transcribeWithGroq(ctx, cfg, path)
		cleanup()
		if err != nil {
			return "", false, err
		}
		if t := strings.TrimSpace(text); t != "" {
			transcripts = append(transcripts, t)
		}
	}
	if len(transcripts) == 0 {
		return "", false, fmt.Errorf("groq returned an empty transcript")
	}
	return strings.Join(transcripts, "\n\n"), true, nil
}

// stageMediaToTemp writes a media part's bytes to a capped temp file for upload to
// Groq, returning the path and a cleanup func. Unlike materializeMedia it does NOT
// virus-scan or content-address: the file is only POSTed to the STT API (never
// executed, never read by an agent), and skipping the (slow) VirusTotal upload
// keeps transcription latency around a second.
func stageMediaToTemp(ctx context.Context, cfg config, p mediaPart) (string, func(), error) {
	noop := func() {}
	root := agyMediaRoot(cfg)
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", noop, err
	}
	ext := mediaExtFor(p.MediaType, p.Filename)
	f, err := os.CreateTemp(root, "stt-*"+ext)
	if err != nil {
		return "", noop, err
	}
	path := f.Name()
	f.Close()
	cleanup := func() { _ = os.Remove(path) }

	perMax := mediaPerFileMax(cfg)
	var src io.Reader
	switch {
	case p.URL != "":
		if !cfg.AgyMediaAllowURLs {
			cleanup()
			return "", noop, fmt.Errorf("remote media URLs are disabled")
		}
		dlTimeout := cfg.AgyMediaTimeout
		if dlTimeout <= 0 {
			dlTimeout = 300 * time.Second
		}
		dctx, c := context.WithTimeout(ctx, dlTimeout)
		defer c()
		body, derr := openMediaURL(dctx, p.URL, perMax)
		if derr != nil {
			cleanup()
			return "", noop, derr
		}
		defer body.Close()
		src = body
	case p.B64 != "":
		src = base64.NewDecoder(base64.StdEncoding, newB64CleanReader(p.B64))
	default:
		cleanup()
		return "", noop, fmt.Errorf("media part has no data")
	}

	var total int64
	if _, _, err := saveStreamCapped(path, src, perMax, perMax, &total); err != nil {
		cleanup()
		return "", noop, err
	}
	return path, cleanup, nil
}

// transcribeWithGroq POSTs an audio file to Groq's Whisper endpoint and returns the
// transcript. response_format=json → {"text": "..."}.
func transcribeWithGroq(ctx context.Context, cfg config, audioPath string) (string, error) {
	model := strings.TrimSpace(cfg.GroqSTTModel)
	if model == "" {
		model = "whisper-large-v3"
	}
	timeout := cfg.GroqSTTTimeout
	if timeout <= 0 {
		timeout = 90 * time.Second
	}

	file, err := os.Open(audioPath)
	if err != nil {
		return "", err
	}
	defer file.Close()

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fw, err := mw.CreateFormFile("file", filepath.Base(audioPath))
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(fw, file); err != nil {
		return "", err
	}
	_ = mw.WriteField("model", model)
	_ = mw.WriteField("response_format", "json")
	_ = mw.WriteField("temperature", "0")
	if lang := strings.TrimSpace(cfg.GroqSTTLanguage); lang != "" {
		_ = mw.WriteField("language", lang)
	}
	if prompt := strings.TrimSpace(cfg.GroqSTTPrompt); prompt != "" {
		_ = mw.WriteField("prompt", prompt)
	}
	if err := mw.Close(); err != nil {
		return "", err
	}

	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodPost, groqTranscribeURL, &body)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(cfg.GroqAPIKey))
	req.Header.Set("Content-Type", mw.FormDataContentType())

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("groq request failed: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("groq status %d: %s", resp.StatusCode, truncateString(strings.TrimSpace(string(raw)), 300))
	}
	var out struct {
		Text  string `json:"text"`
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", fmt.Errorf("groq response not JSON: %s", truncateString(string(raw), 200))
	}
	if strings.TrimSpace(out.Error.Message) != "" {
		return "", fmt.Errorf("groq error: %s", out.Error.Message)
	}
	return out.Text, nil
}
