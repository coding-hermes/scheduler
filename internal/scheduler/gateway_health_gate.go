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
//
// OBSERVABILITY (the half that makes the fix durable). A guard that never ran
// is invisible: from the outside, "every spawn passed the guard" and "no spawn
// ever reached the guard" look identical — which is why the inert guard survived
// for months and why a replacement could go inert just as quietly. So the gate
// now reports itself three ways: its ARMED state (GatewayHealthGateStatus →
// /api/v1/status `gateway_health_gate`; armed is true exactly when a client is
// installed), a boot line exactly once per install (`GATEWAY-HEALTH-GATE: armed
// ttl=30s` / `... NOT ARMED ...`), and its outage EPISODES — one MEDIUM event
// when the verdict flips unhealthy and one INFO when it recovers, never one per
// deferred project, because the gateway is a single fleet-wide dependency.

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"sync/atomic"
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

	// events is where the gate's ONE-per-episode transition events go. nil
	// means "log only" (the unwired default): an event write is never a
	// precondition for an admission decision. Installed by the Loop next to the
	// spawner's and the slot pool's loggers (NewLoop) — the gate is PACKAGE
	// state, so every consult path shares the one logger.
	events *EventLogger

	// deferrals counts EVERY deferral decision this process has made. Atomic
	// on purpose: it is incremented on the hot admission path and read by the
	// status surface without taking the gate lock. Monotonic — never reset (an
	// operator wants "deferrals since boot", not "since the last re-wire").
	deferrals atomic.Uint64

	// episodeActive marks the unhealthy episode currently being reported: true
	// while the latched verdict is a failing one AND that episode's MEDIUM event
	// has been emitted. It is the ONE-per-episode throttle — the transition is
	// reported when this flips false→true and never again for the same episode,
	// however many projects defer inside it.
	episodeActive bool

	// episodeDeferrals counts the deferrals charged to the CURRENT episode; the
	// recovery event reports it ("recovered after N deferrals"). Reset when an
	// episode ends.
	episodeDeferrals uint64

	// wired records that a boot line has been logged for this process — either
	// by an install or by the daemon's explicit boot-state call. It is what
	// makes the boot line "exactly once per install" while still guaranteeing
	// the FIRST wiring call of the process always logs, including the nil case
	// (whose whole point is that the fleet is unguarded).
	wired bool
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
//
// Installing logs the gate's BOOT LINE (SCHED-GAP-170 observability), exactly
// once per install: the first wiring call of the process always logs, a repeated
// call with the same client does not, and a client change logs again. The line
// is what an operator greps in the first seconds of scheduler.log to answer
// "is the gateway-health gate live?" — the question nobody could answer while
// the FIX-STUCK guard was silently inert for months:
//
//	GATEWAY-HEALTH-GATE: armed ttl=30s
//	GATEWAY-HEALTH-GATE: NOT ARMED (no gateway client) - spawns will not be gated
func SetGatewayHealthGateClient(c *GatewayClient) {
	g := gatewayHealth
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.wired && g.client == c {
		return
	}
	g.wired = true
	g.client = c
	g.healthy = false
	g.probedAt = time.Time{}
	g.lastErr = ""
	// A different client invalidates the cached verdict wholesale, so the
	// episode it latched ends here too — SILENTLY: a re-wire is a wiring
	// change, not a health transition, and the next probe decides the next
	// episode. Without this a client swap would leave the gate believing it was
	// still inside the previous endpoint's outage, and the first failure of the
	// new endpoint would never be reported.
	g.episodeActive = false
	g.episodeDeferrals = 0
	g.logBootLineLocked()
}

// LogGatewayHealthGateBootState writes the gate's boot line for a daemon that
// never installs a client at all — main.go with an empty --gateway-url/
// --gateway-key, or a gateway that stayed unreachable through startup's
// retries. Those are exactly the hosts where the gate is unarmed and every
// spawn goes straight to the gateway, so the first seconds of scheduler.log
// must say so rather than stay silent.
//
// No-op once a boot line has been logged: the install path owns the line
// otherwise, so it stays exactly one per install (and never two at boot).
func LogGatewayHealthGateBootState() {
	g := gatewayHealth
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.wired {
		return
	}
	g.wired = true
	g.logBootLineLocked()
}

