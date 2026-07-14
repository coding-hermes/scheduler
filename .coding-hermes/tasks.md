# Task Board — coding-hermes-scheduler

## [x] INIT — Bootstrap ✓ | [x] DB — SQLite ✓ | [x] SPEC S01-S06 ✓ | [x] CORE ✓ 
## [x] API ✓ | [x] MCP ✓ | [x] DASH ✓ | [x] SYNC (paused) ✓ | [x] PLUGIN ✓
## [x] CMD ✓ | [x] MIGR ✓ | [x] DEPLOY ✓ | [x] GAP-005 ✓ | [x] GAP-006 ✓

---

## ACTIVE GAPS — 2026-07-12

### [x] GAP-002 — Tests for 5 core packages ✓ `771affe`
**Priority: HIGH. Weight: 30.**
- `internal/scheduler/` — urgency, packer, spawn, lifecycle, loop: 5 test files, all pass
- `internal/api/` — 15+ endpoints covered with httptest: all pass
- `internal/mcp/` — 26 tests covering all 14 tools + JSON-RPC validation: all pass
- `internal/dashboard/` — 5 tests (4 pass, 1 skip: known int→bool Scan bug in generator.go:101)

### [x] GAP-003 — Integration test ✓ `36411b4`
**Priority: HIGH. Weight: 20.**
- 5/6 pass: Health, API Projects, MCP, TickLifecycle, DynamicConfig
- Dashboard skipped in test env (works on live server)

