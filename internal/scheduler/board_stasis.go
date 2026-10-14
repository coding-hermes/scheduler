package scheduler

import (
	"fmt"
	"log"
	"os"
	"sync"

	"github.com/coding-hermes/scheduler/internal/database"
)

// SCHED-GAP-1678 — the board-stasis spawn gate.
//
// Measured problem (2026-09-29, 20 lanes ticking >=10x/48h): a tick fired on
// a board that has NOT changed since the lane's previous tick no-ops 69% of
// the time (164/235) versus 25% (66/260) when the board moved, and the
// no-ops are the EXPENSIVE ticks ($4.68 avg sticker vs $2.43 for productive
// ones). The daemon already knows a board is stale (board_freshness.go,
// board_rows_seen, the board-wake watcher's mtime poll) but nothing gated
// the spawn on it.
//
// This gate closes the gap at SELECTION time: when a lane's board file has
// not changed since the previous tick's spawn instant, the packers exclude
// the lane and no full LLM tick is fired.
//
// THE SIGNAL (PERF-002 budget): a fingerprint of the board file —
// mtime + size, one os.Stat per lane per pass. It deliberately does NOT run
// the git battery (board_freshness.go) and does not even consult the
// FreshnessVerdictCache's HEAD probe: the measured correlation is between
// BOARD WRITES and no-op ticks, and a board write moves mtime. A landed
// commit that touches no board file does not make a stale-board tick
// productive — the 69% figure already includes those. Zero subprocesses;
// strictly cheaper than the one-exec cache-hit path the PERF-002 verdict
// cache guarantees.
//
// THE BASELINE is recorded at SPAWN time (Loop.evaluate's spawn loop and
// Loop.SpawnNow), keyed by lane, carrying the fingerprint observed at the
// instant the tick was dispatched plus that tick's id. A later pass whose
// fingerprint still equals the baseline proves the board is unchanged since
// the previous tick started — exactly the window the measurement joined on
// (board-change timestamps vs tick timestamps). The skip log names the
// previous tick's id: "board unchanged since tick <id>".
//
// ESCAPE HATCHES (fail-open, every one of them spawns):
//   - no known baseline (first tick — the board_rows_seen=-1 sentinel
//     class): nothing to compare against, spawn;
//   - the board is missing/unreadable at decision time: no evidence of
//     stasis, spawn (the fleet-wide convention an unreadable board signal
//     never defers a tick);
//   - the previous tick did not COMPLETE (failed / timeout / deferred): the
//     tick may have died before reading the board at all, so stasis proves
//     nothing — the packers only apply the gate when last_tick_status is
//     "completed", and a deferred retry is never stranded behind a gate;
//   - ForceEvaluate (an operator's explicit evaluate): the loop disables
//     the gate for that pass — an operator ask always spawns;
//   - the gate itself is OFF: NewLoop constructs it disabled, so every
//     existing entry point keeps its byte-identical behavior until the
//     daemon turns it on (Loop.SetBoardStasisGate).
//
// SCOPE (order/selection only — SCHED-GAP-1660/ADV-R07 unchanged): the gate
// is an admission filter in the packers' greedy loops, AFTER the cooldown
// gates. A board write still reorders/boosts exactly as ADV-R07 wired it,
// and can never re-fire a lane inside its cooldown — the gate sits strictly
// downstream of the pin, so it can only ever REMOVE a spawn the pin had
// already released. It is not a cooldown change: no cooldown_s is consumed,
// ratcheted or extended, no failure is recorded, and the lane keeps its
// selection contract (the next pass re-decides from the live fingerprint).
//
// LANE SCOPE — cooldown-mode BUILDER lanes only, the same conjunction the
// SCHED-GAP-1655 no-work gate uses:
//   - tasks-mode lanes are ungated: the SCHED-GAP-124 waiver exists precisely
//     to re-fire a lane on standing board work; a tasks lane working down a
//     backlog on an unchanged board is the waiver working, not waste;
//   - REPORTER-class lanes (the satellite suffix families or a namespace
//     reporter_class pin) are ungated: their product is a periodic report,
//     their timer cadence is BY DESIGN (deliverable 3), and an unchanged
//     board is not a fault for them.
//
// The measured offenders — release-engineer, the python-audit-* family —
// are builder-class cooldown lanes, exactly the conjunction this gate
// refuses. Criterion 6 of the row ("no lane fires more than twice while
// its board is unchanged") is achieved strictly: the gate holds the count
// at zero until the board moves, the previous tick stops completing, or an
// operator forces an evaluation.
//
// OBSERVABILITY: every skip logs the grep-stable BOARD-UNCHANGED line
// (operators watch it the way they watch NO-WORK / LOAD-GATE), increments
// a process-lifetime counter surfaced on /api/v1/status as
// board_unchanged_skips, and emits an ADMIT line with the
// board_unchanged reason — so a skipped tick is never confused with a
// dry_run that spent money: it never created a tick row at all.

