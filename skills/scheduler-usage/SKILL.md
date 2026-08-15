---
name: scheduler-usage
description: >-
  How to USE the Coding Hermes Scheduler for real: REST API dialect (now
  snake_case), MCP tools (fleet_* only), dashboard, project lifecycle
  (create/PUT/DELETE-soft), board format, sim/verify harnesses, and the
  known traps (simulate doesn't simulate, plugin symlink, doc mirrors).
  Load this before operating the scheduler or writing any integration
  against http://127.0.0.1:9090. Rewritten from the 2026-08-15 dogfood run
  (docs/dogfood/2026-08-15-integration.md); supersedes the 08-04 version.
version: 1.1.0
category: software-development
---

# Scheduler Usage — Operating the Fleet Scheduler for Real

The scheduler is a single Go daemon (`bin/schedulerd`, SQLite/WAL) that
dispatches foreman ticks for 40+ projects. REST on `127.0.0.1:9090/api/v1`,
MCP JSON-RPC on `/mcp`, htmx dashboard at `/`. Live DB:
`~/.hermes/coding-hermes/scheduler.db` (daemon runs via systemd user unit
`coding-hermes-scheduler.service`, `-config /home/kara/.hermes/fleet.toml`,
`--auto-disable-failure-rate 0.9`).

## Golden rules (verified 2026-08-15)

1. **The wire format is snake_case everywhere now** (S06 conformance landed
   08-04+): `cooldown_s`, `decay_rate`, `disabled_at`… Lists come in
   envelopes: `GET /api/v1/projects` → `{"projects":[…]}`; detail →
   `{"project":{…},"latest_tick":…}`. PascalCase keys are still ACCEPTED on
   decode (legacy), so old scripts keep working — but read with snake_case.
2. **Create a project with the minimal spec body** (works since DOGFOOD-001):
   `{"name":"X","repo_url":"…","workdir":"/abs/path"}` → 201, created
   DISABLED with defaults weight 10 / priority 5 / cooldown_s 900 /
   decay_rate 1.0. Dup name → 409. Enable with `PUT {"enabled":true}`.
3. **DELETE is a SOFT delete.** `DELETE /api/v1/projects/{name}?confirm=true`
   → 200 but the row persists (enabled=0, `disabled_by="api-delete"`). 409
   while enabled. There is NO API way to purge rows. Keep scratch lifecycle
   tests to a minimum — they accumulate forever.
4. **PUT to change cooldown/decay/enabled**:
   `curl -X PUT http://127.0.0.1:9090/api/v1/projects/<NAME> -d '{"cooldown_s":900,"decay_rate":1.0}'`.
   This is how the stand-in cron wakes paused foremen (CooldownS/PascalCase
   also accepted).
5. **Do NOT trust `--simulate` or `--sim-count`** (DOGFOOD-007 open):
   `--simulate` still uses the real spawner (ticks fail with "no gateway
   client and exec fallback disabled" without gateway credentials — or spawn
   real foremen with them); `--sim-count` crashes with a UNIQUE ticks.id
   constraint error. Use `--sim-setup --sim-ticks N` on a scratch DB or
   `--test-verify 3` instead.
6. **MCP tool names are the 14 `fleet_*` tools** (`fleet_status`,
   `fleet_projects`, `fleet_project_detail`, `fleet_set_weight`,
   `fleet_set_priority`, `fleet_set_cooldown`, `fleet_set_decay`,
   `fleet_pause`, `fleet_resume`, `fleet_add`, `fleet_ticks`,
   `fleet_evaluate`, `fleet_pause_scheduler`, `fleet_resume_scheduler`).
   The README's second MCP table (`list_projects`, `get_project`, …) is
   fiction (DOGFOOD-011) — `tools/call list_projects` → "unknown tool".
7. **The board is JSONL in `.coding-hermes/board/`** (tasks.jsonl git-tracked
   + DuckDB board.db mirror kept in parity). Append one JSON object per line
   (id, title, status, priority, complexity, capability_tags, created_at…),
   then mirror into board.db with an existence-checked INSERT (the tasks
   table has NO primary key — `INSERT OR IGNORE` fails). Validate with
   `python3 -c "import json;[json.loads(l) for l in open('tasks.jsonl')]"`.

## Endpoint cheat sheet (verified 2026-08-15)

| Route | Use |
|---|---|
| `GET /api/v1/health` | liveness + DB + active ticks + `evaluation_age_seconds` |
| `GET /api/v1/status` | fleet summary; `active_projects`, `auto_disable{enabled,threshold,window,min_ticks}`, `projects_failure_rates` (per-project over `failure_window`=100), `recent_outcomes` (LIFETIME counts — name is misleading), `zero_select_consecutive/eligible/last_at` (GAP-043 diagnostics) |
| `GET /api/v1/projects` | list, `{"projects":[…]}` snake_case |
| `GET /api/v1/projects/{name}` | `{"project":…,"latest_tick":…}`; 404 `{"error":"project not found"}` |
| `POST /api/v1/projects` | create (minimal snake_case body; defaults; created disabled; dup-name 409) |
| `PUT /api/v1/projects/{name}` | partial update (snake_case or PascalCase) |
| `DELETE /api/v1/projects/{name}?confirm=true` | soft delete (409 if enabled) |
| `GET /api/v1/ticks?project=X&limit=N` | newest-first, `{count,ticks}`; rows carry tokens/cost_usd/commits/exit_code |
| `GET /api/v1/events?severity=HIGH&limit=N` | event feed; `loop` HIGH events = eval-stall watchdog firing (GAP-042 — by design) |
| `GET /api/v1/namespaces` | namespace weights |
| `POST /api/v1/evaluate` | force evaluation (safe, signal-only; resets evaluation_age) |
| `POST /api/v1/pause` / `resume` | fleet-wide, process-local, idempotent |
| `POST /mcp` | JSON-RPC 2.0: initialize → tools/list → tools/call (fleet_* only) |

## MCP call format

`{"jsonrpc":"2.0","id":N,"method":"tools/call","params":{"name":"fleet_status","arguments":{}}}`.
Read-only tools (`fleet_status`, `fleet_projects`, `fleet_project_detail`,
`fleet_ticks`) are safe anytime. `fleet_add` creates projects — remember
there's no purge.

## Diagnostics and verification

- `deploy/scheduler-verify.sh` (crontab 2h) → `./bin/schedulerd --test-verify 3`.
  **GREEN since 08-04 fix** (logs in `deploy/verify-*.log`; today's 06/08/10
  UTC runs: "6 checks, 0 failures, ✅ SCHEDULER VERIFIED"). It also prints a
  perf audit (p99 /status ≈16ms at 29k rows — spec S10 <100ms PASS) and a
  per-project failure breakdown.
- `--test-verify 3` on any scratch DB is the fastest way to validate the
  whole engine end-to-end without touching the live fleet.
- `--sim-setup --sim-ticks N` gives a 13-project fixture (12 enabled + 1
  disabled) + simulated ticks + report.
- `--schema` prints a JSON Schema for a root `schedulerd.toml` that the
  daemon does NOT load yet (FEAT-005 wiring); `--show-config` prints the
  EFFECTIVE configuration (CLI flags + SCHEDULER_* env overrides applied)
  and comments which env vars are active. Env vars DO work at boot
  (e.g. `SCHEDULER_AUTO_DISABLE_FAILURE_RATE`). Root TOML values are not
  loaded until FEAT-005 (DOGFOOD-012).
- Live signals: `zero_select_consecutive=0` + regular `EVAL:` lines in
  `~/.hermes/coding-hermes/scheduler.log` = loop healthy. `EVAL-STALL:`
  lines = watchdog forced re-eval after 5 idle minutes (by design, GAP-042).

## Pitfalls

- `.coding-hermes/tasks.md` is dead (`.bak`); the board is
  `.coding-hermes/board/tasks.jsonl`. The outer dir pointer file
  (`/home/kara/coding-hermes-scheduler/.coding-hermes/tasks.md`) is
  intentionally not a board.
- `/fleet …` chat slash commands may be dead — the plugin symlink
  `~/.hermes/plugins/coding-hermes` points at a typo'd path
  (`/home/kara/coding-herms-scheduler/plugin`, missing `-hermes-`;
  DOGFOOD-008). Check `readlink -f ~/.hermes/plugins/coding-hermes` before
  relying on them.
- Repo `fleet.toml` and `docs/fleet.md` are stale mirrors of the live fleet
  (DOGFOOD-013) — trust the API, not the repo files.
- No auth on anything — loopback only, treat as operator-only.
- The daemon binary at `bin/schedulerd` is what's running; systemd user unit
  `coding-hermes-scheduler.service` builds it via ExecStartPre. Rebuild +
  `systemctl --user restart coding-hermes-scheduler` after code changes.
- Disable-provenance: new disables stamp `disabled_at/by/reason`; 25 legacy
  disabled rows are empty (DOGFOOD-010) — don't read their absence as a bug
  in your own changes.
