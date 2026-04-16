# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

**docker-dynamic-limits** is a system for managing dynamic resource limits on Docker containers. It provides per-container control over:

- CPU total usage (with container freezing when limits are reached)
- External paid services spending budget (LLMs, etc.)
- Disk space usage
- RAM usage
- Network total usage (bytes)
- Disk I/O total bytes
- Disk I/O total operations

## Key Requirements

- For each container and each limit type: check current usage, set/increase/decrease limits
- **Host limits** (formerly "global limits"): shared budgets across ALL containers on the machine — the SUM of all container usage is checked against the host limit. When exceeded, enforcement is applied to ALL containers. The `isOtherPauseActive` check includes scope enforcement state. For spending limits, scope enforcement sets `scopeSpendingBlocked` on the proxy, causing tracked API calls to be rejected with HTTP 429. Internally uses `model.Scope` with `ScopeHost` (empty string) for host scope. The API still supports `/global-limits` as an alias for `/host-limits`.
- **Segments**: Named groups of containers with their own scope-level limits. A container belongs to at most one segment. Segment limits are additive on top of host limits — a container in segment "prod" is checked against both segment AND host limits. Enforcement loop checks host scope + each segment scope per tick. Segments have their own config (inheriting from host, API-only via `/segments/{id}/config`). API: `/segments` CRUD, `/segments/{id}/limits`, `/segments/{id}/usage`, `/segments/{id}/containers/{cid}/*`. CLI: `ddl segment` for CRUD/assign, `ddl limits *-segment` for limits, `ddl usage-segment` for usage, `ddl freeze-segment`/`unfreeze-segment` for bulk freeze. All `-all` commands (`limits set-all`, `usage-all`, `freeze-all`, `ls`) are scope-aware — they target whatever scope is active.
- **Scoped listeners**: Persistent API listeners locked to a scope via `POST /scoped-listeners`. A scoped listener only serves containers/limits/config for its scope. CLI: `ddl scope listen/listeners/unlisten`.
- **CLI scope**: `ddl scope set <segment>` persists to `~/.config/ddl/scope`. Also `--segment` flag and `DDL_SEGMENT` env. Resolution order: flag > env > persisted > host.
- **Scope usage accumulator**: When a container is removed, its cumulative usage (all types except `ram` and `disk`) is accumulated into `scope_usage_accum` table for both host scope AND the container's segment scope (if any). This prevents removing a container from reducing totals for irreversible resources. The accumulator is added to the live sum in both the API and enforcement (`checkScopeEnforcement`). Legacy `global_usage_accum` table is kept in sync for backward compat.
- Container cloning capability
- Containers must be able to query their own limits and usage from inside the container

## Runtime Configuration

The daemon supports runtime configuration via `ddl config get` / `ddl config set`. Config keys:

| Key | Type | Set | Notes |
|---|---|---|---|
| `anthropic-enabled` | bool | yes | Enable/disable Anthropic proxy |
| `openai-enabled` | bool | yes | Enable/disable OpenAI proxy |
| `ollama-enabled` | bool | yes | Enable/disable Ollama proxy |
| `anthropic-key` | string | yes | Masked in `config get` |
| `openai-key` | string | yes | Masked in `config get` |
| `ollama-url` | string | yes | Ollama server URL |
| `ollama-models` | list | yes | Comma-separated in CLI |
| `ollama-queue-size` | int | yes | |
| `ollama-timeout` | duration | yes | e.g. `2m`, `120s` |
| `ollama-default-bid` | int | yes | milli-cents/wall-sec |
| `error-webhooks` | list | yes | Comma-separated webhook URLs |
| `keep-limits-consistent` | bool | yes | Validate per-container vs global limits |

API: `GET /config` returns all config (keys masked), `PUT /config` accepts partial JSON updates. The dashboard shows config read-only in a collapsible panel.

**Config persistence**: `PUT /config` changes are persisted to a JSON file (`config.json` in the same directory as the SQLite database). On daemon startup, persisted config is loaded and overlaid on environment variable defaults. Only explicitly set keys are persisted.

**Disabled providers**: When a provider (Anthropic, OpenAI, Ollama) is disabled via config, requests from containers to that provider are blocked with HTTP 403 `{"error":"provider disabled in ddl proxy"}`. The check happens early in `proxyHandler` via `isDisabledProvider()` before any forwarding.

**Keep-limits-consistent**: When `keep_limits_consistent` is enabled, the API validates that per-container and global limits stay consistent. Rule: for each limit type, `sum(all per-container limits) ≤ global limit`. On per-container `increase`: if sum would exceed global → HTTP 400 with `max_increase`. On per-container `set`: if would exceed → auto-cap to max allowed, HTTP 209 with `applied: "partial"`. On global `decrease` or `set` lower: if new global < sum of per-container limits → HTTP 400 with `min_value`. Cannot be enabled when current limits are already inconsistent. All limit PUT responses include enhanced fields: `old_value`, `operation`, `applied`.

## Proxy Activity

