package scheduler

import (
	"database/sql"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/coding-hermes/scheduler/internal/clock"
	"github.com/coding-hermes/scheduler/internal/database"
)

// TickStatus is the lifecycle state.
type TickStatus string

const (
	TickQueued    TickStatus = "queued"
	TickRunning   TickStatus = "running"
	TickCompleted TickStatus = "completed"
	TickFailed    TickStatus = "failed"
	TickTimeout   TickStatus = "timeout"
	// TickDeferred is the terminal status for a tick the harness DEFERRED
	// instead of failing (SCHED-GAP-203). It exists because the gateway-health
	// gate (SCHED-GAP-170) guards only the PRE-SPAWN admission point: a blip
	// that lands INSIDE a spawn — a refused connect, or an SSE stream that
	// ended without a terminal event mid-run — was booked as the lane's own
	// fault, burning a pick, a slot and a cooldown and polluting the project's
	// failure rate (which feeds --auto-disable-failure-rate). LIVE EVIDENCE
	// (scheduler.db, boot 2026-09-19T10:46 → 2026-09-20): 9 of 458 ticks
	// failed this way, every one of them with the error text
	// "gateway unreachable and exec fallback disabled: gateway transient
	// error: …", while /api/v1/status reported the gate armed, healthy and
	// deferrals_total=0.
	//
	// A DEFERRED tick is NOT a failure and NOT a success: it is work the
	// harness declined to charge the lane for. Consequences that are
	// deliberate, not accidental:
	//
	//   - lifecycle.Complete leaves failure_reason EMPTY (that column's
	//     transport-class stamp is only for failed/timeout rows) — the status
	//     itself now names the class, so a second marker would be redundant.
	//   - consecutive_failures is NOT reset and NOT incremented. Failed and
	//     timeout ticks also leave it alone (GAP-133's backoff gate reads it);
	//     only a completed tick clears it.
	//   - The gate's deferrals counter is incremented by the spawn path
	//     (Spawner.transientGatewayDeferral), so the deferral is visible on
	//     the same deferrals_total an operator already reads.
	//
	// Auth rejections are NEVER deferred: a 401/403 is ErrGatewayKeyRejected,
	// terminal by GAP-035, and keeps failing loudly.
	TickDeferred TickStatus = "deferred"
)

// Outcome converts the tick status to the outcome column value.
func (s TickStatus) Outcome() string {
	switch s {
	case TickCompleted:
		return "committed"
	case TickFailed:
		return "failed"
	case TickTimeout:
		return "timeout"
	case TickDeferred:
		// The outcome column must carry the deferral too: an audit reads BOTH
		// columns (status for the lifecycle state, outcome for the terminal
		// verdict), and a deferred row whose outcome fell through to the
		// "dry_run" default would look like a simulated tick.
		return "deferred"
	default:
		return "dry_run"
	}
}

// TickOutcome holds the result of a completed tick.
type TickOutcome struct {
	TickID       string
	Project      string
	SessionID    string
	Started      time.Time
	Finished     time.Time
	Duration     time.Duration
	Status       TickStatus
	ExitCode     int
	Error        string
	TokensIn     int     // simulated or real
	TokensOut    int     // simulated or real
	CostUSD      float64 // simulated or real
	CostSource   string  // ADV-R09/G8: measured | gateway | estimated | simulated
	Commits      int     // simulated or real
	FilesChanged int     // simulated or real
}

// LifecycleTracker manages the tick state machine and outcome persistence.
type LifecycleTracker struct {
	// clk is this component's time seam (SCHED-GAP-169). The zero value
	// reads as the wall clock; NewLoop propagates its own clock here so a
	// test that installs a simulator clock drives the whole component tree,
	// not just evaluate().
	clk clockSeam
	db  *sql.DB
}

// NewLifecycleTracker creates a lifecycle tracker.
func NewLifecycleTracker(db *sql.DB) *LifecycleTracker {
	return &LifecycleTracker{db: db}
}

