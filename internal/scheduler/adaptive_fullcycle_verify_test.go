package scheduler

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

// TestAdaptiveFullCycleVerification — Bane 2026-09-06 full-cycle dry-run proof.
// Drives the REAL adaptiveCooldown engine (not a copy): slow-down doubling to ceiling,
// instant reset on commit progress, opt-out isolation, board-completion speed-up path.
// SCHED-GAP-105 (2026-09-10): board progress = net open-row DECREASE (completions),
// not row growth — injection is input, not output.
func TestAdaptiveFullCycleVerification(t *testing.T) {
	dir := t.TempDir()
	db, err := sql.Open("sqlite", filepath.Join(dir, "sim.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE projects (name TEXT PRIMARY KEY, adaptive_cooldown INTEGER DEFAULT 0,
		cooldown_floor_s INTEGER DEFAULT 0, cooldown_ceiling_s INTEGER DEFAULT 0, no_progress_threshold INTEGER DEFAULT 0,
		no_progress_ticks INTEGER DEFAULT 0, board_rows_seen INTEGER DEFAULT -1, board_open_seen INTEGER DEFAULT -1,
		cooldown_s INTEGER DEFAULT 0)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO projects (name, adaptive_cooldown, cooldown_floor_s, cooldown_ceiling_s,
		no_progress_threshold, cooldown_s) VALUES ('armed', 1, 60, 480, 2, 60), ('control', 0, 60, 480, 2, 60)`); err != nil {
		t.Fatal(err)
	}

	get := func(name string) (streak, cd int) {
		t.Helper()
		if err := db.QueryRow(`SELECT no_progress_ticks, cooldown_s FROM projects WHERE name=?`, name).Scan(&streak, &cd); err != nil {
			t.Fatal(err)
		}
		return
	}

	// T1: idle tick below threshold → streak 1, cooldown unchanged
	adaptiveCooldown(db, "armed", t.TempDir(), TickOutcome{Commits: 0})
	if s, cd := get("armed"); s != 1 || cd != 60 {
		t.Fatalf("T1: want streak=1 cd=60, got %d/%d", s, cd)
	}
	// T2: threshold reached → doubling begins 60→120
	adaptiveCooldown(db, "armed", t.TempDir(), TickOutcome{Commits: 0})
	if s, cd := get("armed"); s != 2 || cd != 120 {
		t.Fatalf("T2: want streak=2 cd=120, got %d/%d", s, cd)
	}
	// T3–T5: continued idling → 240 → 480 (ceiling clamp)
	adaptiveCooldown(db, "armed", t.TempDir(), TickOutcome{})
	adaptiveCooldown(db, "armed", t.TempDir(), TickOutcome{})
	adaptiveCooldown(db, "armed", t.TempDir(), TickOutcome{})
	if s, cd := get("armed"); s != 5 || cd != 480 {
		t.Fatalf("T5: want streak=5 cd=480 (ceiling), got %d/%d", s, cd)
	}
	// T6: ceiling holds forever while idle
	adaptiveCooldown(db, "armed", t.TempDir(), TickOutcome{})
	if _, cd := get("armed"); cd != 480 {
		t.Fatalf("T6: ceiling violated, cd=%d", cd)
	}
	// T7: COMMIT PROGRESS → instant reset to floor (the speed-up path)
	adaptiveCooldown(db, "armed", t.TempDir(), TickOutcome{Commits: 2})
	if s, cd := get("armed"); s != 0 || cd != 60 {
		t.Fatalf("T7: want instant reset streak=0 cd=60, got %d/%d", s, cd)
	}
	// T8: opt-out isolation — adaptive=0 project is never touched
	adaptiveCooldown(db, "control", t.TempDir(), TickOutcome{})
	adaptiveCooldown(db, "control", t.TempDir(), TickOutcome{})
	adaptiveCooldown(db, "control", t.TempDir(), TickOutcome{})
	if s, cd := get("control"); s != 0 || cd != 60 {
		t.Fatalf("T8: adaptive=0 must stay untouched, got %d/%d", s, cd)
	}

	// T9: BOARD-COMPLETION PROGRESS — the foreman CLOSES rows → instant
	// speed-up (reset path). Row GROWTH (injection) must NOT reset (T10).
	wd := t.TempDir()
	boardDir := filepath.Join(wd, ".coding-hermes", "board")
	if err := os.MkdirAll(boardDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// First observation on an absent board: baseline stays -1 (no signal).
	adaptiveCooldown(db, "armed", wd, TickOutcome{Commits: 0})
	// Create the board: 3 open rows → establishes the open baseline only.
	if err := os.WriteFile(filepath.Join(boardDir, "tasks.jsonl"),
		[]byte("{\"id\":\"A\",\"status\":\"pending\"}\n{\"id\":\"B\",\"status\":\"pending\"}\n{\"id\":\"C\",\"status\":\"pending\"}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	adaptiveCooldown(db, "armed", wd, TickOutcome{Commits: 0}) // open baseline now 3
	// The foreman closes 2 of 3 open rows (marks them complete in place):
	f, _ := os.OpenFile(filepath.Join(boardDir, "tasks.jsonl"), os.O_TRUNC|os.O_WRONLY, 0o644)
	f.WriteString("{\"id\":\"A\",\"status\":\"complete\"}\n{\"id\":\"B\",\"status\":\"complete\"}\n{\"id\":\"C\",\"status\":\"pending\"}\n")
	f.Close()
	// Open went 3→1: net completions must RESET the streak (progress).
	adaptiveCooldown(db, "armed", wd, TickOutcome{Commits: 0})
	if s, cd := get("armed"); s != 0 {
		t.Fatalf("T9: board completion (open 3→1) must reset streak to 0, got %d", s)
	} else if cd != 60 {
		t.Logf("T9 note: cooldown=%d (floor reset only applies when elevated)", cd)
	}
	// T10: INJECTION IS NOT PROGRESS — a sibling cron appends a new open row;
	// the streak must build, not reset.
	f, _ = os.OpenFile(filepath.Join(boardDir, "tasks.jsonl"), os.O_APPEND|os.O_WRONLY, 0o644)
	f.WriteString("{\"id\":\"D\",\"status\":\"pending\"}\n")
	f.Close()
	adaptiveCooldown(db, "armed", wd, TickOutcome{Commits: 0})
	if s, _ := get("armed"); s != 1 {
		t.Fatalf("T10: injection (open 1→2) must NOT reset streak, want 1, got %d", s)
	}
}