// logBootLineLocked prints the install's single boot line. Called with g.mu
// held. The token GATEWAY-HEALTH-GATE is the stable grep anchor; "NOT ARMED" is
// the inert-guard warning (armed=false means the gate fails open).
func (g *gatewayHealthGateState) logBootLineLocked() {
	if g.client == nil {
		log.Printf("GATEWAY-HEALTH-GATE: NOT ARMED (no gateway client) - spawns will not be gated")
		return
	}
	log.Printf("GATEWAY-HEALTH-GATE: armed ttl=%s", gatewayHealthTTL)
}

// SetGatewayHealthGateEvents installs the logger the gate emits its
// ONE-per-episode transition events through. nil means "log only" — the
// unwired default, and what tests that only care about the verdict use. The
// gate's decisions never depend on it.
func SetGatewayHealthGateEvents(ev *EventLogger) {
	gatewayHealth.mu.Lock()
	defer gatewayHealth.mu.Unlock()
	gatewayHealth.events = ev
}

// gatewayHealthEpisodeUnhealthy / gatewayHealthEpisodeRecovered are the two
// transitions an unhealthy episode can produce.
const (
	gatewayHealthEpisodeUnhealthy = "unhealthy"
	gatewayHealthEpisodeRecovered = "recovered"
)

// gatewayHealthTransition is a verdict flip that must be reported exactly once
// per episode. The zero value means "no transition" — nothing to report.
type gatewayHealthTransition struct {
	kind      string // gatewayHealthEpisodeUnhealthy | ...Recovered | ""
	reason    string // the failing probe's error text (unhealthy only)
	deferrals uint64 // deferrals the ended episode cost (recovered only)
}

// GatewayHealthGateStatus reports the gate's armed state and its cached
// verdict — the surface that answers "is the gateway-health gate live?" without
// reading the source.
//
// It exists because of the shape of the FIX-STUCK defect: a guard that never
// ran is indistinguishable, from the outside, from a guard that ran and found
// everything fine. Nothing could tell an operator that the fleet was spawning
// into a dead gateway UNGUARDED, and nothing would tell them if it happened
// again. This is that missing surface (exposed as /api/v1/status
// gateway_health_gate).
//
//	armed     — a GatewayClient is INSTALLED on the gate. This is the exact
//	            condition that was silently false for months. armed=false means
//	            the gate fails open: no probe, no deferrals, every spawn goes
//	            straight to the gateway.
//	healthy   — the cached verdict's own field. false means either "the last
//	            probe failed" (lastErr names it, probedAt says when) or "there is
//	            no verdict yet" (probedAt zero: cold cache, or unarmed). An
//	            armed-but-cold gate defers nothing either — read healthy
//	            TOGETHER with armed and probedAt, never alone.
//	probedAt  — when the cached verdict was measured (zero = none).
//	lastErr   — the failing probe's error text ("" for a healthy verdict).
//	ttl       — how long one verdict is trusted (gatewayHealthTTL).
//	deferrals — deferral decisions since boot (monotonic; never reset).
func GatewayHealthGateStatus() (armed bool, healthy bool, probedAt time.Time, lastErr string, ttl time.Duration, deferrals uint64) {
	g := gatewayHealth
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.client != nil, g.healthy, g.probedAt, g.lastErr, gatewayHealthTTL, g.deferrals.Load()
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
		// A deferral decision. Counted here, on the decision itself, so the
		// operator-facing total answers "how much work did the gate stop" and
		// not "how often was it probed".
		gatewayHealthNoteDeferral()
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
		gatewayHealthEmit(gatewayHealthStore(true, "", now))
		log.Printf("GATEWAY-HEALTH: probe rejected the daemon key (%v) — the gate is FAIL-OPEN for auth errors; "+
			"spawns stay classified by the spawn path (GAP-035)", err)
		return false, "", nil
	}

	if err != nil {
		msg := err.Error()
		// Store first (this is what opens the episode), then charge the
		// deferral to it, then report the transition — so the first deferral of
		// an outage is counted in the episode the recovery event reports.
		tr := gatewayHealthStore(false, msg, now)
		gatewayHealthNoteDeferral()
		gatewayHealthEmit(tr)
		log.Printf("GATEWAY-HEALTH: probe failed, deferring spawns for %s: %s", gatewayHealthTTL, msg)
		return true, msg, errors.New(msg)
	}

	gatewayHealthEmit(gatewayHealthStore(true, "", now))
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

