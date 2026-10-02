package mcp_test

// REMOTE-010 (docs/federation-query-spec.md §3, the "MCP" row): the MCP
// federation-query tool tests. The acceptance cells follow the §6 doctrine
// — the internal query entry point (api.Server.FederationQueryHandler,
// REMOTE-008's federationOps + the shared replay window) is the ORACLE;
// the MCP tool answer is diffed against it op-by-op. The test stack wires
// the SAME entry point the daemon's main.go installs
// (mcpServer.SetFederationQueryHandler(apiServer.FederationQueryHandler)),
// so the diff proves the wiring, not a parallel implementation. A refused
// answer must carry the SAME named error the internal entry point produced
// (one error vocabulary across transports), and the unwired arm proves the
// adapter fails closed instead of ever computing an answer itself (spec
// §3: "an adapter that computes an answer itself is a bug").

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coding-hermes/scheduler/internal/api"
	"github.com/coding-hermes/scheduler/internal/bus"
	"github.com/coding-hermes/scheduler/internal/database"
	mcpserver "github.com/coding-hermes/scheduler/internal/mcp"
	"github.com/coding-hermes/scheduler/internal/scheduler"
)

// newFedMCPStack wires the REAL daemon topology for the federation tools:
// ONE api.Server + ONE mcp.Server over one DB, the MCP server's query
// entry point installed from the api server exactly as main.go does. No
// operator auth is armed (reads are open) — the caller identity these
// queries audit under is the mcp:auth-off arm.
func newFedMCPStack(t *testing.T) (*mcpTestServer, *api.Server) {
	t.Helper()
	db, err := database.InitDB(filepath.Join(t.TempDir(), "scheduler.db"))
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	// gap1575 construction: a non-running loop, budget=0 so no real spawn
	// can ever fire — a test must not touch the host.
	loop := scheduler.NewLoop(db, time.Minute, time.Hour, 10, 0, 5)
	loop.SetNoExecFallback(true)
	apiSrv := api.NewServer(db, loop)
	mcpSrv := mcpserver.NewServer(db, loop)
	mcpSrv.SetFederationQueryHandler(apiSrv.FederationQueryHandler)
	ts := httptest.NewServer(mcpSrv.Handler())
	t.Cleanup(ts.Close)
	m := &mcpTestServer{db: db, loop: loop, server: mcpSrv, ts: ts}
	mustCreateFedProject(t, db, "alpha010")
	mustCreateFedProject(t, db, "beta010")
	return m, apiSrv
}

// mustCreateFedProject inserts one deterministic project row (the fixture
// the projects.list comparisons read back).
func mustCreateFedProject(t *testing.T, db *sql.DB, name string) {
	t.Helper()
	if err := database.CreateProject(context.Background(), db, &database.Project{
		Name:     name,
		RepoURL:  "https://example.com/" + name,
		Workdir:  "/tmp/remote010-" + name,
		Weight:   10,
		Priority: 5,
	}); err != nil {
		t.Fatalf("CreateProject %s: %v", name, err)
	}
}

// fedMCPCall invokes an MCP tool through the real JSON-RPC surface and
// returns the raw §2.2 envelope text payload.
func fedMCPCall(t *testing.T, m *mcpTestServer, id int, name string, args map[string]interface{}) string {
	t.Helper()
	text, err := callTool(t, m, id, name, args)
	if err != nil {
		t.Fatalf("tool %s: %+v", name, err)
	}
	return text
}

