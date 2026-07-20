# S06 — REST API Specification

**Status:** Draft  
**Depends on:** S01, S02, S03, S04, S05  
**Pages target:** 4-5

---

## 1. Overview

The scheduler exposes a versioned JSON REST API from the same `net/http` server as the daemon. The default listener is `127.0.0.1:9090`; every route in this specification is rooted at `/api/v1`. The API provides health and fleet status, project registration and configuration, tick and event history, and imperative scheduler controls.

SQLite is authoritative for projects, ticks, and events. Control calls operate on the in-process `scheduler.Loop`. Responses are JSON and use `Content-Type: application/json`. There is no authentication layer, cross-origin API, delete endpoint, offset/cursor pagination, or asynchronous job resource.

The implementation registers fifteen `ServeMux` patterns representing twenty method/path operations. This document uses those twenty operations as the public surface. Empty collections are encoded as arrays, never `null`.

### 1.1 Current implementation conformance

The contract uses the S02 lower snake-case JSON field names. `internal/database/models.go` and `ProjectUpdates` currently omit explicit `json` tags, so Go's encoder emits exported field names such as `Name`, `RepoURL`, and `ProjectName`, while underscore-separated request keys do not bind exactly. The implementation must restore the S02 tags before its wire representation fully conforms to the OpenAPI contract; this specification does not redefine the database model.

Likewise, the current `handleProjectByID` calls `splitPath` on the full URL and selects `parts[0]`; for `/api/v1/projects/alpha`, that value is `api`, not `alpha`. Section 4 records the required relative-path dispatch and identifies this as an implementation defect rather than documenting the defect as the API contract.

---

## 2. Dependencies

| Dependency | Purpose | Failure Mode |
|------------|---------|-------------|
| Go `net/http` | ServeMux routing, method and query access, HTTP responses | API unavailable |
| Go `encoding/json` | Decode project bodies and encode all responses | `400` on decode; encode failure is not recoverable after headers |
| Go `database/sql` | Query SQLite and health-check the connection | `500`, except health reports DB error in a `200` body |
| `internal/database` (S02) | Project CRUD models and persistence | Project operations fail |
| `internal/scheduler.Loop` | Force evaluation; fleet pause/resume | Control endpoint cannot perform action |
| SQLite schema (S02) | Projects, ticks, and events | History/status queries fail |
| Spawn lifecycle (S05) | Supplies running and terminal tick records | Counts/history may be stale |

`Server` is constructed with non-nil `*sql.DB` and `*scheduler.Loop`. `started` is captured by `NewServer` and is the origin for health uptime. Handlers currently use `context.Background()` rather than the request context; client cancellation therefore does not cancel database work.

---

## 3. Interface

The following OpenAPI 3.0 document is normative. Examples use representative values; timestamps are RFC3339 strings and uptime is Go `time.Duration.String()` output.

