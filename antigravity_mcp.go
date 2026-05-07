package main

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

const (
	antigravityMCPName       = "antigravity-browser"
	antigravityMCPVersion    = "0.2.0"
	antigravityStateFile     = ".antigravity-browser-state.json"
	antigravityOverlayID     = "ccp-visible-cursor"
	antigravityScreenshotDir = ".antigravity-screenshots"
	defaultBrowserWaitLimit  = 15 * time.Second
)

type mcpEnvelope struct {
	JSONRPC string          `json:"jsonrpc,omitempty"`
	ID      any             `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type mcpTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

type mcpToolCallParams struct {
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments"`
}

type mcpToolResult struct {
	Content []map[string]any `json:"content"`
	IsError bool             `json:"isError,omitempty"`
}

type cdpTarget struct {
	ID                   string `json:"id"`
	Type                 string `json:"type"`
	Title                string `json:"title"`
	URL                  string `json:"url"`
	WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
}

type cdpClient struct {
	conn    *websocket.Conn
	mu      sync.Mutex
	nextID  int
	target  cdpTarget
	baseURL string
}

type antigravityBrowserState struct {
	UpdatedAt      string `json:"updated_at"`
	PID            int    `json:"pid"`
	Mode           string `json:"mode"`
	BrowserURL     string `json:"browser_url"`
	Connected      bool   `json:"connected"`
	CurrentURL     string `json:"current_url"`
	CurrentTitle   string `json:"current_title"`
	LastTool       string `json:"last_tool"`
	LastAction     string `json:"last_action"`
	LastError      string `json:"last_error"`
	LastScreenshot string `json:"last_screenshot,omitempty"`
}

type chromeProcessInfo struct {
	ID        int    `json:"id"`
	Title     string `json:"title"`
	IsVisible bool   `json:"is_visible"`
}

func loadDotEnvIntoProcess() {
	raw, err := os.ReadFile(".env")
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		idx := strings.Index(line, "=")
		if idx <= 0 {
			continue
		}
		key := strings.TrimSpace(line[:idx])
		value := strings.TrimSpace(line[idx+1:])
		if key == "" || os.Getenv(key) != "" {
			continue
		}
		_ = os.Setenv(key, value)
	}
}

func runAntigravityMCP() {
	loadDotEnvIntoProcess()
	writeAntigravityBrowserState(antigravityBrowserState{
		UpdatedAt:  time.Now().Format(time.RFC3339),
		PID:        os.Getpid(),
		Mode:       antigravityBrowserMode(),
		BrowserURL: antigravityCDPBaseURL(),
		Connected:  false,
		LastAction: "MCP server started",
	})

	reader := bufio.NewScanner(os.Stdin)
	reader.Buffer(make([]byte, 0, 64*1024), 32*1024*1024)
	writer := bufio.NewWriter(os.Stdout)
	defer writer.Flush()

	for reader.Scan() {
		line := strings.TrimSpace(reader.Text())
		if line == "" {
			continue
		}
		var msg mcpEnvelope
		if err := json.Unmarshal([]byte(line), &msg); err != nil {
			writeMCPError(writer, nil, -32700, "Parse error")
			continue
		}
		handleMCPMessage(writer, msg)
	}
}

func handleMCPMessage(writer *bufio.Writer, msg mcpEnvelope) {
	switch msg.Method {
	case "initialize":
		writeMCPResult(writer, msg.ID, map[string]any{
			"protocolVersion": "2024-11-05",
			"capabilities": map[string]any{
				"tools": map[string]any{},
			},
			"serverInfo": map[string]any{
				"name":    antigravityMCPName,
				"version": antigravityMCPVersion,
			},
		})
	case "notifications/initialized", "notifications/cancelled":
		return
	case "ping":
		writeMCPResult(writer, msg.ID, map[string]any{})
	case "tools/list":
		writeMCPResult(writer, msg.ID, map[string]any{"tools": antigravityMCPTools()})
	case "tools/call":
		var params mcpToolCallParams
		if err := json.Unmarshal(msg.Params, &params); err != nil {
			writeMCPError(writer, msg.ID, -32602, "Invalid tools/call params")
			return
		}
		result := callAntigravityTool(context.Background(), params.Name, params.Arguments)
		writeMCPResult(writer, msg.ID, result)
	default:
		if msg.ID != nil {
			writeMCPError(writer, msg.ID, -32601, "Method not found")
		}
	}
}

func writeMCPResult(writer *bufio.Writer, id any, result any) {
	if id == nil {
		return
	}
	writeMCPMessage(writer, map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"result":  result,
	})
}

func writeMCPError(writer *bufio.Writer, id any, code int, message string) {
	if id == nil {
		return
	}
	writeMCPMessage(writer, map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"error": map[string]any{
			"code":    code,
			"message": message,
		},
	})
}

func writeMCPMessage(writer *bufio.Writer, value any) {
	raw, _ := json.Marshal(value)
	_, _ = writer.Write(raw)
	_ = writer.WriteByte('\n')
	_ = writer.Flush()
}

func antigravityMCPTools() []mcpTool {
	return []mcpTool{
		{
			Name:        "browser_status",
			Description: "Check Chrome, Antigravity extension, and browser-control connection status.",
			InputSchema: objectSchema(nil, nil),
		},
		{
			Name:        "browser_pages",
			Description: "List open Chrome pages available to the Antigravity browser bridge.",
			InputSchema: objectSchema(nil, nil),
		},
		{
			Name:        "browser_navigate",
			Description: "Open or navigate the active Chrome page to a URL.",
			InputSchema: objectSchema(map[string]any{
				"url": map[string]any{"type": "string", "description": "URL to open. https:// is added when no scheme is provided."},
			}, []string{"url"}),
		},
		{
			Name:        "browser_snapshot",
			Description: "Read the visible page text and a short list of interactive elements.",
			InputSchema: objectSchema(map[string]any{
				"max_text_chars": map[string]any{"type": "number", "description": "Maximum visible text characters to return."},
			}, nil),
		},
		{
			Name:        "browser_screenshot",
			Description: "Take a screenshot of the active Chrome page and save it to a local PNG file. Inline image data is omitted by default to keep Claude Code responsive.",
			InputSchema: objectSchema(map[string]any{
				"include_image": map[string]any{"type": "boolean", "description": "Also return inline PNG image data. Defaults to false."},
			}, nil),
		},
		{
			Name:        "browser_move",
			Description: "Move the visible overlay cursor to a selector, text, or coordinate without clicking.",
			InputSchema: targetSchema(),
		},
		{
			Name:        "browser_click",
			Description: "Move the visible overlay cursor and click a selector, text, or coordinate.",
			InputSchema: targetSchema(),
		},
		{
			Name:        "browser_type",
			Description: "Click a target, optionally clear it, and type text into the focused field.",
			InputSchema: objectSchema(map[string]any{
				"text":     map[string]any{"type": "string", "description": "Text to type."},
				"selector": map[string]any{"type": "string", "description": "CSS selector to focus before typing."},
				"target_text": map[string]any{
					"type":        "string",
					"description": "Visible text or placeholder to focus before typing when no selector is provided.",
				},
				"x":     map[string]any{"type": "number", "description": "Viewport X coordinate to focus before typing."},
				"y":     map[string]any{"type": "number", "description": "Viewport Y coordinate to focus before typing."},
				"clear": map[string]any{"type": "boolean", "description": "Clear the current field before typing."},
			}, []string{"text"}),
		},
		{
			Name:        "browser_press_key",
			Description: "Press a keyboard key in the active page, such as Enter, Tab, Escape, or Backspace.",
			InputSchema: objectSchema(map[string]any{
				"key": map[string]any{"type": "string", "description": "Key name to press."},
			}, []string{"key"}),
		},
		{
			Name:        "browser_wait",
			Description: "Wait until a selector or visible text appears on the active page.",
			InputSchema: objectSchema(map[string]any{
				"selector":   map[string]any{"type": "string", "description": "CSS selector to wait for."},
				"text":       map[string]any{"type": "string", "description": "Visible text to wait for."},
				"timeout_ms": map[string]any{"type": "number", "description": "Maximum wait time, capped at 15000 ms."},
			}, nil),
		},
	}
}

