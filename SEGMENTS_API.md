# Segments API Reference

This document describes the REST API changes introduced by the segments feature.

## Backward compatibility

All existing endpoints continue to work:

- `GET/PUT /global-limits` -- still works, aliases `/host-limits`
- `GET /containers` -- response now includes both `global_*` and `host_*` fields, plus `segment_id` on each container
- `POST /register` -- accepts optional `segment_id` field

## Segment management

### List segments

```
GET /segments
```

Response `200`:
```json
{
  "segments": [
    {"id": "prod", "name": "Production", "created_at": "2026-04-08T13:34:52Z"},
    {"id": "dev", "name": "Development", "created_at": "2026-04-08T13:35:00Z"}
  ]
}
```

### Create segment

```
POST /segments
```

Request:
```json
{"id": "prod", "name": "Production"}
```

`name` is optional (defaults to `id`).

Response `201`:
```json
{"id": "prod", "name": "Production", "created_at": "2026-04-08T13:34:52Z"}
```

Errors: `400` missing id, `409` duplicate id.

### Get segment detail

```
GET /segments/{id}
```

Response `200`:
```json
{
  "segment": {"id": "prod", "name": "Production", "created_at": "..."},
  "limits": {"cpu": 86400, "spending": 1000000},
  "usage": {"cpu": 3600, "spending": 250000},
  "enforced": {"spending": true},
  "containers": 5
}
```

Usage is aggregated across all member containers plus accumulated usage from removed containers.

Errors: `404` segment not found.

### Delete segment

```
DELETE /segments/{id}
```

Response `200`:
```json
{"deleted": "prod"}
```

Errors: `409` segment has containers (unassign them first).

Side effects: removes segment limits and accumulated usage data.

## Segment limits

### Get segment limits

```
GET /segments/{id}/limits
```

Response `200`:
```json
{"cpu": 86400, "spending": 1000000}
```

### Set segment limit

```
PUT /segments/{id}/limits
```

Request:
```json
{"type": "spending", "value": 1000000, "operation": "set"}
```

`operation` is optional, defaults to `set`. Valid values: `set`, `increase`, `decrease`.

Response `200`:
```json
{
  "type": "spending",
  "value": 1000000,
  "old_value": 500000,
  "operation": "set",
  "applied": "full",
  "scope": "segment:prod"
}
```

Errors: `400` invalid type/operation, `404` segment not found.

## Segment usage

### Get aggregated segment usage

```
GET /segments/{id}/usage
```

Response `200`:
```json
{
  "usage": {"cpu": 3600, "ram": 2147483648, "spending": 250000},
  "limits": {"cpu": 86400, "spending": 1000000},
  "enforced": {"spending": false}
}
```

Usage includes per-container usage summed across all members, plus accumulated usage from containers that were removed while in this segment.

## Container assignment

### Assign container to segment

```
POST /segments/{id}/containers/{cid}/assign
```

No request body.

Response `200`:
```json
{"assigned": "prod", "container": "abc123def456"}
```

Errors:
- `404` segment or container not found
- `409` container already in a different segment (unassign first)

### Unassign container from segment

```
POST /segments/{id}/containers/{cid}/unassign
```

No request body.

Response `200`:
```json
{"unassigned": "prod", "container": "abc123def456"}
```

Errors:
- `404` container not found
- `409` container is not in the specified segment

### Register container with segment

```
POST /register
```

Request (extended):
```json
{"container_id": "abc123", "segment_id": "prod"}
```

`segment_id` is optional. If provided, the container is assigned to the segment on registration.

Errors: `404` segment not found (if segment_id provided).

## Segment containers

### List containers in segment

```
GET /segments/{id}/containers
```

Response `200`:
```json
{
  "containers": [
    {
      "container": {"id": "abc123", "docker_id": "...", "name": "web-1", "segment_id": "prod"},
      "limits": {"cpu": 3600},
      "usage": {"cpu": 1200},
      "enforced": {},
      "state": "running",
      "frozen": false
    }
  ],
  "host_limits": {"spending": 1000000},
  "host_usage": {"spending": 250000},
  "host_enforced": {},
  "global_limits": {"spending": 1000000},
  "global_usage": {"spending": 250000},
  "global_enforced": {},
  "scope": "segment:prod"
}
```

The `host_*`/`global_*` fields contain the **segment** aggregates (not host aggregates). This allows the dashboard to display segment limits in the same UI that normally shows host limits.

### Container operations via segment path

All standard container operations are available through the segment path. The container must be a member of the segment (returns `403` otherwise).

```
GET    /segments/{id}/containers/{cid}             -- container detail
PUT    /segments/{id}/containers/{cid}/limits       -- set container limit
GET    /segments/{id}/containers/{cid}/usage        -- container usage
GET    /segments/{id}/containers/{cid}/activity     -- proxy activity log
POST   /segments/{id}/containers/{cid}/freeze       -- freeze container
POST   /segments/{id}/containers/{cid}/unfreeze     -- unfreeze container
POST   /segments/{id}/containers/{cid}/clone        -- clone container
GET    /segments/{id}/containers/{cid}/ollama/bid   -- get ollama bid
PUT    /segments/{id}/containers/{cid}/ollama/bid   -- set ollama bid
DELETE /segments/{id}/containers/{cid}/ollama/queue  -- cancel ollama request
```

