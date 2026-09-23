# Coding Hermes Scheduler — REST API Reference

Live-verified 2026-08-18 against `schedulerd` on `127.0.0.1:9090` (GAP-056). Field
names below were confirmed against real daemon responses via `curl`; the machine-
readable OpenAPI 3.0 spec is served by the daemon itself at
`GET /api/v1/openapi.json`. Design rationale lives in `specs/S06-rest-api.md`;
operational examples in `docs/integration.md`.

## 1. Overview

The scheduler daemon exposes a JSON REST API under `/api/v1` for managing the
fleet: projects, namespaces, ticks, the event log, the scheduling queue, and
fleet-wide control (pause/resume/evaluate). It listens on loopback only by
default (`--listen 127.0.0.1:9090`).

| Thing | Value |
|-------|-------|
| Base URL | `http://127.0.0.1:9090` |
| API root | `/api/v1` |
| OpenAPI spec | `GET /api/v1/openapi.json` |
| Content type | `application/json` (request and response) |
| HTML dashboard | `/`, `/dashboard/partial`, `/projects/{name}`, `/queue`, `/ticks?page=N`, `/namespaces/{id}`, `/health` (not part of the JSON API) |
| MCP (JSON-RPC) | `POST /mcp` (not part of the REST API) |

Route index (every `/api/v1/*` route documented below; the JSONL deploy
groups/templates routes are listed in the OpenAPI spec at
`GET /api/v1/openapi.json`):

