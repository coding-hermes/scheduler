package scheduler

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// SCHED-430 acceptance — ticks must record WHY they were dry.
//
// The rows these tests exercise are the ones Bane's 2026-11-15 drill could only
// explain by transcript archaeology: 344 dry ticks/week, 86.9% of them
// ownership-barrier. Every assertion below reads the PERSISTED
// ticks.failure_reason column, never a helper's return value, because the column
// is the deliverable (a classifier that computes a token and writes it nowhere
// is the defect this row removes).

// sched430Vocab is the board's verbatim vocabulary, spelled out here so a
// renamed/removed token fails a test as well as a grep.
var sched430Vocab = []string{
	"EMPTY_BOARD",
	"OWNERSHIP_BARRIER",
	"GATE_WITHHELD",
	"DUPLICATE_PREVENTION",
	"DEFERRED",
	"WAVE_PERSISTED",
}

// sched430Board writes a JSONL board into a fresh workdir and returns the
// workdir. No rows → an empty board file (the "read and drained" shape).
func sched430Board(t *testing.T, rows ...string) string {
	t.Helper()
	wd := t.TempDir()
	boardDir := filepath.Join(wd, ".coding-hermes", "board")
	if err := os.MkdirAll(boardDir, 0o755); err != nil {
		t.Fatalf("mkdir board dir: %v", err)
	}
	body := ""
	if len(rows) > 0 {
		body = strings.Join(rows, "\n") + "\n"
	}
	if err := os.WriteFile(filepath.Join(boardDir, "tasks.jsonl"), []byte(body), 0o644); err != nil {
		t.Fatalf("write board: %v", err)
	}
	return wd
}

// sched430Lane creates the project row and points it at workdir.
func sched430Lane(t *testing.T, db *sql.DB, project, workdir string) {
	t.Helper()
	mustCreateProjectINFRA012(t, db, project)
	if _, err := db.Exec(`UPDATE projects SET workdir = ? WHERE name = ?`, workdir, project); err != nil {
		t.Fatalf("set workdir for %s: %v", project, err)
	}
}

// sched430Complete enqueues then completes one tick and reads back the row's
// (status, outcome, failure_reason) — the persisted truth.
func sched430Complete(t *testing.T, db *sql.DB, project, tickID string, o TickOutcome) (string, string, string) {
	t.Helper()
	lt := NewLifecycleTracker(db)
	if err := lt.Enqueue(project, tickID); err != nil {
		t.Fatalf("enqueue %s: %v", tickID, err)
	}
	o.TickID = tickID
	o.Project = project
	if o.Started.IsZero() {
		o.Started = time.Now().Add(-time.Second)
	}
	if o.Finished.IsZero() {
		o.Finished = time.Now()
	}
	if err := lt.Complete(o); err != nil {
		t.Fatalf("complete %s: %v", tickID, err)
	}
	var status, outcome, reason string
	if err := db.QueryRow(
		`SELECT status, COALESCE(outcome,''), COALESCE(failure_reason,'') FROM ticks WHERE id = ?`, tickID,
	).Scan(&status, &outcome, &reason); err != nil {
		t.Fatalf("read tick %s: %v", tickID, err)
	}
	return status, outcome, reason
}

// TestSched430_VocabularyIsClosed — AC-1: the six tokens exist as Go constants
// and nothing else validates.
func TestSched430_VocabularyIsClosed(t *testing.T) {
	if len(DryReasons) != len(sched430Vocab) {
		t.Fatalf("DryReasons has %d members (%v), want the board's %d: %v", len(DryReasons), DryReasons, len(sched430Vocab), sched430Vocab)
	}
	for _, want := range sched430Vocab {
		if !DryReasonIsValid(want) {
			t.Errorf("%s is not a valid dry reason — the closed vocabulary must contain every board token", want)
		}
		found := false
		for _, got := range DryReasons {
			if got == want {
				found = true
			}
		}
		if !found {
			t.Errorf("DryReasons is missing %s", want)
		}
	}
	for _, bad := range []string{"", "dry_run", "empty_board", "UNKNOWN", " no_changes "} {
		if DryReasonIsValid(bad) {
			t.Errorf("DryReasonIsValid(%q) = true — the vocabulary is closed", bad)
		}
	}
}

