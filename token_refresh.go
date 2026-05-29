package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// Codex OAuth refresh — keeps /root/.codex/auth.json fresh non-interactively.
//
// ChatGPT subscription auth (auth_mode=chatgpt) returns an access_token with
// a ~24h lifetime + a long-lived refresh_token. If the access_token expires,
// every POST /anthropic/v1/messages returns 401 token_expired even though
// GET /models still works. A daily / 6-hourly refresh against
// https://auth.openai.com/oauth/token keeps the proxy serving without
// requiring an interactive `codex login`.
//
// The refresh script lives inside the binary (no external Python deps) and
// is wired into a systemd timer the proxy installs itself the first time it
// runs as root on Linux.

const (
	codexOAuthEndpoint        = "https://auth.openai.com/oauth/token"
	codexRefreshEarlyWindow   = 6 * time.Hour
	codexRefreshTimerName     = "connect-ai-proxy-refresh.timer"
	codexRefreshServiceName   = "connect-ai-proxy-refresh.service"
	codexRefreshSystemUnitDir = "/etc/systemd/system"
)

// runRefreshCodexToken implements the `refresh-token` subcommand. Safe to run
// from a timer: if the access_token still has more than codexRefreshEarlyWindow
// of life left, it logs and exits 0 without contacting the network.
func runRefreshCodexToken() error {
	cfg := loadConfig()
	authPath := strings.TrimSpace(cfg.CodexAuthFile)
	if authPath == "" {
		authPath = defaultCodexAuthFile()
	}
	if authPath == "" {
		return errors.New("could not resolve codex auth.json path")
	}

	auth, err := readCodexAuthFile(authPath)
	if err != nil {
		return err
	}

	tokens, _ := auth["tokens"].(map[string]any)
	if tokens == nil {
		return fmt.Errorf("auth file %s has no `tokens` object", authPath)
	}

	refreshToken, _ := tokens["refresh_token"].(string)
	if refreshToken == "" {
		return fmt.Errorf("auth file %s has no refresh_token — interactive `codex login` required", authPath)
	}

	clientID := extractClientIDFromIDToken(tokens["id_token"])
	if clientID == "" {
		return fmt.Errorf("could not extract client_id from id_token in %s", authPath)
	}

	now := time.Now().Unix()
	exp := extractExpFromAccessToken(tokens["access_token"])
	remaining := exp - now
	logTokenRefresh("access_token remaining=%ds (early-refresh threshold=%ds)", remaining, int64(codexRefreshEarlyWindow.Seconds()))

	if exp > 0 && remaining > int64(codexRefreshEarlyWindow.Seconds()) {
		logTokenRefresh("token still fresh — skipping refresh")
		return nil
	}

	newTokens, err := exchangeRefreshToken(clientID, refreshToken)
	if err != nil {
		return err
	}

	for _, k := range []string{"access_token", "id_token", "refresh_token"} {
		if v, ok := newTokens[k].(string); ok && v != "" {
			tokens[k] = v
		}
	}
	auth["tokens"] = tokens
	auth["last_refresh"] = time.Now().UTC().Format(time.RFC3339)

	if err := writeCodexAuthFileAtomic(authPath, auth); err != nil {
		return fmt.Errorf("write %s: %w", authPath, err)
	}

	newExp := extractExpFromAccessToken(tokens["access_token"])
	logTokenRefresh("rotated tokens — new exp=%d (in %ds)", newExp, newExp-now)
	return nil
}

// readCodexAuthFile decodes auth.json into a generic map so we can preserve
// every field — auth_mode, OPENAI_API_KEY, account_id, last_refresh, etc. —
// when writing back. Round-tripping through a typed struct risked dropping
// fields the proxy doesn't know about.
func readCodexAuthFile(path string) (map[string]any, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var auth map[string]any
	if err := json.Unmarshal(raw, &auth); err != nil {
		return nil, fmt.Errorf("decode %s: %w", path, err)
	}
	return auth, nil
}

