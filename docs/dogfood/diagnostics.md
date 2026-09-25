# Diagnostic Trail — coding-hermes-scheduler (dogfood 2026-08-04)

This is the "how it's built, what broke, the right way" record. It explains
the system and the errors encountered — from the project's own history AND
this run — so future agents can answer "does this thing actually work?" from
the repo, not from test colors.

## How it's built

- **Go 1.26+ daemon** (`cmd/schedulerd/`), `net/http` ServeMux, Go 1.22+
  method patterns. Single binary `bin/schedulerd`.
- **SQLite via modernc.org/sqlite** (pure Go, WAL mode). Live DB:
  `~/.hermes/coding-hermes/scheduler.db`. Schema: projects,
  namespaces, ticks, events, migrations. Board state (`.coding-hermes/board/`)
  is separate: DuckDB live store + JSONL git mirror (see INFRA-013).
- **Scheduler loop** (`internal/scheduler/`): every 60s evaluate → urgency =
  f(cooldown age, priority, decay_rate) → greedy weight packing into a 100-unit
  budget with multi-pool namespace mode → spawn via Hermes gateway HTTP API
  (`POST /v1/responses`, `require_approval:false`) with `--no-exec-fallback`
  exec spawn as fallback. Cooldown per project; timeout does NOT back off
  (deliberate); configurable auto-disable (SCHED-GAP-018, default off — `--auto-disable-failure-rate` > 0 to enable; the 10+ consecutive timeouts/24h safety net remains).
- **REST API** (`internal/api/`): 20 method/path ops under `/api/v1` — health,
  status, projects CRUD-ish (no delete), ticks, events, namespaces, evaluate,
  pause/resume. **MCP** (`internal/mcp/`) JSON-RPC on `/mcp` with fleet_* tools.
  **Dashboard** (`internal/dashboard/`): htmx + server-rendered HTML.
  **DuckBrain sync** (`internal/sync/`): pushes fleet state to :3000 every 5m.
- **Deploy verify**: host crontab runs `deploy/scheduler-verify.sh` every 2h →
  `./bin/schedulerd --test-verify 3` (self-contained E2E: temp DB, 7-project
  fixture fleet, N cycles, invariant checks). Logs to `deploy/verify-*.log`.

## Errors hit during this run (and their root causes)

### 1. POST /api/v1/projects — 400 on documented body, 500 on minimal body
- Symptom A: `{"name":"ch-alpha","repo_url":"x","workdir":"/tmp"}` →
  `400 {"error":"name, repo_url, workdir are required"}`.
  Cause: `internal/database/models.go` and `ProjectUpdates` have **no json
  tags**, so Go's decoder binds exported names (`Name`, `RepoURL`...) and the
  snake_case request keys bind to nothing → required-field check fires.
  Documented in S06 §1.1 as a known gap; never fixed. **The spec's own
  example body cannot create a project.**
- Symptom B: PascalCase minimal body → `500 create project "x": CHECK
  constraint failed: weight >= 1 AND weight <= 100`. Cause: handler fills no
  defaults for zero-valued fields (S06 §5.1 admits this), and the schema's
  CHECK rejects weight=0. The documented 409 duplicate path is unreachable —
  constraint errors fire before the duplicate check.
- Right way today: send `{"Name","RepoURL","Workdir","Weight":1-100,...}` all
  PascalCase. Fix direction: DOGFOOD-001 (json tags + defaults + 409).

### 2. deploy verify RED for 4+ days, board claims green
- Symptom: `deploy/verify-*.log` → `VERIFY FAILED: create beta: create project
  "beta": workdir "/tmp/scheduler-verify-<rand>" already registered by enabled
  project "alpha" (case-insensitive duplicate)`. 191/207 logs FAIL (0 PASS),
  continuous streak since 2026-07-31T08:00.
- Cause: `cmd/schedulerd/test_verify.go` registers 7 fixture projects **all
  with `Workdir: tmpDir`**. The duplicate-workdir guard (added `bc438e6`,
  `9f9d6a5` ~tick #184, to prevent ghost projects after the `heading` incident
  INFRA-009) rejects project #2 ("beta"). The guard is correct; the fixture is
  stale. The foreman's tick #185 audit said "dup-workdir check verified" —
  verified at the unit level, never ran `--test-verify` (L3). The foreman gate
  suite (`go test -p 1`, lint, govulncheck, gitreins guard) does not include
  the crontab verify, so the red gate was invisible to the board loop.