// Enqueue creates a queued tick entry for the project.
func (lt *LifecycleTracker) Enqueue(project, tickID string) error {
	_, err := lt.db.Exec(`
		INSERT INTO ticks (id, project_name, status, spawned_at, created_at)
		VALUES (?, ?, ?, ?, ?)
	`, tickID, project, TickQueued, lt.clock().Now().Format(time.RFC3339), lt.clock().Now().Format(time.RFC3339))
	if err != nil {
		return fmt.Errorf("enqueue tick %s: %w", tickID, err)
	}
	return nil
}

// StartRunning transitions a tick from queued to running.
func (lt *LifecycleTracker) StartRunning(tickID string) error {
	_, err := lt.db.Exec(`
		UPDATE ticks SET status = ? WHERE id = ?
	`, TickRunning, tickID)
	if err != nil {
		return fmt.Errorf("start tick %s: %w", tickID, err)
	}
	return nil
}

// Complete writes the final outcome of a tick to the database.
// SCHED-GAP-029: also persists commits/files_changed (previously only
// tokens_in/tokens_out/cost_usd were written, leaving commits/files zero).
func (lt *LifecycleTracker) Complete(outcome TickOutcome) error {
	var exitCode interface{}
	if outcome.ExitCode >= 0 {
		exitCode = outcome.ExitCode
	}
	// SCHED-GAP-143: stamp the TRANSPORT-CLASS marker for failed/timeout
	// outcomes, so a tick the harness refused (drain 503, gateway down) is
	// distinguishable in SQL from a project-side failure without re-parsing
	// ticks.error. Empty = not transport-class, the safe default for every
	// legacy row and for successful ticks. Same classifier the spawn path
	// uses to decide whether a failure may touch consecutive_failures.
	//
	// SCHED-GAP-203: TickDeferred is deliberately NOT in this set. A deferred
	// row already carries its classification in the STATUS column ("deferred"
	// is itself the statement that the harness declined to charge the lane),
	// so a transport marker here would be a second, weaker copy of the same
	// fact — and the marker vocabulary exists for rows whose status cannot
	// express it.
	failureReason := ""
	if outcome.Status == TickFailed || outcome.Status == TickTimeout {
		failureReason = failureReasonClass(outcome.Error)
	}
	_, err := lt.db.Exec(`
		UPDATE ticks SET status = ?, outcome = ?, completed_at = ?, exit_code = ?, error = ?, session_id = ?,
			tokens_in = ?, tokens_out = ?, cost_usd = ?, cost_source = ?,
			commits = ?, files_changed = ?, failure_reason = ?
		WHERE id = ?
	`, string(outcome.Status), outcome.Status.Outcome(), outcome.Finished.Format(time.RFC3339), exitCode,
		stringOrNil(outcome.Error), stringOrNil(outcome.SessionID),
		outcome.TokensIn, outcome.TokensOut, outcome.CostUSD, outcome.CostSource,
		outcome.Commits, outcome.FilesChanged, failureReason,
		outcome.TickID)
	if err != nil {
		return fmt.Errorf("complete tick %s: %w", outcome.TickID, err)
	}

	// Update project's last_tick_completed for ALL outcomes (completed, failed, timeout).
	// Previously only updated on TickCompleted — eduos-e2e demonstrated that projects
	// with only failed ticks need cooldown enforcement too, or they flood the scheduler.
	// SCHED-GAP-214: the SAME write now stamps last_tick_status — the terminal
	// status of this most-recent tick (completed | failed | timeout | deferred).
	// The tasks-mode cooldown waiver (SCHED-GAP-124) consults it: after a FAILED
	// tick the waiver stands down and the lane paces on its full effective
	// cooldown, so a gateway outage samples each tasks lane at the lane cadence
	// instead of re-spawning it every eval (the measured 91-ticks-in-90s crier
	// storm). One UPDATE, one row, atomic in the single completion write.
	lastStatus := ""
	switch outcome.Status {
	case TickCompleted:
		lastStatus = database.LastStatusCompleted
	case TickFailed:
		lastStatus = database.LastStatusFailed
	case TickTimeout:
		lastStatus = database.LastStatusTimeout
	case TickDeferred:
		lastStatus = database.LastStatusDeferred
	}
	_, err = lt.db.Exec(`
		UPDATE projects SET last_tick_completed = ?, last_tick_status = ? WHERE name = ?
	`, outcome.Finished.Format(time.RFC3339), lastStatus, outcome.Project)
	if err != nil {
		log.Printf("WARN: failed to update last_tick_completed/last_tick_status for %s: %v", outcome.Project, err)
	}

	// SCHED-GAP-137a: a successful tick clears the consecutive-failure backoff
	// counter. Local-spawn ticks never pass through spawn.go's spawn-time reset
	// (that path only fires on gateway spawn / new spawn), so a project that
	// failed N times in a drain storm kept the residue across successful local
	// completions (observed: cf=91 on bunker/chimera-v2/crier despite recent
	// successful last_tick_completed). Failed and timeout outcomes
	// intentionally leave the counter alone — GAP-133's FailureBackoff gate
	// reads it to hold admission during consecutive failures.
	//
	// SCHED-GAP-203: a DEFERRED outcome leaves it alone too — it neither
	// clears the counter (the project has not demonstrated a good tick) nor
	// increments it (the spawn path never calls noteSpawnFailure for a
	// deferral: a gateway blip is not the lane's failure).
	if outcome.Status == TickCompleted {
		if _, err := lt.db.Exec(`
			UPDATE projects SET consecutive_failures = 0 WHERE name = ?
		`, outcome.Project); err != nil {
			log.Printf("WARN: failed to reset consecutive_failures for %s: %v", outcome.Project, err)
		}
	}

	return nil
}

