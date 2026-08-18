package api_test

// Wire-format conformance tests for DOGFOOD-001/003 (dogfood audit
// 2026-08-04): POST /api/v1/projects must accept the documented S06
// snake_case body AND the legacy PascalCase spelling used by live fleet
// automation; responses must serialize snake_case per specs/S06-rest-api.md.

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/coding-hermes/scheduler/internal/database"
)

// TestConformance_CreateProject_SnakeCaseDefaults verifies the documented
// S06 minimal body {name, repo_url, workdir} creates a project with defaults
// filled (weight 10, priority 5, cooldown_s 900, decay_rate 1.0) and the
// project is NOT auto-enabled.
func TestConformance_CreateProject_SnakeCaseDefaults(t *testing.T) {
	a := newAPITestServer(t)
	status, resp := a.do(t, "POST", "/api/v1/projects", map[string]interface{}{
		"name":     "snakecase",
		"repo_url": "https://example.com/snakecase",
		"workdir":  "/tmp/snakecase",
	})
	if status != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %v", status, resp)
	}

	got, err := database.GetProject(context.Background(), a.db, "snakecase")
	if err != nil {
		t.Fatalf("GetProject: %v", err)
	}
	if got.Weight != 10 {
		t.Errorf("weight = %d, want default 10", got.Weight)
	}
	if got.Priority != 5 {
		t.Errorf("priority = %d, want default 5", got.Priority)
	}
	if got.CooldownS != 900 {
		t.Errorf("cooldown_s = %d, want default 900", got.CooldownS)
	}
	if got.DecayRate != 1.0 {
		t.Errorf("decay_rate = %v, want default 1.0", got.DecayRate)
	}
	if got.Enabled {
		t.Error("enabled = true, want false — create must not auto-enable")
	}
}

// TestConformance_CreateProject_LegacyPascalCase verifies backward
// compatibility: fleet automation still sends PascalCase keys, and those
// must keep binding after the snake_case json tags landed.
func TestConformance_CreateProject_LegacyPascalCase(t *testing.T) {
	a := newAPITestServer(t)
	status, resp := a.do(t, "POST", "/api/v1/projects", map[string]interface{}{
		"Name":     "legacy",
		"RepoURL":  "https://example.com/legacy",
		"Workdir":  "/tmp/legacy",
		"Weight":   10,
		"Priority": 7,
	})
	if status != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %v", status, resp)
	}
	got, err := database.GetProject(context.Background(), a.db, "legacy")
	if err != nil {
		t.Fatalf("GetProject: %v", err)
	}
	if got.Weight != 10 {
		t.Errorf("weight = %d, want 10 (explicit PascalCase value)", got.Weight)
	}
	if got.Priority != 7 {
		t.Errorf("priority = %d, want 7 (explicit PascalCase value)", got.Priority)
	}
}

// TestConformance_CreateProject_DuplicateName verifies a duplicate name maps
// to 409 — previously the CHECK constraint on weight fired first and the 409
// was unreachable from the documented body shape.
func TestConformance_CreateProject_DuplicateName(t *testing.T) {
	a := newAPITestServer(t)
	mustCreateAPITestProject(t, a.db, "alpha")
	status, resp := a.do(t, "POST", "/api/v1/projects", map[string]interface{}{
		"name":     "alpha",
		"repo_url": "https://example.com/alpha2",
		"workdir":  "/tmp/alpha2",
	})
	if status != http.StatusConflict {
		t.Errorf("status = %d, want 409: %v", status, resp)
	}
}

// TestConformance_CreateProject_DuplicateWorkdir verifies the
// case-insensitive dup-workdir guard surfaces as 409 with the guard's
// message.
func TestConformance_CreateProject_DuplicateWorkdir(t *testing.T) {
	a := newAPITestServer(t)
	mustCreateAPITestProject(t, a.db, "alpha") // enabled, workdir /tmp/alpha
	status, resp := a.do(t, "POST", "/api/v1/projects", map[string]interface{}{
		"name":     "beta",
		"repo_url": "https://example.com/beta",
		"workdir":  "/TMP/ALPHA", // case-insensitive duplicate of /tmp/alpha
	})
	if status != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %v", status, resp)
	}
	msg, _ := resp["error"].(string)
	if !strings.Contains(msg, "already registered by enabled project") {
		t.Errorf("error = %q, want the dup-workdir guard message", msg)
	}
}

