package database

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// latestMigration is the highest migration version known to this build.
// Bump it when adding a new migration to the migrations slice below.
const latestMigration = 41

// migration describes a single forward-only schema change.
type migration struct {
	version int
	desc    string
	stmt    string
	// ownTx marks a migration that must manage its OWN transaction and PRAGMA
	// state, so Migrate runs `stmt` directly instead of wrapping it in a
	// transaction. Only ONE shape needs this today: a table REBUILD that must
	// run with `PRAGMA foreign_keys=OFF` while dropping a PARENT table
	// (v37 widening the ticks status/outcome vocabularies). SQLite makes
	// `PRAGMA foreign_keys` a no-op inside a transaction ("may only be enabled
	// or disabled when there is no pending BEGIN or SAVEPOINT"), and a DROP
	// TABLE with enforcement ON fires the child's ON DELETE CASCADE — which
	// would silently DELETE every tick_workers attribution row. So the
	// statement does its own toggling and carries its own BEGIN/COMMIT, and
	// the runner re-asserts enforcement afterwards whatever happens.
	ownTx bool
}

// migrations is the ordered list of schema migrations. Each entry must be
// idempotent-safe to run exactly once (guarded by the migrations table).
var migrations = []migration{
	{
		version: 1,
		desc:    "create projects, ticks, events tables and indexes",
		stmt: `
CREATE TABLE IF NOT EXISTS projects (
    name       TEXT PRIMARY KEY,
    repo_url   TEXT NOT NULL,
    workdir    TEXT NOT NULL,
    weight     INTEGER NOT NULL DEFAULT 10 CHECK(weight >= 1 AND weight <= 100),
    priority   INTEGER NOT NULL DEFAULT 5 CHECK(priority >= 1 AND priority <= 10),
    cooldown_s INTEGER NOT NULL DEFAULT 900,
    decay_rate REAL NOT NULL DEFAULT 1.0,
    model      TEXT NOT NULL DEFAULT 'deepseek-v4-flash',
    provider   TEXT NOT NULL DEFAULT 'deepseek-foreman',
    worker_model      TEXT NOT NULL DEFAULT '',
    worker_provider   TEXT NOT NULL DEFAULT '',
    fallback_model    TEXT NOT NULL DEFAULT '',
    fallback_provider TEXT NOT NULL DEFAULT '',
    no_global_fallback INTEGER NOT NULL DEFAULT 0,
    idle_model        TEXT NOT NULL DEFAULT '',
    idle_provider     TEXT NOT NULL DEFAULT '',
    daily_budget_usd  REAL NOT NULL DEFAULT 0.0,
    weekly_budget_usd REAL NOT NULL DEFAULT 0.0,
    final_budget_usd  REAL NOT NULL DEFAULT 0.0,
    prompt            TEXT NOT NULL DEFAULT '',
    prompt_mode       TEXT NOT NULL DEFAULT 'append',
    deliver    TEXT NOT NULL DEFAULT '',
    enabled    INTEGER NOT NULL DEFAULT 1,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS ticks (
    id            TEXT PRIMARY KEY,
    project_name  TEXT NOT NULL REFERENCES projects(name),
    session_id    TEXT,
    pid           INTEGER DEFAULT 0,
    status        TEXT NOT NULL DEFAULT 'queued' CHECK(status IN ('queued','running','completed','failed','timeout','deferred')),
    outcome       TEXT CHECK(outcome IN ('committed','dry_run','failed','timeout','deferred')),
    spawned_at    TEXT,
    completed_at  TEXT,
    exit_code     INTEGER,
    commits       INTEGER DEFAULT 0,
    files_changed INTEGER DEFAULT 0,
    tokens_in     INTEGER DEFAULT 0,
    tokens_out    INTEGER DEFAULT 0,
    cost_usd      REAL DEFAULT 0.0,
    urgency       REAL DEFAULT 0.0,
    weight_used   INTEGER DEFAULT 0,
    error         TEXT,
    created_at    TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_ticks_project_spawned ON ticks(project_name, spawned_at);
CREATE INDEX IF NOT EXISTS idx_ticks_status ON ticks(status);

CREATE TABLE IF NOT EXISTS events (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    timestamp    TEXT NOT NULL,
    level        TEXT NOT NULL CHECK(level IN ('info','warn','error','decision')),
    project_name TEXT,
    message      TEXT NOT NULL,
    detail       TEXT,
    created_at   TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_events_project ON events(project_name, timestamp);
CREATE INDEX IF NOT EXISTS idx_events_level ON events(level, timestamp);
`,
	},
	{
		version: 2,
		desc:    "add last_tick_started, last_tick_completed to projects",
		stmt: `
ALTER TABLE projects ADD COLUMN last_tick_started TEXT;
ALTER TABLE projects ADD COLUMN last_tick_completed TEXT;
`,
	},
	{
		version: 3,
		desc:    "add command column to projects for custom spawn commands",
		stmt: `
ALTER TABLE projects ADD COLUMN command TEXT DEFAULT '';
`,
	},
	{
		version: 4,
		desc:    "add namespaces, namespace_ticks tables and namespace_id to projects",
		stmt: `
CREATE TABLE IF NOT EXISTS namespaces (
    id          TEXT PRIMARY KEY NOT NULL,
    weight      INTEGER NOT NULL DEFAULT 10 CHECK(weight >= 1 AND weight <= 100),
    reserved    INTEGER NOT NULL DEFAULT 1 CHECK(reserved >= 0),
    hard_cap    INTEGER NOT NULL DEFAULT 100 CHECK(hard_cap >= 0),
    max_concurrent INTEGER NOT NULL DEFAULT 0 CHECK(max_concurrent >= 0),
    enabled     INTEGER NOT NULL DEFAULT 1 CHECK(enabled IN (0, 1)),
    description TEXT,
    default_prompt TEXT NOT NULL DEFAULT '',
    model_chain    TEXT NOT NULL DEFAULT '',
    created_at  TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at  TEXT NOT NULL DEFAULT (datetime('now'))
);

ALTER TABLE projects ADD COLUMN namespace_id TEXT REFERENCES namespaces(id) ON DELETE SET NULL;

CREATE INDEX IF NOT EXISTS idx_projects_namespace ON projects(namespace_id);

CREATE TABLE IF NOT EXISTS namespace_ticks (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    tick_group   TEXT NOT NULL,
    namespace_id TEXT NOT NULL REFERENCES namespaces(id),
    allocated    INTEGER NOT NULL,
    used         INTEGER NOT NULL,
    borrowed     INTEGER NOT NULL DEFAULT 0,
    lent         INTEGER NOT NULL DEFAULT 0,
    job_count    INTEGER NOT NULL,
    created_at   TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE INDEX IF NOT EXISTS idx_namespace_ticks_group ON namespace_ticks(tick_group);
CREATE INDEX IF NOT EXISTS idx_namespace_ticks_ns ON namespace_ticks(namespace_id, created_at DESC);
`,
	},
	{
		version: 5,
		desc:    "recreate events table with correct column names (severity, component, details)",
		stmt: `
DROP TABLE IF EXISTS events;

CREATE TABLE IF NOT EXISTS events (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    severity   TEXT NOT NULL CHECK(severity IN ('CRITICAL','HIGH','MEDIUM','LOW','INFO')),
    component  TEXT NOT NULL DEFAULT '',
    message    TEXT NOT NULL,
    details    TEXT DEFAULT '{}',
    created_at TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_events_severity ON events(severity, created_at DESC);
`,
	},
	{
		version: 6,
		desc:    "add worker_model and worker_provider columns to projects",
		stmt: `
ALTER TABLE projects ADD COLUMN worker_model TEXT DEFAULT '';
ALTER TABLE projects ADD COLUMN worker_provider TEXT DEFAULT '';
`,
	},
	{
		version: 7,
		desc:    "add per-foreman gateway_key column to projects",
		stmt: `
ALTER TABLE projects ADD COLUMN gateway_key TEXT DEFAULT '';
`,
	},
	{
		version: 8,
		desc:    "add sync_spool table for DuckBrain write fallback",
		stmt: `
CREATE TABLE IF NOT EXISTS sync_spool (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    mem_key    TEXT NOT NULL,
    domain     TEXT NOT NULL,
    content    TEXT NOT NULL,
    attempts   INTEGER NOT NULL DEFAULT 0,
    last_error TEXT DEFAULT '',
    created_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_sync_spool_created ON sync_spool(created_at);
`,
	},
	{
		version: 9,
		desc:    "add consecutive_failures counter to projects for spawn-failure backoff (S-GAP-001)",
		stmt: `
ALTER TABLE projects ADD COLUMN consecutive_failures INTEGER NOT NULL DEFAULT 0;
`,
	},
	{
		version: 10,
		desc:    "add heartbeat_at column to ticks for gateway-tick liveness (S-GAP-003)",
		stmt: `
ALTER TABLE ticks ADD COLUMN heartbeat_at TEXT;
`,
	},
	{
		version: 11,
		desc:    "partial covering index on ticks(status, completed_at) for /api/v1/status outcome counts (S-GAP-007)",
		stmt: `
CREATE INDEX IF NOT EXISTS idx_ticks_status_completed ON ticks(status, completed_at) WHERE completed_at IS NOT NULL;
`,
	},
	{
		version: 12,
		desc:    "add disable provenance columns to projects (GAP-044)",
		stmt: `
ALTER TABLE projects ADD COLUMN disabled_at TEXT;
ALTER TABLE projects ADD COLUMN disabled_by TEXT;
ALTER TABLE projects ADD COLUMN disabled_reason TEXT;
`,
	},
	{
		version: 13,
		desc:    "backfill disable provenance for pre-GAP-044 disabled rows (DOGFOOD-010)",
		stmt: `
UPDATE projects SET
    disabled_by = 'legacy',
    disabled_reason = 'pre-GAP-044 disable',
    disabled_at = COALESCE(disabled_at, COALESCE(updated_at, strftime('%Y-%m-%dT%H:%M:%SZ','now')))
WHERE enabled = 0 AND COALESCE(disabled_by, '') = '';
`,
	},
	{
		version: 14,
		desc:    "add model/provider fallback chain columns to projects (SCHED-GAP-064)",
		stmt: `
ALTER TABLE projects ADD COLUMN fallback_model TEXT DEFAULT '';
ALTER TABLE projects ADD COLUMN fallback_provider TEXT DEFAULT '';
ALTER TABLE projects ADD COLUMN no_global_fallback INTEGER NOT NULL DEFAULT 0;
`,
	},
	{
		version: 15,
		desc:    "add per-project budget cap columns to projects (SCHED-GAP-066)",
		stmt: `
ALTER TABLE projects ADD COLUMN daily_budget_usd REAL NOT NULL DEFAULT 0.0;
ALTER TABLE projects ADD COLUMN weekly_budget_usd REAL NOT NULL DEFAULT 0.0;
ALTER TABLE projects ADD COLUMN final_budget_usd REAL NOT NULL DEFAULT 0.0;
`,
	},
	{
		version: 16,
		desc:    "add idle-tick model routing columns to projects (SCHED-GAP-065)",
		stmt: `
ALTER TABLE projects ADD COLUMN idle_model TEXT NOT NULL DEFAULT '';
ALTER TABLE projects ADD COLUMN idle_provider TEXT NOT NULL DEFAULT '';
`,
	},
	{
		version: 17,
		desc:    "add ordered model chain column to projects (SCHED-GAP-075)",
		stmt: `
ALTER TABLE projects ADD COLUMN model_chain TEXT NOT NULL DEFAULT '';
`,
	},
	{
		version: 18,
		desc:    "configurable foreman prompts: namespace default_prompt + per-project prompt/prompt_mode (Bane 2026-08-27)",
		stmt: `
ALTER TABLE projects ADD COLUMN prompt TEXT NOT NULL DEFAULT '';
ALTER TABLE projects ADD COLUMN prompt_mode TEXT NOT NULL DEFAULT 'append';
ALTER TABLE namespaces ADD COLUMN default_prompt TEXT NOT NULL DEFAULT '';
`,
	},
	{
		version: 19,
		desc:    "per-namespace max_concurrent: cap ticks running at once per namespace (Bane 2026-08-27, duckbrain-sync serialization)",
		stmt: `
ALTER TABLE namespaces ADD COLUMN max_concurrent INTEGER NOT NULL DEFAULT 0 CHECK(max_concurrent >= 0);
`,
	},
	{
		version: 20,
		desc:    "namespace-level model chain: [[namespaces]].model_chain override tier between project and router (Bane 2026-08-27, 3-tier routing)",
		stmt: `
ALTER TABLE namespaces ADD COLUMN model_chain TEXT NOT NULL DEFAULT '';
`,
	},
	{
		version: 21,
		desc:    "session resume-after-restart (SCHED-GAP-091): orphan tracking + nudge counter on ticks",
		stmt: `
ALTER TABLE ticks ADD COLUMN orphaned_at TEXT;
ALTER TABLE ticks ADD COLUMN orphan_reason TEXT;
ALTER TABLE ticks ADD COLUMN nudge_count INTEGER NOT NULL DEFAULT 0;
`,
	},
	{
		version: 22,
		desc:    "opt-in per-project adaptive cooldown (auto slow-down / speed-up): no-progress streaks escalate cooldown_s to a ceiling; new board rows or commits reset it",
		stmt: `
ALTER TABLE projects ADD COLUMN adaptive_cooldown INTEGER NOT NULL DEFAULT 0;
ALTER TABLE projects ADD COLUMN cooldown_floor_s INTEGER NOT NULL DEFAULT 0;
ALTER TABLE projects ADD COLUMN cooldown_ceiling_s INTEGER NOT NULL DEFAULT 0;
ALTER TABLE projects ADD COLUMN no_progress_threshold INTEGER NOT NULL DEFAULT 0;
ALTER TABLE projects ADD COLUMN no_progress_ticks INTEGER NOT NULL DEFAULT 0;
ALTER TABLE projects ADD COLUMN board_rows_seen INTEGER NOT NULL DEFAULT -1;
`,
	},
	{
		version: 23,
		desc:    "zombie session reaper (SCHED-GAP-089): sessions table for api_server sessions + backfill ended_at for existing zombies",
		stmt: `
CREATE TABLE IF NOT EXISTS sessions (
    id         TEXT PRIMARY KEY,
    platform   TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL,
    updated_at TEXT,
    ended_at   TEXT
);

CREATE INDEX IF NOT EXISTS idx_sessions_zombie ON sessions(ended_at) WHERE ended_at IS NULL;

UPDATE sessions SET ended_at = COALESCE(updated_at, created_at)
 WHERE ended_at IS NULL AND platform = 'api_server';
`,
	},
	{
		version: 24,
		desc:    "commit-anatomy signals (SCHED-GAP-104): split tick commits into code vs board bookkeeping so adaptive cooldown measures real output",
		stmt: `
ALTER TABLE ticks ADD COLUMN code_commits INTEGER NOT NULL DEFAULT 0;
ALTER TABLE ticks ADD COLUMN board_commits INTEGER NOT NULL DEFAULT 0;
`,
	},
	{
		version: 25,
		desc:    "open-row baseline (SCHED-GAP-105): track open board rows so completion (not injection) is the board progress signal",
		stmt: `
ALTER TABLE projects ADD COLUMN board_open_seen INTEGER NOT NULL DEFAULT -1;
`,
	},
	{
		version: 26,
		desc:    "task bump (SCHED-GAP-107): temporary project-wide speed-up — bump state + pre-bump snapshot on projects, bump flag on ticks",
		stmt: `
ALTER TABLE projects ADD COLUMN bump_active INTEGER NOT NULL DEFAULT 0;
ALTER TABLE projects ADD COLUMN bump_remaining_ticks INTEGER NOT NULL DEFAULT 0;
ALTER TABLE projects ADD COLUMN bump_cooldown_s INTEGER NOT NULL DEFAULT 0;
ALTER TABLE projects ADD COLUMN bump_reason TEXT NOT NULL DEFAULT '';
ALTER TABLE projects ADD COLUMN bump_saved_cooldown_s INTEGER NOT NULL DEFAULT 0;
ALTER TABLE projects ADD COLUMN bump_saved_floor_s INTEGER NOT NULL DEFAULT 0;
ALTER TABLE projects ADD COLUMN bump_saved_ceiling_s INTEGER NOT NULL DEFAULT 0;
ALTER TABLE projects ADD COLUMN bump_saved_no_progress_ticks INTEGER NOT NULL DEFAULT 0;
ALTER TABLE projects ADD COLUMN bump_started_at TEXT NOT NULL DEFAULT '';
ALTER TABLE ticks ADD COLUMN bump INTEGER NOT NULL DEFAULT 0;
`,
	},
	{
		version: 27,
		desc:    "concurrent wave scheduling (S12): worker attribution on ticks + wave config on namespaces + tick_workers table",
		stmt: `
-- v27: concurrent wave scheduling (S12)
ALTER TABLE ticks ADD COLUMN worker_count INTEGER NOT NULL DEFAULT 0;
ALTER TABLE ticks ADD COLUMN wave_recovery INTEGER NOT NULL DEFAULT 0;

ALTER TABLE namespaces ADD COLUMN wave_enabled INTEGER NOT NULL DEFAULT 0 CHECK(wave_enabled IN (0, 1));
ALTER TABLE namespaces ADD COLUMN wave_tick_timeout TEXT NOT NULL DEFAULT '';
ALTER TABLE namespaces ADD COLUMN wave_workers_cap INTEGER NOT NULL DEFAULT 0 CHECK(wave_workers_cap >= 0);

CREATE TABLE IF NOT EXISTS tick_workers (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    tick_id      TEXT NOT NULL REFERENCES ticks(id) ON DELETE CASCADE,
    task_id      TEXT NOT NULL,
    branch       TEXT NOT NULL,
    worktree     TEXT NOT NULL DEFAULT '',
    commit_sha   TEXT NOT NULL DEFAULT '',
    judge        TEXT NOT NULL DEFAULT 'unknown'
                 CHECK(judge IN ('pass','fail','withdrawn','unknown')),
    merge        TEXT NOT NULL DEFAULT 'pending'
                 CHECK(merge IN ('merged','conflict','preserved','pending')),
    state        TEXT NOT NULL DEFAULT 'running'
                 CHECK(state IN ('running','done','abandoned')),
    cost_usd     REAL NOT NULL DEFAULT 0,
    tokens_in    INTEGER NOT NULL DEFAULT 0,
    tokens_out   INTEGER NOT NULL DEFAULT 0,
    created_at   TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at   TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE INDEX IF NOT EXISTS idx_tick_workers_tick ON tick_workers(tick_id);
CREATE INDEX IF NOT EXISTS idx_tick_workers_task ON tick_workers(task_id);
`,
	},
	{
		version: 28,
		desc:    "per-POST gateway trace on ticks (SCHED-GAP-119): start/finish/elapsed, deadline applied + mode, classification, session link, event count",
		stmt: `
ALTER TABLE ticks ADD COLUMN gateway_trace TEXT NOT NULL DEFAULT '';
`,
	},
	{
		version: 29,
		desc:    "cost provenance on ticks (ADV-R09/G8): measured | gateway | estimated | simulated — lets spend surfaces distinguish measured money from estimated money instead of laundering the estimate through as fact",
		stmt: `
ALTER TABLE ticks ADD COLUMN cost_source TEXT NOT NULL DEFAULT '';
`,
	},
	{
		version: 30,
		desc:    "admission modes (SCHED-GAP-124): namespace admission_mode (cooldown | tasks) + per-project override; tasks mode admits on non-perpetual pending board work instead of wall-clock cooldown",
		stmt: `
ALTER TABLE namespaces ADD COLUMN admission_mode TEXT NOT NULL DEFAULT 'cooldown' CHECK(admission_mode IN ('cooldown','tasks'));
ALTER TABLE projects ADD COLUMN admission_mode TEXT NOT NULL DEFAULT '' CHECK(admission_mode IN ('','cooldown','tasks'));
`,
	},
	{
		version: 31,
		desc:    "load gate (SCHED-GAP-125): namespace load_gate ('' | off) — namespaces may opt out of the global --load-gate-threshold deferral (e.g. always-on infra)",
		stmt: `
ALTER TABLE namespaces ADD COLUMN load_gate TEXT NOT NULL DEFAULT '' CHECK(load_gate IN ('','off'));
`,
	},
	{
		version: 32,
		desc:    "board ownership (SCHED-GAP-141): per-project board_ownership ('' = auto | owner | shared) — the tasks-mode cooldown waiver fires only for a lane that OWNS the board it reads",
		stmt: `
ALTER TABLE projects ADD COLUMN board_ownership TEXT NOT NULL DEFAULT '' CHECK(board_ownership IN ('','owner','shared'));
`,
	},
	{
		version: 33,
		desc:    "transport-class failure marker on ticks (SCHED-GAP-143): '' = not transport-class (project-side, and every legacy row) | gateway_drain | gateway_transport — a tick the harness refused (e.g. a 503 'Gateway is draining') never reached the project, so it must be classifiable in SQL instead of only by re-parsing ticks.error",
		stmt: `
ALTER TABLE ticks ADD COLUMN failure_reason TEXT NOT NULL DEFAULT '';
`,
	},
	{
		version: 34,
		desc:    "host load/memory telemetry (ADV-R13): one host_samples row per evaluation cycle — load1/load5/load15 and MemTotal/MemAvailable in bytes, source 'proc' — MEASUREMENT ONLY: no admission-path consumer reads these rows, any future load threshold must be derived from this recorded history instead of fabricated on zero measurements",
		stmt: `
CREATE TABLE IF NOT EXISTS host_samples (
    id                 INTEGER PRIMARY KEY AUTOINCREMENT,
    sampled_at         TEXT NOT NULL,
    load1              REAL NOT NULL,
    load5              REAL NOT NULL,
    load15             REAL NOT NULL,
    mem_total_bytes    INTEGER NOT NULL,
    mem_available_bytes INTEGER NOT NULL,
    source             TEXT NOT NULL DEFAULT ''
);
`,
	},
	{
		version: 35,
		desc:    "tick lifecycle persistence (SCHED-GAP-157): slot_wait_ms + admit_reason + nudge_source on ticks, and a deferrals table recording WHY a candidate was passed over — the two acceptance questions (\"how long did this lane wait for a slot\" / \"why was it skipped\") become single SQL queries instead of log-line order across a rotated file",
		stmt: `
ALTER TABLE ticks ADD COLUMN slot_wait_ms INTEGER NOT NULL DEFAULT 0;
ALTER TABLE ticks ADD COLUMN admit_reason TEXT NOT NULL DEFAULT '';
ALTER TABLE ticks ADD COLUMN nudge_source TEXT NOT NULL DEFAULT '';

CREATE TABLE IF NOT EXISTS deferrals (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    project_name TEXT NOT NULL,
    reason       TEXT NOT NULL,
    pass_id      INTEGER NOT NULL DEFAULT 0,
    detail       TEXT NOT NULL DEFAULT '',
    created_at   TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_deferrals_project_created ON deferrals(project_name, created_at);
CREATE INDEX IF NOT EXISTS idx_deferrals_created ON deferrals(created_at);
`,
	},
	{
		version: 36,
		desc:    "cost_source backfill: stamp 'legacy' on rows that pre-date metering (SCHED-GAP-127)",
		stmt:    `UPDATE ticks SET cost_source = 'legacy' WHERE cost_source = '';`,
	},
	{
		version: 37,
		desc:    "deferred tick status (SCHED-GAP-203): widen the ticks status/outcome vocabularies with 'deferred' — a transient gateway blip (refused connect, SSE stream ended without a terminal event) is a DEFERRAL the harness absorbed, not a lane failure, and the status column is where that verdict must be stored",
		// WHY A REBUILD AND NOT AN ALTER: SQLite cannot modify a CHECK
		// constraint in place. The vocabulary lives in the column's CHECK, so
		// the only way to admit one new value is to re-create the table and
		// copy every row — the documented rebuild procedure.
		//
		// WHY ownTx: the rebuild DROPs the `ticks` PARENT table, and its child
		// (`tick_workers.tick_id REFERENCES ticks(id) ON DELETE CASCADE`) would
		// lose its 95 live attribution rows to the cascade if enforcement were
		// ON. `PRAGMA foreign_keys=OFF` cannot be set inside the runner's
		// transaction, so this statement owns its transaction: pragma off
		// (autocommit), BEGIN … COMMIT around the rebuild, pragma on. The
		// BEGIN/COMMIT half is what makes a crash MID-REBUILD recoverable —
		// rolled back, the migration is simply re-run at the next boot; without
		// it a half-applied rebuild would fail on "table ticks_203b already
		// exists" and wedge the daemon's boot. `DROP TABLE IF EXISTS` covers the
		// one window the pragma toggling cannot (a leftover from an aborted
		// process) and keeps a re-run idempotent.
		//
		// COLUMN LIST: verbatim from the v1 CREATE plus every column added by
		// migrations 22-36, with the same types, NULL/NOT NULL and defaults —
		// the copy is positional-safe only because both sides list every column
		// by name in the same order.
		ownTx: true,
		stmt: `
PRAGMA foreign_keys=OFF;
BEGIN;
DROP TABLE IF EXISTS ticks_203b;
CREATE TABLE ticks_203b (
    id            TEXT PRIMARY KEY,
    project_name  TEXT NOT NULL REFERENCES projects(name),
    session_id    TEXT,
    pid           INTEGER DEFAULT 0,
    status        TEXT NOT NULL DEFAULT 'queued' CHECK(status IN ('queued','running','completed','failed','timeout','deferred')),
    outcome       TEXT CHECK(outcome IN ('committed','dry_run','failed','timeout','deferred')),
    spawned_at    TEXT,
    completed_at  TEXT,
    exit_code     INTEGER,
    commits       INTEGER DEFAULT 0,
    files_changed INTEGER DEFAULT 0,
    tokens_in     INTEGER DEFAULT 0,
    tokens_out    INTEGER DEFAULT 0,
    cost_usd      REAL DEFAULT 0.0,
    urgency       REAL DEFAULT 0.0,
    weight_used   INTEGER DEFAULT 0,
    error         TEXT,
    created_at    TEXT NOT NULL,
    heartbeat_at  TEXT,
    orphaned_at   TEXT,
    orphan_reason TEXT,
    nudge_count   INTEGER NOT NULL DEFAULT 0,
    code_commits  INTEGER NOT NULL DEFAULT 0,
    board_commits INTEGER NOT NULL DEFAULT 0,
    bump          INTEGER NOT NULL DEFAULT 0,
    worker_count  INTEGER NOT NULL DEFAULT 0,
    wave_recovery INTEGER NOT NULL DEFAULT 0,
    gateway_trace TEXT NOT NULL DEFAULT '',
    cost_source   TEXT NOT NULL DEFAULT '',
    failure_reason TEXT NOT NULL DEFAULT '',
    slot_wait_ms  INTEGER NOT NULL DEFAULT 0,
    admit_reason  TEXT NOT NULL DEFAULT '',
    nudge_source  TEXT NOT NULL DEFAULT ''
);
INSERT INTO ticks_203b (
    id, project_name, session_id, pid, status, outcome, spawned_at, completed_at,
    exit_code, commits, files_changed, tokens_in, tokens_out, cost_usd, urgency,
    weight_used, error, created_at, heartbeat_at, orphaned_at, orphan_reason,
    nudge_count, code_commits, board_commits, bump, worker_count, wave_recovery,
    gateway_trace, cost_source, failure_reason, slot_wait_ms, admit_reason, nudge_source
)
SELECT
    id, project_name, session_id, pid, status, outcome, spawned_at, completed_at,
    exit_code, commits, files_changed, tokens_in, tokens_out, cost_usd, urgency,
    weight_used, error, created_at, heartbeat_at, orphaned_at, orphan_reason,
    nudge_count, code_commits, board_commits, bump, worker_count, wave_recovery,
    gateway_trace, cost_source, failure_reason, slot_wait_ms, admit_reason, nudge_source
FROM ticks;
DROP TABLE ticks;
ALTER TABLE ticks_203b RENAME TO ticks;
CREATE INDEX IF NOT EXISTS idx_ticks_project_spawned ON ticks(project_name, spawned_at);
CREATE INDEX IF NOT EXISTS idx_ticks_status ON ticks(status);
CREATE INDEX IF NOT EXISTS idx_ticks_status_completed ON ticks(status, completed_at) WHERE completed_at IS NOT NULL;
COMMIT;
PRAGMA foreign_keys=ON;
`,
	},
	{
		version: 38,
		desc:    "cooldown pin provenance (SCHED-GAP-219): cooldown_pin_s + who/when columns on projects — the DB becomes the single cooldown authority and the operator pin becomes a durable row attribute",
		// Columns, not a side table: a pin is 1:1 with the project row and
		// every existing reader (loader, API) already round-trips the row.
		//   cooldown_pin_s   — the pinned value (NULL = no operator pin);
		//                      the pin never auto-lowers: writers must
		//                      honour "never silently LOWER an operator
		//                      pin" (the loader skips re-pinning below it).
		//   cooldown_pin_by  — who set it ("fleet-toml-import" on the v38
		//                      backfill, "api" on a PUT-set pin).
		//   cooldown_pin_at  — RFC3339 when the pin landed.
		//
		// THE BACKFILL imports the ELEVATED_PINS whitelist that is being
		// retired from ~/.hermes/scripts/fleet-cooldown-policy.py — these
		// are Bane's dated operator rulings (SCHED-GAP-012/121 lineage)
		// and must survive the script's retirement verbatim. Values match
		// the script's map at retirement time (2026-09-22 owner review);
		// rows that do not exist are silently skipped, and every UPDATE
		// is guarded so an already-pinned row keeps its provenance.
		stmt: `
ALTER TABLE projects ADD COLUMN cooldown_pin_s INTEGER;
ALTER TABLE projects ADD COLUMN cooldown_pin_by TEXT NOT NULL DEFAULT '';
ALTER TABLE projects ADD COLUMN cooldown_pin_at TEXT NOT NULL DEFAULT '';

UPDATE projects SET cooldown_pin_s = 86400, cooldown_pin_by = 'fleet-toml-import', cooldown_pin_at = strftime('%Y-%m-%dT%H:%M:%SZ','now') WHERE name = 'gitreins-qa' AND cooldown_pin_s IS NULL;
UPDATE projects SET cooldown_pin_s = 43200, cooldown_pin_by = 'fleet-toml-import', cooldown_pin_at = strftime('%Y-%m-%dT%H:%M:%SZ','now') WHERE name = 'h3' AND cooldown_pin_s IS NULL;
UPDATE projects SET cooldown_pin_s = 21600, cooldown_pin_by = 'fleet-toml-import', cooldown_pin_at = strftime('%Y-%m-%dT%H:%M:%SZ','now') WHERE name = 'warpfs' AND cooldown_pin_s IS NULL;
UPDATE projects SET cooldown_pin_s = 86400, cooldown_pin_by = 'fleet-toml-import', cooldown_pin_at = strftime('%Y-%m-%dT%H:%M:%SZ','now') WHERE name = 'hermes-canopy-releng' AND cooldown_pin_s IS NULL;
UPDATE projects SET cooldown_pin_s = 900,    cooldown_pin_by = 'fleet-toml-import', cooldown_pin_at = strftime('%Y-%m-%dT%H:%M:%SZ','now') WHERE name = 'hermes-dagger' AND cooldown_pin_s IS NULL;
UPDATE projects SET cooldown_pin_s = 21600, cooldown_pin_by = 'fleet-toml-import', cooldown_pin_at = strftime('%Y-%m-%dT%H:%M:%SZ','now') WHERE name = 'hermes-canopy' AND cooldown_pin_s IS NULL;
`,
	},
	{
		version: 39,
		desc:    "post-failure cooldown stamp (SCHED-GAP-214): last_tick_status on projects ('' = never ticked | completed | failed | timeout | deferred) — the terminal status of the project's most recent tick, stamped by lifecycle.Complete. The tasks-mode cooldown waiver (SCHED-GAP-124) consults it: after a FAILED tick the waiver stands down and the lane paces on its full effective cooldown, closing the 9-second retry-storm the waiver otherwise re-opens every gateway outage",
		stmt: `
ALTER TABLE projects ADD COLUMN last_tick_status TEXT NOT NULL DEFAULT '';
`,
	},
	{
		version: 40,
		desc:    "satellite namespace throughput (SCHED-GAP-215): raise the five big satellite families' max_concurrent from 1 to the ~1-slot-per-3-enabled-lanes policy (qa/pm/dogfood 9, releases 9, duckbrain-sync 12) and refresh the three descriptions that still claimed \"1 concurrent\". Supersedes the v19-era one-slot ruling for these five families: at 20-34 enabled lanes each, a cap of 1 serialized whole families behind single siblings (cap-gate deferral counter: 1445 by 2026-09-24; lane-lag 3-6x bucket = 45 of 150 ticked lanes). The 12-slot global ceiling still bounds the fleet, so the raise redistributes slots, it does not multiply them.",
		// Guarded backfill (v38's pattern): every UPDATE is conditioned on
		// the OLD value, so the statement is idempotent — re-running it on
		// an already-migrated DB (crash between UPDATE and the migrations
		// INSERT, or a hand-raised row) is a no-op, never a double-raise.
		// Rows are named, not family-matched, so a namespace added later
		// with cap 1 keeps its operator's value.
		stmt: `
UPDATE namespaces SET max_concurrent = 9, updated_at = strftime('%Y-%m-%dT%H:%M:%SZ','now') WHERE id = 'qa'             AND max_concurrent = 1;
UPDATE namespaces SET max_concurrent = 9, updated_at = strftime('%Y-%m-%dT%H:%M:%SZ','now') WHERE id = 'pm'             AND max_concurrent = 1;
UPDATE namespaces SET max_concurrent = 9, updated_at = strftime('%Y-%m-%dT%H:%M:%SZ','now') WHERE id = 'dogfood'        AND max_concurrent = 1;
UPDATE namespaces SET max_concurrent = 9, updated_at = strftime('%Y-%m-%dT%H:%M:%SZ','now') WHERE id = 'releases'       AND max_concurrent = 1;
UPDATE namespaces SET max_concurrent = 12, updated_at = strftime('%Y-%m-%dT%H:%M:%SZ','now') WHERE id = 'duckbrain-sync' AND max_concurrent = 1;
UPDATE namespaces SET description = 'QA lanes — clean-machine brittleness battery (skill qa-foreman-ops). Bunker path; the Dagger qa.ts executor is retired until the new dagger is built. Capped at 9 concurrent (SCHED-GAP-215: ~1 slot per 3 enabled lanes).' WHERE id = 'qa' AND description LIKE '%1 concurrent.%';
UPDATE namespaces SET description = 'Per-project PM lane — board hygiene: dedupe by content fingerprint, repair reused/malformed ids, normalise priorities, no refiling. Capped at 9 concurrent (SCHED-GAP-215: ~1 slot per 3 enabled lanes).' WHERE id = 'pm' AND description LIKE '%1 concurrent.%';
UPDATE namespaces SET description = 'DuckBrain namespace sync lanes — skill-driven focused sync (context-sync-duckbrain). Driver retired until the new dagger is built. Capped at 12 concurrent (SCHED-GAP-215: ~1 slot per 3 enabled lanes).' WHERE id = 'duckbrain-sync' AND description LIKE '%1 concurrent.%';
`,
	},
	{
		version: 41,
		desc:    "tick-report delivery mode (SCHED-GAP-1607): deliver_mode on projects ('' = full | full | file | link) — how a completed tick's report reaches the deliver target. full keeps the historical single-message shape (byte-identical); file sends the short header+footer message plus the complete report as a .md document attachment; link sends the short message plus one absolute dashboard URL built from --public-url. '' and unknown values resolve to full at delivery time, so pre-1607 rows are unchanged.",
		stmt: `
ALTER TABLE projects ADD COLUMN deliver_mode TEXT NOT NULL DEFAULT '';
`,
	},
}

