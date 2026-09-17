package scheduler

import (
	"math/rand"
	"sync"
	"time"
)

// SCHED-GAP-136 — tasks-mode post-tick pacing + gateway Retry-After (Bane
// 2026-09-16):
//
//	"when the mode is in task mode and the task finishes dont spawn at 0ms
//	 but set a 60 second cooldown or something with a jitter but enough that
//	 we dont cause a 503 spawning flood. we need to include a backoff for
//	 calling hermes so that we of course do this correctly."
//
// Two halves:
//
//  1. POST-TICK PACING (this file). Tasks-mode admission (SCHED-GAP-124)
//     waives the cooldown pin whenever the board holds pending work, so the
//     only thing spacing two consecutive ticks was the 5s eval debounce —
//     the "spawn at 0ms" class. GAP-133 added a FAILURE-side gate
//     (consecutive_failures > 1); this adds the SUCCESS-side floor: after
//     ANY terminal tick (last_tick_completed updates on completed, failed
//     AND timeout — eduos-e2e law), a tasks-mode project waits at least
//     tasks-pacing (default 60s via the fleet binary) plus up to 20% jitter
//     before the next admission. Jitter de-synchronizes the herd: several
//     tasks lanes finishing in the same window re-admit spread out instead
//     of slamming the gateway in one eval.
//
//   - ZERO-CONFIG SEMANTICS: the package default is 0 (= off, byte-identical
//     for every unit test and embedding binary). The FLEET default lives in
//     cmd/schedulerd/main.go's flag (--tasks-pacing, 60s) per the
//     AGENTS.md "defaults match main.go" law: the shipped schedulerd paces,
//     the library stays inert unless wired.
//
//   - The pacing floor composes with, never replaces, the GAP-133 failure
//     backoff: healthy path = 60s+jitter floor; failing path = FailureBackoff
//     (base cooldown × 2^(failures-1), 2h cap) which dwarfs the floor from
//     the second failure on.
//
//  2. GATEWAY-CALL BACKOFF lives in gateway_client.go (Retry-After parsed
//     onto GatewayStatusError) + spawn.go (gatewayRetrySleep honors it in
//     the GAP-080 retry loop). See those files.

var (
	tasksPacingMu    sync.RWMutex
	tasksPacingValue time.Duration // 0 = disabled

	// tasksJitterFrac is the jitter randomness seam (test-injectable).
	// Returns [0,1); the applied jitter is tasksPacingJitterFrac × base
	// (default fraction 0.2 → up to +12s at the 60s fleet default).
	tasksJitterFrac = rand.Float64
)

// tasksPacingJitterFrac bounds the added jitter as a fraction of the base
// pacing. A constant, not config: "60 seconds or something with a jitter"
// wants anti-herd spread, not another knob to tune.
const tasksPacingJitterFrac = 0.2

// SetTasksPacing installs the global post-tick pacing floor (0 = disabled).
// Called once from main/loop wiring; last writer wins (same pattern as
// SetLoadGateThreshold, SCHED-GAP-125).
func SetTasksPacing(d time.Duration) {
	tasksPacingMu.Lock()
	defer tasksPacingMu.Unlock()
	if d < 0 {
		d = 0
	}
	tasksPacingValue = d
}

// tasksPacing returns the active pacing floor (0 = disabled).
func tasksPacing() time.Duration {
	tasksPacingMu.RLock()
	defer tasksPacingMu.RUnlock()
	return tasksPacingValue
}

// tasksPacingJitter returns the jittered wait for one admission decision:
// base + up to tasksPacingJitterFrac×base. base <= 0 → 0 (no pacing).
func tasksPacingJitter(base time.Duration) time.Duration {
	if base <= 0 {
		return 0
	}
	return time.Duration(tasksJitterFrac() * tasksPacingJitterFrac * float64(base))
}

// tasksPacingDeferredJittered is the PACKER-side predicate: true while a
// project's last terminal tick is younger than pacing+jitter (admission
// deferred). A nil lastTick (never ticked) is never deferred — the floor is
// explicitly a POST-tick pace, not a first-spawn gate.
func tasksPacingDeferredJittered(lastTick *time.Time, now time.Time) bool {
	base := tasksPacing()
	if base <= 0 || lastTick == nil {
		return false
	}
	return now.Sub(*lastTick) < base+tasksPacingJitter(base)
}

// tasksPacingDeferred is the MIRROR-side predicate for
// Loop.countEligibleProjects (the GAP-050 "identical arithmetic" doctrine):
// base pacing, NO jitter. The packer defers up to 1.2×base; the mirror is
// deliberately slightly MORE conservative (deferred through 1.0×base) so it
// can never count as eligible a project the packer is still pacing — the
// safe direction for the GAP-043 zero-select alarm. The residual divergence
// window (base..1.2×base, ≤12s at the fleet default) undercounts eligible
// by at most one tick cycle and is accepted.
func tasksPacingDeferred(lastTick time.Time, now time.Time) bool {
	base := tasksPacing()
	if base <= 0 {
		return false
	}
	return now.Sub(lastTick) < base
}
