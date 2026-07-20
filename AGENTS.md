# AGENTS.md — Coding Hermes Scheduler

AI agent guidelines for the Coding Hermes fleet scheduler. This is the central nervous system of the coding-hermes autonomous development fleet.

## Project Purpose

The Scheduler manages a fleet of 39+ coding-hermes foreman projects. It dispatches tick-based work cycles, enforces cooldowns, manages namespace-level resource allocation with multi-pool weight packing, and exposes both a human dashboard and a machine-readable REST API.

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

# Run (requires gateway at :8642)
./bin/schedulerd --db ~/.hermes/coding-hermes/scheduler.db \
  --max-concurrent 4 --min-interval 30s \
  --tick-timeout 7200s \
  --gateway-url http://127.0.0.1:8642 \
  --gateway-key <key> \
  --no-exec-fallback
```

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
  config/           — TOML config loader: root config, fleet config, env var interpolation.
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
| `/api/v1/evaluate` | Trigger re-evaluation |
| `/mcp` | MCP JSON-RPC endpoint |

## Key Design Decisions

- **No timeout backoff.** Per Bane's fleet rule: timeout means try again at normal cooldown.
- **No auto-disable.** Only human command or scheduler daemon after 10+ consecutive timeouts over 24h.
- **Foremen never use delegate_task.** Workers are spawned via `hermes chat -q` with independent model/provider selection.
- **Multi-pool weight packing.** Namespaces get weighted allocations; within each namespace, urgency-scored projects are packed into available slots.
- **Scheduler daemon runs via bash wrapper, not systemd** — FIX-STUCK blocked by Bane.

## Project Conventions

- Go doc comments on all public functions
- Sequential test runs (`-p 1`) due to cgroup pids limits
- Co-author via `CODING_HERMES_CO_AUTHOR` env var in `~/.hermes/.env`
- GitReins guards enforce secrets, build, lint, and tests before commit
- Hilo graph tracks dependency edges (385 edges, 54 files as of 2026-07-20)
