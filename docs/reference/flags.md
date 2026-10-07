# Scheduler flags

Defaults match `cmd/schedulerd/main.go` — the canonical source. This is the agent-facing copy of the flag table; README.md carries the operator-facing copy.

| Flag | Default | Description |
|------|---------|-------------|
| `--db` | `~/.hermes/coding-hermes/scheduler.db` | SQLite database path |
| `--listen` | `127.0.0.1:9090` | HTTP listen address |
| `--public-url` | (none) | Public base URL of the dashboard (e.g. `https://sched.example.com`) used to build tick-report permalinks for `deliver_mode=link`; empty = link mode falls back to full (SCHED-GAP-1607) |
| `--min-interval` | `30s` | Fastest tick interval |
| `--max-interval` | `24h` | Slowest tick interval |
| `--num-levels` | `10` | Number of priority levels |
| `--budget` | `100` | Weight budget |
| `--max-concurrent` | `10` | Max concurrent foremen |
| `--slot-patience` | `5m0s` | How long a tick waits for a free slot before being dropped; the drop emits an event (ADV-R08/G3) |
| `--tasks-pacing` | `1m0s` | Minimum post-tick spacing before a tasks-mode project re-admits, +up to 20% jitter (SCHED-GAP-136); `0` = disabled. Library default 0; the fleet binary ships 60s. Composes with (never replaces) failure backoff |
| `--load-gate-threshold` | `0` | Defer new spawns while the 1-minute load average is at or above this value (SCHED-GAP-125); `0` = disabled. Work is deferred, not dropped — it runs once load drops. Namespaces opt out via `load_gate='off'` |
| `--feature-prune-weeks` | `8` | Prune window (weeks) for the dead-feature reaper (SCHED-GAP-131): `/api/v1/features/prune-candidates` flags mechanisms whose last proven use is older than this (or never used). Flag only — nothing is auto-deleted |
| `--peer-freshness-window` | `180` | Peer freshness window in seconds for the federation peer registry (REMOTE-003): a peer whose last `POST /api/v1/peers/{id}/heartbeat` is older renders `stale:true` — with its `last_contact`, never a "down" state — on `GET /api/v1/peers`. Env: `SCHEDULER_PEER_FRESHNESS_WINDOW` |
| `--spawn-mem-limit-mb` | `0` | Per-spawn RLIMIT_AS memory cap in MiB applied to spawned foreman processes (ADV-R11, GAP-048 cure); `0` = off (default). NOT an admission gate — every selected project still spawns; the cap constrains the spawned process's resources at spawn time (inherited by its workers). Best-effort: a failed cap WARNs and the spawn continues |
| `--board-stasis-gate` | `true` | SCHED-GAP-1678: exclude a cooldown-mode BUILDER lane from selection while its board file has NOT changed since its previous completed tick — a tick on a static board no-ops ~69% of the time (vs 25% when the board moved) and costs more per tick ($4.68 vs $2.43 sticker), so the spawn is skipped before any LLM call or tick row exists. Fingerprint is mtime+size (no git battery), the check fails OPEN (unreadable/missing board spawns), tasks-mode lanes keep the SCHED-GAP-124 waiver, reporter-class lanes keep their timer cadence, a failed/timeout/deferred previous tick is never gated, no cooldown is consumed, and an operator `ForceEvaluate` bypasses it for one pass. Skips are logged `BOARD-UNCHANGED` and counted as `board_unchanged_skips` on `/api/v1/status`. Env: `SCHEDULER_BOARD_STASIS_GATE`; `false` = pre-gate scheduling |
| `--session-silence-grace` | `0s` | SCHED-GAP-1707: session-silence watchdog — terminate a gateway tick whose Hermes-state session shows no token delta and no tool activity for this long. The row lands `status=timeout` with `failure_reason=session_silent` and the quiet duration on `ticks.session_silence_s`, marked `telemetry_partial=1`; a producing session (any tokens or tools) is never killed, and cooldowns / the no-timeout-backoff chain are untouched. Env: `SCHEDULER_SESSION_SILENCE_GRACE`; `0s` = disabled |
| `--disable-tick-push` | `false` | SCHED-GAP-1594: skip the per-tick git push at tick exit (SCHED-GAP-1694's push-at-exit). Ticks only update the web dashboard (tick rows, `/api/v1/status`, `/health` — the health panel shows the `live` vs `pushed` state) and the fleet-strand-push cron is the only remaining pusher. Default `false` = backward compat (per-tick push stays on). Env: `SCHEDULER_DISABLE_TICK_PUSH` |
| `--namespace-mode` | `false` | Enable multi-namespace scheduling |
| `--tick-timeout` | `2h` | Maximum tick duration before timeout (2h) |
| `--api-read-timeout` | `5s` | Per-request deadline for the heavy read API surfaces (`/api/v1/status`, `/projects`, `/namespaces`, `/ticks`); a stalled DB helper returns 504 naming the helper instead of hanging the handler (SCHED-GAP-1575-B; `<= 0` = keep the 5s default). Layers: `--api-read-timeout` > `SCHEDULER_API_READ_TIMEOUT` > `[api] read_timeout` > default |
| `--test-verify` | `0` | Run N-cycle correctness verification and exit |
| `--verify-board` | (none) | Check board closure-evidence violations (SCHED-GAP-085): exit 0 when no closed row is missing all of reasoning/commit_hash/worker_summary, exit 1 when any |
| `--duckbrain-ns` | `scheduler` | DuckBrain namespace for sync |
| `--duckbrain-url` | `http://localhost:3000` | DuckBrain HTTP server URL |
| `--duckbrain-interval` | `5m0s` | DuckBrain sync interval (spool replay cadence) |
| `--simulate` | `false` | Run in dry-run/simulation mode (no real spawning) |
| `--sim-success` | `0.85` | Simulated success rate (0.0-1.0) |
| `--sim-idle` | `0` | Fraction of completed sim ticks with zero commits (0-1) — exercises adaptive-cooldown slow-down in dry-runs |
| `--sim-count` | `0` | Generate N simulated ticks and exit (0 = run loop) |
| `--gateway-url` | `http://127.0.0.1:8642` | Hermes gateway API URL (empty = use exec.Command) |
| `--gateway-key` | `$API_SERVER_KEY` | Hermes gateway API key |
| `--gateway-response-timeout` | `30m0s` | Per-turn deadline for a gateway /v1/responses POST; a stalled POST fails the tick before `--tick-timeout` (SCHED-GAP-117; 0 disables) |
| `--model-rates-file` | (none) | JSON price-sticker file applied over the builtin model rates at startup (ADV-R09/G8): `{as_of, models:{name:{in_per_m,out_per_m}}, providers:{...}}` — refresh stickers without a rebuild |
| `--no-exec-fallback` | `true` | Disable exec.Command fallback when gateway fails (default true for safety) |
| `--version` | `false` | Print version/build info and exit; version resolves ldflags tag → vcs buildinfo (dev-<shorthash>) → dev; same identity serves /api/v1/health, openapi info.version, MCP serverInfo.version |
| `--foreman-home` | `~/.hermes/foreman` | HERMES_HOME path for foreman sessions |
| `--sim-setup` | `false` | Create test fixture with 13 dry-run projects (12 enabled + 1 disabled) |
| `--sim-ticks` | `10` | Number of evaluation ticks to run in sim-setup mode |
| `--config` | (none) | Path to TOML fleet config file |
| `--log-file` | `~/.hermes/coding-hermes/scheduler.log` | Path to append structured tick logs (JSON lines); empty disables. Default derived from `--db` (SCHED-GAP-1647): the default/production db keeps this path, any other db logs to `<db>.log`; an explicit `--log-file` always wins |
| `--show-config` | `false` | Print resolved config (CLI + env layers) as TOML and exit — the root-TOML layer resolves later in boot and is NOT reflected here |
| `--schema` | `false` | Output JSON Schema for schedulerd.toml and exit |
| `--failure-window` | `100` | Number of recent ticks per project for `/api/v1/status` per-project failure-rate breakdown (SCHED-GAP-018) |
| `--auto-disable-failure-rate` | `0` | Per-project failure-rate threshold (0.0–1.0) for auto-disable; `0` = off (SCHED-GAP-018) |
| `--auto-disable-window` | `100` | Ticks per project over which auto-disable failure rate is computed (SCHED-GAP-018) |
| `--auto-disable-min-ticks` | `50` | Minimum ticks in window before auto-disable can fire (SCHED-GAP-018) |
| `--groups-file` | (none) | JSONL file for deploy groups (default `<db dir>/groups.jsonl` when the blocks store is enabled; empty = default paths) |
| `--templates-file` | (none) | JSONL file for deploy templates (default `<db dir>/templates.jsonl` when the blocks store is enabled; empty = default paths) |
| `--reap-sessions` | `false` | Run one SCHED-GAP-089 reap pass against the agent state store (`~/.hermes/state.db`) and exit — DRY-RUN by default, writes nothing |
| `--reap-sessions-apply` | `false` | With `--reap-sessions`: APPLY the pass — close selected sessions (`ended_at` + `end_reason='reaped'`). Without it the pass is a dry-run (SCHED-GAP-089) |
| `--session-db` | `~/.hermes/state.db` | Agent state database path for `--reap-sessions` (SCHED-GAP-089) |
| `--session-reap-threshold` | `24h0m0s` | Stale api_server session reap threshold (SCHED-GAP-089; default 24h) |

Related: every environment variable the daemon reads is tabulated in [env-vars.md](env-vars.md), including the env-only knobs with no flag (`SCHEDULER_OPERATOR_TOKEN`, `SCHEDULER_WAVE_TICK_TIMEOUT`, the `SCHEDULER_FOREMAN_*` model pins).

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
