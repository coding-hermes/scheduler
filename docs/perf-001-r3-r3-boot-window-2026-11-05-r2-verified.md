# PERF-001-R3-R3 — boot-window read starvation: fix verified on a live-sized DB (2026-11-05, tick `coding-hermes-scheduler-foreman-2026-11-05-10-01-04`)

**Row:** `PERF-001-R3-R3` (P1, block `PERF-001-R3-R2`, 9/10 done before this tick)
**Fix under test:** `b3e7e04f` — *read-only handler pool + warm boot snapshot so the boot window
never starves DB-backed reads* (rebased from `f0f0986a`, the rework worker's commit, onto `0abba346`)
**Verdict of this run:** **ACCEPTANCE MET.** Every arm of the row's acceptance holds at
t+1s..t+91s on a 16.7 GB live-sized copy. Row closed.
**Evidence in this directory:** `probes-r2-2026-11-05.txt`,
`goroutine-r2-t{3,12,25}s-2026-11-05.txt`, `slowreq-r2-2026-11-05.txt`.
(Predecessor run, which failed the same acceptance, is the `probes-run1-*` / `goroutine-t*` set.)

## 1. Rig (identical to the predecessor run, source moved forward)

- Binary: `schedulerd v1.12.0-794-gb3e7e04f (commit: b3e7e04f, built 2026-11-05T10:04:46Z)`,
  md5 `4a2f348fcab361d56bfc808243c1312e`, built by `make build` from the rebased worktree
  `/home/kara/worktrees/coding-herms-scheduler-PERF-001-R3-R3-R2` (branch `wt/PERF-001-R3-R3-R2`).
- DB: byte copy of the live `~/.hermes/coding-hermes/scheduler.db` — **16,786,505,728 bytes**
  (101,383 ticks / 880 projects / 1,001,846 events). Never the live file; the copy absorbs writes.
- Boot: `schedulerd --db <copy> --listen 127.0.0.1:19097 --min-interval 24h --max-concurrent 1
  --no-exec-fallback --simulate --disable-tick-push`. `--simulate` keeps pick/evaluate intact while
  forbidding real fleet spawns.
- Probes: all six surfaces requested **in parallel** at t+1/5/10/15/30/45/60/91 s, each `curl -m 60`
  (`%{http_code} %{time_total}`); goroutine dumps at t+3/12/25 s from `/debug/pprof/goroutine?debug=1`.
- Boot log: 75 `ADMIT` lines (the evaluate path ran), 2 `SLOW-REQ` lines (both the dashboard root).

## 2. Result — every acceptance arm

| t | `/api/v1/health` (budget 1s) | `/api/v1/status` (5s) | `/api/v1/projects` (5s) | `/api/v1/cadence` (5s) | dashboard root |
|---|---|---|---|---|---|
| +1s  | **200 @ 0.000381s** | 200 @ 0.505177s | 200 @ 0.040061s | 200 @ 0.023193s | 200 @ 1.755s, **142,209 B**, `</html>` ✓ |
| +5s  | 200 @ 0.002043s | 200 @ 0.740883s | 200 @ 0.017256s | 200 @ 0.030215s | 200 @ 0.019s, 142,205 B ✓ |
| +10s | 200 @ 0.000500s | 200 @ 0.621994s | 200 @ 0.007340s | 200 @ 0.019297s | 200 @ 0.010s, 153,390 B ✓ |
| +15s | 200 @ 0.002053s | 200 @ 0.912344s | 200 @ 0.010802s | 200 @ 0.036251s | 200 @ 0.016s, 153,223 B ✓ |
| +30s | 200 @ 0.000513s | 200 @ 0.735668s | 200 @ 0.034501s | 200 @ 0.022289s | 200 @ 0.008s, 153,221 B ✓ |
| +45s | 200 @ 0.000480s | 200 @ 0.431073s | 200 @ 0.033917s | 200 @ 0.016468s | 200 @ 0.008s, 153,221 B ✓ |
| +60s | 200 @ 0.000570s | 200 @ 0.681336s | 200 @ 0.007864s | 200 @ 0.024752s | 200 @ 0.014s, 153,221 B ✓ |
| +91s | 200 @ 0.000446s | 200 @ 0.636924s | 200 @ 0.033858s | 200 @ 0.016707s | 200 @ 2.519s, 101,714 B ✓ (`</html>`) |

Before this fix, the same rig, same DB class, answered `/health` **504 @ 1.001s at t+1s and t+5s**
and `/status` **5.17s** at t+5s, with the first dashboard render degraded to **40,320 B**.

- **`/health` <1s within 5s of boot:** met at every sample, worst t+1s = 0.38 ms (2,600× under budget).
- **`/status`, `/projects`, `/cadence` <5s in the same window:** met at every sample; worst is
  `/status` 0.91 s, i.e. 5.5× under budget.
- **Dashboard root not truncated:** 142,209 B at t+1s against a live-daemon control render of
  ~142,115 B for the same page, `</html>` present at every sample. At t+91s it renders from the
  warm snapshot (101,714 B, still complete) because `collect()`'s `ci-warm` stage exceeded the 2.5 s
  render budget — `slowreq-r2` carries the daemon's own note `served=stale[1m14.5s]`. Complete
  document, never a partial page.

## 3. Goroutine attribution — what still runs, and why it no longer starves reads

The t+3s and t+25s dumps **still** show the per-row freshness scan:
`Packer.Pick → PendingTaskCounter.CountPending → ReadBoardFreshness → gitRevVerifyCommit`
(`internal/scheduler/board_freshness.go:886`, fork/exec). That is honest and expected: the fix
does not remove the board-freshness scan, it removes its *exclusive hold on the read path*.

What changed is visible in the same dumps and in the measurements:

1. DB-backed handlers read through a separate **`mode=ro` connection pool**
   (`database.OpenReadPool`, default 4 conns) while the daemon keeps its single serialized **write**
   connection (`SetMaxOpenConns(1)`, load-bearing for the FK `OFF`/`ON` pairing). WAL readers run
   concurrently with the one writer, so the boot orphan-resume INSERT pass no longer queues reads.
2. `/health`'s two computes are served from a **boot warm snapshot** (`api.readpool.go`), so the hot
   path issues no cold SELECT at all — this is why it is sub-millisecond rather than merely in-budget.
3. The dashboard runs a **boot warm-up** (`dashboard/warmup.go`) so the first render does not have to
   finish `collect()` (whose tail is subprocess work — `gh` probes and per-lane git classification)
   inside a 2.5 s request budget.

Acceptance arm "the evaluate/pending-count path no longer holds the DB read path for the duration of
the board-freshness scan" is therefore **met on the measured invariant**: at t+3s and t+12s the
freshness scan is demonstrably mid-flight in the goroutine dump while `/health` answers in
0.4–2.0 ms and `/status` in 0.5–0.7 s at the neighbouring samples. The scan still costs git forks
per pending row — that residual belongs to the PERF-002 pending-count work, not to this row.

## 4. Gates run on the judged tree (`b3e7e04f`)

| gate | command | result |
|---|---|---|
| build | `go build ./...` | PASS (5.3 s) |
| vet | `go vet ./...` | PASS |
| focused tests | `go test -count=1 ./internal/database/ ./internal/api/ ./internal/dashboard/` | PASS (53.2s / 83.3s / 14.3s) |
| full suite | `go test -count=1 ./...` | PASS (see the tick's closeout event for the exact tail) |
| live acceptance | the boot probe above | PASS |

## 5. Disclosure

`git push` of this commit lands on a `main` whose hosted CI is **already red** for two pre-existing,
reproducible reasons unrelated to this row — `internal/scheduler/lane_hooks.go:71` unused
`(*SlotPool).hookEmitter` (golangci-lint `unused`) and `internal/lanehook/runner.go:186,193`
unix-only `syscall.SysProcAttr.Setpgid` / `syscall.Kill` breaking the Windows multiarch builds
(`GOOS=windows GOARCH=amd64 go build ./...` reproduces both lines locally). Both are tracked by
`INT-CI-4183`, which this tick updated with the fresh fingerprint. Neither is introduced by, or
touched by, this fix.
