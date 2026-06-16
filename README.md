# Connect AI Proxy

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
- `GET /anthropic/v1/model-capabilities`
- `POST /anthropic/v1/messages`
- `POST /anthropic/v1/messages/count_tokens`
- `GET /openai/v1/models`
- `GET /openai/v1/model-capabilities`
- `POST /openai/v1/chat/completions`
- `POST /openai/v1/responses`
- `/openai/v1/files...` provider pass-through for OpenAI-compatible file upload/list/retrieve/delete calls
- `GET /ui/api/antigravity`
- `GET /ui/api/update/status`
- `POST /ui/api/update/check`
- `POST /ui/api/update/start`
- `POST /ui/api/update/settings`
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

After the first start, the built binary lives at `bin/connect-ai-proxy.exe` on Windows and `bin/connect-ai-proxy` on Linux/macOS. Existing PowerShell scripts remain as Windows compatibility wrappers.

Useful Go commands:

- `go run . stop`
- `go run . restart`
- `go run . sync apply` / `go run . sync restore`
- `go run . browser-status`
- `go run . browser-start --dry-run`
- `go run . build --all`
- `go run . install-startup` / `go run . uninstall-startup`

## Linux Install Script

On Linux, the installer prepares the machine, builds the proxy, registers `connect-ai-proxy.service`, and starts it through `systemd`:

```sh
chmod +x install-linux.sh
./install-linux.sh
```

To preview the work without changing the system:

```sh
./install-linux.sh --dry-run
```

The installer supports `apt`, `dnf`, `yum`, `zypper`, `pacman`, and `apk`. It checks `sudo`, installs missing base packages, verifies Node.js 18+ with npm, optionally installs Claude Code with `npm i -g @anthropic-ai/claude-code` (it asks first; choosing yes also runs `claude setup-token` to enable the `claude` upstream), installs or updates Go from the official Go release metadata, installs Codex with `npm i -g @openai/codex`, and pauses for Codex sign-in if `~/.codex/auth.json` is not ready.

During install it asks whether this is a local workstation or a server. Server installs skip Chrome/Antigravity browser tools by default. Browser tools are only installed/configured if you approve them; if skipped, the installer sets `ANTIGRAVITY_BROWSER_ENABLED=0` so Claude Code browser MCP setup is not injected.

For a static HTTPS domain, run the installer normally and answer yes when prompted, or pass the values up front:

```sh
./install-linux.sh --server --https --domain proxy.example.com --email admin@example.com --confirm-dns
```

HTTPS mode keeps the Go proxy bound to `127.0.0.1`, installs/configures Caddy, uses HTTP only for ACME validation and redirect, and exposes the public endpoint through HTTPS. Defaults are internal proxy port `4000`, public HTTP port `80`, and public HTTPS port `443`; the installer asks before changing them. DNS must already point to the server before certificate setup. The installer writes `PROXY_PUBLIC_URL` so the dashboard, docs, OpenAPI, and Postman exports show the public HTTPS domain; standard ports `80` and `443` are omitted from that displayed URL.

Service commands:

```sh
sudo systemctl start connect-ai-proxy
sudo systemctl stop connect-ai-proxy
sudo systemctl reload connect-ai-proxy
sudo systemctl restart connect-ai-proxy
sudo systemctl status connect-ai-proxy
sudo systemctl enable --now connect-ai-proxy
sudo systemctl disable --now connect-ai-proxy
```

Installer helper actions:

```sh
./install-linux.sh status
./install-linux.sh restart
./install-linux.sh disable
./install-linux.sh uninstall
```

Maintenance scripts:

```sh
chmod +x install-linux.sh reinstall-linux.sh uninstall-linux.sh update-linux.sh
./update-linux.sh
./reinstall-linux.sh --server --https --domain proxy.example.com --email admin@example.com --confirm-dns
./uninstall-linux.sh --app-only
./uninstall-linux.sh --clean
```

- `update-linux.sh` updates OS packages, pulls the latest code from `https://github.com/xMed-Jordan/claude-code-proxy.git`, updates Go from official Go metadata, refreshes whichever of the Codex and Claude Code npm CLIs are already installed (it never adds one you skipped), rebuilds, reinstalls, and restarts the service. It preserves the installed public URL when `/opt/connect-ai-proxy/.env` or Caddy already points at the proxy.
- `reinstall-linux.sh` stops the service, restores synced Claude settings if possible, removes the service, symlink, and install directory, then runs a fresh install. It keeps Codex auth by default so a reinstall does not force a new device login unless `--remove-codex-auth` is passed.
- `uninstall-linux.sh --app-only` removes only the app service, symlink, and install directory. `--clean` also checks Caddy, Go, Node/npm, Codex/Claude CLI packages, and browser packages; it skips anything that appears to be used by another process, systemd unit, or project on the server.

The installed version is read from `VERSION` and shown in the dashboard. The dashboard Updates page can check the configured `VERSION` URL, edit the update branch, repository path, status-file path, auto-update toggle, and whether manual dashboard updates run the full system updater. It can start the non-interactive Linux updater, show progress while the service restarts, and reconnect when `/health` returns again. Automatic updates run an app-scoped update without OS package upgrades; manual dashboard updates use the full updater unless `PROXY_DASHBOARD_FULL_SYSTEM_UPDATE=0`.

