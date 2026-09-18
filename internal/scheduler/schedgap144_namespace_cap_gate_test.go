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
)

// blockingGateway answers /health immediately and holds every spawn response
// until release() is called — so concurrent ticks stay visibly concurrent.
type blockingGateway struct {
	srv    *httptest.Server
	hold   chan struct{}
	starts chan string
}

func newBlockingGateway(t *testing.T) *blockingGateway {
	t.Helper()
	g := &blockingGateway{hold: make(chan struct{}), starts: make(chan string, 64)}
	g.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			w.WriteHeader(http.StatusOK)
		case "/v1/responses":
			select {
			case g.starts <- r.URL.Query().Get("project"):
				select {
				case <-g.hold:
				case <-time.After(120 * time.Second):
				}
			default:
			}
			select {
			case <-g.hold:
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

func (g *blockingGateway) wire(l *Loop) {
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

	// One must start; the namespace has one slot. (Poll for "at least one" and
	// then assert the exact count, so a breach reads as "running = 3, want 1
	// — the cap was breached" instead of a generic wait timeout.)
	waitUntil(t, 20*time.Second, "a tick to start", func() bool {
		return countTicksInStatus(t, db, "running") >= 1
	})
	// Give the deferred two well past the 250ms gate poll to prove they do NOT
	// slip in while the first is still running.
	time.Sleep(700 * time.Millisecond)
	if got := countTicksInStatus(t, db, "running"); got != 1 {
		t.Fatalf("running = %d, want exactly 1 (namespace cap 1) — the cap was breached", got)
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
	waitUntil(t, 30*time.Second, "all three ticks to drain through one slot", func() bool {
		return countTicksInStatus(t, db, "queued") == 0
	})
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
	waitUntil(t, 20*time.Second, "all three running (unlimited)", func() bool {
		return countTicksInStatus(t, db, "running") == 3
	})
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
	waitUntil(t, 20*time.Second, "gate disabled → all three run", func() bool {
		return countTicksInStatus(t, db, "running") == 3
	})
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

	waitUntil(t, 20*time.Second, "both the capped lane and the namespace-less lane to run", func() bool {
		return countTicksInStatus(t, db, "running") == 2
	})
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

	waitUntil(t, 20*time.Second, "one tick running under a global cap of 1", func() bool {
		return countTicksInStatus(t, db, "running") == 1
	})
	time.Sleep(500 * time.Millisecond)
	if got := countTicksInStatus(t, db, "running"); got != 1 {
		t.Fatalf("running = %d, want 1 (global cap) — the namespace gate must not bypass the global cap", got)
	}
	gw.release()
}
