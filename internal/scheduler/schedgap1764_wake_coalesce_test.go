package scheduler

// SCHED-GAP-1764 — RED/GREEN battery for wake coalescing.
//
// The row: a lane that concludes dry_run/no_changes must not be re-ticked by
// a board write for its coalesce window (default 1x its cooldown_s). These
// tests pin the four arms the acceptance names, plus the predicate boundary
// (a dry_run for any OTHER reason is not coalesced) and the window surfaces
// (default = 1x cooldown, fleet-wide override, per-lane override).
//
// Every arm drives BoardWakeWatcher.wake — the ONLY place the coalescer is
// consulted (asserted in arm (c)) — with the injected clock so windows are
// testable without sleeping.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/coding-hermes/scheduler/internal/clock"
	"github.com/coding-hermes/scheduler/internal/database"
)

// gap1764Now is the fixed instant every arm runs at. Whole-second so RFC3339
// round-trips are exact.
var gap1764Now = time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)

// gap1764Tick inserts one tick row for project near completedAt.
func gap1764Tick(t *testing.T, db *sql.DB, project, id, status, outcome, errText string, completedAt time.Time) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO ticks
		(id, project_name, status, outcome, spawned_at, completed_at, exit_code, error, created_at)
		VALUES (?, ?, ?, ?, ?, ?, 0, ?, ?)`,
		id, project, status, outcome,
		completedAt.Add(-time.Minute).Format(time.RFC3339),
		completedAt.Format(time.RFC3339),
		errText,
		completedAt.Format(time.RFC3339)); err != nil {
		t.Fatalf("insert tick %s: %v", id, err)
	}
}

// gap1764CoalesceEvents returns message+details of every wake_coalesce event
// row in write order.
func gap1764CoalesceEvents(t *testing.T, db *sql.DB) []string {
	t.Helper()
	rows, err := db.Query(`SELECT message || ' | ' || COALESCE(details, '') FROM events
		WHERE component = 'wake_coalesce' ORDER BY id`)
	if err != nil {
		t.Fatalf("query wake_coalesce events: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatalf("scan wake_coalesce event: %v", err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iter wake_coalesce events: %v", err)
	}
	return out
}

// gap1764Target builds the wake target for the newWakeFix project.
func gap1764Target(fx *wakeFix) boardWakeTarget {
	return boardWakeTarget{project: "wakeproj", workdir: fx.workdir, boardPath: fx.boardPath}
}

// gap1764Source reads a package source file (tests run in the package dir), so
// arm (c) can assert the coalescer has exactly one call site.
func gap1764Source(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(name)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(b)
}

// Arm (a) — the acceptance's core: last concluded tick dry_run/no_changes, a
// board write inside the window => NO second spawn (forceEvaluate not called)
// and exactly one suppression line naming lane, last tick id, window, and the
// remaining suppression time.
func TestSCHEDGAP1764_DryRunCoalescesBoardWake(t *testing.T) {
	fx := newWakeFix(t) // cooldown_s = 3600 => default window 1h
	fx.w.SetClock(clock.NewFixed(gap1764Now))
	gap1764Tick(t, fx.db, "wakeproj", "tick-1781", "completed",
		string(database.OutcomeDryRun), boardWakeNoChangesError, gap1764Now.Add(-10*time.Minute))

	fx.w.wake(gap1764Target(fx))

	if got := fx.evals(); got != 0 {
		t.Fatalf("ForceEvaluate calls = %d, want 0 — a dry_run/no_changes lane inside its coalesce window must not be re-ticked by a board write", got)
	}
	ev := gap1764CoalesceEvents(t, fx.db)
	if len(ev) != 1 {
		t.Fatalf("wake_coalesce events = %d, want exactly 1: %v", len(ev), ev)
	}
	line := ev[0]
	for _, want := range []string{
		"WAKE-COALESCE: wakeproj",
		"last_dry_run_tick=tick-1781",
		"window=1h0m0s",
		"remaining=50m0s",
		`"project":"wakeproj"`,
		`"window_s":3600`,
	} {
		if !strings.Contains(line, want) {
			t.Errorf("suppression line missing %q:\n%s", want, line)
		}
	}
	var remaining float64
	if _, err := fmt.Sscanf(line[strings.Index(line, `"remaining_s":`)+len(`"remaining_s":`):], "%g", &remaining); err != nil {
		t.Fatalf("parse remaining_s from %s: %v", line, err)
	}
	if remaining != 3000 {
		t.Errorf("remaining_s = %v, want 3000 (a 60m window minus the 10m since the tick closed)", remaining)
	}
}

// Arm (b) — after the window elapses the same board write spawns again.
func TestSCHEDGAP1764_WindowElapsedSpawnsAgain(t *testing.T) {
	fx := newWakeFix(t)
	fx.w.SetClock(clock.NewFixed(gap1764Now))
	gap1764Tick(t, fx.db, "wakeproj", "tick-1782", "completed",
		string(database.OutcomeDryRun), boardWakeNoChangesError, gap1764Now.Add(-10*time.Minute))

	fx.w.wake(gap1764Target(fx))
	if got := fx.evals(); got != 0 {
		t.Fatalf("in-window ForceEvaluate calls = %d, want 0", got)
	}

	// The window started 10m before gap1764Now and runs 60m, so it expires at
	// +50m. Advance past it with the SAME watcher (the memo must expire).
	fx.w.SetClock(clock.NewFixed(gap1764Now.Add(51 * time.Minute)))
	fx.w.wake(gap1764Target(fx))
	if got := fx.evals(); got != 1 {
		t.Fatalf("post-window ForceEvaluate calls = %d, want 1 — after the coalesce window a board write must spawn normally", got)
	}
	if ev := gap1764CoalesceEvents(t, fx.db); len(ev) != 1 {
		t.Fatalf("wake_coalesce events = %d, want 1 (the post-window wake must NOT be suppressed): %v", len(ev), ev)
	}
}

// Arm (c) — manual/API-triggered spawns and owner actions bypass suppression.
// Two facts together: the coalescer is consulted in exactly ONE place (the
// watcher's wake), and the evaluation pass such a trigger runs still selects a
// lane that has a live window.
func TestSCHEDGAP1764_ManualTriggerBypassesCoalesce(t *testing.T) {
	// (i) structural: exactly one call site of .coalesceSkip( in the package's
	// non-test sources.
	sites := 0
	for _, f := range []string{"board_wake.go", "board_wake_coalesce.go"} {
		sites += strings.Count(gap1764Source(t, f), ".coalesceSkip(")
	}
	if sites != 1 {
		t.Fatalf(".coalesceSkip( call sites = %d in board_wake{,_coalesce}.go, want 1 (only BoardWakeWatcher.wake) — a second call site would suppress a non-watcher trigger", sites)
	}

	// (ii) behavioral: a live window suppresses the watcher's wake, and the
	// evaluation pass a manual trigger runs still selects the lane.
	wd, boardPath := gap1660BoardDir(t, `{"id":"W-1","status":"pending"}`)
	db := newTestDB(t)
	insertGap1660Project(t, db, "coalesce-manual", wd, 60, database.AdmissionModeTasks)
	// Last concluded tick: dry_run/no_changes 24h ago; per-lane window 48h, so
	// the window is LIVE while the lane's own admission facts stay coherent
	// (last_tick_completed 24h ago => admissible by pacing/cooldown).
	gap1764Tick(t, db, "coalesce-manual", "tick-manual-1", "completed",
		string(database.OutcomeDryRun), boardWakeNoChangesError, gap1764Now.Add(-24*time.Hour))
	gap1660SetLastCompleted(t, db, "coalesce-manual", 24*time.Hour)

	var evalCount int64
	w := NewBoardWakeWatcher(db, func() { evalCount++ })
	w.SetClock(clock.NewFixed(gap1764Now))
	w.SetWakeCoalesceWindowFor("coalesce-manual", 48*time.Hour)

	w.wake(boardWakeTarget{project: "coalesce-manual", workdir: wd, boardPath: boardPath})
	if evalCount != 0 {
		t.Fatalf("watcher ForceEvaluate calls = %d, want 0 (window live)", evalCount)
	}

	l := NewLoop(db, 30*time.Second, 24*time.Hour, 10, 100, 4)
	l.SetSimulation(1.0)
	l.SetClock(clock.NewFixed(gap1764Now))
	l.evaluate() // the pass a manual /api/v1/wakes or owner spawn runs
	if got := simSelectedProjects(t, db); len(got) != 1 || got[0] != "coalesce-manual" {
		t.Fatalf("manual evaluation selected %v, want [coalesce-manual] — manual/API triggers MUST bypass wake coalescing", got)
	}
}

// A newer conclusion clears suppression: the coalesce predicate is about the
// lane's LAST concluded tick, so once that tick commits, a board write inside
// the old dry_run window must spawn again — recorded state can never mask
// fresher evidence.
func TestSCHEDGAP1764_NewerConclusionClearsSuppression(t *testing.T) {
	fx := newWakeFix(t)
	fx.w.SetClock(clock.NewFixed(gap1764Now))
	// Dry run closes; a board write is suppressed and records the window.
	first := gap1764Now.Add(-10 * time.Minute)
	gap1764Tick(t, fx.db, "wakeproj", "tick-dry", "completed",
		string(database.OutcomeDryRun), boardWakeNoChangesError, first)
	fx.w.wake(gap1764Target(fx))
	if got := fx.evals(); got != 0 {
		t.Fatalf("in-window ForceEvaluate calls = %d, want 0", got)
	}

	// The lane ticks again and COMMITS, one minute later (still inside the
	// dry_run window that started at first).
	gap1764Tick(t, fx.db, "wakeproj", "tick-commit", "completed",
		string(database.OutcomeCommitted), "", first.Add(time.Minute))
	fx.w.SetClock(clock.NewFixed(gap1764Now.Add(time.Minute)))

	fx.w.wake(gap1764Target(fx))
	if got := fx.evals(); got != 1 {
		t.Fatalf("ForceEvaluate calls after a committed tick = %d, want 1 — suppression must not outlive the conclusion it was computed from", got)
	}
	if ev := gap1764CoalesceEvents(t, fx.db); len(ev) != 1 {
		t.Fatalf("wake_coalesce events = %d, want 1 (only the dry_run wake): %v", len(ev), ev)
	}
}

// Arm (d) — an unreadable last-tick read fails OPEN: no suppression, the wake
// proceeds, and no suppression line is emitted.
func TestSCHEDGAP1764_LastTickReadErrorFailsOpen(t *testing.T) {
	fx := newWakeFix(t)
	fx.w.SetClock(clock.NewFixed(gap1764Now))
	gap1764Tick(t, fx.db, "wakeproj", "tick-fo", "completed",
		string(database.OutcomeDryRun), boardWakeNoChangesError, gap1764Now.Add(-time.Minute))
	fx.w.lastTick = func(context.Context, *sql.DB, string) (boardWakeLastTick, error) {
		return boardWakeLastTick{}, errors.New("simulated ticks read failure")
	}

	fx.w.wake(gap1764Target(fx))

	if got := fx.evals(); got != 1 {
		t.Fatalf("ForceEvaluate calls = %d, want 1 — an unreadable last-tick read must fail OPEN", got)
	}
	if ev := gap1764CoalesceEvents(t, fx.db); len(ev) != 0 {
		t.Fatalf("wake_coalesce events = %d, want 0 on the fail-open arm: %v", len(ev), ev)
	}
}

// Predicate boundary — only dry_run + no_changes coalesces, and only while it
// is inside the window. Every other concluded shape spawns; a lane with no
// concluded tick spawns; a still-running tick is not a conclusion.
func TestSCHEDGAP1764_PredicateBoundary(t *testing.T) {
	cases := []struct {
		name string
		seed func(t *testing.T, db *sql.DB)
		// suppress is the expected verdict: true = the wake must be dropped
		// (zero ForceEvaluate calls, exactly one WAKE-COALESCE line), false =
		// the wake proceeds (one call, no line).
		suppress bool
	}{
		{
			name: "dry_run/result_missing is a harness fault, not a no-op",
			seed: func(t *testing.T, db *sql.DB) {
				gap1764Tick(t, db, "wakeproj", "tick-rm", "completed",
					string(database.OutcomeDryRun), "result_missing", gap1764Now.Add(-time.Minute))
			},
		},
		{
			name: "no_changes string with a non-dry_run outcome",
			seed: func(t *testing.T, db *sql.DB) {
				gap1764Tick(t, db, "wakeproj", "tick-ok", "completed",
					string(database.OutcomeCommitted), boardWakeNoChangesError, gap1764Now.Add(-time.Minute))
			},
		},
		{
			name: "failed tick",
			seed: func(t *testing.T, db *sql.DB) {
				gap1764Tick(t, db, "wakeproj", "tick-fail", "failed",
					string(database.OutcomeFailed), "exit_1", gap1764Now.Add(-time.Minute))
			},
		},
		{
			name: "still-running tick is not a conclusion",
			seed: func(t *testing.T, db *sql.DB) {
				// Coalescible columns, non-concluded status: the status
				// filter must exclude it, never the outcome.
				gap1764Tick(t, db, "wakeproj", "tick-run", "running",
					string(database.OutcomeDryRun), boardWakeNoChangesError, gap1764Now.Add(-time.Minute))
			},
		},
		{
			name: "no tick at all",
			seed: func(t *testing.T, db *sql.DB) {},
		},
		{
			name: "dry_run/no_changes older than the window",
			seed: func(t *testing.T, db *sql.DB) {
				gap1764Tick(t, db, "wakeproj", "tick-stale", "completed",
					string(database.OutcomeDryRun), boardWakeNoChangesError, gap1764Now.Add(-2*time.Hour))
			},
		},
		{
			name: "dry_run/no_changes inside the window",
			seed: func(t *testing.T, db *sql.DB) {
				gap1764Tick(t, db, "wakeproj", "tick-fresh", "completed",
					string(database.OutcomeDryRun), boardWakeNoChangesError, gap1764Now.Add(-time.Minute))
			},
			suppress: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fx := newWakeFix(t)
			fx.w.SetClock(clock.NewFixed(gap1764Now))
			tc.seed(t, fx.db)
			fx.w.wake(gap1764Target(fx))
			wantEvals := int64(1)
			wantEvents := 0
			if tc.suppress {
				wantEvals = 0
				wantEvents = 1
			}
			if got := fx.evals(); got != wantEvals {
				t.Fatalf("ForceEvaluate calls = %d, want %d", got, wantEvals)
			}
			if ev := gap1764CoalesceEvents(t, fx.db); len(ev) != wantEvents {
				t.Fatalf("wake_coalesce events = %d, want %d: %v", len(ev), wantEvents, ev)
			}
		})
	}
}

// Window resolution — default 1x cooldown_s, fleet-wide override, per-lane
// override (explicit 0 = never coalesce that lane).
func TestSCHEDGAP1764_WindowResolution(t *testing.T) {
	fx := newWakeFix(t) // cooldown_s 3600
	fx.w.SetClock(clock.NewFixed(gap1764Now))

	// Default 1x cooldown_s: a 90m-old dry_run tick is OUTSIDE the 60m window.
	gap1764Tick(t, fx.db, "wakeproj", "tick-90m", "completed",
		string(database.OutcomeDryRun), boardWakeNoChangesError, gap1764Now.Add(-90*time.Minute))
	fx.w.wake(gap1764Target(fx))
	if got := fx.evals(); got != 1 {
		t.Fatalf("default window: ForceEvaluate calls = %d, want 1 (a 90m-old tick is outside the 1h default)", got)
	}

	// Fleet-wide override 2h => the same tick is now INSIDE the window.
	fx.w.SetWakeCoalesceWindow(2 * time.Hour)
	fx.w.wake(gap1764Target(fx))
	if got := fx.evals(); got != 1 {
		t.Fatalf("fleet-wide override: ForceEvaluate calls = %d, want 1 (suppressed)", got)
	}
	if ev := gap1764CoalesceEvents(t, fx.db); len(ev) != 1 || !strings.Contains(ev[0], "window=2h0m0s") {
		t.Fatalf("fleet-wide override events = %v, want one naming window=2h0m0s", ev)
	}

	// Per-lane override wins over fleet-wide: 0 disables coalescing.
	fx.w.SetWakeCoalesceWindowFor("wakeproj", 0)
	fx.w.wake(gap1764Target(fx))
	if got := fx.evals(); got != 2 {
		t.Fatalf("per-lane zero override: ForceEvaluate calls = %d, want 2 — an explicit 0 must disable coalescing for the lane", got)
	}

	// An override on ANOTHER lane never leaks to this one.
	fx.w.SetWakeCoalesceWindowFor("other-lane", 5*time.Minute)
	fx.w.wake(gap1764Target(fx))
	if got := fx.evals(); got != 3 {
		t.Fatalf("per-lane override leaked: ForceEvaluate calls = %d, want 3", got)
	}
}

// The rendered line carries the four facts the row requires — lane, last
// dry_run tick id, window, remaining — in one grep-stable line.
func TestSCHEDGAP1764_LineFormat(t *testing.T) {
	sk := boardWakeCoalesceSkip{LastTickID: "tick-42", Window: 3600 * time.Second, Remaining: 2940 * time.Second}
	line := sk.line("voice-mode-foreman")
	want := "WAKE-COALESCE: voice-mode-foreman last_dry_run_tick=tick-42 window=1h0m0s remaining=49m0s — board-wake re-tick suppressed"
	if line != want {
		t.Fatalf("line = %q, want %q", line, want)
	}
}