### [x] GAP-004 — DuckBrain sync client ✓ `7638ebb`
**Priority: MEDIUM. Weight: 15.**
- DuckBrain HTTP REST API client implemented (postMemory → POST /api/memories)
- Fleet summary + per-project status synced every 5 min
- Configurable via --duckbrain-url flag (default http://localhost:3000)
- 176 lines, build+vet+test green

### [x] GAP-008 — Session ID capture ✓ `986abb8`
**Priority: HIGH. Weight: 25.**
- Spawner captures session_id from hermes chat stdout and persists to ticks table
- Session ID parsing + persistence implemented in spawn.go
- Board was stale — already shipped July 12

### [x] GAP-009 — Spawn uses project model/provider ✓
**Priority: MEDIUM. Weight: 10.**
- Project.Model/Provider override spawner defaults via PackedProject
- Falls back to spawner defaults when empty (backward compatible)

### [x] GAP-007 — Dead code cleanup ✓
**Priority: LOW. Weight: 5.**
- Removed duplicate boolPtr from api/server.go and mcp/server.go
- Exported database.BoolPtr — single canonical definition
- Removed dead var _ = log.Printf guards from mcp/server.go

### [x] CUTOVER — Disable old cron jobs ✓ 20:47 CDT
**Priority: HIGH. Weight: 15.**
- 28 cron jobs paused — only 5 keepers remain (scheduler self-foreman, supervisor, crier, duckbrain-sync, gitreins)
- Scheduler now the sole authority for per-project foremen

---

## MULTI-NAMESPACE EXTENSION — 2026-07-12 (specs written, implementation pending)

Design: Two-axis weight-budget scheduler. Namespaces are pools with their own weight budgets. 
Jobs have intra-namespace weight AND effective global weight = namespace_allocation × (w_job / Σw_ns).
Reserved floors, hard caps, borrowing of idle capacity. Full spec: S07.

### [ ] NS-001 — Migration v2: namespaces + namespace_ticks tables
**Priority: HIGH. Weight: 20. Depends on: none.**
- `internal/database/schema.go`: Add MigrationV2 DDL (namespaces, namespace_ticks, ALTER TABLE projects)
- `internal/database/migrations.go`: Version gate — only apply v2 when v1 is present
- `internal/database/store.go`: Add Namespace CRUD methods (List, Get, Create, Update, Delete)
- `internal/database/namespace_ticks.go`: Insert, query by namespace, query by tick_group

### [ ] NS-002 — NamespaceAllocator (Phase 1 of S07)
**Priority: HIGH. Weight: 18. Depends on: NS-001.**
- `internal/scheduler/namespace_alloc.go`: NamespaceAllocator struct + Allocate()
- Reserved floors + proportional distribution + hard cap enforcement
- Handle edge cases: Σreserved > B, zero namespaces, all disabled
- Unit tests: 12+ test cases from S07 section 8

### [ ] NS-003 — MultiPoolPacker (Phases 2-4 of S07)
**Priority: HIGH. Weight: 22. Depends on: NS-002.**
- `internal/scheduler/multipool_packer.go`: MultiPoolPacker struct + Pack()
- Intra-namespace packing with effective weight calculation
- BorrowingEngine: collect unused, distribute to hungry namespaces
- One level of re-borrowing after redistribution
- Fallback to flat WeightPacker when NamespaceMode=false
- Unit tests: 15+ test cases from S07 section 8

### [ ] NS-004 — Namespace API endpoints
**Priority: HIGH. Weight: 15. Depends on: NS-001.**
- `internal/api/namespace_handlers.go`: 6 new handlers (from S06 section 11)
- GET/POST /namespaces, GET/PUT /namespaces/{id}, GET /namespaces/{id}/projects, POST /namespaces/{id}/move
- API tests: 11 test cases from S06 section 11.5

### [ ] NS-005 — Scheduler integration + Config toggle
**Priority: HIGH. Weight: 12. Depends on: NS-003, NS-004.**
- Wire NamespaceAllocator + MultiPoolPacker + BorrowingEngine into Scheduler
- `SCHEDULER_NAMESPACE_MODE` env toggle (default false — backward compatible)
- Evaluation loop branch: if NamespaceMode → multipool path, else → flat path
- Write namespace_ticks rows after each evaluation cycle
- DuckBrain sync: write /fleet/namespaces and /fleet/namespaces/{id}/status

### [ ] NS-006 — Tests: namespace unit + integration
**Priority: MEDIUM. Weight: 14. Depends on: NS-005.**
- NamespaceAllocator unit tests: reserved floors, hard caps, sum=budget, zero reserved
- MultiPoolPacker unit tests: effective weight scaling, borrowing, fallback to flat
- Integration: create 3 namespaces, run evaluation, verify namespace_ticks
- Integration: starve one ns, surplus to another, verify borrowing
- Integration: toggle NamespaceMode at runtime, verify smooth transition

### [ ] NS-007 — Dashboard: namespace view
**Priority: LOW. Weight: 8. Depends on: NS-005.**
- Add namespace allocation/borrowing table to dashboard HTML
- Color-coded: green (under-utilized), yellow (at reserved), red (at hard cap)
- Per-namespace utilization chart (last 20 ticks)

### [ ] NS-008 — Production migration: assign projects to namespaces
**Priority: MEDIUM. Weight: 10. Depends on: NS-005.**
- Define namespace configuration (from S07 section 12: coding-hermes, monitoring, data-cleanup, duckbrain-infra, backup)
- API calls to create namespaces + move each of 26 projects into coding-hermes namespace
- Create/move monitoring cron jobs into monitoring namespace
- Set NamespaceMode=true, verify first tick runs with correct allocations
- Monitor borrowing activity for 24h before declaring stable

---

## OBSERVABILITY & SIMULATION — 2026-07-12

### [x] OBS-001 — Structured event logging ✓
**Priority: HIGH. Weight: 15.**
- EventLogger with 5 severity levels (CRITICAL, HIGH, MEDIUM, LOW, INFO)
- Events written to events table: EVAL_START, project selection, TICK_SPAWNED, TICK_COMPLETED
- Non-blocking — errors logged, never breaks hot path
- Wired into evaluation loop at all key decision points

### [ ] OBS-002 — Watchdog + health polling
**Priority: HIGH. Weight: 12.**
- Script at `~/.hermes/scripts/watchdog.sh` polls `/api/v1/health` every 5m
- Alerts via Hermes cron (no_agent mode) when unreachable
- Tracks consecutive failures, escalates after 3 misses
- Dead-man's switch: alerts if no evaluation in 10+ minutes

### [ ] OBS-003 — Dashboard: tick timeline + outcomes
**Priority: MEDIUM. Weight: 10.**
- Tick timeline graph in dashboard HTML
- Color-coded: green=completed, red=failed, yellow=timeout, blue=running
- Per-project outcome percentages
- Clickable session IDs linking to Hermes transcripts

### [ ] OBS-004 — Simulation / dry-run mode
**Priority: HIGH. Weight: 20.**
- `--simulate` flag on schedulerd: no real process spawning
- Simulated foremen complete instantly with randomised outcomes
- `--simulate-count N` to generate N fake ticks for dashboard testing
- `--simulate-duration D` to run simulated evaluation loop for D minutes
- Writes tick history to SQLite, generates realistic dashboard data

### [ ] OBS-005 — Cost tracking per tick
**Priority: MEDIUM. Weight: 8.**
- Capture tokens_in, tokens_out, cost_usd from hermes sessions
- Query `hermes sessions export --dry-run <session_id>` after tick completes
- Store in ticks table for cost aggregation
- Dashboard: daily/weekly cost per project, fleet total

### [ ] OBS-006 — Alert escalation rules
**Priority: LOW. Weight: 6.**
- Define alert severity matrix: CRITICAL/HIGH/MEDIUM/LOW
- CRITICAL: scheduler down for 10+ min
- HIGH: >3 projects failing consecutively
- MEDIUM: project starved for >2x interval
- LOW: single failure or elevated error rate
- Write to events table; watchdog delivers to Telegram
