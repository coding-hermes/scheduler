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
	"syscall"
	"time"
)

// Cost estimation constants for real ticks where session export is unavailable.
// These are conservative estimates based on typical foreman tick usage.
const (
	estTokensIn    = 8000     // estimated input tokens per tick
	estTokensOut   = 2000     // estimated output tokens per tick
	estCostPerIn   = 0.000002 // foreman-model input $/token (set via env)
	estCostPerOut  = 0.000008 // foreman-model output $/token (set via env)
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
	db             *sql.DB
	maxConcurrent  int
	active         map[string]*exec.Cmd // tickID -> running process
	mu             sync.Mutex
	timeout        time.Duration
	model          string
	provider       string
	skills         string
	foremanHome    string         // HERMES_HOME for foreman config
	gateway        *GatewayClient // HTTP API client (nil = use exec.Command)
	noExecFallback bool           // disable exec.Command fallback on gateway failure

	// events is an optional EventLogger. When set, terminal gateway-key
	// rejections (GAP-035) emit a HIGH event so a key regression is
	// immediately visible instead of producing thousands of silent failed
	// ticks. Nil is safe — tests and tooling construct Spawners standalone.
	events *EventLogger

	// heartbeatInterval is the cadence at which a running tick's row gets its
	// heartbeat_at refreshed (S-GAP-003). The gateway zombie reaper treats a
	// pid=0 row whose heartbeat is stale as orphaned. Tests shrink this.
	heartbeatInterval time.Duration

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
		db:                db,
		maxConcurrent:     maxConcurrent,
		active:            make(map[string]*exec.Cmd),
		timeout:           to,
		model:             getEnvOrDefault("SCHEDULER_FOREMAN_MODEL", "your-model-name"),
		provider:          getEnvOrDefault("SCHEDULER_FOREMAN_PROVIDER", "your-provider-name"),
		skills:            getEnvOrDefault("SCHEDULER_FOREMAN_SKILLS", "coding-hermes-foreman"),
		foremanHome:       os.ExpandEnv("$HOME/.hermes/foreman"),
		heartbeatInterval: 5 * time.Minute,
	}
}

// getEnvOrDefault returns the value of envVar if set, otherwise fallback.
func getEnvOrDefault(envVar, fallback string) string {
	if v := os.Getenv(envVar); v != "" {
		return v
	}
	return fallback
}

// noteSpawnFailure increments the project's consecutive spawn-failure counter,
// which drives the exponential selection backoff (S-GAP-001). Best-effort:
// a DB error here must never mask the real spawn error.
func (s *Spawner) noteSpawnFailure(project string) {
	if _, err := s.db.Exec(
		`UPDATE projects SET consecutive_failures = consecutive_failures + 1 WHERE name = ?`,
		project); err != nil {
		log.Printf("WARN: consecutive_failures increment for %s: %v", project, err)
	}
}

