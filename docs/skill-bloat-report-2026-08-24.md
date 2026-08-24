# Fleet Skill-Bloat Report — 2026-08-24 (SCHED-GAP-067)

> Worker task: fleet skill-bloat audit + fresh-context management.
> Policy: `docs/skill-size-policy.md`. Parent spec: `docs/fleet-cost-governance.md` §5.
> Audits: `/tmp/skill_audit_start474.json` (baseline 05:00) → `/tmp/skill_audit_final.json`.

## 1. Audit summary

| Metric | Baseline (05:00) | After split | Final |
|---|---|---|---|
| Total SKILL.md files under `~/.hermes/skills` | 847 | — | 847 |
| >50 KiB (trim) | 97 | 82 | **78** |
| >100 KiB (emergency) | 2 | 2 | **2** (both non-fleet) |
| Fleet skills >100 KiB | 13 | 0 | **0** |

19 fleet skills were split, moving **1,238,437 bytes (1.24 MB ≈ 310K tokens)**
of inline corpora into `references/` files. Combined inline size of the 19:
1,909,792 B → 671,355 B. No content deleted — every moved byte verified
present in its target reference file (mechanical no-loss invariant, §5).

## 2. Fixed (all 19, each now ≤50 KiB, frontmatter intact)

| Skill | Before | After | Moved to references/ |
|---|---|---|---|
| coding-hermes/coding-hermes-scheduler | 100,905 | **33,515** | troubleshooting.md, fleet-wide-project-audit.md, blackout-slowdown-hours.md, fleet-toml.md; inline section appended to existing foreman-memory-optimization.md |
| coding-hermes/coding-hermes-scheduler-foreman-ops | 100,804 | **30,213** | tick-notes-2026-08.md (per-tick probe corrections #333–#371, ~71KB) |
| coding-hermes/muster-foreman-ops | 101,454 | **40,369** | standing-blocks.md, pitfalls.md |
| coding-hermes/imhotep-foreman-ops | 101,387 | **18,227** | tick-flow-t101-t106.md, productive-code-ticks.md, pitfalls.md |
| coding-hermes/inference-estimator-foreman-ops | 101,293 | **42,018** | e2e-001-battery.md |
| coding-hermes/h3-sdk-python-foreman-ops | 101,193 | **33,437** | tick-cadence.md |
| coding-hermes/dexdat-core-foreman-ops | 101,170 | **36,713** | tick-learnings-2026-08.md (#220–#249) |
| coding-hermes/asce-foreman-ops | 101,064 | **44,717** | pitfalls.md (~56KB) |
| coding-hermes/terminal-jail-foreman-ops | 100,946 | **40,518** | task-queue-tick-251.md, tick-notes.md |
| coding-hermes/h3-sdk-typescript-foreman-ops | 100,904 | **38,276** | tick-cadence.md |
| coding-hermes/hermes-dagger-foreman-ops | 100,875 | **38,742** | e2e-001-battery.md, pitfalls.md |
| coding-hermes/9router-foreman-ops | 100,822 | **44,066** | RECURRED/DISCIPLINE paragraphs (ticks 133–171) appended to declared 9router-tick-ledger-2026-08.md; stewardship-pitfalls.md |
| coding-hermes/helios-foreman-ops | 100,647 | **36,705** | pitfalls.md; prior-state digest appended to existing tick-history-2026-08.md |
| coding-hermes/kobayashi-maru-foreman-ops | 100,864* | **46,972** | tick-learnings-inline-2026-08.md (inline ticks 247–291) |
| coding-hermes-supervisor | 100,924 | **4,222** | supervisor-loop.md (full 97.9KB runbook); SKILL.md keeps an authored phase map + pointer |
| database/coding-hermes-jsonl-board-append | 100,833 | **30,308** | pitfalls.md, support-files.md |
| software-development/coding-hermes-cron | 100,812 | **25,494** | pitfalls.md, troubleshooting.md, changelog.md |
| coding-hermes/mafia-ai-benchmark-foreman-ops | 97,066 | **47,194** | worker-dispatch-tick-107.md, tick-closures-2026-08.md, fixture-cadence-state.md |
| coding-hermes/h3-sdk-go-foreman-ops | 96,093 | **38,852** | tick-cadence.md, pitfalls.md |

\* kobayashi-maru grew +263 B between baseline and split (concurrent agent
edit outside the moved ranges; expect-guards passed, no-loss check verified).

Kept inline everywhere: frontmatter, identity/mission, board tooling,
live-state probes, gate batteries, deployment/pin rules, shipped-feature
contracts — plus a dated pointer section naming each new reference file.

## 3. Remaining fleet skills >50 KiB (17) — NOT fixed this session

All are below the >100K emergency line and outside the dispatched priority
list (scheduler pair + the ~100KB foreman-ops cluster + the two >100K
fleet-infra skills). They are next-sweep candidates; the same splitter +
plan pattern applies unchanged.

| Bytes | Skill |
|---|---|
| 91,251 | coding-hermes/musterflow-foreman-ops |
| 90,755 | coding-hermes/ring-runner-foreman-ops |
| 89,708 | coding-hermes/sibling-tick-collision-recovery |
| 85,342 | coding-hermes/escalation-doctrine-foreman-ops |
| 83,441 | coding-hermes/rethinkdb-foreman-ops |
| 78,836 | coding-hermes/off-by-one-foreman-ops |
| 70,985 | coding-hermes/dexdat-memory-foreman-ops |
| 69,404 | coding-hermes/warpfs-foreman-ops |
| 66,932 | coding-hermes/rabbit-hole-foreman-ops |
| 61,340 | coding-hermes/heading-foreman-ops |
| 60,074 | coding-hermes/wojons-mythos-foreman-ops |
| 58,705 | coding-hermes/h3-shim-foreman-ops |
| 58,100 | coding-hermes/helix-foreman-ops |
| 57,771 | coding-hermes/speclang-foreman-ops |
| 57,620 | coding-hermes/crier-foreman-ops |
| 57,556 | coding-hermes-north-star |
| 52,436 | coding-hermes-self-heal/coding-hermes-self-heal |

## 4. Non-fleet skills >50 KiB (61) — out of scope, follow-up candidates only

Untouched per task scope (no fleet edits). The two >100K emergencies are both
here: **research/research-paper-writing (103,656 B)** and
**development/hivemind (103,288 B)**. Next-largest candidates:
data/git-tracked-jsonl-editing (101,049), note-taking/relationship-offer-framework
(101,021), autonomous-ai-agents/hermes-axiom-cron-loop (100,988),
devops/duckbrain-http-access (100,960), software-development/off-by-one
(100,958), deepseek-dashboard (100,954), devops/fleet-scheduler-http-api
(100,953), note-taking/whatsapp-relationship-analysis (100,951),
creative/frontiers-ghost (100,898), security/secret-scan (100,887),
software-development/hermes-dagger (100,864). Full list in
`/tmp/skill_audit_final.json`.

## 5. Method + verification

- Splitter: `/tmp/skill_split_474.py` + plans `/tmp/skill_plan_474{,b,c}.json`.
  Byte-exact line-range moves; never touches frontmatter; refuses to run when
  an expected section header is not at the expected line (concurrent-edit
  guard); aborts the run on the first failure without writing that SKILL.md.
- No-loss invariant (checked per skill before write): removing the inserted
  pointer block from the new SKILL.md reproduces the exact kept original
  content, and every moved range appears verbatim in its refs file.
- Post-split checks (all 19 pass): size ≤51,200 B; valid `---` frontmatter
  with `name:` in first 6 lines; every `references/X.md` named in SKILL.md
  exists and is non-empty; no literal `[truncated]` anywhere;
  `hermes skills list` still lists the skills (spot-checked muster-foreman-ops).
- Pre-existing finding (NOT introduced by this split — pointers already
  dangling in kept content, files missing before 05:00 baseline):
  muster-foreman-ops → idle-parallel-fire-muster-t142.md,
  gitreins-task-create-*.md (backtick-quoting note), scheduler-idle-light-tick-recipe.md;
  inference-estimator-foreman-ops → inference-estimator-focused-sync-domain-layout.md;
  dexdat-core-foreman-ops → dexdat-core-foreman-ops.md;
  asce-foreman-ops → asce-foreman-ops.md;
  terminal-jail-foreman-ops → judge-tier1-lint-whole-repo-strict.md;
  helios-foreman-ops → helios-work-project.md;
  kobayashi-maru-foreman-ops → orphaned-chain-closeout.md.
  Several look like renames (e.g. muster references/duplicate-fire-t142.md
  exists on disk). Recommend a map-pointer repair pass as a follow-up row.
- Known cosmetic note: 9router's ledger (`references/9router-tick-ledger-2026-08.md`)
  is now ~241KB. It is a pure append-only archive, never loaded whole by
  design — but a future sweep could shard it by month.

## 6. Repo changes from this task

- `docs/skill-size-policy.md` (new) — size tiers, two-tier CORE/DATA rule,
  move-never-delete, fresh-context/cache-miss doctrine, audit+split runbook.
- `docs/fleet-cost-governance.md` §5 — link to the policy + this report.
- `docs/skill-bloat-report-2026-08-24.md` (this file).

No Go code, board files, `.gitreins`, `scheduler.db`, or `.vfs` touched.
Skill edits live under `~/.hermes/skills/` (outside this repo) and are not
part of the commit.
