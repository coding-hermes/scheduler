package scheduler

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/coding-hermes/scheduler/internal/database"
)

// insertAdaptiveProject inserts a project row with explicit adaptive-cooldown
// policy columns so tests can drive the full no-progress / reset state
// machine without touching the update-layer normalization.
func insertAdaptiveProject(t *testing.T, db *sql.DB, name string, cfg struct {
	cooldownS int
	floorS    int
	ceilingS  int
	threshold int
	streak    int
	rowsSeen  int
}) {
	t.Helper()
	_, err := db.Exec(`INSERT INTO projects
		(name, repo_url, workdir, weight, priority, cooldown_s, decay_rate,
		 model, provider, enabled, created_at, updated_at,
		 adaptive_cooldown, cooldown_floor_s, cooldown_ceiling_s,
		 no_progress_threshold, no_progress_ticks, board_rows_seen)
		VALUES (?, ?, ?, 10, 5, ?, 1.0, 'deepseek-v4-pro', 'deepseek-foreman', 1,
		        datetime('now'), datetime('now'), 1, ?, ?, ?, ?, ?)`,
		name, "https://github.com/example/"+name, "/tmp/work/"+name, cfg.cooldownS,
		cfg.floorS, cfg.ceilingS, cfg.threshold, cfg.streak, cfg.rowsSeen,
	)
	if err != nil {
		t.Fatalf("insert adaptive project %s: %v", name, err)
	}
}

// readAdaptiveState reads the adaptive-relevant columns for one project.
func readAdaptiveState(t *testing.T, db *sql.DB, name string) (cooldown, floor, ceiling, threshold, streak, rowsSeen int) {
	t.Helper()
	err := db.QueryRow(`SELECT cooldown_s, cooldown_floor_s, cooldown_ceiling_s,
	       no_progress_threshold, no_progress_ticks, board_rows_seen
	FROM projects WHERE name = ?`, name).
		Scan(&cooldown, &floor, &ceiling, &threshold, &streak, &rowsSeen)
	if err != nil {
		t.Fatalf("read adaptive state for %s: %v", name, err)
	}
	return
}

// writeBoard writes a tasks.jsonl with the given number of dummy rows.
func writeBoard(t *testing.T, workdir string, rows int) {
	t.Helper()
	dir := filepath.Join(workdir, ".coding-hermes", "board")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir board: %v", err)
	}
	var sb strings.Builder
	for i := 0; i < rows; i++ {
		fmt.Fprintf(&sb, `{"id": "T-%03d", "title": "task %d", "status": "pending"}%s`,
			i, i, "\n")
	}
	if err := os.WriteFile(filepath.Join(dir, "tasks.jsonl"), []byte(sb.String()), 0o644); err != nil {
		t.Fatalf("write board: %v", err)
	}
}

func noProgressOutcome(name string) TickOutcome {
	return TickOutcome{Project: name, Status: TickCompleted, Commits: 0}
}

// =============================================================================
// Feature is opt-in: adaptive_cooldown = 0 must leave everything untouched and
// return false so the caller falls back to the legacy autoSlowdown path.
// =============================================================================

func TestAdaptiveCooldown_FeatureOffByDefault(t *testing.T) {
	db := slowdownTestDB(t)
	insertSlowdownProject(t, db, "legacy-proj", 600)

	handled := adaptiveCooldown(db, "legacy-proj", "", noProgressOutcome("legacy-proj"))

	if handled {
		t.Error("adaptiveCooldown returned true for a project with adaptive_cooldown = 0 (must fall through to autoSlowdown)")
	}
	cd, _, _, _, streak, _ := readAdaptiveState(t, db, "legacy-proj")
	if cd != 600 || streak != 0 {
		t.Errorf("state changed with feature off: cooldown=%d streak=%d, want 600/0", cd, streak)
	}
}

// =============================================================================
// Progression math: no-progress ticks extend the streak; once it reaches the
// threshold cooldown_s doubles per tick, capped at the ceiling.
// =============================================================================

