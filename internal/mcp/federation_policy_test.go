package mcp_test

// REMOTE-013 (docs/federation-query-spec.md §4) — the policy refusal seen
// THROUGH the MCP transport (the second transport the acceptance list
// names; the shared entry point itself is proven in the api package's
// REMOTE-013 battery). Because the daemon wires ONE entry point
// (apiServer.FederationQueryHandler) into the MCP server, a deny-all or
// scoped policy on the api.Server must surface here as the named
// op_not_allowed envelope — and the refusal must still write its §4 audit
// row (the refusal is the interesting event), naming the MCP caller.

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coding-hermes/scheduler/internal/api"
	"github.com/coding-hermes/scheduler/internal/database"
	mcpserver "github.com/coding-hermes/scheduler/internal/mcp"
	"github.com/coding-hermes/scheduler/internal/scheduler"
)

// newFedMCPPolicyStack is the policy-scoped variant of newFedMCPStack: the
// same real daemon topology (ONE api.Server + ONE mcp.Server, the entry
// point wired exactly as main.go does), with the given read grants armed
// at construction. nil grants = the deny-all policy (fail-closed).
func newFedMCPPolicyStack(t *testing.T, grants []api.FederationReadGrant) (*mcpTestServer, *api.Server) {
	t.Helper()
	db, err := database.InitDB(filepath.Join(t.TempDir(), "scheduler.db"))
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	loop := scheduler.NewLoop(db, time.Minute, time.Hour, 10, 0, 5)
	loop.SetNoExecFallback(true)
	apiSrv := api.NewServer(db, loop)
	apiSrv.SetFederationReadPolicy(api.ResolveFederationReadPolicy(grants))
	mcpSrv := mcpserver.NewServer(db, loop)
	mcpSrv.SetFederationQueryHandler(apiSrv.FederationQueryHandler)
	ts := httptest.NewServer(mcpSrv.Handler())
	t.Cleanup(ts.Close)
	m := &mcpTestServer{db: db, loop: loop, server: mcpSrv, ts: ts}
	mustCreateFedProject(t, db, "alpha010")
	mustCreateFedProject(t, db, "beta010")
	return m, apiSrv
}

// TestREMOTE013_MCPPolicyRefusalNamed proves the read policy through the
// MCP transport: with NO policy armed (deny-all), a fed tool call for a
// real catalogue op answers status="error" code="op_not_allowed" — a named
// envelope, never silence, never a locally-invented answer (the MCP layer
// is a thin adapter; the refusal is the shared entry point's).
func TestREMOTE013_MCPPolicyRefusalNamed(t *testing.T) {
	m, _ := newFedMCPPolicyStack(t, nil) // deny-all
	text := fedMCPCall(t, m, 1, "fed_queue_get", map[string]interface{}{
		"federation_corr_id": "pol-mcp-deny-1",
	})
	var env struct {
		Status string `json:"status"`
		Error  *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(text), &env); err != nil {
		t.Fatalf("MCP answer not a §2.2 envelope: %v (%s)", err, text)
	}
	if env.Status != "error" || env.Error == nil {
		t.Fatalf("deny-all fed tool must answer status=error: %s", text)
	}
	if env.Error.Code != "op_not_allowed" {
		t.Errorf("refusal code = %q, want op_not_allowed (the spec §4 vocabulary, named not silent): %s", env.Error.Code, text)
	}

	// The refusal still wrote its §4 audit row, naming the MCP caller.
	rs, err := m.db.QueryContext(context.Background(),
		`SELECT message FROM events WHERE component='api.federation'`)
	if err != nil {
		t.Fatalf("query events: %v", err)
	}
	defer rs.Close()
	found := false
	for rs.Next() {
		var msg string
		if err := rs.Scan(&msg); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if strings.Contains(msg, "op=queue.get") &&
			strings.Contains(msg, "pol-mcp-deny-1") &&
			strings.Contains(msg, "caller=mcp:auth-off") &&
			strings.Contains(msg, "op_not_allowed") {
			found = true
		}
	}
	if err := rs.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	if !found {
		t.Error("no api.federation audit row for the REFUSED MCP read (§4: a refused query is audited too)")
	}
}

// TestREMOTE013_MCPScopeRefusalAndAllow proves per-caller scoping end to
// end on the MCP path: arming a policy that grants the mcp:auth-off caller
// only fleet.status lets fed_fleet_status through while fed_queue_get
// stays refused — same server, one policy, two tools.
func TestREMOTE013_MCPScopeRefusalAndAllow(t *testing.T) {
	m, apiSrv := newFedMCPStack(t)
	apiSrv.SetFederationReadPolicy(api.ResolveFederationReadPolicy([]api.FederationReadGrant{
		{Caller: api.FederationAllowAll, Ops: []string{"fleet.status"}},
	}))

	allowed := fedMCPCall(t, m, 1, "fed_fleet_status", map[string]interface{}{
		"federation_corr_id": "pol-mcp-scope-1",
	})
	var envOK struct {
		Status string          `json:"status"`
		Error  json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal([]byte(allowed), &envOK); err != nil {
		t.Fatalf("allowed tool not a §2.2 envelope: %v (%s)", err, allowed)
	}
	if envOK.Status != "ok" && envOK.Status != "stale" {
		t.Errorf("allowed tool status = %q, want ok|stale: %s", envOK.Status, allowed)
	}

	refused := fedMCPCall(t, m, 2, "fed_queue_get", map[string]interface{}{
		"federation_corr_id": "pol-mcp-scope-2",
	})
	var envRef struct {
		Status string `json:"status"`
		Error  *struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(refused), &envRef); err != nil {
		t.Fatalf("refused tool not a §2.2 envelope: %v (%s)", err, refused)
	}
	if envRef.Status != "error" || envRef.Error == nil || envRef.Error.Code != "op_not_allowed" {
		t.Errorf("scoped-out tool = %+v, want error/op_not_allowed (per-peer scoping): %s", envRef.Error, refused)
	}
}
