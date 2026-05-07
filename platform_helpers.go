package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
)

type desktopConfigTarget struct {
	Label        string
	Path         string
	Directory    string
	Snapshot     string
	SnapshotMeta string
	Create       bool
	Official     bool
}

func platformName() string {
	switch runtime.GOOS {
	case "windows":
		return "windows"
	case "darwin":
		return "macos"
	case "linux":
		return "linux"
	default:
		return runtime.GOOS
	}
}

func userHomeDir() string {
	if home, err := os.UserHomeDir(); err == nil && strings.TrimSpace(home) != "" {
		return home
	}
	for _, key := range []string{"HOME", "USERPROFILE"} {
		if value := strings.TrimSpace(os.Getenv(key)); value != "" {
			return value
		}
	}
	return ""
}

func defaultCodexAuthFile() string {
	if home := userHomeDir(); home != "" {
		return filepath.Join(home, ".codex", "auth.json")
	}
	return filepath.Join(".codex", "auth.json")
}

func defaultClaudeSettingsPath() string {
	if home := userHomeDir(); home != "" {
		return filepath.Join(home, ".claude", "settings.json")
	}
	return filepath.Join(".claude", "settings.json")
}

func defaultClaudeMemoryPath() string {
	if home := userHomeDir(); home != "" {
		return filepath.Join(home, ".claude", "CLAUDE.md")
	}
	return filepath.Join(".claude", "CLAUDE.md")
}

func defaultClaudeRootConfigPath() string {
	if home := userHomeDir(); home != "" {
		return filepath.Join(home, ".claude.json")
	}
	return ".claude.json"
}

func defaultClaudeGatewayModelsCachePath() string {
	if home := userHomeDir(); home != "" {
		return filepath.Join(home, ".claude", "cache", "gateway-models.json")
	}
	return filepath.Join(".claude", "cache", "gateway-models.json")
}

func proxyBinaryNameFor(goos string) string {
	if goos == "windows" {
		return "connect-ai-proxy.exe"
	}
	return "connect-ai-proxy"
}

func currentProxyBinaryName() string {
	return proxyBinaryNameFor(runtime.GOOS)
}

func proxyBinDir() string {
	return filepath.Join(mustGetwd(), "bin")
}

func currentProxyBinaryPath() string {
	return filepath.Join(proxyBinDir(), currentProxyBinaryName())
}

func preferredProxyBinaryPath() string {
	if path := currentProxyBinaryPath(); fileExists(path) {
		if abs, err := filepath.Abs(path); err == nil {
			return abs
		}
		return path
	}
	if exe, err := os.Executable(); err == nil && strings.TrimSpace(exe) != "" {
		if abs, err := filepath.Abs(exe); err == nil {
			return abs
		}
		return exe
	}
	return currentProxyBinaryPath()
}

func mcpServerConfig(includeType bool) map[string]any {
	server := map[string]any{
		"command": preferredProxyBinaryPath(),
		"args":    []string{"browser-mcp"},
		"env": map[string]string{
			"CCP_BROWSER_MCP": "antigravity-browser",
		},
	}
	if includeType {
		server["type"] = "stdio"
	}
	return server
}

func hookCommandString(subcommand string) string {
	return shellCommandString(preferredProxyBinaryPath(), subcommand)
}

func shellCommandString(command string, args ...string) string {
	parts := []string{shellQuote(command)}
	for _, arg := range args {
		parts = append(parts, shellQuote(arg))
	}
	return strings.Join(parts, " ")
}

func shellQuote(value string) string {
	if value == "" {
		return `""`
	}
	if runtime.GOOS == "windows" {
		escaped := strings.ReplaceAll(value, `"`, `\"`)
		return `"` + escaped + `"`
	}
	return `'` + strings.ReplaceAll(value, `'`, `'\''`) + `'`
}

func commandDisplay(command string, args ...string) string {
	return shellCommandString(command, args...)
}

func desktopSupportedOnCurrentPlatform() bool {
	return runtime.GOOS == "windows" || runtime.GOOS == "darwin"
}

