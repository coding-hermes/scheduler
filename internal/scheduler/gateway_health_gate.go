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
//
// ============================================================================
// t_cadba34c — ADMISSION IS PER ENDPOINT (decided 2026-10-03, measured).
//
// NAMING: this work is tracked by the KANBAN CARD t_cadba34c (a follow-up filed
// by the SCHED-GAP-1712 landing). It has NO SCHED-GAP board row of its own, and
// the board row SCHED-GAP-1724 is a DIFFERENT issue (the deploy build reading a
// diverged checkout) — grep either id for one meaning only.
// ============================================================================
//
// THE BOUNDARY SCHED-GAP-1712 LEFT. A lane can now be ADDRESSED at its own
// gateway (`projects.gateway_url` > `namespaces.gateway_url` > `[gateway].url`,
// resolved per tick at dispatch), but this gate — and the two
// `l.gatewayClientOrNil() != nil` call sites that consult it (`Loop.SpawnNow`,
// `Loop.evaluate`'s spawn loop) — asked ONE question: is the DAEMON's gateway
// healthy? A lane whose resolved endpoint is a remote box's gateway therefore
// waited whenever the daemon's OWN gateway was down: a deferral caused by a
// dependency that lane was not going to use. That is the exact
// charge-the-wrong-dependency failure this gate exists to prevent, inverted.
//
// MEASURED (live scheduler.db, 2026-10-03; the deciding measurement):
//
//	total lanes 593 / namespaces 19; lanes with a non-global endpoint      0
//	  (`projects.gateway_url` / `ticks.gateway_url` do not exist on the live
//	   schema: migration 61 is landed in git but the daemon has not been
//	   re-deployed, so TODAY every lane resolves to the global endpoint and
//	   (a) "leave it global" and (b) "make it endpoint-aware" are
//	   indistinguishable in live behaviour)
//	GLOBAL-endpoint outage EPISODES (one MEDIUM event each), 09-21 → 09-27:  6
//	  deferrals charged to their episodes (recovered events): 1, 12, 121, 4, 8
//	  09-27 01:20 → 01:35: 121 deferred spawns in ONE 15-minute global outage
//	transient per-lane gateway deferrals, same window:                     163
//
// The harmful case is real and already measured: the daemon's gateway goes down
// and the gate idles the fleet wholesale (121 spawns in a single episode). While
// ZERO lanes are addressed that is CORRECT — they all used that gateway. But the
// fleet is actively standing up per-agent gateways on remote boxes
// (SCHED-GAP-1723; boxes 03/04), so the addressed population is about to be
// non-zero, and the first global outage after that would idle every addressed
// lane for a gateway it does not use. The isolation law the derived
// GatewayClient already follows — "one unreachable lane endpoint fails alone"
// — settles it: ADMISSION IS PART OF THE SAME DEPENDENCY AND MUST FOLLOW THE
// SAME ENDPOINT. Decision: (b), with these constraints.
//
// SHAPE. The verdict cache is keyed by ENDPOINT (the client's `baseURL`), not by
// the daemon. The global endpoint keeps every existing behaviour byte-for-byte
// (same probe with the daemon key, same TTL, same episode events, same boot
// line/status surface, and the same `gatewayDead` latch + reconnect/orphan-nudge
// transitions — those stay GLOBAL-only because they are fleet-wide state). A
// lane whose resolved endpoint is a foreign URL is admitted on ITS endpoint's
// health:
//
//   - probed through its own derived GatewayClient with the RESOLVED key (an
//     empty key falls back to the client's implicit key, exactly as the
//     dispatch path's setAuth does), so an authenticated remote endpoint is
//     probed with the credential it will be dispatched with;
//   - cached per endpoint for gatewayHealthTTL, so N addressed lanes on ONE
//     remote gateway cost ONE probe per window, and one unreachable lane
//     endpoint defers ONLY that lane;
//   - BOUNDED (gatewayHealthMaxEndpoints, oldest-probed evicted first, never a
//     live outage's episode entry while an expired one exists) so a churning
//     endpoint set cannot grow the cache without limit;
//   - reported as its own one-per-episode MEDIUM/INFO event carrying the
//     endpoint, and as `endpoint`/`gateway_url`/`gateway_source`/
//     `gateway_key_source` on the deferral event, so a deferred tick shows WHY
//     and AGAINST WHICH endpoint (no new `ticks` column: the deferral is a
//     decision made BEFORE a row can exist on the evaluation path, so the event
//     detail is the audit surface);
//   - NEVER allowed to flip the fleet-wide `gatewayDead` latch or trigger the
//     global reconnect/orphan-nudge: an unreachable lane endpoint must not
//     degrade the fleet.
//
// Cost: the evaluation pass resolves the packed set's endpoints in ONE batched
// indexed query per pass (only while a gateway client is installed), never one
// query per project — see Spawner.resolvePackedEndpoints.

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