These delegate to the same handlers as `/containers/{cid}/*` after verifying segment membership.

## Segment freeze/unfreeze

### Freeze all containers in segment

```
POST /segments/{id}/freeze-all
```

Response `200`:
```json
{"frozen": ["abc123", "def456"], "count": 2}
```

### Unfreeze all containers in segment

```
POST /segments/{id}/unfreeze-all
```

Response `200`:
```json
{"unfrozen": ["abc123", "def456"], "count": 2}
```

## Segment configuration

Segments can override host-level configuration. Unset keys fall through to the host config.

### Get effective config

```
GET /segments/{id}/config
```

Response `200`:
```json
{
  "anthropic_enabled": true,
  "anthropic_key": "sk-***",
  "openai_enabled": true,
  "ollama_enabled": false,
  "_segment_overrides": {
    "anthropic_key": "sk-prod-xxx",
    "ollama_enabled": "false"
  }
}
```

The `_segment_overrides` field shows which keys are explicitly set on this segment. All other keys are inherited from the host config.

### Set segment config override

```
PUT /segments/{id}/config
```

Request:
```json
{"anthropic_key": "sk-prod-xxx", "ollama_enabled": "false"}
```

Response `200`:
```json
{"updated": 2, "segment": "prod"}
```

Supported config keys: `anthropic_enabled`, `openai_enabled`, `ollama_enabled`, `anthropic_key`, `openai_key`, `ollama_models`, `error_webhooks`.

## Scoped listeners

Scoped listeners are isolated API endpoints that only serve data for a specific scope. Ports 7200-7220 are pre-mapped on the daemon container.

### Create scoped listener

```
POST /scoped-listeners
```

Request:
```json
{"scope": "segment:prod", "listen": ":7200"}
```

Or with unix socket:
```json
{"scope": "segment:prod", "socket": "/run/ddl/prod.sock"}
```

Response `201`:
```json
{"id": "sl-1234567890", "scope": "segment:prod", "listen": "[::]:7200", "socket": ""}
```

Errors: `400` missing listen/socket, `404` segment not found.

The scoped listener serves a filtered API where only containers in the specified scope are visible.

### List scoped listeners

```
GET /scoped-listeners
```

Response `200`:
```json
{
  "listeners": [
    {"id": "sl-1234567890", "scope": "segment:prod", "listen": "[::]:7200", "socket": ""}
  ]
}
```

### Stop scoped listener

```
DELETE /scoped-listeners/{id}
```

Response `200`:
```json
{"deleted": "sl-1234567890"}
```

## Host/scope limits

### Host limits (preferred)

```
GET /host-limits
PUT /host-limits
```

Same behavior as `/global-limits`. Preferred name going forward.

### Scope limits

```
GET /scope-limits?scope=host
PUT /scope-limits?scope=host
```

Query parameter `scope` defaults to `host`. Currently only `host` scope is supported via this endpoint; use `/segments/{id}/limits` for segment scopes.

### Global limits (backward compat)

```
GET /global-limits
PUT /global-limits
```

Still works. Same handler as `/host-limits`.

## Events

New event types emitted by segment operations:

| Event type | Trigger | Payload |
|---|---|---|
| `segment_create` | Segment created | `{segment_id, name}` |
| `segment_delete` | Segment deleted | `{segment_id}` |
| `container_assign` | Container assigned to segment | `{segment_id}` |
| `container_unassign` | Container removed from segment | `{segment_id}` |
| `scope_enforcement_change` | Scope limit enforced/released | `{scope, limit_type, enforced}` |

The existing `global_enforcement_change` event continues to be emitted alongside `scope_enforcement_change` for backward compatibility.

## Containers response changes

`GET /containers` response now includes:

- `segment_id` field on each container (empty string if not in a segment)
- `host_limits`, `host_usage`, `host_enforced` fields (same data as `global_*`)
- `global_limits`, `global_usage`, `global_enforced` fields (kept for backward compat)

## Limit types

All endpoints that accept a limit `type` support:

| Type | Unit | Example |
|---|---|---|
| `cpu` | seconds | `86400` (24h) |
| `ram` | bytes | `4294967296` (4GB) |
| `disk` | bytes | `10737418240` (10GB) |
| `net` | bytes | `1073741824` (1GB) |
| `disk-io-bytes` | bytes | `5368709120` (5GB) |
| `disk-io-ops` | operations | `1000000` |
| `spending` | milli-cents | `1000000` ($10.00) |
| `ram-usage-bsec` | byte-seconds | `1099511627776` (1T) |
| `disk-usage-bsec` | byte-seconds | `1099511627776` (1T) |
| `ram-request-bsec` | byte-seconds | `1099511627776` (1T) |
| `disk-request-bsec` | byte-seconds | `1099511627776` (1T) |