// TestREMOTE010_MCPToolAnswerEqualsInternalAnswer is acceptance cell 1:
// an MCP tool call for an op returns the SAME data the internal entry
// point returns. The internal call (the ORACLE) runs first; the MCP
// fed_queue_get answer is diffed on the whole data member — the §6
// conformance shape with the transport-specific members (as_of/age_ms,
// stamped at answer time) normalized out.
func TestREMOTE010_MCPToolAnswerEqualsInternalAnswer(t *testing.T) {
	m, apiSrv := newFedMCPStack(t)

	// THE ORACLE: the exact entry point the HTTP/bus/MCP adapters run.
	oracle := apiSrv.FederationQueryHandler(
		bus.QueryEnvelope{Op: "queue.get", CorrID: "oracle-q1"}, "test-oracle")
	if oracle.Status != "ok" {
		t.Fatalf("oracle status = %s (%+v)", oracle.Status, oracle.Error)
	}

	// THE MCP ANSWER via the real JSON-RPC surface.
	text := fedMCPCall(t, m, 1, "fed_queue_get", map[string]interface{}{
		"federation_corr_id": "mcp-q1",
	})
	var env struct {
		CorrID string          `json:"corr_id"`
		Op     string          `json:"op"`
		Peer   string          `json:"peer"`
		Status string          `json:"status"`
		Data   json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal([]byte(text), &env); err != nil {
		t.Fatalf("MCP answer not a §2.2 envelope: %v (%s)", err, text)
	}
	if env.Status != "ok" {
		t.Fatalf("MCP status = %s (%s)", env.Status, text)
	}
	if env.CorrID != "mcp-q1" {
		t.Errorf("corr_id echo = %q, want mcp-q1", env.CorrID)
	}
	if env.Op != "queue.get" {
		t.Errorf("op echo = %q, want queue.get", env.Op)
	}
	if env.Peer != database.SchedulerID() {
		t.Errorf("peer = %q, want the local scheduler id", env.Peer)
	}
	b1, _ := json.Marshal(oracle.Data)
	if string(b1) != string(env.Data) {
		t.Errorf("data drifted between the internal entry point and the MCP tool:\ninternal: %s\nmcp:      %s", b1, env.Data)
	}
}

// TestREMOTE010_FedQueryMatchesInternalAnswer proves the generic
// escape-hatch tool on a second op (projects.list) — same oracle shape,
// the args member carried verbatim (the filter).
func TestREMOTE010_FedQueryMatchesInternalAnswer(t *testing.T) {
	m, apiSrv := newFedMCPStack(t)

	oracle := apiSrv.FederationQueryHandler(bus.QueryEnvelope{
		Op:     "projects.list",
		Args:   map[string]any{"filter": "alpha010"},
		CorrID: "oracle-p1",
	}, "test-oracle")
	if oracle.Status != "ok" {
		t.Fatalf("oracle status = %s (%+v)", oracle.Status, oracle.Error)
	}

	text := fedMCPCall(t, m, 1, "fed_query", map[string]interface{}{
		"op":      "projects.list",
		"args":    map[string]interface{}{"filter": "alpha010"},
		"corr_id": "mcp-p1",
	})
	var env struct {
		Status string          `json:"status"`
		Data   json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal([]byte(text), &env); err != nil {
		t.Fatalf("MCP answer not a §2.2 envelope: %v (%s)", err, text)
	}
	if env.Status != "ok" {
		t.Fatalf("MCP status = %s (%s)", env.Status, text)
	}
	b1, _ := json.Marshal(oracle.Data)
	if string(b1) != string(env.Data) {
		t.Errorf("data drifted:\ninternal: %s\nmcp:      %s", b1, env.Data)
	}
	// The fixture is real AND the filter bit: exactly the alpha010 row.
	var rows []map[string]any
	if err := json.Unmarshal(env.Data, &rows); err != nil {
		t.Fatalf("data not a list: %s", env.Data)
	}
	if len(rows) != 1 || rows[0]["name"] != "alpha010" {
		t.Errorf("filtered rows = %v, want exactly alpha010", rows)
	}
}

// TestREMOTE010_RefusalParity proves a bad op through the MCP tool gets
// the SAME named refusal the internal entry point produces — the same
// stable code, the same message (catalogue inline, spec §2.2: an error
// ANSWER, never an empty one; one error vocabulary across transports).
func TestREMOTE010_RefusalParity(t *testing.T) {
	m, apiSrv := newFedMCPStack(t)

	oracle := apiSrv.FederationQueryHandler(
		bus.QueryEnvelope{Op: "nope.op", CorrID: "oracle-u1"}, "test-oracle")
	if oracle.Status != "error" || oracle.Error == nil {
		t.Fatalf("oracle refusal shape wrong: %+v", oracle)
	}

	text := fedMCPCall(t, m, 1, "fed_query", map[string]interface{}{
		"op":      "nope.op",
		"corr_id": "mcp-u1",
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
		t.Fatalf("unknown op must be a status=error envelope: %s", text)
	}
	if env.Error.Code != oracle.Error.Code {
		t.Errorf("code drifted: mcp %s vs internal %s", env.Error.Code, oracle.Error.Code)
	}
	if env.Error.Message != oracle.Error.Message {
		t.Errorf("message drifted:\nmcp:      %s\ninternal: %s", env.Error.Message, oracle.Error.Message)
	}
	if !strings.Contains(env.Error.Message, "projects.list") {
		t.Errorf("refusal message lost the catalogue: %s", env.Error.Message)
	}
}

// TestREMOTE010_UnwiredFailsClosed proves the adapter never answers by
// itself: an MCP server whose query entry point was never installed (the
// nil arm main.go's wiring normally fills) answers the fed_* tools with
// a configuration error — NOT a locally computed answer.
func TestREMOTE010_UnwiredFailsClosed(t *testing.T) {
	m := newMCPTestServer(t) // the standard test stack: no SetFederationQueryHandler
	_, err := callTool(t, m, 1, "fed_queue_get", map[string]interface{}{
		"federation_corr_id": "mcp-unwired-1",
	})
	if err == nil {
		t.Fatal("unwired fed tool answered — the adapter computed an answer itself")
	}
	if !strings.Contains(err.Message, "not wired") {
		t.Errorf("unwired error = %+v, want the not-wired configuration error", err)
	}
}

// TestREMOTE010_FedToolsAreReads pins the read classification: every
// fed_* tool the registry serves must sit in the read-only set — the
// partition guard (TestMCPAuth_MutationClassificationCoversRegistry)
// already fails on an unclassified tool; this cell pins the DIRECTION
// (a fed tool must never be classifiable as a mutator).
func TestREMOTE010_FedToolsAreReads(t *testing.T) {
	mutating := map[string]bool{}
	for _, name := range mcpserver.MutatingToolNames() {
		mutating[name] = true
	}
	for _, name := range []string{
		"fed_query", "fed_peer_status", "fed_fleet_status",
		"fed_projects_list", "fed_queue_get", "fed_ticks_list", "fed_events_list",
	} {
		if mutating[name] {
			t.Errorf("federation tool %q classified MUTATING — queries are read-only by construction (spec §7)", name)
		}
	}
}

// TestREMOTE010_ReplyAuditRowWritten proves the spec §4 cross-box read
// audit fires on the MCP path: an answered fed tool call writes one
// api.federation events row naming the MCP caller identity — the same
// audit mechanism the HTTP surface writes through.
func TestREMOTE010_ReplyAuditRowWritten(t *testing.T) {
	m, _ := newFedMCPStack(t)
	fedMCPCall(t, m, 1, "fed_fleet_status", map[string]interface{}{
		"federation_corr_id": "mcp-audit-1",
	})
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
		if strings.Contains(msg, "op=fleet.status") &&
			strings.Contains(msg, "mcp-audit-1") &&
			strings.Contains(msg, "caller=mcp:") {
			found = true
		}
	}
	if err := rs.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	if !found {
		t.Error("no api.federation audit row for the MCP-answered query (spec §4: reads are observable, not silently free)")
	}
}

// TestREMOTE010_ReplayReturnsFirstAnswer proves §2.5 through the MCP
// transport: two tool calls with the SAME corr_id return the identical
// envelope (the shared replay window, keyed on the MCP caller identity) —
// byte-identical including as_of, the exact shape the bus adapter proves
// for its transport.
func TestREMOTE010_ReplayReturnsFirstAnswer(t *testing.T) {
	m, _ := newFedMCPStack(t)
	first := fedMCPCall(t, m, 1, "fed_peer_status", map[string]interface{}{
		"federation_corr_id": "mcp-replay-1",
	})
	second := fedMCPCall(t, m, 2, "fed_peer_status", map[string]interface{}{
		"federation_corr_id": "mcp-replay-1",
	})
	if first != second {
		t.Errorf("replay drifted:\nfirst:  %s\nsecond: %s", first, second)
	}
	// A different corr_id is a fresh query (new as_of is possible).
	third := fedMCPCall(t, m, 3, "fed_peer_status", map[string]interface{}{
		"federation_corr_id": "mcp-replay-2",
	})
	var e2, e3 map[string]any
	if err := json.Unmarshal([]byte(second), &e2); err != nil {
		t.Fatalf("decode second: %v", err)
	}
	if err := json.Unmarshal([]byte(third), &e3); err != nil {
		t.Fatalf("decode third: %v", err)
	}
	if e2["corr_id"] != "mcp-replay-1" || e3["corr_id"] != "mcp-replay-2" {
		t.Errorf("corr_id echo drifted: %v / %v", e2["corr_id"], e3["corr_id"])
	}
}

// TestREMOTE010_TooFewArgsRefused pins the REQUIRED-member validation at
// the adapter boundary: fed_query without op or corr_id, and a per-op
// tool without federation_corr_id, are argument errors (the §2.2 error
// envelope echoes corr_id, which does not exist to echo here — the same
// named-refusal discipline the HTTP validation writes).
func TestREMOTE010_TooFewArgsRefused(t *testing.T) {
	m, _ := newFedMCPStack(t)
	if _, err := callTool(t, m, 1, "fed_query", map[string]interface{}{"op": "queue.get"}); err == nil {
		t.Fatal("fed_query without corr_id must be refused")
	}
	if _, err := callTool(t, m, 2, "fed_query", map[string]interface{}{"corr_id": "x"}); err == nil {
		t.Fatal("fed_query without op must be refused")
	}
	if _, err := callTool(t, m, 3, "fed_queue_get", map[string]interface{}{}); err == nil {
		t.Fatal("fed_queue_get without federation_corr_id must be refused")
	}
}

// TestREMOTE010_BudgetClampShared pins the budget path: a huge budget_ms
// through the MCP tool is clamped by the SHARED cap
// (federationBudgetClampMS inside the entry point) and the answer still
// returns — the MCP surface runs no second clamp of its own.
func TestREMOTE010_BudgetClampShared(t *testing.T) {
	m, _ := newFedMCPStack(t)
	text := fedMCPCall(t, m, 1, "fed_projects_list", map[string]interface{}{
		"federation_corr_id": "mcp-budget-1",
		"budget_ms":          1 << 30, // far above the shared cap — must clamp, not error
	})
	var env struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal([]byte(text), &env); err != nil {
		t.Fatalf("decode: %v (%s)", err, text)
	}
	if env.Status != "ok" && env.Status != "stale" {
		t.Errorf("status = %s, want ok|stale — the clamped budget must still answer (%s)", env.Status, text)
	}
}
