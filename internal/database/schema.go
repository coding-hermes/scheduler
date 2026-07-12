// Package database provides the SQLite-backed operational store for the
// coding-hermes fleet scheduler — projects, ticks (scheduler runs), and events.
package database

import (
	"context"
	"database/sql"
	"fmt"

	_ "modernc.org/sqlite" // pure-Go SQLite driver
)

// InitDB opens the SQLite database at dbPath, enables WAL mode and foreign-key
// enforcement, runs any pending migrations, and returns the ready *sql.DB.
//
// Pass ":memory:" for an ephemeral in-process database (used by tests).
func InitDB(dbPath string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("open sqlite %q: %w", dbPath, err)
	}

	// SQLite allows at most one writer in WAL mode, but multiple readers.
	// A single connection serialized through SetMaxOpenConns(1) is the
	// simplest correct concurrency model for this scheduler's write volume.
	db.SetMaxOpenConns(1)

	// PRAGMAs must run before any DDL/DML so the on-disk format is correct.
	pragmas := []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA foreign_keys=ON",
		"PRAGMA busy_timeout=5000",
		"PRAGMA synchronous=NORMAL",
	}
	for _, p := range pragmas {
		if _, err := db.Exec(p); err != nil {
			db.Close()
			return nil, fmt.Errorf("pragma %q: %w", p, err)
		}
	}

	if err := Migrate(context.Background(), db); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}

	return db, nil
}