// writeCodexAuthFileAtomic writes auth.json via temp-file + rename so the
// original is never partially overwritten. The prior file is rolled into a
// single .prev slot (no unbounded backup growth).
func writeCodexAuthFileAtomic(path string, auth map[string]any) error {
	dir := filepath.Dir(path)
	body, err := json.MarshalIndent(auth, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".auth.json.tmp.*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(body); err != nil {
		tmp.Close()
		_ = os.Remove(tmpPath)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	if err := os.Chmod(tmpPath, 0o600); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	// Stash the previous file into a single rolling slot — if rename fails
	// because the source file doesn't exist (fresh install) that's fine.
	prev := path + ".prev"
	if err := os.Rename(path, prev); err != nil && !errors.Is(err, os.ErrNotExist) {
		// Non-fatal: continue with the rename.
	}
	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	return nil
}

// exchangeRefreshToken posts grant_type=refresh_token to OpenAI's auth
// endpoint and returns the JSON response as a generic map.
func exchangeRefreshToken(clientID, refreshToken string) (map[string]any, error) {
	payload := map[string]string{
		"grant_type":    "refresh_token",
		"client_id":     clientID,
		"refresh_token": refreshToken,
		"scope":         "openid profile email offline_access",
	}
	body, _ := json.Marshal(payload)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, codexOAuthEndpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("oauth refresh transport error: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("oauth refresh failed: HTTP %d body=%s", resp.StatusCode, truncate(string(respBody), 400))
	}
	var out map[string]any
	if err := json.Unmarshal(respBody, &out); err != nil {
		return nil, fmt.Errorf("decode oauth response: %w", err)
	}
	return out, nil
}

// extractClientIDFromIDToken decodes the JWT payload and returns the first
// audience (the OAuth client_id ChatGPT issued the token to).
func extractClientIDFromIDToken(idToken any) string {
	tok, _ := idToken.(string)
	claims := decodeJWTClaims(tok)
	if claims == nil {
		return ""
	}
	switch aud := claims["aud"].(type) {
	case string:
		return aud
	case []any:
		if len(aud) > 0 {
			if s, ok := aud[0].(string); ok {
				return s
			}
		}
	}
	return ""
}

// extractExpFromAccessToken returns the `exp` claim (unix seconds), 0 when
// unavailable.
func extractExpFromAccessToken(accessToken any) int64 {
	tok, _ := accessToken.(string)
	claims := decodeJWTClaims(tok)
	if claims == nil {
		return 0
	}
	switch v := claims["exp"].(type) {
	case float64:
		return int64(v)
	case int64:
		return v
	case int:
		return int64(v)
	}
	return 0
}

func decodeJWTClaims(jwt string) map[string]any {
	parts := strings.Split(jwt, ".")
	if len(parts) < 2 {
		return nil
	}
	payload := parts[1]
	// JWTs use URL-safe base64 with optional padding stripped.
	if pad := len(payload) % 4; pad != 0 {
		payload += strings.Repeat("=", 4-pad)
	}
	raw, err := base64.URLEncoding.DecodeString(payload)
	if err != nil {
		return nil
	}
	var claims map[string]any
	if err := json.Unmarshal(raw, &claims); err != nil {
		return nil
	}
	return claims
}

func logTokenRefresh(format string, args ...any) {
	fmt.Fprintf(os.Stdout, "[codex-refresh %s] %s\n", time.Now().UTC().Format(time.RFC3339), fmt.Sprintf(format, args...))
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// ─── Systemd timer install / uninstall ───────────────────────────────────────

// installTokenRefreshTimer writes the .service + .timer unit files at
// /etc/systemd/system and enables the timer. Linux + root only — silently
// no-ops on other platforms / unprivileged users, since refreshing as a
// non-root user typically can't touch /root/.codex anyway.
func installTokenRefreshTimer() error {
	if runtime.GOOS != "linux" {
		fmt.Println("install-token-refresh: only supported on Linux (skipped).")
		return nil
	}
	if os.Geteuid() != 0 {
		fmt.Println("install-token-refresh: must run as root to write /etc/systemd/system (skipped).")
		return nil
	}

	bin, err := os.Executable()
	if err != nil || strings.TrimSpace(bin) == "" {
		bin = "/usr/local/bin/connect-ai-proxy"
	}

	service := fmt.Sprintf(`[Unit]
Description=Connect AI Proxy — Codex token refresh
After=network-online.target
Wants=network-online.target

[Service]
Type=oneshot
User=root
WorkingDirectory=/opt/connect-ai-proxy
Environment=HOME=/root
Environment=PATH=/usr/local/go/bin:/usr/local/bin:/usr/bin:/bin
ExecStart=%s refresh-token
StandardOutput=journal
StandardError=journal
`, bin)

	// Fire every 6h with a small randomised delay to spread load. Runs once
	// on boot too so a freshly-rebooted host catches up immediately.
	timer := `[Unit]
Description=Connect AI Proxy — Codex token refresh timer

[Timer]
OnBootSec=2min
OnUnitActiveSec=6h
RandomizedDelaySec=10min
AccuracySec=1min
Persistent=true
Unit=connect-ai-proxy-refresh.service

[Install]
WantedBy=timers.target
`

	servicePath := filepath.Join(codexRefreshSystemUnitDir, codexRefreshServiceName)
	timerPath := filepath.Join(codexRefreshSystemUnitDir, codexRefreshTimerName)

	if err := os.WriteFile(servicePath, []byte(service), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", servicePath, err)
	}
	if err := os.WriteFile(timerPath, []byte(timer), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", timerPath, err)
	}
	if err := runSystemctl("daemon-reload"); err != nil {
		return err
	}
	if err := runSystemctl("enable", "--now", codexRefreshTimerName); err != nil {
		return fmt.Errorf("enable %s: %w", codexRefreshTimerName, err)
	}
	fmt.Printf("Installed %s + %s and enabled the timer (fires every 6h).\n", codexRefreshServiceName, codexRefreshTimerName)
	return nil
}

func uninstallTokenRefreshTimer() error {
	if runtime.GOOS != "linux" {
		fmt.Println("uninstall-token-refresh: only supported on Linux (skipped).")
		return nil
	}
	if os.Geteuid() != 0 {
		fmt.Println("uninstall-token-refresh: must run as root (skipped).")
		return nil
	}
	_ = runSystemctl("disable", "--now", codexRefreshTimerName)
	_ = os.Remove(filepath.Join(codexRefreshSystemUnitDir, codexRefreshTimerName))
	_ = os.Remove(filepath.Join(codexRefreshSystemUnitDir, codexRefreshServiceName))
	_ = runSystemctl("daemon-reload")
	fmt.Println("Removed Codex token refresh timer + service.")
	return nil
}

// ensureTokenRefreshTimer is called from `serve` startup on Linux as root.
// First-run after a binary upgrade auto-provisions the timer so operators
// don't have to remember to run install-token-refresh manually.
func ensureTokenRefreshTimer() {
	if runtime.GOOS != "linux" || os.Geteuid() != 0 {
		return
	}
	if _, err := os.Stat(filepath.Join(codexRefreshSystemUnitDir, codexRefreshTimerName)); err == nil {
		return
	}
	if err := installTokenRefreshTimer(); err != nil {
		fmt.Fprintf(os.Stderr, "ensure-token-refresh: %v (will retry next start)\n", err)
	}
}

func runSystemctl(args ...string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "systemctl", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			msg = err.Error()
		}
		return fmt.Errorf("systemctl %s failed: %s", strings.Join(args, " "), msg)
	}
	return nil
}