// gatewayHealthEndpointVerdict is the cached health of ONE endpoint: the
// gateway-wide verdict for the daemon's own gateway, and a per-lane/per-
// namespace verdict for a foreign resolved endpoint (t_cadba34c). One
// verdict, one probe instant, one last-error text, one outage-episode
// bookkeeping — never per PROJECT (a per-project cache would multiply the
// probes by the fleet size and let one lane's spawn failure masquerade as
// gateway health; the unit is the dependency, and a dependency is an endpoint).
type gatewayHealthEndpointVerdict struct {
	// healthy is the last verdict: true = the probe reached the endpoint and
	// it was not an auth rejection.
	healthy bool

	// probedAt is when `healthy` was measured. Zero time = cold cache, so the
	// next call probes. Read through the clock seam, never the wall clock.
	probedAt time.Time

	// lastErr preserves the failing probe's error TEXT so a deferral can be
	// reported with the real reason (the ticket's "do not swallow the real
	// error" requirement). Only ever set with healthy=false.
	lastErr string

	// episodeActive marks the unhealthy episode currently being reported for
	// THIS endpoint: true while the latched verdict is a failing one AND that
	// episode's MEDIUM event has been emitted. It is the ONE-per-episode
	// throttle — the transition is reported when this flips false→true and
	// never again for the same episode, however many projects defer inside it.
	episodeActive bool

	// episodeDeferrals counts the deferrals charged to THIS endpoint's current
	// episode; the recovery event reports it ("recovered after N deferrals").
	// Reset when an episode ends.
	episodeDeferrals uint64
}

// gatewayHealthMaxEndpoints bounds the endpoint verdict cache. The fleet has a
// handful of gateways (the daemon's own plus one per remote box), so 64 is far
// above any real fleet while still making an ever-churning endpoint set — the
// API can rewrite `projects.gateway_url` at any time — a bounded cost rather
// than an unbounded map. On overflow the entry with the OLDEST probe instant is
// evicted, preferring one that is not inside a live outage episode.
const gatewayHealthMaxEndpoints = 64