// TestSched430_DryTickWritesReason — AC-1/AC-2/AC-3 in one table: each dry path
// completes a tick and the PERSISTED column must carry the mapping's token, in
// the same write as the outcome (outcome is asserted alongside).
func TestSched430_DryTickWritesReason(t *testing.T) {
	claimed := `{"id":"T1","status":"in_progress","title":"owned by a live worker"}`
	claimed2 := `{"id":"T2","status":"claimed","title":"owned by a live tick"}`
	pending := `{"id":"T3","status":"pending","title":"dispatchable"}`
	deferredRow := `{"id":"T4","status":"pending","deferred":true,"title":"owner said not now"}`

	cases := []struct {
		name    string
		board   []string
		noBoard bool
		outcome TickOutcome
		want    string
	}{
		{
			name:  "empty board",
			board: nil,
			outcome: TickOutcome{
				Status: TickCompleted, DispatchDispatched: true,
				DispatchReason: "dispatched", NoTools: true,
			},
			want: DryReasonEmptyBoard,
		},
		{
			name:  "board whose open rows are all owned",
			board: []string{claimed, claimed2},
			outcome: TickOutcome{
				Status: TickCompleted, DispatchDispatched: true, DispatchReason: "dispatched",
				Commits: 0, FilesChanged: 0, MemoryKeys: 0,
			},
			want: DryReasonOwnershipBarrier,
		},
		{
			name:  "board holding only deferred rows",
			board: []string{deferredRow},
			outcome: TickOutcome{
				Status: TickCompleted, DispatchDispatched: true, DispatchReason: "dispatched",
			},
			want: DryReasonDeferred,
		},
		{
			name:  "dispatchable row available, still landed nothing",
			board: []string{pending},
			outcome: TickOutcome{
				Status: TickCompleted, DispatchDispatched: true, DispatchReason: "dispatched",
			},
			want: DryReasonGateWithheld,
		},
		{
			name:  "completed without ever dispatching a foreman",
			board: []string{pending},
			outcome: TickOutcome{
				Status: TickCompleted, DispatchDispatched: false, DispatchReason: "blocked",
			},
			want: DryReasonGateWithheld,
		},
		{
			name:    "lane with no board at all",
			noBoard: true,
			outcome: TickOutcome{
				Status: TickCompleted, DispatchDispatched: true, DispatchReason: "dispatched",
			},
			want: DryReasonEmptyBoard,
		},
		{
			name:  "declared duplicate-prevention stand-down",
			board: []string{pending},
			outcome: TickOutcome{
				Status: TickCompleted, DispatchDispatched: true, DispatchReason: "dispatched",
				DryReason: DryReasonDuplicatePrevention,
			},
			want: DryReasonDuplicatePrevention,
		},
		{
			name:  "declared value outside the vocabulary is ignored, derivation wins",
			board: nil,
			outcome: TickOutcome{
				Status: TickCompleted, DispatchDispatched: true, DispatchReason: "dispatched",
				DryReason: "NOT_A_TOKEN",
			},
			want: DryReasonEmptyBoard,
		},
		{
			name:  "zero-tool session records the dry cause and the no_work verdict",
			board: nil,
			outcome: TickOutcome{
				Status: TickCompleted, DispatchDispatched: true, DispatchReason: "dispatched",
				NoTools: true,
			},
			want: DryReasonEmptyBoard,
		},
	}

	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := newTestDB(t)
			project := "sched430-dry-" + strings.ReplaceAll(tc.name, " ", "-")
			wd := t.TempDir()
			if !tc.noBoard {
				wd = sched430Board(t, tc.board...)
			}
			sched430Lane(t, db, project, wd)
			tickID := project + "-tick-" + string(rune('a'+i))

			status, _, reason := sched430Complete(t, db, project, tickID, tc.outcome)
			if status != "completed" {
				t.Fatalf("status = %q, want completed (a dry tick COMPLETES — it is not a failure)", status)
			}
			if !DryReasonIsValid(reason) {
				t.Fatalf("ticks.failure_reason = %q, want a vocabulary token (one of %v) — this is the AC-3 gap", reason, sched430Vocab)
			}
			if reason != tc.want {
				t.Errorf("ticks.failure_reason = %q, want %q", reason, tc.want)
			}
		})
	}
}

