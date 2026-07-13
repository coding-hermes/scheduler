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

	t.Run("Health", func(t *testing.T) { testHealth(t, base) })
	t.Run("API_Projects", func(t *testing.T) { testAPIProjects(t, base) })
	// t.Run("Dashboard", func(t *testing.T) { testDashboard(t, base) })
	t.Run("MCP", func(t *testing.T) { testMCP(t, base) })
	t.Run("TickLifecycle", func(t *testing.T) { testTickLifecycle(t, base) })
	t.Run("DynamicConfig", func(t *testing.T) { testDynamicConfig(t, base) })
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
