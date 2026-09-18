package api_test

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/coding-hermes/scheduler/internal/database"
)

// schedulerHere is the path from internal/api to the repo root, used both for
// the two retired-driver lists this file pins together.
const schedulerRepoRoot = "../.."

// retiredDriverNames mirrors ops/check-fleet-invariants.py's RETIRED_DRIVERS
// ordering. The gate tests below drive every name through the API, so a name
// that falls out of the Go list fails here.
var retiredDriverNames = []string{
	"pm-standin-tick.sh",
	"qa-scheduler-tick.sh",
	"sync-scheduler-tick.sh",
	"dogfood-scheduler-tick.sh",
	"dagger-role-tick.sh",
}

// SCHED-GAP-150 (AC3): POST /api/v1/projects refuses a command that drives a
// retired driver, for every name in the list, and creates no row. The control
// sub-test proves the gate is not a blanket "reject every custom command".
func TestCreateProject_RejectsRetiredDriverCommand(t *testing.T) {
	a := newAPITestServer(t)
	ctx := context.Background()

	for _, driver := range retiredDriverNames {
		t.Run(driver, func(t *testing.T) {
			name := "retired-create-" + strings.TrimSuffix(driver, ".sh")
			cmd := "bash /home/kara/.hermes/scripts/" + driver
			status, body := a.do(t, "POST", "/api/v1/projects", map[string]interface{}{
				"name":     name,
				"repo_url": "https://example.com/" + name,
				"workdir":  "/tmp/" + name,
				"command":  cmd,
			})
			if status != http.StatusBadRequest {
				t.Fatalf("POST with retired driver %s: status = %d, want 400 (body %v)",
					driver, status, body)
			}
			msg, _ := body["error"].(string)
			if !strings.Contains(msg, driver) {
				t.Errorf("error message must name the retired driver %q, got %q", driver, msg)
			}
			if !strings.Contains(msg, "retired driver") {
				t.Errorf("error message must say 'retired driver', got %q", msg)
			}
			// No row may have been written.
			if _, err := database.GetProject(ctx, a.db, name); err == nil {
				t.Errorf("project %q was created despite the retired driver %s", name, driver)
			}
		})
	}

	t.Run("control_supported_executor_accepted", func(t *testing.T) {
		status, body := a.do(t, "POST", "/api/v1/projects", map[string]interface{}{
			"name":     "retired-create-control",
			"repo_url": "https://example.com/retired-create-control",
			"workdir":  "/tmp/retired-create-control",
			// Test-only fixture name: a real executor script, NOT a retired driver.
			"command": "bash /home/kara/.hermes/scripts/scheduler-foreman-tick.sh",
		})
		if status != http.StatusCreated {
			t.Fatalf("POST with a supported executor: status = %d, want 201 (body %v)", status, body)
		}
		if _, err := database.GetProject(ctx, a.db, "retired-create-control"); err != nil {
			t.Errorf("control project was not created: %v", err)
		}
	})
}

// SCHED-GAP-150 (AC3): PUT /api/v1/projects/{name} refuses a retired driver in
// the incoming command and leaves the stored row untouched.
func TestUpdateProject_RejectsRetiredDriverCommand(t *testing.T) {
	a := newAPITestServer(t)
	ctx := context.Background()
	mustCreateAPITestProject(t, a.db, "retired-update")

	driver := "dagger-role-tick.sh"
	status, body := a.do(t, "PUT", "/api/v1/projects/retired-update", map[string]interface{}{
		"command": "bash /home/kara/.hermes/scripts/" + driver,
	})
	if status != http.StatusBadRequest {
		t.Fatalf("PUT with retired driver %s: status = %d, want 400 (body %v)", driver, status, body)
	}
	msg, _ := body["error"].(string)
	if !strings.Contains(msg, driver) {
		t.Errorf("error message must name the retired driver %q, got %q", driver, msg)
	}

	// The row keeps its previous command (empty — the helper creates no command).
	got, err := database.GetProject(ctx, a.db, "retired-update")
	if err != nil {
		t.Fatalf("GetProject: %v", err)
	}
	if got.Command != "" {
		t.Errorf("command = %q, want unchanged empty string", got.Command)
	}
}

