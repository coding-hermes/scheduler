package scheduler

// SCHED-GAP-1764 — wake coalescing: a dry_run/no_changes lane skips
// board-wake re-ticks for a coalesce window.
//
// Evidence (2026-11-16): voice-mode-foreman logged 14 dry_run (no_changes,
// exit 0) ticks in one day, re-ticking every ~8 minutes during card-manager
// board-write storms. Every one of those ticks was a board-wake-forced
// re-evaluation of a lane that had just concluded it had nothing to do — the
// watcher added a trigger, the packer admitted it (tasks mode is work-driven,
// not cooldown-driven), and the lane spent tokens proving the same nothing.
//
// Shape: before a fired wake forces an evaluation, the watcher reads the
// lane's LAST concluded tick (ticks table, READ-ONLY) and asks: did it close
// dry_run with error=no_changes, and is that close younger than the lane's
// coalesce window (default 1x its cooldown_s)? If yes the wake is CANCELLED —
// no ForceEvaluate, no board_wake nudge stamp — and exactly ONE WAKE-COALESCE
// line records the skip (lane, last dry_run tick id, window, remaining) so
// the suppression is auditable rather than silent.
//
// THE LAW (G1/G7, see board_wake.go) still holds:
//   - detection is a READ of the ticks table; every read failure (query
//     error, unparseable completed_at, no concluded tick) fails OPEN — no
//     suppression, the wake proceeds exactly as today;
//   - only the WATCHER's own added trigger is dropped. The lane's clock
//     cadence, the interval ladder and every cooldown/admission gate are
//     untouched, so a spawn is never vetoed: at most one board-wake trigger
//     is skipped, and the default window is the lane's own cooldown_s;
//   - manual/API-triggered spawns and owner actions never pass through this
//     seam — they call Loop.ForceEvaluate / the operator spawn path directly
//     — so they always bypass suppression (the seam is consulted in exactly
//     one place: BoardWakeWatcher.wake).
//
// Config surface: the window is built-in (1x cooldown_s) with a fleet-wide
// override — `[scheduler] wake_coalesce_s` in the root TOML, applied at boot
// — plus an in-memory per-lane override (SetWakeCoalesceWindowFor). A PER-LANE
// PERSISTED override needs a projects column (a schema migration); that is a
// documented follow-up, deliberately not part of this row.

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/coding-hermes/scheduler/internal/database"
)

const (
	// boardWakeCoalesceDefaultFactor is the built-in window multiplier: a
	// dry_run/no_changes lane's board-wake re-ticks are suppressed for 1x its
	// cooldown_s. Matching the cooldown means coalescing never delays a lane
	// past its own scheduled cadence; it only removes the extra wake-driven
	// triggers that land inside that cadence.
	boardWakeCoalesceDefaultFactor = 1

	// boardWakeNoChangesError is the lifecycle finalization's named reason for
	// a COMPLETED tick that landed no artifact of any kind (lifecycle.go
	// stamps outcome=dry_run + error=no_changes for exactly that class; the
	// same row records result_missing / verification_not_observed for its
	// causes, which are NOT coalesced — those are harness faults, not proof
	// the lane has nothing to do).
	boardWakeNoChangesError = "no_changes"

	// boardWakeCoalesceComponent is the events-table component for the
	// suppression line. Deliberately NOT "board_wake": the existing wake
	// event records the board WRITE, this one records the SKIPPED spawn.
	boardWakeCoalesceComponent = "wake_coalesce"

	// boardWakeCoalesceLinePrefix is the grep-stable log prefix, the same
	// register as NO-WORK / LOAD-GATE / BUILDER-GUARD.
	boardWakeCoalesceLinePrefix = "WAKE-COALESCE"
)

// boardWakeLastTick is the read-side view of one lane's last concluded tick —
// only what the coalesce predicate and its window need.
type boardWakeLastTick struct {
	TickID string
	// Outcome and Error are the ticks columns verbatim (COALESCEd to "").
	Outcome string
	Error   string
	// CompletedAt is the parsed finalization stamp; the ZERO value means
	// absent or unparseable, and the caller fails open on it.
	CompletedAt time.Time
	// Cooldown is the lane's cooldown_s (backing value for the default
	// window). Zero means the row carries none — the window then resolves
	// to 0 and no suppression applies.
	Cooldown time.Duration
}

// noChange is the coalesce predicate: the tick FINISHED (dry_run) with the
// named no-artifact reason. Any other outcome/reason is ordinary operation
// and must not be coalesced.
func (lt boardWakeLastTick) noChange() bool {
	return lt.Outcome == string(database.OutcomeDryRun) && lt.Error == boardWakeNoChangesError
}

