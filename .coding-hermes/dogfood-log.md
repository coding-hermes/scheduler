# Dogfood Log — coding-hermes-scheduler

## 2026-08-04 — 🟡 PROMISING-BUT-ROUGH

**Promise:** "A single Go binary that replaces dozens of static cron jobs" —
priority-weighted fleet scheduler that spawns foreman ticks, tracks outcomes,
exposes REST/MCP/dashboard/DuckBrain sync.

**Verdict evidence:** Core loop works live (44 projects, 1,427 HTTP spawns,
10,105 completed ticks, escalation events firing, dashboard + MCP functional,
warm p50 38ms). But: POST /projects fails on every documented request shape
(400 snake_case / 500 CHECK on minimal PascalCase; 409 unreachable); wire
format is PascalCase while the spec/README say snake_case (README jq example
returns null); the 2-hourly self-verify has been RED 50 consecutive runs
(2026-07-31T08:00 → now) while the board claims all gates green; no delete
endpoint; occasional 13s read stalls.

**Time-to-first-success:** ~2 min (first probe stalled >10s, then 40ms).

**Top 3 findings (task IDs):**
1. **DOGFOOD-001 (P0)** — create-project API broken per documented contract (400/500/409-unreachable).
2. **DOGFOOD-002 (P0)** — `--test-verify` red 4+ days, board green; fixture shares one workdir, dup-workdir guard rejects it; gate suite excludes the project's own verify.
3. **DOGFOOD-003 (P1)** — mixed JSON dialects vs S06 contract + wrong README jq example.
   (Also DOGFOOD-004 spec staleness, DOGFOOD-005 no delete, DOGFOOD-006 latency spikes/69% failure-rate question.)

**Artifacts left:** docs/dogfood/2026-08-04-integration.md,
skills/scheduler-usage/SKILL.md, docs/dogfood/diagnostics.md, board tasks
DOGFOOD-001..006 appended to .coding-hermes/board/tasks.jsonl.

**Foreman:** already at CooldownS=900 / Enabled=true, ticking normally —
no wake needed.

## 2026-08-15 — 🟡 PROMISING-BUT-ROUGH (improved, edge surfaces still rough)

**Promise:** Same as 08-04: "A single Go binary that replaces dozens of static
cron jobs" — priority-weighted fleet scheduler, HTTP-gateway spawns, REST +
MCP + dashboard, outcome tracking.

