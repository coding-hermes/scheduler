# Dogfood Integration Report — 2026-08-25

**Project:** coding-hermes-scheduler — the fleet scheduler (Go, SQLite/WAL,
REST + MCP + htmx dashboard).
**Verdict:** 🟡 PROMISING-BUT-ROUGH — REST surface is excellent and battle-tested;
the MCP and spawn→poll automation surfaces have real breaks.
**Run:** deep dogfood 2026-08-25 (third run; prior: 08-04, 08-15). Live daemon
probed read-only at `127.0.0.1:9090`; full write-path lifecycle exercised on a
scratch daemon (`bin/schedulerd --db /tmp/dogfood-sched/life.db --listen
127.0.0.1:9093`) with throwaway data.

---

## What a real user does (and what happened)

### 1. "Is it alive?" — ✅ 1 second
`GET /api/v1/health` → `{"status":"ok","db":"connected","active_ticks":0,
"evaluation_age_seconds":…,"spawns_http":174,"uptime":"9h45m"}`. Instant,
informative. `/api/v1/status` gives the full fleet picture incl. per-project
failure rates and auto-disable state. No stalls this run (08-04 hit 13s spikes).

### 2. Register a project — ✅
`POST /api/v1/projects` with `{"name","repo_url","workdir"}` → 201, created
DISABLED with defaults weight 10 / priority 5 / cooldown_s 900 / decay 1.
Duplicate name → 409. Same workdir as an enabled project → 409 with a clear
case-insensitive-dup message. `PUT` accepts snake_case AND legacy PascalCase
(snake wins). Validation: weight 1..100 / priority 1..10 / decay>0 → 400 with
the exact rule in the message. Bad JSON → 400 with the parse error.

### 3. Delete a project — ✅ (soft + hard)
`DELETE /projects/{name}` → 400 "confirm=true required"; while enabled → 409;
disabled + `?confirm=true` → 200 soft-delete (row kept, `disabled_by=api-delete`);
`?confirm=true&purge=true` → 200 "purged" and the row is really gone (GET → 404).
DOGFOOD-009 (no purge) is FIXED. Note: soft-deleted rows accumulate forever —
keep scratch lifecycle tests to a minimum (the fleet DB already carries 3+).

### 4. Namespaces — ✅
POST create (201; `enabled` defaults false — send `"enabled":true` per
docs/api.md §6 example), dup → 409, PUT enable, POST `/{id}/move` with
`{"project":…}` assigns the project, GET `/{id}/projects` lists members. All
worked on scratch.

### 5. Trigger work (spawn / evaluate) — 🟡 one broken contract
`POST /api/v1/evaluate` → fires an eval cycle. `POST /projects/{name}/spawn`
→ 202 with `{"status":"spawned","tick_id":"<ID>"}` and a tick row appears —
**but `GET /ticks/{tick_id}` with the returned ID → 404** (see §Break 2).
Without gateway credentials the tick fails cleanly and is recorded:
`{"error":"no gateway client and exec fallback disabled for <proj>"}` —
the documented safety default, graceful.

### 6. Drive it via MCP — 🔴 one dead tool
`POST /mcp` JSON-RPC: initialize handshake OK; tools/list → the 14 `fleet_*`
tools; 13/14 tools/call worked (status, projects, detail, set_weight,
set_priority, set_cooldown, set_decay, pause, resume, evaluate, ticks).
**`fleet_add` fails on every call** (see §Break 1). The `/fleet` Hermes plugin
(plugin/hooks.py) dispatches slash commands through these MCP tools, so
`/fleet add` is dead too; the symlink itself is fixed (DOGFOOD-008).

### 7. Watch the fleet — ✅
Dashboard renders: `/` (92KB fleet overview), `/queue` (urgency bars now
visible — GAP-055 fixed), `/ticks?page=N` paginated, `/health`, per-project
pages, `/namespaces/{id}` (404 for a bogus id is correct). Events:
`GET /api/v1/events?limit=N` returns newest-first JSON with `count`. MCP
fleet_ticks returns PascalCase (see §Break 3).

### 8. The 5-minute README path — 🟡
`make build` → `bin/schedulerd` + `bin/migrate` exist. `bin/migrate -dry-run`
→ "0 imported, 2 skipped" with **no reason** for ordinary jobs (see §Break 4).
`--test-verify 3` on a scratch DB → "6 checks, 0 failures ✅ SCHEDULER
VERIFIED" (the 2-hourly verify harness works standalone). `--sim-count 5` →
5 simulated ticks, exit 0 (DOGFOOD-007 crash FIXED). `--sim-setup` creates the
13-project fixture.

### 9. Read the machine contract — ✅ (OpenAPI)
`GET /api/v1/openapi.json`: 19 paths matching the live route table, 11 paths
with requestBody, 6 component schemas (GAP-057 FIXED). docs/api.md is
live-verified and matches reality (I spot-checked every error code table).

---

## The breaks (evidence)

