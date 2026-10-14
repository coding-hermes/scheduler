package scheduler

// t_cadba34c — ADMISSION IS PER ENDPOINT.
//
// SCHED-GAP-1712 gave a lane its own gateway endpoint (`projects.gateway_url` >
// `namespaces.gateway_url` > `[gateway].url`), but the daemon-wide gateway
// HEALTH GATE (SCHED-GAP-170) still asked ONE question at admission: is the
// DAEMON's gateway healthy? So a lane addressed at a remote box's gateway waited
// whenever the daemon's own gateway was down — a deferral caused by a dependency
// that lane was not going to use.
//
// MEASURED (live scheduler.db, 2026-10-03) — the decision, not an assumption:
//
//	lanes 593 / namespaces 19; lanes with a non-global resolved endpoint  0
//	  (migration 61 is in git but the daemon is not re-deployed, so today (a)
//	   and (b) are behaviourally identical and the decision rests on the harm
//	   the addressing feature is about to make reachable)
//	global-endpoint outage EPISODES 2026-09-21 → 09-27                        6
//	  deferrals charged to them (recovered events): 1, 12, 121, 4, 8
//	  09-27 01:20 → 01:35: 121 deferred spawns in ONE 15-minute global outage
//	transient per-lane gateway deferrals, same window:                       163
//
// A 121-spawn idle episode is the real cost of a dead daemon gateway, and it is
// CORRECT while every lane uses that gateway. Once a lane is addressed elsewhere
// (the fleet is standing up per-agent gateways on boxes 03/04 — SCHED-GAP-1723)
// that same episode would idle remote lanes for nothing, which contradicts the
// isolation law the derived GatewayClient already follows. Decision: (b) — the
// gate judges the RESOLVED endpoint, per lane.
//
// WHAT EACH TEST PINS (and why none is vacuous):
//
//  1. TestEndpointAdmission_AddressedLaneDispatchesWhileGlobalGatewayUnhealthy — the
//     ticket's exact harm, end to end: the daemon's gateway answers 503, an
//     addressed lane still dispatches to ITS endpoint (and its tick row records
//     the resolved endpoint), while a global lane is deferred in the SAME pass
//     with an event carrying the global endpoint.
//  2. TestEndpointAdmission_ForeignLaneEndpointFailsAlone — the isolation half: one
//     unreachable lane endpoint defers ONLY its own lane; the global lane and a
//     second addressed lane dispatch, and the fleet-wide `gatewayDead` latch is
//     NOT flipped by a foreign endpoint's outage.
//  3. TestEndpointAdmission_PerEndpointProbeCacheIsBounded — the verdict cache is per
//     ENDPOINT (one probe per endpoint per TTL, shared by every lane behind it),
//     including one entry per endpoint, the same cache not grow without limit
//     (gatewayHealthMaxEndpoints), and the global entry staying independent.
//  4. TestEndpointAdmission_SpawnNowJudgesTheResolvedEndpoint — the loop.go call site
//     is endpoint-aware too: an addressed lane is admitted while the daemon
//     gateway is down, and deferred (with a queued row + an endpoint-carrying
//     event) when ITS OWN endpoint is down.

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coding-hermes/scheduler/internal/clock"
	"github.com/coding-hermes/scheduler/internal/database"
)

// epAdmSetLaneEndpoint addresses a lane at its own gateway endpoint, the way
// the API/fleet.toml pin would.
func epAdmSetLaneEndpoint(t *testing.T, db *sql.DB, name, url string) {
	t.Helper()
	if _, err := db.Exec(`UPDATE projects SET gateway_url = ? WHERE name = ?`, url, name); err != nil {
		t.Fatalf("address lane %s at %s: %v", name, url, err)
	}
	var got string
	if err := db.QueryRow(`SELECT COALESCE(gateway_url, '') FROM projects WHERE name = ?`, name).Scan(&got); err != nil {
		t.Fatalf("read gateway_url of %s: %v", name, err)
	}
	if got != url {
		t.Fatalf("premise: gateway_url of %s = %q, want %q (the addressing did not round-trip)", name, got, url)
	}
}