```yaml
openapi: 3.0.3
info:
  title: Coding Hermes Scheduler REST API
  version: 1.0.0
servers:
  - url: http://127.0.0.1:9090/api/v1
paths:
  /health:
    get:
      operationId: health
      responses:
        '200':
          description: Process and database health; DB failure is reported in-body.
          content:
            application/json:
              schema: { $ref: '#/components/schemas/Health' }
              example: { status: ok, uptime: 2h3m4.5s, db: connected, active_ticks: 2 }
        '405': { $ref: '#/components/responses/GetOnly' }
  /status:
    get:
      operationId: status
      responses:
        '200':
          description: Fleet overview.
          content:
            application/json:
              schema: { $ref: '#/components/schemas/FleetStatus' }
              example: { budget_total: 100, active_projects: 3, active_ticks: 2, recent_outcomes: { completed: 8, failed: 1, timeout: 0 } }
        '500': { $ref: '#/components/responses/InternalError' }
        '405': { $ref: '#/components/responses/GetOnly' }
  /projects:
    get:
      operationId: listProjects
      responses:
        '200':
          description: All projects ordered by name.
          content:
            application/json:
              schema: { $ref: '#/components/schemas/ProjectList' }
              example: { projects: [{ name: alpha, repo_url: 'https://github.com/acme/alpha', workdir: /srv/alpha, weight: 10, priority: 5, cooldown_s: 900, decay_rate: 1.0, model: gpt-5, provider: openai, enabled: true, created_at: '2026-07-12T10:00:00Z', updated_at: '2026-07-12T10:00:00Z' }] }
        '500': { $ref: '#/components/responses/InternalError' }
        '405': { $ref: '#/components/responses/GetOrPostOnly' }
    post:
      operationId: createProject
      requestBody:
        required: true
        content:
          application/json:
            schema: { $ref: '#/components/schemas/ProjectCreate' }
            example: { name: alpha, repo_url: 'https://github.com/acme/alpha', workdir: /srv/alpha, weight: 10, priority: 5, cooldown_s: 900, decay_rate: 1.0, model: gpt-5, provider: openai, enabled: true }
      responses:
        '201':
          description: Created project, including generated timestamps.
          content:
            application/json:
              schema: { $ref: '#/components/schemas/Project' }
              example: { name: alpha, repo_url: 'https://github.com/acme/alpha', workdir: /srv/alpha, weight: 10, priority: 5, cooldown_s: 900, decay_rate: 1.0, model: gpt-5, provider: openai, enabled: true, created_at: '2026-07-12T10:00:00Z', updated_at: '2026-07-12T10:00:00Z' }
        '400': { $ref: '#/components/responses/BadRequest' }
        '409': { $ref: '#/components/responses/Conflict' }
        '500': { $ref: '#/components/responses/InternalError' }
        '405': { $ref: '#/components/responses/GetOrPostOnly' }
  /projects/{name}:
    parameters:
      - $ref: '#/components/parameters/ProjectName'
    get:
      operationId: getProject
      responses:
        '200':
          description: Project and its latest tick, or null when none exists.
          content:
            application/json:
              schema: { $ref: '#/components/schemas/ProjectDetail' }
              example: { project: { name: alpha, repo_url: 'https://github.com/acme/alpha', workdir: /srv/alpha, weight: 10, priority: 5, cooldown_s: 900, decay_rate: 1.0, model: gpt-5, provider: openai, enabled: true, created_at: '2026-07-12T10:00:00Z', updated_at: '2026-07-12T10:00:00Z' }, latest_tick: null }
        '404': { $ref: '#/components/responses/ProjectNotFound' }
        '500': { $ref: '#/components/responses/InternalError' }
        '405': { $ref: '#/components/responses/ProjectMethods' }
    put:
      operationId: updateProject
      requestBody:
        required: true
        content:
          application/json:
            schema: { $ref: '#/components/schemas/ProjectUpdate' }
            example: { weight: 20, priority: 8, enabled: true }
      responses:
        '200':
          description: Updated project.
          content:
            application/json:
              schema: { $ref: '#/components/schemas/Project' }
              example: { name: alpha, repo_url: 'https://github.com/acme/alpha', workdir: /srv/alpha, weight: 20, priority: 8, cooldown_s: 900, decay_rate: 1.0, model: gpt-5, provider: openai, enabled: true, created_at: '2026-07-12T10:00:00Z', updated_at: '2026-07-12T10:05:00Z' }
        '400': { $ref: '#/components/responses/BadRequest' }
        '404': { $ref: '#/components/responses/ProjectNotFound' }
        '500': { $ref: '#/components/responses/InternalError' }
        '405': { $ref: '#/components/responses/ProjectMethods' }
  /projects/{name}/pause:
    post:
      operationId: pauseProject
      parameters: [{ $ref: '#/components/parameters/ProjectName' }]
      responses:
        '200':
          description: Project disabled.
          content:
            application/json:
              schema: { $ref: '#/components/schemas/ProjectControl' }
              example: { status: paused, project: alpha }
        '500': { $ref: '#/components/responses/InternalError' }
        '405': { $ref: '#/components/responses/ProjectMethods' }
  /projects/{name}/resume:
    post:
      operationId: resumeProject
      parameters: [{ $ref: '#/components/parameters/ProjectName' }]
      responses:
        '200':
          description: Project enabled.
          content:
            application/json:
              schema: { $ref: '#/components/schemas/ProjectControl' }
              example: { status: resumed, project: alpha }
        '500': { $ref: '#/components/responses/InternalError' }
        '405': { $ref: '#/components/responses/ProjectMethods' }
  /ticks:
    get:
      operationId: listTicks
      parameters:
        - { name: project, in: query, schema: { type: string }, example: alpha }
        - { name: limit, in: query, schema: { type: integer, minimum: 1, default: 50 }, example: 25 }
      responses:
        '200':
          description: Newest ticks first.
          content:
            application/json:
              schema: { $ref: '#/components/schemas/TickList' }
              example: { ticks: [], count: 0 }
        '500': { $ref: '#/components/responses/InternalError' }
        '405': { $ref: '#/components/responses/GetOnly' }
  /ticks/{id}:
    get:
      operationId: getTick
      parameters:
        - { name: id, in: path, required: true, schema: { type: string }, example: alpha-2026-07-12-10-00-00 }
      responses:
        '200':
          description: Tick detail.
          content:
            application/json:
              schema: { $ref: '#/components/schemas/Tick' }
              example: { id: alpha-2026-07-12-10-00-00, project_name: alpha, session_id: sess-123, status: completed, outcome: committed, spawned_at: '2026-07-12T10:00:00Z', completed_at: '2026-07-12T10:04:00Z', exit_code: 0, commits: 1, files_changed: 3, tokens_in: 1200, tokens_out: 500, cost_usd: 0.04, urgency: 2.5, weight_used: 10, error: '', created_at: '2026-07-12T10:00:00Z' }
        '400': { $ref: '#/components/responses/BadRequest' }
        '404': { $ref: '#/components/responses/TickNotFound' }
        '405': { $ref: '#/components/responses/GetOnly' }
  /evaluate:
    post:
      operationId: evaluate
      responses:
        '200':
          description: Force-evaluation signal accepted.
          content: { application/json: { schema: { $ref: '#/components/schemas/StatusMessage' }, example: { status: evaluation triggered } } }
        '405': { $ref: '#/components/responses/PostOnly' }
  /pause:
    post:
      operationId: pauseScheduler
      responses:
        '200':
          description: Scheduler loop paused.
          content: { application/json: { schema: { $ref: '#/components/schemas/StatusMessage' }, example: { status: paused } } }
        '405': { $ref: '#/components/responses/PostOnly' }
  /resume:
    post:
      operationId: resumeScheduler
      responses:
        '200':
          description: Scheduler loop resumed.
          content: { application/json: { schema: { $ref: '#/components/schemas/StatusMessage' }, example: { status: resumed } } }
        '405': { $ref: '#/components/responses/PostOnly' }
  /events:
    get:
      operationId: listEvents
      parameters:
        - { name: severity, in: query, schema: { type: string, enum: [CRITICAL, HIGH, MEDIUM, LOW, INFO] }, example: HIGH }
        - { name: component, in: query, schema: { type: string }, example: scheduler }
        - { name: limit, in: query, schema: { type: integer, minimum: 1, default: 100 }, example: 50 }
      responses:
        '200':
          description: Newest events first.
          content:
            application/json:
              schema: { $ref: '#/components/schemas/EventList' }
              example: { events: [{ id: 42, severity: INFO, component: scheduler, message: tick completed, details: '{"tick_id":"alpha-2026-07-12-10-00-00"}', created_at: '2026-07-12T10:04:00Z' }], count: 1 }
        '500': { $ref: '#/components/responses/InternalError' }
        '405': { $ref: '#/components/responses/GetOnly' }
components:
  parameters:
    ProjectName: { name: name, in: path, required: true, schema: { type: string }, example: alpha }
  responses:
    BadRequest: { description: Invalid JSON or missing path/body field, content: { application/json: { schema: { $ref: '#/components/schemas/Error' }, example: { error: bad request } } } }
    ProjectNotFound: { description: Project absent, content: { application/json: { schema: { $ref: '#/components/schemas/Error' }, example: { error: project not found } } } }
    TickNotFound: { description: Tick absent, content: { application/json: { schema: { $ref: '#/components/schemas/Error' }, example: { error: tick not found } } } }
    Conflict: { description: Duplicate project name, content: { application/json: { schema: { $ref: '#/components/schemas/Error' }, example: { error: project already exists } } } }
    InternalError: { description: Database/internal failure, content: { application/json: { schema: { $ref: '#/components/schemas/Error' }, example: { error: database failure } } } }
    GetOnly: { description: Wrong method, content: { application/json: { schema: { $ref: '#/components/schemas/Error' }, example: { error: GET only } } } }
    PostOnly: { description: Wrong method, content: { application/json: { schema: { $ref: '#/components/schemas/Error' }, example: { error: POST only } } } }
    GetOrPostOnly: { description: Wrong method, content: { application/json: { schema: { $ref: '#/components/schemas/Error' }, example: { error: GET or POST only } } } }
    ProjectMethods: { description: Wrong method, content: { application/json: { schema: { $ref: '#/components/schemas/Error' }, example: { error: 'GET, PUT, or POST only' } } } }
  schemas:
    Error: { type: object, required: [error], additionalProperties: false, properties: { error: { type: string } } }
    Health: { type: object, required: [status, uptime, db, active_ticks], properties: { status: { type: string, enum: [ok] }, uptime: { type: string }, db: { type: string, description: 'connected or error: <driver error>' }, active_ticks: { type: integer } } }
    FleetStatus: { type: object, required: [budget_total, active_projects, active_ticks, recent_outcomes], properties: { budget_total: { type: integer, enum: [100] }, active_projects: { type: integer }, active_ticks: { type: integer }, recent_outcomes: { $ref: '#/components/schemas/RecentOutcomes' } } }
    RecentOutcomes: { type: object, required: [completed, failed, timeout], properties: { completed: { type: integer }, failed: { type: integer }, timeout: { type: integer } }, additionalProperties: { type: integer } }
    Project:
      type: object
      required: [name, repo_url, workdir, weight, priority, cooldown_s, decay_rate, model, provider, enabled, created_at, updated_at]
      properties: { name: { type: string }, repo_url: { type: string }, workdir: { type: string }, weight: { type: integer, minimum: 1, maximum: 100, default: 10 }, priority: { type: integer, minimum: 1, maximum: 10, default: 5 }, cooldown_s: { type: integer, default: 900 }, decay_rate: { type: number, format: double, default: 1.0 }, model: { type: string }, provider: { type: string }, enabled: { type: boolean }, created_at: { type: string, format: date-time }, updated_at: { type: string, format: date-time } }
    ProjectCreate:
      type: object
      required: [name, repo_url, workdir]
      description: Other fields are optional inputs; timestamps may be omitted and are generated when empty.
      properties: { name: { type: string }, repo_url: { type: string }, workdir: { type: string }, weight: { type: integer, minimum: 1, maximum: 100 }, priority: { type: integer, minimum: 1, maximum: 10 }, cooldown_s: { type: integer }, decay_rate: { type: number }, model: { type: string }, provider: { type: string }, enabled: { type: boolean }, created_at: { type: string }, updated_at: { type: string } }
    ProjectUpdate:
      type: object
      properties: { repo_url: { type: string }, workdir: { type: string }, weight: { type: integer }, priority: { type: integer }, cooldown_s: { type: integer }, decay_rate: { type: number }, model: { type: string }, provider: { type: string }, enabled: { type: boolean } }
    ProjectList: { type: object, required: [projects], properties: { projects: { type: array, items: { $ref: '#/components/schemas/Project' } } } }
    ProjectDetail: { type: object, required: [project, latest_tick], properties: { project: { $ref: '#/components/schemas/Project' }, latest_tick: { nullable: true, allOf: [{ $ref: '#/components/schemas/Tick' }] } } }
    ProjectControl: { type: object, required: [status, project], properties: { status: { type: string, enum: [paused, resumed] }, project: { type: string } } }
    Tick:
      type: object
      required: [id, project_name, session_id, status, outcome, spawned_at, completed_at, exit_code, commits, files_changed, tokens_in, tokens_out, cost_usd, urgency, weight_used, error, created_at]
      properties: { id: { type: string, description: '<project>-<YYYY>-<MM>-<DD>-<HH>-<mm>-<ss>' }, project_name: { type: string }, session_id: { type: string }, status: { type: string, enum: [queued, running, completed, failed, timeout] }, outcome: { type: string, enum: [committed, dry_run, failed, timeout, ''] }, spawned_at: { type: string }, completed_at: { type: string }, exit_code: { type: integer }, commits: { type: integer }, files_changed: { type: integer }, tokens_in: { type: integer, format: int64 }, tokens_out: { type: integer, format: int64 }, cost_usd: { type: number, format: double }, urgency: { type: number, format: double }, weight_used: { type: integer }, error: { type: string }, created_at: { type: string } }
    TickList: { type: object, required: [ticks, count], properties: { ticks: { type: array, items: { $ref: '#/components/schemas/Tick' } }, count: { type: integer } } }
    Event: { type: object, required: [id, severity, component, message, details, created_at], properties: { id: { type: integer, format: int64 }, severity: { type: string, enum: [CRITICAL, HIGH, MEDIUM, LOW, INFO] }, component: { type: string }, message: { type: string }, details: { type: string }, created_at: { type: string, format: date-time } } }
    EventList: { type: object, required: [events, count], properties: { events: { type: array, items: { $ref: '#/components/schemas/Event' } }, count: { type: integer } } }
    StatusMessage: { type: object, required: [status], properties: { status: { type: string } } }
```

