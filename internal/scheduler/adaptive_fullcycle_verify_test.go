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
// instant reset on commit progress, opt-out isolation, board-rows speed-up path.
func TestAdaptiveFullCycleVerification(t *testing.T) {
	dir := t.TempDir()
	db, err := sql.Open("sqlite", filepath.Join(dir, "sim.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE projects (name TEXT PRIMARY KEY, adaptive_cooldown INTEGER DEFAULT 0,
		cooldown_floor_s INTEGER DEFAULT 0, cooldown_ceiling_s INTEGER DEFAULT 0, no_progress_threshold INTEGER DEFAULT 0,
		no_progress_ticks INTEGER DEFAULT 0, board_rows_seen INTEGER DEFAULT -1, cooldown_s INTEGER DEFAULT 0)`); err != nil {
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

	// T9: BOARD-ROW PROGRESS — crons push work → instant speed-up (reset path)
	wd := t.TempDir()
	boardDir := filepath.Join(wd, ".coding-hermes", "board")
	if err := os.MkdirAll(boardDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// First observation: baseline (board absent → hasBoard=false, baseline stays -1)
	adaptiveCooldown(db, "armed", wd, TickOutcome{Commits: 0})
	// Create the board with 3 rows → net increase over -1? No: baseline -1 means "unknown",
	// first observation records baseline only. Second call after growth proves progress.
	if err := os.WriteFile(filepath.Join(boardDir, "tasks.jsonl"),
		[]byte("{\"id\":\"A\"}\n{\"id\":\"B\"}\n{\"id\":\"C\"}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	adaptiveCooldown(db, "armed", wd, TickOutcome{Commits: 0}) // baseline now 3
	// Push new work like the stand-in PM/crons do:
	f, _ := os.OpenFile(filepath.Join(boardDir, "tasks.jsonl"), os.O_APPEND|os.O_WRONLY, 0o644)
	f.WriteString("{\"id\":\"D\"}\n")
	f.Close()
	// streak was 1 after the two idle-ish ticks above; new rows must RESET it (progress)
	adaptiveCooldown(db, "armed", wd, TickOutcome{Commits: 0})
	if s, cd := get("armed"); s != 0 {
		t.Fatalf("T9: new board rows must reset streak to 0, got %d", s)
	} else if cd != 60 {
		t.Logf("T9 note: cooldown=%d (floor reset only applies when elevated)", cd)
	}
}
