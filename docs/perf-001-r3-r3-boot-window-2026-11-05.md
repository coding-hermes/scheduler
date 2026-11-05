# PERF-001-R3-R3 — boot-window read starvation on a live-sized DB (2026-11-05 re-measurement)

**Row:** `PERF-001-R3-R3` (P1) · **Predecessor fix measured here:** `547373b8`
(`internal/scheduler/packer.go` — two-phase `Packer.Pick`)
**Verdict of this run:** the predecessor closed most of the window (90 s+ → ~10 s) but **the row's
acceptance is NOT met**. The row stays open; a rework worker owns the remainder.
**Evidence in this directory:** `probes-run1-2026-11-05.txt`, `slowreq-run1-2026-11-05.txt`,
`goroutine-t{3,12,25}s-2026-11-05.txt`.

## 1. Rig

- Binary: `dev-b4a89f0f` (sha `b4a89f0f`), built with `go build ./cmd/schedulerd` from the current tip,
  i.e. **with** the `547373b8` fix.
- DB: byte copy of the live `~/.hermes/coding-hermes/scheduler.db` — **16,781,852,672 bytes**,
  101,383 ticks / 880 projects / 1,001,846 events (the 2026-10-27 reproduction used 8.4 GB, so this is
  a stiffer instance of the same shape).
- Boot: `schedulerd --db <copy> --listen 127.0.0.1:19097 --min-interval 24h --max-concurrent 1
  --no-exec-fallback --simulate --disable-tick-push`. `--simulate` keeps the pick/evaluate path intact
  while forbidding real fleet spawns; the copy absorbs all writes.
- Probes: all six surfaces requested **in parallel** at t+1/5/10/15/30/45/60/91 s; each `curl -m 60`.
  Goroutine dumps at t+3/12/25 s via `/debug/pprof/goroutine?debug=1`.
- Control, same minutes, same host (loadavg 15.4 on 16 cores): the LIVE daemon on `:9090` answered
  `/health` 200 @0.20–0.27 s ×5 and `/status` 200 @0.52–0.73 s ×5 — the host is not the limiter.

## 2. Result

| t | /health (1 s budget) | /status (5 s) | /projects (5 s) | /cadence (5 s) | GET / |
|---|---|---|---|---|---|
| +1 s | **504 @1.001 s** | 200 @2.69 s | 200 @1.35 s | 200 @1.68 s | 200 @2.51 s **partial** 40,320 B |
| +5 s | **504 @1.001 s** | **200 @5.17 s** (over budget) | 200 @1.52 s | 200 @1.64 s | 200 @2.50 s **partial** 40,331 B |
| +10 s | 200 @1.011 s | 200 @1.79 s | 200 @0.66 s | 200 @0.64 s | 200 @0.67 s 153,439 B |
| +15 s | 200 @0.319 s | 200 @1.04 s | 200 @0.04 s | 200 @0.04 s | 200 @0.03 s 153,435 B |
| +30 s | 200 @0.821 s | 200 @1.06 s | 200 @0.84 s | 200 @0.04 s | 200 @0.04 s 153,436 B |
| +45 s | 200 @0.322 s | 200 @1.16 s | 200 @0.27 s | 200 @0.33 s | 200 @0.29 s 153,436 B |
| +60 s | 200 @0.747 s | 200 @0.96 s | 200 @0.93 s | 200 @0.74 s | 200 @0.01 s 153,436 B |
| +91 s | 200 @1.031 s | 200 @1.88 s | 200 @0.07 s | 200 @0.41 s | 200 @2.51 s **partial** 101,929 B |

All pages end with `</html>` (no short read), but `served=partial` is logged for the boot-window
requests — the dashboard deliberately renders what it collected before its 2.5 s budget expired:
a partial body is ~40 KB, a full one ~153 KB.

## 3. What still holds the read path

`internal/database/schema.go` pins `SetMaxOpenConns(1)`: **every** statement in the process
serializes on one connection. `547373b8` removed the worst offender (a SELECT cursor left open across
the per-row git freshness battery), but any boot-time writer still queues every handler read.

1. **Boot writer blocks `/health`.** The daemon's own warning names the step:

   ```
   WARN: slow request handler=health step=countActiveTicks elapsed=1s
         slowest_step=countActiveTicks slowest_step_elapsed=1s budget=1s
   SLOW-REQ: GET /api/v1/health 1s status=504 bytes=95 wait=not-instrumented
   ```

   The t+3 s goroutine dump shows the connection holder is the startup resume pass doing a **write**:
   `Loop.Run` (loop.go:767) → `resumeOrphansAtStartup` → `resumeNeedsHuman` (session_resume.go:450)
   → `EmitHighEvent` (loop.go:562) → `EventLogger.Emit` (events.go:56), while
   `countActiveTicks` waits out its budget.

2. **`/status` competes with itself and with boot work**: `handler=status step=configDriftBlock
   elapsed=5.169s slowest_step=ListProjects`, with `computeProjectFailureRates`
   (server_helpers.go:713) in the t+3 s dump and `countRecentOutcomes` (server_helpers.go:420) in the
   t+12 s dump — heavy aggregates on the one connection.

3. **The dashboard partial render is downstream of the same contention**:
   `GET / 2.507s status=200 bytes=40320 stages=db-projects[1.7s],db-lanes[519ms] served=partial
   note="collect exceeded the 2.5s render budget"`.

4. **The freshness battery is still slow, just no longer cursor-holding**: the t+12 s and t+25 s dumps
   still show `Packer.Pick` (packer.go:370) → `CountPending` → `ReadBoardFreshness` →
   `gitRevVerifyCommit` / `gitReachableFromHEAD` (board_freshness.go:613/683/886/902) — one git
   subprocess per pending row, spanning the boot window.

## 4. What the row now asks for

Make the four DB-backed read surfaces meet their budgets from the first second of boot on a
live-sized DB — read-path concurrency (WAL readers) and/or taking the boot resume pass and the
`/status` aggregates off the hot path — without weakening single-writer semantics. Acceptance and the
rig above are reproduced in the rework brief; this document is the baseline the fix is measured
against.