| Method | Path | Section |
|--------|------|---------|
| GET | `/api/v1/health` | [§4](#4-health-status-config) |
| GET | `/api/v1/live` | [§4](#4-health-status-config) |
| GET | `/api/v1/status` | [§4](#4-health-status-config) |
| GET | `/api/v1/config` | [§4](#4-health-status-config) |
| GET, POST | `/api/v1/projects` | [§5](#5-projects) |
| GET, PUT, DELETE | `/api/v1/projects/{name}` | [§5](#5-projects) |
| POST | `/api/v1/projects/{name}/pause` | [§5](#5-projects) |
| POST | `/api/v1/projects/{name}/resume` | [§5](#5-projects) |
| POST | `/api/v1/projects/{name}/spawn` | [§5](#5-projects) |
| POST | `/api/v1/projects/{name}/bump` | [§5](#5-projects) |
| POST | `/api/v1/projects/{name}/unbump` | [§5](#5-projects) |
| GET, POST | `/api/v1/namespaces` | [§6](#6-namespaces) |
| GET, PUT, DELETE | `/api/v1/namespaces/{id}` | [§6](#6-namespaces) |
| GET | `/api/v1/namespaces/{id}/projects` | [§6](#6-namespaces) |
| POST | `/api/v1/namespaces/{id}/move` | [§6](#6-namespaces) |
| GET | `/api/v1/ticks` | [§7](#7-ticks) |
| GET | `/api/v1/ticks/{id}` | [§7](#7-ticks) |
| GET | `/api/v1/events` | [§8](#8-events) |
| GET | `/api/v1/events/stream` | [§8](#8-events) |
| GET | `/api/v1/queue` | [§9](#9-queue) |
| GET | `/api/v1/metrics` | [§10](#10-fleet-metrics) |
| POST | `/api/v1/evaluate` | [§11](#11-fleet-wide-control) |
| POST | `/api/v1/pause` | [§11](#11-fleet-wide-control) |
| POST | `/api/v1/resume` | [§11](#11-fleet-wide-control) |

## 2. Wire format

- **Responses are snake_case.** Every field is emitted with snake_case keys
  (`cooldown_s`, `repo_url`, `decay_rate`, `created_at`, …). This is enforced
  by the JSON struct tags in `internal/database/models.go` (SCHED-GAP-011).
- **Requests accept two dialects.** Project request bodies (POST
  `/api/v1/projects`, PUT `/api/v1/projects/{name}`) accept canonical
  snake_case keys AND the legacy PascalCase Go field names (`Name`, `RepoURL`,
  `CooldownS`, `Enabled`, …) for pre-conformance automation — both decode,
  snake_case wins when a field is supplied twice. Namespace bodies are
  snake_case only.
- **Timestamps are RFC3339 strings** (`2026-08-18T06:52:15Z`); durations in
  `/api/v1/config` are Go duration strings (`"30s"`, `"2h0m0s"`).
- **Errors** are always `{"error": "<message>"}` with a non-2xx status code.

## 3. Common error codes

| Code | Meaning | Typical triggers |
|------|---------|------------------|
| 400 | Validation failure | Malformed JSON, missing required field, out-of-range value (weight 1..100, priority 1..10, `decay_rate <= 0`), DELETE without `confirm=true` |
| 404 | Resource not found | Unknown project/tick/namespace name, unknown sub-route (`{"error":"project not found"}`, `{"error":"tick not found"}`, `{"error":"namespace not found"}`) |
| 405 | Wrong HTTP method | e.g. `DELETE /api/v1/ticks/foo` → `{"error":"GET only"}`; each route advertises its methods in the sections below |
| 409 | Conflict | Duplicate project/namespace name, workdir already registered by an enabled project, deleting an **enabled** project |
| 500 | Server/database error | SQLite failures; also pause/resume on a missing project (see §5) |
| 201 / 202 | Success with resource creation / accepted | POST create → 201; POST spawn → 202 |

Routes are case-sensitive: `GET /api/v1/projects/heading` 404s if the project
is stored as `HEADING`.

## 4. Health, status, config

### GET /api/v1/health

**Purpose:** Daemon liveness + DB connectivity + eval-loop freshness. The
canonical "is the scheduler alive" probe.

**Query params:** none. **Request body:** none.

**Response 200:**

```json
{"active_ticks":1,"db":"connected","evaluation_age_seconds":185.1,
 "last_evaluation":"2026-08-18T06:52:15Z","spawns_exec":0,"spawns_http":3,
 "status":"ok","uptime":"18m35.148135137s"}
```

| Field | Type | Meaning |
|-------|------|---------|
| `status` | string | `"ok"` when the daemon is up |
| `uptime` | string | Go duration since daemon start (`"18m35s"`) |
| `db` | string | `"connected"`, or `"error: <detail>"` when the SQLite ping fails |
| `active_ticks` | int | Ticks currently in `running` status |
| `last_evaluation` | string | RFC3339 of the last evaluation cycle; `""` if never evaluated |
| `evaluation_age_seconds` | float | Seconds since last evaluation (0 when never evaluated — compare against this, not the empty `last_evaluation`) |
| `spawns_http` / `spawns_exec` | int | Cumulative tick spawns via gateway HTTP vs `exec.Command` fallback. Fleet norm is HTTP rising, exec 0 |

**Errors:** 405 `{"error":"GET only"}` on non-GET.

```bash
curl -s http://127.0.0.1:9090/api/v1/health
```

### GET /api/v1/live

**Purpose:** DB-free liveness probe (SCHED-GAP-204-A). Process-memory only —
`status`, `version`, `build_sha`, `uptime`, `started` — and must never touch
the SQLite connection, the loop, or any DB-backed helper. The ops watchdog
probes it FIRST so a saturated fleet (single serialized SQLite connection,
busy_timeout=5000) does not false-fire a `Scheduler DOWN` CRITICAL just
because `/api/v1/health` is stuck in DB-connection contention. `/api/v1/health`
remains the rich DB-backed fallback for dashboards and post-mortem detail.

**Query params:** none. **Request body:** none.

**Response 200:**

```json
{"status":"ok","version":"v1.4.0-29-g5a5a47c2","build_sha":"5a5a47c2",
 "uptime":"3h42m11.5s","started":"2026-09-22T18:13:09Z"}
```

| Field | Type | Meaning |
|-------|------|---------|
| `status` | string | `"ok"` when the handler returns |
| `version` | string | Resolved daemon version (same identity as `/api/v1/health.version` / openapi.info.version / MCP `serverInfo.version`; ldflags tag → vcs buildinfo → `dev`) |
| `build_sha` | string | Short commit SHA the binary was built from (best-effort; empty on `dev` builds) |
| `uptime` | string | Go duration since daemon start |
| `started` | string | RFC3339 UTC of daemon start |

**Errors:** 405 `{"error":"GET only"}` on non-GET.

```bash
curl -s http://127.0.0.1:9090/api/v1/live
```

### GET /api/v1/status

**Purpose:** Fleet overview — budget, active projects/ticks, recent outcome
counts, and a per-project failure-rate breakdown (SCHED-GAP-018) with
auto-disable arming (GAP-047). Also carries the gateway-health admission gate's
armed state (`gateway_health_gate`, SCHED-GAP-170).

**Query params:** none. **Request body:** none.

**Response 200** (key fields; `duckbrain` and `zero_select_*` are conditional):

```json
{"active_projects":44,"active_ticks":1,
 "auto_disable":{"enabled":true,"min_ticks":50,"threshold":0.9,"window":100},
 "budget_total":100,
 "duckbrain":{"base_url":"http://localhost:3000","consecutive_failures":0,
   "interval":"5m0s","last_error":"","last_ok_at":"...","reachable":true,
   "spooled_pending":0},
 "failure_window":100,
 "gateway_health_gate":{"armed":true,"deferrals_total":0,"healthy":true,
   "last_error":"","probed_at":"2026-09-19T07:58:12Z","ttl_s":30},
 "last_evaluation":"2026-08-18T06:52:15Z",
 "projects_failure_rates":{"9router":{"failed":1,"total":100,
   "failure_rate":0.01,"auto_disable_armed":false}, ...},
 "recent_outcomes":{"completed":401,"failed":6,"timeout":1},
 "zero_select_consecutive":0,"zero_select_eligible":0,"zero_select_last_at":""}
```

| Field | Type | Meaning |
|-------|------|---------|
| `budget_total` | int | Weight budget (default 100) |
| `active_projects` | int | Number of enabled projects |
| `active_ticks` | int | Running ticks right now |
| `recent_outcomes` | object | Counts of completed/failed/timeout ticks with a `completed_at` |
| `projects_failure_rates` | object | Map `project_name → {failed, total, failure_rate, auto_disable_armed}` over the last `failure_window` ticks per project; `failed` counts `failed`+`timeout`; only existing projects appear (purged projects never resurface) |
| `failure_window` | int | Window size used for the breakdown (default 100, `--failure-window`) |
| `last_evaluation` | string | RFC3339 of last evaluation |
| `auto_disable` | object | `{enabled, threshold, window, min_ticks}` — the auto-disable policy snapshot; `enabled` is false when `--auto-disable-failure-rate` is 0 |
| `zero_select_consecutive` / `zero_select_eligible` / `zero_select_last_at` | int / int / string | Zero-select diagnostics (present when the loop is attached): consecutive evals that selected nothing despite eligible projects, eligible count at the last one, and its timestamp |
| `gateway_health_gate` | object | Gateway-health admission gate (SCHED-GAP-170), always present (package state, not loop state): `{armed, healthy, probed_at, last_error, ttl_s, deferrals_total}`. `armed` is true exactly when a gateway client is INSTALLED on the gate; **armed=false means the gate fails open** — no probe, no deferrals, every spawn goes straight to the gateway. `healthy` is the raw cached verdict: false when the last probe failed (`last_error` names it, `probed_at` is when) and also when there is no verdict yet (`probed_at` `""` on a cold cache or an unarmed gate) — read `armed`/`probed_at` with it, never `healthy` alone. `ttl_s` is the verdict cache window (30). `deferrals_total` counts deferral decisions since daemon start (monotonic; a deferred project creates no tick row, takes no slot, and charges no cooldown) |
| `duckbrain` | object | DuckBrain sync health `{base_url, consecutive_failures, interval, last_error, last_ok_at, reachable, spooled_pending}` (present when sync health reporting is configured) |

**Errors:** 405 on non-GET.

```bash
curl -s http://127.0.0.1:9090/api/v1/status | jq '.active_projects, .projects_failure_rates["asce"]'
```

### GET /api/v1/config

**Purpose:** Read-only snapshot of the daemon's ACTIVE resolved configuration
(three layers: TOML file < env vars < CLI flags), captured at startup
(SCHED-GAP-034). The gateway key is masked — the plaintext never reaches the wire.

**Query params:** none. **Request body:** none.

**Response 200:**

```json
{"db_path":"/home/kara/.hermes/coding-hermes/scheduler.db","listen":"127.0.0.1:9090",
 "min_interval":"30s","max_interval":"24h0m0s","num_levels":10,
 "weight_budget":100,"max_concurrent":4,"tick_timeout":"2h0m0s",
 "namespace_mode":true,"auto_disable_failure_rate":0.9,
 "auto_disable_window":100,"auto_disable_min_ticks":50,"failure_window":100,
 "gateway":{"url":"http://127.0.0.1:8642","key":"WZJh****",
   "foreman_home":"/home/kara/.hermes/foreman","no_exec_fallback":true},
 "duckbrain":{"namespace":"scheduler","url":"http://localhost:3000"}}
```

| Field | Type | Meaning |
|-------|------|---------|
| `db_path`, `listen`, `min_interval`, `max_interval`, `tick_timeout` | string | Paths/address/durations (`min_interval` "30s", `tick_timeout` "2h0m0s") |
| `num_levels`, `weight_budget`, `max_concurrent` | int | Priority levels, budget, max parallel foremen |
| `namespace_mode` | bool | Multi-namespace weight allocation enabled |
| `auto_disable_failure_rate` | float | Auto-disable threshold (0 = feature off) |
| `auto_disable_window`, `auto_disable_min_ticks`, `failure_window` | int | Auto-disable and failure-rate windows |
| `gateway` | object | `{url, key, foreman_home, no_exec_fallback}` — `key` is masked (first 4 chars + `"****"`, empty when unset) |
| `duckbrain` | object | `{namespace, url}` sync target |

**Errors:** 405 on non-GET.

```bash
curl -s http://127.0.0.1:9090/api/v1/config | jq '{min_interval, max_concurrent, gateway: .gateway.url}'
```

## 5. Projects

A project is one managed codebase the scheduler may spawn ticks against.
Full Project model (response shape, snake_case):

| Field | Type | Meaning |
|-------|------|---------|
| `name` | string | Primary key; also the DuckBrain project key |
| `repo_url` | string | Git clone URL (often `local:/home/kara/<proj>`) |
| `workdir` | string | Absolute path to the working copy |
| `weight` | int | Budget units consumed per tick (1..100, default 10) |
| `priority` | int | Base urgency multiplier (1..10, default 5) |
| `cooldown_s` | int | Seconds between successive ticks (default 900) |
| `decay_rate` | float | Urgency decay rate (default 1.0; must be > 0) |
| `model`, `provider` | string | LLM model/provider passed to the spawned agent |
| `worker_model`, `worker_provider` | string | Optional suggested worker model/provider |
| `gateway_key` | string | Per-foreman Hermes gateway key; empty = daemon's shared key |
| `command` | string | Optional custom spawn command |
| `namespace_id` | string \| null | FK → namespaces.id; null = unscheduled in namespace mode |
| `deliver` | string | Delivery target `platform:chat_id:thread_id` |
| `enabled` | bool | Disabled projects are never scheduled |
| `created_at`, `updated_at` | string | RFC3339 |
| `last_tick_started`, `last_tick_completed` | string | RFC3339; `""` when never |
| `disabled_at`, `disabled_by`, `disabled_reason` | string | Disable provenance (GAP-044); empty while enabled/never disabled; `disabled_by` ∈ `api` \| `api-pause` \| `api-delete` \| `auto-disable` |
| `consecutive_failures` | int | Internal spawn-failure counter (drives selection backoff; not user-editable) |

### GET /api/v1/projects

**Purpose:** List all projects (enabled AND disabled), ordered by name.

**Query params:** none. **Request body:** none.

**Response 200:** `{"projects": [<Project>, ...]}` — empty list when none.

```bash
curl -s http://127.0.0.1:9090/api/v1/projects | jq '.projects[0].cooldown_s'
# 3600
```

**Errors:** 405 on non-GET.

### POST /api/v1/projects

**Purpose:** Create a project. **New projects are created DISABLED** —
resume them explicitly before expecting ticks.

**Request body** (canonical snake_case; PascalCase legacy accepted):

```json
{"name":"my-project","repo_url":"local:/home/kara/my-project",
 "workdir":"/home/kara/my-project"}
```

| Field | Required | Default |
|-------|----------|---------|
| `name`, `repo_url`, `workdir` | **yes** | — |
| `weight` | no | 10 |
| `priority` | no | 5 |
| `cooldown_s` | no | 900 |
| `decay_rate` | no | 1.0 |
| `enabled` | no | **false** (creating never auto-enables) |
| `model`, `provider`, `worker_model`, `worker_provider`, `gateway_key`, `command`, `namespace_id`, `deliver` | no | zero value / null |

**Response 201:** the created project object (flat, with `created_at`/`updated_at`
stamped).

**Errors:**

| Case | Status | Body |
|------|--------|------|
| Malformed JSON | 400 | `{"error":"invalid JSON: <detail>"}` |
| Missing `name`/`repo_url`/`workdir` | 400 | `{"error":"name, repo_url, workdir are required"}` |
| CHECK violation (weight 1..100, priority 1..10, decay_rate > 0) | 400 | `{"error":"invalid project fields: weight must be 1..100; priority 1..10; decay_rate > 0"}` |
| Duplicate name (case-insensitive) | 409 | `{"error":"project already exists"}` |
| Workdir (or name) already registered by an **enabled** project | 409 | `{"error":"create project ... already registered by enabled project ... (case-insensitive duplicate)"}` |
| Wrong method | 405 | `{"error":"GET or POST only"}` |

```bash
curl -s -X POST http://127.0.0.1:9090/api/v1/projects \
  -H 'Content-Type: application/json' \
  -d '{"name":"my-project","repo_url":"local:/home/kara/my-project","workdir":"/home/kara/my-project"}'
# 201 {"name":"my-project","weight":10,"priority":5,"cooldown_s":900,
#      "decay_rate":1,"enabled":false,"created_at":"2026-08-18T...Z", ...}
```

### GET /api/v1/projects/{name}

**Purpose:** Single project detail wrapped under a `project` key alongside its
latest tick.

**Path params:** `name` — exact project name (case-sensitive).

**Response 200:**

```json
{"project": {<Project>}, "latest_tick": {<Tick>}}
```

`latest_tick` is `null` when the project has never been ticked. Note: the
wrapped shape differs from POST/PUT, which return the project flat — do not
copy one parsing shape into the other.

**Errors:** 404 `{"error":"project not found"}`; 405 on non-GET.

```bash
curl -s http://127.0.0.1:9090/api/v1/projects/9router | jq '{name: .project.name, last: .latest_tick.status}'
```

### PUT /api/v1/projects/{name}

**Purpose:** Partial update. Only fields present in the body are applied
(pointer semantics — omit fields to leave them untouched).

**Path params:** `name` — exact project name.

**Request body** — all fields optional (snake_case; PascalCase legacy accepted):

```json
{"cooldown_s":14400,"enabled":true}
```

| Field | Notes |
|-------|-------|
| `repo_url`, `workdir`, `model`, `provider`, `worker_model`, `worker_provider`, `gateway_key`, `command` | String fields; `gateway_key` `""` clears back to the shared key |
| `weight` | 1..100 |
| `priority` | 1..10 |
| `cooldown_s` | Seconds |
| `decay_rate` | **must be > 0** — see guard below |
| `namespace_id` | Set to `""` to unassign from a namespace |
| `enabled` | `false` = disable (stamps `disabled_at`/`disabled_by`/`disabled_reason`, defaults `now`/`"api"`/`"disabled via API update"`, and writes an events entry); `true` = resume (clears provenance) |
| `disabled_at`, `disabled_by`, `disabled_reason` | Optional provenance overrides for the disable transition |

**Response 200:** the updated project object (flat).

**Errors:**

| Case | Status | Body |
|------|--------|------|
| Malformed JSON | 400 | `{"error":"invalid JSON: <detail>"}` |
| `decay_rate <= 0` | 400 | `{"error":"decay_rate must be > 0 (0 causes permanent starvation — urgency never grows)"}` |
| CHECK violation (weight/priority ranges) | 400 | `{"error":"invalid project fields: weight must be 1..100; priority 1..10; decay_rate > 0"}` |
| Unknown project | 404 | `{"error":"project not found"}` |
| Wrong method | 405 | `{"error":"GET, PUT, POST, or DELETE only"}` |

```bash
curl -s -X PUT http://127.0.0.1:9090/api/v1/projects/my-project \
  -H 'Content-Type: application/json' -d '{"cooldown_s":14400,"enabled":true}'
# 200 — updated project object
```

### DELETE /api/v1/projects/{name}

**Purpose:** Remove a project. `?confirm=true` soft-deletes (`enabled=false`,
row retained so historical ticks stay valid); `?confirm=true&purge=true`
permanently removes the row (DOGFOOD-009 — historical ticks keep the
`project_name` string and never resurface in failure rates).

**Query params:**

| Param | Required | Meaning |
|-------|----------|---------|
| `confirm` | **yes** | Must be `true` — guards against stray DELETEs (purge has its own confirm; `purge=true` alone is refused) |
| `purge` | no | `true` = hard-delete the row |

**Response 200:** `{"status":"deleted","project":"<name>"}` or
`{"status":"purged","project":"<name>"}`.

**Errors:**

| Case | Status | Body |
|------|--------|------|
| Missing `confirm=true` | 400 | `{"error":"confirm=true query param required — this soft-deletes the project (enabled=false); add purge=true to permanently remove the row"}` |
| Unknown project | 404 | `{"error":"project not found"}` |
| Project is **enabled** | 409 | `{"error":"project is enabled — pause it first (PUT Enabled=false or POST /projects/{name}/pause) before deleting"}` |
| Wrong method | 405 | `{"error":"GET, PUT, POST, or DELETE only"}` |

```bash
# Pause first, then soft-delete
curl -s -X POST http://127.0.0.1:9090/api/v1/projects/my-project/pause
curl -s -X DELETE "http://127.0.0.1:9090/api/v1/projects/my-project?confirm=true"
# {"project":"my-project","status":"deleted"}
```

### POST /api/v1/projects/{name}/pause

**Purpose:** Disable one project (it stops being scheduled). Stamps
provenance `disabled_by="api-pause"` and writes an events entry.

**Response 200:** `{"status":"paused","project":"<name>"}`.

**Errors:** 405 on non-POST. A missing project surfaces as **500** with
`{"error":"update project \"<name>\": project not found: <name>"}` — this
sub-route does not map to 404 (see also resume).

### POST /api/v1/projects/{name}/resume

**Purpose:** Re-enable a paused project; clears disable provenance.

**Response 200:** `{"status":"resumed","project":"<name>"}`.

**Errors:** 405 on non-POST; missing project → 500 (as with pause).

### POST /api/v1/projects/{name}/spawn

**Purpose:** Manually trigger a tick for one project. The tick is enqueued
synchronously and the returned `tick_id` is the REAL stored row id
(DOGFOOD-015) — `GET /api/v1/ticks/{tick_id}` resolves it immediately
(status `queued` until the spawn session picks it up). The id is the
canonical UTC `<name>-<YYYY>-<MM>-<DD>-<HH>-<MM>-<SS>` (same generator the
scheduler's own spawn path uses), so it never drifts from the stored row on
non-UTC hosts. Actual spawn still respects scheduler state (cooldown,
budget, namespace); a project that already has a queued/running tick is
refused with 409 (duplicate-spawn protection, SCHED-GAP-030).

**Response 202:**

```json
{"status":"spawned","project":"my-project",
 "tick_id":"my-project-2026-08-18-06-55-00"}
```

**Errors:** 404 `{"error":"project not found"}`; 409 `{"error":"project already has a tick in flight"}`; 405 on non-POST.

```bash
curl -s -X POST http://127.0.0.1:9090/api/v1/projects/my-project/spawn
# 202 {"status":"spawned","project":"my-project","tick_id":"my-project-..."}
curl -s http://127.0.0.1:9090/api/v1/ticks/my-project-2026-08-18-06-55-00
# 200 — the returned tick_id always resolves
```

### POST /api/v1/projects/{name}/bump

**Purpose:** Temporarily accelerate a project (SCHED-GAP-107): run it at a
small cooldown for N completed ticks, then auto-revert. Replaces manual
cooldown hand-PUTs with a first-class, self-expiring mechanism. The bump
overrides both the legacy `cooldown_s` and the adaptive floor/ceiling while
active and carries a large urgency boost (same class as the pending-work
boost), so the bumped project actually gets its fast ticks. Normal adaptive
tick-end evaluation CONTINUES during the bump — real work extends speed,
idle ticks burn bump ticks without extending anything. At expiry the
two-phase revert runs: Phase A restores the saved pre-bump state
(`cooldown_s`, floor, ceiling, `no_progress_ticks`), Phase B immediately
re-runs the adaptive evaluation over the last tick outcome — real work
lands the project at the floor, idle bumps resume the decay law exactly
where it was. A 12h hard cap force-reverts a stuck bump regardless of
remaining ticks.

**Body:**

```json
{"ticks": 5, "cooldown": 7200, "reason": "clear the injected backlog"}
```

| Field | Type | Meaning |
|-------|------|---------|
| `ticks` | int | Completed bump ticks before auto-revert. 1..8, default 5 |
| `cooldown` | int | Bump cooldown seconds. >= 7200 (the 6h cooldown law floor applies to bumps), default 7200 |
| `reason` | string | REQUIRED — every bump is auditable; empty/whitespace is a 400 |

**Response 200:** the updated project — `bump_active=true`,
`bump_remaining_ticks=N`, `cooldown_s` already at the bump value
(immediate effect), and the `bump_saved_*` snapshot of the pre-bump policy.

**Errors:** 400 (missing `reason`, `ticks` outside 1..8, `cooldown` < 7200);
404 unknown project; 409 project disabled (a paused project cannot consume
bump ticks) or a bump is already active (let it expire or `unbump`).

Bump state is pure DB — it survives daemon restarts with the remaining
count intact, and `GET /api/v1/status` surfaces active bumps in a `bumps`
array (`[{project, remaining_ticks, cooldown_s, reason, started_at}]`).

### POST /api/v1/projects/{name}/unbump

**Purpose:** Manually abort an active bump. Phase A only — restores the
saved pre-bump cooldown policy verbatim; no adaptive re-evaluation runs,
because an explicit cancel wants the pre-bump state back, not a fresh
verdict. Errors: 404 unknown project; 409 no active bump.

## 6. Namespaces

A namespace is a weight pool for related projects, with a reserved floor and
hard cap (S07). Namespace model:

| Field | Type | Meaning |
|-------|------|---------|
| `id` | string | Primary key — unique slug (e.g. `"coding-hermes"`) |
| `weight` | int | 1..100, relative weight for proportional allocation |
| `reserved` | int | >= 0, guaranteed floor budget units |
| `hard_cap` | int | >= 0, maximum budget; 0 = no cap (interpret as B) |
| `max_concurrent` | int | >= 0, **ticks this namespace may have in flight at once**; 0 = unlimited |
| `admission_mode` | string | `cooldown` (default) or `tasks` — see "Admission and concurrency" below |
| `load_gate` | string | `off` opts the namespace out of the daemon-wide load gate (SCHED-GAP-125) |
| `default_prompt` | string | Base prompt every project in the namespace inherits (project `prompt` appends to it, or replaces it per `prompt_mode`) |
| `model_chain` | string | Optional per-namespace chain override (JSON) |
| `enabled` | bool | Disabled namespaces get zero allocation |
| `description` | string | Human-readable label |
| `created_at`, `updated_at` | string | RFC3339 |

### Admission and concurrency (the current fleet shape)

Three dials, set by the operator — not defaults:

| Dial | Value | Where |
|------|-------|-------|
| Global slot pool | 10 | daemon flag `--max-concurrent` (systemd user unit) |
| `coding-hermes` (foremen) | 8 | namespace `max_concurrent` |
| Each satellite namespace (`qa`, `pm`, `dogfood`, `duckbrain-sync`, `releases`) | 1 | namespace `max_concurrent` |

The shape is deliberate: a satellite can hold at most one global slot, so the
foremen always have room. (`coding-hermes` = 8 + at most one per satellite ≤ 13
possible, inside the global 10 only when the budget is free — the packer admits
by namespace weight and the pool's `--max-concurrent` is the hard ceiling.)

`admission_mode` decides when a namespace's projects may be admitted:

- `cooldown` — wall-clock only: a project is admitted once `cooldown_s` has
  elapsed since its last completed tick. This is the default and the right mode
  for satellite lanes (their work is cadence-driven).
- `tasks` — admit immediately while the project's **own** board still has real
  (non-perpetual) rows to work, then fall back to the cooldown timer when the
  board is drained or holds only perpetual rows. A lane only qualifies when it
  OWNS the board it reads (SCHED-GAP-141, migration v32 `projects.board_ownership`:
  `''` = derived by full symlink resolution, `owner`, `shared`) — a satellite
  reading its primary's board through a symlinked `board/` directory does not.

An API `PUT` is live immediately and the `fleet.toml` regen mirrors it, so a
restart re-pins the operator's choice. Change a namespace cap with
`PUT /api/v1/namespaces/{id}` `{"max_concurrent": N}`; change one lane's
admission with `PUT /api/v1/projects/{name}` `{"admission_mode": "cooldown"}`.

For the full conceptual model behind these two dials — where each knob physically
lives, which store wins on restart, the board-ownership precondition on `tasks`,
the 6h cooldown floor and the anti-snap rule — see
[docs/fleet-config-model.md](fleet-config-model.md); every claim there is paired
with the command that proves it.

### GET /api/v1/namespaces

**Purpose:** List all namespaces, ordered by id. **Response 200:**
`{"namespaces": [<Namespace>, ...]}`. **Errors:** 405 on non-GET.

### POST /api/v1/namespaces

**Purpose:** Create a namespace.

**Request body:**

```json
{"id":"data-cleanup","weight":10,"reserved":0,"hard_cap":15,"enabled":true,
 "description":"Data retention, compaction"}
```

| Field | Required | Notes |
|-------|----------|-------|
| `id` | **yes** | Unique slug |
| `weight` | **yes** | Must be > 0 |
| `reserved`, `hard_cap`, `enabled`, `description` | no | Zero-value defaults (`0` / `false` / `""`) |

**Response 201:** the created namespace (flat). **Errors:** 400
`{"error":"id is required"}`; 400 `{"error":"weight must be greater than 0"}`;
400 invalid JSON; 409 `{"error":"namespace already exists"}`; 405
`{"error":"GET or POST only"}`.

```bash
curl -s -X POST http://127.0.0.1:9090/api/v1/namespaces \
  -H 'Content-Type: application/json' \
  -d '{"id":"data-cleanup","weight":10,"description":"Data retention"}'
# 201 {"id":"data-cleanup","weight":10,"reserved":0,"hard_cap":0,"enabled":false,...}
```

### GET /api/v1/namespaces/{id}

**Purpose:** Single namespace detail (flat object). **Errors:** 404
`{"error":"namespace not found"}`; 405 on non-GET.

### PUT /api/v1/namespaces/{id}

**Purpose:** Partial namespace update — only supplied fields are applied.

**Request body** (all optional): `weight` (int), `reserved` (int),
`hard_cap` (int), `enabled` (bool), `description` (string).

**Response 200:** the updated namespace (flat). **Errors:** 400 invalid JSON;
404 `{"error":"namespace not found"}`; 405 `{"error":"GET or PUT only"}`.

```bash
curl -s -X PUT http://127.0.0.1:9090/api/v1/namespaces/data-cleanup \
  -H 'Content-Type: application/json' -d '{"hard_cap":20,"enabled":true}'
```

### DELETE /api/v1/namespaces/{id}

**Purpose:** Remove a namespace. `?confirm=true` soft-deletes
(`enabled=false`, member projects unassigned — `namespace_id` → NULL — row
retained so historical namespace_ticks stay referentially valid);
`?confirm=true&purge=true` hard-deletes the row permanently
(SCHED-GAP-097 — member projects are unassigned and historical
namespace_ticks keep their namespace_id).

**Query params:**

| Param | Required | Meaning |
|-------|----------|---------|
| `confirm` | **yes** | Must be `true` — guards against stray DELETEs (purge has its own confirm; `purge=true` alone is refused) |
| `purge` | no | `true` = hard-delete the row |

**Response 200:** `{"status":"deleted","namespace":"<id>"}` or
`{"status":"purged","namespace":"<id>"}`.

**Errors:**

| Case | Status | Body |
|------|--------|------|
| Missing `confirm=true` (bare DELETE or `?purge=true` alone) | 400 | `{"error":"confirm=true query param required — this soft-deletes the namespace (enabled=false); add purge=true to permanently remove the row"}` |
| Unknown namespace | 404 | `{"error":"namespace not found"}` |
| Namespace has **enabled** member project(s) | 409 | `{"error":"namespace has enabled project(s) assigned — pause or move them first: <comma-separated names>"}` |
| Wrong method | 405 | `{"error":"GET, PUT, or DELETE only"}` |

```bash
# Soft-delete: enabled=false, member projects unassigned, row retained
curl -s -X DELETE "http://127.0.0.1:9090/api/v1/namespaces/data-cleanup?confirm=true"
# {"namespace":"data-cleanup","status":"deleted"}

# Hard-delete: permanent row removal (pause or move enabled members first)
curl -s -X DELETE "http://127.0.0.1:9090/api/v1/namespaces/data-cleanup?confirm=true&purge=true"
# {"namespace":"data-cleanup","status":"purged"}
```

### GET /api/v1/namespaces/{id}/projects

**Purpose:** List projects assigned to a namespace.

**Response 200:** `{"namespace_id":"<id>","projects":[<Project>, ...]}`.
**Errors:** 405 on non-GET; unknown sub-route → 404.

### POST /api/v1/namespaces/{id}/move

**Purpose:** Assign a project to a namespace (sets its `namespace_id`).

**Request body:** `{"project":"<name>"}` — `project` required.

**Response 200:** the updated project object (flat, `namespace_id` set).
**Errors:** 400 `{"error":"project is required"}`; 400 invalid JSON; 404
`{"error":"project not found"}`; 405 on non-POST.

```bash
curl -s -X POST http://127.0.0.1:9090/api/v1/namespaces/data-cleanup/move \
  -H 'Content-Type: application/json' -d '{"project":"my-project"}'
```

## 7. Ticks

A tick is one spawned agent invocation against one project. Tick model:

| Field | Type | Meaning |
|-------|------|---------|
| `id` | string | `<project>-<YYYY>-<MM>-<DD>-<HH>-<MM>-<SS>` |
| `project_name` | string | Project the tick ran against |
| `session_id` | string | Spawned process/gateway session id |
| `status` | string | `queued` \| `running` \| `completed` \| `failed` \| `timeout` \| `deferred` (`deferred` = terminal gateway-side deferral: a transient gateway blip, SCHED-GAP-203 — not a lane failure) |
| `outcome` | string | Terminal outcome: `committed` \| `dry_run` \| `failed` \| `timeout` \| `deferred`; `""` while running/queued |
| `spawned_at`, `completed_at` | string | RFC3339; `completed_at` `""` while not terminal |
| `exit_code` | int | Process exit code |
| `commits`, `files_changed` | int | Work metrics |
| `tokens_in`, `tokens_out` | int | Token usage |
| `cost_usd` | float | Dollar cost |
| `error` | string | Error text on failure |
| `created_at` | string | RFC3339 |

### GET /api/v1/ticks

**Purpose:** List ticks, newest first, with optional filters.

**Query params:**

| Param | Type | Default | Meaning |
|-------|------|---------|---------|
| `project` | string | — | Filter by exact project name |
| `status` | string | — | Filter by status: `queued`, `running`, `completed`, `failed`, `timeout` |
| `limit` | int | 50 | Max rows returned (must be > 0; invalid values fall back to 50) |

**Response 200:** `{"ticks":[<Tick>, ...], "count":<n>}` — `count` is the
number of ticks in THIS page (not a total).

**Errors:** 405 on non-GET.

```bash
curl -s "http://127.0.0.1:9090/api/v1/ticks?project=9router&status=completed&limit=5" \
  | jq '.ticks[] | {id, outcome, commits}'
```

### GET /api/v1/ticks/{id}

**Purpose:** Full tick detail by id.

**Response 200:** the tick object (flat). **Errors:** 400
`{"error":"tick id required"}` (empty id); 404 `{"error":"tick not found"}`;
405 on non-GET.

```bash
curl -s http://127.0.0.1:9090/api/v1/ticks/9router-2026-08-18-00-16-54
```

## 8. Events

The operational event log: decisions, errors, and routine notes
(severity INFO/LOW/MEDIUM/HIGH/CRITICAL). Event model:

| Field | Type | Meaning |
|-------|------|---------|
| `id` | int | Auto-increment primary key |
| `severity` | string | `INFO` \| `LOW` \| `MEDIUM` \| `HIGH` \| `CRITICAL` |
| `component` | string | Emitting component (e.g. `loop`, `api`, `sync`) |
| `message` | string | Human-readable summary |
| `details` | string | Free-form context, often JSON |
| `created_at` | string | RFC3339 |

### GET /api/v1/events

**Purpose:** List the event log, newest first (by id DESC), with optional
filters.

**Query params:**

| Param | Type | Default | Meaning |
|-------|------|---------|---------|
| `severity` | string | — | Filter by exact severity (`INFO`, `LOW`, `MEDIUM`, `HIGH`, `CRITICAL`) |
| `component` | string | — | Filter by component name |
| `limit` | int | 100 | Max rows returned (must be > 0; invalid values fall back to 100) |

**Response 200:** `{"events":[<Event>, ...], "count":<n>}`.

**Errors:** 405 on non-GET.

```bash
curl -s "http://127.0.0.1:9090/api/v1/events?severity=HIGH&limit=20" | jq '.events[].message'
```

### GET /api/v1/events/stream

**Purpose:** The live counterpart of the endpoint above — one long-lived
Server-Sent Events connection that receives every event as it is committed,
instead of a client poll loop. `internal/database.LogEvent` publishes to the
stream after a successful INSERT, so an event written by any component (the
scheduler loop, the API, the MCP server, the DuckBrain sync) appears on the
stream as it lands.

**Response 200:** `Content-Type: text/event-stream`, `Cache-Control: no-cache`.
One frame per event:

```
id: 12345
data: {"id":12345,"severity":"HIGH","component":"loop","message":"...","details":"{}","created_at":"2026-09-18T13:00:00Z"}

```

While the log is quiet a comment heartbeat (`: heartbeat`) is written every
15 seconds so idle connections survive proxies and intermediate timeouts.

**Reconnect:** the client echoes the last `id:` it saw in the `Last-Event-ID`
request header. Every persisted event with a greater id is replayed before live
events resume. A missing, empty or malformed `Last-Event-ID` (anything that is
not a plain decimal integer) is treated as "no cursor" and replays the newest
100 events — never an error, never an unbounded read. Each replay query is
capped at 500 rows and a whole replay at 100 pages; if that bound is reached
the stream emits a `: replay truncated` comment.

**Slow clients:** each subscriber has a bounded (64-event) buffer. A client that
falls that far behind drops its overflow rather than blocking event writers; the
stream notices the id gap on its next live event and re-reads the missing range
from the log before continuing, so a client that keeps reading still converges
on a gap-free sequence. Subscriptions are released as soon as the request
context is canceled.

**Errors:** 405 on non-GET.

```bash
# Live tail; reconnect automatically resumes from where the client stopped.
curl -N http://127.0.0.1:9090/api/v1/events/stream
# Resume after event 12345.
curl -N -H 'Last-Event-ID: 12345' http://127.0.0.1:9090/api/v1/events/stream
```

## 9. Queue

### GET /api/v1/queue

**Purpose:** The full set of enabled projects with engine-formula urgency
scores (GAP-054), sorted by urgency descending — NOT only currently
dispatchable projects. Every enabled project appears in the queue, including
projects still in cooldown. Urgency = `priority * (1 + elapsed/interval)^decay_rate`
(elapsed since `last_tick_completed`, falling back to `created_at`), matching
the daemon's selection ordering. When no urgency calculator is configured the
score falls back to priority-only.

**Eligibility semantics (live-verified 2026-08-21):** queue membership is NOT
eligibility. `cooldown_s` carries the seconds remaining until the project
becomes dispatchable (`0` = dispatchable now); an integrator must filter on
`cooldown_s == 0` to obtain the dispatchable subset. Filtering without that
check silently yields projects the daemon will not pick until their cooldown
expires.

**Query params:** none. **Request body:** none.

**Response 200:**

```json
{"count":44,
 "queue":[{"project":"warpfs","urgency":8792.4,"weight":15,"priority":10,
   "cooldown_s":43200,"enabled":true}, ...]}
```

| Field | Type | Meaning |
|-------|------|---------|
| `count` | int | Number of queue items |
| `queue[].project` | string | Project name |
| `queue[].urgency` | float | Engine urgency score (descending sort key) |
| `queue[].weight` / `priority` | int | Project scheduling parameters |
| `queue[].cooldown_s` | int | Seconds remaining until the project is dispatchable; `0` = eligible now. Present on every queue item regardless of value — do not use queue membership as an eligibility signal |
| `queue[].enabled` | bool | Always true (only enabled projects are queued) |

**Errors:** 405 on non-GET.

```bash
curl -s http://127.0.0.1:9090/api/v1/queue | jq '.queue[:3] | map(.project)'
```

## 10. Fleet metrics

### GET /api/v1/metrics

**Purpose:** every fleet question that used to be answered with hand-written
SQL against the daemon's SQLite DB, in ONE read-only JSON response
(SCHED-GAP-156):

| # | Question | Field paths |
|---|----------|-------------|
| 1 | spawns by namespace + outcome | `spawns.total`, `spawns.by_namespace`, `spawns.by_outcome` |
| 2 | deferrals by admission reason | `deferrals.by_reason` (+ `deferrals.admitted_by_namespace`, `deferrals.passes`) |
| 3 | orphan nudges by drop path | `nudges.by_path` |
| 4 | active/queued ticks vs global + per-namespace caps | `ticks.active`, `ticks.queued`, `ticks.global_cap`, `ticks.by_namespace.<ns>.active/.queued/.cap` |
| 5 | cooldown expired but unscheduled | `ticks.cooldown_expired_unscheduled` |
| 6 | tick duration p50/p90/p99 | `tick_duration_ms.count`, `.window`, `.p50`, `.p90`, `.p99` |
| 7 | gateway drain-503 count | `gateway.drain_503` |
| 8 | zero-output `committed` ticks | `outcomes.zero_output_committed` |

**Honesty rule (load-bearing):** every block carries `available` — `true` when
the number came from a real query or counter, `false` when its real source is
absent or a query failed. An unsourced block is reported as
`{"available": false, "reason": "<what is missing>"}` and NEVER as a
fabricated number, and the top-level `sources` map names the query/counter (or
the gap) behind every block. A `0` is only ever emitted because a real query
returned it; `tick_duration_ms` reports `p50`/`p90`/`p99` as JSON `null` (not
`0`) when the window holds no completed tick. **Do not treat `0` as "unknown" —
read `available` and `sources` first.**

**Window:** the windowed blocks (`spawns`, `tick_duration_ms`, `gateway`,
`outcomes`) look back **24h** (`tick_duration_ms.window` / `spawns.window` echo
the label). `nudges` is all-time (persisted on `ticks.nudge_count`), and
`deferrals` counts **since daemon boot** (in-memory counters, reset by a
restart — that reset is why `by_reason` only exists while a scheduler Loop is
attached). Every windowed predicate compares with SQLite `julianday()` because
`ticks.spawned_at`/`completed_at` are RFC3339 text that the fleet writes in
BOTH UTC (`...Z`) and local-offset (`...-05:00`) form — a raw string
comparison would silently drop rows.

**Percentile definition:** nearest-rank on the ascending sample of
`completed_at - spawned_at` durations in milliseconds —
`idx = ceil(pct/100 * n) - 1`, clamped to `[0, n-1]`, evaluated with integer
arithmetic (`(pct*n + 99) / 100`).

**Query params:** none. **Request body:** none. Read-only: no state is
mutated, no migration runs, and the endpoint echoes no config secrets.

**Response 200** (real capture, 2026-09-18, from a simulated fixture on a temp
DB — a quiesced fleet, so `ticks.active` is 0; `sources` abbreviated here, the
daemon returns the full text):

```json
{
  "generated_at": "2026-09-18T10:07:46Z",
  "uptime_s": 6,
  "sources": {"spawns": "ticks table, rows with spawned_at inside the window ...",
              "deferrals": "Loop.AdmissionCounters() — per-process SCHED-GAP-155 admission counters ...",
              "nudges": "ticks table, SUM(nudge_count) grouped by orphan_reason ...",
              "ticks": "ticks table: active = COUNT(status='running') ...",
              "tick_duration_ms": "ticks table: ROUND((julianday(completed_at) - julianday(spawned_at)) * 86400000) ...",
              "gateway": "ticks table: rows spawned inside the window whose error text matches the harness drain class ...",
              "outcomes": "ticks table: rows spawned inside the window with outcome='committed' AND commits=0 ..."},
  "spawns": {"available": true, "window": "24h", "total": 113,
             "by_namespace": {"-": 113},
             "by_outcome": {"committed": 0, "dry_run": 0, "failed": 0, "timeout": 0}},
  "deferrals": {"available": true,
                "by_reason": {"ok": 10, "cap": 2, "load_gate": 0, "cooldown": 0,
                              "tasks_no_work": 0, "board_unowned": 0, "budget": 0,
                              "tasks_deferred": 0},
                "admitted_by_namespace": {"-": 10}, "passes": 1},
  "nudges": {"available": true, "by_path": {}},
  "ticks": {"available": true, "active": 0, "queued": 0, "global_cap": 10,
            "by_namespace": {}, "cooldown_expired_unscheduled": 12},
  "tick_duration_ms": {"count": 113, "window": "24h", "p50": 0, "p90": 1000, "p99": 1000},
  "gateway": {"available": true, "drain_503": 0, "window": "24h"},
  "outcomes": {"available": true, "zero_output_committed": 0, "window": "24h"}
}
```

| Field | Type | Meaning |
|-------|------|---------|
| `generated_at` | string | RFC3339 UTC timestamp of this response |
| `uptime_s` | int | Seconds since the daemon's API server was constructed |
| `sources` | object | One entry per block naming the exact query/counter behind it (or the gap) |
| `spawns.available` | bool | `false` + `reason` when the ticks query failed |
| `spawns.total` | int | Tick rows spawned inside the window (any status, including not-yet-terminal ones) |
| `spawns.by_namespace` | object | Rows per `projects.namespace_id`; key `"-"` = project row missing or namespace unset |
| `spawns.by_outcome` | object | Always carries all four CHECK-constraint values; a `NULL` outcome (never terminal) is counted in `total` only |
| `deferrals.by_reason` | object | Admission-decision counters per reason (`ok`, `cap`, `load_gate`, `cooldown`, `tasks_no_work`, `board_unowned`, `budget`, `tasks_deferred`); every reason is present, `0` = never seen this process |
| `deferrals.admitted_by_namespace` | object | Admitted-tick totals per namespace (key `"-"` = namespace unset) |
| `deferrals.passes` | int | Evaluation passes the admission emitter ran in |
| `nudges.by_path` | object | Orphan re-nudges per drop path (`drain_timeout`, `startup_reap`, `zombie_reap`, `unknown`) — all-time |
| `ticks.active` | int | Ticks with `status='running'` — the same number `/api/v1/status` serves as `active_ticks` |
| `ticks.queued` | int | Ticks with `status='queued'` (enqueued, not yet spawned) |
| `ticks.global_cap` | int | `max_concurrent` from the resolved-config snapshot — the same field `/api/v1/config` serves; `0` = no value in this process |
| `ticks.by_namespace.<ns>.active` / `.queued` | int | Live running/queued ticks of that namespace |
| `ticks.by_namespace.<ns>.cap` | int | `namespaces.max_concurrent`; **`0` = unlimited** (the global cap still applies) |
| `ticks.cooldown_expired_unscheduled` | int | Enabled projects with no running AND no queued tick whose wall-clock cooldown has elapsed (bump-aware). Mirror of the packer's wall-clock gate; blackout windows, tasks-mode pacing and `cooldown_s=0` dynamic intervals are not modelled, so it can overcount slightly |
| `tick_duration_ms.count` | int | Sample size (ticks completed inside the window with both timestamps) |
| `tick_duration_ms.window` | string | Always `"24h"` |
| `tick_duration_ms.p50` / `.p90` / `.p99` | int \| null | Nearest-rank duration percentiles in ms; `null` when `count == 0` |
| `gateway.drain_503` | int | Ticks whose `error` text matches the drain class (`%503%` or `%draining%`) in the window. The in-memory `gateway_errors` counter on `/api/v1/status` counts ALL transient gateway failures and cannot classify drains, so it is deliberately not the source |
| `outcomes.zero_output_committed` | int | Ticks with `outcome='committed'` AND `commits=0` in the window — a `committed` outcome with nothing committed |

**Errors:** 405 `{"error":"GET only"}` on non-GET. A block whose query fails is
reported in-band as `available:false` — the endpoint itself still answers 200,
because a dead source is a metric to report, not a request error.

```bash
curl -s http://127.0.0.1:9090/api/v1/metrics | jq '{active: .ticks.active, queued: .ticks.queued, caps: .ticks.by_namespace}'
curl -s http://127.0.0.1:9090/api/v1/metrics | jq '.deferrals.by_reason | to_entries | map(select(.value > 0))'
curl -s http://127.0.0.1:9090/api/v1/metrics | jq '.sources'   # what backs each number
```

## 11. Fleet-wide control

All three take no body. They mutate the live loop — use with care (pausing
stops the entire fleet; running ticks finish, new spawns stop).

### POST /api/v1/evaluate

**Purpose:** Force an evaluation cycle now (also triggered on startup,
slot-freed debounce, and the 5-min eval-stall watchdog, GAP-042).

**Response 200:** `{"status":"evaluation triggered"}`. **Errors:** 405
`{"error":"POST only"}` on non-POST.

### POST /api/v1/pause

**Purpose:** Pause the scheduler loop — stops NEW spawns fleet-wide.

**Response 200:** `{"status":"paused"}`. **Errors:** 405 on non-POST.

### POST /api/v1/resume

**Purpose:** Resume the scheduler loop.

**Response 200:** `{"status":"resumed"}`. **Errors:** 405 on non-POST.

```bash
curl -s -X POST http://127.0.0.1:9090/api/v1/evaluate
# {"status":"evaluation triggered"}
curl -s -X POST http://127.0.0.1:9090/api/v1/pause
# {"status":"paused"}
curl -s -X POST http://127.0.0.1:9090/api/v1/resume
# {"status":"resumed"}
```

Verify the loop is back after resume: `curl -s http://127.0.0.1:9090/api/v1/health`.

## 12. OpenAPI spec

### GET /api/v1/openapi.json

**Purpose:** The machine-readable OpenAPI 3.0.3 document describing this API
(generated from the `openapiSpec` constant in `internal/api/server_helpers.go`).

**Response 200:** `application/json` OpenAPI document. **Errors:** 405 on non-GET.

```bash
curl -s http://127.0.0.1:9090/api/v1/openapi.json | jq '.info, (.paths | keys)'
```
