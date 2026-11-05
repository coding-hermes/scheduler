package database

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

// seedReadPoolFixture creates a scheduler.db with one project and one
// running tick — the minimum the PERF-001-R3-R3 tests need to observe a
// real read.
func seedReadPoolFixture(t *testing.T) string {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "scheduler.db")
	writeDB, err := InitDB(dbPath)
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	t.Cleanup(func() { _ = writeDB.Close() })
	if _, err := writeDB.Exec(`INSERT INTO projects (name, repo_url, workdir, created_at, updated_at)
VALUES ('perf-p1', 'https://example.invalid/perf-p1', '/tmp/perf-p1', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	if _, err := writeDB.Exec(`INSERT INTO ticks (id, project_name, status, spawned_at, created_at)
VALUES ('perf-t1', 'perf-p1', 'running', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`); err != nil {
		t.Fatalf("seed tick: %v", err)
	}
	return dbPath
}

// openWriteFixture re-opens the fixture's write pool (the seeder closes its
// handle only at test cleanup, so this returns the SAME database).
func openWriteFixture(t *testing.T, dbPath string) *sql.DB {
	t.Helper()
	writeDB, err := InitDB(dbPath)
	if err != nil {
		t.Fatalf("re-open write pool: %v", err)
	}
	t.Cleanup(func() { _ = writeDB.Close() })
	return writeDB
}

// holdWriteConnection grabs the write pool's ONLY connection by opening a
// write transaction — the shape the boot orphan-resume pass had while every
// handler SELECT queued behind it (PERF-001-R3-R3 probe evidence: the
// goroutine dump at t+3s showed the pass mid-INSERT while countActiveTicks
// waited on the same connection). The returned rollback releases it.
func holdWriteConnection(t *testing.T, writeDB *sql.DB) *sql.Tx {
	t.Helper()
	tx, err := writeDB.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatalf("BeginTx (hold the write connection): %v", err)
	}
	if _, err := tx.Exec(`INSERT INTO projects (name, repo_url, workdir, created_at, updated_at)
VALUES ('perf-hold', 'hold', 'hold', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`); err != nil {
		_ = tx.Rollback()
		t.Fatalf("hold write (insert): %v", err)
	}
	return tx
}

// TestOpenReadPool_ReaderNotBlockedByHeldWriteConnection pins the
// PERF-001-R3-R3 core property at the pool level: while the write pool's
// single connection is held inside a write transaction (the boot pass's
// shape), a SELECT through the read-only pool completes promptly. This is
// the property whose absence starved /health to a 504 at t+1s on the
// 16.7 GB copy.
func TestOpenReadPool_ReaderNotBlockedByHeldWriteConnection(t *testing.T) {
	dbPath := seedReadPoolFixture(t)
	writeDB := openWriteFixture(t, dbPath)

	ro, err := OpenReadPool(dbPath, 2)
	if err != nil {
		t.Fatalf("OpenReadPool: %v", err)
	}
	defer func() { _ = ro.Close() }()

	tx := holdWriteConnection(t, writeDB)
	defer func() { _ = tx.Rollback() }()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	started := time.Now()
	var name string
	if err := ro.QueryRowContext(ctx, `SELECT name FROM projects WHERE name = 'perf-p1'`).Scan(&name); err != nil {
		t.Fatalf("read while write connection held: %v", err)
	}
	if elapsed := time.Since(started); elapsed > 1500*time.Millisecond {
		t.Fatalf("reader queued behind the held write connection: answered in %s, want a prompt serve (the PERF-001-R3-R3 starvation shape)", elapsed)
	}
	if name != "perf-p1" {
		t.Fatalf("read returned %q, want perf-p1", name)
	}
}

// TestOpenReadPool_ManyReadersWhileWriterHoldsConnection proves the pool's
// concurrency (not just reader/writer independence): several parallel reads
// all complete while the single write connection stays held.
func TestOpenReadPool_ManyReadersWhileWriterHoldsConnection(t *testing.T) {
	dbPath := seedReadPoolFixture(t)
	writeDB := openWriteFixture(t, dbPath)

	ro, err := OpenReadPool(dbPath, 4)
	if err != nil {
		t.Fatalf("OpenReadPool: %v", err)
	}
	defer func() { _ = ro.Close() }()

	tx := holdWriteConnection(t, writeDB)
	defer func() { _ = tx.Rollback() }()

	errCh := make(chan error, 8)
	for i := 0; i < 8; i++ {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			var n int
			if err := ro.QueryRowContext(ctx, `SELECT COUNT(*) FROM ticks WHERE status = 'running'`).Scan(&n); err != nil {
				errCh <- err
				return
			}
			if n != 1 {
				errCh <- &countMismatchError{got: n}
				return
			}
			errCh <- nil
		}()
	}
	for i := 0; i < 8; i++ {
		if err := <-errCh; err != nil {
			t.Fatalf("parallel read while write connection held: %v", err)
		}
	}
}

type countMismatchError struct{ got int }

func (e *countMismatchError) Error() string {
	return "running-tick count mismatch through the read pool"
}

// TestOpenReadPool_WriteRefused pins the safety half of the contract: the
// pool cannot mutate the database. The api package routes only SELECT
// surfaces through it; this test is the seatbelt that keeps the invariant
// verifiable if a future call site hands the pool to a write path by
// mistake.
func TestOpenReadPool_WriteRefused(t *testing.T) {
	dbPath := seedReadPoolFixture(t)

	ro, err := OpenReadPool(dbPath, 1)
	if err != nil {
		t.Fatalf("OpenReadPool: %v", err)
	}
	defer func() { _ = ro.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := ro.ExecContext(ctx, `INSERT INTO projects (name, repo_url, workdir, created_at, updated_at)
VALUES ('x', 'x', 'x', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`); err == nil {
		t.Fatal("INSERT through the read-only pool succeeded — the pool is not read-only")
	}
}
