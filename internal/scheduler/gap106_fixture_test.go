package scheduler

import (
	"os"
	"path/filepath"
	"testing"
)

// SCHED-GAP-106: perpetual fixture rows (NEVER-DONE family / perpetual:true)
// must be invisible to adaptive speed control in BOTH directions — they never
// count as open work (boardOpenRows) and never count as pending work
// (CountPending / countPendingBoard). A finished project whose only open row
// is the fixture must be treated as idle.
func TestBoardOpenRowsExcludesPerpetualFixtures_GAP106(t *testing.T) {
	dir := t.TempDir()
	boardDir := filepath.Join(dir, ".coding-hermes", "board")
	if err := os.MkdirAll(boardDir, 0o755); err != nil {
		t.Fatal(err)
	}
	board := filepath.Join(boardDir, "tasks.jsonl")
	content := "" +
		`{"id":"NEVER-DONE","title":"perpetual audit","status":"pending","perpetual":true}` + "\n" +
		`{"id":"F-1","title":"real work open","status":"pending"}` + "\n" +
		`{"id":"F-2","title":"real work done","status":"completed"}` + "\n"
	if err := os.WriteFile(board, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	got, ok := boardOpenRows(dir)
	if !ok {
		t.Fatal("board exists but boardOpenRows reported absent")
	}
	if got != 1 {
		t.Fatalf("boardOpenRows = %d, want 1 (fixture excluded, F-2 done)", got)
	}

	// Legacy id-family detection WITHOUT the flag: still a fixture.
	content2 := "" +
		`{"id":"never-done-2","title":"variant id","status":"pending"}` + "\n" +
		`{"id":"F-1","title":"real work open","status":"pending"}` + "\n"
	if err := os.WriteFile(board, []byte(content2), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, _ := boardOpenRows(dir); got != 1 {
		t.Fatalf("boardOpenRows = %d, want 1 (never-done-2 excluded by id prefix)", got)
	}
}

func TestCountPendingBoardExcludesPerpetualFixtures_GAP106(t *testing.T) {
	dir := t.TempDir()
	boardDir := filepath.Join(dir, ".coding-hermes", "board")
	if err := os.MkdirAll(boardDir, 0o755); err != nil {
		t.Fatal(err)
	}
	board := filepath.Join(boardDir, "tasks.jsonl")
	content := "" +
		`{"id":"NEVER-DONE","title":"perpetual audit","status":"pending"}` + "\n" +
		`{"id":"W-1","title":"real pending","status":"pending"}` + "\n"
	if err := os.WriteFile(board, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(board)
	if err != nil {
		t.Fatal(err)
	}
	if got := countPendingBoard(board, fi); got != 1 {
		t.Fatalf("countPendingBoard = %d, want 1 (fixture never pending work)", got)
	}
}
