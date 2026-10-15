# Muse (Meta) upstream — how it works and how to go back

Since v0.39.0, a model alias with **Forwarded to = Muse** is served by calling
**Meta's Model API directly over HTTPS** (`api.meta.ai`). The old way — running
the **Muse Code CLI** (`muse exec`) as a subprocess for every turn — is still in
the code and can be switched back on in one line.

| | `api` transport (default since v0.39.0) | `cli` transport (the old way) |
|---|---|---|
| Who calls Meta | the proxy, over HTTPS | a `muse exec` subprocess per turn |
| Code | `muse_api.go` | `muse.go` |
| Connect tools | native tools; the proxy runs the loop and calls Connect's callback | MCP gateway (`claude-mcp-gateway`) inside the CLI |
| Streaming | real (Meta's SSE relayed) | real (CLI JSONL deltas) |
| Muse Code process | only the key guard: every 6h, and when Meta rejects the key | every customer turn |

## Credentials (the proxy's own, never the caller's)

1. `PROXY_MUSE_API_KEY` in the proxy `.env`, if set.
2. Otherwise `providers.meta.api_key` from the service user's `muse login` file:
   `/root/.config/muse/auth.json` on ai-api1 (`PROXY_MUSE_AUTH_PATH` overrides).
   The file is re-read whenever it changes.

**Key guard.** Every `PROXY_MUSE_KEY_CHECK_INTERVAL` (default 6h), and whenever
Meta answers 401/403, the proxy runs one tiny Muse Code turn, re-reads
`auth.json`, compares the key fingerprint before and after, and adopts a new key
if Muse Code changed it. Journal lines:

```
[muse-key] reason=periodic cli=ok before=<fp> after=<fp> changed=false models=200
[muse-key] KEY INVALID — run `muse login` as the proxy's service user (...)
```

`KEY INVALID` means a human has to log in again: `ssh root@ai-api1.cus.cx`, then
`muse login` (browser device code) as root.

## Go back to the old Muse Code CLI method (rollback)

Nothing in Connect has to change — both transports take the same request and
the same `connect_tools` + `X-Connect-Callback-*` handoff.

```bash
ssh root@ai-api1.cus.cx
cp /opt/connect-ai-proxy/.env /opt/connect-ai-proxy/.env.bak-$(date +%F-%H%M)
grep -q '^PROXY_MUSE_TRANSPORT=' /opt/connect-ai-proxy/.env \
  && sed -i 's/^PROXY_MUSE_TRANSPORT=.*/PROXY_MUSE_TRANSPORT=cli/' /opt/connect-ai-proxy/.env \
  || echo 'PROXY_MUSE_TRANSPORT=cli' >> /opt/connect-ai-proxy/.env
systemctl restart connect-ai-proxy
journalctl -u connect-ai-proxy -n 50 --no-pager | grep '\[muse\]'   # → "transport cli"
```

To return to the API: set `PROXY_MUSE_TRANSPORT=api` (or delete the line) and
restart. The CLI path needs `/usr/local/bin/muse` and root's `muse login`; both
stay installed because the key guard uses them too.

**Roll back the whole binary** (only if the new code itself misbehaves):
redeploy the previous release, v0.38.0 (`a616c06`). From a local checkout:

```bash
git checkout a616c06 -b rollback-muse   # or revert the v0.39.0 commit on main and push
# then deploy with update-linux.sh on ai-api1 (see "Deploying" below)
```

## Connect side (for reference — no change needed for either transport)

Connect hands the tool loop to the proxy when **all** of these hold
(`app/Services/Agents/AutonomousAgentProcessor.php`, around `$proxyMuseLoop`):

- the agent's AI service provider is `connect_proxy`,
- its model starts with `muse` (e.g. `muse-spark-1.3-contributor`),
- `PROXY_MANAGED_TOOL_LOOP_MUSE=true` in Connect's `.env` (`config('ai.proxy_managed_tool_loop_muse')`).

The proxy side needs `PROXY_MUSE_TOOLS_ENABLED=1`. Turn either one off and Connect
stops sending `connect_tools`, so the model answers without tools.

## Editing and deploying

**Proxy** (`C:\Users\hrash\Documents\claude-code-proxy`, GitHub `xMed-Jordan/claude-code-proxy`):
edit locally → `go vet ./... && go test ./...` → bump `VERSION` → commit → `git push` →
on ai-api1: `cd /root/claude-code-proxy && ./update-linux.sh -y --no-system-upgrade --skip-go-latest`
(it keeps the existing `.env`, but copy `/opt/connect-ai-proxy/.env` to a dated backup first). The running binary
is `/usr/local/bin/connect-ai-proxy` → `/opt/connect-ai-proxy/connect-ai-proxy`.

**Connect** (`C:\Users\hrash\Herd\connect`, GitHub `xMed-Jordan/connect`): never edit
files on connect2 directly. Edit locally → commit → `git push origin main`; Plesk
auto-deploys into `httpdocs` within seconds. Then, as the Plesk user (never root):
`ssh connect2.xmed.cloud_kswlqt0n5xb@plesk27.xpear.cloud`, `cd httpdocs`, and run
`/opt/plesk/php/8.4/bin/php artisan optimize:clear && ... config:cache && ... horizon:terminate`
(plus `migrate --force` if there is a migration). A `.env` flag change needs `config:cache`
and `horizon:terminate` too.

## Settings (`/opt/connect-ai-proxy/.env`)

| Variable | Default | Meaning |
|---|---|---|
| `PROXY_MUSE_TRANSPORT` | `api` | `api` or `cli` |
| `PROXY_MUSE_API_KEY` | — | proxy-owned key; when set, the login file and key guard are not used |
| `PROXY_MUSE_AUTH_PATH` | `~/.config/muse/auth.json` | the `muse login` file to read |
| `PROXY_MUSE_API_BASE` | `api_base_url` in auth.json, else `https://api.meta.ai/v1` | Meta endpoint |
| `PROXY_MUSE_MIN_MAX_TOKENS` | `16384` | floor for `max_tokens` (Meta counts thinking against it; Connect sends 4096) |
| `PROXY_MUSE_KEY_CHECK_INTERVAL` | `6h` | how often the key guard opens Muse Code (`0` = only on 401/403) |
| `PROXY_MUSE_TOOLS_ENABLED` | `0` | run Connect's tool loop in the proxy |
| `PROXY_MUSE_TOOL_MAX_TURNS` | `20` | tool turns before one forced text answer |
| `PROXY_MUSE_TIMEOUT` | `180` | seconds per Meta call (api) / per CLI attempt (cli) |
| `PROXY_MUSE_TOTAL_TIMEOUT` | `480` | whole turn incl. retries; keep below Connect's 500s |
| `PROXY_MUSE_RETRIES` | `3` | retries on 429/5xx/529/network errors (a tool is never re-run) |

Tool-loop safety (api transport): a call the model repeats with the same arguments
after it already succeeded in the same turn is NOT run again (the model is told
`duplicate_call`); a turn made only of such repeats ends with a forced text answer.
A failed call may be retried. If Meta cannot download a URL attachment
(`failed to download media`), the request is retried once with that attachment
replaced by a short note, so a dead media link never costs the customer the reply.

Prompt caching (api transport, v0.39.3, `PROXY_MUSE_PROMPT_CACHE`, default on): Meta
caches nothing unless the request carries `cache_control` (verified 2026-10-15: an
identical 29k-token prompt sent twice read 0 cached tokens; with a breakpoint the repeat
read 29,297). The proxy adds `{"type":"ephemeral"}` breakpoints on the last tool, the
system prompt and the newest user message (within the 4-breakpoint limit, after any the
caller set), on a copy of the body, so in a tool loop every call reuses the previous
call's whole prefix. Caching does not change what the model sees. A 400 on a request
carrying the proxy's breakpoints is retried once without them, so caching can never cost
a reply; set `PROXY_MUSE_PROMPT_CACHE=0` to switch it off.

### Reading the logs (`journalctl -u connect-ai-proxy -o cat`)

Every Connect tool-loop turn (one customer message) writes, in order:

```
[muse-call] conv=<connect conversation uuid> turn=1 ms=8123 in=56012 cache_read=0 cache_write=0 out=702 stop=tool_use tool_uses=1 text_chars=0 final_forced=false
[muse-tool] conv=… turn=1 name=get_tool_instructions ms=310 ok=true bytes=2211
[muse-call] conv=… turn=2 ms=5410 in=56980 cache_read=55990 … stop=end_turn tool_uses=0 text_chars=184 final_forced=false
[muse-api] model=muse-spark-1.3-contributor tools=67 turns=2 tool_calls=1 repeats_skipped=0 ok=true ms=13900 meta_ms=13533 tool_ms=310 in=112992 cache_read=55990 cache_write=0 out=1311 effort=xhigh msgs=50 conv=… seq=get_tool_instructions
```

- `[muse-call]` — one Meta call: its latency, `in` = all input tokens (cached included),
  `cache_read` = how many of them came from the cache, `out` = output incl. thinking,
  the stop reason, how many tools it asked for, and whether it was the forced final
  (tool_choice none) call. `ms=… err=…` instead when the call failed.
- `[muse-tool]` — one Connect callback: tool name, latency, Connect's `success` flag and
  result size; `dup=true` when a repeated successful call was answered without running.
- `[muse-api] … ok=` — the turn summary. The first six fields keep their pre-v0.39.3
  order. `ms` = whole turn, `meta_ms` / `tool_ms` = time waiting on Meta / Connect,
  `msgs` = history messages Connect sent, `seq` = tool names in call order.
- `[muse-api] … passthrough` — a request without Connect's tool loop (status, ms, tokens).
- `[muse-api] … history repaired: …` — orphan tool pairs fixed before calling Meta.

No arguments, results or message text are logged — names, counts and timings only.
`conv=` is Connect's conversation uuid (the `X-Connect-Conversation` header): grep it to
follow one conversation, or join it to Connect's `agent_conversations.uuid`.

History repair (api transport, v0.39.2): Meta rejects the whole request when a
`tool_result` does not answer a `tool_use` in the assistant message right before it
(`No function call found for function call output with call_id …`) or a `tool_use`
is left unanswered. Connect's history can contain both (old tool rows replayed
without their call; a history window that starts mid-pair). Before calling Meta the
proxy turns each orphan `tool_result` into a text block
(`[result of an earlier tool call]` + its text) and drops each unanswered `tool_use`;
well-formed history is sent byte for byte. Each repair logs
`[muse-api] model=… history repaired: orphan_tool_results=N unanswered_tool_uses=M`.

What the api transport sends to Meta: the caller's body with only the fields Meta
accepts (`model, messages, system, max_tokens, metadata, stream, temperature, top_p,
top_k, tools, tool_choice, thinking, service_tier, output_config.effort`). Dropped
fields (`connect_tools`, `speed`, `stop_sequences`, `output_config.reasoning_summary`,
`thinking.display`) are listed in the trace log as `muse_api.dropped`.