// epAdmTickEndpoint reads the audit columns a dispatched tick records — the
// resolved endpoint and the tier each field came from.
func epAdmTickEndpoint(t *testing.T, db *sql.DB, project string) (url, src, keySrc string) {
	t.Helper()
	err := db.QueryRow(`SELECT COALESCE(gateway_url, ''), COALESCE(gateway_source, ''), COALESCE(gateway_key_source, '')
		FROM ticks WHERE project_name = ? ORDER BY created_at DESC, id DESC LIMIT 1`, project).Scan(&url, &src, &keySrc)
	if err != nil {
		t.Fatalf("read the resolved endpoint of %s's tick: %v", project, err)
	}
	return url, src, keySrc
}

// epAdmPathStub is a gateway stub that can serve several ENDPOINTS at once by
// path prefix (the endpoint identity in the gate is the client's baseURL), so a
// single server can play the daemon gateway and N lane gateways while counting
// probes PER ENDPOINT — which is what makes "one probe per endpoint per window"
// measurable rather than asserted in prose.
type epAdmPathStub struct {
	srv       *httptest.Server
	mu        sync.Mutex
	hits      map[string]int
	downPaths map[string]bool
	spawns    int
}

func newEpAdmPathStub(t *testing.T) *epAdmPathStub {
	t.Helper()
	s := &epAdmPathStub{hits: map[string]int{}, downPaths: map[string]bool{}}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/health"):
			s.mu.Lock()
			s.hits[r.URL.Path]++
			down := s.downPaths[r.URL.Path]
			s.mu.Unlock()
			if down {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			w.WriteHeader(http.StatusOK)
		case strings.HasSuffix(r.URL.Path, "/v1/responses"):
			s.mu.Lock()
			s.spawns++
			s.mu.Unlock()
			schedGap080CompletedResponse(w)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(s.srv.Close)
	return s
}

// setDown makes (or restores) ONE endpoint path unhealthy, so a test can pin a
// single endpoint's outage without touching its neighbours.
func (s *epAdmPathStub) setDown(path string, down bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.downPaths[path] = down
}

func (s *epAdmPathStub) healthHits(path string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.hits[path]
}

func (s *epAdmPathStub) spawnCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.spawns
}