// TestSched430_OutcomeAndReasonShareOneWrite — AC-2: the reason rides the SAME
// finalization UPDATE as the outcome, so a row can never carry one without the
// other.
func TestSched430_OutcomeAndReasonShareOneWrite(t *testing.T) {
	db := newTestDB(t)
	const project = "sched430-shared-write"
	sched430Lane(t, db, project, sched430Board(t, `{"id":"T1","status":"claimed","title":"owned"}`))

	status, outcome, reason := sched430Complete(t, db, project, project+"-tick-1", TickOutcome{
		Status: TickCompleted, DispatchDispatched: true, DispatchReason: "dispatched",
	})
	if status != "completed" || outcome != "dry_run" || reason != DryReasonOwnershipBarrier {
		t.Fatalf("row = (%s, %s, %s), want (completed, dry_run, %s)", status, outcome, reason, DryReasonOwnershipBarrier)
	}
	// The forensics window AC-4 documents: the reason is non-empty on the SAME
	// row as the verdict, so `failure_reason <> ''` selects exactly the rows
	// whose cause is recorded.
	var pairs int
	if err := db.QueryRow(`SELECT COUNT(*) FROM ticks WHERE outcome = 'dry_run' AND failure_reason <> ''`).Scan(&pairs); err != nil {
		t.Fatalf("count dry rows with a reason: %v", err)
	}
	if pairs != 1 {
		t.Errorf("dry_run rows carrying a reason = %d, want 1 (the outcome and the reason must land together)", pairs)
	}
}