// TestConformance_CreateProject_CaseInsensitiveDuplicateName (SCHED-GAP-005)
// verifies that a case-variant project name is rejected with 409 when an
// enabled project with the same lowercase name exists. The workdir is
// deliberately different to isolate the name guard from the workdir guard.
func TestConformance_CreateProject_CaseInsensitiveDuplicateName(t *testing.T) {
	a := newAPITestServer(t)
	// POST creates the project disabled; flip it enabled via the resume route.
	status, resp := a.do(t, "POST", "/api/v1/projects", map[string]interface{}{
		"name":     "casealpha",
		"repo_url": "https://example.com/casealpha",
		"workdir":  "/tmp/casealpha",
	})
	if status != http.StatusCreated {
		t.Fatalf("create casealpha status = %d, want 201: %v", status, resp)
	}
	if status, resp = a.do(t, "POST", "/api/v1/projects/casealpha/resume", nil); status != http.StatusOK {
		t.Fatalf("resume casealpha status = %d, want 200: %v", status, resp)
	}

	// Case-variant name with a DIFFERENT workdir → name guard must trigger.
	status, resp = a.do(t, "POST", "/api/v1/projects", map[string]interface{}{
		"name":     "CASEALPHA",
		"repo_url": "https://example.com/casealpha-dup",
		"workdir":  "/tmp/casealpha-dup",
	})
	if status != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %v", status, resp)
	}
	msg, _ := resp["error"].(string)
	if !strings.Contains(msg, "already registered by enabled project") {
		t.Errorf("error = %q, want the case-insensitive name guard message", msg)
	}
}

// TestConformance_CreateProject_CaseInsensitiveNameDisabledOK (SCHED-GAP-005)
// verifies that a case-variant name is ALLOWED when the existing project is
// disabled (archived) — a disabled duplicate must not block registration.
func TestConformance_CreateProject_CaseInsensitiveNameDisabledOK(t *testing.T) {
	a := newAPITestServer(t)
	// First project created disabled (POST never auto-enables).
	status, resp := a.do(t, "POST", "/api/v1/projects", map[string]interface{}{
		"name":     "casebeta",
		"repo_url": "https://example.com/casebeta",
		"workdir":  "/tmp/casebeta",
	})
	if status != http.StatusCreated {
		t.Fatalf("create casebeta status = %d, want 201: %v", status, resp)
	}
	// Case-variant name with a different workdir → allowed (existing is disabled).
	status, resp = a.do(t, "POST", "/api/v1/projects", map[string]interface{}{
		"name":     "CASEBETA",
		"repo_url": "https://example.com/casebeta-dup",
		"workdir":  "/tmp/casebeta-dup",
	})
	if status != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (disabled dup must not block): %v", status, resp)
	}
}

// TestConformance_CreateProject_InvalidWeight verifies out-of-range weight
// maps to 400 with an actionable message, not 500.
func TestConformance_CreateProject_InvalidWeight(t *testing.T) {
	a := newAPITestServer(t)
	for _, weight := range []int{101, -5} {
		status, resp := a.do(t, "POST", "/api/v1/projects", map[string]interface{}{
			"name":     "badweight",
			"repo_url": "https://example.com/badweight",
			"workdir":  "/tmp/badweight",
			"weight":   weight,
		})
		if status != http.StatusBadRequest {
			t.Errorf("weight=%d: status = %d, want 400: %v", weight, status, resp)
		}
		msg, _ := resp["error"].(string)
		if !strings.Contains(msg, "weight must be 1..100") {
			t.Errorf("weight=%d: error = %q, want actionable range message", weight, msg)
		}
	}
}

