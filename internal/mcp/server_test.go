package mcp_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coding-hermes/scheduler/internal/database"
	mcpserver "github.com/coding-hermes/scheduler/internal/mcp"
	"github.com/coding-hermes/scheduler/internal/scheduler"
)

// mcpTestServer wraps an httptest server with helpers for JSON-RPC calls.
type mcpTestServer struct {
	db     *sql.DB
	loop   *scheduler.Loop
	server *mcpserver.Server
	ts     *httptest.Server
}

func newMCPTestServer(t *testing.T) *mcpTestServer {
	t.Helper()
	db, err := database.InitDB(":memory:")
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	loop := scheduler.NewLoop(db, time.Minute, time.Hour, 10, 0, 5)
	srv := mcpserver.NewServer(db, loop)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return &mcpTestServer{db: db, loop: loop, server: srv, ts: ts}
}

// call sends a JSON-RPC request and returns the parsed response and HTTP status.
func (m *mcpTestServer) call(t *testing.T, req map[string]interface{}) (int, mcpserver.MCPResponse) {
	t.Helper()
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	httpReq, err := http.NewRequest("POST", m.ts.URL+"/mcp", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var parsed mcpserver.MCPResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("unmarshal: %v (body=%q)", err, string(raw))
	}
	return resp.StatusCode, parsed
}

func mustCreateMCPProject(t *testing.T, db *sql.DB, name string) {
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

// --- HTTP envelope & error cases ---

func TestMCP_MethodNotAllowed(t *testing.T) {
	m := newMCPTestServer(t)
	resp, err := http.Get(m.ts.URL + "/mcp")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// handleMCP writes JSON-RPC error with code -32600 regardless of HTTP code;
		// the actual HTTP status is 200 (the body is the JSON-RPC error).
		// We just verify the response is parseable JSON-RPC.
		t.Logf("non-200 status = %d (acceptable for JSON-RPC error envelope)", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	var parsed mcpserver.MCPResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("body not JSON-RPC: %v", err)
	}
	if parsed.Error == nil {
		t.Errorf("expected error envelope, got %+v", parsed)
	} else if parsed.Error.Code != -32600 {
		t.Errorf("error code = %d, want -32600", parsed.Error.Code)
	}
}

func TestMCP_InvalidJSON(t *testing.T) {
	m := newMCPTestServer(t)
	resp, err := http.Post(m.ts.URL+"/mcp", "application/json", strings.NewReader("not json"))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var parsed mcpserver.MCPResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if parsed.Error == nil || parsed.Error.Code != -32700 {
		t.Errorf("expected parse error (-32700), got %+v", parsed)
	}
}

func TestMCP_UnknownMethod(t *testing.T) {
	m := newMCPTestServer(t)
	id := 1
	status, resp := m.call(t, map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  "tools/bogus",
	})
	if status != http.StatusOK {
		t.Errorf("status = %d, want 200", status)
	}
	if resp.Error == nil || resp.Error.Code != -32601 {
		t.Errorf("expected method-not-found (-32601), got %+v", resp)
	}
}

// --- initialize ---

func TestMCP_Initialize(t *testing.T) {
	m := newMCPTestServer(t)
	id := 42
	status, resp := m.call(t, map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  "initialize",
	})
	if status != http.StatusOK {
		t.Errorf("status = %d, want 200", status)
	}
	if resp.JSONRPC != "2.0" {
		t.Errorf("jsonrpc = %q, want 2.0", resp.JSONRPC)
	}
	if resp.ID == nil || *resp.ID != id {
		t.Errorf("id = %v, want %d", resp.ID, id)
	}
	if resp.Error != nil {
		t.Fatalf("unexpected error: %+v", resp.Error)
	}
	result, ok := resp.Result.(map[string]interface{})
	if !ok {
		t.Fatalf("result not an object: %T", resp.Result)
	}
	if result["protocolVersion"] == nil {
		t.Error("protocolVersion missing")
	}
	if result["serverInfo"] == nil {
		t.Error("serverInfo missing")
	}
}

// --- tools/list ---

