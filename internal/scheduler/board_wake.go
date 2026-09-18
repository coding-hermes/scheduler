package scheduler

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/coding-hermes/scheduler/internal/clock"
)

// Board-driven wake (ADV-R07, Option C).
//
// Problem: the eval loop is event-driven (slot-freed debounce + the GAP-042
// stall watchdog). A project whose board GAINS dispatchable work while the
// fleet is idle waits for the next cooldown expiry to be considered — the
// board write itself never reaches the scheduler. Observed
// implementation→board flip lag is 10–55 min (median 4.4, A4 catalog), so
// freshly-filed work routinely sits past its eligibility.
//
// Fix (Option C — wake + ordering, never veto): a watcher polls the board
// files (tasks.jsonl/tasks.md mtime) of ENABLED projects. A detected write
// arms a per-project wake that fires ~one debounce later (bounded: the
// debounce is never extended by later writes, so a wake lands at most
// debounce+poll after the FIRST observed change), reads the board through
// the R06 git-verified freshness reader, emits a board_wake event carrying
// row id + freshness verdict, and calls ForceEvaluate (loop.go). The
// freshness-checked pending count (CountPending, board_awareness.go) feeds
// the EXISTING pendingBoostUrgencyFor tier — ordering only.
//
// THE LAW (G1/G7): wall-clock cooldown remains the SOLE admission
// authority. Nothing here can veto, skip, or delay a spawn:
//   - the watcher only ADDS evaluation triggers; evaluate() and the packer
//     cooldown gates are untouched;
//   - every watcher/reader error fails OPEN — a board-list failure logs
//     and keeps the clock cadence; an unreadable board in the freshness
//     filter keeps the raw pending count (freshnessCheckedPending);
//   - any future admission gate must live in SlotPool.spawn (slot_pool.go)
//     with a declared override (G7 ruling) — see AGENTS.md.
//
// The watcher watches itself: a separate heartbeat-watchdog goroutine
// (GAP-042 applied to this seam) emits a HIGH board_wake event when the
// poll loop stops beating. Severity is calibrated per the SCHED-GAP-061
// lesson: a dead poller is a genuine fault (not an expected idle-fleet
// recurrence), so onset is HIGH, re-emitted at most once per 30 min while
// the stall persists — the same throttle shape as checkEvalStall. A
// stalled watcher degrades the fleet to today's clock cadence; it can
// never make scheduling worse than absent.

const (
	// boardWakePollInterval is the mtime poll cadence. Polling (not
	// inotify) is deliberate: zero new dependencies, robust across
	// network filesystems, and the debounce dominates the latency
	// anyway. ~134 enabled projects stat every minute is trivial.
	boardWakePollInterval = 60 * time.Second

	// boardWakeDebounce is how long after a first-observed board change
	// the wake waits before forcing evaluation. Bounded by the observed
	// 4.4-min median implementation→board flip lag (A4): inside the
	// window a mid-flip board and its follow-up edits coalesce into one
	// wake. LATER writes never extend an armed wake.
	boardWakeDebounce = 5 * time.Minute

	// boardWakeHeartbeatTick is the watchdog's check cadence.
	boardWakeHeartbeatTick = 60 * time.Second

	// boardWakeStallFactor: the poll loop is stalled when the last beat
	// is older than factor×poll (5 missed polls = 5 min at defaults).
	boardWakeStallFactor = 5

	// boardWakeReEmitGap throttles stall-event re-emission while a stall
	// persists — mirrors evalStallReEmitGap (SCHED-GAP-061 shape).
	boardWakeReEmitGap = 30 * time.Minute
)

// boardWakeTarget is one enabled project with an existing board file.
type boardWakeTarget struct {
	project   string
	workdir   string
	boardPath string
}

