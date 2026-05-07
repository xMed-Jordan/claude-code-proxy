package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	claudeSettingsSnapshotPath     = ".claude-settings.snapshot.json"
	claudeSettingsSnapshotMetaPath = ".claude-settings.snapshot.meta.json"
	claudeJSONSnapshotPath         = ".claude-json.snapshot.json"
	claudeJSONSnapshotMetaPath     = ".claude-json.snapshot.meta.json"
	claudeMemorySnapshotPath       = ".claude-memory.snapshot.md"
	claudeMemorySnapshotMetaPath   = ".claude-memory.snapshot.meta.json"
	browserMemoryStart             = "<!-- claude-code-proxy-browser-routing:start -->"
	browserMemoryEnd               = "<!-- claude-code-proxy-browser-routing:end -->"
)

func runClaudeSettingsSyncGo(action string) error {
	switch strings.ToLower(strings.TrimSpace(action)) {
	case "apply":
		return applyProxySettingsGo()
	case "restore":
		return restoreProxySettingsGo()
	default:
		return fmt.Errorf("unknown sync action %q", action)
	}
}

func applyProxySettingsGo() error {
	settingsPath := defaultClaudeSettingsPath()
	settingsDir := filepath.Dir(settingsPath)
	if err := ensureSnapshot(settingsPath, filepath.Join(mustGetwd(), claudeSettingsSnapshotPath), filepath.Join(mustGetwd(), claudeSettingsSnapshotMetaPath), "settings_path", "{}"); err != nil {
		return err
	}
	if err := clearGatewayModelsCacheGo(); err != nil {
		return err
	}
	proxyEnv := readEnvMap()
	port := strings.TrimSpace(proxyEnv["PROXY_PORT"])
	if port == "" {
		port = "4000"
	}
	proxyKey := strings.TrimSpace(firstNonEmpty(proxyEnv["PROXY_API_KEY"], proxyEnv["LITELLM_MASTER_KEY"]))
	if proxyKey == "" {
		return fmt.Errorf("PROXY_API_KEY is not set in .env")
	}
	settings, err := readJSONMap(settingsPath)
	if err != nil {
		return err
	}
	if _, ok := settings["$schema"]; !ok {
		settings["$schema"] = "https://json.schemastore.org/claude-code-settings.json"
	}
	env := objectMap(settings, "env")
	env["ANTHROPIC_BASE_URL"] = "http://127.0.0.1:" + port + "/anthropic"
	delete(env, "ANTHROPIC_API_KEY")
	env["ANTHROPIC_AUTH_TOKEN"] = proxyKey
	env["CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY"] = "1"
	env["CLAUDE_CODE_DISABLE_EXPERIMENTAL_BETAS"] = "1"
	env["CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC"] = firstNonEmpty(proxyEnv["CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC"], "1")
	env["API_TIMEOUT_MS"] = firstNonEmpty(proxyEnv["API_TIMEOUT_MS"], "3000000")
	env["ANTHROPIC_DEFAULT_OPUS_MODEL"] = firstNonEmpty(proxyEnv["ANTHROPIC_DEFAULT_OPUS_MODEL"], "claude-opus-4-7[1m]")
	env["ANTHROPIC_DEFAULT_SONNET_MODEL"] = firstNonEmpty(proxyEnv["ANTHROPIC_DEFAULT_SONNET_MODEL"], "claude-sonnet-4-6[1m]")
	env["ANTHROPIC_DEFAULT_HAIKU_MODEL"] = firstNonEmpty(proxyEnv["ANTHROPIC_DEFAULT_HAIKU_MODEL"], "claude-haiku-4-5")
	env["ANTHROPIC_DEFAULT_OPUS_MODEL_SUPPORTED_CAPABILITIES"] = firstNonEmpty(proxyEnv["ANTHROPIC_DEFAULT_OPUS_MODEL_SUPPORTED_CAPABILITIES"], "effort,xhigh_effort,max_effort,thinking,adaptive_thinking,interleaved_thinking")
	env["ANTHROPIC_DEFAULT_SONNET_MODEL_SUPPORTED_CAPABILITIES"] = firstNonEmpty(proxyEnv["ANTHROPIC_DEFAULT_SONNET_MODEL_SUPPORTED_CAPABILITIES"], "effort,max_effort,thinking,adaptive_thinking,interleaved_thinking")
	env["ANTHROPIC_DEFAULT_HAIKU_MODEL_SUPPORTED_CAPABILITIES"] = firstNonEmpty(proxyEnv["ANTHROPIC_DEFAULT_HAIKU_MODEL_SUPPORTED_CAPABILITIES"], "thinking")
	env["CLAUDE_CODE_EFFORT_LEVEL"] = firstNonEmpty(proxyEnv["CLAUDE_CODE_EFFORT_LEVEL"], "xhigh")
	settings["model"] = "opus[1m]"
	ensurePermissionAllow(settings, []string{
		"mcp__antigravity-browser__browser_status",
		"mcp__antigravity-browser__browser_pages",
		"mcp__antigravity-browser__browser_navigate",
		"mcp__antigravity-browser__browser_snapshot",
		"mcp__antigravity-browser__browser_screenshot",
		"mcp__antigravity-browser__browser_console",
		"mcp__antigravity-browser__browser_move",
		"mcp__antigravity-browser__browser_click",
		"mcp__antigravity-browser__browser_type",
		"mcp__antigravity-browser__browser_press_key",
		"mcp__antigravity-browser__browser_wait",
	})
	objectMap(settings, "mcpServers")["antigravity-browser"] = mcpServerConfig(false)
	ensureClaudeIsolationHooks(settings)
	if err := ensureClaudeBrowserMemory(); err != nil {
		return err
	}
	if err := os.MkdirAll(settingsDir, 0700); err != nil {
		return err
	}
	if err := writeJSONFile(settingsPath, settings); err != nil {
		return err
	}
	if err := ensureAntigravityBrowserUserMCP(); err != nil {
		return err
	}
	if err := ensureAntigravityBrowserDesktopMCP(); err != nil {
		return err
	}
	fmt.Printf("Applied proxy env to Claude Code settings: %s\n", settingsPath)
	return nil
}