// gatewayHealthStore records a probe verdict and applies the ONE-per-episode
// bookkeeping, returning the transition (if any) that the caller must report.
// The zero value means "this verdict was not a transition" — the overwhelmingly
// common case, including every probe inside an episode already reported.
//
// The returned transition is computed under the lock so two concurrent probes
// can never both see the same flip; the EVENT is emitted by the caller, outside
// the lock, because an event write must never hold up an admission decision.
func gatewayHealthStore(healthy bool, lastErr string, at time.Time) gatewayHealthTransition {
	g := gatewayHealth
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.client == nil {
		return gatewayHealthTransition{} // the client was uninstalled mid-probe: keep the fail-open state
	}
	g.healthy = healthy
	g.probedAt = at
	g.lastErr = lastErr

	if !healthy {
		if g.episodeActive {
			// Already reported: this is the SAME outage, however many projects
			// defer inside it. This branch is the "never one event per deferred
			// project" guarantee.
			return gatewayHealthTransition{}
		}
		g.episodeActive = true
		g.episodeDeferrals = 0
		return gatewayHealthTransition{kind: gatewayHealthEpisodeUnhealthy, reason: lastErr}
	}

	if !g.episodeActive {
		return gatewayHealthTransition{} // healthy verdict, no episode to close
	}
	n := g.episodeDeferrals
	g.episodeActive = false
	g.episodeDeferrals = 0
	return gatewayHealthTransition{kind: gatewayHealthEpisodeRecovered, deferrals: n}
}

// gatewayHealthNoteDeferral counts ONE deferral decision: the process-wide
// monotonic total the status surface reports, plus the current episode's tally
// the recovery event reports ("recovered after N deferrals"). Called on every
// path that returns deferSpawn=true — the cached verdict and the fresh failed
// probe alike, so the counter tracks decisions, not probes.
func gatewayHealthNoteDeferral() {
	gatewayHealth.deferrals.Add(1)
	g := gatewayHealth
	g.mu.Lock()
	if g.episodeActive {
		g.episodeDeferrals++
	}
	g.mu.Unlock()
}

