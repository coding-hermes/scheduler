package database

import (
	"context"
	"testing"
	"time"
)

// TestLoadNamespaceTickCounts pins the trailing-window per-namespace
// completed-tick counts that back the cadence ceiling (SCHED-GAP-1686): a tick
// counts toward its lane's namespace once it COMPLETED within the window; a
// running/queued tick (NULL completed_at), a tick outside the window, and a
// lane with no namespace are all excluded.
func TestLoadNamespaceTickCounts(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()
	ctx := context.Background()

	if _, err := db.Exec(`INSERT INTO namespaces (id) VALUES ('ns-a'), ('ns-b')`); err != nil {
		t.Fatalf("insert namespaces: %v", err)
	}
	// proj-a → ns-a, proj-b → ns-b, proj-orphan → no namespace.
	for _, row := range [][2]any{{"proj-a", "ns-a"}, {"proj-b", "ns-b"}, {"proj-orphan", nil}} {
		if _, err := db.Exec(`INSERT INTO projects
			(name, repo_url, workdir, weight, priority, cooldown_s, decay_rate,
			 model, provider, enabled, created_at, updated_at, namespace_id)
			VALUES (?, 'https://x', '/tmp/x', 10, 5, 900, 1.0, 'm', 'p', 1,
			        datetime('now'), datetime('now'), ?)`, row[0], row[1]); err != nil {
			t.Fatalf("insert project %v: %v", row[0], err)
		}
	}

	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	seedTick := func(id, proj, status string, completedAt *time.Time) {
		t.Helper()
		var ca any
		if completedAt != nil {
			ca = completedAt.UTC().Format(time.RFC3339)
		}
		if _, err := db.Exec(`INSERT INTO ticks
			(id, project_name, status, outcome, spawned_at, completed_at, commits,
			 code_commits, board_commits, created_at)
			VALUES (?, ?, ?, 'committed', datetime('now'), ?, 0, 0, 0, datetime('now'))`,
			id, proj, status, ca); err != nil {
			t.Fatalf("insert tick %s: %v", id, err)
		}
	}
	in := now.Add(-1 * time.Hour)
	out := now.Add(-48 * time.Hour)
	// ns-a: two completed ticks in-window, one outside, one running (NULL).
	seedTick("t-1", "proj-a", "completed", &in)
	seedTick("t-2", "proj-a", "failed", &in)
	seedTick("t-3", "proj-a", "completed", &out)
	seedTick("t-4", "proj-a", "running", nil)
	// ns-b: one completed tick in-window.
	seedTick("t-5", "proj-b", "completed", &in)
	// orphan: completed in-window but no namespace — excluded.
	seedTick("t-6", "proj-orphan", "completed", &in)

	counts, err := LoadNamespaceTickCounts(ctx, db, now, 24*time.Hour)
	if err != nil {
		t.Fatalf("LoadNamespaceTickCounts: %v", err)
	}
	if counts["ns-a"] != 2 {
		t.Errorf("ns-a count = %d, want 2 (two completed in-window; running/out-of-window excluded)", counts["ns-a"])
	}
	if counts["ns-b"] != 1 {
		t.Errorf("ns-b count = %d, want 1", counts["ns-b"])
	}
	if _, ok := counts[""]; ok {
		t.Errorf("orphan (no namespace) leaked a count: %v", counts)
	}
	if _, ok := counts["ns-orphan"]; ok {
		t.Errorf("unexpected orphan namespace key: %v", counts)
	}
}

func TestLoadNamespaceTickCounts_RejectsNonPositiveWindow(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()
	if _, err := LoadNamespaceTickCounts(context.Background(), db, time.Now(), 0); err == nil {
		t.Fatal("zero window must error")
	}
}
