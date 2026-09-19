package scheduler

// SCHED-GAP-170 — gateway-health admission gate (the defer-not-drop contract,
// applied to the gateway half of the admission point).
//
// THE DEFECT (first-hand code + live-data evidence, 2026-09-18). evaluate()
// has carried a "gateway liveness ping" since FIX-STUCK: it Pings the gateway
// before the spawn loop and returns early when the probe fails. It never ran.
// `Loop.gatewayClient` — the field that block reads — is assigned NOWHERE in
// the package: `Loop.SetGatewayClient` forwards the client to the SPAWNER only
// (`l.spawner.SetGatewayClient(client)`), and the field stays nil for the
// daemon's whole lifetime. So the guard `if l.gatewayClient != nil && !l.simulate`
// was permanently false and the fleet packed and spawned straight into a dead
// or draining gateway.
//
// MEASURED, live scheduler.db, 7-day window (queried 2026-09-18):
//
//	total failures           859
//	"gateway unreachable…"  793 (89% of failures)
//	  ↳ HTTP 503 gateway draining   763
//	  ↳ dial tcp: connection refused 18
//	  ↳ context deadline exceeded     12
//
// Live shape: 2026-09-18T03:41:10 — 8 lanes failed in the same second, each
// booked as a lane fault, each burning a slot, a pick and a cooldown.
//
// FIX SHAPE (mirrors SCHED-GAP-171, commit ca92700, which did this for the
// load gate): the gate is consulted by the CALLER, before any row is created —
// `Loop.evaluate` per packed project after the dedup skip and the load-gate
// check, and `Loop.SpawnNow` before the spawn session. A deferred project
// creates NO ticks row, takes NO slot, spends NO nudge, charges NO cooldown and
// records NO failure: `continue`, exactly like the load gate. It stays selected
// and the next evaluation re-picks it once the cached verdict flips healthy, so
// recovery needs no resume scan of its own.
//
// WHY A CACHE. The verdict is gateway-WIDE, not per project: probing once per
// packed project would turn one dead gateway into N probes per pass (and the
// daemon packs 60+ projects). One probe per TTL window answers for every
// project in every pass, and the cached verdict keeps the LAST PROBE ERROR TEXT
// so the deferral event names the real failure instead of "unreachable".
//
// FAIL-OPEN, ALWAYS. No client wired (exec-fallback hosts, tests, simulation)
// means no probe is possible and the gate returns "do not defer" — the same
// doctrine as the load gate's missing-telemetry branch: an absent signal must
// never stop the fleet. The gate is also never consulted in --simulate mode
// (DOGFOOD-007: simulated spawns do not touch the gateway).
//
// AUTH REJECTIONS DO NOT DEFER (GAP-035). A 401/403 from the probe is terminal,
// not transient: the spawn path already classifies it as ErrGatewayKeyRejected
// and fails fast with a HIGH event so a key regression is immediately visible.
// Deferring on it would replace that loud, actionable signal with a silent fleet
// stall that no operator action is prompted by. The gate therefore defers on
// EVERY other probe failure (transport, timeout, 5xx drain, wrong path) and logs
// the auth case instead of latching.

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/coding-hermes/scheduler/internal/clock"
)

const (
	// gatewayHealthTTL is how long one probe verdict is trusted. 30s is the
	// same order of magnitude as the evaluation cadence (min-interval), so a
	// flip is seen by the next pass or the one after it — and a health flap
	// can never mislead the gate for longer than this window. Not configurable
	// on purpose: it is a probe cadence, not a policy knob.
	gatewayHealthTTL = 30 * time.Second

	// gatewayHealthProbeTimeout bounds ONE probe. Mirrors the 5s budget the
	// pre-existing liveness block used, and is deliberately shorter than the
	// gateway's own request timeout: the gate must never become the thing that
	// stalls an evaluation pass (a probe that hangs is a failed probe).
	gatewayHealthProbeTimeout = 5 * time.Second
)

