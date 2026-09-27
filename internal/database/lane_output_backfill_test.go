package database

import (
	"strings"
	"testing"
)

// TestSCHEDGAP177_BackfillCounts pins the v47 backfill: the family output
// counts must initialize from tick history with the SAME output predicate
// the runtime accounting uses (code_commits > 0 OR board_commits > 0, with
// the unmeasured -1/-1 sentinel falling open to the raw commit claim), and
// must scope per family namespace — a coding lane's output ticks land on no
// family column.
func TestSCHEDGAP177_BackfillCounts(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()

	seedNS := func(id string) {
		t.Helper()
		if _, err := db.Exec(
			`INSERT INTO namespaces (id) VALUES (?)`, id); err != nil {
			t.Fatalf("insert namespace %s: %v", id, err)
		}
	}
	for _, ns := range []string{"qa", "pm", "duckbrain-sync", "coding-hermes"} {
		seedNS(ns)
	}
	seedProj := func(name, ns string) {
		t.Helper()
		var nsArg any
		if ns != "" {
			nsArg = ns
		}
		if _, err := db.Exec(`INSERT INTO projects
			(name, repo_url, workdir, weight, priority, cooldown_s, decay_rate,
			 model, provider, enabled, created_at, updated_at, namespace_id)
			VALUES (?, 'https://github.com/example/x', '/tmp/x', 10, 5, 900, 1.0,
			 'm', 'p', 1, datetime('now'), datetime('now'), ?)`,
			name, nsArg); err != nil {
			t.Fatalf("insert project %s: %v", name, err)
		}
	}
	seedProj("bf-qa", "qa")
	seedProj("bf-pm", "pm")
	seedProj("bf-sync", "duckbrain-sync")
	seedProj("bf-code", "coding-hermes")

	seedTick := func(id, proj, status string, code, board, commits int) {
		t.Helper()
		if _, err := db.Exec(`INSERT INTO ticks
			(id, project_name, status, outcome, spawned_at, completed_at, commits,
			 code_commits, board_commits, created_at)
			VALUES (?, ?, ?, 'committed', datetime('now'), datetime('now'), ?, ?, ?, datetime('now'))`,
			id, proj, status, commits, code, board); err != nil {
			t.Fatalf("insert tick %s: %v", id, err)
		}
	}
	// Historical ticks in every anatomy shape the predicate distinguishes.
	seedTick("bf-1", "bf-qa", "completed", 2, 0, 2)   // measured code output
	seedTick("bf-2", "bf-qa", "completed", -1, -1, 1) // unmeasured, falls open to claim
	seedTick("bf-3", "bf-qa", "completed", 0, 0, 0)   // zero output — not counted
	seedTick("bf-4", "bf-qa", "deferred", 1, 0, 1)    // deferred — never ran a turn
	seedTick("bf-5", "bf-pm", "completed", 0, 3, 3)   // board-commit output
	seedTick("bf-6", "bf-pm", "timeout", 1, 0, 1)     // timeout WITH output counts
	seedTick("bf-7", "bf-sync", "completed", 0, 0, 0) // zero
	seedTick("bf-8", "bf-code", "completed", 5, 1, 6) // coding lane — never counted

	// Re-run the exact backfill statement the migration executed, against
	// the seeded history — it must produce the counts a fresh deploy of
	// this code derives from the same rows.
	if _, err := db.Exec(laneOutputBackfillStmt); err != nil {
		t.Fatalf("backfill exec: %v", err)
	}

	readCounts := func(name string) (qa, pm, sync, dog int) {
		t.Helper()
		if err := db.QueryRow(`SELECT qa_output_count, pm_output_count,
			sync_output_count, dogfood_output_count FROM projects WHERE name = ?`,
			name).Scan(&qa, &pm, &sync, &dog); err != nil {
			t.Fatalf("read counts for %s: %v", name, err)
		}
		return
	}

	if qa, _, _, _ := readCounts("bf-qa"); qa != 2 {
		t.Errorf("bf-qa qa_output_count = %d, want 2 (measured + unmeasured-with-claim; deferred and zero excluded)", qa)
	}
	if _, pm, _, _ := readCounts("bf-pm"); pm != 2 {
		t.Errorf("bf-pm pm_output_count = %d, want 2 (board-commit output + timeout-with-output)", pm)
	}
	if _, _, sync, _ := readCounts("bf-sync"); sync != 0 {
		t.Errorf("bf-sync sync_output_count = %d, want 0", sync)
	}
	if qa, pm, sync, dog := readCounts("bf-code"); qa|pm|sync|dog != 0 {
		t.Errorf("coding lane family counters = (%d,%d,%d,%d), want all 0", qa, pm, sync, dog)
	}
}

// TestSCHEDGAP177_MigrationLadder pins that v47 exists, carries the eight
// columns and the backfill, and that latestMigration moved with it.
func TestSCHEDGAP177_MigrationLadder(t *testing.T) {
	if latestMigration < 47 {
		t.Fatalf("latestMigration = %d, want >= 47 (SCHED-GAP-177 family output counters)", latestMigration)
	}
	found := false
	var v47 migration
	for _, m := range migrations {
		if m.version == 47 {
			found = true
			v47 = m
		}
	}
	if !found {
		t.Fatal("migration v47 (SCHED-GAP-177) missing from the ladder")
	}
	for _, col := range []string{
		"qa_output_count", "qa_zero_output_streak",
		"pm_output_count", "pm_zero_output_streak",
		"sync_output_count", "sync_zero_output_streak",
		"dogfood_output_count", "dogfood_zero_output_streak",
	} {
		if !strings.Contains(v47.stmt, col) {
			t.Errorf("v47 stmt does not add column %s", col)
		}
	}
	if !strings.Contains(v47.stmt, "UPDATE projects SET") {
		t.Errorf("v47 stmt does not carry the history backfill")
	}
}
