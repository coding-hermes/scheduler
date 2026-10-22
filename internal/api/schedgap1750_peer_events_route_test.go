package api_test

// SCHED-GAP-1750 — the REST arm of the peers_events fix. Before it,
// GET /api/v1/peers/events answered 404 {"error":"unknown peer: events"}:
// the subtree dispatcher TrimSuffix'd a path with no suffix to strip and
// looked the residue up as a peer id (the live defect docs/api.md §14 had
// carried as "known" since 2026-10-19), and a 500 ("remote last event id:
// scheduler_id required") lay behind the auth gate behind that. These
// cells pin the fleet-wide slice (200, scheduler_id "*", union state) and
// the preserved per-scheduler route, over the real HTTP surface with a
// populated store — plus empty-state typing (last_event null key-present,
// "" mark, 0 drops) and the preserved not-found arm.

import (
	"context"
	"testing"
	"time"

	"github.com/coding-hermes/scheduler/internal/database"
)

// seedAPIPeerEvents registers two peers, ingests one durable event each,
// advances both marks (as the bus ingest wrapper does) and bumps one
// dedupe counter — the union the fleet-wide slice must report.
func seedAPIPeerEvents(t *testing.T, a *apiTestServer) {
	t.Helper()
	ctx := context.Background()
	for _, p := range []*database.Peer{
		{ID: "sched-alpha", URL: "http://alpha:8080"},
		{ID: "sched-beta", URL: "http://beta:8080"},
	} {
		if err := database.UpsertPeer(ctx, a.db, p); err != nil {
			t.Fatalf("UpsertPeer %s: %v", p.ID, err)
		}
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	for _, e := range []*database.RemoteEvent{
		{SchedulerID: "sched-alpha", EventID: "sched-alpha-00000000000000000009",
			Kind: "tick.terminal", Project: "proj-a", TickID: "t-9", Status: "completed",
			TS: now, IngestedAt: now, Source: "live"},
		{SchedulerID: "sched-beta", EventID: "sched-beta-00000000000000000012",
			Kind: "tick.terminal", Project: "proj-b", TickID: "t-12", Status: "completed",
			TS: now, IngestedAt: now, Source: "live"},
	} {
		if _, err := database.IngestRemoteEvent(ctx, a.db, e); err != nil {
			t.Fatalf("ingest %s: %v", e.SchedulerID, err)
		}
		if err := database.SetRemoteLastEventID(ctx, a.db, e.SchedulerID, e.EventID); err != nil {
			t.Fatalf("set mark %s: %v", e.SchedulerID, err)
		}
	}
	if err := database.BumpRemoteDedupeDrops(ctx, a.db, "sched-alpha", 3); err != nil {
		t.Fatalf("bump alpha drops: %v", err)
	}
}

func TestAPI_PeerEvents_FleetWideSlice(t *testing.T) {
	a := newAPITestServer(t)
	seedAPIPeerEvents(t, a)

	// The aggregate route answers 200 — not 404 "unknown peer: events",
	// not 500 "scheduler_id required".
	status, body := a.do(t, "GET", "/api/v1/peers/events", nil)
	if status != 200 {
		t.Fatalf("GET /api/v1/peers/events status = %d, want 200 (body %v)", status, body)
	}
	if sid, _ := body["scheduler_id"].(string); sid != "*" {
		t.Errorf("scheduler_id = %v, want literal \"*\"", body["scheduler_id"])
	}
	last, _ := body["last_event"].(map[string]interface{})
	if last == nil {
		t.Fatalf("last_event = nil, want the newest union event")
	}
	if id, _ := last["event_id"].(string); id != "sched-beta-00000000000000000012" {
		t.Errorf("last_event.event_id = %q, want beta's (newest ingested)", id)
	}
	if mark, _ := body["last_event_id"].(string); mark != "sched-beta-00000000000000000012" {
		t.Errorf("last_event_id = %q, want the aggregate MAX mark", mark)
	}
	if drops, _ := body["dedupe_drops"].(float64); drops != 3 {
		t.Errorf("dedupe_drops = %v, want 3 (summed)", body["dedupe_drops"])
	}

	// Per-scheduler route preserved.
	status, body = a.do(t, "GET", "/api/v1/peers/sched-alpha/events", nil)
	if status != 200 {
		t.Fatalf("per-scheduler route status = %d, want 200", status)
	}
	if body["scheduler_id"] != "sched-alpha" {
		t.Errorf("per-scheduler scheduler_id = %v, want sched-alpha", body["scheduler_id"])
	}
}

func TestAPI_PeerEvents_EmptyStoreAndUnknownPeer(t *testing.T) {
	a := newAPITestServer(t)

	// Empty store: the fleet-wide slice is still 200 with the typed
	// empty state — last_event present and null, "" mark, 0 drops.
	status, body := a.do(t, "GET", "/api/v1/peers/events", nil)
	if status != 200 {
		t.Fatalf("empty-store fleet-wide status = %d, want 200", status)
	}
	last, present := body["last_event"]
	if !present || last != nil {
		t.Errorf("empty-store last_event = %v (present=%v), want present and null", last, present)
	}
	if body["last_event_id"] != "" || body["dedupe_drops"] != float64(0) {
		t.Errorf("empty-store mark/drops = %v/%v, want \"\"/0", body["last_event_id"], body["dedupe_drops"])
	}

	// Unknown peer on the events slice: the preserved not-found arm
	// ("unknown peer: nope" with 404) — never a wrong-aggregate answer.
	status, body = a.do(t, "GET", "/api/v1/peers/nope/events", nil)
	if status != 404 {
		t.Errorf("unknown peer read status = %d, want 404 (body %v)", status, body)
	}
}
