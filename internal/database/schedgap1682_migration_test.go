package database

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

// SCHED-GAP-1682 acceptance at the STORAGE layer.
//
// The verdict is only real if the database stores it, and only SAFE if the
// migration is additive on a database that already holds the fleet's tick
// history:
//
//   - a FRESH schema carries projects.noop_allowed (NULLABLE) and the six
//     tick trigger columns (pre_commit/pre_branch/pre_board,
//     post_commit/post_branch, noop_flag);
//   - an EXISTING pre-1682 (v67) database upgrades in place: the project
//     override reads NULL on rows already there (the lane-class
//     derivation — never a fabricated policy), legacy ticks keep reading
//     their historical data with honest-zero trigger columns;
//   - the trigger round-trips through the real write path
//     (StampTickPreTrigger → RecordTickNoopVerdict) and the noop_flag=1
//     verdict lands atomically with the post capture.

// TestSCHEDGAP1682_FreshSchemaCarriesNoopColumns pins the fresh-install
// shape: projects.noop_allowed is present and NULLABLE (a NULL must mean
// "derive from the lane class", so a NOT NULL column would be a defect),
// and the tick row carries the five capture columns + the flag with
// honest-zero defaults.
func TestSCHEDGAP1682_FreshSchemaCarriesNoopColumns(t *testing.T) {
	db, err := InitDB(":memory:")
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	defer db.Close()

	v, err := MigrationVersion(context.Background(), db)
	if err != nil {
		t.Fatalf("MigrationVersion: %v", err)
	}
	if v < 68 {
		t.Fatalf("migration version = %d, want >= 68", v)
	}

	// projects.noop_allowed must be NULLABLE.
	var notNull int
	if err := db.QueryRow(
		`SELECT "notnull" FROM pragma_table_info('projects') WHERE name = 'noop_allowed'`).Scan(&notNull); err != nil {
		t.Fatalf("projects.noop_allowed missing — migration 68 did not land: %v", err)
	}
	if notNull != 0 {
		t.Fatalf("projects.noop_allowed is NOT NULL — the column must stay nullable: NULL = derive from lane class")
	}

	for _, col := range []string{"pre_commit", "pre_branch", "pre_board", "post_commit", "post_branch", "noop_flag"} {
		var got string
		if err := db.QueryRow(
			`SELECT name FROM pragma_table_info('ticks') WHERE name = ?`, col).Scan(&got); err != nil {
			t.Errorf("ticks.%s is missing — migration 68 did not land", col)
		}
	}
}

// schedGap1682V67DB builds a genuine pre-1682 (v67) database: the full
// ladder except v68, with a project and a legacy tick already in it.
func schedGap1682V67DB(t *testing.T) *sql.DB {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "v67.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open sqlite %s: %v", dbPath, err)
	}
	t.Cleanup(func() { db.Close() })
	applyTestDurabilityOff(t, db)
	ctx := context.Background()

	if _, err := db.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS migrations (
    version   INTEGER PRIMARY KEY,
    desc      TEXT NOT NULL,
    applied_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now'))
);`); err != nil {
		t.Fatalf("create migrations table: %v", err)
	}

	for _, m := range migrations {
		if m.version >= 68 {
			break // stop before v68 — the migration under test
		}
		if _, err := db.ExecContext(ctx, m.stmt); err != nil {
			// Mirror Migrate's tolerance (revised CREATE TABLEs replay
			// as "duplicate column name").
			if !containsDuplicateColumn(err.Error()) {
				t.Fatalf("apply v%d (%s): %v", m.version, m.desc, err)
			}
		}
		if _, err := db.ExecContext(ctx,
			`INSERT INTO migrations (version, desc) VALUES (?, ?)`, m.version, m.desc); err != nil {
			t.Fatalf("record v%d: %v", m.version, err)
		}
	}
	return db
}

func containsDuplicateColumn(msg string) bool {
	return indexOf(msg, "duplicate column name") >= 0
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// TestSCHEDGAP1682_UpgradeV67_MigratesCleanlyWithLegacyRows is A1's
// existing-DB arm: migrate the v67 database to HEAD and prove (a) the
// project row's override reads NULL (the lane-class derivation — never a
// fabricated policy), (b) the legacy tick survives with honest-zero
// trigger columns, and (c) a NEW project created through the real write
// path carries an explicit override intact.
func TestSCHEDGAP1682_UpgradeV67_MigratesCleanlyWithLegacyRows(t *testing.T) {
	db := schedGap1682V67DB(t)
	ctx := context.Background()

	if _, err := db.ExecContext(ctx, `INSERT INTO projects (name, repo_url, workdir, enabled, created_at, updated_at)
		VALUES ('legacy-lane', 'https://example.com/l', '/tmp/l', 1, datetime('now'), datetime('now'))`); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO ticks (id, project_name, status, outcome, created_at)
		VALUES ('legacy-t1', 'legacy-lane', 'completed', 'committed', datetime('now'))`); err != nil {
		t.Fatalf("seed legacy tick: %v", err)
	}

	if err := Migrate(ctx, db); err != nil {
		t.Fatalf("migrate v67 -> HEAD: %v", err)
	}

	// (a) the legacy project's override reads NULL.
	p, err := GetProject(ctx, db, "legacy-lane")
	if err != nil {
		t.Fatalf("read legacy project: %v", err)
	}
	if p.NoopAllowed != nil {
		t.Fatalf("legacy project noop_allowed = %v, want nil (NULL = lane-class derivation)", *p.NoopAllowed)
	}

	// (b) the legacy tick survives with honest-zero trigger columns.
	var preCommit, postCommit string
	var noopFlag int
	if err := db.QueryRowContext(ctx,
		`SELECT pre_commit, post_commit, noop_flag FROM ticks WHERE id = 'legacy-t1'`).Scan(&preCommit, &postCommit, &noopFlag); err != nil {
		t.Fatalf("read legacy tick: %v", err)
	}
	if preCommit != "" || postCommit != "" || noopFlag != 0 {
		t.Fatalf("legacy tick trigger columns = (%q,%q,%d), want ('','',0)", preCommit, postCommit, noopFlag)
	}

	// (c) the explicit override round-trips through the real write path.
	if err := UpdateProject(ctx, db, "legacy-lane", ProjectUpdates{NoopAllowed: BoolPtr(false)}); err != nil {
		t.Fatalf("update noop_allowed: %v", err)
	}
	p2, err := GetProject(ctx, db, "legacy-lane")
	if err != nil {
		t.Fatalf("re-read project: %v", err)
	}
	if p2.NoopAllowed == nil || *p2.NoopAllowed {
		t.Fatalf("noop_allowed = %v, want &false (explicit override stored)", p2.NoopAllowed)
	}
}

