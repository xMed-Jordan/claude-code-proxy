# Claude Code Codex Proxy

Local cross-platform proxy for Windows, Linux, and macOS that exposes Anthropic-compatible endpoints for Claude Code and forwards requests to Codex/ChatGPT subscription auth.

It also includes a local control panel at `http://127.0.0.1:4000/`.
On first use, the control panel requires creating a local admin login. The password is stored in `.env` as `ADMIN_PASSWORD_HASH`, not as plaintext.

Claude Code's `WebSearch` tool is translated to Codex Responses web search, and Claude Code `/fast` requests are translated to a supported Codex model with `service_tier=priority`.
Codex reasoning summaries are returned as Anthropic `thinking` blocks so Claude Code can place them in its thinking UI separately from the final answer.
Claude Code sessions are isolated by mapping `X-Claude-Code-Session-Id` to a local Codex `prompt_cache_key`; subagents, `/btw`, and compact-style one-shot requests get separate child keys.
Claude Code browser automation is exposed as a separate MCP sidecar named `antigravity-browser`, powered by the proxy's built-in Chrome DevTools Protocol bridge with a visible in-page cursor overlay.

## Endpoints

- `GET /health`
- `GET /docs`
- `GET /openapi.json`
- `GET /postman.json`
- `GET /anthropic/v1/models`
- `POST /anthropic/v1/messages`
- `POST /anthropic/v1/messages/count_tokens`
- `GET /openai/v1/models`
- `POST /openai/v1/chat/completions`
- `POST /openai/v1/responses`
- `/openai/v1/files...` provider pass-through for OpenAI-compatible file upload/list/retrieve/delete calls
- `GET /ui/api/antigravity`
- `GET /antigravity/bridge`

The old root `/v1/...` API paths are intentionally not registered. Claude Code should use `ANTHROPIC_BASE_URL=http://127.0.0.1:4000/anthropic`; OpenAI-compatible clients should use `http://127.0.0.1:4000/openai/v1`.

## API Documentation

The proxy serves public local documentation with placeholder values only:

- Browser docs: `http://127.0.0.1:4000/docs`
- OpenAPI import: `http://127.0.0.1:4000/openapi.json`
- Postman collection: `http://127.0.0.1:4000/postman.json`

Postman variables include `baseUrl`, `proxyApiKey`, `anthropicVersion`, `model`, `adminUsername`, `adminPassword`, `adminSessionCookie`, and provider/client-key placeholders. Proxy endpoints accept `x-api-key`, `anthropic-api-key`, `api-key`, or `Authorization: Bearer <key>`. Admin `/ui/api/...` endpoints use the `ccp_admin_session` cookie set by `/ui/api/auth/setup` or `/ui/api/auth/login`.

## Quick Start

1. Copy `.env.example` to `.env`.
2. Update `PROXY_API_KEY` and any model mappings. `CODEX_AUTH_FILE` defaults to `~/.codex/auth.json`.
3. Start the proxy:

```sh
go run . start
```

4. Start Claude Code through the proxy:

```sh
go run . launch-claude
```

After the first start, the built binary lives at `bin/claude-code-proxy.exe` on Windows and `bin/claude-code-proxy` on Linux/macOS. Existing PowerShell scripts remain as Windows compatibility wrappers.

Useful Go commands:

- `go run . stop`
- `go run . restart`
- `go run . sync apply` / `go run . sync restore`
- `go run . browser-status`
- `go run . browser-start --dry-run`
- `go run . build --all`
- `go run . install-startup` / `go run . uninstall-startup`

## Claude Code and Desktop Settings

Starting the proxy applies local Claude Code settings:

- `ANTHROPIC_BASE_URL=http://127.0.0.1:4000/anthropic`
- `ANTHROPIC_AUTH_TOKEN=<PROXY_API_KEY>`
- `API_TIMEOUT_MS=3000000`
- `CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1`
- MCP server `antigravity-browser` in `~/.claude.json`, launched by the Go binary with `browser-mcp`
- MCP server `antigravity-browser` in Claude Desktop config on Windows/macOS, plus best-effort Linux config updates only when an existing Linux Desktop config file is present
- Claude Code isolation hooks for subagent worktrees and child Codex sessions
- A proxy-managed `~/.claude/CLAUDE.md` memory block that tells Claude Code to use `antigravity-browser` for browser navigation, clicking, typing, page reads, and screenshots

Claude Desktop's **Customize > Connectors** screen is for remote/web connectors and can be empty even when local MCP servers are configured. Local Desktop MCP servers are managed through `claude_desktop_config.json`; these tools appear inside chats from the `+` / connectors tool picker after Claude Desktop reloads the config. Official Desktop integration targets Windows and macOS; Linux Desktop config support is best-effort.

To manage the browser bridge from Claude Desktop's **Settings > Extensions** screen, build a local Desktop Extension package:

```sh
go run . package-extension
```

Then use **Install Extension** with `dist/claude-code-proxy-browser.mcpb` or `dist/claude-code-proxy-browser.dxt`, or **Install Unpacked Extension** with `dist/claude-code-proxy-browser`.

## API Keys

The control panel includes an API Keys page for:

