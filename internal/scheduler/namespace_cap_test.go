package scheduler_test

import (
	"context"
	"testing"
	"time"

	"github.com/coding-hermes/scheduler/internal/database"
	"github.com/coding-hermes/scheduler/internal/scheduler"
)

// TestPick_RespectsNamespaceMaxConcurrent verifies the per-namespace
// concurrency cap (Bane 2026-08-27): a namespace with max_concurrent=1 may
// have at most ONE tick running at once — an already-running project in the
// namespace blocks every other project of that namespace, while projects in
// other namespaces (and uncapped namespaces) pack normally.
func TestPick_RespectsNamespaceMaxConcurrent(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	// Namespace "sync" with max_concurrent=1.
	if err := database.CreateNamespace(ctx, db, &database.Namespace{
		ID:            "sync",
		Weight:        10,
		Reserved:      1,
		HardCap:       100,
		MaxConcurrent: 1,
		Enabled:       true,
	}); err != nil {
		t.Fatalf("CreateNamespace sync: %v", err)
	}
	// Namespace "foreman" with NO cap (0 = unlimited).
	if err := database.CreateNamespace(ctx, db, &database.Namespace{
		ID:       "foreman",
		Weight:   10,
		Reserved: 1,
		HardCap:  100,
		Enabled:  true,
	}); err != nil {
		t.Fatalf("CreateNamespace foreman: %v", err)
	}

	syncNs := "sync"
	foremanNs := "foreman"
	// Three sync-namespace projects (all eligible, cooldown 0).
	for _, n := range []string{"sync-a", "sync-b", "sync-c"} {
		p := makeProject(n, 1, 5, 0, 1.0)
		p.NamespaceID = &syncNs
		if err := database.CreateProject(ctx, db, p); err != nil {
			t.Fatalf("CreateProject %s: %v", n, err)
		}
	}
	// Two uncapped-namespace projects.
	for _, n := range []string{"foreman-a", "foreman-b"} {
		p := makeProject(n, 1, 5, 0, 1.0)
		p.NamespaceID = &foremanNs
		if err := database.CreateProject(ctx, db, p); err != nil {
			t.Fatalf("CreateProject %s: %v", n, err)
		}
	}

	calc := scheduler.NewUrgencyCalculator(time.Minute, time.Hour, 10)
	// Generous global budget + concurrency: ONLY the namespace cap can bind.
	p := scheduler.NewPacker(db, calc, 100, 10, nil)

	// Case 1: nothing running → exactly one sync project may pack (the cap
	// limits same-cycle packing too), and both foreman projects pack.
	got, err := p.Pick(time.Now(), map[string]bool{})
	if err != nil {
		t.Fatalf("Pick: %v", err)
	}
	var syncPacked, foremanPacked int
	for _, gp := range got {
		switch gp.Name {
		case "sync-a", "sync-b", "sync-c":
			syncPacked++
		case "foreman-a", "foreman-b":
			foremanPacked++
		}
	}
	if syncPacked != 1 {
		t.Errorf("sync-namespace projects packed = %d, want exactly 1 (max_concurrent=1)", syncPacked)
	}
	if foremanPacked != 2 {
		t.Errorf("foreman-namespace projects packed = %d, want 2 (uncapped)", foremanPacked)
	}

	// Case 2: one sync project already running → NO other sync project may
	// pack; foreman projects still pack.
	got2, err := p.Pick(time.Now(), map[string]bool{"sync-a": true})
	if err != nil {
		t.Fatalf("Pick (running): %v", err)
	}
	syncPacked, foremanPacked = 0, 0
	for _, gp := range got2 {
		switch gp.Name {
		case "sync-a", "sync-b", "sync-c":
			syncPacked++
		case "foreman-a", "foreman-b":
			foremanPacked++
		}
	}
	if syncPacked != 0 {
		t.Errorf("sync-namespace projects packed with one already running = %d, want 0 (cap=1)", syncPacked)
	}
	if foremanPacked != 2 {
		t.Errorf("foreman-namespace projects packed with sync running = %d, want 2 (uncapped)", foremanPacked)
	}
}

// TestPick_NamespaceCapHigherThanOne verifies a namespace with max_concurrent=2
// packs at most two of its projects in one cycle (and respects the cap when
// one is already running: packs at most one more).
func TestPick_NamespaceCapHigherThanOne(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	if err := database.CreateNamespace(ctx, db, &database.Namespace{
		ID:            "sync2",
		Weight:        10,
		Reserved:      1,
		HardCap:       100,
		MaxConcurrent: 2,
		Enabled:       true,
	}); err != nil {
		t.Fatalf("CreateNamespace: %v", err)
	}
	ns := "sync2"
	for _, n := range []string{"s1", "s2", "s3", "s4"} {
		p := makeProject(n, 1, 5, 0, 1.0)
		p.NamespaceID = &ns
		if err := database.CreateProject(ctx, db, p); err != nil {
			t.Fatalf("CreateProject %s: %v", n, err)
		}
	}

	calc := scheduler.NewUrgencyCalculator(time.Minute, time.Hour, 10)
	p := scheduler.NewPacker(db, calc, 100, 10, nil)

	got, err := p.Pick(time.Now(), map[string]bool{})
	if err != nil {
		t.Fatalf("Pick: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("packed = %d, want 2 (max_concurrent=2, nothing running)", len(got))
	}

	// One already running → at most one more.
	got2, err := p.Pick(time.Now(), map[string]bool{"s1": true})
	if err != nil {
		t.Fatalf("Pick (running): %v", err)
	}
	if len(got2) != 1 {
		t.Errorf("packed with one running = %d, want 1 (cap 2, 1 in use)", len(got2))
	}
}
