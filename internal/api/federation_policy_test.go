package api

// REMOTE-013 (docs/federation-query-spec.md §4) acceptance battery: the
// read-access policy on the federation query surface.
//
// The named cells map 1:1 to the row's acceptance list:
//   - an unauthorized read is REFUSED (fail-closed) on the shared entry
//     point, with the refusal visible on TWO transports (the shared
//     FederationQueryHandler once — the bus/MCP/CLI path — and the HTTP
//     wire once);
//   - an authorized read still succeeds unchanged (no regression);
//   - a REFUSED read still writes its §4 audit row;
//   - refusals are NAMED (the stable op_not_allowed code), never silent.
//
// The deny-all default is the fail-closed law: a Server that never received
// SetFederationReadPolicy answers NOTHING (every read op_not_allowed) —
// the SCHED-GAP-1602 posture carried to the read path.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coding-hermes/scheduler/internal/bus"
	"github.com/coding-hermes/scheduler/internal/database"
	"github.com/coding-hermes/scheduler/internal/scheduler"
)

// fedPolicyAllOps is the full v1 read surface (the six §2.3 catalogue ops
// plus the §5 aggregate) — the grant an "allow everything" test arm uses.
var fedPolicyAllOps = []string{
	"peer.status", "fleet.status", "projects.list",
	"queue.get", "ticks.list", "events.list", fedOpAggregate,
}

// fedGrantAllOps is the grant an "allow everything" test arm uses: the
// caller-agnostic default ("*") over the whole v1 read surface (the six
// §2.3 catalogue ops plus the §5 aggregate).
func fedGrantAllOps() FederationReadGrant {
	return FederationReadGrant{Caller: FederationAllowAll, Ops: fedPolicyAllOps}
}

// newFedPolicyServer builds a Server on a fresh temp-file SQLite database
// (the newRemote009Server construction: a non-running loop, budget=0 so no
// real spawn can ever fire) with the given read grants armed.
func newFedPolicyServer(t *testing.T, grants ...FederationReadGrant) *Server {
	t.Helper()
	db, err := database.InitDB(filepath.Join(t.TempDir(), "scheduler.db"))
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	loop := scheduler.NewLoop(db, time.Minute, time.Hour, 10, 0, 5)
	loop.SetNoExecFallback(true)
	s := NewServer(db, loop)
	s.SetFederationReadPolicy(ResolveFederationReadPolicy(grants))
	return s
}

// fedPolicyBusQuery runs one query through the SHARED entry point the
// non-HTTP transports funnel through (FederationQueryHandler — the exact
// function main.go installs into the bus responder and the MCP server).
func fedPolicyBusQuery(s *Server, op, corrID, caller string) bus.ResponseEnvelope {
	return s.FederationQueryHandler(bus.QueryEnvelope{Op: op, CorrID: corrID}, caller)
}

// fedPolicyHTTPQuery posts one envelope to the HTTP wire with the operator
// credential armed (the caller identity that surface audits under is
// operator:token).
func fedPolicyHTTPQuery(t *testing.T, s *Server, token, body string) (int, map[string]any) {
	t.Helper()
	s.SetAuthConfig(ResolveAuthConfig(token, "", ""))
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	req, err := http.NewRequest(http.MethodPost, ts.URL+"/api/v1/federation/query", strings.NewReader(body))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Operator-Token", token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST federation/query: %v", err)
	}
	defer resp.Body.Close()
	var env map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return resp.StatusCode, env
}