// boardWakeLastTickFromDB reads the lane's most recently spawned CONCLUDED
// tick and its cooldown. rowid order is spawn order (a tick row is inserted
// when it is queued), so the terminal row with the highest rowid is the last
// tick that concluded. Read-only by contract: the caller treats every error
// as "no suppression" (fail-open).
//
// A lane with no concluded tick at all returns sql.ErrNoRows, which the
// caller reads as "nothing to coalesce against", never as a fault.
func boardWakeLastTickFromDB(ctx context.Context, db *sql.DB, project string) (boardWakeLastTick, error) {
	var (
		lt       boardWakeLastTick
		stamp    string
		cooldown int
	)
	err := db.QueryRowContext(ctx, `
		SELECT t.id,
		       COALESCE(t.outcome, ''),
		       COALESCE(t.error, ''),
		       COALESCE(t.completed_at, ''),
		       COALESCE(p.cooldown_s, 0)
		  FROM ticks t
		  JOIN projects p ON p.name = t.project_name
		 WHERE t.project_name = ?
		   AND t.status IN ('completed', 'failed', 'timeout', 'aborted', 'deferred')
		 ORDER BY t.rowid DESC
		 LIMIT 1`, project).Scan(&lt.TickID, &lt.Outcome, &lt.Error, &stamp, &cooldown)
	if err != nil {
		return boardWakeLastTick{}, err
	}
	if at, ok := parseBoardStamp(stamp); ok {
		lt.CompletedAt = at
	}
	lt.Cooldown = time.Duration(cooldown) * time.Second
	return lt, nil
}

// boardWakeCoalesceEntry is one lane's live suppression window.
type boardWakeCoalesceEntry struct {
	tickID  string
	window  time.Duration
	expires time.Time
}

// boardWakeCoalescer is the watcher's in-memory suppression state: a map keyed
// by lane with an expiry per entry, plus the window-resolution knobs. Guarded
// by one mutex — the watcher's wake path is single-goroutine, so contention is
// nil, and the mutex keeps the knob setters (boot/test) race-free against it.
type boardWakeCoalescer struct {
	mu sync.Mutex
	// lanes holds the live windows (a table with no rows is a read-only map:
	// remember lazily allocates it so a zero-value coalescer is usable).
	lanes map[string]boardWakeCoalesceEntry
	// global is the fleet-wide override (0 = built-in 1x cooldown_s).
	global time.Duration
	// perLane is the in-memory per-lane override. A present entry WINS over
	// global and default, including an explicit 0 = "never coalesce this
	// lane".
	perLane map[string]time.Duration
}

// setGlobal installs the fleet-wide window (0 restores the built-in default).
// Live windows are dropped: they were computed under the previous resolution,
// and a knob change must take effect on the very next wake rather than after
// the old window drains.
func (c *boardWakeCoalescer) setGlobal(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.global = d
	c.lanes = nil
}

// setLane installs a per-lane override (0 = coalescing disabled for the lane).
// Live windows are dropped for the same reason as setGlobal.
func (c *boardWakeCoalescer) setLane(lane string, d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.perLane == nil {
		c.perLane = make(map[string]time.Duration)
	}
	c.perLane[lane] = d
	c.lanes = nil
}

// windowFor resolves the coalesce window for one lane: per-lane override →
// fleet-wide override → 1x the lane's cooldown_s. A resolved window <= 0
// disables suppression for the lane (fail-open: never suppress on a bad
// window).
func (c *boardWakeCoalescer) windowFor(lane string, cooldown time.Duration) time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	if d, ok := c.perLane[lane]; ok {
		return d
	}
	if c.global > 0 {
		return c.global
	}
	return time.Duration(boardWakeCoalesceDefaultFactor) * cooldown
}

// live returns the lane's unexpired entry. An expired entry is evicted on
// read so the map cannot grow stale windows.
func (c *boardWakeCoalescer) live(lane string, now time.Time) (boardWakeCoalesceEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.lanes[lane]
	if !ok {
		return boardWakeCoalesceEntry{}, false
	}
	if !now.Before(e.expires) {
		delete(c.lanes, lane)
		return boardWakeCoalesceEntry{}, false
	}
	return e, true
}

// remember records a live window and prunes any entries that have expired.
func (c *boardWakeCoalescer) remember(lane string, e boardWakeCoalesceEntry, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.lanes == nil {
		c.lanes = make(map[string]boardWakeCoalesceEntry)
	}
	for k, v := range c.lanes {
		if !now.Before(v.expires) {
			delete(c.lanes, k)
		}
	}
	c.lanes[lane] = e
}