// TestSched430_WavePersisted — a wave tick's product is its worker rows; the
// manifest is the evidence, and it outranks the board read.
func TestSched430_WavePersisted(t *testing.T) {
	db := newTestDB(t)
	const project = "sched430-wave"
	const tickID = "sched430-wave-2026-11-16-00-00-01"
	wd := sched430Board(t) // empty board: the wave is the only artifact
	sched430Lane(t, db, project, wd)

	wavesDir := filepath.Join(wd, ".coding-hermes", "waves")
	if err := os.MkdirAll(wavesDir, 0o755); err != nil {
		t.Fatalf("mkdir waves: %v", err)
	}
	manifest := `{"tick_id":"` + tickID + `","project":"` + project + `","started_at":"2026-11-16T00:00:00Z","finished_at":"2026-11-16T00:10:00Z","workers":[{"task_id":"W1","branch":"wt/W1","worktree":"/home/kara/worktrees/w1"}]}`
	if err := os.WriteFile(filepath.Join(wavesDir, tickID+".json"), []byte(manifest), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	_, outcome, reason := sched430Complete(t, db, project, tickID, TickOutcome{
		Status: TickCompleted, DispatchDispatched: true, DispatchReason: "dispatched",
	})
	if outcome != "dry_run" {
		t.Errorf("outcome = %q, want dry_run (the tick itself landed no commit)", outcome)
	}
	if reason != DryReasonWavePersisted {
		t.Errorf("ticks.failure_reason = %q, want %q — a persisted wave must not be filed as an empty board", reason, DryReasonWavePersisted)
	}
}

// TestSched430_ExistingClassesUntouched — the rows this change must NOT disturb:
// a productive tick, a transport-class failure, and a SCHED-GAP-203 deferral all
// keep exactly the column they had.
func TestSched430_ExistingClassesUntouched(t *testing.T) {
	t.Run("productive tick keeps an empty column", func(t *testing.T) {
		db := newTestDB(t)
		const project = "sched430-productive"
		sched430Lane(t, db, project, sched430Board(t))
		_, outcome, reason := sched430Complete(t, db, project, project+"-tick-1", TickOutcome{
			Status: TickCompleted, DispatchDispatched: true, DispatchReason: "dispatched",
			Commits: 2, FilesChanged: 3,
		})
		if outcome != "committed" {
			t.Fatalf("outcome = %q, want committed", outcome)
		}
		if reason != "" {
			t.Errorf("ticks.failure_reason = %q, want \\\"\\\" — a productive tick is not dry", reason)
		}
	})

	t.Run("project-side failure keeps the classifier verdict", func(t *testing.T) {
		db := newTestDB(t)
		const project = "sched430-failed"
		sched430Lane(t, db, project, sched430Board(t))
		status, _, reason := sched430Complete(t, db, project, project+"-tick-1", TickOutcome{
			Status: TickFailed, Error: "project-side explosion", ExitCode: -1,
			DispatchDispatched: true, DispatchReason: "dispatched",
		})
		if status != "failed" {
			t.Fatalf("status = %q, want failed", status)
		}
		if reason != "" {
			t.Errorf("ticks.failure_reason = %q, want \\\"\\\" — the classifier untouched, and a failure is not a dry tick", reason)
		}
	})

	t.Run("deferred row keeps its empty column", func(t *testing.T) {
		db := newTestDB(t)
		const project = "sched430-deferred"
		sched430Lane(t, db, project, sched430Board(t))
		status, outcome, reason := sched430Complete(t, db, project, project+"-tick-1", TickOutcome{
			Status: TickDeferred, Error: "gateway transient error: connection refused",
		})
		if status != "deferred" || outcome != "deferred" {
			t.Fatalf("row = (%s, %s), want (deferred, deferred)", status, outcome)
		}
		if reason != "" {
			t.Errorf("ticks.failure_reason = %q, want \\\"\\\" — SCHED-GAP-203's doctrine: the status column already names the class", reason)
		}
	})
}

// TestSched430_BoardEvidenceSplit — the ONE board scan must keep boardOpenRows'
// contract byte-for-byte while exposing the pending/owned split the vocabulary
// needs (a second parser is how these counts drift apart).
func TestSched430_BoardEvidenceSplit(t *testing.T) {
	wd := sched430Board(t,
		`{"id":"A","status":"pending"}`,
		`{"id":"B","status":"in_progress"}`,
		`{"id":"C","status":"claimed"}`,
		`{"id":"D","status":"rework"}`,
		`{"id":"E","status":"complete"}`,
		`{"id":"F","status":"pending","deferred":true}`,
		`not-json`,
	)
	ev := boardDryEvidence(wd)
	if !ev.read {
		t.Fatal("boardDryEvidence read=false on a readable board")
	}
	// open = A, B, C, D + the malformed row (never hide work); E is complete and
	// F is deferred, both excluded — exactly boardOpenRows' rule.
	if ev.open != 5 {
		t.Errorf("open = %d, want 5", ev.open)
	}
	if ev.pending != 2 {
		t.Errorf("pending = %d, want 2 (A + the malformed row)", ev.pending)
	}
	if ev.deferred != 1 {
		t.Errorf("deferred = %d, want 1 (F)", ev.deferred)
	}
	if open, ok := boardOpenRows(wd); !ok || open != ev.open {
		t.Errorf("boardOpenRows = (%d, %t), want (%d, true) — the delegation must not change the count", open, ok, ev.open)
	}

	if got := dryReasonFromBoardEvidence(ev); got != DryReasonGateWithheld {
		t.Errorf("mixed board classified %q, want %q", got, DryReasonGateWithheld)
	}
	// A board with no board file at all is an evidence gap, not an empty board.
	if got := boardDryEvidence(t.TempDir()); got.read {
		t.Errorf("dryBoardEvidence(%q).read = true, want false (no board)", t.TempDir())
	}
}
