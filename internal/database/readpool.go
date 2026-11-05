package database

import (
	"database/sql"
	"errors"
	"fmt"
	"os"

	_ "modernc.org/sqlite" // pure-Go sqlite driver, registered as "sqlite"
)

// DefaultReadPoolSize is the connection count OpenReadPool arms when the
// caller does not pick one (PERF-001-R3-R3). Four covers the six-endpoint
// boot probe (health/status/projects/cadence/dashboard each issue sequential
// reads, so the simultaneous demand is ~4-6 conns) without inviting the WAL
// growth a large idle reader herd can cause (readers pin snapshots against
// checkpoint truncation; reads are bounded by the request deadlines).
const DefaultReadPoolSize = 4

// OpenReadPool opens a READ-ONLY connection POOL against the scheduler
// database (PERF-001-R3-R3).
//
// The daemon's WRITE pool stays SetMaxOpenConns(1) (schema.go) — that
// single-connection invariant is load-bearing for the PRAGMA
// foreign_keys=OFF/ON pairing in PurgeProject/PurgeNamespace and the
// FK-toggling migrations, which are race-free ONLY while database/sql can
// never run the pairing on two different connections. A mode=ro connection
// cannot write, so it can never enter that FK window; and SQLite WAL lets
// any number of readers proceed concurrently with the one writer. That is
// exactly the two properties the boot-window starvation diagnosis needed:
// the boot orphan-resume pass holds the single write connection for its
// event INSERTs, and before this pool every handler SELECT queued behind
// it (measured: /health 504 at 1.001s, /status 5.17s at t+5s on a
// live-sized 16.7 GB copy — see the PERF-001-R3-R3 probe evidence).
//
// Callers MUST treat the returned handle as read-only. Production wiring is
// main.go → api.Server.SetReadDB / dashboard.Generator.SetDB; the api
// Server's rdb() accessor falls back to the shared write handle when this
// pool was never installed (every existing test Server), so a caller that
// forgets the setter loses the concurrency win, never correctness.
//
// This is deliberately NOT InitDB: no journal_mode switch, no migration, no
// checkpoint — the same side-effect-free contract as OpenReadOnly, which
// this function generalizes to N connections. maxConns <= 0 selects
// DefaultReadPoolSize.
func OpenReadPool(path string, maxConns int) (*sql.DB, error) {
	if path == "" {
		return nil, errors.New("no scheduler database path")
	}
	if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("scheduler database %s not accessible: %w", path, err)
	}
	if maxConns <= 0 {
		maxConns = DefaultReadPoolSize
	}
	dsn := fmt.Sprintf("file:%s?mode=ro&_pragma=busy_timeout(%d)", path, schedulerBusyTimeoutMS)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open scheduler read pool %s: %w", path, err)
	}
	// Keep every pooled connection warm: boot-window requests arrive while
	// the OS page cache is cold, and a fresh connection per request would
	// re-open the file (and re-read its header pages) exactly when latency
	// matters most.
	db.SetMaxOpenConns(maxConns)
	db.SetMaxIdleConns(maxConns)
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("open scheduler read pool %s: %w", path, err)
	}
	return db, nil
}
