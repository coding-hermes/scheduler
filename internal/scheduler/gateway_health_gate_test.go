package scheduler

// SCHED-GAP-170 — the gateway-health deferral contract.
//
// THE DEFECT (first-hand code + live-data evidence, 2026-09-18). evaluate()'s
// gateway "liveness ping" guarded on `l.gatewayClient`, a field NOTHING in the
// package assigns: Loop.SetGatewayClient forwarded the client to the SPAWNER
// only, so the guard was permanently false and the fleet packed and spawned
// straight into a dead gateway. Live scheduler.db, 7 days to 2026-09-18:
// 793 of 859 failures carried "gateway unreachable and exec fallback disabled"
// (763 HTTP 503 gateway-drain, 18 connection-refused, 12 deadline-exceeded), and
// every one of them was booked as a LANE fault with a slot, a pick and a
// cooldown burned. 2026-09-18T03:41:10 shows the shape: 8 lanes failing in the
// same second, across the fleet, from one gateway that never answered.
//
// THE FIX (mirrors SCHED-GAP-171's load-gate deferral at the same admission
// point): the caller consults the gate BEFORE any row exists — Loop.evaluate per
// packed project (after the dedup skip and the load-gate check) and
// Loop.SpawnNow before the spawn session — against a 30s cached probe, so a dead
// gateway costs ZERO rows, ZERO cooldowns, ZERO failures and at most ONE probe
// per 30s. The deferral is reported as a `loop` event with a stable
// `event_type=gateway_defer` and the CACHED PROBE ERROR VERBATIM, and recovery
// needs no separate resume scan: the next pass re-picks the project.
//
// WHAT EACH TEST PINS, and why none of them is vacuous:
//
//  1. TestGatewayHealthGate_CacheTTL — the cache is real: the first call probes,
//     a call within the TTL does not, an EXPIRED window probes again and CATCHES
//     the flip, and the cached failure (not just a bool) carries the probe's own
//     error text. Plus the fail-open branch: no client installed ⇒ no probe, no
//     deferral (an absent signal must never stop the fleet).
//  2. TestGatewayHealthGate_UnauthorizedProbeDoesNotDefer — GAP-035's doctrine
//     survives the gate: a 401 probe is terminal-and-loud in the spawn path, so
//     the gate must NOT latch a fleet-wide stall on it.
//  3. TestGatewayHealthGate_DeadGatewayDeferral (AC3) — a REAL evaluation pass
//     against a gateway answering 503 on /health: 0 tick rows, 0 charges
//     (cooldown / failures / progress untouched), 0 spawns, 0 slots, and one
//     deferral event per pass. Asserted TWICE: the second pass defers the SAME
//     project, which is the "the pack still selects it" half — a deferral that
//     dropped the project from the pack would be a silent stall, not a defer.
//  4. TestGatewayHealthGate_OneProbePerPass — the cadence property the ticket
//     demands: TWO packed projects, ONE probe. The clock is rolled past the TTL
//     between passes so the second pass probes again (hits 1 → 2), which is what
//     makes "cached, not per project" and "not cached forever" both observable.
//  5. TestGatewayHealthGate_Recovery (AC4) — flip the stub dead → healthy, roll
//     the clock past the TTL, evaluate: the previously deferred project is
//     enqueued and spawned in that pass, with no second deferral event. The
//     gatewayDead latch is asserted to clear (the reconnect transition), since
//     the deferred pass is what latched it.
//  6. TestGatewayHealthGate_SpawnNowDefersWithEvent — the OTHER call site
//     (loop.go). Without it the API path could silently lose its gate again
//     while the eval-path tests stay green, so the deferral is pinned where it
//     is observable: a stored `queued` row whose id still resolves, no spawn, and
//     a `gateway_defer` event carrying that tick id.
//
// Every fixture checks its own PREMISE (the gate is actually refusing, the
// namespace cap is 0 so no other gate can be the reason) before it drives the
// path under test, and every gateway stub is a mock with exec fallback disabled
// — a gate that failed to bite spawns the MOCK, never a real foreman process.

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coding-hermes/scheduler/internal/clock"
	"github.com/coding-hermes/scheduler/internal/database"
)

// gap170Gateway is the gateway stub this file drives. /health answers per
// `healthy` (200 / 503) and counts every PROBE, which is what makes the cache
// cadence measurable instead of merely asserted in prose. `unauth` makes both
// endpoints reject the daemon key with 401 — the GAP-035 terminal class.
type gap170Gateway struct {
	srv        *httptest.Server
	healthy    atomic.Bool
	unauth     atomic.Bool
	healthHits atomic.Int32
	spawns     atomic.Int32
}

func newGap170Gateway(t *testing.T) *gap170Gateway {
	t.Helper()
	g := &gap170Gateway{}
	g.healthy.Store(true)
	g.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			g.healthHits.Add(1)
			if g.unauth.Load() {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"error":{"type":"auth_error","message":"Invalid gateway API key"}}`))
				return
			}
			if !g.healthy.Load() {
				// The measured production shape for an unreachable gateway:
				// the probe route itself fails.
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			w.WriteHeader(http.StatusOK)
		case "/v1/responses":
			if g.unauth.Load() {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"error":{"type":"auth_error","message":"Invalid gateway API key"}}`))
				return
			}
			g.spawns.Add(1)
			schedGap080CompletedResponse(w)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(g.srv.Close)
	return g
}

// wire attaches the stub to the loop the way production does (the single
// SetGatewayClient call is what registers it with the gate) and disables exec
// fallback so a refused spawn can never be rescued by a local process.
func (g *gap170Gateway) wire(l *Loop) {
	l.SetGatewayClient(NewGatewayClient(g.srv.URL, "sk-daemon-shared", 5*time.Second))
	l.spawner.SetNoExecFallback(true)
}

// gap170ResetGate restores the process-wide gate after a test. The cache is
// PACKAGE state (the gateway is one fleet-wide dependency, mirroring the load
// gate's package threshold), so a test that leaves a verdict or a client behind
// would decide the next test's admission.
func gap170ResetGate(t *testing.T) {
	t.Helper()
	t.Cleanup(func() {
		SetGatewayHealthGateClient(nil)
		SetGatewayHealthGateClock(clock.Real())
	})
}