func objectSchema(properties map[string]any, required []string) map[string]any {
	if properties == nil {
		properties = map[string]any{}
	}
	schema := map[string]any{
		"type":                 "object",
		"properties":           properties,
		"additionalProperties": false,
	}
	if len(required) > 0 {
		schema["required"] = required
	}
	return schema
}

func targetSchema() map[string]any {
	return objectSchema(map[string]any{
		"selector": map[string]any{"type": "string", "description": "CSS selector for the target."},
		"text":     map[string]any{"type": "string", "description": "Visible text, aria-label, placeholder, or title to target."},
		"x":        map[string]any{"type": "number", "description": "Viewport X coordinate."},
		"y":        map[string]any{"type": "number", "description": "Viewport Y coordinate."},
	}, nil)
}

func callAntigravityTool(ctx context.Context, name string, args map[string]any) mcpToolResult {
	if args == nil {
		args = map[string]any{}
	}
	ctx, cancel := context.WithTimeout(ctx, defaultBrowserWaitLimit)
	defer cancel()

	var result mcpToolResult
	var err error

	switch name {
	case "browser_status":
		result = textToolResult(jsonText(antigravityRuntimeStatus(ctx)))
	case "browser_pages":
		result, err = toolBrowserPages(ctx)
	case "browser_navigate":
		result, err = toolBrowserNavigate(ctx, stringArg(args, "url"))
	case "browser_snapshot":
		result, err = toolBrowserSnapshot(ctx, intArg(args, "max_text_chars", 6000))
	case "browser_screenshot":
		result, err = toolBrowserScreenshot(ctx, boolArg(args, "include_image", false))
	case "browser_move":
		result, err = toolBrowserMove(ctx, args)
	case "browser_click":
		result, err = toolBrowserClick(ctx, args)
	case "browser_type":
		result, err = toolBrowserType(ctx, args)
	case "browser_press_key":
		result, err = toolBrowserPressKey(ctx, stringArg(args, "key"))
	case "browser_wait":
		result, err = toolBrowserWait(ctx, args)
	default:
		err = fmt.Errorf("unknown Antigravity browser tool %q", name)
	}

	if err != nil {
		writeAntigravityBrowserState(antigravityBrowserState{
			UpdatedAt:  time.Now().Format(time.RFC3339),
			PID:        os.Getpid(),
			Mode:       antigravityBrowserMode(),
			BrowserURL: antigravityCDPBaseURL(),
			Connected:  antigravityBrowserEndpointRunning(antigravityCDPBaseURL()),
			LastTool:   name,
			LastAction: "error",
			LastError:  err.Error(),
		})
		return errorToolResult(err.Error())
	}
	return result
}

func toolBrowserPages(ctx context.Context) (mcpToolResult, error) {
	if err := ensureChromeDebugEndpoint(ctx); err != nil {
		return mcpToolResult{}, err
	}
	targets, err := listCDPTargets(ctx)
	if err != nil {
		return mcpToolResult{}, err
	}
	rows := make([]map[string]any, 0, len(targets))
	for _, target := range targets {
		if target.Type != "page" {
			continue
		}
		rows = append(rows, map[string]any{
			"id":    target.ID,
			"title": target.Title,
			"url":   target.URL,
			"type":  target.Type,
		})
	}
	return textToolResult(jsonText(map[string]any{"pages": rows})), nil
}

func toolBrowserNavigate(ctx context.Context, rawURL string) (mcpToolResult, error) {
	if strings.TrimSpace(rawURL) == "" {
		return mcpToolResult{}, errors.New("url is required")
	}
	normalized, err := normalizeBrowserURL(rawURL)
	if err != nil {
		return mcpToolResult{}, err
	}
	client, err := newCDPClient(ctx)
	if err != nil {
		return mcpToolResult{}, err
	}
	defer client.Close()

	if _, err := client.Call(ctx, "Page.enable", map[string]any{}); err != nil {
		return mcpToolResult{}, err
	}
	if _, err := client.Call(ctx, "Page.navigate", map[string]any{"url": normalized}); err != nil {
		return mcpToolResult{}, err
	}
	_ = client.waitReady(ctx, 8*time.Second)
	page := client.pageInfo(ctx)
	writeAntigravityBrowserStateFromPage("browser_navigate", "navigated to "+normalized, client.baseURL, page, "")
	return textToolResult(jsonText(map[string]any{
		"ok":     true,
		"url":    page["url"],
		"title":  page["title"],
		"target": client.target.ID,
	})), nil
}

func toolBrowserSnapshot(ctx context.Context, maxTextChars int) (mcpToolResult, error) {
	if maxTextChars <= 0 || maxTextChars > 20000 {
		maxTextChars = 6000
	}
	client, err := newCDPClient(ctx)
	if err != nil {
		return mcpToolResult{}, err
	}
	defer client.Close()

	script := fmt.Sprintf(snapshotScriptTemplate, maxTextChars)
	value, err := client.Evaluate(ctx, script, true)
	if err != nil {
		return mcpToolResult{}, err
	}
	page := mapFromAny(value)
	writeAntigravityBrowserStateFromPage("browser_snapshot", "read page snapshot", client.baseURL, page, "")
	return textToolResult(jsonText(value)), nil
}

