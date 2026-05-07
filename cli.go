package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

func runCLI(args []string) (bool, error) {
	if len(args) == 0 {
		return false, nil
	}
	cmd := strings.ToLower(strings.TrimSpace(args[0]))
	switch cmd {
	case "serve":
		return true, runServe()
	case "--antigravity-mcp", "browser-mcp":
		runAntigravityMCP()
		return true, nil
	case "start":
		return true, runStart()
	case "stop":
		return true, runStop()
	case "restart":
		return true, runRestart()
	case "launch-claude":
		return true, runLaunchClaude(args[1:])
	case "sync":
		if len(args) < 2 {
			return true, fmt.Errorf("usage: %s sync apply|restore", os.Args[0])
		}
		return true, runClaudeSettingsSyncGo(args[1])
	case "browser-start":
		return true, runBrowserStart(args[1:])
	case "browser-stop":
		return true, stopControlledAntigravityBrowser(true)
	case "browser-status":
		return true, printBrowserStatus()
	case "validate":
		return true, runValidate()
	case "build":
		return true, runBuild(args[1:])
	case "package-extension":
		return true, runPackageExtension()
	case "install-startup":
		return true, installStartup()
	case "uninstall-startup":
		return true, uninstallStartup()
	case "hook-worktree-create":
		return true, runHookWorktreeCreate()
	case "hook-worktree-remove":
		return true, runHookWorktreeRemove()
	case "hook-subagent-context":
		return true, runHookSubagentContext()
	case "help", "-h", "--help":
		printCLIHelp()
		return true, nil
	default:
		return true, fmt.Errorf("unknown command %q; run %s help", args[0], os.Args[0])
	}
}

func printCLIHelp() {
	fmt.Println(`claude-code-proxy commands:
  serve                         Run the HTTP proxy in the foreground
  start | stop | restart         Manage the background proxy process
  launch-claude [args...]        Start Claude Code through the local proxy
  sync apply|restore             Apply or restore Claude Code settings
  browser-mcp                    Run the Antigravity browser MCP stdio server
  browser-start [--dry-run]       Start the controlled browser profile
  browser-stop | browser-status   Manage or inspect browser bridge state
  validate                       Run local endpoint validation checks
  build [--all]                  Build current or all platform binaries
  package-extension              Build the local MCPB/DXT browser package
  install-startup                Install optional per-user autostart
  uninstall-startup              Remove optional per-user autostart`)
}

