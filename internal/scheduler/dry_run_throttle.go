package scheduler

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"time"

	"github.com/coding-hermes/scheduler/internal/clock"
	"github.com/coding-hermes/scheduler/internal/database"
)

// SCHED-GAP-17000 — DRY-RUN THROTTLE: two lines on the no-artifact streak.
//
// Bane 2026-11-07 (verbatim intent): "if there are five dry runs then do
// something ... if it has ten dry runs as in does not come in actual work then
// we suspend the format for running and we hit it with the cooldown
// essentially."
//
// The streak itself already exists and is already the right measurement:
// projects.no_progress_ticks counts CONSECUTIVE ticks that produced no artifact
// of the lane's own work, using the SCHED-GAP-104 definition (a CODE commit or a
// memory key counts; board bookkeeping does not) and failing OPEN when the
// commit window is unmeasurable. NOOP-PACING then spends that number as a
// pacing input on tasks-admitted lanes (streak 4+ -> 8x the cooldown floor).
//
// What was missing is a CEILING on how long a lane may keep doing that. Two
// things were measurably wrong before this:
//
//  1. The streak was only recorded for admission_mode='tasks'. A satellite
//     lane (cooldown admission — the docs/lint/format flavour) could dry-run
//     forever: no counter, no sanction, and because a satellite's no-op is
//     explicitly ALLOWED by class (SCHED-GAP-1682) the noop_guard re-entry
//     never fires either. "Allowed to no-op" was being read as "allowed to
//     no-op indefinitely", which is not the same thing.
//  2. Even on a tasks lane the only sanction was exponential pacing. Pacing
//     spaces a productive-but-slow lane; it does not stop a lane that has been
//     empty for ten consecutive ticks from eating its next slot.
//
// So: warn at DryRunWarnStreak (5) — "do something" — and SUSPEND at the
// project's dry_run_throttle (default 10) by pinning the lane's cooldown to its
// ceiling. Suspension here means exactly what the owner asked for: the lane
// stops running because the cooldown pin outlasts the interval between its
// spawns. It is NOT a disable — enabled stays 1, the row stays visible, and the
// pin is cleared by the first tick that produces real work, so a lane whose
// board refills comes straight back.
//
// The pin is namespaced by provenance ('dry_run_throttle'). Recovery clears
// ONLY a pin this feature set: an operator pin is never silently removed by a
// scheduler decision, because the operator set it for a reason the scheduler
// cannot see.

const (
	// DryRunWarnStreak / DefaultDryRunThrottle are the SHARED policy numbers
	// (database package) so the scheduler, the config loader and the API can
	// never disagree about them.
	DryRunWarnStreak      = database.DryRunWarnStreak
	DefaultDryRunThrottle = database.DefaultDryRunThrottle

	// DryRunThrottlePinBy is the cooldown_pin_by provenance this feature
	// stamps. Recovery clears ONLY pins carrying it.
	DryRunThrottlePinBy = "dry_run_throttle"

	// dryRunPolicyTimeout bounds the policy read + pin write. Both are two
	// indexed statements on one row; a slow DB must not stall a tick's
	// close-out (the same 2s class noop_guard uses for its stamps).
	dryRunPolicyTimeout = 2 * time.Second
)

// DryRunThrottleSettingError delegates to the shared rule (database package).
func DryRunThrottleSettingError(cfg int) error {
	return database.DryRunThrottleSettingError(cfg)
}

// dryRunThrottleLimit resolves the effective suspension threshold.
func dryRunThrottleLimit(cfg int) int {
	return database.EffectiveDryRunThrottle(cfg)
}

// dryRunSuspendDue reports whether a streak has reached the suspension line.
func dryRunSuspendDue(streak, limit int) bool {
	return limit > 0 && streak >= limit
}

// dryRunSuspendPinS is the cooldown a suspension pins the lane to: the lane's
// own ceiling when it has one, else 8x its floor (the ADV-R10 derived ceiling
// shape, the same 8x noop pacing tops out at). Zero means the lane has no
// cooldown policy at all — then there is nothing to pin and suspension degrades
// to the warning, which is honest: inventing a cooldown for a lane nobody
// configured would be an operator-visible policy change made in the dark.
func dryRunSuspendPinS(floorS, ceilingS int) int {
	if ceilingS > 0 {
		return ceilingS
	}
	if floorS > 0 {
		return floorS * 8
	}
	return 0
}

