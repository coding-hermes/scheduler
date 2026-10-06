package api_test

// SCHED-GAP-1729 — the create->configure->enable provisioning order.
//
// Measured live 2026-10-06 (MSF-022/mischief-chaos): POST a row, then PUT
// {"admission_mode":"cooldown"} while the row's namespace is still NULL —
// the boundary check 400-refused it, while the identical PUT AFTER the
// namespace PUT was accepted. Between create and the fixed PUT the row sat
// ARMED (enabled=true, cooldown_s=900 default) with an empty prompt, one
// evaluation away from admission.
//
// Two acceptance points:
//
//  1. Admission classification is keyed on the EXPLICIT lane class
//     (internal/scheduler/lane_class.go: name suffix, parent, satellite
//     ownership) — never on the namespace. A lane whose namespace is not
//     yet set classifies identically to the same lane after it is assigned;
//     the one bounded deferral is a cooldown PUT on a foreman-classed lane
//     that carries no namespace yet (the provisioning window between create
//     and the configure PUT).
//  2. A newly created row is born DISARMED — the POST stores enabled=false
//     no matter what the body carried, so the create->configure->enable
//     order can never leave an armed empty-prompt row behind.

import (
	"context"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/coding-hermes/scheduler/internal/database"
)

// TestSchedGap1729_CooldownPUTAcceptedBeforeNamespaceSet pins the measured
// order end to end: create -> PUT admission_mode=cooldown (namespace still
// NULL) must be ACCEPTED, and the row must land in the DB carrying it. The
// control proves the law still bites once the lane is configured into a
// tasks-mode namespace: the same cooldown PUT is refused there (foreman law).
func TestSchedGap1729_CooldownPUTAcceptedBeforeNamespaceSet(t *testing.T) {
	a := newAPITestServer(t)
	seed1696(t, a) // namespaces coding-hermes (tasks) + qa; rows a1/a1-qa
	ctx := context.Background()

	// The provisioning window: a fresh primary lane, created disabled and
	// with NO namespace yet.
	status, body := a.do(t, "POST", "/api/v1/projects", map[string]interface{}{
		"name":     "fresh-foreman",
		"repo_url": "local:/tmp/fresh-foreman",
		"workdir":  "/tmp/fresh-foreman",
	})
	if status != http.StatusCreated {
		t.Fatalf("POST create: status = %d, want 201 (body %v)", status, body)
	}

	// The PUT that used to 400 while the namespace was NULL.
	status, body = a.do(t, "PUT", "/api/v1/projects/fresh-foreman",
		map[string]interface{}{"admission_mode": "cooldown"})
	if status != http.StatusOK {
		t.Fatalf("cooldown PUT on unnamespaced foreman: status = %d, want 200 (body %v)", status, body)
	}
	p, err := database.GetProject(ctx, a.db, "fresh-foreman")
	if err != nil {
		t.Fatalf("GetProject: %v", err)
	}
	if p.AdmissionMode != database.AdmissionModeCooldown {
		t.Fatalf("stored admission_mode = %q, want cooldown", p.AdmissionMode)
	}

	// Control: the SAME cooldown PUT after the lane is configured into the
	// tasks-mode namespace is refused — a foreman there must carry tasks.
	ns := "coding-hermes"
	if err := database.UpdateProject(ctx, a.db, "fresh-foreman",
		database.ProjectUpdates{NamespaceID: &ns}); err != nil {
		t.Fatalf("assign namespace: %v", err)
	}
	status, body = a.do(t, "PUT", "/api/v1/projects/fresh-foreman",
		map[string]interface{}{"admission_mode": "cooldown"})
	if status != http.StatusBadRequest {
		t.Fatalf("cooldown PUT after tasks-namespace configured: status = %d, want 400 (body %v)", status, body)
	}
	// And the forward direction is accepted: the lane lands on tasks.
	status, body = a.do(t, "PUT", "/api/v1/projects/fresh-foreman",
		map[string]interface{}{"admission_mode": "tasks"})
	if status != http.StatusOK {
		t.Fatalf("tasks PUT after configure: status = %d, want 200 (body %v)", status, body)
	}
}

