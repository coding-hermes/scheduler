# Coding Hermes Scheduler — Operator Integration Guide

Live-verified 2026-08-18 against `schedulerd` on `127.0.0.1:9090` (spawns_http=49 /
spawns_exec=0, CI green, `./bin/schedulerd --test-verify 3` → `SCHEDULER VERIFIED`).
Every example below was exercised against the running daemon; response bodies are
trimmed for readability.

## 1. Overview

The Coding Hermes Scheduler is a Go daemon that packs fleet projects into a weight
budget (default 100, max concurrency 10) and spawns foreman ticks through the Hermes
gateway HTTP API (`--gateway-url`, default `http://127.0.0.1:8642`) — gateway-first
spawns are the primary mechanism. The legacy `exec.Command("hermes", "chat", ...)`
fallback is disabled by default (`--no-exec-fallback=true`), so a gateway outage
drops ticks rather than silently degrading to local CLI spawns.
It listens on `127.0.0.1:9090` (loopback only) and persists state in
`~/.hermes/coding-hermes/scheduler.db`.

| Thing | Value |
|-------|-------|
| Base URL | `http://127.0.0.1:9090` |
| API root | `/api/v1` |
| OpenAPI | `/api/v1/openapi.json` |
| Resolved config | `GET /api/v1/config` (three-layer snapshot TOML < env < CLI; gateway key masked) |
| MCP (JSON-RPC) | `POST /mcp` |
| HTML dashboard | `/dashboard/partial` (also `/` and `/health`) |
| Wire format | **snake_case** emission (`cooldown_s`, `repo_url`, `created_at`) |
| Request dialects | snake_case AND legacy PascalCase (`Name`, `RepoURL`, `CooldownS`) both accepted |

## 2. Wire format

Responses are snake_case per `specs/S06-rest-api.md`. Requests accept both snake_case
and the legacy PascalCase Go field names so pre-conformance automation keeps working.

On create, omitted `weight`/`priority`/`cooldown_s`/`decay_rate` default to
`10`/`5`/`900`/`1.0`. **New projects are created disabled** — resume them explicitly.

## 3. Health & status

```bash
curl -s http://127.0.0.1:9090/api/v1/health
# {"active_ticks":4,"db":"connected","evaluation_age_seconds":116,"last_evaluation":"...Z",
#  "spawns_exec":0,"spawns_http":49,"status":"ok","uptime":"2h40m20s"}
```

- `spawns_exec` / `spawns_http` — tick spawn mechanism. Gateway HTTP spawns are the
  PRIMARY mechanism and the fleet norm: `spawns_http` rising with `spawns_exec=0`
  (the live fleet matches this shape). `spawns_exec > 0` occurs only when the gateway
  is unavailable AND the operator explicitly enabled the fallback
  (`--no-exec-fallback=false`); with the default `true`, a gateway outage drops ticks
  instead of exec-spawning (GAP-048). See `docs/dogfood/diagnostics.md`.

```bash
curl -s http://127.0.0.1:9090/api/v1/status
# {"active_projects":44,"active_ticks":4,"budget_total":100,"failure_window":100,
#  "last_evaluation":"...Z","projects_failure_rates":{...}, ...}
```

## 4. Projects

### List all projects

```bash
curl -s http://127.0.0.1:9090/api/v1/projects
# {"projects":[{"name":"9router","repo_url":"local:/home/kara/9router","workdir":"/home/kara/9router",
#   "weight":15,"priority":10,"cooldown_s":900,"decay_rate":1,"enabled":true, ...}]}
```

```bash
# Field access example (jq)
curl -s http://127.0.0.1:9090/api/v1/projects | jq '.projects[0].cooldown_s'
# 900
```

### Get one project (includes latest tick)

```bash
curl -s http://127.0.0.1:9090/api/v1/projects/coding-hermes-scheduler
# {"latest_tick":{"id":"coding-hermes-scheduler-2026-08-08-07-06-18","status":"running", ...},
#  "project":{"name":"coding-hermes-scheduler","cooldown_s":7200,"enabled":true, ...}}
```

### Create a project (documented body → 201)

```bash
curl -s -X POST http://127.0.0.1:9090/api/v1/projects \
  -H 'Content-Type: application/json' \
  -d '{"name":"my-project","repo_url":"local:/home/kara/my-project","workdir":"/home/kara/my-project"}'
# 201 — defaults applied, created DISABLED:
# {"name":"my-project","weight":10,"priority":5,"cooldown_s":900,"decay_rate":1,
#  "enabled":false,"created_at":"2026-08-08T12:10:52Z", ...}
```

Error contract:

| Case | Status | Body |
|------|--------|------|
| Missing `name`/`repo_url`/`workdir` | 400 | `name, repo_url, workdir are required` |
| CHECK violation (weight 1..100, priority 1..10, decay_rate > 0) | 400 | actionable message |
| Duplicate name | 409 | `project already exists` |
| Duplicate workdir owned by an enabled project | 409 | `already registered by enabled project ...` |

### Update a project (partial)

