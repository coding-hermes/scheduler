//go:build integration
// +build integration

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

const testPort = ":9199"

var testDB string

func TestMain(m *testing.M) {
	// Use a unique temp file per run.
	f, err := os.CreateTemp("", "scheduler-test-*.db")
	if err != nil {
		fmt.Fprintf(os.Stderr, "create temp db: %v\n", err)
		os.Exit(1)
	}
	testDB = f.Name()
	f.Close()
	os.Remove(testDB) // Let SQLite create it.

	// Build the binary.
	if err := exec.Command("go", "build", "-o", "/tmp/schedulerd-test", "./cmd/schedulerd/").Run(); err != nil {
		fmt.Fprintf(os.Stderr, "build schedulerd: %v\n", err)
		os.Exit(1)
	}

	code := m.Run()

	os.Remove("/tmp/schedulerd-test")
	os.Remove(testDB)
	os.Remove(testDB + "-wal")
	os.Remove(testDB + "-shm")
	os.Exit(code)
}

func TestIntegrationAllLayers(t *testing.T) {
	// Start the scheduler.
	cmd := exec.Command("/tmp/schedulerd-test",
		"-listen", "127.0.0.1"+testPort,
		"-db", testDB,
	)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start schedulerd: %v", err)
	}
	defer func() {
		cmd.Process.Signal(os.Interrupt)
		cmd.Wait()
	}()

	// Wait for it to come up.
	base := "http://127.0.0.1" + testPort
	if !waitForReady(t, base+"/api/v1/health", 10*time.Second) {
		t.Fatal("schedulerd did not become healthy in time")
	}

	// Pass the cmd reference to sub-tests that need to restart the server.
	t.Run("Health", func(t *testing.T) { testHealth(t, base) })
	t.Run("API_Projects", func(t *testing.T) { testAPIProjects(t, base) })
	// t.Run("Dashboard", func(t *testing.T) { testDashboard(t, base) })
	t.Run("MCP", func(t *testing.T) { testMCP(t, base) })
	t.Run("TickLifecycle", func(t *testing.T) { testTickLifecycle(t, base) })
	t.Run("DynamicConfig", func(t *testing.T) { testDynamicConfig(t, base) })
	t.Run("NamespaceCRUD", func(t *testing.T) { testNamespaceCRUD(t, base) })
	t.Run("NamespaceProjectAssignment", func(t *testing.T) { testNamespaceProjectAssignment(t, base) })
	t.Run("NamespaceModeToggle", func(t *testing.T) { testNamespaceModeToggle(t, cmd, base) })
}

// restartScheduler starts a fresh schedulerd process with the given env vars
// and returns the new process. It also registers a cleanup that stops it.
func restartScheduler(t *testing.T, old *exec.Cmd, envVars []string) *exec.Cmd {
	t.Helper()
	old.Process.Signal(os.Interrupt)
	old.Wait()

	newCmd := exec.Command("/tmp/schedulerd-test",
		"-listen", "127.0.0.1"+testPort,
		"-db", testDB,
	)
	newCmd.Env = append(os.Environ(), envVars...)
	newCmd.Stdout = os.Stdout
	newCmd.Stderr = os.Stderr
	if err := newCmd.Start(); err != nil {
		t.Fatalf("restart schedulerd: %v", err)
	}
	t.Cleanup(func() {
		newCmd.Process.Signal(os.Interrupt)
		newCmd.Wait()
	})
	return newCmd
}

// restartSchedulerWithDB starts a fresh schedulerd process with the given
// database path, env vars, and registers a cleanup that stops it. It returns
// the new process so callers can update the outer cmd variable.
func restartSchedulerWithDB(t *testing.T, old *exec.Cmd, dbPath string, envVars []string) *exec.Cmd {
	t.Helper()
	old.Process.Signal(os.Interrupt)
	old.Wait()

	newCmd := exec.Command("/tmp/schedulerd-test",
		"-listen", "127.0.0.1"+testPort,
		"-db", dbPath,
	)
	newCmd.Env = append(os.Environ(), envVars...)
	newCmd.Stdout = os.Stdout
	newCmd.Stderr = os.Stderr
	if err := newCmd.Start(); err != nil {
		t.Fatalf("restart schedulerd: %v", err)
	}
	t.Cleanup(func() {
		newCmd.Process.Signal(os.Interrupt)
		newCmd.Wait()
	})
	return newCmd
}