func TestAdaptiveCooldown_NoProgressProgression(t *testing.T) {
	tests := []struct {
		name       string
		threshold  int // 0 exercises the built-in default (10)
		ceilingS   int // 0 exercises the built-in default (604800)
		ticks      int // number of consecutive no-progress ticks to run
		wantCD     int
		wantStreak int
	}{
		{
			name: "below threshold — cooldown unchanged, streak counts",
			// floor 600, threshold 3: ticks 1-2 stay at 600.
			threshold: 3, ticks: 2, wantCD: 600, wantStreak: 2,
		},
		{
			name: "threshold tick doubles cooldown",
			// tick 3 (== threshold): 600 -> 1200.
			threshold: 3, ticks: 3, wantCD: 1200, wantStreak: 3,
		},
		{
			name: "progressive doubling per no-progress tick past threshold",
			// ticks 4,5: 1200 -> 2400 -> 4800.
			threshold: 3, ticks: 5, wantCD: 4800, wantStreak: 5,
		},
		{
			name: "built-in default threshold (10) when column is 0",
			// 10th tick doubles 600 -> 1200.
			threshold: 0, ticks: 10, wantCD: 1200, wantStreak: 10,
		},
		{
			name:      "default threshold — 9 no-progress ticks is still below it",
			threshold: 0, ticks: 9, wantCD: 600, wantStreak: 9,
		},
		{
			name: "explicit ceiling caps escalation",
			// threshold 1, ceiling 1000: 600*2=1200 -> capped at 1000.
			threshold: 1, ceilingS: 1000, ticks: 1, wantCD: 1000, wantStreak: 1,
		},
		{
			name: "at-ceiling ticks stay at ceiling",
			// Second no-progress tick with cooldown already == ceiling: no growth.
			threshold: 1, ceilingS: 1000, ticks: 2, wantCD: 1000, wantStreak: 2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db := slowdownTestDB(t)
			name := strings.ReplaceAll(tt.name, " ", "_")
			insertAdaptiveProject(t, db, name, struct {
				cooldownS int
				floorS    int
				ceilingS  int
				threshold int
				streak    int
				rowsSeen  int
			}{cooldownS: 600, floorS: 600, ceilingS: tt.ceilingS, threshold: tt.threshold})

			for i := 0; i < tt.ticks; i++ {
				if !adaptiveCooldown(db, name, "", noProgressOutcome(name)) {
					t.Fatalf("tick %d: adaptiveCooldown returned false (feature should be on)", i+1)
				}
			}

			cd, _, _, _, streak, _ := readAdaptiveState(t, db, name)
			if cd != tt.wantCD {
				t.Errorf("cooldown = %d, want %d", cd, tt.wantCD)
			}
			if streak != tt.wantStreak {
				t.Errorf("no_progress_ticks = %d, want %d", streak, tt.wantStreak)
			}
		})
	}
}

// TestAdaptiveCooldown_EscalationToWeeklyCeiling drives the default policy from
// a 600s floor all the way to the 604800s (weekly) ceiling to pin the full
// progression chain end-to-end.
func TestAdaptiveCooldown_EscalationToWeeklyCeiling(t *testing.T) {
	db := slowdownTestDB(t)
	insertAdaptiveProject(t, db, "to-weekly", struct {
		cooldownS int
		floorS    int
		ceilingS  int
		threshold int
		streak    int
		rowsSeen  int
	}{cooldownS: 600, floorS: 600, ceilingS: 0, threshold: 1}) // threshold 1 = escalate on first no-progress tick

	want := []int{1200, 2400, 4800, 9600, 19200, 38400, 76800, 153600, 307200, 604800, 604800}
	for i, w := range want {
		if !adaptiveCooldown(db, "to-weekly", "", noProgressOutcome("to-weekly")) {
			t.Fatalf("tick %d: adaptiveCooldown returned false", i+1)
		}
		cd, _, _, _, _, _ := readAdaptiveState(t, db, "to-weekly")
		if cd != w {
			t.Fatalf("tick %d: cooldown = %d, want %d (600 * 2^%d capped at 604800)", i+1, cd, w, i+1)
		}
	}
}

