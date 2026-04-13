# Segments User Guide

## What are segments?

Segments let you group containers and apply shared resource limits to each group. Think of them as budgets for teams or projects: you set a spending limit for the "prod" segment, and the system enforces it across all containers in that group.

Without segments, you have two levels of control:

```
Host (all containers)
  +-- container-a (per-container limits)
  +-- container-b
  +-- container-c
```

With segments, you add a middle layer:

```
Host (all containers)
  +-- Segment "prod"
  |     +-- container-a
  |     +-- container-b
  |
  +-- Segment "dev"
  |     +-- container-c
  |
  +-- container-d (no segment)
```

Key rules:
- A container belongs to **at most one** segment
- **Host limits** always apply to ALL containers, regardless of segment
- **Segment limits** apply on top of host limits, only to members of that segment
- If either the host limit or the segment limit is exceeded, enforcement kicks in

## Getting started

### Create a segment

```bash
ddl segments create prod --name "Production"
ddl segments create dev --name "Development"
```

The `id` is what you use in commands. The `--name` is an optional human-readable label.

### List segments

```bash
ddl segments list
```

```
ID    NAME         CREATED
prod  Production   2026-04-08T13:34:52Z
dev   Development  2026-04-08T13:35:00Z
```

### Assign containers to segments

```bash
ddl segments assign my-container prod
```

A container can only be in one segment at a time. To move it:

```bash
ddl segments unassign my-container prod
ddl segments assign my-container dev
```

You can also assign during registration:

```bash
ddl register --segment prod my-new-container
```

### View segment status

```bash
ddl segments show prod
```

```
Segment: prod (Production)
Containers: 3

Limits:
  spending: $10.00
  cpu: 24h0m0s
Usage:
  spending: $3.50
  cpu: 2h15m30s
```

## Setting segment limits

Segment limits work exactly like host limits, but scoped to the segment's containers.

```bash
ddl segments limits set prod spending 10.00
ddl segments limits set prod cpu 24h
ddl segments limits set prod ram 4g
```

To increase or decrease:

```bash
ddl segments limits increase prod spending 5.00
ddl segments limits decrease prod cpu 2h
```

View current limits:

```bash
ddl segments limits get prod
```

### How segment limits interact with host limits

Segment limits are **additive**, not exclusive. A container in segment "prod" is checked against:

1. Its own per-container limits
2. The segment "prod" limits (sum of all prod containers)
3. The host limits (sum of ALL containers on the machine)

If **any** of these is exceeded, enforcement applies. For example:

```
Host spending limit:    $50.00
Prod segment limit:     $20.00
Container-a limit:      $10.00
```

Container-a will be blocked if:
- Its own spending hits $10.00, OR
- Total prod segment spending hits $20.00, OR
- Total host spending hits $50.00

### View aggregated usage

```bash
ddl segments usage prod
```

```
TYPE       USAGE    SEGMENT LIMIT  STATUS
cpu        2h15m    24h            9.4%
ram        1.2G     4.0G           30.0%
spending   $3.50    $10.00         35.0%
...
```

## Scoping the CLI

Instead of typing `--segment prod` on every command, you can set a persistent scope:

```bash
ddl scope set prod
```

Now all commands automatically operate within the "prod" segment:

```bash
ddl ls              # only shows prod containers
ddl usage-all       # only prod container usage
ddl freeze-all      # only freezes prod containers
```

Check your current scope:

```bash
ddl scope
```

```
scope: segment "prod"
  (from /Users/you/.config/ddl/scope)
```

Clear it to go back to seeing everything:

```bash
ddl scope clear
```

### Scope resolution order

The CLI determines scope from (highest priority first):

1. `--segment` flag on the command
2. `DDL_SEGMENT` environment variable
3. Persisted scope (`ddl scope set`)
4. Host (default, all containers)

So even with a persisted scope, you can override for one command:

```bash
ddl --segment dev ls     # see dev containers, regardless of persisted scope
```

## Segment-scoped dashboard

### From the main dashboard

The main dashboard at `http://localhost:7124` shows all segments in a panel at the top. Each segment card has:

- **Set Limit** -- open a dialog to set/increase/decrease segment limits
- **View** -- opens a new tab with a dashboard scoped to that segment
- **Delete** -- remove the segment

