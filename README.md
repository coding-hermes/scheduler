# Coding Hermes Fleet Scheduler

**A weight-budget knapsack scheduler for autonomous AI coding fleets.**

The scheduler is a Go daemon that decides **which projects run each tick** based on priority, resource consumption, and utilization efficiency. It replaces static cron scheduling with a dynamic two-axis model — weight (how much concurrency budget a project consumes) and priority (how aggressively the scheduler attempts to run it).

```
┌──────────────────────────────────────────────────┐
│               SCHEDULER DAEMON                     │
│                                                    │
│  ┌──────────┐  ┌──────────┐  ┌──────────────────┐ │
│  │ URGENCY  │  │  PACKER  │  │    SPAWNER       │ │
│  │ compute  │──│ greedy   │──│ launch foremen   │ │
│  │ scores   │  │ knapsack │  │ hermes chat -q   │ │
│  └──────────┘  └──────────┘  └──────────────────┘ │
│                                                    │
│  ┌──────────┐  ┌──────────┐  ┌──────────────────┐ │
│  │  HTTP    │  │   MCP    │  │   DASHBOARD      │ │
│  │ REST API │  │ protocol │  │   HTML (/)       │ │
│  └──────────┘  └──────────┘  └──────────────────┘ │
└──────────────────────────────────────────────────┘
```

## Two-Axis Scheduling

Weight and priority are **independent**. A project can be high-priority AND high-weight (runs often, hogs resources), or low-priority AND low-weight (rarely scheduled but always finds a slot because it costs almost nothing).

| Axis | Range | Purpose |
|------|-------|---------|
| **Weight** | 1–100 | How much of the concurrency budget this project consumes per tick |
| **Priority** | 1–10 | How aggressively the scheduler attempts to run it |

**The scheduler packs projects greedily by urgency into a fixed weight budget (default 100).** Urgency is computed as:

```
urgency = priority × (1 + time_since_last_run / base_interval) ^ decay_rate
```

This guarantees starvation is impossible — even a low-priority project eventually accumulates enough urgency to run.

### Geometric Interval Mapping

Priority maps to tick interval via an exponential curve:

```
interval = min × (max/min) ^ ((priority - 1) / (N - 1))
```

High priorities (1-3) spread out meaningfully — each step is a real difference. Low priorities cluster near max — all roughly the same very long interval. The range is runtime-configurable.

## Quick Start

```bash
# Build
make build

# Initialize database and migrate projects
./bin/migrate -jobs /path/to/your/jobs.json

# Run
./bin/schedulerd -db ~/.hermes/coding-hermes/scheduler.db

# Or via systemd
sudo make deploy-install
sudo systemctl start coding-hermes-scheduler
```

### Flags

| Flag | Default | Purpose |
|------|---------|---------|
| `-db` | `$HOME/.hermes/scheduler.db` | SQLite database path |
| `-listen` | `127.0.0.1:9090` | HTTP listen address |
| `-min-interval` | `20m` | Fastest tick interval (priority 1) |
| `-max-interval` | `24h` | Slowest tick interval (priority N) |
| `-num-levels` | `10` | Number of priority levels |
| `-budget` | `100` | Total weight budget |
| `-max-concurrent` | `8` | Max concurrent foremen |
| `-duckbrain-ns` | `coding-hermes` | DuckBrain namespace for sync |

## Architecture

### Components

| Package | Purpose |
|---------|---------|
| `cmd/schedulerd` | Main binary — wires all components, starts HTTP server + eval loop |
| `cmd/migrate` | Bootstrap tool — imports projects from your Hermes cron jobs.json |
| `internal/database` | SQLite schema, migrations, CRUD for projects/ticks/events |
| `internal/scheduler` | Urgency calculator, greedy knapsack packer, process spawner, tick lifecycle |
| `internal/api` | REST API — `/api/v1/health`, `/api/v1/projects`, `/api/v1/ticks`, `/api/v1/evaluate` |
| `internal/mcp` | MCP protocol server at `/mcp` — fleet management tools |
| `internal/dashboard` | Self-contained dark-themed HTML dashboard at `/` |
| `internal/sync` | DuckBrain read-replica sync |

### Database

- **SQLite** (WAL mode) is the authoritative operational store — project registry, tick queue, tick history, concurrency pool, event log.
- **DuckBrain** (optional) serves as a read replica synced every 5 minutes — compact status blobs for cross-session visibility and git-versioned audit trail.
- Single file, single directory (`~/.hermes/coding-hermes/`). Backup: `cp scheduler.db scheduler.db.bak`.

### Evaluation Loop (every 60 seconds)

1. **Cleanup** — mark stale running ticks (90+ min) as failed
2. **Compute urgency** — for every enabled project, calculate `urgency = priority × (1 + elapsed/interval)^decay`
3. **Sort** by urgency descending
4. **Pack greedily** — fit projects into the weight budget, respecting concurrency cap and per-project cooldown
5. **Spawn** — launch foremen via `hermes chat -q` for each selected project
6. **Track** — monitor completion, record outcomes

## API

### REST Endpoints

| Method | Path | Purpose |
|--------|------|---------|
| `GET` | `/api/v1/health` | Server health + active ticks |
| `GET` | `/api/v1/status` | Fleet-wide status with all projects |
| `GET` | `/api/v1/projects` | List all projects |
| `POST` | `/api/v1/projects` | Create a project |
| `GET` | `/api/v1/projects/{name}` | Get project by name |
| `PUT` | `/api/v1/projects/{name}` | Update project (weight, priority, etc.) |
| `DELETE` | `/api/v1/projects/{name}` | Delete project |
| `GET` | `/api/v1/ticks` | List recent ticks |
| `GET` | `/api/v1/ticks/{id}` | Get tick by ID |
| `POST` | `/api/v1/evaluate` | Force immediate evaluation |
| `POST` | `/api/v1/pause` | Pause the evaluation loop |
| `POST` | `/api/v1/resume` | Resume the evaluation loop |
| `GET` | `/api/v1/events` | List events |

### MCP Tools (at `/mcp`)

| Tool | Purpose |
|------|---------|
| `fleet_status` | Live view of all projects with urgency, weight, last tick |
| `fleet_set_priority` | Set project priority (1-N) |
| `fleet_set_weight` | Set project weight (1-100) |
| `fleet_set_budget` | Adjust total weight budget |
| `fleet_set_cooldown` | Set minimum gap between ticks |
| `fleet_pause` / `fleet_resume` | Pause/resume a project |
| `fleet_add_project` / `fleet_remove_project` | Add/remove projects from the pool |
| `fleet_force_evaluate` | Trigger immediate evaluation |
| `fleet_list_ticks` | Query tick history |
| `fleet_list_events` | Query event log |
| `fleet_set_range` | Adjust geometric interval range at runtime |

## Model

A scheduler instance manages **projects**. Each project has a name, weight, priority, decay rate, cooldown, workdir, and repo URL. The scheduler spawns foremen — per-project orchestrators that scan task boards, compile worker prompts, and verify quality.

The scheduler does NOT contain foreman logic. It decides **when** to run — foremen decide **what** to do.

## Build

```bash
make build        # Build both binaries
make test         # Run tests (short)
make test-full    # Run all tests
make lint         # go vet
make fmt          # gofmt
make migrate      # Import projects from jobs.json
```

## Deploy

```bash
make deploy-install   # Install systemd unit
make deploy           # Build + install + restart
```

## License

MIT
