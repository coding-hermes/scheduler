package scheduler

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"strings"
	"time"
)

// AlertEscalator checks scheduler health, project starvation, and failure
// patterns after each evaluation cycle and emits events at escalating severity.
type AlertEscalator struct {
	db     *sql.DB
	events *EventLogger
	// autoDisable configures per-project failure-rate auto-disable
	// (SCHED-GAP-018). failureRate <= 0 = feature off.
	autoDisable autoDisablePolicy
}

// NewAlertEscalator creates an escalator backed by db and events. The
// autoDisable policy enables per-project failure-rate auto-disable when
// autoDisable.failureRate > 0 (SCHED-GAP-018); a zero-value policy leaves
// the feature off.
func NewAlertEscalator(db *sql.DB, events *EventLogger, autoDisable autoDisablePolicy) *AlertEscalator {
	return &AlertEscalator{db: db, events: events, autoDisable: autoDisable}
}

// CheckSchedulerHealth emits CRITICAL if the scheduler has not evaluated in
// more than 10 minutes.
func (ae *AlertEscalator) CheckSchedulerHealth(ctx context.Context, lastEval time.Time) error {
	if lastEval.IsZero() {
		ae.events.Emit(ctx, SeverityCritical, "escalation", "scheduler not evaluating — never ran", map[string]any{
			"age_seconds": -1,
		})
		return nil
	}

	age := time.Since(lastEval)
	if age > 10*time.Minute {
		ae.events.Emit(ctx, SeverityCritical, "escalation",
			fmt.Sprintf("scheduler not evaluating — last eval %v ago", age.Round(time.Second)),
			map[string]any{
				"last_eval":   lastEval.Format(time.RFC3339),
				"age_seconds": age.Seconds(),
			})
	}
	return nil
}

// starvationThrottleWindow is the minimum spacing between consecutive
// starvation events for the same project (SCHED-GAP-014). The daemon
// evaluates roughly every 60s, so without throttling a starved project
// generates ~1 MEDIUM event/minute forever — 179 events in 33 minutes were
// observed across 20 projects. The escalator is constructed fresh on every
// evaluation cycle (tick_process.go), so the throttle survives by querying
// the events table for the last starvation event timestamp per project.
const starvationThrottleWindow = 30 * time.Minute