// BoardWakeWatcher watches enabled projects' board files and forces a
// re-evaluation shortly after a board write. Construct with
// NewBoardWakeWatcher, optionally tune with SetIntervals (tests), then
// Start. Stop is idempotent and waits for both goroutines.
type BoardWakeWatcher struct {
	// clk is this component's time seam (SCHED-GAP-169). The zero value
	// reads as the wall clock; NewLoop propagates its own clock here so a
	// test that installs a simulator clock drives the whole component tree,
	// not just evaluate().
	clk       clockSeam
	db        *sql.DB
	events    *EventLogger
	forceEval func()

	// Intervals — set before Start (the `go` statement publishes them;
	// never mutated afterwards).
	poll       time.Duration
	debounce   time.Duration
	heartbeat  time.Duration
	freshnessO FreshnessOptions // zero value = time.Now read clock (production)

	// listBoards is the enabled-projects board enumeration. A field so
	// fault-injection tests can replace it (AC2); production uses
	// listBoardsFromDB.
	listBoards func(context.Context) (map[string]boardWakeTarget, error)

	mu           sync.Mutex
	baselined    bool                 // first successful enumeration recorded
	mtimes       map[string]time.Time // project → last-seen board mtime (zero = no board)
	pendingWake  map[string]time.Time // project → first-observed change instant (armed)
	lastBeat     time.Time            // poll-loop heartbeat (watchdog observable)
	lastStallEvt time.Time            // last stall-event emit (throttle)
	stopCh       chan struct{}
	wg           sync.WaitGroup
}

// NewBoardWakeWatcher creates the watcher. forceEvaluate is called once
// per fired wake (production wiring passes Loop.ForceEvaluate; the loop's
// own GAP-101 choke point makes it a no-op while the daemon is paused).
func NewBoardWakeWatcher(db *sql.DB, forceEvaluate func()) *BoardWakeWatcher {
	w := &BoardWakeWatcher{
		db:          db,
		events:      NewEventLogger(db),
		forceEval:   forceEvaluate,
		poll:        boardWakePollInterval,
		debounce:    boardWakeDebounce,
		heartbeat:   boardWakeHeartbeatTick,
		mtimes:      make(map[string]time.Time),
		pendingWake: make(map[string]time.Time),
	}
	w.listBoards = w.listBoardsFromDB
	return w
}

// SetIntervals overrides the poll/debounce/heartbeat cadences. Must be
// called before Start (test seam for fast, deterministic cycles).
func (w *BoardWakeWatcher) SetIntervals(poll, debounce, heartbeat time.Duration) {
	w.poll = poll
	w.debounce = debounce
	w.heartbeat = heartbeat
}

// listBoardsFromDB enumerates enabled projects that currently have a
// board file. Projects without boards are simply absent — absence is an
// ordinary condition, never an error (fail-open).
func (w *BoardWakeWatcher) listBoardsFromDB(ctx context.Context) (map[string]boardWakeTarget, error) {
	rows, err := w.db.QueryContext(ctx,
		`SELECT name, COALESCE(workdir, '') FROM projects WHERE enabled = 1`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	targets := make(map[string]boardWakeTarget)
	for rows.Next() {
		var name, workdir string
		if err := rows.Scan(&name, &workdir); err != nil {
			continue // malformed row — never fatal (fail-open)
		}
		if boardPath, ok := findBoardFile(workdir); ok {
			targets[name] = boardWakeTarget{project: name, workdir: workdir, boardPath: boardPath}
		}
	}
	return targets, rows.Err()
}

// Start launches the poll loop and the heartbeat watchdog. The initial
// baseline scan records current mtimes WITHOUT waking, so a watcher
// restart never fires a wake for pre-existing board state. Idempotent.
func (w *BoardWakeWatcher) Start() {
	w.mu.Lock()
	if w.stopCh != nil {
		w.mu.Unlock()
		return
	}
	w.stopCh = make(chan struct{})
	w.lastBeat = w.clock().Now()
	w.mu.Unlock()

	// Baseline: record what exists now; changes are only wakes from
	// here on. A baseline failure is logged and left un-baselined — the
	// FIRST successful poll then becomes the baseline (no wake), so a
	// transient DB hiccup at startup never fires a mass wake.
	if targets, err := w.listBoards(context.Background()); err != nil {
		log.Printf("BOARD-WAKE: baseline scan failed (fail-open, first successful poll re-baselines): %v", err)
	} else {
		w.recordMtimes(targets)
	}

	// Capture the channel for the goroutines: they must never re-read
	// the field (Stop nils it after they exit — a dynamic read could
	// select on nil and hang forever).
	stopCh := w.stopCh
	w.wg.Add(2)
	go w.run(stopCh)
	go w.runWatchdog(stopCh)
}

// recordMtimes stores the current mtime of every target's board without
// arming any wake (the baseline path).
func (w *BoardWakeWatcher) recordMtimes(targets map[string]boardWakeTarget) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for name, t := range targets {
		w.mtimes[name] = statMtime(t.boardPath)
	}
	w.baselined = true
}