// gatewayKeyRejected is the terminal classification path for GAP-035: a
// gateway 401/403 on either the pre-dispatch probe or SendResponse. It
// (1) logs a distinct GATEWAY KEY REJECTED line, (2) bumps the selection
// backoff so the broken project is not re-picked into a retry flood, and
// (3) emits an immediate HIGH event so a key regression is visible in the
// events table/dashboard instead of vanishing into thousands of generic
// failed ticks. The returned error wraps ErrGatewayKeyRejected so callers
// can classify it with errors.Is.
func (s *Spawner) gatewayKeyRejected(project, tickID string, err error) error {
	log.Printf("GATEWAY KEY REJECTED: %s tick=%s error=%v — failing fast, no dispatch, no exec fallback", project, tickID, err)
	s.noteSpawnFailure(project)
	if s.events != nil {
		s.events.Emit(context.Background(), SeverityHigh, "spawn", "gateway key rejected", map[string]any{
			"project": project,
			"tick_id": tickID,
			"error":   err.Error(),
		})
	}
	return fmt.Errorf("gateway key rejected for %s: %w", project, err)
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

// SetNoExecFallback disables the exec.Command fallback when gateway spawns fail.
func (s *Spawner) SetNoExecFallback(v bool) {
	s.noExecFallback = v
}

// SetEventLogger wires an optional EventLogger for HIGH events on terminal
// gateway-key rejections (GAP-035). Nil (the default) disables emission.
func (s *Spawner) SetEventLogger(el *EventLogger) {
	s.events = el
}

// gatewayKeyProbeTimeout bounds the pre-dispatch per-project key validation
// (GAP-035). The probe is a cheap GET /health — it must never add meaningful
// latency to a spawn, and a slow probe must not stall the tick.
const gatewayKeyProbeTimeout = 5 * time.Second

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

// startHeartbeat launches a goroutine that refreshes the tick row's
// heartbeat_at every heartbeatInterval until the returned channel is closed.
// S-GAP-003: heartbeat_at is the liveness signal for pid=0 (gateway) ticks —
// both zombie reapers treat a heartbeat older than gatewayZombieMaxAge as an
// orphaned tick. The goroutine never outlives its request/process: callers
// close the stop channel as soon as the spawn returns. A DB error is logged
// and the goroutine keeps beating (a transient error must not kill liveness).
func (s *Spawner) startHeartbeat(tickID string) chan<- struct{} {
	stop := make(chan struct{})
	go func() {
		ticker := time.NewTicker(s.heartbeatInterval)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
			}
			// A stop that raced with this tick wins — never write after the
			// spawn returned. At most one in-flight write can land after
			// close, and the next loop iteration exits immediately.
			select {
			case <-stop:
				return
			default:
			}
			if _, err := s.db.Exec(`UPDATE ticks SET heartbeat_at = ? WHERE id = ?`,
				time.Now().Format(time.RFC3339), tickID); err != nil {
				log.Printf("WARN: heartbeat refresh for tick %s: %v", tickID, err)
			}
		}
	}()
	return stop
}

