package scheduler

import (
	"bufio"
	"database/sql"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// Cost estimation constants for real ticks where session export is unavailable.
// These are conservative estimates based on typical foreman tick usage.
const (
	estTokensIn    = 8000     // estimated input tokens per tick
	estTokensOut   = 2000     // estimated output tokens per tick
	estCostPerIn   = 0.000015 // deepseek-v4-pro input $/token
	estCostPerOut  = 0.00006  // deepseek-v4-pro output $/token
	estCostPerTick = float64(estTokensIn)*estCostPerIn + float64(estTokensOut)*estCostPerOut
)

// estimateTickCost returns estimated token counts and cost for a real tick.
// Real session export (hermes sessions export) is a future task; for now we
// use fixed estimates so cost aggregation works from day one.
func estimateTickCost() (tokensIn, tokensOut int, costUSD float64) {
	return estTokensIn, estTokensOut, estCostPerTick
}

// Spawner launches coding-hermes foreman processes.
type Spawner struct {
	db            *sql.DB
	maxConcurrent int
	active        map[string]*exec.Cmd // tickID -> running process
	mu            sync.Mutex
	timeout       time.Duration
	model         string
	provider      string
	skills        string
}

// NewSpawner creates a spawner with the given concurrency limit and defaults.
func NewSpawner(db *sql.DB, maxConcurrent int) *Spawner {
	return &Spawner{
		db:            db,
		maxConcurrent: maxConcurrent,
		active:        make(map[string]*exec.Cmd),
		timeout:       30 * time.Minute,
		model:         "deepseek-v4-pro",
		provider:      "deepseek-foreman",
		skills:        "coding-hermes-foreman,coding-hermes-cron,hilo-usage,gitreins",
	}
}

// ActiveCount returns the number of currently running spawns.
func (s *Spawner) ActiveCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.active)
}

// canSpawn checks concurrency limits.
func (s *Spawner) canSpawn() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.active) < s.maxConcurrent
}

// Spawn launches a foreman for the given project and tick ID.
// Returns an error only if the process fails to start.
// The spawned process is tracked internally and reaped by the lifecycle tracker.
func (s *Spawner) Spawn(project PackedProject, tickID string) (*SpawnedTick, error) {
	if !s.canSpawn() {
		return nil, fmt.Errorf("max concurrency %d reached", s.maxConcurrent)
	}

	var cmd *exec.Cmd

	if project.Command != "" {
		// Custom command.
		if strings.Contains(project.Command, "bash -c") {
			// Shell one-liner — pass the script string directly to bash -c.
			script := strings.TrimPrefix(project.Command, "bash -c ")
			script = strings.TrimSpace(script)
			// Strip surrounding quotes if present.
			script = strings.Trim(script, "'\"")
			cmd = exec.Command("bash", "-c", script)
		} else {
			parts := splitCommand(project.Command)
			cmd = exec.Command(parts[0], parts[1:]...)
		}
		cmd.Dir = project.Workdir
	} else {
		model := s.model
		if project.Model != "" {
			model = project.Model
		}
		provider := s.provider
		if project.Provider != "" {
			provider = project.Provider
		}

		prompt := fmt.Sprintf(
			"[Scheduler tick: %s] "+
				"Load skills coding-hermes-foreman, coding-hermes-cron, hilo-usage, gitreins. "+
				"Read .coding-hermes/tasks.md. Execute ONE foreman tick per the foreman skill. "+
				"Workdir: %s. Report result.",
			tickID, project.Workdir,
		)

		args := []string{
			"chat", "-q", prompt,
			"-m", model,
			"--provider", provider,
			"-s", "coding-hermes-foreman",
			"-s", "coding-hermes-cron",
			"-s", "hilo-usage",
			"-s", "gitreins",
			"--ignore-rules", "-Q",
		}

		cmd = exec.Command("hermes", args...)
		cmd.Dir = project.Workdir
		cmd.Env = append(os.Environ(),
			"CODING_HERMES_TICK="+tickID,
			"CODING_HERMES_SOURCE=scheduler",
			"CODING_HERMES_PROJECT="+project.Name,
		)
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start process: %w", err)
	}

	s.mu.Lock()
	s.active[tickID] = cmd
	s.mu.Unlock()

	st := &SpawnedTick{
		TickID:  tickID,
		Project: project.Name,
		PID:     cmd.Process.Pid,
		Started: time.Now(),
		cmd:     cmd,
		stdout:  stdout,
		stderr:  stderr,
		spawner: s,
	}

	// Parse session ID from stdout and persist it.
	go func() {
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			line := scanner.Text()
			if strings.HasPrefix(line, "session_id:") {
				id := strings.TrimSpace(strings.TrimPrefix(line, "session_id:"))
				st.mu.Lock()
				st.SessionID = id
				st.mu.Unlock()
				// Persist session_id to the database.
				if _, err := s.db.Exec(`UPDATE ticks SET session_id = ? WHERE id = ?`, id, tickID); err != nil {
					log.Printf("ERROR persisting session_id for %s: %v", tickID, err)
				}
				break
			}
		}
	}()

	// Update tick to running.
	_, err = s.db.Exec(`
		UPDATE ticks SET status = 'running', spawned_at = ?
		WHERE id = ?
	`, st.Started.Format(time.RFC3339), tickID)
	if err != nil {
		log.Printf("ERROR updating tick %s to running: %v", tickID, err)
	}

	log.Printf("SPAWN: %s tick=%s pid=%d workdir=%s", project.Name, tickID, st.PID, project.Workdir)
	return st, nil
}