The container table includes a **Segment** column showing which segment each container belongs to. Each container row has a **Segment** button to assign or reassign it.

### Opening a scoped dashboard directly

```bash
ddl dashboard --segment prod --open
```

Or navigate to `http://localhost:7124/?segment=prod` in your browser.

A scoped dashboard:
- Only shows containers in the segment
- Displays segment limits in the top panel (not host limits)
- Shows a scope badge in the header: **[prod]**
- All operations (freeze, set limits, register) are scoped to the segment

## Per-segment configuration

Segments can override host-level configuration. This lets you use different API keys, enable/disable providers, or set different webhooks per segment.

```bash
ddl segments config set prod anthropic-key sk-prod-xxx
ddl segments config set dev openai-enabled false
```

View effective configuration (host defaults + segment overrides):

```bash
ddl segments config get prod
```

Overridden keys are marked. Keys not overridden fall through to the host config.

Supported config keys:
- `anthropic-enabled`, `openai-enabled`, `ollama-enabled` -- toggle providers
- `anthropic-key`, `openai-key` -- API key overrides
- `ollama-models` -- restrict available models
- `error-webhooks` -- segment-specific error notification URLs

## Segment-scoped freeze/unfreeze

Freeze or unfreeze all containers in a segment at once:

```bash
ddl segments freeze-all prod
ddl segments unfreeze-all prod
```

This is useful for maintenance or when you need to pause a whole project's containers without affecting others.

## Scoped API listeners

For advanced isolation, you can spawn a separate API listener locked to a segment:

```bash
ddl scope listen prod --port 7200
```

This creates a persistent API endpoint that only serves data for the "prod" segment. Requests to other segments or host-level data are not accessible through this listener.

List active listeners:

```bash
ddl scope listeners
```

```
ID                      SCOPE         LISTEN
sl-1775655765953770466  segment:prod  [::]:7200
```

Stop a listener:

```bash
ddl scope unlisten sl-1775655765953770466
```

## Events

Segment operations emit events visible via `ddl events`:

| Event | When |
|---|---|
| `segment_create` | A segment is created |
| `segment_delete` | A segment is deleted |
| `container_assign` | A container is assigned to a segment |
| `container_unassign` | A container is removed from a segment |
| `scope_enforcement_change` | A segment (or host) limit is enforced or released |

Subscribe to segment events:

```bash
ddl events --types segment_create,segment_delete,container_assign,container_unassign
```

## Enforcement details

When a segment limit is exceeded:

- **CPU, network, disk I/O**: all containers in the segment are Docker-paused
- **Spending**: all proxy API calls for containers in the segment return HTTP 429
- **RAM**: kernel cgroup limit applied (OOM killer handles it)
- **Byte-second limits** (ram-usage-bsec, etc.): containers are killed via `docker stop`

A container won't be unpaused until ALL enforcement levels release it -- per-container, segment, and host.

### Accumulated usage from removed containers

When a container is removed from a segment (or from the system entirely), its cumulative usage (spending, CPU time, network, I/O) is preserved in a scope accumulator. This prevents "gaming" the system by removing and re-creating containers to reset usage counters.

## Deleting a segment

A segment can only be deleted if it has no containers assigned:

```bash
ddl segments unassign my-container prod
ddl segments delete prod
```

Deleting a segment also removes its limits and accumulated usage data.

## Quick reference

```bash
# Segment management
ddl segments create <id> [--name <name>]
ddl segments list
ddl segments show <id>
ddl segments delete <id>

# Container assignment
ddl segments assign <container> <segment>
ddl segments unassign <container> <segment>

# Segment limits
ddl segments limits set <segment> <type> <value>
ddl segments limits get <segment>
ddl segments limits increase <segment> <type> <value>
ddl segments limits decrease <segment> <type> <value>

# Segment usage
ddl segments usage <segment>

# Segment-wide actions
ddl segments freeze-all <segment>
ddl segments unfreeze-all <segment>

# Segment config
ddl segments config get <segment>
ddl segments config set <segment> <key> <value>

# CLI scope
ddl scope                              # show current scope
ddl scope set <segment-id|host>        # persist scope
ddl scope clear                        # clear persisted scope

# Scoped listeners
ddl scope listen <segment> --port <port>
ddl scope listeners
ddl scope unlisten <id>

# Scoped dashboard
ddl dashboard --segment <id> --open
```
