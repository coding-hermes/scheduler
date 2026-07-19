package scheduler

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
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
	foremanHome   string         // HERMES_HOME for foreman config
	gateway       *GatewayClient // HTTP API client (nil = use exec.Command)

	// Prometheus-style spawn counters since last restart.
	spawnCountHTTP int64
	spawnCountExec int64
}

// NewSpawner creates a spawner with the given concurrency limit and defaults.
func NewSpawner(db *sql.DB, maxConcurrent int, timeout ...time.Duration) *Spawner {
	to := 30 * time.Minute
	if len(timeout) > 0 {
		to = timeout[0]
	}
	return &Spawner{
		db:            db,
		maxConcurrent: maxConcurrent,
		active:        make(map[string]*exec.Cmd),
		timeout:       to,
		model:         "deepseek-v4-pro",
		provider:      "deepseek-foreman",
		skills:        "coding-hermes-foreman,coding-hermes-cron,hilo-usage,gitreins",
		foremanHome:   os.ExpandEnv("$HOME/.hermes/foreman"),
	}
}

// SetForemanHome overrides the default HERMES_HOME for foreman sessions.
func (s *Spawner) SetForemanHome(path string) {
	s.foremanHome = path
}

// RunningSet returns the set of project names that currently have a spawned
// process (in-memory). This is more accurate than the DB query because spawns
// haven't been committed to the DB yet when the packer queries.
func (s *Spawner) RunningSet() map[string]bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	set := make(map[string]bool, len(s.active))
	for tickID := range s.active {
		// Extract project name from tick ID: "project-YYYY-MM-DD-HH-MM-SS"
		idx := strings.LastIndex(tickID, "-202")
		if idx > 0 {
			set[tickID[:idx]] = true
		}
	}
	return set
}

// SetGatewayClient configures the HTTP API client. If set, Spawn() prefers
// HTTP over process spawning. Pass nil to disable and fall back to exec.Command.
func (s *Spawner) SetGatewayClient(client *GatewayClient) {
	s.gateway = client
}

// GatewayAvailable returns true if the gateway client is configured and reachable.
func (s *Spawner) GatewayAvailable() bool {
	if s.gateway == nil {
		return false
	}
	return s.gateway.Ping(context.Background()) == nil
}

// SpawnMethodCounts returns HTTP and exec spawn counts since last restart.
func (s *Spawner) SpawnMethodCounts() (httpCount, execCount int64) {
	return atomic.LoadInt64(&s.spawnCountHTTP), atomic.LoadInt64(&s.spawnCountExec)
}

// ActiveCount returns the number of currently running spawns.
func (s *Spawner) ActiveCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.active)
}