### Break 1 (P0): MCP `fleet_add` always fails
```
tools/call fleet_add {"name":"mcp-added","repo":"https://…","workdir":"/tmp/…"}
→ error -32000: "create project \"mcp-added\": constraint failed:
  CHECK constraint failed: priority >= 1 AND priority <= 10 (275)"
```
`internal/mcp/handlers.go` toolFleetAdd builds `Project{Name,RepoURL,Workdir,
Weight}` — no Priority, no default. REST create defaults priority=5; MCP doesn't,
so the DB CHECK constraint rejects every add. The error is a raw sqlite string
with no hint. Any agent onboarding a project via MCP (or `/fleet add`) is stuck.
Workaround: create via REST instead.

### Break 2 (P1): spawn returns an unresolvable tick_id
```
POST /projects/dogfood-tick/spawn → 202 {"tick_id":"dogfood-tick-2026-08-26-04-10-52"}
GET  /ticks/dogfood-tick-2026-08-26-04-10-52  → 404 {"error":"tick not found"}
GET  /ticks/dogfood-tick-2026-08-25-23-10-52  → 200   (stored id, local time)
```
Same wall-clock second, two renderings. `server_projects.go:388` formats
`time.Now().UTC()`; `slot_pool.go:144` formats the eval loop's LOCAL `now`.
On a -05:00 host they differ by 5 hours. Live daemon shows the same split
(its stored ids are local-rendered, e.g. `muster-2026-08-25-23-10-57`).
Workaround: after spawn, list `/ticks?project=<name>` and use the listed id.

### Break 3 (P2): MCP fleet_ticks is PascalCase
`fleet_ticks` → `{"ticks":[{"ID":…,"ProjectName":…,"Status":…,"Outcome":…,
"SpawnedAt":…}]}` while REST and the other 13 MCP tools are snake_case.
An agent mixing surfaces gets two dialects (S06 conformance missed this path).

### Break 4 (P2): migrate silently skips + undocumented filters
`bin/migrate` only imports jobs whose name/skills contain "coding-hermes" or
"foreman" AND whose prompt contains a workdir path. Jobs failing the first
check are skipped with NO log line — "0 imported, 2 skipped" and nothing else,
in both -dry-run and real mode. README step 4 presents it as a general
"imports from ~/.hermes/cron/jobs.json". Workaround: name jobs
`<proj> coding-hermes-foreman` and put `Workdir: /abs/path` in the prompt.

### Break 5 (P2): eval-stall alarm noise
27 `eval loop stalled — forced re-evaluation (recovered)` MEDIUM events in the
24h before 2026-08-25T23:00 (one every ~30-60 min, `age_seconds` 300-330 each),
plus ~571 "evaluation started" events on 08-25. SCHED-GAP-061's pass criterion
("0 HIGH in 24h") was met by demoting severity; the hourly stall/force-recovery
cycle itself persists. The alarm stream buries real escalations (e.g. 26 HIGH
rethinkdb consecutive-failure events).

---

## What held up (promise vs reality)

| Promise (README/AGENTS) | Reality 2026-08-25 |
|---|---|
| One binary replaces N cron jobs | ✅ True; fleet runs on it (174 HTTP spawns/9h45m uptime) |
| Event-driven eval + watchdog | ✅ Works; watchdog noise is the only blemish |
| REST API (docs/api.md contract) | ✅ 19/19 routes live; every guard/error code verified |
| MCP 14 fleet_* tools | 🟡 13/14 work; fleet_add broken; fleet_ticks dialect drift |
| /fleet plugin slash commands | 🟡 loads (symlink fixed); `add` inherits the fleet_add break |
| migrate tool (README step 4) | 🟡 works only for coding-hermes-named jobs, silently |
| Dashboard | ✅ renders; urgency bars fixed |
| OpenAPI machine spec | ✅ 19 paths + requestBodies (GAP-057 fixed) |
| Sim harness | ✅ --sim-count fixed; --simulate still real-spawner (known trap) |

## Time-to-first-success
~1 second (health probe). Full lifecycle (create→update→delete→purge) on
scratch: ~5 minutes including reading docs. MCP add-project: never succeeds.

## What to fix first (1 hour of maintainer time)
1. Default `Priority: 5` in toolFleetAdd (+ weight range validation) — one
   line + a test unblocks the entire MCP add surface.
2. Use `database.NextTickID` in slot_pool.go's Spawn so stored ids match the
   spawn endpoint's returned ids — restores the documented spawn→poll contract.
3. snake_case the fleet_ticks mapping; add the MCP wire check to the battery.

## Scratch setup used (reproducible)
```bash
rm -f /tmp/dogfood-sched/life.db
./bin/schedulerd --db /tmp/dogfood-sched/life.db --listen 127.0.0.1:9093 \
  --min-interval 30s --max-concurrent 2 --no-exec-fallback
# lifecycle tests via curl; spawn tests expect the documented
# "no gateway client and exec fallback disabled" failure (no creds → safe)
```
