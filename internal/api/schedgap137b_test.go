package api

import (
	"context"
	"database/sql"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/coding-hermes/scheduler/internal/config"
	"github.com/coding-hermes/scheduler/internal/database"
)

// SCHED-GAP-219 rewrite: the SCHED-GAP-137b regen is RETIRED. Pause/resume no
// longer shell out to the ops policy script to regenerate fleet.toml — the DB
// is the cooldown authority and the seed-only loader (config.ApplyFleetConfig)
// no longer re-pins enabled/cooldown/model/provider for an existing row, so an
// API state change is durable across restarts WITHOUT a toml mirror.
//
// These tests now pin the STRONGER property the regen existed to approximate:
// even a STALE fleet.toml that still says enabled=true cannot undo a pause
// (or a resume) across a restart. That is the exact failure shape the regen
// was a workaround for, now impossible by construction.

// gap137bFleetToml is the fixture toml: one namespace + one project block,
// mirroring a minimal production fleet.toml.
const gap137bFleetToml = `[[namespaces]]
id = "default"
weight = 100
reserved = 0
hard_cap = 0
enabled = true

[[projects]]
name = "alpha"
repo_url = "https://example.com/alpha"
workdir = "/tmp/alpha"
weight = 1
priority = 1
cooldown_s = 21600
decay_rate = 1.0
model = "test"
provider = "test"
namespace_id = "default"
enabled = true
`

// writeGap137bFleetToml writes the fixture fleet.toml into dir and returns
// its path.
func writeGap137bFleetToml(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "fleet.toml")
	if err := os.WriteFile(path, []byte(gap137bFleetToml), 0o644); err != nil {
		t.Fatalf("write fleet.toml: %v", err)
	}
	return path
}

// setupGap137bRestartTest builds a temp fleet.toml (project "alpha",
// enabled=true), loads it via the REAL config.LoadFleetConfig, seeds the DB
// from it via the REAL config.ApplyFleetConfig, and returns the server and
// the toml path. The toml is NEVER rewritten: it stays stale (enabled=true)
// for the whole test — that staleness is the point.
func setupGap137bRestartTest(t *testing.T) (*Server, *sql.DB, string) {
	t.Helper()

	dir := t.TempDir()
	tomlPath := writeGap137bFleetToml(t, dir)

	cfg, err := config.LoadFleetConfig(tomlPath)
	if err != nil {
		t.Fatalf("LoadFleetConfig: %v", err)
	}

	db, err := database.InitDB(":memory:")
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	if err := config.ApplyFleetConfig(context.Background(), db, cfg); err != nil {
		t.Fatalf("ApplyFleetConfig: %v", err)
	}

	alpha, err := database.GetProject(context.Background(), db, "alpha")
	if err != nil {
		t.Fatalf("GetProject alpha (precondition): %v", err)
	}
	if !alpha.Enabled {
		t.Fatal("precondition failed: alpha disabled right after initial fleet.toml load")
	}

	s := NewServer(db, nil)
	return s, db, tomlPath
}

// restartFromToml re-runs the exact boot sequence over the (stale) toml —
// LoadFleetConfig + ApplyFleetConfig — the code path a daemon restart runs.
func restartFromFleetToml(t *testing.T, db *sql.DB, tomlPath string) {
	t.Helper()
	reloaded, err := config.LoadFleetConfig(tomlPath)
	if err != nil {
		t.Fatalf("LoadFleetConfig (restart): %v", err)
	}
	if err := config.ApplyFleetConfig(context.Background(), db, reloaded); err != nil {
		t.Fatalf("ApplyFleetConfig (restart): %v", err)
	}
}

// TestSCHEDGAP219_PauseSurvivesRestartWithStaleToml proves the pause
// durability the 137b regen approximated, now by construction: pause alpha,
// then run the FULL restart path against the UNCHANGED fleet.toml (which
// still says enabled=true). The pause must survive.
func TestSCHEDGAP219_PauseSurvivesRestartWithStaleToml(t *testing.T) {
	s, db, tomlPath := setupGap137bRestartTest(t)

	req := httptest.NewRequest("POST", "/api/v1/projects/alpha/pause", nil)
	rec := httptest.NewRecorder()
	s.pauseProject(rec, req, "alpha")
	if rec.Code != 200 {
		t.Fatalf("pause status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"paused"`) {
		t.Errorf("pause body = %s, want status paused", rec.Body.String())
	}

	// Restart simulation: re-load the STALE toml (alpha still enabled=true
	// in the file — nobody regenerated it) through the REAL loader.
	restartFromFleetToml(t, db, tomlPath)

	alpha, err := database.GetProject(context.Background(), db, "alpha")
	if err != nil {
		t.Fatalf("GetProject alpha after restart: %v", err)
	}
	if alpha.Enabled {
		t.Errorf("SCHED-GAP-219 FAILED: alpha enabled=true after pause + restart against a STALE fleet.toml — the seed loader re-pinned enabled and undid the pause")
	}
	// The cooldown pin import must ALSO not have run for a paused row's
	// sibling state unexpectedly — alpha's toml cooldown is recorded as the
	// pin but never overwrote anything.
	if alpha.CooldownPinS != nil && *alpha.CooldownPinS != 21600 {
		t.Errorf("pin import: cooldown_pin_s = %v, want 21600 (the toml value imports as the pin)", alpha.CooldownPinS)
	}
}

// TestSCHEDGAP219_ResumeSurvivesRestartWithStaleToml mirrors the pause case:
// a resumed project stays enabled across a restart against a stale toml that
// never said enabled=false in the first place.
func TestSCHEDGAP219_ResumeSurvivesRestartWithStaleToml(t *testing.T) {
	s, db, tomlPath := setupGap137bRestartTest(t)

	// Pause alpha via the handler so resume has a previously-paused project.
	pauseReq := httptest.NewRequest("POST", "/api/v1/projects/alpha/pause", nil)
	pauseRec := httptest.NewRecorder()
	s.pauseProject(pauseRec, pauseReq, "alpha")
	if pauseRec.Code != 200 {
		t.Fatalf("pre-pause status = %d, want 200", pauseRec.Code)
	}

	req := httptest.NewRequest("POST", "/api/v1/projects/alpha/resume", nil)
	rec := httptest.NewRecorder()
	s.resumeProject(rec, req, "alpha")
	if rec.Code != 200 {
		t.Fatalf("resume status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"resumed"`) {
		t.Errorf("resume body = %s, want status resumed", rec.Body.String())
	}

	// Restart simulation over the same stale toml.
	restartFromFleetToml(t, db, tomlPath)

	alpha, err := database.GetProject(context.Background(), db, "alpha")
	if err != nil {
		t.Fatalf("GetProject alpha after restart: %v", err)
	}
	if !alpha.Enabled {
		t.Errorf("AC FAILED: alpha enabled=false after resume + restart — the seed loader rewrote enabled from the stale toml (SCHED-GAP-219)")
	}
}