func runServe() error {
	cfg := loadConfig()
	if err := initProxyDB(cfg); err != nil {
		return fmt.Errorf("proxy database init failed: %w", err)
	}
	proxyEnabled.Store(true)
	mux := newProxyMux(cfg)
	server := &http.Server{Addr: "127.0.0.1:" + cfg.Port, Handler: loggingMiddleware(mux), ReadHeaderTimeout: 15 * time.Second}
	fmt.Printf("claude-code-proxy listening on http://%s\n", server.Addr)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func runStart() error {
	basePath := mustGetwd()
	envFile := filepath.Join(basePath, ".env")
	if !fileExists(envFile) {
		return fmt.Errorf("missing .env. Copy .env.example to .env and set PROXY_API_KEY")
	}
	loadDotEnvIntoProcess()
	cfg := loadConfig()
	if err := ensureCurrentBinaryBuilt(); err != nil {
		return err
	}
	if err := runClaudeSettingsSyncGo("apply"); err != nil {
		return err
	}
	if envFlag("ANTIGRAVITY_BROWSER_PRELAUNCH_WITH_PROXY", false) {
		if err := startAntigravityBrowser(false, false); err != nil {
			fmt.Printf("Antigravity browser prelaunch skipped: %v\n", err)
		}
	}
	pidPath := filepath.Join(basePath, ".proxy.pid")
	if pid, err := readPIDFile(pidPath); err == nil && processExists(pid) {
		fmt.Printf("Go proxy already running with PID %d.\n", pid)
		return nil
	}
	_ = os.Remove(pidPath)
	if healthOK(cfg.Port) {
		fmt.Printf("Go proxy already responding on http://127.0.0.1:%s.\n", cfg.Port)
		return nil
	}
	logFile, err := os.OpenFile(filepath.Join(basePath, ".proxy.log"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		return err
	}
	errFile, err := os.OpenFile(filepath.Join(basePath, ".proxy.err.log"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		_ = logFile.Close()
		return err
	}
	bin := currentProxyBinaryPath()
	cmd := exec.Command(bin, "serve")
	cmd.Dir = basePath
	cmd.Stdout = logFile
	cmd.Stderr = errFile
	configureBackgroundCommand(cmd)
	fmt.Printf("Starting Go proxy on http://127.0.0.1:%s\n", cfg.Port)
	if err := cmd.Start(); err != nil {
		_ = logFile.Close()
		_ = errFile.Close()
		return err
	}
	_ = os.WriteFile(pidPath, []byte(strconv.Itoa(cmd.Process.Pid)), 0600)
	time.Sleep(500 * time.Millisecond)
	if !processExists(cmd.Process.Pid) {
		_ = os.Remove(pidPath)
		return fmt.Errorf("Go proxy exited during startup. See %s", filepath.Join(basePath, ".proxy.err.log"))
	}
	fmt.Printf("Started Go proxy (PID %d).\n", cmd.Process.Pid)
	fmt.Printf("Use this URL in Claude Code: http://127.0.0.1:%s\n", cfg.Port)
	return nil
}

func runStop() error {
	basePath := mustGetwd()
	pidPath := filepath.Join(basePath, ".proxy.pid")
	if pid, err := readPIDFile(pidPath); err == nil {
		if processExists(pid) {
			if err := terminateProcess(pid); err != nil {
				return err
			}
			fmt.Printf("Stopped Go proxy (PID %d).\n", pid)
		} else {
			fmt.Printf("No running process found for PID %d.\n", pid)
		}
	} else {
		fmt.Println("No .proxy.pid file found.")
	}
	_ = os.Remove(pidPath)
	_ = stopControlledAntigravityBrowser(false)
	return runClaudeSettingsSyncGo("restore")
}

func runRestart() error {
	_ = runStop()
	return runStart()
}

func runLaunchClaude(args []string) error {
	loadDotEnvIntoProcess()
	if err := runStart(); err != nil {
		return err
	}
	cfg := loadConfig()
	env := os.Environ()
	env = setEnvValue(env, "ANTHROPIC_BASE_URL", "http://127.0.0.1:"+cfg.Port+"/anthropic")
	env = unsetEnvValue(env, "ANTHROPIC_API_KEY")
	env = setEnvValue(env, "ANTHROPIC_AUTH_TOKEN", cfg.ProxyKey)
	env = setEnvValue(env, "CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY", "1")
	env = setEnvValue(env, "CLAUDE_CODE_DISABLE_EXPERIMENTAL_BETAS", "1")
	env = setEnvValue(env, "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC", "1")
	env = setEnvValue(env, "API_TIMEOUT_MS", "3000000")
	for _, key := range []string{
		"ANTHROPIC_DEFAULT_OPUS_MODEL",
		"ANTHROPIC_DEFAULT_SONNET_MODEL",
		"ANTHROPIC_DEFAULT_HAIKU_MODEL",
		"ANTHROPIC_DEFAULT_OPUS_MODEL_SUPPORTED_CAPABILITIES",
		"ANTHROPIC_DEFAULT_SONNET_MODEL_SUPPORTED_CAPABILITIES",
		"ANTHROPIC_DEFAULT_HAIKU_MODEL_SUPPORTED_CAPABILITIES",
		"CLAUDE_CODE_EFFORT_LEVEL",
	} {
		if value := strings.TrimSpace(os.Getenv(key)); value != "" {
			env = setEnvValue(env, key, value)
		}
	}
	fmt.Printf("Starting Claude Code through local Codex proxy at http://127.0.0.1:%s/anthropic\n", cfg.Port)
	cmd := exec.Command("claude", args...)
	cmd.Env = env
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func setEnvValue(env []string, key, value string) []string {
	prefix := key + "="
	for i, item := range env {
		if strings.HasPrefix(item, prefix) {
			env[i] = prefix + value
			return env
		}
	}
	return append(env, prefix+value)
}

func unsetEnvValue(env []string, key string) []string {
	prefix := key + "="
	out := env[:0]
	for _, item := range env {
		if !strings.HasPrefix(item, prefix) {
			out = append(out, item)
		}
	}
	return out
}

func healthOK(port string) bool {
	client := http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get("http://127.0.0.1:" + port + "/health")
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode >= 200 && resp.StatusCode < 300
}

func ensureCurrentBinaryBuilt() error {
	binDir := proxyBinDir()
	if err := os.MkdirAll(binDir, 0700); err != nil {
		return err
	}
	out := currentProxyBinaryPath()
	if exe, err := os.Executable(); err == nil {
		exeAbs, _ := filepath.Abs(exe)
		outAbs, _ := filepath.Abs(out)
		if samePath(exeAbs, outAbs) && fileExists(out) {
			return nil
		}
	}
	fmt.Println("Building Go proxy...")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "build", "-o", out, ".")
	cmd.Dir = mustGetwd()
	raw, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("go build failed: %s", strings.TrimSpace(string(raw)))
	}
	_ = os.Chmod(out, executableModeFor(runtime.GOOS))
	return nil
}

func samePath(a, b string) bool {
	a = filepath.Clean(a)
	b = filepath.Clean(b)
	if runtime.GOOS == "windows" {
		return strings.EqualFold(a, b)
	}
	return a == b
}

func runBrowserStart(args []string) error {
	dryRun := false
	for _, arg := range args {
		if arg == "--dry-run" {
			dryRun = true
		}
	}
	return startAntigravityBrowser(true, dryRun)
}

func startAntigravityBrowser(strict bool, dryRun bool) error {
	loadDotEnvIntoProcess()
	chromePath := chromeExecutablePath()
	extensionPath := antigravityExtensionPath()
	if chromePath == "" {
		if strict {
			return fmt.Errorf("Google Chrome was not found. Set ANTIGRAVITY_CHROME_PATH if it is installed in a custom location")
		}
		return fmt.Errorf("Google Chrome was not found")
	}
	if extensionPath == "" && strict {
		return fmt.Errorf("Antigravity extension %s was not found. Install it once in Chrome, or set ANTIGRAVITY_EXTENSION_PATH", antigravityExtensionID)
	}
	args := chromeLaunchArgs()
	if dryRun {
		out := map[string]any{"chrome_path": chromePath, "args": args, "mode": plannedChromeMode(), "browser_url": antigravityCDPBaseURL(), "extension_path": extensionPath}
		raw, _ := json.MarshalIndent(out, "", "  ")
		fmt.Println(string(raw))
		return nil
	}
	if err := startChromeForCDP(); err != nil {
		return err
	}
	fmt.Printf("Started browser bridge at %s\n", antigravityCDPBaseURL())
	return nil
}

func printBrowserStatus() error {
	loadDotEnvIntoProcess()
	status := antigravityStatus()
	raw, err := json.MarshalIndent(status, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(raw))
	return nil
}

func stopControlledAntigravityBrowser(verbose bool) error {
	pidPath := filepath.Join(mustGetwd(), ".antigravity-browser.pid")
	pid, err := readPIDFile(pidPath)
	if err != nil {
		if verbose {
			fmt.Println("No Antigravity browser PID file found.")
		}
		return nil
	}
	profilePath := antigravityProfilePath()
	commandLine := processCommandLine(pid)
	if commandLine != "" && !strings.Contains(commandLine, profilePath) {
		return fmt.Errorf("refusing to stop PID %d because it does not use the Antigravity profile", pid)
	}
	if processExists(pid) {
		if err := terminateProcess(pid); err != nil {
			return err
		}
		if verbose {
			fmt.Printf("Stopped Antigravity browser (PID %d).\n", pid)
		}
	}
	_ = os.Remove(pidPath)
	return nil
}

func runValidate() error {
	loadDotEnvIntoProcess()
	cfg := loadConfig()
	baseURL := "http://127.0.0.1:" + cfg.Port
	headers := map[string]string{
		"Content-Type":      "application/json",
		"anthropic-version": "2023-06-01",
		"x-api-key":         cfg.ProxyKey,
	}
	if err := validateRequest(http.MethodGet, baseURL+"/anthropic/v1/models", headers, nil, "GET /anthropic/v1/models"); err != nil {
		return err
	}
	tokenBody := []byte(`{"model":"claude-3-7-sonnet-latest","messages":[{"role":"user","content":"Quick count test"}]}`)
	if err := validateRequest(http.MethodPost, baseURL+"/anthropic/v1/messages/count_tokens", headers, tokenBody, "POST /anthropic/v1/messages/count_tokens"); err != nil {
		return err
	}
	if cfg.Upstream != "codex" && (cfg.OpenAIAPIKey == "" || strings.HasPrefix(cfg.OpenAIAPIKey, "sk-or-") || cfg.OpenAIAPIKey == "YOUR_OPENAI_API_KEY") {
		fmt.Println("Skipping POST /anthropic/v1/messages because OPENAI_API_KEY is a placeholder and UPSTREAM is not codex.")
		return nil
	}
	messageBody := []byte(`{"model":"claude-3-7-sonnet-latest","max_tokens":32,"messages":[{"role":"user","content":"Say hello in one sentence."}]}`)
	if err := validateRequest(http.MethodPost, baseURL+"/anthropic/v1/messages", headers, messageBody, "POST /anthropic/v1/messages"); err != nil {
		return err
	}
	streamBody := []byte(`{"model":"claude-3-7-sonnet-latest","max_tokens":32,"stream":true,"messages":[{"role":"user","content":"Say streaming hello in two words."}]}`)
	raw, err := validateRequestRaw(http.MethodPost, baseURL+"/anthropic/v1/messages", headers, streamBody)
	if err != nil {
		return err
	}
	text := string(raw)
	if !strings.Contains(text, "message_start") || !strings.Contains(text, "content_block_delta") || !strings.Contains(text, "message_stop") {
		return fmt.Errorf("POST /anthropic/v1/messages streaming response did not include expected Anthropic SSE events")
	}
	fmt.Println("POST /anthropic/v1/messages stream: OK")
	return nil
}

func validateRequest(method, url string, headers map[string]string, body []byte, label string) error {
	raw, err := validateRequestRaw(method, url, headers, body)
	if err != nil {
		return err
	}
	fmt.Printf("%s: OK\n", label)
	if len(raw) > 0 {
		fmt.Println(string(raw))
	}
	return nil
}

func validateRequestRaw(method, url string, headers map[string]string, body []byte) ([]byte, error) {
	client := http.Client{Timeout: 30 * time.Second}
	req, err := http.NewRequest(method, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	for k, v := range headers {
		if strings.TrimSpace(v) != "" {
			req.Header.Set(k, v)
		}
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4*1024*1024))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return raw, fmt.Errorf("%s returned %d: %s", url, resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	return raw, nil
}

func runHookWorktreeCreate() error {
	raw, err := io.ReadAll(os.Stdin)
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(raw)) == "" {
		return fmt.Errorf("WorktreeCreate hook received no JSON input")
	}
	var input struct {
		Name string `json:"name"`
		CWD  string `json:"cwd"`
	}
	if err := json.Unmarshal(raw, &input); err != nil {
		return err
	}
	name := safeHookName(input.Name)
	cwd := input.CWD
	if strings.TrimSpace(cwd) == "" || !fileExists(cwd) {
		cwd = mustGetwd()
	}
	root := filepath.Join(mustGetwd(), ".claude-worktrees")
	if err := os.MkdirAll(root, 0700); err != nil {
		return err
	}
	suffix := strings.ToLower(randomSuffix())
	target := filepath.Join(root, name+"-"+suffix)
	metadata := map[string]any{"created_at": time.Now().Format(time.RFC3339), "source_cwd": cwd, "name": name}
	if executablePath("git") != "" && insideGitWorkTree(cwd) {
		repoRoot := strings.TrimSpace(commandOutput(5*time.Second, "git", "-C", cwd, "rev-parse", "--show-toplevel"))
		if repoRoot != "" {
			branch := "claude-agent/" + name + "-" + suffix
			if out, err := gitCombinedOutput(repoRoot, "worktree", "add", "-b", branch, target, "HEAD"); err != nil {
				_, _ = os.Stderr.WriteString(string(out))
				if out, err = gitCombinedOutput(repoRoot, "worktree", "add", "--detach", target, "HEAD"); err != nil {
					_, _ = os.Stderr.WriteString(string(out))
					return fmt.Errorf("git worktree add failed for %s", repoRoot)
				}
				branch = ""
			}
			metadata["kind"] = "git"
			metadata["repo_root"] = repoRoot
			metadata["branch"] = branch
			_ = writeJSONFile(filepath.Join(target, ".claude-proxy-worktree.json"), metadata)
			fmt.Println(target)
			return nil
		}
	}
	if err := os.MkdirAll(target, 0700); err != nil {
		return err
	}
	metadata["kind"] = "plain"
	_ = writeJSONFile(filepath.Join(target, ".claude-proxy-worktree.json"), metadata)
	_ = os.WriteFile(filepath.Join(target, "README.txt"), []byte("Temporary Claude Code isolated workspace created by claude-code-proxy.\nOriginal working directory: "+cwd+"\nThis folder is safe to remove after the subagent finishes.\n"), 0600)
	fmt.Println(target)
	return nil
}

func runHookWorktreeRemove() error {
	raw, err := io.ReadAll(os.Stdin)
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(raw)) == "" {
		return nil
	}
	var input struct {
		WorktreePath string `json:"worktree_path"`
	}
	if err := json.Unmarshal(raw, &input); err != nil {
		return err
	}
	target := strings.TrimSpace(input.WorktreePath)
	if target == "" {
		return nil
	}
	root := filepath.Join(mustGetwd(), ".claude-worktrees")
	if !safeChildPath(root, target) {
		return fmt.Errorf("refusing to remove worktree outside proxy root: %s", target)
	}
	if !fileExists(target) {
		return nil
	}
	metadata, _ := readJSONMap(filepath.Join(target, ".claude-proxy-worktree.json"))
	if metadata["kind"] == "git" && executablePath("git") != "" {
		repoRoot, _ := metadata["repo_root"].(string)
		branch, _ := metadata["branch"].(string)
		if repoRoot != "" {
			if out, err := gitCombinedOutput(repoRoot, "worktree", "remove", "--force", target); err == nil {
				if branch != "" {
					_, _ = gitCombinedOutput(repoRoot, "branch", "-D", branch)
				}
				return nil
			} else {
				_, _ = os.Stderr.WriteString(string(out))
			}
		}
	}
	return os.RemoveAll(target)
}

func runHookSubagentContext() error {
	raw, err := io.ReadAll(os.Stdin)
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(raw)) == "" {
		return nil
	}
	var input struct {
		AgentID   string `json:"agent_id"`
		AgentType string `json:"agent_type"`
	}
	_ = json.Unmarshal(raw, &input)
	agentID := safeHookID(firstNonEmpty(input.AgentID, "unknown-"+randomSuffix()))
	agentType := safeHookID(firstNonEmpty(input.AgentType, "unknown"))
	context := fmt.Sprintf("Internal proxy routing note: codex_proxy_subagent_id=%s; codex_proxy_subagent_type=%s. This note is only for local Codex session isolation and token accounting. Do not mention it to the user.", agentID, agentType)
	out := map[string]any{"hookSpecificOutput": map[string]any{"hookEventName": "SubagentStart", "additionalContext": context}}
	rawOut, _ := json.MarshalIndent(out, "", "  ")
	fmt.Println(string(rawOut))
	return nil
}

func safeHookName(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		name = "agent"
	}
	var b strings.Builder
	for _, r := range name {
		if (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '.' || r == '_' || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "agent"
	}
	return out
}

func safeHookID(value string) string {
	value = strings.TrimSpace(value)
	var b strings.Builder
	for _, r := range value {
		if (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '.' || r == '_' || r == '-' || r == ':' {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	if b.Len() == 0 {
		return "unknown"
	}
	return b.String()
}

func randomSuffix() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return strconv.FormatInt(time.Now().UnixNano()%100000000, 16)
	}
	const hex = "0123456789abcdef"
	out := make([]byte, 8)
	for i := range out {
		out[i] = hex[int(b[i])%len(hex)]
	}
	return string(out)
}

func insideGitWorkTree(path string) bool {
	out := strings.TrimSpace(commandOutput(5*time.Second, "git", "-C", path, "rev-parse", "--is-inside-work-tree"))
	return out == "true"
}

func gitCombinedOutput(repoRoot string, args ...string) ([]byte, error) {
	full := append([]string{"-C", repoRoot}, args...)
	cmd := exec.Command("git", full...)
	return cmd.CombinedOutput()
}
