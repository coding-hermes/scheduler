package database

import (
	"context"
	"strings"
	"testing"
)

// TestMigrationV47LaneOutputCounters pins the SCHED-GAP-177 migration: the
// eight per-family output columns land on projects, existing rows read the
// 0 default, and the ladder's latest version is 47.
func TestMigrationV47LaneOutputCounters(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	v, err := MigrationVersion(ctx, db)
	if err != nil {
		t.Fatalf("migration version: %v", err)
	}
	if v != latestMigration {
		t.Fatalf("migration version = %d, want %d", v, latestMigration)
	}
	if latestMigration != 47 {
		t.Fatalf("latestMigration = %d, want 47 (SCHED-GAP-177 lane-family output counters)", latestMigration)
	}

	var m47 *migration
	for i := range migrations {
		if migrations[i].version == 47 {
			m47 = &migrations[i]
			break
		}
	}
	if m47 == nil {
		t.Fatal("migration v47 missing from the ladder")
	}
	for _, col := range []string{
		"qa_output_count", "qa_zero_output_streak",
		"pm_output_count", "pm_zero_output_streak",
		"sync_output_count", "sync_zero_output_streak",
		"dogfood_output_count", "dogfood_zero_output_streak",
	} {
		if !strings.Contains(m47.stmt, col) {
			t.Errorf("v47 stmt does not add %s: %s", col, m47.stmt)
		}
	}

	// The columns actually exist post-migration and read 0 on a fresh row.
	if _, err := db.Exec(`INSERT INTO projects
		(name, repo_url, workdir, created_at, updated_at)
		VALUES ('v47-probe', 'https://example.com/x', '/tmp/x',
		datetime('now'), datetime('now'))`); err != nil {
		t.Fatalf("insert probe project: %v", err)
	}
	var qaOut, qaStreak, dogOut int
	err = db.QueryRow(`SELECT qa_output_count, qa_zero_output_streak, dogfood_output_count
FROM projects WHERE name = 'v47-probe'`).Scan(&qaOut, &qaStreak, &dogOut)
	if err != nil {
		t.Fatalf("read counters: %v", err)
	}
	if qaOut != 0 || qaStreak != 0 || dogOut != 0 {
		t.Errorf("fresh-row counters = (%d, %d, %d), want all 0 (DEFAULT 0)", qaOut, qaStreak, dogOut)
	}
}

// TestGetProjectReadsLaneOutputCounters proves the row-level scan surfaces
// the new columns (API GET /api/v1/projects/{name} and the list endpoint
// both scan through these paths).
func TestGetProjectReadsLaneOutputCounters(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	if _, err := db.Exec(`INSERT OR IGNORE INTO namespaces (id) VALUES ('qa')`); err != nil {
		t.Fatalf("seed namespace: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO projects
		(name, repo_url, workdir, created_at, updated_at, namespace_id, qa_output_count, qa_zero_output_streak)
		VALUES ('v47-scan', 'https://example.com/x', '/tmp/x',
		datetime('now'), datetime('now'), 'qa', 5, 3)`); err != nil {
		t.Fatalf("insert: %v", err)
	}

	p, err := GetProject(ctx, db, "v47-scan")
	if err != nil {
		t.Fatalf("GetProject: %v", err)
	}
	if p.QAOutputCount != 5 || p.QAZeroOutputStreak != 3 {
		t.Errorf("GetProject counters = (%d, %d), want (5, 3)", p.QAOutputCount, p.QAZeroOutputStreak)
	}
	if p.PMOutputCount != 0 || p.SyncOutputCount != 0 || p.DogfoodOutputCount != 0 {
		t.Errorf("untouched families = (%d, %d, %d), want all 0", p.PMOutputCount, p.SyncOutputCount, p.DogfoodOutputCount)
	}

	// List path (same scan columns).
	projects, err := ListProjects(ctx, db, false)
	if err != nil {
		t.Fatalf("ListProjects: %v", err)
	}
	for _, lp := range projects {
		if lp.Name == "v47-scan" {
			if lp.QAOutputCount != 5 {
				t.Errorf("ListProjects qa_output_count = %d, want 5", lp.QAOutputCount)
			}
			return
		}
	}
	t.Fatal("v47-scan not returned by ListProjects")
}
