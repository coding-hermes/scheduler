package lanehook

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/coding-hermes/scheduler/internal/clock"
)

// ErrHookFailed is returned by Pre when a closed step failed. The caller turns
// it into ticks.outcome=failed with failure_reason=hook_failed:<id>.
type ErrHookFailed struct {
	HookID string
	Code   int
	Reason string
}

func (e *ErrHookFailed) Error() string {
	return fmt.Sprintf("hook %s failed (exit %d): %s", e.HookID, e.Code, e.Reason)
}

// ErrBudgetExhausted is returned when the PRE list cannot finish inside its
// total budget (S13 §5).
var ErrBudgetExhausted = errors.New("hook budget exhausted")

// ExecRunner executes hook steps as child processes in their own process group.
// A nil *sql.DB disables receipts; a nil Emitter disables events. Both are
// legal (tests use one or the other).
type ExecRunner struct {
	db   *sql.DB
	emit Emitter
	// clk is the time choke point (internal/clock). Every read goes through it
	// so a sim clock can drive hook timeouts and receipts deterministically;
	// nil falls back to clock.Real(), which is byte-identical to stdlib.
	clk clock.Clock
}

// NewExecRunner builds a runner. db may be nil (no receipts), emit may be nil.
// Time defaults to the real clock; use SetClock to install a simulated one.
func NewExecRunner(db *sql.DB, emit Emitter) *ExecRunner {
	return &ExecRunner{db: db, emit: emit, clk: clock.Real()}
}

// SetClock installs the clock used for timeouts and receipt timestamps.
func (r *ExecRunner) SetClock(c clock.Clock) {
	if c != nil {
		r.clk = c
	}
}

// now is the one place this package reads the wall clock.
func (r *ExecRunner) now() time.Time {
	if r.clk == nil {
		return clock.Real().Now()
	}
	return r.clk.Now()
}

func (r *ExecRunner) since(t time.Time) time.Duration {
	if r.clk == nil {
		return clock.Real().Since(t)
	}
	return r.clk.Since(t)
}

func (r *ExecRunner) after(d time.Duration) <-chan time.Time {
	if r.clk == nil {
		return clock.Real().After(d)
	}
	return r.clk.After(d)
}

// Pre runs the PRE list. A non-nil error means the tick must not spawn.
func (r *ExecRunner) Pre(ctx context.Context, env Env, plan Plan) error {
	if len(plan.Pre) == 0 {
		return nil
	}
	budget := int64(0)
	for _, s := range plan.Pre {
		budget += int64(s.Timeout())
	}
	deadline := r.now().Add(time.Duration(budget))
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	for i, step := range plan.Pre {
		seq := i + 1
		if r.now().After(deadline) {
			r.markSkipped(ctx, env, plan.Pre[i:], seq, "budget_exhausted")
			r.event(ctx, "HIGH", "hook.pre.budget", map[string]any{
				"tick_id": env.TickID, "lane": env.Lane, "skipped": len(plan.Pre) - i,
			})
			return ErrBudgetExhausted
		}
		res := r.run(ctx, env, step, seq, PhasePre)
		if res.failed() && step.OnFailureOrDefault() == FailClosed {
			r.event(ctx, "CRITICAL", "hook.pre.veto", map[string]any{
				"tick_id": env.TickID, "lane": env.Lane, "hook_id": step.ID,
				"exit_code": res.code, "attempts": res.attempts,
			})
			return &ErrHookFailed{HookID: step.ID, Code: res.code, Reason: res.state}
		}
	}
	return nil
}

// Post runs the POST list. A closed failure stops the REMAINING steps; the tick
// outcome is never changed (S13 §2.3.8).
func (r *ExecRunner) Post(ctx context.Context, env Env, plan Plan) {
	if len(plan.Post) == 0 {
		return
	}
	for i, step := range plan.Post {
		seq := i + 1
		res := r.run(ctx, env, step, seq, PhasePost)
		if res.failed() && step.OnFailureOrDefault() == FailClosed {
			if i+1 < len(plan.Post) {
				r.markSkipped(ctx, env, plan.Post[i+1:], seq+1, "aborted_by:"+step.ID)
			}
			r.event(ctx, "HIGH", "hook.post.chain_aborted", map[string]any{
				"tick_id": env.TickID, "lane": env.Lane, "hook_id": step.ID,
				"skipped": len(plan.Post) - i - 1,
			})
			return
		}
	}
}