func restoreProxySettingsGo() error {
	var errs []string
	if err := restoreSnapshot(defaultClaudeSettingsPath(), filepath.Join(mustGetwd(), claudeSettingsSnapshotPath), filepath.Join(mustGetwd(), claudeSettingsSnapshotMetaPath), "settings_path"); err != nil {
		errs = append(errs, err.Error())
	}
	if err := restoreSnapshot(defaultClaudeRootConfigPath(), filepath.Join(mustGetwd(), claudeJSONSnapshotPath), filepath.Join(mustGetwd(), claudeJSONSnapshotMetaPath), "config_path"); err != nil {
		errs = append(errs, err.Error())
	}
	if err := restoreClaudeDesktopConfigs(); err != nil {
		errs = append(errs, err.Error())
	}
	if err := restoreSnapshot(defaultClaudeMemoryPath(), filepath.Join(mustGetwd(), claudeMemorySnapshotPath), filepath.Join(mustGetwd(), claudeMemorySnapshotMetaPath), "memory_path"); err != nil {
		errs = append(errs, err.Error())
	}
	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "; "))
	}
	return nil
}

func clearGatewayModelsCacheGo() error {
	path := defaultClaudeGatewayModelsCachePath()
	if fileExists(path) {
		if err := os.Remove(path); err != nil {
			return err
		}
		fmt.Printf("Removed stale Claude Code gateway models cache: %s\n", path)
	}
	return nil
}

func ensureSnapshot(source, snapshot, snapshotMeta, pathKey, emptyContent string) error {
	if fileExists(snapshot) {
		if !fileExists(snapshotMeta) {
			meta := map[string]any{
				pathKey:    source,
				"existed":  true,
				"saved_at": nil,
				"reused":   true,
				"note":     "Existing snapshot preserved; not overwritten during proxy start.",
			}
			if err := writeJSONFile(snapshotMeta, meta); err != nil {
				return err
			}
		}
		fmt.Printf("Existing snapshot preserved: %s\n", snapshot)
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(source), 0700); err != nil {
		return err
	}
	existed := fileExists(source)
	if existed {
		raw, err := os.ReadFile(source)
		if err != nil {
			return err
		}
		if err := os.WriteFile(snapshot, raw, 0600); err != nil {
			return err
		}
	} else if err := os.WriteFile(snapshot, []byte(emptyContent), 0600); err != nil {
		return err
	}
	meta := map[string]any{
		pathKey:    source,
		"existed":  existed,
		"saved_at": time.Now().Format(time.RFC3339),
		"reused":   false,
	}
	if err := writeJSONFile(snapshotMeta, meta); err != nil {
		return err
	}
	fmt.Printf("Created snapshot: %s\n", snapshot)
	return nil
}

