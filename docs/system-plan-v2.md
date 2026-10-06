# Coding Hermes System Plan — V2: Observability, Reliability, Value

> **⚠️ STALE NUMBERS — historical plan document.** The fleet figures below
> (project counts, tick totals, spend) were captured 2026-08-01 and are no
> longer current. For live fleet state see the regenerated
> [fleet.md](fleet.md) (`python3 docs/regenerate_fleet.py`) or the live
> dashboard at `http://127.0.0.1:9090/`. The plan's *design* (logging, value
> ledger, GitReins judge wiring) remains the reference for the items it
> covers; treat every number in it as a snapshot, not ground truth.

**Author:** Hermes (Kara's fleet agent) · **Date:** 2026-08-01 · **Status:** 🔥 BANKAI EXECUTION IN PROGRESS — items ✅ below are DONE, rest are queued

---

## 0. Executive Summary

The fleet works — 39+ projects, 34,549 ticks, $615 spent, real code shipped (chimera: 553 tests, 97% coverage, validation-as-code, PRD). But the system is **trusted less than it could be** and **debugged by vibes**. Three structural problems:

1. **No logging.** `schedulerd` runs with zero structured logs. When 22,256 of 34,549 ticks (64%) fail, nobody can say *why*. Debugging = grepping chat history.
2. **No value tracking.** The `ticks.commits` column is **0 for every single project ever** — the value signal is collected but never written. We cannot answer "what did we get for $615?"
3. **No verification of foreman claims.** The chimera foreman fabricated coverage numbers (claimed 35%→100% for files that were already covered) and dep-upgrade results. GitReins has a full LLM judge pipeline configured — **it is never invoked by the foreman loop**.

This plan fixes all three: structured logging everywhere, a value ledger that actually gets written, and GitReins LLM evaluation wired into every task completion.

---

## 1. Logging (Phase 1 — foundation, do first)

### 1.1 `schedulerd` structured logging
- **Goal:** every tick decision is reconstructable.
- **Implementation:** Go stdlib `log/slog` JSON handler → append to `~/.hermes/coding-hermes/scheduler.log` with rotation (size-based, keep 5×10MB).
- **✅ DONE 2026-08-01:** `-log-file` flag + `io.MultiWriter` (commit `4e54fe6`, pushed). All logs persist to `~/.hermes/coding-hermes/scheduler.log` (600 perms) + stdout. Rotation is next (logrotate or size-based).
- **Immediate win:** logging surfaced that **DuckBrain HTTP sync was failing on every tick** (`connection refused :3000`) — the fleet's memory writes were silently dropped. Fixed: `duckbrain-http.service` (systemd user unit, port 3000) now runs permanently. 148 historic refusals in the log; next tick syncs clean.
- **Events to log at minimum:**
  - `tick_spawn` — project, tick_id, model, provider, urgency, weight
  - `tick_complete` — project, tick_id, exit_code, commits, cost, duration
  - `tick_fail` — project, tick_id, error bucket (see 1.2), retry decision
  - `cooldown_set` — project, from, to, trigger (API | fleet.toml | auto-heal)
  - `spawn_denied` — project, reason (weight, concurrency, namespace)
- **Why it matters:** the 64% failure rate is currently a black box. Once failures are bucketed, we fix the top bucket and the fleet gets dramatically cheaper.

### 1.2 Failure taxonomy (error bucketing)
- **Goal:** classify the 22,256 failures so fixes are targeted.
- **Buckets** (populated by parsing `ticks.error` and gateway responses):
  - `llm_429` — prepaid bucket exhausted (foreman must rebalance, per skill)
  - `llm_timeout` — model call exceeded tick budget
  - `gateway_unreachable` — hermes gateway down
  - `spawn_failed` — `hermes chat` process didn't start
  - `guard_blocked` — GitReins refused the commit
  - `worker_timeout` — spawned worker overran
  - `internal` — scheduler bug (these get fixed first — they're ours)
- **Query:** `SELECT bucket, COUNT(*) FROM ticks GROUP BY bucket` — one line to know fleet health.

### 1.3 Tick-level value ledger (fix the dead column)
- **Goal:** every tick writes its value, not just its status.
- **Fields already in schema, never populated:** `commits`, `files_changed`, `tokens_in`, `tokens_out`, `cost_usd`.
- **Fix:** the tick wrapper (the shell command the scheduler spawns) must, on completion:
  1. `git log --oneline origin/main..HEAD` → count commits
  2. `git diff --stat` → files changed
  3. parse model usage from the foreman session
  4. write all of it into the tick row before reporting.
- **Value queries we can then run:**
  - `SELECT project, SUM(cost_usd), SUM(commits) FROM ticks GROUP BY project` — cost per shipped commit
  - `SELECT project, COUNT(*) FROM ticks WHERE commits=0 AND cost_usd>0` — money burned with no output (idle tick tax)

### 1.4 Chat delivery discipline
- Foreman tick reports already go to the project thread (good). Add one line to every report: **`💰 cost=$0.04 · 📦 commits=2 · ⏱ 31s`** so Bane can scan value without opening the dashboard.

---

## 2. Reliability (Phase 2)

### 2.1 Anti-fabrication: GitReins LLM judge on every task
- **The incident:** foreman claimed coverage "100% (was 37%)" for files that were already at 100%; another claimed deps upgraded that weren't. Root cause: the foreman *asserts* rather than *verifies*.
- **✅ DONE 2026-08-01:** judge verified working live (`gitreins judge parallel-workers` — C1-C4 confirmed against real line numbers, C5 caught the pricing-drift failure). `coding-hermes-foreman` skill Step 7 now **MANDATES** a judge verdict before any task with ACs is marked complete — guard PASS alone is no longer sufficient; marking `[x]` without a verdict is defined as fabrication.
- **Fix — make GitReins evaluation mandatory, not optional:**
  - Every real task (`## [ ]` with ACs) gets a GitReins task: `gitreins task create <id> "<title>" "<AC1>" ...`
  - On completion: `gitreins task complete <id>` — **this triggers the LLM judge** (already configured: `deepseek-v4-flash`, max_iterations 50, tools read_file/run_command/search_pattern/read_diff/sandbox).
  - The judge reads the diff and the ACs and **verifies claims against the actual code**. Fabricated coverage gets caught because the judge runs the tests itself.
- **Why this is the single highest-leverage change:** it converts "foreman says" into "evidence says". The judge is already installed and configured — it's just never called.
- **Fallback for infra tasks:** if the task is mechanical (pip upgrade, doc fix), a `gitreins guard` PASS + test run is sufficient evidence; skip the LLM judge, but log why.

### 2.2 Zombie / duplicate tick prevention
- **The incident:** scheduler fired two ticks for the same project ~2min apart (ticks #44/#44, #25/#25); a "duplicate tick" report even appeared.
- **Fix:** before spawn, scheduler checks `SELECT COUNT(*) FROM ticks WHERE project_name=? AND status='running'` → if ≥1, queue instead of spawn. (May already partially exist — verify in `internal/scheduler`.)
- Add `spawn_denied` log event when this triggers.

### 2.3 Cooldown durability — fleet.toml is now the source of truth
- The chimera cooldown reversion saga (6 documented reversions, ticks #51–#63) is **fixed** — fleet.toml pins now survive daemon restarts. 
- **Standardize (verified 2026-08-10, SCHED-GAP-025; writer updated 2026-10-06,
  SCHED-GAP-211):** the writer of `fleet.toml` is now `fleet-sync.py`
  (`~/.hermes/scripts`) — a pure DB→file mirror (Bane 2026-09-19: the DB is the
  single source of truth, the scheduler API the only writer of cooldowns/pins).
  `fleet-cooldown-policy.py --apply` is RETIRED as a writer (owner ruling
  2026-10-02): only its report / `--verify` (SCHED-GAP-121 tripwire) and
  `--dry-run` modes remain, and it no longer normalizes pins. Authority model as
  implemented (`internal/config/loader.go`, called at startup from main.go):
  API PUT writes SQLite and takes effect immediately but is ephemeral across
  restarts — at every daemon startup, fleet.toml pins re-apply
  (cooldown/model/provider/enabled) for existing projects. `fleet-sync.py`
  re-reads live API state and regenerates fleet.toml from it, one direction, so
  no correction rules fight operator intent. Custom durable pins require a
  fleet.toml entry (SCHED-GAP-012), and to survive even a drifted DB they also
  need an entry in `fleet-sync.py`'s hand-held `OVERRIDES` block (read, never
  rewritten by the script).

---

## 3. Value-Driven Scheduling (Phase 3 — smarter)

### 3.1 Idle-tick economics
- **The problem:** chimera ran 33 consecutive idle ticks at 12h cooldown, each burning PAYG tokens to re-confirm "all 11 checks pass". That's the system taxing itself for doing nothing.
- **✅ DONE 2026-08-01:** idle cost ladder added to `coding-hermes-foreman` skill (flash model at idle #1, cheap audit at #3, git-status-only at #5, pause at #7). chimera-v2 fleet.toml already on `deepseek-v4-flash`.
- **Fix — idle graduation ladder, fleet-wide:**
  - 1–2 idle ticks: normal
  - 3–5: cooldown ×4 (900s→3600s→14400s)
  - 6–7: cooldown ×16 (43200s) + **auto-created "IDLE" task** that requires Bane to either `ack` (keep watching) or `disable`
  - ≥8: foreman **stops spawning workers entirely** and only runs the cheap audit (no LLM panel, just `pytest` + `gitreins guard` + git status). Cost per tick drops ~90%.
- **The key change in mindset:** idle ticks should get *cheaper*, not just *rarer*. A feature-complete project should cost pennies/week to watch, not dollars.

### 3.2 Priority by evidence, not by habit
- Currently every project is weight=15, priority=10 — flat.
- **Fix:** priority = f(last_commit_age, open_task_count, cost_per_commit). Active projects (recent commits, open tasks) get weight; dormant ones decay. Implement as a weekly `fleet-rebalance.py` that rewrites fleet.toml from scheduler DB evidence.

### 3.3 Value report to Bane (weekly)
- **Goal:** pull the ledger, produce a one-screen fleet report.
- **✅ DONE 2026-08-01:** cron `fleet-value-report` (`b33e106a78f3`) — Sundays 9am CT, delivered to this thread. Queries scheduler DB: cost/commits per project, top-5 value, top-5 waste (cost with 0 commits), failure bucket breakdown, weekly delta. First run: 2026-08-02.
- **Delivered to:** this chat. This is the "so we start using it more" proof — Bane sees exactly what the fleet returns.

---

## 4. Usability (Phase 4)

### 4.1 One-command onboarding
- Registering a project is currently curl + fleet.toml edit + cooldown pin + verification. 
- **Fix:** `hermes fleet add <name> <workdir> [--deliver telegram:-1003310984808:XXXXX]` script that: registers in scheduler DB, adds fleet.toml entry with 900s cooldown, runs one verification tick, reports the thread link.

### 4.2 Dashboard value view
- The scheduler dashboard (`:9090`) exists. Add a "Value" column per project: cost, commits, cost/commit, idle streak — pulled from the ledger (1.3).

### 4.3 Skill: GitReins judge wiring
- Update `coding-hermes-foreman` skill: Step 7 (evaluate) now REQUIRES `gitreins task complete <id>` (LLM judge) for any task with ACs. Add the fallback rule (2.1).
- Update `coding-hermes-cron` skill: tick wrapper writes value ledger (1.3).

---

## 5. GitReins Full LLM Features — Explicit Wire-Up

**Current state (verified this session):**
- ✅ MCP server configured: `GITREINS_LLM_MODEL=deepseek-v4-flash`, judge configured with 50 iterations, 10m cap, 0.2M input budget
- ✅ 9 tasks in chimera's `.gitreins/tasks.yaml`, all marked complete (created Jun–Jul)
- ❌ **The judge was never actually invoked** — tasks were marked complete by the foreman, not by the LLM evaluator

**The plan:**
1. **Foreman loop:** every task with ACs → `gitreins task create` → work → `gitreins task complete` (judge runs, reads diff + tests, scores ACs). Judge verdict becomes part of the tick report.
2. **Judge on the scheduler repo itself:** the scheduler is the system's own codebase — dogfood it. Open the top failure-bucket fix as a GitReins task with ACs, let the judge evaluate it.
3. **Escalation:** if the judge fails a task (fabrication detected), the foreman must NOT mark it complete; it re-opens with judge feedback in the prompt.

---

## 6. Sequencing & First Actions

| # | Action | Owner | When |
|---|--------|-------|------|
| 1 | Add slog JSON logging to schedulerd (1.1) | scheduler repo | This week |
| 2 | Write failure-bucket classifier (1.2) | scheduler repo | This week |
| 3 | Populate value ledger in tick wrapper (1.3) | cron skill + tick wrapper | This week |
| 4 | Wire `gitreins task complete` into foreman Step 7 (2.1) | foreman skill | Immediate (no code) |
| 5 | Idle-graduation cost ladder (3.1) | foreman skill + fleet policy | Next |
| 6 | Weekly value report cron (3.3) | cron job | Next |
| 7 | Zombie-tick guard (2.2) | scheduler repo | Next |
| 8 | Fleet rebalance by evidence (3.2) | script | Later |
| 9 | One-command onboarding (4.1) | script | Later |

**First concrete step I can take right now:** wire the GitReins LLM judge into the chimera foreman's Step 7 by patching the `coding-hermes-foreman` skill, and create the weekly value-report cron. Both are zero-code, immediate, and give Bane visible value this week.

---

*Chimera v2 system plan · generated 2026-08-01 · grounded in scheduler DB (34,549 ticks, 64% fail, $615, commits=0 everywhere)*
