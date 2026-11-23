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
			log.Printf("WAKE-TABLE: %s (%s->%s): spawn refused: %v", w.ID, source, w.Project, err)
			if errors.Is(err, ErrProjectRunning) {
				// ErrProjectRunning (the SCHED-GAP-030 dedup) is a BUSY
				// lane, not a failure: keep the wake pending and retry
				// after the same five-minute delay used by the blackout
				// path. The skip/fired pair is throttled so a daily wake
				// cannot emit on every poll while its lane remains busy.
				if perr := database.PushWakeRetry(ctx, r.db, w.ID, 5*time.Minute, r.clock().Now()); perr != nil {
					log.Printf("WAKE-TABLE: %s busy retry push failed: %v", w.ID, perr)
				}
				if r.recentlyEmitted(ctx, w, 30*time.Minute) {
					return
				}
				r.emit(ctx, w, "wake_skipped", "spawn refused", map[string]any{"reason": err.Error()})
				r.emit(ctx, w, "wake_fired", fmt.Sprintf("wake %s fired (%s)", w.ID, source),
					map[string]any{"spawned": false, "tick_id": "", "source": source, "severity_override": string(SeverityMedium)})
				return
			}
			// Any other spawn error is an honest skip, never an endless
			// retry loop: record the refusal and settle skipped below
			// (the attempt is visible, nothing is hidden).
		} else {
			tickID, spawned = id, true
			// REV-004: a spawn that returned a tick id is not yet a KEPT
			// promise. The tick row's terminal status lands moments after the
			// spawn returns, and under --no-exec-fallback every gateway outage
			// stamps it deferred almost immediately (observed live:
			// outcome=deferred, error="no gateway client and exec fallback
			// disabled"). Observe briefly; a not-fired verdict must NOT settle
			// the wake as fired — a daily that did would re-arm to tomorrow
			// while the audit trail claimed success, silently losing the day's
			// scheduled run.
			if st, oc, reason, notFired := r.tickVerdict(ctx, tickID); notFired {
				log.Printf("WAKE-TABLE: %s (%s->%s): spawned tick %s settled %s (%s) — wake NOT fired, retry pushed 5m",
					w.ID, source, w.Project, tickID, st, reason)
				// The REV-003 busy-lane pattern: keep the wake pending on the
				// SAME occurrence with a 5-minute backoff — never touch
				// fired_at or fired_tick_id for the deferred tick.
				if perr := database.PushWakeRetry(ctx, r.db, w.ID, 5*time.Minute, r.clock().Now()); perr != nil {
					log.Printf("WAKE-TABLE: %s not-fired retry push failed: %v", w.ID, perr)
				}
				if r.recentlyEmitted(ctx, w, 30*time.Minute) {
					return
				}
				msg := fmt.Sprintf("wake %s spawn %s: tick %s settled %s — occurrence kept, retry pushed 5m",
					w.ID, source, tickID, st)
				if reason != "" {
					msg += ": " + reason
				}
				r.emitSev(ctx, w, SeverityMedium, "wake_spawn_not_fired", msg,
					map[string]any{
						"spawned":           false,
						"tick_id":           tickID,
						"tick_status":       st,
						"tick_outcome":      oc,
						"reason":            reason,
						"source":            source,
						"severity_override": string(SeverityMedium),
					})
				return
			}
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

// REV-004 — the tick verdict observation. A spawn that returned a tick id is
// not yet a kept promise: the tick row exists (the spawn enqueues it
// synchronously before returning), but its TERMINAL status lands moments
// later, and under --no-exec-fallback every gateway outage stamps the tick
// deferred almost immediately. settleWake therefore observes the row briefly
// before settling; these bounds keep that observation cheap (it runs inside
// the wake poll loop): one immediate read, then up to three re-reads ~300ms
// apart (~0.9s worst case).
const (
	tickVerdictReads   = 4
	tickVerdictPollGap = 300 * time.Millisecond
)

// notFiredTickStatuses are the tick statuses that mean the lane's run did
// NOT happen. A tick still queued/running after the window — or completed —
// reads as fired, exactly as before this check existed.
var notFiredTickStatuses = map[string]bool{
	string(database.StatusDeferred): true,
	string(database.StatusFailed):   true,
	string(database.StatusTimeout):  true,
	string(database.StatusAborted):  true,
}

// tickVerdict briefly observes the tick row a successful spawn produced.
// Returns the last observed status, outcome and error text; notFired is true
// only when a terminal not-fired verdict was READ. Everything else — no row
// visible (a spawn hook that never enqueued one), a read failure, still
// queued/running after the window — reads notFired=false so the settlement
// is exactly today's: the happy path is unchanged, and observation can never
// invent a verdict the row does not carry.
func (r *WakeTableRunner) tickVerdict(ctx context.Context, tickID string) (status, outcome, reason string, notFired bool) {
	for i := 0; i < tickVerdictReads; i++ {
		if i > 0 {
			timer := r.clock().NewTimer(tickVerdictPollGap)
			select {
			case <-ctx.Done():
				timer.Stop()
				return status, outcome, reason, false
			case <-timer.C:
			}
		}
		var st, oc, errCol sql.NullString
		err := r.db.QueryRowContext(ctx, `SELECT status, outcome, error FROM ticks WHERE id = ?`, tickID).
			Scan(&st, &oc, &errCol)
		if err != nil {
			if !errors.Is(err, sql.ErrNoRows) {
				log.Printf("WAKE-TABLE: tick %s verdict read failed (fail-open, settling as fired): %v", tickID, err)
			}
			return status, outcome, reason, false
		}
		status, outcome, reason = st.String, oc.String, errCol.String
		if status == string(database.StatusCompleted) {
			return status, outcome, reason, false
		}
		if notFiredTickStatuses[status] {
			return status, outcome, reason, true
		}
		// queued/running (or an unknown status): keep observing.
	}
	return status, outcome, reason, false
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
	r.emitSev(ctx, w, SeverityInfo, eventType, message, extra)
}

// emitSev is emit with an explicit severity (REV-004): a not-fired spawn is
// an operator-visible failure, so its event reads MEDIUM at the severity
// column, not only as an override hint in the details JSON.
func (r *WakeTableRunner) emitSev(ctx context.Context, w *database.Wake, severity EventSeverity, eventType, message string, extra map[string]any) {
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
	r.events.Emit(ctx, severity, "wake_table", message, details)
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
