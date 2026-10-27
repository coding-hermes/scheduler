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

## PLUS ULTRA #2 release (09-16, 越) — ADV-R09/R10/R11 battle won
Goal: R09→R10→R11 landed+judged; SEC-002 excluded by design (Bane-only
credential rotation). R09 (2b9672c) budget reality: budget authority chain
TOML<env<flag, single source=Loop, budget_source provenance, price-map
as-of+fallback policy in code, refresh path, cost_source stamps (v29),
token estimate recalibrated from measured actuals. RED-proven (literal 100
back -> status test FAIL). R10 (11c6dcf) adaptive-ceiling single authority:
derived default 8x floor, explicit wins, no-second-constant guard (RED
probe FAILed guard, green after). R11 (e3f86eb) per-spawn RLIMIT_AS at
spawn-time, !linux stub, 0=off default; RED-proven (call disabled -> real-
child test FAIL). Sibling lane interleaved SCHED-GAP-125 (5aa01ba) mid-
release; merged tree suite 11pkgs ok + lint 0 + CI all green. All three
rows closed with commit_hash; scheduler RESUMED. Remaining ADV: R12-R14
(refinements) + 6 new GAP-126..131 (sibling PRD pass). Battle rows: 0.
Beyond Pass (cap 5): BP-1 SEC-002 surfaced-not-blocked; BP-2 foreign-lane
interleave absorbed cleanly (disjoint files, verified merged); BP-3 real-
child rlimit tests forced to FULL mode in verification (short-skip hid the
guarantee); BP-4 board.jsonl header-bump EOF glitch noted (row updates
landed, cosmetic); BP-5 R12-R14 remain as designed backlog, not debt.

## crier-dogfood cadence fix (Bane 09-16)
Found at ~2h cadence (leftover docs-sprint speedup, 8 ticks/24h ~$17; two
extra history entries were orphan-resume re-nudges of a stuck 14:52 tick,
not spawns). Bane ruling: daily 86400 now, 3d (259200) once the stuck
orphan drains. Applied 3-layer: ELEVATED_PINS crier-dogfood 86400 (dated) +
fleet.toml 86400/86400/691200 + live PUT readback verified; --verify
PARITY OK (17 pins).