// =============================================================================
// Reset (speed-up) paths.
// =============================================================================

// TestAdaptiveCooldown_CommitResets verifies the non-zero-commit tick reset:
// an escalated cooldown drops straight back to the floor and the streak is 0.
func TestAdaptiveCooldown_CommitResets(t *testing.T) {
	db := slowdownTestDB(t)
	insertAdaptiveProject(t, db, "commit-reset", struct {
		cooldownS int
		floorS    int
		ceilingS  int
		threshold int
		streak    int
		rowsSeen  int
	}{cooldownS: 600, floorS: 600, ceilingS: 604800, threshold: 1})

	// Escalate to 1200.
	adaptiveCooldown(db, "commit-reset", "", noProgressOutcome("commit-reset"))
	cd, _, _, _, streak, _ := readAdaptiveState(t, db, "commit-reset")
	if cd != 1200 || streak != 1 {
		t.Fatalf("precondition: cooldown=%d streak=%d, want 1200/1", cd, streak)
	}

	// A productive tick resets immediately.
	handled := adaptiveCooldown(db, "commit-reset", "", TickOutcome{Project: "commit-reset", Status: TickCompleted, Commits: 3})
	if !handled {
		t.Fatal("adaptiveCooldown returned false")
	}
	cd, _, _, _, streak, _ = readAdaptiveState(t, db, "commit-reset")
	if cd != 600 {
		t.Errorf("cooldown = %d, want 600 (reset to floor after productive tick)", cd)
	}
	if streak != 0 {
		t.Errorf("no_progress_ticks = %d, want 0", streak)
	}
}

// TestAdaptiveCooldown_BoardRowReset verifies the speed-up path that matters
// for the fleet: a NEW board row (e.g. a UPD-* wave injected between ticks)
// resets an escalated project even when the tick itself committed nothing.
func TestAdaptiveCooldown_BoardRowReset(t *testing.T) {
	db := slowdownTestDB(t)
	workdir := t.TempDir()
	writeBoard(t, workdir, 3)
	insertAdaptiveProject(t, db, "board-reset", struct {
		cooldownS int
		floorS    int
		ceilingS  int
		threshold int
		streak    int
		rowsSeen  int
	}{cooldownS: 600, floorS: 600, ceilingS: 604800, threshold: 1, rowsSeen: 3})

	// Escalate to 1200 (no-progress tick, board unchanged at 3 rows).
	adaptiveCooldown(db, "board-reset", workdir, noProgressOutcome("board-reset"))
	cd, _, _, _, streak, rowsSeen := readAdaptiveState(t, db, "board-reset")
	if cd != 1200 || streak != 1 || rowsSeen != 3 {
		t.Fatalf("precondition: cooldown=%d streak=%d rowsSeen=%d, want 1200/1/3", cd, streak, rowsSeen)
	}

	// A UPD-* board task injects two new rows between ticks.
	writeBoard(t, workdir, 5)

	// The next tick commits nothing but the board grew — reset to the floor.
	handled := adaptiveCooldown(db, "board-reset", workdir, noProgressOutcome("board-reset"))
	if !handled {
		t.Fatal("adaptiveCooldown returned false")
	}
	cd, _, _, _, streak, rowsSeen = readAdaptiveState(t, db, "board-reset")
	if cd != 600 {
		t.Errorf("cooldown = %d, want 600 (reset to floor on new board rows)", cd)
	}
	if streak != 0 {
		t.Errorf("no_progress_ticks = %d, want 0", streak)
	}
	if rowsSeen != 5 {
		t.Errorf("board_rows_seen = %d, want 5 (baseline advanced)", rowsSeen)
	}
}

