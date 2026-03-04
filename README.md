[![Tests](https://github.com/keneo/docker-dynamic-limits/actions/workflows/test.yml/badge.svg)](https://github.com/keneo/docker-dynamic-limits/actions/workflows/test.yml)
[![E2E](https://github.com/keneo/docker-dynamic-limits/actions/workflows/e2e.yml/badge.svg)](https://github.com/keneo/docker-dynamic-limits/actions/workflows/e2e.yml)

# docker-dynamic-limits

Dynamic resource limit management for Docker containers. Set, monitor, and enforce cumulative limits on CPU time, RAM, disk, network, I/O, and API spending per container — with automatic enforcement and in-container self-querying.

## Features

| Limit type | Unit | Enforcement |
|---|---|---|
| **CPU** | Cumulative seconds | Container pause |
| **RAM** | Bytes | cgroup `memory.max` + Docker API |
| **Disk** | Bytes (writable layer) | Container pause |
| **Network** | Cumulative bytes | Network disconnect |
| **Disk I/O bytes** | Cumulative bytes | cgroup `io.max` throttle |
| **Disk I/O ops** | Cumulative operations | cgroup `io.max` throttle |
| **Spending** | USD milli-cents | HTTP proxy budget block |
| **RAM usage B·s** | Byte-seconds (actual RAM × time) | Container kill |
| **Disk usage B·s** | Byte-seconds (actual disk × time) | Container kill |
| **RAM request B·s** | Byte-seconds (ddl RAM limit × time) | Container kill |
| **Disk request B·s** | Byte-seconds (ddl disk limit × time) | Container kill |

- **Per-container limits** — set, increase, or decrease any limit at any time
- **Automatic enforcement** — daemon polls every second and applies/releases enforcement actions
- **Spending tracking** — transparent HTTP proxy intercepts OpenAI and Anthropic API calls, extracts token usage from responses, and calculates costs using built-in model pricing
- **Container cloning** — clone a running container with all its limits copied over
- **In-container self-query** — containers can check their own limits and usage via REST API or `ddl-guest` binary
- **Web dashboard** — real-time browser UI for monitoring and managing containers

## Architecture

```
                         ┌──────────────────────────────────────────┐
                         │  ddld (daemon)                           │
┌──────────┐  unix sock  │                                          │
│  ddl CLI ├─────────────┤  Full API (unix socket /run/ddl/ddl.sock)│
└──────────┘             │    register, limits, clone, delete, ...  │
                         │                                          │
┌──────────┐  TCP :7123  │  Read-only API (TCP)                     │
│containers├─────────────┤    GET /containers, /usage, /limits      │
└──────────┘  (by src IP)│    Container identified by source IP     │
                         │                                          │
┌──────────┐  TCP :7124  │  ┌──────────┐  ┌──────────────────┐     │
│ dashboard├─────────────┤  │  SQLite   │  │ Enforcement Mgr  │     │
└──────────┘             │  │  Store    │  └──┬───────────┬───┘     │
                         │  └──────────┘  ┌───┴────┐ ┌────┴──┐     │
                         │                │ Docker │ │ cgroup │     │
                         │                │ Client │ │ Reader │     │
                         │                └────────┘ └───────┘     │
                         │  ┌──────────────────┐                    │
                         │  │ Spending Proxy    │                    │
                         │  │ (per-container)   │                    │
                         │  └──────────────────┘                    │
                         └──────────────────────────────────────────┘
```

On macOS (Docker Desktop), the unix socket is not accessible from the host. The CLI automatically falls back to `docker exec` to reach the daemon's socket from inside the container.

## Installation

### Using `go install`

```bash
go install github.com/keneo/docker-dynamic-limits/cmd/ddl@latest
go install github.com/keneo/docker-dynamic-limits/cmd/ddld@latest
go install github.com/keneo/docker-dynamic-limits/cmd/ddl-guest@latest
```

### Using Make

```bash
git clone https://github.com/keneo/docker-dynamic-limits.git
cd docker-dynamic-limits
make install          # builds and installs to /usr/local/bin
```

To install to a different location:

```bash
make install PREFIX=~/.local/bin
```

### Build without installing

```bash
make build            # produces ./ddl, ./ddld, ./ddl-guest in the repo root
```

## Quick start

### Run the daemon (containerized)

The recommended way to run ddld is as a Docker container:

```bash
ddl daemon start          # build image (first time) and start container
ddl daemon start --build  # force rebuild of the image
ddl daemon status         # check if running
ddl daemon stop           # stop and remove container
```

This starts ddld in a container named `ddl-daemon` with:
- TCP API on port 7123 (read-only, for containers)
- Unix socket at `/run/ddl/ddl.sock` (full API, for host management)
- SQLite database on a persistent Docker volume

### Run the daemon (directly)

```bash
# Default: TCP on :7123, socket at /run/ddl/ddl.sock
sudo ddld

# Custom options
ddld -addr :8080 -db ./ddl.db -sock /tmp/ddl.sock

# No socket (full API on TCP, useful for development)
ddld -addr :8080 -db ./ddl.db -sock ""
```

### Register a container

```bash
ddl register <container_id>
```

### Set limits

```bash
ddl limits set <container> cpu 1h          # 1 hour of CPU time
ddl limits set <container> ram 512m        # 512 MiB RAM
ddl limits set <container> disk 10g        # 10 GiB disk
ddl limits set <container> net 1g          # 1 GiB network transfer
ddl limits set <container> disk-io-bytes 5g
ddl limits set <container> disk-io-ops 1000000
ddl limits set <container> spending 10.00  # $10.00 USD

ddl limits set <container> ram-usage-bsec 100g    # 100 GB·s of RAM usage over time
ddl limits set <container> disk-usage-bsec 500g   # 500 GB·s of disk usage over time
ddl limits set <container> ram-request-bsec 1t    # 1 TB·s of RAM reservation over time
ddl limits set <container> disk-request-bsec 1t   # 1 TB·s of disk reservation over time
```

### Adjust limits

```bash
ddl limits increase <container> cpu 30m
ddl limits decrease <container> ram 128m
```

### Monitor

```bash
ddl usage <container>    # usage vs limits with percentages
ddl usage-all            # usage for all containers at once
ddl limits get <container>
ddl ls                   # list all managed containers
```

### Clone a container

```bash
ddl clone <container> [new-name]
```

### Remove from management

```bash
ddl remove <container>
```

### JSON output

All commands support `--json` for machine-readable output:

```bash
ddl ls --json                    # array of container status objects
ddl usage <container> --json     # {usage, limits, status} with percentages
ddl usage-all --json             # full container array with usage/limits
ddl limits get <container> --json
ddl register <container> --json
ddl clone <container> --json
ddl remove <container> --json
ddl limits set <c> cpu 1h --json
```

### Real-time events

Stream live events from the daemon via WebSocket:

```bash
ddl events                                      # stream all events
ddl events --container my-container              # filter by container
ddl events --types limit_change                  # filter by event type
ddl events --types limit_change,enforcement_change
ddl events --raw                                 # NDJSON output (one JSON object per line)
ddl events --raw | jq .                          # pipe to jq for pretty-printing
```

Event types:

| Type | Description | Source |
|---|---|---|
| `usage_update` | Per-container usage snapshot | Enforcement loop (~1s per container) |
| `limit_change` | A limit was set, increased, or decreased | `PUT /containers/{id}/limits` |
| `enforcement_change` | Enforcement was applied or released | Enforcement manager |
| `container_register` | A new container was registered | `POST /register` or clone |
| `container_remove` | A container was removed | `DELETE /containers/{id}` |

The WebSocket endpoint is `GET /events` with optional query parameters:
- `container_id` — comma-separated container IDs to filter
- `types` — comma-separated event types to filter

**Note:** WebSocket is not available via the docker exec transport (macOS fallback). Use `--sock` or `DDL_SOCK` to connect via unix socket, or `--api` for TCP.

## Web dashboard

A browser-based UI for real-time monitoring and management:

```bash
ddl dashboard             # start on :7124
ddl dashboard --open      # start and open browser
ddl dashboard stop        # stop the dashboard
```

The dashboard shows all containers with their limits, usage, and enforcement status. You can register, clone, remove containers and set limits directly from the UI. An offline banner appears when the daemon is unreachable.

## In-container self-query

Containers are automatically identified by their source IP address (refreshed every 5 seconds). No tokens or headers needed.

### Using ddl-guest (recommended)

The `ddl-guest` binary is included in the daemon container image and can be copied into managed containers:

```bash
# Copy ddl-guest into a running container
docker cp ddl-daemon:/ddl-guest /tmp/ddl-guest
docker cp /tmp/ddl-guest <container>:/usr/local/bin/ddl-guest

# Inside the container
ddl-guest          # formatted table output
ddl-guest -json    # raw JSON
```

`ddl-guest` auto-discovers the daemon by trying `host.docker.internal:7123` and `172.17.0.1:7123`, or use `DDL_API_URL` to override.

### Using curl

```bash
# From inside a container (identified by source IP)
curl http://host.docker.internal:7123/usage
curl http://host.docker.internal:7123/limits
```

## REST API

The daemon exposes two interfaces:

**Full API** (unix socket — for host management):

| Method | Endpoint | Description |
|---|---|---|
| `GET` | `/containers` | List all managed containers with status |
| `POST` | `/register` | Register a container `{"container_id": "..."}` |
| `GET` | `/containers/{id}` | Get container status (limits, usage, enforcement) |
| `DELETE` | `/containers/{id}` | Stop managing a container |
| `GET` | `/containers/{id}/limits` | Get all limits |
| `PUT` | `/containers/{id}/limits` | Set/increase/decrease a limit |
| `GET` | `/containers/{id}/usage` | Get current usage |
| `POST` | `/containers/{id}/clone` | Clone container with limits |
| `GET` | `/usage` | In-container usage self-query |
| `GET` | `/limits` | In-container limits self-query |
| `GET` | `/events` | WebSocket event stream (query: `container_id`, `types`) |

**Read-only API** (TCP — for containers):

| Method | Endpoint | Description |
|---|---|---|
| `GET` | `/containers` | List all managed containers |
| `GET` | `/usage` | Self-query usage + limits (by source IP) |
| `GET` | `/limits` | Self-query limits (by source IP) |
| `GET` | `/events` | WebSocket event stream (query: `container_id`, `types`) |

## Using LLMs with spending limits

The daemon includes a per-container HTTP proxy that tracks and enforces spending budgets on LLM API calls. Containers make plain HTTP requests through the proxy; the proxy upgrades them to HTTPS, injects the real API key, tracks token usage from responses, and blocks requests once the budget is exhausted.

### How it works

```
Container                    ddld Spending Proxy               LLM API
   │                              │                              │
   │  curl http://api.anthropic   │                              │
   │  .com/v1/messages            │                              │
   │  (no API key, plain HTTP)    │                              │
   │─────────────────────────────>│                              │
   │                              │  POST https://api.anthropic  │
   │                              │  .com/v1/messages            │
   │                              │  x-api-key: sk-ant-...       │
   │                              │─────────────────────────────>│
   │                              │                              │
   │                              │  200 OK {usage: ...}         │
   │                              │<─────────────────────────────│
   │                              │                              │
   │  200 OK {usage: ...}         │  (track tokens + cost)       │
   │<─────────────────────────────│                              │
```

The proxy:
1. Receives HTTP requests from the container
2. Upgrades the connection to HTTPS for the real API
3. Strips any auth headers the container sent and injects the daemon-configured API key
4. Forwards the request and reads the response to extract token usage
5. Calculates costs using built-in model pricing and accumulates spending
6. Returns HTTP 429 `{"error":"spending budget exceeded"}` once the budget is hit

This means containers never see the real API key and cannot bypass the spending limit.

### Supported APIs

| Provider | Host | Auth header | Models with built-in pricing |
|---|---|---|---|
| Anthropic | `api.anthropic.com` | `x-api-key` | claude-3-opus, claude-3-sonnet, claude-3-haiku, claude-haiku-4-5 |
| OpenAI | `api.openai.com` | `Authorization: Bearer` | gpt-4, gpt-4-turbo, gpt-4o, gpt-4o-mini, gpt-3.5-turbo |

Unknown models are charged at a conservative default rate. Custom pricing can be loaded via `LoadPrices()`.

### Setup

Pass API keys as environment variables when starting the daemon:

```bash
# Anthropic only
DDL_ANTHROPIC_API_KEY=sk-ant-... ddl daemon start --build

# OpenAI only
DDL_OPENAI_API_KEY=sk-... ddl daemon start --build

# Both
DDL_ANTHROPIC_API_KEY=sk-ant-... DDL_OPENAI_API_KEY=sk-... ddl daemon start --build
```

The keys are forwarded into the daemon container automatically.

### Using from a container

After registering a container, the API returns a `proxy_addr` field. Configure the container to use this as its HTTP proxy:

```bash
# Register and get proxy address
ddl register <container>
# Response includes: "proxy_addr": "0.0.0.0:12345"

# Set a spending budget
ddl limits set <container> spending 1.00    # $1.00

# From inside the container, use the proxy (no API key needed):
export http_proxy=http://<daemon-ip>:<proxy-port>
curl -X POST http://api.anthropic.com/v1/messages \
  -H "Content-Type: application/json" \
  -d '{"model":"claude-haiku-4-5-20251001","max_tokens":100,"messages":[{"role":"user","content":"Hello"}]}'
```

The container sends plain HTTP with no API key. The proxy handles HTTPS and authentication transparently.

### Demo

A self-contained demo script shows the full flow — start daemon with API key, create a container, make API calls through the proxy, and watch spending accumulate until the budget is hit with a 429:

```bash
# First run (builds Docker image + CLI):
BUILD=1 ANTHROPIC_API_KEY=sk-ant-... bash examples/llm-budget-demo.sh

# Subsequent runs (reuses Docker image, only rebuilds CLI):
ANTHROPIC_API_KEY=sk-ant-... bash examples/llm-budget-demo.sh
```

Example output:
```
Request #1: "What is the tallest mountain on Earth? Answer in one sentence."
  => Mount Everest is the tallest mountain on Earth.
     [model=claude-haiku-4-5-20251001  tokens: 16 in / 10 out]
     Spending: $0.0001 / $0.0005  (13 / 50 milli-cents)
...
Request #4: HTTP 429 — Budget exceeded!
{"error":"spending budget exceeded"}
```

### Non-API traffic

Requests to non-tracked hosts (anything other than `api.openai.com` and `api.anthropic.com`) pass through the proxy unmodified and are never blocked by the spending budget. Only LLM API calls count toward spending.

## Value formats

| Type | Format examples |
|---|---|
| CPU time | `3600s`, `60m`, `1h` |
| Bytes (RAM, disk, network, I/O) | `1024`, `512k`, `256m`, `1g`, `1.5t` |
| Byte-seconds (usage/request B·s) | `100g`, `1.5t` (same byte suffixes, displayed as e.g. `1.5G·s`) |
| I/O operations | Plain integer |
| Spending | `10.00` (USD, stored as milli-cents) |

## Requirements

- Linux with cgroup v2 (for enforcement; daemon runs in Docker)
- Docker Engine
- Go 1.21+

## Testing

```bash
# Unit tests (80 tests across 8 packages)
go test ./...

# E2E: CLI + spending proxy
bash e2e/cli_proxy_test.sh

# E2E: Docker-in-Docker (full integration)
docker build -t ddl-e2e -f e2e/Dockerfile . && docker run --privileged ddl-e2e
```

## License

MIT
