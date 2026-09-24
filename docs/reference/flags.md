# Scheduler flags

Defaults match `cmd/schedulerd/main.go` — the canonical source. This is the agent-facing copy of the flag table; README.md carries the operator-facing copy.

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
| `--api-read-timeout` | `5s` | Per-request deadline for the heavy read API surfaces (`/api/v1/status`, `/projects`, `/namespaces`, `/ticks`); a stalled DB helper returns 504 naming the helper instead of hanging the handler (SCHED-GAP-1575-B; `<= 0` = keep the 5s default). Layers: `--api-read-timeout` > `SCHEDULER_API_READ_TIMEOUT` > `[api] read_timeout` > default |
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
| `--reap-sessions` | `false` | Run one SCHED-GAP-089 reap pass against the agent state store (`~/.hermes/state.db`) and exit — DRY-RUN by default, writes nothing |
| `--reap-sessions-apply` | `false` | With `--reap-sessions`: APPLY the pass — close selected sessions (`ended_at` + `end_reason='reaped'`). Without it the pass is a dry-run (SCHED-GAP-089) |
| `--session-db` | `~/.hermes/state.db` | Agent state database path for `--reap-sessions` (SCHED-GAP-089) |
| `--session-reap-threshold` | `24h0m0s` | Stale api_server session reap threshold (SCHED-GAP-089; default 24h) |

## Canonical invocation

```
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

## DuckBrain sync auth

When `DUCKBRAIN_API_KEY` is set, every sync request carries it as the `X-API-Key` header (`internal/sync/duckbrain.go`), and the daemon validates the key once at startup with a side-effect-free probe — a rejected key (HTTP 401/403) fails fast with a distinct HIGH `DuckBrain API key REJECTED` event and gates sync cycles off instead of spooling every failed write. A 429 is treated as backpressure, not an error: the current burst stops, remaining writes spool and replay on the next `--duckbrain-interval` tick. With the env var unset or empty the daemon stays in pre-auth compatibility mode — no probe and no header (deliberate, not a bug).
