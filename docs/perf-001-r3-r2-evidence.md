# PERF-001-R3-R2 — dashboard root lock-starvation: evidence

Status: **landed** (commit `815e0873`, branch `wt/PERF-001-R3-R2`).
Row: `PERF-001-R3-R2` (P2). Parent: `PERF-001-R3` (`13dbbc44`).

This is the measurement record behind the fix, kept because the row's fourth
acceptance criterion exists precisely because the original 79.7s event left no
record anywhere: `grep 'GET / ' scheduler.log` matched **nothing**.

---

## 1. What the row reported, and what we re-measured

| Surface | Row (2026-10-23T16:31Z) | Re-measured (live daemon `8ec55a6f`, uptime 27h, 2026-10-23T12:47Z) |
|---|---|---|
| first `GET /` after an idle window | 79.685s, 135,376 B | **52.127627s**, 135,294 B |
| warm `GET /` | 5–6 ms | 3.6s → 9 ms → 5 ms (first hit after the snapshot TTL) |
| `GET /health` | 2.81s | 0.176s |

The 300s CI-conclusion cache TTL is what makes this shape **deterministic
without restarting the daemon**: wait >300s (the snapshot's own 60s TTL is
irrelevant — it expires first and re-collect is cheap while the CI cache is
warm), then request `GET /`. No restart, no fleet disturbance.

Reproduction (non-destructive, live daemon):

```sh
sleep 315
curl -s -o /dev/null -w '%{http_code} %{time_total}s\n' http://127.0.0.1:9090/   # 52.1s
curl -s 'http://127.0.0.1:9090/debug/pprof/goroutine?debug=2' -o /tmp/g1.txt
```

## 2. AC1 — the holder, named (pre-fix live dump)

`GET /` (goroutine `23993863`) is parked on the **snapshot mutex**:

```
goroutine 23993863 [sync.Mutex.Lock]:
internal/sync.runtime_SemacquireMutex(...)
internal/sync.(*Mutex).lockSlow(...)
sync.(*Mutex).Lock(...)
dashboard.(*Generator).cachedOverview(_, {_, _})
        internal/dashboard/overview_cache.go:68
dashboard.(*Generator).GenerateParams(...)
        internal/dashboard/generator.go:298
main.main.func7(...)                       # the GET / handler
```

The mutex was held **across `collect()`** by a *concurrent* render — in this
capture the 10s htmx autorefresh/`fleet-board` poller hitting
`/dashboard/partial` (goroutine `23987243`) — which was inside the CI fan-out:

```
goroutine 23987243 [chan send]:
dashboard.(*Generator).warmCIConclusions(0x…, {0x…, 0x34d, …})   # 0x34d = 845 workdirs
        internal/dashboard/generator_data.go:1685                  # sem <- struct{}{}
dashboard.(*Generator).collect(_, {_, _})
        internal/dashboard/generator_data.go:776
dashboard.(*Generator).cachedOverview(_, {_, _})
        internal/dashboard/overview_cache.go:75                    # g.snap.mu held here
dashboard.(*Generator).GenerateFleetTableParams(...)
main.main.func8(...)                       # the /dashboard/partial handler
```

…and **8** goroutines were blocked in `os/exec.(*Cmd).Output` →
`warmCIConclusions.func1` (`grep -c warmCIConclusions.func1 /tmp/gc1.txt` = 8):
one `gh run list` subprocess per workdir, 8-wide, each capped at 2s.

### Same holder as PERF-001-R3? **No.**

* PERF-001-R3's dump named `Loop.ZeroSelectStats` **RLock** on `Loop.mu`.
* The dashboard root never touches `Loop.mu`. Its only scheduler-package
  dependencies are the immutable `scheduler.UrgencyCalculator` (built once from
  resolved config, no locks) and `Loop.IsPaused()` (an `atomic.Bool`). Every
  dashboard read is `g.db` (SQLite, `SetMaxOpenConns(1)`) plus file/git work.
* The waits here are (a) the **snapshot mutex** held across collect, and
  (b) the **CI subprocess fan-out** — both inside `internal/dashboard`.
  R3's fix could not have covered this surface; the hypothesis in the row that
  the dashboard "reads scheduler state through the same mutex-protected paths
  as /api/v1/status" is **false**, and this is what the dump establishes.

Secondary wait seen mid-collect (earlier capture, one overlapping render):
the handler inside `database/sql.(*Rows).Next` →
`modernc.org/sqlite … _sqlite3VdbeSorterNext` — i.e. a temp b-tree sort on the
single shared connection, still not a scheduler lock.

`/debug/pprof/mutex` remains dark (`sampling period=0`, never armed — the
`sync.Mutex` waits above are visible in `/debug/pprof/block`, which
`ArmBlockProfile` arms at the head of `evaluate()`).

## 3. The fix (4 parts)

