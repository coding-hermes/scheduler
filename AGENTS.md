# AGENTS.md — Coding Hermes Scheduler

AI agent guidelines for the Coding Hermes fleet scheduler. This is the central nervous system of the coding-hermes autonomous development fleet.

## Project Purpose

The Scheduler manages a fleet of coding-hermes foreman projects (counts change as projects are added/disabled — see docs/fleet.md for the live count and enabled subset, regenerated from the running daemon). It dispatches tick-based work cycles, enforces cooldowns, manages namespace-level resource allocation with multi-pool weight packing, and exposes both a human dashboard and a machine-readable REST API.

## Tech Stack

- **Language:** Go 1.26+
- **Database:** SQLite (via modernc.org/sqlite — pure Go, no CGO)
- **Frontend:** htmx + server-rendered HTML templates
- **Transport:** HTTP (net/http with Go 1.22+ ServeMux patterns)
- **Config:** TOML (BurntSushi/toml)
- **CI:** GitHub Actions (golangci-lint, go test)

## Build & Run

```
# Build
go build -o bin/schedulerd ./cmd/schedulerd/

# Test (sequential — cgroup pids limits in fleet environment)
go test -short -p 1 ./...

# Lint
golangci-lint run

# Run (requires Hermes gateway)
./bin/schedulerd --db <YOUR_DB_PATH>/scheduler.db \
  --listen 127.0.0.1:9090 \
  --max-concurrent 4 --min-interval 30s \
  --tick-timeout 7200s \
  --budget 100 \
  --namespace-mode \
  --gateway-url <YOUR_GATEWAY_URL> \
  --gateway-key <YOUR_GATEWAY_KEY> \
  --foreman-home ~/.hermes/foreman \
  --config ~/.hermes/fleet.toml \
  --no-exec-fallback
```

All flags (defaults match `cmd/schedulerd/main.go` — the canonical source):

| Flag | Default | Description |
|------|---------|-------------|
| `--db` | `~/.hermes/coding-hermes/scheduler.db` | SQLite database path |
| `--listen` | `127.0.0.1:9090` | HTTP listen address |
| `--min-interval` | `30s` | Fastest tick interval |
| `--max-interval` | `24h` | Slowest tick interval |
| `--num-levels` | `10` | Number of priority levels |
| `--budget` | `100` | Weight budget |
| `--max-concurrent` | `10` | Max concurrent foremen |
| `--namespace-mode` | `false` | Enable multi-namespace scheduling |
| `--tick-timeout` | `2h` | Maximum tick duration before timeout (2h) |
| `--test-verify` | `0` | Run N-cycle correctness verification and exit |
| `--duckbrain-ns` | `scheduler` | DuckBrain namespace for sync |
| `--duckbrain-url` | `http://localhost:3000` | DuckBrain HTTP server URL |
| `--duckbrain-interval` | `5m0s` | DuckBrain sync interval (spool replay cadence) |
| `--simulate` | `false` | Run in dry-run/simulation mode (no real spawning) |
| `--sim-success` | `0.85` | Simulated success rate (0.0-1.0) |
| `--sim-count` | `0` | Generate N simulated ticks and exit (0 = run loop) |
| `--gateway-url` | `http://127.0.0.1:8642` | Hermes gateway API URL (empty = use exec.Command) |
| `--gateway-key` | `$API_SERVER_KEY` | Hermes gateway API key |
| `--no-exec-fallback` | `true` | Disable exec.Command fallback when gateway fails (default true for safety) |
| `--version` | `false` | Print version/build info and exit; version resolves ldflags tag → vcs buildinfo (dev-<shorthash>) → dev; same identity serves /api/v1/health, openapi info.version, MCP serverInfo.version |
| `--foreman-home` | `~/.hermes/foreman` | HERMES_HOME path for foreman sessions |
| `--sim-setup` | `false` | Create test fixture with 13 dry-run projects (12 enabled + 1 disabled) |
| `--sim-ticks` | `10` | Number of evaluation ticks to run in sim-setup mode |
| `--config` | (none) | Path to TOML fleet config file |
| `--log-file` | `~/.hermes/coding-hermes/scheduler.log` | Path to append structured tick logs (JSON lines); empty disables |
| `--show-config` | `false` | Print resolved config (CLI + env layers) as TOML and exit — the root-TOML layer resolves later in boot and is NOT reflected here |
| `--schema` | `false` | Output JSON Schema for schedulerd.toml and exit |
| `--failure-window` | `100` | Number of recent ticks per project for `/api/v1/status` per-project failure-rate breakdown (SCHED-GAP-018) |
| `--auto-disable-failure-rate` | `0` | Per-project failure-rate threshold (0.0–1.0) for auto-disable; `0` = off (SCHED-GAP-018) |
| `--auto-disable-window` | `100` | Ticks per project over which auto-disable failure rate is computed (SCHED-GAP-018) |
| `--auto-disable-min-ticks` | `50` | Minimum ticks in window before auto-disable can fire (SCHED-GAP-018) |

