package scheduler

// SCHED-GAP-144 — regression tests for the namespace-cap backstop at
// SlotPool.spawn (the declared admission point, G7).
//
// The defect these pin: only the packer and the orphan re-nudge respected
// `namespaces.max_concurrent`; every other entry into the pool (API spawn
// endpoint, wave resume, queue replay) could exceed it. Measured live: the
// 2026-09-17 23:51 restart admitted three `duckbrain-sync` lanes against a cap
// of ONE together with all 8 foremen, so the qa/pm/dogfood families got no
// global slot at all.
//
// Assertions are made against a BLOCKING gateway so overlap is observable:
// a tick that starts holds its response open until the test releases it, which
// makes "how many are running at once" a deterministic observation instead of a
// race against an instant completion.

import (
	"database/sql"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/coding-hermes/scheduler/internal/clock"
)

// blockingGateway answers /health immediately and holds every spawn response
// until release() is called — so concurrent ticks stay visibly concurrent.
//
// sim is the loop's clock seam (SCHED-GAP-197). The pre-seam tests asserted
// "the deferred spawn did not slip in" with a fixed wall-clock sleep
// (700ms / 500ms); the only engine that can actually let one slip in is
// SlotPool.waitNamespaceSlot's nsGatePollInterval poll, so the tests below
// drive that wait on a DORMANT simulator clock instead: each poll is an
// explicit Advance, and the number of polls the deferral survived is stated
// in the test rather than implied by a duration.
type blockingGateway struct {
	srv    *httptest.Server
	hold   chan struct{} // closed by release(): broadcast, every parked handler proceeds
	pass   chan struct{} // unbuffered: one token = exactly one parked handler proceeds
	starts chan string
	sim    *clock.SimClock
}

func newBlockingGateway(t *testing.T) *blockingGateway {
	t.Helper()
	sim := clock.NewManualSimClock(time.Date(2026, 9, 20, 14, 0, 0, 0, time.UTC))
	t.Cleanup(sim.Close)
	g := &blockingGateway{hold: make(chan struct{}), pass: make(chan struct{}), starts: make(chan string, 64), sim: sim}
	g.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			w.WriteHeader(http.StatusOK)
		case "/v1/responses":
			// Record arrival WITHOUT ever blocking the recorder: a spawn
			// goroutine parked in waitNamespaceSlot has not reached here yet,
			// so a blocking send would make the very deferral under test
			// unobservable (and stall the tick).
			select {
			case g.starts <- r.URL.Query().Get("project"):
			default:
			}
			select {
			case <-g.hold: // broadcast: everything parked may finish
			case <-g.pass: // single token: exactly ONE parked handler finishes
			case <-time.After(120 * time.Second):
			}
			schedGap080CompletedResponse(w)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(func() {
		g.release()
		g.srv.Close()
	})
	return g
}

func (g *blockingGateway) release() {
	select {
	case <-g.hold:
	default:
		close(g.hold)
	}
}

// releaseOneRequest hands a single completion token to exactly one parked
// handler: the next spawned tick finishes, the ones behind it stay parked. The
// caller must observe the corresponding forward progress (the start counter
// moving / a tick draining) before releasing the next one — that is what makes
// "freed slots admit exactly the waiters, one per release" a measured claim
// rather than a wall-clock window.
func (g *blockingGateway) releaseOneRequest() {
	select {
	case g.pass <- struct{}{}:
	case <-g.hold: // already broadcasting (cleanup/release) — nothing to do
	case <-time.After(60 * time.Second):
		panic("blockingGateway.releaseOneRequest: no parked handler accepted the token")
	}
}

// waitTickCount polls the tick-status census until it reaches want, or fails.
// It replaces fixed "sleep and hope the work finished" pads: the wait is a
// bounded real-clock poll because the work being awaited is genuinely
// concurrent bookkeeping (a spawn goroutine, an HTTP handler), not the clock.
func waitTickCount(t *testing.T, db *sql.DB, status string, want int) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	var got int
	for time.Now().Before(deadline) {
		got = countTicksInStatus(t, db, status)
		if got == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d tick(s) in status %q (last read: %d)", want, status, got)
}