// ExportSession runs `hermes sessions export` for the given session and parses stats.
func (lt *LifecycleTracker) ExportSession(sessionID string) (SessionStats, error) {
	// Placeholder: actual session export requires CLI parsing.
	return SessionStats{SessionID: sessionID}, nil
}

// SessionStats holds parsed session outcome data.
type SessionStats struct {
	SessionID    string
	Commits      int
	FilesChanged int
	TokensIn     int
	TokensOut    int
	CostUSD      float64
	Outcome      string // committed, dry_run, failed
}

// CleanupStale clears running ticks older than the given duration.
func (lt *LifecycleTracker) CleanupStale(maxAge time.Duration) (int, error) {
	_, n, err := lt.CleanupStaleProjects(maxAge)
	return n, err
}

// CleanupStaleProjects is CleanupStale with SCHED-GAP-186 reporting: it also
// returns the DISTINCT project names whose running rows were flipped terminal,
// so the caller can reconcile the SlotPool claims those rows owned (the UPDATE
// itself never touches the in-process slot pool). The status/completed_at/
// error UPDATE is byte-identical to the original CleanupStale.
func (lt *LifecycleTracker) CleanupStaleProjects(maxAge time.Duration) ([]string, int, error) {
	cutoff := lt.clock().Now().Add(-maxAge)
	rows, err := lt.db.Query(`
		SELECT DISTINCT project_name FROM ticks
		WHERE status = ? AND spawned_at < ?
	`, TickRunning, cutoff.Format(time.RFC3339))
	if err != nil {
		return nil, 0, err
	}
	var projects []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			continue
		}
		projects = append(projects, name)
	}
	rows.Close()
	res, err := lt.db.Exec(`
		UPDATE ticks SET status = ?, completed_at = ?, error = ?
		WHERE status = ? AND spawned_at < ?
	`, TickTimeout, lt.clock().Now().Format(time.RFC3339), "stale — timeout at "+maxAge.String(), TickRunning, cutoff.Format(time.RFC3339))
	if err != nil {
		return nil, 0, err
	}
	n, _ := res.RowsAffected()
	if n > 0 {
		log.Printf("CLEANUP: %d stale running ticks timed out", n)
	}
	return projects, int(n), nil
}

func stringOrNil(s string) interface{} {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	return s
}

// RunningCount returns the number of currently running ticks.
func (lt *LifecycleTracker) RunningCount() int {
	var n int
	lt.db.QueryRow(`SELECT COUNT(*) FROM ticks WHERE status = 'running'`).Scan(&n)
	return n
}

// SetClock installs the clock this LifecycleTracker reads and waits on (SCHED-GAP-169).
// nil keeps the wall clock.
func (lt *LifecycleTracker) SetClock(c clock.Clock) { lt.clk.Set(c) }

// clock returns the component's clock, never nil.
func (lt *LifecycleTracker) clock() clock.Clock { return lt.clk.Get() }
