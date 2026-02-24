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

- **Per-container limits** — set, increase, or decrease any limit at any time
- **Automatic enforcement** — daemon polls every second and applies/releases enforcement actions
- **Spending tracking** — transparent HTTP proxy intercepts OpenAI and Anthropic API calls, extracts token usage from responses, and calculates costs using configurable model pricing
- **Container cloning** — clone a running container with all its limits copied over
- **In-container self-query** — containers can check their own limits and usage via REST API

## Architecture

```
┌──────────┐         HTTP          ┌─────────────────────────────────────┐
│  ddl CLI ├───────────────────────┤  ddld (daemon on :7123)             │
└──────────┘                       │                                     │
                                   │  ┌─────────┐  ┌──────────────────┐ │
                                   │  │ REST API │  │ Enforcement Mgr  │ │
                                   │  └────┬─────┘  └──┬───────────┬──┘ │
                                   │       │           │           │     │
                                   │  ┌────┴─────┐ ┌───┴────┐ ┌───┴───┐│
                                   │  │  SQLite   │ │ Docker │ │ cgroup ││
                                   │  │  Store    │ │ Client │ │ Reader ││
                                   │  └──────────┘ └────────┘ └───────┘│
                                   │                                     │
                                   │  ┌──────────────────┐              │
                                   │  │ Spending Proxy    │              │
                                   │  │ (per-container)   │              │
                                   │  └──────────────────┘              │
                                   └─────────────────────────────────────┘
```

## Quick start

### Build

```bash
go build ./cmd/ddld   # daemon
go build ./cmd/ddl    # CLI
```

### Run the daemon

```bash
# Default: listens on :7123, stores data in /var/lib/ddl/ddl.db
sudo ddld

# Custom options
ddld -addr :8080 -db ./ddl.db -cgroup-base /sys/fs/cgroup
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

## In-container self-query

Containers can query their own limits and usage from inside:

```bash
# Using query parameter
curl "http://<host>:7123/usage?id=<container_id>"
curl "http://<host>:7123/limits?id=<container_id>"

# Using header
curl -H "X-Container-ID: <container_id>" "http://<host>:7123/usage"
```

## REST API

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
| `GET` | `/usage?id=...` | In-container usage self-query |
| `GET` | `/limits?id=...` | In-container limits self-query |

## Spending tracking

The daemon runs a per-container HTTP forward proxy that intercepts API calls to:
- `api.openai.com`
- `api.anthropic.com`

Token usage is extracted from responses and costs are calculated using built-in model pricing (configurable). When the spending budget is exceeded, further API requests are blocked with HTTP 429.

## Value formats

| Type | Format examples |
|---|---|
| CPU time | `3600s`, `60m`, `1h` |
| Bytes (RAM, disk, network, I/O) | `1024`, `512k`, `256m`, `1g`, `1.5t` |
| I/O operations | Plain integer |
| Spending | `10.00` (USD, stored as cents) |

## Requirements

- Linux with cgroup v2
- Docker Engine
- Go 1.18+

## Testing

```bash
go test ./... -v
```

89 tests across 8 packages covering the store (real SQLite `:memory:`), cgroup parsers, proxy spending logic, enforcement manager, REST API (httptest), and CLI formatting.

## License

MIT
