package main

// mcpgateway.go — the "connect" MCP stdio gateway for the claude tool loop.
//
// When a claude request carries caller-defined tools (Connect's autonomous
// agent delegating its tool loop to the proxy), serveClaude* runs `claude -p`
// with `--mcp-config` pointing at THIS binary in `claude-mcp-gateway` mode. The
// Claude Code CLI spawns it as a stdio MCP server and, whenever the model wants
// a tool, calls our single umbrella tool `connect_execute_tool({tool_name,
// arguments})`. We validate the name against the allow-list and bridge the call
// back to Connect's callback endpoint (which runs the real tool and returns the
// result). The CLI loops until the model produces its final text.
//
// Per-request context (callback URL, bearer token, conversation/agent ids, the
// allowed tool names) is passed in via the mcp-config `env` block — verified to
// propagate to the spawned server. Nothing sensitive is on argv.
//
// Transport: newline-delimited JSON-RPC over stdin/stdout (the MCP stdio
// framing). The exact protocol below was validated end-to-end against the
// installed CLI before this Go port was written.

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

const mcpGatewayToolName = "connect_execute_tool"

type mcpRPC struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// runClaudeMCPGateway is the entry point for `connect-ai-proxy claude-mcp-gateway`.
// It speaks MCP over stdio until stdin closes (the CLI exits).
func runClaudeMCPGateway() error {
	callbackURL := strings.TrimSpace(os.Getenv("CONNECT_CALLBACK_URL"))
	callbackToken := strings.TrimSpace(os.Getenv("CONNECT_CALLBACK_TOKEN"))

	allowed := map[string]bool{}
	if raw := strings.TrimSpace(os.Getenv("CONNECT_ALLOWED_TOOLS")); raw != "" {
		var names []string
		if json.Unmarshal([]byte(raw), &names) == nil {
			for _, n := range names {
				if n = strings.TrimSpace(n); n != "" {
					allowed[n] = true
				}
			}
		}
	}

	httpc := &http.Client{Timeout: 120 * time.Second}

	in := bufio.NewReaderSize(os.Stdin, 1<<20)
	out := bufio.NewWriter(os.Stdout)

	send := func(v any) {
		b, err := json.Marshal(v)
		if err != nil {
			return
		}
		out.Write(b)
		out.WriteByte('\n')
		out.Flush()
	}

	for {
		line, err := in.ReadBytes('\n')
		if len(bytes.TrimSpace(line)) > 0 {
			handleMCPLine(bytes.TrimSpace(line), send, httpc, callbackURL, callbackToken, allowed)
		}
		if err != nil {
			// EOF (CLI closed stdin) or read error → exit cleanly.
			return nil
		}
	}
}

func handleMCPLine(line []byte, send func(any), httpc *http.Client, callbackURL, callbackToken string, allowed map[string]bool) {
	var req mcpRPC
	if json.Unmarshal(line, &req) != nil {
		return
	}

	switch req.Method {
	case "initialize":
		pv := "2024-11-05"
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		if json.Unmarshal(req.Params, &p) == nil && strings.TrimSpace(p.ProtocolVersion) != "" {
			pv = p.ProtocolVersion
		}
		send(map[string]any{
			"jsonrpc": "2.0",
			"id":      req.ID,
			"result": map[string]any{
				"protocolVersion": pv,
				"capabilities":    map[string]any{"tools": map[string]any{}},
				"serverInfo":      map[string]any{"name": "connect", "version": "1.0.0"},
			},
		})

	case "notifications/initialized", "notifications/cancelled":
		// notifications carry no id and need no response

	case "tools/list":
		send(map[string]any{
			"jsonrpc": "2.0",
			"id":      req.ID,
			"result": map[string]any{
				"tools": []any{map[string]any{
					"name":        mcpGatewayToolName,
					"description": "Execute ONE Connect agent tool by name. Pass {\"tool_name\": \"<name>\", \"arguments\": { ... }} where tool_name is one of the tools listed in the system instructions and arguments match that tool's schema. Returns the tool result as JSON ({success, data} or {success:false, error}).",
					"inputSchema": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"tool_name": map[string]any{"type": "string", "description": "Exact name of the Connect tool to run."},
							"arguments": map[string]any{"type": "object", "description": "Arguments object matching the named tool's input schema. Use {} if none.", "additionalProperties": true},
						},
						"required": []string{"tool_name"},
					},
				}},
			},
		})

	case "tools/call":
		var p struct {
			Name      string `json:"name"`
			Arguments struct {
				ToolName  string          `json:"tool_name"`
				Arguments json.RawMessage `json:"arguments"`
			} `json:"arguments"`
		}
		_ = json.Unmarshal(req.Params, &p)
		text := mcpExecuteTool(httpc, callbackURL, callbackToken, allowed, p.Arguments.ToolName, p.Arguments.Arguments)
		send(map[string]any{
			"jsonrpc": "2.0",
			"id":      req.ID,
			"result": map[string]any{
				"content": []any{map[string]any{"type": "text", "text": text}},
				"isError": false,
			},
		})

	default:
		// Respond with method-not-found only to requests (those carrying an id);
		// silently ignore unknown notifications.
		if len(req.ID) > 0 {
			send(map[string]any{
				"jsonrpc": "2.0",
				"id":      req.ID,
				"error":   map[string]any{"code": -32601, "message": "method not found: " + req.Method},
			})
		}
	}
}

// mcpExecuteTool validates the tool name and bridges the call to Connect's
// callback endpoint, returning the JSON the model should see as the tool result.
func mcpExecuteTool(httpc *http.Client, callbackURL, callbackToken string, allowed map[string]bool, toolName string, arguments json.RawMessage) string {
	toolName = strings.TrimSpace(toolName)
	if toolName == "" {
		return `{"success":false,"error":"tool_name is required"}`
	}
	if len(allowed) > 0 && !allowed[toolName] {
		return `{"success":false,"error":"tool '` + jsonEscape(toolName) + `' is not available to this agent"}`
	}
	if callbackURL == "" || callbackToken == "" {
		return `{"success":false,"error":"tool callback not configured"}`
	}

	if len(bytes.TrimSpace(arguments)) == 0 {
		arguments = json.RawMessage(`{}`)
	}
	payload, _ := json.Marshal(map[string]any{"tool_name": toolName, "arguments": json.RawMessage(arguments)})

	req, err := http.NewRequest(http.MethodPost, callbackURL, bytes.NewReader(payload))
	if err != nil {
		return `{"success":false,"error":"bad callback request"}`
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+callbackToken)

	resp, err := httpc.Do(req)
	if err != nil {
		return `{"success":false,"error":"tool callback failed: ` + jsonEscape(err.Error()) + `"}`
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	body = bytes.TrimSpace(body)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// Surface a structured failure the model can react to (don't crash the loop).
		detail := jsonEscape(truncateString(string(body), 400))
		return `{"success":false,"error":"tool callback HTTP ` + strconv.Itoa(resp.StatusCode) + `: ` + detail + `"}`
	}
	if len(body) == 0 {
		return `{"success":true}`
	}
	// Pass the callback's JSON straight through as the tool result.
	if json.Valid(body) {
		return string(body)
	}
	return `{"success":true,"data":"` + jsonEscape(string(body)) + `"}`
}

// jsonEscape escapes a string for safe inclusion inside a hand-built JSON string literal.
func jsonEscape(s string) string {
	b, _ := json.Marshal(s)
	// json.Marshal wraps in quotes; strip them.
	if len(b) >= 2 {
		return string(b[1 : len(b)-1])
	}
	return s
}