// SpawnedTick represents a running foreman process.
type SpawnedTick struct {
	TickID    string
	Project   string
	PID       int
	Started   time.Time
	SessionID string
	cmd       *exec.Cmd
	stdout    interface{ Close() error }
	stderr    interface{ Close() error }
	spawner   *Spawner
	mu        sync.Mutex
}

// Wait blocks until the process exits and returns the outcome.
func (st *SpawnedTick) Wait() TickOutcome {
	defer func() {
		st.spawner.mu.Lock()
		delete(st.spawner.active, st.TickID)
		st.spawner.mu.Unlock()
	}()

	timer := time.AfterFunc(st.spawner.timeout, func() {
		if st.cmd.Process != nil {
			_ = st.cmd.Process.Kill()
		}
	})
	defer timer.Stop()

	err := st.cmd.Wait()
	finished := time.Now()

	outcome := TickOutcome{
		TickID:    st.TickID,
		Project:   st.Project,
		SessionID: st.SessionID,
		Started:   st.Started,
		Finished:  finished,
	}

	if err != nil {
		if strings.Contains(err.Error(), "signal: killed") || strings.Contains(err.Error(), "killed") {
			outcome.Status = TickTimeout
		} else {
			outcome.Status = TickFailed
			outcome.Error = err.Error()
		}
	} else {
		outcome.Status = TickCompleted
	}

	outcome.ExitCode = st.cmd.ProcessState.ExitCode()
	outcome.Duration = finished.Sub(st.Started)

	// Cost estimation: real session export (hermes sessions export) is a future
	// task. For now we populate estimated token counts and cost so that cost
	// aggregation works from day one. Only estimate on completed ticks — failed
	// or timed-out ticks consumed fewer tokens (process exited early).
	if outcome.Status == TickCompleted {
		tin, tout, cost := estimateTickCost()
		outcome.TokensIn = tin
		outcome.TokensOut = tout
		outcome.CostUSD = cost
	}

	log.Printf("TICK: %s %s → %s (%v)", st.Project, st.TickID, outcome.Status, outcome.Duration.Round(time.Second))
	return outcome
}

func splitCommand(cmd string) []string {
	// Simple split for shell commands. Does basic quote handling.
	var parts []string
	var current string
	inQuote := false
	for _, c := range cmd {
		switch c {
		case '"':
			inQuote = !inQuote
		case ' ':
			if inQuote {
				current += string(c)
			} else if current != "" {
				parts = append(parts, current)
				current = ""
			}
		default:
			current += string(c)
		}
	}
	if current != "" {
		parts = append(parts, current)
	}
	return parts
}
