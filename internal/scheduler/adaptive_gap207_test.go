package scheduler

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// SCHED-GAP-207: satellite lanes reuse a row id as a recurring SLOT and never
// close the previous row — 29% of open rows fleet-wide collided on id. The
// open-work signal must count UNIQUE open ids (the number a human calls
// "open findings"), so a re-file under the same id is churn, not new work,
// and the no-progress direction rule (SCHED-GAP-105) stops rewarding it.

func writeGap207Board(t *testing.T, rows []map[string]interface{}) string {
	t.Helper()
	dir := t.TempDir()
	boardDir := filepath.Join(dir, ".coding-hermes", "board")
	if err := os.MkdirAll(boardDir, 0o755); err != nil {
		t.Fatal(err)
	}
	var content []byte
	for _, r := range rows {
		line, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		content = append(content, line...)
		content = append(content, '\n')
	}
	if err := os.WriteFile(filepath.Join(boardDir, "tasks.jsonl"), content, 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// RED/GREEN core: the same id holding N open rows is ONE finding, not N.
// On the pre-fix code the equivalent assertion (unique == raw) fails — the
// raw counter reported every slot copy as work.
func TestBoardOpenUniqueIDsCollapsesIdSlots_GAP207(t *testing.T) {
	dir := writeGap207Board(t, []map[string]interface{}{
		{"id": "QA-CONSENSUS-1", "title": "cycle 1", "status": "pending"},
		{"id": "QA-CONSENSUS-1", "title": "cycle 2", "status": "pending"},
		{"id": "QA-CONSENSUS-1", "title": "cycle 3", "status": "pending"},
		{"id": "F-9", "title": "distinct real work", "status": "pending"},
	})

	unique, ok := boardOpenUniqueIDs(dir)
	if !ok {
		t.Fatal("board exists but boardOpenUniqueIDs reported absent")
	}
	if unique != 2 {
		t.Fatalf("boardOpenUniqueIDs = %d, want 2 (3 copies of one id = 1 finding + 1 distinct)", unique)
	}
	// The raw counter deliberately still reports rows (dispatchability
	// semantics unchanged) — pin the split so the two numbers stay distinct.
	if raw, _ := boardOpenRows(dir); raw != 4 {
		t.Fatalf("boardOpenRows = %d, want 4 (raw count is row-lines, unchanged)", raw)
	}
}

// The closure protocol expressed in the metric: closing the previous row and
// re-filing under a NEW id keeps the unique count flat (2→2), while closing
// without re-filing drops it. Re-filing WITHOUT closing leaves it equal —
// the "no progress" incentive.
func TestBoardOpenUniqueIDSDirectionTracksClosure_GAP207(t *testing.T) {
	rows := make([]map[string]interface{}, 0, 3)
	rows = append(rows,
		map[string]interface{}{"id": "S-1", "title": "finding one", "status": "pending"},
		map[string]interface{}{"id": "S-2", "title": "finding two", "status": "pending"},
	)
	dir := writeGap207Board(t, rows)
	before, _ := boardOpenUniqueIDs(dir)
	if before != 2 {
		t.Fatalf("baseline = %d, want 2", before)
	}

	// Slot churn: a sibling copies S-2's condition under S-2 again.
	rows = append(rows, map[string]interface{}{"id": "S-2", "title": "finding two (re-check)", "status": "pending"})
	dir = writeGap207Board(t, rows)
	afterChurn, _ := boardOpenUniqueIDs(dir)
	if afterChurn != 2 {
		t.Fatalf("after id-slot refile = %d, want 2 (a re-file is not new work)", afterChurn)
	}

	// Closure: S-1 completes → unique count drops to 1.
	rows[0]["status"] = "complete"
	dir = writeGap207Board(t, rows)
	afterClose, _ := boardOpenUniqueIDs(dir)
	if afterClose != 1 {
		t.Fatalf("after closure = %d, want 1", afterClose)
	}
}

// Contract parity with boardOpenRows: missing board → (0, false); fixture
// exclusion identical; malformed rows still count (never hidden).
func TestBoardOpenUniqueIDsContractParity_GAP207(t *testing.T) {
	if n, ok := boardOpenUniqueIDs(t.TempDir()); ok || n != 0 {
		t.Fatalf("boardOpenUniqueIDs(no board) = (%d, %v), want (0, false)", n, ok)
	}

	dir := writeGap207Board(t, []map[string]interface{}{
		{"id": "NEVER-DONE", "title": "perpetual audit", "status": "pending", "perpetual": true},
		{"id": "F-1", "title": "real open", "status": "pending"},
	})
	if n, _ := boardOpenUniqueIDs(dir); n != 1 {
		t.Fatalf("fixture exclusion: boardOpenUniqueIDs = %d, want 1", n)
	}

	// Malformed line: open work whose id is unreadable → one synthetic id.
	boardDir := filepath.Join(dir, ".coding-hermes", "board")
	board := filepath.Join(boardDir, "tasks.jsonl")
	content, _ := os.ReadFile(board)
	if err := os.WriteFile(board, append(content, []byte("{not json at all\n")...), 0o644); err != nil {
		t.Fatal(err)
	}
	if n, _ := boardOpenUniqueIDs(dir); n != 2 {
		t.Fatalf("malformed row: boardOpenUniqueIDs = %d, want 2 (F-1 + synthetic)", n)
	}
}