// TestEndpointAdmission_AddressedLaneDispatchesWhileGlobalGatewayUnhealthy is the
// ticket's harm, pinned end to end: the daemon gateway is dead, but a lane
// addressed at its own (healthy) gateway dispatches anyway, in the same pass that
// defers a global lane.
func TestEndpointAdmission_AddressedLaneDispatchesWhileGlobalGatewayUnhealthy(t *testing.T) {
	gap170ResetGate(t)
	const addressed, plain = "epAdm-addressed", "epAdm-global-lane"

	db, l, gwGlobal, ns, _ := gap170Fixture(t)
	gwGlobal.healthy.Store(false) // the DAEMON's gateway answers 503 on /health
	gwGlobal.wire(l)
	gap170AssertNoOtherGate(t, db, ns)

	foreign := newGap170Gateway(t) // the lane's own gateway: healthy

	gap146SeedLane(t, db, addressed, ns)
	gap146SeedLane(t, db, plain, ns)
	epAdmSetLaneEndpoint(t, db, addressed, foreign.srv.URL)

	// PREMISE — the global verdict really refuses, so the pass below is a real
	// outage pass and the addressed lane's dispatch is not a fail-open accident.
	if deferSpawn, reason, _ := GatewayHealthGateShouldDefer(); !deferSpawn {
		t.Fatalf("premise: the global gate admitted a spawn while the daemon gateway answered 503 (reason=%q)", reason)
	}

	logbuf := admitCaptureLog(t)
	l.evaluate()

	// The addressed lane reached ITS endpoint...
	waitUntil(t, 60*time.Second, "the addressed lane to reach its own gateway", func() bool {
		return foreign.spawns.Load() == 1
	})
	if got := foreign.spawns.Load(); got != 1 {
		t.Errorf("the addressed lane's own gateway saw %d spawn(s), want 1 — a dead DAEMON gateway must not idle a lane addressed elsewhere", got)
	}
	// ...and the daemon gateway was never asked to serve it.
	if got := gwGlobal.spawns.Load(); got != 0 {
		t.Errorf("the daemon gateway saw %d spawn(s), want 0 — the addressed lane must not be dispatched to the global endpoint", got)
	}

	// The GLOBAL lane was deferred in the same pass, with the grep-stable line.
	if got := gap170LogCount(logbuf, "GATEWAY-DEFER: deferring "+plain); got != 1 {
		t.Errorf("GATEWAY-DEFER lines for %s = %d, want 1 — the global lane must still be gated by the global endpoint", plain, got)
	}
	if got := gap170LogCount(logbuf, "GATEWAY-DEFER: deferring "+addressed); got != 0 {
		t.Errorf("GATEWAY-DEFER lines for %s = %d, want 0 — its endpoint was healthy", addressed, got)
	}
	evs := gap170DeferralEvents(t, db)
	if len(evs) != 1 {
		t.Fatalf("gateway deferral events = %d, want 1 (only the global lane; first: %+v)", len(evs), evs)
	}
	gap170AssertDeferralEvent(t, evs[0], plain, "")
	// The deferral event names the endpoint the gate judged, in the tick row's
	// own audit vocabulary — that is how a deferred tick shows WHY and AGAINST
	// WHICH endpoint (the evaluation path defers before any row exists).
	if got, _ := evs[0].Details["gateway_source"].(string); got != "global" {
		t.Errorf("deferral event gateway_source = %q, want %q", got, "global")
	}
	if got, _ := evs[0].Details["gateway_url"].(string); got != gwGlobal.srv.URL {
		t.Errorf("deferral event gateway_url = %q, want the daemon endpoint %q", got, gwGlobal.srv.URL)
	}

	// The addressed lane's tick row carries ITS resolved endpoint — the audit
	// trail SCHED-GAP-1712 defined, now reached by an endpoint-aware admission.
	url, src, keySrc := epAdmTickEndpoint(t, db, addressed)
	if url != foreign.srv.URL || src != "lane" {
		t.Errorf("tick endpoint for %s = (%q, %q), want (%q, %q) — the resolved endpoint must be recorded on the dispatched row",
			addressed, url, src, foreign.srv.URL, "lane")
	}
	// The lane sets no key, so the credential tier resolves to the daemon's
	// shared key — recorded honestly as "global" (the tier law is per field).
	if keySrc != "global" {
		t.Errorf("tick gateway_key_source for %s = %q, want %q (the lane sets no key, so the key tier inherits the daemon's)", addressed, keySrc, "global")
	}
	if got := gap146TickRows(t, db, plain, ""); got != 0 {
		t.Errorf("tick rows for the deferred global lane = %d, want 0 — a deferred spawn creates no row", got)
	}
}

