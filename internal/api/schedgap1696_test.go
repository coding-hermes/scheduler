package api_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/coding-hermes/scheduler/internal/config"
	"github.com/coding-hermes/scheduler/internal/database"
)

// SCHED-GAP-1696 acceptance fixtures. One tasks namespace (coding-hermes), one
// cooldown namespace (qa); a foreman "a1" (owner of a1-qa) and its satellite.
const gap1696FleetToml = `[[namespaces]]
id = "coding-hermes"
weight = 100
admission_mode = "tasks"

[[namespaces]]
id = "qa"
weight = 10
admission_mode = "cooldown"

[[projects]]
name = "a1"
repo_url = "https://example.com/a1"
workdir = "/tmp/a1"
weight = 1
priority = 1
cooldown_s = 900
decay_rate = 1.0
model = "test"
provider = "test"
namespace_id = "coding-hermes"
admission_mode = "tasks"

[[projects]]
name = "a1-qa"
repo_url = "https://example.com/a1-qa"
workdir = "/tmp/a1-qa"
weight = 1
priority = 1
cooldown_s = 21600
decay_rate = 1.0
model = "test"
provider = "test"
namespace_id = "qa"
admission_mode = "cooldown"
`

func seed1696(t *testing.T, a *apiTestServer) {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "fleet.toml")
	if err := os.WriteFile(p, []byte(gap1696FleetToml), 0o644); err != nil {
		t.Fatalf("write fleet.toml: %v", err)
	}
	cfg, err := config.LoadFleetConfig(p)
	if err != nil {
		t.Fatalf("LoadFleetConfig: %v", err)
	}
	if err := config.ApplyFleetConfig(context.Background(), a.db, cfg); err != nil {
		t.Fatalf("ApplyFleetConfig: %v", err)
	}
}

// Criteria 1 + 2: PUT refuses tasks-on-satellite and cooldown-on-foreman, and
// the refused row is left UNCHANGED.
func TestSchedGap1696_PUTRefusesClassContradiction(t *testing.T) {
	a := newAPITestServer(t)
	seed1696(t, a)
	ctx := context.Background()

	// 1. satellite + tasks → 400, row unchanged.
	code, body := a.do(t, "PUT", "/api/v1/projects/a1-qa", map[string]any{"admission_mode": "tasks"})
	if code != 400 {
		t.Fatalf("satellite+tasks: got %d, want 400 (body=%v)", code, body)
	}
	p, err := database.GetProject(ctx, a.db, "a1-qa")
	if err != nil {
		t.Fatalf("GetProject: %v", err)
	}
	if p.AdmissionMode != "cooldown" {
		t.Fatalf("refused PUT mutated the row: admission_mode=%q, want cooldown", p.AdmissionMode)
	}

	// 2. foreman + cooldown → 400.
	code, body = a.do(t, "PUT", "/api/v1/projects/a1", map[string]any{"admission_mode": "cooldown"})
	if code != 400 {
		t.Fatalf("foreman+cooldown: got %d, want 400 (body=%v)", code, body)
	}

	// The accepted direction is a no-op write (already correct) and returns 200.
	code, _ = a.do(t, "PUT", "/api/v1/projects/a1", map[string]any{"admission_mode": "tasks"})
	if code != 200 {
		t.Fatalf("foreman+tasks: got %d, want 200", code)
	}
}

// Criterion 4: the OWNERSHIP EDGE — a lane that carries a parent for org
// nesting but OWNS satellites is a FOREMAN: tasks accepted, cooldown refused.
func TestSchedGap1696_OwnershipEdge(t *testing.T) {
	a := newAPITestServer(t)
	seed1696(t, a)
	ctx := context.Background()

	// Give the foreman a parent for org nesting; it still owns a1-qa.
	parent := "h3"
	if err := database.UpdateProject(ctx, a.db, "a1", database.ProjectUpdates{Parent: &parent}); err != nil {
		t.Fatalf("set parent: %v", err)
	}

	code, _ := a.do(t, "PUT", "/api/v1/projects/a1", map[string]any{"admission_mode": "cooldown"})
	if code != 400 {
		t.Fatalf("parented-but-owns foreman + cooldown: got %d, want 400", code)
	}
	code, _ = a.do(t, "PUT", "/api/v1/projects/a1", map[string]any{"admission_mode": "tasks"})
	if code != 200 {
		t.Fatalf("parented-but-owns foreman + tasks: got %d, want 200", code)
	}

	// The inverse: a parented lane with NO satellites is a satellite → tasks refused.
	shim := "shim-x"
	if err := database.UpdateProject(ctx, a.db, "a1-qa", database.ProjectUpdates{Parent: &shim}); err != nil {
		t.Fatalf("set parent on satellite: %v", err)
	}
	code, _ = a.do(t, "PUT", "/api/v1/projects/a1-qa", map[string]any{"admission_mode": "tasks"})
	if code != 400 {
		t.Fatalf("parented ownerless satellite + tasks: got %d, want 400", code)
	}
}

// Criterion 5: GET /api/v1/status carries admission_law_violations and it reads
// 0 on a compliant fleet.
func TestSchedGap1696_StatusCount(t *testing.T) {
	a := newAPITestServer(t)
	seed1696(t, a)

	code, body := a.do(t, "GET", "/api/v1/status", nil)
	if code != 200 {
		t.Fatalf("status: got %d", code)
	}
	v, ok := body["admission_law_violations"]
	if !ok {
		t.Fatalf("admission_law_violations missing from /api/v1/status (keys=%d)", len(body))
	}
	if n, _ := v.(float64); n != 0 {
		t.Fatalf("compliant fleet: want 0, got %v", v)
	}
}