**Verdict evidence:** All six 08-04 P0/P1 findings are FIXED and verified live
this run: create-project works per spec (snake_case body → 201 + defaults;
dup-name → 409), wire format is snake_case everywhere with envelopes
(projects → {"projects":[...]}, detail → {"project":...,"latest_tick"}),
DELETE endpoint exists (409 guard vs enabled projects), `--test-verify 3`
passes 6/6 (SCHEDULER VERIFIED, p99 16ms < 100ms spec at 29k rows), 2-hourly
verify logs green (06/08/10 UTC runs), S06 spec Approved. Live fleet healthy:
44 enabled, verify green, eval-stall watchdog firing as designed (GAP-042),
zero-select counters 0. But the NEW user/evaluator journey is still rough:
`--simulate` does NOT simulate (real spawner, gateway-key dependent),
`--sim-count` FATAL-crashes (UNIQUE ticks.id collision), /fleet plugin
symlink dangling (typo'd path), README §MCP has a fictional second tool
table, DELETE is an undocumented soft-delete (junk still accumulates;
3 scratch rows 69→72), GAP-044 provenance missing on all 25 legacy disabled
rows, repo fleet.toml + docs/fleet.md mirrors stale.

**Time-to-first-success:** ~1s (health check; no stalls this run).

**Top 3 findings (task IDs):**
1. **DOGFOOD-007 (P1)** — simulation broken on both documented entry points
   (--simulate no-op for spawning; --sim-count FATAL crash).
2. **DOGFOOD-008 (P1)** — /fleet plugin symlink dangling → slash commands dead.
3. **DOGFOOD-009 (P2)** — DELETE is undocumented soft-delete; hard-deleted
   projects still pollute projects_failure_rates.
   (Also DOGFOOD-010 provenance backfill, DOGFOOD-011 fictional MCP table,
   DOGFOOD-012 config over-claims, DOGFOOD-013 stale mirrors.)

**Artifacts left:** docs/dogfood/2026-08-15-integration.md (new),
skills/scheduler-usage/SKILL.md (rewritten to current reality),
docs/dogfood/diagnostics.md (appended), board tasks DOGFOOD-007..013
appended to tasks.jsonl + mirrored into board.db.

**Foreman:** Cooldown was 21600s (≥14400) → woken to 900s via API PUT after
task commit (Enabled stayed true).

## 2026-08-25 — 🟡 PROMISING-BUT-ROUGH (REST surface strong; MCP/spawn contracts broken)

**Promise:** "A single Go binary that replaces dozens of static cron jobs" —
priority-weighted fleet scheduler, HTTP-gateway spawns, REST + MCP + dashboard,
outcome tracking.

**Verdict evidence:** Live fleet healthy (74 projects/42 enabled, verify logs
green, DuckBrain sync OK). ALL prior findings re-verified FIXED live: create-
project 201/409, snake_case everywhere on REST, DELETE soft+purge (DOGFOOD-009),
--sim-count no longer crashes (DOGFOOD-007), OpenAPI 19 paths w/ requestBodies
(GAP-057), queue urgency computed (GAP-054), no ZgotmplZ bars (GAP-055), /fleet
symlink fixed (DOGFOOD-008), fleet.md mirror current. Full write-path lifecycle
on scratch daemon :9093 passed (create/dup-409/workdir-409/PUT dual-dialect/
400-validation/404/405/DELETE guards/namespaces CRUD+move/MCP 13 of 14 tools/
--test-verify 6/6/migrate import). NEW breaks: MCP fleet_add fails on EVERY call
(priority=0, no default → raw sqlite CHECK error; /fleet add routes through it);
spawn returns UTC tick_id that GET /ticks/{id} 404s (stored IDs are local-time);
MCP fleet_ticks still PascalCase; migrate silently skips non-coding-hermes jobs
(no reason logged, filters undocumented); eval-stall watchdog still fires ~hourly
(27 MEDIUM events/24h, GAP-061 closed by severity demotion only).

**Time-to-first-success:** ~1s (live health probe; no stalls this run).

**Top 3 findings (task IDs):**
1. **DOGFOOD-014 (P0)** — MCP fleet_add always fails (CHECK constraint
   priority>=1; toolFleetAdd never defaults Priority; /fleet add dead).
2. **DOGFOOD-015 (P1)** — spawn returns unresolvable tick_id (UTC vs local
   timezone ID mismatch between server_projects.go and slot_pool.go).
3. **DOGFOOD-016 (P2)** — MCP fleet_ticks PascalCase + AGENTS.md SSE over-claim.
   (Also DOGFOOD-017 migrate silent skips, DOGFOOD-018 hourly eval-stall alarm
   noise, DOGFOOD-019 MCP repo vs REST repo_url naming.)

**Artifacts left:** docs/dogfood/2026-08-25-integration.md (new),
docs/dogfood/diagnostics.md (appended), skills/scheduler-usage/SKILL.md
(v1.2.0), board tasks DOGFOOD-014..019 appended to tasks.jsonl + mirrored into
board.db.

**Foreman:** Cooldown was 21600s (≥14400) → woken to 900s via API PUT after
task commit (Enabled stayed true).

## 2026-09-09 — 🟡 PROMISING-BUT-ROUGH

**Promise:** single Go binary = dynamic priority-weighted fleet scheduler
spawning foremen via gateway HTTP; REST + MCP + dashboard; safe degradation.
**Reality:** core promise holds in production (178 projects, 61h uptime,
v1.1.1-58-g611828a). Full operator workflow verified on a --simulate scratch
daemon; --sim-setup harness + --test-verify 3 pass. Two reliability edges:
SCHED-GAP-101 (redundant resume wedges eval loop) RE-PROVEN at HEAD 5d0ab6f
(code unchanged since c8fcb13 — fix never landed); --sim-setup boot FATAL
~1/7 fresh DBs (FK race, retry hides it).

**Time-to-first-success:** ~4 min (build + health probe; no stalls this run).

**Top 3 findings (task IDs):**
1. **DOGFOOD-020 (P1)** — GAP-101 resume-wedge re-proven at HEAD; prod runs
   the affected code path; deployer resume step is the trigger.
2. **DOGFOOD-021 (P2)** — --sim-setup intermittent FK FATAL on fresh DB
   (boot race at fixture wipe; ~1/7, retry-succeeds).
3. **DOGFOOD-022 (P3)** — spawn-path counters (2415 exec / 66 http) contradict
   --no-exec-fallback default + README HTTP-first claim.
   (DOGFOOD-023: SKIPPED-install-bunker — las-bunker-03 unreachable.)

**Artifacts left:** docs/dogfood/2026-09-09-integration.md (new),
docs/dogfood/diagnostics.md (appended), skills/scheduler-usage/SKILL.md
(v1.3.0), board rows DOGFOOD-020..023 appended to
.coding-hermes/board/tasks.jsonl. Tests: go test -short 38s PASS,
--test-verify 3 ✅ SCHEDULER VERIFIED.

**Foreman:** coding-hermes-scheduler-pm Cooldown 86400s (≥14400) → PUT
CooldownS=900 + DecayRate=1.0 via API after task commit (Enabled stayed true).
Main coding-hermes-scheduler row already at 3600s — untouched.

## PLUS ULTRA release (09-15, 越) — ADV wave to done-and-live
Goal: R05+R06+R07 merged + deployed. Outcome: R05/R06 had ALREADY landed via
hourly-pin ticks (13:09/19:55, judged PASS) before engagement; R07 driven
directly (worker glm-5.2@zai-glm, commit 7d6aa0b, foreman adversarial verify
incl. RED mutation on forceEval, pushed, CI green); R01 verified already-true
(11:02 restart rebuilt from HEAD, migrations v27, wave surface live).
Project paused-for-daemon 22:45Z→resumed after R07 close (verified enabled=True).
BATTLE = won with evidence. Beyond Pass: see ledger below (cap 5).