// NoteTransientGatewayDeferral records a transient gateway blip as a deferral
// rather than a lane failure (SCHED-GAP-203-A; the spawn-side caller is
// SCHED-GAP-203-B, which invokes this from spawn.go when
// errors.Is(gwErr, ErrGatewayTransient) is true).
//
// THE DEFECT THIS HALF OF THE FIX SERVES (SCHED-GAP-203, measured on the live
// daemon 2026-09-20): /api/v1/status reported the gateway-health gate
// armed=true healthy=true deferrals_total=0 over 32h while 9 of the 458 ticks
// spawned in that window completed TickFailed with "gateway unreachable and
// exec fallback disabled" — 4 of them a mid-run "gateway transient error: sse
// stream ended without a terminal event" (i.e. AFTER a successful spawn), the
// rest a failed connect. The gate (SCHED-GAP-170) guards only the PRE-SPAWN
// admission point, so a blip that lands inside a spawn never reaches it and is
// booked as the lane's own fault: a pick, a slot and a cooldown burned, and the
// project's failure rate polluted (those rates feed --auto-disable-failure-rate).
//
// The COUNTER plumbing mirrors gatewayHealthNoteDeferral EXACTLY — the same
// process-wide monotonic total (atomic, so the status surface reads it without
// the gate lock) plus the same current-episode tally — because deferrals_total
// and the recovery event's "after N deferrals" must count BOTH kinds of
// deferral in one number: the operator question is "how much work did the
// gateway stop", not "which half of the code deferred it".
//
// The EVENT is per-CALL, not per-episode, and that is deliberate: an outage
// transition is one fleet-wide fact reported once (gatewayHealthEmit), whereas a
// mid-spawn blip is charged to a SPECIFIC lane and its project/reason are what
// make the deferral auditable per tick. The payload carries the machine marker
// `event_type` (gateway_health_transient_defer) for the same reason the episode
// events do — so the deferrals are queryable after the fact with
// json_extract(details,'$.event_type') instead of by matching message text.
//
// A 401/403 is NEVER transient by construction (gateway_client.go classifies
// it as ErrGatewayKeyRejected), so a key regression still fails the tick loudly
// under GAP-035 and can never be laundered into a deferral by this method.
func (g *gatewayHealthGateState) NoteTransientGatewayDeferral(project, reason string) {
	if g == nil {
		return
	}
	// Same order and lock shape as gatewayHealthNoteDeferral: the atomic total
	// first (the hot-path counter the status surface reads lock-free), then the
	// episode tally under the gate lock.
	g.deferrals.Add(1)
	g.mu.Lock()
	if g.episodeActive {
		g.episodeDeferrals++
	}
	ev := g.events
	g.mu.Unlock()
	if ev == nil {
		return // unwired logger: log-only, exactly like the episode transitions
	}
	// Emitted OUTSIDE the lock (an event write must never hold up an admission
	// decision) and ignoring the write result, mirroring gatewayHealthEmit —
	// EventLogger.Emit already logs its own failure and never returns one.
	ev.Emit(context.Background(), SeverityInfo, "gateway_health_gate",
		"transient gateway deferral",
		map[string]any{
			"event_type": "gateway_health_transient_defer",
			"project":    project,
			"reason":     reason,
		})
}

// gatewayHealthEmit reports an episode transition through the installed logger
// and is a NO-OP for the zero transition (see gatewayHealthStore) and for an
// unwired logger (the gate then only logs). The event is a single fleet-level
// row per episode — deliberately NOT per project: the gateway is one
// fleet-wide dependency, so N deferred projects are one outage, and N rows
// would be the very per-lane noise the deferral exists to avoid.
//
// The payload carries the machine marker `event_type` (gateway_health_defer /
// gateway_health_recovered) so both directions are queryable after the fact
// with `json_extract(details,'$.event_type')`, mirroring the load-gate and
// gateway-deferral events.
func gatewayHealthEmit(tr gatewayHealthTransition) {
	if tr.kind == "" {
		return
	}
	g := gatewayHealth
	g.mu.RLock()
	ev := g.events
	g.mu.RUnlock()
	if ev == nil {
		return
	}
	switch tr.kind {
	case gatewayHealthEpisodeUnhealthy:
		// MEDIUM, mirroring the load-gate deferral and the slot-wait drop: work
		// the evaluator selected is not running, which an operator must see, but
		// the fleet is deferring-and-retrying by design rather than failing.
		ev.Emit(context.Background(), SeverityMedium, "gateway_health",
			"gateway health gate deferring spawns: "+tr.reason,
			map[string]any{
				"event_type": "gateway_health_defer",
				"reason":     tr.reason,
				"deferred":   true,
				"ttl_s":      int(gatewayHealthTTL.Seconds()),
			})
	case gatewayHealthEpisodeRecovered:
		log.Printf("GATEWAY-HEALTH-GATE: recovered after %d deferral(s) — spawns admitted again", tr.deferrals)
		ev.Emit(context.Background(), SeverityInfo, "gateway_health",
			fmt.Sprintf("gateway health gate recovered after %d deferrals", tr.deferrals),
			map[string]any{
				"event_type": "gateway_health_recovered",
				"deferrals":  tr.deferrals,
				"ttl_s":      int(gatewayHealthTTL.Seconds()),
			})
	}
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