func claudeDesktopConfigTargets() []desktopConfigTarget {
	base := mustGetwd()
	var targets []desktopConfigTarget
	switch runtime.GOOS {
	case "windows":
		if localAppData := strings.TrimSpace(os.Getenv("LOCALAPPDATA")); localAppData != "" {
			path := filepath.Join(localAppData, "Claude-3p", "claude_desktop_config.json")
			targets = append(targets, desktopConfigTarget{
				Label:        "Claude Desktop active config",
				Path:         path,
				Directory:    filepath.Dir(path),
				Snapshot:     filepath.Join(base, ".claude-desktop-active-config.snapshot.json"),
				SnapshotMeta: filepath.Join(base, ".claude-desktop-active-config.snapshot.meta.json"),
				Create:       true,
				Official:     true,
			})
		}
		if appData := strings.TrimSpace(os.Getenv("APPDATA")); appData != "" {
			path := filepath.Join(appData, "Claude", "claude_desktop_config.json")
			targets = append(targets, desktopConfigTarget{
				Label:        "Claude Desktop config",
				Path:         path,
				Directory:    filepath.Dir(path),
				Snapshot:     filepath.Join(base, ".claude-desktop-config.snapshot.json"),
				SnapshotMeta: filepath.Join(base, ".claude-desktop-config.snapshot.meta.json"),
				Create:       true,
				Official:     true,
			})
		}
	case "darwin":
		if home := userHomeDir(); home != "" {
			path := filepath.Join(home, "Library", "Application Support", "Claude", "claude_desktop_config.json")
			targets = append(targets, desktopConfigTarget{
				Label:        "Claude Desktop config",
				Path:         path,
				Directory:    filepath.Dir(path),
				Snapshot:     filepath.Join(base, ".claude-desktop-config.snapshot.json"),
				SnapshotMeta: filepath.Join(base, ".claude-desktop-config.snapshot.meta.json"),
				Create:       true,
				Official:     true,
			})
		}
	case "linux":
		configHome := strings.TrimSpace(os.Getenv("XDG_CONFIG_HOME"))
		if configHome == "" {
			if home := userHomeDir(); home != "" {
				configHome = filepath.Join(home, ".config")
			}
		}
		if configHome != "" {
			path := filepath.Join(configHome, "Claude", "claude_desktop_config.json")
			if fileExists(path) {
				targets = append(targets, desktopConfigTarget{
					Label:        "Claude Desktop config",
					Path:         path,
					Directory:    filepath.Dir(path),
					Snapshot:     filepath.Join(base, ".claude-desktop-config.snapshot.json"),
					SnapshotMeta: filepath.Join(base, ".claude-desktop-config.snapshot.meta.json"),
					Create:       false,
					Official:     false,
				})
			}
		}
	}
	return dedupeDesktopTargets(targets)
}

func dedupeDesktopTargets(targets []desktopConfigTarget) []desktopConfigTarget {
	seen := map[string]bool{}
	out := make([]desktopConfigTarget, 0, len(targets))
	for _, target := range targets {
		if strings.TrimSpace(target.Path) == "" {
			continue
		}
		key := filepath.Clean(target.Path)
		if runtime.GOOS == "windows" {
			key = strings.ToLower(key)
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, target)
	}
	return out
}

func chromeExecutableCandidates() []string {
	var candidates []string
	if path := strings.TrimSpace(os.Getenv("ANTIGRAVITY_CHROME_PATH")); path != "" {
		candidates = append(candidates, path)
	}
	switch runtime.GOOS {
	case "windows":
		for _, root := range []string{os.Getenv("ProgramFiles"), os.Getenv("ProgramFiles(x86)"), os.Getenv("LOCALAPPDATA")} {
			if strings.TrimSpace(root) != "" {
				candidates = append(candidates, filepath.Join(root, "Google", "Chrome", "Application", "chrome.exe"))
			}
		}
	case "darwin":
		candidates = append(candidates,
			"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
			filepath.Join(userHomeDir(), "Applications", "Google Chrome.app", "Contents", "MacOS", "Google Chrome"),
			"/Applications/Chromium.app/Contents/MacOS/Chromium",
			filepath.Join(userHomeDir(), "Applications", "Chromium.app", "Contents", "MacOS", "Chromium"),
		)
	default:
		for _, name := range []string{"google-chrome", "google-chrome-stable", "chromium", "chromium-browser", "chrome"} {
			if path, err := exec.LookPath(name); err == nil && path != "" {
				candidates = append(candidates, path)
			}
		}
	}
	return uniqueNonEmptyStrings(candidates)
}

func findChromeExecutablePath() string {
	for _, candidate := range chromeExecutableCandidates() {
		if fileExists(candidate) {
			if abs, err := filepath.Abs(candidate); err == nil {
				return abs
			}
			return candidate
		}
	}
	return ""
}