// lastStarvationEvent returns the created_at timestamp of the most recent
// MEDIUM starvation event for the given project, or ok=false if none exists.
// Uses json_extract for exact project-name matching (parameter-bound, no
// string interpolation) so similarly-named projects don't collide.
func (ae *AlertEscalator) lastStarvationEvent(ctx context.Context, project string) (time.Time, bool) {
	var ts string
	err := ae.db.QueryRowContext(ctx,
		`SELECT created_at FROM events
		 WHERE severity = ? AND component = ? AND json_extract(details, '$.project') = ?
		 ORDER BY created_at DESC LIMIT 1`,
		string(SeverityMedium), "escalation", project).Scan(&ts)
	if err != nil {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// CheckStarvation emits MEDIUM for each enabled project that has not had a
// completed tick in more than 2× its configured maximum interval. Events are
// throttled per-project: a starvation event is emitted at most once per
// starvationThrottleWindow per project (SCHED-GAP-014).
func (ae *AlertEscalator) CheckStarvation(ctx context.Context) error {
	// Query enabled projects with their intervals.
	prows, err := ae.db.QueryContext(ctx,
		`SELECT name, cooldown_s FROM projects WHERE enabled = 1`)
	if err != nil {
		return fmt.Errorf("query projects: %w", err)
	}
	defer prows.Close()

	type projInfo struct {
		name     string
		cooldown int
	}
	var projects []projInfo
	for prows.Next() {
		var p projInfo
		if err := prows.Scan(&p.name, &p.cooldown); err != nil {
			log.Printf("ESCALATION: scan project: %v", err)
			continue
		}
		projects = append(projects, p)
	}
	if err := prows.Err(); err != nil {
		return fmt.Errorf("iter projects: %w", err)
	}

	// Get last completed tick per project.
	crows, err := ae.db.QueryContext(ctx,
		`SELECT project_name, MAX(completed_at) FROM ticks WHERE status = 'completed' GROUP BY project_name`)
	if err != nil {
		return fmt.Errorf("query completions: %w", err)
	}
	defer crows.Close()

	lastCompleted := make(map[string]time.Time)
	for crows.Next() {
		var name string
		var ts string
		if err := crows.Scan(&name, &ts); err != nil {
			log.Printf("ESCALATION: scan completion: %v", err)
			continue
		}
		t, err := time.Parse(time.RFC3339, ts)
		if err != nil {
			log.Printf("ESCALATION: parse time %q: %v", ts, err)
			continue
		}
		lastCompleted[name] = t
	}
	if err := crows.Err(); err != nil {
		return fmt.Errorf("iter completions: %w", err)
	}

	// Check each project for starvation.
	now := time.Now()
	for _, proj := range projects {
		last, ok := lastCompleted[proj.name]
		if !ok {
			// Never completed a tick — not necessarily starved if it just started.
			continue
		}
		age := now.Sub(last)
		threshold := time.Duration(proj.cooldown) * time.Second * 2
		if age > threshold {
			// SCHED-GAP-014: throttle — only emit if no starvation event for
			// this project was recorded within the throttle window. This
			// prevents ~1 event/minute spam while still emitting once every
			// 30 minutes so the signal is not suppressed forever.
			if lastEmit, ok := ae.lastStarvationEvent(ctx, proj.name); ok {
				if now.Sub(lastEmit) < starvationThrottleWindow {
					continue
				}
			}
			ae.events.Emit(ctx, SeverityMedium, "escalation",
				fmt.Sprintf("project starved: %s — last tick %v ago, cooldown %ds",
					proj.name, age.Round(time.Second), proj.cooldown),
				map[string]any{
					"project":     proj.name,
					"last_tick":   last.Format(time.RFC3339),
					"cooldown":    proj.cooldown,
					"age_seconds": age.Seconds(),
				})
		}
	}
	return nil
}

// CheckConsecutiveFailures emits HIGH for any project with more than 3
// consecutive failed ticks (no completed tick interspersed).
func (ae *AlertEscalator) CheckConsecutiveFailures(ctx context.Context) error {
	// Get all enabled project names.
	prows, err := ae.db.QueryContext(ctx, `SELECT name FROM projects WHERE enabled = 1`)
	if err != nil {
		return fmt.Errorf("query projects: %w", err)
	}
	defer prows.Close()

	var names []string
	for prows.Next() {
		var name string
		if err := prows.Scan(&name); err != nil {
			log.Printf("ESCALATION: scan project name: %v", err)
			continue
		}
		names = append(names, name)
	}
	if err := prows.Err(); err != nil {
		return fmt.Errorf("iter project names: %w", err)
	}

	for _, name := range names {
		rows, err := ae.db.QueryContext(ctx,
			`SELECT id, status FROM ticks WHERE project_name = ? ORDER BY completed_at DESC LIMIT 5`,
			name)
		if err != nil {
			log.Printf("ESCALATION: query ticks for %s: %v", name, err)
			continue
		}

		var failures []string
		consecutive := 0
		for rows.Next() {
			var tickID, status string
			if err := rows.Scan(&tickID, &status); err != nil {
				log.Printf("ESCALATION: scan tick: %v", err)
				continue
			}
			if status == "failed" || status == "timeout" {
				consecutive++
				failures = append(failures, tickID)
			} else {
				break // completed/queued/running breaks the streak
			}
		}
		rows.Close()

		if consecutive > 3 {
			ae.events.Emit(ctx, SeverityHigh, "escalation",
				fmt.Sprintf("more than 3 consecutive failures: %s", name),
				map[string]any{
					"project":              name,
					"consecutive_failures": consecutive,
					"last_failure":         failures[0],
				})
		}
	}
	return nil
}

// CheckDuplicateWorkdirs emits HIGH for any pair of ENABLED projects sharing
// the same workdir. Two active foremen pointing at one directory split ticks
// unpredictably and can both commit to the same board (INFRA-009: ghost
// project `heading` duplicated `HEADING`'s workdir, spawned a tick, and
// committed into HEADING's repo). The create-path guard (database/projects.go)
// prevents new duplicates; this check flags any that already exist so the
// fleet audit surfaces them instead of letting them run silently.
func (ae *AlertEscalator) CheckDuplicateWorkdirs(ctx context.Context) error {
	prows, err := ae.db.QueryContext(ctx,
		`SELECT name, workdir FROM projects WHERE enabled = 1 AND workdir != '' ORDER BY workdir, name`)
	if err != nil {
		return fmt.Errorf("query enabled projects: %w", err)
	}
	defer prows.Close()

	byWorkdir := make(map[string][]string)
	for prows.Next() {
		var name, workdir string
		if err := prows.Scan(&name, &workdir); err != nil {
			log.Printf("ESCALATION: scan workdir project: %v", err)
			continue
		}
		key := strings.ToLower(workdir)
		byWorkdir[key] = append(byWorkdir[key], name)
	}
	if err := prows.Err(); err != nil {
		return fmt.Errorf("iter workdir projects: %w", err)
	}

	for wd, names := range byWorkdir {
		if len(names) < 2 {
			continue
		}
		ae.events.Emit(ctx, SeverityHigh, "escalation",
			fmt.Sprintf("duplicate workdir: %d enabled projects share %s", len(names), wd),
			map[string]any{
				"workdir":  wd,
				"projects": names,
			})
	}
	return nil
}

// CheckFailureRateAutoDisable disables enabled projects whose recent failure
// rate meets or exceeds the configured threshold (SCHED-GAP-018). A project
// is auto-disabled only when ALL of the following hold:
//   - the feature is on (autoDisable.failureRate > 0);
//   - the project is currently enabled;
//   - the project has at least autoDisable.minTicks completed ticks within
//     the last autoDisable.window ticks;
//   - the failure rate (failed+timeout / total) >= autoDisable.failureRate.
//
// On disable, the projects row is updated (enabled=0) and a HIGH event is
// emitted with the computed details. This check runs only from RunAll (the
// eval cycle), never inside spawn paths.
func (ae *AlertEscalator) CheckFailureRateAutoDisable(ctx context.Context) error {
	if ae.autoDisable.failureRate <= 0 {
		return nil // feature off
	}
	window := ae.autoDisable.window
	if window <= 0 {
		window = 100
	}
	minTicks := ae.autoDisable.minTicks
	if minTicks <= 0 {
		minTicks = 50
	}

	// Get all enabled project names.
	prows, err := ae.db.QueryContext(ctx, `SELECT name FROM projects WHERE enabled = 1`)
	if err != nil {
		return fmt.Errorf("query projects for auto-disable: %w", err)
	}
	defer prows.Close()

	var names []string
	for prows.Next() {
		var name string
		if err := prows.Scan(&name); err != nil {
			log.Printf("AUTO-DISABLE: scan project name: %v", err)
			continue
		}
		names = append(names, name)
	}
	if err := prows.Err(); err != nil {
		return fmt.Errorf("iter project names for auto-disable: %w", err)
	}

	for _, name := range names {
		rows, err := ae.db.QueryContext(ctx,
			`SELECT status, COALESCE(error, '') FROM ticks
			 WHERE project_name = ? AND completed_at IS NOT NULL
			 ORDER BY spawned_at DESC LIMIT ?`,
			name, window)
		if err != nil {
			log.Printf("AUTO-DISABLE: query ticks for %s: %v", name, err)
			continue
		}

		var failed, total int
		for rows.Next() {
			var status, errText string
			if err := rows.Scan(&status, &errText); err != nil {
				continue
			}
			// Harness/infrastructure failures say nothing about the project:
			// the tick never reached it. Counting them let one gateway restart
			// mass-disable healthy lanes (SCHED-GAP-134 — 2026-09-16: six of
			// the fleet's top foreman lanes were auto-disabled on 90/100
			// gateway-drain 503s during a graceful gateway restart).
			if status == "failed" && harnessFailure(errText) {
				continue
			}
			total++
			if status == "failed" || status == "timeout" {
				failed++
			}
		}
		rows.Close()

		if total < minTicks {
			continue // sample size too small
		}

		rate := float64(failed) / float64(total)
		if rate < ae.autoDisable.failureRate {
			continue // below threshold
		}

		// Disable the project (GAP-044: stamp provenance — who/when/why —
		// so disabled fleet state stays auditable).
		nowStr := time.Now().UTC().Format(time.RFC3339)
		reason := fmt.Sprintf("failure rate %.1f%% (%d/%d ticks, window %d, threshold %.2f)",
			rate*100, failed, total, window, ae.autoDisable.failureRate)
		_, err = ae.db.ExecContext(ctx,
			`UPDATE projects SET enabled = 0, disabled_at = ?, disabled_by = 'auto-disable', disabled_reason = ?, updated_at = ? WHERE name = ? AND enabled = 1`,
			nowStr, reason, nowStr, name)
		if err != nil {
			log.Printf("AUTO-DISABLE: failed to disable %s: %v", name, err)
			continue
		}

		log.Printf("AUTO-DISABLE: disabled %s — failure_rate=%.4f (%d/%d, window=%d, threshold=%.2f)",
			name, rate, failed, total, window, ae.autoDisable.failureRate)

		ae.events.Emit(ctx, SeverityHigh, "auto-disable",
			fmt.Sprintf("project auto-disabled: %s — %.1f%% failure rate (%d/%d ticks)", name, rate*100, failed, total),
			map[string]any{
				"project":         name,
				"failure_rate":    rate,
				"failed":          failed,
				"total":           total,
				"window":          window,
				"min_ticks":       minTicks,
				"threshold":       ae.autoDisable.failureRate,
				"disabled_at":     nowStr,
				"disabled_by":     "auto-disable",
				"disabled_reason": reason,
			})
	}
	return nil
}

// harnessFailure reports whether a failed tick's error came from the harness
// (gateway / scheduler infrastructure) rather than from the project itself.
// These ticks never reached the project, so they must not feed per-project
// health accounting (SCHED-GAP-134). Keep this list tight: only outage classes
// where every project on the box fails identically.
func harnessFailure(errText string) bool {
	if errText == "" {
		return false
	}
	lower := strings.ToLower(errText)
	for _, marker := range []string{
		"gateway unreachable",
		"gateway is draining",
		"gateway_auth_error",
		"invalid gateway api key",
		"connection refused",
		"exec fallback disabled",
		"aborted by graceful shutdown",
	} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

// RunAll executes all escalation checks. Errors from individual checks
// are logged but not propagated — one check failing does not block the others.
func (ae *AlertEscalator) RunAll(ctx context.Context, lastEval time.Time) error {
	if err := ae.CheckSchedulerHealth(ctx, lastEval); err != nil {
		log.Printf("ESCALATION: health check error: %v", err)
	}
	if err := ae.CheckStarvation(ctx); err != nil {
		log.Printf("ESCALATION: starvation check error: %v", err)
	}
	if err := ae.CheckConsecutiveFailures(ctx); err != nil {
		log.Printf("ESCALATION: consecutive failures check error: %v", err)
	}
	if err := ae.CheckDuplicateWorkdirs(ctx); err != nil {
		log.Printf("ESCALATION: duplicate workdir check error: %v", err)
	}
	if err := ae.CheckFailureRateAutoDisable(ctx); err != nil {
		log.Printf("ESCALATION: auto-disable check error: %v", err)
	}
	return nil
}