// gap170Fixture builds ONE namespace (unlimited) and a loop with a gateway stub,
// on a pinned clock: the gateway gate is then the only gate that can act, so
// whatever the test observes is the gate under test. Each test seeds its own
// project(s) with gap146SeedLane (a distinctive scheduling state, so "unchanged"
// is a real assertion rather than a comparison against defaults).
func gap170Fixture(t *testing.T) (db *sql.DB, l *Loop, gw *gap170Gateway, ns string, now time.Time) {
	t.Helper()
	now = fixedEvalNow()
	db = newTestDB(t)
	ns = "gap170-ns"
	// cap 0 = unlimited (each test re-checks this premise).
	capTestNamespace(t, db, ns, 0, "cooldown")
	l = NewLoop(db, time.Minute, time.Hour, 10, 100, 10)
	l.SetClock(clock.NewFixed(now)) // deterministic selection instant
	l.noDeliver = true
	gw = newGap170Gateway(t)
	return db, l, gw, ns, now
}

// gap170AssertNoOtherGate fails the test when a gate other than the gateway
// gate could explain why nothing ran.
func gap170AssertNoOtherGate(t *testing.T, db *sql.DB, ns string) {
	t.Helper()
	if got := namespaceCapDB(db, ns); got != 0 {
		t.Fatalf("premise: namespace %s max_concurrent = %d, want 0 (unlimited) — the cap gate must not be the gate under test", ns, got)
	}
	if LoadGateShouldDefer(db, ns) {
		t.Fatalf("premise: the LOAD gate is active for namespace %q — the gateway gate must be the only gate firing", ns)
	}
}

// gap170Event is one decoded gateway deferral event.
type gap170Event struct {
	Component string
	Severity  string
	Message   string
	Details   map[string]any
}

