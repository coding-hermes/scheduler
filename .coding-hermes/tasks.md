# Task Board — coding-hermes-scheduler

## [x] INIT — Bootstrap project structure **✓ 2026-07-12 — ccbcbcf**
- [x] Create Go package layout: cmd/schedulerd/, internal/scheduler/, internal/database/, internal/api/, internal/mcp/, internal/dashboard/
- [x] Write Makefile with build, test, run, install targets
- [x] Write systemd unit file (coding-hermes-scheduler.service)
- [x] Write README.md with architecture overview, build instructions, config reference

## [ ] DB — Implement SQLite data layer
- [ ] Create internal/database/schema.go — CREATE TABLE projects, ticks, events with indexes
- [ ] Create internal/database/migrations.go — versioned schema migration, auto-run on startup
- [ ] Create internal/database/projects.go — CRUD operations for projects table
- [ ] Create internal/database/ticks.go — insert tick, update outcome, query history, prune old ticks
- [ ] Create internal/database/events.go — append event, query with filters, pagination
- [ ] Write tests for all database operations with in-memory SQLite

## [ ] CORE — Implement urgency calculator
- [ ] Create internal/scheduler/urgency.go — compute urgency from priority, elapsed time, decay rate
- [ ] Write unit tests: zero elapsed, normal elapsed, extreme elapsed, custom decay rates, edge cases

## [ ] CORE — Implement weight-budget greedy packer
- [ ] Create internal/scheduler/packer.go — sort by urgency, pack greedily into weight budget
- [ ] Enforce: cooldown check, weight <= remaining budget, max concurrent cap
- [ ] Write tests: budget exhausted, all fit, cooldown blocks, mixed weights, empty project list

## [ ] CORE — Implement spawn engine
- [ ] Create internal/scheduler/spawn.go — build spawn command, run hermes chat --quiet, capture session_id from stdout
- [ ] Implement PID tracking and per-tick timeout (30 min default)
- [ ] Implement process exit handling — update tick outcome in SQLite
- [ ] Write integration test: spawn a real hermes chat process, capture session_id, verify outcome query

## [ ] CORE — Implement tick lifecycle tracker
- [ ] Create internal/scheduler/lifecycle.go — QUEUED→RUNNING→COMPLETED/FAILED/TIMEOUT state machine
- [ ] Implement session outcome query (hermes sessions export --dry-run)
- [ ] Parse outcome: commits, files_changed, tokens, cost, exit_code
- [ ] Update SQLite tick record on completion

## [ ] CORE — Main scheduler loop
- [ ] Create internal/scheduler/loop.go — 60-second ticker, load projects, compute urgency, pack, spawn, track
- [ ] Wire all components together
- [ ] Implement graceful shutdown (SIGTERM/SIGINT — complete running ticks, close DB)
- [ ] Write integration test: full cycle with mock project data

## [ ] API — REST HTTP server
- [ ] Create internal/api/server.go — net/http mux, middleware (logging, CORS-localhost-only, JSON content-type)
- [ ] GET /api/v1/health — uptime, project count, budget, version
- [ ] GET /api/v1/projects — list all with optional filtering
- [ ] GET /api/v1/projects/:name — single project detail + recent ticks
- [ ] PUT /api/v1/projects/:name — update weight/priority/cooldown/decay/enabled
- [ ] POST /api/v1/projects — register new project
- [ ] DELETE /api/v1/projects/:name — soft-delete
- [ ] GET /api/v1/projects/:name/ticks — paginated tick history
- [ ] GET /api/v1/fleet/status — budget utilization, running count, urgency snapshot
- [ ] POST /api/v1/fleet/evaluate — force evaluation cycle
- [ ] POST /api/v1/fleet/budget — set budget total
- [ ] GET /api/v1/events — paginated event log
- [ ] Write HTTP handler tests with httptest

## [ ] MCP — MCP server over HTTP
- [ ] Create internal/mcp/server.go — MCP streamable-http handler, tool registration
- [ ] Implement all MCP tools from spec (/spec/platform/api-mcp):
  - fleet_status, fleet_set_weight, fleet_set_priority, fleet_set_cooldown, fleet_set_decay
  - fleet_pause, fleet_resume, fleet_add_project, fleet_remove_project
  - fleet_set_budget, fleet_get_project, fleet_get_tick, fleet_force_evaluate, fleet_rebalance
- [ ] Each tool translates to REST API calls on the scheduler
- [ ] Write MCP compliance tests (initialize, tools/list, tools/call)

## [ ] DASH — Dashboard generator
- [ ] Create internal/dashboard/generator.go — Go html/template with embedded CSS
- [ ] Fleet overview page: budget bar, project table, running/queued sections
- [ ] Per-project detail page: tick history table, aggregates, config panel
- [ ] Session ID links (link to hermes sessions export --format html or transcript viewer URL)
- [ ] Dark theme, mobile-responsive, auto-refresh
- [ ] Serve at http://localhost:9090/ and write to ~/coding-hermes-dashboard.html

## [ ] SYNC — DuckBrain read-replica sync
- [ ] Create internal/sync/duckbrain.go — every 5 minutes, write compact status blobs
- [ ] Sync /fleet/summary — fleet-wide metrics
- [ ] Sync /fleet/projects/<name>/status — per-project compact status
- [ ] Sync /fleet/events — notable events (error, decision level)
- [ ] Use DuckBrain MCP (or direct git+JSONL if MCP not reachable)
- [ ] Write test: verify sync output format matches DuckBrain key schema

## [ ] CMD — Main entry point
- [ ] Create cmd/schedulerd/main.go — parse flags (--port, --socket, --db-path, --budget), init DB, start loop + API + MCP
- [ ] Wire graceful shutdown
- [ ] Create cmd/schedulerd/config.go — config from flags + env vars + optional config file

## [ ] PLUGIN — Hermes plugin
- [ ] Create plugin.yaml with name, version, description
- [ ] Create __init__.py — register(ctx) validates scheduler health, registers hooks
- [ ] Create hooks.py — pre_llm_call hook: parse /fleet slash commands, route to MCP tools
- [ ] Implement all slash commands from spec
- [ ] Register pre_verify hook for fleet context injection (Bane sessions)
- [ ] Write plugin test: simulate slash commands, verify correct MCP calls

## [ ] MIGR — Migration tool
- [ ] Create cmd/migrate/main.go — reads existing coding-hermes cron configs from jobs.json
- [ ] Auto-detects all foreman jobs (skills contain coding-hermes-cron)
- [ ] Extracts: name, workdir, schedule speed, model, provider
- [ ] Defaults: weight=10, priority=5, cooldown=extracted from schedule, decay=1.0
- [ ] Generates import JSON for scheduler POST /api/v1/projects
- [ ] Dry-run mode: preview without writing
- [ ] Write test with sample jobs.json fixture

## [ ] TEST — End-to-end integration
- [ ] Start scheduler with test projects
- [ ] Verify 60s loop fires, ticks spawn correctly
- [ ] Verify session_id captured from spawned hermes chat
- [ ] Verify dashboard HTML generated
- [ ] Verify MCP tools respond correctly via Hermes mcp_servers config
- [ ] Verify slash commands route correctly
- [ ] Verify graceful shutdown

## [ ] DEPLOY — Production setup
- [ ] Install systemd unit
- [ ] Start scheduler service
- [ ] Configure Hermes mcp_servers for coding-hermes
- [ ] Enable Hermes plugin
- [ ] Create trigger cron (60s, no_agent script → scheduler health check)
- [ ] Run migration tool to import existing 33 projects
- [ ] Verify dashboard accessible at localhost:9090
