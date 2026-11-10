package scheduler

import (
	"database/sql"
	"net/http"
	"net/http/httptest"
	"testing"
)

const schedFAILCNT001Gateway429 = `gateway POST: HTTP 429: {"error":"Too many concurrent runs (max 44)"}`

func schedFAILCNT001Counter(t *testing.T, name string, db *sql.DB) int {
	t.Helper()
	var counter int
	if err := db.QueryRow("SELECT consecutive_failures FROM projects WHERE name = ?", name).Scan(&counter); err != nil {
		t.Fatalf("read consecutive_failures: %v", err)
	}
	return counter
}

// TestSCHEDFAILCNT001_Gateway429DoesNotMoveCounter proves the raw gateway
// refusal passed to noteSpawnFailureClassed is excluded before any DB write.
func TestSCHEDFAILCNT001_Gateway429DoesNotMoveCounter(t *testing.T) {
	db := newTestDB(t)
	const project = "sched-failcnt-429-unit"
	mustCreateProjectINFRA012(t, db, project)
	if _, err := db.Exec("UPDATE projects SET consecutive_failures = 8 WHERE name = ?", project); err != nil {
		t.Fatalf("seed counter: %v", err)
	}

	spawner := NewSpawner(db, 1)
	before := schedFAILCNT001Counter(t, project, db)
	spawner.noteSpawnFailureClassed(project, schedFAILCNT001Gateway429)
	after := schedFAILCNT001Counter(t, project, db)
	if after != before {
		t.Fatalf("gateway 429 moved consecutive_failures from %d to %d", before, after)
	}
}

// TestSCHEDFAILCNT001_ThreeGateway429TicksThenCommittedTickResetsCounter
// models three transport-class terminal spawn attempts followed by the first
// committed tick. Transport attempts preserve the residue; completion clears it.
func TestSCHEDFAILCNT001_ThreeGateway429TicksThenCommittedTickResetsCounter(t *testing.T) {
	db := newTestDB(t)
	const project = "sched-failcnt-429-integration"
	mustCreateProjectINFRA012(t, db, project)
	if _, err := db.Exec("UPDATE projects SET consecutive_failures = 4 WHERE name = ?", project); err != nil {
		t.Fatalf("seed counter: %v", err)
	}

	var healthy bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if !healthy {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":"Too many concurrent runs (max 44)"}`))
			return
		}
		schedGap080CompletedResponse(w)
	}))
	t.Cleanup(srv.Close)

	spawner := schedGap1654Spawner(t, db, srv.URL)
	for i := 0; i < 3; i++ {
		tickID := "sched-failcnt-429-transport-" + string(rune('a'+i))
		insertRunningTick(t, db, tickID, project, 0)
		tick, err := spawner.Spawn(PackedProject{Name: project, Workdir: t.TempDir()}, tickID)
		if err != nil || tick == nil {
			t.Fatalf("transport tick %s = tick %v, err %v; want a deferred tick", tickID, tick, err)
		}
		if outcome := tick.Wait(); outcome.Status != TickDeferred {
			t.Fatalf("transport tick %s status = %s, want %s", tickID, outcome.Status, TickDeferred)
		}
	}
	if got := schedFAILCNT001Counter(t, project, db); got != 4 {
		t.Fatalf("counter after three gateway transport ticks = %d, want 4", got)
	}

	healthy = true
	const tickID = "sched-failcnt-429-committed"
	insertRunningTick(t, db, tickID, project, 0)
	tick, err := spawner.Spawn(PackedProject{Name: project, Workdir: t.TempDir()}, tickID)
	if err != nil {
		t.Fatalf("committed gateway spawn: %v", err)
	}
	outcome := tick.Wait()
	if outcome.Status != TickCompleted {
		t.Fatalf("committed gateway tick status = %s, want %s", outcome.Status, TickCompleted)
	}
	outcome.Commits = 1
	if err := NewLifecycleTracker(db).Complete(outcome); err != nil {
		t.Fatalf("complete committed tick: %v", err)
	}
	if got := schedFAILCNT001Counter(t, project, db); got != 0 {
		t.Fatalf("counter after committed tick = %d, want 0", got)
	}
}

func TestSCHEDFAILCNT001_OrdinaryFailureStillIncrementsCounter(t *testing.T) {
	db := newTestDB(t)
	const project = "sched-failcnt-ordinary"
	mustCreateProjectINFRA012(t, db, project)
	spawner := NewSpawner(db, 1)

	spawner.noteSpawnFailureClassed(project, "ordinary project failure")
	if got := schedFAILCNT001Counter(t, project, db); got != 1 {
		t.Fatalf("ordinary failure counter = %d, want 1", got)
	}
}