// TestEndpointAdmission_ForeignLaneEndpointFailsAlone pins the isolation law: one
// unreachable LANE endpoint defers only the lanes behind it, never the fleet —
// the global lane and a second addressed lane both dispatch, and the fleet-wide
// gatewayDead latch stays clear.
func TestEndpointAdmission_ForeignLaneEndpointFailsAlone(t *testing.T) {
	gap170ResetGate(t)
	const globalLane, downLane, upLane = "epAdm-iso-global", "epAdm-iso-down", "epAdm-iso-up"

	db, l, gwGlobal, ns, _ := gap170Fixture(t) // the daemon gateway is HEALTHY
	gwGlobal.wire(l)
	gap170AssertNoOtherGate(t, db, ns)

	down := newGap170Gateway(t)
	down.healthy.Store(false) // this lane's own gateway is unreachable
	up := newGap170Gateway(t)

	gap146SeedLane(t, db, globalLane, ns)
	gap146SeedLane(t, db, downLane, ns)
	gap146SeedLane(t, db, upLane, ns)
	epAdmSetLaneEndpoint(t, db, downLane, down.srv.URL)
	epAdmSetLaneEndpoint(t, db, upLane, up.srv.URL)

	// PREMISE — the global endpoint is HEALTHY, so nothing here can be explained
	// by a global outage: whatever defers is the lane endpoint's own verdict.
	if deferSpawn, reason, _ := GatewayHealthGateShouldDefer(); deferSpawn {
		t.Fatalf("premise: the healthy global gate deferred (reason=%q) — the foreign endpoint must be the only deferring one", reason)
	}

	logbuf := admitCaptureLog(t)
	l.evaluate()

	// Both addressable lanes with a healthy endpoint dispatch (the global lane to
	// the daemon gateway, the up lane to its own).
	waitUntil(t, 60*time.Second, "the two healthy lanes to dispatch", func() bool {
		return gwGlobal.spawns.Load() == 1 && up.spawns.Load() == 1
	})
	if got := gwGlobal.spawns.Load(); got != 1 {
		t.Errorf("the daemon gateway saw %d spawn(s), want 1 — a foreign endpoint's outage must not stop global lanes", got)
	}
	if got := up.spawns.Load(); got != 1 {
		t.Errorf("the OTHER addressed lane's gateway saw %d spawn(s), want 1 — one unreachable lane endpoint must not degrade the others", got)
	}
	// The unreachable endpoint was never dispatched to.
	if got := down.spawns.Load(); got != 0 {
		t.Errorf("the unreachable lane gateway saw %d spawn(s), want 0 — the gate must not make a spawn that will fail", got)
	}
	if got := gap170LogCount(logbuf, "GATEWAY-DEFER: deferring "+downLane); got != 1 {
		t.Errorf("GATEWAY-DEFER lines for %s = %d, want 1", downLane, got)
	}
	if got := gap170LogCount(logbuf, "GATEWAY-DEFER: deferring "+upLane); got != 0 {
		t.Errorf("GATEWAY-DEFER lines for %s = %d, want 0 — its endpoint answers", upLane, got)
	}
	if got := gap170LogCount(logbuf, "GATEWAY-DEFER: deferring "+globalLane); got != 0 {
		t.Errorf("GATEWAY-DEFER lines for %s = %d, want 0 — the daemon gateway is healthy", globalLane, got)
	}

	evs := gap170DeferralEvents(t, db)
	if len(evs) != 1 {
		t.Fatalf("gateway deferral events = %d, want exactly 1 (the failing lane; first: %+v)", len(evs), evs)
	}
	if got, _ := evs[0].Details["project"].(string); got != downLane {
		t.Errorf("the deferral event attributes project=%q, want %q — a foreign outage is charged to ITS lane", got, downLane)
	}
	if got, _ := evs[0].Details["gateway_url"].(string); got != down.srv.URL {
		t.Errorf("deferral event gateway_url = %q, want the lane's endpoint %q", got, down.srv.URL)
	}
	if got, _ := evs[0].Details["gateway_source"].(string); got != "lane" {
		t.Errorf("deferral event gateway_source = %q, want %q", got, "lane")
	}

	// THE ISOLATION LAW — the fleet-wide transitions belong to the daemon's own
	// endpoint. A lane endpoint being down must not latch gatewayDead (which
	// releases the pool and gates the escalator fleet-wide) nor, later, trigger
	// the reconnect/orphan-nudge.
	if l.gatewayDead {
		t.Errorf("gatewayDead latched by a FOREIGN lane endpoint outage — the fleet-wide transition belongs to the daemon endpoint only")
	}
}

