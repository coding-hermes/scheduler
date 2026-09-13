# S12 — Concurrent Wave Scheduling

**Status:** Draft
**Depends on:** S01, S02, S05, S06, S07
**Pages target:** 4-6

---

## 1. Overview

The fleet's unit of work changed. One foreman per project now composes **waves**: it
dispatches 2-3 workers in isolated git worktrees (`wt/<taskid>`), each with its own
guard + judge, then merges the branches serially. The foreman skill (v2.10.0,
`## Parallel Ticks — Git Worktrees`) and the worker skill (v1.6.0, `## Worktree Mode`)
codify that contract. **The scheduler does not.**

Today the scheduler's data model asserts `1 tick = 1 foreman process = 1 worker run`:

| Scheduler belief | Reality under waves |
|---|---|
| `ticks` row = one agent session, one cost, one commit count | one tick = 1 foreman session **+ up to 3 worker sessions** |
| `ticks.cost_usd` is "the cost of this tick" | true, but unattributable — no worker split, no count |
| tick wall-clock is bounded by `--tick-timeout` (2h) | 3 workers + 3 serial merges can exceed 2h |
| `max_concurrent` bounds the box load | bounds *foremen*, not the 3x worker processes inside one tick |
| a crashed tick is reaped by pid/heartbeat | the foreman's crash orphans worktrees + `in_progress` rows |

This spec decides how the scheduler and waves cooperate. It is **scheduler-side only**:
nothing in this document requires a change to the foreman or worker skills, which
already define wave composition, worktree discipline, and the merge contract.

Five questions are answered with a concrete DECISION each (§4-§8). One shared
invariant runs through all five:

> **W0 — The scheduler admits TICKS, never worker processes.** A wave is *inside*
> one tick, behind one gateway HTTP call, owned by one foreman process. Every
> decision below preserves that: the scheduler can measure waves, price them,
> report them, and shed them at tick boundaries — it can never serialize them.

---

## 2. Dependencies

| Dependency | Purpose | Failure mode |
|---|---|---|
| S01 (config/loader) | `tick_timeout` resolution, per-namespace flags | Wave flags unavailable → waves off (backward compatible) |
| S02 (data model) | `ticks` / `projects` / `namespaces` schema, migration ladder (currently **v26**) | Migration failure → daemon refuses to start (unchanged behavior) |
| S05 (spawn engine + lifecycle) | The single `ctx` deadline wraps the whole foreman session; terminal tick states | Waves inherit the 2h bound unless §4 lands |
| S06 (REST API) | `/api/v1/status` is the visibility surface for `wave_depth` | Wave load invisible to operators/dashboards |
| S07 (namespace extension) | Namespace caps (`max_concurrent`) and the borrowing pool | Namespace cap would need to grow a worker dimension |
| Foreman skill v2.10.0 | Wave composition: cap 3 workers, one wave per tick, worktree + merge contract | Without it no wave exists; no scheduler change can create one |
| Worker skill v1.6.0 | Sibling awareness; worktree-only writes; never push | — |

Relevant code paths (read, not modified, by this spec):

| Path | What it owns |
|---|---|
| `internal/config/loader.go` | `defaultTickTimeout = "2h"` (:54), `ApplyRootConfig` (:166-172), `SCHEDULER_TICK_TIMEOUT` env (:230), bump-aware pin skip in `ApplyFleetConfig` (:438-442), `namespaceFromDef` `max_concurrent` (:642-655) |
| `internal/scheduler/spawn.go` | `context.WithTimeout(ctx, s.timeout)` around the gateway call (:854) — the one deadline that wraps a whole foreman session; cost resolution at completion (:1676) |
| `internal/scheduler/slot_pool.go` | `SlotPool` counting semaphore, `running` refcount, `reserved` map (SCHED-GAP-103), `RunningSet()` |
| `internal/scheduler/packer_select.go` | `nsCapMap` / `nsRunningMap` (:45-67), namespace cap enforcement (:251, :365) |
| `internal/scheduler/lifecycle.go` | `TickStatus` set, `LifecycleTracker.Complete`, `CleanupStale`, `RunningCount` |
| `internal/scheduler/tick_process.go` | `timeoutReapSQL` (:296), `cleanDanglingOnStartup` (:307), `reapZombies` (:408) |
| `internal/scheduler/adaptive_cooldown.go` | progress = `code_commits > 0 \|\| netClosed`; fixture exclusion |
| `internal/scheduler/bump.go` | `markBumpTick`, `bumpTickCompleted`, `bumpHardCap = 12h` (:42) |
| `internal/scheduler/budget.go` | `BudgetBlockReason` — per-project daily/weekly/final caps (SCHED-GAP-066) |
| `internal/scheduler/cost.go` | `resolveRealTickCost` — foreman-home `state.db` session-cost sum over the tick window |
| `internal/api/server.go` | `status` handler (:154), `bumps` array convention (:249) |
| `internal/sync/duckbrain.go` | `ActiveTicks` in the fleet snapshot (:428) |

---

## 3. Definitions and Invariants

| Term | Definition |
|---|---|
| **Wave** | ≥2 worker dispatches inside ONE tick, each in its own worktree/branch. Foreman-created; never scheduler-created. |
| **Wave depth** | Number of workers the foreman dispatched in the current tick. `1` = serial tick (no wave). |
| **Foreman session** | The gateway session the scheduler spawned (the `ticks.session_id`). Owns the wave. |
| **Worker session** | A `hermes chat` session the foreman spawned inside its own tick. Invisible to the scheduler's spawn path. |
| **Wave manifest** | `.coding-hermes/waves/<tick_id>.json` in the project workdir — the foreman's own record of its wave (§9.3). The scheduler's only read path into wave internals. |
| **Slot** | A unit of scheduler admission. Counts ticks (foremen), never workers. |