func testHealth(t *testing.T, base string) {
	resp, err := http.Get(base + "/api/v1/health")
	if err != nil {
		t.Fatalf("GET /health: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	var body map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&body)

	if body["status"] != "ok" {
		t.Errorf("expected status ok, got %v", body["status"])
	}
	if body["db"] != "connected" {
		t.Errorf("expected db connected, got %v", body["db"])
	}
}

func testAPIProjects(t *testing.T, base string) {
	proj := map[string]interface{}{
		"Name":      "integration-test",
		"RepoURL":   "local:/tmp/integration-test",
		"Workdir":   "/tmp/integration-test",
		"Weight":    10,
		"Priority":  5,
		"CooldownS": 900,
		"DecayRate": 1.0,
		"Model":     "test-model",
		"Provider":  "test-provider",
		"Enabled":   true,
	}
	body := mustJSON(proj)
	resp, err := http.Post(base+"/api/v1/projects", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST /projects: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 201 && resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200/201, got %d: %s", resp.StatusCode, string(b))
	}

	// GET the project.
	resp, err = http.Get(base + "/api/v1/projects/integration-test")
	if err != nil {
		t.Fatalf("GET /projects/integration-test: %v", err)
	}
	defer resp.Body.Close()

	var result map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&result)
	project := result["project"].(map[string]interface{})

	if project["Name"] != "integration-test" {
		t.Errorf("expected Name integration-test, got %v", project["Name"])
	}

	// List projects.
	resp, err = http.Get(base + "/api/v1/projects")
	if err != nil {
		t.Fatalf("GET /projects: %v", err)
	}
	defer resp.Body.Close()

	var list map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&list)
	projects := list["projects"].([]interface{})
	if len(projects) == 0 {
		t.Error("expected at least 1 project in list")
	}
}

func testDashboard(t *testing.T, base string) {
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(base + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	body, _ := io.ReadAll(resp.Body)
	html := string(body)

	if !strings.Contains(html, "<!DOCTYPE html>") {
		t.Error("dashboard missing DOCTYPE")
	}
	if !strings.Contains(html, "<title>") {
		t.Error("dashboard missing title")
	}
	if len(html) < 500 {
		t.Errorf("dashboard too small: %d bytes", len(html))
	}
}

func testMCP(t *testing.T, base string) {
	// Initialize.
	initReq := mcpRequest("initialize", map[string]interface{}{
		"protocolVersion": "2024-11-05",
		"capabilities":    map[string]interface{}{},
	})
	resp, err := http.Post(base+"/mcp", "application/json", bytes.NewReader(initReq))
	if err != nil {
		t.Fatalf("POST /mcp initialize: %v", err)
	}
	defer resp.Body.Close()

	var initResult map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&initResult)

	result := initResult["result"].(map[string]interface{})
	serverInfo := result["serverInfo"].(map[string]interface{})
	if serverInfo["name"] != "coding-hermes-scheduler" {
		t.Errorf("server name: got %v", serverInfo["name"])
	}

	// List tools.
	listReq := mcpRequest("tools/list", nil)
	resp, err = http.Post(base+"/mcp", "application/json", bytes.NewReader(listReq))
	if err != nil {
		t.Fatalf("POST /mcp tools/list: %v", err)
	}
	defer resp.Body.Close()

	var listResult map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&listResult)
	tools := listResult["result"].(map[string]interface{})["tools"].([]interface{})
	if len(tools) < 10 {
		t.Errorf("expected at least 10 tools, got %d", len(tools))
	}

	// Call fleet_status.
	callReq := mcpRequest("tools/call", map[string]interface{}{
		"name":      "fleet_status",
		"arguments": map[string]interface{}{},
	})
	resp, err = http.Post(base+"/mcp", "application/json", bytes.NewReader(callReq))
	if err != nil {
		t.Fatalf("POST /mcp fleet_status: %v", err)
	}
	defer resp.Body.Close()

	var callResult map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&callResult)
	content := callResult["result"].(map[string]interface{})["content"].([]interface{})
	if len(content) == 0 {
		t.Error("fleet_status returned no content")
	}
}