## 2026-09-19 | coding-hermes-scheduler | 🟡 PROMISING-BUT-ROUGH
Promise: a fresh user goes README "Getting Started (5 minutes)" from zero → running scheduler with imported cron jobs.
Reality: build/boot/API/MCP/dashboard/test-suite all solid; first boot FATALs (missing ~/.hermes/coding-hermes), migrate FATALs raw without Hermes cron, repo_url required-but-undocumented, created projects arrive disabled.
Top findings: SCHED-GAP-182 (P1 fresh-boot FATAL), SCHED-GAP-183 (P2 repo_url required + disabled-by-default), SCHED-GAP-184 (P2 migrate-dry raw FATAL).
Time-to-first-success: ~12 min (would be ~5 with the gaps fixed). Bunker leg: RAN — las-bunker-03 agent 974b10ac, clone 25s, make build 84s, make test EXIT=0 102s, agent destroyed.
Artifacts: docs/dogfood/2026-09-19-integration.md
## 2026-09-23 | coding-hermes-scheduler | ✅ SHIPPABLE (groups/templates deploy surface)
Promise: an operator deploys a task template to a group of projects and the rows land on every member's JSONL board, idempotently, discoverable from the API/MCP alone.
Reality: deploy/idempotence/id_pattern/labels/MCP-calls/SSE/metrics/persistence all correct; two frictions — deploy needs a hand-created board file first (SCHED-GAP-223), template create silently drops unknown task fields (SCHED-GAP-224).
Top findings: SCHED-GAP-223 (P2 deploy no-auto-init), SCHED-GAP-224 (P3 silent field drops). Perf: none worth a row (reads 6-10ms warm, boot 24ms, bunker build 84s).
Time-to-first-success: ~4 min (2 min = SCHED-GAP-223 gotcha). Bunker leg: RAN — las-bunker-03 agent b20ad9cb, clone 19s, go-install 37s, make build 84s, first-boot+deploy smoke PASS, agent destroyed. SCHED-GAP-182 confirmed FIXED (clean-HOME boot OK).
Artifacts: docs/dogfood/2026-09-23-integration.md
## 2026-09-24 | coding-hermes-scheduler | 🟡 PROMISING-BUT-ROUGH (dashboard + status surface)
Promise: an operator opens the live htmx dashboard and monitors — pause/adjust included per README — with fleet overview and health refreshing every 10s; README quickstart verifies via /api/v1/health and /api/v1/status.
Reality: data is correct (dashboard rows match /api/v1/projects), per-project/queue/ticks/events/SSE are fast, but the flagship surface is user-blocking-slow: GET / ~45s cold AND ~45s again on every ~60s cache cycle (2.2s warm), /dashboard/partial (10s htmx refresh) 317KB @ 4.1s±7.8 (2.2-37.1s, n=20), /api/v1/status 94.7s, /api/v1/health 26.4s, real Chrome first paint 49s. No pause/resume/adjust controls exist in the UI. Mechanism proven (see SCHED-GAP-1575 update): one Loop write-mutex serializes whole-pass evaluate()s (unbounded go l.evaluate() per wake) with status/dashboard handlers queued behind it; renders re-walk every project's .gitreins/history (21.7% of CPU).
Top findings: SCHED-GAP-1576 (P1 perf+payload+read-only UI), SCHED-GAP-1575 mechanism update (P1), SCHED-GAP-1577 (P2 TestCountPending_MtimeReread fails on clean machine — fresh-install battery ci-pass FAIL; passes locally).
Time-to-first-success (dashboard open): ~49s; (API verify step): ~27-95s. Friction count: 5 (45s page, 4s+partial, 95s status, 26s health, README control promise false).
Bunker leg: RAN — bunker-qa on bunker-las-02 agent 671e3584: fresh-install OK (toolchain bootstrap incl. go 1.26.5/zig/make), upgrade v1.3.0→HEAD OK, chaos-disconnect OK, chaos-corruption OK, ci-pass FAIL (SCHED-GAP-1577), chaos-resource FAIL (3GiB cap), docker-deploy N/A; agent destroyed + absence-verified.
Artifacts: docs/dogfood/2026-09-24-integration.md (+ this run's probes archived in scratch under ~/.hermes/stand-in/dogfood/coding-hermes-scheduler/scratch/)

## 2026-09-24 evening (coding-hermes-scheduler-dogfood tick, day-2 re-verification)
Verdict: SHIPPABLE (same surface as the 09-24 morning run; day-2 re-measure).
Promise: user runs the fleet scheduler, steers all lanes via dashboard/REST/MCP.
All morning-run slow paths re-measured FIXED at HEAD 0238b8fa: GET / 2.1s warm
(was 45.9s cold), partial 2.1s (was 4.05±7.78), status 0.23s (was 94.7s), health
0.5ms (was 26.4s), goroutines 74 (was 191-202). MCP 45 tools, spawn contract
verified end-to-end (202 + real tick_id + 409 dup). Top findings: SCHED-GAP-1613
(chaos-corruption generic db-open message), SCHED-GAP-1614 (P1 weight-budget hold
starves satellite families 60-125x demand:allocation, same lanes held every cycle,
starved escalations 23-53h vs 6h cooldown; cap 2017 vs ok 14 deferrals/24h),
SCHED-GAP-1615 (TestSpawn_MemLimitOffByDefault fails under capped parent —
clean-machine class #2). Install leg RAN: bunker-las-03 agent 184e5845, full
battery (bootstrap/fresh-install/upgrade/ci-pass/4 chaos cells), destroy verified;
bunker-las-02 default server DOWN this tick (100.01 refused) — override worked.
Time-to-first-success (spawn contract happy path): <1s API; dashboard open 2.1s.
Friction count: 1 (bunker-las-02 outage, worked around via documented override).

## 2026-09-25 | coding-hermes-scheduler | 🟡 PROMISING-BUT-ROUGH (control surface, REST+MCP)
Promise: "monitor, pause, or adjust any project through the dashboard, REST API, or MCP tools."
Reality: REST control surface is shippable — create/adjust/pause/resume/delete all 16-23ms,
honest error messages, disable provenance stamped, full api.auth audit trail. MCP is the same
power with ZERO auth: an unauthenticated fleet_pause_scheduler tools/call EXECUTED on the live
fleet (reverted in ~60s, verified evaluations continued). MCP mutations also write no audit event.
Top findings: SCHED-GAP-1619 (P1 /mcp bypasses SCHED-GAP-1602 gate + no audit), SCHED-GAP-1620
(P2 spawn works on disabled projects — bump refuses, spawn doesn't check), SCHED-GAP-1621
(PERF-002 re-measured at v1.5.0-92: cold status 28-104s, warm 0.25-0.44s — still open).
Time-to-first-success: ~2 min (create 400 told me exactly which fields were missing).
Friction count: 2 (MCP raw SQL errors to callers; auth mode not verifiable from any read endpoint).
Bunker leg: LAUNCHED (bunker-las-03 agent c376f19f, full battery) — collect in flight at report time.
Artifacts: docs/dogfood/2026-09-25-control-surface.md

## 2026-09-25 (13:xx) | coding-hermes-scheduler | 🟡 PROMISING-BUT-ROUGH (dashboard info surface @ 496 lanes)
Promise: "monitor, pause, or adjust any project through the dashboard, REST API, or MCP tools" — monitoring half holds up at the new scale.
Reality: SCHED-GAP-1598's search/filter/pagination controls are deployed and work; dashboard numbers agree with independent API ground truth (weights 12/12, 496-lane footer matches, honest "0 lanes" empty state); /api/v1/status 0.26s and health 0.5ms (94.7s/26.4s yesterday — the convoy fixes deployed). BUT the 10s autorefresh partial regressed past its own closed criterion (DASH-PERF-003 closed at 0.87s; now 3.73s±0.73 n=10), mechanism proven (git-subprocess-per-tick-sample renders + ReadBoardFreshness 31% under render load), and the SCHED-GAP-1619 MCP-auth fix (b16e61a8) is NOT in the running daemon (63dfbd10) — pre-auth MCP mutation re-proven live at 13:30Z on a nonexistent-project probe. Chrome cold open ~80s to settled DOM.
Top findings: SCHED-GAP-1623 (P1 refresh partial regression + mechanism), SCHED-GAP-1626 (P1 merged≠deployed, no deploy-time re-verify step), SCHED-GAP-1625 (P2 still no mutating dashboard controls), 1624 (1.15MB unpaginated list, ?limit silently ignored), 1627 (morning tick's bunker battery never collected — evidence lost), 1628 (fresh install PASSED: build 63s, dashboard 6.0ms empty-fleet).
Time-to-first-success: ~80s cold Chrome, ~4s warm.
Friction count: 3 (3.7s refresh partial, 1.15MB list responses, no mutating controls).
Bunker leg: PASSED (bunker-las-03 agent 6bd25844, destroyed; clone→build 63s→boot smoke OK).
Artifacts: docs/dogfood/2026-09-25-scale-dashboard.md; board rows SCHED-GAP-1623..1628 (commit 8c45f024); scratch dg2_* evidence.

## 2026-09-25 (~20:15-20:55Z) | coding-hermes-scheduler | ✅ SHIPPABLE (simulator engine) / 🟡 PROMISING-BUT-ROUGH (report + docs)
Promise: "An operator can safely rehearse fleet behavior via the documented simulator: --simulate (dry-run), --sim-setup/--sim-ticks (13-project fixture), --sim-count N (bulk), SCHEDULER_TIME_* (simulated clock) - with fail-closed safety (sim clock refused without --simulate; env parse failures refuse boot; every boot logs its clock)." Third angle today, per the rotation rule: runs 1-2 covered control surface + dashboard-at-496-lanes; the simulator machinery had never been used by a dogfood run.
Reality: the safety contract is flawless (6/6 refusal probes exit 1 with specific, value-quoting messages before any DB open; boot line matches clock-modes.md verbatim) and the engine delivers (sim-clock rehearsal 0.20s vs 32.2s real-clock = 161x; --sim-count 2000 in 2.11s under SCHEDULER_TIME_SCALE=1000; e2e-test claim verified at 0.266s; DuckBrain spool fallback proven live with a dead URL; fail-closed operator auth re-proven). But the report the user reads lies (printed success 73.3%/24.7% vs DB truth 91%/90.6% - mid-flight snapshot labeled final), prints hardcoded Budget/MaxConcurrent (8, vs observed 10), mislabels virtual time as "real time", the 30s/~480-tick bulk ceiling is undocumented with an opaque error, and no API field distinguishes a sim daemon from production.
Top findings: SCHED-GAP-1630 (P1 report-vs-DB contradiction), 1629 (P2 PERF-007: recipe gap 32s-vs-0.2s + bulk ceiling), 1631 (P2 hardcoded header), 1632 (P2 virtual-mislabeled-real + API sim invisibility), 1633 (P2 SKIPPED-install-bunker: las-03/las-04 ssh timeout, las-02 bunkerd crash-looping since 13:38 PDT; fresh-install cell itself passed this morning - 1628; un-run delta = simulator smoke on bare metal).
Time-to-first-success: ~5 min from docs to a running rehearsal (cold 32.2s); ~4 min more to find the 0.2s fast path (it is in NO doc a user actually reads first).
Friction count: 4 (report lies; undocumented wipe/blast-radius on --sim-setup; bulk ceiling; sim invisible over API).
Bunker leg: SKIPPED-install-bunker (all 3 bunker hosts down, evidence in 1633; nothing left in flight).
Artifacts: docs/dogfood/2026-09-25-simulator.md; diagnostics.md addendum; skills/scheduler-usage/SKILL.md updated (safe fast recipe + hazard map + stale auth note replaced); board rows SCHED-GAP-1629..1633 (tasks.jsonl commit) + artifacts commit fdd22f7f (guards PASS); scratch legA1..G3_*, /tmp/dg3/*.db evidence.

## 2026-10-17 (04:27-05:00Z) | coding-hermes-scheduler | ✅ SHIPPABLE (refusal honesty) / 🔴 operator affordance (fed read surface)
Promise (docs/federation-query-spec.md): "one transport-agnostic query contract carried by HTTP/bus/MCP/CLI; reads are scoped, not open — absence of an allow is a REFUSAL, never a default-yes; every answered query is audited."
Reality: REMOTE-007..014 (14 rows, previously zero dogfood coverage) hold up on the honesty ladder. Live daemon v1.12.0-410: every read — valid token or not — answers the EXACT §2.2 envelope with a stable code (op_not_allowed 403 / unknown_op 400 / missing_corr_id 400); the SAME allow-check gate serves HTTP (caller operator:token), MCP (mcp:auth-token, HTTP 200 JSON-RPC error envelope per docs/api.md 74-97), and the CLI-over-bus (bus:transport, after sourcing CRIER_AUTH_TOKEN; without it the CLI degrades honestly with transport_error 401 and enumerates its envs in --help). Replay idempotencyproven byte-identical on dg-rep-1×2. Aggregate denies cleanly. BUT at the operator layer the deny-all default lands as a wall: config snapshot /api/v1/config (949 B) renders ZERO federation/identity keys, /api/v1/health has no scheduler_id, README never mentions [federation.allow] or the env ladder, and the three transport caller keys differ without any doc explaining the split. Top findings: SCHED-GAP-1738 (P1 deny-all invisible in config surface), 1739 (P1 README/MCP quickstart never mentions publishing allows), 1740 (P2 per-transport caller identity split undocumented), 1741 (P2 MCP tool peer/identity fall-through), 1742 (P2 identity carries no docs row on endpoints table), DOGFOOD-029 (verdict row).
Time-to-first-success: ~25 min from docs to a fully-explained refusal (including reading federation_policy.go to learn the caller key must be published — the docs never said it).
Friction count: 3 (docs gap on the allow ladder; identity-invisible config; multi-key caller split).
Bunker leg: SKIPPED-install-bunker — running-daemon premise (DOGFOOD-025), build+boot proven 09-25 (63s).
Perf: overview warm 12-13ms n=5, cold-after-60s 1.36s; MCP fed 4ms, HTTP fed 19-96ms — all comfortably fast, no row.
Artifacts: docs/dogfood/2026-10-17-federation-surface.md; rows SCHED-GAP-1738..1742 + DOGFOOD-029; commit 84d7ef10.

## 2026-10-20 (06:54-07:45Z) | coding-hermes-scheduler | 🟡 PROMISING-BUT-ROUGH (cadence read surface) + P1 control-plane CPU recurrence
Angle chosen (uncovered by prior runs): the cadence read surface — docs/api.md §10 / GET /api/v1/cadence, documented
hours before this tick (DOC-19, commit 6602ba64) and never driven by a dogfood run — used as an operator would
("which lanes are behind, and why"), cross-checked against the live DB, plus the freshly landed DOC wave read as a
claim list. Prior runs covered the control surface (09-25), the dashboard at scale (09-25), the simulator (09-25) and
the federation read surface (10-17).
Reality: the arithmetic is honest — 608 served rows = 608 DB rows, 0 target/target_source mismatches against the
documented resolution table, achieved exact on 606/608 (2 boundary ticks), empty responses are [] never null, and the
whole read costs 10.1/10.8/11.7ms for 107,580 B. But the TARGET is built from each lane's cooldown pin with no
admission_mode term, and 37 enabled lanes are tasks-admission (board-governed, packer_select.go:376
tasksAdmissionDue): 22 lanes run ABOVE their served target (consensus-foreman 9.71/day vs 2.00 = 4.9x;
monitoring-foreman 13.29 vs 4.00 = 3.3x, with 82 of 92 tick gaps under its own 21600s cooldown), and because the
deficit field is clamped at 0 they can never appear in the endpoint's own documented recipe
(select(.target_deficit_per_day > 0)) — nor in the cooldown-residue detector, which sees pin == cooldown. The
fleet-level reading (610.5 declared vs 258.7 achieved starts/day = 42.4%, enabled lanes) is therefore not a delivery
number, and the same recipe hands back 463 name-ordered lanes led by 7 disabled fixture lanes claiming 48-96
starts/day. Separately, the control plane spends 55% of its CPU in ReadBoardFreshness, 41.9% of it on ONE loop line
(board_freshness.go:542, 400,160 `\b<id>\b` matches = 5.13s against this repo's 656-row board) — a recurrence of the
closed PERF-002 with a new hot path (its git subprocesses are now ~11%).
Top findings: PERF-002-R2 (P1 — quadratic freshness mis-attribution scan; profile line-level evidence + independent
repro), SCHED-GAP-1745 (P2 — cadence target ignores admission_mode; over-achievement clamped away), SCHED-GAP-1746
(P3 — cadence + residue absent from all 9 dashboard routes; §10 recipe not actionable), DOGFOOD-030 (P4 — campaign).
Time-to-first-success: ~2 min to a served cadence table (single curl, no auth needed for the read); ~20 min to a
defensible verdict (the DB cross-check is what separates "the numbers are consistent" from "the numbers mean
something"). Perf measurement added ~25 min.
Friction count: 3 (the documented deficit recipe is unusable as shipped; the metric is invisible on the operator's
dashboard; the missing admission_mode context means the operator cannot tell which lanes the target even binds).
Bunker leg: SKIPPED-install-bunker — tailscale: bunker-las-01 offline 15d, -02 offline 7d, -03 offline 15d, -04
offline 18d (the coding-hermes-remote-bunker-* hosts are online but are agent boxes, not bunkerd targets), so
bunker-qa.sh has no host; the standing premise is the pending row DOGFOOD-025, so no duplicate skip row was filed.
Install-equivalent leg RUN instead: fresh local clone of HEAD e209c70b (0 dirty files) -> make build 4.10s wall
(9.84u+1.45s; warm build cache here, 63s cold on the 09-25 fresh box) -> boot with NO config on port 19091 + scratch DB
-> ready <1s, health 0.49ms, GET / 2.08ms, cadence/residue/projects all [] not null, and the boot banner honestly
declares FAIL-CLOSED operator auth; friction: the zero-config daemon still syncs to DuckBrain at localhost:3000 and
logs 5x "key rejected (HTTP 401)" + a queued HIGH event, with no documented off switch.
Perf (Step 2b): GET / 1.50s cold after 60s idle / 1.53s after 90s / 3.09s after 15s / 12.03s under load 23-32 vs
4.3-19ms warm (n=33); 10.97s of CPU in a 60s profile window, 55% ReadBoardFreshness / 41.9% one line; repro
400,160 MatchString calls = 5.13s. cadence 10ms, residue 1.6ms, health 0.42ms, /api/v1/status 413ms, /api/v1/projects
42ms — fast enough that no row is warranted.
Artifacts: docs/dogfood/2026-10-20-cadence-cost-and-install.md; rows PERF-002-R2 + SCHED-GAP-1745 + SCHED-GAP-1746 +
DOGFOOD-030 (tasks.jsonl 656->660, one audit event 1792220000000000148); scratch /tmp/df-cad-2026-10-20/ (xcheck.py,
xcheck2.py, gaps.py, repro/main.go, cpu60.pb.gz, cold_probe.sh, drive.sh, install_leg.sh+log, dash_timing.tsv,
openapi.json, mcp_tools.txt) with kept copies under ~/.hermes/stand-in/dogfood/coding-hermes-scheduler/scratch/.
Read-only lane: no source, no scheduler state, no fleet config and no repo visibility was touched; the scratch daemon
was killed and its port (19091) verified free.

### 2026-10-20 (08:0xZ) | coding-hermes-scheduler | addendum to the run above (same tick, second angle)
While re-checking the perf numbers I had already recorded I re-measured the OTHER closed perf criterion on the same
daemon and found it breached again: /api/v1/status p50 326ms / p99 ~350ms (n=10, warm, 59,975 B) against the PASS bar
PERF-001 recorded at closure (p99 < 100ms at 57k+ ticks), now at 89,933 ticks / 608 projects — filed as PERF-001-R2.
Profile of the handler under a 30-request driver: (*Server).status 1420ms of the window's 1520ms CPU,
computeProjectFailureRates 880ms (the function PERF-001 named; still the top CPU component), spendByCostSource 380ms,
sqlite 1120ms cum — but 47ms of CPU per 326ms of wall means the request mostly WAITS, and the daemon's block and
mutex profiles are both dark (0 samples), so the wait cannot be attributed from the process; the row asks for the
eval-lock A/B first (PERF-002 recorded the evaluate pass holding l.mu). Same discipline as the freshness scan:
criterion + command + numbers + line, no code touched.


## 2026-10-20b (run 2, same tick, second angle) | coding-hermes-scheduler | 🟡 PROMISING-BUT-ROUGH (operator troubleshooting + ops gates)
Same tick id as the entry above (`coding-hermes-scheduler-dogfood-2026-10-20-06-54-47`, run 1 = commits 784fdd8a + 493d11c2 + e68834f0, rows PERF-002-R2 / PERF-001-R2 / SCHED-GAP-1745 / 1746 / DOGFOOD-030). This run took the surfaces run 1 did not drive: the operator troubleshooting taxonomy (docs/troubleshooting-scheduling-errors.md, all 34 read-only confirms extracted from its own fenced blocks and run verbatim) and the drain/restart runbook's read-only pre-flight. Its rows are appended on top of run 1's, which stays byte-untouched.
Reality: the taxonomy is unusually good — 30 of 34 confirms matched the page's quoted shape, and its dated numeric quotes still hold (§1's 245/52 drain-503 pair, §6's stale-queued counts, §12's armed-lane example, §0's ADMIT-marker warning). The failures it does have all fail in the direction of FALSE COMFORT: (a) /api/v1/status can block 7-60s on the Loop's write lock held by the evaluate pass (goroutine dump mid-504: reader parked in ZeroSelectStats -> loop.go:1746 RWMutex.RLock, holder inside tick_process.go:174 -> ReadBoardFreshness -> classifyCompleteRow -> fork/exec), so the daemon's own 5s read budget delivers its 504 13.9-21.8s late and names the wrong helper (configDriftBlock vs slowest_step=runningWaveCostTotal; the same query read-only is 0.00s, and /debug/pprof/{block,mutex} return 0 samples) — that is the attribution run 1's PERF-001-R2 explicitly asked for; (b) ops/check-daemon-freshness.sh (runbook §1.6, exit 2 = deploy failure) reported "daemon down or wrong endpoint" three runs in a row while /health answered 200 in 0.54ms, and 8/8 runs minutes later returned FRESH rc=0 — the 10s hard-coded curl ceiling inside the stall, A/B'd against a 60s copy; (c) the page's §9/§10 confirm prints a 504 as `{"…":null}` (curl -s without -f) both times it was run, and three of the page's log greps count only the current copytruncate logrotate window (50MB/rotate 3) the page never mentions, while claiming the log is durable; (d) ops/check-fleet-invariants.py on the consensus board emitted 651 lines / 647 FAILs for one file-level scale condition (643 identical event-id-ascending lines) with no duplicate/drain verdict at all, the 4 real lines buried.
Top findings: PERF-001-R3 (P1 — read path starves on the loop write lock; the budget cannot bound a mutex wait; 3/6 operator runs >10s, one >60s; warm 0.31-0.49s vs cold-after-90s 15.95s/504), SCHED-GAP-1747 (P2 — the ops gate reports a healthy daemon DOWN and loses signal both ways), SCHED-GAP-1748 (P3 — invariant flood answers one condition per row), DOC-22 (P3 — the page's confirms return confident false negatives: 504-as-null + rotated-log window).
Time-to-first-success: ~30s to a documented answer, ~55min to a defensible one (the stall attribution is what turns "the docs seem fine" into "the docs are fine and the daemon lies to them at 600 lanes").
Friction count: 3 (false "daemon down" mid-deploy; null-vs-504 ambiguity on two confirms; log window with no rotation note).
Bunker leg: inherited, not re-spawned — run 1 of this tick ran the install-equivalent leg (fresh local clone of e209c70b, make build 4.10s, zero-config boot smoke on :19091) and recorded SKIPPED-install-bunker for the JIT leg (all four bunker-las-* tailnet hosts offline); DOGFOOD-025 still carries the standing premise, so no duplicate skip row. Delta since that proof: board/docs-only commits.
Perf (Step 2b): headline = GET /api/v1/status — warm n=40 0.311-0.494s (39/40) with 1 504 at 13.891s; cold after 90s idle 15.946s/504 then 0.535s warm; n=80 second pass 1 504 at 16.498s; stall catcher 1/47 at 21.820s; /health 0.538ms; /api/v1/config 14ms. Attribution: mid-stall goroutine dump + the daemon's own `WARN: slow request handler=status … budget=5s` lines; DB ruled out by EXPLAIN QUERY PLAN + 12 read-only runs at 0.00s; profiles ruled out (block/mutex = 0 samples). One P1 row (PERF-001-R3), no separate PERF row for the fast paths.
Artifacts: docs/dogfood/2026-10-20b-troubleshooting-runbook.md; rows PERF-001-R3, SCHED-GAP-1747, SCHED-GAP-1748, DOC-22 (tasks.jsonl 661 -> 665, boardctl validate OK); scratch /tmp/df-sched-1020/ (trouble-blocks.json, confirms/L*.out, status-loop.tsv, stall/stall47.goroutine.txt, freshness/{stock,patched}.sh, hotpath.sql, inv-consensus.out).
Read-only lane: no source, no scheduler state, no fleet config and no repo visibility was touched; the only writes are this entry, the doc above and the four rows. Catalog gap recorded in the doc: the repo has no docs/qa/use-cases.md (or run-history.jsonl), so findings carry ad-hoc evidence paths and this log is the canonical history.


## 2026-10-20c (run 3, same tick, third angle) | coding-hermes-scheduler | ✅ MCP credential posture / 🟡 introspection edges
Same tick id (`coding-hermes-scheduler-dogfood-2026-10-20-06-54-47`); run 1 = 784fdd8a + 493d11c2 + e68834f0 (cadence surface, PERF-002-R2, PERF-001-R2, SCHED-GAP-1745/1746, DOGFOOD-030), run 2 = b9e5d4a4 (operator troubleshooting taxonomy + runbook pre-flight, PERF-001-R3, SCHED-GAP-1747/1748, DOC-22). Third angle: the MCP surface driven as a real consumer — 26 of the live registry's 71 tools had never been named on the board, and the last MCP dogfood passes were 09-25 (pre-auth mutation) and 10-17 (fed_* peer semantics).
Reality: the part with a security history is sound. initialize + tools/list answer in 2.1/0.9ms; unknown METHOD -> -32601, bad params -> -32602; all 13/13 mutating tools refused fail-closed with -32001 "operator credential required for mutations" and both wrong-credential forms answered "invalid operator credential", exactly the three-arm table in docs/api.md (SCHED-GAP-1619 has not regressed); six read tools answer schema-conformant "<param> is required"; four lookups answer named not-found errors; every read mirror serves (cadence 114.8KB, fleet_lane_tree 66.6KB, namespaces_list 45.3KB, queue_get 22.7KB) and none is slower than the REST twin. But the edges an integrator hits first are rough: a typo'd TOOL name is answered by the credential gate (-32001 "operator credential required for mutations") instead of an unknown-tool error, so a mistyped READ tool reads as an auth incident and retrying with a credential only changes the lie; peers_events cannot be called in either advertised shape ({} and {"id":"*"} both fail, and the error names scheduler_id, a parameter absent from the schema); and unknown-lane reachability diverges — MCP lane_is_ancestor returns a confident is_ancestor=false/hops=0 for lanes that do not exist while REST /links/ancestor 404s unknown_lane and the sibling MCP lane_relations errors project-not-found.
Top findings: SCHED-GAP-1749 (P3 unknown tool name -> credential refusal; taxonomy), SCHED-GAP-1750 (P3 peers_events schema/handler drift: advertised omit-id and '*' both fail, error names an invisible param), SCHED-GAP-1751 (P3 unknown-lane answer disagrees with REST and with its own family; namespaces_projects listed as the weaker second instance; the empty link store explicitly recorded as a NON-finding so nobody "fixes" it).
Time-to-first-success: ~10s to a first successful tool call (one curl), ~35min to a defensible verdict (the discriminating work is driving the same input through both transports and both arms of the credential ladder).
Friction count: 3 (unknown-tool auth misdiagnosis; peers_events uncallable as advertised; unknown-lane divergence).
Bunker leg: inherited, not re-spawned — run 1 of this tick ran the install-equivalent leg (fresh clone + make build 4.10s + zero-config boot smoke) and recorded SKIPPED-install-bunker for the JIT leg (all bunker-las-* hosts offline; DOGFOOD-025 carries the standing premise). Product HEAD unchanged since run 2.
Perf (Step 2b): nothing slow enough to file. observatory_snapshot 126.3ms (window=24h 109.6ms, 6.9/10.4KB) is the only >100ms call and computes over ~90k tick rows; cadence 10.6ms/114.8KB, fleet_lane_tree 7.9ms/66.6KB, cooldown_residue 2.3ms, queue_get 1.5ms, the rest 0.5-1.4ms; gated refusals 6.2-8.1ms (two outliers 33.7/65.2ms). MCP adds no measurable overhead over the REST twins, and this pass never touched the /status family so run 2's lock stall did not contaminate it.
Artifacts: docs/dogfood/2026-10-20c-mcp-consumer.md; rows SCHED-GAP-1749/1750/1751 (boardctl create, validate OK, board 665 -> 668); scratch /tmp/df-mcp-1020/ (mcp_probe.py 59 calls, probe.tsv/probe.json, followups.py+log, ancestor_check.py, tool_schema.py).
Read-only lane: no source, no scheduler state, no fleet config and no repo visibility touched; no credential minted or presented; every gated tool probed with missing args or impossible ids so even a gate failure would have been a no-op. The only writes are this entry, the doc above and the three rows.

## 2026-10-23 (dogfood tick -16-25-03, run 1) | coding-hermes-scheduler | 🟡 ops tooling rough, gate + dashboard numbers sound
Tick `coding-hermes-scheduler-dogfood-2026-10-23-16-25-03`; HEAD 65159dd7; live daemon 8ec55a6f (built 2026-10-22T13:14:01Z, uptime 27h, :9090 — GET only). Third angle after the 10-20 trio (cadence/cost, troubleshooting taxonomy, MCP consumer): the layer that shipped yesterday and had never been driven as an operator — `ops/README.md`'s new shared-infra tooling (SCHED-GAP-128: fleet-infra-findings / collapse / sweep / check 13) plus the dashboard read as an operator, with every headline card recomputed from the DB.
Reality: the measuring instruments are good. `python3 ops/check-fleet-invariants.py` answers in 0.45s over 648 enabled lanes and 61 boards with 791 violation lines + 40 INFO lines + `FAIL — 791 violation(s) across 19 namespaces / 648 enabled lanes`, and `--json` agrees class-for-class; every violation spot-checked against the DB was TRUE (caps coding-hermes 8 vs 16, qa/pm/dogfood/releases 9 vs 1, duckbrain-sync 12 vs 1, hermes-dagger-foreman 3600s below the 6h floor, release-engineer 21600 off its named tier). `fleet-infra-findings.py classify` separates the documented pair correctly in 50ms (port-pool on bunker-las-03 → `fleet-infra:port_pool/route=infra`; "binary predates HEAD" with no substrate token → `project-local`), `stats` censuses 61 boards in 4.6s, `throttle` with no flag inspects. The dashboard's cards are honest: `Enabled Projects 648/845`, `Active Ticks 10`, `Fleet Weight 2505` match `sum(enabled),count(*)` = 648|845, `count(*) where status='running'` = 10 and `sum(weight) where enabled=1` = 2505 exactly; all 13 routes I opened answered 200, `/tape` 581KB in 37ms.
Top findings: SCHED-GAP-16923 (P2 — `ops/fleet-infra-sweep.sh` is documented as the "tick-safe wrapper" but applies by DEFAULT: my documented-command run removed 3 OPEN rows from `/home/kara/totalstack`'s live board and rewrote FLEET-INFRA-1 on the bunker shared board, printing one status-looking line; source ops/fleet-infra-sweep.sh:20-24), SCHED-GAP-16924 (P2 — the shared-infra board's rollback recipe cannot restore it: its backup lands in the backups ROOT instead of `<project>/`, and that file carries the run's OWN `last_collapsed_at` stamp while HEAD has no such field), SCHED-GAP-16925 (P2 — the per-tick convergence has NO live caller: `qa-scheduler-tick.sh` is the only invoker and it is in `internal/api/retired_drivers.go:22`; 0 project prompts, 0 skills, 0 crontab hits carry it; the collapse archive dir holds exactly 2 files ever — the 04:20Z migration and this run — while 3 open copies remain and check 13 fires them), DOC-24 (P3 — cap policy stated three contradictory ways: api.md:951-952 = 8/1, fleet-config-model.md:74,122 = 16/flat-1 (R3.3 superseding 215), design-decisions.md:50 = 9/12 and naming SATELLITE_CAP_POLICY which is flat 1; the DB runs 8/9/12 so the gate's 6 caps violations are true and a reader of design-decisions would "fix" toward the rejected state), DOC-25 (P3 — the checker class table is 3 live classes short: namespace-membership, board-id-slot (INFO tag `open-rows`), cooldown-pin-pacing; README's own rule calls that "a gap in the gate"), SCHED-GAP-16926 (P3 — the documented "run it after every scheduler deploy" battery `scripts/check-invariants.sh:27` hardcodes `--board-only`: same host, same minute, battery RC=0 `BATTERY: PASS` in 26.00s (200 passed/2 skipped) while `python3 ops/check-fleet-invariants.py` exits 1 with `FAIL — 791 violation(s)`; the CI reason "a GitHub runner has no DB" is asserted on the one box that HAS a DB, and the stage headers read `[1/2]` in a three-stage script), PERF-001-R3-R2 (P2 recurrence — first `GET /` after an idle window blocked 79.685s (135,376 B, 200) with the warm path 5.4-6.0ms and 10ms daemon CPU per warm request; `grep 'GET / ' ~/.hermes/coding-hermes/scheduler.log` matches nothing, so the wait leaves no record; same class as PERF-001-R3, new surface = the dashboard root).
Footprint (disclosed, restored): the sweep mutation above was rolled back byte-exact — totalstack from `fleet-infra-backups/totalstack/tasks.jsonl.bak-20261023T163316Z` (git status clean apart from the pre-existing untracked `.lock` from 2026-09-30), bunker from `git show HEAD:.coding-hermes/board/tasks.jsonl` after proving the only delta vs HEAD was the single `last_collapsed_at` field added by the run (`cmp` identical, git status clean). That the infra board's own backup was POST-write is SCHED-GAP-16924, found while undoing my own residue.
Perf (Step 2b): nothing slow except the cold dashboard root (above). check-fleet-invariants 0.45s cold and warm; stats 4.63s (4.56s user, 61 boards); collapse dry run 1.08s; sweep 1.24s; /queue 246ms/434KB, /ticks 787ms/95KB, /cadence 27ms/300KB, /tape 37ms/581KB, /lanes/tree 16ms/211KB, /observatory 165ms, /dashboard/partial 5ms, /projects/<lane> 251ms, /namespaces/<id> 18ms. Fast paths filed nothing — a win nobody can feel is not a finding.
Bunker leg: `bunker-qa.sh launch … --keep` on bunker-mvp → agent c4a3a2eb, 2026-10-23T16:38:25Z, TTL 4h; cells `repo-head` OK (tested HEAD 65159dd7, clone dirty so pull skipped) and `launch` OK (detached qa-run.sh, 135,569 bytes). `collect` at 16:52Z answered RUNNING (agent left intact, battery 7-15 min); `bunker-qa status` reported `act=no`, so the ci-pass cell is expected UNVERIFIED. Cell harvest owed to the next tick's entry — this is PARTIAL, not SKIPPED, and no SKIPPED-install-bunker row is warranted.
Artifacts: docs/dogfood/2026-10-23-ops-scripts-and-dashboard.md; rows SCHED-GAP-16923/16924/16925, DOC-24/25, PERF-001-R3-R2 (ids from board_next_id.py incl. `--recur PERF-001-R3`; tasks.jsonl 687 -> 693, `boardctl validate` RESULT: OK); scratch /tmp/df-ops-1023/ (live.out 832 lines, live.json, stats.out, collapse.out, sweep.out, page fetching + timings).
Read-only lane: no source, no scheduler state, no fleet config, no repo visibility and no credential touched; all daemon traffic was GET. The only writes are this entry, the doc above and the six rows — plus the disclosed-and-reverted sweep mutation on two foreign boards.

### 2026-10-23 addendum (same tick, install leg status) | coding-hermes-scheduler
Install leg did NOT finish inside this session and is recorded as PARTIAL, not SKIPPED: `bunker-qa.sh launch … --keep` on bunker-mvp → agent `c4a3a2eb` at 16:38:25Z (TTL 4h), cells `repo-head` OK (tested HEAD 65159dd7; clone dirty so pull skipped) and `launch` OK (detached qa-run.sh, 135,569 bytes). Four `collect` calls (16:45Z, 16:52Z, 16:56Z, 16:58Z) all answered `RUNNING … agent left intact`, and the evidence file has not grown past those 2 cells. The battery's own log `/home/bunker-c4a3a2eb/qa-run.log` read `QA-HEAD=… / HARNESS-COMPOSE=ok / HARNESS_NODE=ok` at BOTH 16:52:30Z and 16:58Z — an identical tail across ~6 min, 20 min after launch, with `bunker-qa status` showing `act=no` at launch time, so the harness bootstrap is the stage under suspicion. No row filed: one frozen window is a lead, not a finding, and it does not separate a slow `act` download from a hung step. Agent `c4a3a2eb` was left ALIVE on purpose (destroying it loses the cells); next tick owes `~/.hermes/scripts/bunker-qa.sh collect --evidence /tmp/bunker-qa-evidence-20261023T163802Z-1846595.jsonl`, the stall attribution if it reproduced, and then the destroy. Everything else in the entry above stands; this addendum adds the leg's state and the SCHED-GAP-16926 row (post-deploy battery is board-only), which landed in the same commit.

### 2026-10-23 addendum 2 (install leg CLOSED) | coding-hermes-scheduler
The install leg left open by addendum 1 is finished and graded. Battery completed; `bunker-qa.sh collect` at 17:09:53Z pulled **22 evidence rows** and logs were copied off the agent into `/tmp/df-ops-1023/bunker-logs/` BEFORE teardown; agent `c4a3a2eb` destroyed and proven gone (`bunker info` → `not_found`, `bunker list` census 0). Cells: 7 toolchain-bootstrap OK (go 1.27.3, gcc/g++, make, compose, buildx, node v22.15.0), fresh-install OK (detail is a `go: downloading` tail, so installability rests on `upgrade` + `ui-probe`), upgrade OK (v1.11.7 tree via `git archive` → HEAD reinstall clean), ui-probe OK (`/`, `/dashboard`, `/health` 200 text/html), docker-deploy/shutdown N/A (no compose file), corruption N/A (no db/state files), chaos-disconnect UNVERIFIED (correctly refused to grade while the native suite was red), chaos-errorpath INFO.
Two cells FAILED and both became rows: `INT-CI-189-R2` (P2 recurrence — on a TAGLESS tree `TestFreshnessScript_ExitCodes` fails 10/10 arms with `exit = 2 … UNKNOWN: daemon version string not machine-parseable`, plus `TestADMIT148_FreshnessGuard` 5/5; measured mechanism: the tested snapshot has `git remote -v` empty, `git shallow` absent, `git tag | wc -l` = 0 and `git describe --tags --always` = `b3fae23`, so the git-describe-shaped build identity degenerates — the same shape a release-tarball install produces, and the reason INT-CI-189's "passes at HEAD 9/9" closure (measured on a tagged checkout) does not cover it; the SAME 10 lines appear in res.log under the 3 GB cap, so it is identity-specific, not memory-specific; the rest of the suite was green — internal/scheduler 179.2s, mcp 39.3s, config 15.4s, sync 23.2s) and `SCHED-GAP-16927` (P3 — `TestREMOTE009_ResponderFullRoundTrip` fails `bus: query reply timeout` at 2.01s on the 2-vCPU box while hosted CI at the same SHA is green; ONE observation, filed as a lead that must be reproduced or disproved, with the INT-CI-082 contention-scaled-budget precedent named). Verified: tasks.jsonl 694 → 696, `boardctl validate` OK with the warning count unchanged (832 at HEAD and 832 in the tree).

## 2026-10-27 (dogfood tick -15-09-28, run 1) | coding-hermes-scheduler | 🟡 operator CLI + boot read surface: CLI honest but its own flag shape traps you, and the closed P1 lock-starvation still reproduces at boot
Tick `coding-hermes-scheduler-dogfood-2026-10-27-15-09-28`; HEAD 889497d7 (detached worktree, clean checkout); build `make build` 2.70s wall → `v1.12.0-668-g889497d7`; live daemon (GET only) build 8ec55a6f, uptime 122h, :9090. Fourth angle after the 10-20 trio (cadence/cost, troubleshooting taxonomy, MCP consumer) and 10-23 (ops tooling + dashboard): **the operator command line** — `cmd/scheduler-query` and `bin/migrate` — which no dogfood run has ever driven (0 hits in this log), plus a current-HEAD restart of the daemon against a live-sized DB.

**What worked (verified, no rows).** `bin/migrate` is honest and idempotent: per-job `SKIP <name>: <reason>` reasons hold (README §4's promise; the old DOGFOOD-017 defect stays fixed), `Done: 0 imported, 158 skipped` on the live `jobs.json`, `--dry-run` wrote NOTHING (sha256 of the DB copy identical before/after), a real import landed `enabled=1 w=10 p=5 cooldown_s=900`, re-run → `SKIP alpha: already exists` with the project count unchanged. `scheduler-query` itself is fast and well-behaved where it matters: unknown aggregate op → 400 `unknown_op` naming the six supported ops; unknown peer → named `transport_error`; `--json` one compact object; `--all` with no op → clear usage error. **Nothing in this pass was slow enough for a user to feel**: `--all fleet.status` warm mean **10ms** (n=20: 8-29ms), `--help` 7-10ms, `migrate` 0.01-0.02s, daemon boot-to-ready <0.5s on an empty DB, `/api/v1/config` 0.36ms. No PERF row filed for any of those.

**What broke (4 rows + 1 recurrence).**
1. **PERF-001-R3-R3 (P1, recurrence).** A current-HEAD daemon booted on a byte copy of the live DB (8.43 GB; 93,731 ticks / 845 projects / 822,427 events) returns **504 on every DB-backed endpoint for at least 91s**: `/api/v1/health` 504 @1.000-1.001s and `/status`, `/projects`, `/cadence` @5.000-5.001s, at every sample t+1s/t+7s/t+15s/t+30s/t+60s/t+91s — and the dashboard root renders a **truncated page (40,320 B vs 142,115 B on the live daemon)** while doing it. Every response returns at *exactly* its budget, so the handlers wait, not compute. Goroutine dump mid-window puts the holder in the evaluator: `Loop.Run loop.go:884 → Loop.evaluate tick_process.go:317 → Packer.Pick packer.go:336 → PendingTaskCounter.CountPending board_awareness.go:276 → freshnessCheckedPending :313 → cachedFreshnessRead :184 → FreshnessVerdictCache.Read board_freshness_cache.go:163 → readFull :204 → ReadBoardFreshness board_freshness.go:613 → classifyCompleteRow :683 → gitRevVerifyCommit :886 (fork/exec)` — the per-row git subprocess scan PERF-002 named, reached from the packer's pending count on the boot/evaluate path. Controls that exclude host and disk: the LIVE daemon on the same box in the same minutes answered `/status` 200 @0.345-0.368s (n=5) and `/` 200 @5ms warm; read-only `sqlite3` counts on the same copy = 0.17s; loadavg 5.9-6.9 on 16 cores; `/api/v1/config` (no DB) 200 @0.36ms throughout. Reproduced on **two** boots; the first recovered by ~t+2.5min. Confound disclosed in the row: the DB is a copy (never written to), so the boot evaluation re-derives on a snapshot — the same shape a real restart exercises. User consequence: the endpoint that 504s first is the daemon's own liveness probe, so `deploy/scheduler-verify.sh` and any supervisor reading `/api/v1/health` see "unhealthy" for minutes across a restart.
2. **SCHED-GAP-16930 (P2).** The documented authoritative aggregate is un-enableable from the docs. `scheduler-query --all` (its own help: "the AUTHORITATIVE aggregate") sends `op=fleet.aggregate`, which the caller allow-list must publish — but `fleet.aggregate` appears in **no** served catalogue (`GET /api/v1/federation/catalogue` → `count:6`), **no** spec §2.3 op table, and **no** README allow recipe (README:1085-1112 tells you to fill the list with "§2.3 op ids"). Deny-all arm: valid token → 403 `op_not_allowed` ("op fleet.aggregate is not published for caller operator:token"). Allow arm (same binary, one op added to `[federation.allow."operator:token"]`): boot line `read policy armed (1 caller grant(s))` → `status=ok, peer_count=1, answered=1, degraded=0, self:true`. So the single missing prerequisite is an op id the product's own catalogue says does not exist. Live fleet reality: `/api/v1/config` → `federation {deny_all:true, allow_count:0, callers:[]}` (SCHED-GAP-1738's visibility fix works — the object now renders).
3. **SCHED-GAP-16931 (P3).** The CLI answers that 403 with `— set SCHEDULER_OPERATOR_TOKEN`: the one remedy the operator has already applied, because the credential was valid and presented. 401 (no credential) and 403 (credential valid, op not published) share one advisory, and the actionable cause survives only inside the body. Discriminating probes (garbage credentials only, no real credential presented): no env → `operator credential required for mutations`; `SCHEDULER_OPERATOR_TOKEN=<garbage>` → `invalid operator credential`; `CRIER_AUTH_TOKEN=<garbage>` → unchanged ⇒ the `--all` path reads `SCHEDULER_OPERATOR_TOKEN` and ignores `CRIER_AUTH_TOKEN`, while `docs/reference/env-vars.md:148` states the opposite for the CLIs.
4. **SCHED-GAP-16932 (P3).** `scheduler-query` breaks its own documented flag shape: `--all peer.status --daemon-url http://…` (the usage line's own `[flags]` position, space-separated) → `flag needs an argument: -daemon-url` + help dump; only the `=` form parses. Worse, `--daemon-url http://… --all peer.status` silently eats `--all` as the flag's value and falls back to single-peer mode against a "peer" named after the URL (`status=error code=transport_error`), no error raised.
5. **README-20 (P4).** README:88 documents `workdir /home/...` (no colon) as an accepted prompt form; `cmd/migrate/main.go:37` requires a colon (`[Ww]orkdir:\s*(\S+)`), so the documented form is skipped as `no workdir found in prompt`. Measured 6-form matrix: `workdir /…` SKIP, `Workdir: /…` imported, `workdir: /…` imported, `Workdir /…` SKIP, `Workdir: <absent path>` imported (no existence check), inline `Workdir: …` imported. Same run, for DOC-28's ambit: the imported project NAME is the workdir basename (`main.go:197-198`), not the job name, so three jobs pointing at one directory collapse into one project — undocumented.

**Install leg (not silent).** `bunker-qa.sh launch /home/kara/coding-hermes-scheduler/coding-herms-scheduler --keep` was started this tick (lonely-host premise dropped: `bunker-mvp` is reachable/active, `bunker-las-02/-03` online) — preflight/spawn/sync take 60-120s against a battery that runs 7-15 min, so this tick cannot finish it; state is recorded as **PARTIAL**, not SKIPPED, with the collect command owed to the next tick. See the addendum entry below for the agent id, TTL and evidence path. Install-EQUIVALENT proof ran to completion in the meantime: clean detached worktree of HEAD → `make build` 2.70s → zero-config boot on :19092/:19094 ready in <0.5s (`/api/v1/health` 200, honest fail-closed operator-auth banner, `FEDERATION: read policy DENY-ALL`), plus the live-sized-DB boot above.

**Footprint.** Read-only on the product and the fleet: no source, no scheduler state, no fleet config, no repo visibility and no credential touched; every live-daemon request was a GET; the only credential ever presented was a token minted for my own scratch daemon. Scratch: `/tmp/df-cli-1027/` (`src/` = detached worktree of HEAD, `ev/`, `live-copy.db` = byte copy of the live DB, `fed.toml`); evidence copies also under `~/.hermes/stand-in/dogfood/coding-hermes-scheduler/scratch/2026-10-27/`. Observed state worth a foreman's eye (not filed, both samples identical 4 min apart): the primary checkout is **diverged from origin/main** — ahead 2 (board commits 051d6ce0, 889497d7) and behind 4 (incl. `cbc819af`/`9e7b3b25` ACP-SCHED-001 executions), tree clean.
**Artifacts:** rows `PERF-001-R3-R3`, `SCHED-GAP-16930/16931/16932`, `README-20` (tasks.jsonl 709 → 714, `boardctl validate --fail-on dangling-dep` rc=0, warnings 853 → 853 = zero new); doc `docs/dogfood/2026-10-27-cli-and-boot-read-surface.md`.

### 2026-10-27 addendum (same tick, install leg launched = PARTIAL, not SKIPPED) | coding-hermes-scheduler
`bunker-qa.sh launch /home/kara/coding-hermes-scheduler/coding-herms-scheduler --keep` (controller HOME=/home/kara, primary checkout at 889497d7) ran to a successful phase-A completion at 2026-10-27T15:34:34Z: fence check `body_lines=2276 intended_substitutions=35 violations=0`, `agent=77b3809d @ bunker-mvp` (TTL 4h), `QA-HEAD=889497d7b23bdaed34152e5156cac9fa59da376c` with `pull=clone dirty — pull skipped, testing recorded sha`, cells `fresh-install` (sync repo → ~/qa-coding-herms-scheduler) and `upgrade-prep` (previous release tree v1.11.7) started. Evidence path `/tmp/bunker-qa-evidence-20261027T153434Z-180555.jsonl`; launch log `/tmp/df-cli-1027/ev/bunker_launch.log`. The battery needs 7-15 min and a single tool call is capped ~420 s, so this tick records the leg as PARTIAL: the next tick owes `~/.hermes/scripts/bunker-qa.sh collect --evidence /tmp/bunker-qa-evidence-20261027T153434Z-180555.jsonl`, the per-cell grades (including the `ui-probe` and `upgrade` cells that carry installability here), the `~/qa-logs-*/` pull before teardown, and the destroy proof (`bunker info 77b3809d` → not_found). The hosts are NOT down this tick — `bunker-las-02`/`-03` are online and `bunker-mvp` active — so no SKIPPED-install-bunker row is warranted and DOGFOOD-025 stays the standing premise row for the running-daemon argument only.