func toolBrowserScreenshot(ctx context.Context, includeImage bool) (mcpToolResult, error) {
	client, err := newCDPClient(ctx)
	if err != nil {
		return mcpToolResult{}, err
	}
	defer client.Close()

	if _, err := client.Call(ctx, "Page.enable", map[string]any{}); err != nil {
		return mcpToolResult{}, err
	}
	result, err := client.Call(ctx, "Page.captureScreenshot", map[string]any{
		"format":                "png",
		"captureBeyondViewport": false,
	})
	if err != nil {
		return mcpToolResult{}, err
	}
	data, _ := result["data"].(string)
	if data == "" {
		return mcpToolResult{}, errors.New("Chrome returned an empty screenshot")
	}
	page := client.pageInfo(ctx)
	screenshotPath, byteCount, err := saveScreenshotPNG(data, page)
	if err != nil {
		return mcpToolResult{}, err
	}
	writeAntigravityBrowserScreenshotState(client.baseURL, page, screenshotPath, "")
	content := []map[string]any{
		{"type": "text", "text": jsonText(map[string]any{
			"ok":              true,
			"url":             stringFromMap(page, "url"),
			"title":           stringFromMap(page, "title"),
			"screenshot_path": screenshotPath,
			"bytes":           byteCount,
			"inline_image":    includeImage,
		})},
	}
	if includeImage {
		content = append(content, map[string]any{"type": "image", "data": data, "mimeType": "image/png"})
	}
	return mcpToolResult{Content: content}, nil
}

func toolBrowserMove(ctx context.Context, args map[string]any) (mcpToolResult, error) {
	client, err := newCDPClient(ctx)
	if err != nil {
		return mcpToolResult{}, err
	}
	defer client.Close()
	point, err := moveOverlayToTarget(ctx, client, args)
	if err != nil {
		return mcpToolResult{}, err
	}
	writeAntigravityBrowserStateFromPage("browser_move", "moved visible cursor", client.baseURL, point, "")
	return textToolResult(jsonText(point)), nil
}

func toolBrowserClick(ctx context.Context, args map[string]any) (mcpToolResult, error) {
	client, err := newCDPClient(ctx)
	if err != nil {
		return mcpToolResult{}, err
	}
	defer client.Close()
	point, err := moveOverlayToTarget(ctx, client, args)
	if err != nil {
		return mcpToolResult{}, err
	}
	x, y, err := xyFromPoint(point)
	if err != nil {
		return mcpToolResult{}, err
	}
	if _, err := client.Call(ctx, "Input.dispatchMouseEvent", map[string]any{"type": "mouseMoved", "x": x, "y": y}); err != nil {
		return mcpToolResult{}, err
	}
	if _, err := client.Call(ctx, "Input.dispatchMouseEvent", map[string]any{"type": "mousePressed", "x": x, "y": y, "button": "left", "clickCount": 1}); err != nil {
		return mcpToolResult{}, err
	}
	if _, err := client.Call(ctx, "Input.dispatchMouseEvent", map[string]any{"type": "mouseReleased", "x": x, "y": y, "button": "left", "clickCount": 1}); err != nil {
		return mcpToolResult{}, err
	}
	writeAntigravityBrowserStateFromPage("browser_click", "clicked visible target", client.baseURL, point, "")
	return textToolResult(jsonText(point)), nil
}

func toolBrowserType(ctx context.Context, args map[string]any) (mcpToolResult, error) {
	text := stringArg(args, "text")
	if text == "" {
		return mcpToolResult{}, errors.New("text is required")
	}
	client, err := newCDPClient(ctx)
	if err != nil {
		return mcpToolResult{}, err
	}
	defer client.Close()

	targetArgs := map[string]any{}
	for _, key := range []string{"selector", "x", "y"} {
		if value, ok := args[key]; ok {
			targetArgs[key] = value
		}
	}
	if targetText := stringArg(args, "target_text"); targetText != "" {
		targetArgs["text"] = targetText
	}
	if len(targetArgs) > 0 {
		point, err := moveOverlayToTarget(ctx, client, targetArgs)
		if err != nil {
			return mcpToolResult{}, err
		}
		x, y, err := xyFromPoint(point)
		if err != nil {
			return mcpToolResult{}, err
		}
		_, _ = client.Call(ctx, "Input.dispatchMouseEvent", map[string]any{"type": "mouseMoved", "x": x, "y": y})
		_, _ = client.Call(ctx, "Input.dispatchMouseEvent", map[string]any{"type": "mousePressed", "x": x, "y": y, "button": "left", "clickCount": 1})
		_, _ = client.Call(ctx, "Input.dispatchMouseEvent", map[string]any{"type": "mouseReleased", "x": x, "y": y, "button": "left", "clickCount": 1})
	}
	if boolArg(args, "clear", false) {
		if err := dispatchCtrlAAndBackspace(ctx, client); err != nil {
			return mcpToolResult{}, err
		}
	}
	for _, chunk := range chunkString(text, 1000) {
		if _, err := client.Call(ctx, "Input.insertText", map[string]any{"text": chunk}); err != nil {
			return mcpToolResult{}, err
		}
	}
	page := client.pageInfo(ctx)
	writeAntigravityBrowserStateFromPage("browser_type", fmt.Sprintf("typed %d characters", len(text)), client.baseURL, page, "")
	return textToolResult(jsonText(map[string]any{
		"ok":          true,
		"typed_chars": len(text),
		"url":         page["url"],
		"title":       page["title"],
	})), nil
}

func toolBrowserPressKey(ctx context.Context, key string) (mcpToolResult, error) {
	key = strings.TrimSpace(key)
	if key == "" {
		return mcpToolResult{}, errors.New("key is required")
	}
	client, err := newCDPClient(ctx)
	if err != nil {
		return mcpToolResult{}, err
	}
	defer client.Close()
	if err := dispatchKeyPress(ctx, client, key); err != nil {
		return mcpToolResult{}, err
	}
	page := client.pageInfo(ctx)
	writeAntigravityBrowserStateFromPage("browser_press_key", "pressed "+key, client.baseURL, page, "")
	return textToolResult(jsonText(map[string]any{"ok": true, "key": key, "url": page["url"], "title": page["title"]})), nil
}

