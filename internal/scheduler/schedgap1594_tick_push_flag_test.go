package scheduler

import (
	"testing"
	"time"
)

// SCHED-GAP-1594 — the per-tick push flag plumbing. Default false
// (SCHED-GAP-1694's push-at-exit stays on, backward compat); SetTickPushDisabled
// flips the spawner field the Wait() completion path reads, and the Loop
// delegate reaches the same field the daemon arms once at boot.

// TestSCHEDGAP1594_DefaultPushEnabled — a zero-value Spawner and a fresh
// Loop must both report tick push ENABLED (default false for backward
// compat, per the row's acceptance).
func TestSCHEDGAP1594_DefaultPushEnabled(t *testing.T) {
	s := NewSpawner(nil, 1)
	if s.TickPushDisabled() {
		t.Fatal("fresh Spawner must keep the per-tick push enabled (default false)")
	}
}

// TestSCHEDGAP1594_SetterFlipsState — the setter flips the flag on and back
// off; the Loop delegate reads the same spawner field.
func TestSCHEDGAP1594_SetterFlipsState(t *testing.T) {
	s := NewSpawner(nil, 1)
	s.SetTickPushDisabled(true)
	if !s.TickPushDisabled() {
		t.Fatal("SetTickPushDisabled(true) must disable the per-tick push")
	}
	s.SetTickPushDisabled(false)
	if s.TickPushDisabled() {
		t.Fatal("SetTickPushDisabled(false) must restore the per-tick push")
	}

	db := newTestDB(t)
	l := NewLoop(db, 30*time.Second, 24*time.Hour, 10, 100, 4)
	t.Cleanup(l.Stop)
	if l.TickPushDisabled() {
		t.Fatal("fresh Loop must keep the per-tick push enabled (default false)")
	}
	l.SetTickPushDisabled(true)
	if !l.TickPushDisabled() {
		t.Fatal("Loop.SetTickPushDisabled(true) must reach the spawner field")
	}
}
