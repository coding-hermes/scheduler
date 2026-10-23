# Dogfood — ops scripts + dashboard as an operator (2026-10-23)

Tick: `coding-hermes-scheduler-dogfood-2026-10-23-16-25-03`
Target: `/home/kara/coding-hermes-scheduler/coding-herms-scheduler` (HEAD `65159dd7`)
Live daemon: `8ec55a6f` built 2026-10-22T13:14:01Z, uptime 27h, `:9090` (read-only GETs only)
Lane contract: read-only — no source, no scheduler state, no fleet config, no board mutation (see Footprint).

## Promise (null hypothesis)

> An operator can verify this fleet's live configuration and keep its shared-substrate findings
> consolidated by running the documented `ops/` commands, then read the same state on the dashboard.

Both halves are documented in-repo (`ops/README.md`, 216 lines, new in `99b3e8b6`/`SCHED-GAP-128`;
`docs/reference/endpoints.md` + the dashboard routes in `cmd/schedulerd/main.go`).

## What I did (real operator session, in order)

1. `python3 ops/check-fleet-invariants.py` and `--json --global-cap 10` — the documented post-deploy gate.
2. `scripts/check-invariants.sh` (the documented one-command battery) — 26.0s, PASS, and the reason
   that PASS is the most misleading output of the session (SCHED-GAP-16926).
3. `ops/fleet-infra-findings.py` — `classify` (both documented examples), `classify --list`, `stats`,
   `throttle` (inspect).
4. `ops/collapse-fleet-infra-findings.py` — documented DEFAULT (dry run).
5. `ops/fleet-infra-sweep.sh` — the documented "tick-safe wrapper".
6. Dashboard as an operator: `/`, `/projects/<lane>`, `/namespaces/<id>`, `/queue`, `/ticks`,
   `/lanes/tree`, `/cadence`, `/blocks`, `/tape`, `/observatory`, `/remote`, `/dashboard/partial`.
   Each page timed; the overview's headline numbers recomputed from the live DB.

## Step 2b — what it cost in time

| operation | cold | warm | notes |
|---|---|---|---|
| `check-fleet-invariants.py` (live, 61 boards + 648 lanes) | 0.45s | 0.45s | 832 stdout lines, exit 1 |
| `--json --global-cap 10` | 0.45s | — | same run, machine-readable |
| `fleet-infra-findings.py stats` (61 boards) | 4.63s | — | 4.56s user CPU — a full board census |
| `collapse…py` dry run (61 boards) | 1.08s | — | writes nothing |
| `fleet-infra-sweep.sh` (apply) | 1.24s | — | *wrote two boards*, see SCHED-GAP-16923 |
| `scripts/check-invariants.sh` (documented battery) | 26.00s | — | PASS: board-only clean, lint-guard 5/5, pytest 200 passed/2 skipped |
| `GET /` (dashboard root, 135KB) | **79.685s** | 5.4–6.0ms | cold wait with ~0 request CPU → PERF-001-R3-R2 |
| `/health` | 2.81s | — | first-hit |
| `/queue` (434KB), `/ticks` (95KB), `/cadence` (300KB), `/tape` (581KB), `/lanes/tree` (211KB) | 0.25 / 0.79 / 0.03 / 0.04 / 0.02s | — | steady state, nothing a user would notice |

Verdict on speed: everything except the dashboard's first paint is comfortably fast; the one wait a
real user would feel is the first `GET /` after an idle window (79.7s), filed as a recurrence.

## What worked

- **The invariant gate is honest and fast.** 0.45s over 648 enabled lanes and 61 boards; text and
  `--json` agree (791 violations in both, plus 40 INFO lines and one summary line: `FAIL — 791
  violation(s) across 19 namespaces / 648 enabled lanes`). Every violation I spot-checked against the
  DB was true: `coding-hermes max_concurrent=8 expected 16`, `qa/pm/dogfood/releases=9 expected 1`,
  `duckbrain-sync=12 expected 1`, `hermes-dagger-foreman cooldown_s=3600` below the 6h floor,
  `release-engineer cooldown_s=21600` off its named tier.
- **The classifier's discriminator works.** `classify "run_battery FAIL — bunkerd port-pool exhaustion
  on bunker-las-03"` → `fleet-infra:port_pool / route=infra`; `classify "deployed binary predates HEAD
  by 12 commits: bin/schedulerd built before c17311c"` → `project-local` — the exact split the docs
  say is load-bearing, in 50ms.
- **`stats` / `throttle` behave as documented.** `boards_scanned=61, boards_with_hits=32,
  infra_rows=41, local_rows_mentioning=70` in 4.6s; `throttle` with no flag is a read-only inspect.
- **The dashboard's headline numbers are TRUE.** Overview cards vs the live DB:
  `Enabled Projects 648/845` = `select sum(enabled),count(*) from projects` → `648|845`;
  `Active Ticks 10` = `select count(*) from ticks where status='running'` → 10;
  `Fleet Weight 2505` = `select sum(weight) from projects where enabled=1` → 2505.
  No metric drift found on the cards I could recompute.
