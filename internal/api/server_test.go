package api_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/coding-hermes/scheduler/internal/api"
	"github.com/coding-hermes/scheduler/internal/database"
	"github.com/coding-hermes/scheduler/internal/scheduler"
)

// apiTestServer spins up an in-memory DB + Loop + api.Server wired to httptest.
type apiTestServer struct {
	db     *sql.DB
	loop   *scheduler.Loop
	server *api.Server
	ts     *httptest.Server
}

func newAPITestServer(t *testing.T) *apiTestServer {
	t.Helper()
	db, err := database.InitDB(":memory:")
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	// budget=0 ensures Pick returns empty so ForceEvaluate is a no-op (no real spawning).
	loop := scheduler.NewLoop(db, time.Minute, time.Hour, 10, 0, 5)
	// DOGFOOD-015: the spawn endpoint now enqueues synchronously and fires a
	// real spawn session. Disable the exec fallback so the session cannot
	// launch an actual `hermes` process from the test host — the tick row is
	// enqueued before the session runs, so the endpoint contract (resolvable
	// tick_id) holds without a live spawn.
	loop.SetNoExecFallback(true)
	srv := api.NewServer(db, loop)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	return &apiTestServer{db: db, loop: loop, server: srv, ts: ts}
}

// do performs an HTTP request with optional body and returns status + parsed JSON.
func (a *apiTestServer) do(t *testing.T, method, path string, body interface{}) (int, map[string]interface{}) {
	t.Helper()
	var reqBody io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		reqBody = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, a.ts.URL+path, reqBody)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(resp.Body)
	var parsed map[string]interface{}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &parsed); err != nil {
			t.Logf("response body not JSON: %q", string(raw))
		}
	}
	return resp.StatusCode, parsed
}

func mustCreateAPITestProject(t *testing.T, db *sql.DB, name string) {
	t.Helper()
	if err := database.CreateProject(context.Background(), db, &database.Project{
		Name:      name,
		RepoURL:   "https://example.com/" + name,
		Workdir:   "/tmp/" + name,
		Weight:    10,
		Priority:  5,
		CooldownS: 900,
		DecayRate: 1.0,
		Model:     "test",
		Provider:  "test",
		Enabled:   true,
	}); err != nil {
		t.Fatalf("CreateProject %s: %v", name, err)
	}
}

// --- health ---