// AdmissionReasonBoardUnchanged is the SCHED-GAP-1678 vocabulary entry: a
// cooldown-mode BUILDER lane whose board file has not changed since its
// previous tick's spawn instant was excluded from selection by the
// board-stasis gate. Carries no cooldown_remaining_s (the pin had already
// elapsed — the gate sits strictly downstream of it) and is distinct from
// no_work (an EMPTY board) and from a dry_run (which ran and spent money):
// this tick was never created.
const AdmissionReasonBoardUnchanged = "board_unchanged"

// boardFingerprint returns a change-sensitive fingerprint of the lane's
// board file: mtime + size, the observable surface a board write always
// moves. ok is false when the lane has no board file or it cannot be
// stat'ed — the fail-open signal (no evidence of stasis, never a reason to
// skip). One os.Stat; no subprocess (PERF-002: never the git battery).
func boardFingerprint(workdir string) (fp string, ok bool) {
	if workdir == "" {
		return "", false
	}
	boardPath, hasBoard := findBoardFile(workdir)
	if !hasBoard {
		return "", false
	}
	fi, err := os.Stat(boardPath)
	if err != nil {
		return "", false
	}
	return fmt.Sprintf("%d:%d", fi.ModTime().UnixNano(), fi.Size()), true
}

// boardBaseline is one lane's recorded spawn-time board state: the
// fingerprint observed when the previous tick was dispatched, and that
// tick's id (the "<tick id>" the skip log names).
type boardBaseline struct {
	fingerprint string
	tickID      string
}

// BoardStasisGate is the SCHED-GAP-1678 spawn gate: per-lane spawn-time
// board baselines plus the enabled switch the evaluation pass arms. All
// methods are safe for concurrent use; the gate holds no clock (the
// fingerprint is clock-free), so no seam is installed.
//
// It is deliberately disabled at construction: NewLoop builds it inert so
// every existing selection contract is byte-identical until the daemon (or
// a test) turns it on with SetEnabled.
type BoardStasisGate struct {
	mu        sync.Mutex
	enabled   bool
	baselines map[string]boardBaseline
	skips     int
}

// NewBoardStasisGate creates the gate, disabled, with no baselines.
func NewBoardStasisGate() *BoardStasisGate {
	return &BoardStasisGate{baselines: make(map[string]boardBaseline)}
}

// SetEnabled arms or disarms the gate for the current evaluation pass. The
// Loop's evaluate() arms it from the loop-level default (xor the
// ForceEvaluate bypass) before the packers run and every packer consults
// it inside the same pass, so the switch is pass-scoped in practice; the
// mutex only makes the write itself safe.
func (g *BoardStasisGate) SetEnabled(on bool) {
	g.mu.Lock()
	g.enabled = on
	g.mu.Unlock()
}

// Enabled reports whether the gate is armed for the pass in flight.
func (g *BoardStasisGate) Enabled() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.enabled
}

// RecordSpawn records the lane's board fingerprint AT SPAWN TIME together
// with the tick being dispatched — the baseline the next pass compares
// against. Called from the evaluation pass's spawn loop (real and sim) and
// from Loop.SpawnNow, always after the gate decision, for every spawn the
// pass fires (a load-gate / gateway deferral records nothing: the spawn did
// not happen, so the next pass must decide from the pre-deferral state —
// fail-open).
func (g *BoardStasisGate) RecordSpawn(lane, workdir, tickID string) {
	fp, ok := boardFingerprint(workdir)
	if !ok {
		return // no board: nothing observable to baseline
	}
	g.mu.Lock()
	g.baselines[lane] = boardBaseline{fingerprint: fp, tickID: tickID}
	g.mu.Unlock()
}

// ClearBaseline drops the lane's baseline so the next pass cannot skip it.
// Kept for completion-side callers that know a tick died before it could
// observe the board (the packers additionally treat any non-completed
// last_tick_status as gate-transparent, so this is belt-and-braces for
// direct gate users).
func (g *BoardStasisGate) ClearBaseline(lane string) {
	g.mu.Lock()
	delete(g.baselines, lane)
	g.mu.Unlock()
}

