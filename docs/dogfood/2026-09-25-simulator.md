# Dogfood: the simulator surface (SCHED-GAP-169) — 2026-09-25 ~20:15-20:40Z

Tick: `coding-hermes-scheduler-dogfood-2026-09-25-20-15-09`, binary `bin/schedulerd` =
v1.5.0-92-g63dfbd10 (built 2026-09-25T01:28:07Z), repo HEAD b8291bf3. Third dogfood pass
on this project today — angle rotation: the two earlier runs covered the REST/MCP control
surface and the dashboard info surface at 496 lanes; nobody had ever USED the
clock/simulator machinery documented in AGENTS.md, `docs/reference/clock-modes.md`, and
the `--simulate`/`--sim-*` flags.

## Promise under test

"An operator can safely rehearse fleet behavior via the documented simulator:
`--simulate` (dry-run, no real spawning), `--sim-setup/--sim-ticks` (13-project fixture +
N ticks), `--sim-count N` (bulk ticks, then exit), and `SCHEDULER_TIME_*` env vars
(simulated clock) — with fail-closed safety: the sim clock is refused without `--simulate`,
unparseable env values refuse boot, and every boot logs its clock."

## Verdict: ✅ SHIPPABLE (engine) / 🟡 PROMISING-BUT-ROUGH (reporting + docs)

The core machinery does exactly what the docs claim, and the safety contract is the best
error-message surface in the repo (6/6 refusals, every one names the offending value).
What falls short is the report the user reads at the end, and four undocumented footguns
with real-fleet-blast-radius potential.

## What was exercised, verbatim results

### Leg A — safety contract (no DB touched; process FATALs at the clock seam)

| Probe | Result |
|---|---|
| `SCHEDULER_TIME_MODE=sim` without `--simulate` | exit 1, `FATAL: SCHEDULER_TIME_MODE=sim (sim (scale=1 …)) requires --simulate — refusing to run the real fleet on a simulated clock` |
| `SCHEDULER_TIME_MODE=bogus` | exit 1, `want "real" or "sim"` |
| `SCHEDULER_TIME_SCALE=0` | exit 1, `must be a positive finite multiplier` |
| `SCHEDULER_TIME_SCALE=-5` | exit 1, same |
| `SCHEDULER_TIME_START=not-a-time` | exit 1, `want RFC3339: parsing time …` |
| `SCHEDULER_TIME_AUTOADVANCE=maybe` | exit 1, `want 0/1 or true/false` |

Boot line matches the doc exactly: `TIME: clock sim (scale=1000 start=2026-09-25T00:00:00Z auto=false driven=true)`.

### Leg B — the documented rehearsal, real clock

`./bin/schedulerd --simulate --sim-setup --sim-ticks 10 --db /tmp/dg3/b-cold.db --listen 127.0.0.1:0 --log-file ""`

- **32.2s cold / 32.3s warm, 0.10s CPU** — 10 ticks × 3s fixture-cooldown sleeps on the
  real clock. "Fast-forward" it is not, unless you know the env vars (Leg D).
- Report: 90 spawned / 66 completed / 1 failed / 5 timeout, "success 73.3%".
  DB at exit: **82 completed / 1 failed / 7 timeout = 91% resolved-success**.
  The printed success rate is a mid-flight snapshot labeled as a final verdict.
- Report header says `Budget: 100 | Max concurrent: 8`; per-tick lines show
  `budget=115/100 … 200/100` and the log says `PACKER: max concurrency reached (10)`.
  Both header numbers are hardcoded (`sim_fixture.go:175-176`) — 8 is not even the
  engine default (10) and never the observed behavior.
- Fixture wipe verified as designed: 13 fixture projects, ticks table fresh.

### Leg D — sim clock (the flagship claim)

Same command + `SCHEDULER_TIME_MODE=sim SCHEDULER_TIME_SCALE=1000 SCHEDULER_TIME_START=2026-09-25T00:00:00Z`:

- **0.20s real, 0.08s CPU — 161x faster than the same rehearsal on the real clock.**
  AGENTS.md's "~0.2s for hour-scale scenarios" claim VERIFIES on the real flag path.
- DB at exit: 85/85 ticks resolved (77/4/4), zero stuck. The DB story is clean.
- But the printed report said **24.7% success (21 completed)** — same snapshot artifact,
  more extreme because virtual completion outruns the print.

### Leg E/G — `--sim-count` bulk mode

- 16 ticks on real clock: 2.03s, exit 0, all recorded (additive — bulk does NOT wipe).
- Fresh DB + `--sim-count 4`: exit 1 `no enabled projects for simulation` — correct
  fast-fail, but the precondition (needs an existing fleet/fixture DB) is undocumented.
- **2000 ticks on the real clock: FATAL `simulation: context deadline exceeded` at
  exactly 30s.** `RunBulkSim` (loop.go:492) generates ≤8 ticks per 500ms ticker fire
  ≈ 480 ticks max, and the 30s `context.WithTimeout` (main.go:674) is a hard ceiling.
  The error does not say either number.
- 2000 ticks under `SCHEDULER_TIME_SCALE=1000`: **exit 0 in 2.11s** — the sim clock
  fixes bulk mode too, but nothing in the docs connects those dots.

### Leg C — plain `--simulate` daemon (dry-run mode)

Boot on scratch DB, dead DuckBrain URL, free port; drove the live API:

