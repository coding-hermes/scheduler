package database

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

// TestPerf006HookRunsBusyMigration proves the production receipt store admits
// quiesce-check's named busy verdict without losing pre-migration receipts.
// The pre-v85 premise is derived from the real ladder with v81's new token
// removed, so the test fails loudly if the migration ever becomes vacuous.
func TestPerf006HookRunsBusyMigration(t *testing.T) {
	if latestMigration < 85 {
		t.Fatalf("latestMigration = %d, want >= 85 (PERF-006 busy receipts)", latestMigration)
	}
	ctx := context.Background()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "v84.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	applyTestDurabilityOff(t, db)

	pre85 := make([]migration, 0, len(migrations))
	for _, m := range migrations {
		if m.version >= 85 {
			break
		}
		if m.version == 81 {
			m.stmt = strings.ReplaceAll(m.stmt, ",'busy'", "")
		}
		pre85 = append(pre85, m)
	}
	if err := applyMigrations(ctx, db, pre85); err != nil {
		t.Fatalf("build pre-v85 schema: %v", err)
	}
	_, err = db.ExecContext(ctx, `INSERT INTO hook_runs (
		tick_id, lane, phase, level, hook_id, priority, seq, command,
		state, exit_code, started_at, finished_at, duration_ms, attempts, output, argv
	) VALUES (
		't-old', 'cf-qa', 'post', 'lane', 'lane-merge', 80, 2,
		'/hooks/lane-merge.sh', 'skipped', 10,
		'2026-11-06T00:00:00Z', '2026-11-06T00:00:01Z', 1000, 1,
		'merge=no_commits', '/hooks/lane-merge.sh --push 0'
	)`)
	if err != nil {
		t.Fatalf("seed pre-v85 receipt: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`UPDATE hook_runs SET state='busy' WHERE tick_id='t-old'`); err == nil {
		t.Fatal("premise: pre-v85 hook_runs CHECK accepted busy")
	}

	if err := Migrate(ctx, db); err != nil {
		t.Fatalf("Migrate to v%d: %v", latestMigration, err)
	}
	version, err := MigrationVersion(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	if version != latestMigration {
		t.Fatalf("migration version = %d, want %d", version, latestMigration)
	}
	if _, err := db.ExecContext(ctx,
		`UPDATE hook_runs SET state='busy', exit_code=11 WHERE tick_id='t-old'`); err != nil {
		t.Fatalf("post-migration busy receipt rejected: %v", err)
	}
	var state, argv, output string
	var code, attempts int
	if err := db.QueryRowContext(ctx, `SELECT state, exit_code, argv, output, attempts
		FROM hook_runs WHERE tick_id='t-old'`).
		Scan(&state, &code, &argv, &output, &attempts); err != nil {
		t.Fatal(err)
	}
	if state != "busy" || code != 11 || argv != "/hooks/lane-merge.sh --push 0" ||
		output != "merge=no_commits" || attempts != 1 {
		t.Fatalf("rebuilt receipt drifted: state=%q code=%d argv=%q output=%q attempts=%d",
			state, code, argv, output, attempts)
	}
	if _, err := db.ExecContext(ctx,
		`UPDATE hook_runs SET state='bogus' WHERE tick_id='t-old'`); err == nil {
		t.Fatal("rebuilt hook_runs CHECK accepted an unknown state")
	}
	for _, index := range []string{"hook_runs_tick_idx", "hook_runs_hook_idx"} {
		var count int
		if err := db.QueryRowContext(ctx,
			`SELECT count(*) FROM sqlite_master WHERE type='index' AND name=?`, index).
			Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 1 {
			t.Errorf("index %s count = %d, want 1", index, count)
		}
	}
}
