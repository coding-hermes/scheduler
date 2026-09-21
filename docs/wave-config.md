# Wave configuration — operator reference (S12)

Operator-facing reference for the namespace wave keys: `wave_enabled`,
`wave_tick_timeout`, `wave_workers_cap`. Written for ADV-R14 (G9 residue);
covers the config surface landed by SCHED-GAP-109/113 plus the load-scaled
budget from SCHED-GAP-170. The rollout status and the deferred
slot-equivalent decision live in [docs/s12-wave-rollout.md](s12-wave-rollout.md).

## What a wave is

A serial tick runs ONE worker. A wave tick composes N workers that run in
parallel, each on a mutually independent board task (no `depends_on` edges,
disjoint file sets) in its own git worktree. Independence is a
composition-layer doctrine carried by the foreman skill and the namespace
prompt — the scheduler never inspects row dependencies. Scheduler-side
records: `ticks.worker_count` and the `tick_workers` rows ingested from the
foreman's wave manifest (SCHED-GAP-110, attribution-only), per-worker cost
attribution (SCHED-GAP-115), and the reaper's recover-before-dispatch
preamble for interrupted waves (SCHED-GAP-114).

## The three namespace keys

All three live on the `namespaces` table (migration v27,
`internal/database/migrations.go`) and on the TOML `[[namespaces]]` blocks
(`internal/config/config.go`, `NamespaceDef`). All default OFF: with
`wave_enabled = false` namespace behavior is byte-identical to pre-S12
scheduling.

| Key | Type / default | Meaning |
|-----|----------------|---------|
| `wave_enabled` | bool, default `false` | Master switch. `false` → no `WAVE_BUDGET` line is ever injected; the foreman runs its usual serial tick. |
| `wave_tick_timeout` | duration string, default `""` | Max duration of a wave tick. `""` = inherit the scheduler `--tick-timeout`. Parsed and validated at config load; a bad value is a load error naming `namespaces[<id>].wave_tick_timeout`. Hard upper bound: 4h (`config.WaveTickTimeoutCeiling`, `internal/config/loader.go`). |
| `wave_workers_cap` | int, default `0` | Max concurrent wave workers across the namespace's RUNNING ticks. `0` = unlimited: no `WAVE_BUDGET` line is injected and the foreman prompt alone governs wave width. Negative values normalize to `0` at load. |

## Advisory vs enforcement (Option C sub-ruling, G9 / ADV-R14)

`wave_workers_cap` is **advisory at the composition layer, forever**. The
scheduler never blocks or reorders selection because of it. It does exactly
two things with the cap:

1. **Injects the budget line** into the foreman prompt — semantics defined
   at `internal/scheduler/wave_budget.go:108`.
2. **Sheds to serial at the tick boundary** — when the namespace has
   `wave_workers_cap > 0` AND a live wave in flight (a running tick with
   `worker_count > 0`), newly selected projects in that namespace run
   SERIAL (`WAVE_BUDGET: 0` at spawn). This is a coarse shed, not a block:
   the packer may still select them (`internal/scheduler/wave_budget.go`
   + the `waveShedSet` path in `internal/scheduler/packer_select.go`).

Enforcement of actual worker concurrency therefore lives at the
tick-boundary shed — NOT in the cap value itself. Any future
capacity-accounting scheme (e.g. counting a 3-worker wave as 1 slot) is a
policy change that is explicitly DEFERRED; see
[docs/s12-wave-rollout.md](s12-wave-rollout.md) for the trigger conditions.

## Budget arithmetic (what the foreman actually receives)

The injected line is exactly:

```
WAVE_BUDGET: <n> — max concurrent wave workers this tick (0 = serial tick, do not compose a wave).
```

with `n = min(loadCap, wave_workers_cap − live_depth)` clamped at 0, where
`live_depth` is `SUM(worker_count)` over the namespace's RUNNING ticks.
`n = 0` means "run a serial tick this tick". The line is absent — and the
prompt byte-identical to the pre-113 builder — for projects with no
namespace or `wave_workers_cap = 0`. It always lands on its own line at
the END of the prompt, so a `prompt_mode = "replace"` project prompt
cannot lose it.

Load dimension (SCHED-GAP-170): when the load gate is armed
(`--load-gate-threshold` > 0, wired via `scheduler.SetWaveLoadCeiling` in
`cmd/schedulerd/main.go`), `loadCap = min(wave_workers_cap, headroom)`
with `headroom = threshold − current_1m_loadavg` — one worker per spare
load point, floored at 1 ("min 1": a serial tick is always allowed). A
missing loadavg reading fails open (depth arithmetic only); with the gate
disabled (threshold 0) the load dimension is skipped entirely. At load ≥
threshold the global load gate itself defers the whole spawn
(SCHED-GAP-125) before the budget matters.

Worked example on a namespace with `wave_workers_cap = 3` and a live depth
of 1, with the gate armed at 12 and load 8: headroom 4 → `loadCap =
min(3, 4) = 3` → budget `min(3, 3 − 1) = 2` → `WAVE_BUDGET: 2`.

## Enabling waves — TOML (applies at namespace CREATION only)

```toml
[[namespaces]]
id = "example-lane"
weight = 10
max_concurrent = 2
enabled = true
admission_mode = "tasks"
wave_enabled = true
wave_tick_timeout = "3h"
wave_workers_cap = 3
```

DURABILITY WARNING: the TOML namespace import is create-only — an existing
DB namespace is never re-pinned from `fleet.toml` for the wave fields (the
loader's existing-namespace path pins only `max_concurrent`,
`default_prompt`, `admission_mode`, `load_gate`). A namespace block that
omits the wave keys recreates the row with waves DISABLED. Consequences:

- To make wave config durable, put the wave keys in the `[[namespaces]]`
  block BEFORE the row is ever recreated, AND set them on the live row via
  the API (below) — the TOML alone does not update an existing row.
- API writes are session-local and revert on restart unless the TOML block
  also carries the keys (the same two-store rule as cooldown pins).

## Enabling waves — API (live row, session-local)

`PUT /api/v1/namespaces/{id}` accepts the same fields via `NamespacePatch`
(`internal/database/models.go`):

```sh
curl -s -X PUT "http://127.0.0.1:9090/api/v1/namespaces/coding-hermes" \
  -H 'Content-Type: application/json' \
  -d '{"wave_enabled": true, "wave_tick_timeout": "3h", "wave_workers_cap": 3}'
```

Verify what the daemon actually has (do not trust either store alone):

```sh
curl -s http://127.0.0.1:9090/api/v1/namespaces/coding-hermes | jq '{wave_enabled, wave_tick_timeout, wave_workers_cap}'
sqlite3 "file:$HOME/.hermes/coding-hermes/scheduler.db?mode=ro" \
  "SELECT id, wave_enabled, wave_tick_timeout, wave_workers_cap FROM namespaces ORDER BY id;"
```

## Status surfaces and the one known trap

`GET /api/v1/status` carries `wave_workers_cap_configured` — that is the
WAVE worker cap, not the namespace `max_concurrent` cap. Do not read it as
the namespace cap; caps live in the DB and on process argv
(see docs/runbook-drain-restart.md §1.7). Per-tick wave evidence lives in
`ticks.worker_count` and the `tick_workers` rows.
