# Scheduler routes

The complete in-repo route set: the HTML pages registered in `cmd/schedulerd/main.go` and every `/api/v1/*` path registered by `internal/api/server.go`. The full per-endpoint reference (request/response bodies, status codes, curl recipes) lives in [docs/api.md](../api.md).

## Endpoints

| Route | Purpose |
|-------|---------|
| `/` | Fleet dashboard (full HTML page; `/dashboard` is an alias) |
| `/dashboard/partial` | htmx partial: project table refresh |
| `/static/htmx.min.js` | Bundled htmx asset (Go embed) |
| `/projects/{name}` | Per-project detail page |
| `/queue` | Global queue view |
| `/ticks?page=N` | Paginated tick history |
| `/namespaces/{id}` | Namespace drill-down |
| `/health` | Dashboard health panel |
| `/api/v1/health` | Machine health check (JSON) |
| `/api/v1/live` | DB-free liveness probe (JSON) — process-memory fields only, safe under a saturated single-SQLite-connection fleet; watchdog's first probe, `/api/v1/health` is the rich fallback (SCHED-GAP-204-A) |
| `/api/v1/status` | Fleet status summary (JSON) — budget, spend tiers, failure rates, eval/zero-select diagnostics |
| `/api/v1/config` | Resolved daemon config (JSON) |
| `/api/v1/metrics` | Read-only fleet metrics: spawns, deferrals, nudges, ticks, durations, drains and outcomes in one request (SCHED-GAP-156) |
| `/api/v1/projects` | List/manage projects (GET/POST) |
| `/api/v1/projects/{name}` | One project: detail (GET) / partial update (PUT) / delete (DELETE — `?confirm=true`; `&purge=true` hard-deletes) plus the `/pause`, `/resume`, `/spawn`, `/bump`, `/unbump` sub-routes |
| `/api/v1/namespaces` | List namespaces |
| `/api/v1/namespaces/{id}` | Namespace detail (GET) / partial update (PUT) / delete (DELETE — `?confirm=true` soft-deletes: enabled=false, member projects unassigned, row retained; `?confirm=true&purge=true` hard-deletes the row permanently, SCHED-GAP-097) |
| `/api/v1/groups` | Deploy groups — named project lists, JSONL-backed (GET list / POST create) |
| `/api/v1/groups/{name}` | One deploy group: GET / partial update (PUT, name immutable) / DELETE |
| `/api/v1/groups/{name}/deploy` | POST: apply a template's task rows to every member project's board (`dry_run=true` plans without writing) |
| `/api/v1/templates` | Deploy templates — named task definitions, JSONL-backed (GET list / POST create) |
| `/api/v1/templates/{name}` | One deploy template: GET / partial update (PUT, name immutable) / DELETE |
| `/api/v1/ticks` | List ticks |
| `/api/v1/ticks/{id}` | One tick by id (GET) |
| `/api/v1/events` | List event log (GET) |
| `/api/v1/events/stream` | SSE push stream of the event log (CTL-002) |
| `/api/v1/evaluate` | Trigger re-evaluation |
| `/api/v1/pause` | Pause scheduling (POST) |
| `/api/v1/resume` | Resume scheduling (POST) |
| `/api/v1/queue` | Global queue (JSON) |
| `/api/v1/openapi.json` | OpenAPI schema (JSON) |
| `/mcp` | MCP JSON-RPC endpoint |

**MCP surface:** `POST /mcp` serves **45 tools** — the registry in `internal/mcp/server.go` is the source of truth and the full per-tool table is in the README's [MCP Tools](README.md#mcp-tools) section (not duplicated here). Two guards keep the documented surface honest against that registry: `internal/mcp/readme_tools_parity_test.go` (README tool table ↔ registry) and `internal/mcp/agents_endpoint_parity_test.go` (this endpoint table ↔ the route registrations). A running daemon built from an older tree reports fewer tools — measure, don't assume.
