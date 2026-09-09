# Dogfood Integration Report — 2026-09-09

Project: **coding-hermes-scheduler** (fleet scheduler daemon, Go/SQLite)
Verdict: 🟡 **PROMISING-BUT-ROUGH** — the core promise is real and proven in
production every hour, but two reliability edges (one P1 wedge bug still
unfixed at HEAD, one intermittent boot FATAL) keep it from SHIPPABLE.
Runner: dogfood cron (coding-hermes-dogfood skill v1.1.0), target picked as
`coding-hermes-scheduler-pm`.
Head tested: `5d0ab6f` (== `origin/main`; production runs
`v1.1.1-58-g611828a`, 61h uptime at probe time).
Test suite: `go test -short -p 1 ./...` → PASS in 38s.
L3 verify harness: `--test-verify 3` → **✅ SCHEDULER VERIFIED** (6 checks, 0 failures).

## Promise (null hypothesis)

> "A single Go binary replaces dozens of static cron jobs with a dynamic,
> priority-weighted fleet scheduler for LLM-powered coding agents" — README.
> Specifically: event-driven evaluation packs urgent projects into a weight
> budget, spawns foremen via the Hermes gateway HTTP API, records every tick,
> exposes REST + MCP + dashboard, and degrades safely when the gateway is down.

## How it was used for real (not test scripts)

| Leg | What a real user did | Result |
|---|---|---|
| Live health probe | `GET /api/v1/health`, `/api/v1/status`, `/mcp tools/list` on prod | OK. 14 MCP tools. status shows 178 active projects, 1-4 active ticks, auto-disable armed (0.9/100) |
| Live error paths | Unknown project → `{"error":"project not found"}` (404); DELETE w/o `confirm` → 400 guard | Clean JSON errors, guards work |
| MCP read | `tools/call fleet_status` over JSON-RPC | `{"active_ticks":4,"budget":100,"total_projects":178}` |
| Dashboard | `/`, `/queue`, `/ticks`, `/projects/{name}` | 200s; fleet overview 224KB, 2.2s cold at 178 projects |
| Operator workflow (scratch) | POST project ×2 → POST evaluate → ticks spawn & complete → pause → resume on a `--simulate` daemon (port 9123, scratch DB) | Full lifecycle works; ticks recorded completed with sim outcomes |
| Sim harness | `--sim-setup --sim-ticks 10` fresh DB | Report: 85 spawned / 60 completed / 3 failed / 2 timeout, 70.6% success, priority spread + budget per tick, adaptive cooldown + OVERDUE force-select visible in logs |
| Install path | `make build` (bin/schedulerd + bin/migrate), migrate `--help`, `--show-config` | Clean. Bunker fresh-install leg **SKIPPED** — las-bunker-03 (100.69.3.13) unreachable (see DOGFOOD-023) |
| Regression probe | Repeated redundant `POST /api/v1/resume` on unpaused sim loop | **Wedged the eval loop** — SCHED-GAP-101 still live at HEAD (DOGFOOD-020) |

## Integration notes (for the next agent)

1. **Build:** `make build` → `bin/schedulerd` + `bin/migrate`. No CGO
   (modernc sqlite). ~22MB binary, 38s `-short` suite, 6-check `--test-verify`.
2. **Safe scratch daemon:** pick a FREE port first (`ss -tlnp` — 9090/9091 are
   taken by prod + an unknown `/query` service; 9123 worked). Then:
   `./bin/schedulerd --db /tmp/x.db --listen 127.0.0.1:9123 --simulate
   --log-file /tmp/x.log`. `--simulate` spawns nothing real. Projects created
   via `POST /api/v1/projects` with `cooldown_s:1` tick within seconds.
3. **Sim harness:** `./bin/schedulerd --db /tmp/fresh.db --sim-setup
   --sim-ticks 10` → prints the SIMULATION REPORT and exits. **Known flake:**
   ~1/7 fresh-DB runs FATALs at boot with `clear projects: FOREIGN KEY
   constraint failed (787)` (DOGFOOD-021) — rerun on a clean db path.
4. **Pause/resume trap (FIXED 2026-09-09, b4e52d0):** a redundant
   `POST /api/v1/resume` used to wedge evaluation (SCHED-GAP-101 /
   DOGFOOD-020). Pause state is now an atomic flag (idempotent
   Pause/Resume), `evaluate()` gates on it, the stall watchdog stays
   quiet while paused, and `/api/v1/status` exposes `paused`.
   Regression: `internal/scheduler/gap101_resume_wedge_test.go`.
5. **REST dialect:** snake_case, envelopes (`{"projects":[...]}`). PUT accepts
   PascalCase too. MCP = JSON-RPC POST `/mcp`; `fleet_add` now accepts
   `repo_url` as alias (DOGFOOD-019 fixed).
6. **Observability gap:** prod health counters `spawns_exec=2415` vs
   `spawns_http=66` (~97% exec) contradict the README "spawns via HTTP, zero
   subprocess overhead" headline and `--no-exec-fallback=true` default —
   counters or config need reconciliation (DOGFOOD-022).

## Friction log (what a new user would hit)

1. Port squat on 9091 by an unrelated service produced a misleading raw-HTML
   response; the daemon's own bind error appears only in its log after the
   startup banner (already documented in diagnostics.md #4).
2. Sim-setup FK FATAL is indistinguishable from real DB corruption at first
   sight — took 6 reruns to establish it is a boot race, not data loss.
3. Sim report shows `budget=200/100` on overdue ticks with no in-report
   explanation (the OVERDUE force-select bypass lives in logs only).
4. MCP tool list (14 tools) is healthy but README/AGENTS endpoints table is
   drifting from the OpenAPI surface (24 paths) — cross-check before trusting.

## Files left behind

- `docs/dogfood/2026-09-09-integration.md` (this file)
- `docs/dogfood/diagnostics.md` (appended 2026-09-09 section)
- `skills/scheduler-usage/SKILL.md` (bumped to v1.3.0 with 09-09 verified traps)
- `.coding-hermes/dogfood-log.md` (run entry)
- Board rows: DOGFOOD-020 (P1), DOGFOOD-021 (P2), DOGFOOD-022 (P3),
  DOGFOOD-023 (P2 SKIPPED-install-bunker)