// TestSchedGap1729_ClassKeyedOnLaneNotNamespace pins the class rule as
// namespace-independent in the direction that must NOT regress: a solo
// primary with no namespace is a foreman (tasks accepted) exactly as the
// same lane after it is assigned. A NULL namespace must never flip a class.
func TestSchedGap1729_ClassKeyedOnLaneNotNamespace(t *testing.T) {
	a := newAPITestServer(t)
	seed1696(t, a)

	status, body := a.do(t, "POST", "/api/v1/projects", map[string]interface{}{
		"name":     "unassigned-foreman",
		"repo_url": "local:/tmp/unassigned-foreman",
		"workdir":  "/tmp/unassigned-foreman",
	})
	if status != http.StatusCreated {
		t.Fatalf("POST create: status = %d, want 201 (body %v)", status, body)
	}

	// Solo primary, namespace still NULL: the class rule says foreman.
	status, body = a.do(t, "PUT", "/api/v1/projects/unassigned-foreman",
		map[string]interface{}{"admission_mode": "tasks"})
	if status != http.StatusOK {
		t.Fatalf("tasks PUT on unnamespaced foreman-class lane: status = %d, want 200 (body %v)", status, body)
	}
}

// TestSchedGap1729_SatelliteTasksRefusedWithoutNamespace is the satellite
// half: the suffix rule holds with no namespace in sight — a tasks PUT on an
// unnamespaced -qa lane is refused, so the deferral window cannot be ridden
// past the law's satellite side.
func TestSchedGap1729_SatelliteTasksRefusedWithoutNamespace(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HERMES_HOME", home)
	a := newAPITestServer(t)

	// A -qa lane's canonical workdir leaf is the name minus the suffix
	// (stand-in/{pm,qa}/<primary>), per the SCHED-GAP-138 gate.
	status, body := a.do(t, "POST", "/api/v1/projects", gap138LaneBody("fresh-qa",
		filepath.Join(home, "stand-in", "qa", "fresh"), nil))
	if status != http.StatusCreated {
		t.Fatalf("POST create satellite: status = %d, want 201 (body %v)", status, body)
	}

	status, body = a.do(t, "PUT", "/api/v1/projects/fresh-qa",
		map[string]interface{}{"admission_mode": "tasks"})
	if status != http.StatusBadRequest {
		t.Fatalf("tasks PUT on unnamespaced satellite: status = %d, want 400 (body %v)", status, body)
	}
	msg, _ := body["error"].(string)
	if msg == "" || !strings.Contains(msg, "admission law") {
		t.Errorf("refusal must name the admission law, got %q", msg)
	}
}

// TestSchedGap1729_CombinedPUTNoDeferral pins the effective-row rule: a PUT
// that assigns the tasks-mode namespace AND sets cooldown in ONE body is
// already configured the moment it lands, so the deferral window does not
// apply — the law refuses it exactly as it would after a separate
// namespace PUT.
func TestSchedGap1729_CombinedPUTNoDeferral(t *testing.T) {
	a := newAPITestServer(t)
	seed1696(t, a)

	status, body := a.do(t, "POST", "/api/v1/projects", map[string]interface{}{
		"name":     "combo-foreman",
		"repo_url": "local:/tmp/combo-foreman",
		"workdir":  "/tmp/combo-foreman",
	})
	if status != http.StatusCreated {
		t.Fatalf("POST create: status = %d, want 201 (body %v)", status, body)
	}

	status, body = a.do(t, "PUT", "/api/v1/projects/combo-foreman", map[string]interface{}{
		"namespace_id":   "coding-hermes",
		"admission_mode": "cooldown",
	})
	if status != http.StatusBadRequest {
		t.Fatalf("combined namespace+cooldown PUT: status = %d, want 400 (body %v)", status, body)
	}
}