// gatewayHealthGateState is the GATEWAY-WIDE health cache: one verdict, one
// probe instant, one last-error text — never a per-project map (a per-project
// cache would multiply the probes by the fleet size and let one lane's spawn
// failure masquerade as gateway health).
type gatewayHealthGateState struct {
	mu sync.RWMutex

	// clk is this component's clock (SCHED-GAP-169): nil reads as the WALL
	// clock. Held as a plain field under the gate's own mutex rather than a
	// clock.Seam — the gate is PACKAGE state, so it outlives the per-test
	// components and a seam's atomic.Value would panic the whole package the
	// first time two tests installed different concrete clock types.
	clk clock.Clock

	// client is the GatewayClient the probe uses. nil = no probe is possible
	// and the gate fails open (never blocks the fleet on absent telemetry).
	client *GatewayClient

	// healthy is the last verdict: true = the probe reached the gateway and it
	// was not an auth rejection.
	healthy bool

	// probedAt is when `healthy` was measured. Zero time = cold cache, so the
	// next call probes. Read through the clock seam, never the wall clock.
	probedAt time.Time

	// lastErr preserves the failing probe's error TEXT so a deferral can be
	// reported with the real reason (the ticket's "do not swallow the real
	// error" requirement). Only ever set with healthy=false.
	lastErr string
}

// gatewayHealth is the process-wide gate, mirroring load_gate.go's package-level
// threshold: the gateway is a single fleet-wide dependency, so its verdict is
// fleet-wide state, not a Loop field.
var gatewayHealth = &gatewayHealthGateState{}

// SetGatewayHealthGateClient installs the client the probe uses. Called from
// Loop.SetGatewayClient (the daemon's single wiring point, including the
// startup and reconnector paths). Passing a DIFFERENT client invalidates the
// cache — a verdict measured against an old endpoint must never be served for a
// new one. nil clears the gate back to fail-open.
func SetGatewayHealthGateClient(c *GatewayClient) {
	gatewayHealth.mu.Lock()
	defer gatewayHealth.mu.Unlock()
	if gatewayHealth.client == c {
		return
	}
	gatewayHealth.client = c
	gatewayHealth.healthy = false
	gatewayHealth.probedAt = time.Time{}
	gatewayHealth.lastErr = ""
}

// SetGatewayHealthGateClock installs the clock the TTL window is measured on.
// nil keeps the current clock (the same convention as clock.Seam.Set: a nil
// argument must never move a component onto a different timeline).
func SetGatewayHealthGateClock(c clock.Clock) {
	if c == nil {
		return
	}
	gatewayHealth.mu.Lock()
	defer gatewayHealth.mu.Unlock()
	gatewayHealth.clk = c
}

// clock returns the gate's clock, never nil (nil field = the wall clock). Call
// with gateHealth.mu held (read or write).
func (g *gatewayHealthGateState) clock() clock.Clock {
	if g.clk == nil {
		return clock.Real()
	}
	return g.clk
}

// GatewayHealthGateShouldDefer is the single gateway-health decision point, the
// mirror of LoadGateShouldDefer for the gateway dependency. It returns:
//
//	deferSpawn — true when the cached verdict says the gateway is unreachable,
//	             i.e. this spawn would be charged to the lane and fail;
//	reason     — the LAST PROBE ERROR TEXT (empty when healthy), so the caller
//	             can report why instead of guessing;
//	err        — the same text as an error, for callers that want the class.
//
// The first call on a cold cache probes the gateway through
// GatewayClient.Ping(ctx); every call within gatewayHealthTTL of that probe
// returns the cached verdict without touching the network. Fail-open: no client
// installed ⇒ (false, "", nil).
func GatewayHealthGateShouldDefer() (bool, string, error) {
	healthy, reason, probed := gatewayHealthVerdict()
	if probed || healthy {
		// Cached verdict (within TTL) or the fail-open "no client" answer.
		if healthy {
			return false, "", nil
		}
		return true, reason, errors.New(reason)
	}

	// Cold or expired cache: probe once, then answer for everyone.
	client, now := gatewayHealthProbeTarget()
	if client == nil {
		return false, "", nil // no client — fail open (race-free re-check)
	}

	ctx, cancel := context.WithTimeout(context.Background(), gatewayHealthProbeTimeout)
	err := client.Ping(ctx)
	cancel()

	if err != nil && errors.Is(err, ErrGatewayKeyRejected) {
		// GAP-035: a rejected key is TERMINAL and must stay loud. Cache the
		// probe instant (so the key is not re-probed per project) but do NOT
		// latch unhealthy — deferring here would silently stall every lane and
		// hide the key regression the spawn path exists to escalate.
		gatewayHealthStore(true, "", now)
		log.Printf("GATEWAY-HEALTH: probe rejected the daemon key (%v) — the gate is FAIL-OPEN for auth errors; "+
			"spawns stay classified by the spawn path (GAP-035)", err)
		return false, "", nil
	}

	if err != nil {
		msg := err.Error()
		gatewayHealthStore(false, msg, now)
		log.Printf("GATEWAY-HEALTH: probe failed, deferring spawns for %s: %s", gatewayHealthTTL, msg)
		return true, msg, errors.New(msg)
	}

	gatewayHealthStore(true, "", now)
	return false, "", nil
}