func chromeProfileRoots() []string {
	var roots []string
	if path := strings.TrimSpace(os.Getenv("ANTIGRAVITY_CHROME_USER_DATA_DIR")); path != "" {
		roots = append(roots, path)
	}
	home := userHomeDir()
	switch runtime.GOOS {
	case "windows":
		if localAppData := strings.TrimSpace(os.Getenv("LOCALAPPDATA")); localAppData != "" {
			roots = append(roots, filepath.Join(localAppData, "Google", "Chrome", "User Data"))
		}
	case "darwin":
		if home != "" {
			roots = append(roots,
				filepath.Join(home, "Library", "Application Support", "Google", "Chrome"),
				filepath.Join(home, "Library", "Application Support", "Chromium"),
			)
		}
	default:
		configHome := strings.TrimSpace(os.Getenv("XDG_CONFIG_HOME"))
		if configHome == "" && home != "" {
			configHome = filepath.Join(home, ".config")
		}
		if configHome != "" {
			roots = append(roots,
				filepath.Join(configHome, "google-chrome"),
				filepath.Join(configHome, "google-chrome-stable"),
				filepath.Join(configHome, "chromium"),
				filepath.Join(configHome, "chromium-browser"),
			)
		}
	}
	return uniqueNonEmptyStrings(roots)
}

func chromeProfileDirs(root string) []string {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	var out []string
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		name := entry.Name()
		if name == "Default" || strings.HasPrefix(name, "Profile ") {
			out = append(out, filepath.Join(root, name))
		}
	}
	sort.Strings(out)
	return out
}

func findChromeExtensionPath(extensionID string) string {
	if path := strings.TrimSpace(os.Getenv("ANTIGRAVITY_EXTENSION_PATH")); path != "" && fileExists(filepath.Join(path, "manifest.json")) {
		if abs, err := filepath.Abs(path); err == nil {
			return abs
		}
		return path
	}
	bestPath := ""
	var bestMod int64
	for _, root := range chromeProfileRoots() {
		for _, profile := range chromeProfileDirs(root) {
			extensionRoot := filepath.Join(profile, "Extensions", extensionID)
			versions, err := os.ReadDir(extensionRoot)
			if err != nil {
				continue
			}
			for _, version := range versions {
				if !version.IsDir() {
					continue
				}
				path := filepath.Join(extensionRoot, version.Name())
				if !fileExists(filepath.Join(path, "manifest.json")) {
					continue
				}
				mod := int64(0)
				if info, err := version.Info(); err == nil {
					mod = info.ModTime().UnixNano()
				}
				if bestPath == "" || mod > bestMod {
					bestPath = path
					bestMod = mod
				}
			}
		}
	}
	return bestPath
}

func uniqueNonEmptyStrings(values []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		key := value
		if runtime.GOOS == "windows" {
			key = strings.ToLower(key)
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, value)
	}
	return out
}

func launcherCommands(cfg config) map[string]any {
	bin := preferredProxyBinaryPath()
	root := mustGetwd()
	baseURL := "http://127.0.0.1:" + cfg.Port + "/anthropic"
	return map[string]any{
		"binary":        bin,
		"start":         commandDisplay(bin, "start"),
		"stop":          commandDisplay(bin, "stop"),
		"restart":       commandDisplay(bin, "restart"),
		"launch_claude": commandDisplay(bin, "launch-claude"),
		"validate":      commandDisplay(bin, "validate"),
		"manual_env": map[string]string{
			"ANTHROPIC_BASE_URL":                         baseURL,
			"ANTHROPIC_AUTH_TOKEN":                       cfg.ProxyKey,
			"CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY": "1",
			"CLAUDE_CODE_DISABLE_EXPERIMENTAL_BETAS":     "1",
			"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC":   "1",
			"API_TIMEOUT_MS":                             "3000000",
		},
		"working_directory": root,
	}
}

func executableModeFor(goos string) os.FileMode {
	if goos == "windows" {
		return 0600
	}
	return 0700
}

func safeChildPath(parent, child string) bool {
	parentFull, err := filepath.Abs(parent)
	if err != nil {
		return false
	}
	childFull, err := filepath.Abs(child)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(parentFull, childFull)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator)))
}

func formatPlatformTarget(goos, goarch string) string {
	return fmt.Sprintf("%s/%s", goos, goarch)
}