func testTickLifecycle(t *testing.T, base string) {
	// Force evaluate.
	resp, err := http.Post(base+"/api/v1/evaluate", "application/json", nil)
	if err != nil {
		t.Fatalf("POST /evaluate: %v", err)
	}
	resp.Body.Close()

	time.Sleep(2 * time.Second)

	// Check status endpoint.
	resp, err = http.Get(base + "/api/v1/status")
	if err != nil {
		t.Fatalf("GET /status: %v", err)
	}
	defer resp.Body.Close()

	var status map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&status)

	if status["active_projects"] == nil {
		t.Error("status missing active_projects")
	}
	if status["budget_total"] == nil {
		t.Error("status missing budget_total")
	}
}

func testDynamicConfig(t *testing.T, base string) {
	update := map[string]interface{}{
		"Weight":   50,
		"Priority": 8,
	}
	body := mustJSON(update)
	req, _ := http.NewRequest(http.MethodPut, base+"/api/v1/projects/integration-test",
		bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PUT /projects/integration-test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, string(b))
	}

	// Verify.
	resp, err = http.Get(base + "/api/v1/projects/integration-test")
	if err != nil {
		t.Fatalf("GET verify: %v", err)
	}
	defer resp.Body.Close()

	var result map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&result)
	project := result["project"].(map[string]interface{})

	if int(project["Weight"].(float64)) != 50 {
		t.Errorf("expected Weight 50, got %v", project["Weight"])
	}
	if int(project["Priority"].(float64)) != 8 {
		t.Errorf("expected Priority 8, got %v", project["Priority"])
	}

	// Pause and resume.
	resp, _ = http.Post(base+"/api/v1/projects/integration-test/pause", "application/json", nil)
	resp.Body.Close()
	resp, _ = http.Post(base+"/api/v1/projects/integration-test/resume", "application/json", nil)
	resp.Body.Close()
}

func waitForReady(t *testing.T, url string, timeout time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		resp, err := http.Get(url)
		if err == nil && resp.StatusCode == 200 {
			resp.Body.Close()
			return true
		}
		if resp != nil {
			resp.Body.Close()
		}
		time.Sleep(200 * time.Millisecond)
	}
	return false
}

