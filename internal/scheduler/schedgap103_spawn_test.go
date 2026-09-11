package scheduler

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// SCHED-GAP-103: same-project double-spawn TOCTOU on non-armed lanes.
//
// SlotPool.Spawn is fire-and-forget: the caller launches the goroutine and
// returns, but the project only becomes visible to RunningSet when the
// goroutine's Acquire() lands. A second evaluation cycle inside that window
// (slot-freed debounce or ForceEvaluate) saw the project as idle and fired a
// second Spawn for it, producing two concurrent ticks for one project —
// bypassing cooldown (live evidence: asce-qa 55s apart against a 43200s
// cooldown, crier-sync 2s apart). Post-GAP-101 the armed foreman lanes are
// single-fly, but qa/sync/dogfood lanes are not armed and stay exposed.
//
// The fix reserves the project name atomically at Spawn time (tryReserve),
// folds reservations into RunningSet (the dedup view the packer and the
// evaluation loop consult), and clears the reservation on every goroutine
// exit path (clearReserve).

// TestSlotPool_TryReservePreventsDoubleSpawn pins the reservation primitive:
// the first claim wins, a concurrent claim for the same name is refused while
// the reservation is held, and the name is claimable again once released.
func TestSlotPool_TryReservePreventsDoubleSpawn(t *testing.T) {
	db := newTestDB(t)
	lc := NewLifecycleTracker(db)
	sp := NewSpawner(db, 1)
	pool := NewSlotPool(1, 10*time.Second, sp, lc)

	if !pool.tryReserve("gap103-proj") {
		t.Fatal("first tryReserve = false, want true (project was idle)")
	}
	if pool.tryReserve("gap103-proj") {
		t.Error("second tryReserve = true, want false — reservation did not dedup")
	}

	pool.clearReserve("gap103-proj")

	if !pool.tryReserve("gap103-proj") {
		t.Error("tryReserve after clearReserve = false, want true — reservation leaked")
	}
}

// TestSlotPool_TryReserveRefusedWhileRunning pins the other half of
// tryReserve: a project that already holds a slot can never be reserved, so
// Spawn can never launch a duplicate goroutine for a running project.
func TestSlotPool_TryReserveRefusedWhileRunning(t *testing.T) {
	db := newTestDB(t)
	lc := NewLifecycleTracker(db)
	sp := NewSpawner(db, 1)
	pool := NewSlotPool(1, 10*time.Second, sp, lc)

	if !pool.Acquire(context.Background(), "gap103-running") {
		t.Fatal("Acquire")
	}
	if pool.tryReserve("gap103-running") {
		t.Error("tryReserve = true for a project holding a slot, want false")
	}
	pool.Release("gap103-running")
	if !pool.tryReserve("gap103-running") {
		t.Error("tryReserve = false after the slot was released, want true")
	}
}

// TestSlotPool_RunningSetIncludesReserved pins that the dedup view (used by
// the evaluation loop in tick_process.go and by the packer) reports a
// reserved-but-not-yet-acquired project as busy — that is the entire fix: a
// second eval cycle no longer sees the project as idle.
func TestSlotPool_RunningSetIncludesReserved(t *testing.T) {
	db := newTestDB(t)
	lc := NewLifecycleTracker(db)
	sp := NewSpawner(db, 1)
	pool := NewSlotPool(1, 10*time.Second, sp, lc)

	if !pool.tryReserve("gap103-proj") {
		t.Fatal("tryReserve")
	}
	if !pool.RunningSet()["gap103-proj"] {
		t.Errorf("RunningSet = %v, want it to contain the reserved project", pool.RunningSet())
	}

	pool.clearReserve("gap103-proj")

	if pool.RunningSet()["gap103-proj"] {
		t.Errorf("RunningSet = %v, want the cleared reservation gone", pool.RunningSet())
	}
}