func toolBrowserWait(ctx context.Context, args map[string]any) (mcpToolResult, error) {
	selector := stringArg(args, "selector")
	text := stringArg(args, "text")
	if selector == "" && text == "" {
		return mcpToolResult{}, errors.New("selector or text is required")
	}
	timeout := time.Duration(intArg(args, "timeout_ms", 5000)) * time.Millisecond
	if timeout <= 0 || timeout > 15*time.Second {
		timeout = 5 * time.Second
	}
	client, err := newCDPClient(ctx)
	if err != nil {
		return mcpToolResult{}, err
	}
	defer client.Close()

	deadline := time.Now().Add(timeout)
	for {
		found, err := client.Evaluate(ctx, waitScript(selector, text), true)
		if err == nil {
			if ok, _ := found.(bool); ok {
				page := client.pageInfo(ctx)
				writeAntigravityBrowserStateFromPage("browser_wait", "wait condition matched", client.baseURL, page, "")
				return textToolResult(jsonText(map[string]any{"ok": true, "selector": selector, "text": text})), nil
			}
		}
		if time.Now().After(deadline) {
			return mcpToolResult{}, fmt.Errorf("timed out waiting for selector/text after %s", timeout)
		}
		time.Sleep(250 * time.Millisecond)
	}
}

func antigravityRuntimeStatus(ctx context.Context) map[string]any {
	baseURL := antigravityCDPBaseURL()
	debugRunning := antigravityBrowserEndpointRunning(baseURL)
	status := map[string]any{
		"server": map[string]any{
			"name":    antigravityMCPName,
			"version": antigravityMCPVersion,
			"pid":     os.Getpid(),
		},
		"chrome": map[string]any{
			"path":          chromeExecutablePath(),
			"mode":          antigravityBrowserMode(),
			"browser_url":   baseURL,
			"debug_running": debugRunning,
		},
		"extension": map[string]any{
			"id":       antigravityExtensionID,
			"path":     antigravityExtensionPath(),
			"manifest": antigravityManifestInfo(antigravityExtensionPath()),
		},
		"state": readAntigravityBrowserState(),
	}
	if !debugRunning {
		status["ok"] = true
		status["pages"] = 0
		status["note"] = "Browser control is idle. It will open only when a browser action tool is called."
		return status
	}
	targets, err := listCDPTargets(ctx)
	if err != nil {
		status["ok"] = false
		status["error"] = err.Error()
		return status
	}
	status["ok"] = true
	status["pages"] = len(targets)
	return status
}

func newCDPClient(ctx context.Context) (*cdpClient, error) {
	if err := ensureChromeDebugEndpoint(ctx); err != nil {
		return nil, err
	}
	targets, err := listCDPTargets(ctx)
	if err != nil {
		return nil, err
	}
	target, err := chooseCDPTarget(ctx, targets)
	if err != nil {
		return nil, err
	}
	conn, _, err := websocket.DefaultDialer.DialContext(ctx, target.WebSocketDebuggerURL, nil)
	if err != nil {
		return nil, fmt.Errorf("could not connect to Chrome page websocket: %w", err)
	}
	return &cdpClient{conn: conn, nextID: 1, target: target, baseURL: antigravityCDPBaseURL()}, nil
}

func (c *cdpClient) Close() {
	if c != nil && c.conn != nil {
		_ = c.conn.Close()
	}
}

func (c *cdpClient) Call(ctx context.Context, method string, params map[string]any) (map[string]any, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	id := c.nextID
	c.nextID++
	if params == nil {
		params = map[string]any{}
	}
	if err := c.conn.WriteJSON(map[string]any{"id": id, "method": method, "params": params}); err != nil {
		return nil, err
	}
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}
		_ = c.conn.SetReadDeadline(time.Now().Add(15 * time.Second))
		var msg map[string]any
		if err := c.conn.ReadJSON(&msg); err != nil {
			return nil, err
		}
		if gotID, ok := numericID(msg["id"]); !ok || gotID != id {
			continue
		}
		if rawErr, ok := msg["error"]; ok {
			return nil, fmt.Errorf("Chrome DevTools error for %s: %s", method, jsonText(rawErr))
		}
		if result, ok := msg["result"].(map[string]any); ok {
			return result, nil
		}
		return map[string]any{}, nil
	}
}

func (c *cdpClient) Evaluate(ctx context.Context, expression string, awaitPromise bool) (any, error) {
	result, err := c.Call(ctx, "Runtime.evaluate", map[string]any{
		"expression":    expression,
		"awaitPromise":  awaitPromise,
		"returnByValue": true,
	})
	if err != nil {
		return nil, err
	}
	if details, ok := result["exceptionDetails"]; ok {
		return nil, fmt.Errorf("page script failed: %s", jsonText(details))
	}
	raw, ok := result["result"].(map[string]any)
	if !ok {
		return nil, nil
	}
	if value, ok := raw["value"]; ok {
		return value, nil
	}
	if desc, ok := raw["description"].(string); ok {
		return desc, nil
	}
	return nil, nil
}

func (c *cdpClient) waitReady(ctx context.Context, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		value, err := c.Evaluate(ctx, `document.readyState`, false)
		if err == nil {
			if state, _ := value.(string); state == "interactive" || state == "complete" {
				return nil
			}
		}
		time.Sleep(250 * time.Millisecond)
	}
	return errors.New("page did not become ready before timeout")
}

func (c *cdpClient) pageInfo(ctx context.Context) map[string]any {
	value, err := c.Evaluate(ctx, `({url: location.href, title: document.title})`, false)
	if err != nil {
		return map[string]any{"url": c.target.URL, "title": c.target.Title}
	}
	page := mapFromAny(value)
	if page["url"] == "" {
		page["url"] = c.target.URL
	}
	if page["title"] == "" {
		page["title"] = c.target.Title
	}
	return page
}

func ensureChromeDebugEndpoint(ctx context.Context) error {
	baseURL := antigravityCDPBaseURL()
	if antigravityBrowserEndpointRunning(baseURL) {
		return nil
	}
	if err := startChromeForCDP(); err != nil {
		return err
	}
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		if antigravityBrowserEndpointRunning(baseURL) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
	return fmt.Errorf("Chrome did not expose DevTools at %s. Modern Chrome blocks DevTools control on the normal Default profile; the bridge uses a controlled profile unless ANTIGRAVITY_BROWSER_FORCE_DEFAULT_CDP=1 is set", baseURL)
}