| # | Change | Where |
|---|---|---|
| 1 | **Bounded wait.** `collect()` runs on a detached goroutine; the render waits at most `SCHEDULER_DASHBOARD_RENDER_BUDGET` (default 2500 ms) and then serves the previous snapshot (aged, `refresh-pending` notice) or a partial page naming the running stage. The collect finishes detached (`context.WithoutCancel`) and fills the snapshot for the next render. The snapshot mutex is never held across `collect()`. | `internal/dashboard/overview_cache.go` |
| 2 | **Bounded CI warm pass.** Dispatch stops at a 1s window / 256-workdir cap, staleness-ordered; the per-lane read inside collect is cache-only (`ciConclusionCached`) so no `gh` ever runs from the enrichment pool. | `generator_data.go` (`warmCIConclusionsWithin`, `ciConclusionCached`) |
| 3 | **Bounded enrichment work.** `enrichProjectsWithin` takes a work deadline; lanes past it keep their batched-DB fields and skip the O(lanes) git work (tick-work classification, GitReins walk). The window advances by what the pass **covered**, so the tail is never starved. | `generator_data.go` (`enrichProjectsWithin`, `enrichWindowStart`, `advanceEnrichRotation`) |
| 4 | **Observation.** Middleware over the whole mux logs one line per request over `SCHEDULER_SLOW_REQUEST_MS` (default 1000 ms) with path, duration, status, bytes and the render's stage breakdown. Per-request `renderTrace` records stages inside `collect()`. | `cmd/schedulerd/slowreq.go`, `internal/dashboard/render_trace.go` |

Both knobs are documented in [reference/env-vars.md](reference/env-vars.md).

## 4. AC3 — cold and warm, with the daemon's own CPU

Scratch instance: **this binary**, a `.backup` copy of the live 3.7 GB /
91,560-tick / 845-lane DB, **every lane disabled** so the only work measured is
the dashboard, `--listen 127.0.0.1:9191`, gateway + DuckBrain pointed at a dead
port (no external side effects).

```
cold GET /            2.502s  200  40320 B   partial notice
                      CPU 94 ticks (idle-burn corrected) = 938 ms
                      -> 2.29 s of the 2.50 s is WAIT, 0.21 s is compute
converge to full page +3.1 s after the cold request
warm GET / x5         6.0 / 5.9 / 5.3 / 5.5 / 4.9 ms   p50 5.5 ms  p99 5.9 ms
warm CPU/req          ~7 ms over a 100-request window  -> COMPUTE, not wait
enrich convergence    pass 1 skipped 226 lanes -> pass 2 skipped 0 (all classified)
```

`enrichSkipped` is rendered in the page (`id="enrichSkipped"`) so a truncated
pass is never silent; it reaches 0 on the next snapshot refresh.

### AC5 — steady state on the same instance

```
/queue               1.7 ms
/ticks               248 ms
/lanes/tree          15 ms
/cadence             22 ms
/tape                35 ms
/observatory         135 ms
/blocks              0.7 ms
/remote              0.8 ms
/health              132 ms
/dashboard/partial   2.8 ms
```

## 5. AC4 — the log line that did not exist before

```
SLOW-REQ: GET / 2.502s status=200 bytes=40320 wait=enrich[257ms,in-flight] \
  stages=db-projects[254ms],db-lanes[12ms],fleet-learned[223ms],ci-warm[1.4s],db-samples[391ms] \
  served=partial budget=2.5s

SLOW-REQ: GET /dashboard/partial 2.5s status=200 bytes=2 wait=await-collect[2.5s] \
  stages=await-collect[2.5s] served=partial \
  note="waiting for the overview collect already in flight" budget=2.5s
```

`wait=` names the stage that was still in flight when the render stopped
waiting (or the slowest finished stage), so "79 seconds" no longer needs a
goroutine dump to interpret. `wait=not-instrumented` is printed for surfaces
the dashboard trace does not cover (API, pprof, static) rather than implying a
clean path.

## 6. Post-fix goroutine state (same in-flight window)

Two overlapping renders on the patched binary:

```
goroutine 3833 [select]:   …cachedOverview… (budget select)     <- GET /
goroutine 4472 [select]:   …cachedOverview… (await-collect)     <- /dashboard/partial
goroutine 3835 [chan send]: warmCIConclusionsWithin…            <- the detached collect
grep -c '[sync.Mutex.Lock]'  ->  0
```

Pre-fix the same window contained a `[sync.Mutex.Lock]` request goroutine; now
the requests sit on a bounded `select` and the collect is a detached goroutine
with no lock held across it.

## 7. Tests

`internal/dashboard/perf001r3r2_test.go` (10) — bounded first render, stale
serve inside the budget, joining render does not queue behind a collect, CI
warm bound, CI coverage convergence, enrich rotation arithmetic, enrich
deadline skip, trace names the in-flight stage, collect records stages,
cache-only CI read.

`cmd/schedulerd/slowreq_test.go` (5) — slow line fields, silence under the
threshold, disable via 0, env resolution, SSE flushability through the wrapper.

`go build ./...` + `go vet ./...` green; the pre-commit guard (full mode:
secrets + build + golangci-lint + whole-module tests) PASSED on the commit.

## 8. Scope / what was NOT done

* The **live daemon was not restarted**: the cold shape was reproduced against
  it by letting the CI cache TTL lapse (52.1s) and the patched binary was
  measured on a scratch instance over a copy of the same DB. Deploying the fix
  to the live scheduler is a foreman/owner step (`13dbbc44`'s deploy pattern).
* `/ticks` (248ms) and `/observatory` (135ms) are unchanged by this row — they
  are cold-query pages, not the overview pipeline; they are listed for the
  steady-state comparison AC5 asks for.
* `PERF-001-R3`'s `evaluate()`-side `evalMu` serialization (23.7 ks of
  cumulative block-profile delay inside `evaluate` → `sync.(*Mutex).Lock`) is a
  separate finding; it is not on the dashboard root's path.