// TestConformance_UpdateProject_LegacyPascalCase verifies the live
// fleet-automation PUT shape (CooldownS, Enabled, DecayRate) still binds.
func TestConformance_UpdateProject_LegacyPascalCase(t *testing.T) {
	a := newAPITestServer(t)
	mustCreateAPITestProject(t, a.db, "alpha")

	status, resp := a.do(t, "PUT", "/api/v1/projects/alpha", map[string]interface{}{
		"CooldownS": 300,
		"Enabled":   false,
		"DecayRate": 0.5,
	})
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200: %v", status, resp)
	}
	got, err := database.GetProject(context.Background(), a.db, "alpha")
	if err != nil {
		t.Fatalf("GetProject: %v", err)
	}
	if got.CooldownS != 300 {
		t.Errorf("cooldown_s = %d, want 300", got.CooldownS)
	}
	if got.Enabled {
		t.Error("enabled = true, want false after legacy Enabled:false PUT")
	}
	if got.DecayRate != 0.5 {
		t.Errorf("decay_rate = %v, want 0.5", got.DecayRate)
	}
}

// TestConformance_UpdateProject_InvalidWeight verifies out-of-range weight
// on update maps to 400, not 500.
func TestConformance_UpdateProject_InvalidWeight(t *testing.T) {
	a := newAPITestServer(t)
	mustCreateAPITestProject(t, a.db, "alpha")
	status, resp := a.do(t, "PUT", "/api/v1/projects/alpha", map[string]interface{}{
		"weight": 0,
	})
	if status != http.StatusBadRequest {
		t.Errorf("status = %d, want 400: %v", status, resp)
	}
}