- **Every dashboard route serves.** All 13 pages I opened answered 200 with plausible content and no
  500s; the sidebar/htmx partials (`/dashboard/partial`) render in 5ms.
- **The one-command battery is fast and legible.** `bash scripts/check-invariants.sh` → 26.0s, three
  named stages, `200 passed, 2 skipped`, exit 0 — a genuinely pleasant gate to run. Which is exactly
  why its blind spot matters: see SCHED-GAP-16926.

## What broke, confused or annoyed

| # | finding | row |
|---|---|---|
| 1 | The documented "tick-safe" sweep **applies by default** and rewrote two boards — one of them another project's live queue — while printing a line that looks like status | `SCHED-GAP-16923` |
| 2 | The shared-infra board's **rollback recipe cannot restore it**: its backup lands in the backups ROOT, not `<project>/`, and carries the run's own stamp | `SCHED-GAP-16924` |
| 3 | The per-tick convergence has **no live caller** — its only invoker is the retired `qa-scheduler-tick.sh`; 3 open copies persist | `SCHED-GAP-16925` |
| 4 | Cap policy documented **three contradictory ways** (8/1, 16/1, 9/12); the gate enforces 16/1; the DB runs 8/9/12 | `DOC-24` |
| 5 | The checker class table is **three live classes short**, by the README's own rule a gap in the gate | `DOC-25` |
| 6 | Dashboard root repeats PERF-001-R3: **first `GET /` blocked 79.7s** with ~0 CPU, and the daemon logs no request at all | `PERF-001-R3-R2` |
| 7 | The documented "run it after every deploy" battery is **board-only**: it prints `BATTERY: PASS` on a host whose live fleet fails 791 violations, and its stage headers say `[1/2]` in a three-stage script | `SCHED-GAP-16926` |

Friction count: 5 in the ops-tool leg (write-by-default; unaddressable rollback; dead convergence
hook; a post-deploy gate that cannot see the fleet; docs that disagree about the cap), 1 in the
dashboard leg (cold first paint), 1 docs-table gap.

## Footprint disclosure (this run mutated two foreign boards)

`ops/fleet-infra-sweep.sh` is documented as a safe per-tick convergence wrapper, so I ran it as the
documented command — it **applied**, removing 3 open rows from
`/home/kara/totalstack/.coding-hermes/board/tasks.jsonl` (`QA-TOTALSTACK-1-R9`, `-R13`,
`QA-TOTALSTACK-2-R7`) and rewriting `FLEET-INFRA-1` on
`/home/kara/deployBunker/bunker/.coding-hermes/board/tasks.jsonl`
(`last_collapsed_at=2026-10-23T16:33:14Z`).

Both boards were restored and verified byte-exact:
- totalstack: `cp fleet-infra-backups/totalstack/tasks.jsonl.bak-20261023T163316Z <board>` →
  `git status --short .coding-hermes/` shows only the pre-existing untracked `.lock` (dated 2026-09-30).
- bunker: the tool's own backup was **post-write** (that is SCHED-GAP-16924), so the board was restored
  from `git show HEAD:.coding-hermes/board/tasks.jsonl` after proving the only difference vs HEAD was
  the single `last_collapsed_at` field; `cmp` → identical to HEAD, `git status` clean.

Nothing in this run touched scheduler state, fleet config, repo visibility or credentials; the live
daemon was only ever `GET`-ed.

## Install leg (fresh-system battery)

`bunker-qa.sh launch /home/kara/coding-hermes-scheduler/coding-herms-scheduler --keep`
- agent `c4a3a2eb` on `bunker-mvp`, launched 2026-10-23T16:38:25Z, TTL 4h
- cell `repo-head` OK: tested HEAD `65159dd7ec1aa7a52d73c1bcb9f19a69808b0c4c` (clone dirty — pull
  skipped, recorded sha)
- cell `launch` OK: battery started detached (`qa-run.sh`), 135,569 script bytes
- `collect` at 16:52Z → `RUNNING … agent left intact`; the cells land in the evidence file and are
  summarised in the next tick's entry (the battery takes 7–15 min; `bunker-qa status` showed
  `act=no`, so the `ci-pass` cell is expected to report UNVERIFIED rather than pass).

## Verdict

🟡 **PROMISING-BUT-ROUGH** for the ops surface: the gate, the classifier and the dashboard's numbers
are genuinely good — fast, honest, recomputable. The tooling *around* the collapse step is the rough
part: the "safe" wrapper writes by default, its rollback story does not cover the one board that
matters, the convergence it implements has no live caller, and the one-command gate an operator is
told to run after every deploy cannot see the fleet (it prints PASS while the same checker fails 791
violations). Two docs also contradict themselves about a value the gate fails the fleet for.

Time-to-first-success: ~2 min (first successful `check-fleet-invariants.py` run). A defensible verdict
took ~70 min, most of it reconstructing the board state after the "safe" sweep wrote to it.