func TestMCP_ToolsList(t *testing.T) {
	m := newMCPTestServer(t)
	id := 1
	_, resp := m.call(t, map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  "tools/list",
	})
	result, ok := resp.Result.(map[string]interface{})
	if !ok {
		t.Fatalf("result not an object: %T", resp.Result)
	}
	tools, ok := result["tools"].([]interface{})
	if !ok {
		t.Fatalf("tools not an array: %T", result["tools"])
	}
	if len(tools) == 0 {
		t.Fatal("tools list is empty")
	}

	// Verify every tool has a name and description.
	names := map[string]bool{}
	for _, tool := range tools {
		td, ok := tool.(map[string]interface{})
		if !ok {
			t.Errorf("tool not object: %T", tool)
			continue
		}
		name, _ := td["name"].(string)
		if name == "" {
			t.Error("tool has empty name")
		}
		names[name] = true
		if td["description"] == nil {
			t.Errorf("tool %q missing description", name)
		}
	}

	// Spot-check that key tools are present.
	for _, want := range []string{"fleet_status", "fleet_projects", "fleet_set_weight", "fleet_pause"} {
		if !names[want] {
			t.Errorf("expected tool %q in registry, missing", want)
		}
	}
}

// --- tools/call: happy paths ---

func TestMCP_FleetStatus(t *testing.T) {
	m := newMCPTestServer(t)
	mustCreateMCPProject(t, m.db, "alpha")
	mustCreateMCPProject(t, m.db, "beta")

	_, resp := m.call(t, map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params": map[string]interface{}{
			"name":      "fleet_status",
			"arguments": map[string]interface{}{},
		},
	})
	if resp.Error != nil {
		t.Fatalf("error: %+v", resp.Error)
	}
	text := extractText(t, resp.Result)
	if !strings.Contains(text, `"total_projects":2`) {
		t.Errorf("total_projects not 2: %s", text)
	}
}

func TestMCP_FleetProjects(t *testing.T) {
	m := newMCPTestServer(t)
	mustCreateMCPProject(t, m.db, "alpha")

	_, resp := m.call(t, map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params": map[string]interface{}{
			"name":      "fleet_projects",
			"arguments": map[string]interface{}{},
		},
	})
	if resp.Error != nil {
		t.Fatalf("error: %+v", resp.Error)
	}
	text := extractText(t, resp.Result)
	if !strings.Contains(text, "alpha") {
		t.Errorf("expected 'alpha' in response: %s", text)
	}
}

func TestMCP_FleetProjectDetail(t *testing.T) {
	m := newMCPTestServer(t)
	mustCreateMCPProject(t, m.db, "alpha")

	_, resp := m.call(t, map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params": map[string]interface{}{
			"name":      "fleet_project_detail",
			"arguments": map[string]interface{}{"name": "alpha"},
		},
	})
	if resp.Error != nil {
		t.Fatalf("error: %+v", resp.Error)
	}
	text := extractText(t, resp.Result)
	// Wire format is snake_case per S06 (DOGFOOD-001/003 conformance).
	if !strings.Contains(text, `"name":"alpha"`) {
		t.Errorf("project name missing: %s", text)
	}
}

func TestMCP_FleetProjectDetail_MissingName(t *testing.T) {
	m := newMCPTestServer(t)
	_, resp := m.call(t, map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params": map[string]interface{}{
			"name":      "fleet_project_detail",
			"arguments": map[string]interface{}{},
		},
	})
	if resp.Error == nil {
		t.Fatal("expected error for missing name")
		return
	}
	if !strings.Contains(resp.Error.Message, "name is required") {
		t.Errorf("error = %q, want mention of name", resp.Error.Message)
	}
}

func TestMCP_FleetSetWeight(t *testing.T) {
	m := newMCPTestServer(t)
	mustCreateMCPProject(t, m.db, "alpha")

	_, resp := m.call(t, map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params": map[string]interface{}{
			"name":      "fleet_set_weight",
			"arguments": map[string]interface{}{"name": "alpha", "weight": 50},
		},
	})
	if resp.Error != nil {
		t.Fatalf("error: %+v", resp.Error)
	}

	got, _ := database.GetProject(context.Background(), m.db, "alpha")
	if got.Weight != 50 {
		t.Errorf("weight = %d, want 50", got.Weight)
	}
}