---

## 4. Behavior

### 4.1 Middleware and response pipeline

The effective current stack is: `http.Server` → `http.ServeMux` → route method gate → optional JSON decode/database or loop action → `writeJSON`. `writeJSON` sets `Content-Type: application/json`, writes the status, then emits one newline-terminated JSON value. `writeError` delegates to it with `{"error":"<human-readable message>"}`.

There is no request-logging middleware. Daemon logs use Go's `log.LstdFlags | log.Lshortfile` format, approximately `YYYY/MM/DD HH:MM:SS file.go:line: message`; only startup, shutdown, and scheduler events are logged, not each HTTP request.

There is no `Access-Control-Allow-Origin` header. Browsers therefore enforce the default same-origin policy; cross-origin API access is unsupported. There is no preflight/`OPTIONS` handler.

JSON body operations require `Content-Type: application/json` by contract. The current decoder does not inspect the request header, so missing or incorrect media types are not rejected before decoding. This is a conformance gap: strict enforcement would return `400` in the existing error shape. Responses, including errors, are always `application/json`.

### 4.2 Pagination and filtering

`GET /ticks` accepts `project` and `limit`; default limit is 50. `GET /events` accepts `severity`, `component`, and `limit`; default limit is 100. A positive base-10 integer overrides the default. Missing, non-numeric, zero, and negative limits silently use the default. There is no upper cap, offset, cursor, or total database count.

