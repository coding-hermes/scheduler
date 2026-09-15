package scheduler

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// ADV-R05 (G10): fixture-exclusion classes, one test per class. The CANONICAL
// representation is the board's own fixtures.jsonl registry (sibling of
// tasks.jsonl), read by CountPending and boardOpenRows; the perpetual row
// flag and the NEVER-DONE id prefix are documented fallback layers.

func writeAdvR05Board(t *testing.T, dir, tasksContent, fixturesContent string) string {
	t.Helper()
	boardDir := filepath.Join(dir, ".coding-hermes", "board")
	if err := os.MkdirAll(boardDir, 0o755); err != nil {
		t.Fatal(err)
	}
	board := filepath.Join(boardDir, "tasks.jsonl")
	if err := os.WriteFile(board, []byte(tasksContent), 0o644); err != nil {
		t.Fatal(err)
	}
	if fixturesContent != "" {
		fx := filepath.Join(boardDir, "fixtures.jsonl")
		if err := os.WriteFile(fx, []byte(fixturesContent), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return board
}

const advR05Registry = `{"id":"E2E-001","title":"E2E Testing Tick","active":true}` + "\n" +
	`{"id":"GITREINS-JUDGE","title":"LLM evaluator config","active":true}` + "\n" +
	`{"id":"NEVER-DONE","title":"11-point perpetual audit","active":true}` + "\n"

// CLASS 1 — declared-in-data fixture: the row's id is declared active in
// fixtures.jsonl. Exclusion must hold even when the row carries the
// dangerous status — status:"pending" — because exclusion is by data, not
// by status-vocabulary accident (pre-change, both rows counted as real
// pending work: E2E-001/GITREINS-JUDGE had no id-shape protection).
func TestCountPending_DeclaredFixtureExcludedByRegistry_ADV_R05(t *testing.T) {
	dir := t.TempDir()
	tasks := `{"id":"E2E-001","title":"fixture","status":"pending"}` + "\n" +
		`{"id":"GITREINS-JUDGE","title":"fixture","status":"pending"}` + "\n" +
		`{"id":"W-1","title":"real work","status":"pending"}` + "\n"
	board := writeAdvR05Board(t, dir, tasks, advR05Registry)
	fi, err := os.Stat(board)
	if err != nil {
		t.Fatal(err)
	}
	if got := countPendingBoard(board, fi); got != 1 {
		t.Fatalf("countPendingBoard = %d, want 1 (declared fixtures excluded by data, W-1 counts)", got)
	}
	c := NewPendingTaskCounter(60 * time.Second)
	if got := c.CountPending(dir); got != 1 {
		t.Fatalf("CountPending = %d, want 1 (declared fixtures excluded by data)", got)
	}
	// The same exclusion applies to boardOpenRows (adaptive cooldown must
	// agree with the pending boost — GAP-106's both-directions rule).
	if got, ok := boardOpenRows(dir); !ok || got != 1 {
		t.Fatalf("boardOpenRows = %d,%v want 1,true (declared fixtures are not open work)", got, ok)
	}
}

// CLASS 1b — a registry entry with active:false is NOT a fixture: the row
// counts as real work again (deactivation is how an operator retires a
// fixture exclusion).
func TestCountPending_InactiveFixtureEntryCounts_ADV_R05(t *testing.T) {
	dir := t.TempDir()
	tasks := `{"id":"E2E-001","title":"retired fixture","status":"pending"}` + "\n"
	fixtures := `{"id":"E2E-001","title":"retired","active":false}` + "\n"
	board := writeAdvR05Board(t, dir, tasks, fixtures)
	fi, err := os.Stat(board)
	if err != nil {
		t.Fatal(err)
	}
	if got := countPendingBoard(board, fi); got != 1 {
		t.Fatalf("countPendingBoard = %d, want 1 (active:false entry must NOT exclude)", got)
	}
}

// CLASS 2 — perpetual-flag fixture: "perpetual":true on the row itself,
// id not in any registry and not in the NEVER-DONE family (SCHED-GAP-106
// class, restated here so each fixture class has its own pin).
func TestCountPending_PerpetualFlagFixtureExcluded_ADV_R05(t *testing.T) {
	dir := t.TempDir()
	tasks := `{"id":"AUDIT-X","title":"flagged fixture","status":"pending","perpetual":true}` + "\n" +
		`{"id":"W-1","title":"real work","status":"pending"}` + "\n"
	board := writeAdvR05Board(t, dir, tasks, advR05Registry)
	fi, err := os.Stat(board)
	if err != nil {
		t.Fatal(err)
	}
	if got := countPendingBoard(board, fi); got != 1 {
		t.Fatalf("countPendingBoard = %d, want 1 (perpetual:true row excluded)", got)
	}
}

// CLASS 3 — id-shape fixture (fallback): a NEVER-DONE-family id on a board
// with NO registry file and NO perpetual flag. Fleet boards that carry the
// fleet-standard fixture without declaring it (e.g. a get-h3 workdir whose
// board has no fixtures.jsonl) must keep being excluded — this fallback may
// not regress, or previously-excluded rows would start counting.
func TestCountPending_IdShapeFallbackFixtureExcluded_ADV_R05(t *testing.T) {
	dir := t.TempDir()
	tasks := `{"id":"NEVER-DONE","title":"unflagged, undeclared","status":"pending"}` + "\n" +
		`{"id":"never-done-2","title":"suffix variant","status":"pending"}` + "\n" +
		`{"id":"W-1","title":"real work","status":"pending"}` + "\n"
	board := writeAdvR05Board(t, dir, tasks, "") // no fixtures.jsonl at all
	fi, err := os.Stat(board)
	if err != nil {
		t.Fatal(err)
	}
	if got := countPendingBoard(board, fi); got != 1 {
		t.Fatalf("countPendingBoard = %d, want 1 (NEVER-DONE id fallback still excludes)", got)
	}
}

// CLASS 4 — non-fixture lookalikes must COUNT: ids that merely resemble a
// declared fixture id are not excluded (exact id match only — no substring
// accident replacing the old status accident). Prefix-adjacent NEVER-DONE
// ids (e.g. NEVER-DONELESS) are a different, documented class: the id-shape
// fallback (class 3) intentionally excludes the whole NEVER-DONE family.
func TestCountPending_RegistryLookalikeNonFixtureCounts_ADV_R05(t *testing.T) {
	dir := t.TempDir()
	tasks := `{"id":"E2E-0011","title":"lookalike id","status":"pending"}` + "\n" +
		`{"id":"RE-RUN-E2E-001","title":"id mentioning a fixture","status":"pending"}` + "\n" +
		`{"id":"sub/E2E-001","title":"id containing fixture id","status":"pending"}` + "\n"
	board := writeAdvR05Board(t, dir, tasks, advR05Registry)
	fi, err := os.Stat(board)
	if err != nil {
		t.Fatal(err)
	}
	if got := countPendingBoard(board, fi); got != 3 {
		t.Fatalf("countPendingBoard = %d, want 3 (no lookalike may be excluded)", got)
	}
}

// Registry robustness: malformed registry lines are skipped (never fatal),
// and a missing registry file leaves only the fallback layers.
func TestCountPending_MalformedRegistryIgnored_ADV_R05(t *testing.T) {
	dir := t.TempDir()
	tasks := `{"id":"E2E-001","title":"declared","status":"pending"}` + "\n" +
		`{"id":"W-1","title":"real work","status":"pending"}` + "\n"
	fixtures := `{this is not json` + "\n" +
		`{"id":"E2E-001","active":true}` + "\n"
	board := writeAdvR05Board(t, dir, tasks, fixtures)
	fi, err := os.Stat(board)
	if err != nil {
		t.Fatal(err)
	}
	if got := countPendingBoard(board, fi); got != 1 {
		t.Fatalf("countPendingBoard = %d, want 1 (malformed registry line skipped, valid entry excludes)", got)
	}
}

// Markdown boards: a declared fixture's unchecked header ("## [ ] E2E-001
// ...") is excluded by the registry via first-token id match, while a
// lookalike header still counts. (Pre-change the markdown branch had no
// registry knowledge at all.)
func TestCountPending_MarkdownDeclaredFixtureExcluded_ADV_R05(t *testing.T) {
	dir := t.TempDir()
	chDir := filepath.Join(dir, ".coding-hermes")
	boardDir := filepath.Join(chDir, "board")
	if err := os.MkdirAll(boardDir, 0o755); err != nil {
		t.Fatal(err)
	}
	md := "## [ ] E2E-001 - e2e testing tick\n" +
		"## [ ] E2E-0011 - lookalike\n" +
		"## [ ] GAP-9 - real work\n" +
		"## [x] NEVER-DONE - done fixture\n"
	if err := os.WriteFile(filepath.Join(chDir, "tasks.md"), []byte(md), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(boardDir, "fixtures.jsonl"), []byte(advR05Registry), 0o644); err != nil {
		t.Fatal(err)
	}
	c := NewPendingTaskCounter(60 * time.Second)
	if got := c.CountPending(dir); got != 2 {
		t.Fatalf("CountPending(md) = %d, want 2 (declared E2E-001 excluded, lookalike + real work count)", got)
	}
}

// Cache wiring: editing fixtures.jsonl must invalidate the pending cache
// even when tasks.jsonl is untouched — a fixture declared after the first
// read takes effect on the next CountPending, not after the TTL.
func TestCountPending_RegistryEditInvalidatesCache_ADV_R05(t *testing.T) {
	dir := t.TempDir()
	tasks := `{"id":"E2E-001","title":"soon a fixture","status":"pending"}` + "\n" +
		`{"id":"W-1","title":"real work","status":"pending"}` + "\n"
	boardDir := filepath.Join(dir, ".coding-hermes", "board")
	if err := os.MkdirAll(boardDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(boardDir, "tasks.jsonl"), []byte(tasks), 0o644); err != nil {
		t.Fatal(err)
	}
	c := NewPendingTaskCounter(60 * time.Second)
	if got := c.CountPending(dir); got != 2 {
		t.Fatalf("first CountPending = %d, want 2 (no registry yet)", got)
	}
	if err := os.WriteFile(filepath.Join(boardDir, "fixtures.jsonl"), []byte(advR05Registry), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := c.CountPending(dir); got != 1 {
		t.Fatalf("CountPending after registry write = %d, want 1 (registry mtime must invalidate cache)", got)
	}
}

// Pending-vs-open rule (documented intentional): CountPending counts ONLY
// status=="pending" — the dispatchable state. The wider open vocabulary
// (todo here) stays invisible to the boost by design; boardOpenRows is the
// open-work signal that sees it.
func TestCountPending_TodoRowsNotCounted_RULE_ADV_R05(t *testing.T) {
	dir := t.TempDir()
	tasks := `{"id":"SCHED-GAP-999","title":"todo row","status":"todo"}` + "\n" +
		`{"id":"W-1","title":"real pending","status":"pending"}` + "\n"
	board := writeAdvR05Board(t, dir, tasks, advR05Registry)
	fi, err := os.Stat(board)
	if err != nil {
		t.Fatal(err)
	}
	if got := countPendingBoard(board, fi); got != 1 {
		t.Fatalf("countPendingBoard = %d, want 1 (todo is not dispatchable; pending counts)", got)
	}
	open, ok := boardOpenRows(dir)
	if !ok || open != 2 {
		t.Fatalf("boardOpenRows = %d,%v want 2,true (open-work signal sees todo + pending)", open, ok)
	}
}
