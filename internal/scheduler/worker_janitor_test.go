package scheduler

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/coding-hermes/scheduler/internal/database"
)

// SCHED-GAP-1706 regression: a tick's own terminal state is authoritative —
// once the ticks row is completed/failed/timeout, its tick_workers rows can
// never finish, so any state='running' left on them is a leak. The janitor
// (reapOrphanedWorkerRows, hooked in Loop.Run after the reapers and before
// the resume nudge) flips those rows to 'abandoned' and keeps the row
// (attribution reconstructable). It must also flip running rows older than
// the per-tick wave cap even on a live tick, and must never touch
// non-running rows or a fresh running row of a still-active tick.

// insertJanitorTick inserts a ticks row in the given status.
func insertJanitorTick(t *testing.T, db *sql.DB, project, tickID, status string) {
	t.Helper()
	_, err := db.Exec(
		`INSERT INTO ticks (id, project_name, status, created_at) VALUES (?, ?, ?, ?)`,
		tickID, project, status, time.Now().UTC().Format(time.RFC3339))
	if err != nil {
		t.Fatalf("insert tick %s: %v", tickID, err)
	}
}

// janitorWorkerRow is one table case: a single tick_workers row seeded under
// a tick in tickStatus, with workerState and an explicit updated_at age.
type janitorWorkerRow struct {
	tickStatus  string        // ticks.status for the owning tick
	workerState string        // seeded tick_workers.state
	age         time.Duration // seeded updated_at age (negative = old)
	wantState   string        // expected state after the janitor pass
	wantFlip    bool          // true = row must be flipped, updated_at restamped
}

func TestReapOrphanedWorkerRows(t *testing.T) {
	db := newTestDB(t)
	const project = "sgap1706-janitor"
	mustCreateProjectINFRA012(t, db, project)

	loop := NewLoop(db, time.Minute, time.Hour, 10, 100, 5)
	// The wave-cap branch derives its cutoff from the SCHED-GAP-217 backstop;
	// seed the "old" rows beyond it so the case stays robust to the spawner's
	// configured timeout.
	oldAge := -(loop.backstopMaxAge() + 24*time.Hour)
	freshAge := -time.Minute

	cases := []struct {
		name string
		row  janitorWorkerRow
	}{
		{"running under completed tick leaks and flips", janitorWorkerRow{"completed", database.TickWorkerStateRunning, freshAge, database.TickWorkerStateAbandoned, true}},
		{"running under failed tick leaks and flips", janitorWorkerRow{"failed", database.TickWorkerStateRunning, freshAge, database.TickWorkerStateAbandoned, true}},
		{"running under timeout tick leaks and flips", janitorWorkerRow{"timeout", database.TickWorkerStateRunning, freshAge, database.TickWorkerStateAbandoned, true}},
		{"running older than wave cap under live tick flips", janitorWorkerRow{"running", database.TickWorkerStateRunning, oldAge, database.TickWorkerStateAbandoned, true}},
		{"fresh running under live tick is not classified", janitorWorkerRow{"running", database.TickWorkerStateRunning, freshAge, database.TickWorkerStateRunning, false}},
		{"done row under terminal tick is untouched", janitorWorkerRow{"completed", database.TickWorkerStateDone, freshAge, database.TickWorkerStateDone, false}},
		{"abandoned row under terminal tick is untouched", janitorWorkerRow{"failed", database.TickWorkerStateAbandoned, freshAge, database.TickWorkerStateAbandoned, false}},
	}

	var wantFlipped int
	seeded := make([]string, len(cases))
	for i, tc := range cases {
		if tc.row.wantFlip {
			wantFlipped++
		}
		tickID := project + "-t" + string(rune('a'+i))
		insertJanitorTick(t, db, project, tickID, tc.row.tickStatus)
		seeded[i] = time.Now().UTC().Add(tc.row.age).Format(time.RFC3339)
		w := &database.TickWorker{
			TickID:    tickID,
			TaskID:    project + "-w" + string(rune('a'+i)),
			Branch:    "wt/" + tc.row.workerState,
			State:     tc.row.workerState,
			UpdatedAt: seeded[i],
		}
		if _, err := database.CreateTickWorker(context.Background(), db, w); err != nil {
			t.Fatalf("%s: seed worker row: %v", tc.name, err)
		}
	}

	if got := loop.reapOrphanedWorkerRows(); got != wantFlipped {
		t.Errorf("reapOrphanedWorkerRows() = %d flipped rows, want %d", got, wantFlipped)
	}

	for i, tc := range cases {
		workerID := project + "-w" + string(rune('a'+i))
		var gotState, gotUpdated string
		if err := db.QueryRow(
			`SELECT state, updated_at FROM tick_workers WHERE task_id = ?`, workerID,
		).Scan(&gotState, &gotUpdated); err != nil {
			t.Fatalf("%s: read back worker row: %v", tc.name, err)
		}
		if gotState != tc.row.wantState {
			t.Errorf("%s: state = %q, want %q", tc.name, gotState, tc.row.wantState)
		}
		if tc.row.wantFlip {
			// Flipped rows must carry a restamped updated_at (datetime('now')
			// format), never the seeded stamp — the row must read as touched.
			if gotUpdated == seeded[i] {
				t.Errorf("%s: flipped row kept its seeded updated_at %q — row not restamped", tc.name, gotUpdated)
			}
		}
	}
}

// TestReapOrphanedWorkerRows_KeepsAttribution pins the no-DELETE contract:
// after the pass every seeded row still exists (count unchanged), only its
// state moved.
func TestReapOrphanedWorkerRows_KeepsAttribution(t *testing.T) {
	db := newTestDB(t)
	const project = "sgap1706-keep"
	mustCreateProjectINFRA012(t, db, project)

	loop := NewLoop(db, time.Minute, time.Hour, 10, 100, 5)
	tickID := project + "-t1"
	insertJanitorTick(t, db, project, tickID, "timeout")
	for _, task := range []string{"w1", "w2", "w3"} {
		if _, err := database.CreateTickWorker(context.Background(), db, &database.TickWorker{
			TickID: tickID, TaskID: project + "-" + task, Branch: "wt/" + task,
		}); err != nil {
			t.Fatalf("seed %s: %v", task, err)
		}
	}

	var before int
	if err := db.QueryRow(`SELECT COUNT(*) FROM tick_workers WHERE tick_id = ?`, tickID).Scan(&before); err != nil {
		t.Fatalf("count before: %v", err)
	}
	if got := loop.reapOrphanedWorkerRows(); got != 3 {
		t.Errorf("flipped = %d, want 3", got)
	}
	var after int
	if err := db.QueryRow(`SELECT COUNT(*) FROM tick_workers WHERE tick_id = ?`, tickID).Scan(&after); err != nil {
		t.Fatalf("count after: %v", err)
	}
	if after != before {
		t.Errorf("tick_workers rows for %s: before=%d after=%d — janitor must UPDATE, never DELETE", tickID, before, after)
	}
	var running int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM tick_workers WHERE tick_id = ? AND state = 'running'`, tickID,
	).Scan(&running); err != nil {
		t.Fatalf("count running: %v", err)
	}
	if running != 0 {
		t.Errorf("%d worker row(s) still state='running' under a terminal tick — count of running must be 0 after the janitor", running)
	}
}
