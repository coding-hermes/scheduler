package mcp_test

/*
ch:trace row=SCHED-GAP-1751 spec=docs/api.md#18 wave=.coding-hermes/waves/coding-hermes-scheduler-foreman-2026-10-22-21-23-23.json#SCHED-GAP-1751 test=internal/mcp/*_test.go::UnknownLane doc=docs/api.md#18 prompt=/tmp/sched-20261022212323-SCHED-GAP-1751-brief.txt evidence=commit witness=none:foreman-verifies-test verdict=none:pending-foreman-trace commit=none:pending-foreman memory=none:not-written
STATUS: complete | partial | blocked
TIER: T0 | T1 | T2 | T3 (state what was invoked and observed for T3)
EVIDENCE: <resolvable commit or command output>
*/

// SCHED-GAP-1751 — unknown-lane handling is ONE contract across the three
// surfaces that answer link questions: REST GET /api/v1/links/ancestor,
// MCP lane_relations and MCP lane_is_ancestor. The REST read refuses a
// subject lane that does not name a projects row with the NAMED code
// `unknown_lane` (404, internal/api/links.go laneMustExist — "a typo never
// silently returns nothing found"). The MCP mirrors must carry the same
// named code instead of their old divergent answers: lane_is_ancestor
// answered `is_ancestor: false, hops: 0` (a typo indistinguishable from a
// real negative) and lane_relations leaked the raw DB text
// `project not found: <lane>`. These cells drive the real JSON-RPC surface
// and the real REST handler against the same in-memory DB and pin the
// shared refusal. Known-lane reachability behaviour is unchanged: an
// existing subject with an unknown ANCESTOR still answers a legitimate
// false (REST validates the subject lane only), and a real parent→child
// edge still answers true with a witness path.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coding-hermes/scheduler/internal/api"
	"github.com/coding-hermes/scheduler/internal/database"
	"github.com/coding-hermes/scheduler/internal/scheduler"
)

// seedSCHED1751Lanes creates two real lanes joined by one edge, so both the
// positive reachability arm and the negative arms have live rows to walk.
// Distinct lane names keep any name-suffix heuristic out of the picture.
func seedSCHED1751Lanes(t *testing.T, m *mcpTestServer) {
	t.Helper()
	mustCreateMCPProject(t, m.db, "1751-parent")
	mustCreateMCPProject(t, m.db, "1751-child")
	if _, err := database.CreateLaneLink(context.Background(), m.db,
		"1751-parent", "1751-child", "", "sched-gap-1751 seed"); err != nil {
		t.Fatalf("CreateLaneLink seed: %v", err)
	}
}