// Migrate applies all pending migrations to db. Already-applied migrations
// are skipped, so this is safe to call on every startup (including against
// a freshly created schema).
func Migrate(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS migrations (
    version   INTEGER PRIMARY KEY,
    desc      TEXT NOT NULL,
    applied_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now'))
);
`); err != nil {
		return fmt.Errorf("create migrations table: %w", err)
	}

	for _, m := range migrations {
		applied, err := migrationApplied(ctx, db, m.version)
		if err != nil {
			return err
		}
		if applied {
			continue
		}

		if m.ownTx {
			// A table-rebuild migration owns its transaction: it must toggle
			// `PRAGMA foreign_keys` (a no-op inside a transaction), so the
			// runner cannot wrap it. Enforcement is re-asserted here no matter
			// how the statement ends — a failure path must never leave the
			// daemon's single connection unenforced (migration 37's DROP TABLE
			// would then cascade-delete tick_workers rows on a later retry).
			//
			// The rebuild and the version INSERT are therefore NOT one atomic
			// unit: a crash between them re-runs the migration on the next boot.
			// That is why such a migration must be idempotent by construction
			// (v37's DROP TABLE IF EXISTS + full re-copy).
			merr := func() error {
				defer func() { _, _ = db.ExecContext(ctx, `PRAGMA foreign_keys=ON`) }()
				_, err := db.ExecContext(ctx, m.stmt)
				return err
			}()
			if merr != nil {
				return fmt.Errorf("migration %d (%s): %w", m.version, m.desc, merr)
			}
			if _, err := db.ExecContext(ctx,
				`INSERT INTO migrations (version, desc) VALUES (?, ?)`,
				m.version, m.desc,
			); err != nil {
				return fmt.Errorf("record migration %d: %w", m.version, err)
			}
			continue
		}

		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("begin tx for migration %d: %w", m.version, err)
		}
		defer func() { _ = tx.Rollback() }()

		if _, err := tx.ExecContext(ctx, m.stmt); err != nil {
			// SQLite ALTER TABLE ADD COLUMN is not idempotent — if the column
			// already exists (e.g. added in a later revision of the initial
			// CREATE TABLE), treat "duplicate column name" as success.
			if strings.Contains(err.Error(), "duplicate column name") {
				// Fall through to record the migration as applied.
			} else {
				return fmt.Errorf("migration %d (%s): %w", m.version, m.desc, err)
			}
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO migrations (version, desc) VALUES (?, ?)`,
			m.version, m.desc,
		); err != nil {
			return fmt.Errorf("record migration %d: %w", m.version, err)
		}

		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit migration %d: %w", m.version, err)
		}
	}

	return nil
}

// migrationApplied reports whether version v has been recorded in the
// migrations table.
func migrationApplied(ctx context.Context, db *sql.DB, version int) (bool, error) {
	var v int
	err := db.QueryRowContext(ctx,
		`SELECT version FROM migrations WHERE version = ?`, version).Scan(&v)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("check migration %d: %w", version, err)
	}
	return true, nil
}

// MigrationVersion returns the highest applied migration version, or 0 if
// none have been recorded yet. Useful for diagnostics.
func MigrationVersion(ctx context.Context, db *sql.DB) (int, error) {
	var v int
	err := db.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) FROM migrations`).Scan(&v)
	if err != nil {
		return 0, fmt.Errorf("query migration version: %w", err)
	}
	return v, nil
}