func restoreSnapshot(defaultTarget, snapshot, snapshotMeta, pathKey string) error {
	if !fileExists(snapshot) {
		return nil
	}
	target := defaultTarget
	existed := true
	if meta, err := readJSONMap(snapshotMeta); err == nil {
		if raw, ok := meta[pathKey].(string); ok && strings.TrimSpace(raw) != "" {
			target = raw
		}
		if raw, ok := meta["existed"].(bool); ok {
			existed = raw
		}
	}
	if existed {
		raw, err := os.ReadFile(snapshot)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
			return err
		}
		if err := os.WriteFile(target, raw, 0600); err != nil {
			return err
		}
		fmt.Printf("Restored from snapshot: %s\n", target)
	} else {
		_ = os.Remove(target)
		fmt.Printf("Removed file created for proxy: %s\n", target)
	}
	_ = os.Remove(snapshot)
	_ = os.Remove(snapshotMeta)
	return nil
}

func readJSONMap(path string) (map[string]any, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]any{}, nil
		}
		return nil, err
	}
	if strings.TrimSpace(string(raw)) == "" {
		return map[string]any{}, nil
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("invalid JSON in %s: %w", path, err)
	}
	if out == nil {
		out = map[string]any{}
	}
	return out, nil
}

func writeJSONFile(path string, value any) error {
	raw, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil && filepath.Dir(path) != "." {
		return err
	}
	return os.WriteFile(path, raw, 0600)
}

func objectMap(parent map[string]any, key string) map[string]any {
	if existing, ok := parent[key].(map[string]any); ok && existing != nil {
		return existing
	}
	if existing, ok := parent[key].(map[string]string); ok && existing != nil {
		out := make(map[string]any, len(existing))
		for k, v := range existing {
			out[k] = v
		}
		parent[key] = out
		return out
	}
	out := map[string]any{}
	parent[key] = out
	return out
}

func ensurePermissionAllow(settings map[string]any, tools []string) {
	permissions := objectMap(settings, "permissions")
	current := stringSliceFromAny(permissions["allow"])
	seen := map[string]bool{}
	for _, value := range current {
		seen[value] = true
	}
	for _, tool := range tools {
		if !seen[tool] {
			current = append(current, tool)
			seen[tool] = true
		}
	}
	permissions["allow"] = current
}

func stringSliceFromAny(value any) []string {
	var out []string
	switch raw := value.(type) {
	case []string:
		out = append(out, raw...)
	case []any:
		for _, item := range raw {
			if s, ok := item.(string); ok {
				out = append(out, s)
			}
		}
	}
	return out
}

func ensureClaudeIsolationHooks(settings map[string]any) {
	ensureHookEvent(settings, "WorktreeCreate", "hook-worktree-create", "", true)
	ensureHookEvent(settings, "WorktreeRemove", "hook-worktree-remove", "", true)
	ensureHookEvent(settings, "SubagentStart", "hook-subagent-context", "*", false)
}

func ensureHookEvent(settings map[string]any, eventName, subcommand, matcher string, onlyWhenEmpty bool) {
	hooks := objectMap(settings, "hooks")
	current := hookGroupsFromAny(hooks[eventName])
	command := hookCommandString(subcommand)
	for _, group := range current {
		for _, hook := range hookListFromAny(group["hooks"]) {
			if strings.Contains(fmt.Sprint(hook["command"]), subcommand) {
				return
			}
		}
	}
	if onlyWhenEmpty && len(current) > 0 {
		return
	}
	group := map[string]any{
		"hooks": []map[string]any{{"type": "command", "command": command}},
	}
	if strings.TrimSpace(matcher) != "" {
		group["matcher"] = matcher
	}
	hooks[eventName] = append(current, group)
}

func hookGroupsFromAny(value any) []map[string]any {
	var out []map[string]any
	switch raw := value.(type) {
	case []map[string]any:
		return raw
	case []any:
		for _, item := range raw {
			if group, ok := item.(map[string]any); ok {
				out = append(out, group)
			}
		}
	case map[string]any:
		out = append(out, raw)
	}
	return out
}

func hookListFromAny(value any) []map[string]any {
	var out []map[string]any
	switch raw := value.(type) {
	case []map[string]any:
		return raw
	case []any:
		for _, item := range raw {
			if hook, ok := item.(map[string]any); ok {
				out = append(out, hook)
			}
		}
	case map[string]any:
		out = append(out, raw)
	}
	return out
}