type stepResult struct {
	state    string // ok | failed | timeout
	code     int
	attempts int
}

func (r stepResult) failed() bool { return r.state != "ok" }

// run executes one step with its retry policy.
func (r *ExecRunner) run(ctx context.Context, env Env, step Step, seq int, phase Phase) stepResult {
	policy := step.OnFailureOrDefault()
	maxAttempts := 1
	if policy == FailRetry {
		maxAttempts = step.Retries
		if maxAttempts < 1 {
			maxAttempts = 1
		}
	}
	var res stepResult
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		res = r.runOnce(ctx, env, step, seq, phase, attempt)
		if res.state == "ok" {
			return res
		}
		if attempt < maxAttempts && step.RetryDelayS > 0 {
			select {
			case <-ctx.Done():
				return res
			case <-r.after(time.Duration(step.RetryDelayS) * time.Second):
			}
		}
	}
	return res
}

// runOnce executes one attempt and records its receipt.
func (r *ExecRunner) runOnce(ctx context.Context, env Env, step Step, seq int, phase Phase, attempt int) stepResult {
	stepEnv := env
	stepEnv.Phase = phase
	stepEnv.HookID = step.ID

	cctx, cancel := context.WithTimeout(ctx, step.Timeout())
	defer cancel()

	cmd := exec.CommandContext(cctx, step.Command, Expand(step.Args, stepEnv)...)
	cmd.Dir = env.Workdir
	cmd.Env = append(os.Environ(), envVars(stepEnv)...)
	// Own process group: a timeout must kill the children too (S13 §2.3.2).
	// The syscall level is platform-specific (INT-CI-4184): runner_unix.go
	// sets Setpgid and does the negative-pid group kill; runner_windows.go
	// carries the documented windows degradation (no POSIX process groups).
	cmd.SysProcAttr = processGroupAttrs()
	// Go's default Cancel kills only the leader; kill the whole group. A
	// WaitDelay bounds how long a grandchild holding a pipe can block us.
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return killProcessGroup(cmd.Process, os.Kill)
	}
	cmd.WaitDelay = 5 * time.Second

	var buf safeBuffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf

	r.event(ctx, "INFO", "hook."+string(phase)+".start", map[string]any{
		"tick_id": env.TickID, "lane": env.Lane, "hook_id": step.ID,
		"level": string(step.Level), "priority": step.Priority, "seq": seq,
		"timeout_s": int(step.Timeout().Seconds()), "attempt": attempt,
	})

	started := r.now()
	runID := r.insertReceipt(ctx, env, step, seq, phase, attempt, started, RenderArgv(step, stepEnv))
	err := cmd.Run()
	dur := r.since(started)

	res := stepResult{state: "ok", attempts: attempt}
	reason := ""
	switch {
	case err == nil:
	case cctx.Err() == context.DeadlineExceeded:
		res.state, res.code, reason = "timeout", -1, "timeout"
	case ctx.Err() != nil:
		res.state, res.code, reason = "failed", -1, "canceled"
	default:
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			res.code = ee.ExitCode()
		} else {
			res.code = -2 // could not start
		}
		res.state, reason = "failed", "exit"
	}

	r.finishReceipt(ctx, runID, res, dur, buf.String())

	if res.state == "ok" {
		r.event(ctx, "INFO", "hook."+string(phase)+".ok", map[string]any{
			"tick_id": env.TickID, "hook_id": step.ID, "duration_ms": dur.Milliseconds(),
		})
	} else {
		r.event(ctx, "HIGH", "hook."+string(phase)+"."+res.state, map[string]any{
			"tick_id": env.TickID, "hook_id": step.ID, "exit_code": res.code,
			"attempts": attempt, "reason": reason, "output_head": head(buf.String(), 400),
		})
	}
	return res
}

// markSkipped writes one receipt per skipped step so the plan is reconstructable.
func (r *ExecRunner) markSkipped(ctx context.Context, env Env, steps []Step, firstSeq int, reason string) {
	for i, s := range steps {
		r.insertSkippedReceipt(ctx, env, s, firstSeq+i, reason)
	}
}

