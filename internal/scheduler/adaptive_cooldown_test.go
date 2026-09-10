package scheduler

import (
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

// writeBoardStatus writes a tasks.jsonl with rows in the given statuses
// ("open"/"done" shorthand: open→pending, done→complete).
func writeBoardStatus(t *testing.T, workdir string, open, done int) {
	t.Helper()
	dir := filepath.Join(workdir, ".coding-hermes", "board")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir board: %v", err)
	}
	var sb strings.Builder
	for i := 0; i < open; i++ {
		fmt.Fprintf(&sb, `{"id": "O-%03d", "title": "open %d", "status": "pending"}%s`, i, i, "\n")
	}
	for i := 0; i < done; i++ {
		fmt.Fprintf(&sb, `{"id": "D-%03d", "title": "done %d", "status": "complete"}%s`, i, i, "\n")
	}
	if err := os.WriteFile(filepath.Join(dir, "tasks.jsonl"), []byte(sb.String()), 0o644); err != nil {
		t.Fatalf("write board: %v", err)
	}
}

func noProgressOutcome(name string) TickOutcome {
	return TickOutcome{Project: name, Status: TickCompleted, Commits: 0}
}

// initTickRepo turns workdir into a minimal git repo with one code file and
// a board file, committing a baseline so later classifications have a diff
// surface. Returns the git binary invocation prefix for tests.
func initTickRepo(t *testing.T, workdir string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(workdir, ".coding-hermes", "board"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	for _, args := range [][]string{
		{"init"},
		{"config", "user.email", "t@example.com"},
		{"config", "user.name", "t"},
		{"add", "-A"},
		{"commit", "-m", "baseline", "--allow-empty"},
	} {
		cmd := exec.Command("git", append([]string{"-C", workdir}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
}

// gitCommitFiles stages and commits the given repo-relative paths.
func gitCommitFiles(t *testing.T, workdir string, files []string, msg string) time.Time {
	t.Helper()
	args := make([]string, 0, 3+len(files))
	args = append(args, "-C", workdir, "add")
	args = append(args, files...)
	cmd := exec.Command("git", args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git add: %v\n%s", err, out)
	}
	cmd = exec.Command("git", "-C", workdir, "commit", "-m", msg, "--allow-empty")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git commit: %v\n%s", err, out)
	}
	return time.Now().Add(-time.Minute)
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

// TestAdaptiveCooldown_BoardInjectionIsNotProgress pins the SCHED-GAP-105
// semantics: sibling crons (qa-cron, error-scanner) injecting new rows onto
// the board must NOT reset the no-progress streak — injection is input, not
// output. Only the foreman closing rows (open count down) counts.
func TestAdaptiveCooldown_BoardInjectionIsNotProgress(t *testing.T) {
	db := slowdownTestDB(t)
	workdir := t.TempDir()
	writeBoardStatus(t, workdir, 3, 0)
	insertAdaptiveProject(t, db, "board-inject", struct {
		cooldownS int
		floorS    int
		ceilingS  int
		threshold int
		streak    int
		rowsSeen  int
	}{cooldownS: 600, floorS: 600, ceilingS: 604800, threshold: 1, rowsSeen: 3})

	// Escalate to 1200 (no-progress tick, open rows unchanged at 3).
	adaptiveCooldown(db, "board-inject", workdir, noProgressOutcome("board-inject"))
	cd, _, _, _, streak, rowsSeen := readAdaptiveState(t, db, "board-inject")
	if cd != 1200 || streak != 1 || rowsSeen != 3 {
		t.Fatalf("precondition: cooldown=%d streak=%d rowsSeen=%d, want 1200/1/3", cd, streak, rowsSeen)
	}

	// A sibling cron injects two new open rows between ticks (3→5 total).
	writeBoardStatus(t, workdir, 5, 0)

	// The next tick commits nothing and the board grew — NO reset.
	adaptiveCooldown(db, "board-inject", workdir, noProgressOutcome("board-inject"))
	cd, _, _, _, streak, rowsSeen = readAdaptiveState(t, db, "board-inject")
	if cd != 2400 {
		t.Errorf("cooldown = %d, want 2400 (injection is NOT progress — streak escalates)", cd)
	}
	if streak != 2 {
		t.Errorf("no_progress_ticks = %d, want 2", streak)
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

// =============================================================================
// SCHED-GAP-104: commit-anatomy signals. Board-bookkeeping commits must NOT
// count as progress; only code commits (paths outside .coding-hermes/) do.
// =============================================================================

func TestAdaptiveCooldown_BoardOnlyCommitIsNotProgress(t *testing.T) {
	db := slowdownTestDB(t)
	workdir := t.TempDir()
	initTickRepo(t, workdir)
	insertAdaptiveProject(t, db, "self-commit-proj", struct {
		cooldownS, floorS, ceilingS, threshold, streak, rowsSeen int
	}{cooldownS: 3600, floorS: 3600, ceilingS: 28800, threshold: 3, streak: 2, rowsSeen: 1})
	db.Exec(`INSERT INTO ticks (id, project_name, status, created_at) VALUES ('TICK-BO-1','self-commit-proj','running',datetime('now'))`)
	writeBoard(t, workdir, 1) // board file exists with 1 row (baseline seen)

	// Tick commits ONLY board bookkeeping.
	gitCommitFiles(t, workdir, []string{".coding-hermes/board/tasks.jsonl"}, "chore: board tick")
	outcome := noProgressOutcome("self-commit-proj")
	outcome.Commits = 1
	outcome.TickID = "TICK-BO-1"
	outcome.Started = time.Now().Add(-2 * time.Minute)
	if !adaptiveCooldown(db, "self-commit-proj", workdir, outcome) {
		t.Fatal("adaptiveCooldown returned false for an armed project")
	}
	_, _, _, _, streak, _ := readAdaptiveState(t, db, "self-commit-proj")
	if streak != 3 {
		t.Errorf("board-only commit counted as progress: streak=%d, want 3", streak)
	}
	// The split must be persisted for observability.
	var code, board int
	if err := db.QueryRow(`SELECT code_commits, board_commits FROM ticks WHERE id='TICK-BO-1'`).
		Scan(&code, &board); err != nil {
		t.Fatalf("read tick signals: %v", err)
	}
	if code != 0 || board != 1 {
		t.Errorf("persisted split = (%d, %d), want (0, 1)", code, board)
	}
}

func TestAdaptiveCooldown_CodeCommitStillResets(t *testing.T) {
	db := slowdownTestDB(t)
	workdir := t.TempDir()
	initTickRepo(t, workdir)
	insertAdaptiveProject(t, db, "code-proj", struct {
		cooldownS, floorS, ceilingS, threshold, streak, rowsSeen int
	}{cooldownS: 7200, floorS: 3600, ceilingS: 57600, threshold: 3, streak: 4, rowsSeen: 1})
	db.Exec(`INSERT INTO ticks (id, project_name, status, created_at) VALUES ('TICK-CODE-1','code-proj','running',datetime('now'))`)
	writeBoard(t, workdir, 1)

	// A mixed commit: one code file + one board file.
	if err := os.WriteFile(filepath.Join(workdir, "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCommitFiles(t, workdir, []string{"main.go", ".coding-hermes/board/tasks.jsonl"}, "feat: real work + bookkeep")
	outcome := noProgressOutcome("code-proj")
	outcome.Commits = 1
	outcome.TickID = "TICK-CODE-1"
	outcome.Started = time.Now().Add(-2 * time.Minute)
	adaptiveCooldown(db, "code-proj", workdir, outcome)
	cd, _, _, _, streak, _ := readAdaptiveState(t, db, "code-proj")
	if streak != 0 {
		t.Errorf("code commit did not reset streak: streak=%d", streak)
	}
	if cd != 3600 {
		t.Errorf("cooldown not dropped to floor: %d, want 3600", cd)
	}
	var code, board int
	db.QueryRow(`SELECT code_commits, board_commits FROM ticks WHERE id='TICK-CODE-1'`).Scan(&code, &board)
	if code != 1 || board != 0 {
		t.Errorf("persisted split = (%d, %d), want (1, 0)", code, board)
	}
}

func TestClassifyGitCommits_Fallbacks(t *testing.T) {
	t.Run("no repo falls open (ok=false)", func(t *testing.T) {
		code, board, ok := classifyGitCommits(t.TempDir(), time.Now().Add(-time.Hour), 2)
		if ok {
			t.Errorf("expected failed measurement, got code=%d board=%d ok=%v", code, board, ok)
		}
	})
	t.Run("zero claimed is a valid empty measurement", func(t *testing.T) {
		code, board, ok := classifyGitCommits(t.TempDir(), time.Now().Add(-time.Hour), 0)
		if !ok || code != 0 || board != 0 {
			t.Errorf("claimed=0 should be (0,0,true), got (%d, %d, %v)", code, board, ok)
		}
	})
	t.Run("non-repo with claimed commits → legacy fallback", func(t *testing.T) {
		db := slowdownTestDB(t)
		insertAdaptiveProject(t, db, "norepo", struct {
			cooldownS, floorS, ceilingS, threshold, streak, rowsSeen int
		}{cooldownS: 600, floorS: 600, ceilingS: 4800, threshold: 3, streak: 0, rowsSeen: 0})
		outcome := noProgressOutcome("norepo")
		outcome.Commits = 1 // claims work but workdir has no git
		outcome.Started = time.Now().Add(-time.Minute)
		adaptiveCooldown(db, "norepo", t.TempDir(), outcome)
		_, _, _, _, streak, _ := readAdaptiveState(t, db, "norepo")
		if streak != 0 {
			t.Errorf("unmeasurable commits were treated as no-progress: streak=%d, want 0", streak)
		}
	})
}

// =============================================================================
// SCHED-GAP-105: open-row completion signal. The board proves output when the
// foreman CLOSES rows (open count decreases), not when rows are added.
// =============================================================================

func TestAdaptiveCooldown_BoardCompletionResets(t *testing.T) {
	db := slowdownTestDB(t)
	workdir := t.TempDir()
	writeBoardStatus(t, workdir, 5, 2)
	insertAdaptiveProject(t, db, "closer", struct {
		cooldownS int
		floorS    int
		ceilingS  int
		threshold int
		streak    int
		rowsSeen  int
	}{cooldownS: 1200, floorS: 600, ceilingS: 604800, threshold: 3, streak: 3, rowsSeen: 7})

	// Establish the open baseline (first observation must not fire progress).
	adaptiveCooldown(db, "closer", workdir, noProgressOutcome("closer"))

	// Foreman closes 2 of 5 open rows (no code commits).
	writeBoardStatus(t, workdir, 3, 4)
	adaptiveCooldown(db, "closer", workdir, noProgressOutcome("closer"))

	cd, _, _, _, streak, _ := readAdaptiveState(t, db, "closer")
	if streak != 0 {
		t.Errorf("streak = %d, want 0 (net open-row decrease is progress)", streak)
	}
	if cd != 600 {
		t.Errorf("cooldown = %d, want 600 (dropped to floor)", cd)
	}
}

func TestAdaptiveCooldown_BoardCompletionBaselineNotFired(t *testing.T) {
	db := slowdownTestDB(t)
	workdir := t.TempDir()
	writeBoardStatus(t, workdir, 5, 0)
	insertAdaptiveProject(t, db, "baseline-open", struct {
		cooldownS int
		floorS    int
		ceilingS  int
		threshold int
		streak    int
		rowsSeen  int
	}{cooldownS: 600, floorS: 600, ceilingS: 604800, threshold: 3, streak: 0, rowsSeen: 5})

	// First-ever observation: open baseline unseen (-1) — must NOT count the
	// mere existence of open rows as progress.
	adaptiveCooldown(db, "baseline-open", workdir, noProgressOutcome("baseline-open"))
	_, _, _, _, streak, _ := readAdaptiveState(t, db, "baseline-open")
	if streak != 1 {
		t.Errorf("streak = %d, want 1 (first observation establishes baseline only)", streak)
	}
}

func TestBoardOpenRows(t *testing.T) {
	t.Run("counts by status vocabulary", func(t *testing.T) {
		dir := t.TempDir()
		writeBoardStatus(t, dir, 2, 1) // 2 pending + 1 complete
		n, ok := boardOpenRows(dir)
		if !ok || n != 2 {
			t.Errorf("boardOpenRows = (%d, %v), want (2, true)", n, ok)
		}
	})
	t.Run("missing board", func(t *testing.T) {
		n, ok := boardOpenRows(t.TempDir())
		if ok || n != 0 {
			t.Errorf("boardOpenRows(empty) = (%d, %v), want (0, false)", n, ok)
		}
	})
	t.Run("malformed row counts as open", func(t *testing.T) {
		dir := t.TempDir()
		d := filepath.Join(dir, ".coding-hermes", "board")
		os.MkdirAll(d, 0o755)
		os.WriteFile(filepath.Join(d, "tasks.jsonl"),
			[]byte(`{"id":"X","title":"broken"`+"\n"+`{"id":"Y","title":"ok","status":"complete"}`+"\n"), 0o644)
		n, ok := boardOpenRows(dir)
		if !ok || n != 1 {
			t.Errorf("boardOpenRows = (%d, %v), want (1, true) — broken row stays visible", n, ok)
		}
	})
}
