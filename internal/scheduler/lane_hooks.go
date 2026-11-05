package scheduler

import (
	"context"
	"strings"
	"time"

	"github.com/coding-hermes/scheduler/internal/lanehook"
)

// S13 wiring: the slot pool runs the lane's PRE hook list after the slot is
// claimed and before the model is resolved, and the POST list after
// lifecycle.Complete has written the outcome. A nil set or runner is the
// feature-off path and is byte-identical to pre-S13 behaviour.

// SetHooks arms lane hooks on the pool. Called once at boot; safe to call with
// nils (tests, fleets without hooks.toml).
func (p *SlotPool) SetHooks(set *lanehook.Set, runner lanehook.Runner) {
	p.hooks = set
	p.hookRunner = runner
}

// SetLaneHooks loads the definition set onto the loop and builds the exec
// runner with the loop's event logger. A nil set is the feature-off path.
func (l *Loop) SetLaneHooks(set *lanehook.Set) {
	if l == nil || l.slotPool == nil || set == nil {
		return
	}
	var emit lanehook.Emitter
	if l.events != nil {
		ev := l.events
		emit = func(ctx context.Context, severity, message string, details map[string]any) {
			ev.Emit(ctx, EventSeverity(severity), "hook", message, details)
		}
	}
	l.slotPool.SetHooks(set, lanehook.NewExecRunner(l.db, emit))
}

// hooksEnabled reports whether hooks are armed at all.
func (p *SlotPool) hooksEnabled() bool {
	return p != nil && p.hooks != nil && p.hookRunner != nil
}

// repoPath strips the repo_url scheme the fleet uses ("local:/home/kara/x").
func repoPath(u string) string {
	return strings.TrimPrefix(u, "local:")
}

// hookEnv builds the environment handed to every step.
func hookEnv(proj PackedProject, tickID string, deadline time.Time) lanehook.Env {
	_, role := lanehook.RoleFor(proj.Name)
	return lanehook.Env{
		TickID:    tickID,
		Lane:      proj.Name,
		Project:   proj.Name,
		Role:      role,
		Namespace: proj.NamespaceID,
		Workdir:   proj.Workdir,
		Repo:      repoPath(proj.RepoURL),
		Deadline:  deadline,
	}
}

// hookEmitter adapts the pool's event logger to lanehook.Emitter.
func (p *SlotPool) hookEmitter() lanehook.Emitter {
	events := p.events
	if events == nil {
		return nil
	}
	return func(ctx context.Context, severity, message string, details map[string]any) {
		events.Emit(ctx, EventSeverity(severity), "hook", message, details)
	}
}

// hookPlan resolves the lane's plan. The second return is false when hooks are
// off or the plan is empty, so callers can skip both phases cheaply.
func (p *SlotPool) hookPlan(proj PackedProject) (lanehook.Plan, bool) {
	if !p.hooksEnabled() {
		return lanehook.Plan{}, false
	}
	// Cheap mtime+size stat; a broken edit keeps the previous set armed.
	_, _ = p.hooks.ReloadIfChanged()
	var skip []string
	if spec, ok := p.hooks.Lane(proj.Name); ok {
		skip = spec.SkipHooks
	}
	plan, err := p.hooks.PlanFor(proj.Name, proj.NamespaceID, proj.Workdir, repoPath(proj.RepoURL), skip)
	if err != nil {
		if events := p.events; events != nil {
			events.Emit(context.Background(), SeverityHigh, "hook",
				"hook plan rejected — tick runs without hooks", map[string]any{
					"project": proj.Name, "error": err.Error(),
				})
		}
		return lanehook.Plan{}, false
	}
	if plan.Empty() {
		return plan, false
	}
	return plan, true
}

// runPreHooks executes the PRE list. A non-nil error means the tick must not
// spawn and the caller records failure_reason=hook_failed:<id>.
func (p *SlotPool) runPreHooks(proj PackedProject, tickID string, now time.Time, plan lanehook.Plan) error {
	if len(plan.Pre) == 0 {
		return nil
	}
	deadline := now.Add(p.hookDeadlineHint())
	env := hookEnv(proj, tickID, deadline)
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	return p.hookRunner.Pre(ctx, env, plan)
}

// runPostHooks executes the POST list. Failures are recorded, never returned.
func (p *SlotPool) runPostHooks(proj PackedProject, tickID string, plan lanehook.Plan) {
	if len(plan.Post) == 0 || p.hookRunner == nil {
		return
	}
	env := hookEnv(proj, tickID, time.Now().Add(p.hookDeadlineHint()))
	ctx, cancel := context.WithTimeout(context.Background(), p.hookDeadlineHint())
	defer cancel()
	p.hookRunner.Post(ctx, env, plan)
}

// hookDeadlineHint bounds a hook phase when the caller has no better deadline.
func (p *SlotPool) hookDeadlineHint() time.Duration {
	if p.spawner != nil && p.spawner.timeout > 0 {
		return p.spawner.timeout
	}
	return 30 * time.Minute
}
