# Migration Guide: Global to Scope/Segments

This document describes the transition from "global" terminology and flat limits to the new scope-aware system with segment support.

## Summary

The system now supports **segments** — named groups of containers with their own limits, enforcement, and configuration. The old "global" concept is now called "host" (all containers on the machine). Both old and new names work during the transition.

## What changed

### Scope hierarchy

```
Host scope (all containers)
  |
  +-- Segment "prod" (subset of containers)
  |     +-- container-a
  |     +-- container-b
  |
  +-- Segment "dev" (another subset)
  |     +-- container-c
  |
  +-- container-d (no segment, host-only)
```

- **Host limits** apply to ALL containers (regardless of segment)
- **Segment limits** apply only to containers in that segment
- A container is checked against both its segment limits AND host limits (additive)
- A container belongs to at most one segment

### API endpoints

| Old | New (preferred) | Status |
|---|---|---|
| `GET/PUT /global-limits` | `GET/PUT /host-limits` | Both work |
| — | `GET/PUT /scope-limits?scope=...` | New |
| — | `POST/GET /segments` | New |
| — | `GET/DELETE /segments/{id}` | New |
| — | `GET/PUT /segments/{id}/limits` | New |
| — | `GET /segments/{id}/usage` | New |
| — | `GET /segments/{id}/containers` | New |
| — | `POST /segments/{id}/containers/{cid}/assign` | New |
| — | `POST /segments/{id}/containers/{cid}/unassign` | New |
| — | `POST /segments/{id}/freeze-all` | New |
| — | `POST /segments/{id}/unfreeze-all` | New |
| — | `GET/PUT /segments/{id}/config` | New |
| — | `POST/GET/DELETE /scoped-listeners` | New |

### JSON response fields

`GET /containers` now includes both old and new field names:

```json
{
  "containers": [...],
  "global_limits": {"cpu": 86400, "spending": 1000000},
  "global_usage": {"cpu": 3600},
  "global_enforced": {},
  "host_limits": {"cpu": 86400, "spending": 1000000},
  "host_usage": {"cpu": 3600},
  "host_enforced": {}
}
```

Consumers should migrate to `host_*` fields. The `global_*` fields will be removed in a future version.

### CLI commands

| Old | New (preferred) | Status |
|---|---|---|
| `ddl limits set-global` | `ddl limits set-host` | Old is hidden alias |
| `ddl limits get-global` | `ddl limits get-host` | Old is hidden alias |
| `ddl limits increase-global` | `ddl limits increase-host` | Old is hidden alias |
| `ddl limits decrease-global` | `ddl limits decrease-host` | Old is hidden alias |
| `ddl usage-global` | `ddl usage-host` | Old is hidden alias |

New commands:
```
# Segment management
ddl segment create/list/delete/show/assign/unassign

# Segment limits, usage, freeze (explicit target)
ddl limits set-segment/get-segment/increase-segment/decrease-segment
ddl usage-segment <segment>
ddl freeze-segment/unfreeze-segment <segment>

# Scope-aware commands (target current scope: host or segment)
ddl limits set-all/get-all/increase-all/decrease-all
ddl usage-all, ddl ls, ddl freeze-all, ddl unfreeze-all

# CLI scope persistence
ddl scope set/clear/show

# Scoped API listeners
ddl scope listen/listeners/unlisten
```

### Events

| Old | New | Status |
|---|---|---|
| `global_enforcement_change` | `scope_enforcement_change` | Both emitted |

New event types: `segment_create`, `segment_delete`, `container_assign`, `container_unassign`

### Database

Automatic migration on daemon startup:
- New tables: `scope_limits`, `scope_usage_accum`, `segments`, `segment_config`
- Old tables kept: `global_limits`, `global_usage_accum` (kept in sync)
- New column: `containers.segment_id`

No manual migration steps required.

## Using segments

### Create segments and assign containers

```bash
ddl segment create prod --name "Production"
ddl segment assign my-container prod
```

### Set segment limits

```bash
ddl limits set-segment prod spending 10.00
ddl limits set-segment prod cpu 24h
```

### View segment status

```bash
ddl segment show prod
ddl usage-segment prod
```

### Scope the CLI to a segment

```bash
ddl scope set prod          # persist scope
ddl --segment prod ls       # one-off scope
DDL_SEGMENT=prod ddl ls     # env-based scope

# With scope set, -all commands target the segment:
ddl limits set-all spending 10.00   # sets prod segment limit
ddl usage-all                        # shows prod containers
ddl freeze-all                       # freezes prod containers
```

### Scoped API listeners

```bash
ddl scope listen prod --port 7200   # isolated API on :7200
ddl scope listeners                  # list active
ddl scope unlisten <id>              # stop
```

### Scoped dashboard

```bash
ddl dashboard --segment prod   # opens dashboard for prod only
```

### Per-segment config

Per-segment config is managed via the REST API:

```bash
curl -X PUT localhost:7123/segments/prod/config \
  -H 'Content-Type: application/json' \
  -d '{"anthropic_key": "sk-prod-xxx"}'
```

## Timeline

- **Current**: Both `global_*` and `host_*` names work everywhere
- **Future**: `global_*` names will be deprecated with warnings
- **Later**: `global_*` names removed
