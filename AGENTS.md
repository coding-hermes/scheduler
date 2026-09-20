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
| `/api/v1/namespaces/{id}` | Namespace detail (GET) / partial update (PUT) / delete (DELETE — `?confirm=true` soft-deletes: enabled=false, member projects unassigned, row retained; `?confirm=true&purge=true` hard-deletes the row permanently, SCHED-GAP-097) |
| `/api/v1/ticks` | List ticks |
| `/api/v1/events` | List event log (GET) |
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

- **Eval-stall watchdog (GAP-042).** The evaluation loop is event-driven (startup + slot-freed debounce only) — when the fleet is fully idle (all projects in cooldown, 0 running ticks) nothing re-triggers evaluation, so cooldown-expired projects can sit unscheduled for hours (observed 66-min silent gap 2026-08-13, recovered only by manual POST /api/v1/evaluate). The 30s health ticker now checks `lastEval` age: past 10× min-interval (5 min) with 0 running ticks it forces a re-evaluation and emits a `loop` event ("eval loop stalled — forced re-evaluation"), re-emitted every 30 min while the stall persists. Grep-able `EVAL-STALL:` log line in scheduler.log. **Severity demotion (SCHED-GAP-061):** on a healthy idle fleet the condition legitimately recurs every 5 min (the forced eval runs, finds nothing, lastEval ages again) — 37 self-recovered HIGH events over 08-19..08-21 desensitized operators to real wedges. A detection whose previous forced eval was consumed by the loop (evaluate() refreshed `lastEval` after the force, within the 30-min episode window) is a one-cycle recovery and logs MEDIUM ("... (recovered)"); first onset and non-recovery (lastEval frozen — the loop never consumed the force) stay HIGH. Both severities share the 30-min re-emit throttle. The events table has no WARN level (CHECK constraint CRITICAL/HIGH/MEDIUM/LOW/INFO), so MEDIUM carries the demoted tier.
- **Zero-select monitoring (GAP-043).** Evaluations log nothing on an empty pick, so an operator cannot tell "evaluating" from "evaluating nothing" (observed 2026-08-13 20:55-21:08Z: evals every ~5 min, last `EVAL: N selected` line 15:55:06). `evaluate()` now counts consecutive zero-select evals with eligible projects present (enabled, not running, cooldown elapsed); at 2 consecutive it logs a distinct `EVAL-ZERO-SELECT:` line and emits a HIGH `loop` event, re-emitted at most once per 30 min while the condition persists. Diagnostics (`zero_select_consecutive` / `zero_select_eligible` / `zero_select_last_at`) are exposed in `/api/v1/status`. A zero select with no eligible projects is normal fleet-idle and resets the counter.
- **Disable provenance (GAP-044).** Disabled projects previously carried no record of how/when/why they were disabled (ch-delta: enabled=false, no reason, no event). Every disable path now stamps `disabled_at`/`disabled_by`/`disabled_reason` on the projects row — API pause (`api-pause`), PUT enabled=false (`api`), DELETE confirm=true (`api-delete`, with COALESCE legacy backfill for pre-migration rows), and the failure-rate auto-disable (`auto-disable` with failure stats as the reason) — and the API paths mirror a matching entry into the events table. A false→true transition (resume) clears all three. Fields are exposed in `/api/v1/projects` and the dashboard.
- **No timeout backoff.** Timeout means try again at normal cooldown — do not escalate.
- **Configurable auto-disable (SCHED-GAP-018, default off).** When `--auto-disable-failure-rate` > 0 (e.g. 0.95 = 95%), a project whose recent failure rate (failed+timeout over the last `--auto-disable-window` ticks, default 100) meets or exceeds the threshold AND has at least `--auto-disable-min-ticks` (default 50) ticks in the window is automatically disabled. A HIGH event is emitted to the events table on disable. Operators must explicitly opt in; the default (0) leaves the feature off. The same flags are available as env vars (`SCHEDULER_AUTO_DISABLE_FAILURE_RATE`, `SCHEDULER_AUTO_DISABLE_WINDOW`, `SCHEDULER_AUTO_DISABLE_MIN_TICKS`, `SCHEDULER_FAILURE_WINDOW`). The TOML `[scheduler]` keys (`auto_disable_failure_rate`, `auto_disable_window`, `auto_disable_min_ticks`) are documented in the `--schema` output but are NOT loaded from a root `schedulerd.toml` yet — root TOML wiring arrives in FEAT-005 (DOGFOOD-012). The existing 10+ consecutive-timeout/24h safety net remains.
- **Per-project failure-rate visibility (SCHED-GAP-018).** `GET /api/v1/status` now includes `projects_failure_rates` (per-project breakdown over the last `--failure-window` ticks) and `failure_window` fields, so per-project failure rates stay observable without manual SQL.
- **Cooldown authority model (SCHED-GAP-025).** `fleet-cooldown-policy.py` (ops script at `~/.hermes/scripts/fleet-cooldown-policy.py`) is the ONLY writer of `fleet.toml`; it reads live SQLite state first, honors the `ELEVATED_PINS` whitelist (h3=21600, warpfs=43200 — never written below the pin), and regenerates `fleet.toml` so restarts re-pin existing projects (loader.go, not create-only). An API `PUT /api/v1/projects/{name}` cooldown change is durable only within the daemon session — the next policy run normalizes it back unless the project is in `ELEVATED_PINS`. To pin a project permanently: add it to `ELEVATED_PINS` in the policy script + set the pin in `fleet.toml`, then run the script with `--apply`. Operator pins must exist in BOTH stores — the live SQLite row and the `fleet.toml` block: a pin present only in the DB is drift, not policy, and the next restart re-pins from the toml (SCHED-GAP-121: Bane's 2026-09-15 dagger speed ruling — 900s / 15 min on hermes-dagger — lived only in the DB while `OPERATOR_7200` kept regenerating 7200 into `fleet.toml`; it is durable now because `ELEVATED_PINS` carries `hermes-dagger: 900` and the regen emits 900/900/7200). Tripwire: `python3 ~/.hermes/scripts/fleet-cooldown-policy.py --verify` — exit 0 = every operator pin agrees across both stores; exit 1 with `MISMATCH <project> <field>: db=<v> toml=<v>` lines = drift.
- **Gateway key never in argv (GAP-038).** The daemon's `--gateway-key` flag defaults to `$API_SERVER_KEY` (main.go), which the systemd unit supplies via `EnvironmentFile` pointing at a 0600 env file (`/etc/coding-hermes/gateway.env`, template `deploy/gateway.env.example`). Do NOT add `--gateway-key` to `ExecStart` — argv is world-readable via `ps aux`.
- **Local layout (GAP-039/040).** The canonical checkout on the fleet host is `/home/kara/coding-hermes-scheduler/coding-herms-scheduler/` (typo'd double nesting); the outer `/home/kara/coding-hermes-scheduler/` dir is NOT the repo — it holds only the `.coding-hermes/tasks.md` pointer. Build/run from the inner checkout.
- **Foremen never use delegate_task.** Workers are spawned via `hermes chat -q` with independent model/provider selection.
- **Budget authority chain (ADV-R09/G8).** Every budget figure on every surface comes from ONE source: the `Loop` (`WeightBudget()`), built in `NewLoop` from the resolved config chain — TOML `[scheduler] weight_budget` < `SCHEDULER_BUDGET` env < `--budget` flag (highest wins; provenance detected via `flag.Visit` + the env/TOML default-guard blocks in main.go and surfaced as `budget_source`: "toml"/"env"/"flag"/"flag-default"). Unset behavior is documented and identical across surfaces: 100 weight units, labeled `flag-default`. `/api/v1/status` (`budget_total`, `budget_source`), `/api/v1/config` (`weight_budget`, `budget_source`), MCP `fleet_status` (`budget`), and the dashboard (`SetWeightBudget`) all read the loop — a hardcoded budget number anywhere is a G8 regression (test-pinned: `TestADVR09_StatusBudgetFlowsFromLoop` fails the moment the literal returns). Weight budget is scheduling ADMISSION currency, not money; USD caps are the per-project `daily/weekly/final_budget_usd` fields (SCHED-GAP-066). Money surfaces follow the same doctrine: `ticks.cost_source` (migration v29) stamps every row `measured`/`gateway`/`estimated`/`simulated` (legacy rows read as `legacy`, never rewritten); `/api/v1/status`'s `spend` block splits spend by tier with token sums/averages and states the price vintage (`price_as_of` + `price_source`). The builtin sticker map carries a code-level as-of (`modelRatesAsOf`, 2026-08); unknown models bill at the DOCUMENTED `unknownModelFallbackRate` policy ($2/$8 per 1M); stickers refresh without a rebuild via `--model-rates-file`/`SCHEDULER_MODEL_RATES_FILE` (`scheduler.ApplyModelRatesFile`, per-key JSON override — a bad file is a fatal boot error, never a silent stale fallback). The token estimate for telemetry-less ticks was recalibrated from measured fleet actuals (435K in / 3.5K out, Sep-2026 window; the prior 8000/2000 literal was 9.3x–111x low) and is the ONLY estimate tier — measured ticks record their real token totals from the same Hermes state.db rows the USD came from (previously they wrote 0/0 tokens).

- **Adaptive-ceiling single authority (ADV-R10).** The default adaptive-cooldown ceiling is DERIVED, never hardcoded: when no explicit `cooldown_ceiling_s` is set (config resolution — TOML loader, API enable transition, and the runtime zero-column fallback all agree), the cap is `8 × cooldown_floor_s` (`database.AdaptiveCeilingFloorMultiplier`, via `database.DefaultAdaptiveCooldownCeiling(floor)`). This matches the fleet.toml pin shape (`cooldown_ceiling_s = 8 × cooldown_floor_s`), so a project whose fleet entry loses its explicit ceiling derives the same cap the pins carry instead of parking at the retired weekly (7d) constant — at a 43200s floor that was 604800 against an intended 345600 (1.75× the cap, ~2 days of extra latency per escalation step). An explicit ceiling always wins, verbatim. There is NO second default ceiling constant in circulation; `TestADVR10_NoSecondHardcodedWeeklyCeiling` fails the build if a hardcoded weekly value returns to any non-test source under `internal/` or `cmd/`. Escalator interaction (`adaptiveCooldownFactor = 2`): once the no-progress streak reaches `no_progress_threshold` (default 10), each further no-progress tick doubles `cooldown_s`, so a project reaches the derived cap in exactly three escalating ticks (8 × floor = 2³ × floor) and stays there — still on normal cooldown mechanics, never abandoned; ANY progress (code commit or net open-board-row decrease) resets the streak and snaps `cooldown_s` back to the floor immediately. A floorless row (floor 0 AND ceiling 0 — hand-edited SQL relic; the enable path always writes a floor) derives ceiling 0: the streak is tracked for observability but `cooldown_s` never escalates.

- **Board-driven wake + ordering-only boost (ADV-R07, Option C).** A watcher (`internal/scheduler/board_wake.go`) polls enabled projects' board files (tasks.jsonl/tasks.md mtime, 60s cadence). A detected write arms a per-project wake that fires ~5 min later (bounded debounce — later writes never extend an armed wake; the bound covers the observed 4.4-min median implementation→board flip lag), reads the board through the R06 git-verified freshness reader, emits a `board_wake` event (row id + freshness verdict per row), and calls `ForceEvaluate`. The pending count feeding the existing `pendingBoostUrgencyFor` tier (board_awareness.go) is now freshness-checked: a pending row whose work verifiably landed in git (verdict flip-window/flip-overdue) stops counting. Ordering only — wall-clock cooldown remains the SOLE admission authority; no code path skips a spawn solely on board state. Every watcher/reader error fails open (fleet behavior identical to pre-R07; clock cadence unchanged), and a heartbeat watchdog watches the watcher (HIGH `board_wake` stall event on a silent poll loop, 30-min re-emit throttle). Any future admission gate MUST live in `SlotPool.spawn` (slot_pool.go) with a declared override — the G7 ruling; adding one anywhere else (evaluate, packers, watcher) is a design violation.

- **Tier ladder (G2 ruling).** The four urgency tiers are documented policy, not an accident (G2 ruling: tiers stay, continuous scoring rejected). Tiers 2x apart are stable — no coefficient edit can silently reorder the fleet, and continuous scoring creates exactly that silent-global-reorder hazard (A5 2 G2; A1 5 confirms the code matches the corrected C12 reading).

  | Tier | Base urgency | Within-tier ordering | Cap | Source |
  |------|-------------|----------------------|-----|--------|
  | Organic | `priority * (1 + elapsed/interval)^decay` | within-tier: priority desc, then by score | ~12k live | `internal/scheduler/urgency.go:63` |
  | Pending-boost (board) | `5e11` | + `min(pending, pendingBoostMaxCount)` | `< 1e12` (preserves starvation guarantee) | `internal/scheduler/board_awareness.go:97` |
  | Bump (actively bumped) | `5e11 + 1e6` | bumped project wins ties against plain pending-boost | n/a (per-bump) | `internal/scheduler/board_awareness.go:109` |
  | Starvation (fairness) | `1e12` | + `elapsed.Seconds()` (most-starved first) | `1e12 + ~1e6s` (still >10^7× organic) | `internal/scheduler/fairness.go:42` |

  Across tiers the base ladder dominates: any pending-boost project outranks any organic one, any starvation-boost outranks any pending-boost, etc. Within a tier, projects sort by their computed score. `MaxNudgesPerTick = 2` (`internal/scheduler/session_resume.go:36`) caps continuation re-spawns per tick row — a safety bound, NOT a tier knob; changing it does not reorder anything.

## Project Conventions

- Go doc comments on all public functions
- Sequential test runs (`-p 1`) due to cgroup pids limits
- Co-author via `CODING_HERMES_CO_AUTHOR` env var
- GitReins guards enforce secrets, build, lint, and tests before commit