func startChromeForCDP() error {
	chromePath := chromeExecutablePath()
	if chromePath == "" {
		return errors.New("Google Chrome was not found. Set ANTIGRAVITY_CHROME_PATH to chrome.exe")
	}
	debugPort := antigravityBrowserDebugPort()
	mode := antigravityBrowserMode()
	effectiveMode := mode
	lastAction := "started Chrome for browser control"
	args := []string{
		"--remote-debugging-address=127.0.0.1",
		"--remote-debugging-port=" + debugPort,
		"--no-first-run",
		"--no-default-browser-check",
	}
	if mode != "dedicated" && chromeProcessRunning() {
		if !defaultChromeCDPForced() {
			effectiveMode = "dedicated_fallback"
			lastAction = "started dedicated fallback because Chrome blocks DevTools on the Default profile"
		} else if canRelaunchDefaultChrome() {
			if err := stopChromeProcessesForDefaultBridge(); err != nil {
				return err
			}
			effectiveMode = "default_relaunched"
			lastAction = "relaunched default Chrome for browser control"
		} else {
			effectiveMode = "dedicated_fallback"
			lastAction = "started dedicated fallback because default Chrome has user windows without DevTools"
		}
	}
	if mode != "dedicated" && !chromeProcessRunning() && !defaultChromeCDPForced() {
		effectiveMode = "dedicated_fallback"
		lastAction = "started dedicated fallback because Chrome blocks DevTools on the Default profile"
	}
	if effectiveMode == "dedicated" || effectiveMode == "dedicated_fallback" {
		profilePath := antigravityProfilePath()
		_ = os.MkdirAll(profilePath, 0700)
		args = append(args, "--user-data-dir="+profilePath)
		if extensionPath := antigravityExtensionPath(); extensionPath != "" {
			args = append(args, "--load-extension="+extensionPath, "--disable-extensions-except="+extensionPath)
		}
	} else {
		args = append(args, "--profile-directory=Default")
	}
	args = append(args, "about:blank")
	cmd := exec.Command(chromePath, args...)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("could not start Chrome for browser control: %w", err)
	}
	if (effectiveMode == "dedicated" || effectiveMode == "dedicated_fallback") && cmd.Process != nil {
		_ = os.WriteFile(filepath.Join(mustGetwd(), ".antigravity-browser.pid"), []byte(strconv.Itoa(cmd.Process.Pid)), 0600)
	}
	writeAntigravityBrowserState(antigravityBrowserState{
		UpdatedAt:  time.Now().Format(time.RFC3339),
		PID:        os.Getpid(),
		Mode:       effectiveMode,
		BrowserURL: antigravityCDPBaseURL(),
		Connected:  false,
		LastAction: lastAction,
	})
	return nil
}

func chromeProcessRunning() bool {
	return len(chromeProcesses()) > 0
}

func chromeProcesses() []chromeProcessInfo {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "tasklist.exe", "/FI", "IMAGENAME eq chrome.exe", "/V", "/FO", "CSV", "/NH").Output()
	if err != nil {
		return nil
	}
	raw := strings.TrimSpace(string(out))
	if raw == "" || strings.Contains(strings.ToLower(raw), "no tasks are running") {
		return nil
	}
	reader := csv.NewReader(strings.NewReader(raw))
	records, err := reader.ReadAll()
	if err != nil {
		return nil
	}
	processes := make([]chromeProcessInfo, 0, len(records))
	for _, record := range records {
		if len(record) < 2 {
			continue
		}
		pid, err := strconv.Atoi(strings.TrimSpace(record[1]))
		if err != nil {
			continue
		}
		title := ""
		if len(record) > 0 {
			title = strings.TrimSpace(record[len(record)-1])
		}
		if strings.EqualFold(title, "N/A") {
			title = ""
		}
		processes = append(processes, chromeProcessInfo{
			ID:        pid,
			Title:     title,
			IsVisible: title != "",
		})
	}
	return processes
}

func canRelaunchDefaultChrome() bool {
	return canRelaunchDefaultChromeFrom(chromeProcesses())
}

func canRelaunchDefaultChromeFrom(processes []chromeProcessInfo) bool {
	if !defaultChromeCDPForced() {
		return false
	}
	if !envFlag("ANTIGRAVITY_BROWSER_SAFE_DEFAULT_RELAUNCH", true) {
		return false
	}
	visible := visibleChromeWindowsFrom(processes)
	if len(visible) == 0 {
		return true
	}
	for _, proc := range visible {
		if !safeChromeWindowTitle(proc.Title) {
			return false
		}
	}
	return true
}

func visibleChromeWindows() []chromeProcessInfo {
	return visibleChromeWindowsFrom(chromeProcesses())
}

func visibleChromeWindowsFrom(processes []chromeProcessInfo) []chromeProcessInfo {
	visible := make([]chromeProcessInfo, 0, len(processes))
	for _, proc := range processes {
		if proc.IsVisible {
			visible = append(visible, proc)
		}
	}
	return visible
}

func safeChromeWindowTitle(title string) bool {
	normalized := strings.ToLower(strings.TrimSpace(title))
	if normalized == "" {
		return true
	}
	if normalized == "about:blank - google chrome" || normalized == "new tab - google chrome" || normalized == "olemainthreadwndname" {
		return true
	}
	safeParts := []string{
		"claude code codex proxy",
		"codex proxy control panel",
		"127.0.0.1:4000",
		"localhost:4000",
	}
	for _, part := range safeParts {
		if strings.Contains(normalized, part) {
			return true
		}
	}
	return false
}

func stopChromeProcessesForDefaultBridge() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = exec.CommandContext(ctx, "taskkill.exe", "/IM", "chrome.exe", "/F", "/T").Run()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if !chromeProcessRunning() {
			return nil
		}
		time.Sleep(250 * time.Millisecond)
	}
	if chromeProcessRunning() {
		return errors.New("could not stop existing Chrome background processes before launching the default profile with DevTools")
	}
	return nil
}

func defaultChromeCDPForced() bool {
	return envFlag("ANTIGRAVITY_BROWSER_FORCE_DEFAULT_CDP", false)
}

func defaultChromeCDPNote() string {
	if defaultChromeCDPForced() {
		return "Default profile CDP is forced by ANTIGRAVITY_BROWSER_FORCE_DEFAULT_CDP=1. This is intended only for older or custom Chrome builds."
	}
	return "Modern Chrome blocks DevTools control on the normal Default profile. Claude Code browser actions use a controlled profile unless Chrome is already exposing a DevTools endpoint."
}