// fedPolicyAuditRows returns the api.federation event messages (the §4
// cross-box read audit rows).
func fedPolicyAuditRows(t *testing.T, s *Server) []string {
	t.Helper()
	rows, err := s.db.QueryContext(context.Background(),
		`SELECT message FROM events WHERE component='api.federation' ORDER BY id`)
	if err != nil {
		t.Fatalf("query events: %v", err)
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var msg string
		if err := rows.Scan(&msg); err != nil {
			t.Fatalf("scan event: %v", err)
		}
		out = append(out, msg)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	return out
}

// TestREMOTE013_PolicyResolution_Matrix pins the decision table: absence of
// an allow is a refusal (fail-closed), blank grants are dropped, "*" is the
// inherited default, and a caller's own row wins over "*" in BOTH
// directions (per-peer scoping, spec §4).
func TestREMOTE013_PolicyResolution_Matrix(t *testing.T) {
	grants := []FederationReadGrant{
		{Caller: FederationAllowAll, Ops: []string{"fleet.status"}},
		{Caller: "primary-01", Ops: []string{"queue.get"}},
		{Caller: "  ", Ops: []string{"peer.status"}},   // blank caller: dropped
		{Caller: "blank-ops", Ops: []string{"  ", ""}}, // blank ops: dropped
	}
	p := ResolveFederationReadPolicy(grants)
	cases := []struct {
		caller, op string
		want       bool
	}{
		{"primary-01", "queue.get", true},      // own row
		{"primary-01", "fleet.status", false},  // own row wins — NOT widened by "*"
		{"someone-else", "fleet.status", true}, // inherits "*"
		{"someone-else", "queue.get", false},   // not in "*"
		{"blank-ops", "peer.status", false},    // blank allow = unset, not a grant
		{"nobody", "queue.get", false},         // unknown caller
		{"", "queue.get", false},               // blank caller never allowed
	}
	for _, tc := range cases {
		if got := p.allows(tc.caller, tc.op); got != tc.want {
			t.Errorf("allows(%q, %q) = %v, want %v", tc.caller, tc.op, got, tc.want)
		}
	}
	// The zero policy denies everything (fail-closed by construction).
	var zero FederationReadPolicy
	for _, op := range fedPolicyAllOps {
		if zero.allows("anyone", op) {
			t.Errorf("zero policy allowed %q — deny-all default broken", op)
		}
	}
}

// TestREMOTE013_UnauthorizedRead_RefusedOnSharedEntryPoint is acceptance
// cell 1: an unauthorized read is REFUSED on the shared entry point with
// the refusal visible on TWO transports — (a) FederationQueryHandler (the
// function every non-HTTP transport is wired through) and (b) the HTTP
// wire. No policy armed = deny-all: even the operator credential does not
// confer read authority (authentication ≠ authorization).
func TestREMOTE013_UnauthorizedRead_RefusedOnSharedEntryPoint(t *testing.T) {
	s := newFedPolicyServer(t) // no policy: deny-all

	// (a) The SHARED entry point — the bus transport's exact call shape.
	resp := fedPolicyBusQuery(s, "queue.get", "pol-deny-bus-1", "bus:transport")
	if resp.Status != fedStatusError {
		t.Fatalf("shared entry point status = %q, want error", resp.Status)
	}
	if resp.Error == nil || resp.Error.Code != fedErrOpNotAllowed {
		t.Fatalf("shared entry point error = %+v, want code=%q (a NAMED refusal, never silence)", resp.Error, fedErrOpNotAllowed)
	}
	if resp.Error.Message == "" {
		t.Error("refusal message empty — refusals are named, not silent")
	}

	// (b) The HTTP transport — same caller class of refusal on the wire.
	status, env := fedPolicyHTTPQuery(t, s, "pol-operator",
		`{"op":"queue.get","corr_id":"pol-deny-http-1"}`)
	if status != http.StatusForbidden {
		t.Errorf("HTTP status = %d, want 403", status)
	}
	if env["status"] != fedStatusError {
		t.Errorf("HTTP envelope status = %v, want error", env["status"])
	}
	if code, _ := env["error"].(map[string]any); code == nil || code["code"] != fedErrOpNotAllowed {
		t.Errorf("HTTP envelope error = %v, want code=%q", env["error"], fedErrOpNotAllowed)
	}

	// And the refusal exists on the shared entry point for a DIFFERENT
	// transport identity too (MCP names its auth mode): the policy keys on
	// the caller, so an unlisted MCP caller is refused the same way.
	mcpResp := fedPolicyBusQuery(s, "fleet.status", "pol-deny-mcp-1", "mcp:auth-token")
	if mcpResp.Error == nil || mcpResp.Error.Code != fedErrOpNotAllowed {
		t.Errorf("MCP-shaped caller status = %+v, want op_not_allowed", mcpResp.Error)
	}
}

// TestREMOTE013_RefusedReadStillAudited is acceptance cell 3: a REFUSED
// read still writes its §4 audit row — on BOTH entry surfaces (the shared
// entry point's row and the HTTP surface's row), through the ONE existing
// audit mechanism (component=api.federation), never a second one.
func TestREMOTE013_RefusedReadStillAudited(t *testing.T) {
	s := newFedPolicyServer(t) // deny-all

	refused := fedPolicyBusQuery(s, "queue.get", "pol-audit-bus-1", "bus:transport")
	if refused.Error == nil || refused.Error.Code != fedErrOpNotAllowed {
		t.Fatalf("expected the refused read, got %+v", refused.Error)
	}
	status, _ := fedPolicyHTTPQuery(t, s, "pol-operator",
		`{"op":"queue.get","corr_id":"pol-audit-http-1"}`)
	if status != http.StatusForbidden {
		t.Fatalf("HTTP status = %d, want 403", status)
	}

	rows := fedPolicyAuditRows(t, s)
	foundBus, foundHTTP := false, false
	for _, msg := range rows {
		if strings.Contains(msg, "op=queue.get") &&
			strings.Contains(msg, "pol-audit-bus-1") &&
			strings.Contains(msg, "caller=bus:transport") &&
			strings.Contains(msg, fedErrOpNotAllowed) {
			foundBus = true
		}
		if strings.Contains(msg, "op=queue.get") &&
			strings.Contains(msg, "pol-audit-http-1") &&
			strings.Contains(msg, "caller=operator:token") &&
			strings.Contains(msg, fedErrOpNotAllowed) {
			foundHTTP = true
		}
	}
	if !foundBus {
		t.Errorf("no api.federation audit row for the REFUSED shared-entry read (§4: the refusal is the interesting event); rows: %v", rows)
	}
	if !foundHTTP {
		t.Errorf("no api.federation audit row for the REFUSED HTTP read (§4); rows: %v", rows)
	}
}

// TestREMOTE013_AuthorizedRead_SucceedsUnchanged is acceptance cell 2: an
// authorized read still succeeds — same envelope, same data — on both
// transports (no regression for the configured operator).
func TestREMOTE013_AuthorizedRead_SucceedsUnchanged(t *testing.T) {
	s := newFedPolicyServer(t, FederationReadGrant{Caller: FederationAllowAll, Ops: fedPolicyAllOps})

	// Shared entry point (bus transport): the allowed read answers ok with
	// the §2.2 envelope invariants intact.
	resp := fedPolicyBusQuery(s, "queue.get", "pol-allow-bus-1", "bus:transport")
	if resp.Status != fedStatusOK && resp.Status != fedStatusStale {
		t.Fatalf("authorized bus read status = %q (%+v), want ok|stale", resp.Status, resp.Error)
	}
	if resp.Error != nil {
		t.Fatalf("authorized bus read carried an error: %+v", resp.Error)
	}
	if resp.CorrID != "pol-allow-bus-1" || resp.Op != "queue.get" {
		t.Errorf("envelope echo drifted: corr=%q op=%q", resp.CorrID, resp.Op)
	}
	if resp.Data == nil {
		t.Error("data is null — the §2.2 null law requires []/{}")
	}

	// HTTP transport: same op, the operator caller — ok on the wire.
	status, env := fedPolicyHTTPQuery(t, s, "pol-operator",
		`{"op":"queue.get","corr_id":"pol-allow-http-1"}`)
	if status != http.StatusOK {
		t.Fatalf("authorized HTTP read status = %d, want 200; env: %v", status, env)
	}
	if env["status"] != fedStatusOK && env["status"] != fedStatusStale {
		t.Errorf("authorized HTTP read envelope status = %v, want ok|stale", env["status"])
	}
}

// TestREMOTE013_PerCallerScoping pins the spec §4 per-peer model through
// the shared entry point: a caller reads only what ITS row (or "*")
// publishes — one caller allowed, another refused, same server, same op.
func TestREMOTE013_PerCallerScoping(t *testing.T) {
	s := newFedPolicyServer(t,
		FederationReadGrant{Caller: "primary-01", Ops: []string{"queue.get"}},
	)

	ok := fedPolicyBusQuery(s, "queue.get", "pol-scope-1", "primary-01")
	if ok.Status != fedStatusOK && ok.Status != fedStatusStale {
		t.Fatalf("allowed caller status = %q (%+v), want ok|stale", ok.Status, ok.Error)
	}
	refused := fedPolicyBusQuery(s, "queue.get", "pol-scope-2", "secondary-99")
	if refused.Error == nil || refused.Error.Code != fedErrOpNotAllowed {
		t.Fatalf("unlisted caller status = %+v, want op_not_allowed (per-peer scoping)", refused.Error)
	}
}

// TestREMOTE013_UnknownOpStillUnknownOp pins the vocabulary boundary: the
// policy governs REAL reads — an op outside the catalogue keeps its
// unknown_op refusal (spec §2.2) even under deny-all, so the error
// vocabulary does not drift for typos.
func TestREMOTE013_UnknownOpStillUnknownOp(t *testing.T) {
	s := newFedPolicyServer(t) // deny-all
	resp := fedPolicyBusQuery(s, "nope.op", "pol-unknown-1", "bus:transport")
	if resp.Error == nil || resp.Error.Code != fedErrUnknownOp {
		t.Fatalf("unknown op under deny-all = %+v, want code=%q", resp.Error, fedErrUnknownOp)
	}
}

// TestREMOTE013_AggregateGatedToo pins that the §5 aggregate — the one
// envelope op outside the catalogue — is behind the SAME policy key: an
// unauthorized aggregate is op_not_allowed (the aggregate is a read of
// reads; leaving it open would bypass per-op scoping wholesale).
func TestREMOTE013_AggregateGatedToo(t *testing.T) {
	s := newFedPolicyServer(t) // deny-all
	status, env := fedPolicyHTTPQuery(t, s, "pol-operator",
		`{"op":"fleet.aggregate","corr_id":"pol-agg-1","args":{"op":"queue.get"}}`)
	if status != http.StatusForbidden {
		t.Errorf("aggregate status = %d, want 403 (gated like any read)", status)
	}
	if code, _ := env["error"].(map[string]any); code == nil || code["code"] != fedErrOpNotAllowed {
		t.Errorf("aggregate error = %v, want code=%q", env["error"], fedErrOpNotAllowed)
	}
	// And an authorized caller can run it (no regression on the armed side).
	s2 := newFedPolicyServer(t, FederationReadGrant{Caller: FederationAllowAll, Ops: fedPolicyAllOps})
	status2, env2 := fedPolicyHTTPQuery(t, s2, "pol-operator",
		`{"op":"fleet.aggregate","corr_id":"pol-agg-2","args":{"op":"queue.get"}}`)
	if status2 != http.StatusOK {
		t.Errorf("authorized aggregate status = %d, want 200; env: %v", status2, env2)
	}
}
