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
- Container cloning capability
- Containers must be able to query their own limits and usage from inside the container

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