func listCDPTargets(ctx context.Context) ([]cdpTarget, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(antigravityCDPBaseURL(), "/")+"/json/list", nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("could not list Chrome pages: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return nil, fmt.Errorf("Chrome page list returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var targets []cdpTarget
	if err := json.NewDecoder(resp.Body).Decode(&targets); err != nil {
		return nil, err
	}
	return targets, nil
}

func chooseCDPTarget(ctx context.Context, targets []cdpTarget) (cdpTarget, error) {
	for _, target := range targets {
		if target.Type == "page" && target.WebSocketDebuggerURL != "" && !strings.HasPrefix(target.URL, "devtools://") {
			return target, nil
		}
	}
	if err := openBlankCDPTarget(ctx); err != nil {
		return cdpTarget{}, err
	}
	targets, err := listCDPTargets(ctx)
	if err != nil {
		return cdpTarget{}, err
	}
	for _, target := range targets {
		if target.Type == "page" && target.WebSocketDebuggerURL != "" {
			return target, nil
		}
	}
	return cdpTarget{}, errors.New("no controllable Chrome page target found")
}

func openBlankCDPTarget(ctx context.Context) error {
	endpoint := strings.TrimRight(antigravityCDPBaseURL(), "/") + "/json/new?" + url.QueryEscape("about:blank")
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, endpoint, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	return fmt.Errorf("could not open a new Chrome page, status %d", resp.StatusCode)
}

func antigravityCDPBaseURL() string {
	if raw := strings.TrimSpace(os.Getenv("ANTIGRAVITY_BROWSER_URL")); raw != "" {
		return strings.TrimRight(raw, "/")
	}
	return "http://127.0.0.1:" + antigravityBrowserDebugPort()
}

func moveOverlayToTarget(ctx context.Context, client *cdpClient, args map[string]any) (map[string]any, error) {
	target := map[string]any{}
	if selector := stringArg(args, "selector"); selector != "" {
		target["selector"] = selector
	}
	if text := stringArg(args, "text"); text != "" {
		target["text"] = text
	}
	if x, ok := floatArg(args, "x"); ok {
		target["x"] = x
	}
	if y, ok := floatArg(args, "y"); ok {
		target["y"] = y
	}
	raw, _ := json.Marshal(target)
	overlayID, _ := json.Marshal(antigravityOverlayID)
	script := strings.ReplaceAll(visibleCursorScript, "__ARGS__", string(raw))
	script = strings.ReplaceAll(script, "__OVERLAY_ID__", string(overlayID))
	value, err := client.Evaluate(ctx, script, true)
	if err != nil {
		return nil, err
	}
	point := mapFromAny(value)
	if _, _, err := xyFromPoint(point); err != nil {
		return nil, err
	}
	return point, nil
}

func xyFromPoint(point map[string]any) (float64, float64, error) {
	x, okX := floatFromAny(point["x"])
	y, okY := floatFromAny(point["y"])
	if !okX || !okY || math.IsNaN(x) || math.IsNaN(y) {
		return 0, 0, errors.New("target did not resolve to a valid viewport coordinate")
	}
	return x, y, nil
}

func dispatchCtrlAAndBackspace(ctx context.Context, client *cdpClient) error {
	events := []map[string]any{
		{"type": "keyDown", "key": "Control", "code": "ControlLeft", "windowsVirtualKeyCode": 17},
		{"type": "keyDown", "key": "a", "code": "KeyA", "windowsVirtualKeyCode": 65, "modifiers": 2},
		{"type": "keyUp", "key": "a", "code": "KeyA", "windowsVirtualKeyCode": 65, "modifiers": 2},
		{"type": "keyUp", "key": "Control", "code": "ControlLeft", "windowsVirtualKeyCode": 17},
		{"type": "keyDown", "key": "Backspace", "code": "Backspace", "windowsVirtualKeyCode": 8},
		{"type": "keyUp", "key": "Backspace", "code": "Backspace", "windowsVirtualKeyCode": 8},
	}
	for _, event := range events {
		if _, err := client.Call(ctx, "Input.dispatchKeyEvent", event); err != nil {
			return err
		}
	}
	return nil
}

func dispatchKeyPress(ctx context.Context, client *cdpClient, key string) error {
	spec := keySpec(key)
	if spec["text"] != "" {
		if _, err := client.Call(ctx, "Input.dispatchKeyEvent", map[string]any{
			"type":                  "keyDown",
			"key":                   spec["key"],
			"code":                  spec["code"],
			"text":                  spec["text"],
			"windowsVirtualKeyCode": spec["vk"],
		}); err != nil {
			return err
		}
	} else {
		if _, err := client.Call(ctx, "Input.dispatchKeyEvent", map[string]any{
			"type":                  "keyDown",
			"key":                   spec["key"],
			"code":                  spec["code"],
			"windowsVirtualKeyCode": spec["vk"],
		}); err != nil {
			return err
		}
	}
	_, err := client.Call(ctx, "Input.dispatchKeyEvent", map[string]any{
		"type":                  "keyUp",
		"key":                   spec["key"],
		"code":                  spec["code"],
		"windowsVirtualKeyCode": spec["vk"],
	})
	return err
}

func keySpec(key string) map[string]any {
	normalized := strings.ToLower(strings.TrimSpace(key))
	switch normalized {
	case "enter", "return":
		return map[string]any{"key": "Enter", "code": "Enter", "vk": 13, "text": "\r"}
	case "tab":
		return map[string]any{"key": "Tab", "code": "Tab", "vk": 9, "text": "\t"}
	case "escape", "esc":
		return map[string]any{"key": "Escape", "code": "Escape", "vk": 27, "text": ""}
	case "backspace":
		return map[string]any{"key": "Backspace", "code": "Backspace", "vk": 8, "text": ""}
	case "delete":
		return map[string]any{"key": "Delete", "code": "Delete", "vk": 46, "text": ""}
	case "arrowdown", "down":
		return map[string]any{"key": "ArrowDown", "code": "ArrowDown", "vk": 40, "text": ""}
	case "arrowup", "up":
		return map[string]any{"key": "ArrowUp", "code": "ArrowUp", "vk": 38, "text": ""}
	case "arrowleft", "left":
		return map[string]any{"key": "ArrowLeft", "code": "ArrowLeft", "vk": 37, "text": ""}
	case "arrowright", "right":
		return map[string]any{"key": "ArrowRight", "code": "ArrowRight", "vk": 39, "text": ""}
	}
	if len([]rune(key)) == 1 {
		upper := strings.ToUpper(key)
		return map[string]any{"key": key, "code": "Key" + upper, "vk": int([]rune(upper)[0]), "text": key}
	}
	return map[string]any{"key": key, "code": key, "vk": 0, "text": ""}
}

func waitScript(selector string, text string) string {
	payload, _ := json.Marshal(map[string]string{"selector": selector, "text": text})
	return `(() => {
  const args = ` + string(payload) + `;
  if (args.selector && document.querySelector(args.selector)) return true;
  if (args.text) {
    const q = args.text.toLowerCase();
    return (document.body && document.body.innerText || "").toLowerCase().includes(q);
  }
  return false;
})()`
}

func normalizeBrowserURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", errors.New("url is required")
	}
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "", fmt.Errorf("invalid url %q", raw)
	}
	return parsed.String(), nil
}

func textToolResult(text string) mcpToolResult {
	return mcpToolResult{Content: []map[string]any{{"type": "text", "text": text}}}
}

func errorToolResult(text string) mcpToolResult {
	return mcpToolResult{IsError: true, Content: []map[string]any{{"type": "text", "text": text}}}
}

func jsonText(value any) string {
	raw, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return fmt.Sprint(value)
	}
	return string(raw)
}