// TestSchedGap1729_CreatedRowBornDisarmed pins acceptance point 2: the row a
// POST leaves behind is DISABLED no matter what the body carried — the
// minimal body, an explicit enabled:true on a primary, and the armed
// satellite shape the reconciler writes are all born parked. The response
// mirrors the stored row.
func TestSchedGap1729_CreatedRowBornDisarmed(t *testing.T) {
	a := newAPITestServer(t)
	ctx := context.Background()

	t.Run("minimal_body_disabled", func(t *testing.T) {
		status, body := a.do(t, "POST", "/api/v1/projects", map[string]interface{}{
			"name":     "born1",
			"repo_url": "local:/tmp/born1",
			"workdir":  "/tmp/born1",
		})
		if status != http.StatusCreated {
			t.Fatalf("POST: status = %d, want 201 (body %v)", status, body)
		}
		if enabled, _ := body["enabled"].(bool); enabled {
			t.Errorf("response enabled = true, want false")
		}
		p, err := database.GetProject(ctx, a.db, "born1")
		if err != nil {
			t.Fatalf("GetProject: %v", err)
		}
		if p.Enabled {
			t.Fatal("stored row is ENABLED — a fresh row must be born disarmed, one eval from admission otherwise")
		}
	})

	t.Run("explicit_enabled_true_clamped", func(t *testing.T) {
		status, body := a.do(t, "POST", "/api/v1/projects", map[string]interface{}{
			"name":     "born2",
			"repo_url": "local:/tmp/born2",
			"workdir":  "/tmp/born2",
			"enabled":  true,
		})
		if status != http.StatusCreated {
			t.Fatalf("POST enabled:true: status = %d, want 201 (body %v)", status, body)
		}
		if enabled, _ := body["enabled"].(bool); enabled {
			t.Errorf("response enabled = true, want the clamped false")
		}
		p, err := database.GetProject(ctx, a.db, "born2")
		if err != nil {
			t.Fatalf("GetProject: %v", err)
		}
		if p.Enabled {
			t.Fatal("explicit enabled:true survived the create — the row must be born disarmed")
		}
	})

	t.Run("armed_satellite_body_still_born_parked", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HERMES_HOME", home)
		status, body := a.do(t, "POST", "/api/v1/projects", gap138LaneBody("born3-sync",
			filepath.Join(home, "sync-workdirs", "born3-sync"), map[string]interface{}{
				"enabled":            true,
				"cooldown_floor_s":   21600,
				"cooldown_ceiling_s": 172800,
			}))
		if status != http.StatusCreated {
			t.Fatalf("POST armed satellite: status = %d, want 201 (body %v)", status, body)
		}
		p, err := database.GetProject(ctx, a.db, "born3-sync")
		if err != nil {
			t.Fatalf("GetProject: %v", err)
		}
		if p.Enabled {
			t.Fatal("armed satellite body was stored enabled — must be born disarmed")
		}
		// The arming fields ride along: the row is provisioned, only parked.
		if p.CooldownFloorS != 21600 {
			t.Errorf("cooldown_floor_s = %d, want 21600 (provisioning fields must survive the clamp)", p.CooldownFloorS)
		}
	})
}

// TestSchedGap1729_CreateRefusalShapeUnchanged pins the SCHED-GAP-138 create
// refusal the born-disarmed clamp must not swallow: an ENABLED UNARMED
// satellite body is still refused with the family-pin remedy (and no row).
// The pin asserts the remedy text rather than a bare "unarmed" fragment,
// which any refusal would satisfy — a vacuous match measured once already.
func TestSchedGap1729_CreateRefusalShapeUnchanged(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HERMES_HOME", home)
	a := newAPITestServer(t)

	status, body := a.do(t, "POST", "/api/v1/projects", gap138LaneBody("unarmed-sync",
		filepath.Join(home, "sync-workdirs", "unarmed-sync"), map[string]interface{}{
			"enabled": true,
		}))
	if status != http.StatusBadRequest {
		t.Fatalf("POST enabled unarmed satellite: status = %d, want 400 (body %v)", status, body)
	}
	msg, _ := body["error"].(string)
	if !strings.Contains(msg, "set cooldown_floor_s=21600") {
		t.Errorf("error must name the -sync family pin remedy, got %q", msg)
	}
	if _, err := database.GetProject(context.Background(), a.db, "unarmed-sync"); err == nil {
		t.Error("refused create left a row behind")
	}
}