// TestConformance_WireFormat_Projects asserts the exact JSON field names on
// GET /api/v1/projects: snake_case per S06 (cooldown_s, repo_url, enabled),
// never PascalCase (CooldownS, RepoURL, Enabled).
func TestConformance_WireFormat_Projects(t *testing.T) {
	a := newAPITestServer(t)
	mustCreateAPITestProject(t, a.db, "alpha")

	status, body := a.do(t, "GET", "/api/v1/projects", nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	projs := body["projects"].([]interface{})
	if len(projs) != 1 {
		t.Fatalf("got %d projects, want 1", len(projs))
	}
	p := projs[0].(map[string]interface{})
	for _, key := range []string{"name", "repo_url", "workdir", "weight", "priority", "cooldown_s", "decay_rate", "enabled", "created_at", "updated_at", "last_tick_started", "last_tick_completed"} {
		if _, ok := p[key]; !ok {
			t.Errorf("projects[0] missing snake_case key %q: %v", key, p)
		}
	}
	for _, key := range []string{"Name", "RepoURL", "Workdir", "CooldownS", "Enabled", "CreatedAt"} {
		if _, ok := p[key]; ok {
			t.Errorf("projects[0] has legacy PascalCase key %q — wire format must be snake_case", key)
		}
	}
	if p["cooldown_s"].(float64) != 900 {
		t.Errorf("cooldown_s = %v, want 900", p["cooldown_s"])
	}
	if p["enabled"].(bool) != true {
		t.Errorf("enabled = %v, want true", p["enabled"])
	}
}

// TestConformance_WireFormat_Status asserts GET /api/v1/status exposes
// active_projects (README previously documented the nonexistent
// project_count).
func TestConformance_WireFormat_Status(t *testing.T) {
	a := newAPITestServer(t)
	mustCreateAPITestProject(t, a.db, "alpha")

	status, body := a.do(t, "GET", "/api/v1/status", nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if _, ok := body["active_projects"]; !ok {
		t.Errorf("status response missing active_projects: %v", body)
	}
	if _, ok := body["project_count"]; ok {
		t.Error("status response has project_count — contract field is active_projects")
	}
}

// TestConformance_WireFormat_Ticks asserts tick records serialize snake_case
// (project_name, session_id, spawned_at, weight_used, cost_usd).
func TestConformance_WireFormat_Ticks(t *testing.T) {
	a := newAPITestServer(t)
	mustCreateAPITestProject(t, a.db, "alpha")

	tickID := database.NextTickID("alpha")
	if err := database.CreateTick(context.Background(), a.db, &database.Tick{
		ID:          tickID,
		ProjectName: "alpha",
		Status:      database.StatusQueued,
		CreatedAt:   time.Now().UTC().Format(time.RFC3339),
	}); err != nil {
		t.Fatalf("CreateTick: %v", err)
	}

	status, body := a.do(t, "GET", "/api/v1/ticks", nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	ticks := body["ticks"].([]interface{})
	if len(ticks) != 1 {
		t.Fatalf("got %d ticks, want 1", len(ticks))
	}
	tk := ticks[0].(map[string]interface{})
	for _, key := range []string{"id", "project_name", "session_id", "status", "spawned_at", "completed_at", "exit_code", "tokens_in", "tokens_out", "cost_usd", "weight_used", "created_at"} {
		if _, ok := tk[key]; !ok {
			t.Errorf("ticks[0] missing snake_case key %q: %v", key, tk)
		}
	}
	for _, key := range []string{"ID", "ProjectName", "SessionID", "SpawnedAt", "WeightUsed", "CostUSD"} {
		if _, ok := tk[key]; ok {
			t.Errorf("ticks[0] has legacy PascalCase key %q — wire format must be snake_case", key)
		}
	}
	if tk["project_name"].(string) != "alpha" {
		t.Errorf("project_name = %v, want alpha", tk["project_name"])
	}
}

// TestConformance_WireFormat_Events asserts event records serialize
// snake_case (created_at), never PascalCase (CreatedAt).
func TestConformance_WireFormat_Events(t *testing.T) {
	a := newAPITestServer(t)
	if err := database.LogEvent(context.Background(), a.db, &database.Event{
		Severity:  database.SeverityInfo,
		Component: "conformance-test",
		Message:   "wire format check",
	}); err != nil {
		t.Fatalf("LogEvent: %v", err)
	}

	status, body := a.do(t, "GET", "/api/v1/events", nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	events := body["events"].([]interface{})
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}
	ev := events[0].(map[string]interface{})
	for _, key := range []string{"id", "severity", "component", "message", "details", "created_at"} {
		if _, ok := ev[key]; !ok {
			t.Errorf("events[0] missing snake_case key %q: %v", key, ev)
		}
	}
	for _, key := range []string{"ID", "Severity", "Component", "Message", "CreatedAt"} {
		if _, ok := ev[key]; ok {
			t.Errorf("events[0] has legacy PascalCase key %q — wire format must be snake_case", key)
		}
	}
}

// --- delete project (DOGFOOD-005) ---

// TestConformance_DeleteProject_NoConfirm verifies DELETE without the
// confirm=true query param is refused with 400 and an actionable message,
// and the project is left untouched.
func TestConformance_DeleteProject_NoConfirm(t *testing.T) {
	a := newAPITestServer(t)
	// POST creates a project disabled (create never auto-enables).
	status, resp := a.do(t, "POST", "/api/v1/projects", map[string]interface{}{
		"name":     "doomed",
		"repo_url": "https://example.com/doomed",
		"workdir":  "/tmp/doomed",
	})
	if status != http.StatusCreated {
		t.Fatalf("create status = %d, want 201: %v", status, resp)
	}

	status, resp = a.do(t, "DELETE", "/api/v1/projects/doomed", nil)
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %v", status, resp)
	}
	msg, _ := resp["error"].(string)
	if !strings.Contains(msg, "confirm=true") {
		t.Errorf("error = %q, want mention of confirm=true", msg)
	}
	// Project still present and untouched.
	if _, err := database.GetProject(context.Background(), a.db, "doomed"); err != nil {
		t.Errorf("GetProject after refused delete: %v", err)
	}
}

// TestConformance_DeleteProject_EnabledWithoutConfirm verifies the confirm
// check runs FIRST: an enabled project without confirm gets 400, not 409.
func TestConformance_DeleteProject_EnabledWithoutConfirm(t *testing.T) {
	a := newAPITestServer(t)
	mustCreateAPITestProject(t, a.db, "alpha") // enabled
	status, resp := a.do(t, "DELETE", "/api/v1/projects/alpha", nil)
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (confirm checked before enabled guard): %v", status, resp)
	}
	msg, _ := resp["error"].(string)
	if !strings.Contains(msg, "confirm=true") {
		t.Errorf("error = %q, want mention of confirm=true", msg)
	}
	got, err := database.GetProject(context.Background(), a.db, "alpha")
	if err != nil {
		t.Fatalf("GetProject: %v", err)
	}
	if !got.Enabled {
		t.Error("project disabled by DELETE without confirm")
	}
}

// TestConformance_DeleteProject_EnabledProjectRefused verifies an enabled
// project is refused with 409 and an actionable pause-first message.
func TestConformance_DeleteProject_EnabledProjectRefused(t *testing.T) {
	a := newAPITestServer(t)
	// Create disabled via POST, then enable through the API resume route.
	status, resp := a.do(t, "POST", "/api/v1/projects", map[string]interface{}{
		"name":     "liveproj",
		"repo_url": "https://example.com/liveproj",
		"workdir":  "/tmp/liveproj",
	})
	if status != http.StatusCreated {
		t.Fatalf("create status = %d, want 201: %v", status, resp)
	}
	status, resp = a.do(t, "POST", "/api/v1/projects/liveproj/resume", nil)
	if status != http.StatusOK {
		t.Fatalf("resume status = %d, want 200: %v", status, resp)
	}

	status, resp = a.do(t, "DELETE", "/api/v1/projects/liveproj?confirm=true", nil)
	if status != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %v", status, resp)
	}
	msg, _ := resp["error"].(string)
	if !strings.Contains(msg, "pause it first") {
		t.Errorf("error = %q, want the pause-first guard message", msg)
	}
	// Still enabled and intact.
	got, err := database.GetProject(context.Background(), a.db, "liveproj")
	if err != nil {
		t.Fatalf("GetProject: %v", err)
	}
	if !got.Enabled {
		t.Error("project disabled by refused delete")
	}
}

