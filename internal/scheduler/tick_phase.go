package scheduler

import (
	"fmt"
	"sync"
	"time"
)

// Tick runner phase vocabulary (SCHED-GAP-16941).
//
// The defect: a tick killed by its own session wall recorded
// "tick-timeout wall 3h0m0s: tick deadline exceeded" and nothing else — the
// row could not say WHICH PHASE consumed the three hours. The phases below
// are the boundaries the runner already has, named — not a parallel state
// machine:
//
//   - tickPhaseSpawn:    setup — chain/endpoint resolution, key probe,
//     prompt build, session ctx arming.
//   - tickPhaseAwait:    the gateway /v1/responses POST is in flight (or,
//     on the exec path, the process is running) — the runner is waiting on
//     the session/turn. Today's wall kills land here by construction: the
//     session ctx is only OBSERVED inside the POST read.
//   - tickPhaseCollect:  the response came back and the runner is
//     processing it — completion gate, telemetry capture, git/commit
//     counting.
//   - tickPhaseFinalize: the outcome is booked; the board/lifecycle write
//     (slot_pool → lifecycle.Complete) is in progress.
const (
	tickPhaseSpawn    = "spawn"
	tickPhaseAwait    = "await"
	tickPhaseCollect  = "collect"
	tickPhaseFinalize = "finalize"
)

// tickPhaseLog records the ONE phase a tick is currently in and the instant
// it entered that phase. It is deliberately tiny — a mutex, a name, and a
// timestamp — so stamping a transition costs one clock read on the hot path
// and nothing else. Timestamps come from the component clock seam, so a test
// drives the phases on the SCHED-GAP-169 simulator instead of sleeping.
//
// The log answers exactly one question, at exactly one kind of site: when the
// wall kills this tick, which phase was it in and for how long. The answer is
// frozen into the kill text AT the kill site (wallAnnotation); transitions
// after that point (collect/finalize) keep the trail truthful for the ticks
// that survive the wall.
//
// All methods are nil-safe: a SpawnedTick built by a path that never armed a
// tracker (tests constructing literals, remote-dispatch legs) carries nil and
// every stamp/read is a no-op.
type tickPhaseLog struct {
	mu      sync.Mutex
	phase   string
	entered time.Time
}

// newTickPhaseLog starts a tick's trail in the spawn phase at `at`
// (the spawner's clock reading at Spawn() entry).
func newTickPhaseLog(at time.Time) *tickPhaseLog {
	return &tickPhaseLog{phase: tickPhaseSpawn, entered: at}
}

// enter transitions the tick into `phase` at instant `at`. A transition to
// the CURRENT phase is a no-op (re-stamping collect inside Wait() after
// Spawn() already entered it must not reset the spent clock).
func (l *tickPhaseLog) enter(phase string, at time.Time) {
	if l == nil {
		return
	}
	l.mu.Lock()
	if l.phase != phase {
		l.phase = phase
		l.entered = at
	}
	l.mu.Unlock()
}

// snapshot returns the current phase and how long the tick has spent in it
// as of `now`. A nil or never-armed log reports ("", 0).
func (l *tickPhaseLog) snapshot(now time.Time) (phase string, spent time.Duration) {
	if l == nil {
		return "", 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	spent = now.Sub(l.entered)
	if spent < 0 {
		spent = 0
	}
	return l.phase, spent
}

// wallAnnotation formats the SCHED-GAP-16941 kill-site suffix,
// "wall-phase=await spent=2h58m0s", or "" when no trail exists. Appended to
// the existing wall text (never replacing it) so the ticks.error row and the
// TICK journal line both name the phase the wall interrupted, readable by
// SQL or a log grep without opening the code.
func (l *tickPhaseLog) wallAnnotation(now time.Time) string {
	phase, spent := l.snapshot(now)
	if phase == "" {
		return ""
	}
	return fmt.Sprintf("wall-phase=%s spent=%s", phase, spent.Round(time.Second))
}