Per-container proxy activity is recorded in an in-memory ring buffer (last 20 entries per container) in `SpendingTracker`. Activity is recorded for:
- Cloud API calls (Anthropic/OpenAI) — including request/response bodies (truncated to 4KB), model, tokens, cost, duration
- Ollama requests — via `ActivityRecorder` callback from `ollama.Queue.processEntry()`
- Blocked requests (403 disabled provider, 429 budget exceeded) — with error message

API: `GET /containers/{id}/activity` returns `[]ProxyActivity`. Activity is also included in `GET /containers/{id}` response. The dashboard shows a "Recent Proxy Activity" table in the container detail panel with expandable rows for request/response bodies.

## Daemon Restart Hooks

CLI users can register shell commands that run on the host after every `ddl daemon start`. Managed via `ddl hooks add/list/remove`. Stored locally in `~/.config/ddl/hooks.json`. Executed sequentially via `sh -c` after daemon readiness check in `daemonStartCmd()`. Failed hooks warn but don't abort.

## Container Freeze

User-initiated freeze/unfreeze for pausing container lifetime. "Freeze" is distinct from Docker's `pause` and enforcement's pause.

**Frozen = all 4 byte-second accumulators suspended**: `ram-usage-bsec`, `disk-usage-bsec`, `ram-request-bsec`, `disk-request-bsec`. Only user-freeze suspends these — enforcement-pause does NOT.

**Freeze vs enforcement-pause interaction:**

| Scenario | Behavior |
|---|---|
| Freeze running container | Docker pause + set frozen flag + suspend byte-sec |
| Freeze enforcement-paused container | Just set frozen flag (already Docker-paused) |
| Enforcement pauses frozen container | Just set enforced flag (already Docker-paused) |
| Enforcement releases on frozen container | Clear enforced flag, DON'T Docker-unpause |
| Unfreeze during enforcement-pause | Clear frozen flag, resume byte-sec, container stays Docker-paused |
| Unfreeze with no enforcement active | Docker unpause + clear frozen flag + resume byte-sec |

**Implementation:** `frozen` column in SQLite, `frozen` map in `enforcement.Manager`, `isOtherPauseActive()` returns true when frozen. CLI: `ddl freeze/unfreeze/freeze-all/unfreeze-all`. API: `POST /containers/{id}/freeze`, `POST /containers/{id}/unfreeze`, `POST /freeze-all`, `POST /unfreeze-all`. Events: `container_frozen`, `container_unfrozen`. Ollama: `CancelAllPending()` on freeze cancels pending+active requests but keeps bid.

## Container Kill on Byte-Second Limits

When a byte-second limit (`ram-usage-bsec`, `disk-usage-bsec`, `ram-request-bsec`, `disk-request-bsec`) is exceeded, the container is **killed** via `docker stop` (SIGTERM + 10s timeout + SIGKILL). This is more aggressive than other limit types which pause or throttle. A `container_killed` event is emitted with `limit_type`, `usage_at_kill`, and `limit_at_kill` fields, visible via `ddl events`. Logs are persisted to `/data/ddld.log` on the data volume.

## Upstream Error Webhooks

When an upstream LLM API (Anthropic, OpenAI) returns an error response (HTTP 4xx/5xx), the daemon can call configurable webhook URLs. This notifies the operator of issues like expired API keys or exhausted credit balances.

**Error detection**: In `proxyHandler()`, after receiving the upstream response, errors are parsed for known provider formats (Anthropic `{"type":"error","error":{...}}`, OpenAI `{"error":{...}}`).

**Deduplication**: Same `host:error_type` fires at most once per 5 minutes to prevent flooding. Tracked in-memory in `SpendingTracker.errorDedup`.

**Webhook payload**: POST JSON with `type`, `timestamp`, `container_id`, `container_name`, `host`, `status_code`, `error_type`, `error_message`, `request_id`. 5s timeout per call, runs in goroutine.

**Config**: `ddl config set error-webhooks "url1,url2"`. API: `PUT /config {"error_webhooks": ["url1","url2"]}`. Persisted to `config.json`.

**Event**: `proxy_upstream_error` event published via bus, visible in `ddl events`.

## System Sleep Handling

When the host Mac sleeps (lid close), Docker Desktop's Linux VM suspends. The daemon detects this via wall-clock time gaps between enforcement ticks and takes corrective action:

**Affected limits** (tick-based accumulation):
- `ram-usage-bsec`, `disk-usage-bsec`, `ram-request-bsec`, `disk-request-bsec` — byte-second accumulation is skipped during detected sleep
- Ollama wall-clock billing — sleep duration is subtracted from billed wall time

**Not affected** (hardware counters / token-based):
- `cpu`, `ram`, `disk`, `net`, `disk-io-bytes`, `disk-io-ops`, `spending` (OpenAI/Anthropic)

**Implementation details:**
- Sleep is detected when wall-clock gap between ticks exceeds 5× the tick interval
- `time.Now().Round(0)` is used to strip Go's monotonic clock reading (monotonic clock does not advance during VM suspend, so `time.Sub` would miss the gap)
- `ticker.Reset()` is called after sleep detection because Go tickers get stuck after VM suspend/resume
- A `system_sleep` event is emitted (deduplicated across containers) and visible via `ddl events`