// TestConformance_DeleteProject_Success verifies a disabled project is
// soft-deleted: 200 with the paused-style envelope and Enabled=false
// afterwards (row retained — GetProject still finds it).
func TestConformance_DeleteProject_Success(t *testing.T) {
	a := newAPITestServer(t)
	status, resp := a.do(t, "POST", "/api/v1/projects", map[string]interface{}{
		"name":     "testdummy",
		"repo_url": "https://example.com/testdummy",
		"workdir":  "/tmp/testdummy",
	})
	if status != http.StatusCreated {
		t.Fatalf("create status = %d, want 201: %v", status, resp)
	}

	status, resp = a.do(t, "DELETE", "/api/v1/projects/testdummy?confirm=true", nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200: %v", status, resp)
	}
	if resp["status"] != "deleted" {
		t.Errorf("status field = %v, want deleted", resp["status"])
	}
	if resp["project"] != "testdummy" {
		t.Errorf("project field = %v, want testdummy", resp["project"])
	}
	// Soft delete: row retained, Enabled=false.
	got, err := database.GetProject(context.Background(), a.db, "testdummy")
	if err != nil {
		t.Fatalf("GetProject after delete: %v", err)
	}
	if got.Enabled {
		t.Error("enabled = true, want false after soft delete")
	}
}

// TestConformance_DeleteProject_NotFound verifies an unknown project maps
// to 404 even with confirm=true.
func TestConformance_DeleteProject_NotFound(t *testing.T) {
	a := newAPITestServer(t)
	status, resp := a.do(t, "DELETE", "/api/v1/projects/nope?confirm=true", nil)
	if status != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: %v", status, resp)
	}
	msg, _ := resp["error"].(string)
	if !strings.Contains(msg, "not found") {
		t.Errorf("error = %q, want project not found", msg)
	}
}

// --- purge (hard delete, DOGFOOD-009) ---