// AdmissionBlocked is the gate decision for ONE candidate: has this lane's
// board file not changed since its previous tick's spawn instant?
//
// blocked is false — spawn — unless every leg of the conjunction holds: the
// gate is armed, a baseline exists (first tick never blocks), the board is
// readable now (unreadable never blocks), and the fingerprint still equals
// the spawn-time baseline. prevTickID is the previous tick's id for the
// skip log; "" whenever blocked is false.
func (g *BoardStasisGate) AdmissionBlocked(lane, workdir string) (blocked bool, prevTickID string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.enabled {
		return false, ""
	}
	base, ok := g.baselines[lane]
	if !ok {
		return false, "" // no known baseline (first tick): spawn
	}
	fp, ok := boardFingerprint(workdir)
	if !ok {
		return false, "" // unreadable board: no evidence of stasis — spawn
	}
	if fp != base.fingerprint {
		return false, "" // the board moved since the previous tick: spawn
	}
	return true, base.tickID
}

// noteSkip records one gate skip for /api/v1/status and logs the
// grep-stable line naming the previous tick. The reason phrase
// "board unchanged since tick <id>" is the row's required log contract.
// Called only from the packers' selection sites (once per lane per pass —
// the borrowing/queued mirrors consult the gate silently, mirroring the
// SCHED-GAP-1655 no-work gate's split).
func (g *BoardStasisGate) noteSkip(lane, prevTickID string) {
	g.mu.Lock()
	g.skips++
	g.mu.Unlock()
	log.Printf("BOARD-UNCHANGED: skipping %s — board unchanged since tick %s (SCHED-GAP-1678)", lane, prevTickID)
}

// Skips returns the process-lifetime count of board-unchanged skips.
// Surfaced on /api/v1/status as board_unchanged_skips so a skipped tick is
// never confused with a dry_run that spent money: the counter increments
// without a tick row ever existing.
func (g *BoardStasisGate) Skips() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.skips
}

// BoardUnchangedSkips returns the process-lifetime count of board-unchanged
// skips (SCHED-GAP-1678), read by /api/v1/status as board_unchanged_skips.
// Zero on a loop built before any gate was wired (struct-literal tests).
func (l *Loop) BoardUnchangedSkips() int {
	if l.boardStasisGate == nil {
		return 0
	}
	return l.boardStasisGate.Skips()
}

// recordBoardStasisSpawn is the loop-side baseline recorder: it records the
// lane's board fingerprint at spawn time on the loop's gate, tolerating a
// nil gate (a struct-literal test Loop). One call per spawn the loop
// actually fires, from every entry point (evaluate's sim path, evaluate's
// slot-pool path, SpawnNow).
func (l *Loop) recordBoardStasisSpawn(lane, workdir, tickID string) {
	if l.boardStasisGate != nil {
		l.boardStasisGate.RecordSpawn(lane, workdir, tickID)
	}
}

// boardStasisBlocks is the packer-side conjunction wrapper: the gate's own
// fingerprint verdict AND the row-scoped legs the packers already hold in
// hand (cooldown admission mode, builder class, completed previous tick).
// One helper so all three selection paths apply the identical predicate —
// the ADV-R03/G5 single-source rule applied to this gate.
//
//   - admissionMode != cooldown: tasks lanes keep the GAP-124 waiver;
//   - reporter-class lanes keep their timer cadence (SCHED-GAP-1655 d3);
//   - lastStatus != completed: a failed/timeout/deferred previous tick
//     must be retried, never stranded behind stasis;
//   - reporterConfig is the namespace reporter_class column as the packers
//     see it ("" in the packer paths — the same value the 1655 gates pass).
func boardStasisBlocks(g *BoardStasisGate, lane, workdir, admissionMode, lastStatus, reporterConfig string) (blocked bool, prevTickID string) {
	blocked, prevTickID = boardStasisBlockedQuiet(g, lane, workdir, admissionMode, lastStatus, reporterConfig)
	if blocked {
		g.noteSkip(lane, prevTickID)
	}
	return blocked, prevTickID
}

// boardStasisBlockedQuiet is the noteSkip-free mirror of boardStasisBlocks,
// for classification surfaces that must COUNT the skip exactly once per
// pass (the packers' selection site notes it; the admission classifier
// re-derives the same verdict for the ADMIT line and must not double-count).
func boardStasisBlockedQuiet(g *BoardStasisGate, lane, workdir, admissionMode, lastStatus, reporterConfig string) (blocked bool, prevTickID string) {
	if g == nil || admissionMode != database.AdmissionModeCooldown {
		return false, ""
	}
	if !laneIsBuilder(lane, reporterConfig) {
		return false, ""
	}
	if lastStatus != database.LastStatusCompleted {
		return false, ""
	}
	blocked, prevTickID = g.AdmissionBlocked(lane, workdir)
	return blocked, prevTickID
}