// dryRunPolicy is the slice of the project row the throttle decides with.
type dryRunPolicy struct {
	Streak     int
	Limit      int
	FloorS     int
	CeilingS   int
	PinBy      string
	PinS       int
	configured int // the raw stored value, kept for the event payload
}

// loadDryRunPolicy reads the policy for one lane. A missing row is not an
// error the caller can act on (the tick already completed) — it degrades to
// "no policy", which suspends nothing.
func loadDryRunPolicy(ctx context.Context, db *sql.DB, project string) (dryRunPolicy, error) {
	var (
		streak, floorS, ceilingS int
		throttle                 sql.NullInt64
		pinS                     sql.NullInt64
		pinBy                    string
	)
	err := db.QueryRowContext(ctx, `
SELECT COALESCE(no_progress_ticks, 0), COALESCE(cooldown_floor_s, 0), COALESCE(cooldown_ceiling_s, 0),
       dry_run_throttle, cooldown_pin_s, COALESCE(cooldown_pin_by, '')
  FROM projects WHERE name = ?`, project,
	).Scan(&streak, &floorS, &ceilingS, &throttle, &pinS, &pinBy)
	if err != nil {
		return dryRunPolicy{}, err
	}
	p := dryRunPolicy{
		Streak: streak, FloorS: floorS, CeilingS: ceilingS,
		PinBy: pinBy, Limit: DefaultDryRunThrottle,
	}
	if throttle.Valid && throttle.Int64 > 0 {
		p.configured = int(throttle.Int64)
		p.Limit = dryRunThrottleLimit(p.configured)
	}
	if pinS.Valid {
		p.PinS = int(pinS.Int64)
	}
	return p, nil
}

// dryRunAction names what the throttle did on this pass, so the caller (and the
// tests) can assert behaviour without parsing logs.
type dryRunAction string

const (
	dryRunNone      dryRunAction = ""            // nothing to do
	dryRunWarned    dryRunAction = "warn"        // streak crossed the warn line
	dryRunSuspended dryRunAction = "suspend"     // lane pinned to its cooldown ceiling
	dryRunRecovered dryRunAction = "recover"     // real work arrived; own pin cleared
	dryRunDeferred  dryRunAction = "pin_present" // suspension due but an operator pin owns the row
)