**Invariants**

| # | Invariant |
|---|---|
| W1 | `max_concurrent` (`SlotPool`) counts running **ticks**. A 3-worker wave = 1 slot. |
| W2 | Namespace `max_concurrent` counts running **ticks** in that namespace. Unchanged from S07. |
| W3 | Worker processes never appear in `SlotPool.running`, `reserved`, `RunningSet()`, or the `active_ticks` status counter. |
| W4 | Ticket cost is counted **once**, at the tick (§5). Worker-level costs are attribution, never additive to the fleet total. |
| W5 | A wave never changes tick state names, `outcome` values, or the CHECK constraints in S02. Wave state is a separate table (§9.1). |
| W6 | The scheduler never writes into a worktree, never runs `git worktree`, and never deletes a worker branch. Recovery authority stays with the next foreman (§8). |

---

## 4. Q1 — Tick Wall-Clock Timeout Under a Wave

### 4.1 Current behavior

One deadline covers the entire foreman session. In `internal/scheduler/spawn.go:854`:

```go
ctx, cancel := context.WithTimeout(context.Background(), s.timeout)
```

`s.timeout` is the resolved `--tick-timeout` (`defaultTickTimeout = "2h"`,
`internal/config/loader.go:54`). That one `ctx` wraps the gateway `SendResponse` call
that *is* the foreman's whole session — the wave, the workers, the merges, all of it.
On expiry, S05 §4.5 escalates TERM → KILL and the tick lands terminal.

Wave wall-clock has two phases with different scaling:

| Phase | Scaling across a wave |
|---|---|
| Worker runtime (guard + judge per branch, concurrent) | ≈ `max(worker)`, **not** the sum |
| Merge phase (serial, gates re-run on the merged tree per branch) | ≈ `depth × merge-gate` |

A 3-wave is therefore roughly `max(worker) + 3 × merge_gate` — under 2h in the
common case, over it when a worker is slow or the merged-tree gates are heavy.

### 4.2 Options considered

| Option | Assessment |
|---|---|
| **(a) Per-namespace wave timeout bump** — a namespace-level `wave_tick_timeout` applied when the project's namespace has waves enabled | Static, computable at spawn time, no new protocol. Over-provisions serial ticks unless scoped correctly. |
| **(b) Wave budget accounting** — charge `weight × depth` so a wave costs more of the S04 budget | Weight is a *scheduling-share* currency, not a wall-clock bound. Charging 3x makes waves lose the pack contest and buys zero seconds. **Rejected as a timeout mechanism** (it remains a valid *fairness* question — see §16 OQ-4). |
| **(c) Timeout counts only the foreman's merge phase** — workers already ran, so exempt the worker phase | Not implementable: the deadline is one HTTP request; the scheduler cannot see phase boundaries inside it. **Rejected.** |
| **(d) Dynamic deadline extension** — the foreman's heartbeat extends the lease while it makes progress | Requires a new liveness protocol and re-opens the unbounded-tick hazard the deadline exists to prevent. **Rejected** (§4.4). |

### 4.3 DECISION 1 — Static, namespace-scoped wave deadline; overrun is recovered, never re-run

1. Add `namespaces.wave_tick_timeout` (duration string, empty = inherit). When a
   project's namespace sets it AND the namespace has waves enabled
   (`namespaces.wave_enabled = 1`), the spawn path resolves the tick deadline as
   `wave_tick_timeout`, otherwise `--tick-timeout` as today.
2. Resolution happens in one place — the same place `s.timeout` is set today
   (`internal/scheduler/spawn.go:854` call path, fed from `ApplyRootConfig`).
   Recommended value for wave-enabled namespaces: **`3h`** (1.5x base). Hard
   ceiling: **`4h`**, enforced at config validation; a larger value is rejected with
   a field-named error, never silently clamped.
3. The deadline is **never extended at runtime**. No heartbeat-driven lease, no
   mid-tick renegotiation.
4. On overrun the tick is terminal `timeout` (unchanged, S05 §4.5). The wave's work
   is **recovered, not re-run**: worker branches and the wave manifest survive the
   kill, and the next tick for that project runs wave recovery first (§8).

### 4.4 Rationale

The deadline cannot be derived from the wave because **the wave does not exist at
spawn time**. The scheduler spawns a foreman; the foreman reads its board and
*then* decides how many workers are wave-eligible. Any "wave-aware" timeout is
therefore either a static assumption or a new runtime protocol. A static
assumption with a bounded, per-namespace override (a) is honest, needs no new
liveness machinery, and keeps the single property the 2h bound buys: **a wedged
tick always releases its slot**. Option (d) would trade that for a class of tick
that can live arbitrarily long, on the strength of a heartbeat written by the very
process that might be wedged — the heartbeat already exists for *liveness
detection* (`S-GAP-003`, stale-heartbeat reaping), and reusing it for *deadline
extension* inverts its meaning.

3x is deliberately **not** the multiplier. Workers run concurrently, so the worker
phase scales as `max(worker)`, not the sum; only merges are serial and each is one
gate run. 1.5x covers a 3-wave whose gates are heavy without handing a serial tick
a 2x budget it does not need. The 4h ceiling exists because the scheduler's
`CleanupStale`/reaper staleness windows and the fleet's operational expectation
("a stuck project is visible within a few hours") are calibrated below it.

Overrun is a **recovery** event, not a loss event: the workers' branches are
committed artifacts, and S05's timeout path kills the *foreman* — it does not and
must not run `git worktree` cleanup (§8.4).

---

## 5. Q2 — Cost and Visibility of a 3x-Spend Tick

### 5.1 How cost works today — and why it already includes waves