func (r *ExecRunner) event(ctx context.Context, severity, msg string, details map[string]any) {
	if r.emit == nil {
		return
	}
	r.emit(ctx, severity, msg, details)
}

func envVars(env Env) []string {
	out := []string{
		"CODING_HERMES_TICK=" + env.TickID,
		"CODING_HERMES_PROJECT=" + env.Project,
		"CODING_HERMES_LANE=" + env.Lane,
		"CODING_HERMES_ROLE=" + env.Role,
		"CODING_HERMES_NAMESPACE=" + env.Namespace,
		"CODING_HERMES_WORKDIR=" + env.Workdir,
		"CODING_HERMES_REPO=" + env.Repo,
		"CODING_HERMES_PHASE=" + string(env.Phase),
		"CODING_HERMES_HOOK_ID=" + env.HookID,
		// Reentrancy guard: a hook that starts a scheduler is a loop.
		"CODING_HERMES_HOOK=1",
	}
	if !env.Deadline.IsZero() {
		out = append(out, "CODING_HERMES_DEADLINE_AT="+env.Deadline.UTC().Format(time.RFC3339))
	}
	return out
}

// safeBuffer is a minimal concurrency-safe writer for cmd.Stdout/Stderr.
type safeBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (b *safeBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *safeBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// head returns the first n bytes of s (already UTF-8-emitted by the child in
// practice; byte slicing is fine for a log field).
func head(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// truncate applies the S13 output cap, keeping head and tail.
func truncate(s string) string {
	if len(s) <= OutputCapBytes {
		return s
	}
	half := OutputCapBytes / 2
	return s[:half] + "\n…[truncated]…\n" + s[len(s)-half:]
}

func (r *ExecRunner) insertReceipt(ctx context.Context, env Env, step Step, seq int, phase Phase, attempt int, started time.Time, argv string) int64 {
	if r.db == nil {
		return 0
	}
	res, err := r.db.ExecContext(ctx, `INSERT INTO hook_runs
        (tick_id, lane, phase, level, hook_id, priority, seq, command, argv, state, started_at, attempts)
        VALUES (?,?,?,?,?,?,?,?,?,'running',?,?)`,
		env.TickID, env.Lane, string(phase), string(step.Level), step.ID, step.Priority, seq,
		step.Command, argv, started.UTC().Format(time.RFC3339), attempt)
	if err != nil {
		log.Printf("hook receipt insert failed: %v", err)
		return 0
	}
	id, _ := res.LastInsertId()
	return id
}

func (r *ExecRunner) finishReceipt(ctx context.Context, id int64, res stepResult, dur time.Duration, output string) {
	if r.db == nil || id == 0 {
		return
	}
	_, err := r.db.ExecContext(ctx, `UPDATE hook_runs
        SET state=?, exit_code=?, finished_at=?, duration_ms=?, output=? WHERE run_id=?`,
		res.state, res.code, r.now().UTC().Format(time.RFC3339), dur.Milliseconds(),
		truncate(output), id)
	if err != nil {
		log.Printf("hook receipt update failed: %v", err)
	}
}

func (r *ExecRunner) insertSkippedReceipt(ctx context.Context, env Env, step Step, seq int, reason string) {
	if r.db == nil {
		return
	}
	// One instant for both stamps: a skipped step has no duration, so a
	// started_at/finished_at pair that differs by the time it took to format
	// the first one is an artefact, not a measurement.
	stamp := r.now().UTC().Format(time.RFC3339)
	_, err := r.db.ExecContext(ctx, `INSERT INTO hook_runs
        (tick_id, lane, phase, level, hook_id, priority, seq, command, argv, state, started_at, finished_at, output)
        VALUES (?,?,?,?,?,?,?,?,?,'skipped',?,?,?)`,
		env.TickID, env.Lane, string(step.Phase), string(step.Level), step.ID, step.Priority, seq,
		step.Command, RenderArgv(step, env), stamp, stamp,
		"skipped: "+reason)
	if err != nil {
		log.Printf("hook skip receipt failed: %v", err)
	}
}