// TestEndpointAdmission_PerEndpointProbeCacheIsBounded pins the cache shape the
// decision demands: per ENDPOINT (so N lanes behind one gateway cost one probe
// per window), shared by every lane behind that endpoint, bounded so a churning
// endpoint set cannot grow it without limit, and independent of the global
// entry.
func TestEndpointAdmission_PerEndpointProbeCacheIsBounded(t *testing.T) {
	gap170ResetGate(t)
	now := fixedEvalNow()
	SetGatewayHealthGateClock(clock.NewFixed(now))

	stub := newEpAdmPathStub(t)
	global := NewGatewayClient(stub.srv.URL+"/global", "", 5*time.Second)
	SetGatewayHealthGateClient(global)

	// The GLOBAL endpoint: one probe on the cold cache, then cached (the
	// pre-change cadence, unchanged).
	if deferSpawn, reason, err := GatewayHealthGateShouldDefer(); deferSpawn || reason != "" || err != nil {
		t.Fatalf("healthy global endpoint: (defer=%t reason=%q err=%v), want no deferral", deferSpawn, reason, err)
	}
	if got := stub.healthHits("/global/health"); got != 1 {
		t.Fatalf("probes of the global endpoint = %d, want 1", got)
	}
	if deferSpawn, _, _ := GatewayHealthGateShouldDefer(); deferSpawn {
		t.Errorf("the cached global verdict was not served within its TTL")
	}
	if got := stub.healthHits("/global/health"); got != 1 {
		t.Errorf("probes of the global endpoint within the TTL = %d, want 1 — the verdict is cached", got)
	}

	// A FOREIGN endpoint: probed once, then shared by every lane behind it.
	foreign := NewGatewayClient(stub.srv.URL+"/lane-a", "", 5*time.Second)
	for i := 0; i < 3; i++ {
		if deferSpawn, reason, _ := GatewayHealthGateShouldDeferEndpoint(foreign, ""); deferSpawn {
			t.Fatalf("healthy foreign endpoint deferred (%q)", reason)
		}
	}
	if got := stub.healthHits("/lane-a/health"); got != 1 {
		t.Errorf("probes of a foreign endpoint for 3 consults = %d, want 1 — N lanes behind one gateway cost ONE probe per window", got)
	}
	// ...and a DIFFERENT endpoint is probed on its own account.
	other := NewGatewayClient(stub.srv.URL+"/lane-b", "", 5*time.Second)
	if deferSpawn, _, _ := GatewayHealthGateShouldDeferEndpoint(other, ""); deferSpawn {
		t.Fatalf("second healthy foreign endpoint deferred")
	}
	if got := stub.healthHits("/lane-b/health"); got != 1 {
		t.Errorf("probes of the second endpoint = %d, want 1", got)
	}
	if got := stub.healthHits("/global/health"); got != 1 {
		t.Errorf("consults of a foreign endpoint probed the GLOBAL endpoint (%d hits) — endpoints must be judged independently", got)
	}

	// The TTL is per endpoint: rolling the window re-probes that endpoint only.
	SetGatewayHealthGateClock(clock.NewFixed(now.Add(gatewayHealthTTL + time.Second)))
	if deferSpawn, _, _ := GatewayHealthGateShouldDeferEndpoint(foreign, ""); deferSpawn {
		t.Errorf("the foreign endpoint deferred after the window rolled onto a healthy stub")
	}
	if got := stub.healthHits("/lane-a/health"); got != 2 {
		t.Errorf("probes of the foreign endpoint after its TTL rolled = %d, want 2 (expired windows re-probe)", got)
	}

	// A foreign endpoint's FAILURE is latched against itself, per endpoint.
	stub.setDown("/lane-a/health", true)
	SetGatewayHealthGateClock(clock.NewFixed(now.Add(2*gatewayHealthTTL + 2*time.Second)))
	if deferSpawn, reason, _ := GatewayHealthGateShouldDeferEndpoint(foreign, ""); !deferSpawn || reason == "" {
		t.Fatalf("a 503 foreign endpoint did not defer its own lane: (defer=%t reason=%q)", deferSpawn, reason)
	}
	if deferSpawn, _, _ := GatewayHealthGateShouldDeferEndpoint(other, ""); deferSpawn {
		t.Errorf("one endpoint's outage deferred ANOTHER endpoint's lanes — the verdict cache must be per endpoint")
	}
	if deferSpawn, _, _ := GatewayHealthGateShouldDefer(); deferSpawn {
		t.Errorf("a foreign endpoint's outage changed the GLOBAL verdict")
	}

	// BOUNDED: an ever-churning endpoint set cannot grow the cache without limit.
	for i := 0; i < gatewayHealthMaxEndpoints+5; i++ {
		c := NewGatewayClient(fmt.Sprintf("%s/churn-%d", stub.srv.URL, i), "", 5*time.Second)
		GatewayHealthGateShouldDeferEndpoint(c, "")
	}
	gatewayHealth.mu.RLock()
	tracked := len(gatewayHealth.endpoints)
	gatewayHealth.mu.RUnlock()
	if tracked > gatewayHealthMaxEndpoints {
		t.Errorf("endpoint verdicts tracked = %d, want <= %d — the per-endpoint cache must be bounded", tracked, gatewayHealthMaxEndpoints)
	}
	if tracked == 0 {
		t.Errorf("endpoint verdicts tracked = 0 — the cache never filled, so the bound proved nothing")
	}
}