At completion (`internal/scheduler/spawn.go:1676`) the tick's cost comes from
`resolveRealTickCost(foremanHome, workdir, project, start, finished)`
(`internal/scheduler/cost.go`), which **sums `estimated_cost_usd` for every session
in the foreman's isolated `HERMES_HOME` `state.db` whose activity window overlaps
the tick window**, plus `.gitreins/usage.jsonl` cost in the workdir. The function's
own doc comment already says so — `cost.go:70-75` describes the sum as *"foreman +
worker sessions in the foreman's Hermes state.db overlapping the tick window (the
dominant cost)"*.

Worker sessions spawned by the foreman use that same home. Consequence, stated
precisely because it drives the decision:

> **A wave's worker spend is already inside `ticks.cost_usd`.** The window sum does
> not filter by session id. What is missing is not the money — it is the
> **decomposition**: how many workers ran, which tasks/branches, which judge
> verdicts, which merges. The existing code even takes the session count `n` from
> `sumSessionCostInWindow` (`cost.go:84`) only to set the `real` flag, and discards
> it.

This means **adding worker costs to `ticks.cost_usd` would double-count**. The
schema change must therefore be attribution-only.

### 5.2 DECISION 2 — `worker_count` on `ticks` + a `tick_workers` attribution table; money counted exactly once

1. `ticks.worker_count INTEGER NOT NULL DEFAULT 0` — the number of **worker
   sessions the foreman dispatched inside this tick**, excluding the foreman
   itself. Legacy/serial rows read `0` (correct: the foreman session is the tick's
   own `session_id`); a 3-wave reads `3`. Session arithmetic is exact:
   `sessions = 1 + worker_count`.
2. `tick_workers` (new table, §9.1) — one row per dispatched worker: task id,
   branch, commit sha, judge result, merge result, per-worker token/cost as
   *reported by the foreman's manifest*.
3. **Money invariant (W4):** `ticks.cost_usd` remains the tick's single cost figure
   and already covers workers. `tick_workers.cost_usd` is attribution from the
   manifest and is **never** added to fleet totals. The hop-rate ledger and cost
   reporting sum `ticks.cost_usd` for money and use `worker_count` /
   `tick_workers` for the per-worker dimension (per-worker average, per-task
   attribution) — one number counted once.
4. `/api/v1/status` exposes wave load live (§9.4): a `waves` array (empty array, not
   null — the `bumps` convention at `internal/api/server.go:249`) and
   `wave_depth_total`.
5. `worker_count` and manifest ingestion are **best-effort and tolerant**: a missing
   or malformed manifest yields `worker_count = 0` plus a `warn` event. Cost
   reporting must never fail because a foreman wrote a bad manifest.

### 5.3 Why `worker_count` defaults to 0, not 1

The board row asked for "`ticks` should carry `worker_count`". `0` is the only value
that keeps the arithmetic honest against the existing columns: `ticks.session_id`
already *is* the foreman session, so a default of `1` would silently claim two
sessions for every historical serial tick and make "average sessions per tick" wrong
on the whole back catalogue. A serial tick's agent work is the foreman's own — it is
not a worker. `1 + worker_count` reads correctly on both v26 history and v27 waves.

### 5.4 Rationale

Attribution without double counting is the whole decision. The foreman is the only
component that knows the wave's shape (which task → which branch → which verdict →
which merge), and the fleet already has a convention for foreman-owned bookkeeping
files under `.coding-hermes/` — plus a useful property from SCHED-GAP-104: commits
touching only `.coding-hermes/` classify as *board* commits and cannot fake code
progress. A wave manifest placed there inherits that property for free: a foreman
cannot inflate `code_commits` by writing manifests.

---

## 6. Q3 — Concurrency Accounting (Slots vs. Workers)

### 6.1 Today

`SlotPool` (`internal/scheduler/slot_pool.go`) is a counting semaphore of
`maxConcurrent` (default 8) with a project-keyed refcount map and a `reserved` map
that closes the SCHED-GAP-103 double-spawn window; `RunningSet()` is the packer's
and evaluator's "already scheduled" truth. Namespace caps are computed from that
same running set (`internal/scheduler/packer_select.go:45-67`) and enforced at
`:251` and `:365`.

### 6.2 DECISION 3 — Slots stay foreman-counted; waves are made *visible*, and shed at tick boundaries

1. **`slot_pool.go` does not change.** No worker accounting, no wave-aware
   reservation, no new field. A 3-wave occupies exactly one slot, one entry in
   `running`, one entry in `RunningSet()` (W1, W3). The SCHED-GAP-103 reservation
   semantics are untouched.
2. **Namespace `max_concurrent` continues to count ticks** (W2). It is a
   tick-admission cap; a wave counts 1.
3. **New namespace column `wave_workers_cap`** (`0` = unlimited): the maximum
   number of concurrent *worker processes* the namespace may run, across all its
   running ticks. It is enforced in two layers, because no single layer can enforce
   it alone:
   - **Composition layer (advisory, authoritative in practice):** the scheduler
     injects `WAVE_BUDGET: <n>` into the foreman prompt, where
     `n = wave_workers_cap - live_wave_depth(namespace)`. `0` means *serial tick —
     do not compose a wave this tick*. The foreman caps its own wave at `n`.
   - **Admission layer (shed):** when `wave_workers_cap > 0` and the namespace has
     an in-flight tick with `worker_count > 0` that has not completed, the packer
     does not select further projects in that namespace for a wave — it selects at
     most the projects it can spawn with `WAVE_BUDGET: 0`. This is a coarse
     tick-boundary shed, deliberately consistent with SCHED-GAP-103's
     no-overlap principle.
4. **`/api/v1/status` exposes `wave_depth`** per in-flight wave and
   `wave_depth_total` fleet-wide (§9.4). This is the load-shedding input and the
   operator's view of "the box is running 3 extra agents right now".