// waitTickCountAtLeast polls until at least `want` tick rows are in `status`.
// Deliberately a lower bound, so a CAP BREACH surfaces as the test's own exact
// census assertion ("running = 3, want strictly 1") instead of a generic wait
// timeout — the diagnostic quality the pre-seam shape had.
func waitTickCountAtLeast(t *testing.T, db *sql.DB, status string, want int) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		if countTicksInStatus(t, db, status) >= want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for at least %d tick(s) in status %q (last read: %d)",
				want, status, countTicksInStatus(t, db, status))
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// waitAdmitted polls until at least one tick is RUNNING — the precondition for
// the deferral assertions (the first tick holds the namespace's only slot). A
// breach is left for the caller's exact count assertion to report.
func waitAdmitted(t *testing.T, db *sql.DB) {
	t.Helper()
	waitTickCountAtLeast(t, db, "running", 1)
}

// waitQueueDrained steps the gate's poll interval on the seam until no tick is
// left queued. Each advance wakes the parked waiters, which re-check the
// namespace; with the gateway already released, every admitted tick completes
// and frees the slot for the next poll. Bounded in REAL time (the loop is
// driving, so a failure is a stalled test, not a slow runner).
func waitQueueDrained(t *testing.T, db *sql.DB, sim *clock.SimClock) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		if countTicksInStatus(t, db, "queued") == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out driving the seam: queued = %d after 30s of poll intervals", countTicksInStatus(t, db, "queued"))
		}
		sim.Advance(nsGatePollInterval)
		time.Sleep(2 * time.Millisecond) // let the woken waiter reach the DB
	}
}

func (g *blockingGateway) wire(l *Loop) {
	l.SetClock(g.sim)
	l.SetGatewayClient(NewGatewayClient(g.srv.URL, "sk-test", 5*time.Second))
	l.spawner.SetNoExecFallback(true)
}

// capTestNamespace inserts a namespace row with an explicit cap + mode.
func capTestNamespace(t *testing.T, db *sql.DB, nsID string, maxConcurrent int, mode string) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO namespaces (id, weight, max_concurrent, admission_mode)
		VALUES (?, 10, ?, ?)`, nsID, maxConcurrent, mode); err != nil {
		t.Fatalf("insert namespace %s: %v", nsID, err)
	}
}

// queuedTickRow inserts the queued tick row a nudge/API spawn would have
// enqueued before handing it to the pool.
func queuedTickRow(t *testing.T, db *sql.DB, tickID, project string) {
	t.Helper()
	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := db.Exec(
		`INSERT INTO ticks (id, project_name, status, spawned_at, created_at)
		 VALUES (?, ?, 'queued', ?, ?)`, tickID, project, now, now); err != nil {
		t.Fatalf("insert queued tick %s: %v", tickID, err)
	}
}

func countTicksInStatus(t *testing.T, db *sql.DB, status string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM ticks WHERE status = ?`, status).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", status, err)
	}
	return n
}

func waitUntil(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out after %s waiting for %s", d, what)
}