Queries are parameterized and ordered newest first. Envelopes are exactly `{"ticks":[...],"count":N}` and `{"events":[...],"count":N}`. `count` is the returned array length, not all matching rows. Nil slices are normalized to `[]`.

### 4.3 Error handling

Handlers return immediately after `writeError`. Decoder messages retain the `encoding/json` error text after the `invalid JSON: ` prefix. Database errors are exposed verbatim by most `500` responses. JSON encoding errors after headers are ignored because a second status cannot safely be sent.

Health is deliberately liveness-oriented: a failed ping still returns HTTP `200`, `status="ok"`, and `db="error: <message>"`. Failure of the active-tick count query is ignored and produces zero. Status similarly treats recent-outcome query failure as zero counts, but project-list failure is `500`.

### 4.4 Route dispatch and controls

`handleProjectByID` must trim `/api/v1/projects/`, split the remaining relative path, and interpret one segment as `{name}` and two as `{name}/pause|resume`. Unknown two-segment POST sub-routes return `404 {"error":"not found"}`. Other wrong methods return `405`.

Project pause/resume performs `UpdateProject(enabled=false|true)`. In the current handlers a missing project is surfaced as `500`, unlike get/update which translate it to `404`. Fleet controls are synchronous in-memory calls: `ForceEvaluate`, `Pause`, and `Resume`; success means the method was invoked, not that a tick completed or persisted.

