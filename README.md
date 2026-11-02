# dspool

A local, balance-aware proxy for a pool of DeepSeek API keys. Point Claude Code, Codex CLI, pi, or any OpenAI/Anthropic-compatible client at dspool; it picks a healthy key, retries around failures, and keeps the pool in shape.

[![MIT License](https://img.shields.io/badge/license-MIT-1f5f70.svg)](LICENSE)
![Go 1.27+](https://img.shields.io/badge/Go-1.27%2B-1f5f70?logo=go&logoColor=white)

## Features

- Pools multiple DeepSeek keys with transparent failover.
- Checks `GET /user/balance` and manages **hot**, **cold**, and **invalid** pools.
- Removes revoked keys (401/403) and persists tombstones across restarts.
- Cools down keys after 429/5xx/network errors and retries on another key.
- Serves Anthropic Messages, OpenAI Chat Completions, and Responses APIs.
- Proxies DeepSeek's Search & Fetch APIs through the same pool.
- Balance-aware routing: `spread` (default), `weighted`, `highest`, `drain`, `round_robin`.
- One-command agent launch: `dspool claude`, `dspool codex`, `dspool pi`.
- Local dashboard, background daemon, single binary, no database.

## How it works

```
Claude Code ─┐
Codex CLI  ──┼──▶  dspool :8787  ──▶  api.deepseek.com
pi / SDK   ──┘         │
                       ├── /v1/messages          Anthropic passthrough
                       ├── /v1/chat/completions  OpenAI passthrough
                       ├── /v1/responses         Responses → Chat translation
                       ├── /v1/tools/search      Search & Fetch passthrough
                       ├── hot / cold / invalid pools + cooldowns
                       └── balance monitor + dashboard
```

Key lifecycle:

```
              balance OK
  probing ───────────────▶ hot ──402──▶ cold ──top-up──▶ hot
     │                     │
     └──── 401/403 ────────┴──▶ invalid (tombstoned, manually restorable)
```

- **Hot**: serving traffic. 429/5xx/network errors add a short cooldown.
- **Cold**: out of balance. Rechecked every `cold_check_interval`; also refreshed immediately when the hot pool is exhausted.
- **Invalid**: rejected by DeepSeek. Removed from routing and remembered in `state.json`.

Routing:

| Strategy | Behavior | Use when |
| --- | --- | --- |
| `spread` (default) | Least in-flight key; among idle keys, prefer the smallest balance | Shared keys; maximize aggregate throughput and drain remnants first |
| `weighted` | Random, weighted by balance and damped by in-flight count | Prefer richer keys and reduce 402s |
| `highest` | Highest balance first | Maximize runway |
| `drain` | Smallest usable balance first | Spend small balances before someone else does |
| `round_robin` | Cycle through eligible keys | Simple, predictable distribution |

Failover per request (up to `max_attempts` distinct keys):

| Upstream result | Action |
| --- | --- |
| 2xx | Relay |
| 401 / 403 | Mark invalid, tombstone, retry |
| 402 | Move to cold pool, retry |
| 429 | Cool down (`Retry-After` wins), retry |
| 5xx / network | Short cooldown, retry |
| Other 4xx | Relay unchanged |

For streaming requests, failover happens before the first byte is sent to the client; once a stream has started it cannot be replayed.

## Quick start

Requires Go 1.27+.

```bash
cp config.example.yaml config.yaml
cp keys.txt.example keys.txt
$EDITOR keys.txt              # add at least one real DeepSeek key
make build
./dspool start
./dspool status
```

Open <http://127.0.0.1:8787/>. Stop with `./dspool stop`.

No config file needed:

```bash
DSPOOL_KEYS="sk-key-1,sk-key-2" ./dspool start
```

`keys.txt` accepts one key per line, with an optional label:

```
main = sk-xxxxxxxx
backup = sk-yyyyyyyy
sk-zzzzzzzz
```

`config.yaml`, `keys.txt`, `data/`, logs, `.env` files, and key material are gitignored.

To install the binary on `PATH`, run this from the repository root:

```bash
ln -sfn "$PWD/dspool" ~/.local/bin/dspool
dspool --version
```

If no `-config` is given, dspool looks for `config.yaml` in the current directory, then `~/.config/dspool/`, `~/.dspool/`, and the real binary directory.

## CLI

| Command | Description |
| --- | --- |
| `dspool` / `dspool serve` | Run the proxy in the foreground |
| `dspool start` | Start a detached background daemon |
| `dspool stop` | Stop the daemon |
| `dspool status` | Show pid, pool counts, uptime, and log path |
| `dspool claude [args...]` | Start Claude Code against the pool |
| `dspool codex [args...]` | Start Codex CLI against the pool |
| `dspool pi [args...]` | Start pi against the pool |
| `dspool help` | Show help |

Agent shortcuts reuse a running daemon or start one, inject the right base URL and token, and pass all arguments and the exit code through unchanged. `-no-start` requires an existing daemon (`-no-embed` is a deprecated alias).

Flags: `-config <path>`, `-listen <addr>`, `-log-level <level>`, `-no-start`, `-version`.

Shortcut logs go to `<state_dir>/dspool-agent.log`; daemon logs go to `<state_dir>/dspool.log`. Override both with `log_file` or `DSPOOL_LOG_FILE`.

## Clients

### Claude Code

```bash
dspool claude
```

Or configure it manually:

```bash
export ANTHROPIC_BASE_URL=http://127.0.0.1:8787
export ANTHROPIC_AUTH_TOKEN=dspool-local
export ANTHROPIC_MODEL='deepseek-flash[1m]'
export ANTHROPIC_DEFAULT_OPUS_MODEL='deepseek-flash[1m]'
export ANTHROPIC_DEFAULT_SONNET_MODEL='deepseek-flash[1m]'
export ANTHROPIC_DEFAULT_HAIKU_MODEL=deepseek-flash
export CLAUDE_CODE_SUBAGENT_MODEL=deepseek-flash
claude
```

`[1m]` is a client-side context marker; dspool removes it before forwarding. If `ANTHROPIC_MODEL` is not set, `claude-*` names are mapped to `models.default`.

### Codex CLI

```bash
dspool codex exec "fix the failing tests"
```

The shortcut injects a provider with `-c`, sets `DSPOOL_API_KEY`, advertises the 1M context window (`model_context_window`), and points `model_catalog_json` at a generated catalog so `/model` lists the DeepSeek models with selectable reasoning levels (`low`, `high`, `max`). The catalog is written to `<state_dir>/codex-model-catalog.json`; entries reuse the bundled GPT-6 Astra instructions because Codex rejects catalog entries without instructions.

Manual `~/.codex/config.toml` equivalent:

```toml
model = "deepseek-flash"
model_provider = "dspool"
model_reasoning_effort = "high"
model_context_window = 1000000
model_catalog_json = "/path/to/codex-model-catalog.json"

[model_providers.dspool]
name = "dspool"
base_url = "http://127.0.0.1:8787/v1"
env_key = "DSPOOL_API_KEY"
wire_api = "responses"
```

```bash
export DSPOOL_API_KEY=dspool-local
```

Codex uses the Responses API. dspool translates requests and streaming events to Chat Completions, including function tools and Codex's freeform `apply_patch` tool. DeepSeek reasoning is round-tripped as Responses reasoning items and back to `reasoning_content`, because DeepSeek requires prior reasoning to be sent back in tool-calling conversations.

### pi

```bash
dspool pi
```

The shortcut maintains a `dspool` provider in `$PI_CODING_AGENT_DIR/models.json` (default `~/.pi/agent/models.json`) and injects `--provider`, `--model`, and `--api-key`. Other providers are preserved.

On each launch it also writes the bundled search extension to `$PI_CODING_AGENT_DIR/extensions/dspool-tools/` and exports `DSPOOL_API_KEY` and `DSPOOL_BASE_URL`. The extension registers `web_search` and `web_fetch` only when those variables are present, so plain pi sessions are unaffected. Set `DSPOOL_PI_SEARCH=0` to skip the install.

For manual setup, use `openai-completions` with `baseUrl: http://127.0.0.1:8787/v1`, or `anthropic-messages` with `baseUrl: http://127.0.0.1:8787`.

### Any OpenAI-compatible client

```python
from openai import OpenAI

client = OpenAI(base_url="http://127.0.0.1:8787/v1", api_key="dspool-local")
resp = client.chat.completions.create(
    model="deepseek-flash",
    messages=[{"role": "user", "content": "hi"}],
)
```

### Web search and fetch

dspool proxies DeepSeek's raw Search & Fetch APIs through the same key pool and failover as chat:

| Endpoint | Upstream |
| --- | --- |
| `POST /v1/tools/search` | `POST /v1/tools/search` |
| `POST /v1/tools/fetch` | `POST /v1/tools/fetch` |

Both accept the documented JSON bodies and use the normal client token. The `/tools/search` and `/tools/fetch` aliases also work, and responses are relayed unchanged.

```bash
curl -s http://127.0.0.1:8787/v1/tools/search \
  -H "Authorization: Bearer dspool-local" \
  -H "Content-Type: application/json" \
  -d '{"query":"DeepSeek V4","count":5}'
```

`dspool pi` installs a small extension that exposes these endpoints as `web_search` and `web_fetch`. DeepSeek bills each successful search or fetch request separately from model tokens; check its current pricing.

## Dashboard and admin

<http://127.0.0.1:8787/> (also `/dashboard`) shows hot-pool balance, balance distribution, pool health, and a searchable/filterable key ledger. Expand a row for request stats, timestamps, full errors, and a copyable key ID. Invalid keys can be restored in place.

- Refreshes every 5 seconds; pause with the live toggle.
- English-first UI with an EN/中文 switch; the choice is remembered locally.
- Manual balance refresh, key-list reload, and light/dark theme.
- Unauthenticated by design; it only exposes masked keys.

| Endpoint | Purpose |
| --- | --- |
| `GET /dashboard/data` | Dashboard JSON |
| `POST /dashboard/refresh` | Probe all balances |
| `POST /dashboard/reload` | Re-read `keys.txt` / `config.yaml` and sync keys |
| `POST /dashboard/keys/{id}/restore` | Restore an invalid key |
| `GET /healthz` | 200 with any usable key, 503 otherwise |
| `GET /admin/status` | Full status including tombstones |
| `POST /admin/refresh` | Probe all balances |
| `POST /admin/reload` | Re-read the key list and sync keys |
| `POST /admin/keys/{id}/restore` | Restore an invalid key |

`/admin/*` uses `admin_token` via `Authorization: Bearer` or `X-Admin-Token`; without a token it is loopback-only.

## Configuration

See [`config.example.yaml`](config.example.yaml) for the complete reference. Main settings:

| Key | Default | Description |
| --- | --- | --- |
| `listen` | `127.0.0.1:8787` | HTTP listen address |
| `state_file` | `data/state.json` | Tombstone state file |
| `log_level` | `info` | `debug`, `info`, `warn`, `error` |
| `log_file` | `""` | Log destination; see CLI section |
| `client_keys` | `[]` | Accepted proxy tokens; empty accepts any token |
| `admin_token` | `""` | Admin token; empty is loopback-only |
| `upstream.anthropic_base` | `https://api.deepseek.com/anthropic` | Anthropic base |
| `upstream.openai_base` | `https://api.deepseek.com/v1` | OpenAI base |
| `upstream.balance_url` | `https://api.deepseek.com/user/balance` | Balance endpoint |
| `pool.strategy` | `spread` | Routing strategy |
| `pool.max_attempts` | `4` | Max upstream attempts per request |
| `pool.max_inflight_per_key` | `0` | Local concurrency cap; 0 = unlimited |
| `pool.balance_interval` | `30s` | Hot-key probe interval |
| `pool.cold_check_interval` | `2m` | Cold-pool probe interval |
| `pool.min_balance` | `0` | Cold-pool threshold |
| `models.default` | `deepseek-flash` | Fallback model mapping |
| `models.small` | `deepseek-flash` | Haiku-style model mapping |
| `models.map` | `{}` | Exact or glob model mappings |
| `responses.thinking` | `auto` | `auto`, `enabled`, `disabled` |
| `keys_file` / `keys` | — | Key file or inline keys |

Environment overrides: `DSPOOL_LISTEN`, `DSPOOL_STATE_FILE`, `DSPOOL_LOG_LEVEL`, `DSPOOL_LOG_FILE`, `DSPOOL_ADMIN_TOKEN`, `DSPOOL_STRATEGY`, `DSPOOL_KEYS`, `DSPOOL_KEYS_FILE`, `DSPOOL_CLIENT_KEYS`.

## Operations

- `state.json` stores only rejected-key fingerprints, labels, masks, and reasons; never plaintext keys.
- Reload keys without a restart: click **Reload keys** in the dashboard, `POST /dashboard/reload`, or send `SIGHUP` to a foreground `serve` process. Only the key list is reloaded; other config changes need a restart.
- After rebuilding the binary, restart the daemon with `make restart` (or `dspool stop && dspool start`).
- `deploy/dspool.service`, `deploy/com.dspool.plist`, and the `Dockerfile` provide deployment examples.
- Cold-pool keys recover automatically after a top-up, or immediately via a dashboard/admin refresh.

## Security

- Binds to loopback by default.
- Set `client_keys` if exposing the proxy beyond localhost.
- The dashboard is unauthenticated; keep it on loopback or put it behind your own access control.
- dspool sends no CORS headers and rejects cross-site browser requests.
- With empty `client_keys`, only loopback `Host` headers are accepted (DNS-rebinding mitigation).
- Logs contain masked keys and upstream errors, never full credentials.

## Limitations

- Streaming failover only happens before the first response byte.
- `/v1/responses` is a translation layer: `previous_response_id` is rejected, server-side tools are unsupported, and some Codex-specific history items (agent messages, compaction, local shell calls) are ignored. Namespace tools and structured outputs are best-effort.
- `/v1/tools/search` and `/v1/tools/fetch` proxy DeepSeek's raw endpoints; they do not add server-side web search to Chat Completions. Each successful request is billed by DeepSeek.
- `count_tokens` returns a local estimate.
- Agent shortcuts require a fixed listen port; a running daemon only reloads the key list (dashboard button, `POST /admin/reload`, or `SIGHUP`). Other config changes need `dspool stop && dspool start`.
- The Codex `/model` picker and reasoning shortcuts depend on `-c model_catalog_json` and `codex debug models --bundled`. With a Codex build that lacks either, the shortcut still runs but the picker keeps Codex's bundled models.
- DeepSeek model names may change. As of October 2026, dspool defaults to `deepseek-flash` and `deepseek-v4-pro`; adjust `models.*` if needed.

## Development

```bash
make fmt
make vet
make test
make race
make build
```

Layout: `cmd/dspool` (CLI/daemon), `internal/config`, `internal/balance`, `internal/pool`, `internal/monitor`, `internal/state`, `internal/proxy` (HTTP proxy, Responses translation, dashboard), `integrations/pi/dspool-tools` (pi search/fetch extension, embedded into the binary).

## License

[MIT](LICENSE)
