# Dogfood integration report — coding-hermes-scheduler, 2026-09-25 (control-surface run)

Tick: coding-hermes-scheduler-dogfood-2026-09-25-05-49-50 · daemon under test: v1.5.0-92-g63dfbd10 (systemd user unit, live fleet, 332 active projects)

## The angle

This week's earlier runs covered fresh boot (09-19), the groups/templates deploy
surface (09-23) and the dashboard/status perf surface (09-24, twice). Never
exercised as a real operator: the **mutating control surface** — create a
project, adjust it, pause/resume it, spawn a manual tick, delete it — through
REST and MCP, including the SCHED-GAP-1602 authentication that just landed at
HEAD. A control plane is exactly the surface where "tests are green" and
"trustworthy" diverge, because its failure mode is not a broken page, it is an
action you did not mean to allow.

## Promise under test

> "You can monitor, pause, or adjust any project through the dashboard, REST
> API, or MCP tools." (README)

## What I did (real operator flow, on a throwaway probe project)

Created `dogfood-probe-0925` (empty git workdir under /tmp, `enabled:false`)
through the documented REST create, then drove its full lifecycle and probed
the guards. All mutating calls authenticated with the operator token via
`X-Operator-Token`; the unauthenticated-refusal probes deliberately used none.

| # | Operation | Result | Latency |
|---|---|---|---|
| 1 | `POST /api/v1/projects` create (after learning `workdir` is required from the 400 message) | 201, full project JSON | 16ms |
| 2 | duplicate create | 409 `project already exists` | 23ms |
| 3 | `POST .../spawn` on the **disabled** project | **202 + real tick row, status running** | 20ms |
| 4 | `PUT` cooldown 900→1800 | 200, `updated_at` bumped | 18ms |
| 5 | `POST .../resume` (enable) | 200 | 16ms |
| 6 | `POST .../pause` | 200, disable provenance stamped (`api-pause` + reason) | 20ms |
| 7 | `DELETE` without `confirm` | 400 with a helpful message naming both flags | 20ms |
| 8 | `DELETE ?confirm=true` | 200 `deleted` | 20ms |
| 9 | `GET /api/v1/events?component=api.auth` | every mutation audited, refusals MEDIUM, identity recorded | — |
| 10 | unauthenticated `POST /api/v1/projects` | 401 `operator credential required for mutations` | — |

The REST control surface is in good shape: consistent status codes, honest
error messages (the 400-on-missing-workdir message named the three required
fields; the delete-400 message documents both `confirm` and `purge`), disable
provenance stamped on every pause, and a complete per-call audit trail in the
events table. Latency of every mutating call: 16-23ms at fleet scale.

Then the MCP leg found the hole.

## F1 (P1): /mcp bypasses the operator auth — reproduced live

```
curl -s -X POST http://127.0.0.1:9090/mcp -H 'Content-Type: application/json' \
  -d '{"jsonrpc":"2.0","id":10,"method":"tools/call","params":{"name":"fleet_pause_scheduler","arguments":{}}}'
→ HTTP 200 {"result":{"content":[{"text":"{\"status\":\"scheduler paused\"}"}]}}
```

No token. The whole-fleet kill switch **executed**. I reverted within ~60s via
the authenticated `fleet_resume_scheduler` and verified evaluations continued
(`last_evaluation` advancing, board_wake events flowing). A `fleet_pause`
tools/call against a nonexistent project answered from the database
(`sql: no rows in result set`) — handlers run pre-auth, so every mutating MCP
tool (fleet_pause/resume, fleet_set_*, fleet_add, project_delete, project_spawn,
project_bump, namespaces_*/groups_*/templates_* mutations, groups_deploy) is
reachable without a credential.

Mechanism: SCHED-GAP-1602's gate lives in `api.Server`
(internal/api/auth_gate.go) and is applied only to the `/api/` mux
(cmd/schedulerd/main.go:950). MCP is mounted bare at main.go:953-954, and
`mcp.Server` has no credential config at all. Secondary defect: REST mutations
audit to `component=api.auth`, MCP mutations audit to nothing — my fleet-wide
pause left no event row.

Filed as **SCHED-GAP-1619**.

## F2 (P2): spawn works on disabled projects

`POST /api/v1/projects/{name}/spawn` on a disabled project returned 202 and
produced a real running tick, while `bump` in the same file refuses disabled
projects with an explicit, well-worded 409. `spawnProject`
(internal/api/server_projects.go:660) never reads `p.Enabled`. The tick even
outlived the project's hard delete. Filed as **SCHED-GAP-1620**.

## F3: PERF-002 re-measured (open, unchanged)

First `/api/v1/status` after each ~2-minute evaluate boundary blocked **28-104s**
(103.9s once, overlapped with an evaluation start); warm calls 0.42-0.44s
(hyperfine n=10: 253.8ms ± 7.6ms including client overhead). `/api/v1/health`
1.0s, `/api/v1/projects` 1.1s, `/api/v1/queue` 4ms — only the loop-state-touched
surface stalls. Goroutine dump mid-block: no status handler goroutine visible
(blocked before doing measurable work). This is the post-restart re-measurement
PERF-002's verify block calls for; filed as a note row **SCHED-GAP-1621** so
nobody re-files the symptom as new.

## What a new operator needs that the docs don't say

1. The operator credential is `SCHEDULER_OPERATOR_TOKEN` (or `[api]
   operator_token` in the root TOML); fail-closed mode 503s every mutation if
   unset. docs/reference/flags.md does not say how to verify which mode the
   running daemon is in — `GET /api/v1/health` does not expose auth mode.
2. REST error messages are genuinely good — the create-400 and delete-400
   messages are self-explanatory. The MCP surface, by contrast, gives JSON-RPC
   `-32000` with raw SQL errors (`sql: no rows in result set`) to callers.
3. `POST /api/v1/projects` requires `workdir` as a separate absolute path even
   for local repos (`repo_url:"local:..."` is an example in docs/api.md — this
   one is documented, good).

## Verdict

🟡 **PROMISING-BUT-ROUGH** (control surface). REST: shippable, audited,
16-23ms per mutation, honest errors. MCP: same power, zero authentication, zero
audit — the exact opposite of the SCHED-GAP-1602 contract that REST just
earned. Until SCHED-GAP-1619 closes, "secured control surface" is only true for
half of it.