// gap170DeferralEvents returns every gateway deferral event, oldest first,
// selected by the machine marker in the payload (not by message text), so the
// event is queryable on its own after the fact.
func gap170DeferralEvents(t *testing.T, db *sql.DB) []gap170Event {
	t.Helper()
	rows, err := db.Query(`SELECT component, severity, message, details FROM events
		WHERE json_extract(details, '$.event_type') = 'gateway_defer' ORDER BY id`)
	if err != nil {
		t.Fatalf("query gateway deferral events: %v", err)
	}
	defer rows.Close()
	var out []gap170Event
	for rows.Next() {
		var e gap170Event
		var raw string
		if err := rows.Scan(&e.Component, &e.Severity, &e.Message, &raw); err != nil {
			t.Fatalf("scan gateway deferral event: %v", err)
		}
		if err := json.Unmarshal([]byte(raw), &e.Details); err != nil {
			t.Fatalf("decode deferral event details %q: %v", raw, err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate gateway deferral events: %v", err)
	}
	return out
}

// gap170AssertDeferralEvent pins the payload contract: the component, the
// message, the machine marker, the project attribution, and the preserved probe
// error — plus tick_id when (and only when) a row already existed.
func gap170AssertDeferralEvent(t *testing.T, e gap170Event, project, tickID string) {
	t.Helper()
	if e.Component != "loop" {
		t.Errorf("deferral event component = %q, want %q (the admission decision is the loop's)", e.Component, "loop")
	}
	if got, want := e.Message, "gateway deferral for "+project; got != want {
		t.Errorf("deferral event message = %q, want %q", got, want)
	}
	if got, _ := e.Details["event_type"].(string); got != "gateway_defer" {
		t.Errorf("deferral event event_type = %q, want %q (the stable machine marker)", got, "gateway_defer")
	}
	if got, _ := e.Details["project"].(string); got != project {
		t.Errorf("deferral event project = %q, want %q", got, project)
	}
	if got, _ := e.Details["deferred"].(bool); !got {
		t.Errorf("deferral event deferred = %v, want true", e.Details["deferred"])
	}
	reason, _ := e.Details["reason"].(string)
	if reason == "" {
		t.Fatalf("deferral event carries no reason — the cached probe error must be preserved, not swallowed")
	}
	if !strings.Contains(reason, "503") {
		t.Errorf("deferral event reason = %q, want the probe's own error text (the stub answered 503 on /health)", reason)
	}
	if tickID == "" {
		if v, ok := e.Details["tick_id"]; ok {
			t.Errorf("deferral event carries tick_id=%v, want none — the evaluation path defers before any row exists", v)
		}
		return
	}
	if got, _ := e.Details["tick_id"].(string); got != tickID {
		t.Errorf("deferral event tick_id = %q, want %q (the id is how an API caller correlates the row that stays queued)", got, tickID)
	}
}

// TestGatewayHealthGate_CacheTTL pins the cache itself (AC1): one probe per
// gatewayHealthTTL window, the verdict (and its error text) preserved between
// probes, and a fresh probe once the window expires — which is what lets the
// gate SEE a recovery instead of trusting a stale verdict forever.
func TestGatewayHealthGate_CacheTTL(t *testing.T) {
	gap170ResetGate(t)
	gw := newGap170Gateway(t)
	t0 := fixedEvalNow()
	SetGatewayHealthGateClient(NewGatewayClient(gw.srv.URL, "sk-daemon-shared", 5*time.Second))
	SetGatewayHealthGateClock(clock.NewFixed(t0))

	// Cold cache ⇒ the first call probes and the verdict is healthy.
	if deferSpawn, reason, err := GatewayHealthGateShouldDefer(); deferSpawn || reason != "" || err != nil {
		t.Fatalf("healthy gateway: (defer=%t reason=%q err=%v), want no deferral", deferSpawn, reason, err)
	}
	if got := gw.healthHits.Load(); got != 1 {
		t.Fatalf("probes on the cold cache = %d, want 1", got)
	}

	// Within the TTL ⇒ cached. This is the property that makes one dead gateway
	// cost ONE probe instead of one per packed project.
	if deferSpawn, _, _ := GatewayHealthGateShouldDefer(); deferSpawn {
		t.Errorf("a second call within the %s TTL deferred the spawn", gatewayHealthTTL)
	}
	if got := gw.healthHits.Load(); got != 1 {
		t.Errorf("probes after a second call within the TTL = %d, want 1 — the verdict must be cached, not re-probed", got)
	}

	// The gateway dies INSIDE the window: the cached verdict is served (stale by
	// design — a TTL window is the documented price of not probing per pack).
	gw.healthy.Store(false)
	if deferSpawn, _, _ := GatewayHealthGateShouldDefer(); deferSpawn {
		t.Errorf("the cached healthy verdict was not served within its own TTL window")
	}
	if got := gw.healthHits.Load(); got != 1 {
		t.Errorf("probes inside the TTL window = %d, want 1", got)
	}

	// The window expires ⇒ probe again, and now the failure is the verdict,
	// carrying the probe's own error text.
	SetGatewayHealthGateClock(clock.NewFixed(t0.Add(gatewayHealthTTL + time.Second)))
	deferSpawn, reason, err := GatewayHealthGateShouldDefer()
	if !deferSpawn {
		t.Fatalf("the gate did not defer after the TTL window expired over a gateway answering 503")
	}
	if !strings.Contains(reason, "503") {
		t.Errorf("reason = %q, want the probe's error text (\"...HTTP 503\")", reason)
	}
	if err == nil {
		t.Errorf("err = nil for a deferring verdict — the caller must be able to relay the class")
	}
	if got := gw.healthHits.Load(); got != 2 {
		t.Errorf("probes after the TTL expired = %d, want 2 (the expired window must re-probe)", got)
	}

	// ...and the FAILURE is cached too: the next packed project costs no probe.
	if deferSpawn, cached, _ := GatewayHealthGateShouldDefer(); !deferSpawn || cached != reason {
		t.Errorf("cached failing verdict = (defer=%t reason=%q), want (true, %q)", deferSpawn, cached, reason)
	}
	if got := gw.healthHits.Load(); got != 2 {
		t.Errorf("probes after a call inside the failing verdict's TTL = %d, want 2", got)
	}

	// The gateway recovers; the gate must not see it until the window rolls.
	gw.healthy.Store(true)
	if deferSpawn, _, _ := GatewayHealthGateShouldDefer(); !deferSpawn {
		t.Errorf("the cached FAILING verdict flipped early — the TTL must be the only clock")
	}

	t.Run("no client fails open", func(t *testing.T) {
		before := gw.healthHits.Load()
		SetGatewayHealthGateClient(nil)
		// (a new client would re-arm the gate; here we want the no-client branch)
		if deferSpawn, reason, err := GatewayHealthGateShouldDefer(); deferSpawn || reason != "" || err != nil {
			t.Fatalf("no client installed: (defer=%t reason=%q err=%v), want fail-open (no deferral) — an absent signal must never stop the fleet",
				deferSpawn, reason, err)
		}
		if got := gw.healthHits.Load(); got != before {
			t.Errorf("probes with no client installed = %d, want %d (there is nothing to probe)", got, before)
		}
	})
}

// TestGatewayHealthGate_UnauthorizedProbeDoesNotDefer pins the GAP-035
// carve-out: a rejected key is TERMINAL and must stay loud. The spawn path
// classifies it as ErrGatewayKeyRejected and escalates it; a gate that latched
// "unreachable" on a 401 would replace that signal with a silent fleet stall.
func TestGatewayHealthGate_UnauthorizedProbeDoesNotDefer(t *testing.T) {
	gap170ResetGate(t)
	gw := newGap170Gateway(t)
	gw.unauth.Store(true)
	SetGatewayHealthGateClient(NewGatewayClient(gw.srv.URL, "sk-daemon-shared", 5*time.Second))
	SetGatewayHealthGateClock(clock.NewFixed(fixedEvalNow()))

	if deferSpawn, reason, err := GatewayHealthGateShouldDefer(); deferSpawn || reason != "" || err != nil {
		t.Fatalf("401 probe: (defer=%t reason=%q err=%v), want NO deferral — an auth rejection is terminal and must stay classified by the spawn path (GAP-035)",
			deferSpawn, reason, err)
	}
	if got := gw.healthHits.Load(); got != 1 {
		t.Errorf("probes = %d, want 1", got)
	}
	if got := gatewayHealthDescribe(); strings.Contains(got, "healthy=false") {
		t.Errorf("the gate latched a failing verdict on an auth rejection: %s", got)
	}
}

// TestGatewayHealthGate_DeadGatewayDeferral is AC3: a real evaluation pass
// against a gateway whose /health answers 503 creates NO row, charges NOTHING,
// takes NO slot, emits the deferral, and keeps the project selected.
func TestGatewayHealthGate_DeadGatewayDeferral(t *testing.T) {
	gap170ResetGate(t)
	const name = "gap170-dead-gateway"
	db, l, gw, ns, _ := gap170Fixture(t)
	gw.healthy.Store(false)
	gw.wire(l)
	gap170AssertNoOtherGate(t, db, ns)

	want := gap146SeedLane(t, db, name, ns)

	// PREMISE — the gate really refuses this project's spawn. Every "unchanged"
	// assertion below depends on this line: a gate that failed to bite would
	// spawn the mock and the test would report that instead.
	if deferSpawn, reason, _ := GatewayHealthGateShouldDefer(); !deferSpawn {
		t.Fatalf("premise: the gate admitted a spawn while the gateway answered 503 on /health (reason=%q)", reason)
	}

	logbuf := admitCaptureLog(t)
	l.evaluate()

	// The pass reached the packed project and DEFERRED it.
	if got := gap146LogCount(logbuf, "GATEWAY-DEFER: deferring "+name); got != 1 {
		t.Fatalf("GATEWAY-DEFER lines for %s = %d, want 1 — the evaluation pass must consult the gate at the admission point", name, got)
	}

	// AC3(a) — zero rows, for the project and for the DB.
	if got := gap146TickRows(t, db, name, ""); got != 0 {
		t.Errorf("tick rows for %s = %d, want 0 — a deferred spawn creates no row, not even a queued one", name, got)
	}
	if got := gap146TickCountAll(t, db); got != 0 {
		t.Errorf("tick rows in the whole DB = %d, want 0 — the deferral must not touch the ticks table at all", got)
	}
	if got := l.namespaceInflight(context.Background(), ns); got != 0 {
		t.Errorf("namespace in-flight = %d, want 0 — a stranded queued row would inflate this and defer the lane's siblings", got)
	}

	// AC3(b) — nothing charged: no cooldown extension, no failure, no progress
	// penalty (the 793 live failures were exactly this charge, booked per lane).
	gap146AssertStateUnchanged(t, db, name, want, "SCHED-GAP-170 gateway deferral")

	// AC3(c) — no slot, no reservation, no spawn.
	if got := l.slotPool.Running(); got != 0 {
		t.Errorf("slot pool Running() = %d, want 0 — a deferred project takes no global slot", got)
	}
	if l.slotPool.RunningSet()[name] {
		t.Errorf("%s is in RunningSet after a gateway deferral — no reservation may outlive it", name)
	}
	if got := gw.spawns.Load(); got != 0 {
		t.Errorf("the gateway saw %d spawn(s), want 0 — a deferred project must not reach the spawner", got)
	}

	// AC3(d) — the deferral is a `loop` event with the stable marker and the
	// probe's own error text.
	evs := gap170DeferralEvents(t, db)
	if len(evs) != 1 {
		t.Fatalf("gateway deferral events = %d, want 1 (first: %+v)", len(evs), evs)
	}
	gap170AssertDeferralEvent(t, evs[0], name, "")

	// AC3(e) — the pack STILL selects the project: a second pass defers the same
	// project again instead of dropping it (a drop would be a silent stall).
	l.evaluate()
	if got := gap170LogCount(logbuf, "GATEWAY-DEFER: deferring "+name); got != 2 {
		t.Errorf("GATEWAY-DEFER lines for %s across two deferred passes = %d, want 2 — the deferred project must stay selected", name, got)
	}
	if got := len(gap170DeferralEvents(t, db)); got != 2 {
		t.Errorf("gateway deferral events after two passes = %d, want 2", got)
	}
	if got := gap146TickRows(t, db, name, "queued"); got != 0 {
		t.Errorf("queued rows for %s = %d, want 0 — a deferred project must not leave a row nothing dispatches", name, got)
	}
	gap146AssertStateUnchanged(t, db, name, want, "SCHED-GAP-170 second deferral")

	// The latch + the reconnect owner: the pass that discovered the outage
	// latched gatewayDead (per-spawn signal) and released the pool.
	if !l.gatewayDead {
		t.Errorf("gatewayDead = false after a deferring pass — the dead transition (latch + ReleaseAll) must ride the first deferral")
	}
}

// TestGatewayHealthGate_OneProbePerPass pins the cadence the ticket demands: the
// cache is GATEWAY-wide, so a pass with two packed projects probes once — and a
// pass after the TTL window probes again (the window is not a permanent latch).
func TestGatewayHealthGate_OneProbePerPass(t *testing.T) {
	gap170ResetGate(t)
	db := newTestDB(t)
	const ns = "gap170-cadence-ns"
	capTestNamespace(t, db, ns, 0, "cooldown")
	const a, b = "gap170-cadence-a", "gap170-cadence-b"
	gap146SeedLane(t, db, a, ns)
	gap146SeedLane(t, db, b, ns)

	now := fixedEvalNow()
	l := NewLoop(db, time.Minute, time.Hour, 10, 100, 10)
	l.SetClock(clock.NewFixed(now))
	l.noDeliver = true
	gw := newGap170Gateway(t)
	gw.healthy.Store(false)
	gw.wire(l)
	gap170AssertNoOtherGate(t, db, ns)

	logbuf := admitCaptureLog(t)
	l.evaluate()

	if got := gap170LogCount(logbuf, "GATEWAY-DEFER: deferring "+a); got != 1 {
		t.Fatalf("GATEWAY-DEFER lines for %s = %d, want 1", a, got)
	}
	if got := gap170LogCount(logbuf, "GATEWAY-DEFER: deferring "+b); got != 1 {
		t.Fatalf("GATEWAY-DEFER lines for %s = %d, want 1", b, got)
	}
	if got := gw.healthHits.Load(); got != 1 {
		t.Fatalf("probes for a 2-project pass = %d, want 1 — the verdict is GATEWAY-wide; probing per packed project is the anti-pattern", got)
	}
	evs := gap170DeferralEvents(t, db)
	if len(evs) != 2 {
		t.Fatalf("deferral events for a 2-project pass = %d, want 2 (one per deferred project)", len(evs))
	}
	seen := map[string]bool{}
	for _, e := range evs {
		name, _ := e.Details["project"].(string)
		seen[name] = true
		gap170AssertDeferralEvent(t, e, name, "")
	}
	if !seen[a] || !seen[b] {
		t.Errorf("deferred projects = %v, want both %s and %s", seen, a, b)
	}

	// Roll the window: the second pass must probe again and defer again — a cache
	// that never expires would pin a stale verdict on a long outage.
	l.SetClock(clock.NewFixed(now.Add(gatewayHealthTTL + time.Second)))
	l.evaluate()
	if got := gw.healthHits.Load(); got != 2 {
		t.Errorf("probes after the TTL window rolled = %d, want 2 (one per window, not one per project and not zero)", got)
	}
	if got := len(gap170DeferralEvents(t, db)); got != 4 {
		t.Errorf("deferral events after two 2-project passes = %d, want 4", got)
	}
}

// TestGatewayHealthGate_Recovery is AC4: when the gateway answers again, the
// next pass picks the previously deferred project up normally — no resume scan,
// no manual intervention, and the latch cleared.
func TestGatewayHealthGate_Recovery(t *testing.T) {
	gap170ResetGate(t)
	const name = "gap170-recovery"
	db, l, gw, ns, now := gap170Fixture(t)
	gw.healthy.Store(false)
	gw.wire(l)
	gap170AssertNoOtherGate(t, db, ns)

	want := gap146SeedLane(t, db, name, ns)
	l.evaluate()

	if got := gap146TickCountAll(t, db); got != 0 {
		t.Fatalf("premise: the deferred pass created %d tick row(s), want 0", got)
	}
	if !l.gatewayDead {
		t.Fatalf("premise: the outage pass did not latch gatewayDead")
	}
	// The outage charged the lane nothing: cooldown, failures and progress are
	// exactly where they were (the 793 live failures were this charge, per lane).
	gap146AssertStateUnchanged(t, db, name, want, "SCHED-GAP-170 outage pass")

	// The gateway comes back. The cached verdict is only re-probed once its
	// window rolls — that is the documented cost of the cache, so the test rolls
	// the clock (the same seam the gate measures on).
	gw.healthy.Store(true)
	l.SetClock(clock.NewFixed(now.Add(gatewayHealthTTL + time.Second)))
	l.evaluate()

	// The project is enqueued and spawned by THIS pass, with no deferral event.
	// The barrier is the GATEWAY, not the tick row: the row is enqueued before
	// the POST, so waiting on the row alone would race the spawn (and the test
	// would then close the stub out from under its own request).
	waitUntil(t, 20*time.Second, "the recovered gateway to accept the spawn", func() bool {
		return gw.spawns.Load() == 1
	})
	if got := gap146TickRows(t, db, name, ""); got != 1 {
		t.Errorf("tick rows for %s after recovery = %d, want 1 — the deferred project must be enqueued on the next pass", name, got)
	}
	if got := len(gap170DeferralEvents(t, db)); got != 1 {
		t.Errorf("deferral events after recovery = %d, want 1 (only the outage pass)", got)
	}
	if got := gw.spawns.Load(); got != 1 {
		t.Errorf("gateway spawns after recovery = %d, want 1 — the deferred project must be picked up on the next pass", got)
	}
	if l.gatewayDead {
		t.Errorf("gatewayDead is still latched after a healthy pass — the reconnect transition (latch clear + SCHED-GAP-091 orphan scan) must ride the first admitted project")
	}
	// The recovery pass is a NORMAL pass: the outage itself levied nothing, so
	// the lane comes back at its own cooldown/streak (never escalated by the
	// outage) and the successful tick is what resets the failure counter.
	after := gap146ReadState(t, db, name)
	if after.cooldownS != want.cooldownS || after.noProgressTicks != want.noProgressTicks {
		t.Errorf("after the recovery tick cooldown/streak = %d/%d, want %d/%d — the outage must not escalate the lane",
			after.cooldownS, after.noProgressTicks, want.cooldownS, want.noProgressTicks)
	}
	if after.consecutiveFailures != 0 {
		t.Errorf("consecutive_failures after a successful recovery tick = %d, want 0 (SCHED-GAP-137a reset)", after.consecutiveFailures)
	}
}

// TestGatewayHealthGate_SpawnNowDefersWithEvent pins the loop.go call site (AC2,
// second half): the API spawn path defers on a dead gateway WITHOUT breaking its
// contract — a stored, resolvable tick id whose row stays `queued` — and reports
// the deferral with that id.
func TestGatewayHealthGate_SpawnNowDefersWithEvent(t *testing.T) {
	gap170ResetGate(t)
	const name = "gap170-spawn-now"
	db, l, gw, ns, _ := gap170Fixture(t)
	gw.healthy.Store(false)
	gw.wire(l)
	gap170AssertNoOtherGate(t, db, ns)

	want := gap146SeedLane(t, db, name, ns)

	p, err := database.GetProject(context.Background(), db, name)
	if err != nil {
		t.Fatalf("GetProject %s: %v", name, err)
	}
	tickID, err := l.SpawnNow(*p)
	if err != nil || tickID == "" {
		t.Fatalf("SpawnNow on a dead gateway = (%q, %v), want a stored tick id and no error (the API contract is preserved)", tickID, err)
	}

	// The row exists and stays queued: it is the id the caller polls, so it must
	// resolve — this is the deferral, not a drop.
	if got := gap146TickStatus(t, db, tickID); got != "queued" {
		t.Errorf("enqueued tick status = %q, want %q", got, "queued")
	}
	if got := gap146TickRows(t, db, name, "running"); got != 0 {
		t.Errorf("running rows for %s = %d, want 0 — a deferral must not start a row", name, got)
	}
	if got := gw.spawns.Load(); got != 0 {
		t.Errorf("the gateway saw %d spawn(s), want 0 — the deferred row must not reach the spawner", got)
	}
	if got := l.slotPool.Running(); got != 0 {
		t.Errorf("slot pool Running() = %d, want 0", got)
	}
	if got := gap146TickNudgeCount(t, db, tickID); got != 0 {
		t.Errorf("nudge_count = %d, want 0 — a deferral spends no retry budget", got)
	}
	gap146AssertStateUnchanged(t, db, name, want, "SCHED-GAP-170 API deferral")

	evs := gap170DeferralEvents(t, db)
	if len(evs) != 1 {
		t.Fatalf("gateway deferral events = %d, want 1 (first: %+v)", len(evs), evs)
	}
	gap170AssertDeferralEvent(t, evs[0], name, tickID)

	// The API path REPORTS the deferral but does not own the fleet-wide latch:
	// gatewayDead belongs to the evaluation pass (a request goroutine must not
	// flip scheduling state for the whole fleet).
	if l.gatewayDead {
		t.Errorf("gatewayDead latched by an API-triggered deferral — the evaluation pass owns that transition")
	}
}

// gap170LogCount counts captured log lines containing substr (the shared
// logger is written from spawn goroutines, so the read takes its mutex).
func gap170LogCount(buf *admitLogBuf, substr string) int {
	buf.mu.Lock()
	defer buf.mu.Unlock()
	n := 0
	for _, ln := range strings.Split(buf.b.String(), "\n") {
		if strings.Contains(ln, substr) {
			n++
		}
	}
	return n
}

// ---------------------------------------------------------------------------
// SCHED-GAP-170 observability: the ARMED surface, the boot line and the
// ONE-per-episode transition events.
//
// Why these tests exist at all. The defect being closed is not "the gate is
// wrong" but "the gate was INERT and nothing said so": evaluate()'s liveness
// ping read a field no code assigned, so the guard was permanently false for
// months while the fleet spawned into a dead gateway (793 of 859 failures in
// the 7 days to 2026-09-18, every one booked as a lane fault). Replacing it
// with a working gate does not retire that risk — a gate that stops being
// consulted, or is never wired at all, is silent again. These tests pin the
// three things that make the new gate impossible to lose silently:
//
//  1. the ARMED accessor is computed from the installed client and nothing
//     else (armed=false is the inert-guard state, and it is now readable);
//  2. the boot line lands in the log exactly once per install, in both the
//     armed and the NOT ARMED form, so the first seconds of scheduler.log
//     answer "is the gate live?";
//  3. an outage is ONE event — one MEDIUM when the verdict flips unhealthy,
//     one INFO when it recovers — however many projects defer inside it.
// ---------------------------------------------------------------------------

// gap170GateState is a faithful snapshot of the gate's process-wide state. The
// gate is package state, so a test that needs a FRESH PROCESS has to save the
// state it found, clear it, and restore it afterwards — the same seam the
// load-gate tests use when they swap the package-level sampler.
type gap170GateState struct {
	client           *GatewayClient
	healthy          bool
	probedAt         time.Time
	lastErr          string
	events           *EventLogger
	episodeActive    bool
	episodeDeferrals uint64
	wired            bool
}

// gap170BootState clears the gate to its boot values (no client, cold cache,
// no logger, boot latch clear) and restores the previous state when the test
// ends.
//
// The exported setters cannot produce a fresh boot on their own:
// SetGatewayHealthGateClient deliberately REFUSES to re-log an unchanged client
// (that refusal is what keeps the boot line at exactly one per install in
// production), so a test that pins the boot line must clear the latch itself.
// The deferral COUNTER is deliberately left alone: it is monotonic since boot by
// contract, so the tests below assert deltas, never absolute values.
func gap170BootState(t *testing.T) {
	t.Helper()
	g := gatewayHealth
	g.mu.Lock()
	saved := gap170GateState{
		client:           g.client,
		healthy:          g.healthy,
		probedAt:         g.probedAt,
		lastErr:          g.lastErr,
		events:           g.events,
		episodeActive:    g.episodeActive,
		episodeDeferrals: g.episodeDeferrals,
		wired:            g.wired,
	}
	g.client = nil
	g.healthy = false
	g.probedAt = time.Time{}
	g.lastErr = ""
	g.events = nil
	g.episodeActive = false
	g.episodeDeferrals = 0
	g.wired = false
	g.mu.Unlock()

	t.Cleanup(func() {
		g.mu.Lock()
		g.client = saved.client
		g.healthy = saved.healthy
		g.probedAt = saved.probedAt
		g.lastErr = saved.lastErr
		g.events = saved.events
		g.episodeActive = saved.episodeActive
		g.episodeDeferrals = saved.episodeDeferrals
		g.wired = saved.wired
		g.mu.Unlock()
	})
}

// gap170Deferrals returns the gate's process-wide deferral counter.
func gap170Deferrals() uint64 {
	_, _, _, _, _, d := GatewayHealthGateStatus()
	return d
}

// gap170EpisodeEvents returns the gate's episode-transition events of one kind,
// oldest first, selected by the machine marker in the payload (never by message
// text), so both directions stay queryable after the fact.
func gap170EpisodeEvents(t *testing.T, db *sql.DB, eventType string) []gap170Event {
	t.Helper()
	rows, err := db.Query(`SELECT component, severity, message, details FROM events
		WHERE component = 'gateway_health' AND json_extract(details, '$.event_type') = ? ORDER BY id`, eventType)
	if err != nil {
		t.Fatalf("query %s events: %v", eventType, err)
	}
	defer rows.Close()
	var out []gap170Event
	for rows.Next() {
		var e gap170Event
		var raw string
		if err := rows.Scan(&e.Component, &e.Severity, &e.Message, &raw); err != nil {
			t.Fatalf("scan %s event: %v", eventType, err)
		}
		if err := json.Unmarshal([]byte(raw), &e.Details); err != nil {
			t.Fatalf("decode %s event details %q: %v", eventType, raw, err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate %s events: %v", eventType, err)
	}
	return out
}

// TestGatewayHealthGate_StatusArmedRequiresInstalledClient is the REGRESSION
// TEST for the defect this whole file is about: the gate must report itself
// armed if and only if a client is actually INSTALLED.
//
// It is deliberately hostile to the FIX-STUCK reading of the world. The wiring
// function has been called in the cases below, so a gate that computed "armed"
// from "was the wiring code reached?" — or from "is the cached verdict
// healthy?", or from any constant — would report true and this test would fail.
// Only the installed client answers it, and the RED proof in the commit
// evidence was produced by computing armed from the boot latch instead.
func TestGatewayHealthGate_StatusArmedRequiresInstalledClient(t *testing.T) {
	gap170BootState(t)
	gap170ResetGate(t)
	gw := newGap170Gateway(t)

	// A boot-fresh process: nothing installed, so nothing is gated.
	if armed, _, _, _, ttl, _ := GatewayHealthGateStatus(); armed {
		t.Fatalf("armed = true with no client installed — the gate would advertise a guard it does not have "+
			"(ttl=%s)", ttl)
	}

	// The failure mode the old code had: the wiring path RUNS but installs
	// nothing. Running the function must not be mistaken for arming the gate.
	SetGatewayHealthGateClient(nil)
	if armed, _, _, _, _, _ := GatewayHealthGateStatus(); armed {
		t.Fatalf("armed = true after SetGatewayHealthGateClient(nil) — armed must come from the INSTALLED " +
			"CLIENT, not from the wiring call having happened; this is the FIX-STUCK shape (a guard that " +
			"looks installed and never runs)")
	}

	// Installing one arms it — and the accessor must flip on the same call that
	// changes the gate's behavior, or an operator could not use it to predict
	// whether spawns get gated.
	SetGatewayHealthGateClient(NewGatewayClient(gw.srv.URL, "sk-daemon-shared", 5*time.Second))
	armed, _, _, _, _, _ := GatewayHealthGateStatus()
	if !armed {
		t.Fatalf("armed = false after installing a client — the gate IS gating spawns now; the surface must say so")
	}

	// Uninstalling disarms it again (and only then).
	SetGatewayHealthGateClient(nil)
	if armed, _, _, _, _, _ := GatewayHealthGateStatus(); armed {
		t.Fatalf("armed = true after the client was uninstalled — the gate is fail-open again, and the " +
			"surface must not keep advertising a guard")
	}
}

// TestGatewayHealthGate_BootLines pins the boot line: exactly one grep-able line
// per install, in both forms, including the NOT ARMED form — the whole point of
// which is that the fleet is UNGUARDED, which is precisely the state nobody
// could see before.
func TestGatewayHealthGate_BootLines(t *testing.T) {
	gap170BootState(t)
	gap170ResetGate(t)
	logbuf := admitCaptureLog(t)
	gw := newGap170Gateway(t)

	const notArmed = "GATEWAY-HEALTH-GATE: NOT ARMED (no gateway client) - spawns will not be gated"
	const armedLine = "GATEWAY-HEALTH-GATE: armed ttl=30s"

	// The daemon's explicit boot-state call (cmd/schedulerd, for a host that
	// never installs a client at all) writes the warning once ...
	LogGatewayHealthGateBootState()
	if got := gap170LogCount(logbuf, notArmed); got != 1 {
		t.Fatalf("NOT ARMED boot lines = %d, want 1 — the first seconds of scheduler.log must answer "+
			"\"is the gate live?\" on a host whose gate is unarmed (got %d)", got, got)
	}
	// ... and a second call is not a second install.
	LogGatewayHealthGateBootState()
	if got := gap170LogCount(logbuf, notArmed); got != 1 {
		t.Errorf("NOT ARMED boot lines after a second boot-state call = %d, want 1 (one line per install)", got)
	}

	// Installing a client logs the armed line, once.
	client := NewGatewayClient(gw.srv.URL, "sk-daemon-shared", 5*time.Second)
	SetGatewayHealthGateClient(client)
	if got := gap170LogCount(logbuf, armedLine); got != 1 {
		t.Fatalf("armed boot lines = %d, want 1 — a wired daemon must say so at boot", got)
	}
	// Re-installing the SAME client is not an install (the reconnector calls
	// this path every reconnect): no second line.
	SetGatewayHealthGateClient(client)
	if got := gap170LogCount(logbuf, armedLine); got != 1 {
		t.Errorf("armed boot lines after re-installing the same client = %d, want 1 — the line is "+
			"exactly-once-per-INSTALL, and a reconnect is not an install", got)
	}

	// A change back to nil is an install, and it must warn again: the gate just
	// went from gating spawns to gating nothing.
	SetGatewayHealthGateClient(nil)
	if got := gap170LogCount(logbuf, notArmed); got != 2 {
		t.Errorf("NOT ARMED boot lines after uninstalling = %d, want 2 — losing the client must be visible", got)
	}
	SetGatewayHealthGateClient(nil)
	if got, want := gap170LogCount(logbuf, "GATEWAY-HEALTH-GATE"), 3; got != want {
		t.Errorf("total GATEWAY-HEALTH-GATE lines = %d, want %d (2 warnings + 1 armed, nothing else)", got, want)
	}
}

// TestGatewayHealthGate_EpisodeEventsOncePerEpisode drives the gate directly,
// across the TTL window, and pins the episode contract: N consecutive deferrals
// produce ONE MEDIUM event, recovery produces ONE INFO event, and the recovery
// names how many deferrals the episode cost.
func TestGatewayHealthGate_EpisodeEventsOncePerEpisode(t *testing.T) {
	gap170BootState(t)
	gap170ResetGate(t)
	db := newTestDB(t)
	SetGatewayHealthGateEvents(NewEventLogger(db))
	gw := newGap170Gateway(t)
	gw.healthy.Store(false)
	t0 := fixedEvalNow()
	SetGatewayHealthGateClient(NewGatewayClient(gw.srv.URL, "sk-daemon-shared", 5*time.Second))
	SetGatewayHealthGateClock(clock.NewFixed(t0))

	before := gap170Deferrals()

	// Five deferral decisions inside one outage: the first probes and fails,
	// the other four are served from the cached failing verdict.
	const deferralsInEpisode = 5
	for i := 0; i < deferralsInEpisode; i++ {
		deferSpawn, _, _ := GatewayHealthGateShouldDefer()
		if !deferSpawn {
			t.Fatalf("deferral %d/5: the gate admitted a spawn while the gateway answered 503 (premise)", i+1)
		}
	}

	evs := gap170EpisodeEvents(t, db, "gateway_health_defer")
	if len(evs) != 1 {
		t.Fatalf("unhealthy episode events after %d deferrals = %d, want 1 — one event per EPISODE, not per "+
			"deferral (a fleet-wide gateway outage is one outage; N rows is the per-lane noise this gate exists "+
			"to remove)", deferralsInEpisode, len(evs))
	}
	if evs[0].Component != "gateway_health" {
		t.Errorf("episode event component = %q, want %q", evs[0].Component, "gateway_health")
	}
	if evs[0].Severity != "MEDIUM" {
		t.Errorf("unhealthy episode severity = %q, want MEDIUM (the load-gate deferral / slot-drop tier: work "+
			"is not running, but the fleet is deferring and retrying by design, not failing)", evs[0].Severity)
	}
	if !strings.Contains(evs[0].Message, "503") {
		t.Errorf("unhealthy episode message = %q, want the probe's own error text (the stub answers 503 on /health)", evs[0].Message)
	}
	if got, _ := evs[0].Details["reason"].(string); !strings.Contains(got, "503") {
		t.Errorf("episode event reason = %q, want the cached probe error verbatim", got)
	}

	// The counter follows the DECISIONS: one per deferral, not one per probe.
	if got, want := gap170Deferrals()-before, uint64(deferralsInEpisode); got != want {
		t.Errorf("deferrals_total delta = %d, want %d — the counter must count every deferral decision", got, want)
	}

	// A second pass after the TTL window re-probes (and fails again) — still the
	// SAME episode, so still exactly one event.
	SetGatewayHealthGateClock(clock.NewFixed(t0.Add(gatewayHealthTTL + time.Second)))
	if deferSpawn, _, _ := GatewayHealthGateShouldDefer(); !deferSpawn {
		t.Fatalf("the gate admitted a spawn after the TTL window rolled over a 503 gateway")
	}
	if got := len(gap170EpisodeEvents(t, db, "gateway_health_defer")); got != 1 {
		t.Errorf("unhealthy episode events after a second failing probe = %d, want 1 — a re-probe inside the "+
			"same outage is the same episode", got)
	}
	if got, want := gap170Deferrals()-before, uint64(deferralsInEpisode+1); got != want {
		t.Errorf("deferrals_total delta after the second pass = %d, want %d", got, want)
	}

	// The gateway answers again: exactly one INFO event, naming what the outage
	// cost. This is the transition an operator can actually act on ("the fleet
	// was held for N deferrals, and it is moving again").
	gw.healthy.Store(true)
	SetGatewayHealthGateClock(clock.NewFixed(t0.Add(2 * (gatewayHealthTTL + time.Second))))
	if deferSpawn, _, _ := GatewayHealthGateShouldDefer(); deferSpawn {
		t.Fatalf("the gate kept deferring after the gateway recovered")
	}
	rec := gap170EpisodeEvents(t, db, "gateway_health_recovered")
	if len(rec) != 1 {
		t.Fatalf("recovery events = %d, want 1", len(rec))
	}
	if rec[0].Component != "gateway_health" || rec[0].Severity != "INFO" {
		t.Errorf("recovery event = (%s, %s), want (gateway_health, INFO)", rec[0].Component, rec[0].Severity)
	}
	if !strings.Contains(rec[0].Message, "after 6 deferrals") {
		t.Errorf("recovery message = %q, want it to name the episode's cost (\"after 6 deferrals\": 5 cached + 1 "+
			"after the TTL roll)", rec[0].Message)
	}
	if got, _ := rec[0].Details["deferrals"].(float64); int(got) != deferralsInEpisode+1 {
		t.Errorf("recovery event deferrals = %v, want %d", rec[0].Details["deferrals"], deferralsInEpisode+1)
	}

	// A second healthy call is not a transition: the episode is closed once.
	SetGatewayHealthGateClock(clock.NewFixed(t0.Add(3 * (gatewayHealthTTL + time.Second))))
	if deferSpawn, _, _ := GatewayHealthGateShouldDefer(); deferSpawn {
		t.Fatalf("premise: a healthy gateway must not defer")
	}
	if got := len(gap170EpisodeEvents(t, db, "gateway_health_recovered")); got != 1 {
		t.Errorf("recovery events after a second healthy probe = %d, want 1", got)
	}
}

// TestGatewayHealthGate_EpisodeEventNotPerProject is the fleet-level half of the
// episode contract: two lanes deferred in ONE pass produce two per-project
// deferral events (the lane audit trail) but only ONE gateway-health episode
// event — the gateway is a single fleet-wide dependency, and the whole point of
// the gate is that one dead gateway is one event, not 60.
func TestGatewayHealthGate_EpisodeEventNotPerProject(t *testing.T) {
	gap170BootState(t)
	gap170ResetGate(t)
	const a, b = "gap170-episode-a", "gap170-episode-b"
	db, l, gw, ns, _ := gap170Fixture(t)
	gw.healthy.Store(false)
	gw.wire(l)
	gap170AssertNoOtherGate(t, db, ns)
	gap146SeedLane(t, db, a, ns)
	gap146SeedLane(t, db, b, ns)

	// PREMISE: the pass really defers BOTH lanes to the gateway.
	l.evaluate()
	if got := len(gap170DeferralEvents(t, db)); got != 2 {
		t.Fatalf("per-project deferral events = %d, want 2 — both packed lanes must be deferred for this test "+
			"to say anything about episode throttle", got)
	}
	if got := len(gap170EpisodeEvents(t, db, "gateway_health_defer")); got != 1 {
		t.Fatalf("unhealthy episode events for a 2-lane outage = %d, want 1 — never one per deferred project", got)
	}

	// A second deferred pass inside the same outage: two more per-project
	// events, still ONE episode event.
	l.evaluate()
	if got := len(gap170DeferralEvents(t, db)); got != 4 {
		t.Errorf("per-project deferral events after two passes = %d, want 4", got)
	}
	if got := len(gap170EpisodeEvents(t, db, "gateway_health_defer")); got != 1 {
		t.Errorf("unhealthy episode events after two deferred passes = %d, want 1 — the episode outlives the "+
			"passes inside it", got)
	}
}

// TestGatewayHealthGate_StatusMatchesCachedVerdict pins the accessor against the
// cache it reports on: armed from the installed client, healthy/probed_at from
// the verdict the gate actually serves, ttl from the gate's own window, and the
// deferral counter moving with real deferral decisions. A status surface that
// disagreed with the gate would be worse than none — it would be trusted.
func TestGatewayHealthGate_StatusMatchesCachedVerdict(t *testing.T) {
	gap170BootState(t)
	gap170ResetGate(t)
	gw := newGap170Gateway(t)
	gw.healthy.Store(false)
	t0 := fixedEvalNow()
	SetGatewayHealthGateClient(NewGatewayClient(gw.srv.URL, "sk-daemon-shared", 5*time.Second))
	SetGatewayHealthGateClock(clock.NewFixed(t0))

	// Armed but COLD: no verdict has been measured yet. This is the state that
	// must not be mistaken for "healthy" (and must not be mistaken for "dead"
	// either — the honest answer is probed_at="").
	armed, healthy, probedAt, lastErr, ttl, deferrals := GatewayHealthGateStatus()
	if !armed {
		t.Fatalf("armed = false with a client installed (premise)")
	}
	if healthy || !probedAt.IsZero() || lastErr != "" {
		t.Fatalf("cold cache reported as healthy=%t probed_at=%s last_error=%q, want (false, zero, \"\")",
			healthy, probedAt, lastErr)
	}
	if ttl != gatewayHealthTTL {
		t.Errorf("ttl = %s, want %s (the gate's own window)", ttl, gatewayHealthTTL)
	}

	// The failing probe becomes the cached verdict, and the status must serve
	// the SAME numbers the gate's decision was made from.
	before := deferrals
	deferSpawn, reason, _ := GatewayHealthGateShouldDefer()
	if !deferSpawn {
		t.Fatalf("the gate admitted a spawn while the gateway answered 503 (premise)")
	}
	armed, healthy, probedAt, lastErr, ttl, deferrals = GatewayHealthGateStatus()
	if !armed {
		t.Errorf("armed flipped to false after a probe — only an install/uninstall may move it")
	}
	if healthy {
		t.Errorf("healthy = true while the cached verdict is a failing probe")
	}
	if !probedAt.Equal(t0) {
		t.Errorf("probed_at = %s, want %s (the instant the verdict was measured, through the gate's clock)", probedAt, t0)
	}
	if lastErr != reason {
		t.Errorf("last_error = %q, want the probe error text the deferral reported (%q)", lastErr, reason)
	}
	if ttl != gatewayHealthTTL {
		t.Errorf("ttl = %s, want %s", ttl, gatewayHealthTTL)
	}
	if got := deferrals - before; got != 1 {
		t.Errorf("deferrals_total delta = %d, want 1 for one deferral decision", got)
	}

	// Recovery: the verdict (and the error text) follows the gateway, not the
	// gate's opinion of it.
	gw.healthy.Store(true)
	SetGatewayHealthGateClock(clock.NewFixed(t0.Add(gatewayHealthTTL + time.Second)))
	if deferSpawn, _, _ := GatewayHealthGateShouldDefer(); deferSpawn {
		t.Fatalf("the gate kept deferring after the gateway recovered")
	}
	armed, healthy, probedAt, lastErr, _, _ = GatewayHealthGateStatus()
	if !armed || !healthy {
		t.Errorf("after recovery status = (armed=%t healthy=%t), want (true, true)", armed, healthy)
	}
	if want := t0.Add(gatewayHealthTTL + time.Second); !probedAt.Equal(want) {
		t.Errorf("probed_at = %s, want %s (the recovery probe's instant)", probedAt, want)
	}
	if lastErr != "" {
		t.Errorf("last_error = %q after recovery, want \"\" — a stale failure text would misreport a healthy gateway", lastErr)
	}
}