// TestAdaptiveCooldown_NoFalseProgressOnInPlaceEdits pins the row-count design:
// the foreman rewriting tasks.jsonl in place (status flips) must NOT read as
// new work — only a net row-count increase does.
func TestAdaptiveCooldown_NoFalseProgressOnInPlaceEdits(t *testing.T) {
	db := slowdownTestDB(t)
	workdir := t.TempDir()
	writeBoard(t, workdir, 3)
	insertAdaptiveProject(t, db, "inplace-edit", struct {
		cooldownS int
		floorS    int
		ceilingS  int
		threshold int
		streak    int
		rowsSeen  int
	}{cooldownS: 600, floorS: 600, ceilingS: 604800, threshold: 1, rowsSeen: 3})

	// Simulate the foreman marking a row done (same 3 rows, mtime changes).
	writeBoard(t, workdir, 3)

	if !adaptiveCooldown(db, "inplace-edit", workdir, noProgressOutcome("inplace-edit")) {
		t.Fatal("adaptiveCooldown returned false")
	}
	cd, _, _, _, streak, _ := readAdaptiveState(t, db, "inplace-edit")
	if cd != 1200 {
		t.Errorf("cooldown = %d, want 1200 (in-place rewrite is NOT progress — no reset)", cd)
	}
	if streak != 1 {
		t.Errorf("no_progress_ticks = %d, want 1 (still a no-progress tick)", streak)
	}
}

// TestAdaptiveCooldown_FirstObservationEstablishesBaseline verifies the first
// adaptive tick on a board-heavy project records the baseline instead of
// reading the pre-existing rows as "new work since the previous tick".
func TestAdaptiveCooldown_FirstObservationEstablishesBaseline(t *testing.T) {
	db := slowdownTestDB(t)
	workdir := t.TempDir()
	writeBoard(t, workdir, 29)
	insertAdaptiveProject(t, db, "baseline-first", struct {
		cooldownS int
		floorS    int
		ceilingS  int
		threshold int
		streak    int
		rowsSeen  int
	}{cooldownS: 600, floorS: 600, ceilingS: 604800, threshold: 1, rowsSeen: database.AdaptiveUnseenBoardRows})

	if !adaptiveCooldown(db, "baseline-first", workdir, noProgressOutcome("baseline-first")) {
		t.Fatal("adaptiveCooldown returned false")
	}
	cd, _, _, _, streak, rowsSeen := readAdaptiveState(t, db, "baseline-first")
	if cd != 1200 {
		t.Errorf("cooldown = %d, want 1200 (no baseline yet ⇒ no board progress signal)", cd)
	}
	if streak != 1 {
		t.Errorf("no_progress_ticks = %d, want 1", streak)
	}
	if rowsSeen != 29 {
		t.Errorf("board_rows_seen = %d, want 29 (baseline established)", rowsSeen)
	}
}

// =============================================================================
// Board row counter.
// =============================================================================

func TestCountBoardRows(t *testing.T) {
	t.Run("no board file", func(t *testing.T) {
		n, ok := countBoardRows(t.TempDir())
		if ok || n != 0 {
			t.Errorf("countBoardRows(empty dir) = (%d, %v), want (0, false)", n, ok)
		}
	})

	t.Run("jsonl rows counted", func(t *testing.T) {
		workdir := t.TempDir()
		writeBoard(t, workdir, 7)
		n, ok := countBoardRows(workdir)
		if !ok || n != 7 {
			t.Errorf("countBoardRows = (%d, %v), want (7, true)", n, ok)
		}
	})

	t.Run("malformed lines still count as rows", func(t *testing.T) {
		workdir := t.TempDir()
		dir := filepath.Join(workdir, ".coding-hermes", "board")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		content := "{\"id\": \"a\", \"status\": \"pending\"}\nthis is not json but is a row\n\n{\"id\": \"b\"}\n"
		if err := os.WriteFile(filepath.Join(dir, "tasks.jsonl"), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		n, ok := countBoardRows(workdir)
		if !ok || n != 3 {
			t.Errorf("countBoardRows = (%d, %v), want (3, true) — every non-empty line is a row", n, ok)
		}
	})
}
