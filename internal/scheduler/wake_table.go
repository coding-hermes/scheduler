package scheduler

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/coding-hermes/scheduler/internal/clock"
	"github.com/coding-hermes/scheduler/internal/database"
)

// SCHED-GAP-17002 — the wake-table runner.
//
// The board-wake watcher (board_wake.go) wakes a lane when its board CHANGES.
// The wake table wakes a lane when the OPERATOR (or an external system) said
// it should be woken — at a set time, daily at a time, or when an external
// event POSTs to /fire. Same family, different trigger source, and one
// critical difference: a wake-table entry is a PRE-AUTHORIZED operator
// instruction, so its spawn rides the operator path (the manual bypass (e) —
// the one bypass that is always granted and always logged) with its OWN
// nudge stamp (wake_table), never silently wearing "manual".
//
// Fail-open contract, identical to board_wake: a DB error logs, beats, and
// leaves the wake pending — the runner can never crash the loop, block a
// spawn, or fire a wake twice. The claim gate (state pending → firing, a
// conditional UPDATE) is the once-only guarantee: two runners, or a runner
// racing an API /fire, resolve to exactly one claimant.

// WakePollInterval is the due-wake poll cadence. Wakes are operator
// time-of-day intent, not latency-critical: 15s resolution is far inside any
// human meaning of "at 2am", and the poll is one indexed query.
const WakePollInterval = 15 * time.Second

// WakeTableRunner fires due wakes. Construct with NewWakeTableRunner, then
// Start. Stop is idempotent and waits.
type WakeTableRunner struct {
	clk    clockSeam
	db     *sql.DB
	events *EventLogger
	// spawn is the hook that runs the lane (production:
	// Loop.SpawnForWakeTable — the manual bypass with wake_table
	// provenance). ErrWakeBlackout re-pends the wake (retry after the
	// window); any other error settles it skipped — the row keeps the
	// attempt visible either way.
	spawn func(database.Project, string) (string, error)
	// project loads the lane row for a wake (production: the DB reader).
	project func(context.Context, *sql.DB, string) (*database.Project, error)

	poll   time.Duration
	stopCh chan struct{}
	wg     sync.WaitGroup
}

// NewWakeTableRunner wires the runner. spawnFn is the loop's wake spawn
// entry; nil spawn is a valid construction (wakes settle SKIPPED, events
// still fire) so wiring order never panics.
func NewWakeTableRunner(db *sql.DB, spawnFn func(database.Project, string) (string, error)) *WakeTableRunner {
	return &WakeTableRunner{
		db:      db,
		events:  NewEventLogger(db),
		spawn:   spawnFn,
		project: database.GetProject,
		poll:    WakePollInterval,
	}
}

// SetClock installs the clock seam (SCHED-GAP-169 convention). Must be called
// before Start.
func (r *WakeTableRunner) SetClock(c clock.Clock) { r.clk.Set(c) }

// SetPoll overrides the poll cadence (test seam). Must be called before Start.
func (r *WakeTableRunner) SetPoll(d time.Duration) { r.poll = d }

// Start launches the poll loop. Idempotent.
func (r *WakeTableRunner) Start() {
	if r.stopCh != nil {
		return
	}
	r.stopCh = make(chan struct{})
	r.wg.Add(1)
	go r.run(r.stopCh)
}

// Stop terminates the loop and waits. Idempotent.
func (r *WakeTableRunner) Stop() {
	if r.stopCh == nil {
		return
	}
	close(r.stopCh)
	r.wg.Wait()
	r.stopCh = nil
}

func (r *WakeTableRunner) run(stopCh <-chan struct{}) {
	defer r.wg.Done()
	ticker := r.clock().NewTicker(r.poll)
	defer ticker.Stop()
	for {
		select {
		case <-stopCh:
			return
		case <-ticker.C:
			r.pollOnce(context.Background())
		}
	}
}

// pollOnce claims and fires every due wake. Each wake is settled
// independently: one bad row must never hold the others hostage.
func (r *WakeTableRunner) pollOnce(ctx context.Context) {
	claimed, err := database.ClaimDueWakes(ctx, r.db, r.clock().Now())
	if err != nil {
		log.Printf("WAKE-TABLE: due-wake read failed (fail-open, wakes stay pending): %v", err)
		return
	}
	for _, w := range claimed {
		r.settleWake(ctx, w, "schedule")
	}
}

// FireExternal claims and fires ONE wake by id on demand — the API's
// external-event entry. ok=false when the wake was not pending (already
// fired, cancelled, or unknown).
func (r *WakeTableRunner) FireExternal(ctx context.Context, id string) (*database.Wake, bool, error) {
	w, claimed, err := database.ClaimWake(ctx, r.db, id)
	if err != nil || !claimed {
		return nil, claimed, err
	}
	if w.Kind != database.WakeKindExternal {
		// Not an external wake: put it back and refuse. A 'daily' or 'at'
		// wake fires on its own terms, not because someone POSTed.
		if _, err := r.db.ExecContext(ctx, `UPDATE wakes SET state = ? WHERE id = ?`, database.WakeStatePending, id); err != nil {
			return w, false, err
		}
		return w, false, &WakeKindError{ID: id, Kind: w.Kind}
	}
	r.settleWake(ctx, w, "external")
	return w, true, nil
}