// SCHED-GAP-150: the re-enable path. A DISABLED row that already carries a
// retired driver must not be switched on by a bare {"enabled": true} update —
// otherwise the retired command reaches an enabled project without any command
// write, exactly the state the fleet gate (check #4) exists to prevent.
func TestUpdateProject_ReEnablingRetiredDriverCommandRejected(t *testing.T) {
	a := newAPITestServer(t)
	ctx := context.Background()

	driver := "sync-scheduler-tick.sh"
	if err := database.CreateProject(ctx, a.db, &database.Project{
		Name:      "retired-reenable",
		RepoURL:   "https://example.com/retired-reenable",
		Workdir:   "/tmp/retired-reenable",
		Weight:    10,
		Priority:  5,
		CooldownS: 900,
		DecayRate: 1.0,
		Model:     "test",
		Provider:  "test",
		Command:   "bash /home/kara/.hermes/scripts/" + driver,
		Enabled:   false,
	}); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}

	status, body := a.do(t, "PUT", "/api/v1/projects/retired-reenable", map[string]interface{}{
		"enabled": true,
	})
	if status != http.StatusBadRequest {
		t.Fatalf("PUT enabled=true on a retired-driver row: status = %d, want 400 (body %v)", status, body)
	}
	msg, _ := body["error"].(string)
	if !strings.Contains(msg, driver) {
		t.Errorf("error message must name the retired driver %q, got %q", driver, msg)
	}
	got, err := database.GetProject(ctx, a.db, "retired-reenable")
	if err != nil {
		t.Fatalf("GetProject: %v", err)
	}
	if got.Enabled {
		t.Errorf("project was enabled despite the retired driver %s", driver)
	}
}

// SCHED-GAP-150 (step 4): the two retired-driver lists are one fact in two
// languages. Parsed from source text on purpose — the Go var is unexported and
// the Python tuple lives in a script, and this is the parity assertion that
// fails when either list is edited alone.
func TestCheckFleetInvariants_RetiredDriversMatchPythonGate(t *testing.T) {
	goSrc, err := os.ReadFile(filepath.Join(schedulerRepoRoot, "internal", "api", "retired_drivers.go"))
	if err != nil {
		t.Fatalf("read retired_drivers.go: %v", err)
	}
	pySrc, err := os.ReadFile(filepath.Join(schedulerRepoRoot, "ops", "check-fleet-invariants.py"))
	if err != nil {
		t.Fatalf("read check-fleet-invariants.py: %v", err)
	}

	goList := quotedNamesIn(t, string(goSrc), `(?s)retiredDriverScripts\s*=\s*\[\]string\{(.*?)\}`)
	pyList := quotedNamesIn(t, string(pySrc), `(?s)RETIRED_DRIVERS\s*=\s*\((.*?)\)`)

	if len(goList) != 5 {
		t.Errorf("internal/api/retired_drivers.go lists %d drivers (%v), want 5", len(goList), goList)
	}
	if len(pyList) != 5 {
		t.Errorf("ops/check-fleet-invariants.py lists %d drivers (%v), want 5", len(pyList), pyList)
	}
	if strings.Join(goList, ",") != strings.Join(pyList, ",") {
		t.Errorf("retired driver lists diverged:\n  go     = %v\n  python = %v", goList, pyList)
	}
}

// quotedNamesIn extracts the double-quoted strings inside the first match of
// pattern in src.
func quotedNamesIn(t *testing.T, src, pattern string) []string {
	t.Helper()
	m := regexp.MustCompile(pattern).FindStringSubmatch(src)
	if m == nil {
		t.Fatalf("pattern %q not found in source", pattern)
	}
	matches := regexp.MustCompile(`"([^"]+)"`).FindAllStringSubmatch(m[1], -1)
	out := make([]string, 0, len(matches))
	for _, q := range matches {
		out = append(out, q[1])
	}
	return out
}
