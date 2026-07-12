# Task Board — coding-hermes-scheduler

## [x] INIT — Bootstrap project structure **✓ ccbcbcf**
- [x] Go package layout, Makefile, systemd unit, README, .gitignore, GitReins guards

## [x] DB — Implement SQLite data layer **✓ e91ab0f**
- [x] Schema, migrations, CRUD for projects/ticks/events — 29 tests passing

## [ ] SPEC — Write axiom-level implementation specs (BLOCKING all CORE/API/MCP work)

An agent reading these specs must be unable to take a wrong path. Every spec follows the 10-section template: Overview → Dependencies → Interface → Behavior → Data → States → Errors → Testing → Security → Performance. Standard: "so detailed a blind person could visualize."

- [ ] SPEC-S01 — System architecture spec (3-4 pages)
  - Component diagram (Mermaid), data flow between components, directory tree, dependency graph
  - Exact Go interfaces at system boundaries, what injects what
  - Config struct with exact env var names, types, defaults, validation
  - System failure modes: what breaks, how it manifests, recovery path

- [ ] SPEC-S02 — Data model spec (4-5 pages)
  - Exact DDL: CREATE TABLE projects/ticks/events with every column type, constraint, index
  - Exact Go model structs with db/sql tags
  - DuckBrain key schema with exact JSON shapes for /fleet/summary, /fleet/projects/<name>/status, /fleet/events
  - Migration strategy: version table, up/down scripts, auto-run on startup
  - Query patterns: exact SQL for every query the scheduler needs
  - Data retention: 200 ticks per project, compaction strategy, WAL config

- [ ] SPEC-S03 — Urgency calculator spec (3-4 pages)
  - Exact urgency formula implementation in Go pseudocode with all branches
  - Geometric interval mapping: exact formula, configurable min/max, runtime recalculation
  - Edge cases: zero elapsed, negative elapsed (clock skew), decay_rate=0, priority=0, max_interval overflow
  - Unit test scenarios with exact inputs and expected outputs

- [ ] SPEC-S04 — Weight-budget packer spec (2-3 pages)
  - Greedy packing algorithm: step-by-step with decision tree
  - Edge cases: empty project list, all projects overweight, budget=0, cooldown blocking, max concurrent reached
  - Urgency tie-breaking rules
  - Concurrency pool: claim/release semantics, starvation prevention
  - Unit test scenarios

- [ ] SPEC-S05 — Spawn engine + tick lifecycle spec (3-4 pages)
  - Exact spawn command template with all flags
  - Session ID capture: parse stdout format, handle parse failures
  - PID tracking: data structure, timeout enforcement, cleanup on SIGTERM
  - Tick state machine: full transition diagram, guard conditions per transition
  - Session outcome query: exact command, output parsing, error handling
  - Integration test: exact scenario with mock hermes chat

- [ ] SPEC-S06 — REST API spec (4-5 pages)
  - OpenAPI 3.0 YAML for all 15 endpoints — exact request/response schemas
  - Error catalog: every HTTP status code, exact JSON error body shape, when each fires
  - Middleware stack: logging format, CORS policy, content-type enforcement
  - Pagination: query params, response envelope, Link headers
  - Health endpoint: exact response shape, what gets checked

- [ ] SPEC-S07 — MCP server spec (2-3 pages)
  - MCP protocol compliance: initialize, tools/list response shape, tools/call envelope
  - All 14 tool schemas with exact JSON Schema parameters
  - Error handling: what MCP errors map to what scheduler errors
  - Hermes config.yaml snippet for connection

- [ ] SPEC-S08 — Dashboard spec (3-4 pages)
  - HTML structure: exact element hierarchy, CSS class naming convention
  - Design tokens: color palette (hex), typography (font, sizes, weights), spacing scale
  - Fleet overview: weight budget bar component, project table, running/queued sections
  - Per-project detail: tick history table, aggregate stats card, config edit panel
  - Session ID links: exact URL template, click behavior
  - Auto-refresh: polling strategy, error state, loading skeleton
  - All UI states: loading, empty, error, populated — per component

- [ ] SPEC-S09 — Hermes plugin spec (2 pages)
  - Exact file structure: plugin.yaml content, __init__.py register() logic, hooks.py handler signatures
  - pre_llm_call hook: slash command parsing regex, argument extraction, MCP tool routing
  - Error handling: scheduler unreachable, invalid command, MCP timeout
  - Installation: exact commands, config.yaml snippet

- [ ] SPEC-S10 — Testing strategy spec (2-3 pages)
  - Unit test scenarios per component with exact inputs/outputs
  - Integration test: full scheduler cycle with mock projects
  - E2E test: scheduler → spawn hermes chat → capture session_id → query outcome
  - MCP compliance test: initialize, tools/list, tools/call roundtrip
  - Dashboard rendering test: verify HTML output structure

- [ ] SPEC-S11 — Deployment + migration spec (2 pages)
  - systemd unit: exact file content, install commands, log access
  - Hermes integration: config.yaml entries, plugin enable, trigger cron
  - Migration from 33 static crons: exact steps, rollback plan
  - Verification checklist: health check, first tick, dashboard loads, slash commands work

## [ ] CORE — Implement from specs (AFTER SPEC complete)
- [ ] urgency.go — from SPEC-S03
- [ ] packer.go — from SPEC-S04
- [ ] spawn.go — from SPEC-S05
- [ ] lifecycle.go — from SPEC-S05
- [ ] loop.go — from SPEC-S01 wiring diagram

## [ ] API — Implement from specs (AFTER CORE)
- [ ] server.go — from SPEC-S06 OpenAPI

## [ ] MCP — Implement from specs (AFTER API)
- [ ] server.go — from SPEC-S07

## [ ] DASH — Implement from specs (AFTER API)
- [ ] generator.go — from SPEC-S08

## [ ] SYNC — DuckBrain sync (AFTER CORE)
- [ ] duckbrain.go — from SPEC-S02 DuckBrain keys

## [ ] CMD — Main entry point (AFTER CORE+API+MCP)
- [ ] main.go, config.go — from SPEC-S01 config struct

## [ ] PLUGIN — Hermes plugin (AFTER MCP)
- [ ] plugin.yaml, __init__.py, hooks.py — from SPEC-S09

## [ ] MIGR — Migration tool (AFTER CORE)
- [ ] main.go — from SPEC-S11

## [ ] TEST — End-to-end (AFTER ALL)
- [ ] From SPEC-S10 scenarios

## [ ] DEPLOY — Production (AFTER TEST)
- [ ] From SPEC-S11 checklist