// TestSlotPool_ReservationBlocksSecondSpawn walks the exact live sequence.
// The single slot is held by a blocked tick; two Spawn calls for the SAME
// project then arrive back-to-back (the second stands in for an eval cycle
// firing inside the first spawn's Acquire window). Exactly one tick row may
// ever be created for that project — pre-GAP-103 both goroutines parked on
// Acquire and both spawned, producing the duplicate.
func TestSlotPool_ReservationBlocksSecondSpawn(t *testing.T) {
	db := newTestDB(t)

	const (
		blockerName = "gap103-blocker"
		targetName  = "gap103-target"
	)
	mustCreateProjectINFRA012(t, db, blockerName)
	mustCreateProjectINFRA012(t, db, targetName)

	// The blocker's gateway request hangs until released, keeping the pool's
	// only slot occupied while the two target spawns arrive.
	unblock := make(chan struct{})
	var unblockOnce sync.Once
	unblockBlocker := func() { unblockOnce.Do(func() { close(unblock) }) }

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), blockerName) {
			select {
			case <-unblock:
			case <-time.After(30 * time.Second): // safety net, never hang the suite
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":     "resp_gap103",
			"status": "completed",
			"output": []map[string]any{},
			"usage":  map[string]int{"input_tokens": 100, "output_tokens": 10, "total_tokens": 110},
		})
	}))
	defer srv.Close()
	// Deferred AFTER srv.Close so it runs FIRST (LIFO): a failing test
	// unblocks the handler before httptest waits on outstanding requests.
	defer unblockBlocker()

	spawner := NewSpawner(db, 1)
	spawner.SetGatewayClient(NewGatewayClient(srv.URL, "sk-daemon-shared", 30*time.Second))
	spawner.SetNoExecFallback(true)

	lc := NewLifecycleTracker(db)
	pool := NewSlotPool(1, 30*time.Second, spawner, lc)

	now := time.Now()
	pool.Spawn(PackedProject{Name: blockerName, Workdir: t.TempDir()}, now, true, nil)
	// SCHED-GAP-103: wait for the blocker to actually ACQUIRE its slot (Running()==1),
	// not just appear in RunningSet — the reservation fix makes RunningSet include
	// reserved names, which are set before the goroutine calls Acquire.
	waitForPool(t, 5*time.Second, "blocker to acquire the only slot", func() bool {
		return pool.Running() == 1
	})

	// Eval cycle #1 fires the target — no free slot, so its goroutine parks
	// on Acquire. The reservation must already be visible.
	firstTickID := pool.Spawn(PackedProject{Name: targetName, Workdir: t.TempDir()}, now, true, nil)
	if !pool.RunningSet()[targetName] {
		t.Fatalf("RunningSet = %v immediately after Spawn — reserved project not visible to the dedup view", pool.RunningSet())
	}

	// Eval cycle #2 fires the same project inside the Acquire window (this is
	// the TOCTOU race: pre-fix RunningSet had no entry, so this spawned a
	// second goroutine). The pool must refuse it synchronously.
	//
	// database.NextTickID is second-granular on the wall clock, so the sleep
	// makes the two ids distinct — that is what lets the assertions below
	// prove the refused spawn wrote NO row of its own (a same-second pair
	// shares an id and the second Enqueue would die on the PK conflict
	// instead, masking the duplicate).
	time.Sleep(1100 * time.Millisecond)
	secondTickID := pool.Spawn(PackedProject{Name: targetName, Workdir: t.TempDir()}, now, true, nil)
	if secondTickID == firstTickID {
		t.Fatalf("both spawns returned the same tick id %s — tick id generation is not unique", firstTickID)
	}
	if got := pool.Running(); got != 1 {
		t.Errorf("Running = %d after the duplicate Spawn, want 1 (only the blocker holds a slot)", got)
	}

	// Let everything drain: the blocker finishes, then the surviving target
	// tick acquires the slot and completes.
	unblockBlocker()
	waitForPool(t, 10*time.Second, "pool to drain", func() bool {
		return pool.Running() == 0 && !pool.RunningSet()[targetName]
	})

	// THE regression assertion: exactly ONE tick row for the target exists.
	// Pre-GAP-103 the second goroutine also acquired a slot and enqueued,
	// leaving two rows (the duplicate concurrent tick).
	var rows int
	if err := db.QueryRow(`SELECT COUNT(*) FROM ticks WHERE project_name = ?`, targetName).Scan(&rows); err != nil {
		t.Fatalf("count target ticks: %v", err)
	}
	if rows != 1 {
		t.Errorf("target tick rows = %d, want 1 — duplicate spawn leaked through the TOCTOU window (SCHED-GAP-103)", rows)
	}
	if got := tickStatusOf(t, db, firstTickID); got != string(TickCompleted) {
		t.Errorf("first target tick status = %q, want %q", got, TickCompleted)
	}
	var refusedRows int
	if err := db.QueryRow(`SELECT COUNT(*) FROM ticks WHERE id = ?`, secondTickID).Scan(&refusedRows); err != nil {
		t.Fatalf("count refused tick row: %v", err)
	}
	if refusedRows != 0 {
		t.Errorf("refused spawn %s wrote %d tick row(s), want 0 — the duplicate goroutine ran", secondTickID, refusedRows)
	}
}