// gatewayHealthVerdict reads the cached verdict. probed=false means the cache
// cannot answer (cold, expired, or no client installed) — the caller must probe
// and must not treat the value as a verdict.
func gatewayHealthVerdict() (healthy bool, reason string, probed bool) {
	g := gatewayHealth
	g.mu.RLock()
	defer g.mu.RUnlock()

	if g.client == nil {
		// Fail open, and say so through `probed=true`: there is nothing to
		// probe, so this IS the answer (never a reason to block a spawn).
		return true, "", true
	}
	if g.probedAt.IsZero() {
		return false, "", false // cold
	}
	if g.clock().Now().Sub(g.probedAt) >= gatewayHealthTTL {
		return false, "", false // expired
	}
	return g.healthy, g.lastErr, true
}

// gatewayHealthProbeTarget returns the client to probe with and the instant to
// stamp the verdict with (read through the gate's clock, never the wall clock
// directly — SCHED-GAP-169).
func gatewayHealthProbeTarget() (*GatewayClient, time.Time) {
	g := gatewayHealth
	g.mu.RLock()
	defer g.mu.RUnlock()
	if g.client == nil {
		return nil, time.Time{}
	}
	return g.client, g.clock().Now()
}

// gatewayHealthStore records a probe verdict.
func gatewayHealthStore(healthy bool, lastErr string, at time.Time) {
	g := gatewayHealth
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.client == nil {
		return // the client was uninstalled mid-probe: keep the fail-open state
	}
	g.healthy = healthy
	g.probedAt = at
	g.lastErr = lastErr
}

// gatewayHealthDescribe renders the cache for operator logs/tests.
func gatewayHealthDescribe() string {
	g := gatewayHealth
	g.mu.RLock()
	defer g.mu.RUnlock()
	state := "cold"
	switch {
	case g.client == nil:
		state = "no-client (fail open)"
	case !g.probedAt.IsZero():
		state = fmt.Sprintf("healthy=%t probed_at=%s err=%q", g.healthy, g.probedAt.UTC().Format(time.RFC3339), g.lastErr)
	}
	return "gateway-health: " + state
}

// noteGatewayDeadOnce applies the dead-gateway transition exactly once per
// outage episode: the gatewayDead latch (the per-spawn "did this POST work"
// flag — untouched in meaning, only its trigger moved) plus the slot-pool
// release the pre-existing liveness block performed, so phantom reservations
// from the pass that discovered the outage cannot block the next cycle.
//
// Called from the evaluation pass only: SpawnNow must not write the latch (an
// API caller's deferral is reported, not latched — the eval loop owns the
// fleet-wide state transition).
func (l *Loop) noteGatewayDeadOnce(reason string) {
	if l.gatewayDead {
		return
	}
	log.Printf("GATEWAY DEAD — deferring spawns, re-probing every %s: %s", gatewayHealthTTL, reason)
	l.gatewayDead = true
	if l.slotPool != nil {
		l.slotPool.ReleaseAll()
	}
}

// noteGatewayReconnectedOnce applies the reconnect transition: gatewayDead is
// cleared and — SCHED-GAP-091 — the ticks orphaned by the drop are scanned and
// re-nudged with continuation prompts. It rides the existing flip, so no new
// cron and no new goroutine is introduced. No-op while the latch is clear
// (i.e. on every healthy pass that never saw an outage).
func (l *Loop) noteGatewayReconnectedOnce() {
	if !l.gatewayDead {
		return
	}
	log.Printf("GATEWAY reconnected — resuming spawns")
	l.gatewayDead = false
	l.resumeOrphans("reconnect")
	l.resumeNeedsHuman("reconnect")
}
