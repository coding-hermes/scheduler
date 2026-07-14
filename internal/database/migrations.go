package database

import (
	"context"
	"database/sql"
	"fmt"
)

// latestMigration is the highest migration version known to this build.
// Bump it when adding a new migration to the migrations slice below.
const latestMigration = 4

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
    model      TEXT NOT NULL DEFAULT 'deepseek-v4-pro',
    provider   TEXT NOT NULL DEFAULT 'deepseek-foreman',
    enabled    INTEGER NOT NULL DEFAULT 1,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS ticks (
    id            TEXT PRIMARY KEY,
    project_name  TEXT NOT NULL REFERENCES projects(name),
    session_id    TEXT,
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
			return fmt.Errorf("migration %d (%s): %w", m.version, m.desc, err)
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
