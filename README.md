# Claude Code Codex Proxy

Local Windows proxy that exposes Anthropic-compatible endpoints for Claude Code and forwards requests to Codex/ChatGPT subscription auth.

It also includes a local control panel at `http://127.0.0.1:4000/`.

Claude Code's `WebSearch` tool is translated to Codex Responses web search, and Claude Code `/fast` requests are translated to a supported Codex model with `service_tier=priority`.
Codex reasoning summaries are returned as Anthropic `thinking` blocks so Claude Code can place them in its thinking UI separately from the final answer.
Claude Code sessions are isolated by mapping `X-Claude-Code-Session-Id` to a local Codex `prompt_cache_key`; subagents, `/btw`, and compact-style one-shot requests get separate child keys.
Claude Code browser automation is exposed as a separate MCP sidecar named `antigravity-browser`, powered by the proxy's built-in Chrome DevTools Protocol bridge with a visible in-page cursor overlay.

## Endpoints

- `GET /health`
- `GET /v1/models`
- `POST /v1/messages`
- `POST /v1/messages/count_tokens`
- `POST /v1/chat/completions`
- `POST /v1/responses`
- `GET /ui/api/antigravity`
- `GET /antigravity/bridge`

## Quick Start

1. Copy `.env.example` to `.env`.
2. Update `CODEX_AUTH_FILE`, `PROXY_API_KEY`, and any model mappings.
3. Start the proxy:

```powershell
.\start-proxy.ps1
```

4. Start Claude Code in a fresh PowerShell session:

```powershell
.\start-claude-code.ps1
```

## Claude Code Settings

Starting the proxy applies local Claude Code settings:

- `ANTHROPIC_BASE_URL=http://127.0.0.1:4000`
- `ANTHROPIC_AUTH_TOKEN=<PROXY_API_KEY>`
- `API_TIMEOUT_MS=3000000`
- `CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1`
- MCP server `antigravity-browser` in `~/.claude.json`, launched by `start-antigravity-browser-mcp.ps1`
- Claude Code isolation hooks for subagent worktrees and child Codex sessions
- A proxy-managed `~/.claude/CLAUDE.md` memory block that tells Claude Code to use `antigravity-browser` for browser navigation, clicking, typing, page reads, and screenshots

Fast mode and web search are controlled by:

- `OPENAI_CLAUDE_FAST_MODEL=gpt-5.5`
- `CODEX_FAST_SERVICE_TIER=priority`
- `CODEX_WEB_SEARCH_TOOL_TYPE=web_search`
- `CODEX_REASONING_SUMMARY=auto`
- `CODEX_SESSION_ISOLATION=1`
- `CODEX_PROMPT_CACHE_KEY=1`
- `CLAUDE_TOOL_ACTIVITY_THINKING=1`

Tool calls are also mirrored as short synthetic `thinking` blocks so Claude Code can show the requested tool name and arguments in its thinking UI. Sensitive-looking argument fields such as tokens, passwords, cookies, and API keys are redacted.

When Claude Code launches a subagent with `isolation: "worktree"`, the proxy settings add a WorktreeCreate fallback. In git repositories it creates a real git worktree under `.claude-worktrees`; outside git repositories it creates a temporary empty isolated workspace so read-only agents can still launch. A SubagentStart hook injects a local routing marker so the proxy gives that child agent its own stable Codex session key and token accounting.

Before applying settings, the proxy creates snapshots of the current Claude Code settings, root MCP config, and user memory file. Existing snapshots are preserved so a reboot while proxy mode is active does not overwrite the original settings. Stopping the proxy restores the snapshots.

## Antigravity Browser Bridge

The Antigravity browser bridge is intentionally separate from `/v1/messages`. Claude Code discovers it through MCP, then uses built-in browser tools for navigation, screenshots, page snapshots, visible cursor movement, clicks, typing, key presses, and waits.

The launcher validates:

- Google Chrome is installed.
- Antigravity extension `eeijfnjmjelapkebgockoeaadonbchdd` is installed in Chrome.
- `bin\claude-code-proxy.exe` exists and can run `--antigravity-mcp`.

Set `ANTIGRAVITY_BROWSER_MODE=default` to prefer an already-running Chrome DevTools endpoint. Modern Chrome blocks `--remote-debugging-port` and `--remote-debugging-pipe` on the normal Default data directory, so if Chrome is not already exposing DevTools the bridge opens a visible controlled profile instead. This is why Claude Code browser actions can appear in a separate Chrome window even when the Antigravity extension is installed in your regular browser.

Set `ANTIGRAVITY_BROWSER_FORCE_DEFAULT_CDP=1` only for older or custom Chrome builds where Default-profile DevTools still works. In that forced mode, `ANTIGRAVITY_BROWSER_SAFE_DEFAULT_RELAUNCH=1` allows the bridge to close only background/proxy Chrome windows before trying to relaunch Default Chrome with DevTools. It will not close normal visible user tabs just to take control.

Set `ANTIGRAVITY_BROWSER_MODE=dedicated` to use an isolated profile under `.antigravity-browser-profile`. Claude Code launches the stdio MCP process itself; proxy startup normally only prepares the settings and does not open Chrome.

Normal proxy start, dashboard checks, tests, and the `browser_status` MCP tool are passive and do not open Chrome. Set `ANTIGRAVITY_BROWSER_PRELAUNCH_WITH_PROXY=1` only if you explicitly want proxy startup to open the controlled browser ahead of the first Claude Code browser action.

Optional overrides:

- `ANTIGRAVITY_CHROME_PATH`
- `ANTIGRAVITY_EXTENSION_PATH`
- `ANTIGRAVITY_BROWSER_MODE`
- `ANTIGRAVITY_BROWSER_PROFILE`
- `ANTIGRAVITY_BROWSER_PRELAUNCH_WITH_PROXY`
- `ANTIGRAVITY_BROWSER_DEBUG_PORT`
- `ANTIGRAVITY_BROWSER_FORCE_DEFAULT_CDP`
- `ANTIGRAVITY_BROWSER_SAFE_DEFAULT_RELAUNCH`

Open `http://127.0.0.1:4000/antigravity/bridge` from Chrome to run a safe extension wake/connection probe. The probe records only local status fields; it does not log tokens or browsing history. Real browser-control activity is reported in the dashboard from `.antigravity-browser-state.json`, which stores only current URL/title, last action, and last error.

## Safety

Do not commit `.env`, Claude settings snapshots, logs, PID files, or built binaries. They are ignored by `.gitignore`.

Codex auth is read from the local `CODEX_AUTH_FILE`. Tokens are never written to logs; diagnostics use masked fingerprints only.

## Validation

Run:

```powershell
.\validate-proxy.ps1
```

For fast checks, prefer:

```powershell
Invoke-RestMethod -Uri "http://127.0.0.1:4000/health"
```
