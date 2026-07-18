# Changelog

All notable changes to the Coding Hermes Scheduler.

## [1.0.0] — 2026-07-18

### Core Scheduler

- **Dynamic priority-weighted fleet scheduler** — single Go binary replaces 33+ static cron jobs
- **Urgency-based packing** — greedy knapsack fill with configurable weight budget and max concurrency
- **Geometric priority curve** — priority 1-10 maps to intervals from 20 minutes to 24 hours
- **Dynamic cooldown** — derived from priority when explicit cooldown is 0, preventing starvation
- **Auto-slowdown for idle projects** — doubles cooldown after consecutive idle ticks (capped at 4h), resets on first non-idle tick
- **Process-liveness zombie detection** — `/proc/pid/stat` instead of blind timeouts (30min cap rejected)

### HTTP API Spawn (FEAT-003)

- **Zero subprocess overhead** — `POST /v1/responses` to Hermes gateway instead of `exec.Command`
- **No MCP duplication** — duckbrain + gitreins loaded once by gateway, shared across ticks
- **~500MB → 0MB per tick** process overhead eliminated
- **Graceful fallback** — exec.Command when gateway is unreachable
- **Per-foreman MCP optimization** — HERMES_HOME with minimal config (duckbrain+gitreins only, no browser/chimera/flights)

### Dedicated Gateway (FEAT-004)

- **Cgroup isolation** — separate Hermes instance on :8643 with MemoryMax=16G
- **Independent restart cycle** — scheduler OOM doesn't kill main chat
- **Scheduler profile** — minimal MCPs, auto-approve mode, PAYG foreman provider

### API & Control Plane

- **REST API** — 15 endpoints (health, projects CRUD, ticks, events, evaluate)
- **MCP server** — 14 fleet_* tools at `/mcp` endpoint (status, projects, weight, priority, pause/resume, ticks, evaluate)
- **Dark theme dashboard** — HTML at `/` showing fleet status, project cards, tick history
- **Hermes plugin** — `/fleet` slash commands (status, weight, priority, pause, resume, ticks, evaluate)

### Configuration & Infrastructure

- **TOML fleet config** — `--config fleet.toml` for declarative project/namespace seeding
- **Cron migration tool** — `cmd/migrate/` imports Hermes cron jobs.json into SQLite
- **Multi-namespace DuckBrain** — separate namespaces per project with read-replica sync
- **Systemd deployment** — user units with MemoryMax, Restart=always, journal logging
- **Built-in verification** — `--test-verify N` with temp DB, 7-project fleet, 6 invariant checks

### Quality & Reliability

- **Goroutine leak fix** — context-cancellable stdout scanner, explicit pipe closure, tick timeout
- **Memory optimization** — per-chat MCP reduction (500MB → 175MB), MemoryMax=32G for 8 concurrent
- **pprof debugging** — net/http/pprof endpoint for production diagnostics
- **Alert escalation** — configurable thresholds with event emission
- **SQLite schema migrations** — versioned with automatic upgrade path
- **Built-in simulation** — `SimSpawner` for testing without real subprocesses

### Developer Experience

- **Makefile** — build, test, test-full, lint, fmt, migrate, deploy
- **Go 1.26** — latest stable toolchain
- **Vulnerability scanning** — govulncheck integration
- **Conventional commits** — feat/fix/docs/chore with co-author template