func ensureClaudeBrowserMemory() error {
	memoryPath := defaultClaudeMemoryPath()
	if err := ensureSnapshot(memoryPath, filepath.Join(mustGetwd(), claudeMemorySnapshotPath), filepath.Join(mustGetwd(), claudeMemorySnapshotMetaPath), "memory_path", ""); err != nil {
		return err
	}
	body := strings.TrimSpace(browserMemoryStart + `
## Claude Code Proxy Browser Routing

When a user asks to open a website, browse, search in a browser, inspect a page, check browser console errors, click, type into a web page, or take a screenshot, use the antigravity-browser MCP tools first.

Do not say browser MCP tools are unavailable until you have checked /mcp or tried antigravity-browser browser_status.

Do not use shell commands, Node, Python, browser CLI commands, headless Chrome, Playwright, raw Chrome DevTools scripts, or generic command-line discovery for browser tasks before trying antigravity-browser.

Use these tools in this order for browser work:
- antigravity-browser browser_status only to check availability; it is passive and does not open Chrome.
- antigravity-browser browser_pages to list available pages.
- antigravity-browser browser_navigate, browser_snapshot, browser_console, and browser_wait for page state.
- antigravity-browser browser_move, browser_click, browser_type, and browser_press_key for visible browser actions.
- antigravity-browser browser_screenshot for screenshots. It returns a local PNG path by default; report that path to the user. Unless ANTIGRAVITY_SCREENSHOT_DIR is set, screenshots save under a "Claude Code Screenshots" folder in the directory where Claude Code was launched.

Use shell commands for browser tasks only when antigravity-browser is unavailable or explicitly fails after being tried, and explain that fallback clearly.
` + browserMemoryEnd)
	current := ""
	if raw, err := os.ReadFile(memoryPath); err == nil {
		current = string(raw)
	}
	updated := replaceManagedBlock(current, browserMemoryStart, browserMemoryEnd, body)
	if err := os.MkdirAll(filepath.Dir(memoryPath), 0700); err != nil {
		return err
	}
	if err := os.WriteFile(memoryPath, []byte(updated), 0600); err != nil {
		return err
	}
	fmt.Printf("Applied browser routing memory to Claude Code: %s\n", memoryPath)
	return nil
}

func replaceManagedBlock(current, start, end, body string) string {
	startIdx := strings.Index(current, start)
	endIdx := strings.Index(current, end)
	if startIdx >= 0 && endIdx >= startIdx {
		endIdx += len(end)
		return current[:startIdx] + body + current[endIdx:]
	}
	separator := ""
	if strings.TrimSpace(current) != "" && !strings.HasSuffix(current, "\n") {
		separator = "\n"
	}
	return current + separator + "\n" + body + "\n"
}

func ensureAntigravityBrowserUserMCP() error {
	path := defaultClaudeRootConfigPath()
	if err := ensureSnapshot(path, filepath.Join(mustGetwd(), claudeJSONSnapshotPath), filepath.Join(mustGetwd(), claudeJSONSnapshotMetaPath), "config_path", "{}"); err != nil {
		return err
	}
	config, err := readJSONMap(path)
	if err != nil {
		return err
	}
	objectMap(config, "mcpServers")["antigravity-browser"] = mcpServerConfig(true)
	if err := writeJSONFile(path, config); err != nil {
		return err
	}
	fmt.Printf("Applied Antigravity MCP to Claude Code root config: %s\n", path)
	return nil
}

func ensureAntigravityBrowserDesktopMCP() error {
	targets := claudeDesktopConfigTargets()
	if len(targets) == 0 {
		return nil
	}
	merged := map[string]any{}
	for _, target := range targets {
		if !fileExists(target.Path) {
			continue
		}
		config, err := readJSONMap(target.Path)
		if err != nil {
			continue
		}
		if servers, ok := config["mcpServers"].(map[string]any); ok {
			for name, value := range servers {
				if _, exists := merged[name]; !exists {
					merged[name] = value
				}
			}
		}
	}
	merged["antigravity-browser"] = mcpServerConfig(false)
	for _, target := range targets {
		if !target.Create && !fileExists(target.Path) {
			continue
		}
		if err := ensureSnapshot(target.Path, target.Snapshot, target.SnapshotMeta, "config_path", "{}"); err != nil {
			return err
		}
		config, err := readJSONMap(target.Path)
		if err != nil {
			return err
		}
		config["mcpServers"] = merged
		if err := writeJSONFile(target.Path, config); err != nil {
			return err
		}
		fmt.Printf("Applied Antigravity MCP to %s: %s\n", target.Label, target.Path)
	}
	return nil
}

func restoreClaudeDesktopConfigs() error {
	var errs []string
	for _, target := range claudeDesktopConfigTargets() {
		if err := restoreSnapshot(target.Path, target.Snapshot, target.SnapshotMeta, "config_path"); err != nil {
			errs = append(errs, err.Error())
		}
	}
	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "; "))
	}
	return nil
}
