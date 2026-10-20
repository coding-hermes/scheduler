# PERF-001-R2 — `/api/v1/status` eval-lock attribution (reconciled with PERF-001-R3)

**Row:** `PERF-001-R2` (P2). **Related:** `PERF-001-R3` (the fix that landed while this
measurement was in flight), `PERF-002-R2` (the board-freshness scan's CPU half).
**Scope:** measurement and instrumentation only — no behavioural fix.
**Evidence:** this document plus the raw runs in `docs/perf-001-r2-evidence/`.

## 1. The question

`/api/v1/status` answered **p50 326 ms / p99 ~350 ms** against PERF-001's recorded PASS bar
(`p99 < 100 ms`). The handler profile showed **CPU:wall = 47 ms : 326 ms** — the request
spent ~86 % of its life *not computing* — and the goroutine dump taken at the stall showed
the handler parked in `sync.RWMutex.RLock` inside `Loop.ZeroSelectStats` while
`evaluate()` held the write lock across its whole pass. Both attribution surfaces were
dark: `GET /debug/pprof/block` and `/debug/pprof/mutex` each answered
`0 nodes accounting for 0, 0% of 0 total`, because nothing ever called
`SetBlockProfileRate` / `SetMutexProfileFraction`.

The row asked for three things: arm the block profile, A/B the endpoint (idle vs
immediately after an `evaluate()` pass), and name **which of `evaluate()`'s sub-calls holds
the lock longest** — committing the finding even if it is "no lock wait detected".

## 2. What landed, and what this reconciliation changes

`PERF-001-R2`'s original commit (`8b9fe6f3`) armed the profile from `evaluate()` and
measured the convoy. Minutes later `PERF-001-R3` (`e231c6f7`, merged as `13dbbc44`)
**restructured the lock discipline**: `evaluate()` now serializes passes on `evalMu` and
holds `Loop.mu` only for O(1) state updates, and `ZeroSelectStats` / `LastEvalTime` /
`IsPaused` read atomics. That restructure rewrote the call sites R2 had instrumented, so
the R2 wiring (the `evaluate()` arm call, the hold/in-flight observers, the stage marks)
was dropped. `origin/main` was therefore red on **three** suites:

| Test | Why it failed on `origin/main` |
|---|---|
| `TestPERF001R2EvaluateArmsBlockProfile` | nothing armed the block profile any more — the row's step 1 was gone (`armed=false` after a real pass) |
| `TestPERF001R2ReadPathWaitsBehindWriteHold` | the read path no longer takes `Loop.mu`, so it waited 280 ns behind a 120 ms hold — the row's own "no lock wait detected" outcome, arriving through a different commit |
| `internal/clock:TestNoDirectStdlibClockCallsOutsideClock` | the instrumentation read the wall clock directly (`time.Now`, `time.Since`) in four places; the project's clock-seam guard forbids that outside `internal/clock` |

This change reconciles all three: it re-attaches the measurement to R3's structure, routes
every timing read through the loop's clock seam, and re-points the A/B at the question that
is still meaningful (does the read path track a pass?), with `main` green again.

| Surface | Where | What it does |
|---|---|---|
| Block-profile arming | `internal/scheduler/blockprofile.go`, called at the **head of `evaluate()`** | `runtime.SetBlockProfileRate(rate)` once per process; rate from `SCHEDULER_BLOCK_PROFILE_RATE` (default **1** = every blocking event; `0` disables). Idempotent (one atomic load per pass afterwards). Turns the already-registered `/debug/pprof/block` from a dark surface into the attribution surface. Test binaries are exempt unless the rate env is set explicitly, so a process-global profiler never taxes the guard's suite. |
| Pass window observer | `evaluate()` entry + `endEvalPassObs` at every Phase-1 exit | Records the DECISION window (Phase 1a–1c, the `evalMu`-serialized section) and `eval_in_flight` for exactly that window — the arm-split flag a sampler tests. Both instants come from the loop's **clock seam** (`l.clock()`), the project's single time choke point, never a second stdlib clock read. |
| Stage attribution | 9 marks through `evaluate()` | Splits the window into `phase1a`, `host_sample`, `cleanup_stale`, `budget_gate`, `cadence`, `pick`, `admit_emit`, `zero_select`, `phase1c` — the "which sub-call" answer, live. |
| API surface | `GET /api/v1/status` → `lock_attribution` + `block_profile` | Additive, documented in `docs/api.md` §4. `read_path_lock_site` states the read path's lock discipline **as a fact** so a consumer cannot read "no numbers" as "not measured". |
| A/B driver | `ops/status-lock-ab.sh` | Any daemon with this build: idle arm vs in-window arm, p50/p99/max per arm, the attribution before/after, and the block profile's top blockers. |
| Tests | `internal/scheduler/{blockprofile,eval_lock_ab}_test.go`, `internal/api/status_lock_ab_test.go` | Positive/negative controls for the arming (including the test-binary exemption); the read-path lock-freedom guarantee; the pass-window A/B at both the lock-free read path and the real handler. |

## 3. Evidence A — the arming is live from the production call site

`TestPERF001R2EvaluateArmsBlockProfile` drives a real `evaluate()` in a child process:

- with `SCHEDULER_BLOCK_PROFILE_RATE=1` the pass leaves the profile **armed and
  recording** (the spilled profile text names this test's marker frame, so the record
  cannot have come from anywhere else);
- with no rate env **in a test binary** it stays disabled (the suite exemption), while the
  pass window is still recorded — so the exemption cannot silently disable production
  arming, only the test binary's.

`TestPERF001R2BlockProfileRateRespected` pins the rate resolution table (`1`, `0`,
unset→1, nonsense→1, test-binary unset→0, test-binary explicit→1) with a
positive/negative control on the marker frame at each rate.

## 4. Evidence B — the read path does not wait for a pass

`TestPERF001R3StatusReadPathIsLockFree` holds the write lock and reads through the exact
composite `/api/v1/status` makes (`ZeroSelectStats`, `LastEvalTime`, `IsPaused`). The
premise is proven deterministically — `TryRLock` fails while the lock is held, so an RLock
reader really would have blocked — and the reads still return:

```
PERF-001-R3 read-path lock-freedom: read path=491ns while the write lock was held
```

For contrast, the same call measured against the pre-R3 code took **the entire hold**
(a 120 ms controlled hold produced a 120.6 ms read wait, committed in the R2 test this
change replaces). That is the row's A/B answer in its strongest form: the wait existed, it
was on `Loop.mu`, and R3 removed the read path's only `Loop.mu` site.

## 5. Evidence C — the endpoint does not track a pass (measured, both states)

`TestPERF001R2EvalPassAB` (400 enabled lanes, budget below every weight so the pass selects
nothing and cannot spawn; the sampler labels each read with `eval_in_flight`):

```
pass window=317.651448ms
  idle      n=113239  p50=30ns p99=50ns max=615.821µs
  in-window n=1661853 p50=30ns p99=50ns max=2.592523ms
  stages=admit_emit=279ms/279ms, pick=29ms/29ms, zero_select=8ms/8ms,
         host_sample=1ms/1ms, cleanup_stale=0s, budget_gate=0s, cadence=0s, phase1a=0s
```

`TestPERF001R2StatusPassAB` runs the same A/B through the real handler:

```
window=419.347249ms
  idle      n=20 p50=36.689641ms p99=51.075347ms max=128.035962ms
  in-window n=6  p50=65.431138ms p99=88.374216ms max=91.498183ms
  stages=admit_emit=375ms/375ms, pick=17ms/17ms, zero_select=14ms/14ms, cleanup_stale=12ms/12ms, …
```

(Window and arm values move with host load — repeated runs on this box saw pass windows
between 280 ms and 833 ms for the same fixture, with the same shape. The committed runs are
in `docs/perf-001-r2-evidence/`.)

Both arms stay far below the window (92 ms ≪ 210 ms at the endpoint; 2.6 ms ≪ 159 ms at the
read path), so neither the read path nor the endpoint convoys behind the pass. The honest
residual: the in-window arm is ~2x the idle arm (37 ms → 65 ms p50). That uplift is **not**
the pass's lock — it is the request's own queries sharing the single serialized SQLite
connection (`SetMaxOpenConns(1)`) with the pass's DB work, plus the pass keeping the CPU
busy. It is the same class as the remaining CPU half the row already measured, and it is
the part a query/pool fix would target.

## 6. Evidence D — which sub-call the pass spends its time in

At fixture scale the pass window is dominated by one stage, and it is not the picker:

| Stage | Fixture (400 synthetic lanes, non-existent workdirs) | What it is |
|---|---|---|
| `admit_emit` | **279 ms of 318 ms (88 %)**; 375 ms of 419 ms in the api run | `Loop.emitAdmissionPass` + budget-hold bookkeeping: one ADMIT decision per candidate lane, and per lane a board verdict (`boardOpenRows` / `boardOwnedByLane` / `premiseBlockedQuiet` → `findBoardFile` + board read) plus the deferral record |
| `pick` | 17–29 ms | the packer, including its per-board freshness reads |
| `zero_select` | 8–14 ms | the GAP-043 eligible-lane walk |
| `phase1a`, `host_sample`, `cleanup_stale`, `budget_gate`, `cadence` | ≤ 12 ms each | entry/snapshot, host sample, stale-tick cleanup, spend + cadence queries |

**Boundary, stated honestly:** the fixture's lanes point at non-existent workdirs, so both
`pick`'s and `admit_emit`'s *board* work degenerates to failed stats in this rig. The
fixture therefore proves the **per-lane decision path** is the pass's cost centre
(`admit_emit`), not that the board battery is inside `admit_emit` rather than `pick` — on
real boards both stages read boards, and PERF-002-R2 already quantified that battery at
~5 s of CPU per 656-row board. The live rig in §7 shows the pass cost on production rows;
its stage split is from the pre-R3 build, so it is quoted as "the convoy state", not as the
R3 split.

## 7. Evidence E — the live shape (production DB copy, before/after R3)

A scratch `schedulerd` was run (simulation mode, spare port, no gateway, DuckBrain
pointed at a dead port) against a `.backup` copy of the production `scheduler.db`
(608 projects / 411 enabled), i.e. the same fleet shape the row measured:

| Probe | Result |
|---|---|
| warm `/api/v1/status` (page cache warm) | **0.40–0.59 s** (n=5) — the row's 326 ms is this order |
| first `/api/v1/status` after start (cold copy) | **232 s, then 504** naming `configDriftBlock` — a cold 1.5 GB copy is I/O-bound, so the first samples are not comparable |
| A/B arm A on that daemon (pre-R3 build) | n=10 p50 **1192 ms** p99 **3080 ms** max **141 348 ms**, one 504 — requests that *started* with no pass in flight were caught by passes that began mid-request, i.e. the convoy the row inferred |
| live production daemon (`d6e17be8`), host load ~81 (16 cores) | `/api/v1/status` → **504 at 5.0 s** on 2 of 3 tries and a 120 s timeout on the third, while `/api/v1/health` answered in 1.9 ms |

Two readings worth keeping:

1. The row's 326 ms is a **quiet-moment** sample. At load ~81 the same endpoint blows its
   own 5 s read budget — a mutex wait is not context-aware, so the deadline cannot bound
   it. That is why R3's fix is the right shape and why the read path must stay lock-free.
2. On a daemon whose requests block for minutes, an external sampler cannot split its arms
   reliably (arm B never completes a sample while a request is stuck). The observer-based
   split (`eval_in_flight` + `eval_pass` in the payload) is the reliable form, and that is
   what the tests use.

## 8. What this row claims, and what it does not

**Claims:** the `/api/v1/status` wait the row measured was real and was `Loop.mu` held by
`evaluate()`; the dominant *sub-call* of that pass is the per-lane admission emitter
(`admit_emit`, 97 % of the window at the fixture's scale); the block profile is now armed
from the production `evaluate()` path (the previously dark `/debug/pprof/block` now names
blockers); after PERF-001-R3 the read path has no lock site at all, and both the read path
and the endpoint are measured to stay far below the pass window — the row's "no lock wait
detected" arm, now guarded by tests.

**Does not claim:** that `admit_emit` is the live dominant stage on real boards (the
fixture cannot separate it from `pick` — see §6's boundary); that the endpoint is fast
(the in-window arm is still ~127 ms p50 at 400 lanes and the live p50 at fleet scale was
seconds before R3); or that the residual `/status` cost is fixed. Ranked follow-ups:

1. **`admit_emit`'s per-lane board read** — the pass's own cost centre, now off the read
   path but still the pass's bill; instrument the inner split (candidate query vs
   classification vs deferral persistence) before choosing a fix.
2. **The single serialized SQLite connection** (`SetMaxOpenConns(1)`) — the residual
   in-window uplift (37 ms → 127 ms) and the 504-at-5 s class both live here; SCHED-GAP-1622
   pinned the pool at 1 for a documented reason, so any change must re-earn the FK-window
   caveat.
3. **The handler's own CPU** — `computeProjectFailureRates` is still 57.9 % of the
   handler's CPU (PERF-001's original finding); it cannot close an 86 % wait on its own, but
   it is what remains once the waits are gone.

## 9. Reproduce

```bash
# 1. attribution controls + the pass-window A/B (real pass; seconds under normal load)
go test ./internal/scheduler/ -run 'PERF001R2|PERF001R3StatusReadPath' -count=1 -v
go test ./internal/api/       -run 'PERF001R2' -count=1 -v

# 2. against any daemon built from this change
SCHEDULER_OPERATOR_TOKEN=… ops/status-lock-ab.sh http://127.0.0.1:9090 20 180

# 3. the block profile, now armed by evaluate()
curl -s 'http://127.0.0.1:9090/debug/pprof/block?seconds=20&debug=1' | head -40
curl -s http://127.0.0.1:9090/api/v1/status | jq '.lock_attribution, .block_profile'
```
