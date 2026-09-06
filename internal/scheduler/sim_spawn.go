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
	success float64
	// idleRate is the fraction of COMPLETED ticks that produce zero commits
	// (simulated idle foreman). 0 = legacy behavior (every success "commits"),
	// which never exercises the adaptive-cooldown slow-down path. Set via
	// SetIdleRate (wired to --sim-idle) to simulate quiet boards.
	idleRate float64
	mu       sync.Mutex
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

// SetIdleRate sets the fraction of completed ticks that carry zero commits
// (and zero file changes), simulating idle foremen for adaptive-cooldown
// dry-runs. Must be called before Spawn.
func (s *SimSpawner) SetIdleRate(rate float64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if rate < 0 {
		rate = 0
	}
	if rate > 1 {
		rate = 1
	}
	s.idleRate = rate
}

// Spawn simulates launching a foreman. It creates a tick, marks it running,
// then immediately completes it with a randomised outcome in a goroutine.
func (s *SimSpawner) Spawn(project PackedProject, tickID string) (*SimSpawned, error) {
	now := time.Now()

	_, err := s.db.Exec(`
		INSERT INTO ticks (id, project_name, status, spawned_at, urgency, weight_used, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
	`, tickID, project.Name, TickRunning, now.Format(time.RFC3339),
		project.Urgency, project.Weight, now.Format(time.RFC3339))
	if err != nil {
		return nil, fmt.Errorf("sim spawn %s: %w", tickID, err)
	}

	sessionID := fmt.Sprintf("sim-%s-%d", tickID[:8], rand.Intn(99999))
	_, _ = s.db.Exec(`UPDATE ticks SET session_id = ? WHERE id = ?`, sessionID, tickID)

	spawned := &SimSpawned{
		TickID:    tickID,
		Project:   project.Name,
		SessionID: sessionID,
		spawner:   s,
		started:   now,
	}

	go func() {
		time.Sleep(time.Duration(50+rand.Intn(200)) * time.Millisecond)
		outcome := spawned.Wait()
		s.mu.Lock()
		defer s.mu.Unlock()
		finish := outcome.Finished.Format(time.RFC3339)
		s.db.Exec(`
			UPDATE ticks SET status = ?, completed_at = ?, exit_code = ?, error = ?,
				tokens_in = ?, tokens_out = ?, cost_usd = ?, commits = ?, files_changed = ?
			WHERE id = ?
		`, string(outcome.Status), finish, outcome.ExitCode, outcome.Error,
			outcome.TokensIn, outcome.TokensOut, outcome.CostUSD, outcome.Commits, outcome.FilesChanged,
			outcome.TickID)
		// Update last_tick_completed for ALL outcomes so cooldown check catches failed projects.
		s.db.Exec(`UPDATE projects SET last_tick_completed = ? WHERE name = ?`, finish, outcome.Project)
		// Feed the outcome through the SAME post-tick hook the real spawner
		// uses (adaptive cooldown first, legacy autoSlowdown as fallback) so
		// dry-runs exercise the speed-control engine identically to live
		// ticks (Bane 2026-09-06). PackedProject carries the project's
		// configured workdir (sim fixture gives each project a dummy board).
		if s.db != nil {
			if !adaptiveCooldown(s.db, outcome.Project, project.Workdir, outcome) {
				autoSlowdown(s.db, outcome.Project, nil)
			}
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
		outcome.Status = TickCompleted
		outcome.ExitCode = 0
		outcome.TokensIn = 2000 + rand.Intn(8000)
		outcome.TokensOut = 500 + rand.Intn(3000)
		outcome.CostUSD = float64(outcome.TokensIn)*0.00001 + float64(outcome.TokensOut)*0.00003
		// idleRate split: some "completed" ticks are idle foremen (zero
		// commits, zero files) so the adaptive-cooldown slow-down path can
		// be exercised in dry-runs (Bane 2026-09-06). Legacy default 0 keeps
		// the old always-progress behavior.
		if rand.Float64() < s.spawner.idleRate {
			outcome.Commits = 0
			outcome.FilesChanged = 0
		} else {
			outcome.Commits = 1 + rand.Intn(3)
			outcome.FilesChanged = 1 + rand.Intn(8)
		}
	} else if roll < s.spawner.success+0.10 {
		outcome.Status = TickTimeout
		outcome.Error = "simulated timeout after 30m"
	} else {
		outcome.Status = TickFailed
		outcome.ExitCode = 1
		outcome.Error = "simulated build failure"
	}

	return outcome
}
