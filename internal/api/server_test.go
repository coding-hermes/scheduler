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
	"strings"
	"testing"
	"time"

	"github.com/coding-herms/scheduler/internal/api"
	"github.com/coding-herms/scheduler/internal/database"
	"github.com/coding-herms/scheduler/internal/scheduler"
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
	status, _ := a.do(t, "DELETE", "/api/v1/projects/alpha", nil)
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

// Compile-time sanity: use fmt so the import isn't unused when we add debug later.
var _ = fmt.Sprintf
