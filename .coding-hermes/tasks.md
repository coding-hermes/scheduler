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