5. The scheduler **never** counts wave workers against `active_ticks`.

### 6.3 Why the cap cannot be enforced inside a tick

The scheduler admits a tick by spawning one gateway HTTP call. Worker processes are
*grandchildren* — spawned by the foreman's session, in its own worktrees, with no
channel back to the scheduler until the tick completes and the manifest lands. Any
"hard" real-time cap would require a worker-level RPC that does not exist, or a
polling loop that races its own answer. So the honest design is: **advise at the
composition point (where the decision is actually made), shed at the tick boundary
(where the scheduler has authority).** Over-admission is bounded by
`wave_workers_cap - live_depth`, and the worst case (`cap=3`, one 3-wave in flight)
is exactly the intended maximum.

### 6.4 Rationale

Charging slots to workers was the tempting alternative and it fails on both counts.
It would make the cap lie: `active_ticks` would report 3 for one tick, breaking
`SlotPool.Running()`, the dashboard, and S05's documented "active children never
exceed the configured limit". And it would let one project monopolize the fleet —
a 3-wave of a heavy project would eat 3 of 8 slots while other projects starve,
which is precisely the load concentration the namespace/borrowing model (S07) exists
to prevent. Keeping slots = ticks and adding a *separate* process-count signal keeps
each number meaning one thing.

---

## 7. Q4 — Bump Interplay (SCHED-GAP-107)

### 7.1 The arithmetic

A bump (`bump_active`, `bump_cooldown_s`, `bump_remaining_ticks = N`) runs a project
at an accelerated cooldown for N completed ticks
(`internal/scheduler/bump.go`). With waves enabled at depth 3:

```
up to N ticks × up to 3 workers = up to 15 worker runs in one bump window
```

### 7.2 DECISION 4 — The leverage is intended; a bump multiplies TICKS, never workers

1. **`bump_active` does not cap the wave.** A bumped project keeps its full wave
   allowance. A bump is an explicit operator request to spend more on one project
   for a bounded window; suppressing its waves would defeat the feature.
2. **One tick consumes exactly one bump tick, regardless of depth.**
   `bumpTickCompleted` decrements `bump_remaining_ticks` once per completed tick
   (`internal/scheduler/bump.go:125-130`); this stays true for wave ticks. The
   countdown is a *tick* countdown — worker count never touches it. (Stated
   explicitly because "consume one bump unit per worker" is a plausible-looking
   mistake that would collapse a 5-tick window into a 2-tick window.)
3. **The bounds are the existing ones, not a new wave cap:**

   | Bound | Source | Value |
   |---|---|---|
   | Ticks in the window | `bump_remaining_ticks` | operator-set (e.g. 5) |
   | Wall clock of the window | `bumpHardCap` (`bump.go:42`) | 12h — force-revert |
   | Workers per tick | foreman wave cap (skill) | 3 |
   | Namespace worker load | `wave_workers_cap` (§6) | opt-in |
   | Money | per-project daily/weekly/final caps (SCHED-GAP-066, `budget.go`) | opt-in |

4. **Budget caps are the spend guardrail, and they now bind harder.** `budgetBlocked`
   is evaluated per project at pack time (`packer_select.go:105`); a wave multiplies
   *per-tick* spend, so a bumped, wave-enabled project reaches its daily cap in
   roughly `1/depth` of the ticks. This is correct behavior — the cap is a money
   guardrail and waves spend money faster — but it **must be visible**: when a
   project is budget-blocked while `bump_active = 1`, emit a `MEDIUM` event naming
   both conditions ("bump starved by budget cap"), because otherwise a 5-tick bump
   silently dies at tick 2 with no explanation.
5. **No new project-level wave flag**, therefore no new `ApplyFleetConfig` pin
   interaction. Wave config lives at the namespace level, which `ApplyFleetConfig`
   already treats as create-only + `default_prompt` update
   (`internal/config/loader.go:389-413`) — so the bump-aware cooldown pin skip
   (`loader.go:438-442`) needs no wave-specific branch.

### 7.3 Rationale

Bane's bump directive is "roughly 5 ticks running at a fast cooldown" — a time-boxed
resource injection. Multiplying workers is the *point*: the operator is asking the
project to consume more capacity, faster. The failure mode to guard against is not
"too much leverage", it is **unbounded** leverage, and every bound already exists:
the tick countdown, the 12h hard cap (which bounds a wave project whose ticks are
slow), the per-tick wave cap, the namespace worker cap, and the money caps. Adding a
sixth, wave-specific cap would give operators two knobs that fight (raise the bump,
lose the waves) with no new safety. The one real gap is *observability* — item 4's
event — because a budget starved bump otherwise looks like a broken bump.

---

## 8. Q5 — Failure Shape and Recovery

### 8.1 Two distinct failure classes

| Class | Example | Artifacts left behind |
|---|---|---|
| **Member failure** (worker fails judge) | guard red on `wt/TASK-1`, judge FAIL | branch with commits, worktree, board row, manifest entry |
| **Tick failure** (foreman dies mid-wave) | gateway drop, daemon restart, 2h/3h timeout, `SIGKILL` | `in_progress` board rows, **live worktrees**, partial manifest, `running` tick row + `tick_workers` rows |

### 8.2 DECISION 5 — In-tick rework; crash recovery is a first-class next-tick phase

1. **Member failure → rework inside the same tick** (existing doctrine, unchanged).
   The worker reworks in its own worktree; guard + judge re-run; the merge phase
   starts only when the branch is green or the worker is abandoned. The scheduler
   adds nothing here — it cannot see inside the tick.
2. **Abandoned member → branch preserved as evidence.** A worker that fails its
   bound is recorded `judge = fail`, `merge = preserved`, and its board row is marked
   failed **with a reason**. Never deleted: the branch is the honest output
   (§13, W6).
