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
| `ollama-url` | string | no | Read-only (requires restart) |
| `ollama-models` | list | yes | Comma-separated in CLI |
| `ollama-queue-size` | int | yes | |
| `ollama-timeout` | duration | yes | e.g. `2m`, `120s` |
| `ollama-default-bid` | int | yes | milli-cents/wall-sec |

API: `GET /config` returns all config (keys masked), `PUT /config` accepts partial JSON updates. The dashboard shows config read-only in a collapsible panel.

**Config persistence**: `PUT /config` changes are persisted to a JSON file (`config.json` in the same directory as the SQLite database). On daemon startup, persisted config is loaded and overlaid on environment variable defaults. Only explicitly set keys are persisted.

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