// settleWake runs the spawn hook for one claimed wake and records what
// happened. Blackout handling lives in the SPAWN path's gate decision — the
// runner passes the wake's identity through and reports honestly here.
func (r *WakeTableRunner) settleWake(ctx context.Context, w *database.Wake, source string) {
	laneRow, err := r.project(ctx, r.db, w.Project)
	if err != nil {
		// Unknown/disabled lane: the promise cannot be kept. Record it —
		// a wake that dies silently is a lie the operator eventually pays.
		log.Printf("WAKE-TABLE: %s (%s->%s): lane unreadable, settling skipped: %v", w.ID, source, w.Project, err)
		r.emit(ctx, w, "wake_skipped", "lane unreadable", map[string]any{"reason": fmt.Sprintf("%v", err)})
		_ = database.SettleFiredWake(ctx, r.db, w, database.WakeSettlement{}, r.clock().Now())
		return
	}
	if laneRow == nil || !laneRow.Enabled {
		log.Printf("WAKE-TABLE: %s (%s->%s): lane disabled — skipped", w.ID, source, w.Project)
		r.emit(ctx, w, "wake_skipped", "lane disabled", nil)
		_ = database.SettleFiredWake(ctx, r.db, w, database.WakeSettlement{}, r.clock().Now())
		return
	}

	var tickID string
	spawned := false
	if r.spawn != nil {
		if id, err := r.spawn(*laneRow, w.ID); err != nil {
			if errors.Is(err, ErrWakeBlackout) {
				// AC6: a blackout window defers the wake — it stays pending
				// and retries after the window. The next attempt is pushed
				// 5 minutes out so a long window produces a handful of
				// retries, not a poll-storm; the event fires at most once
				// per 30 min per wake.
				if perr := database.PushWakeRetry(ctx, r.db, w.ID, 5*time.Minute, r.clock().Now()); perr != nil {
					log.Printf("WAKE-TABLE: %s re-pend failed: %v", w.ID, perr)
				}
				if r.recentlyEmitted(ctx, w, 30*time.Minute) {
					return
				}
				r.emit(ctx, w, "wake_deferred_blackout", "wake deferred — inside a blackout window", nil)
				return
			}
			// ErrProjectRunning (the SCHED-GAP-030 dedup) is a BUSY lane,
			// not a failure: the record shows the attempt either way.
			log.Printf("WAKE-TABLE: %s (%s->%s): spawn refused: %v", w.ID, source, w.Project, err)
			r.emit(ctx, w, "wake_skipped", "spawn refused", map[string]any{"reason": err.Error()})
		} else {
			tickID, spawned = id, true
		}
	} else {
		log.Printf("WAKE-TABLE: %s (%s->%s): no spawn hook wired — skipped", w.ID, source, w.Project)
		r.emit(ctx, w, "wake_skipped", "no spawn hook", nil)
	}

	sev := SeverityInfo
	if !spawned {
		sev = SeverityMedium
	}
	r.emit(ctx, w, "wake_fired", fmt.Sprintf("wake %s fired (%s)", w.ID, source),
		map[string]any{"spawned": spawned, "tick_id": tickID, "source": source, "severity_override": string(sev)})
	_ = database.SettleFiredWake(ctx, r.db, w, database.WakeSettlement{Spawned: spawned, TickID: tickID}, r.clock().Now())
}

// recentlyEmitted reports whether this runner already wrote an event for
// wake w within lookback — the blackout-retry throttle. One indexed query on a
// rare path; a failure reads as "not recent" (fail-open, one extra event).
func (r *WakeTableRunner) recentlyEmitted(ctx context.Context, w *database.Wake, lookback time.Duration) bool {
	since := r.clock().Now().UTC().Add(-lookback).Format(time.RFC3339)
	var n int
	err := r.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM events
	 WHERE component = 'wake_table' AND details LIKE ? AND created_at >= ?`,
		"%"+w.ID+"%", since).Scan(&n)
	return err == nil && n > 0
}

func (r *WakeTableRunner) emit(ctx context.Context, w *database.Wake, eventType, message string, extra map[string]any) {
	if r.events == nil {
		return
	}
	details := map[string]any{
		"event_type": eventType,
		"project":    w.Project,
		"wake_id":    w.ID,
		"kind":       w.Kind,
		"payload":    w.Payload,
	}
	for k, v := range extra {
		details[k] = v
	}
	r.events.Emit(ctx, SeverityInfo, "wake_table", message, details)
}

// WakeKindError is the semantic refusal for firing a wake whose kind is not
// external — the API maps it to 400 (the caller asked for something the wake
// cannot do), never 500 (nothing failed).
type WakeKindError struct {
	ID   string
	Kind string
}

func (e *WakeKindError) Error() string {
	return fmt.Sprintf("wake %s is kind %q, not external", e.ID, e.Kind)
}

// clock returns the runner's clock, never nil (the zero seam reads as the
// wall clock — same convention as BoardWakeWatcher.clock).
func (r *WakeTableRunner) clock() clock.Clock { return r.clk.Get() }