func TestMCP_FleetSetWeight_OutOfRange(t *testing.T) {
	m := newMCPTestServer(t)
	mustCreateMCPProject(t, m.db, "alpha")

	_, resp := m.call(t, map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params": map[string]interface{}{
			"name":      "fleet_set_weight",
			"arguments": map[string]interface{}{"name": "alpha", "weight": 200},
		},
	})
	if resp.Error == nil {
		t.Fatal("expected error for weight out of range")
		return
	}
	if !strings.Contains(resp.Error.Message, "weight must be 1-100") {
		t.Errorf("error = %q, want range error", resp.Error.Message)
	}
}

func TestMCP_FleetSetPriority(t *testing.T) {
	m := newMCPTestServer(t)
	mustCreateMCPProject(t, m.db, "alpha")

	_, resp := m.call(t, map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params": map[string]interface{}{
			"name":      "fleet_set_priority",
			"arguments": map[string]interface{}{"name": "alpha", "priority": 9},
		},
	})
	if resp.Error != nil {
		t.Fatalf("error: %+v", resp.Error)
	}
	got, _ := database.GetProject(context.Background(), m.db, "alpha")
	if got.Priority != 9 {
		t.Errorf("priority = %d, want 9", got.Priority)
	}
}

func TestMCP_FleetSetPriority_OutOfRange(t *testing.T) {
	m := newMCPTestServer(t)
	mustCreateMCPProject(t, m.db, "alpha")

	_, resp := m.call(t, map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params": map[string]interface{}{
			"name":      "fleet_set_priority",
			"arguments": map[string]interface{}{"name": "alpha", "priority": 99},
		},
	})
	if resp.Error == nil {
		t.Fatal("expected error for priority out of range")
		return
	}
}

func TestMCP_FleetSetCooldown(t *testing.T) {
	m := newMCPTestServer(t)
	mustCreateMCPProject(t, m.db, "alpha")

	_, resp := m.call(t, map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params": map[string]interface{}{
			"name":      "fleet_set_cooldown",
			"arguments": map[string]interface{}{"name": "alpha", "cooldown": 300},
		},
	})
	if resp.Error != nil {
		t.Fatalf("error: %+v", resp.Error)
	}
}

func TestMCP_FleetSetDecay(t *testing.T) {
	m := newMCPTestServer(t)
	mustCreateMCPProject(t, m.db, "alpha")

	_, resp := m.call(t, map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params": map[string]interface{}{
			"name":      "fleet_set_decay",
			"arguments": map[string]interface{}{"name": "alpha", "decay": 2.5},
		},
	})
	if resp.Error != nil {
		t.Fatalf("error: %+v", resp.Error)
	}
}

// TestMCP_FleetSetDecay_RejectsZero guards against the starvation hole: the
// HTTP API rejects decay_rate <= 0, and the MCP path must mirror it. Before
// this guard, foremen could set decay=0 via fleet_set_decay and permanently
// starve themselves (urgency = pri × 1^0 = flat, packer never picks).
func TestMCP_FleetSetDecay_RejectsZero(t *testing.T) {
	m := newMCPTestServer(t)
	mustCreateMCPProject(t, m.db, "alpha")

	_, resp := m.call(t, map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params": map[string]interface{}{
			"name":      "fleet_set_decay",
			"arguments": map[string]interface{}{"name": "alpha", "decay": 0},
		},
	})
	if resp.Error == nil {
		t.Fatal("expected error for decay=0, got nil")
		return
	}
	if !strings.Contains(resp.Error.Message, "decay must be > 0") {
		t.Fatalf("error message = %q, want decay guard message", resp.Error.Message)
	}

	// Verify the value was not applied.
	got, err := database.GetProject(context.Background(), m.db, "alpha")
	if err != nil {
		t.Fatalf("get project: %v", err)
	}
	if got.DecayRate == 0 {
		t.Fatal("decay_rate was written despite guard rejection")
	}
}

