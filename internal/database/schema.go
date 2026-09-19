// Package database provides the SQLite-backed operational store for the
// coding-hermes fleet scheduler — projects, ticks (scheduler runs), and events.
package database

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite" // pure-Go SQLite driver
)

// InitDB opens the SQLite database at dbPath, enables WAL mode and foreign-key
// enforcement, runs any pending migrations, and returns the ready *sql.DB.
//
// Pass ":memory:" for an ephemeral in-process database (used by tests).
func InitDB(dbPath string) (*sql.DB, error) {
	// SCHED-GAP-182: ensure the parent directory exists so a fresh-boot install
	// does not FATAL on PRAGMA journal_mode=WAL with "unable to open database
	// file (14)" when ~/.hermes/coding-hermes/ has never been created.
	if dbPath != ":memory:" {
		if dir := filepath.Dir(dbPath); dir != "" && dir != "." {
			if err := os.MkdirAll(dir, 0o750); err != nil {
				return nil, fmt.Errorf("mkdir db dir %q: %w", dir, err)
			}
		}
	}

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
		// DOGFOOD-006: bound WAL checkpoints. SQLite's default
		// wal_autocheckpoint is 1000 pages (~4MB); on the single shared
		// connection, the commit that crosses the threshold runs the whole
		// checkpoint synchronously and every queued read stalls behind it
		// (13s-class spikes on GET /api/v1/projects under tick load).
		// 200 pages (~800KB) keeps each crossing checkpoint small.
		"PRAGMA wal_autocheckpoint=200",
	}
	for _, p := range pragmas {
		if _, err := db.Exec(p); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("pragma %q: %w", p, err)
		}
	}

	// Compact any WAL left over from a previous run (e.g. after a crash)
	// before serving traffic, so the first checkpoint after boot is small
	// instead of a full multi-MB replay on the request path. TRUNCATE blocks
	// until done — at boot with a single connection there is no contention —
	// and fully resets the WAL file to zero bytes (PASSIVE does not).
	if _, err := db.Exec("PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("startup checkpoint: %w", err)
	}

	if err := Migrate(context.Background(), db); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}

	return db, nil
}