3. **Tick failure → the existing reaper reaps the tick row, and now also closes its
   worker rows.** `cleanDanglingOnStartup` (`internal/scheduler/tick_process.go:307`)
   and `reapZombies` (:408) keep their current tick semantics
   (`status='timeout'`, `outcome` unset — the CHECK constraint at S02 §3.2 admits no
   `zombie_reaped`; `completed_at` stamped per GAP-045; orphan stamps per
   SCHED-GAP-091). Added: a shared step that marks the tick's `tick_workers` rows
   (`state = 'abandoned'`) using the same collect-ids-then-close-rows-then-UPDATE
   discipline the reapers already follow (SQLite single writer, `:417-431`).
4. **The reaper does not touch worktrees** (W6). It has no workdir authority and a
   reaper that deletes a live worker's tree is a data-loss bug, not a cleanup.
   Cleanup stays with the next foreman.
5. **Wave recovery is a scheduler-visible, next-tick phase.** The scheduler scans
   `.coding-hermes/waves/*.json` (bounded `readdir`, tolerant parse — same shape as
   `countBoardRows`, `adaptive_cooldown.go:215`) and flags the project's next tick
   `wave_recovery = true` when a manifest exists whose `tick_id`'s row is terminal
   and whose own `finished_at` is empty. The injection into the foreman prompt is a
   preamble: **recover before dispatch** — for each preserved branch: re-run gates
   on the branch, merge or re-dispatch a fixup worker in a fresh worktree; then
   compose a new wave.
6. **One recovery tick at a time** — recovery is dispatched like any other tick
   (one slot), never as an extra parallel spawn.
7. **A reaped wave tick does not double-count progress.** `worker_count` was already
   persisted; the reaper leaves it. Adaptive cooldown reads the tick's
   `code_commits`/board signals as usual (a timed-out wave counts as no-progress —
   the same treatment a timed-out serial tick gets).

### 8.3 Recovery sequence (next tick, foreman side)

```text
1. Read .coding-hermes/waves/*.json → any manifest with finished_at == "" ?
2. Check the manifest's tick row: terminal (failed/timeout) → recovery candidate.
3. git worktree list --porcelain → match branches from the manifest.
4. Per branch: gates on the branch tip → green? merge into main (serial).
                                 → red?     re-dispatch fixup worker, fresh worktree.
5. Board truth: every affected task complete, or failed WITH a reason.
6. Worktrees removed; merged branches deleted; unmerged preserved as evidence.
7. Only then: compose the new wave for this tick.
8. Write the current manifest; set finished_at when the tick's wave is done.
```

### 8.4 Rationale

