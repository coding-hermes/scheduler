package database

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// GAP-060 load-hygiene probe (temporary, runs only via -run Gap060Probe).
// Measures: InitDB(:memory:) vs InitDB(file) vs blank-copy(file) startup cost.

func gap060TimeInit(t *testing.T, path string) time.Duration {
	t.Helper()
	start := time.Now()
	db, err := InitDB(path)
	if err != nil {
		t.Fatalf("InitDB(%s): %v", path, err)
	}
	d := time.Since(start)
	db.Close()
	return d
}

func TestGap060Probe_InitDBCosts(t *testing.T) {
	const n = 30
	var memSum, fileSum, copySum time.Duration
	dir := t.TempDir()
	tmpl := filepath.Join(dir, "tmpl.db")

	for range n {
		memSum += gap060TimeInit(t, ":memory:")

		f := filepath.Join(dir, "file"+t.Name()+".db")
		fileSum += gap060TimeInit(t, f)

		// Build template once, then measure blank-copy cost.
		if _, err := os.Stat(tmpl); err != nil {
			if db, err := InitDB(tmpl); err != nil {
				t.Fatalf("template InitDB: %v", err)
			} else {
				db.Close()
			}
		}
		dst := filepath.Join(dir, "copy"+t.Name()+".db")
		start := time.Now()
		if err := copySQLite(t, tmpl, dst); err != nil {
			t.Fatalf("copy: %v", err)
		}
		db, err := sql.Open("sqlite", dst)
		if err != nil {
			t.Fatalf("open copy: %v", err)
		}
		db.SetMaxOpenConns(1)
		if _, err := db.Exec("PRAGMA foreign_keys=ON"); err != nil {
			t.Fatalf("pragma copy: %v", err)
		}
		var v int
		if err := db.QueryRowContext(context.Background(),
			`SELECT count(*) FROM migrations`).Scan(&v); err != nil {
			t.Fatalf("verify copy: %v", err)
		}
		if v == 0 {
			t.Fatal("copy has no migrations recorded")
		}
		db.Close()
		copySum += time.Since(start)
	}
	t.Logf("InitDB(:memory:) avg=%v  InitDB(file) avg=%v  blank-copy avg=%v  (n=%d)",
		memSum/time.Duration(n), fileSum/time.Duration(n), copySum/time.Duration(n), n)
}

func copySQLite(t *testing.T, src, dst string) error {
	t.Helper()
	for _, sfx := range []string{"", "-wal", "-shm"} {
		in, err := os.Open(src + sfx)
		if err != nil {
			if os.IsNotExist(err) && sfx != "" {
				continue
			}
			return err
		}
		defer in.Close()
		out, err := os.Create(dst + sfx)
		if err != nil {
			return err
		}
		defer out.Close()
		if _, err := out.ReadFrom(in); err != nil {
			return err
		}
	}
	return nil
}
