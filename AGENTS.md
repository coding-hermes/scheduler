# AGENTS.md — Coding Hermes Scheduler

AI agent guidelines for the Coding Hermes fleet scheduler. This is the central nervous system of the coding-hermes autonomous development fleet.

## Project Purpose

The Scheduler manages a fleet of 72 coding-hermes foreman projects (44 enabled; counts change as projects are added/disabled — see docs/fleet.md for the live mirror). It dispatches tick-based work cycles, enforces cooldowns, manages namespace-level resource allocation with multi-pool weight packing, and exposes both a human dashboard and a machine-readable REST API.

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
  --config fleet.toml \
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
| `--duckbrain-ns` | `coding-hermes` | DuckBrain namespace for sync |
| `--duckbrain-url` | `http://localhost:3000` | DuckBrain HTTP server URL |
| `--simulate` | `false` | Run in dry-run/simulation mode (no real spawning) |
| `--sim-success` | `0.85` | Simulated success rate (0.0-1.0) |
| `--sim-count` | `0` | Generate N simulated ticks and exit (0 = run loop) |
| `--gateway-url` | `http://127.0.0.1:8642` | Hermes gateway API URL (empty = use exec.Command) |
| `--gateway-key` | `$API_SERVER_KEY` | Hermes gateway API key |
| `--no-exec-fallback` | `true` | Disable exec.Command fallback when gateway fails (default true for safety) |
| `--foreman-home` | `~/.hermes/foreman` | HERMES_HOME path for foreman sessions |
| `--sim-setup` | `false` | Create test fixture with 13 dry-run projects (12 enabled + 1 disabled) |
| `--sim-ticks` | `10` | Number of evaluation ticks to run in sim-setup mode |
| `--config` | (none) | Path to TOML fleet config file |
| `--log-file` | `~/.hermes/coding-hermes/scheduler.log` | Path to append structured tick logs (JSON lines); empty disables |
| `--show-config` | `false` | Print resolved config (CLI + env) as TOML and exit |
| `--schema` | `false` | Output JSON Schema for schedulerd.toml and exit |
| `--failure-window` | `100` | Number of recent ticks per project for `/api/v1/status` per-project failure-rate breakdown (SCHED-GAP-018) |
| `--auto-disable-failure-rate` | `0` | Per-project failure-rate threshold (0.0–1.0) for auto-disable; `0` = off (SCHED-GAP-018) |
| `--auto-disable-window` | `100` | Ticks per project over which auto-disable failure rate is computed (SCHED-GAP-018) |
| `--auto-disable-min-ticks` | `50` | Minimum ticks in window before auto-disable can fire (SCHED-GAP-018) |

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
  config/           — TOML config loader: fleet config (wired via `--config`), root schedulerd.toml LoadConfig (exists but NOT wired into daemon boot until FEAT-005), env var interpolation.
  mcp/              — MCP server for AI agent integration (JSON-RPC over HTTP).
  sync/             — DuckBrain sync: pushes fleet state to DuckBrain memory.