Troubleshooting notes:

- If `sudo` fails, run as a sudo-capable user or as root.
- If the distro package manager provides an older Node.js, install Node.js 18+ from your distro or Node.js docs and rerun the installer.
- If Claude Code is missing, the installer installs it; if `claude` is still not on `PATH`, fix the global npm bin path and rerun the installer.
- If Codex login is incomplete, run `codex` as the installing user and finish sign-in, then rerun the installer.
- If HTTPS fails, confirm DNS A/AAAA records point to the server and inbound ports `80` and `443` are open.
- If reinstalling over a running service ever reports `Text file busy`, pull the latest repository and rerun the installer or `./update-linux.sh`; new installs replace the running binary through an atomic file move.
- If the dashboard shows `127.0.0.1` on a server install, set `PROXY_PUBLIC_URL=https://your-domain` in `/opt/connect-ai-proxy/.env` and restart `connect-ai-proxy`.
- If browser tools are enabled later, rerun with `--browser-tools`; install the Antigravity Chrome extension in Chrome/Chromium before using browser MCP actions.
- Caddy status and reload are managed with `sudo systemctl status caddy` and `sudo systemctl reload caddy`.

Installer sources: [Go install docs](https://go.dev/doc/install), [Go downloads JSON](https://go.dev/dl/?mode=json), [Node.js downloads](https://nodejs.org/en/download), [Claude Code setup docs](https://docs.anthropic.com/en/docs/claude-code/setup), [Codex CLI docs](https://developers.openai.com/codex/cli), [Caddy install docs](https://caddyserver.com/docs/install), and [Caddy automatic HTTPS](https://caddyserver.com/docs/automatic-https).

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

Set `ANTIGRAVITY_BROWSER_ENABLED=0` to keep browser MCP tools out of Claude Code and Desktop settings. This is the Linux installer default for server installs unless browser tools are explicitly enabled.

Claude Desktop's **Customize > Connectors** screen is for remote/web connectors and can be empty even when local MCP servers are configured. Local Desktop MCP servers are managed through `claude_desktop_config.json`; these tools appear inside chats from the `+` / connectors tool picker after Claude Desktop reloads the config. Official Desktop integration targets Windows and macOS; Linux Desktop config support is best-effort.

To manage the browser bridge from Claude Desktop's **Settings > Extensions** screen, build a local Desktop Extension package:

```sh
go run . package-extension
```

Then use **Install Extension** with `dist/connect-ai-proxy-browser.mcpb` or `dist/connect-ai-proxy-browser.dxt`, or **Install Unpacked Extension** with `dist/connect-ai-proxy-browser`.

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

Claude Code compacts based on the model context window it sees. For Codex/ChatGPT routing, the proxy advertises a `200000` token active context window on `/model-capabilities`, even for aliases whose names include `[1m]`. That makes Claude Code compact before hidden tool history, transformed request JSON, and subagent payloads hit the live backend. The proxy no longer returns assistant-message recovery text. If the upstream still returns `context_length_exceeded`, the proxy forwards it as an API error instead of turning it into a model reply.

Tool calls are also mirrored as short synthetic `thinking` blocks so Claude Code can show the requested tool name and arguments in its thinking UI. Sensitive-looking argument fields such as tokens, passwords, cookies, and API keys are redacted.

When Claude Code launches a subagent with `isolation: "worktree"`, the proxy settings add a WorktreeCreate fallback. In git repositories it creates a real git worktree under `.claude-worktrees`; outside git repositories it creates a temporary empty isolated workspace so read-only agents can still launch. A SubagentStart hook injects a local routing marker so the proxy gives that child agent its own stable Codex session key and token accounting. A SubagentStop hook also blocks empty or incomplete subagent final messages so the main agent receives a usable handoff summary.

Before applying settings, the proxy creates snapshots of the current Claude Code settings, Claude Code root MCP config, Claude Desktop MCP config, and user memory file. Existing snapshots are preserved so a reboot while proxy mode is active does not overwrite the original settings. Stopping the proxy restores the snapshots.

## Model Selection & Available Models

The proxy dynamically routes request payloads to the correct upstream backend (Codex/ChatGPT or Antigravity/Gemini) by inspecting the model name or configured model alias.

### How to Select Models
- **Route to Antigravity (Gemini)**: If the resolved model name contains `"antigravity"` or `"banana"`, the request is routed to the Google Antigravity sidecar.
- **Route to Codex (ChatGPT)**: If the resolved model name contains `"codex"` or `"gpt-5"`, the request is routed to Codex.
- **Global Default**: If the model name doesn't match these keywords, the request routes to the global default upstream specified by `UPSTREAM` in the `.env` file (which can be `codex`, `openai`, or `antigravity`).
- **Model Aliases**: You can configure custom aliases in the local control panel at `http://127.0.0.1:4000/` or by updating the `PROXY_MODEL_ALIASES` variable in your `.env` file.
- **Per-alias "Forwarded to"**: A model alias can be routed to a non-default backend via its `forward_to` setting (Models page or `PROXY_MODEL_ALIASES`): `agy` serves it from the local Antigravity CLI, and `claude` serves it from the local Claude Code CLI backed by a Claude subscription.

### Available Upstreams & Models

#### 1. Codex (ChatGPT Upstream)
Supports standard Codex/ChatGPT models.

#### 2. Google Antigravity (Gemini Upstream)
Supports direct Gemini models (e.g., `gemini-2.5-flash`, `gemini-2.5-pro`).
- **Nano Banana (Image Generation & Multimodal Editing)**:
  - `nano-banana` &rarr; maps to `gemini-2.5-flash-image` (Nano Banana)
  - `nano-banana-2` &rarr; maps to `gemini-3.1-flash-image-preview` (Nano Banana 2)
  - `nano-banana-pro` &rarr; maps to `gemini-3-pro-image-preview` (Nano Banana Pro)
- **Image Inputs**: Multimodal chat requests containing base64 images (in OpenAI's `image_url` / `input_image` or Anthropic's `image` format) are automatically translated to Gemini's native `inlineData` structure.
- **Thinking / Reasoning Level**: Sets the Gemini reasoning depth (`thinkingLevel` or `thinkingBudget`) dynamically. The client can request the level via payload `reasoning_effort` or environment variables `CLAUDE_CODE_EFFORT_LEVEL`/`OPENAI_REASONING_EFFORT`:
  - `low` / `minimal` &rarr; Maps to minimal reasoning budget.
  - `medium` &rarr; Maps to medium reasoning budget.
  - `high` / `xhigh` / `max` &rarr; Maps to maximum reasoning budget (dynamic thinking).

#### 3. Claude (Anthropic subscription Upstream)
Set a model alias's "Forwarded to" to `Claude` to serve it from the local Claude Code CLI (`claude -p`) backed by a Claude Max/Pro subscription, instead of Codex/OpenAI. Authenticate once with `claude setup-token` and set `PROXY_CLAUDE_OAUTH_TOKEN` (the installer offers to do this). Chat-only: built-in tools are disabled and the run is single-turn and stateless, so caller-defined tools are not honored (the CLI uses its own). Responses stream token-by-token. **Images and PDFs** in a request are forwarded to the model natively (inline via stream-json); audio/video are dropped (use agy for those). See the `PROXY_CLAUDE_*` variables in `.env.example`.

## Live Server + Local Browser Mode

Use this split when a VPS hosts the AI proxy and your local workstation should provide only the visible browser tools:

- On the VPS, install with server defaults and browser tools disabled. The proxy should listen on `127.0.0.1:4000`, while Caddy owns public `80/443` and forwards the HTTPS domain to the local proxy.
- On Windows, run `.\install-browser-startup.ps1`. It removes old full-proxy startup tasks such as `ConnectAIProxy` and `ClaudeCodeCodexProxy`, then applies only browser MCP settings and hooks. It does not rewrite `ANTHROPIC_BASE_URL`, so Claude Code can keep using the live endpoint such as `https://ai-api1.cus.cx/anthropic`.
- To remove the local browser MCP wiring later, run `.\uninstall-browser-startup.ps1`.

Equivalent CLI actions are `connect-ai-proxy sync browser-apply` and `connect-ai-proxy sync browser-restore`. Browser-only sync never starts the local proxy and never changes Claude Code API endpoint environment variables.

## Antigravity Browser Bridge

The Antigravity browser bridge is intentionally separate from `/anthropic/v1/messages`. Claude Code and Claude Desktop discover it through MCP, then use browser tools for navigation, screenshots, page snapshots, console checks, visible cursor movement, clicks, typing, key presses, and waits.

The browser bridge validates:

- Google Chrome is installed.
- Antigravity extension `eeijfnjmjelapkebgockoeaadonbchdd` is installed in Chrome.
- The platform binary exists and can run `browser-mcp`.

Set `ANTIGRAVITY_BROWSER_MODE=default` to prefer an already-running Chrome DevTools endpoint. Modern Chrome blocks `--remote-debugging-port` and `--remote-debugging-pipe` on the normal Default data directory, so if Chrome is not already exposing DevTools the bridge opens a visible controlled profile instead. This is why Claude Code browser actions can appear in a separate Chrome window even when the Antigravity extension is installed in your regular browser.

Set `ANTIGRAVITY_BROWSER_FORCE_DEFAULT_CDP=1` only for older or custom Chrome builds where Default-profile DevTools still works. In that forced mode, `ANTIGRAVITY_BROWSER_SAFE_DEFAULT_RELAUNCH=1` allows the bridge to close only background/proxy Chrome windows before trying to relaunch Default Chrome with DevTools. It will not close normal visible user tabs just to take control.

Set `ANTIGRAVITY_BROWSER_MODE=dedicated` to use an isolated profile under `.antigravity-browser-profile`. Claude Code launches the stdio MCP process itself; proxy startup normally only prepares the settings and does not open Chrome. This is the recommended local-browser mode when the AI proxy itself runs on a live server.

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
