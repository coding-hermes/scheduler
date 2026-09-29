package database

import (
	"database/sql"
	"errors"
	"fmt"
	"os"

	_ "modernc.org/sqlite" // pure-Go sqlite driver, registered as "sqlite"
)

// OpenReadOnly opens the scheduler database READ-ONLY (SCHED-GAP-1670).
//
// The wake-residue detector only ever READS the live scheduler DB (projects:
// cooldown_s, cooldown_pin_s, enabled), so its handle is mode=ro: it can never
// take a write lock from the running daemon, never apply a migration, and
// never mutate a row — which is what makes a scheduled detector safe to run on
// the box whose daemon owns the file. A missing file is an error, never a
// fresh database: READ-ONLY must not create one.
//
// This is deliberately NOT InitDB. InitDB opens read-write and applies every
// pending migration, exactly the class of side effect a read-only surface must
// not have.
func OpenReadOnly(path string) (*sql.DB, error) {
	if path == "" {
		return nil, errors.New("no scheduler database path")
	}
	if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("scheduler database %s not accessible: %w", path, err)
	}
	dsn := fmt.Sprintf("file:%s?mode=ro&_pragma=busy_timeout(%d)", path, BusyTimeoutMS)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open scheduler database read-only %s: %w", path, err)
	}
	// One connection, mirroring InitDB: readers queue behind the live writer's
	// checkpoint instead of piling connections onto a WAL file.
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("open scheduler database read-only %s: %w", path, err)
	}
	return db, nil
}
