package scheduler

import (
	"database/sql"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

// TestTasksNoopStreak_RecordedThenReset — the RECORDING half of the no-op
// pacing fix, driven through the real adaptiveCooldown entry point.
//
// The admission gate can only pace a lane whose streak actually moves. On the
// pre-fix tree it never did for the measured lane: task-router-foreman has
// adaptive_cooldown=0, and the only writer of projects.no_progress_ticks sat
// behind that opt-in check (adaptive_cooldown.go), so 63 dry_run ticks in one
// day left the column at 0 and the gate had nothing to read.
//
// This test therefore uses adaptive_cooldown=0 — the measured lane's real
// config — and asserts the streak moves anyway. It FAILS on the pre-fix tree
// at the first assertion (streak stays 0).
func TestTasksNoopStreak_RecordedThenReset(t *testing.T) {
	dir := t.TempDir()
	db, err := sql.Open("sqlite", filepath.Join(dir, "spin.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE namespaces (id TEXT PRIMARY KEY, admission_mode TEXT DEFAULT '')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE projects (name TEXT PRIMARY KEY, admission_mode TEXT DEFAULT '',
		namespace_id TEXT, adaptive_cooldown INTEGER DEFAULT 0, cooldown_floor_s INTEGER DEFAULT 0,
		cooldown_ceiling_s INTEGER DEFAULT 0, no_progress_threshold INTEGER DEFAULT 0,
		no_progress_ticks INTEGER DEFAULT 0, board_rows_seen INTEGER DEFAULT -1,
		board_open_seen INTEGER DEFAULT -1, cooldown_s INTEGER DEFAULT 0)`); err != nil {
		t.Fatal(err)
	}
	// The measured lane's shape: tasks admission, adaptive cooldown OFF,
	// 21600s cooldown/floor, ceiling 8x floor.
	if _, err := db.Exec(`INSERT INTO projects (name, admission_mode, adaptive_cooldown,
		cooldown_floor_s, cooldown_ceiling_s, no_progress_threshold, cooldown_s)
		VALUES ('spin', 'tasks', 0, 21600, 172800, 10, 21600)`); err != nil {
		t.Fatal(err)
	}

	streak := func() int {
		t.Helper()
		var s int
		if err := db.QueryRow(`SELECT no_progress_ticks FROM projects WHERE name='spin'`).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	// A workdir with no board and no .git: board signals are absent, the
	// commit window is measured-empty (claimed == 0), so every tick below is a
	// genuine no-artifact tick.
	wd := t.TempDir()

	// Three clean-but-empty ticks (dry_run: status completed, zero artifacts).
	for i := 1; i <= 3; i++ {
		adaptiveCooldown(db, "spin", wd, TickOutcome{Project: "spin", Status: TickCompleted, Commits: 0})
		if got := streak(); got != i {
			t.Fatalf("no-op tick %d: want no_progress_ticks=%d, got %d — a dry_run on a tasks lane at adaptive_cooldown=0 must extend the streak", i, i, got)
		}
	}

	// A DEFERRED tick is a harness-side gateway blip (SCHED-GAP-203): it must
	// not charge the lane an extra no-op.
	adaptiveCooldown(db, "spin", wd, TickOutcome{Project: "spin", Status: TickDeferred})
	if got := streak(); got != 3 {
		t.Fatalf("deferred tick must not extend the streak: want 3, got %d", got)
	}

	// A tick that landed code (measured) resets the streak: the pause is
	// re-earned from scratch every time the lane actually produces work.
	// No .git in wd means the classification is unmeasurable, which must fall
	// OPEN — a claimed commit is progress, never a 6h charge.
	adaptiveCooldown(db, "spin", wd, TickOutcome{Project: "spin", Status: TickCompleted, Commits: 2, FilesChanged: 3})
	if got := streak(); got != 0 {
		t.Fatalf("a commit-producing tick must reset the streak to 0, got %d", got)
	}
}