- Provider keys: OpenAI or Google AI Studio/Gemini keys, saved encrypted in the local SQLite database at `PROXY_DB_PATH`. Saved provider keys can be renewed from the Provider keys table without changing the local client keys that route to them.
- Provider schema: provider keys use the OpenAI-compatible schema. Google AI Studio defaults to `https://generativelanguage.googleapis.com/v1beta/openai`.
- Client keys: local proxy keys for Claude Code or OpenAI-compatible clients. A client key can use the default proxy route or route directly through a saved OpenAI/Gemini provider key.
- Client key schema scope: choose `both`, `anthropic`, or `openai` to restrict which public namespace a local client key can call.

Client keys are shown only once when created. Provider keys are only shown as masked previews after saving.

## OpenAI/Gemini Compatibility

The local OpenAI-compatible path is `/openai/v1`. For Gemini through Google AI Studio, create a Gemini provider key in the dashboard and route a client key to that provider. The proxy forwards OpenAI-compatible Chat Completions requests, including `reasoning_effort` and `extra_body` for Gemini-specific thinking configuration.

Image content parts are preserved as `image_url` parts. Anthropic image blocks are converted to OpenAI-compatible `image_url` parts for provider routing. File/document blocks are preserved as OpenAI-compatible `file` parts when they contain `file_id` or base64 `file_data`; text files become text parts, and URL-only non-image files are included as text pointers because the OpenAI-compatible Chat Completions file URL shape is not universal.

Custom session IDs can be supplied with `X-Proxy-Session-Id`, `X-Codex-Session-Id`, `X-OpenAI-Session-Id`, `X-Claude-Code-Session-Id`, `X-Claude-Session-Id`, or `X-Session-Id`. OpenAI-compatible Chat Completions can also use `user` or `metadata.session_id` / `metadata.conversation_id` / `metadata.thread_id`. On Codex routes these become stable local `prompt_cache_key` sessions; on provider routes a session header fills the OpenAI `user` field when the request did not already set it.

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

Before applying settings, the proxy creates snapshots of the current Claude Code settings, Claude Code root MCP config, Claude Desktop MCP config, and user memory file. Existing snapshots are preserved so a reboot while proxy mode is active does not overwrite the original settings. Stopping the proxy restores the snapshots.

## Antigravity Browser Bridge

The Antigravity browser bridge is intentionally separate from `/anthropic/v1/messages`. Claude Code and Claude Desktop discover it through MCP, then use browser tools for navigation, screenshots, page snapshots, console checks, visible cursor movement, clicks, typing, key presses, and waits.

The browser bridge validates:

- Google Chrome is installed.
- Antigravity extension `eeijfnjmjelapkebgockoeaadonbchdd` is installed in Chrome.
- The platform binary exists and can run `browser-mcp`.

Set `ANTIGRAVITY_BROWSER_MODE=default` to prefer an already-running Chrome DevTools endpoint. Modern Chrome blocks `--remote-debugging-port` and `--remote-debugging-pipe` on the normal Default data directory, so if Chrome is not already exposing DevTools the bridge opens a visible controlled profile instead. This is why Claude Code browser actions can appear in a separate Chrome window even when the Antigravity extension is installed in your regular browser.

Set `ANTIGRAVITY_BROWSER_FORCE_DEFAULT_CDP=1` only for older or custom Chrome builds where Default-profile DevTools still works. In that forced mode, `ANTIGRAVITY_BROWSER_SAFE_DEFAULT_RELAUNCH=1` allows the bridge to close only background/proxy Chrome windows before trying to relaunch Default Chrome with DevTools. It will not close normal visible user tabs just to take control.

Set `ANTIGRAVITY_BROWSER_MODE=dedicated` to use an isolated profile under `.antigravity-browser-profile`. Claude Code launches the stdio MCP process itself; proxy startup normally only prepares the settings and does not open Chrome.

Normal proxy start, dashboard checks, tests, and the `browser_status` MCP tool are passive and do not open Chrome. Set `ANTIGRAVITY_BROWSER_PRELAUNCH_WITH_PROXY=1` only if you explicitly want proxy startup to open the controlled browser ahead of the first Claude Code browser action. `browser_screenshot` saves PNG files under `Claude Code Screenshots` in the directory where Claude Code was launched and returns the local path by default; set `ANTIGRAVITY_SCREENSHOT_DIR` to override that folder. Inline image data is opt-in with `include_image=true`.

Optional overrides:

- `ANTIGRAVITY_CHROME_PATH`
- `ANTIGRAVITY_EXTENSION_PATH`
- `ANTIGRAVITY_BROWSER_MODE`
- `ANTIGRAVITY_BROWSER_PROFILE`
- `ANTIGRAVITY_BROWSER_PRELAUNCH_WITH_PROXY`
- `ANTIGRAVITY_BROWSER_DEBUG_PORT`
- `ANTIGRAVITY_SCREENSHOT_DIR`
- `ANTIGRAVITY_BROWSER_FORCE_DEFAULT_CDP`
- `ANTIGRAVITY_BROWSER_SAFE_DEFAULT_RELAUNCH`

Open `http://127.0.0.1:4000/antigravity/bridge` from Chrome to run a safe extension wake/connection probe. The probe records only local status fields; it does not log tokens or browsing history. Real browser-control activity is reported in the dashboard from `.antigravity-browser-state.json`, which stores only current URL/title, last action, and last error.

## Safety

Do not commit `.env`, Claude settings snapshots, logs, PID files, or built binaries. They are ignored by `.gitignore`.

Codex auth is read from the local `CODEX_AUTH_FILE`. Tokens are never written to logs; diagnostics use masked fingerprints only.

## Validation

Run:

```sh
go run . validate
```

For fast checks, prefer:

```sh
curl http://127.0.0.1:4000/health
```