// mustRequest is a helper for integration tests: creates a request, sends it,
// checks status, and returns the decoded JSON body.
func mustRequest(t *testing.T, method, url string, status int, body interface{}) map[string]interface{} {
	t.Helper()
	var req *http.Request
	var err error
	if body != nil {
		b, _ := json.Marshal(body)
		req, err = http.NewRequest(method, url, bytes.NewReader(b))
		req.Header.Set("Content-Type", "application/json")
	} else {
		req, err = http.NewRequest(method, url, nil)
	}
	if err != nil {
		t.Fatalf("NewRequest %s %s: %v", method, url, err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != status {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("%s %s expected status %d, got %d: %s", method, url, status, resp.StatusCode, string(b))
	}
	var result map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil && status != 204 {
		t.Fatalf("decode %s %s response: %v", method, url, err)
	}
	return result
}

// createNamespace is a helper for namespace integration tests.
func createNamespace(t *testing.T, base string, id string, weight int) {
	t.Helper()
	body := map[string]interface{}{
		"id":       id,
		"weight":   weight,
		"reserved": 1,
		"hard_cap": 100,
		"enabled":  true,
	}
	mustRequest(t, http.MethodPost, base+"/api/v1/namespaces", http.StatusCreated, body)
}

// listNamespaces returns the namespaces array from the list endpoint.
func listNamespaces(t *testing.T, base string) []interface{} {
	t.Helper()
	resp := mustRequest(t, http.MethodGet, base+"/api/v1/namespaces", http.StatusOK, nil)
	list, ok := resp["namespaces"].([]interface{})
	if !ok {
		t.Fatalf("namespaces field not an array: %T", resp["namespaces"])
	}
	return list
}

func testNamespaceCRUD(t *testing.T, base string) {
	createNamespace(t, base, "alpha", 10)
	createNamespace(t, base, "beta", 10)
	createNamespace(t, base, "gamma", 10)

	list := listNamespaces(t, base)
	if len(list) != 3 {
		t.Fatalf("expected 3 namespaces, got %d", len(list))
	}

	// Update one namespace.
	update := map[string]interface{}{"weight": 50}
	mustRequest(t, http.MethodPut, base+"/api/v1/namespaces/alpha", http.StatusOK, update)

	// Verify update.
	alpha := mustRequest(t, http.MethodGet, base+"/api/v1/namespaces/alpha", http.StatusOK, nil)
	if w, ok := alpha["weight"].(float64); !ok || int(w) != 50 {
		t.Errorf("alpha weight = %v, want 50", alpha["weight"])
	}

	// Delete one namespace via soft-delete.
	mustRequest(t, http.MethodPut, base+"/api/v1/namespaces/beta", http.StatusOK,
		map[string]interface{}{"enabled": false})

	// Final list should reflect remaining namespaces (alpha, gamma, beta disabled).
	list = listNamespaces(t, base)
	if len(list) != 3 {
		t.Errorf("expected 3 namespace rows after soft-delete, got %d", len(list))
	}

	// Count enabled namespaces.
	enabled := 0
	for _, item := range list {
		ns := item.(map[string]interface{})
		if enabledVal, ok := ns["enabled"].(bool); ok && enabledVal {
			enabled++
		}
	}
	if enabled != 2 {
		t.Errorf("expected 2 enabled namespaces, got %d", enabled)
	}
}

func testNamespaceProjectAssignment(t *testing.T, base string) {
	// Create namespace first.
	createNamespace(t, base, "team-a", 10)

	// Create a project.
	proj := map[string]interface{}{
		"Name":      "ns-proj-test",
		"RepoURL":   "local:/tmp/ns-proj-test",
		"Workdir":   "/tmp/ns-proj-test",
		"Weight":    10,
		"Priority":  5,
		"CooldownS": 900,
		"DecayRate": 1.0,
		"Model":     "test-model",
		"Provider":  "test-provider",
		"Enabled":   true,
	}
	mustRequest(t, http.MethodPost, base+"/api/v1/projects", http.StatusCreated, proj)

	// Move project to namespace.
	mustRequest(t, http.MethodPost, base+"/api/v1/namespaces/team-a/move", http.StatusOK,
		map[string]interface{}{"project": "ns-proj-test"})

	// Verify project appears in namespace projects.
	resp := mustRequest(t, http.MethodGet, base+"/api/v1/namespaces/team-a/projects", http.StatusOK, nil)
	if resp["namespace_id"] != "team-a" {
		t.Errorf("namespace_id = %v, want team-a", resp["namespace_id"])
	}
	projects, ok := resp["projects"].([]interface{})
	if !ok {
		t.Fatalf("projects field not an array: %T", resp["projects"])
	}
	if len(projects) != 1 {
		t.Fatalf("expected 1 project in namespace, got %d", len(projects))
	}
	p := projects[0].(map[string]interface{})
	if nsID, ok := p["NamespaceID"].(string); !ok || nsID != "team-a" {
		t.Errorf("project NamespaceID = %v, want team-a", p["NamespaceID"])
	}
}

func testNamespaceModeToggle(t *testing.T, cmd *exec.Cmd, base string) {
	// Restart schedulerd with namespace mode enabled via env var first, so
	// we test the toggle against a fresh state with namespace mode on.
	cmd = restartSchedulerWithDB(t, cmd, testDB+"-toggle", []string{"SCHEDULER_NAMESPACE_MODE=true"})

	if !waitForReady(t, base+"/api/v1/health", 10*time.Second) {
		t.Fatal("schedulerd did not become healthy after restart")
	}

	// Create namespaces after namespace mode is enabled.
	createNamespace(t, base, "prod", 10)
	createNamespace(t, base, "staging", 10)

	// Verify namespaces are listed in the multi-namespace response.
	list := listNamespaces(t, base)
	if len(list) != 2 {
		t.Fatalf("expected 2 namespaces, got %d", len(list))
	}

	// Dashboard should render successfully when namespace mode is enabled.
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(base + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	html := string(body)
	if len(html) < 500 {
		t.Errorf("dashboard too small: %d bytes", len(html))
	}
	respHealth, err := client.Get(base + "/api/v1/health")
	if err != nil {
		t.Fatalf("GET /api/v1/health after namespace mode toggle: %v", err)
	}
	respHealth.Body.Close()
	if respHealth.StatusCode != 200 {
		t.Errorf("health after namespace mode toggle = %d, want 200", respHealth.StatusCode)
	}
}

func mcpRequest(method string, params map[string]interface{}) []byte {
	req := map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  method,
	}
	if params != nil {
		req["params"] = params
	}
	return mustJSON(req)
}

func mustJSON(v interface{}) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}
