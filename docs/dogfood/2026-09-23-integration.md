# Scheduler Dogfood Integration — 2026-09-23 (groups/templates deploy workflow + MCP + SSE)

## What this run tested

Past runs (08-04, 08-15, 08-25, 09-09, 09-19) covered fresh-install, boot, core
API, MCP list, dashboard, and the test suite. This run took the surface none of
them touched: the **JSONL-backed deploy groups/templates workflow** (the 09-03
thin-JSONL doctrine feature, live in production), driven as a real operator
would — plus real MCP tool CALLS (not just tools/list), the SSE event stream,
/metrics, and a restart/persistence check.

Method: a scratch daemon on 127.0.0.1:9093 (build dev-6dffc753 = public HEAD at
the time) with its own DB, workdirs, and foreman-home. The live fleet on :9090
was never touched.

## The working operator recipe (as actually experienced)

```bash
# 0. scratch instance (never point this at the fleet DB)
go build -o bin/schedulerd ./cmd/schedulerd/
./bin/schedulerd --db /tmp/scratch/sched.db --listen 127.0.0.1:9093 \
  --foreman-home /tmp/scratch/hermes

# 1. projects (repo_url required, arrive disabled — known from 09-19)
curl -X POST :9093/api/v1/projects -d '{"name":"df-alpha","repo_url":"...","workdir":"/tmp/ws/alpha",...}'
curl -X PUT  :9093/api/v1/projects/df-alpha -d '{"enabled":true}'

# 2. template + group (OpenAPI schema is the real doc — docs/api.md defers to it)
curl -X POST :9093/api/v1/templates -d '{"name":"t1","tasks":[
  {"title":"task for {PROJECT} on {DATE}","detail":"...","id_pattern":"DFM-{DATE}-{PROJECT}-{TASK}","labels":["dogfood"]}]}'
curl -X POST :9093/api/v1/groups    -d '{"name":"g1","projects":["df-alpha","df-beta"]}'

# 3. GOTCHA (SCHED-GAP-223): the member workdir needs a board file FIRST
mkdir -p /tmp/ws/alpha/.coding-hermes/board && touch /tmp/ws/alpha/.coding-hermes/board/tasks.jsonl
#   (a nonexistent workdir is ACCEPTED at project create; the failure only
#    surfaces at deploy as "no JSONL board found", dry_run gives the same)

# 4. deploy: dry_run plans, real run appends, re-run same-day is a no-op
curl -X POST :9093/api/v1/groups/g1/deploy -d '{"template":"t1","dry_run":true}'
curl -X POST :9093/api/v1/groups/g1/deploy -d '{"template":"t1","dry_run":false}'
curl -X POST :9093/api/v1/groups/g1/deploy -d '{"template":"t1","dry_run":false}'  # → skipped, idempotent

# 5. verify on disk: full-schema rows, ids DFM-<DATE>-<proj>-01, labels → capability_tags
cat /tmp/ws/alpha/.coding-hermes/board/tasks.jsonl
```

The same workflow works over MCP (`groups_create` → `groups_deploy`,
`templates_create`) — 45 tools served at /mcp, JSON-RPC 2024-11-05.

## What held up

- Template → group → deploy → board rows: correct full-schema JSONL rows,
  id_pattern placeholders substituted, labels landed as capability_tags,
  foreman_note stamped with template+group provenance.
- Idempotence: same-day re-deploy skips with an explicit reason per project.
- MCP real calls (groups_get, groups_deploy, templates_create) — not just listing.
- SSE /api/v1/events/stream: replay from id 1, proper `id:`/`data:` framing.
- /api/v1/metrics: deferrals, escalations, lane churn, outcomes in one read.
- Persistence: killed the daemon, restarted — groups/templates/board rows all
  survived (JSONL + SQLite both intact).
- Honesty: spawn with no gateway client → tick `failed` with a precise error
  ("no gateway client and exec fallback disabled"), not a fake success.
- DuckBrain sync fail-fast fired exactly as documented in AGENTS.md (401 → HIGH
  event, sync gated off, nothing spooled) — the scratch box had no key.
- SCHED-GAP-182 (09-19 finding) is FIXED at this build: a brand-new empty HOME
  boots to ready, auto-creating ~/.hermes/coding-hermes. Zero FATALs.

## Judged friction points (filed to the board)

| ID | Severity | Issue |
|----|----------|-------|
| SCHED-GAP-223 | P2 | `groups/{name}/deploy` fails on projects with no pre-existing JSONL board; no auto-init, dry_run gives the same opaque error |
| SCHED-GAP-224 | P3 | template create silently drops unknown per-task fields (id/priority/status) instead of 400 — only title/detail/labels/id_pattern exist per OpenAPI TemplateTask |

## Verdict

🟢 The deploy groups/templates workflow is SHIPPABLE for real operator use:
time-to-first-success ~4 min on a scratch instance (2 min of that was the
SCHED-GAP-223 board-file gotcha). Everything measured warm was 6-10 ms per
read; cold boot-to-first-200 was 24 ms; the bunker fresh-clone build took 84s
(identical to the 09-19 run — reproducible). Nothing was slow enough to file a
PERF row.