The foreman skill already mandates the two artifacts that make recovery possible —
`in_progress` marks before dispatch ("a crashed tick must leave recoverable state,
not silent loss") and one worktree per worker with a self-identifying branch name.
The scheduler's job is therefore narrow: (a) keep the tick row truthful, (b) keep
the recovery *trigger* observable, (c) never destroy the artifacts. Putting recovery
in the next tick rather than in the reaper is the load-bearing choice: the reaper
runs at startup and on a periodic tick with no workdir context, no gate runner, and
no merge authority — exactly the wrong process to decide whether a half-finished
branch is good. The next foreman has all three.

---

## 9. Data

### 9.1 Migration v27

Latest applied migration is **v26** (task bump, SCHED-GAP-107). Wave support is
v27, additive and default-off:

```sql
-- v27: concurrent wave scheduling (S12)
ALTER TABLE ticks ADD COLUMN worker_count INTEGER NOT NULL DEFAULT 0;
ALTER TABLE ticks ADD COLUMN wave_recovery INTEGER NOT NULL DEFAULT 0;

ALTER TABLE namespaces ADD COLUMN wave_enabled INTEGER NOT NULL DEFAULT 0
    CHECK(wave_enabled IN (0, 1));
ALTER TABLE namespaces ADD COLUMN wave_tick_timeout TEXT NOT NULL DEFAULT '';
ALTER TABLE namespaces ADD COLUMN wave_workers_cap INTEGER NOT NULL DEFAULT 0
    CHECK(wave_workers_cap >= 0);

CREATE TABLE IF NOT EXISTS tick_workers (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    tick_id      TEXT NOT NULL REFERENCES ticks(id) ON DELETE CASCADE,
    task_id      TEXT NOT NULL,             -- board task the worker was dispatched for
    branch       TEXT NOT NULL,             -- wt/<task-id>
    worktree     TEXT NOT NULL DEFAULT '',  -- /home/kara/worktrees/<project>-<task>
    commit_sha   TEXT NOT NULL DEFAULT '',  -- branch tip reported by the foreman
    judge        TEXT NOT NULL DEFAULT 'unknown'
                 CHECK(judge IN ('pass','fail','withdrawn','unknown')),
    merge        TEXT NOT NULL DEFAULT 'pending'
                 CHECK(merge IN ('merged','conflict','preserved','pending')),
    state        TEXT NOT NULL DEFAULT 'running'
                 CHECK(state IN ('running','done','abandoned')),
    cost_usd     REAL NOT NULL DEFAULT 0,   -- attribution ONLY (W4)
    tokens_in    INTEGER NOT NULL DEFAULT 0,
    tokens_out   INTEGER NOT NULL DEFAULT 0,
    created_at   TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at   TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE INDEX IF NOT EXISTS idx_tick_workers_tick ON tick_workers(tick_id);
CREATE INDEX IF NOT EXISTS idx_tick_workers_task ON tick_workers(task_id);
```

Notes:
- `wave_recovery` on `ticks` is set at spawn so the row itself records that the tick
  was a recovery tick (report/dashboard can count them).
- No `ticks.outcome` / `ticks.status` change (W5): worker state lives in
  `tick_workers.state`, and the S02 CHECK constraints are untouched.
- `tick_workers` is 0..3 rows per wave tick, ≤8 accepted from a manifest (§9.3).

### 9.2 Go model additions (`internal/database/models.go`)

```go
// Tick (existing struct) gains:
WorkerCount  int `json:"worker_count"`    // worker sessions dispatched (0 = serial tick)
WaveRecovery int `json:"wave_recovery"`   // 1 = tick ran the wave-recovery phase

// Namespace (existing struct) gains:
WaveEnabled     bool   `json:"wave_enabled"`
WaveTickTimeout string `json:"wave_tick_timeout"` // "" = inherit scheduler.tick_timeout
WaveWorkersCap  int    `json:"wave_workers_cap"`  // 0 = unlimited

// New:
type TickWorker struct {
    ID        int64   `json:"id"`
    TickID    string  `json:"tick_id"`
    TaskID    string  `json:"task_id"`
    Branch    string  `json:"branch"`
    Worktree  string  `json:"worktree"`
    CommitSHA string  `json:"commit_sha"`
    Judge     string  `json:"judge"`   // pass|fail|withdrawn|unknown
    Merge     string  `json:"merge"`   // merged|conflict|preserved|pending
    State     string  `json:"state"`   // running|done|abandoned
    CostUSD   float64 `json:"cost_usd"`
    TokensIn  int64   `json:"tokens_in"`
    TokensOut int64   `json:"tokens_out"`
    CreatedAt string  `json:"created_at"`
    UpdatedAt string  `json:"updated_at"`
}
```

`NamespacePatch` gains `WaveEnabled *bool`, `WaveTickTimeout *string`,
`WaveWorkersCap *int` so the existing namespace PUT/PATCH surface can set them.

### 9.3 Wave manifest contract (`.coding-hermes/waves/<tick_id>.json`)

Written by the **foreman** (it owns `.coding-hermes/`; workers never write board
state). Read by the scheduler at tick completion and at pack time for the recovery
flag.

```json
{
  "tick_id": "h3-sdk-go-2026-09-13-02-50-55",
  "project": "h3-sdk-go",
  "started_at": "2026-09-13T02:50:55Z",
  "finished_at": "",
  "workers": [
    {
      "task_id": "SCHED-GAP-109",
      "branch": "wt/SCHED-GAP-109",
      "worktree": "/home/kara/worktrees/h3-sdk-go-SCHED-GAP-109",
      "commit_sha": "abc1234",
      "judge": "pass",
      "merge": "merged",
      "cost_usd": 0.0,
      "tokens_in": 0,
      "tokens_out": 0
    }
  ]
}
```

Ingestion rules (all deliberately fail-safe):

| Rule | Behavior |
|---|---|
| File missing | `worker_count = 0`, no `tick_workers` rows, **no event** (serial tick is normal) |
| Malformed JSON | `worker_count = 0`, `WARN` event naming tick + path |
| `workers` > 8 entries | first 8 ingested, `WARN` event (bounded write) |
| File > 64 KiB | not parsed, `WARN` event |
| Unknown keys | ignored |
| `finished_at` non-empty at crash-scan time | not a recovery candidate (the foreman closed it) |
| `tick_id` mismatches the tick being completed | rejected, `WARN` event |

### 9.4 `/api/v1/status` additions

```json
{
  "wave_depth_total": 4,
  "wave_workers_cap_configured": true,
  "waves": [
    {
      "project": "h3-sdk-go",
      "tick_id": "h3-sdk-go-2026-09-13-02-50-55",
      "namespace": "coding-hermes",
      "worker_count": 3,
      "started_at": "2026-09-13T02:50:55Z",
      "age_s": 1840
    }
  ]
}
```

- `waves` is **always** an array (`[]`, never `null`) — the `bumps` convention
  (`internal/api/server.go:249`).
- `wave_depth_total` = sum of `worker_count` over **running** ticks. This is the
  number the operator watches; it is *not* `active_ticks`.
- Per-project wave depth is the `worker_count` of that project's running tick (a
  project can have at most one — SCHED-GAP-103).
- `/api/v1/ticks/{id}` includes `worker_count`, `wave_recovery`, and the
  `tick_workers[]` rows when present.

### 9.5 DuckBrain sync additions (`internal/sync/duckbrain.go`)

The fleet snapshot (`ActiveTicks`, :428) gains `ActiveWaves`,
`WaveDepthTotal`, and `WaveWorkers` (the compact §9.4 array). Frozen semantics from
S02: DuckBrain stays a read replica — SQLite remains authoritative.

---

## 10. States

### 10.1 Tick state machine — unchanged

```text
QUEUED ──Start succeeds + running row persisted──▶ RUNNING
   │                                                 │
   └──validation/Start failure──▶ FAILED             ├──exit 0 + export valid──▶ COMPLETED
                                                     ├──nonzero/signal/export failure──▶ FAILED
                                                     └──tick deadline──▶ TIMEOUT
```

Waves add **no** tick states and no new terminal values (W5). A wave tick that
overruns §4 is `timeout`; a wave tick whose merges all landed is `completed`.

### 10.2 Wave lifecycle inside one RUNNING tick

```text
RUNNING (foreman)
   │
   ├─ compose wave (eligibility: independent tasks, ≤ WAVE_BUDGET, ≤ 3)
   ├─ worktrees created, board rows → in_progress      ──┐ artifacts exist here:
   ├─ workers run (concurrent, own worktree, own judge)  │ in_progress rows
   │      └─ member fails judge → rework in-tick ────────┤ worktrees + branches
   ├─ merges serial (gates re-run on the merged tree)    │ manifest (finished_at="")
   ├─ worktrees removed, board truth, manifest closed ───┘
   └─ tick terminal (COMPLETED / FAILED / TIMEOUT)
```

