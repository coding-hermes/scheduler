package api_test

// GAP-060 (load hygiene): session-scoped schema template for newTestDB.
// The template is built once per test binary and each test receives a
// byte-level COPY in its own t.TempDir(), so tests stay fully isolated while
// the package's helper-mediated call sites pay one migration chain per
// binary instead of one per test. Same shape as the helpers this fleet uses
// in internal/database, internal/scheduler, and internal/dashboard.

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

func newTestDB(t *testing.T) *sql.DB {
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
	// caught exactly that in internal/database).
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