---

## 5. Data

### 5.1 Project requests

Create requires non-empty `name`, `repo_url`, and `workdir`. Other fields are accepted as supplied; defaults are database-schema concerns and zero-valued fields are not filled by the handler. Created/updated timestamps are generated in UTC when empty. Duplicate `name` conflicts.

Update is a partial object. Optional pointer fields distinguish omission from zero/false: `repo_url`, `workdir`, `weight`, `priority`, `cooldown_s`, `decay_rate`, `model`, `provider`, and `enabled`. `name`, `created_at`, and `updated_at` are not update inputs. An empty object only refreshes `updated_at`.

### 5.2 Response models

| Model | Exact fields |
|-------|--------------|
| Project | `name:string`, `repo_url:string`, `workdir:string`, `weight:int`, `priority:int`, `cooldown_s:int`, `decay_rate:number`, `model:string`, `provider:string`, `enabled:bool`, `created_at:string`, `updated_at:string` |
| Tick | `id:string`, `project_name:string`, `session_id:string`, `status:string`, `outcome:string`, timestamps, integer metrics, `cost_usd:number`, `urgency:number`, `weight_used:int`, `error:string` |
| Event | `id:int64`, `severity:string`, `component:string`, `message:string`, `details:string`, `created_at:string` |
| Error | exactly one required `error:string` field |

Tick IDs use `<project>-<YYYY>-<MM>-<DD>-<HH>-<mm>-<ss>`. Tick status is `queued|running|completed|failed|timeout`; terminal outcome is `committed|dry_run|failed|timeout`, while non-terminal rows may contain an empty outcome. Event severity is `CRITICAL|HIGH|MEDIUM|LOW|INFO`. Timestamp strings originate in SQLite and are intended to be RFC3339; incomplete tick timestamps may be empty strings rather than JSON `null`.

`GET /projects/{name}` returns `latest_tick:null` when `getLatestTick` finds no row or encounters any query/scan error; that secondary error is intentionally ignored.

---

## 6. States

