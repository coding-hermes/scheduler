package database

import (
	"context"
	"path/filepath"
	"testing"
)

func TestPERF008Migration89Upgrade(t *testing.T) {
	ctx := context.Background()
	db, err := InitDB(filepath.Join(t.TempDir(), "scheduler.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	// Simulate an existing v88 database while preserving all preceding schema.
	for _, stmt := range []string{
		`DROP INDEX idx_ticks_created`,
		`DELETE FROM migrations WHERE version=89`,
		`INSERT INTO projects (name, repo_url, workdir, created_at, updated_at) VALUES ('upgrade','','','2026','2026')`,
		`INSERT INTO ticks (id, project_name, status, created_at) VALUES ('preserved','upgrade','queued','2026-01-02T00:00:00Z')`,
	} {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("prepare upgrade %q: %v", stmt, err)
		}
	}
	for i := 0; i < 2; i++ {
		if err := Migrate(ctx, db); err != nil {
			t.Fatalf("migration pass %d: %v", i+1, err)
		}
	}
	var count int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM migrations WHERE version=89`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("migration 89 count=%d err=%v", count, err)
	}
	var col string
	if err := db.QueryRowContext(ctx, `SELECT name FROM pragma_index_info('idx_ticks_created') WHERE seqno=0`).Scan(&col); err != nil || col != "created_at" {
		t.Fatalf("index column=%q err=%v", col, err)
	}
	var created string
	if err := db.QueryRowContext(ctx, `SELECT created_at FROM ticks WHERE id='preserved'`).Scan(&created); err != nil || created != "2026-01-02T00:00:00Z" {
		t.Fatalf("preserved tick created_at=%q err=%v", created, err)
	}
}