// TestEndpointAdmission_SpawnNowJudgesTheResolvedEndpoint pins the OTHER call site
// (loop.go): the API spawn path is endpoint-aware too.
func TestEndpointAdmission_SpawnNowJudgesTheResolvedEndpoint(t *testing.T) {
	t.Run("addressed lane admitted while the daemon gateway is down", func(t *testing.T) {
		gap170ResetGate(t)
		const name = "epAdm-spawnnow-up"
		db, l, gwGlobal, ns, _ := gap170Fixture(t)
		gwGlobal.healthy.Store(false)
		gwGlobal.wire(l)
		gap170AssertNoOtherGate(t, db, ns)

		foreign := newGap170Gateway(t)
		gap146SeedLane(t, db, name, ns)
		epAdmSetLaneEndpoint(t, db, name, foreign.srv.URL)

		p, err := database.GetProject(context.Background(), db, name)
		if err != nil {
			t.Fatalf("GetProject %s: %v", name, err)
		}
		tickID, err := l.SpawnNow(*p)
		if err != nil || tickID == "" {
			t.Fatalf("SpawnNow for an addressed lane behind a healthy endpoint = (%q, %v), want a stored tick id and no error", tickID, err)
		}
		waitUntil(t, 60*time.Second, "the addressed lane to reach its own gateway", func() bool {
			return foreign.spawns.Load() == 1
		})
		if got := gwGlobal.spawns.Load(); got != 0 {
			t.Errorf("the daemon gateway saw %d spawn(s), want 0", got)
		}
		if got := len(gap170DeferralEvents(t, db)); got != 0 {
			t.Errorf("deferral events = %d, want 0 — the lane's own endpoint answers", got)
		}
	})

	t.Run("addressed lane deferred when its own endpoint is down", func(t *testing.T) {
		gap170ResetGate(t)
		const name = "epAdm-spawnnow-down"
		db, l, gwGlobal, ns, _ := gap170Fixture(t) // the daemon gateway is healthy
		gwGlobal.wire(l)
		gap170AssertNoOtherGate(t, db, ns)

		down := newGap170Gateway(t)
		down.healthy.Store(false)
		gap146SeedLane(t, db, name, ns)
		epAdmSetLaneEndpoint(t, db, name, down.srv.URL)

		want := gap146ReadState(t, db, name)
		p, err := database.GetProject(context.Background(), db, name)
		if err != nil {
			t.Fatalf("GetProject %s: %v", name, err)
		}
		tickID, err := l.SpawnNow(*p)
		if err != nil || tickID == "" {
			t.Fatalf("SpawnNow on a down LANE endpoint = (%q, %v), want a stored tick id and no error (the API contract is preserved)", tickID, err)
		}
		if got := gap146TickStatus(t, db, tickID); got != "queued" {
			t.Errorf("enqueued tick status = %q, want %q", got, "queued")
		}
		if got := down.spawns.Load(); got != 0 {
			t.Errorf("the down lane gateway saw %d spawn(s), want 0", got)
		}
		gap146AssertStateUnchanged(t, db, name, want, "t_cadba34c API deferral of an addressed lane")

		evs := gap170DeferralEvents(t, db)
		if len(evs) != 1 {
			t.Fatalf("gateway deferral events = %d, want 1 (first: %+v)", len(evs), evs)
		}
		gap170AssertDeferralEvent(t, evs[0], name, tickID)
		if got, _ := evs[0].Details["gateway_url"].(string); got != down.srv.URL {
			t.Errorf("deferral event gateway_url = %q, want the lane endpoint %q", got, down.srv.URL)
		}
		if got, _ := evs[0].Details["gateway_source"].(string); got != "lane" {
			t.Errorf("deferral event gateway_source = %q, want %q", got, "lane")
		}
		// The API path still never owns the fleet-wide latch.
		if l.gatewayDead {
			t.Errorf("gatewayDead latched by an API-triggered deferral of a FOREIGN endpoint")
		}
	})
}
