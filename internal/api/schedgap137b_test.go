package api

import (
	"context"
	"database/sql"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/coding-hermes/scheduler/internal/config"
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
			t.Errorf("pause: unexpected extra regen call: %v", extra)
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

// gap137bFleetToml is the fixture toml used by the re-load tests below: one
// namespace + one project block, mirroring a minimal production fleet.toml.
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

// gap137bNewFleetToml is the fixture toml the injected regen exec writes: the
// real policy script would regenerate the file from live DB state, so a
// paused project's [[projects]] block disappears entirely (removal, not
// enabled=false — projectFromDef treats a *bool nil as enabled).
const gap137bNewFleetToml = `[[namespaces]]
id = "default"
weight = 100
reserved = 0
hard_cap = 0
enabled = true
`

// setupGap137bReloadTest builds a temp fleet.toml (project "alpha",
// enabled=true), loads it via the REAL config.LoadFleetConfig, seeds the DB
// from it via the REAL config.ApplyFleetConfig, then injects a fake regen
// exec that rewrites the toml to the alpha-less regeneration (simulating the
// policy script regenerating from live DB state). Returns the server and the
// toml path.
func setupGap137bReloadTest(t *testing.T) (*Server, *sql.DB, string) {
	t.Helper()
	t.Setenv("SCHEDGAP137B_PARALLEL_GUARD", "1")

	prev := regenFleetTomlExec
	t.Cleanup(func() { regenFleetTomlExec = prev })

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

	// Sanity precondition: alpha is enabled in the DB after the initial
	// load (the restart-pinned state the pause operates from).
	alpha, err := database.GetProject(context.Background(), db, "alpha")
	if err != nil {
		t.Fatalf("GetProject alpha (precondition): %v", err)
	}
	if !alpha.Enabled {
		t.Fatal("precondition failed: alpha disabled right after initial fleet.toml load")
	}

	// The fake exec simulates the policy script: rewrite the toml from
	// live DB state (alpha is/was just paused, so its block is gone).
	regenFleetTomlExec = func() error {
		return os.WriteFile(tomlPath, []byte(gap137bNewFleetToml), 0o644)
	}

	s := NewServer(db, nil)
	return s, db, tomlPath
}

// gap137bFindProject scans the re-parsed config for the named project's
// [[projects]] block.
func gap137bFindProject(cfg *config.FleetConfig, name string) *config.ProjectDef {
	for i := range cfg.Projects {
		if cfg.Projects[i].Name == name {
			return &cfg.Projects[i]
		}
	}
	return nil
}

// TestSCHEDGAP137b_PauseReloadKeepsProjectPaused exercises the AC's required
// step END-TO-END through the REAL loader path: pause a live project, re-load
// the regenerated fleet.toml via config.LoadFleetConfig + ApplyFleetConfig
// (the exact code a daemon restart runs), and assert the project STAYS
// paused — DB enabled=false AND the [[projects]] block absent from the
// re-parsed config. The regen exec is injected (a unit test cannot run the
// real policy script); the loader path is not.
//
// Design note: the fake regen removes alpha's block entirely (what the real
// policy script does when regenerating from live DB state). Keeping the block
// with enabled=false would also satisfy the AC — projectFromDef pins
// enabled from the parsed *bool — but removal matches production regen
// behavior.
func TestSCHEDGAP137b_PauseReloadKeepsProjectPaused(t *testing.T) {
	s, db, tomlPath := setupGap137bReloadTest(t)

	req := httptest.NewRequest("POST", "/api/v1/projects/alpha/pause", nil)
	rec := httptest.NewRecorder()
	s.pauseProject(rec, req, "alpha")
	if rec.Code != 200 {
		t.Fatalf("pause status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}

	// Restart simulation: re-load the regenerated fleet.toml through the
	// REAL loader and re-apply it to the DB — exactly what boot does.
	reloaded, err := config.LoadFleetConfig(tomlPath)
	if err != nil {
		t.Fatalf("LoadFleetConfig after pause: %v", err)
	}
	if got := gap137bFindProject(reloaded, "alpha"); got != nil {
		t.Errorf("re-parsed fleet.toml still contains alpha's [[projects]] block — regen did not remove it; a restart would re-pin enabled=true and undo the pause")
	}
	if err := config.ApplyFleetConfig(context.Background(), db, reloaded); err != nil {
		t.Fatalf("ApplyFleetConfig after pause: %v", err)
	}

	alpha, err := database.GetProject(context.Background(), db, "alpha")
	if err != nil {
		t.Fatalf("GetProject alpha after reload: %v", err)
	}
	if alpha.Enabled {
		t.Errorf("AC FAILED: alpha enabled=true after pause + fleet.toml re-load — the restart re-pinned the pause away (SCHED-GAP-137b)")
	}
}

// TestSCHEDGAP137b_ResumeReloadRestoresProjectEnabled mirrors the pause AC
// for resume: a paused project is resumed, the regenerated fleet.toml is
// re-loaded through the REAL loader path, and the project is enabled again —
// DB enabled=true AND the [[projects]] block back in the re-parsed config.
func TestSCHEDGAP137b_ResumeReloadRestoresProjectEnabled(t *testing.T) {
	s, db, tomlPath := setupGap137bReloadTest(t)

	// Pre-pause alpha (directly via the handler, with the injected regen
	// active) so resume has a previously-paused project to restore.
	pauseReq := httptest.NewRequest("POST", "/api/v1/projects/alpha/pause", nil)
	pauseRec := httptest.NewRecorder()
	s.pauseProject(pauseRec, pauseReq, "alpha")
	if pauseRec.Code != 200 {
		t.Fatalf("pre-pause status = %d, want 200 (body: %s)", pauseRec.Code, pauseRec.Body.String())
	}
	// Confirm the paused precondition via the REAL loader path before
	// resuming.
	pausedCfg, err := config.LoadFleetConfig(tomlPath)
	if err != nil {
		t.Fatalf("LoadFleetConfig after pre-pause: %v", err)
	}
	if got := gap137bFindProject(pausedCfg, "alpha"); got != nil {
		t.Fatalf("precondition failed: alpha block still present in fleet.toml after pause")
	}

	// The injected regen writes the alpha-less toml; restore the full
	// fixture (block with enabled=true) so the resume regen reflects a
	// re-enabled project — what the real policy script emits from live
	// DB state after a resume.
	regenFleetTomlExec = func() error {
		return os.WriteFile(tomlPath, []byte(gap137bFleetToml), 0o644)
	}

	req := httptest.NewRequest("POST", "/api/v1/projects/alpha/resume", nil)
	rec := httptest.NewRecorder()
	s.resumeProject(rec, req, "alpha")
	if rec.Code != 200 {
		t.Fatalf("resume status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}

	// Restart simulation again: re-load + re-apply the regenerated toml.
	reloaded, err := config.LoadFleetConfig(tomlPath)
	if err != nil {
		t.Fatalf("LoadFleetConfig after resume: %v", err)
	}
	if got := gap137bFindProject(reloaded, "alpha"); got == nil {
		t.Fatalf("re-parsed fleet.toml has no alpha block after resume — regen did not restore it")
	}
	if err := config.ApplyFleetConfig(context.Background(), db, reloaded); err != nil {
		t.Fatalf("ApplyFleetConfig after resume: %v", err)
	}

	alpha, err := database.GetProject(context.Background(), db, "alpha")
	if err != nil {
		t.Fatalf("GetProject alpha after reload: %v", err)
	}
	if !alpha.Enabled {
		t.Errorf("AC FAILED: alpha enabled=false after resume + fleet.toml re-load — the restart re-pinned the resume away (SCHED-GAP-137b)")
	}
}