The API itself is stateless apart from server uptime. It exposes and mutates two underlying state domains:

```text
Project: ENABLED ──POST /projects/{name}/pause──▶ DISABLED
         DISABLED ──POST /projects/{name}/resume──▶ ENABLED
Scheduler: RUNNING ──POST /pause──▶ PAUSED
           PAUSED ──POST /resume──▶ RUNNING
Evaluation: IDLE ──POST /evaluate──▶ SIGNAL_PENDING ──loop consumes──▶ IDLE
Tick: QUEUED → RUNNING → COMPLETED | FAILED | TIMEOUT   (read-only here)
```

Pause/resume calls are idempotent at the desired-state level. Repeated evaluate calls use the loop's force-evaluation signaling semantics and do not return an evaluation identifier. API restart resets uptime but does not change persisted project/tick/event state. Scheduler pause state is process-local.

---

## 7. Errors

Every explicit handler error is newline-terminated JSON with `Content-Type: application/json`:

```json
{"error":"<human-readable message>"}
```

| Status | Exact/representative message | When it fires |
|--------|------------------------------|---------------|
| `400` | `invalid JSON: <decoder error>` | POST project or PUT project body cannot decode |
| `400` | `name, repo_url, workdir are required` | Create omits/empties a required field |
| `400` | `project name required` | Project-by-ID dispatch has no relative name |
| `400` | `tick id required` | Tick-by-ID suffix is empty |
| `404` | `project not found` | Project GET/PUT targets an absent row |
| `404` | `tick not found` | Tick lookup returns any query/scan error |
| `404` | `not found` | Unknown project POST sub-route |
| `405` | `GET only` | Non-GET health, status, ticks, tick detail, or events |
| `405` | `POST only` | Non-POST evaluate, fleet pause, or fleet resume |
| `405` | `GET or POST only` | Unsupported method on `/projects` |
| `405` | `GET, PUT, or POST only` | Unsupported method on project-by-ID dispatch |
| `409` | `project already exists` | Create error text contains `UNIQUE constraint` |
| `500` | database error text | List/get/update/create/history query fails |
| `500` | update error text | Project pause/resume targets a missing row or fails |

Unregistered paths receive Go ServeMux's default plain-text `404 page not found`, not the JSON error envelope. Panics also use standard `net/http` behavior because no recovery middleware is installed. These are current implementation limitations.

---

## 8. Testing

Use `httptest.NewServer` or `httptest.NewRecorder`, a temporary SQLite database initialized by migrations, and a real `scheduler.Loop` with no background run. Tests must not bind the production port or spawn Hermes.

```text
1. Assert all fourteen method/path operations and exact success status/body.
2. Assert every response and explicit error has application/json.
3. Create, list, get, partial-update, pause, and resume a project.
4. Verify duplicate create is 409 and malformed/missing fields are 400.
5. Verify absent project/tick and unknown sub-route behavior.
6. Seed ticks/events; verify filters, descending order, default limits, and envelopes.
7. Verify invalid/zero/negative limits fall back to 50 or 100.
8. Verify health keys, connected/error DB text, uptime string, and active count.
9. Verify status budget=100, enabled-project count, active count, and outcomes map.
10. Verify evaluate/pause/resume invoke loop state changes and return exact messages.
11. Exercise every wrong method and exact 405 message.
12. Close the DB and verify the documented 500 and health degradation paths.
13. Verify no CORS headers and default OPTIONS behavior.
14. Add JSON-tag and relative-path regression tests for the conformance gaps in 1.1.
```

Required golden JSON should decode and compare structurally except `uptime` and generated timestamps. Also assert list arrays are `[]`, not `null`, and `count == len(items)`. Run API-focused tests under `go test -race` when implementing; this documentation task does not modify or execute Go code.

---

## 9. Security

| Vector | Current control / requirement |
|--------|-------------------------------|
| Remote access | Default bind is loopback `127.0.0.1:9090`; do not expose unauthenticated controls publicly |
| Cross-origin browser calls | No CORS allow header; same-origin only by browser default |
| SQL injection | Query values and limits are parameterized; no user input is concatenated into SQL |
| JSON ambiguity | Accept one JSON value; strict content type and unknown-field rejection are desirable gaps |
| Secret leakage | Project URLs, provider/model IDs, workdirs, DB errors, and event details are returned; treat API as operator-only |
| Denial of service | Limits have no upper bound and handlers lack body-size, request-timeout, and query-timeout controls |
| State-changing CSRF | Loopback plus same-origin policy reduces browser risk, but no token/authentication exists |
| Path input | Project/tick IDs are SQL values, not filesystem paths in these handlers |

