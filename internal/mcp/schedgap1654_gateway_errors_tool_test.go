package mcp_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coding-hermes/scheduler/internal/scheduler"
)

// SCHED-GAP-1654 — the gateway_errors MCP tool: the agent-facing half of the
// gateway-availability surface. The REST handler
// (internal/api/gateway_errors_test.go) already pins the wire contract; this
// pins that the TOOL serves the SAME scheduler.BuildGatewayErrorsReport over
// the SAME events — the CTL-003 pairing — and never fabricates an answer.
//
// The events are seeded through the real producer (a Spawner driven against a
// mock gateway serving 503), not by hand-inserted rows, so a change to the
// spawn path's component/class vocabulary fails here just as it fails the
// REST half.

// TestSCHEDGAP1654_MCPGatewayErrors_CountsSpawnLosses drives a real Spawner
// against a draining mock gateway, then reads the loss back through the MCP
// tools/call surface: total 1, the class named, the lane named.
func TestSCHEDGAP1654_MCPGatewayErrors_CountsSpawnLosses(t *testing.T) {
	m := newMCPTestServer(t)

	project := "gap1654-mcp-503"
	const tickID = "gap1654-mcp-tick-1"
	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := m.db.Exec(
		`INSERT INTO projects (name, enabled, repo_url, workdir, created_at, updated_at) VALUES (?, 1, '', ?, ?, ?)`,
		project, t.TempDir(), now, now); err != nil {
		t.Fatalf("insert project: %v", err)
	}
	if _, err := m.db.Exec(
		`INSERT INTO ticks (id, project_name, status, spawned_at, created_at, pid) VALUES (?,?, 'running', ?,?, 0)`,
		tickID, project, now, now); err != nil {
		t.Fatalf("insert running tick: %v", err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]string{"type": "gateway_draining", "message": "Gateway is draining"},
		})
	}))
	t.Cleanup(srv.Close)

	spawner := scheduler.NewSpawner(m.db, 4)
	spawner.SetGatewayClient(scheduler.NewGatewayClient(srv.URL, "***", 5*time.Second))
	spawner.SetNoExecFallback(true)
	spawner.SetGatewayTransientRetries(0)
	spawner.SetEventLogger(scheduler.NewEventLogger(m.db))
	if _, err := spawner.Spawn(scheduler.PackedProject{Name: project, Workdir: t.TempDir()}, tickID); err == nil {
		t.Fatal("Spawn err = nil on 503, want the tick to be dropped")
	}

	text, callErr := callTool(t, m, 1, "gateway_errors", map[string]interface{}{})
	if callErr != nil {
		t.Fatalf("gateway_errors tool: %+v", callErr)
	}

	var report struct {
		Total     int64            `json:"total"`
		ByClass   map[string]int64 `json:"by_class"`
		ByProject []struct {
			Project string           `json:"project"`
			Total   int64            `json:"total"`
			ByClass map[string]int64 `json:"by_class"`
		} `json:"by_project"`
	}
	if err := json.Unmarshal([]byte(text), &report); err != nil {
		t.Fatalf("decode tool output %s: %v", text, err)
	}
	if report.Total != 1 {
		t.Errorf("total = %d, want 1 (output: %s)", report.Total, text)
	}
	if got := report.ByClass[scheduler.GatewayErrClassUnavailable503]; got != 1 {
		t.Errorf("by_class[unavailable_503] = %d, want 1", got)
	}
	for _, class := range []string{scheduler.GatewayErrClassRateLimited429, scheduler.GatewayErrClassConnRefused} {
		if got, ok := report.ByClass[class]; !ok || got != 0 {
			t.Errorf("by_class[%s] = %d (present=%t), want 0 and present — every class must appear even at zero", class, got, ok)
		}
	}
	if len(report.ByProject) != 1 || report.ByProject[0].Project != project || report.ByProject[0].Total != 1 {
		t.Errorf("by_project = %+v, want exactly %s with total 1", report.ByProject, project)
	}
}

// TestSCHEDGAP1654_MCPGatewayErrors_EmptyWindowIsHonest — an empty window is
// an honest zero on the tool surface too: total 0, every class present at 0,
// by_project an empty array (never null), no error.
func TestSCHEDGAP1654_MCPGatewayErrors_EmptyWindowIsHonest(t *testing.T) {
	m := newMCPTestServer(t)

	text, callErr := callTool(t, m, 1, "gateway_errors", map[string]interface{}{})
	if callErr != nil {
		t.Fatalf("gateway_errors tool: %+v", callErr)
	}
	if !strings.Contains(text, `"total": 0`) {
		t.Errorf("tool output %s does not read total 0 on an empty window", text)
	}
	if !strings.Contains(text, `"by_project": []`) {
		t.Errorf("tool output %s does not serialize by_project as an empty array", text)
	}
	for _, class := range scheduler.GatewayErrorClassOrder {
		if !strings.Contains(text, "\""+class+"\"") {
			t.Errorf("tool output %s is missing class %q — every class must be present even at 0", text, class)
		}
	}
}
