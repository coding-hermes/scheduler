// Package lanehook implements S13: operator-declared, priority-ordered command
// lists that run before and after a lane tick.
//
// The package is deliberately independent of internal/scheduler (no import
// cycle): it takes a *sql.DB for receipts, an Emitter for events, and plain
// values for everything else. The scheduler wires it in at Spawn (PRE) and
// after CompleteTick (POST).
package lanehook

import (
	"context"
	"time"
)

// Phase is where a step runs relative to the agent tick.
type Phase string

const (
	PhasePre  Phase = "pre"
	PhasePost Phase = "post"
)

// Level is the scope a step is declared at.
type Level string

const (
	LevelGlobal    Level = "global"
	LevelNamespace Level = "namespace"
	LevelLane      Level = "lane"
)

// OnFailure decides what a non-zero exit does to the tick. Its meaning is
// per phase and the difference is normative (S13 §2.3.8):
//
//	PRE  closed -> the tick does not spawn; the POST list still runs.
//	PRE  open   -> recorded, the tick spawns.
//	POST closed -> the REMAINING post steps are skipped, tick outcome untouched.
//	POST open   -> recorded, the remaining post steps still run.
type OnFailure string

const (
	FailClosed OnFailure = "closed"
	FailOpen   OnFailure = "open"
	FailRetry  OnFailure = "retry"
)

// Order selects the cross-level ordering rule (S13 §2.2).
type Order string

const (
	OrderLevelFirst Order = "level_first"
	OrderFlat       Order = "flat"
)

// Step is one entry as declared by the operator. Field tags carry both the
// TOML surface (hooks.toml) and the JSON surface (REST API); they are the same
// shape on purpose so the API can only write what the file can express.
type Step struct {
	ID          string    `toml:"id"            json:"id"`
	Phase       Phase     `toml:"phase"         json:"phase"`
	Level       Level     `toml:"level"         json:"level"`
	Target      string    `toml:"target"        json:"target"`
	Priority    int       `toml:"priority"      json:"priority"`
	Command     string    `toml:"command"       json:"command"`
	Args        []string  `toml:"args,omitempty" json:"args,omitempty"`
	TimeoutS    int       `toml:"timeout_s"     json:"timeout_s"`
	OnFailure   OnFailure `toml:"on_failure"    json:"on_failure"`
	Retries     int       `toml:"retries,omitempty"       json:"retries,omitempty"`
	RetryDelayS int       `toml:"retry_delay_s,omitempty" json:"retry_delay_s,omitempty"`
	Enabled     *bool     `toml:"enabled,omitempty" json:"enabled,omitempty"`
}

// IsEnabled reports the tri-state Enabled flag (nil means true).
func (s Step) IsEnabled() bool { return s.Enabled == nil || *s.Enabled }

// Timeout returns the effective per-step timeout, applying the default and cap
// from S13 §2.1 (default 60s, cap 600s).
func (s Step) Timeout() time.Duration {
	secs := s.TimeoutS
	if secs <= 0 {
		secs = DefaultStepTimeoutS
	}
	if secs > MaxStepTimeoutS {
		secs = MaxStepTimeoutS
	}
	return time.Duration(secs) * time.Second
}

// OnFailureOrDefault returns the declared policy, or the phase default
// (closed for PRE, open for POST) when the field is empty.
func (s Step) OnFailureOrDefault() OnFailure {
	if s.OnFailure != "" {
		return s.OnFailure
	}
	if s.Phase == PhasePost {
		return FailOpen
	}
	return FailClosed
}

const (
	// DefaultStepTimeoutS is applied when a step declares no timeout.
	DefaultStepTimeoutS = 60
	// MaxStepTimeoutS caps a declared timeout.
	MaxStepTimeoutS = 600
	// OutputCapBytes bounds captured output per attempt (head+tail kept).
	OutputCapBytes = 64 * 1024
	// ExitSkip is the hook contract's reserved "nothing to do" exit code:
	// lane-merge returns it for merge=no_commits, worktree-ensure for a
	// workdir that IS the repo's primary checkout (SCHED-GAP-16991). The
	// runner records the step as state=skipped — non-fatal, never retried,
	// never a veto or a chain abort.
	ExitSkip = 10
	// ExitBusy is quiesce-check's reserved "a live process holds the tree"
	// exit code. The runner records state=busy, distinct from an exit-10 skip;
	// the normal on_failure policy still applies, so a closed POST gate stops
	// merge and prune.
	ExitBusy = 11
)

// Plan is the resolved, ordered list for one tick.
type Plan struct {
	Lane      string `json:"lane"`
	Namespace string `json:"namespace"`
	Workdir   string `json:"workdir"`
	Repo      string `json:"repo"`
	Order     Order  `json:"order"`
	Pre       []Step `json:"pre"`
	Post      []Step `json:"post"`
}

// Empty reports whether the plan has no steps at all. The zero step case must
// be free: no events, no receipts, no log (S13 §6).
func (p Plan) Empty() bool { return len(p.Pre) == 0 && len(p.Post) == 0 }

// Env is the placeholder and environment set available to every step.
type Env struct {
	TickID    string
	Lane      string
	Project   string
	Role      string
	Namespace string
	Workdir   string
	Repo      string
	Deadline  time.Time
	HookID    string
	Phase     Phase
}

// Emitter mirrors scheduler.EventLogger.Emit without importing it.
type Emitter func(ctx context.Context, severity, message string, details map[string]any)

// Runner executes a plan for one tick.
type Runner interface {
	// Pre runs the PRE list. A non-nil error means the tick must not spawn.
	Pre(ctx context.Context, env Env, plan Plan) error
	// Post runs the POST list. Errors are recorded, never propagated as a
	// tick failure.
	Post(ctx context.Context, env Env, plan Plan)
}
