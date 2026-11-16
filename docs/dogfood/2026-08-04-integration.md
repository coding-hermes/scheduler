# Dogfood Integration Report — coding-hermes-scheduler (2026-08-04)

**Verdict: 🟡 PROMISING-BUT-ROUGH**
**Probed by:** Hermes dogfood cron (independent user, not the foreman)
**Target:** live daemon at `http://127.0.0.1:9090` (pid 4704, up 40h+, `--namespace-mode`, gateway HTTP spawns)

## The promise vs. reality

**Promise (README/AGENTS.md):** "A single Go binary that replaces dozens of static
cron jobs" — knows all projects, evaluates every 60s, packs a weight budget,
spawns foremen via the Hermes gateway HTTP API, tracks every tick, exposes
REST API + MCP + dashboard + DuckBrain sync.

**Reality:** The core loop genuinely works — 44 active projects, 1,427 HTTP
spawns, 10,105 completed ticks, live escalation events, working dashboard and
MCP. But the **operator-facing API contract is broken exactly where a new user
hits first** (create project), the docs describe a wire format the server does
not speak, and the project's own 2-hourly self-verification has been red for
4+ days while the board claims all gates green.

## Time-to-first-success: ~2 min (after one 13s stall)
First probe `GET /api/v1/projects` stalled >10s (curl timeout), retry 40ms.
Everything after that: sub-50ms.

## What WORKS (verified live, 2026-08-04)

| Endpoint | Result |
|---|---|
| `GET /api/v1/health` | 200 `{status:ok, db:connected, active_ticks:6}` |
| `GET /api/v1/status` | 200 (44 projects, recent_outcomes) |
| `GET /api/v1/projects` | 200, 44 projects |
| `GET /api/v1/projects/{name}` | 200 (project + latest_tick); 404 `{"error":"project not found"}` for missing |
| `PUT /api/v1/projects/{name}` | 200 partial update (PascalCase keys) |
| `GET /api/v1/ticks?project=&limit=` | 200, newest-first, envelopes `{ticks,count}`; `limit=abc`/`0` fall back to 50 |
| `GET /api/v1/ticks/{id}` | 200 detail; 404 `tick not found` |
| `GET /api/v1/events?severity=&limit=` | 200, filters work |
| `GET /api/v1/namespaces` | 200 (snake_case fields!) |
| `POST /api/v1/evaluate` | 200 `{"status":"evaluation triggered"}` |
| `POST /api/v1/pause` / `resume` | 200 `{"status":"paused"}` / `{"status":"resumed"}` |
| Wrong methods | 405 with JSON error envelope |
| MCP `POST /mcp` | initialize, tools/list, tools/call (fleet_status, fleet_projects, fleet_project_detail, fleet_set_weight, fleet_set_priority, …), unknown tool → JSON-RPC error |
| Dashboard `/`, `/projects/{name}`, `/queue`, `/ticks?page=1`, `/namespaces/{id}`, `/health`, `/dashboard/partial` | all 200 HTML |
| Latency | historical p50 38ms, p90 42ms on /status (the current PERF-001-R3-R4 calibration is p99 <= 700ms at 105,061 ticks; see the decision record) |

## What's BROKEN / traps for integrators

1. **Create a project (P0).** Every documented shape fails:
   - Spec S06 body `{"name","repo_url","workdir"}` → **400** "name, repo_url, workdir are required" (keys don't bind; models have no json tags)
   - PascalCase minimal `{"Name","RepoURL","Workdir"}` → **500** CHECK constraint `weight >= 1 AND weight <= 100` (weight defaults to 0; handler fills no defaults)
   - Duplicate-name → constraint error (500), the documented 409 is unreachable
   - **Only working incantation (undocumented):** PascalCase WITH explicit `"Weight": 10` (1-100). This is how the fleet's own deploy-verify harness creates fixtures.
2. **Two JSON dialects on one API (P1).** projects/ticks/events: `Name`, `RepoURL`, `CooldownS`, `CreatedAt` (PascalCase). namespaces: `id`, `weight`, `hard_cap` (snake_case). `jq '.projects[0].cooldown_s'` → null. README's `jq '.project_count'` → null (real field: `active_projects`).
3. **Spec staleness (P1).** S06 §1.1 still claims `GET /projects/{name}` returns the wrong project (splitPath bug). It was fixed; verified working today. Spec status still "Draft".
4. **No delete endpoint (P2).** You cannot remove a project via API. Creating test projects permanently pollutes the live DB (16+ test-dummy rows already there).
5. **Latency spikes (P2).** `GET /api/v1/projects` took 13.07s twice in one session. Warm p50 38ms. Likely WAL checkpoint contention.
6. **Verify gate blind spot (P0 for ops).** `deploy/scheduler-verify.sh` (crontab 2h) has failed 50 consecutive runs since 2026-07-31T08:00. The foreman gate suite does not include it.

## Working integration patterns (the "right way")

- **Read** endpoints: use PascalCase field names (`jq '.projects[] | {name: .Name, workdir: .Workdir, cooldown: .CooldownS, enabled: .Enabled}'`).
- **Write** endpoints (PUT/POST): send PascalCase keys (`{"CooldownS":900,"DecayRate":1.0}`) — this is what the stand-in gap-pusher scripts do, and it works.
- **Create**: `POST /api/v1/projects` with `{"Name":"<n>","RepoURL":"<r>","Workdir":"<w>","Weight":10,"Priority":5,"CooldownS":900,"Enabled":true}` (Weight required!).
- **MCP** for agent-to-agent control: `POST /mcp`, JSON-RPC 2.0; tools `fleet_status`, `fleet_projects`, `fleet_project_detail`, `fleet_set_weight`, `fleet_set_priority`, `fleet_set_cooldown` (see tools/list).
- **Board format**: this project's board lives in `.coding-hermes/board/` (tasks.jsonl/events.jsonl/board.jsonl, DuckDB-backed, JSONL is the git mirror) — NOT `.coding-hermes/tasks.md` (archived to tasks.md.bak). Task records are one JSON object per line; see INFRA-013 for the migration history.
- **Verify**: `./bin/schedulerd --test-verify 3` — currently RED (see DOGFOOD-002).