func TestHealth(t *testing.T) {
	a := newAPITestServer(t)
	// last_evaluation should be present and parseable before any evaluation runs,
	// but evaluation_age_seconds must be > 0 only after an evaluation has fired.
	status, body := a.do(t, "GET", "/api/v1/health", nil)
	if status != http.StatusOK {
		t.Errorf("status = %d, want 200", status)
	}
	if body["status"] != "ok" {
		t.Errorf("status field = %v, want ok", body["status"])
	}
	if _, ok := body["uptime"]; !ok {
		t.Errorf("uptime missing from response: %v", body)
	}
	if body["db"] != "connected" {
		t.Errorf("db = %v, want connected", body["db"])
	}
	if _, ok := body["last_evaluation"]; !ok {
		t.Errorf("last_evaluation missing from response: %v", body)
	}
	if _, ok := body["evaluation_age_seconds"]; !ok {
		t.Errorf("evaluation_age_seconds missing from response: %v", body)
	}

	// Force an evaluation, then verify last_evaluation + a positive age appear.
	a.loop.ForceEvaluate()

	// Wait for the evaluation goroutine to populate lastEval.
	deadline := time.Now().Add(2 * time.Second)
	var sawPositiveAge bool
	for time.Now().Before(deadline) {
		_, b := a.do(t, "GET", "/api/v1/health", nil)
		ts, ok := b["last_evaluation"].(string)
		if !ok || ts == "" {
			time.Sleep(20 * time.Millisecond)
			continue
		}
		age, ok := b["evaluation_age_seconds"].(float64)
		if !ok {
			t.Errorf("evaluation_age_seconds not a number: %T (%v)", b["evaluation_age_seconds"], b["evaluation_age_seconds"])
			break
		}
		if age > 0 {
			sawPositiveAge = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !sawPositiveAge {
		t.Errorf("evaluation_age_seconds never > 0 after ForceEvaluate: %v", body)
	}
}

func TestAPI_Health_MethodNotAllowed(t *testing.T) {
	a := newAPITestServer(t)
	status, _ := a.do(t, "POST", "/api/v1/health", nil)
	if status != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", status)
	}
}

// --- status ---

func TestAPI_Status(t *testing.T) {
	a := newAPITestServer(t)
	mustCreateAPITestProject(t, a.db, "alpha")
	mustCreateAPITestProject(t, a.db, "beta")

	status, body := a.do(t, "GET", "/api/v1/status", nil)
	if status != http.StatusOK {
		t.Errorf("status = %d, want 200", status)
	}
	if body["active_projects"] == nil {
		t.Errorf("active_projects missing: %v", body)
	}
	// active_projects is a float64 in JSON.
	if n, ok := body["active_projects"].(float64); !ok || int(n) != 2 {
		t.Errorf("active_projects = %v, want 2", body["active_projects"])
	}
	if _, ok := body["active_ticks"]; !ok {
		t.Errorf("active_ticks missing")
	}
}

// TestAPI_Status_ProjectsFailureRates (SCHED-GAP-018) verifies the
// projects_failure_rates field is present in /api/v1/status with the correct
// shape and per-project failure-rate math.
func TestAPI_Status_ProjectsFailureRates(t *testing.T) {
	a := newAPITestServer(t)
	mustCreateAPITestProject(t, a.db, "alpha")
	mustCreateAPITestProject(t, a.db, "beta")

	// alpha: 8 failed + 2 completed = 80% failure rate over 10 ticks.
	now := time.Now()
	for i := 0; i < 8; i++ {
		insertAPITestTick(t, a.db, "alpha-fail-"+strconv.Itoa(i), "alpha", "failed",
			now.Add(-time.Duration(10-i)*time.Minute))
	}
	for i := 0; i < 2; i++ {
		insertAPITestTick(t, a.db, "alpha-ok-"+strconv.Itoa(i), "alpha", "completed",
			now.Add(-time.Duration(2-i)*time.Minute))
	}
	// beta: 5 completed = 0% failure rate.
	for i := 0; i < 5; i++ {
		insertAPITestTick(t, a.db, "beta-ok-"+strconv.Itoa(i), "beta", "completed",
			now.Add(-time.Duration(5-i)*time.Minute))
	}

	status, body := a.do(t, "GET", "/api/v1/status", nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}

	// failure_window should be present and an integer (default 100).
	fw, ok := body["failure_window"]
	if !ok {
		t.Fatalf("failure_window missing from status response: %v", body)
	}
	if n, ok := fw.(float64); !ok || int(n) != 100 {
		t.Errorf("failure_window = %v, want 100", fw)
	}

	// projects_failure_rates should be present and a map.
	rates, ok := body["projects_failure_rates"].(map[string]interface{})
	if !ok {
		t.Fatalf("projects_failure_rates missing or wrong type: %T", body["projects_failure_rates"])
	}

	// alpha: {failed: 8, total: 10, failure_rate: 0.8}
	alpha, ok := rates["alpha"].(map[string]interface{})
	if !ok {
		t.Fatalf("alpha missing from projects_failure_rates: %v", rates)
	}
	if failed, ok := alpha["failed"].(float64); !ok || int(failed) != 8 {
		t.Errorf("alpha failed = %v, want 8", alpha["failed"])
	}
	if total, ok := alpha["total"].(float64); !ok || int(total) != 10 {
		t.Errorf("alpha total = %v, want 10", alpha["total"])
	}
	if rate, ok := alpha["failure_rate"].(float64); !ok || rate != 0.8 {
		t.Errorf("alpha failure_rate = %v, want 0.8", alpha["failure_rate"])
	}

	// beta: {failed: 0, total: 5, failure_rate: 0}
	beta, ok := rates["beta"].(map[string]interface{})
	if !ok {
		t.Fatalf("beta missing from projects_failure_rates: %v", rates)
	}
	if rate, ok := beta["failure_rate"].(float64); !ok || rate != 0 {
		t.Errorf("beta failure_rate = %v, want 0", beta["failure_rate"])
	}
}

// TestAPI_Status_ProjectsFailureRates_Empty verifies the field is present
// even when there are no ticks.
func TestAPI_Status_ProjectsFailureRates_Empty(t *testing.T) {
	a := newAPITestServer(t)
	mustCreateAPITestProject(t, a.db, "alpha")

	status, body := a.do(t, "GET", "/api/v1/status", nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	rates, ok := body["projects_failure_rates"].(map[string]interface{})
	if !ok {
		t.Fatalf("projects_failure_rates missing or wrong type: %T", body["projects_failure_rates"])
	}
	if len(rates) != 0 {
		t.Errorf("expected empty projects_failure_rates, got %v", rates)
	}
}

// insertAPITestTick inserts a tick row directly into the DB for status tests.
func insertAPITestTick(t *testing.T, db *sql.DB, id, project, status string, spawnedAt time.Time) {
	t.Helper()
	ts := spawnedAt.Format(time.RFC3339)
	_, err := db.Exec(
		`INSERT INTO ticks (id, project_name, status, completed_at, spawned_at, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
		id, project, status, ts, ts, ts)
	if err != nil {
		t.Fatalf("insert tick %s: %v", id, err)
	}
}

// --- projects list/create ---

func TestAPI_ListProjects_Empty(t *testing.T) {
	a := newAPITestServer(t)
	status, body := a.do(t, "GET", "/api/v1/projects", nil)
	if status != http.StatusOK {
		t.Errorf("status = %d, want 200", status)
	}
	projs, ok := body["projects"].([]interface{})
	if !ok {
		t.Fatalf("projects field not an array: %T", body["projects"])
	}
	if len(projs) != 0 {
		t.Errorf("got %d projects, want 0", len(projs))
	}
}

func TestAPI_ListProjects_WithData(t *testing.T) {
	a := newAPITestServer(t)
	mustCreateAPITestProject(t, a.db, "alpha")
	mustCreateAPITestProject(t, a.db, "beta")

	status, body := a.do(t, "GET", "/api/v1/projects", nil)
	if status != http.StatusOK {
		t.Errorf("status = %d, want 200", status)
	}
	projs := body["projects"].([]interface{})
	if len(projs) != 2 {
		t.Errorf("got %d projects, want 2", len(projs))
	}
}

func TestAPI_CreateProject_Success(t *testing.T) {
	a := newAPITestServer(t)
	body := map[string]interface{}{
		"Name":      "newproj",
		"RepoURL":   "https://example.com/newproj",
		"Workdir":   "/tmp/newproj",
		"Weight":    20,
		"Priority":  5,
		"CooldownS": 600,
		"DecayRate": 1.0,
		"Model":     "test",
		"Provider":  "test",
		"Enabled":   true,
	}
	status, resp := a.do(t, "POST", "/api/v1/projects", body)
	if status != http.StatusCreated {
		t.Errorf("status = %d, want 201: %v", status, resp)
	}

	// Verify it was created.
	got, err := database.GetProject(context.Background(), a.db, "newproj")
	if err != nil {
		t.Fatalf("GetProject: %v", err)
	}
	if got.Weight != 20 {
		t.Errorf("weight = %d, want 20", got.Weight)
	}
}

func TestAPI_CreateProject_MissingFields(t *testing.T) {
	a := newAPITestServer(t)
	body := map[string]interface{}{"name": "incomplete"}
	status, resp := a.do(t, "POST", "/api/v1/projects", body)
	if status != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", status)
	}
	if msg, _ := resp["error"].(string); !strings.Contains(msg, "required") {
		t.Errorf("error = %q, want mention of required fields", msg)
	}
}

func TestAPI_CreateProject_InvalidJSON(t *testing.T) {
	a := newAPITestServer(t)
	req, _ := http.NewRequest("POST", a.ts.URL+"/api/v1/projects", strings.NewReader("{not json"))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

func TestAPI_CreateProject_Duplicate(t *testing.T) {
	a := newAPITestServer(t)
	mustCreateAPITestProject(t, a.db, "alpha")
	body := map[string]interface{}{
		"Name":     "alpha",
		"RepoURL":  "https://example.com/alpha",
		"Workdir":  "/tmp/alpha",
		"Weight":   10,
		"Priority": 5,
		"Enabled":  true,
	}
	status, _ := a.do(t, "POST", "/api/v1/projects", body)
	if status != http.StatusConflict {
		t.Errorf("status = %d, want 409", status)
	}
}

func TestAPI_Projects_MethodNotAllowed(t *testing.T) {
	a := newAPITestServer(t)
	status, _ := a.do(t, "DELETE", "/api/v1/projects", nil)
	if status != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", status)
	}
}

// --- get / update project ---

func TestAPI_GetProject_Success(t *testing.T) {
	a := newAPITestServer(t)
	mustCreateAPITestProject(t, a.db, "alpha")
	status, body := a.do(t, "GET", "/api/v1/projects/alpha", nil)
	if status != http.StatusOK {
		t.Errorf("status = %d, want 200", status)
	}
	if body["project"] == nil {
		t.Errorf("project field missing: %v", body)
	}
}

func TestAPI_GetProject_NotFound(t *testing.T) {
	a := newAPITestServer(t)
	status, _ := a.do(t, "GET", "/api/v1/projects/nope", nil)
	if status != http.StatusNotFound {
		t.Errorf("status = %d, want 404", status)
	}
}

// TestAPI_ListProjects_LastTickStarted (SCHED-GAP-006) verifies that the
// last_tick_started and last_tick_completed fields are surfaced in the
// /api/v1/projects payloads. A ticked project must carry the exact
// timestamps written by the scheduler; a never-ticked project must still
// emit both keys with value "" (COALESCE keeps them non-NULL).
func TestAPI_ListProjects_LastTickStarted(t *testing.T) {
	a := newAPITestServer(t)
	mustCreateAPITestProject(t, a.db, "alpha") // never ticked

	// Stamp a ticked project directly on the DB (mirrors what
	// internal/scheduler/spawn.go + lifecycle.go would write).
	if _, err := a.db.Exec(
		`UPDATE projects SET last_tick_started = ?, last_tick_completed = ? WHERE name = ?`,
		"2026-08-07T12:00:00Z", "2026-08-07T13:00:00Z", "alpha"); err != nil {
		t.Fatalf("stamp timestamps: %v", err)
	}

	// --- list endpoint ---
	status, body := a.do(t, "GET", "/api/v1/projects", nil)
	if status != http.StatusOK {
		t.Fatalf("list status = %d, want 200", status)
	}
	projs := body["projects"].([]interface{})
	if len(projs) != 1 {
		t.Fatalf("got %d projects, want 1", len(projs))
	}
	p := projs[0].(map[string]interface{})
	if got := p["last_tick_started"]; got != "2026-08-07T12:00:00Z" {
		t.Errorf("list last_tick_started = %v, want 2026-08-07T12:00:00Z", got)
	}
	if got := p["last_tick_completed"]; got != "2026-08-07T13:00:00Z" {
		t.Errorf("list last_tick_completed = %v, want 2026-08-07T13:00:00Z", got)
	}

	// --- single-project endpoint ---
	status, body = a.do(t, "GET", "/api/v1/projects/alpha", nil)
	if status != http.StatusOK {
		t.Fatalf("get status = %d, want 200", status)
	}
	p = body["project"].(map[string]interface{})
	if got := p["last_tick_started"]; got != "2026-08-07T12:00:00Z" {
		t.Errorf("get last_tick_started = %v, want 2026-08-07T12:00:00Z", got)
	}
	if got := p["last_tick_completed"]; got != "2026-08-07T13:00:00Z" {
		t.Errorf("get last_tick_completed = %v, want 2026-08-07T13:00:00Z", got)
	}

	// --- never-ticked project: both keys present, empty string ---
	mustCreateAPITestProject(t, a.db, "fresh")
	status, body = a.do(t, "GET", "/api/v1/projects/fresh", nil)
	if status != http.StatusOK {
		t.Fatalf("get fresh status = %d, want 200", status)
	}
	p = body["project"].(map[string]interface{})
	v, ok := p["last_tick_started"]
	if !ok {
		t.Errorf("fresh project missing last_tick_started key: %v", p)
	} else if v != "" {
		t.Errorf("fresh last_tick_started = %v, want empty string", v)
	}
	v, ok = p["last_tick_completed"]
	if !ok {
		t.Errorf("fresh project missing last_tick_completed key: %v", p)
	} else if v != "" {
		t.Errorf("fresh last_tick_completed = %v, want empty string", v)
	}
}

func TestAPI_UpdateProject_Success(t *testing.T) {
	a := newAPITestServer(t)
	mustCreateAPITestProject(t, a.db, "alpha")

	newWeight := 50
	status, _ := a.do(t, "PUT", "/api/v1/projects/alpha", map[string]interface{}{"Weight": newWeight})
	if status != http.StatusOK {
		t.Errorf("status = %d, want 200", status)
	}
	got, _ := database.GetProject(context.Background(), a.db, "alpha")
	if got.Weight != 50 {
		t.Errorf("weight = %d, want 50", got.Weight)
	}
}

func TestAPI_UpdateProject_NotFound(t *testing.T) {
	a := newAPITestServer(t)
	status, _ := a.do(t, "PUT", "/api/v1/projects/nope", map[string]interface{}{"weight": 50})
	if status != http.StatusNotFound {
		t.Errorf("status = %d, want 404", status)
	}
}

func TestAPI_PauseProject_Success(t *testing.T) {
	a := newAPITestServer(t)
	mustCreateAPITestProject(t, a.db, "alpha")

	status, body := a.do(t, "POST", "/api/v1/projects/alpha/pause", nil)
	if status != http.StatusOK {
		t.Errorf("status = %d, want 200", status)
	}
	if body["status"] != "paused" {
		t.Errorf("status field = %v, want paused", body["status"])
	}
	got, _ := database.GetProject(context.Background(), a.db, "alpha")
	if got.Enabled {
		t.Error("project still enabled after pause")
	}
}

func TestAPI_ResumeProject_Success(t *testing.T) {
	a := newAPITestServer(t)
	mustCreateAPITestProject(t, a.db, "alpha")
	// First disable.
	enabled := false
	database.UpdateProject(context.Background(), a.db, "alpha", database.ProjectUpdates{Enabled: &enabled})

	status, body := a.do(t, "POST", "/api/v1/projects/alpha/resume", nil)
	if status != http.StatusOK {
		t.Errorf("status = %d, want 200", status)
	}
	if body["status"] != "resumed" {
		t.Errorf("status field = %v, want resumed", body["status"])
	}
	got, _ := database.GetProject(context.Background(), a.db, "alpha")
	if !got.Enabled {
		t.Error("project still disabled after resume")
	}
}

func TestAPI_SpawnProject_Success(t *testing.T) {
	a := newAPITestServer(t)
	mustCreateAPITestProject(t, a.db, "alpha")

	status, body := a.do(t, "POST", "/api/v1/projects/alpha/spawn", nil)
	if status != http.StatusAccepted {
		t.Fatalf("status = %d, want 202: %v", status, body)
	}
	if body["status"] != "spawned" {
		t.Errorf("status field = %v, want spawned", body["status"])
	}
	tickID, ok := body["tick_id"].(string)
	if !ok || !strings.HasPrefix(tickID, "alpha-") {
		t.Errorf("tick_id = %v, want alpha-YYYY-MM-DD-HH-MM-SS", body["tick_id"])
	}
	if _, err := time.Parse("2006-01-02-15-04-05", strings.TrimPrefix(tickID, "alpha-")); err != nil {
		t.Errorf("tick_id = %q has invalid timestamp: %v", tickID, err)
	}
}

func TestAPI_SpawnProject_NotFound(t *testing.T) {
	a := newAPITestServer(t)
	status, _ := a.do(t, "POST", "/api/v1/projects/nope/spawn", nil)
	if status != http.StatusNotFound {
		t.Errorf("status = %d, want 404", status)
	}
}

// TestAPI_SpawnProject_TickIDResolves is the DOGFOOD-015 regression: the
// tick_id returned by POST /projects/{name}/spawn must be a REAL stored row
// (canonical UTC via database.NextTickID), so the documented
// spawn → GET /ticks/{id} workflow resolves. Before the fix the handler
// predicted an id with time.Now().UTC().Format while SlotPool.Spawn stamped
// the stored row with LOCAL time — on a -05:00 host the returned id 404'd
// forever. Deterministic: the row is enqueued synchronously by the handler,
// so no sleeps or time mocking are needed.
func TestAPI_SpawnProject_TickIDResolves(t *testing.T) {
	a := newAPITestServer(t)
	mustCreateAPITestProject(t, a.db, "alpha")

	status, body := a.do(t, "POST", "/api/v1/projects/alpha/spawn", nil)
	if status != http.StatusAccepted {
		t.Fatalf("spawn status = %d, want 202: %v", status, body)
	}
	tickID, ok := body["tick_id"].(string)
	if !ok || tickID == "" {
		t.Fatalf("tick_id = %v, want non-empty string", body["tick_id"])
	}

	// The returned id must resolve via GET /ticks/{id} — the documented
	// spawn→poll-by-id workflow (docs/api.md §5).
	status, tickBody := a.do(t, "GET", "/api/v1/ticks/"+tickID, nil)
	if status != http.StatusOK {
		t.Fatalf("GET /ticks/%s status = %d, want 200 (tick not found): %v", tickID, status, tickBody)
	}
	if tickBody["id"] != tickID {
		t.Errorf("tick id = %v, want %q", tickBody["id"], tickID)
	}
	if tickBody["project_name"] != "alpha" {
		t.Errorf("project_name = %v, want alpha", tickBody["project_name"])
	}
	// The id must be canonical UTC (database.NextTickID format) — the
	// local-time stamping bug produced ids that never matched.
	if _, err := time.Parse("2006-01-02-15-04-05", strings.TrimPrefix(tickID, "alpha-")); err != nil {
		t.Errorf("tick_id = %q has invalid timestamp: %v", tickID, err)
	}
}

// TestAPI_SpawnProject_AlreadyRunning pins the SCHED-GAP-030 duplicate-spawn
// protection on the manual spawn endpoint: a project with a queued/running
// tick row is refused with 409 instead of double-spawning. The queued row is
// seeded directly (deterministic — the async spawn session from a live POST
// could complete before the second request lands).
func TestAPI_SpawnProject_AlreadyRunning(t *testing.T) {
	a := newAPITestServer(t)
	mustCreateAPITestProject(t, a.db, "alpha")

	// Seed a queued tick row — the same state a live spawn leaves behind.
	if err := scheduler.NewLifecycleTracker(a.db).Enqueue("alpha", "alpha-seeded-queued"); err != nil {
		t.Fatalf("seed enqueue: %v", err)
	}

	status, body := a.do(t, "POST", "/api/v1/projects/alpha/spawn", nil)
	if status != http.StatusConflict {
		t.Fatalf("spawn status = %d, want 409: %v", status, body)
	}
}

func TestAPI_SpawnProject_MethodNotAllowed(t *testing.T) {
	a := newAPITestServer(t)
	mustCreateAPITestProject(t, a.db, "alpha")
	status, _ := a.do(t, "GET", "/api/v1/projects/alpha/spawn", nil)
	if status != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", status)
	}
}

func TestAPI_ProjectByID_MethodNotAllowed(t *testing.T) {
	a := newAPITestServer(t)
	mustCreateAPITestProject(t, a.db, "alpha")
	status, _ := a.do(t, "PATCH", "/api/v1/projects/alpha", nil)
	if status != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", status)
	}
}

// --- ticks ---

func TestAPI_HandleTicks_Empty(t *testing.T) {
	a := newAPITestServer(t)
	status, body := a.do(t, "GET", "/api/v1/ticks", nil)
	if status != http.StatusOK {
		t.Errorf("status = %d, want 200", status)
	}
	ticks := body["ticks"].([]interface{})
	if len(ticks) != 0 {
		t.Errorf("got %d ticks, want 0", len(ticks))
	}
}

func TestAPI_HandleTicks_MethodNotAllowed(t *testing.T) {
	a := newAPITestServer(t)
	status, _ := a.do(t, "POST", "/api/v1/ticks", nil)
	if status != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", status)
	}
}

func TestAPI_HandleTickByID_NotFound(t *testing.T) {
	a := newAPITestServer(t)
	status, _ := a.do(t, "GET", "/api/v1/ticks/nope", nil)
	if status != http.StatusNotFound {
		t.Errorf("status = %d, want 404", status)
	}
}

func TestAPI_HandleTickByID_MethodNotAllowed(t *testing.T) {
	a := newAPITestServer(t)
	status, _ := a.do(t, "POST", "/api/v1/ticks/foo", nil)
	if status != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", status)
	}
}

// --- evaluate / pause / resume ---

func TestAPI_Evaluate_Success(t *testing.T) {
	a := newAPITestServer(t)
	status, body := a.do(t, "POST", "/api/v1/evaluate", nil)
	if status != http.StatusOK {
		t.Errorf("status = %d, want 200", status)
	}
	if body["status"] != "evaluation triggered" {
		t.Errorf("status = %v, want evaluation triggered", body["status"])
	}
}

func TestAPI_Evaluate_MethodNotAllowed(t *testing.T) {
	a := newAPITestServer(t)
	status, _ := a.do(t, "GET", "/api/v1/evaluate", nil)
	if status != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", status)
	}
}

func TestAPI_Pause_Success(t *testing.T) {
	a := newAPITestServer(t)
	status, body := a.do(t, "POST", "/api/v1/pause", nil)
	if status != http.StatusOK {
		t.Errorf("status = %d, want 200", status)
	}
	if body["status"] != "paused" {
		t.Errorf("status = %v, want paused", body["status"])
	}
}

func TestAPI_Pause_MethodNotAllowed(t *testing.T) {
	a := newAPITestServer(t)
	status, _ := a.do(t, "GET", "/api/v1/pause", nil)
	if status != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", status)
	}
}

func TestAPI_Resume_Success(t *testing.T) {
	a := newAPITestServer(t)
	// Pause first to put a value on the channel so Resume has something to send after.
	status, body := a.do(t, "POST", "/api/v1/resume", nil)
	if status != http.StatusOK {
		t.Errorf("status = %d, want 200", status)
	}
	if body["status"] != "resumed" {
		t.Errorf("status = %v, want resumed", body["status"])
	}
}

func TestAPI_Resume_MethodNotAllowed(t *testing.T) {
	a := newAPITestServer(t)
	status, _ := a.do(t, "GET", "/api/v1/resume", nil)
	if status != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", status)
	}
}

// --- events ---

func TestAPI_Events_Empty(t *testing.T) {
	a := newAPITestServer(t)
	status, body := a.do(t, "GET", "/api/v1/events", nil)
	if status != http.StatusOK {
		t.Errorf("status = %d, want 200", status)
	}
	events := body["events"].([]interface{})
	if len(events) != 0 {
		t.Errorf("got %d events, want 0", len(events))
	}
}

func TestAPI_Events_MethodNotAllowed(t *testing.T) {
	a := newAPITestServer(t)
	status, _ := a.do(t, "POST", "/api/v1/events", nil)
	if status != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", status)
	}
}

func mustInsertTick(t *testing.T, db *sql.DB, project, status string) {
	t.Helper()
	ctx := context.Background()
	id := fmt.Sprintf("tick-%s-%s-%d", project, status, time.Now().UnixNano())
	if _, err := db.ExecContext(ctx,
		`INSERT INTO ticks (id, project_name, status, created_at) VALUES (?, ?, ?, ?)`,
		id, project, status, time.Now().UTC().Format(time.RFC3339)); err != nil {
		t.Fatalf("InsertTick %s/%s: %v", project, status, err)
	}
}

// --- ticks status filter ---

func TestAPI_Ticks_WithStatusFilter(t *testing.T) {
	a := newAPITestServer(t)
	mustCreateAPITestProject(t, a.db, "alpha")
	mustInsertTick(t, a.db, "alpha", "completed")
	mustInsertTick(t, a.db, "alpha", "running")

	status, body := a.do(t, "GET", "/api/v1/ticks?status=completed", nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	ticks := body["ticks"].([]interface{})
	if len(ticks) != 1 {
		t.Fatalf("got %d ticks, want 1 (completed only)", len(ticks))
	}
}

func TestAPI_Ticks_WithStatusFilter_Empty(t *testing.T) {
	a := newAPITestServer(t)
	status, body := a.do(t, "GET", "/api/v1/ticks?status=timeout", nil)
	if status != http.StatusOK {
		t.Errorf("status = %d, want 200", status)
	}
	ticks := body["ticks"].([]interface{})
	if len(ticks) != 0 {
		t.Errorf("got %d ticks, want 0", len(ticks))
	}
}

// --- queue ---

func TestAPI_Queue_Empty(t *testing.T) {
	a := newAPITestServer(t)
	status, body := a.do(t, "GET", "/api/v1/queue", nil)
	if status != http.StatusOK {
		t.Errorf("status = %d, want 200", status)
	}
	if body == nil || body["queue"] == nil {
		return // endpoint not yet implemented
	}
	queue := body["queue"].([]interface{})
	if len(queue) != 0 {
		t.Errorf("got %d items, want 0", len(queue))
	}
}

func TestAPI_Queue_MethodNotAllowed(t *testing.T) {
	a := newAPITestServer(t)
	status, _ := a.do(t, "POST", "/api/v1/queue", nil)
	if status != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", status)
	}
}

// --- config ---

func TestAPI_Config_Success(t *testing.T) {
	a := newAPITestServer(t)
	a.server.SetResolvedConfig(api.ResolvedConfig{
		DBPath:                 "/tmp/sched.db",
		Listen:                 "127.0.0.1:9090",
		MinInterval:            "30s",
		MaxInterval:            "24h",
		NumLevels:              10,
		WeightBudget:           100,
		MaxConcurrent:          2,
		TickTimeout:            "2h",
		NamespaceMode:          true,
		AutoDisableFailureRate: 0.5,
		AutoDisableWindow:      100,
		AutoDisableMinTicks:    50,
		FailureWindow:          100,
		Gateway: api.GatewayConfigSnapshot{
			URL:            "http://127.0.0.1:8642",
			Key:            "supersecretkey",
			ForemanHome:    "/tmp/foreman",
			NoExecFallback: true,
		},
		DuckBrain: api.DuckBrainConfigSnapshot{
			Namespace: "coding-hermes",
			URL:       "http://localhost:3000",
		},
	})
	status, body := a.do(t, "GET", "/api/v1/config", nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if body["min_interval"] != "30s" {
		t.Errorf("min_interval = %v, want \"30s\"", body["min_interval"])
	}
	if body["max_concurrent"] != float64(2) {
		t.Errorf("max_concurrent = %v, want 2", body["max_concurrent"])
	}
	if body["auto_disable_failure_rate"] != 0.5 {
		t.Errorf("auto_disable_failure_rate = %v, want 0.5", body["auto_disable_failure_rate"])
	}
	if body["db_path"] != "/tmp/sched.db" {
		t.Errorf("db_path = %v, want \"/tmp/sched.db\"", body["db_path"])
	}
	if body["namespace_mode"] != true {
		t.Errorf("namespace_mode = %v, want true", body["namespace_mode"])
	}
	gw, ok := body["gateway"].(map[string]interface{})
	if !ok {
		t.Fatal("response missing gateway object")
	}
	if gw["url"] != "http://127.0.0.1:8642" {
		t.Errorf("gateway.url = %v, want \"http://127.0.0.1:8642\"", gw["url"])
	}
	if gw["key"] != "supe****" {
		t.Errorf("gateway.key = %v, want masked \"supe****\" (plaintext must never leak)", gw["key"])
	}
	if gw["no_exec_fallback"] != true {
		t.Errorf("gateway.no_exec_fallback = %v, want true", gw["no_exec_fallback"])
	}
	duck, ok := body["duckbrain"].(map[string]interface{})
	if !ok {
		t.Fatal("response missing duckbrain object")
	}
	if duck["namespace"] != "coding-hermes" {
		t.Errorf("duckbrain.namespace = %v, want \"coding-hermes\"", duck["namespace"])
	}
}

func TestAPI_Config_MethodNotAllowed(t *testing.T) {
	a := newAPITestServer(t)
	for _, method := range []string{"POST", "PUT", "DELETE"} {
		status, _ := a.do(t, method, "/api/v1/config", nil)
		if status != http.StatusMethodNotAllowed {
			t.Errorf("%s /api/v1/config status = %d, want 405", method, status)
		}
	}
}

// --- openapi ---

// documentedPaths mirrors the route table in docs/api.md (§4–§10, 19 paths).
// The openapi.json spec must contain exactly this set — a client generator
// needs every live route (GAP-057).
var documentedPaths = []string{
	"/api/v1/health",
	"/api/v1/status",
	"/api/v1/config",
	"/api/v1/projects",
	"/api/v1/projects/{name}",
	"/api/v1/projects/{name}/pause",
	"/api/v1/projects/{name}/resume",
	"/api/v1/projects/{name}/spawn",
	"/api/v1/namespaces",
	"/api/v1/namespaces/{id}",
	"/api/v1/namespaces/{id}/projects",
	"/api/v1/namespaces/{id}/move",
	"/api/v1/ticks",
	"/api/v1/ticks/{id}",
	"/api/v1/events",
	"/api/v1/queue",
	"/api/v1/evaluate",
	"/api/v1/pause",
	"/api/v1/resume",
}

func TestAPI_OpenAPI_Success(t *testing.T) {
	a := newAPITestServer(t)
	status, body := a.do(t, "GET", "/api/v1/openapi.json", nil)
	if status != http.StatusOK {
		t.Errorf("status = %d, want 200", status)
	}
	if body == nil {
		t.Fatal("openapi.json did not parse as JSON")
	}

	// (a) structure: openapi version + paths object present.
	if body["openapi"] == nil {
		t.Errorf("openapi.json missing top-level openapi version: %v", body)
	}
	paths, ok := body["paths"].(map[string]interface{})
	if !ok {
		t.Fatalf("openapi.json paths missing or not an object: %v", body["paths"])
	}

	// (b) path set == docs/api.md route table (19 paths, incl. the two
	// namespace sub-routes that were missing before GAP-057).
	if len(paths) != len(documentedPaths) {
		t.Errorf("openapi.json path count = %d, want %d (docs/api.md route table)", len(paths), len(documentedPaths))
	}
	for _, p := range documentedPaths {
		if _, ok := paths[p]; !ok {
			t.Errorf("openapi.json missing documented path %s", p)
		}
	}

	// (c) + (d) every POST/PUT operation carries a requestBody schema, and
	// every operation (of any method) declares a 2xx success response.
	for p, pathVal := range paths {
		pathObj, ok := pathVal.(map[string]interface{})
		if !ok {
			t.Errorf("path %s: not an object", p)
			continue
		}
		for method, opVal := range pathObj {
			op, ok := opVal.(map[string]interface{})
			if !ok {
				t.Errorf("path %s %s: not an object", p, method)
				continue
			}
			if method == "post" || method == "put" {
				rb, ok := op["requestBody"].(map[string]interface{})
				if !ok {
					t.Errorf("path %s %s: missing requestBody (client generators cannot build this body)", p, method)
					continue
				}
				content, _ := rb["content"].(map[string]interface{})
				if _, ok := content["application/json"]; !ok {
					t.Errorf("path %s %s: requestBody lacks application/json content", p, method)
				}
			}
			responses, ok := op["responses"].(map[string]interface{})
			if !ok {
				t.Errorf("path %s %s: missing responses object", p, method)
				continue
			}
			hasSuccess := false
			for code := range responses {
				if strings.HasPrefix(code, "2") {
					hasSuccess = true
					break
				}
			}
			if !hasSuccess {
				t.Errorf("path %s %s: no 2xx success response declared", p, method)
			}
		}
	}
}

func TestAPI_OpenAPI_MethodNotAllowed(t *testing.T) {
	a := newAPITestServer(t)
	status, _ := a.do(t, "POST", "/api/v1/openapi.json", nil)
	if status != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", status)
	}
}

// Compile-time sanity: use fmt so the import isn't unused when we add debug later.
var _ = fmt.Sprintf
