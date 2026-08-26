package scheduler

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// Spawn-level regression test for SCHED-GAP-021 (2026-08-09, scheduler.log):
// ring-runner acquired a slot at 21:45:21; when asce's shorter tick completed
// at 21:58:12, the FIFO Release() popped RING-RUNNER's marker; two more
// completions popped two more markers; at 22:07:01 EVAL's dedup
// (tick_process.go: alreadyRunning := slotPool.RunningSet()) saw ring-runner
// as NOT running and spawned a duplicate concurrent tick while the 21:45
// session (PID 869980) was still live.
//
// This test drives the real SlotPool.Spawn path: a blocked gateway request
// keeps the slow project's tick running while the fast project's tick
// completes out of acquisition order. The completing tick's deferred
// Release(name) must free ONLY its own marker, and the dedup view must keep
// reporting the still-running project.

// waitForPool polls cond every 15ms until it holds or the deadline expires.
func waitForPool(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(15 * time.Millisecond)
	}
}

func TestSlotPool_OutOfOrderCompletionKeepsRunningMarker(t *testing.T) {
	db := newTestDB(t)

	const (
		slowName = "gap021-slow"
		fastName = "gap021-fast"
	)
	mustCreateProjectINFRA012(t, db, slowName)
	mustCreateProjectINFRA012(t, db, fastName)

	// Gateway handler: the slow project's request blocks until released
	// (simulating a long-running tick); every other request completes
	// immediately. The prompt embeds the tick ID, which starts with the
	// project name — that is how the handler tells the two spawns apart.
	unblock := make(chan struct{})
	var unblockOnce sync.Once
	unblockSlow := func() { unblockOnce.Do(func() { close(unblock) }) }

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), slowName) {
			select {
			case <-unblock:
			case <-time.After(30 * time.Second): // safety net, never hang the suite
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":     "resp_gap021",
			"status": "completed",
			"output": []map[string]any{},
			"usage":  map[string]int{},
		})
	}))
	defer srv.Close()
	// Deferred AFTER srv.Close so it runs FIRST (LIFO): a failing test
	// unblocks the slow handler before httptest waits for outstanding
	// requests, so cleanup never hangs on the 30s safety net.
	defer unblockSlow()

	spawner := NewSpawner(db, 2)
	spawner.SetGatewayClient(NewGatewayClient(srv.URL, "sk-daemon-shared", 30*time.Second))
	spawner.SetNoExecFallback(true)

	lc := NewLifecycleTracker(db)
	pool := NewSlotPool(2, 30*time.Second, spawner, lc)

	now := time.Now()
	pool.Spawn(PackedProject{Name: slowName, Workdir: t.TempDir()}, now, true, nil)

	// The slow tick must actually hold its slot before the fast one starts.
	waitForPool(t, 5*time.Second, "slow project to acquire its slot", func() bool {
		return pool.RunningSet()[slowName]
	})

	// The fast tick acquires the second slot and completes almost immediately
	// — out of acquisition order, exactly like the live incident.
	fastTickID := pool.Spawn(PackedProject{Name: fastName, Workdir: t.TempDir()}, now, true, nil)

	// Wait for the fast tick to COMPLETE. Its RunningSet entry is transient
	// (acquire→spawn→complete→release can all land inside one poll
	// interval), so poll the persisted tick row instead: status=completed is
	// written by lifecycle.Complete, which runs just before the deferred
	// Release in the same goroutine.
	waitForPool(t, 5*time.Second, "fast tick to complete", func() bool {
		var status string
		err := db.QueryRow(`SELECT status FROM ticks WHERE id = ?`, fastTickID).Scan(&status)
		return err == nil && status == string(TickCompleted)
	})

	// The fast tick must have completed for real — not failed to enqueue.
	if got := tickStatusOf(t, db, fastTickID); got != string(TickCompleted) {
		t.Errorf("fast tick status = %q, want %q", got, TickCompleted)
	}

	// Now wait for the fast tick's deferred Release to run (marker gone).
	waitForPool(t, 5*time.Second, "fast project to release its slot", func() bool {
		return !pool.RunningSet()[fastName]
	})

	// THE regression assertion: the still-running project's marker survived
	// the out-of-order completion, and ONLY that marker remains.
	rs := pool.RunningSet()
	if !rs[slowName] {
		t.Error("slow project's marker was evicted by the fast tick's release — SCHED-GAP-021 FIFO bug")
	}
	if len(rs) != 1 {
		t.Errorf("RunningSet = %v, want exactly {%s}", rs, slowName)
	}

	// ...which is the exact view the tick_process dedup consults before
	// spawning: it must still skip the running project (no duplicate spawn).
	if alreadyRunning := pool.RunningSet(); !alreadyRunning[slowName] {
		t.Error("dedup would NOT skip the still-running project — duplicate spawn possible (SCHED-GAP-021)")
	}

	// Cleanup: let the slow tick finish; its own release drains the pool.
	unblockSlow()
	waitForPool(t, 5*time.Second, "slow project to release its slot", func() bool {
		return pool.Running() == 0
	})
}