// TestGAP144_NamespaceCapBackstop_DefersBeyondCap is the live defect replayed:
// three same-namespace spawns enter the POOL DIRECTLY (the path that bypassed
// the cap), so exactly one may run and the other two must stay queued — not
// dropped, not failed.
func TestGAP144_NamespaceCapBackstop_DefersBeyondCap(t *testing.T) {
	db := newTestDB(t)
	capTestNamespace(t, db, "sync-lanes", 1, "cooldown")
	for _, p := range []string{"a-sync", "b-sync", "c-sync"} {
		capTestProject(t, db, p, "sync-lanes")
		queuedTickRow(t, db, p+"-2026-09-18-00-00-00", p)
	}

	l := NewLoop(db, time.Minute, time.Hour, 10, 100, 10)
	gw := newBlockingGateway(t)
	gw.wire(l)

	for _, p := range []string{"a-sync", "b-sync", "c-sync"} {
		l.slotPool.SpawnEnqueued(PackedProject{Name: p, NamespaceID: "sync-lanes"},
			p+"-2026-09-18-00-00-00", time.Now(), true, db)
	}

	// One must start; the namespace has one slot. Wait for AT LEAST one so a
	// breach is reported by the exact census assertion below rather than as a
	// wait timeout.
	waitAdmitted(t, db)
	// The deferred two must not slip in. The only engine that could admit them
	// is waitNamespaceSlot's nsGatePollInterval poll, so step that wait
	// explicitly: FOUR poll intervals of virtual time (nsGatePollInterval each
	// — the const, never a hardcoded 250ms) with the namespace still full, then
	// assert the census. The pre-seam shape slept 700ms of real time, which is
	// 2.8 polls and therefore a weaker claim than the four below. Each advance
	// is gated on the DB still showing exactly one runner, so a breach that
	// happened at any poll is caught at that poll.
	for i := 0; i < 4; i++ {
		if got := countTicksInStatus(t, db, "running"); got != 1 {
			t.Fatalf("running = %d after %d gate poll(s), want exactly 1 (namespace cap 1) — the cap was breached", got, i)
		}
		gw.sim.Advance(nsGatePollInterval)
	}
	if got := countTicksInStatus(t, db, "running"); got != 1 {
		t.Fatalf("running = %d, want exactly 1 (namespace cap 1) — the cap was breached after %d gate polls", got, 4)
	}
	if got := countTicksInStatus(t, db, "queued"); got != 2 {
		t.Fatalf("queued = %d, want 2 (defer-not-drop: the rest wait, never fail)", got)
	}
	if got := countTicksInStatus(t, db, "failed"); got != 0 {
		t.Fatalf("failed = %d, want 0 — a busy namespace must not charge the lane a failure", got)
	}
	if got := l.slotPool.NamespacePending("sync-lanes"); got != 0 {
		t.Fatalf("namespace claims pending = %d, want 0 — a running row must own the occupancy (a held claim double-counts and halves an 8-slot cap)", got)
	}

	// Releasing the running tick lets a waiter through: defer, not drop.
	gw.release()
	waitQueueDrained(t, db, gw.sim)
	if got := countTicksInStatus(t, db, "failed"); got != 0 {
		t.Fatalf("failed = %d, want 0 after draining", got)
	}
}

// TestGAP144_UnlimitedNamespaceNotGated: max_concurrent 0 means unlimited —
// the declared override — so all three run concurrently.
func TestGAP144_UnlimitedNamespaceNotGated(t *testing.T) {
	db := newTestDB(t)
	capTestNamespace(t, db, "open-lanes", 0, "cooldown")
	for _, p := range []string{"a-sync", "b-sync", "c-sync"} {
		capTestProject(t, db, p, "open-lanes")
		queuedTickRow(t, db, p+"-t1", p)
	}

	l := NewLoop(db, time.Minute, time.Hour, 10, 100, 10)
	gw := newBlockingGateway(t)
	gw.wire(l)

	for _, p := range []string{"a-sync", "b-sync", "c-sync"} {
		l.slotPool.SpawnEnqueued(PackedProject{Name: p, NamespaceID: "open-lanes"}, p+"-t1", time.Now(), true, db)
	}
	waitTickCount(t, db, "running", 3) // unlimited: all three admitted at once
	gw.release()
}

// TestGAP144_EmergencyKillSwitch: SCHEDULER_NAMESPACE_CAP_GATE=off disables the
// gate fleet-wide (operator escape hatch).
func TestGAP144_EmergencyKillSwitch(t *testing.T) {
	t.Setenv("SCHEDULER_NAMESPACE_CAP_GATE", "off")
	db := newTestDB(t)
	capTestNamespace(t, db, "sync-lanes", 1, "cooldown")
	for _, p := range []string{"a-sync", "b-sync", "c-sync"} {
		capTestProject(t, db, p, "sync-lanes")
		queuedTickRow(t, db, p+"-t1", p)
	}

	l := NewLoop(db, time.Minute, time.Hour, 10, 100, 10)
	gw := newBlockingGateway(t)
	gw.wire(l)

	for _, p := range []string{"a-sync", "b-sync", "c-sync"} {
		l.slotPool.SpawnEnqueued(PackedProject{Name: p, NamespaceID: "sync-lanes"}, p+"-t1", time.Now(), true, db)
	}
	waitTickCount(t, db, "running", 3) // gate disabled → all three run
	gw.release()
}