// Stop terminates both goroutines and waits. Idempotent. The channel is
// closed once; the field is cleared only AFTER wg.Wait confirms both
// goroutines have exited, so no select can ever observe a nil channel.
func (w *BoardWakeWatcher) Stop() {
	w.mu.Lock()
	stopCh := w.stopCh
	if stopCh == nil {
		w.mu.Unlock()
		return
	}
	w.stopCh = nil
	close(stopCh)
	w.mu.Unlock()
	w.wg.Wait()
}

// run is the poll loop: every poll tick, scan board mtimes, arm wakes on
// changes, fire wakes whose debounce has elapsed, and beat the heartbeat.
func (w *BoardWakeWatcher) run(stopCh <-chan struct{}) {
	defer w.wg.Done()
	ticker := w.clock().NewTicker(w.poll)
	defer ticker.Stop()
	for {
		select {
		case <-stopCh:
			return
		case <-ticker.C:
			w.pollOnce(w.clock().Now())
		}
	}
}

// pollOnce is one poll cycle at instant now (parameterized so tests drive
// the debounce arithmetic deterministically). Fail-open contract: a board
// enumeration failure logs and beats — it can never panic, block, or
// synthesize a wake.
func (w *BoardWakeWatcher) pollOnce(now time.Time) {
	targets, err := w.listBoards(context.Background())
	w.mu.Lock()
	if err != nil {
		// Fail-open (AC2): no wake, no crash, cadence unchanged. The
		// poll loop itself is alive — beat so the watchdog stays quiet.
		log.Printf("BOARD-WAKE: board enumeration failed (fail-open, no wake): %v", err)
		w.lastBeat = now
		w.mu.Unlock()
		return
	}
	if !w.baselined {
		// Startup baseline failed (fail-open) — THIS successful
		// enumeration is the baseline; changes are wakes from here on.
		for name, t := range targets {
			w.mtimes[name] = statMtime(t.boardPath)
		}
		w.baselined = true
		w.lastBeat = now
		w.mu.Unlock()
		return
	}
	for name, t := range targets {
		mtime := statMtime(t.boardPath)
		prev, had := w.mtimes[name]
		w.mtimes[name] = mtime
		if mtime.IsZero() {
			continue // board gone since last poll — a removal adds no work
		}
		if had && mtime.Equal(prev) {
			continue // unchanged
		}
		// !had: a board file that appeared since the baseline — a new
		// board is itself a write (freshly-filed work).
		if _, armed := w.pendingWake[name]; !armed {
			w.pendingWake[name] = now
			log.Printf("BOARD-WAKE: %s board changed — wake armed (debounce %v)", name, w.debounce)
		}
		// Already armed: a later write inside the window must NOT
		// extend the debounce (bounded wake law).
	}
	fire := make([]string, 0, len(w.pendingWake))
	for name, changedAt := range w.pendingWake {
		if now.Sub(changedAt) >= w.debounce {
			delete(w.pendingWake, name)
			fire = append(fire, name)
		}
	}
	w.lastBeat = now
	w.mu.Unlock()

	for _, name := range fire {
		if t, ok := targets[name]; ok {
			w.wake(t)
		}
	}
}