// forget drops the lane's recorded window. The lane's most recently concluded
// tick is no longer a coalescible no-change close (it committed, failed, or the
// window ran out), so suppression state from an older conclusion must not
// survive into the next wake.
func (c *boardWakeCoalescer) forget(lane string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.lanes, lane)
}

// boardWakeCoalesceSkip names one suppressed wake.
type boardWakeCoalesceSkip struct {
	LastTickID string
	Window     time.Duration
	Remaining  time.Duration
}

// line renders the grep-stable tick line (the noWorkTickLine register): lane,
// last dry_run tick id, window and remaining suppression in ONE line.
func (s boardWakeCoalesceSkip) line(project string) string {
	return fmt.Sprintf("%s: %s last_dry_run_tick=%s window=%s remaining=%s — board-wake re-tick suppressed",
		boardWakeCoalesceLinePrefix, project, s.LastTickID,
		s.Window.Round(time.Second), s.Remaining.Round(time.Second))
}

// SetWakeCoalesceWindow sets the fleet-wide coalesce window applied to every
// lane without its own override. 0 restores the built-in 1x cooldown_s. Call
// before Start; safe at any time (the coalescer is mutex-guarded) but the
// production wiring is boot-time.
func (w *BoardWakeWatcher) SetWakeCoalesceWindow(d time.Duration) {
	w.coalesce.setGlobal(d)
}

// SetWakeCoalesceWindowFor sets a per-lane coalesce window, overriding the
// fleet-wide/default resolution for that lane; 0 disables coalescing for the
// lane. In-memory only: persisting a per-lane window needs a projects column
// (schema follow-up), so this is the operator/test seam until then.
func (w *BoardWakeWatcher) SetWakeCoalesceWindowFor(project string, d time.Duration) {
	if project == "" {
		return
	}
	w.coalesce.setLane(project, d)
}

// coalesceSkip decides whether THIS wake must be suppressed for project.
//
// The lane's last concluded tick is re-read on EVERY wake: it is the
// authoritative evidence, so an intervening tick that committed (or failed, or
// moved the window) can never be masked by stale in-memory state. The
// coalescer's map records the current window (with its expiry) and is reused
// only when it belongs to the SAME conclusion the read just returned — it is a
// record of live suppression state, never a substitute for the read.
//
// Fail-open by construction: every unreadable fact (no concluded tick, query
// error, unparseable stamp, non-positive window) answers "not suppressed" and
// leaves the wake's normal behavior intact.
func (w *BoardWakeWatcher) coalesceSkip(project string) (boardWakeCoalesceSkip, bool) {
	if w == nil || project == "" || w.db == nil {
		return boardWakeCoalesceSkip{}, false
	}
	now := w.clock().Now()
	read := w.lastTick
	if read == nil {
		read = boardWakeLastTickFromDB
	}
	lt, err := read(context.Background(), w.db, project)
	if err != nil {
		if err == sql.ErrNoRows {
			// No concluded tick: nothing to coalesce against.
			w.coalesce.forget(project)
			return boardWakeCoalesceSkip{}, false
		}
		log.Printf("%s: %s last-tick read failed (fail-open, no suppression): %v",
			boardWakeCoalesceLinePrefix, project, err)
		return boardWakeCoalesceSkip{}, false
	}
	if !lt.noChange() || lt.CompletedAt.IsZero() {
		w.coalesce.forget(project)
		return boardWakeCoalesceSkip{}, false
	}
	window := w.coalesce.windowFor(project, lt.Cooldown)
	if window <= 0 {
		w.coalesce.forget(project)
		return boardWakeCoalesceSkip{}, false
	}
	expires := lt.CompletedAt.Add(window)
	if !now.Before(expires) {
		w.coalesce.forget(project)
		return boardWakeCoalesceSkip{}, false
	}
	// A recorded window is reusable only for the conclusion it was computed
	// from; a DIFFERENT tick id means the lane concluded again and the
	// recorded window (and its remaining time) no longer applies.
	if e, ok := w.coalesce.live(project, now); ok && e.tickID == lt.TickID {
		return boardWakeCoalesceSkip{LastTickID: e.tickID, Window: e.window, Remaining: e.expires.Sub(now)}, true
	}
	entry := boardWakeCoalesceEntry{tickID: lt.TickID, window: window, expires: expires}
	w.coalesce.remember(project, entry, now)
	return boardWakeCoalesceSkip{LastTickID: lt.TickID, Window: window, Remaining: expires.Sub(now)}, true
}
