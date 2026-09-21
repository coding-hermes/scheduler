package scheduler

// GAP-060 (load hygiene): session-scoped schema template for newTestDB.
//
// This package called database.InitDB(":memory:") once per test (356 helper
// call sites); every call paid the full open + PRAGMA + v40-migration chain
// (13.97 ms per call measured on this host, 30-call probe). The template here
// is built ONCE per test binary and every test receives a byte-level COPY, so
// each test still owns a private, fully migrated database — no state can leak
// between tests and no assertion changes. Copy + open measured 8.55 ms per
// test vs 13.97 ms for a full InitDB(:memory:) (same 30-call probe).
//
// This file keeps gap060-prefixed helper names throughout, so the temporary
// InitDB cost probe (gap060_probe_test.go in internal/database, built only
// with -tags gap060Probe) can define its own differently-named copy helper
// without colliding.

import (
	"database/sql"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/coding-hermes/scheduler/internal/database"
)

var (
	gap060SchemaOnce sync.Once
	gap060SchemaDir  string
	gap060SchemaErr  error
)

// gap060SchemaTemplate builds the one-per-process empty-schema template
// database that newTestDB copies (GAP-060). Failure is sticky: every test
// fails with the same error rather than silently sharing a live database.
func gap060SchemaTemplate() (string, error) {
	gap060SchemaOnce.Do(func() {
		gap060SchemaDir, gap060SchemaErr = os.MkdirTemp("", "gap060-schema-*")
		if gap060SchemaErr != nil {
			return
		}
		db, err := database.InitDB(filepath.Join(gap060SchemaDir, "schema.db"))
		if err != nil {
			gap060SchemaErr = err
			return
		}
		gap060SchemaErr = db.Close()
	})
	return filepath.Join(gap060SchemaDir, "schema.db"), gap060SchemaErr
}

// copySQLiteShm byte-copies a closed WAL-mode SQLite database (db plus any
// -wal/-shm sidecars) so the copy opens as the same fully migrated schema.
func copySQLiteShm(t *testing.T, src, dst string) {
	t.Helper()
	for _, sfx := range []string{"", "-wal", "-shm"} {
		in, err := os.Open(src + sfx)
		if err != nil {
			if os.IsNotExist(err) && sfx != "" {
				continue
			}
			t.Fatalf("copy schema %s%s: %v", src, sfx, err)
		}
		out, err := os.Create(dst + sfx)
		if err != nil {
			in.Close()
			t.Fatalf("create copy %s%s: %v", dst, sfx, err)
		}
		_, err = out.ReadFrom(in)
		in.Close()
		out.Close()
		if err != nil {
			t.Fatalf("copy %s%s -> %s%s: %v", src, sfx, dst, sfx, err)
		}
	}
}

func newTestDBGap060(t *testing.T) *sql.DB {
	t.Helper()
	tmpl, err := gap060SchemaTemplate()
	if err != nil {
		t.Fatalf("build schema template: %v", err)
	}
	dst := filepath.Join(t.TempDir(), "test.db")
	copySQLiteShm(t, tmpl, dst)
	db, err := sql.Open("sqlite", dst)
	if err != nil {
		t.Fatalf("open copied test db: %v", err)
	}
	// Match database.InitDB's single-connection concurrency model.
	db.SetMaxOpenConns(1)
	// Mirror database.InitDB's per-CONNECTION pragma set: foreign_keys and
	// busy_timeout are connection-scoped, not stored in the file, so a
	// byte-copy alone would silently drop them (the FK-constraint tests
	// caught exactly that).
	applyGap060Pragmas(t, db)
	t.Cleanup(func() { db.Close() })
	return db
}

func applyGap060Pragmas(t *testing.T, db *sql.DB) {
	t.Helper()
	for _, p := range []string{
		"PRAGMA foreign_keys=ON",
		"PRAGMA busy_timeout=5000",
		"PRAGMA synchronous=NORMAL",
		"PRAGMA wal_autocheckpoint=200",
	} {
		if _, err := db.Exec(p); err != nil {
			t.Fatalf("pragma %q on copied test db: %v", p, err)
		}
	}
}