// TestMCP_LaneIsAncestor_UnknownLaneErrorCode pins the SCHED-GAP-1751 fix:
// lane_is_ancestor with a NONEXISTENT subject lane must refuse with the same
// named unknown-lane code REST /links/ancestor writes — not the old
// is_ancestor=false/hops=0 success shape.
func TestMCP_LaneIsAncestor_UnknownLaneErrorCode(t *testing.T) {
	m := newMCPTestServer(t)
	seedSCHED1751Lanes(t, m)

	// REST ground truth on the same DB: the subject lane is checked for
	// existence and its refusal names unknown_lane with status 404.
	restSrv := api.NewServer(m.db, scheduler.NewLoop(m.db, time.Minute, time.Hour, 10, 0, 5))
	restTS := httptest.NewServer(restSrv.Handler())
	t.Cleanup(restTS.Close)
	resp, err := http.Get(restTS.URL + "/api/v1/links/ancestor?lane=1751-ghost&ancestor=1751-parent")
	if err != nil {
		t.Fatalf("REST GET: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("REST unknown-lane status = %d, want 404 (body=%s)", resp.StatusCode, body)
	}
	var restErr struct {
		ErrorMessage string `json:"error"`
		Code         string `json:"code"`
	}
	if err := json.Unmarshal(body, &restErr); err != nil {
		t.Fatalf("REST body not JSON: %v (%s)", err, body)
	}
	if restErr.Code != "unknown_lane" {
		t.Fatalf("REST code = %q, want unknown_lane (body=%s)", restErr.Code, body)
	}

	// MCP arm: the same ghost subject, over JSON-RPC, must be a refusal —
	// NOT the old is_ancestor=false success — and the message must carry
	// the SAME named code the REST answer just returned.
	_, errResp := callTool(t, m, 1, "lane_is_ancestor", map[string]interface{}{
		"lane": "1751-ghost", "ancestor": "1751-parent",
	})
	if errResp == nil {
		t.Fatal("lane_is_ancestor on a nonexistent subject lane answered success — want a refusal carrying the REST unknown_lane code")
	}
	if errResp.Code != -32000 {
		t.Errorf("MCP error code = %d, want -32000 (message=%q)", errResp.Code, errResp.Message)
	}
	if !strings.Contains(errResp.Message, "unknown_lane") {
		t.Errorf("MCP message %q does not carry the REST named code unknown_lane (REST code=%q)", errResp.Message, restErr.Code)
	}
	if !strings.Contains(errResp.Message, "1751-ghost") {
		t.Errorf("MCP message %q must name the offending lane 1751-ghost", errResp.Message)
	}
}

// TestMCP_LaneIsAncestor_KnownLaneReachabilityPreserved pins the negative
// arm of the fix: the existence check is on the SUBJECT lane only (the REST
// contract), the ancestor argument is NOT existence-checked, and a real
// edge still answers true with its witness path.
func TestMCP_LaneIsAncestor_KnownLaneReachabilityPreserved(t *testing.T) {
	m := newMCPTestServer(t)
	seedSCHED1751Lanes(t, m)

	// Known subject, unknown ancestor: still a legitimate false reply with
	// an empty path — REST /links/ancestor behaves the same way, so the MCP
	// mirror keeps it (an existence check on BOTH endpoints would be a
	// redesign, not a parity fix).
	text, errResp := callTool(t, m, 1, "lane_is_ancestor", map[string]interface{}{
		"lane": "1751-child", "ancestor": "1751-ghost-ancestor",
	})
	if errResp != nil {
		t.Fatalf("unknown ancestor on known subject: unexpected refusal %q — reachability behaviour must be preserved", errResp.Message)
	}
	out := map[string]interface{}{}
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		t.Fatalf("payload not JSON: %v (%s)", err, text)
	}
	if out["is_ancestor"] != false {
		t.Errorf("is_ancestor = %v, want false for a nonexistent ancestor", out["is_ancestor"])
	}

	// Real edge: the answer stays true with a witness path and hop count.
	text, errResp = callTool(t, m, 2, "lane_is_ancestor", map[string]interface{}{
		"lane": "1751-child", "ancestor": "1751-parent",
	})
	if errResp != nil {
		t.Fatalf("real edge: unexpected refusal %q", errResp.Message)
	}
	out = map[string]interface{}{}
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		t.Fatalf("payload not JSON: %v (%s)", err, text)
	}
	if out["is_ancestor"] != true {
		t.Errorf("is_ancestor = %v, want true for the seeded parent→child edge", out["is_ancestor"])
	}
	path, _ := out["path"].([]interface{})
	if len(path) != 2 || path[0] != "1751-parent" || path[1] != "1751-child" {
		t.Errorf("path = %v, want [1751-parent 1751-child]", path)
	}
	if out["hops"] != float64(1) {
		t.Errorf("hops = %v, want 1", out["hops"])
	}
}

// TestMCP_LaneRelations_UnknownLaneErrorCode pins lane_relations on the same
// contract: a nonexistent subject lane answers the named unknown_lane code,
// not the raw DB text "project not found: <lane>".
func TestMCP_LaneRelations_UnknownLaneErrorCode(t *testing.T) {
	m := newMCPTestServer(t)
	seedSCHED1751Lanes(t, m)

	for _, direction := range []string{"parents", "children", "ancestors", "descendants"} {
		_, errResp := callTool(t, m, 1, "lane_relations", map[string]interface{}{
			"lane": "1751-ghost", "direction": direction,
		})
		if errResp == nil {
			t.Fatalf("lane_relations direction=%s on a nonexistent lane answered success — want the unknown_lane refusal", direction)
		}
		if !strings.Contains(errResp.Message, "unknown_lane") {
			t.Errorf("lane_relations %s message %q does not carry the REST named code unknown_lane", direction, errResp.Message)
		}
		if !strings.Contains(errResp.Message, "1751-ghost") {
			t.Errorf("lane_relations %s message %q must name the offending lane", direction, errResp.Message)
		}
	}

	// Known lane: unaffected. The direct-child read still returns the
	// recorded edge (and never null links).
	text, errResp := callTool(t, m, 2, "lane_relations", map[string]interface{}{
		"lane": "1751-parent", "direction": "children",
	})
	if errResp != nil {
		t.Fatalf("known lane: unexpected refusal %q", errResp.Message)
	}
	out := map[string]interface{}{}
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		t.Fatalf("payload not JSON: %v (%s)", err, text)
	}
	if out["count"] != float64(1) {
		t.Errorf("children count = %v, want 1 for the seeded edge", out["count"])
	}
}

// Compile-time guard that the shared helpers this file relies on keep their
// shapes (unused-import insurance written as checks the compiler can fold).
var (
	_ = errors.New
	_ = fmt.Sprintf
)