- `/api/v1/health` 200 in ms; evaluate answered `mutations disabled: no operator
  credential configured` (503, 10ms) — SCHED-GAP-1602's fail-closed auth re-proven.
- Spawn gating works: log says `LOOP: starting simulated (success=85%)`; `--simulate`
  gates only spawns (loop.go:867) — eval loop, adaptive cooldown, alert escalation,
  delivery all stay real.
- DuckBrain sync started at boot against the dead URL: 6 writes spooled for replay
  (`SYNC: spooled … for replay after failure`), `sync_spool=6` rows in the scratch DB.
  The documented fallback-recovery path works under real use; real memory untouched.
- **Sim invisibility finding:** neither `/api/v1/health` nor `/api/v1/status` exposes
  ANY sim/mode marker (checked keys; status has duckbrain/gateway/auth state, no mode).
  The only sim evidence is the process's own stdout. A dashboard or orchestrator
  pointed at a sim daemon cannot tell.

## Performance (Step 2b, PERF-007 → board row SCHED-GAP-1629)

Headline operation = the documented 10-tick rehearsal (`--sim-setup --sim-ticks 10`):

- real clock: **32.2s cold / 32.3s warm** (mean of `time -p`, 0.10s user CPU — sleep-bound)
- sim clock 1000x: **0.20s cold** (0.08s user CPU)
- `--sim-count` 2000: real clock = 30s FATAL; sim clock = 2.11s, exit 0
- e2e test claim: `go test -short -p 1 -count=1 -run TestClockSimulator ./internal/scheduler/`
  = 0.266s (claim "~0.2s" verified; 4.77s wall incl. compile+module resolution)

A user following only README/flags.md gets the 32s version and never learns the 0.2s
version exists. One profile justified: the 0.1s CPU vs 32s wall gap needs no flamegraph —
it is fixture-cooldown sleeps on the real clock (loop tick advancement, sim_runner.go),
which the sim clock collapses by construction.

## Hazard map (all code-verified before running anything)

1. **Default `--db` IS the live fleet DB** (`$HOME/.hermes/coding-hermes/scheduler.db`,
   main.go:36) and default `--listen` IS the live port (127.0.0.1:9090, main.go:37).
   Every sim invocation MUST pass explicit `--db <scratch>` + `--listen 127.0.0.1:0`.
2. **`SimFixture.Setup` WIPES `ticks` and `projects` tables** (one transaction,
   sim_fixture.go:60-82). `--sim-setup` against the default DB would erase the fleet
   registry. flags.md describes the flag as "Create test fixture with 13 dry-run
   projects" — no wipe warning, no blast radius.
3. **Default `--log-file` IS the live fleet's scheduler.log** (main.go:98) — sim runs
   append sim lines to the production log unless `--log-file ""`.
4. **DuckBrain sync targets real memory** (`http://localhost:3000`, ns `scheduler`)
   and fires `syncOnce` immediately at start (duckbrain.go:163). In `--sim-setup`
   mode the daemon returns before the syncer is created (no exposure); plain
   `--simulate` daemons DO sync — a sim fleet summary would be pushed to real
   DuckBrain memory. Keep sim daemons on `--duckbrain-url http://127.0.0.1:1` or
   short-lived.

## Friction list

1. Report-vs-DB disagreement (73.3% printed vs 91% in DB; 24.7% vs 90.6% on sim clock).
2. Hardcoded `Budget: 100 | Max concurrent: 8` header (8 ≠ observed 10, ≠ default).
3. "32.2s real time" label on the sim-clock report's elapsed (it is VIRTUAL time).
4. Bulk-sim 30s/480-tick ceiling: opaque error, undocumented limits.
5. No sim-mode marker in /health or /status JSON.
6. flags.md `--simulate` description says only "dry-run"; the sim-clock coupling,
   the DB wipe, and the safe-invocation recipe live in three other files.

## The safe recipe (what the README should say)

```
# Rehearse 10 evaluation cycles on the 13-project fixture, ~0.2s, zero prod exposure:
SCHEDULER_TIME_MODE=sim SCHEDULER_TIME_SCALE=1000 \
  ./bin/schedulerd --simulate --sim-setup --sim-ticks 10 \
  --db /tmp/sched-sim.db --listen 127.0.0.1:0 --log-file "" \
  --duckbrain-url http://127.0.0.1:1
```

Never omit `--db` (default = live fleet DB) or `--listen` (default = live port).
`--sim-setup` wipes ticks+projects on whatever DB you pass it — pass it a scratch one.

## Evidence

- Scratch artifacts: `~/.hermes/stand-in/dogfood/coding-hermes-scheduler/scratch/`
  (`legA1..6_*.log`, `legB_{cold,warm}.log`, `legD_simclock.log`, `legE_*.log`,
  `legC_{daemon.log,status.json}`, `legG_*.log`, `legF_e2etest.log`, `README-dg3.md`).
- Scratch DBs: `/tmp/dg3/*.db` (b-cold, b-warm, d-simclock, c, e-seed, e-seed2, g-fresh).
- Board rows filed: SCHED-GAP-1629..1633 (commit referenced in dogfood-log.md).
- Install leg: SKIPPED-install-bunker this tick (las-03 ssh timeout, las-04 ssh timeout,
  las-02 bunkerd crash-looping exit 1 since 13:38 PDT) — the fresh-install cell itself
  PASSED this morning on las-03 (agent 6bd25844, SCHED-GAP-1628); the delta this tick
  would have added (simulator smoke on bare metal) is filed as SCHED-GAP-1633.
