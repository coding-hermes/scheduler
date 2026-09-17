package api

import (
	"context"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/coding-hermes/scheduler/internal/database"
)

// TestSCHEDGAP137b_PauseResyncsFleetToml proves pauseProject and
// resumeProject regenerate fleet.toml via the ops policy script
// (SCHED-GAP-137b). The exec runner is injected so the test never shells out
// and never touches the real ~/.hermes/fleet.toml; the recorded argv is
// asserted against the documented production invocation.
func TestSCHEDGAP137b_PauseResyncsFleetToml(t *testing.T) {
	// regenFleetTomlExec is package-level: guard against parallel tests in
	// this package mutating it mid-run.
	t.Setenv("SCHEDGAP137B_PARALLEL_GUARD", "1")

	prev := regenFleetTomlExec
	t.Cleanup(func() { regenFleetTomlExec = prev })

	calls := make(chan []string, 4) // buffered: a missed assertion fails the test, not the handler
	regenFleetTomlExec = func() error {
		calls <- []string{
			"python3", "/home/kara/.hermes/scripts/fleet-cooldown-policy.py", "--apply",
		}
		return nil
	}

	db, err := database.InitDB(":memory:")
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	defer db.Close()
	mustCreateHelperTestProject(t, db, "gap137b")
	s := NewServer(db, nil)

	wantArgv := []string{"python3", "/home/kara/.hermes/scripts/fleet-cooldown-policy.py", "--apply"}

	t.Run("pause regenerates fleet.toml", func(t *testing.T) {
		req := httptest.NewRequest("POST", "/api/v1/projects/gap137b/pause", nil)
		rec := httptest.NewRecorder()
		s.pauseProject(rec, req, "gap137b")
		if rec.Code != 200 {
			t.Fatalf("pause status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), `"paused"`) {
			t.Errorf("pause body = %s, want status paused", rec.Body.String())
		}

		// Exactly one regen call, with the documented argv.
		select {
		case got := <-calls:
			if !slices.Equal(got, wantArgv) {
				t.Errorf("pause regen argv = %v, want %v", got, wantArgv)
			}
		default:
			t.Fatal("pause: regenFleetTomlViaPolicy was NOT called — fleet.toml left out of parity with DB (SCHED-GAP-137b regression)")
		}
		select {
		case extra := <-calls:
			t.Errorf("pause: unexpected extra regen call: %v", extra)
		default:
		}

		// The DB row must actually be disabled (the pause precondition).
		p, err := database.GetProject(context.Background(), db, "gap137b")
		if err != nil {
			t.Fatalf("GetProject after pause: %v", err)
		}
		if p.Enabled {
			t.Error("project still enabled after pause")
		}
	})

	t.Run("resume regenerates fleet.toml", func(t *testing.T) {
		req := httptest.NewRequest("POST", "/api/v1/projects/gap137b/resume", nil)
		rec := httptest.NewRecorder()
		s.resumeProject(rec, req, "gap137b")
		if rec.Code != 200 {
			t.Fatalf("resume status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), `"resumed"`) {
			t.Errorf("resume body = %s, want status resumed", rec.Body.String())
		}

		select {
		case got := <-calls:
			if !slices.Equal(got, wantArgv) {
				t.Errorf("resume regen argv = %v, want %v", got, wantArgv)
			}
		default:
			t.Fatal("resume: regenFleetTomlViaPolicy was NOT called — fleet.toml left out of parity with DB (SCHED-GAP-137b regression)")
		}
		select {
		case extra := <-calls:
			t.Errorf("resume: unexpected extra regen call: %v", extra)
		default:
		}

		p, err := database.GetProject(context.Background(), db, "gap137b")
		if err != nil {
			t.Fatalf("GetProject after resume: %v", err)
		}
		if !p.Enabled {
			t.Error("project still disabled after resume")
		}
	})
}