DuckBrain sync auth: when `DUCKBRAIN_API_KEY` is set, every sync request carries it as the `X-API-Key` header (`internal/sync/duckbrain.go`), and the daemon validates the key once at startup with a side-effect-free probe — a rejected key (HTTP 401/403) fails fast with a distinct HIGH `DuckBrain API key REJECTED` event and gates sync cycles off instead of spooling every failed write. A 429 is treated as backpressure, not an error: the current burst stops, remaining writes spool and replay on the next `--duckbrain-interval` tick. With the env var unset or empty the daemon stays in pre-auth compatibility mode — no probe and no header (deliberate, not a bug).

## Architecture

```
cmd/schedulerd/     — Entry point. Wires HTTP mux, starts daemon, registers all routes.
internal/
  scheduler/        — Core scheduling engine: namespace allocation, urgency calculation,
                      multi-pool weight packing, spawn lifecycle, cooldown management,
                      slowdown/backoff, zombie detection, alert escalation, delivery.
  api/              — REST API server (/api/v1/*): projects, namespaces, ticks, status, evaluation.
  database/         — SQLite data layer: projects, namespaces, ticks, events, migrations.
  dashboard/        — HTML dashboard generator: fleet overview, project detail, queue view,
                      tick history, namespace view, health panel. htmx-powered partials.
  config/           — TOML config loader: fleet config (wired via `--config`), root schedulerd.toml LoadRootConfig (LOADED at daemon boot since FEAT-005 — applied-key list under "Root schedulerd.toml layer is LIVE" in Key Design Decisions), env var interpolation.
  mcp/              — MCP server for AI agent integration (JSON-RPC over HTTP).
  sync/             — DuckBrain sync: pushes fleet state to DuckBrain memory.
```

## Test-time simulator (SCHED-GAP-169)

Every clock read and wait in non-test code goes through `internal/clock.Clock` — the daemon's single time choke point. Injection is per component: each long-lived struct holds a `clock.Seam` (zero value = wall clock) and `Loop.SetClock` propagates one clock to every component it owns (spawner, slot pool, lifecycle tracker, sim spawner, gateway client, board watcher); free functions that already thread a `context.Context` take theirs from `clock.WithClock(ctx, c)` (the sql-backed DB helpers). There is deliberately NO package-level global clock, so two components — or two tests in one binary — can hold different clocks. `internal/clock/stdlib_guard_test.go` fails the build the moment a direct `time.Now()/time.Since()/time.Until()/time.Sleep()/time.After()/time.NewTicker()/time.AfterFunc()` reappears outside `internal/clock` (comment lines are allowed and reported).

Three implementations:

| Clock | Use |
|-------|-----|
| `clock.Real()` | production; stdlib delegation, identical to the calls it replaced (the default) |
| `clock.NewFixed(t)` | the ADV-R04/G6 determinism seam: frozen decision instant, real waits |
| `clock.NewSimClock(scale)` | the test-time simulator |

