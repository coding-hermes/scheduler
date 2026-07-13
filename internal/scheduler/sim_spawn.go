package scheduler

import (
	"database/sql"
	"fmt"
	"math/rand"
	"sync"
	"time"
)

// SimSpawner replaces the real Spawner for dry-run/simulation mode.
// It simulates foreman ticks that complete instantly with randomised outcomes.
type SimSpawner struct {
	db      *sql.DB
	success float64 // probability of TickCompleted (0.0-1.0), default 0.85
	mu      sync.Mutex
}

// NewSimSpawner creates a simulated spawner.
func NewSimSpawner(db *sql.DB, successRate float64) *SimSpawner {
	if successRate <= 0 {
		successRate = 0.85
	}
	return &SimSpawner{
		db:      db,
		success: successRate,
	}
}

// Spawn simulates launching a foreman. It creates a tick, marks it running,
// then immediately completes it with a randomised outcome in a goroutine.
func (s *SimSpawner) Spawn(project PackedProject, tickID string) (*SimSpawned, error) {
	now := time.Now()

	// Simulate the tick lifecycle: enqueue → running.
	_, err := s.db.Exec(`
		INSERT INTO ticks (id, project_name, status, spawned_at, urgency, weight_used, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
	`, tickID, project.Name, TickRunning, now.Format(time.RFC3339),
		project.Urgency, project.Weight, now.Format(time.RFC3339))
	if err != nil {
		return nil, fmt.Errorf("sim spawn %s: %w", tickID, err)
	}

	// Simulate random session ID.
	sessionID := fmt.Sprintf("sim-%s-%d", tickID[:8], rand.Intn(99999))
	_, _ = s.db.Exec(`UPDATE ticks SET session_id = ? WHERE id = ?`, sessionID, tickID)

	spawned := &SimSpawned{
		TickID:    tickID,
		Project:   project.Name,
		SessionID: sessionID,
		spawner:   s,
		started:   now,
	}

	// Complete asynchronously after a tiny simulated delay.
	go func() {
		time.Sleep(time.Duration(50+rand.Intn(200)) * time.Millisecond)
		outcome := spawned.Wait()
		s.mu.Lock()
		defer s.mu.Unlock()
		finish := outcome.Finished.Format(time.RFC3339)
		_, err := s.db.Exec(`
			UPDATE ticks SET status = ?, completed_at = ?, exit_code = ?, error = ?, 
				tokens_in = ?, tokens_out = ?, cost_usd = ?, commits = ?, files_changed = ?
			WHERE id = ?
		`, string(outcome.Status), finish, outcome.ExitCode, outcome.Error,
			outcome.TokensIn, outcome.TokensOut, outcome.CostUSD, outcome.Commits, outcome.FilesChanged,
			outcome.TickID)
		if err != nil {
			// Non-fatal in simulation.
		}
	}()

	return spawned, nil
}

// SimSpawned represents a simulated running tick.
type SimSpawned struct {
	TickID    string
	Project   string
	SessionID string
	spawner   *SimSpawner
	started   time.Time
}

// Wait simulates the foreman running and returns a randomised outcome.
func (s *SimSpawned) Wait() TickOutcome {
	finished := time.Now()
	duration := finished.Sub(s.started)

	outcome := TickOutcome{
		TickID:    s.TickID,
		Project:   s.Project,
		SessionID: s.SessionID,
		Started:   s.started,
		Finished:  finished,
		Duration:  duration,
	}

	roll := rand.Float64()
	if roll < s.spawner.success {
		// Completed with some work.
		outcome.Status = TickCompleted
		outcome.ExitCode = 0
		outcome.TokensIn = 2000 + rand.Intn(8000)
		outcome.TokensOut = 500 + rand.Intn(3000)
		outcome.CostUSD = float64(outcome.TokensIn)*0.00001 + float64(outcome.TokensOut)*0.00003
		outcome.Commits = 1 + rand.Intn(3)
		outcome.FilesChanged = 1 + rand.Intn(8)
	} else if roll < s.spawner.success+0.10 {
		// Timeout.
		outcome.Status = TickTimeout
		outcome.Error = "simulated timeout after 30m"
	} else {
		// Failed.
		outcome.Status = TickFailed
		outcome.ExitCode = 1
		outcome.Error = "simulated build failure"
	}

	return outcome
}