// TestConformance_DeleteProject_PurgeWithoutConfirm verifies that purge has
// its OWN confirm requirement: ?purge=true without confirm=true is refused
// with 400 and the row is left untouched.
func TestConformance_DeleteProject_PurgeWithoutConfirm(t *testing.T) {
	a := newAPITestServer(t)
	status, resp := a.do(t, "POST", "/api/v1/projects", map[string]interface{}{
		"name":     "purgedummy",
		"repo_url": "https://example.com/purgedummy",
		"workdir":  "/tmp/purgedummy",
	})
	if status != http.StatusCreated {
		t.Fatalf("create status = %d, want 201: %v", status, resp)
	}

	status, resp = a.do(t, "DELETE", "/api/v1/projects/purgedummy?purge=true", nil)
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (purge without confirm): %v", status, resp)
	}
	msg, _ := resp["error"].(string)
	if !strings.Contains(msg, "confirm=true") {
		t.Errorf("error = %q, want mention of confirm=true", msg)
	}
	// Row still present.
	if _, err := database.GetProject(context.Background(), a.db, "purgedummy"); err != nil {
		t.Errorf("GetProject after refused purge: %v", err)
	}
}

// TestConformance_DeleteProject_PurgeSuccess verifies confirm=true&purge=true
// hard-deletes: 200 with status=purged, the row disappears from
// GET /api/v1/projects AND from the database, while historical ticks stay.
func TestConformance_DeleteProject_PurgeSuccess(t *testing.T) {
	a := newAPITestServer(t)
	status, resp := a.do(t, "POST", "/api/v1/projects", map[string]interface{}{
		"name":     "purgevictim",
		"repo_url": "https://example.com/purgevictim",
		"workdir":  "/tmp/purgevictim",
	})
	if status != http.StatusCreated {
		t.Fatalf("create status = %d, want 201: %v", status, resp)
	}
	// Historical tick for the doomed project.
	tickID := database.NextTickID("purgevictim")
	if err := database.CreateTick(context.Background(), a.db, &database.Tick{
		ID:          tickID,
		ProjectName: "purgevictim",
		Status:      database.StatusCompleted,
		CreatedAt:   time.Now().UTC().Format(time.RFC3339),
	}); err != nil {
		t.Fatalf("CreateTick: %v", err)
	}

	status, resp = a.do(t, "DELETE", "/api/v1/projects/purgevictim?confirm=true&purge=true", nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200: %v", status, resp)
	}
	if resp["status"] != "purged" {
		t.Errorf("status field = %v, want purged", resp["status"])
	}

	// Gone from the API list.
	status, body := a.do(t, "GET", "/api/v1/projects", nil)
	if status != http.StatusOK {
		t.Fatalf("list status = %d, want 200", status)
	}
	for _, p := range body["projects"].([]interface{}) {
		if p.(map[string]interface{})["name"] == "purgevictim" {
			t.Error("purged project still listed in GET /api/v1/projects")
		}
	}
	// Gone from the DB.
	if _, err := database.GetProject(context.Background(), a.db, "purgevictim"); err == nil {
		t.Error("purged project still present in DB")
	}
	// Historical tick retained.
	got, err := database.GetTick(context.Background(), a.db, tickID)
	if err != nil || got == nil {
		t.Errorf("historical tick lost after purge: %v", err)
	}
}

// TestConformance_DeleteProject_PurgeEnabledRefused verifies an enabled
// project is refused with 409 even with confirm=true&purge=true.
func TestConformance_DeleteProject_PurgeEnabledRefused(t *testing.T) {
	a := newAPITestServer(t)
	status, resp := a.do(t, "POST", "/api/v1/projects", map[string]interface{}{
		"name":     "purgelive",
		"repo_url": "https://example.com/purgelive",
		"workdir":  "/tmp/purgelive",
	})
	if status != http.StatusCreated {
		t.Fatalf("create status = %d, want 201: %v", status, resp)
	}
	if status, resp = a.do(t, "POST", "/api/v1/projects/purgelive/resume", nil); status != http.StatusOK {
		t.Fatalf("resume status = %d, want 200: %v", status, resp)
	}

	status, resp = a.do(t, "DELETE", "/api/v1/projects/purgelive?confirm=true&purge=true", nil)
	if status != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %v", status, resp)
	}
	// Still enabled and present.
	got, err := database.GetProject(context.Background(), a.db, "purgelive")
	if err != nil {
		t.Fatalf("GetProject: %v", err)
	}
	if !got.Enabled {
		t.Error("project disabled by refused purge")
	}
}