func TestMCP_FleetPauseAndResume(t *testing.T) {
	m := newMCPTestServer(t)
	mustCreateMCPProject(t, m.db, "alpha")

	// Pause.
	_, resp := m.call(t, map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params": map[string]interface{}{
			"name":      "fleet_pause",
			"arguments": map[string]interface{}{"name": "alpha"},
		},
	})
	if resp.Error != nil {
		t.Fatalf("pause error: %+v", resp.Error)
	}
	got, _ := database.GetProject(context.Background(), m.db, "alpha")
	if got.Enabled {
		t.Error("project still enabled after pause")
	}

	// Resume.
	_, resp = m.call(t, map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      2,
		"method":  "tools/call",
		"params": map[string]interface{}{
			"name":      "fleet_resume",
			"arguments": map[string]interface{}{"name": "alpha"},
		},
	})
	if resp.Error != nil {
		t.Fatalf("resume error: %+v", resp.Error)
	}
	got, _ = database.GetProject(context.Background(), m.db, "alpha")
	if !got.Enabled {
		t.Error("project still disabled after resume")
	}
}

func TestMCP_FleetPause_MissingName(t *testing.T) {
	m := newMCPTestServer(t)
	_, resp := m.call(t, map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params": map[string]interface{}{
			"name":      "fleet_pause",
			"arguments": map[string]interface{}{},
		},
	})
	if resp.Error == nil {
		t.Fatal("expected error for missing name")
		return
	}
}

// TestMCP_FleetAdd verifies the ONLY add-project path through MCP works:
// tools/call fleet_add with a minimal {name, repo, workdir} body (no priority
// arg) must succeed and create a row with the S06 defaults (weight 10,
// priority 5). Before DOGFOOD-014 the tool built database.Project without
// Priority, so the explicit 0 defeated the schema DEFAULT and every call
// failed with "CHECK constraint failed: priority >= 1 AND priority <= 10".
func TestMCP_FleetAdd(t *testing.T) {
	m := newMCPTestServer(t)

	_, resp := m.call(t, map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params": map[string]interface{}{
			"name": "fleet_add",
			"arguments": map[string]interface{}{
				"name":    "newproj",
				"repo":    "https://example.com/newproj",
				"workdir": "/tmp/newproj",
			},
		},
	})
	if resp.Error != nil {
		t.Fatalf("fleet_add failed: %+v", resp.Error)
	}
	text := extractText(t, resp.Result)
	if !strings.Contains(text, `"status":"added"`) {
		t.Errorf("expected added status, got: %s", text)
	}

	// The row must exist with the default priority (and weight).
	got, err := database.GetProject(context.Background(), m.db, "newproj")
	if err != nil {
		t.Fatalf("GetProject: %v", err)
	}
	if got.Priority != 5 {
		t.Errorf("priority = %d, want default 5", got.Priority)
	}
	if got.Weight != 10 {
		t.Errorf("weight = %d, want default 10", got.Weight)
	}
}

// TestMCP_FleetAdd_WeightValidation verifies out-of-range weight values are
// rejected with a friendly validation error before reaching the database —
// never a raw sqlite CHECK constraint string.
func TestMCP_FleetAdd_WeightValidation(t *testing.T) {
	m := newMCPTestServer(t)

	for _, tc := range []struct {
		weight int
	}{
		{weight: 0},
		{weight: 150},
	} {
		_, resp := m.call(t, map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      1,
			"method":  "tools/call",
			"params": map[string]interface{}{
				"name": "fleet_add",
				"arguments": map[string]interface{}{
					"name":    "badweight",
					"repo":    "https://example.com/badweight",
					"workdir": "/tmp/badweight",
					"weight":  tc.weight,
				},
			},
		})
		if resp.Error == nil {
			t.Fatalf("expected error for weight=%d, got nil", tc.weight)
		}
		if !strings.Contains(resp.Error.Message, "weight must be 1-100") {
			t.Errorf("weight=%d: error = %q, want friendly range error", tc.weight, resp.Error.Message)
		}
		if strings.Contains(resp.Error.Message, "CHECK constraint") {
			t.Errorf("weight=%d: raw CHECK constraint surfaced: %q", tc.weight, resp.Error.Message)
		}
	}

	// No row may have been created by the rejected calls.
	if _, err := database.GetProject(context.Background(), m.db, "badweight"); err == nil {
		t.Fatal("project was created despite weight validation rejection")
	}
}

