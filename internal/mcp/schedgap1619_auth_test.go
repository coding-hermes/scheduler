package mcp_test

// SCHED-GAP-1619 — operator-authentication gate on MCP mutating tools/call.
//
// The MCP surface must answer the same auth contract the REST mutation gate
// (SCHED-GAP-1602, internal/api/auth_gate.go) answers: mutating tools/call
// require the resolved operator credential (HTTP headers only — credentials
// in tool arguments are never accepted), reads stay open, auth-off fails
// closed, and every decision — allowed or refused — writes an mcp.auth
// events row. The parity classification test pins the mutating/read tool
// partition so a newly added mutator cannot silently bypass the gate.
//
// RED proof: every refusal subtest here fails against the pre-1619 server,
// which invoked invokeTool unauthenticated (fleet_set_weight mutated state
// with no credential); the audit subtests fail because no mcp.auth rows
// existed. The authenticated-success and read-open subtests pass on both
// sides — they pin the preservation contract, not the fix.

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coding-hermes/scheduler/internal/api"
	"github.com/coding-hermes/scheduler/internal/database"
	mcpserver "github.com/coding-hermes/scheduler/internal/mcp"
	"github.com/coding-hermes/scheduler/internal/scheduler"
)

// mcpOperatorToken is the test credential armed on the shared
// newMCPTestServer stack (server_test.go). Any change here must keep the
// header shapes in the cred helpers below in sync.
const mcpOperatorToken = "mcp-operator-test-token-1619"

// newMCPAuthedServer is a fresh stack with a token-mode operator credential.
func newMCPAuthedServer(t *testing.T) *mcpTestServer {
	t.Helper()
	db, err := database.InitDB(":memory:")
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	loop := scheduler.NewLoop(db, time.Minute, time.Hour, 10, 0, 5)
	srv := mcpserver.NewServer(db, loop)
	srv.SetOperatorAuth(api.ResolveAuthConfig(mcpOperatorToken, "", ""))
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return &mcpTestServer{db: db, loop: loop, server: srv, ts: ts}
}