// WorkerDefaults returns a prompt suffix with the project's preferred worker
// model and provider. Empty string when neither is configured. Includes
// fallback instructions so the foreman can switch models freely.
func WorkerDefaults(project PackedProject) string {
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
		"Worker model/provider (AUTHORITATIVE, do not change): use model %s with provider %s. "+
			"The board's model column is an advisory routing suggestion only; the configured worker_model here takes precedence and MUST be used for every worker you dispatch. "+
			"Only switch models if this one errors with an actual unavailable/rate-limited failure — do not second-guess based on the board's recommendation. ",
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
				"Load skills coding-hermes-foreman, coding-hermes-board, coding-hermes-model-router, coding-hermes-never-done, coding-hermes-specs, coding-hermes-testing, coding-hermes-middle-out, systematic-debugging, trust-but-verify, reality-validation, github-pr-workflow, github-repo-management, claude-design, popular-web-designs, hilo-usage, gitreins, off-by-one. "+
				"Read the project board: .coding-hermes/board/tasks.jsonl if present (JSONL-canonical), else .coding-hermes/tasks.md. Execute ONE foreman tick per the foreman skill. "+
				"Workdir: %s. "+
				"OFF-BY-ONE (pre-solve lab, localhost:8766): BEFORE debugging any error or designing a fix from scratch, discover a pre-verified answer via `curl -s -X POST http://localhost:8766/api/v1/problems/discover -H 'Content-Type: application/json' -d '{\"problem_class\":\"<class>\"}'` or grep the flat corpus data/answers/ (per the off-by-one skill). If you had to debug something non-trivial, submit it (`cadence: post-debug`) so future ticks hit a cached answer. "+
				"IMPORTANT — worker dispatch: You are the FOREMAN. You pick ONE board task, then dispatch a WORKER to implement it. "+
				"Do NOT implement complex tasks yourself. To dispatch a worker, run a BACKGROUND process via your terminal tool: "+
				"`hermes chat -q \"<task brief from the board, plus files-to-modify and acceptance criteria>\" -m <worker_model> --provider <worker_provider> -s coding-hermes-worker --ignore-rules -Q` "+
				"(terminal background=true). The worker shares this same workdir, so it edits files and commits directly. "+
				"Then poll the background process until it exits, verify build/lint/test and the commit landed, update the board, and report. "+
				"MANDATORY PUSH AFTER EVERY COMMIT — do not skip: after ANY commit (worker or yours), run `git push origin <branch>` (or `git push`) and verify `git fetch origin && git rev-list --count origin/<branch>..HEAD` is 0. A tick that ends with unpushed commits is NOT complete. Never rely on the worker having pushed — verify the remote HEAD yourself. On non-fast-forward push, `git pull --rebase`, re-run the gate, push. "+
				"Only implement trivial one-file changes yourself; anything multi-file or architectural goes to a worker. "+
				"Worker model/provider: %s. "+
				"MANDATORY GitReins lifecycle — do not skip: (1) BEFORE any implementation, run `gitreins task create <TASK-ID> \"<title>\" \"<criterion>\"` then `gitreins task start <TASK-ID>` for the board task you picked. "+
				"(2) AFTER the worker commits the work (verify the commit exists in git log), ALWAYS run `gitreins task complete <TASK-ID>` — this fires the Tier 2 LLM judge and writes verdict.json. "+
				"NEVER end a tick without running `gitreins task complete` for the picked task — even if the tick is near its timeout, complete the gitreins task FIRST, then update the board. "+
				"(3) Then delete the gitreins task with `gitreins task delete <TASK-ID>` to keep tasks.yaml clean (optional — the fleet default keeps completed tasks for audit). "+
				"If the worker committed but you missed the gitreins lifecycle, run `gitreins task complete` on the committed work before finishing. "+
				"MANDATORY CI-health check — do not skip: run `gh run list --repo <org>/<repo> --limit 3 --json status,conclusion,displayTitle,headBranch,createdAt` (derive org/repo from `git remote -v` — the on-disk folder name may not match the GitHub org). If ANY recent run shows conclusion=failure that YOU did not just create, file a board task for the broken CI (e.g. INT-CI-<n> '<what failed>') before ending the tick, so it does not rot. Report CI health (green or the failure you flagged) in your output. "+
				"Format your final output as clean, well-structured markdown with tables and sections. "+
				"Report result.",
			tickID, project.Workdir,
			WorkerDefaults(project),
		)

		// GAP-048: when the gateway was unreachable at startup and
		// noExecFallback is set, the spawner has no HTTP client (gateway is
		// nil) and must NOT silently degrade to exec.Command. Without this
		// guard the nil-gateway path falls straight through to the exec spawn
		// block below, bypassing the noExecFallback check that only lives
		// inside the gateway-fail branch — the daemon exec-spawns forever
		// even though the flag says to stay idle. The background reconnector
		// will SetGatewayClient when the gateway recovers, re-engaging HTTP.
		if s.gateway == nil && s.noExecFallback {
			log.Printf("SKIPPED: %s tick=%s no gateway client and exec fallback disabled — staying idle", project.Name, tickID)
			s.noteSpawnFailure(project.Name)
			if s.events != nil {
				s.events.Emit(context.Background(), SeverityHigh, "spawn",
					"gateway unavailable and exec fallback disabled — tick dropped", map[string]any{
						"project":          project.Name,
						"tick_id":          tickID,
						"gateway":          "nil",
						"no_exec_fallback": true,
					})
			}
			return nil, fmt.Errorf("no gateway client and exec fallback disabled for %s", project.Name)
		}

		// Try HTTP gateway spawn first (zero process overhead).
		if s.gateway != nil {
			reqStart := time.Now() // SCHED-GAP-029: capture before SendResponse for git window

			ctx, cancel := context.WithTimeout(context.Background(), s.timeout)

			// GAP-035: validate a per-project gateway key BEFORE dispatch.
			// The 2026-08-04 outage had the fleet send revoked fk-* keys
			// blindly — every spawn burned a full gateway cycle, failed, and
			// retried next eval: 8208+ silent failures. A cheap authenticated
			// probe turns that into a fail-fast terminal error with a HIGH
			// event. Non-auth probe errors (network blip, slow gateway) are
			// non-terminal: dispatch proceeds and the SendResponse 401
			// classification backstops.
			if project.GatewayKey != "" {
				vctx, vcancel := context.WithTimeout(ctx, gatewayKeyProbeTimeout)
				verr := s.gateway.ValidateKey(vctx, project.GatewayKey)
				vcancel()
				if errors.Is(verr, ErrGatewayKeyRejected) {
					cancel()
					return nil, s.gatewayKeyRejected(project.Name, tickID, verr)
				}
				if verr != nil {
					log.Printf("GATEWAY KEY PROBE: %s tick=%s error=%v — proceeding to dispatch", project.Name, tickID, verr)
				}
			}

			// S-GAP-003: the gateway call below is synchronous and blocks for
			// the whole tick (up to --tick-timeout). Persist a placeholder
			// session id + first heartbeat BEFORE it starts, so the tick row
			// never sits 'running' with session_id NULL (the zombie
			// signature). The placeholder is the tick's own id — unique and
			// self-describing; the real gateway session id replaces it on
			// success below.
			if _, err := s.db.Exec(`UPDATE ticks SET session_id = ?, heartbeat_at = ? WHERE id = ?`,
				tickID, time.Now().Format(time.RFC3339), tickID); err != nil {
				log.Printf("WARN: placeholder session_id/heartbeat for %s: %v", tickID, err)
			}
			stopHeartbeat := s.startHeartbeat(tickID)

			// Per-foreman gateway key: project.GatewayKey when set, else the
			// daemon's shared --gateway-key (Bane 2026-07-31).
			resp, gwErr := func() (*Response, error) {
				// The heartbeat goroutine must never outlive the request —
				// stop it on every return path (success AND failure).
				defer close(stopHeartbeat)
				defer cancel()
				return s.gateway.SendResponse(ctx, prompt, model, project.GatewayKey)
			}()
			if gwErr == nil && resp != nil {
				atomic.AddInt64(&s.spawnCountHTTP, 1)
				text := resp.ExtractText()
				now := time.Now()
				// NOTE: tick completion is handled by slot_pool → lifecycle.Complete
				// (correct columns + outcome CHECK). The legacy direct UPDATE here was
				// removed in GAP-002 — it referenced non-existent columns
				// (finished_at, output) and outcome='ok' violated the ticks CHECK, so
				// it silently no-oped on every run.
				// S-GAP-001: a successful spawn also resets the consecutive-failure
				// backoff counter.
				_, _ = s.db.Exec(`UPDATE projects SET last_tick_started = ?, consecutive_failures = 0 WHERE name = ?`,
					now.Format(time.RFC3339), project.Name)

				// S-GAP-003: persist the REAL gateway session id (resp.ID);
				// fall back to the placeholder tick id when the gateway
				// returns none, so the row never goes back to NULL.
				sessionID := resp.ID
				if sessionID == "" {
					sessionID = tickID
				}
				log.Printf("GATEWAY: %s tick=%s tokens=%d/%d",
					project.Name, tickID, resp.Usage.InputTokens, resp.Usage.OutputTokens)
				return &SpawnedTick{
					TickID:     tickID,
					Project:    project.Name,
					SessionID:  sessionID,
					Started:    reqStart, // SCHED-GAP-029: use request start, not completion
					Deliver:    project.Deliver,
					Output:     *bytes.NewBufferString(text),
					spawner:    s,
					completed:  true,
					completeAt: now,
					// SCHED-GAP-029: carry real usage + context for outcome metrics.
					usage:    resp.Usage,
					model:    model,
					workdir:  project.Workdir,
					reqStart: reqStart,
				}, nil
			}
			log.Printf("GATEWAY FAIL: %s tick=%s error=%v — falling back to exec.Command", project.Name, tickID, gwErr)
			// GAP-035: an AUTH rejection is terminal. Falling back to exec
			// would silently mask a key regression and keep flooding the
			// fleet with disguised failures — fail fast with a classified
			// error + HIGH event instead, regardless of the no-exec-fallback
			// flag (which only governs transient gateway errors).
			if errors.Is(gwErr, ErrGatewayKeyRejected) {
				return nil, s.gatewayKeyRejected(project.Name, tickID, gwErr)
			}
			if s.noExecFallback {
				log.Printf("SKIPPED: %s tick=%s exec fallback disabled, dropping tick", project.Name, tickID)
				s.noteSpawnFailure(project.Name)
				return nil, fmt.Errorf("gateway unreachable and exec fallback disabled: %w", gwErr)
			}
		}

		provider := s.provider
		if project.Provider != "" {
			provider = project.Provider
		}

		args := []string{
			"chat", "-q", prompt,
			"-m", model,
			"--provider", provider,
			"-s", "coding-hermes-foreman",
			"-s", "coding-hermes-board",
			"-s", "coding-hermes-model-router",
			"-s", "coding-hermes-never-done",
			"-s", "coding-hermes-specs",
			"-s", "coding-hermes-testing",
			"-s", "coding-hermes-middle-out",
			"-s", "systematic-debugging",
			"-s", "trust-but-verify",
			"-s", "reality-validation",
			"-s", "github-pr-workflow",
			"-s", "github-repo-management",
			"-s", "claude-design",
			"-s", "popular-web-designs",
			"-s", "hilo-usage",
			"-s", "gitreins",
			"-s", "off-by-one",
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

	// Count the spawn method for /api/v1/health (spawns_exec). Custom-command
	// spawns (tests/probes) are exec spawns too — the HTTP path returned
	// above, so reaching here always means an exec spawn (GAP-049).
	atomic.AddInt64(&s.spawnCountExec, 1)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		s.noteSpawnFailure(project.Name)
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		s.noteSpawnFailure(project.Name)
		return nil, fmt.Errorf("stderr pipe: %w", err)
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if err := cmd.Start(); err != nil {
		s.noteSpawnFailure(project.Name)
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
		// SCHED-GAP-029: carry workdir for potential future metric enrichment.
		workdir: project.Workdir,
	}
	// S-GAP-003: keep the tick row's heartbeat fresh for the life of the
	// process; Wait() stops the goroutine. The gateway branch above runs
	// the same heartbeat around its blocking request.
	st.stopHeartbeat = s.startHeartbeat(tickID)

	// Snapshot the repo at spawn so the completion path can count commits and
	// files the foreman added during this tick.
	st.preHead, st.preCommits = gitBaseline(project.Workdir)

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
		defer func() {
			if r := recover(); r != nil {
				log.Printf("ERROR: stdout scanner panic for tick %s: %v", tickID, r)
			}
		}()
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
				continue
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
	// S-GAP-003: also stamp session_id (placeholder = tick id — the stdout
	// scanner above overwrites it with the real parsed session id when a
	// "session_id:" line appears) and the first heartbeat, so no running row
	// ever has NULL session_id and the reapers always have a liveness signal.
	_, err = s.db.Exec(`
		UPDATE ticks SET status = 'running', spawned_at = ?, pid = ?, session_id = ?, heartbeat_at = ?
		WHERE id = ?
	`, st.Started.Format(time.RFC3339), st.PID, tickID, st.Started.Format(time.RFC3339), tickID)
	if err != nil {
		log.Printf("ERROR updating tick %s to running: %v", tickID, err)
	}
	// Also set last_tick_started on the project so cooldown tracking works.
	// S-GAP-001: a successful spawn resets the consecutive-failure backoff
	// counter (atomically with the last_tick_started write).
	_, _ = s.db.Exec(`UPDATE projects SET last_tick_started = ?, consecutive_failures = 0 WHERE name = ?`,
		st.Started.Format(time.RFC3339), project.Name)

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

	// stopHeartbeat is closed by Wait() to stop the tick-row heartbeat
	// goroutine started in Spawn() (S-GAP-003). Nil for gateway spawns —
	// their heartbeat is stopped inside Spawn() when the request returns.
	stopHeartbeat chan<- struct{}

	// Git baseline captured at spawn (exec path only) so Wait() can measure
	// the commits/files the foreman produced during this tick. preCommits < 0
	// means the workdir was not a usable git repo at spawn.
	preHead    string
	preCommits int

	// completed is true for gateway-spawned ticks that finished in Spawn().
	completed  bool
	completeAt time.Time

	// SCHED-GAP-029: real usage + context for outcome metrics.
	usage    Usage     // gateway response token usage (gateway path only)
	model    string    // model used for this tick (for cost lookup)
	workdir  string    // project workdir for git commit/file counting
	reqStart time.Time // request start time (before SendResponse) for git window
}

