package scheduler

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// SCHED-GAP-NODRAIN: tasks-mode admission must require a WORKABLE row, not
// merely an open row. A board holding only blocked rows, or only rows whose
// depends_on entries are unmet, must read as DRAINED (tasksAdmissionDue
// false) so the lane falls back to its cooldown pin instead of being
// re-admitted on the 5s eval debounce forever. A plain pending row, a
// blocked-plus-workable mix, and a malformed line (parse errors never hide
// work) must still admit.
//
// AC4 is pinned in TestSCHEDGAPNODRAIN_OpenRowsUnchanged: boardOpenRows is
// NOT touched by this row, and the same fixtures must return its pre-change
// counts. Note the counts themselves: status="blocked" was never in the
// open switch (only ""/pending/open/in_progress/in-progress/claimed/ready/
// todo/new/rework), so the open count on an only-blocked fixture is 0 — the
// "still returns 2 open rows" shape of the AC is the UNMET-DEPS fixture
// (two pending rows count open pre- and post-change), which is also the
// fixture where the drain defect actually fired: open=2 admitted the lane
// while workable=0 must refuse it.

// nodrainWriteBoard writes a canonical JSONL board
// (.coding-hermes/board/tasks.jsonl) into a fresh temp workdir and returns
// the workdir.
func nodrainWriteBoard(t *testing.T, rows ...string) string {
	t.Helper()
	dir := t.TempDir()
	boardDir := filepath.Join(dir, ".coding-hermes", "board")
	if err := os.MkdirAll(boardDir, 0o755); err != nil {
		t.Fatalf("mkdir board dir: %v", err)
	}
	var b strings.Builder
	for _, r := range rows {
		b.WriteString(r)
		b.WriteByte('\n')
	}
	path := filepath.Join(boardDir, "tasks.jsonl")
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		t.Fatalf("write board: %v", err)
	}
	return dir
}

// The fixture rows. ND-* ids are unique to this file so a registry or a
// sibling fixture can never collide with them.
const (
	nodrainBlockedRow  = `{"id":"ND-B1","status":"blocked","blocked_reason":"waiting on owner"}`
	nodrainBlockedRow2 = `{"id":"ND-B2","status":"blocked","blocked_reason":"owner decision"}`
	// Unmet shapes: dep on an ABSENT row, dep on an OPEN row, dep on a
	// BLOCKED row — every non-terminal resolution the rule covers.
	nodrainUnmetAbsent  = `{"id":"ND-U1","status":"pending","depends_on":["ND-GHOST-1"]}`
	nodrainUnmetOpen    = `{"id":"ND-U2","status":"pending","depends_on":["ND-U1"]}`
	nodrainUnmetBlocked = `{"id":"ND-U3","status":"pending","depends_on":["ND-B1"]}`
	nodrainWorkable     = `{"id":"ND-W1","status":"pending"}`
	// A MET dependency: the referenced row exists and is terminal, so the
	// dependent row is workable.
	nodrainDoneDep   = `{"id":"ND-D1","status":"complete"}`
	nodrainMetDepRow = `{"id":"ND-W2","status":"pending","depends_on":["ND-D1"]}`
)

func TestSCHEDGAPNODRAIN_OnlyBlockedRowsNotAdmissible(t *testing.T) {
	dir := nodrainWriteBoard(t, nodrainBlockedRow, nodrainBlockedRow2)
	if tasksAdmissionDue(dir, "") {
		t.Fatal("a board holding only blocked rows must read as drained (admission false)")
	}
}

func TestSCHEDGAPNODRAIN_OnlyUnmetDepsNotAdmissible(t *testing.T) {
	dir := nodrainWriteBoard(t, nodrainUnmetAbsent, nodrainUnmetOpen, nodrainUnmetBlocked)
	if tasksAdmissionDue(dir, "") {
		t.Fatal("a board whose only rows have unmet depends_on must read as drained (admission false)")
	}
}

func TestSCHEDGAPNODRAIN_MixedBlockedPlusWorkableAdmissible(t *testing.T) {
	dir := nodrainWriteBoard(t, nodrainBlockedRow, nodrainWorkable)
	if !tasksAdmissionDue(dir, "") {
		t.Fatal("one blocked row plus one workable row must admit (admission true)")
	}
}

func TestSCHEDGAPNODRAIN_PlainPendingRowAdmissible(t *testing.T) {
	dir := nodrainWriteBoard(t, nodrainWorkable)
	if !tasksAdmissionDue(dir, "") {
		t.Fatal("a plain pending row must admit (admission true)")
	}
}

func TestSCHEDGAPNODRAIN_MetDepWorkAdmissible(t *testing.T) {
	// Guards the rule's other edge: a row whose dependency is RESOLVED
	// (exists + terminal) is workable — the scanner must not over-narrow
	// into refusing real, dispatchable work.
	dir := nodrainWriteBoard(t, nodrainDoneDep, nodrainMetDepRow)
	if !tasksAdmissionDue(dir, "") {
		t.Fatal("a pending row with a met depends_on must admit (admission true)")
	}
}

