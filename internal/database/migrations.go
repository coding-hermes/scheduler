package database

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// latestMigration is the highest migration version known to this build.
// Bump it when adding a new migration to the migrations slice below.
const latestMigration = 14

// migration describes a single forward-only schema change.
type migration struct {
	version int
	desc    string
	stmt    string
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
    model      TEXT NOT NULL DEFAULT 'your-model-name',
    provider   TEXT NOT NULL DEFAULT 'your-provider-name',
    worker_model      TEXT NOT NULL DEFAULT '',
    worker_provider   TEXT NOT NULL DEFAULT '',
    fallback_model    TEXT NOT NULL DEFAULT '',
    fallback_provider TEXT NOT NULL DEFAULT '',
    no_global_fallback INTEGER NOT NULL DEFAULT 0,
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
    status        TEXT NOT NULL DEFAULT 'queued' CHECK(status IN ('queued','running','completed','failed','timeout')),
    outcome       TEXT CHECK(outcome IN ('committed','dry_run','failed','timeout')),
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
    enabled     INTEGER NOT NULL DEFAULT 1 CHECK(enabled IN (0, 1)),
    description TEXT,
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