The bracketed region is exactly the window in which a foreman crash produces
recoverable-but-unfinished state — and it is why the manifest's `finished_at`
field, not the tick's status alone, decides recovery eligibility.

### 10.3 `tick_workers` states

```text
running ──worker reports done──▶ done
   │
   ├── reaper marks the tick terminal ──▶ abandoned   (§8.2 item 3)
   └── foreman abandons the member ────▶ done + judge='fail', merge='preserved'
```

A row never returns from `done`/`abandoned`. `abandoned` is an outcome of the tick
dying, not of the worker failing — the distinction matters when reading a recovery
report.

---

## 11. Errors

| Condition | Behavior | Recovery |
|---|---|---|
| Wave exceeded the tick deadline | Tick `timeout`; foreman killed; branches/manifest survive | Next tick runs wave recovery (§8.3) |
| Foreman crashed (gateway drop, restart) | Reaper reaps tick; `tick_workers` → `abandoned`; orphan stamp (SCHED-GAP-091) | Same as above |
| Manifest missing on a known wave tick | `worker_count = 0`; no rows; no event | Wave detail lost; tick cost already correct (§5.1) |
| Manifest malformed / oversized / mismatched | Ignored + `WARN` event | Re-dispatch not needed; tick is otherwise intact |
| Worker branch conflicts on merge | Foreman: `merge = conflict`, fresh fixup worker in a new worktree | Fixup worker; never hand-resolved |
| Worker abandoned after rework bound | `judge = fail`, `merge = preserved`; board row failed WITH reason | Branch kept as evidence |
| `wave_workers_cap` exceeded at composition | Foreman caps/refuses the wave; scheduler sheds at the tick boundary (§6.2) | Next tick, lower depth |
| Bumped project blocked by budget cap | Tick not packed; `MEDIUM` event naming bump + budget (§7.2 item 4) | Operator raises the cap or lets the bump expire |
| `wave_tick_timeout` > 4h | Config validation error at startup, field-named | Fix the value |
| Worktree dir missing at recovery | Skip that branch, note it in the manifest/report | Branch may still be mergeable from `git branch` |

---

## 12. Testing

### Unit tests

```go
// Timeout resolution (§4)
func TestEffectiveTickTimeout_WaveNamespace(t *testing.T)        // wave_enabled + wave_tick_timeout → that value
func TestEffectiveTickTimeout_SerialNamespaceInherits(t *testing.T)
func TestConfig_RejectsWaveTickTimeoutAboveCeiling(t *testing.T) // > 4h → field-named error

// Manifest ingestion (§9.3)
func TestParseWaveManifest_Valid(t *testing.T)                   // 3 workers, all fields
func TestParseWaveManifest_MissingFile(t *testing.T)             // 0 workers, NO event
func TestParseWaveManifest_Malformed(t *testing.T)               // 0 workers + WARN
func TestParseWaveManifest_TooManyWorkers(t *testing.T)          // 8 ingested + WARN
func TestParseWaveManifest_TickIDMismatch(t *testing.T)          // rejected + WARN

// Slot accounting (§6) — the regression guard for "slots must not see workers"
func TestSlotPool_WaveTickOccupiesOneSlot(t *testing.T)
func TestRunningSet_UnaffectedByWorkerCount(t *testing.T)
func TestNamespaceCap_CountsWaveTickAsOne(t *testing.T)

// Bump interplay (§7)
func TestBumpTickCompleted_WaveTickConsumesOneBumpTick(t *testing.T)
func TestBumpWave_BudgetStarvedEvent(t *testing.T)

// Reaper (§8)
func TestReaper_MarksTickWorkersAbandoned(t *testing.T)
func TestReaper_DoesNotTouchWorktrees(t *testing.T)              // asserts no fs/git calls
func TestWaveRecoveryFlag_OnlyForTerminalUnfinishedManifest(t *testing.T)
```

### Integration flow

```text
1. Temp DB at v26 → migrate v27 → assert columns, CHECKs, indexes.
2. Project in a wave-enabled namespace (wave_tick_timeout=3h, wave_workers_cap=3).
3. Spawn a tick with a mock gateway; write a manifest with 3 workers mid-tick.
4. GET /api/v1/status → waves[0].worker_count == 3, wave_depth_total == 3,
   active_ticks == 1 (W1/W3).
5. Complete the tick → ticks.worker_count == 3, 3 tick_workers rows,
   ticks.cost_usd unchanged by the manifest's cost_usd (W4, no double count).
6. Kill the foreman mid-tick (mock) → run cleanDanglingOnStartup → tick timeout,
   tick_workers all 'abandoned', wave_recovery flag set on the next spawn.
7. Namespace cap: wave_workers_cap=3 with a live 3-wave → next eval packs a
   serial tick (WAVE_BUDGET: 0), not a second wave.
```

### Required RED checks before landing (implementation rows)

- Reverse the namespace cap to count workers → `TestNamespaceCap_CountsWaveTickAsOne` must fail.
- Add manifest `cost_usd` into `ticks.cost_usd` → the double-count integration assertion must fail.
- Remove the reaper's `tick_workers` step → `TestReaper_MarksTickWorkersAbandoned` must fail.

---

## 13. Security

| Vector | Mitigation |
|---|---|
| Manifest as an injection surface | Manifest is read-only data: bounded size (64 KiB), bounded rows (8), unknown keys ignored, `tick_id` must match the completing tick; never executed, never interpolated into SQL (parameterized inserts). |
| Manifest faking progress | Lives under `.coding-hermes/` → its commits classify as *board* commits (SCHED-GAP-104) and cannot inflate `code_commits`. |
| `wave_tick_timeout` as a tick-immortality knob | Hard 4h ceiling at config validation; no runtime extension (§4.3). |
| Resource exhaustion via deep waves | `wave_workers_cap` (namespace) + foreman wave cap 3 + `WAVE_BUDGET` injection; budget caps bound spend. |
| Worktree path injection from a manifest | Paths are recorded, never used as argv. The scheduler never runs `git worktree`/`os.RemoveAll` on manifest data (W6). |
| Worker rows leaking cost/secret data | Only ids, branch names, shas, verdicts, and numeric cost/token columns — no prompts, no stdout, no session content. |