// TestGAP144_FailOpenOnUnknownNamespace pins the fail-open contract at the
// helper level (the schema FK already forbids a project row pointing at a
// missing namespace, so this is where the contract is observable): an unknown
// namespace reads as cap 0 / running 0, which the gate treats as "unlimited",
// and a project with NO namespace is never gated at all.
func TestGAP144_FailOpenOnUnknownNamespace(t *testing.T) {
	db := newTestDB(t)

	if got := namespaceCapDB(db, "no-such-namespace"); got != 0 {
		t.Fatalf("namespaceCapDB(unknown) = %d, want 0 (unlimited/fail-open)", got)
	}
	if got := namespaceRunningDB(db, "no-such-namespace"); got != 0 {
		t.Fatalf("namespaceRunningDB(unknown) = %d, want 0 (fail-open)", got)
	}
	if got := namespaceCapDB(nil, "anything"); got != 0 {
		t.Fatalf("namespaceCapDB(nil db) = %d, want 0 (fail-open)", got)
	}

	// A project with no namespace (namespace_id NULL, the common fleet shape)
	// is never gated: it must spawn even while another namespace is saturated.
	capTestNamespace(t, db, "busy-lanes", 1, "cooldown")
	capTestProject(t, db, "busy-sync", "busy-lanes")
	queuedTickRow(t, db, "busy-sync-t1", "busy-sync")
	// Create it through the helper (which fills every NOT NULL column), then
	// NULL its namespace — the common shape for a repo-less/data-source lane.
	capTestProject(t, db, "naked", "busy-lanes")
	if _, err := db.Exec(`UPDATE projects SET namespace_id = NULL WHERE name = 'naked'`); err != nil {
		t.Fatalf("clear namespace on naked: %v", err)
	}
	queuedTickRow(t, db, "naked-t1", "naked")

	l := NewLoop(db, time.Minute, time.Hour, 10, 100, 10)
	gw := newBlockingGateway(t)
	gw.wire(l)

	l.slotPool.SpawnEnqueued(PackedProject{Name: "busy-sync", NamespaceID: "busy-lanes"}, "busy-sync-t1", time.Now(), true, db)
	l.slotPool.SpawnEnqueued(PackedProject{Name: "naked"}, "naked-t1", time.Now(), true, db)

	waitTickCount(t, db, "running", 2) // capped lane + namespace-less lane
	gw.release()
}

// TestGAP144_GlobalSlotWaitStillApplies: the namespace gate must not displace
// the global semaphore — with a global cap of 1, two different namespaces still
// run one at a time.
func TestGAP144_GlobalSlotWaitStillApplies(t *testing.T) {
	db := newTestDB(t)
	capTestNamespace(t, db, "ns-a", 5, "cooldown")
	capTestNamespace(t, db, "ns-b", 5, "cooldown")
	capTestProject(t, db, "p-a", "ns-a")
	capTestProject(t, db, "p-b", "ns-b")
	queuedTickRow(t, db, "p-a-t1", "p-a")
	queuedTickRow(t, db, "p-b-t1", "p-b")

	l := NewLoop(db, time.Minute, time.Hour, 10, 100, 1) // ONE global slot
	gw := newBlockingGateway(t)
	gw.wire(l)

	l.slotPool.SpawnEnqueued(PackedProject{Name: "p-a", NamespaceID: "ns-a"}, "p-a-t1", time.Now(), true, db)
	l.slotPool.SpawnEnqueued(PackedProject{Name: "p-b", NamespaceID: "ns-b"}, "p-b-t1", time.Now(), true, db)

	// Both namespaces are capped at 5, so the NAMESPACE gate admits both
	// attempts; the single GLOBAL slot is what must hold p-b back. That wait
	// is Acquire(sem), a channel — not the clock — so the polls below are the
	// explicit "still held" check the pre-seam 500ms sleep only sampled once
	// (and 500ms is 2 gate polls, so the old window could not even prove it
	// survived a poll cycle with the global slot still taken).
	waitAdmitted(t, db)
	for i := 0; i < 4; i++ {
		if got := countTicksInStatus(t, db, "running"); got != 1 {
			t.Fatalf("running = %d after %d gate poll(s), want 1 (global cap) — the namespace gate must not bypass the global cap", got, i)
		}
		gw.sim.Advance(nsGatePollInterval)
	}
	if got := countTicksInStatus(t, db, "running"); got != 1 {
		t.Fatalf("running = %d, want 1 (global cap) — the namespace gate must not bypass the global cap after %d gate polls", got, 4)
	}
	gw.release()
}