// TestMCP_FleetAdd_DuplicateName verifies a duplicate name surfaces as a
// friendly "already exists" error, not a raw sqlite UNIQUE constraint string.
func TestMCP_FleetAdd_DuplicateName(t *testing.T) {
	m := newMCPTestServer(t)
	mustCreateMCPProject(t, m.db, "alpha")

	_, resp := m.call(t, map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params": map[string]interface{}{
			"name": "fleet_add",
			"arguments": map[string]interface{}{
				"name":    "alpha",
				"repo":    "https://example.com/alpha2",
				"workdir": "/tmp/alpha2",
			},
		},
	})
	if resp.Error == nil {
		t.Fatal("expected error for duplicate name")
		return
	}
	// CreateProject's case-insensitive duplicate check fires before the
	// INSERT, so the message is the readable "already registered by enabled
	// project" form (same text the REST handler returns verbatim) rather
	// than a raw UNIQUE constraint string. Either way it must be friendly.
	if !strings.Contains(resp.Error.Message, "already") {
		t.Errorf("error = %q, want friendly duplicate message", resp.Error.Message)
	}
	if strings.Contains(resp.Error.Message, "UNIQUE constraint") {
		t.Errorf("raw UNIQUE constraint surfaced: %q", resp.Error.Message)
	}
}

func TestMCP_FleetAdd_MissingFields(t *testing.T) {
	m := newMCPTestServer(t)
	_, resp := m.call(t, map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params": map[string]interface{}{
			"name":      "fleet_add",
			"arguments": map[string]interface{}{"name": "incomplete"},
		},
	})
	if resp.Error == nil {
		t.Fatal("expected error for missing repo/workdir")
		return
	}
}

func TestMCP_FleetTicks(t *testing.T) {
	m := newMCPTestServer(t)
	mustCreateMCPProject(t, m.db, "alpha")

	// No ticks yet.
	_, resp := m.call(t, map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params": map[string]interface{}{
			"name":      "fleet_ticks",
			"arguments": map[string]interface{}{},
		},
	})
	if resp.Error != nil {
		t.Fatalf("error: %+v", resp.Error)
	}
	text := extractText(t, resp.Result)
	if !strings.Contains(text, `"count":0`) {
		t.Errorf("expected count:0, got: %s", text)
	}
}

func TestMCP_FleetTicks_SnakeCaseWire(t *testing.T) {
	m := newMCPTestServer(t)
	mustCreateMCPProject(t, m.db, "alpha")

	// Insert a tick row directly (created_at is NOT NULL).
	if _, err := m.db.Exec(`INSERT INTO ticks (id, project_name, status, spawned_at, created_at)
		VALUES ('dogfood-016-tick', 'alpha', 'running', '2026-08-25T10:00:00Z', '2026-08-25T10:00:00Z')`); err != nil {
		t.Fatalf("insert tick: %v", err)
	}

	_, resp := m.call(t, map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params": map[string]interface{}{
			"name":      "fleet_ticks",
			"arguments": map[string]interface{}{},
		},
	})
	if resp.Error != nil {
		t.Fatalf("error: %+v", resp.Error)
	}
	text := extractText(t, resp.Result)

	// S06 conformance: row keys must be snake_case (REST dialect).
	for _, want := range []string{`"id":`, `"project_name":`, `"status":`, `"spawned_at":`, `"files_changed":`} {
		if !strings.Contains(text, want) {
			t.Errorf("fleet_ticks missing %s; got: %s", want, text)
		}
	}
	// PascalCase keys must be gone.
	for _, bad := range []string{`"ID":`, `"ProjectName"`, `"SpawnedAt"`, `"FilesChanged"`} {
		if strings.Contains(text, bad) {
			t.Errorf("fleet_ticks still emits PascalCase key %s; got: %s", bad, text)
		}
	}
	// The tick row itself must be present.
	if !strings.Contains(text, "dogfood-016-tick") {
		t.Errorf("expected inserted tick in result, got: %s", text)
	}

	// fleet_project_detail's recent_ticks must be snake_case too.
	_, resp = m.call(t, map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      2,
		"method":  "tools/call",
		"params": map[string]interface{}{
			"name":      "fleet_project_detail",
			"arguments": map[string]interface{}{"name": "alpha"},
		},
	})
	if resp.Error != nil {
		t.Fatalf("error: %+v", resp.Error)
	}
	text = extractText(t, resp.Result)
	if !strings.Contains(text, `"recent_ticks"`) {
		t.Fatalf("expected recent_ticks in result, got: %s", text)
	}
	if !strings.Contains(text, `"id":`) {
		t.Errorf("recent_ticks missing snake_case \"id\":; got: %s", text)
	}
	for _, bad := range []string{`"ID":`, `"SpawnedAt"`, `"CompletedAt"`} {
		if strings.Contains(text, bad) {
			t.Errorf("fleet_project_detail recent_ticks still emits PascalCase key %s; got: %s", bad, text)
		}
	}
}

