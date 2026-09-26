package api_test

import (
	"net/http"
	"testing"
)

// SCHED-GAP-1620: POST /projects/{name}/spawn used to spawn a REAL tick for a
// DISABLED project (dogfooded 2026-09-25: enabled=false → 202 + a running
// tick row). bumpProject in the same file refuses with 409 — spawn must
// refuse the same way: a lane an operator paused must not fire because a
// script re-ran a manual spawn.
func TestAPI_SpawnProject_Disabled(t *testing.T) {
	a := newAPITestServer(t)
	mustCreateAPITestProject(t, a.db, "paused")
	if _, err := a.db.Exec(`UPDATE projects SET enabled = 0 WHERE name = 'paused'`); err != nil {
		t.Fatalf("disable: %v", err)
	}

	status, body := a.do(t, "POST", "/api/v1/projects/paused/spawn", nil)
	if status != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (disabled project): %v", status, body)
	}
	if got, _ := body["error"].(string); got != "project is disabled — resume it before spawning" {
		t.Errorf("error = %q, want %q", got, "project is disabled — resume it before spawning")
	}

	// No tick row may exist — the 202 the bug produced also created one.
	var n int
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM ticks WHERE project_name = 'paused'`).Scan(&n); err != nil {
		t.Fatalf("count ticks: %v", err)
	}
	if n != 0 {
		t.Errorf("ticks table has %d row(s) for a disabled project, want 0", n)
	}
}

// The other half of the guard: an ENABLED project must still spawn — the
// check refuses only disabled projects, never over-refuses the live path.
func TestAPI_SpawnProject_EnabledAccepted(t *testing.T) {
	a := newAPITestServer(t)
	mustCreateAPITestProject(t, a.db, "live")

	status, body := a.do(t, "POST", "/api/v1/projects/live/spawn", nil)
	if status != http.StatusAccepted {
		t.Fatalf("status = %d, want 202: %v", status, body)
	}
	if body["status"] != "spawned" {
		t.Errorf("status field = %v, want spawned", body["status"])
	}
	// The tick row IS created for the enabled project (symmetric proof that
	// the disabled half's "no row" assertion means something).
	var n int
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM ticks WHERE project_name = 'live'`).Scan(&n); err != nil {
		t.Fatalf("count ticks: %v", err)
	}
	if n != 1 {
		t.Errorf("ticks table has %d row(s) for an enabled spawn, want 1", n)
	}
}
