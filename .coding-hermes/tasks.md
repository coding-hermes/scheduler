# Task Board — coding-hermes-scheduler

## [x] INIT — Bootstrap project structure **✓ ccbcbcf**
- [x] Go package layout, Makefile, systemd unit, README, .gitignore, GitReins guards

## [x] DB — Implement SQLite data layer **✓ e91ab0f**
- [x] Schema, migrations, CRUD for projects/ticks/events — 29 tests passing

## [x] SPEC — Write axiom-level implementation specs **✓ ce99654 + f5d3445 + 43c1442**
- [x] SPEC-S01 — System architecture spec **✓ 422 lines**
- [x] SPEC-S02 — Data model spec **✓ 416 lines**
- [x] SPEC-S03 — Urgency calculator spec **✓ 354 lines**
- [x] SPEC-S04 — Weight-budget packer spec **✓ 312 lines**
- [x] SPEC-S05 — Spawn engine + tick lifecycle spec **✓ 390 lines**
- [x] SPEC-S06 — REST API spec **✓ OpenAPI 3.0.3**

## [x] CORE — Implement from specs **✓ 71d5b3c**
- [x] urgency.go — geometric interval + urgency calculator (101 lines)
- [x] packer.go — weight-budget greedy packer with cooldown (115 lines)
- [x] spawn.go — hermes chat spawn + session_id capture (198 lines)
- [x] lifecycle.go — tick state machine + outcome persistence (119 lines)
- [x] loop.go — 60s evaluation loop with pause/resume (158 lines)

## [x] API — REST API server **✓ cadc05b**
- [x] 15 endpoints: health, status, projects CRUD, ticks, events, pause, resume, evaluate

## [x] MCP — MCP server **✓ 938ca1e**
- [x] 14 fleet tools, JSON-RPC 2.0, initialize/tools/list/tools/call

## [x] DASH — Dashboard **✓ 57cad52**
- [x] Dark theme single-file HTML, fleet overview, budget bar, project table, tick history

## [x] SYNC — DuckBrain read-replica **✓ e45f799**
- [x] 5-min sync, fleet summary + per-project status

## [x] PLUGIN — Hermes plugin **✓ e45f799**
- [x] plugin.yaml, __init__.py, hooks.py — 17 slash commands

## [x] CMD — Main entry point **✓ 9672322**
- [x] Wired: API + MCP + Dashboard + DuckBrain sync in one binary

## [x] MIGR — Migration tool **✓ c4e9ca0**
- [x] 33 cron → scheduler import, dry-run mode

## [x] DEPLOY — Deployment config **✓ c4e9ca0**
- [x] systemd unit, Makefile targets

---

## HILO GAP ANALYSIS — 18 files, 114 edges, 18 orphans found

## [ ] GAP-001 — Duplicate boolPtr across packages
- `api/server.go:277` defines `func boolPtr(b bool) *bool`
- `mcp/server.go:493` defines `func boolPtr(b bool) *bool`  
- `database/projects.go:190` defines `func boolPtr(b bool) *bool`
- Fix: export from database package, api + mcp use `database.BoolPtr`

## [ ] GAP-002 — No tests for 5 core packages
- `internal/scheduler/` — urgency, packer, spawn, lifecycle, loop: 0 tests
- `internal/api/` — 15 endpoints: 0 tests
- `internal/mcp/` — 14 tools: 0 tests
- `internal/dashboard/` — template rendering: 0 tests
- `internal/sync/` — DuckBrain sync: 0 tests

## [ ] GAP-003 — No integration test
- Nothing proves API + MCP + Dashboard + Loop + Sync coexist
- Spin up schedulerd, hit /health, /, /mcp, /api/v1/projects in one test

## [ ] GAP-004 — Sync uses os/exec for DuckBrain
- `internal/sync/duckbrain.go:115` calls `hermes mcp duckbrain remember` via shell
- Should use DuckBrain MCP client directly or a proper HTTP call

## [ ] GAP-005 — Plugin not registered with Hermes
- `plugin/hooks.py` exists but not symlinked to `~/.hermes/plugins/coding-hermes/`
- No trigger cron created to hit `/api/v1/evaluate`

## [ ] GAP-006 — Migrate tool not run
- 33 coding-hermes foreman crons still running on Hermes cron scheduler
- `make migrate-dry` → `make migrate` → cutover trigger cron

## [ ] GAP-007 — Unused imports and dead code
- `mcp/server.go:1` imports `log` and `time` with `var _ = log.Printf; var _ = time.Now` guards
- Clean up dead code before production deploy

## [ ] DEPLOY — Production cutover checklist
- [ ] Run `make migrate-dry` and verify 33 projects
- [ ] Run `make migrate` to create SQLite records
- [ ] `make deploy-install` for systemd unit
- [ ] Create trigger cron: `*/1 * * * * curl -X POST http://localhost:9090/api/v1/evaluate`
- [ ] Symlink plugin: `ln -s ~/coding-herms-scheduler/plugin ~/.hermes/plugins/coding-hermes`
- [ ] Disable old 33 cron jobs via `cronjob action=update job_id=<id> enabled=false`
- [ ] Verify: dashboard loads, /fleet status works, first foreman tick spawns