func numericID(value any) (int, bool) {
	switch v := value.(type) {
	case float64:
		return int(v), true
	case int:
		return v, true
	case json.Number:
		n, err := v.Int64()
		return int(n), err == nil
	default:
		return 0, false
	}
}

func stringArg(args map[string]any, key string) string {
	if value, ok := args[key]; ok {
		return strings.TrimSpace(fmt.Sprint(value))
	}
	return ""
}

func intArg(args map[string]any, key string, fallback int) int {
	value, ok := args[key]
	if !ok {
		return fallback
	}
	switch v := value.(type) {
	case float64:
		return int(v)
	case int:
		return v
	case string:
		n, err := strconv.Atoi(strings.TrimSpace(v))
		if err == nil {
			return n
		}
	}
	return fallback
}

func boolArg(args map[string]any, key string, fallback bool) bool {
	value, ok := args[key]
	if !ok {
		return fallback
	}
	switch v := value.(type) {
	case bool:
		return v
	case string:
		return strings.EqualFold(v, "true") || v == "1" || strings.EqualFold(v, "yes")
	default:
		return fallback
	}
}

func floatArg(args map[string]any, key string) (float64, bool) {
	value, ok := args[key]
	if !ok {
		return 0, false
	}
	return floatFromAny(value)
}

func floatFromAny(value any) (float64, bool) {
	switch v := value.(type) {
	case float64:
		return v, true
	case float32:
		return float64(v), true
	case int:
		return float64(v), true
	case int64:
		return float64(v), true
	case json.Number:
		n, err := v.Float64()
		return n, err == nil
	case string:
		n, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
		return n, err == nil
	default:
		return 0, false
	}
}

func mapFromAny(value any) map[string]any {
	if m, ok := value.(map[string]any); ok {
		return m
	}
	return map[string]any{}
}

func stringFromMap(value map[string]any, key string) string {
	if value == nil {
		return ""
	}
	return fmt.Sprint(value[key])
}

func saveScreenshotPNG(data string, page map[string]any) (string, int, error) {
	raw, err := base64.StdEncoding.DecodeString(data)
	if err != nil {
		return "", 0, fmt.Errorf("could not decode Chrome screenshot: %w", err)
	}
	dir := filepath.Join(mustGetwd(), antigravityScreenshotDir)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", 0, fmt.Errorf("could not create screenshot folder: %w", err)
	}
	host := "page"
	if parsed, err := url.Parse(stringFromMap(page, "url")); err == nil && strings.TrimSpace(parsed.Hostname()) != "" {
		host = parsed.Hostname()
	}
	name := sanitizeFilePart(host)
	if name == "" {
		name = "page"
	}
	path := filepath.Join(dir, fmt.Sprintf("%s-%s-%d.png", name, time.Now().Format("20060102-150405"), time.Now().UnixNano()%1000000))
	if err := os.WriteFile(path, raw, 0600); err != nil {
		return "", 0, fmt.Errorf("could not write screenshot: %w", err)
	}
	return path, len(raw), nil
}

func sanitizeFilePart(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	var b strings.Builder
	lastDash := false
	for _, r := range value {
		ok := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')
		if ok {
			b.WriteRune(r)
			lastDash = false
			continue
		}
		if !lastDash {
			b.WriteByte('-')
			lastDash = true
		}
	}
	return strings.Trim(b.String(), "-")
}

func chunkString(value string, size int) []string {
	if size <= 0 || len(value) <= size {
		return []string{value}
	}
	var chunks []string
	for len(value) > size {
		chunks = append(chunks, value[:size])
		value = value[size:]
	}
	if value != "" {
		chunks = append(chunks, value)
	}
	return chunks
}

func writeAntigravityBrowserStateFromPage(tool string, action string, browserURL string, page map[string]any, lastError string) {
	writeAntigravityBrowserState(antigravityBrowserState{
		UpdatedAt:    time.Now().Format(time.RFC3339),
		PID:          os.Getpid(),
		Mode:         antigravityBrowserMode(),
		BrowserURL:   browserURL,
		Connected:    true,
		CurrentURL:   stringFromMap(page, "url"),
		CurrentTitle: stringFromMap(page, "title"),
		LastTool:     tool,
		LastAction:   action,
		LastError:    lastError,
	})
}

func writeAntigravityBrowserScreenshotState(browserURL string, page map[string]any, screenshotPath string, lastError string) {
	state := antigravityBrowserState{
		UpdatedAt:      time.Now().Format(time.RFC3339),
		PID:            os.Getpid(),
		Mode:           antigravityBrowserMode(),
		BrowserURL:     browserURL,
		Connected:      true,
		CurrentURL:     stringFromMap(page, "url"),
		CurrentTitle:   stringFromMap(page, "title"),
		LastTool:       "browser_screenshot",
		LastAction:     "saved screenshot",
		LastError:      lastError,
		LastScreenshot: screenshotPath,
	}
	writeAntigravityBrowserState(state)
}

func writeAntigravityBrowserState(state antigravityBrowserState) {
	if state.UpdatedAt == "" {
		state.UpdatedAt = time.Now().Format(time.RFC3339)
	}
	if state.PID == 0 {
		state.PID = os.Getpid()
	}
	raw, _ := json.MarshalIndent(state, "", "  ")
	_ = os.WriteFile(antigravityBrowserStatePath(), raw, 0600)
}

func readAntigravityBrowserState() map[string]any {
	raw, err := os.ReadFile(antigravityBrowserStatePath())
	if err != nil {
		return map[string]any{
			"exists": false,
			"path":   antigravityBrowserStatePath(),
		}
	}
	var state map[string]any
	if json.Unmarshal(raw, &state) != nil {
		return map[string]any{"exists": true, "path": antigravityBrowserStatePath(), "error": "state file is invalid JSON"}
	}
	state["exists"] = true
	state["path"] = antigravityBrowserStatePath()
	if updatedAt, _ := state["updated_at"].(string); updatedAt != "" {
		if ts, err := time.Parse(time.RFC3339, updatedAt); err == nil {
			state["fresh"] = time.Since(ts) < 5*time.Minute
		}
	}
	return state
}

func antigravityBrowserStatePath() string {
	return filepath.Join(mustGetwd(), antigravityStateFile)
}

