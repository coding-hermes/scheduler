# coding-hermes-scheduler

[![Status](https://img.shields.io/badge/status-WIP-yellow)]()

Weight-budget priority scheduler that replaces 33 static foreman cron jobs with a single
daemon that packs work into a configurable daily budget and spawns foremen on
demand.

## Architecture

```
                    ┌─────────────────────────────────────┐
                    │           schedulerd (binary)        │
                    │                                     │
  task sources ───▶ │  internal/sync  ──▶  internal/db    │
  (boards, git)     │                        │            │
                    │                        ▼            │
                    │            internal/scheduler       │
                    │     ┌──────────────────────────┐     │
                    │     │ urgency calculator       │     │
                    │     │ greedy weight packer     │     │
                    │     │ foreman spawn engine     │     │
                    │     │ tick lifecycle           │     │
                    │     └──────────┬───────────────┘     │
                    │                │                     │
                    │      spawns: hermes chat -q ...      │
                    │                │                     │
                    │    ┌───────────┼───────────┐         │
                    │    ▼           ▼           ▼         │
                    │ internal/api  internal/mcp  internal/dashboard │
                    │  (REST API)   (MCP server)  (HTML UI)          │
                    └───────────────┬─────────────────────┘
                                    │
                           localhost:9090
```

### Components

| Package                  | Responsibility                                      |
| ------------------------ | --------------------------------------------------- |
| `cmd/schedulerd`         | Binary entry point; flag parsing, wiring            |
| `internal/scheduler`     | Urgency calc, greedy packer, foreman spawn, ticks   |
| `internal/database`      | SQLite operational store (projects, ticks, events)  |
| `internal/api`           | HTTP REST API for fleet management and control      |
| `internal/mcp`           | MCP server exposing scheduler ops to AI agents      |
| `internal/dashboard`     | HTML dashboard for visualization                    |
| `internal/sync`          | DuckBrain read-replica sync                         |

## Build

```sh
make build       # compile to bin/schedulerd
make test        # run short tests
make test-full   # run all tests
make run         # build then run
make install     # go install ./...
make lint        # go vet ./...
make fmt         # gofmt -w .
make clean       # rm -rf bin/
```

## Configuration

All configuration is via command-line flags:

| Flag         | Default                                   | Description                         |
| ------------ | ----------------------------------------- | ----------------------------------- |
| `--port`     | `9090`                                    | HTTP listen port                    |
| `--socket`   | _(none)_                                  | Unix socket path (overrides port)   |
| `--db-path`  | `~/.coding-hermes-scheduler/scheduler.db` | SQLite database path                |
| `--budget`   | `100`                                     | Daily weight budget                 |

## Deployment

### systemd

```sh
# Copy binary to install location
sudo cp bin/schedulerd /home/kara/bin/schedulerd

# Install systemd unit
sudo cp deploy/coding-hermes-scheduler.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable coding-hermes-scheduler
sudo systemctl start coding-hermes-scheduler

# Check status
sudo systemctl status coding-hermes-scheduler
journalctl -u coding-hermes-scheduler -f
```

### Hermes Plugin Setup

1. Install the plugin: `hermes plugins install coding-hermes`
2. Configure MCP server in `~/.hermes/config.yaml`:
   ```yaml
   mcp_servers:
     coding-hermes:
       url: http://localhost:9090/mcp
       transport: streamable-http
   ```
3. The scheduler's MCP tools will auto-register on next Hermes restart
4. Use `/fleet status`, `/fleet weight`, `/fleet budget` slash commands

### Trigger Cron

Replace 33 static foreman crons with a single trigger:
```json
{
  "name": "coding-hermes — scheduler trigger",
  "state": "scheduled",
  "enabled": true,
  "schedule": {"kind": "cron", "display": "* * * * *", "expr": "* * * * *"},
  "skills": [],
  "no_agent": true,
  "script": "trigger-scheduler.py",
  "model": null,
  "provider": null
}
```

## Quick Start

```sh
# Build
make build

# Run (foreground, for testing)
./bin/schedulerd --port 9090 --budget 100

# Run with custom DB path
./bin/schedulerd --db-path /tmp/scheduler.db

# Health check
curl http://localhost:9090/api/v1/health
```