// newMCPBareServer builds a stack with NO SetOperatorAuth call — the
// zero-value Server exactly as every pre-1619 constructor produced it. This
// is the fail-closed arm's subject: mutations must be refused even when a
// caller presents a credential, reads stay open.
func newMCPBareServer(t *testing.T) *mcpTestServer {
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

// credHeader selects the credential a raw request carries. "" = anonymous.
func credHeader(cred string) func(*http.Request) {
	return func(r *http.Request) {
		switch cred {
		case "":
			// no header — the unauthenticated caller
		case "wrong":
			r.Header.Set("X-Operator-Token", "not-the-token")
		default:
			r.Header.Set("X-Operator-Token", cred)
		}
	}
}

// callRaw posts one JSON-RPC request with an explicit credential header and
// returns the HTTP status plus parsed envelope.
func (m *mcpTestServer) callRaw(t *testing.T, req map[string]interface{}, cred string) (int, mcpserver.MCPResponse) {
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
	credHeader(cred)(httpReq)
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

// callMutationTool drives one mutating tools/call with the given credential
// and returns the envelope.
func callMutationTool(t *testing.T, m *mcpTestServer, name string, args map[string]interface{}, cred string) mcpserver.MCPResponse {
	t.Helper()
	_, resp := m.callRaw(t, map[string]interface{}{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]interface{}{"name": name, "arguments": args},
	}, cred)
	return resp
}

// projectWeight reads a project's weight directly from the DB — the state
// the fleet_set_weight probes must leave untouched when refused.
func projectWeight(t *testing.T, m *mcpTestServer, name string) int {
	t.Helper()
	var w int
	if err := m.db.QueryRow(`SELECT COALESCE(weight,0) FROM projects WHERE name=?`, name).Scan(&w); err != nil {
		t.Fatalf("read weight %s: %v", name, err)
	}
	return w
}

// mcpAuthEvents reads every mcp.auth event row, oldest first.
type mcpAuthEvent struct {
	Severity  string
	Component string
	Message   string
	Details   string
}

func mcpAuthEvents(t *testing.T, m *mcpTestServer) []mcpAuthEvent {
	t.Helper()
	rows, err := m.db.Query(`SELECT severity, component, message, COALESCE(details,'') FROM events WHERE component='mcp.auth' ORDER BY id`)
	if err != nil {
		t.Fatalf("query mcp.auth events: %v", err)
	}
	defer rows.Close()
	var out []mcpAuthEvent
	for rows.Next() {
		var e mcpAuthEvent
		if err := rows.Scan(&e.Severity, &e.Component, &e.Message, &e.Details); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	return out
}

func mustCreateMCPAuthProject(t *testing.T, m *mcpTestServer, name string) {
	t.Helper()
	if err := database.CreateProject(context.Background(), m.db, &database.Project{
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

// ── refusals ────────────────────────────────────────────────────────────────

// TestMCPAuth_UnauthenticatedMutationRefused: a mutating tools/call with NO
// credential is refused before invokeTool and leaves state untouched.
func TestMCPAuth_UnauthenticatedMutationRefused(t *testing.T) {
	m := newMCPAuthedServer(t)
	mustCreateMCPAuthProject(t, m, "auth-proj")

	resp := callMutationTool(t, m, "fleet_set_weight", map[string]interface{}{"name": "auth-proj", "weight": 99}, "")
	if resp.Error == nil {
		t.Fatalf("expected JSON-RPC error for unauthenticated mutation, got success: %+v", resp)
	}
	if resp.Result != nil {
		t.Errorf("refused call must not carry a result: %+v", resp)
	}
	if got := projectWeight(t, m, "auth-proj"); got != 10 {
		t.Errorf("state mutated despite refusal: weight = %d, want 10", got)
	}
	// The refusal message must be actionable, and must not echo the
	// configured token.
	if !strings.Contains(resp.Error.Message, "operator credential") {
		t.Errorf("refusal message %q lacks credential guidance", resp.Error.Message)
	}
	if strings.Contains(resp.Error.Message, mcpOperatorToken) {
		t.Error("refusal message leaks the operator token")
	}
}

// TestMCPAuth_BadCredentialRefused: a wrong credential is refused and the
// state is untouched.
func TestMCPAuth_BadCredentialRefused(t *testing.T) {
	m := newMCPAuthedServer(t)
	mustCreateMCPAuthProject(t, m, "auth-proj")

	resp := callMutationTool(t, m, "fleet_set_weight", map[string]interface{}{"name": "auth-proj", "weight": 77}, "wrong")
	if resp.Error == nil {
		t.Fatalf("expected JSON-RPC error for bad credential, got success: %+v", resp)
	}
	if got := projectWeight(t, m, "auth-proj"); got != 10 {
		t.Errorf("state mutated despite bad credential: weight = %d, want 10", got)
	}
	if !strings.Contains(resp.Error.Message, "invalid operator credential") {
		t.Errorf("refusal message %q should name the bad-credential arm", resp.Error.Message)
	}
	if strings.Contains(resp.Error.Message, mcpOperatorToken) {
		t.Error("refusal message leaks the operator token")
	}
}

// TestMCPAuth_AuthOffFailClosed: a server with NO SetOperatorAuth refuses
// every mutating tool even when a caller presents a credential — the same
// fail-closed arm the REST gate has.
func TestMCPAuth_AuthOffFailClosed(t *testing.T) {
	m := newMCPBareServer(t)
	mustCreateMCPAuthProject(t, m, "auth-proj")

	for _, cred := range []string{"", mcpOperatorToken} {
		resp := callMutationTool(t, m, "fleet_set_weight", map[string]interface{}{"name": "auth-proj", "weight": 50}, cred)
		if resp.Error == nil {
			t.Fatalf("auth-off: expected refusal (cred=%q), got success: %+v", cred, resp)
		}
		if got := projectWeight(t, m, "auth-proj"); got != 10 {
			t.Fatalf("auth-off: state mutated (cred=%q): weight = %d, want 10", cred, got)
		}
		if !strings.Contains(resp.Error.Message, "mutations disabled") {
			t.Errorf("auth-off refusal %q should name the misconfiguration arm", resp.Error.Message)
		}
	}
}

// TestMCPAuth_AuthOffReadsStayOpen: the bare server keeps reads usable — the
// observers that poll the surface must survive the fail-closed arm.
func TestMCPAuth_AuthOffReadsStayOpen(t *testing.T) {
	m := newMCPBareServer(t)
	mustCreateMCPAuthProject(t, m, "auth-proj")

	resp := callMutationTool(t, m, "fleet_status", map[string]interface{}{}, "")
	if resp.Error != nil {
		t.Fatalf("read refused on bare server: %+v", resp.Error)
	}
	if text := extractText(t, resp.Result); !strings.Contains(text, "auth-proj") && !strings.Contains(text, "total_projects") {
		t.Errorf("fleet_status payload unexpected: %s", text)
	}
}

// ── allowed paths ───────────────────────────────────────────────────────────

// TestMCPAuth_AuthenticatedMutationSucceeds: with the right credential a
// mutating tools/call executes normally and the success shape is unchanged.
func TestMCPAuth_AuthenticatedMutationSucceeds(t *testing.T) {
	m := newMCPAuthedServer(t)
	mustCreateMCPAuthProject(t, m, "auth-proj")

	resp := callMutationTool(t, m, "fleet_set_weight", map[string]interface{}{"name": "auth-proj", "weight": 42}, mcpOperatorToken)
	if resp.Error != nil {
		t.Fatalf("authenticated mutation refused: %+v", resp.Error)
	}
	text := extractText(t, resp.Result)
	if !strings.Contains(text, `"status":"updated"`) || !strings.Contains(text, `"weight":"42"`) {
		t.Errorf("success payload changed: %s", text)
	}
	if got := projectWeight(t, m, "auth-proj"); got != 42 {
		t.Errorf("weight not applied: got %d, want 42", got)
	}
}

// TestMCPAuth_ReadsOpenWithoutCredential: initialize, tools/list and
// read-only tools stay usable with no credential at all.
func TestMCPAuth_ReadsOpenWithoutCredential(t *testing.T) {
	m := newMCPAuthedServer(t)
	mustCreateMCPAuthProject(t, m, "auth-proj")

	// initialize
	if _, resp := m.callRaw(t, map[string]interface{}{"jsonrpc": "2.0", "id": 1, "method": "initialize"}, ""); resp.Error != nil {
		t.Errorf("initialize refused: %+v", resp.Error)
	}
	// tools/list
	_, resp := m.callRaw(t, map[string]interface{}{"jsonrpc": "2.0", "id": 2, "method": "tools/list"}, "")
	if resp.Error != nil {
		t.Fatalf("tools/list refused: %+v", resp.Error)
	}
	result, _ := resp.Result.(map[string]interface{})
	if tools, ok := result["tools"].([]interface{}); !ok || len(tools) == 0 {
		t.Fatalf("tools/list empty: %+v", resp.Result)
	}
	// read-only tools
	for _, tool := range []string{"fleet_status", "fleet_projects", "fleet_ticks", "namespaces_list", "config_get", "queue_get", "metrics_get"} {
		r := callMutationTool(t, m, tool, map[string]interface{}{}, "")
		if r.Error != nil {
			t.Errorf("read-only tool %s refused without credential: %+v", tool, r.Error)
		}
	}
}

// ── audit trail ─────────────────────────────────────────────────────────────

// TestMCPAuth_AuditRowsForRefusalsAndAllowance: every mutating decision —
// allowed, unauthenticated, bad-credential, auth-off — writes an mcp.auth
// events row with the outcome, tool target, mode and identity, and no row
// ever carries the credential material.
func TestMCPAuth_AuditRowsForRefusalsAndAllowance(t *testing.T) {
	m := newMCPAuthedServer(t)
	mustCreateMCPAuthProject(t, m, "auth-proj")

	// 1. refused-unauthenticated
	callMutationTool(t, m, "fleet_set_weight", map[string]interface{}{"name": "auth-proj", "weight": 99}, "")
	// 2. refused-bad-credential
	callMutationTool(t, m, "fleet_set_weight", map[string]interface{}{"name": "auth-proj", "weight": 98}, "wrong")
	// 3. allowed
	callMutationTool(t, m, "fleet_set_weight", map[string]interface{}{"name": "auth-proj", "weight": 42}, mcpOperatorToken)

	rows := mcpAuthEvents(t, m)
	if len(rows) != 3 {
		t.Fatalf("mcp.auth rows = %d, want 3: %+v", len(rows), rows)
	}
	want := []struct {
		severity, outcome, identity string
	}{
		{"MEDIUM", "refused-unauthenticated", "anonymous"},
		{"MEDIUM", "refused-bad-credential", "invalid"},
		{"INFO", "allowed", "operator:token"},
	}
	for i, w := range want {
		row := rows[i]
		if row.Severity != w.severity {
			t.Errorf("row %d severity = %s, want %s", i, row.Severity, w.severity)
		}
		if !strings.Contains(row.Message, w.outcome) {
			t.Errorf("row %d message %q lacks outcome %q", i, row.Message, w.outcome)
		}
		if !strings.Contains(row.Message, "target=fleet_set_weight") {
			t.Errorf("row %d message %q lacks target", i, row.Message)
		}
		if !strings.Contains(row.Message, "identity="+w.identity) {
			t.Errorf("row %d message %q lacks identity %q", i, row.Message, w.identity)
		}
		if !strings.Contains(row.Details, `"outcome":"`+w.outcome+`"`) ||
			!strings.Contains(row.Details, `"tool":"fleet_set_weight"`) ||
			!strings.Contains(row.Details, `"identity":"`+w.identity+`"`) {
			t.Errorf("row %d details %q incomplete", i, row.Details)
		}
		if !strings.Contains(row.Details, `"mode":"token"`) {
			t.Errorf("row %d details %q lacks mode", i, row.Details)
		}
		if strings.Contains(row.Message, mcpOperatorToken) || strings.Contains(row.Details, mcpOperatorToken) {
			t.Errorf("row %d leaks the operator token", i)
		}
	}
}

// TestMCPAuth_AuthOffAuditRow: the auth-off arm writes refused-no-credential
// rows with mode "off" — the same outcome vocabulary the REST gate uses.
func TestMCPAuth_AuthOffAuditRow(t *testing.T) {
	m := newMCPBareServer(t)
	mustCreateMCPAuthProject(t, m, "auth-proj")

	callMutationTool(t, m, "fleet_set_weight", map[string]interface{}{"name": "auth-proj", "weight": 99}, mcpOperatorToken)

	rows := mcpAuthEvents(t, m)
	if len(rows) != 1 {
		t.Fatalf("mcp.auth rows = %d, want 1: %+v", len(rows), rows)
	}
	row := rows[0]
	if row.Severity != "MEDIUM" {
		t.Errorf("severity = %s, want MEDIUM", row.Severity)
	}
	if !strings.Contains(row.Details, `"outcome":"refused-no-credential"`) {
		t.Errorf("details %q lacks refused-no-credential", row.Details)
	}
	if !strings.Contains(row.Details, `"mode":"off"`) {
		t.Errorf("details %q lacks mode off", row.Details)
	}
}

// ── classification parity ───────────────────────────────────────────────────

// TestMCPAuth_MutationClassificationCoversRegistry pins the mutating/read
// partition over the LIVE registry: no overlap, no gap. A tool added to the
// registry but classified neither way fails here, so a new mutator cannot
// silently bypass the gate.
func TestMCPAuth_MutationClassificationCoversRegistry(t *testing.T) {
	m := newMCPAuthedServer(t)

	_, resp := m.callRaw(t, map[string]interface{}{"jsonrpc": "2.0", "id": 1, "method": "tools/list"}, "")
	if resp.Error != nil {
		t.Fatalf("tools/list: %+v", resp.Error)
	}
	result, _ := resp.Result.(map[string]interface{})
	rawTools, _ := result["tools"].([]interface{})
	live := map[string]bool{}
	for _, tool := range rawTools {
		td, _ := tool.(map[string]interface{})
		name, _ := td["name"].(string)
		live[name] = true
	}
	if len(live) == 0 {
		t.Fatal("empty live registry")
	}

	mutating := mcpserver.MutatingToolNames()
	readOnlyNames := mcpserver.ReadOnlyToolNames()
	mutatingSet := map[string]bool{}
	for _, name := range mutating {
		mutatingSet[name] = true
	}
	readOnly := map[string]bool{}
	for _, name := range readOnlyNames {
		readOnly[name] = true
	}

	// partition: every live tool classified exactly once.
	for _, name := range mutating {
		if readOnly[name] {
			t.Errorf("tool %q classified both mutating and read-only", name)
		}
	}
	seen := map[string]bool{}
	for _, name := range mutating {
		seen[name] = true
	}
	for _, name := range readOnlyNames {
		if seen[name] {
			t.Errorf("tool %q classified twice", name)
		}
		seen[name] = true
	}
	for name := range live {
		if !seen[name] {
			t.Errorf("tool %q is in the live registry but classified in NEITHER set — it would run ungated (or the classification lists are stale)", name)
		}
	}
	for name := range seen {
		if !live[name] {
			t.Errorf("tool %q is classified but not in the live registry (stale classification entry)", name)
		}
	}

	// external cross-check: every non-GET REST operation the parity contract
	// maps to MCP tools must be covered by MUTATING tools only — reads are
	// GET-shaped, so a mutator hiding in the read list fails here.
	for op, tools := range apiToolCoverage {
		if strings.HasPrefix(op, "GET ") {
			continue
		}
		for _, name := range tools {
			if !mutatingSet[name] {
				t.Errorf("REST mutation %s is covered by tool %q which MCP classifies read-only", op, name)
			}
		}
	}
}
