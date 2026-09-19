package database

import (
	"os"
	"path/filepath"
	"testing"
)

// SCHED-GAP-182: InitDB must create the parent directory of dbPath before
// opening SQLite, so a fresh-boot install (where ~/.hermes/coding-hermes/ has
// never been created) does not FATAL on PRAGMA journal_mode=WAL with
// "unable to open database file (14)".

func TestInitDB_CreatesParentDir(t *testing.T) {
	// t.TempDir() exists, but the nested parents under it do not.
	dbPath := filepath.Join(t.TempDir(), "nested", "that", "does", "not", "exist", "test.db")
	parent := filepath.Dir(dbPath)

	if _, err := os.Stat(parent); !os.IsNotExist(err) {
		t.Fatalf("precondition: parent dir %q should not exist, stat err = %v", parent, err)
	}

	db, err := InitDB(dbPath)
	if err != nil {
		t.Fatalf("InitDB(%q) error = %v, want nil (parent dir should be created)", dbPath, err)
	}
	defer db.Close()

	if fi, err := os.Stat(parent); err != nil || !fi.IsDir() {
		t.Errorf("parent dir %q not created after InitDB: err=%v", parent, err)
	}
	if _, err := os.Stat(dbPath); err != nil {
		t.Errorf("db file %q does not exist after InitDB: %v", dbPath, err)
	}
}

func TestInitDB_ParentDirAlreadyExists(t *testing.T) {
	// Idempotency: an existing parent directory must not break InitDB.
	dbPath := filepath.Join(t.TempDir(), "already", "exists", "test.db")
	parent := filepath.Dir(dbPath)
	if err := os.MkdirAll(parent, 0o750); err != nil {
		t.Fatalf("precondition MkdirAll: %v", err)
	}

	db, err := InitDB(dbPath)
	if err != nil {
		t.Fatalf("InitDB(%q) error = %v, want nil (existing parent is fine)", dbPath, err)
	}
	defer db.Close()

	if fi, err := os.Stat(dbPath); err != nil || fi.IsDir() {
		t.Errorf("db file %q missing or unexpected after InitDB: err=%v", dbPath, err)
	}
}