// workerDefaults returns a prompt suffix with the project's preferred worker
// model and provider. Empty string when neither is configured. Includes
// fallback instructions so the foreman can switch models freely.
func workerDefaults(project PackedProject) string {
	if project.WorkerModel == "" && project.WorkerProvider == "" {
		return ""
	}
	m := project.WorkerModel
	p := project.WorkerProvider
	if m == "" {
		m = "(no default)"
	}
	if p == "" {
		p = "(no default)"
	}
	return fmt.Sprintf(
		"Worker default: use model %s with provider %s if available. "+
			"Feel free to use a different model if this one is unavailable or rate-limited. ",
		m, p,
	)
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

		prompt := fmt.Sprintf(
			"[Scheduler tick: %s] "+
				"Load skills coding-hermes-foreman, coding-hermes-cron, hilo-usage, gitreins. "+
				"Read .coding-hermes/tasks.md. Execute ONE foreman tick per the foreman skill. "+
				"Workdir: %s. "+
				"IMPORTANT: You are a FOREMAN, not a worker. Browser/interactive work belongs in workers (delegate). "+
				"Format your final output as clean, well-structured markdown with tables and sections. "+
				"%s"+
				"Report result.",
			tickID, project.Workdir,
			workerDefaults(project),
		)

		// Try HTTP gateway spawn first (zero process overhead).
		if s.gateway != nil {
			ctx, cancel := context.WithTimeout(context.Background(), s.timeout)
			resp, gwErr := s.gateway.SendResponse(ctx, prompt, model)
			cancel()
			if gwErr == nil && resp != nil {
				atomic.AddInt64(&s.spawnCountHTTP, 1)
				text := resp.ExtractText()
				now := time.Now()
				_, _ = s.db.Exec(`UPDATE ticks SET status='completed', outcome='ok', spawned_at=?, finished_at=?, output=?, session_id='gateway' WHERE id=?`,
					now.Format(time.RFC3339), now.Format(time.RFC3339), text, tickID)

				log.Printf("GATEWAY: %s tick=%s tokens=%d/%d",
					project.Name, tickID, resp.Usage.InputTokens, resp.Usage.OutputTokens)
				return &SpawnedTick{
					TickID:     tickID,
					Project:    project.Name,
					SessionID:  "gateway",
					Started:    now,
					Deliver:    project.Deliver,
					Output:     *bytes.NewBufferString(text),
					spawner:    s,
					completed:  true,
					completeAt: now,
				}, nil
			}
			log.Printf("GATEWAY FAIL: %s tick=%s error=%v — falling back to exec.Command", project.Name, tickID, gwErr)
		}

		atomic.AddInt64(&s.spawnCountExec, 1)

		provider := s.provider
		if project.Provider != "" {
			provider = project.Provider
		}

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
			"HERMES_HOME="+s.foremanHome,
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
		Deliver: project.Deliver,
		cmd:     cmd,
		stdout:  stdout,
		stderr:  stderr,
		spawner: s,
	}

	// Tee stdout: scanner reads session_id from one side, buffer captures full output.
	teeReader := io.TeeReader(stdout, &st.Output)

	// Parse session ID from stdout and persist it. The scanner goroutine must
	// exit when the process exits or times out so it cannot leak.
	scanCtx, scanCancel := context.WithTimeout(context.Background(), s.timeout)
	st.scanCancel = scanCancel

	// Close stdout when context expires — unblocks scanner.Scan().
	go func() {
		<-scanCtx.Done()
		_ = stdout.Close()
	}()

	go func() {
		defer scanCancel()
		scanner := bufio.NewScanner(teeReader)
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
				return
			}
		}
		if err := scanner.Err(); err != nil {
			// Expected on timeout (pipe closed) or process exit — not a leak.
			if !errors.Is(err, io.EOF) {
				log.Printf("WARN: stdout scanner error for tick %s: %v", tickID, err)
			}
		}
	}()

	// Update tick to running with PID for zombie detection.
	_, err = s.db.Exec(`
		UPDATE ticks SET status = 'running', spawned_at = ?, pid = ?
		WHERE id = ?
	`, st.Started.Format(time.RFC3339), st.PID, tickID)
	if err != nil {
		log.Printf("ERROR updating tick %s to running: %v", tickID, err)
	}

	log.Printf("SPAWN: %s tick=%s pid=%d workdir=%s", project.Name, tickID, st.PID, project.Workdir)
	return st, nil
}

// SpawnedTick represents a running foreman process.
type SpawnedTick struct {
	TickID     string
	Project    string
	PID        int
	Started    time.Time
	SessionID  string
	Output     bytes.Buffer // full stdout for delivery after completion
	Deliver    string       // delivery target (telegram:chat_id:thread_id)
	cmd        *exec.Cmd
	stdout     interface{ Close() error }
	stderr     interface{ Close() error }
	spawner    *Spawner
	scanCancel context.CancelFunc
	mu         sync.Mutex

	// completed is true for gateway-spawned ticks that finished in Spawn().
	completed  bool
	completeAt time.Time
}

// Wait blocks until the process exits and returns the outcome.
// For gateway-completed ticks (HTTP spawn), returns immediately.
func (st *SpawnedTick) Wait() TickOutcome {
	defer func() {
		st.spawner.mu.Lock()
		delete(st.spawner.active, st.TickID)
		st.spawner.mu.Unlock()
	}()

	// Gateway-spawned ticks are already complete — return immediately.
	if st.completed {
		return TickOutcome{
			TickID:    st.TickID,
			Project:   st.Project,
			SessionID: st.SessionID,
			Started:   st.Started,
			Finished:  st.completeAt,
			Status:    TickCompleted,
			Duration:  st.completeAt.Sub(st.Started),
		}
	}

	defer st.closePipes()
	if st.scanCancel != nil {
		defer st.scanCancel()
	}

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

func (st *SpawnedTick) closePipes() {
	if st.stdout != nil {
		_ = st.stdout.Close()
	}
	if st.stderr != nil {
		_ = st.stderr.Close()
	}
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
