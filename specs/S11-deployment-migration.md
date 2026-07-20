# S11 — Deployment & Migration

**Status:** Draft  
**Depends on:** S01, S06  
**Pages target:** 2-3

---

## 1. Overview

The scheduler daemon (`schedulerd`) runs as a long-lived process on the Hermes host. It manages the coding-hermes fleet by evaluating projects, packing them into weight-budgeted slots, and spawning foremen via the Hermes Gateway.

## 2. Runtime Model

### 2.1 Current: Bash Wrapper

The daemon runs via a bash wrapper script (not systemd — see FIX-STUCK below):

```bash
#!/bin/bash
# Wrapper starts schedulerd with fleet config
./bin/schedulerd \
  --db ~/.hermes/coding-hermes/scheduler.db \
  --max-concurrent 4 \
  --min-interval 5m \
  --tick-timeout 7200s \
  --gateway-url http://127.0.0.1:8642 \
  --gateway-key "$HERMES_GATEWAY_KEY" \
  --no-exec-fallback
```

### 2.2 Flags

| Flag | Default | Description |
|---|---|---|
| `--db` | (required) | SQLite database path |
| `--max-concurrent` | 4 | Max concurrent foreman spawns |
| `--min-interval` | 5m | Minimum interval between ticks |
| `--tick-timeout` | 7200s | Foreman tick timeout (2h) |
| `--gateway-url` | `http://127.0.0.1:8642` | Hermes Gateway URL |
| `--gateway-key` | (required) | Gateway auth key |
| `--no-exec-fallback` | false | Disable exec-based spawn fallback |
| `--namespace-mode` | false | Enable multi-namespace weight packing |

## 3. Configuration

### 3.1 TOML Config (`config.toml` or `fleet.toml`)

```toml
[daemon]
bind = "127.0.0.1:9090"
# unix_socket = "/run/coding-hermes/scheduler.sock"  # alternative

[gateway]
url = "http://127.0.0.1:8642"
key = "${HERMES_GATEWAY_KEY}"  # env var interpolation

[scheduler]
max_concurrent = 4
min_interval = "5m"
tick_timeout = "2h"
namespace_mode = true
no_exec_fallback = true
```

## 4. Database

- **Engine:** SQLite via `modernc.org/sqlite` (pure Go, no CGO)
- **Location:** `~/.hermes/coding-hermes/scheduler.db`
- **Migrations:** Auto-applied on startup via `database.Migrate()`
- **WAL mode:** Enabled for concurrent reads during writes
- **Backup:** `sqlite3 scheduler.db ".backup scheduler-backup.db"`

## 5. Migration Path

### 5.1 From Static Crons to Scheduler

The scheduler replaces 33 static Hermes cron jobs:
1. **Old model:** Each project has its own cron → 33 individual crons firing independently
2. **New model:** One trigger cron (60s) → scheduler evaluates all projects → packs into slots → spawns to gateway

### 5.2 Fleet Config Migration

Project configs migrated from individual cron entries to `fleet.toml`:
- `schedule` → `urgency_config` (cooldown, weight, priority)
- `cron_command` → `spawn_command` (hermes chat invocation)
- Static `skills` → configurable per-project in fleet.toml

## 6. FIX-STUCK (Blocked)

Systemd enable is deferred by Bane. The current bash wrapper works but lacks:
- Automatic restart on crash
- Log rotation via journald
- Resource limits (MemoryMax, CPUQuota)
- Socket activation

When unblocked, the systemd unit will follow the pattern:
```ini
[Service]
ExecStart=/usr/local/bin/schedulerd --config /etc/coding-hermes/fleet.toml
Restart=always
RestartSec=5s
MemoryMax=512M
```