| Env var | Values | Meaning |
|---------|--------|---------|
| `SCHEDULER_TIME_MODE` | `real` (default) \| `sim` | selects the implementation |
| `SCHEDULER_TIME_SCALE` | positive float, default `1.0` | simulator speed: a blocked wait costs `d/scale` of REAL time while virtual now advances the full `d` (10x/100x/1000x) |
| `SCHEDULER_TIME_START` | RFC3339 | simulator start instant (default: now) |
| `SCHEDULER_TIME_AUTOADVANCE` | `0` (default) \| `1` | skip straight to the next armed timer at zero real cost |

Sim mode is REFUSED at boot unless `--simulate` is also set (a stray env var must never put the live fleet on a fake clock) and every boot logs its clock: `TIME: clock real` or `TIME: clock sim (scale=1000 start=… auto=false driven=true)`. `clock.FromEnv` fails closed on any unparseable value.

In tests, install a simulator with `l.SetClock(clock.NewSimClockAt(1000, time.Now()))` and drive it: `Advance`/`AdvanceTo` jump virtual time and fire every due timer in (deadline, registration-sequence) order, `WaitForNextTimer` is the check-in point (park until the next timer is due instead of sleeping for a fixed period), `RunUntilIdle` drains one-shot timers, and `clock.NewManualSimClock(t)` is a dormant clock that moves only when the test says so. `internal/scheduler/clock_simulator_e2e_test.go` runs a drain-window + 2h adaptive-cooldown-expiry scenario — a shape that previously required real hours — in ~0.2s.

## Endpoints

The table names the complete in-repo route set — the HTML pages registered in `cmd/schedulerd/main.go` and every `/api/v1/*` path registered by `internal/api/server.go`. `internal/mcp/agents_endpoint_parity_test.go` fails the build when this table drifts from those registrations in either direction (an undocumented route, or a row for a route that no longer exists). The full per-endpoint reference (request/response bodies, status codes, curl recipes) lives in [docs/api.md](docs/api.md).

| Route | Purpose |
|-------|---------|
| `/` | Fleet dashboard (full HTML page; `/dashboard` is an alias) |
| `/dashboard/partial` | htmx partial: project table refresh |
| `/static/htmx.min.js` | Bundled htmx asset (Go embed) |
| `/projects/{name}` | Per-project detail page |
| `/queue` | Global queue view |
| `/ticks?page=N` | Paginated tick history |
| `/namespaces/{id}` | Namespace drill-down |
| `/health` | Dashboard health panel |
| `/api/v1/health` | Machine health check (JSON) |
| `/api/v1/live` | DB-free liveness probe (JSON) — process-memory fields only, safe under a saturated single-SQLite-connection fleet; watchdog's first probe, `/api/v1/health` is the rich fallback (SCHED-GAP-204-A) |
| `/api/v1/status` | Fleet status summary (JSON) — budget, spend tiers, failure rates, eval/zero-select diagnostics |
| `/api/v1/config` | Resolved daemon config (JSON) |
| `/api/v1/metrics` | Read-only fleet metrics: spawns, deferrals, nudges, ticks, durations, drains and outcomes in one request (SCHED-GAP-156) |
| `/api/v1/projects` | List/manage projects (GET/POST) |
| `/api/v1/projects/{name}` | One project: detail (GET) / partial update (PUT) / delete (DELETE — `?confirm=true`; `&purge=true` hard-deletes) plus the `/pause`, `/resume`, `/spawn`, `/bump`, `/unbump` sub-routes |
| `/api/v1/namespaces` | List namespaces |
| `/api/v1/namespaces/{id}` | Namespace detail (GET) / partial update (PUT) / delete (DELETE — `?confirm=true` soft-deletes: enabled=false, member projects unassigned, row retained; `?confirm=true&purge=true` hard-deletes the row permanently, SCHED-GAP-097) |
| `/api/v1/groups` | Deploy groups — named project lists, JSONL-backed (GET list / POST create) |
| `/api/v1/groups/{name}` | One deploy group: GET / partial update (PUT, name immutable) / DELETE |
| `/api/v1/groups/{name}/deploy` | POST: apply a template's task rows to every member project's board (`dry_run=true` plans without writing) |
| `/api/v1/templates` | Deploy templates — named task definitions, JSONL-backed (GET list / POST create) |
| `/api/v1/templates/{name}` | One deploy template: GET / partial update (PUT, name immutable) / DELETE |
| `/api/v1/ticks` | List ticks |
| `/api/v1/ticks/{id}` | One tick by id (GET) |
| `/api/v1/events` | List event log (GET) |
| `/api/v1/events/stream` | SSE push stream of the event log (CTL-002) |
| `/api/v1/evaluate` | Trigger re-evaluation |
| `/api/v1/pause` | Pause scheduling (POST) |
| `/api/v1/resume` | Resume scheduling (POST) |
| `/api/v1/queue` | Global queue (JSON) |
| `/api/v1/openapi.json` | OpenAPI schema (JSON) |
| `/mcp` | MCP JSON-RPC endpoint |

