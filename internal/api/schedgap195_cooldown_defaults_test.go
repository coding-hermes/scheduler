package api_test

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/coding-hermes/scheduler/internal/config"
	"github.com/coding-hermes/scheduler/internal/database"
)

// SCHED-GAP-195: the POST-created and config-seeded cooldown defaults are an
// INTENTIONAL split — API create stamps 900 (15 min; an interactive create is
// meant to be observed soon), while fleet.toml/config seeding resolves 7200
// (2h baseline, defaultProjectCooldown in internal/config/loader.go). Both
// defaults are pinned here through their real entry points so a silent change
// on either side fails the build. The documentation surfaces (README POST
// sample, README fleet.toml durability prose, and the --schema cooldown_s
// description in cmd/schedulerd/show_config.go) quote exactly these numbers —
// update all of them together if this test ever legitimately moves.
func TestSCHEDGAP195_CooldownDefaultSplit(t *testing.T) {
	ctx := context.Background()

	// Side 1 — API create path: a minimal POST body with no cooldown_s
	// must produce CooldownS = 900 (the S06 zero-value defaults in
	// server_projects.go createProject).
	t.Run("api_create_defaults_900", func(t *testing.T) {
		a := newAPITestServer(t)
		status, body := a.do(t, "POST", "/api/v1/projects", map[string]interface{}{
			"name":     "gap195-api-create",
			"repo_url": "https://example.com/gap195-api-create",
			"workdir":  "/tmp/gap195-api-create",
		})
		if status != http.StatusCreated {
			t.Fatalf("POST /api/v1/projects: status = %d, want 201 (body %v)", status, body)
		}
		p, err := database.GetProject(ctx, a.db, "gap195-api-create")
		if err != nil {
			t.Fatalf("GetProject: %v", err)
		}
		if p.CooldownS != 900 {
			t.Errorf("API-created cooldown_s = %d, want 900 (SCHED-GAP-195 pins the interactive-create default; README POST sample + --schema description name this number)", p.CooldownS)
		}
	})

	// Side 2 — config seeding path: a fleet.toml project entry with no
	// cooldown_s must resolve to 7200 through the real producer chain
	// (LoadFleetConfig → ApplyFleetConfig → defaultProjectCooldown).
	t.Run("config_seeded_defaults_7200", func(t *testing.T) {
		db, err := database.InitDB(":memory:")
		if err != nil {
			t.Fatalf("InitDB: %v", err)
		}
		defer db.Close()

		path := filepath.Join(t.TempDir(), "fleet.toml")
		tomlContent := `[[projects]]
name = "gap195-config-seeded"
repo_url = "https://example.com/gap195-config-seeded"
workdir = "/tmp/gap195-config-seeded"
`
		if err := os.WriteFile(path, []byte(tomlContent), 0644); err != nil {
			t.Fatal(err)
		}
		cfg, err := config.LoadFleetConfig(path)
		if err != nil {
			t.Fatalf("LoadFleetConfig: %v", err)
		}
		if err := config.ApplyFleetConfig(ctx, db, cfg); err != nil {
			t.Fatalf("ApplyFleetConfig: %v", err)
		}
		p, err := database.GetProject(ctx, db, "gap195-config-seeded")
		if err != nil {
			t.Fatalf("GetProject: %v", err)
		}
		if p.CooldownS != 7200 {
			t.Errorf("config-seeded cooldown_s = %d, want 7200 (SCHED-GAP-195 pins the fleet.toml baseline; README durability prose + --schema description name this number)", p.CooldownS)
		}
	})
}
