# Task Board — coding-hermes-scheduler

## [x] INIT — Bootstrap ✓ | [x] DB — SQLite ✓ | [x] SPEC S01-S06 ✓ | [x] CORE ✓ 
## [x] API ✓ | [x] MCP ✓ | [x] DASH ✓ | [x] SYNC (paused) ✓ | [x] PLUGIN ✓
## [x] CMD ✓ | [x] MIGR ✓ | [x] DEPLOY ✓ | [x] GAP-005 ✓ | [x] GAP-006 ✓

---

## ACTIVE GAPS — 2026-07-12

### [ ] GAP-002 — Tests for 5 core packages
**Priority: HIGH. Weight: 30.**
- `internal/scheduler/` — urgency, packer, spawn, lifecycle, loop: 0 tests
- `internal/api/` — 15 endpoints: 0 tests
- `internal/mcp/` — 14 tools: 0 tests
- `internal/dashboard/` — template rendering: 0 tests

### [ ] GAP-003 — Integration test
**Priority: HIGH. Weight: 20.**
- Spin up schedulerd, hit /health, /, /mcp, /api/v1/projects in one test
- Verify ticks are created and transition through states

### [ ] GAP-004 — DuckBrain sync client
**Priority: MEDIUM. Weight: 15.**
- Replace os/exec with MCP HTTP client in sync/duckbrain.go
- DuckBrain MCP runs as stdio via wrapper — need HTTP bridge or direct MCP calls

### [ ] GAP-008 — Session ID capture
**Priority: HIGH. Weight: 25.**
- Spawner launches hermes chat but doesn't capture session_id from stdout
- Need to parse `session_id: <ID>` from stdout and update tick record
- Need outcome query: `hermes sessions export --dry-run <session_id>` 

### [ ] GAP-009 — Spawn uses project model/provider
**Priority: MEDIUM. Weight: 10.**
- Spawner hardcodes deepseek-v4-pro/deepseek-foreman
- Should read model/provider from project config for per-project model selection

### [ ] GAP-007 — Dead code cleanup
**Priority: LOW. Weight: 5.**
- mcp/server.go: `var _ = log.Printf; var _ = time.Now` guards
- api/server.go: duplicate boolPtr
- mcp/server.go: duplicate boolPtr

### [ ] CUTOVER — Disable old cron jobs
**Priority: HIGH. Weight: 15.**
- 26 projects migrated to scheduler and running
- Old Hermes cron jobs still running in parallel → double-spawning
- Disable old cron jobs for all 26 migrated projects
- Only keep non-foreman crons (daily reports, health checks, etc.)

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