// wake handles one fired debounce: read the board through the R06
// git-verified freshness reader, emit the board_wake event (row id +
// freshness verdict per row — the AC3 contract), and force evaluation.
// The freshness read itself is fail-safe by construction (an unreadable
// board yields a zero report; the event still fires, evaluate still
// runs).
func (w *BoardWakeWatcher) wake(t boardWakeTarget) {
	opts := w.freshnessO
	rep := ReadBoardFreshness(t.workdir, t.boardPath, opts)
	rows := make([]map[string]string, 0, len(rep.Rows))
	for _, v := range rep.Rows {
		rows = append(rows, map[string]string{"row_id": v.ID, "verdict": string(v.Status)})
	}
	w.events.Emit(context.Background(), SeverityInfo, "board_wake",
		fmt.Sprintf("board write on %s — forced re-evaluation", t.project),
		map[string]any{
			"project":         t.project,
			"board":           t.boardPath,
			"work_to_spawn":   rep.WorkToSpawn,
			"verifiably_idle": rep.VerifiablyIdle,
			"can_verify":      rep.CanVerify,
			"total_rows":      rep.TotalRows,
			"malformed_lines": rep.MalformedLines,
			"rows":            rows,
		})
	log.Printf("BOARD-WAKE: %s wake — forced re-evaluation (work_to_spawn=%t, rows=%d, can_verify=%t)",
		t.project, rep.WorkToSpawn, rep.TotalRows, rep.CanVerify)
	if w.forceEval != nil {
		w.forceEval()
	}
}

// runWatchdog is the watcher's own heartbeat watchdog (GAP-042 applied to
// this seam): an independent goroutine whose ticker always fires, so a
// dead or wedged poll loop surfaces even though the poll loop itself can
// never notice.
func (w *BoardWakeWatcher) runWatchdog(stopCh <-chan struct{}) {
	defer w.wg.Done()
	ticker := w.clock().NewTicker(w.heartbeat)
	defer ticker.Stop()
	for {
		select {
		case <-stopCh:
			return
		case <-ticker.C:
			w.checkWatchdog(w.clock().Now())
		}
	}
}

// checkWatchdog emits a HIGH board_wake stall event when the poll loop's
// last beat is older than boardWakeStallFactor×poll. GAP-061
// calibration: unlike the idle-fleet eval-stall cadence, a silent poll
// loop is always a genuine fault, so onset is HIGH (never demoted);
// persistence is throttled to one event per boardWakeReEmitGap. Recovery
// is implicit — a fresh beat drops the age below the threshold with no
// event, and the next stall after the gap is a fresh onset.
func (w *BoardWakeWatcher) checkWatchdog(now time.Time) {
	w.mu.Lock()
	last := w.lastBeat
	threshold := w.poll * boardWakeStallFactor
	if last.IsZero() || now.Sub(last) < threshold {
		w.mu.Unlock()
		return
	}
	emit := w.lastStallEvt.IsZero() || now.Sub(w.lastStallEvt) >= boardWakeReEmitGap
	if emit {
		w.lastStallEvt = now
	}
	w.mu.Unlock()
	if !emit {
		return
	}
	age := now.Sub(last)
	log.Printf("BOARD-WAKE-WATCHDOG: poll loop silent for %v (threshold %v) — board-driven wake dead, clock cadence unaffected",
		age.Round(time.Second), threshold)
	w.events.Emit(context.Background(), SeverityHigh, "board_wake",
		"board watcher stalled — board-driven wake dead, clock cadence unaffected",
		map[string]any{
			"last_beat":   last.UTC().Format(time.RFC3339),
			"age_seconds": age.Seconds(),
			"threshold_s": threshold.Seconds(),
			"poll_s":      w.poll.Seconds(),
			"debounce_s":  w.debounce.Seconds(),
		})
}

// HeartbeatAge returns how long ago the poll loop last beat (zero when
// never). Exposed for diagnostics and tests.
func (w *BoardWakeWatcher) HeartbeatAge() time.Duration {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.lastBeat.IsZero() {
		return 0
	}
	return w.clock().Since(w.lastBeat)
}

// SetClock installs the clock this BoardWakeWatcher reads and waits on (SCHED-GAP-169).
// nil keeps the wall clock.
func (w *BoardWakeWatcher) SetClock(c clock.Clock) { w.clk.Set(c) }

// clock returns the component's clock, never nil.
func (w *BoardWakeWatcher) clock() clock.Clock { return w.clk.Get() }
