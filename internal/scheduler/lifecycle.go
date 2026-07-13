package scheduler

import (
	"database/sql"
	"fmt"
	"log"
	"strings"
	"time"
)

// TickStatus is the lifecycle state.
type TickStatus string

const (
	TickQueued    TickStatus = "queued"
	TickRunning   TickStatus = "running"
	TickCompleted TickStatus = "completed"
	TickFailed    TickStatus = "failed"
	TickTimeout   TickStatus = "timeout"
)

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
	Commits      int     // simulated or real
	FilesChanged int     // simulated or real
}

// LifecycleTracker manages the tick state machine and outcome persistence.
type LifecycleTracker struct {
	db *sql.DB
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
	`, tickID, project, TickQueued, time.Now().Format(time.RFC3339), time.Now().Format(time.RFC3339))
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
func (lt *LifecycleTracker) Complete(outcome TickOutcome) error {
	var exitCode interface{}
	if outcome.ExitCode >= 0 {
		exitCode = outcome.ExitCode
	}
	_, err := lt.db.Exec(`
		UPDATE ticks SET status = ?, completed_at = ?, exit_code = ?, error = ?, session_id = ?
		WHERE id = ?
	`, string(outcome.Status), outcome.Finished.Format(time.RFC3339), exitCode, stringOrNil(outcome.Error), stringOrNil(outcome.SessionID), outcome.TickID)
	if err != nil {
		return fmt.Errorf("complete tick %s: %w", outcome.TickID, err)
	}

	// If completed successfully, update project's last_tick_completed.
	if outcome.Status == TickCompleted {
		_, err = lt.db.Exec(`
			UPDATE projects SET last_tick_completed = ? WHERE name = ?
		`, outcome.Finished.Format(time.RFC3339), outcome.Project)
		if err != nil {
			log.Printf("WARN: failed to update last_tick_completed for %s: %v", outcome.Project, err)
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
	cutoff := time.Now().Add(-maxAge)
	res, err := lt.db.Exec(`
		UPDATE ticks SET status = ?, completed_at = ?, error = ?
		WHERE status = ? AND spawned_at < ?
	`, TickTimeout, time.Now().Format(time.RFC3339), "stale — timeout at "+maxAge.String(), TickRunning, cutoff.Format(time.RFC3339))
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	if n > 0 {
		log.Printf("CLEANUP: %d stale running ticks timed out", n)
	}
	return int(n), nil
}

func stringOrNil(s string) interface{} {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	return s
}
