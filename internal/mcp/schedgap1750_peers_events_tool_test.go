package mcp_test

// SCHED-GAP-1750 — `peers_events` was unusable in BOTH advertised forms: the
// schema offered only an optional `id` while the description promised the
// all-schedulers default, and every no-id call answered
// `-32000 "remote last event id: scheduler_id required"` — naming a
// parameter that was not in the schema at all. These cells pin the fixed
// behaviour over the real JSON-RPC surface:
//
//   - `{}`, `{"id":"*"}` and `{"scheduler_id":"*"}` answer the AGGREGATE
//     slice (scheduler_id "*", union state exactly as the fleet-wide REST
//     route renders it);
//   - a registered peer id still answers its own slice (happy path);
//   - an unknown peer id still answers the register-it error (negative arm);
//   - the advertised inputSchema carries BOTH `id` and `scheduler_id`
//     properties (no error may name a parameter a client cannot supply).
//
// The store arms are seeded with real ingests (database.IngestRemoteEvent +
// SetRemoteLastEventID) so the assertions run against live rows, not mocks.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/coding-hermes/scheduler/internal/database"
)

// seedPeerEvents registers two peers and ingests one durable event each,
// with distinct marks and dedupe-drop counts, so the aggregate assertions
// have a union to read. Returns both ids for the per-peer arms.
func seedPeerEvents(t *testing.T, m *mcpTestServer) (string, string) {
	t.Helper()
	ctx := context.Background()
	for _, p := range []*database.Peer{
		{ID: "sched-alpha", URL: "http://alpha:8080", Version: "v1.0.0"},
		{ID: "sched-beta", URL: "http://beta:8080"},
	} {
		if err := database.UpsertPeer(ctx, m.db, p); err != nil {
			t.Fatalf("UpsertPeer %s: %v", p.ID, err)
		}
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := database.IngestRemoteEvent(ctx, m.db, &database.RemoteEvent{
		SchedulerID: "sched-alpha", EventID: "sched-alpha-00000000000000000009",
		Kind: "tick.terminal", Project: "proj-a", TickID: "t-9", Status: "completed",
		TS: now, IngestedAt: now, Source: "live",
	}); err != nil {
		t.Fatalf("ingest alpha: %v", err)
	}
	if err := database.BumpRemoteDedupeDrops(ctx, m.db, "sched-alpha", 3); err != nil {
		t.Fatalf("bump alpha drops: %v", err)
	}
	// The bus ingest wrapper advances the mark on every stored frame
	// (internal/bus/ingest_store.go); mirror it here.
	if err := database.SetRemoteLastEventID(ctx, m.db, "sched-alpha", "sched-alpha-00000000000000000009"); err != nil {
		t.Fatalf("set alpha mark: %v", err)
	}
	if _, err := database.IngestRemoteEvent(ctx, m.db, &database.RemoteEvent{
		SchedulerID: "sched-beta", EventID: "sched-beta-00000000000000000012",
		Kind: "tick.terminal", Project: "proj-b", TickID: "t-12", Status: "completed",
		TS: now, IngestedAt: now, Source: "live",
	}); err != nil {
		t.Fatalf("ingest beta: %v", err)
	}
	if err := database.SetRemoteLastEventID(ctx, m.db, "sched-beta", "sched-beta-00000000000000000012"); err != nil {
		t.Fatalf("set beta mark: %v", err)
	}
	return "sched-alpha", "sched-beta"
}

func TestMCP_PeersEvents_AggregateFormsUsable(t *testing.T) {
	m := newMCPTestServer(t)
	alpha, beta := seedPeerEvents(t, m)

	// All three advertised aggregate shapes must succeed with the SAME
	// union slice — no -32000, no error of any kind.
	for _, args := range []map[string]interface{}{
		{},                    // the advertised default: omit id
		{"id": "*"},           // the advertised wildcard
		{"scheduler_id": "*"}, // the alias the old error named
	} {
		text, err := callTool(t, m, 1, "peers_events", args)
		if err != nil {
			t.Fatalf("peers_events %v: unexpected error %q — the advertised all-schedulers form must succeed", args, err.Message)
		}
		var out map[string]interface{}
		if e := json.Unmarshal([]byte(text), &out); e != nil {
			t.Fatalf("peers_events %v: payload not JSON: %v (%q)", args, e, text)
		}
		if got := out["scheduler_id"]; got != "*" {
			t.Errorf("peers_events %v: scheduler_id = %v, want \"*\"", args, got)
		}
		// Union state: the newest event carries beta's LATER counter (12 > 9);
		// the greatest mark is beta's; drops sum across schedulers.
		last, _ := out["last_event"].(map[string]interface{})
		if last == nil {
			t.Fatalf("peers_events %v: last_event = nil, want the newest union event", args)
		}
		if id, _ := last["event_id"].(string); id != beta+"-00000000000000000012" {
			t.Errorf("peers_events %v: last_event.event_id = %q, want beta's (newest ingested)", args, id)
		}
		if id, _ := out["last_event_id"].(string); id != beta+"-00000000000000000012" {
			t.Errorf("peers_events %v: last_event_id = %q, want beta's mark (aggregate MAX)", args, id)
		}
		if drops, _ := out["dedupe_drops"].(float64); drops != 3 {
			t.Errorf("peers_events %v: dedupe_drops = %v, want 3 (summed)", args, drops)
		}
		_ = alpha
	}

	// The no-events empty state is typed exactly as docs render it for the
	// per-peer route: last_event null (key present), "" mark, 0 drops.
	m2 := newMCPTestServer(t)
	text, err := callTool(t, m2, 1, "peers_events", map[string]interface{}{})
	if err != nil {
		t.Fatalf("empty-store aggregate: %v", err.Message)
	}
	var empty map[string]interface{}
	if e := json.Unmarshal([]byte(text), &empty); e != nil {
		t.Fatalf("empty-store payload not JSON: %v", e)
	}
	if _, present := empty["last_event"]; !present {
		t.Errorf("empty-store aggregate: last_event key absent — it must be present and null, never omitted")
	}
	if empty["last_event"] != nil {
		t.Errorf("empty-store aggregate: last_event = %v, want nil", empty["last_event"])
	}
	if empty["last_event_id"] != "" || empty["dedupe_drops"] != float64(0) {
		t.Errorf("empty-store aggregate: last_event_id/dedupe_drops = %v/%v, want \"\"/0", empty["last_event_id"], empty["dedupe_drops"])
	}
}

func TestMCP_PeersEvents_RegisteredAndUnknownPeers(t *testing.T) {
	m := newMCPTestServer(t)
	seedPeerEvents(t, m)

	// Happy path: a REGISTERED id answers its own slice — preserved.
	text, err := callTool(t, m, 1, "peers_events", map[string]interface{}{"id": "sched-alpha"})
	if err != nil {
		t.Fatalf("registered peer: %v", err.Message)
	}
	var out map[string]interface{}
	if e := json.Unmarshal([]byte(text), &out); e != nil {
		t.Fatalf("registered peer payload not JSON: %v", e)
	}
	if out["scheduler_id"] != "sched-alpha" {
		t.Errorf("registered peer: scheduler_id = %v, want sched-alpha", out["scheduler_id"])
	}
	if last, _ := out["last_event"].(map[string]interface{}); last != nil {
		if id, _ := last["event_id"].(string); id != "sched-alpha-00000000000000000009" {
			t.Errorf("registered peer: last_event.event_id = %q, want alpha's", id)
		}
	} else {
		t.Errorf("registered peer: last_event = nil, want alpha's event")
	}
	if mark, _ := out["last_event_id"].(string); mark != "sched-alpha-00000000000000000009" {
		t.Errorf("registered peer: last_event_id = %q, want alpha's mark", mark)
	}
	if mark, err2 := callTool(t, m, 2, "peers_events", map[string]interface{}{"id": "sched-alpha"}); err2 != nil || mark == "" {
		t.Errorf("registered peer repeat call unstable: %q %v", mark, err2)
	}

	// Negative arm preserved: an unknown id still names the register step.
	_, err = callTool(t, m, 3, "peers_events", map[string]interface{}{"id": "nope"})
	if err == nil {
		t.Fatalf("unknown peer: want the register-it error")
	}
	if !strings.Contains(err.Message, "unknown peer") || !strings.Contains(err.Message, "peers_register") {
		t.Errorf("unknown peer: message %q, want unknown-peer + register guidance", err.Message)
	}
}

func TestMCP_PeersEvents_SchemaCarriesNamedParameters(t *testing.T) {
	m := newMCPTestServer(t)
	// Board acceptance: every parameter an error message names must exist
	// in the advertised schema. Read the LIVE tools/list entry.
	_, resp := m.call(t, map[string]interface{}{
		"jsonrpc": "2.0", "id": 1, "method": "tools/list",
	})
	result, _ := resp.Result.(map[string]interface{})
	tools, _ := result["tools"].([]interface{})
	var schema map[string]interface{}
	for _, raw := range tools {
		tl, _ := raw.(map[string]interface{})
		if tl["name"] == "peers_events" {
			schema, _ = tl["inputSchema"].(map[string]interface{})
			break
		}
	}
	if schema == nil {
		t.Fatalf("peers_events not found in live tools/list")
	}
	props, _ := schema["properties"].(map[string]interface{})
	for _, name := range []string{"id", "scheduler_id"} {
		if _, ok := props[name]; !ok {
			t.Errorf("inputSchema.properties missing %q — an error message names it, so a client must be able to supply it", name)
		}
	}
	if desc, _ := schema["properties"].(map[string]interface{})["id"].(map[string]interface{})["description"].(string); !strings.Contains(desc, "*") {
		t.Errorf("id description %q must keep advertising the '*' wildcard", desc)
	}
}