// gatewayHealthGateState is the ENDPOINT-KEYED health cache (SCHED-GAP-1712's
// addressing + t_cadba34c's admission): one verdict per endpoint, keyed by
// the endpoint's baseURL, so a lane admitted through a foreign endpoint is
// judged by the health of THAT endpoint while the daemon's own gateway keeps
// its own verdict. `client` stays the daemon's own (global) client — the probe
// target for the global endpoint, and the field the armed/boot/status surfaces
// and `gatewayDead` transitions are defined by.
type gatewayHealthGateState struct {
	mu sync.RWMutex

	// clk is this component's clock (SCHED-GAP-169): nil reads as the WALL
	// clock. Held as a plain field under the gate's own mutex rather than a
	// clock.Seam — the gate is PACKAGE state, so it outlives the per-test
	// components and a seam's atomic.Value would panic the whole package the
	// first time two tests installed different concrete clock types.
	clk clock.Clock

	// client is the GLOBAL GatewayClient: the daemon's own gateway, installed
	// by SetGatewayHealthGateClient. nil = no probe is possible and the gate
	// fails open (never blocks the fleet on absent telemetry). Its baseURL is
	// the key of the global entry in `endpoints`.
	client *GatewayClient

	// endpoints is the per-endpoint verdict cache, keyed by the endpoint's
	// baseURL (the global client's own baseURL included). Entries are created
	// lazily on the first consult of an endpoint and bounded by
	// gatewayHealthMaxEndpoints. It is NOT a per-project map: N lanes behind
	// one gateway share one entry (and one probe per TTL window).
	endpoints map[string]*gatewayHealthEndpointVerdict

	// events is where the gate's ONE-per-episode transition events go. nil
	// means "log only" (the unwired default): an event write is never a
	// precondition for an admission decision. Installed by the Loop next to the
	// spawner's and the slot pool's loggers (NewLoop) — the gate is PACKAGE
	// state, so every consult path shares the one logger.
	events *EventLogger

	// deferrals counts EVERY deferral decision this process has made, across
	// every endpoint (atomic on purpose: it is incremented on the hot admission
	// path and read by the status surface without taking the gate lock).
	// Monotonic — never reset. The operator question it answers is "how much
	// work did the gateway(s) stop", so a per-endpoint deferral counts here too.
	deferrals atomic.Uint64

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
	// A different client invalidates the cached verdicts wholesale, so every
	// episode they latched ends here too — SILENTLY: a re-wire is a wiring
	// change, not a health transition, and the next probe decides the next
	// episode. Without this a client swap would leave the gate believing it was
	// still inside the previous endpoint's outage, and the first failure of the
	// new endpoint would never be reported. The foreign-endpoint verdicts are
	// keyed by their own URL and would survive a daemon re-wire correctly, but
	// they are cleared here too so a test (or an operator) can reset the gate
	// with ONE call — and so a re-wire can never inherit a verdict measured
	// before the daemon's own endpoint moved.
	g.endpoints = nil
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
	// endpoint is the baseURL of the endpoint whose episode flipped. EMPTY for
	// the daemon's own (global) gateway, which keeps the pre-1724 event payload
	// and message byte-for-byte; a FOREIGN endpoint names itself so its episode
	// event is attributable (t_cadba34c).
	endpoint string
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
//	deferrals — deferral decisions since boot (monotonic; never reset; counts
//	            every endpoint's deferrals, which is the operator question).
//
// The verdict reported is the GLOBAL endpoint's (the daemon's own gateway).
// Since t_cadba34c a foreign resolved endpoint keeps its OWN verdict, keyed
// by its baseURL; those episodes are visible as events carrying the endpoint, and
// they never overwrite this surface's global answer.
func GatewayHealthGateStatus() (armed bool, healthy bool, probedAt time.Time, lastErr string, ttl time.Duration, deferrals uint64) {
	g := gatewayHealth
	g.mu.RLock()
	defer g.mu.RUnlock()
	v := g.endpoints[g.globalKeyLocked()]
	if g.client == nil || v == nil {
		return g.client != nil, false, time.Time{}, "", gatewayHealthTTL, g.deferrals.Load()
	}
	return true, v.healthy, v.probedAt, v.lastErr, gatewayHealthTTL, g.deferrals.Load()
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

// globalKeyLocked is the cache key of the daemon's own endpoint ("" when no
// client is installed). Call with g.mu held.
func (g *gatewayHealthGateState) globalKeyLocked() string {
	if g.client == nil {
		return ""
	}
	return g.client.baseURL
}

// GatewayHealthGateShouldDefer is the gateway-health decision point for the
// DAEMON's own (global) endpoint, the mirror of LoadGateShouldDefer for the
// gateway dependency. It returns:
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
//
// t_cadba34c: this is now the GLOBAL-endpoint special case of
// GatewayHealthGateShouldDeferEndpoint. A caller holding a RESOLVED endpoint
// must use that function instead, so an addressed lane is judged by the endpoint
// its spawn will actually use.
func GatewayHealthGateShouldDefer() (bool, string, error) {
	g := gatewayHealth
	g.mu.RLock()
	client := g.client
	g.mu.RUnlock()
	if client == nil {
		return false, "", nil // fail open: no probe target at all
	}
	return GatewayHealthGateShouldDeferEndpoint(client, "")
}

// GatewayHealthGateShouldDeferEndpoint is the t_cadba34c decision point:
// it judges the health of the ENDPOINT the caller resolved for this spawn,
// not the daemon's own gateway.
//
//   - For the daemon's own client (the installed global client, or any client
//     with the same baseURL) it is exactly GatewayHealthGateShouldDefer, and
//     shares the global entry's verdict, episode events and TTL window.
//   - For any other endpoint it probes and caches PER ENDPOINT: the key is the
//     client's baseURL, so N lanes behind one remote gateway cost one probe per
//     gatewayHealthTTL, and one unreachable lane endpoint defers only the lanes
//     that resolve to it. The probe presents `key` (the RESOLVED credential for
//     this dispatch; "" falls back to the client's implicit key, exactly as the
//     dispatch path's setAuth does) — an authenticated remote endpoint is
//     therefore probed with the credential it will be dispatched with.
//
// Fail-open on a nil client and on an auth rejection (GAP-035), exactly like
// the global path. A foreign endpoint's deferral NEVER touches the fleet-wide
// gatewayDead latch or the global reconnect/orphan-nudge: those transitions are
// the evaluation loop's, and only for the global endpoint (see
// noteGatewayDeadOnce / noteGatewayReconnectedOnce).
func GatewayHealthGateShouldDeferEndpoint(client *GatewayClient, key string) (bool, string, error) {
	if client == nil {
		return false, "", nil // no client — fail open
	}
	url := client.baseURL

	healthy, reason, probed := gatewayHealthEndpointReadVerdict(url)
	if probed || healthy {
		// Cached verdict (within TTL) or the fail-open "no client" answer.
		if healthy {
			return false, "", nil
		}
		// A deferral decision. Counted here, on the decision itself, so the
		// operator-facing total answers "how much work did the gate stop" and
		// not "how often was it probed".
		gatewayHealthNoteDeferral(url)
		return true, reason, errors.New(reason)
	}

	// Cold or expired cache: probe once, then answer for every lane behind this
	// endpoint.
	now := gatewayHealthNow()
	isGlobal := gatewayHealthIsGlobalClient(client)

	ctx, cancel := context.WithTimeout(context.Background(), gatewayHealthProbeTimeout)
	err := client.health(ctx, key)
	cancel()

	if err != nil && errors.Is(err, ErrGatewayKeyRejected) {
		// GAP-035: a rejected key is TERMINAL and must stay loud. Cache the
		// probe instant (so the key is not re-probed per project) but do NOT
		// latch unhealthy — deferring here would silently stall every lane and
		// hide the key regression the spawn path exists to escalate.
		gatewayHealthEmit(gatewayHealthStore(url, true, "", now, isGlobal))
		log.Printf("GATEWAY-HEALTH: probe rejected the key for %s (%v) — the gate is FAIL-OPEN for auth errors; "+
			"spawns stay classified by the spawn path (GAP-035)", gatewayHealthEndpointLabel(url), err)
		return false, "", nil
	}

	if err != nil {
		msg := err.Error()
		// Store first (this is what opens the episode), then charge the
		// deferral to it, then report the transition — so the first deferral of
		// an outage is counted in the episode the recovery event reports.
		tr := gatewayHealthStore(url, false, msg, now, isGlobal)
		gatewayHealthNoteDeferral(url)
		gatewayHealthEmit(tr)
		if isGlobal {
			log.Printf("GATEWAY-HEALTH: probe failed, deferring spawns for %s: %s", gatewayHealthTTL, msg)
		} else {
			log.Printf("GATEWAY-HEALTH: probe failed for endpoint %s, deferring its lanes for %s: %s",
				url, gatewayHealthTTL, msg)
		}
		return true, msg, errors.New(msg)
	}

	gatewayHealthEmit(gatewayHealthStore(url, true, "", now, isGlobal))
	return false, "", nil
}

// gatewayHealthEndpointLabel renders an endpoint for operator output. The
// global endpoint ("" from globalKeyLocked, or a client with no URL) reads as
// "the daemon gateway" so a global outage log line stays readable.
func gatewayHealthEndpointLabel(url string) string {
	if url == "" {
		return "the daemon gateway"
	}
	return url
}

// gatewayHealthIsGlobalClient reports whether `c` is the daemon's own gateway
// client (the one installed by SetGatewayHealthGateClient), by identity or by
// baseURL. A lane addressed at the SAME url as the daemon resolves to the
// global client (gateway_endpoint.go endpointClient), so both checks are needed
// for the semantics "shares the global verdict".
func gatewayHealthIsGlobalClient(c *GatewayClient) bool {
	if c == nil {
		return false
	}
	g := gatewayHealth
	g.mu.RLock()
	defer g.mu.RUnlock()
	if g.client == nil {
		return false
	}
	return c == g.client || (c.baseURL != "" && c.baseURL == g.client.baseURL)
}

// gatewayHealthNow reads the gate's clock (never the wall clock directly —
// SCHED-GAP-169).
func gatewayHealthNow() time.Time {
	g := gatewayHealth
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.clock().Now()
}

// gatewayHealthEndpointReadVerdict reads one endpoint's cached verdict.
// probed=false means the cache cannot answer (cold or expired) — the caller
// must probe and must not treat the value as a verdict.
func gatewayHealthEndpointReadVerdict(url string) (healthy bool, reason string, probed bool) {
	g := gatewayHealth
	g.mu.RLock()
	defer g.mu.RUnlock()
	v, ok := g.endpoints[url]
	if !ok || v.probedAt.IsZero() {
		return false, "", false // cold
	}
	if g.clock().Now().Sub(v.probedAt) >= gatewayHealthTTL {
		return false, "", false // expired
	}
	return v.healthy, v.lastErr, true
}

// gatewayHealthEnsureEndpointLocked returns the cache entry for one endpoint,
// creating it lazily and evicting to stay within gatewayHealthMaxEndpoints.
// Call with g.mu held (write).
func (g *gatewayHealthGateState) gatewayHealthEnsureEndpointLocked(url string) *gatewayHealthEndpointVerdict {
	if g.endpoints == nil {
		g.endpoints = make(map[string]*gatewayHealthEndpointVerdict)
	}
	if v, ok := g.endpoints[url]; ok {
		return v
	}
	if len(g.endpoints) >= gatewayHealthMaxEndpoints {
		g.evictEndpointLocked()
	}
	v := &gatewayHealthEndpointVerdict{}
	g.endpoints[url] = v
	return v
}

// evictEndpointLocked drops ONE entry to make room: the one with the oldest
// probe instant, preferring an entry that is NOT inside a live outage episode
// (its bookkeeping is what the eventual recovery event reports). If every entry
// is mid-episode the oldest is dropped anyway — bounded beats perfect. Call with
// g.mu held (write).
func (g *gatewayHealthGateState) evictEndpointLocked() {
	var victim string
	var victimAt time.Time
	for k, v := range g.endpoints {
		if v.episodeActive {
			continue
		}
		if victim == "" || v.probedAt.Before(victimAt) {
			victim, victimAt = k, v.probedAt
		}
	}
	if victim == "" {
		for k, v := range g.endpoints {
			if victim == "" || v.probedAt.Before(victimAt) {
				victim, victimAt = k, v.probedAt
			}
		}
	}
	if victim != "" {
		delete(g.endpoints, victim)
		log.Printf("GATEWAY-HEALTH: endpoint verdict cache full (%d) — evicted %s",
			gatewayHealthMaxEndpoints, gatewayHealthEndpointLabel(victim))
	}
}

// gatewayHealthStore records a probe verdict for ONE endpoint and applies the
// ONE-per-episode bookkeeping, returning the transition (if any) that the caller
// must report. The zero value means "this verdict was not a transition" — the
// overwhelmingly common case, including every probe inside an episode already
// reported.
//
// The returned transition is computed under the lock so two concurrent probes
// can never both see the same flip; the EVENT is emitted by the caller, outside
// the lock, because an event write must never hold up an admission decision.
//
// `isGlobal` marks the daemon's own endpoint: when the global client was
// UNINSTALLED mid-probe the verdict is dropped (the fail-open state must not be
// overwritten with a stale measurement). A foreign endpoint has no install/
// uninstall lifecycle — the caller holds its client — so it is always stored.
func gatewayHealthStore(url string, healthy bool, lastErr string, at time.Time, isGlobal bool) gatewayHealthTransition {
	g := gatewayHealth
	g.mu.Lock()
	defer g.mu.Unlock()
	if isGlobal && (g.client == nil || g.client.baseURL != url) {
		return gatewayHealthTransition{} // the client was uninstalled/replaced mid-probe: keep the fail-open state
	}
	v := g.gatewayHealthEnsureEndpointLocked(url)
	v.healthy = healthy
	v.probedAt = at
	v.lastErr = lastErr

	ep := ""
	if !isGlobal {
		ep = url
	}
	if !healthy {
		if v.episodeActive {
			// Already reported: this is the SAME outage, however many projects
			// defer inside it. This branch is the "never one event per deferred
			// project" guarantee.
			return gatewayHealthTransition{}
		}
		v.episodeActive = true
		v.episodeDeferrals = 0
		return gatewayHealthTransition{kind: gatewayHealthEpisodeUnhealthy, reason: lastErr, endpoint: ep}
	}

	if !v.episodeActive {
		return gatewayHealthTransition{} // healthy verdict, no episode to close
	}
	n := v.episodeDeferrals
	v.episodeActive = false
	v.episodeDeferrals = 0
	return gatewayHealthTransition{kind: gatewayHealthEpisodeRecovered, deferrals: n, endpoint: ep}
}

// gatewayHealthNoteDeferral counts ONE deferral decision for one endpoint: the
// process-wide monotonic total the status surface reports, plus that endpoint's
// current episode tally the recovery event reports ("recovered after N
// deferrals"). Called on every path that returns deferSpawn=true — the cached
// verdict and the fresh failed probe alike, so the counter tracks decisions,
// not probes.
func gatewayHealthNoteDeferral(url string) {
	gatewayHealth.deferrals.Add(1)
	g := gatewayHealth
	g.mu.Lock()
	if v, ok := g.endpoints[url]; ok && v.episodeActive {
		v.episodeDeferrals++
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
//
// t_cadba34c note: this path sits INSIDE a spawn, where the drop site has no
// resolved endpoint in scope, so it charges the process-wide total and the
// DAEMON endpoint's episode tally (the coarse "how much did this outage cost"
// number). The per-call event below carries the project and the reason, which is
// what makes a foreign endpoint's mid-spawn blip identifiable after the fact.
func (g *gatewayHealthGateState) NoteTransientGatewayDeferral(project, reason string) {
	if g == nil {
		return
	}
	// Same order and lock shape as gatewayHealthNoteDeferral: the atomic total
	// first (the hot-path counter the status surface reads lock-free), then the
	// episode tally under the gate lock.
	g.deferrals.Add(1)
	g.mu.Lock()
	if v, ok := g.endpoints[g.globalKeyLocked()]; ok && v.episodeActive {
		v.episodeDeferrals++
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
// unwired logger (the gate then only logs). The event is a single row per
// endpoint-episode — deliberately NOT per project: an endpoint is one
// dependency, so N deferred projects behind it are one outage, and N rows would
// be the very per-lane noise the deferral exists to avoid.
//
// t_cadba34c: the global endpoint's transition (endpoint == "") keeps the
// pre-1724 message and payload byte-for-byte. A FOREIGN endpoint's transition
// names itself in the message and carries `endpoint` in the payload, so one dead
// remote gateway is attributable from the events table alone.
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
	at := ""
	if tr.endpoint != "" {
		at = " at " + tr.endpoint
	}
	switch tr.kind {
	case gatewayHealthEpisodeUnhealthy:
		// MEDIUM, mirroring the load-gate deferral and the slot-wait drop: work
		// the evaluator selected is not running, which an operator must see, but
		// the fleet is deferring-and-retrying by design rather than failing.
		details := map[string]any{
			"event_type": "gateway_health_defer",
			"reason":     tr.reason,
			"deferred":   true,
			"ttl_s":      int(gatewayHealthTTL.Seconds()),
		}
		if tr.endpoint != "" {
			details["endpoint"] = tr.endpoint
		}
		ev.Emit(context.Background(), SeverityMedium, "gateway_health",
			"gateway health gate deferring spawns"+at+": "+tr.reason, details)
	case gatewayHealthEpisodeRecovered:
		log.Printf("GATEWAY-HEALTH-GATE: recovered%s after %d deferral(s) — spawns admitted again", at, tr.deferrals)
		details := map[string]any{
			"event_type": "gateway_health_recovered",
			"deferrals":  tr.deferrals,
			"ttl_s":      int(gatewayHealthTTL.Seconds()),
		}
		if tr.endpoint != "" {
			details["endpoint"] = tr.endpoint
		}
		ev.Emit(context.Background(), SeverityInfo, "gateway_health",
			fmt.Sprintf("gateway health gate recovered%s after %d deferrals", at, tr.deferrals),
			details)
	}
}

// gatewayHealthDescribe renders the GLOBAL endpoint's cache entry for operator
// logs/tests (foreign endpoint entries are observable through their events).
func gatewayHealthDescribe() string {
	g := gatewayHealth
	g.mu.RLock()
	defer g.mu.RUnlock()
	state := "cold"
	v := g.endpoints[g.globalKeyLocked()]
	switch {
	case g.client == nil:
		state = "no-client (fail open)"
	case v != nil && !v.probedAt.IsZero():
		state = fmt.Sprintf("healthy=%t probed_at=%s err=%q", v.healthy, v.probedAt.UTC().Format(time.RFC3339), v.lastErr)
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
// fleet-wide state transition). t_cadba34c: the eval loop calls this ONLY
// for a deferral charged to the daemon's own (global) endpoint — a foreign
// lane endpoint's outage must never flip fleet-wide state.
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
//
// t_cadba34c: like the latch, this transition is GLOBAL-only — the eval loop
// calls it after a project was admitted through the daemon's own endpoint, never
// after a foreign lane endpoint admitted a lane, so an unrelated remote endpoint
// recovering can never clear the fleet's outage state or re-nudge its orphans.
func (l *Loop) noteGatewayReconnectedOnce() {
	if !l.gatewayDead {
		return
	}
	log.Printf("GATEWAY reconnected — resuming spawns")
	l.gatewayDead = false
	l.resumeOrphans("reconnect")
	l.resumeNeedsHuman("reconnect")
}