const snapshotScriptTemplate = `(() => {
  const maxText = %d;
  const visible = (el) => {
    if (!el || !el.getBoundingClientRect) return false;
    const style = window.getComputedStyle(el);
    if (style.visibility === "hidden" || style.display === "none" || Number(style.opacity) === 0) return false;
    const rect = el.getBoundingClientRect();
    return rect.width > 0 && rect.height > 0 && rect.bottom >= 0 && rect.right >= 0 && rect.top <= innerHeight && rect.left <= innerWidth;
  };
  const label = (el) => (el.innerText || el.value || el.placeholder || el.getAttribute("aria-label") || el.title || "").trim().replace(/\s+/g, " ");
  const cssPath = (el) => {
    if (!el || !el.tagName) return "";
    if (el.id) return "#" + CSS.escape(el.id);
    const parts = [];
    while (el && el.nodeType === 1 && parts.length < 4) {
      let part = el.tagName.toLowerCase();
      if (el.classList && el.classList.length) part += "." + Array.from(el.classList).slice(0, 2).map(c => CSS.escape(c)).join(".");
      const parent = el.parentElement;
      if (parent) {
        const same = Array.from(parent.children).filter(x => x.tagName === el.tagName);
        if (same.length > 1) part += ":nth-of-type(" + (same.indexOf(el) + 1) + ")";
      }
      parts.unshift(part);
      el = parent;
    }
    return parts.join(" > ");
  };
  const elements = Array.from(document.querySelectorAll("a,button,input,textarea,select,[role],[contenteditable='true'],summary,label"))
    .filter(visible)
    .slice(0, 80)
    .map((el) => {
      const rect = el.getBoundingClientRect();
      return {
        tag: el.tagName.toLowerCase(),
        text: label(el).slice(0, 160),
        role: el.getAttribute("role") || "",
        type: el.getAttribute("type") || "",
        selector: cssPath(el),
        x: Math.round(rect.left + rect.width / 2),
        y: Math.round(rect.top + rect.height / 2)
      };
    });
  return {
    url: location.href,
    title: document.title,
    text: (document.body && document.body.innerText || "").trim().replace(/\n{3,}/g, "\n\n").slice(0, maxText),
    elements
  };
})()`

const visibleCursorScript = `(async () => {
  const args = __ARGS__;
  const overlayId = __OVERLAY_ID__;
  const sleep = (ms) => new Promise(resolve => setTimeout(resolve, ms));
  const ensureOverlay = () => {
    let cursor = document.getElementById(overlayId);
    if (!cursor) {
      const style = document.createElement("style");
      style.id = overlayId + "-style";
      style.textContent = ` + "`" + `
        #${overlayId} {
          position: fixed;
          z-index: 2147483647;
          width: 26px;
          height: 26px;
          border-radius: 999px;
          pointer-events: none;
          transform: translate(-50%, -50%);
          background: rgba(255, 193, 7, 0.22);
          border: 2px solid #ffc107;
          box-shadow: 0 0 0 4px rgba(10, 132, 255, 0.30), 0 8px 24px rgba(0,0,0,0.28);
          transition: left 180ms ease, top 180ms ease, transform 120ms ease;
        }
        #${overlayId}::after {
          content: "";
          position: absolute;
          left: 50%;
          top: 50%;
          width: 5px;
          height: 5px;
          margin: -2.5px 0 0 -2.5px;
          border-radius: 999px;
          background: #0a84ff;
          box-shadow: 0 0 0 2px #ffffff;
        }
      ` + "`" + `;
      document.documentElement.appendChild(style);
      cursor = document.createElement("div");
      cursor.id = overlayId;
      cursor.setAttribute("aria-hidden", "true");
      cursor.style.left = Math.round(innerWidth / 2) + "px";
      cursor.style.top = Math.round(innerHeight / 2) + "px";
      document.documentElement.appendChild(cursor);
    }
    return cursor;
  };
  const visible = (el) => {
    if (!el || !el.getBoundingClientRect) return false;
    const style = window.getComputedStyle(el);
    if (style.visibility === "hidden" || style.display === "none" || Number(style.opacity) === 0) return false;
    const rect = el.getBoundingClientRect();
    return rect.width > 0 && rect.height > 0;
  };
  const label = (el) => (el.innerText || el.value || el.placeholder || el.getAttribute("aria-label") || el.title || "").trim().replace(/\s+/g, " ");
  const cssPath = (el) => {
    if (!el || !el.tagName) return "";
    if (el.id) return "#" + CSS.escape(el.id);
    const parts = [];
    while (el && el.nodeType === 1 && parts.length < 4) {
      let part = el.tagName.toLowerCase();
      if (el.classList && el.classList.length) part += "." + Array.from(el.classList).slice(0, 2).map(c => CSS.escape(c)).join(".");
      const parent = el.parentElement;
      if (parent) {
        const same = Array.from(parent.children).filter(x => x.tagName === el.tagName);
        if (same.length > 1) part += ":nth-of-type(" + (same.indexOf(el) + 1) + ")";
      }
      parts.unshift(part);
      el = parent;
    }
    return parts.join(" > ");
  };
  const byText = (q) => {
    q = String(q || "").toLowerCase();
    const candidates = Array.from(document.querySelectorAll("a,button,input,textarea,select,[role],[contenteditable='true'],summary,label"));
    return candidates.find(el => visible(el) && label(el).toLowerCase().includes(q)) ||
      Array.from(document.querySelectorAll("body *")).find(el => visible(el) && label(el).toLowerCase().includes(q));
  };
  let target = null;
  if (args.selector) {
    target = document.querySelector(args.selector);
    if (!target) throw new Error("No element found for selector: " + args.selector);
  }
  if (!target && args.text) {
    target = byText(args.text);
    if (!target) throw new Error("No visible element found containing text: " + args.text);
  }
  let x = Number(args.x);
  let y = Number(args.y);
  if (target) {
    target.scrollIntoView({ block: "center", inline: "center", behavior: "instant" });
    await sleep(80);
    const rect = target.getBoundingClientRect();
    x = rect.left + rect.width / 2;
    y = rect.top + rect.height / 2;
    const previousOutline = target.style.outline;
    const previousShadow = target.style.boxShadow;
    target.style.outline = "2px solid #0a84ff";
    target.style.boxShadow = "0 0 0 4px rgba(255,193,7,0.35)";
    setTimeout(() => {
      target.style.outline = previousOutline;
      target.style.boxShadow = previousShadow;
    }, 1200);
  }
  if (!Number.isFinite(x) || !Number.isFinite(y)) {
    throw new Error("Provide selector/text or numeric x and y viewport coordinates");
  }
  x = Math.max(0, Math.min(innerWidth - 1, x));
  y = Math.max(0, Math.min(innerHeight - 1, y));
  const cursor = ensureOverlay();
  cursor.style.left = x + "px";
  cursor.style.top = y + "px";
  cursor.style.transform = "translate(-50%, -50%) scale(1.12)";
  await sleep(180);
  cursor.style.transform = "translate(-50%, -50%) scale(1)";
  return {
    ok: true,
    x,
    y,
    url: location.href,
    title: document.title,
    target: target ? { tag: target.tagName.toLowerCase(), text: label(target).slice(0, 180), selector: cssPath(target) } : null
  };
})()`