// applyDryRunThrottle is the whole policy for one terminal tick.
//
// progress=true means this tick produced real work (the caller's SCHED-GAP-104
// classification): the streak has already been reset, so the only thing left is
// to release a suspension this feature applied. progress=false extends the
// streak and may warn or suspend.
//
// Best-effort by contract: every failure is logged and returned, never
// propagated into the tick's outcome — the throttle observes ticks, it does not
// own them.
func applyDryRunThrottle(ctx context.Context, db *sql.DB, project string, progress bool) (dryRunAction, error) {
	if db == nil || project == "" {
		return dryRunNone, nil
	}
	ctx, cancel := context.WithTimeout(ctx, dryRunPolicyTimeout)
	defer cancel()

	p, err := loadDryRunPolicy(ctx, db, project)
	if err != nil {
		if err == sql.ErrNoRows {
			return dryRunNone, nil
		}
		return dryRunNone, fmt.Errorf("dry-run throttle: read policy for %q: %w", project, err)
	}

	if progress {
		// Only our own pin is ours to clear. An operator pin stays.
		if p.PinBy != DryRunThrottlePinBy {
			return dryRunNone, nil
		}
		if _, err := db.ExecContext(ctx, `
UPDATE projects
   SET cooldown_pin_s = NULL, cooldown_pin_by = '', cooldown_pin_at = ''
 WHERE name = ? AND cooldown_pin_by = ?`, project, DryRunThrottlePinBy); err != nil {
			return dryRunNone, fmt.Errorf("dry-run throttle: clear pin for %q: %w", project, err)
		}
		log.Printf("DRY-RUN-THROTTLE: %s produced work — suspension lifted (pin %ds cleared)", project, p.PinS)
		emitDryRunEvent(ctx, db, "INFO", "dry_run_recovered", project,
			"dry-run suspension lifted: the lane produced work",
			map[string]any{"cleared_pin_s": p.PinS, "streak": p.Streak})
		return dryRunRecovered, nil
	}

	limit := p.Limit
	if dryRunSuspendDue(p.Streak, limit) {
		pin := dryRunSuspendPinS(p.FloorS, p.CeilingS)
		if pin <= 0 {
			log.Printf("DRY-RUN-THROTTLE: %s at %d dry ticks (limit %d) but has no cooldown floor/ceiling — nothing to pin, staying at warn", project, p.Streak, limit)
			emitDryRunEvent(ctx, db, "MEDIUM", "dry_run_suspend_unavailable", project,
				"suspension due but the lane has no cooldown policy to pin",
				map[string]any{"streak": p.Streak, "limit": limit})
			return dryRunNone, nil
		}
		// Never clobber an operator pin: if one is present and it is not ours,
		// the lane is already slower than we would make it.
		if p.PinS > 0 && p.PinBy != DryRunThrottlePinBy {
			log.Printf("DRY-RUN-THROTTLE: %s at %d dry ticks (limit %d) — operator pin %ds (by %q) already in force, not touching it",
				project, p.Streak, limit, p.PinS, p.PinBy)
			return dryRunDeferred, nil
		}
		res, err := db.ExecContext(ctx, `
UPDATE projects
   SET cooldown_pin_s = ?, cooldown_pin_by = ?, cooldown_pin_at = ?
 WHERE name = ?
   AND (cooldown_pin_s IS NULL OR cooldown_pin_s <= ? OR cooldown_pin_by = ?)`,
			pin, DryRunThrottlePinBy, nowUTCString(ctx), project, pin, DryRunThrottlePinBy)
		if err != nil {
			return dryRunNone, fmt.Errorf("dry-run throttle: suspend %q: %w", project, err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			// The row carried a pin higher than ours — respect it.
			return dryRunDeferred, nil
		}
		log.Printf("DRY-RUN-THROTTLE: %s SUSPENDED — %d consecutive dry ticks (limit %d), cooldown pinned to %ds",
			project, p.Streak, limit, pin)
		emitDryRunEvent(ctx, db, "MEDIUM", "dry_run_suspended", project,
			"dry-run throttle: lane suspended until it produces work",
			map[string]any{
				"streak":     p.Streak,
				"limit":      limit,
				"pin_s":      pin,
				"floor_s":    p.FloorS,
				"ceiling_s":  p.CeilingS,
				"pin_by":     DryRunThrottlePinBy,
				"configured": p.configured,
			})
		return dryRunSuspended, nil
	}

	// Warn exactly once per crossing of the line (the streak is monotone
	// within a dry run, so an equality test is a transition test).
	if p.Streak == DryRunWarnStreak {
		log.Printf("DRY-RUN-THROTTLE: %s has %d consecutive dry ticks — one warning before suspension at %d",
			project, p.Streak, limit)
		emitDryRunEvent(ctx, db, "MEDIUM", "dry_run_warn", project,
			"dry-run warning: the lane is being spawned without producing work",
			map[string]any{"streak": p.Streak, "limit": limit, "suspend_at": limit})
		return dryRunWarned, nil
	}
	return dryRunNone, nil
}

// emitDryRunEvent writes one event under the noop_guard component — the guard
// already owns "what a tick did or did not produce", and a second component for
// the same question would split the operator's query.
func emitDryRunEvent(ctx context.Context, db *sql.DB, severity, eventType, project, message string, extra map[string]any) {
	if db == nil {
		return
	}
	details := map[string]any{
		"event_type": eventType,
		"project":    project,
	}
	for k, v := range extra {
		details[k] = v
	}
	raw, err := json.Marshal(details)
	if err != nil {
		return
	}
	if err := database.LogEvent(ctx, db, &database.Event{
		Severity:  database.EventSeverity(severity),
		Component: NoopGuardEventComponent,
		Message:   message,
		Details:   string(raw),
	}); err != nil {
		log.Printf("DRY-RUN-THROTTLE: %s event %q write failed: %v", project, eventType, err)
	}
}

// nowUTCString is the timestamp format the cooldown_pin_at column carries.
// Routed through the clock seam (clock.Real() as the explicit default) so the
// single time choke point holds — see internal/clock/stdlib_guard_test.go.
func nowUTCString(ctx context.Context) string {
	return clock.Real().Now().UTC().Format(time.RFC3339)
}