---

## 14. Performance

| Metric | Target |
|---|---|
| Manifest parse + ingest at tick completion | < 20ms p99 (single small file, ≤8 rows, one transaction) |
| Wave-recovery scan at pack time | < 5ms per project (bounded `readdir` + tolerant parse; same class as `countBoardRows`) |
| `/api/v1/status` wave block | < 2ms added (one indexed query over running ticks) |
| `wave_depth_total` | O(running ticks), no extra query per project |
| Migration v27 | < 100ms (five ADD COLUMN + one CREATE TABLE on the existing DB) |
| Slot accounting | zero change — no new hot-path work in `slot_pool.go` |
| Serial ticks (waves off) | byte-identical behavior and cost |

---

## 15. Rollout and Backward Compatibility

The wave feature is **default off** at every layer:

| Layer | Default | Effect |
|---|---|---|
| `namespaces.wave_enabled` | `0` | Wave deadline/budget logic inert; schedules exactly as today |
| `namespaces.wave_tick_timeout` | `''` | Inherits `--tick-timeout` (2h) |
| `namespaces.wave_workers_cap` | `0` | Unlimited = today's (unenforced) behavior |
| `ticks.worker_count` | `0` | Historical rows read correctly (§5.3) |
| Migrations | v27 additive | Existing DBs migrate in place |

Migration path:

1. Deploy the binary with v27 migrations. No behavior change (waves inert).
2. Implementation rows land the manifest ingest + status surface. Operators begin
   seeing `waves`/`wave_depth_total` from real foremen (foremen already write
   nothing, so this stays `0` until §9.3 is adopted in the foreman skill).
3. Add `.coding-hermes/waves/<tick_id>.json` writing to the foreman skill, then
   verify `worker_count` lands on real wave ticks.
4. **Then** enable `wave_enabled` + `wave_tick_timeout` per namespace, heaviest
   namespaces first.
5. `wave_workers_cap` last: it is the load-shed lever, and only meaningful once
   waves are common enough to contend.

---

## 16. Open Questions

Each item below becomes an implementation row after this spec is ratified. Ordered
by dependency: 1 → 2 → (3,4) → (5,6).

| # | Row (proposed id) | Scope | Blocked by |
|---|---|---|---|
| 1 | `SCHED-GAP-109` — migration v27 + model plumbing | `ticks.worker_count`/`wave_recovery`, `namespaces.wave_*`, `tick_workers` table, `TickWorker` model + CRUD, namespace patch fields, `NamespacePatch` surface | — |
| 2 | `SCHED-GAP-110` — wave manifest ingest | `.coding-hermes/waves/<tick_id>.json` parse (bounded/tolerant), completion-path ingestion at `spawn.go:1676`-adjacent hook, `worker_count` + rows in one transaction, WARN events | 109 |
| 3 | `SCHED-GAP-111` — effective tick deadline | `wave_tick_timeout` resolution in the spawn path (`spawn.go:854`), 4h ceiling in `config.Validate`, `SCHEDULER_WAVE_TICK_TIMEOUT` env override | 109 |
| 4 | `SCHED-GAP-112` — `/api/v1/status` wave surface | `waves[]`, `wave_depth_total`, per-tick `tick_workers[]` on `/api/v1/ticks/{id}`, DuckBrain snapshot fields | 110 |
| 5 | `SCHED-GAP-113` — namespace worker cap + `WAVE_BUDGET` | `wave_workers_cap` enforcement: prompt injection at spawn, tick-boundary shed in `packer_select.go` | 109, 110 |
| 6 | `SCHED-GAP-114` — reaper + recovery trigger | `tick_workers` → `abandoned` in `cleanDanglingOnStartup`/`reapZombies`, `wave_recovery` flag from the manifest scan, "recover before dispatch" prompt preamble | 110 |
| 7 | `SCHED-GAP-115` — wave cost attribution in reporting | per-worker average + per-task attribution in the cost/hop-rate rollup; explicitly **non-additive** (W4); observatory column | 110 |

Deliberately **out of scope** here, with rationale:

- **Wave-aware weight charging (OQ-4 from §4.2 option b).** Whether a 3-deep wave
  should consume `3 × weight` of the S04 budget is a *fairness* question, not a
  timeout one. It belongs with the fairness/packing work (SCHED-GAP-103 line,
  `internal/scheduler/fairness.go`) and needs spend data from row 7 first.
- **Per-worker session capture by the scheduler.** The scheduler could in principle
  export each worker session from the foreman home `state.db` (the sessions are
  there). Deferred: it needs a session→task mapping the scheduler cannot infer
  without the manifest, i.e. it is strictly downstream of row 2.
- **Automatic worktree reaping by a cron.** Rejected on principle (W6, §8.4): a
  process that deletes worktrees without running gates can delete unmerged work.
  Recovery belongs to the foreman that will merge the branch.

Open questions that would change this spec's decisions if answered:

1. Does the gateway expose per-request child-session identity? If a wave tick's
   HTTP response ever carries worker session ids, §5's manifest indirection becomes
   optional and the scheduler could attribute directly.
2. Should `wave_workers_cap` be enforced as a hard refuse-to-spawn (leave the project
   queued) once waves are common, rather than the advisory + shed of §6.2? Decide
   after real `wave_depth_total` data exists.