Deploy behind an authenticated local proxy if access beyond localhost is required. Do not enable wildcard CORS. Set server read-header/read/write/idle timeouts before hostile-network exposure. Database error strings should eventually be replaced with stable public messages and internal logs.

---

## 10. Performance

| Metric | Target / bound |
|--------|----------------|
| Health/status response | < 50ms p99 on local SQLite |
| Project/tick/event read | < 100ms p99 at default limits |
| Default tick page | At most 50 rows |
| Default event page | At most 100 rows |
| Project list | O(number of projects), ordered by indexed/primary name |
| Tick/event list | O(filter + limit), newest-first; indexes should support filters/order |
| Control response | < 10ms excluding scheduler work; evaluate is signal-only |
| JSON memory | O(rows returned); slices are fully materialized before encoding |

The API has no response cache, compression, streaming, rate limiting, or pagination cursor. Caller-supplied limits are unbounded, so operators must keep them reasonable; a future hard maximum should preserve the response envelope. Health performs one count query and one DB ping. Status performs an enabled-project list, an active-tick count, and a grouped terminal-status query. No handler should hold a database transaction while invoking scheduler controls.

---

## 11. Namespace API Endpoints (Migration v2)

When `NamespaceMode=true`, the following additional endpoints are available. When `NamespaceMode=false`, they return `{"namespaces":[],"mode":"flat","message":"namespace mode disabled"}`.

### 11.1 OpenAPI — Namespace Paths

```yaml
  /namespaces:
    get:
      operationId: listNamespaces
      responses:
        '200':
          description: All namespaces with project counts and last utilization.
          content:
            application/json:
              schema: { $ref: '#/components/schemas/NamespaceList' }
        '405': { $ref: '#/components/responses/GetOrPostOnly' }
    post:
      operationId: createNamespace
      requestBody:
        required: true
        content:
          application/json:
            schema: { $ref: '#/components/schemas/NamespaceCreate' }
      responses:
        '201':
          description: Created namespace.
          content:
            application/json:
              schema: { $ref: '#/components/schemas/Namespace' }
        '400': { $ref: '#/components/responses/BadRequest' }
        '409': { $ref: '#/components/responses/Conflict' }
        '405': { $ref: '#/components/responses/GetOrPostOnly' }
  /namespaces/{id}:
    parameters:
      - { name: id, in: path, required: true, schema: { type: string } }
    get:
      operationId: getNamespace
      responses:
        '200':
          description: Namespace detail with recent utilization history.
          content:
            application/json:
              schema: { $ref: '#/components/schemas/NamespaceDetail' }
        '404': { $ref: '#/components/responses/NamespaceNotFound' }
        '405': { $ref: '#/components/responses/NamespaceMethods' }
    put:
      operationId: updateNamespace
      requestBody:
        required: true
        content:
          application/json:
            schema: { $ref: '#/components/schemas/NamespaceUpdate' }
      responses:
        '200':
          description: Updated namespace.
          content:
            application/json:
              schema: { $ref: '#/components/schemas/Namespace' }
        '400': { $ref: '#/components/responses/BadRequest' }
        '404': { $ref: '#/components/responses/NamespaceNotFound' }
        '405': { $ref: '#/components/responses/NamespaceMethods' }
  /namespaces/{id}/projects:
    get:
      operationId: listNamespaceProjects
      parameters:
        - { name: id, in: path, required: true, schema: { type: string } }
      responses:
        '200':
          description: Projects in this namespace.
          content:
            application/json:
              schema: { $ref: '#/components/schemas/ProjectList' }
        '404': { $ref: '#/components/responses/NamespaceNotFound' }
        '405': { $ref: '#/components/responses/GetOnly' }
  /namespaces/{id}/move:
    post:
      operationId: moveProjectToNamespace
      parameters:
        - { name: id, in: path, required: true, schema: { type: string } }
      requestBody:
        required: true
        content:
          application/json:
            schema:
              type: object
              required: [project]
              properties:
                project: { type: string }
      responses:
        '200':
          description: Project moved.
          content:
            application/json:
              schema: { $ref: '#/components/schemas/ProjectControl' }
        '400': { $ref: '#/components/responses/BadRequest' }
        '404': { $ref: '#/components/responses/NamespaceNotFound' }
        '405': { $ref: '#/components/responses/PostOnly' }
```

### 11.2 Schemas

