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

# Run (requires Hermes gateway) — the canonical invocation is in docs/reference/flags.md
```

All flags and defaults (defaults match `cmd/schedulerd/main.go` — the canonical source) are tabulated in [docs/reference/flags.md](docs/reference/flags.md).

DuckBrain sync auth — `X-API-Key` when `DUCKBRAIN_API_KEY` is set, the once-at-startup probe, 429 backpressure, and pre-auth compatibility mode — is documented in [docs/reference/flags.md](docs/reference/flags.md#duckbrain-sync-auth).

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

The three clock implementations and the `SCHEDULER_TIME_*` env vars are tabulated in [docs/reference/clock-modes.md](docs/reference/clock-modes.md).

Sim mode is REFUSED at boot unless `--simulate` is also set (a stray env var must never put the live fleet on a fake clock) and every boot logs its clock: `TIME: clock real` or `TIME: clock sim (scale=1000 start=… auto=false driven=true)`. `clock.FromEnv` fails closed on any unparseable value.

In tests, install a simulator with `l.SetClock(clock.NewSimClockAt(1000, time.Now()))` and drive it: `Advance`/`AdvanceTo` jump virtual time and fire every due timer in (deadline, registration-sequence) order, `WaitForNextTimer` is the check-in point (park until the next timer is due instead of sleeping for a fixed period), `RunUntilIdle` drains one-shot timers, and `clock.NewManualSimClock(t)` is a dormant clock that moves only when the test says so. `internal/scheduler/clock_simulator_e2e_test.go` runs a drain-window + 2h adaptive-cooldown-expiry scenario — a shape that previously required real hours — in ~0.2s.

## Endpoints

The complete in-repo route table and the MCP surface statement live in [docs/reference/endpoints.md](docs/reference/endpoints.md). `internal/mcp/agents_endpoint_parity_test.go` fails the build when that table drifts from the route registrations in either direction (an undocumented route, or a row for a route that no longer exists). The full per-endpoint reference (request/response bodies, status codes, curl recipes) lives in [docs/api.md](docs/api.md).

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