```bash
curl -s -X PUT http://127.0.0.1:9090/api/v1/projects/my-project \
  -H 'Content-Type: application/json' -d '{"cooldown_s":14400,"enabled":true}'
# 200 — updated project object
```

Guard: `decay_rate` must be > 0 (`0` causes permanent starvation — urgency never grows).

### Delete (soft) a project

```bash
# Without confirm: 400
curl -s -X DELETE http://127.0.0.1:9090/api/v1/projects/my-project
# {"error":"confirm=true query param required — this soft-deletes the project (enabled=false)"}

# Enabled projects are rejected: 409 — pause/disable first
curl -s -X DELETE "http://127.0.0.1:9090/api/v1/projects/my-project?confirm=true"
# {"error":"project is enabled — pause it first (PUT Enabled=false or POST /projects/{name}/pause) before deleting"}

# Pause, then delete: 200
curl -s -X POST http://127.0.0.1:9090/api/v1/projects/my-project/pause
curl -s -X DELETE "http://127.0.0.1:9090/api/v1/projects/my-project?confirm=true"
# {"project":"my-project","status":"deleted"}
```

### Per-project pause / resume / spawn

```bash
curl -s -X POST http://127.0.0.1:9090/api/v1/projects/my-project/pause
# 200
curl -s -X POST http://127.0.0.1:9090/api/v1/projects/my-project/resume
# 200
curl -s -X POST http://127.0.0.1:9090/api/v1/projects/my-project/spawn
# 200 — forces an immediate tick for that project
```

## 5. Scheduling control (fleet-wide)

```bash
# Pause the whole scheduler loop — stops NEW spawns (running ticks finish)
curl -s -X POST http://127.0.0.1:9090/api/v1/pause
# {"status":"paused"}

# Resume
curl -s -X POST http://127.0.0.1:9090/api/v1/resume
# {"status":"resumed"}
```

Use with care — pausing stops the entire fleet. Verify the loop is back with
`GET /api/v1/health` after resume.

## 6. Observability

```bash
curl -s "http://127.0.0.1:9090/api/v1/ticks?limit=10"     # recent ticks, newest first
curl -s http://127.0.0.1:9090/api/v1/ticks/<tick-id>      # single tick detail
curl -s "http://127.0.0.1:9090/api/v1/events?limit=50"    # event log
curl -s http://127.0.0.1:9090/api/v1/queue                # queued/pending pack state
curl -s http://127.0.0.1:9090/api/v1/namespaces           # namespaces
curl -s http://127.0.0.1:9090/api/v1/namespaces/<id>      # namespace detail
curl -s http://127.0.0.1:9090/dashboard/partial           # live dashboard HTML partial
```

## 7. MCP endpoint

The daemon exposes its own MCP server (JSON-RPC 2.0 over HTTP):

```bash
curl -s -X POST http://127.0.0.1:9090/mcp \
  -H 'Content-Type: application/json' \
  -d '{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}'
# 200 — {"jsonrpc":"2.0","result":{"tools":[...]}, ...}
```

## 8. Self-verification

The repo ships a self-check that the host crontab runs every 2 hours:

```bash
./deploy/scheduler-verify.sh          # wraps: ./bin/schedulerd --test-verify 3
# ... 6 checks, 0 failures
# ✅ SCHEDULER VERIFIED
```

Exit 0 = all checks pass. Logs land in `deploy/verify-*.log`. CI runs the same
`./bin/schedulerd --test-verify 3` step on every push to `main`.

## 9. Fleet integration notes

- Project names are **case-sensitive** in the API and DB (`heading` ≠ `HEADING`).
- The scheduler API routes are case-sensitive: `GET /api/v1/projects/heading` 404s
  if the project is stored as `HEADING`. Always use the exact name from
  `GET /api/v1/projects`.
- `GET /api/v1/projects/{name}` wraps the project under a `"project"` key alongside
  `"latest_tick"`; `POST/PUT` return the project object flat. Do not copy one
  parsing shape into the other.
- New projects are disabled by default — resume them before expecting ticks.
- The scheduler evaluates every 60s. Cooldown authority model (code-verified
  in `internal/config/loader.go`, SCHED-GAP-025): `fleet.toml` is the DURABLE
  pin — at every daemon startup, `ApplyFleetConfig` re-pins existing projects'
  cooldown/model/provider/enabled from `fleet.toml`, overwriting API-side
  changes made before the restart. API PUTs write SQLite and take effect
  immediately, but for projects listed in `fleet.toml` they survive only until
  the next restart. `fleet-cooldown-policy.py` (ops script,
  `~/.hermes/scripts`) is the ONLY writer of `fleet.toml`: it reads live API
  (SQLite) state first, applies the pending-based 900s/7200s policy plus the
  ELEVATED_PINS whitelist, PUTs the normalized cooldowns, then regenerates
  `fleet.toml` so restarts re-pin to the policy decision. To pin a custom
  cooldown durably, set it in `fleet.toml` AND whitelist the project in
  ELEVATED_PINS (SCHED-GAP-012); otherwise the next policy run normalizes it
  back.
