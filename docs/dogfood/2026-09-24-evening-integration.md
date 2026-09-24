# Dogfood: coding-hermes-scheduler — 2026-09-24 (evening tick)

Lane: coding-hermes-scheduler-dogfood (stand-in workdir). Verdict: **SHIPPABLE** —
the same verdict as the morning run on the identical surface (day-2 re-verification,
not a duplicate finding: every morning number re-measured, two deltas found and filed).

## Promise re-statement

"A user can run the fleet scheduler daemon, watch and steer every lane (foreman + satellites)
through a dashboard, REST API, and MCP endpoint, and spawn/bump/pause any lane on demand."

## Real use performed (3.5h, live daemon v1.5.0-75-g0238b8fa on :9090)

- MCP surface: tools/list (45 tools, matches the parity-test promise), fleet_status,
  fleet_project_detail, queue_get, config_get, project_spawn — all correct.
- REST: 12 endpoints curl'd including openapi.json (29 paths = documented set),
  /ticks drill-down filters, /api/v1/live, /api/v1/metrics, /api/v1/events.
- Dashboard pages: /, /queue, /ticks, /tape, /health (rendered content verified).
- The documented spawn contract end-to-end: POST spawn → 202 with canonical UTC tick_id
  → GET /ticks/{id} resolves it → duplicate spawn 409s with the exact documented error.
  The id format (`<name>-YYYY-MM-DD-HH-MM-SS`, local time matching the doc's
  "same generator" note) was verified against a non-UTC host (-05).
- Gateway integration: schedulerd's dependency :8642 answers /health in 11ms (v0.21.1).
- Ephemeral bunker leg: bunker-qa full battery on bunker-las-03, agent 184e5845
  (spawn → toolchain bootstrap → fresh-install → upgrade → chaos cells → collect →
  destroy verified by absence in `bunker list`). bunker-las-02 was DOWN this tick
  (100.01 refused at :10001 despite active bunkerd) — the script's default server
  failed, the documented BUNKER_QA_SERVER override worked first try.

## Measured (Step 2b, perf law: same command, warm, n repeated)

| operation | morning run (09-23/24) | this run (22:24-22:47) |
|---|---|---|
| GET / full dashboard | 45.9s cold / 2.2s warm | **2.1s warm** |
| GET /dashboard/partial | 4.05s ± 7.78 (n=20) | **2.1s** |
| GET /api/v1/status | 94.7s | **0.23s** |
| GET /api/v1/health | 26.4s | **0.0005s** (1s DB deadline holds) |
| GET /api/v1/live | — | 0.0003s |
| GET /queue | — | 0.003s |
| GET /tape | — | 0.014s (339KB) |
| GET /health panel | — | 0.67s (16KB) |
| GET /ticks | — | 0.14s (56KB) |
| MCP fleet_status / queue_get | — | 0.5s / ~0.2s |
| goroutines | 191-202 (WARN thrash) | **74** (baseline healthy) |

Every regressed number from the 09-23 run is FIXED and re-measured on the same
commands: the lock convoy (SCHED-GAP-1575) and the render cost (SCHED-GAP-1576)
fixes landed and hold at HEAD under a busier fleet (12 active ticks). Cold-cache
re-measure was not repeatable this tick (the 60s render cache expires into a
faster path; the fresh-install battery re-proves cold behavior on a clean box).

## New findings → board rows (SCHED-GAP-1613/1614/1615, appended to tasks.jsonl)

1. **SCHED-GAP-1613 (P2)** chaos-corruption: truncated scheduler.db exits rc=1 with
   `unable to open database file (14)` — honest fail-closed but the message never
   names corruption/recovery (misleading to an operator).
2. **SCHED-GAP-1614 (P1)** weight-budget hold exposes chronic satellite starvation:
   per-cycle demand:allocation 60-125x in satellite namespaces, the SAME lane set
   held every cycle (tatara 7/7), starved-escalation events firing for lanes 23-53h
   past their 6h cooldown, and `metrics.deferrals.by_reason` shows cap 2017 vs
   ok 14 over 24h. Enforcement works (it is loud, per SCHED-GAP-1582 doctrine);
   the net effect — enabled lanes with board work held multi-day with no rotation —
   is the regression question.
3. **SCHED-GAP-1615 (P2)** TestSpawn_MemLimitOffByDefault fails under a capped
   parent (3GiB RLIMIT_AS battery): the OFF path inherits the parent's cap instead
   of RLIM_INFINITY. Clean-machine-only, second distinct clean-machine test failure
   class (first: SCHED-GAP-1577, which did NOT reproduce this run).

## Install leg (never silent)

RAN. bunker-las-03, agent 184e5845: toolchain bootstrap OK (go 1.26.5, zig cc,
make 4.4.1, compose + buildx plugins), fresh-install (build from clone) OK,
upgrade v1.4.0→HEAD OK, ci-pass OK (act had no triggerable workflow → native
suite PASS instead), chaos-disconnect OK, chaos-errorpath OK (clean rc=1 on
missing config), chaos-corruption INFO (fail-closed, message misleading → 1613),
chaos-resource FAIL (→ 1615). docker-deploy/chaos-shutdown N/A (no compose file,
unchanged). Agent destroyed and absence verified. No SKIPPED row needed; the
bunker-las-02 default-server outage is noted here as infra evidence.

## Verdict

✅ SHIPPABLE. Promise holds; API/MCP/persistence/spawn contracts verified live;
every previously-measured slow path is now fast; docs remain the fleet's best
(endpoint parity tests, api.md curl recipes work verbatim). The one live system
concern is the satellite-starvation question (1614) — filed for the foreman.
