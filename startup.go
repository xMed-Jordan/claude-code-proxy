package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const startupName = "claude-code-proxy"

func installStartup() error {
	if err := ensureCurrentBinaryBuilt(); err != nil {
		return err
	}
	switch runtime.GOOS {
	case "windows":
		return installWindowsStartup()
	case "darwin":
		return installMacStartup()
	case "linux":
		return installLinuxStartup()
	default:
		return fmt.Errorf("autostart is not implemented for %s", runtime.GOOS)
	}
}

func uninstallStartup() error {
	switch runtime.GOOS {
	case "windows":
		return runCommand("schtasks.exe", "/Delete", "/TN", "ClaudeCodeCodexProxy", "/F")
	case "darwin":
		path := filepath.Join(userHomeDir(), "Library", "LaunchAgents", "com.claude-code-proxy.plist")
		_ = runCommand("launchctl", "unload", path)
		_ = os.Remove(path)
		fmt.Println("Removed macOS LaunchAgent.")
		return nil
	case "linux":
		path := filepath.Join(userHomeDir(), ".config", "systemd", "user", "claude-code-proxy.service")
		_ = runCommand("systemctl", "--user", "disable", "--now", "claude-code-proxy.service")
		_ = os.Remove(path)
		fmt.Println("Removed Linux user service.")
		return nil
	default:
		return fmt.Errorf("autostart is not implemented for %s", runtime.GOOS)
	}
}

func installWindowsStartup() error {
	bin := preferredProxyBinaryPath()
	psCommand := fmt.Sprintf("Set-Location -LiteralPath %s; & %s start", powershellQuote(mustGetwd()), powershellQuote(bin))
	taskRun := `powershell.exe -NoProfile -ExecutionPolicy Bypass -Command "` + strings.ReplaceAll(psCommand, `"`, `\"`) + `"`
	if err := runCommand("schtasks.exe", "/Create", "/SC", "ONLOGON", "/TN", "ClaudeCodeCodexProxy", "/TR", taskRun, "/F"); err != nil {
		return err
	}
	fmt.Println("Installed startup task 'ClaudeCodeCodexProxy'.")
	return nil
}

func powershellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "''") + "'"
}

func installMacStartup() error {
	launchAgents := filepath.Join(userHomeDir(), "Library", "LaunchAgents")
	if err := os.MkdirAll(launchAgents, 0700); err != nil {
		return err
	}
	path := filepath.Join(launchAgents, "com.claude-code-proxy.plist")
	content := []byte(fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>com.claude-code-proxy</string>
  <key>ProgramArguments</key>
  <array>
    <string>%s</string>
    <string>start</string>
  </array>
  <key>WorkingDirectory</key>
  <string>%s</string>
  <key>RunAtLoad</key>
  <true/>
  <key>KeepAlive</key>
  <false/>
</dict>
</plist>
`, xmlEscape(preferredProxyBinaryPath()), xmlEscape(mustGetwd())))
	if err := os.WriteFile(path, content, 0600); err != nil {
		return err
	}
	_ = runCommand("launchctl", "unload", path)
	if err := runCommand("launchctl", "load", "-w", path); err != nil {
		fmt.Printf("Wrote LaunchAgent at %s. Load it manually with: launchctl load -w %s\n", path, path)
		return nil
	}
	fmt.Printf("Installed macOS LaunchAgent: %s\n", path)
	return nil
}

func xmlEscape(value string) string {
	value = strings.ReplaceAll(value, "&", "&amp;")
	value = strings.ReplaceAll(value, "<", "&lt;")
	value = strings.ReplaceAll(value, ">", "&gt;")
	value = strings.ReplaceAll(value, `"`, "&quot;")
	return value
}

func installLinuxStartup() error {
	systemctl, err := exec.LookPath("systemctl")
	if err != nil || strings.TrimSpace(systemctl) == "" {
		fmt.Printf("systemd user services are not available. Start manually with: %s\n", commandDisplay(preferredProxyBinaryPath(), "start"))
		return nil
	}
	dir := filepath.Join(userHomeDir(), ".config", "systemd", "user")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	path := filepath.Join(dir, "claude-code-proxy.service")
	service := fmt.Sprintf(`[Unit]
Description=Claude Code Codex Proxy

[Service]
Type=oneshot
WorkingDirectory=%s
ExecStart=%s start
RemainAfterExit=yes
ExecStop=%s stop

[Install]
WantedBy=default.target
`, systemdEscape(mustGetwd()), systemdEscape(preferredProxyBinaryPath()), systemdEscape(preferredProxyBinaryPath()))
	if err := os.WriteFile(path, []byte(service), 0600); err != nil {
		return err
	}
	if err := runCommand("systemctl", "--user", "daemon-reload"); err != nil {
		return err
	}
	if err := runCommand("systemctl", "--user", "enable", "--now", "claude-code-proxy.service"); err != nil {
		fmt.Printf("Wrote systemd user service at %s. Enable manually with: systemctl --user enable --now claude-code-proxy.service\n", path)
		return nil
	}
	fmt.Printf("Installed Linux user service: %s\n", path)
	return nil
}

func systemdEscape(value string) string {
	if strings.ContainsAny(value, " \t\n\"'") {
		return strconv.Quote(value)
	}
	return value
}

func runCommand(name string, args ...string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			msg = err.Error()
		}
		return fmt.Errorf("%s failed: %s", name, msg)
	}
	if strings.TrimSpace(string(out)) != "" {
		fmt.Println(strings.TrimSpace(string(out)))
	}
	return nil
}