**MCP surface:** `POST /mcp` serves **45 tools** — the registry in `internal/mcp/server.go` is the source of truth and the full per-tool table is in the README's [MCP Tools](README.md#mcp-tools) section (not duplicated here). Two guards keep the documented surface honest against that registry: `internal/mcp/readme_tools_parity_test.go` (README tool table ↔ registry) and `internal/mcp/agents_endpoint_parity_test.go` (this endpoint table ↔ the route registrations). A running daemon built from an older tree reports fewer tools — measure, don't assume.

## Manual Database Operations

The daemon's default DB path is `~/.hermes/coding-hermes/scheduler.db` (`--db` flag; `db_path` in config). For operations the API deliberately guards against, operators can go straight to SQLite:

- **Remove a junk test-dummy project (soft delete, same semantics as the API):**

  ```sh
  sqlite3 ~/.hermes/coding-hermes/scheduler.db "UPDATE projects SET enabled=0 WHERE name='<name>';"
  ```

  The row is retained (historical ticks stay referentially valid); the project just stops being scheduled. Prefer the API (`DELETE /api/v1/projects/{name}?confirm=true`) when the daemon is up — it refuses enabled projects with 409. This fallback bypasses that guard, so only use it on projects you are certain are dead weight.

- **Hard-delete (only when the row itself must go, e.g. a typo'd name):**

  ```sh
  sqlite3 ~/.hermes/coding-hermes/scheduler.db "DELETE FROM projects WHERE name='<name>';"
  ```

  Prefer the API (`DELETE /api/v1/projects/{name}?confirm=true&purge=true`, DOGFOOD-009) when the daemon is up — it applies the same guards as soft delete (400 without confirm, 409 while enabled) and, unlike this SQL fallback, keeps foreign-key enforcement intact on the daemon connection. Both paths retain historical ticks (referenced by name string); `/api/v1/status` failure rates only include existing projects, so purged rows never resurface as ghosts.

## Design Decisions

One line per decision; full rationale in [docs/design-decisions.md](docs/design-decisions.md)
(moved there 2026-09-22, SCHED-GAP-220 — this file is a working guide, not a decision log).

- **Eval-stall watchdog (GAP-042)**
- **Zero-select monitoring (GAP-043)**
- **Disable provenance (GAP-044)**
- **No timeout backoff**
- **Configurable auto-disable (SCHED-GAP-018, default off)**
- **Root schedulerd.toml layer is LIVE (FEAT-005 landed)**
- **Per-project failure-rate visibility (SCHED-GAP-018)**
- **Cooldown authority model (SCHED-GAP-025)**
- **Gateway key never in argv (GAP-038)**
- **Local layout (GAP-039/040)**
- **Foremen never use delegate_task**
- **Budget authority chain (ADV-R09/G8)**
- **Adaptive-ceiling single authority (ADV-R10)**
- **Board-driven wake + ordering-only boost (ADV-R07, Option C)**
- **Tier ladder (G2 ruling)**
- **Wave workers cap is advisory forever (Option C sub-ruling, G9, ADV-R14)**

## Project Conventions

- Go doc comments on all public functions
- Sequential test runs (`-p 1`) due to cgroup pids limits
- Co-author via `CODING_HERMES_CO_AUTHOR` env var
- GitReins guards enforce secrets, build, lint, and tests before commit
