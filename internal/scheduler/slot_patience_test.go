package scheduler_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/coding-hermes/scheduler/internal/scheduler"
)

// TestSlotPool_PatienceEffectiveValue pins the patience contract
// (ADV-R08/G3): the default is the historical 5m window, SetPatience
// overrides it, and a d <= 0 keeps the current value (the drop always
// exists — "0" must never mean "never drop").
func TestSlotPool_SlotPatienceEffectiveValue(t *testing.T) {
	db := newTestDB(t)
	pool := scheduler.NewSlotPool(1, scheduler.NewSpawner(db, 1), scheduler.NewLifecycleTracker(db))

	if got := pool.Patience(); got != 5*time.Minute {
		t.Errorf("Patience() = %v on a fresh pool, want the 5m default", got)
	}

	pool.SetPatience(90 * time.Second)
	if got := pool.Patience(); got != 90*time.Second {
		t.Errorf("Patience() = %v after SetPatience(90s), want 90s", got)
	}

	// d <= 0 means "keep the default/current value", never "never drop".
	pool.SetPatience(0)
	if got := pool.Patience(); got != 90*time.Second {
		t.Errorf("Patience() = %v after SetPatience(0), want unchanged 90s", got)
	}
	pool.SetPatience(-time.Minute)
	if got := pool.Patience(); got != 90*time.Second {
		t.Errorf("Patience() = %v after SetPatience(-1m), want unchanged 90s", got)
	}
}

// TestSlotPool_SlotWaitExpiryEmitsEvent drives the full expiry path
// (ADV-R08/G3): the pool's only slot is held, a second project's spawn
// waits out its (shortened) patience and must be dropped WITH a MEDIUM
// slot_pool event naming the project — and WITHOUT ever creating a tick
// row (the drop returns before enqueue/StartRunning/Spawn).
func TestSlotPool_SlotPatienceExpiryEmitsEvent(t *testing.T) {
	db := newTestDB(t)

	const (
		holderName  = "advr08-holder"
		droppedName = "advr08-dropped"
		patience    = 50 * time.Millisecond
	)
	mustCreateProjectAt(t, db, droppedName, 10, 5, 60, 1.0)

	lc := scheduler.NewLifecycleTracker(db)
	sp := scheduler.NewSpawner(db, 1)
	pool := scheduler.NewSlotPool(1, sp, lc)

	// Occupy the only slot so the spawned project must park on Acquire.
	if !pool.Acquire(context.Background(), holderName) {
		t.Fatal("Acquire for the holder failed")
	}
	defer pool.Release(holderName)

	pool.SetPatience(patience)
	pool.SetEventLogger(scheduler.NewEventLogger(db))

	tickID := pool.Spawn(scheduler.PackedProject{Name: droppedName, Workdir: t.TempDir()}, time.Now(), true, db)

	// The drop fires at ~patience; poll the events table for it.
	var severity, component, message, detailsJSON string
	deadline := time.Now().Add(5 * time.Second)
	for {
		err := db.QueryRow(`
			SELECT severity, component, message, details
			FROM events WHERE component = 'slot_pool'
			ORDER BY id DESC LIMIT 1`).
			Scan(&severity, &component, &message, &detailsJSON)
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no slot_pool event within 5s (patience=%v): %v", patience, err)
		}
		time.Sleep(10 * time.Millisecond)
	}

	if severity != "MEDIUM" {
		t.Errorf("event severity = %q, want MEDIUM", severity)
	}
	if want := "slot wait expired — dropped " + droppedName; message != want {
		t.Errorf("event message = %q, want %q", message, want)
	}

	var details struct {
		Project         string  `json:"project"`
		TickID          string  `json:"tick_id"`
		WaitedSeconds   float64 `json:"waited_seconds"`
		PatienceSeconds float64 `json:"patience_seconds"`
		MaxSlots        int     `json:"max_slots"`
		Running         int     `json:"running"`
	}
	if err := json.Unmarshal([]byte(detailsJSON), &details); err != nil {
		t.Fatalf("event details not JSON: %v\nraw: %s", err, detailsJSON)
	}
	if details.Project != droppedName {
		t.Errorf("details.project = %q, want %q", details.Project, droppedName)
	}
	if details.TickID != tickID {
		t.Errorf("details.tick_id = %q, want the spawned tick id %q", details.TickID, tickID)
	}
	if details.WaitedSeconds <= 0 || details.WaitedSeconds < patience.Seconds() {
		t.Errorf("details.waited_seconds = %v, want >= patience (%v)", details.WaitedSeconds, patience.Seconds())
	}
	if details.PatienceSeconds != patience.Seconds() {
		t.Errorf("details.patience_seconds = %v, want %v", details.PatienceSeconds, patience.Seconds())
	}
	if details.MaxSlots != 1 {
		t.Errorf("details.max_slots = %d, want 1", details.MaxSlots)
	}
	if details.Running != 1 {
		t.Errorf("details.running = %d, want 1 (the holder's slot)", details.Running)
	}

	// The drop must return BEFORE enqueue/StartRunning/Spawn — no tick row
	// may exist for the dropped project.
	var rows int
	if err := db.QueryRow(`SELECT COUNT(*) FROM ticks WHERE project_name = ?`, droppedName).Scan(&rows); err != nil {
		t.Fatalf("count ticks for %s: %v", droppedName, err)
	}
	if rows != 0 {
		t.Errorf("tick rows for the dropped project = %d, want 0 (the drop must precede enqueue)", rows)
	}
}