- Right way: each fixture project gets its own subdir under tmpDir; add
  `--test-verify` to the gate suite; NEVER-DONE audit greps
  `deploy/verify-*.log` for FAIL. Fix direction: DOGFOOD-002.

### 3. Mixed JSON dialects
- projects/ticks/events serialize PascalCase; namespaces serialize snake_case.
  Root cause: models.go lacks json tags (gap #1) while the namespace model has
  them. README `jq '.project_count'` is wrong (field is `active_projects`).
  Impact: any consumer following S06 gets nulls; the ecosystem scripts
  (stand-in, pickers) work only because they reverse-engineered PascalCase.
  Fix direction: DOGFOOD-003.

### 4. 13s read stalls
- `GET /api/v1/projects` twice took 13.07s/10s+ while `/status` p50 was 38ms.
  Working hypothesis: SQLite WAL checkpoint contention while 6 ticks write
  continuously. Not root-caused in this run (time-boxed) — measurement task
  DOGFOOD-006 with `busy_timeout`/checkpoint tuning candidates.

### 5. Open question: 69% tick failure rate
- `recent_outcomes`: 10,105 completed / 22,274 failed / 331 timeout.
  Escalation events keep firing ("project starved: my-project — last tick
  3h22m ago, cooldown 900s") — some enabled projects (e.g. `my-project` with a
  placeholder repo) appear to be spawned and fail repeatedly. Whether this is
  test-junk, config drift, or a real packer/failure-loop bug needs an audit
  (folded into DOGFOOD-006's observability ask; NEVER-DONE performance audit
  should own it).

## Historical context from the board (things already learned the hard way)

- **INFRA-012 (tick #190-191):** daemon restart marked live gateway ticks
  timeout → duplicate spawns. Fixed: only reap dead pid>0 ticks on startup.
- **REC-ZOMBIE-OUTCOME (tick #192):** `outcome=zombie_reaped` violated the
  ticks CHECK → every reap silently no-oped (SQLite rejects whole UPDATE).
  Fixed by dropping outcome from the UPDATE; also fixed a rows-open-during-
  UPDATE deadlock. Lesson: SQLite CHECK constraints silently kill multi-row
  UPDATEs — always test the reap path end-to-end.
- **INFRA-009/010 (tick #183):** ghost `heading` row deleted; dup-workdir and
  decay_rate=0 guards added at API level — the very guards that later broke
  --test-verify (this run) and that `fleet_set_decay` now enforces.
- **INFRA-003:** tick storms (cooldown < tick_timeout) — preemptively solved
  by config (min cooldown 900s > timeout 600s... note daemon now runs
  `--tick-timeout 7200s`, so re-check this invariant).

## The right way (summary)

1. Read the wire, not the spec, for field names (PascalCase; namespaces
   snake_case) — or fix the tags (DOGFOOD-001/003).
2. Never create throwaway projects (no delete endpoint) — DOGFOOD-005.
3. Treat `--test-verify` as the L3 gate; board "gates green" ≠ verified.
4. Writes to the board: append JSONL records to `.coding-hermes/board/tasks.jsonl`.
5. All control endpoints are idempotent and loopback-only — safe to probe;
   only POST /projects with a real intent (it persists forever).

---

# 2026-08-15 follow-up run — what changed, errors hit, the right way

Second dogfood (first: 2026-08-04). All six DOGFOOD-001..006 fixes verified
live. This section records the NEW errors/lessons; details in
`2026-08-15-integration.md` and board tasks DOGFOOD-007..013.

## How the sim/simulation path is actually built (and why it misleads)

- `Loop` holds BOTH a real `spawner` and a `simSpawner`
  (`internal/scheduler/loop.go`, `sim_spawn.go`). `SetSimulation()` only sets
  `l.simulate` + the success rate — it never swaps `l.spawner`. The main
  `Run()` loop therefore ALWAYS spawns via the real path: gateway client or
  (if disabled/absent) "no gateway client and exec fallback disabled".
- `simSpawner.Spawn` is used only in `RunBulkSim` (`--sim-count`) and the
  `--sim-setup` fixture. `RunBulkSim`'s ticker fires every 500ms but IDs are
  `sim-<project>-<HHMMSS>` (1s granularity) → second tick in the same second
  hits the `ticks.id` UNIQUE constraint → FATAL.
- **Right way today:** `--sim-setup --sim-ticks N` (works end-to-end, 13
  fixture projects) or `--test-verify 3` (full E2E + perf + failure audit).
  `--simulate` is a log-string-only no-op; `--sim-count` crashes. Task:
  DOGFOOD-007.

## Soft-delete mechanics (found by lifecycle test)

- `DELETE /api/v1/projects/{name}?confirm=true` → `deleteProject` soft-deletes:
  `enabled=0` + provenance stamp (`disabled_by='api-delete'`,
  `disabled_reason='soft-deleted via DELETE ?confirm=true'`). Row remains
  visible in list/detail. 409 guard while enabled ("pause it first").
- Hard deletes (SQL `DELETE FROM projects`) orphan the rows' ticks, which
  then live forever in `projects_failure_rates` (eduos/eduos-e2e/helios-work
  — eduos-e2e shows rate=1.0, `auto_disable_armed=true` with no row).
- **Right way:** treat DELETE as pause+mark; if you need a clean DB for
  scratch work, use your own `--db` instance, not the live one. Task:
  DOGFOOD-009.

## Config reality (schema vs loader)

- `internal/config` has a `RootConfig`/`LoadConfig` for `schedulerd.toml`
  (unit-tested) but `cmd/schedulerd/main.go` never calls it — only the fleet
  TOML loader (`--config fleet.toml`) is wired. `--schema` documents the
  un-wired root file; `--show-config` prints "CLI flags only" and echoes env
  overrides as comments (env vars ARE applied at boot — verified
  `SCHEDULER_AUTO_DISABLE_FAILURE_RATE=0.5` → rate 0.50 in the boot log).
- **Right way:** treat env vars as real config (auto-disable family at
  least), treat `schedulerd.toml` as not-yet-loaded, and don't use
  `--show-config` to audit effective values. Task: DOGFOOD-012.

## Board append mechanics (for future dogfood/stand-in runs)

- Board = `.coding-hermes/board/tasks.jsonl` (git) + `board.db` (DuckDB
  mirror). The DuckDB `tasks` table has NO primary key → `INSERT OR IGNORE`
  fails with "no UNIQUE/PRIMARY KEY constraints"; mirror with
  `SELECT count(*) WHERE id=?` then plain INSERT (foreman's pattern:
  "lockstep pre-append" to both stores).
- `migrate-board-to-duckdb.py` is one-shot (tasks.md → DuckDB), not an append
  tool. `validate-board-format.py` + `fleet-board-audit.py` exist for checks.

## Environment facts worth knowing

- Live daemon: systemd user unit, `-db ~/.hermes/coding-hermes/scheduler.db`,
  `-config /home/kara/.hermes/fleet.toml`, gateway `:8642`, exec fallback
  disabled, auto-disable at 0.9 (sim-beta/sim-delta/eduos-e2e all armed at
  rate 1.0 but already disabled/deleted — auto-disable has nothing left to
  fire on).
- `~/.hermes/plugins/coding-hermes` → typo'd path (DOGFOOD-008) — /fleet
  slash commands dead. The fleet runs entirely on the API/MCP/dashboard.
- Verify cron green; eval-stall watchdog fires ~30-min cadence when the fleet
  idles (by design, GAP-042); `EVAL-ZERO-SELECT` counters at 0.

---

# Diagnostic Trail — 2026-08-25 (dogfood run 3)

## How the thing is actually built (verified this run)

- **One daemon, three surfaces, one SQLite DB (WAL).** `bin/schedulerd` serves
  REST `/api/v1/*`, MCP JSON-RPC at `/mcp`, and the htmx dashboard at `/`.
  Routes registered in `cmd/schedulerd/main.go`; REST handlers in
  `internal/api/*`; MCP handlers in `internal/mcp/handlers.go`; scheduler core
  (eval loop, slot pool, cooldown/urgency) in `internal/scheduler/*`; DB in
  `internal/database/*` (modernc.org/sqlite, pure Go).
- **Spawn pipeline:** REST `POST /projects/{name}/spawn` enqueues via
  `loop.ForceEvaluate()`; the eval loop packs the most urgent projects into the
  weight budget and `SlotPool.Spawn` fires gateway-HTTP foreman sessions
  (`POST /v1/responses`, `require_approval:false`), with exec.Command fallback
  disabled by default (`--no-exec-fallback`). Every tick is a DB row
  (queued→running→completed/failed) with tokens/cost when the session reports
  them — verified live: completed tick carries `tokens_in:40711,
  cost_usd:0.0058`.
- **Eval loop is event-driven** (startup + slot-freed debounce + manual
  evaluate) with a 30s health-ticker watchdog that forces a re-eval when
  `lastEval` age > 10×min-interval (300s) with 0 running ticks (GAP-042).
  *Observed:* the forced re-eval fires ~hourly, not every 5 min, and logs
  MEDIUM "eval loop stalled — forced re-evaluation (recovered)" each time.
  Open question for the foreman: why ~1/hr instead of the designed 5-min
  cadence (DOGFOOD-018).
- **Wire dialect:** REST is snake_case everywhere with envelopes
  (`{"projects":[...]}`, `{"project":{...},"latest_tick":...}`); requests
  accept legacy PascalCase too. MCP is snake_case EXCEPT `fleet_ticks`, which
  still emits PascalCase (DOGFOOD-016) — S06 conformance covered REST only.
- **Board:** `.coding-hermes/board/tasks.jsonl` is the source of truth;
  `board.db` is a DuckDB mirror (schema.sql in the same dir; no PK on tasks —
  mirror inserts need delete-then-insert). `board.jsonl` is the pointer file.
  Task rows are JSON objects with status/priority/complexity/worker_* fields.

## Errors hit this run and the right way

1. **`CHECK constraint failed: priority >= 1 AND priority <= 10 (275)` on MCP
   fleet_add** — raw sqlite error surfaced to the MCP client. Right way: MCP
   handlers must default fields the REST path defaults (Priority:5) and map DB
   constraint errors to actionable messages. Root cause: toolFleetAdd builds
   Project without Priority; REST create defaults it. (DOGFOOD-014)
2. **spawn tick_id 404 on GET /ticks/{id}** — two ID generators disagree on
   timezone: `server_projects.go:388` (UTC) vs `slot_pool.go:144` (local).
   Right way: single canonical generator (`database.NextTickID`, UTC) in both
   places + round-trip regression test. (DOGFOOD-015)
3. **`bin/migrate` → "0 imported, 2 skipped", no reason** — the eligibility
   gate `isCodingHermesJob` (name/skills must mention coding-hermes/foreman)
   skips WITHOUT logging; only the second gate (workdir in prompt) logs.
   Right way: log every skip with its reason; document the gates in README.
   (DOGFOOD-017)
4. **Port collision on scratch runs** — 127.0.0.1:9091 was already taken by an
   unknown process; the daemon exits with "bind: address already in use"
   AFTER printing its banner (routes registered before listen). Right way: pick
   a free port first (`ss -tlnp`) — 9093 worked. Minor UX: bind failure should
   exit before the banner.
5. **sqlite3 can't read board.db** — it's DuckDB, not SQLite ("file is not a
   database"). Right way: `python3 -c "import duckdb"` (module is installed).
6. **SSE claim vs reality** — AGENTS.md says events support SSE; the endpoint
   returns the same JSON for `stream=1`, `stream=true`, and
   `Accept: text/event-stream`. Either implement SSE or drop the claim.
   (folded into DOGFOOD-016)

## The right way (condensed)

- **Probe first:** `/api/v1/health` → `/api/v1/status` → `/api/v1/projects`.
  Everything else is read-only-safe; writes go to a scratch daemon
  (`--db /tmp/... --listen 127.0.0.1:9093 --no-exec-fallback`) so a missing
  gateway key makes spawns fail *safely and recorded*.
- **Create projects via REST** (works); avoid MCP fleet_add until DOGFOOD-014
  lands.
- **Never poll by spawn's returned tick_id** until DOGFOOD-015 lands — list
  ticks for the project instead.
- **Verify harness:** `./bin/schedulerd --db /tmp/x.db --test-verify 3` →
  "SCHEDULER VERIFIED" is the L3 check that `go test` doesn't run.
- **Read fleet state:** docs/fleet.md regenerated by docs/regenerate_fleet.py;
  live source of truth is the daemon API (74 projects/42 enabled 2026-08-25).

## 2026-09-09 dogfood run (addendum — see 2026-09-09-integration.md)

**What was exercised:** live read-only probes (prod v1.1.1-58-g611828a, 61h
uptime), full operator workflow on a `--simulate` scratch daemon (port 9123,
scratch DB), `--sim-setup --sim-ticks 10` harness, `make build`, migrate
`--help`, `--test-verify 3`, `go test -short -p 1 ./...`.

**Findings this run (board: DOGFOOD-020..023):**

1. **GAP-101 still live at HEAD 5d0ab6f** (== origin/main). Redundant
   `POST /api/v1/resume` on an unpaused `--simulate` loop re-wedged evaluation
   ("LOOP: paused" in loop.go:336, no ticks afterward, `paused` null in
   status). `git log -S pauseCh` shows the channel logic untouched since the
   initial core commit c8fcb13 — the fix filed 2026-09-06 never landed. Anyone
   operating the live daemon must treat resume as non-idempotent.
2. **Sim-setup intermittent boot FATAL**: `--sim-setup` on a brand-new DB
   failed once in 7 runs with `FATAL: sim setup: clear projects: constraint
   failed: FOREIGN KEY constraint failed (787)` at main.go:328. On an empty DB
   this implies a boot-time writer inserts child rows before SimFixture.Setup
   wipes projects (sim_fixture.go:67) — a race, not corruption. Retry
   succeeds. Docs should warn; code should order/guard the wipe.
3. **Spawn-path counters contradiction**: prod /api/v1/health reports
   spawns_exec=2415 vs spawns_http=66 while `--no-exec-fallback` defaults true
   and gateway_errors=0. Either counters mislabel or the unit predates the
   default — needs reconciliation, then README ("zero subprocess overhead")
   or the unit config must match.
4. **Dashboard scale**: `/` serves a 224KB page for 178 projects (~2.2s cold,
   one 5s+ flake observed) — fine today, watch as fleet grows; consider
   pagination.
5. **Installability NOT verified this run**: las-bunker-03 (100.69.3.13)
   unreachable (ssh timeout x2, ping 100% loss, :19090 timeout). Filed
   DOGFOOD-023; re-run the ephemeral install leg when the box is back.

**Right way (updated):** scratch-testing the scheduler is safe and cheap —
`--simulate` on a free port with a scratch DB; add `cooldown_s:1` projects to
see ticks in seconds; never trust the spawn API's returned tick_id blindly
(still DOGFOOD-015's territory); check `ss -tlnp` before choosing a port
(9091 is squatted by an unrelated local service).

## 2026-09-24 dogfood run (addendum — see 2026-09-24-integration.md)

**The "watch as fleet grows" item arrived.** The 2026-09-09 run measured `/`
at 224KB / 178 projects / ~2.2s cold and said "watch as fleet grows; consider
pagination". At 480 lanes (289 enabled) the same page is 355KB and costs
~45s — and not just cold: the cache cycle re-pays it (~60s TTL). The growth
curve crossed usability somewhere in the last 300 lanes; pagination or
lazy-render is no longer optional polish.

**What was exercised:** the previously-unswept surface — the htmx dashboard
as a real operator (`/`, `/dashboard/partial`, per-project, queue, ticks,
health, namespaces), the README quickstart verify commands, the `/debug/pprof`
self-profiling surface, the board JSONL (id census + row format), and the
fresh-install battery on bunker-las-02.

**Findings this run (board: SCHED-GAP-1576 new; SCHED-GAP-1575 updated with
the proven mechanism; SCHED-GAP-1577 new):**

1. **One mutex is the whole story.** `Loop.evaluate()` holds the Loop
   write-mutex across the entire pass (tick_process.go:19);
   `ForceEvaluate()` = `go l.evaluate()` with no coalescing (loop.go:682);
   `GatewayResponseTimeout()` takes the same write lock to READ one value
   (loop.go:422). Board-wake storms queue ~180 evaluate goroutines (ages
   28–80 min observed) and every status/dashboard request joins the tail.
   The 2026-09-23 "API dark" outage (SCHED-GAP-1575) was this shape at its
   limit; the ~45s reads are it on an ordinary day. The fix directions are
   ranked: coalesce ForceEvaluate; split/shrink the critical section; read
   status fields through atomics; handler deadlines.
2. **Render cost is dominated by per-project enrichment**: `readGitReins`
   walks every workdir's `.gitreins/history` on each cache-cold render
   (21.7% of a 72%-busy profile). Cheap fix: background refresh or per-
   project cache; the payload itself (621 rows) needs pagination regardless.
3. **The daemon's own WARN (`goroutine count > 100`) fired continuously at
   ~200 while I measured.** It is the right alarm wired to no action —
   SCHED-GAP-1575's ask 3 (act on the threshold) stands.
4. **Clean-machine test failure**: `TestCountPending_MtimeReread` failed on
   the fresh-install battery while passing on the dev box — the test relies
   on natural filesystem mtime behavior (SCHED-GAP-1577). Fix: build the
   fixture with explicit `os.Chtimes`.
5. **Right way for agents**: prefer `/api/v1/projects` (0.14s) and
   `/api/v1/events` (0.8ms) for automation; treat `/` and `/api/v1/status`
   as interactive-only until 1575/1576 close; per-project pages (0.17s),
   `/queue` (4.5ms), `/ticks` (0.2s) are all safe. SSE at
   `/api/v1/events/stream` streams immediately (the 08-25-era note that
   events had no SSE is outdated at HEAD).

**2026-09-25 (496-lane scale sweep) — how the dashboard's speed is actually made, and how "done" and "serving" diverge:**

6. **The dashboard's wall-clock is dominated by waiting, not rendering.** A render is
   "correct but slow" for two compounding reasons. First, `enrichProjects` runs a
   bounded pool (8-wide) whose workers shell out — `tickWork`
   (generator_data.go:1746) runs `git log --since --until --pretty=%s` as a
   subprocess per tick sample per project — so one page render is hundreds of
   fork/execs. The main handler parks on the pool's channel
   (generator_data.go:1027) and the goroutine dump shows exactly that shape: one
   handler in `[chan send]` at 1027, workers in `os/exec.(*Cmd).Output` at 1746.
   Second, the freshness walk (`ReadBoardFreshness` → `classifyCompleteRow`) burns
   31% of CPU under render load — but note it is mostly called by the BOARD-WAKE
   WATCHER and the PACKER, not the dashboard; the dashboard renders slowly partly
   because it shares SQLite and the machine with those loops. Lesson: when wall
   time >> CPU time for a handler, profile WAITING (goroutine dump), not CPU —
   the CPU profile alone would have blamed the wrong component.
   The right way: renders should serve a snapshot; enrichment and freshness belong
   to background loops with their own cadence, and per-sample git data should come
   from a source already in memory (gitreins history) instead of fresh subprocesses.

7. **"Merged + verified" and "deployed" are different facts, and only one of them
   protects the fleet.** SCHED-GAP-1619 (unauthenticated MCP mutations) was fixed
   (b16e61a8, 222 lines), judged PASS, and the row closed at 12:01Z. The running
   daemon (63dfbd10, built 01:28:07Z, 13 commits behind) still had the hole, and a
   two-line curl at 13:30Z proved it — a nonexistent-project probe was enough,
   because it cannot mutate anything while still proving the handler ran pre-auth.
   Same family: DASH-PERF-003's 0.87s closure number was real in the worker tree;
   the serving process measures 3.73s. The lesson is procedural: a closeout is not
   complete until something re-runs the row's own probe against the LIVE daemon
   (health endpoint exposes build_sha — compare it to the fix commit) and records
   that build. Filing rows that say "closed" while the box serves old code is how
   a fleet ends up believing its own board instead of its own process.

8. **Scale changes are when regressions hide behind old verdicts.** DASH-PERF-003's
   <3s criterion was verified at 44-ish lanes of dashboard load in August; the fleet
   is now 496 registered lanes and the same page takes 3.73s. Nobody did anything
   wrong — the fleet just grew past the number's validity domain. Performance
   criteria on boards should carry their input scale ("<3s at N lanes"), and a
   periodic dogfood/perf pass should re-run closed rows' exact commands when the
   input scale has moved materially (here +40% lanes since closure). A closed row
   whose command was never re-run at the new scale is an assumption, not a result.
