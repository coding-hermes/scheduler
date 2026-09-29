package database

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// SCHED-GAP-1670 — the read-only handle the wake-residue detector runs on.
//
// The detector is scheduled, which means it runs on a box whose daemon owns
// the scheduler file. Its handle must therefore be provably incapable of
// writing: no lock taken from the live writer, no migration applied, and no
// row mutated even if a future caller asks it to. These tests are that proof —
// the negative arm (a write is refused) matters more than the positive one.

func TestOpenReadOnly_RefusesWritesAndLeavesRowsAlone(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "scheduler.db")
	write, err := InitDB(path)
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	if err := CreateProject(ctx, write, sampleProject("detector")); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	if err := write.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
	}

	db, err := OpenReadOnly(path)
	if err != nil {
		t.Fatalf("OpenReadOnly: %v", err)
	}
	defer db.Close()

	var before int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM projects`).Scan(&before); err != nil {
		t.Fatalf("read count: %v", err)
	}
	if before != 1 {
		t.Fatalf("projects count = %d, want 1", before)
	}

	// Every write class the detector might accidentally grow must be refused:
	// an UPDATE, an INSERT, a DELETE and a DDL statement.
	for name, stmt := range map[string]string{
		"update": `UPDATE projects SET cooldown_s = 1`,
		"insert": `INSERT INTO projects (name, repo_url, workdir) VALUES ('x', 'r', 'w')`,
		"delete": `DELETE FROM projects`,
		"ddl":    `ALTER TABLE projects ADD COLUMN detector_probe INTEGER`,
	} {
		if _, err := db.ExecContext(ctx, stmt); err == nil {
			t.Errorf("%s through the read-only handle succeeded, want a refusal", name)
		}
	}

	var after int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM projects`).Scan(&after); err != nil {
		t.Fatalf("re-read count: %v", err)
	}
	if after != before {
		t.Errorf("projects count = %d after refused writes, want %d", after, before)
	}
	var cooldown int
	if err := db.QueryRowContext(ctx, `SELECT cooldown_s FROM projects WHERE name = 'detector'`).Scan(&cooldown); err != nil {
		t.Fatalf("read cooldown: %v", err)
	}
	if cooldown != sampleProject("detector").CooldownS {
		t.Errorf("cooldown_s = %d after a refused UPDATE, want %d", cooldown, sampleProject("detector").CooldownS)
	}
}

func TestOpenReadOnly_NeverCreatesADatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "absent.db")
	if _, err := OpenReadOnly(path); err == nil {
		t.Fatal("OpenReadOnly(absent) = nil error, want an error")
	}
	if _, statErr := os.Stat(path); statErr == nil {
		t.Error("OpenReadOnly created the database file, want it to refuse instead (InitDB is the only creating entry point)")
	}
}

func TestOpenReadOnly_RejectsEmptyPath(t *testing.T) {
	if _, err := OpenReadOnly(""); err == nil {
		t.Error("OpenReadOnly(\"\") = nil error, want an error")
	}
}