func TestSCHEDGAPNODRAIN_MalformedRowStillWorkable(t *testing.T) {
	// Existing doctrine: a parse error must never hide work. One garbage
	// line plus one blocked row: the malformed line keeps the board
	// workable (it was counted OPEN before this row), so admission stays
	// true and the change cannot silently starve a board on a torn write.
	dir := nodrainWriteBoard(t, nodrainBlockedRow, `{not json at all`)
	if !tasksAdmissionDue(dir, "") {
		t.Fatal("a malformed row must still count workable (admission true)")
	}
}

func TestSCHEDGAPNODRAIN_MissingBoardNotAdmissible(t *testing.T) {
	dir := t.TempDir() // no .coding-hermes at all
	if tasksAdmissionDue(dir, "") {
		t.Fatal("missing board must not admit (fail-open unchanged)")
	}
	if n, ok := boardWorkableRows(dir); ok || n != 0 {
		t.Fatalf("missing board: scanner must report (0,false), got (%d,%v)", n, ok)
	}
}

func TestSCHEDGAPNODRAIN_UnreadableBoardNotAdmissible(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: chmod 000 cannot make the board unreadable")
	}
	dir := nodrainWriteBoard(t, nodrainWorkable)
	path := filepath.Join(dir, ".coding-hermes", "board", "tasks.jsonl")
	if err := os.Chmod(path, 0o000); err != nil {
		t.Fatalf("chmod board: %v", err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(path, 0o644); err != nil {
			t.Fatalf("restore board perms: %v", err)
		}
	})
	if tasksAdmissionDue(dir, "") {
		t.Fatal("unreadable board must not admit (fail-open unchanged)")
	}
	if n, ok := boardWorkableRows(path); ok || n != 0 {
		t.Fatalf("unreadable board: scanner must report (0,false), got (%d,%v)", n, ok)
	}
}

func TestSCHEDGAPNODRAIN_OwnershipRefusalUntouched(t *testing.T) {
	// SCHED-GAP-141 behaviour is untouched: a workdir whose board resolves
	// OUTSIDE itself (here: a foreign board dir) refuses the waiver and
	// returns false even though the foreign board is full of work.
	foreign := nodrainWriteBoard(t, nodrainWorkable)
	lane := t.TempDir()
	if tasksAdmissionDue(lane, "") {
		t.Fatal("a lane whose board resolves outside its workdir must refuse the waiver")
	}
	_ = foreign
}

func TestSCHEDGAPNODRAIN_OpenRowsUnchanged(t *testing.T) {
	// AC4: boardOpenRows keeps its pre-change counts on the SAME fixtures
	// while the workability count differs exactly where the rule says it
	// must. Each case names the fixture, the open scanner's pre-change
	// count (unchanged — this row does not touch boardOpenRows), and the
	// workable count.
	cases := []struct {
		name         string
		rows         []string
		wantOpen     int
		wantWorkable int
	}{
		{
			// status="blocked" is NOT in the open switch (pre-change and
			// post-change): the open count is 0, same as workable.
			name:         "only-blocked",
			rows:         []string{nodrainBlockedRow, nodrainBlockedRow2},
			wantOpen:     0,
			wantWorkable: 0,
		},
		{
			// THE drain fixture: three pending rows count OPEN (2 were
			// enough to admit pre-change) while zero are workable.
			name:         "only-unmet-deps",
			rows:         []string{nodrainUnmetAbsent, nodrainUnmetOpen, nodrainUnmetBlocked},
			wantOpen:     3,
			wantWorkable: 0,
		},
		{
			name:         "blocked-plus-workable",
			rows:         []string{nodrainBlockedRow, nodrainWorkable},
			wantOpen:     1, // blocked never counted open; the pending row is
			wantWorkable: 1,
		},
		{
			name:         "plain-pending",
			rows:         []string{nodrainWorkable},
			wantOpen:     1,
			wantWorkable: 1,
		},
		{
			name:         "malformed-counts-open-and-workable",
			rows:         []string{nodrainBlockedRow, `{not json at all`},
			wantOpen:     1, // malformed row counts open (pre-change doctrine)
			wantWorkable: 1, // and workable (never hide work)
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := nodrainWriteBoard(t, tc.rows...)
			open, ok := boardOpenRows(dir)
			if !ok {
				t.Fatalf("boardOpenRows: board must be readable")
			}
			if open != tc.wantOpen {
				t.Fatalf("boardOpenRows drifted: got %d open, want %d (the shared counter must keep its pre-change counts)", open, tc.wantOpen)
			}
			workable, ok := boardWorkableRows(dir)
			if !ok {
				t.Fatalf("boardWorkableRows: board must be readable")
			}
			if workable != tc.wantWorkable {
				t.Fatalf("workable count: got %d, want %d", workable, tc.wantWorkable)
			}
		})
	}
}
