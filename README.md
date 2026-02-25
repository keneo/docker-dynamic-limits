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
| **Spending** | USD cents | HTTP proxy budget block |
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

## Quick start

### Build

```bash
go build ./cmd/ddld   # daemon
go build ./cmd/ddl    # CLI
```

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

**Read-only API** (TCP — for containers):

| Method | Endpoint | Description |
|---|---|---|
| `GET` | `/containers` | List all managed containers |
| `GET` | `/usage` | Self-query usage + limits (by source IP) |
| `GET` | `/limits` | Self-query limits (by source IP) |

## Spending tracking

The daemon runs a per-container HTTP forward proxy that intercepts API calls to:
- `api.openai.com`
- `api.anthropic.com`

Token usage is extracted from responses and costs are calculated using built-in model pricing. When the spending budget is exceeded, further API requests are blocked with HTTP 429.

## Value formats

| Type | Format examples |
|---|---|
| CPU time | `3600s`, `60m`, `1h` |
| Bytes (RAM, disk, network, I/O) | `1024`, `512k`, `256m`, `1g`, `1.5t` |
| Byte-seconds (usage/request B·s) | `100g`, `1.5t` (same byte suffixes, displayed as e.g. `1.5G·s`) |
| I/O operations | Plain integer |
| Spending | `10.00` (USD, stored as cents) |

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