// TestSCHEDGAP1682_TriggerRoundTrip pins the storage contract of the
// trigger: the pre capture lands at spawn time, and the close writes the
// post capture + the flag in ONE UPDATE (the row never carries a post
// measurement without its verdict).
func TestSCHEDGAP1682_TriggerRoundTrip(t *testing.T) {
	db, err := InitDB(":memory:")
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	defer db.Close()
	ctx := context.Background()

	if err := CreateProject(ctx, db, &Project{
		Name: "round-trip", RepoURL: "https://example.com/r", Workdir: "/tmp/r",
		Weight: 10, Priority: 5, CooldownS: 900,
		Enabled: true, CreatedAt: "2026-10-10T00:00:00Z", UpdatedAt: "2026-10-10T00:00:00Z",
	}); err != nil {
		t.Fatalf("create project: %v", err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO ticks (id, project_name, status, created_at)
		VALUES ('rt-t1', 'round-trip', 'running', datetime('now'))`); err != nil {
		t.Fatalf("seed tick: %v", err)
	}

	// Pre capture (spawn time).
	if err := StampTickPreTrigger(ctx, db, "rt-t1", "aaa111", "main", "mt:1"); err != nil {
		t.Fatalf("stamp pre: %v", err)
	}
	var preCommit, preBranch, preBoard string
	if err := db.QueryRowContext(ctx,
		`SELECT pre_commit, pre_branch, pre_board FROM ticks WHERE id = 'rt-t1'`).Scan(&preCommit, &preBranch, &preBoard); err != nil {
		t.Fatalf("read pre: %v", err)
	}
	if preCommit != "aaa111" || preBranch != "main" || preBoard != "mt:1" {
		t.Fatalf("pre = (%q,%q,%q), want (aaa111,main,mt:1)", preCommit, preBranch, preBoard)
	}

	// Close: post capture + verdict, one UPDATE.
	if err := RecordTickNoopVerdict(ctx, db, "rt-t1", "bbb222", "feature", 0); err != nil {
		t.Fatalf("record verdict: %v", err)
	}
	var postCommit, postBranch string
	var flag int
	if err := db.QueryRowContext(ctx,
		`SELECT post_commit, post_branch, noop_flag FROM ticks WHERE id = 'rt-t1'`).Scan(&postCommit, &postBranch, &flag); err != nil {
		t.Fatalf("read verdict: %v", err)
	}
	if postCommit != "bbb222" || postBranch != "feature" || flag != 0 {
		t.Fatalf("post = (%q,%q,%d), want (bbb222,feature,0)", postCommit, postBranch, flag)
	}

	// The enforcement verdict lands with its evidence.
	if err := RecordTickNoopVerdict(ctx, db, "rt-t1", "aaa111", "main", 1); err != nil {
		t.Fatalf("record noop verdict: %v", err)
	}
	if err := db.QueryRowContext(ctx,
		`SELECT post_commit, post_branch, noop_flag FROM ticks WHERE id = 'rt-t1'`).Scan(&postCommit, &postBranch, &flag); err != nil {
		t.Fatalf("re-read verdict: %v", err)
	}
	if postCommit != "aaa111" || postBranch != "main" || flag != 1 {
		t.Fatalf("enforcement verdict = (%q,%q,%d), want (aaa111,main,1)", postCommit, postBranch, flag)
	}
}