```

## Endpoints

| Route | Purpose |
|-------|---------|
| `/` | Fleet dashboard (full HTML page) |
| `/dashboard/partial` | htmx partial: project table refresh |
| `/projects/{name}` | Per-project detail page |
| `/queue` | Global queue view |
| `/ticks?page=N` | Paginated tick history |
| `/namespaces/{id}` | Namespace drill-down |
| `/health` | Dashboard health panel |
| `/api/v1/health` | Machine health check (JSON) |
| `/api/v1/status` | Fleet status summary (JSON) |
| `/api/v1/projects` | List/manage projects |
| `/api/v1/namespaces` | List namespaces |
| `/api/v1/ticks` | List ticks |
| `/api/v1/events` | List event log (GET; SSE streaming supported) |
| `/api/v1/evaluate` | Trigger re-evaluation |
| `/api/v1/pause` | Pause scheduling (POST) |
| `/api/v1/resume` | Resume scheduling (POST) |
| `/api/v1/config` | Resolved daemon config (JSON) |
| `/api/v1/queue` | Global queue (JSON) |
| `/api/v1/openapi.json` | OpenAPI schema (JSON) |
| `/mcp` | MCP JSON-RPC endpoint |

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

## Key Design Decisions

- **Eval-stall watchdog (GAP-042).** The evaluation loop is event-driven (startup + slot-freed debounce only) — when the fleet is fully idle (all projects in cooldown, 0 running ticks) nothing re-triggers evaluation, so cooldown-expired projects can sit unscheduled for hours (observed 66-min silent gap 2026-08-13, recovered only by manual POST /api/v1/evaluate). The 30s health ticker now checks `lastEval` age: past 10× min-interval (5 min) with 0 running ticks it forces a re-evaluation and emits a HIGH `loop` event ("eval loop stalled — forced re-evaluation"), re-emitted every 30 min while the stall persists. Grep-able `EVAL-STALL:` log line in scheduler.log.
- **Zero-select monitoring (GAP-043).** Evaluations log nothing on an empty pick, so an operator cannot tell "evaluating" from "evaluating nothing" (observed 2026-08-13 20:55-21:08Z: evals every ~5 min, last `EVAL: N selected` line 15:55:06). `evaluate()` now counts consecutive zero-select evals with eligible projects present (enabled, not running, cooldown elapsed); at 2 consecutive it logs a distinct `EVAL-ZERO-SELECT:` line and emits a HIGH `loop` event, re-emitted at most once per 30 min while the condition persists. Diagnostics (`zero_select_consecutive` / `zero_select_eligible` / `zero_select_last_at`) are exposed in `/api/v1/status`. A zero select with no eligible projects is normal fleet-idle and resets the counter.
- **Disable provenance (GAP-044).** Disabled projects previously carried no record of how/when/why they were disabled (ch-delta: enabled=false, no reason, no event). Every disable path now stamps `disabled_at`/`disabled_by`/`disabled_reason` on the projects row — API pause (`api-pause`), PUT enabled=false (`api`), DELETE confirm=true (`api-delete`, with COALESCE legacy backfill for pre-migration rows), and the failure-rate auto-disable (`auto-disable` with failure stats as the reason) — and the API paths mirror a matching entry into the events table. A false→true transition (resume) clears all three. Fields are exposed in `/api/v1/projects` and the dashboard.
- **No timeout backoff.** Timeout means try again at normal cooldown — do not escalate.
- **Configurable auto-disable (SCHED-GAP-018, default off).** When `--auto-disable-failure-rate` > 0 (e.g. 0.95 = 95%), a project whose recent failure rate (failed+timeout over the last `--auto-disable-window` ticks, default 100) meets or exceeds the threshold AND has at least `--auto-disable-min-ticks` (default 50) ticks in the window is automatically disabled. A HIGH event is emitted to the events table on disable. Operators must explicitly opt in; the default (0) leaves the feature off. The same flags are available as env vars (`SCHEDULER_AUTO_DISABLE_FAILURE_RATE`, `SCHEDULER_AUTO_DISABLE_WINDOW`, `SCHEDULER_AUTO_DISABLE_MIN_TICKS`, `SCHEDULER_FAILURE_WINDOW`). The TOML `[scheduler]` keys (`auto_disable_failure_rate`, `auto_disable_window`, `auto_disable_min_ticks`) are documented in the `--schema` output but are NOT loaded from a root `schedulerd.toml` yet — root TOML wiring arrives in FEAT-005 (DOGFOOD-012). The existing 10+ consecutive-timeout/24h safety net remains.
- **Per-project failure-rate visibility (SCHED-GAP-018).** `GET /api/v1/status` now includes `projects_failure_rates` (per-project breakdown over the last `--failure-window` ticks) and `failure_window` fields, so per-project failure rates stay observable without manual SQL.
- **Cooldown authority model (SCHED-GAP-025).** `fleet-cooldown-policy.py` (ops script at `~/.hermes/scripts/fleet-cooldown-policy.py`) is the ONLY writer of `fleet.toml`; it reads live SQLite state first, honors the `ELEVATED_PINS` whitelist (h3=21600, warpfs=43200 — never written below the pin), and regenerates `fleet.toml` so restarts re-pin existing projects (loader.go, not create-only). An API `PUT /api/v1/projects/{name}` cooldown change is durable only within the daemon session — the next policy run normalizes it back unless the project is in `ELEVATED_PINS`. To pin a project permanently: add it to `ELEVATED_PINS` in the policy script + set the pin in `fleet.toml`, then run the script with `--apply`.
- **Gateway key never in argv (GAP-038).** The daemon's `--gateway-key` flag defaults to `$API_SERVER_KEY` (main.go), which the systemd unit supplies via `EnvironmentFile` pointing at a 0600 env file (`/etc/coding-hermes/gateway.env`, template `deploy/gateway.env.example`). Do NOT add `--gateway-key` to `ExecStart` — argv is world-readable via `ps aux`.
- **Local layout (GAP-039/040).** The canonical checkout on the fleet host is `/home/kara/coding-hermes-scheduler/coding-herms-scheduler/` (typo'd double nesting); the outer `/home/kara/coding-hermes-scheduler/` dir is NOT the repo — it holds only the `.coding-hermes/tasks.md` pointer. Build/run from the inner checkout.
- **Foremen never use delegate_task.** Workers are spawned via `hermes chat -q` with independent model/provider selection.

## Project Conventions

- Go doc comments on all public functions
- Sequential test runs (`-p 1`) due to cgroup pids limits
- Co-author via `CODING_HERMES_CO_AUTHOR` env var
- GitReins guards enforce secrets, build, lint, and tests before commit
