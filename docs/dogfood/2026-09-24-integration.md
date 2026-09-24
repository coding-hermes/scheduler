# Dogfood Integration Report — 2026-09-24 — htmx dashboard + status surface (real use + perf)

Run: coding-hermes-scheduler-dogfood tick 2026-09-24-01-05-44. Prior runs swept
fresh-boot (2026-09-19) and the groups/templates deploy API (2026-09-23); this run took
the untouched surface: the README's flagship UI — the live htmx dashboard — used the way
an operator uses it, with the perf measurement folded in.

## The promise tested

README §Dashboard: "Live HTML dashboard at http://127.0.0.1:9090/ — htmx-powered live
updates: fleet overview and health panel every 10s, queue and tick history every 30s";
README §Overview: "You can monitor, pause, or adjust any project through the dashboard,
REST API, or MCP tools"; README §Getting Started step 6: verify with
`curl http://127.0.0.1:9090/api/v1/health` and `/api/v1/status`.

## What a real session looks like (measured, HEAD 8f9811b8f270, board 366 rows / 289 enabled lanes)

| Operation | Result |
|---|---|
| `GET /` cold (first visit) | 45.9s, 355KB, HTTP 200 — then 44.8s AGAIN after the ~60s cache expiry: the flagship page costs ~45s on every cache cycle |
| `GET /` warm | 2.196s ± 0.035 (hyperfine n=8) |
| `GET /dashboard/partial` (refetched every 10s by htmx) | 317KB, warm 4.052s ± 7.782, range 2.2–37.1s (n=20) — a user with the tab open pays this continuously |
| `GET /api/v1/status` (README verify step) | 94.7s (first try timed out at 90s) |
| `GET /api/v1/health` (README verify step) | 26.4s |
| Real Chrome load of `/` | ~49s wall to first rendered 621-row page (headless trace) |
| `GET /projects/<name>` | 0.17s — good |
| `GET /queue`, `/ticks`, `/api/v1/projects`, `/api/v1/events` | 4.5ms / 0.2s / 0.14s / 0.8ms — good |
| `/api/v1/events/stream` (SSE) | streams immediately — good |

Data correctness: dashboard table values cross-checked against `/api/v1/projects` for the
9 scheduler-family lanes — enabled flags and last-tick times agree.

Read-only by design?: the dashboard has NO pause/resume/adjust controls (only read tables,
one htmx refresh). README's "monitor, pause, or adjust any project through the dashboard"
is not something the current UI can do — filed as part of SCHED-GAP-1576.

## Why it is slow (mechanism, proven not guessed)

1. **One mutex serializes everything.** `Loop.evaluate()` holds the Loop write-mutex for
   the whole pass (internal/scheduler/tick_process.go:19). `ForceEvaluate()` is
   `go l.evaluate()` with no coalescing (internal/scheduler/loop.go:682) — every board
   wake, every API-triggered evaluate spawns another goroutine that queues on the lock.
2. **Handlers join the same convoy.** `GET /api/v1/status` →
   `Loop.GatewayResponseTimeout()` takes the full WRITE lock to READ one value
   (loop.go:419–422). Dashboard pages read the same status data. Goroutine dump
   (`/debug/pprof/goroutine?debug=2`) under load: ~180 goroutines in `Loop.evaluate`
   blocked on the mutex with ages 28–80 MINUTES, plus 2 handlers stuck at loop.go:422.
3. **Renders are expensive.** 14s CPU profile under concurrent `/` load (72.4% busy):
   dashboard `enrichProjects` 34.4%, `Generator.collect` 28.2%, `readGitReins` 21.7%
   (walks every project workdir's `.gitreins/history` on each cache-cold render),
   `evaluate → Pack → ReadBoardFreshness` 12.3%. Artifacts: `cpu_profile.svg`,
   `pprof_top_cum.txt`, `pprof_top_flat.txt` (this run's evidence pack; the daemon also
   self-serves `/debug/pprof/*`).
4. **The daemon knows and only warns.** `WARN: goroutine count = 191–202 (threshold: 100)`
   has fired continuously at HEAD; the count stays ~200 (never drains). The full wedge
   history is SCHED-GAP-1575 (2026-09-23 outage: API fully dark 2h24m); this run updated
   that row with the proven mechanism.

Payload shape: 621 `<tr>` rows / 355KB rendered from 480 lanes (289 enabled) — the page
renders every lane; `/ticks` paginates and answers in 0.2s, the projects table does not.

## Board rows filed (committed on the board)

- **SCHED-GAP-1575** (updated, P1, existing) — wedge mechanism narrowed: unbounded
  `go evaluate()` + whole-pass write lock + write-lock read in `GatewayResponseTimeout`.
- **SCHED-GAP-1576** (new, P1) — the perf numbers above + payload size + fix directions
  (paginate/lazy-render, serve cached partial, background readGitReins, 1575 lock fixes).
- **SCHED-GAP-1577** (new, P2) — `TestCountPending_MtimeReread` fails on a clean machine
  (fresh-install battery) while passing on the dev box.

## Fresh-install battery (bunker-qa, bunker-las-02, agent 671e3584, destroyed after)

| Cell | Result |
|---|---|
| toolchain-bootstrap (go 1.26.5, zig cc, make, compose) | OK |
| fresh-install (clone → build) | OK |
| ci-pass | FAIL — `TestCountPending_MtimeReread` (passes on dev box → machine-dependent test, SCHED-GAP-1577) |
| upgrade (v1.3.0 → HEAD) | OK |
| chaos-disconnect | OK (fails fast) |
| chaos-corruption (truncated scheduler.db) | OK (next start unaffected) |
| chaos-resource (3GiB cap) | FAIL — suite rc=1 under memory cap (noted in SCHED-GAP-1577) |
| docker-deploy / chaos-shutdown | N/A (no compose file) |
| ui-probe | INFO — server ran 20s, none of the probed ports answered (3000/5173/8000/8080/8081/8766/8767/9000); schedulerd binds :9090, which is not on the probe list |

Install leg verdict: RAN (no SKIPPED row needed). Install from scratch works; the
clean-machine test failure is the one hard defect.

## Verdict

🟡 PROMISING-BUT-ROUGH. The scheduler's API internals, per-project pages, queue/ticks
views, SSE, MCP and persistence are solid and fast; the flagship operator surface
(dashboard root + status endpoints) is user-blocking-slow (~45s every cache cycle) with a
proven, named mechanism, and the README over-promises dashboard control actions.
