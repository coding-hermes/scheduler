package scheduler

import (
	"database/sql"
	"testing"

	"github.com/coding-herms/scheduler/internal/database"
)

func newTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := database.InitDB(":memory:")
	if err != nil {
		t.Fatalf("InitDB(:memory:): %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// TestNewSpawner_Defaults verifies the constructor sets sane defaults.
func TestNewSpawner_Defaults(t *testing.T) {
	db := newTestDB(t)
	s := NewSpawner(db, 5)
	if s == nil {
		t.Fatal("NewSpawner returned nil")
	}
	if s.ActiveCount() != 0 {
		t.Errorf("initial ActiveCount = %d, want 0", s.ActiveCount())
	}
}

// TestSpawner_CanSpawn verifies the concurrency check before any spawn.
func TestSpawner_CanSpawn(t *testing.T) {
	db := newTestDB(t)

	// maxConcurrent=2 → canSpawn should be true with no active.
	s := NewSpawner(db, 2)
	if !s.canSpawn() {
		t.Error("canSpawn() = false with no active, want true")
	}
}

// TestSpawner_ActiveCountConsistent verifies ActiveCount stays in sync with the active map.
func TestSpawner_ActiveCountConsistent(t *testing.T) {
	db := newTestDB(t)
	s := NewSpawner(db, 10)

	// Initially empty.
	if got := s.ActiveCount(); got != 0 {
		t.Errorf("initial ActiveCount = %d, want 0", got)
	}
	if !s.canSpawn() {
		t.Error("canSpawn() = false at start, want true")
	}
}

// TestSpawner_ZeroMaxConcurrent verifies a 0-concurrency spawner can never spawn.
// We don't actually call Spawn (which would invoke hermes); we just inspect the invariant.
func TestSpawner_ZeroMaxConcurrent(t *testing.T) {
	db := newTestDB(t)
	s := NewSpawner(db, 0)

	if s.ActiveCount() != 0 {
		t.Errorf("ActiveCount = %d, want 0", s.ActiveCount())
	}
	// canSpawn should return false because 0 active < 0 is false.
	if s.canSpawn() {
		t.Error("canSpawn() = true with maxConcurrent=0, want false")
	}
}

// TestSpawner_MaxConcurrentOne confirms boundary behavior at maxConcurrent=1.
func TestSpawner_MaxConcurrentOne(t *testing.T) {
	db := newTestDB(t)
	s := NewSpawner(db, 1)

	// No active → canSpawn true.
	if !s.canSpawn() {
		t.Error("canSpawn() = false with maxConcurrent=1 and no active, want true")
	}
}