package scheduler

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// Spawn-level regression tests for S-GAP-001 Fix B: the consecutive-failure
// counter must increment on spawn failure and reset on the first successful
// spawn, and a failed spawn must advance the project's attempt clock
// (completed_at) so cooldown/backoff/starvation math has a real timestamp.

// failingGatewayHandler always rejects the spawn (the 2026-08-04 outage
// shape: gateway reachable but refusing requests).
func failingGatewayHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]string{
			"type":    "auth_error",
			"message": "Invalid gateway API key",
		},
	})
}

func consecutiveFailuresOf(t *testing.T, db *sql.DB, name string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT consecutive_failures FROM projects WHERE name = ?`, name).Scan(&n); err != nil {
		t.Fatalf("query consecutive_failures for %s: %v", name, err)
	}
	return n
}

// TestSpawn_GatewayFailureIncrementsConsecutiveFailures — every dropped
// spawn (gateway fail + noExecFallback) bumps the counter by exactly 1.
func TestSpawn_GatewayFailureIncrementsConsecutiveFailures(t *testing.T) {
	db := newTestDB(t)
	mustCreateProjectINFRA012(t, db, "sgap001-inc")

	srv := httptest.NewServer(http.HandlerFunc(failingGatewayHandler))
	defer srv.Close()

	spawner := NewSpawner(db, 4)
	spawner.SetGatewayClient(NewGatewayClient(srv.URL, "sk-daemon-shared", 5*time.Second))
	spawner.SetNoExecFallback(true)

	project := PackedProject{Name: "sgap001-inc", Workdir: t.TempDir()}
	for i := 1; i <= 3; i++ {
		tickID := fmt.Sprintf("sgap001-inc-2026-08-04-19-00-0%d", i)
		if _, err := spawner.Spawn(project, tickID); err == nil {
			t.Fatalf("Spawn #%d returned nil error on failing gateway", i)
		}
		if got := consecutiveFailuresOf(t, db, "sgap001-inc"); got != i {
			t.Errorf("after %d failed spawn(s): consecutive_failures = %d, want %d", i, got, i)
		}
	}
}

// TestSpawn_GatewaySuccessResetsConsecutiveFailures — a successful spawn
// resets the backoff counter to 0 (and still bumps last_tick_started).
func TestSpawn_GatewaySuccessResetsConsecutiveFailures(t *testing.T) {
	db := newTestDB(t)
	mustCreateProjectINFRA012(t, db, "sgap001-reset")
	if _, err := db.Exec(`UPDATE projects SET consecutive_failures = 5 WHERE name = ?`, "sgap001-reset"); err != nil {
		t.Fatalf("preset consecutive_failures: %v", err)
	}

	var capturedAuth string
	srv := httptest.NewServer(gatewaySpawnOKHandler(&capturedAuth))
	defer srv.Close()

	spawner := NewSpawner(db, 4)
	spawner.SetGatewayClient(NewGatewayClient(srv.URL, "sk-daemon-shared", 5*time.Second))
	spawner.SetNoExecFallback(true)

	project := PackedProject{Name: "sgap001-reset", Workdir: t.TempDir()}
	tick, err := spawner.Spawn(project, "sgap001-reset-2026-08-04-19-00-00")
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if tick == nil {
		t.Fatal("Spawn returned nil tick on gateway success")
		return
	}

	if got := consecutiveFailuresOf(t, db, "sgap001-reset"); got != 0 {
		t.Errorf("consecutive_failures = %d after successful spawn, want 0 (backoff reset)", got)
	}
	var started sql.NullString
	if err := db.QueryRow(`SELECT last_tick_started FROM projects WHERE name = ?`, "sgap001-reset").Scan(&started); err != nil {
		t.Fatalf("query last_tick_started: %v", err)
	}
	if !started.Valid || started.String == "" {
		t.Error("last_tick_started not set after successful spawn — cooldown tracking broken")
	}
}

// TestSlotPool_SpawnFailureAdvancesAttemptClock pins the storm-clock fix:
// before S-GAP-001 the spawn-failure path completed the tick with a zero
// Finished time, writing completed_at="0001-01-01T00:00:00Z" — the last
// attempt clock never advanced, cooldown never re-armed, and a broken
// gateway produced thousands of failed ticks per day. The tick's and the
// project's attempt timestamps must now be real.
func TestSlotPool_SpawnFailureAdvancesAttemptClock(t *testing.T) {
	db := newTestDB(t)
	mustCreateProjectINFRA012(t, db, "sgap001-clock")

	srv := httptest.NewServer(http.HandlerFunc(failingGatewayHandler))
	defer srv.Close()

	spawner := NewSpawner(db, 4)
	spawner.SetGatewayClient(NewGatewayClient(srv.URL, "sk-daemon-shared", 5*time.Second))
	spawner.SetNoExecFallback(true)

	lc := NewLifecycleTracker(db)
	pool := NewSlotPool(1, 10*time.Second, spawner, lc)

	now := time.Now()
	tickID := pool.Spawn(PackedProject{Name: "sgap001-clock", Workdir: t.TempDir()}, now, true, db)

	// The pool spawns in a goroutine — poll for the terminal status. The
	// tick row does not exist until the goroutine enqueues it, so use a
	// non-fatal lookup (tickStatusOf would Fatalf on the first poll).
	deadline := time.Now().Add(5 * time.Second)
	for {
		var status string
		err := db.QueryRow(`SELECT status FROM ticks WHERE id = ?`, tickID).Scan(&status)
		if err == nil && status == string(TickFailed) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("tick %s never reached failed status within 5s (last status %q, err %v)", tickID, status, err)
		}
		time.Sleep(25 * time.Millisecond)
	}

	y2k := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)

	var tickCompleted sql.NullString
	if err := db.QueryRow(`SELECT completed_at FROM ticks WHERE id = ?`, tickID).Scan(&tickCompleted); err != nil {
		t.Fatalf("query tick completed_at: %v", err)
	}
	if !tickCompleted.Valid {
		t.Fatal("tick completed_at is NULL after failed spawn — attempt clock did not advance")
	}
	tt, err := time.Parse(time.RFC3339, tickCompleted.String)
	if err != nil {
		t.Fatalf("parse tick completed_at %q: %v", tickCompleted.String, err)
	}
	if tt.Before(y2k) {
		t.Errorf("tick completed_at = %v — zero-time regression (storm clock frozen)", tt)
	}

	var projCompleted sql.NullString
	if err := db.QueryRow(`SELECT last_tick_completed FROM projects WHERE name = ?`, "sgap001-clock").Scan(&projCompleted); err != nil {
		t.Fatalf("query project last_tick_completed: %v", err)
	}
	if !projCompleted.Valid {
		t.Fatal("project last_tick_completed is NULL after failed spawn")
	}
	pt, err := time.Parse(time.RFC3339, projCompleted.String)
	if err != nil {
		t.Fatalf("parse project last_tick_completed %q: %v", projCompleted.String, err)
	}
	if pt.Before(y2k) {
		t.Errorf("project last_tick_completed = %v — zero-time regression (storm clock frozen)", pt)
	}

	if got := consecutiveFailuresOf(t, db, "sgap001-clock"); got != 1 {
		t.Errorf("consecutive_failures = %d after one failed spawn, want 1", got)
	}
}
