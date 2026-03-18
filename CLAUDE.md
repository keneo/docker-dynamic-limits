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
- **Global limits**: shared budgets across all containers — the SUM of all container usage is checked against the global limit. When exceeded, enforcement is applied to ALL containers (same action per limit type as per-container enforcement). The `isOtherPauseActive` check includes global enforcement state to prevent unpausing a container that's globally enforced.
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

API: `GET /config` returns all config (keys masked), `PUT /config` accepts partial JSON updates. The dashboard shows config read-only in a collapsible panel.

**Config persistence**: `PUT /config` changes are persisted to a JSON file (`config.json` in the same directory as the SQLite database). On daemon startup, persisted config is loaded and overlaid on environment variable defaults. Only explicitly set keys are persisted.

**Disabled providers**: When a provider (Anthropic, OpenAI, Ollama) is disabled via config, requests from containers to that provider are blocked with HTTP 403 `{"error":"provider disabled in ddl proxy"}`. The check happens early in `proxyHandler` via `isDisabledProvider()` before any forwarding.

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