func TestMCP_FleetEvaluate(t *testing.T) {
	m := newMCPTestServer(t)
	_, resp := m.call(t, map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params": map[string]interface{}{
			"name":      "fleet_evaluate",
			"arguments": map[string]interface{}{},
		},
	})
	if resp.Error != nil {
		t.Fatalf("error: %+v", resp.Error)
	}
	text := extractText(t, resp.Result)
	if !strings.Contains(text, "evaluation triggered") {
		t.Errorf("expected 'evaluation triggered', got: %s", text)
	}
}

// TestMCP_FleetPauseResumeScheduler verifies the scheduler-pause tools accept calls.
// We don't chain pause+resume in one test because the channel is size 1 and the loop
// isn't running to drain it; that interaction is covered by TestLoop_PauseResume in
// the scheduler package. Here we just confirm the tool calls don't error.
func TestMCP_FleetPauseScheduler(t *testing.T) {
	m := newMCPTestServer(t)
	_, resp := m.call(t, map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params": map[string]interface{}{
			"name":      "fleet_pause_scheduler",
			"arguments": map[string]interface{}{},
		},
	})
	if resp.Error != nil {
		t.Fatalf("error: %+v", resp.Error)
	}
}

// TestMCP_FleetResumeScheduler calls resume on a freshly-created loop (channel empty).
func TestMCP_FleetResumeScheduler(t *testing.T) {
	m := newMCPTestServer(t)
	_, resp := m.call(t, map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params": map[string]interface{}{
			"name":      "fleet_resume_scheduler",
			"arguments": map[string]interface{}{},
		},
	})
	if resp.Error != nil {
		t.Fatalf("error: %+v", resp.Error)
	}
}

func TestMCP_UnknownTool(t *testing.T) {
	m := newMCPTestServer(t)
	_, resp := m.call(t, map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params": map[string]interface{}{
			"name":      "fleet_nonexistent",
			"arguments": map[string]interface{}{},
		},
	})
	if resp.Error == nil {
		t.Fatal("expected error for unknown tool")
		return
	}
	if !strings.Contains(resp.Error.Message, "unknown tool") {
		t.Errorf("error = %q, want 'unknown tool'", resp.Error.Message)
	}
}

// TestMCP_InvalidParams verifies tools/call with malformed params returns -32602.
func TestMCP_InvalidParams(t *testing.T) {
	m := newMCPTestServer(t)
	resp, err := http.Post(m.ts.URL+"/mcp", "application/json",
		strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":"not-an-object"}`))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var parsed mcpserver.MCPResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if parsed.Error == nil {
		t.Fatal("expected error envelope")
		return
	}
}

// TestMCP_PreservesID verifies the response echoes the client's id field.
func TestMCP_PreservesID(t *testing.T) {
	m := newMCPTestServer(t)
	id := 9999
	_, resp := m.call(t, map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  "initialize",
	})
	if resp.ID == nil {
		t.Fatal("response id missing")
		return
	}
	if *resp.ID != id {
		t.Errorf("response id = %d, want %d", *resp.ID, id)
	}
}

// extractText pulls the first text field from a tools/call content array.
func extractText(t *testing.T, result interface{}) string {
	t.Helper()
	r, ok := result.(map[string]interface{})
	if !ok {
		t.Fatalf("result not object: %T", result)
	}
	content, ok := r["content"].([]interface{})
	if !ok || len(content) == 0 {
		t.Fatalf("content not array: %v", r)
	}
	first, ok := content[0].(map[string]interface{})
	if !ok {
		t.Fatalf("content[0] not object: %T", content[0])
	}
	text, ok := first["text"].(string)
	if !ok {
		t.Fatalf("text not string: %T", first["text"])
	}
	return text
}