// Wait blocks until the process exits and returns the outcome.
// For gateway-completed ticks (HTTP spawn), returns immediately.
func (st *SpawnedTick) Wait() TickOutcome {
	defer func() {
		// S-GAP-003: stop the tick-row heartbeat goroutine started in Spawn()
		// — it must never outlive the process. Nil for gateway spawns (their
		// heartbeat was stopped inside Spawn() when the request returned).
		if st.stopHeartbeat != nil {
			close(st.stopHeartbeat)
			st.stopHeartbeat = nil
		}
		st.spawner.mu.Lock()
		delete(st.spawner.active, st.TickID)
		st.spawner.mu.Unlock()
	}()

	// Gateway-spawned ticks are already complete — return immediately.
	// SCHED-GAP-029: populate real tokens/cost/commits/files from gateway
	// usage + git. Previously every gateway tick returned zero metrics.
	if st.completed {
		tokensIn := st.usage.InputTokens
		tokensOut := st.usage.OutputTokens
		cost := computeCostUSD(st.model, tokensIn, tokensOut)
		commits, files := countGitChanges(st.workdir, st.reqStart, st.completeAt)
		log.Printf("TICK: %s %s → %s (%v) %s",
			st.Project, st.TickID, TickCompleted,
			st.completeAt.Sub(st.Started).Round(time.Second),
			formatCostSummary(st.model, tokensIn, tokensOut, cost, commits, files))
		return TickOutcome{
			TickID:       st.TickID,
			Project:      st.Project,
			SessionID:    st.SessionID,
			Started:      st.Started,
			Finished:     st.completeAt,
			Status:       TickCompleted,
			Duration:     st.completeAt.Sub(st.Started),
			TokensIn:     tokensIn,
			TokensOut:    tokensOut,
			CostUSD:      cost,
			Commits:      commits,
			FilesChanged: files,
		}
	}

	defer st.closePipes()
	if st.scanCancel != nil {
		defer st.scanCancel()
	}

	timer := time.AfterFunc(st.spawner.timeout, func() {
		if st.cmd.Process != nil {
			// Each scheduler-owned worker has its own process group. Killing the
			// group prevents shells, Hermes workers, and test runners from
			// surviving after the tick is marked timed out.
			_ = syscall.Kill(-st.cmd.Process.Pid, syscall.SIGKILL)
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

	// Cost: prefer REAL per-tick cost from the foreman's Hermes state.db
	// (session_model_usage.estimated/actual_cost_usd overlapping this tick's
	// window). Falls back to the flat estimate when telemetry is unavailable.
	// Populated on completed AND timed-out ticks: a timeout runs the full
	// window (killed at the cap), so it consumes a full tick's tokens and has a
	// real cost. Failed ticks that exit early consumed fewer and stay near 0.
	if outcome.Status == TickCompleted || outcome.Status == TickTimeout {
		workdir := ""
		if st.cmd != nil && st.cmd.Dir != "" {
			workdir = st.cmd.Dir
		}
		cost, isReal := resolveRealTickCost(st.spawner.foremanHome, workdir, st.Project, st.Started, finished)
		outcome.CostUSD = cost
		if !isReal {
			// Still record the estimated token counts so aggregation works
			// even when telemetry is missing.
			tin, tout, _ := estimateTickCost()
			outcome.TokensIn = tin
			outcome.TokensOut = tout
		} else {
			outcome.TokensIn = 0
			outcome.TokensOut = 0
		}
		// Measure real git work the foreman produced this tick (exec path only —
		// gateway spawns have no process/repo baseline). Best-effort: a non-git
		// or unreadable workdir leaves commits/files at 0.
		if st.preCommits >= 0 && st.cmd != nil && st.cmd.Dir != "" {
			outcome.Commits, outcome.FilesChanged = gitWorkDelta(st.cmd.Dir, st.preHead, st.preCommits)
		}
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