```yaml
    Namespace:
      type: object
      required: [id, weight, reserved, hard_cap, enabled, created_at, updated_at]
      properties:
        id: { type: string }
        weight: { type: integer, minimum: 1, maximum: 100 }
        reserved: { type: integer, minimum: 0 }
        hard_cap: { type: integer, minimum: 0, description: "0 = unlimited" }
        enabled: { type: boolean }
        description: { type: string }
        created_at: { type: string, format: date-time }
        updated_at: { type: string, format: date-time }
    NamespaceCreate:
      type: object
      required: [id]
      properties:
        id: { type: string }
        weight: { type: integer, minimum: 1, maximum: 100, default: 10 }
        reserved: { type: integer, minimum: 0, default: 1 }
        hard_cap: { type: integer, minimum: 0, default: 100 }
        enabled: { type: boolean, default: true }
        description: { type: string }
    NamespaceUpdate:
      type: object
      properties:
        weight: { type: integer }
        reserved: { type: integer }
        hard_cap: { type: integer }
        enabled: { type: boolean }
        description: { type: string }
    NamespaceList:
      type: object
      required: [namespaces, mode]
      properties:
        namespaces: { type: array, items: { $ref: '#/components/schemas/Namespace' } }
        mode: { type: string, enum: [flat, multi-namespace] }
    NamespaceDetail:
      type: object
      required: [namespace, projects, recent_ticks]
      properties:
        namespace: { $ref: '#/components/schemas/Namespace' }
        projects: { type: array, items: { $ref: '#/components/schemas/Project' } }
        recent_ticks:
          type: array
          items:
            type: object
            properties:
              tick_group: { type: string }
              allocated: { type: integer }
              used: { type: integer }
              borrowed: { type: integer }
              lent: { type: integer }
              job_count: { type: integer }
    NamespaceNotFound:
      description: Namespace absent.
      content:
        application/json:
          schema: { $ref: '#/components/schemas/Error' }
          example: { error: "namespace not found" }
    NamespaceMethods:
      description: Wrong method.
      content:
        application/json:
          schema: { $ref: '#/components/schemas/Error' }
          example: { error: "GET, PUT, or POST only" }
```

### 11.3 Error Messages

|| Status | Message | When |
||--------|---------|------|
|| `400` | `"namespace id required"` | Create/update with empty id |
|| `400` | `"invalid JSON: <decoder error>"` | POST/PUT body decode failure |
|| `400` | `"project name required"` | Move without project field |
|| `404` | `"namespace not found"` | GET/PUT/DELETE on absent namespace |
|| `404` | `"project not found"` | Move references absent project |
|| `409` | `"namespace already exists"` | Duplicate namespace id |
|| `405` | `"GET or POST only"` | Wrong method on /namespaces |
|| `405` | `"GET, PUT, or POST only"` | Wrong method on /namespaces/{id} |
|| `405` | `"GET only"` | Wrong method on /namespaces/{id}/projects |
|| `405` | `"POST only"` | Wrong method on /namespaces/{id}/move |

### 11.4 Behavior

- **`GET /namespaces`**: Returns all namespaces with `mode: "multi-namespace"` when enabled. When `NamespaceMode=false`, returns `{"namespaces":[],"mode":"flat"}`.
- **`POST /namespaces`**: Creates namespace. ID must be unique. Weight defaults to 10, reserved to 1, hard_cap to 100 (unlimited), enabled to true.
- **`GET /namespaces/{id}`**: Returns namespace detail with its projects and last 20 namespace_ticks rows.
- **`PUT /namespaces/{id}`**: Partial update. Omitted fields unchanged. `updated_at` auto-set.
- **`GET /namespaces/{id}/projects`**: Lists all projects assigned to this namespace (same shape as `GET /projects`).
- **`POST /namespaces/{id}/move`**: Moves a project into this namespace. Body: `{"project":"<name>"}`. Project must exist. Returns `{"status":"moved","namespace":"<id>","project":"<name>"}`.
- **Namespace deletion**: Not exposed via API. Use SQLite directly or MCP for safety (prevents accidental mass unassignment).

### 11.5 Testing

```text
1. Create namespace, verify 201 with correct defaults
2. List namespaces, verify mode field
3. Get namespace detail with zero projects initially
4. Create project, move to namespace, verify GET /namespaces/{id}/projects includes it
5. Update namespace weight, verify 200 and persisted
6. Disable namespace, verify enabled=false
7. Verify 409 on duplicate namespace
8. Verify 404 on absent namespace
9. Verify 400 on missing required fields
10. Verify flat mode response when NamespaceMode=false
11. Verify all wrong-method 405 responses
```
