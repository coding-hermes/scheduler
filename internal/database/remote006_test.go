package database

import (
	"context"
	"errors"
	"testing"
	"time"
)

// REMOTE-006 acceptance tests (§5): durable idempotent ingest (same envelope
// twice → exactly one row + one counted dedupe drop), spooled replay after a
// simulated gap (recovers the missed events; replaying again is a no-op),
// and the monotone last-event mark.

func TestMigrationV58RemoteEvents(t *testing.T) {
	if latestMigration < 58 {
		t.Fatalf("latestMigration = %d, want >= 58 (REMOTE-006 lands v58 remote_events)", latestMigration)
	}
	db := newTestDB(t)
	ctx := context.Background()
	var n int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name IN ('remote_events','remote_events_stats')`,
	).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("v58 tables missing: found %d of (remote_events, remote_events_stats)", n)
	}
	var desc string
	if err := db.QueryRowContext(ctx, `SELECT desc FROM migrations WHERE version = 58`).Scan(&desc); err != nil {
		t.Fatalf("v58 ledger row missing: %v", err)
	}
	if !contains(desc, "REMOTE-006") {
		t.Errorf("v58 desc = %q, want it to name REMOTE-006", desc)
	}
}

// Acceptance 2: the SAME envelope ingested twice yields exactly ONE stored
// row, and the second observation reports one dedupe-drop (not an error).
func TestIngestRemoteEventDeduplicates(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	env := &RemoteEvent{
		SchedulerID: "peer-alpha",
		EventID:     "peer-alpha-00000000000000000001",
		Kind:        "tick.terminal",
		Project:     "proj-a",
		TickID:      "t-1",
		Status:      "completed",
		TS:          now,
		IngestedAt:  now,
	}
	res1, err := IngestRemoteEvent(ctx, db, env)
	if err != nil {
		t.Fatalf("first ingest: %v", err)
	}
	if !res1.Stored || res1.DedupeDrop {
		t.Errorf("first ingest = %+v, want Stored", res1)
	}
	res2, err := IngestRemoteEvent(ctx, db, env)
	if err != nil {
		t.Fatalf("second ingest: %v (dedupe must never be an error)", err)
	}
	if !res2.DedupeDrop || res2.Stored {
		t.Errorf("second ingest = %+v, want DedupeDrop", res2)
	}
	var count int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM remote_events WHERE scheduler_id = 'peer-alpha'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Errorf("stored rows = %d, want exactly 1 after duplicate ingest", count)
	}
	// A different event id from the same scheduler stores independently —
	// the key is the PAIR, not the scheduler.
	env.EventID = "peer-alpha-00000000000000000002"
	res3, err := IngestRemoteEvent(ctx, db, env)
	if err != nil {
		t.Fatalf("third ingest: %v", err)
	}
	if !res3.Stored {
		t.Errorf("third ingest = %+v, want Stored (different event id)", res3)
	}
	// An empty key half is a loud refusal, not a silent 200.
	if _, err := IngestRemoteEvent(ctx, db, &RemoteEvent{EventID: "x-1", IngestedAt: now}); !errors.Is(err, ErrRemoteEventInvalid) {
		t.Errorf("empty scheduler_id: err = %v, want ErrRemoteEventInvalid", err)
	}
}

// Acceptance 3: replay after a simulated gap recovers the missed events, and
// replaying again is a no-op (idempotent).
func TestReplayAfterGapIsIdempotent(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	stamp := func(min int) string {
		return time.Now().UTC().Add(time.Duration(min) * time.Minute).Format(time.RFC3339Nano)
	}
	id := func(n int) string {
		return "peer-beta-0000000000000000000" + string(rune('0'+n))
	}
	ingest := func(n int) RemoteIngestResult {
		t.Helper()
		res, err := IngestRemoteEvent(ctx, db, &RemoteEvent{
			SchedulerID: "peer-beta",
			EventID:     id(n),
			Kind:        "tick.terminal",
			Project:     "proj-b",
			TickID:      "t-b",
			Status:      "completed",
			IngestedAt:  stamp(n),
		})
		if err != nil {
			t.Fatalf("ingest %d: %v", n, err)
		}
		return res
	}

	// Phase 1: connected. Events 1-2 arrive live; the mark follows.
	if !ingest(1).Stored || !ingest(2).Stored {
		t.Fatal("live events 1-2 must store")
	}
	if err := SetRemoteLastEventID(ctx, db, "peer-beta", id(2)); err != nil {
		t.Fatal(err)
	}
	mark, err := RemoteLastEventID(ctx, db, "peer-beta")
	if err != nil || mark != id(2) {
		t.Fatalf("mark after live = %q (%v), want %s", mark, err, id(2))
	}

	// Phase 2: DISCONNECTED — events 3-5 happen at the peer while we store
	// them (spool semantics: they land in the store with no mark advance
	// past 2). The gap = everything above the mark.
	for n := 3; n <= 5; n++ {
		if !ingest(n).Stored {
			t.Fatalf("spooled event %d must store", n)
		}
	}

	// Phase 3: RESUBSCRIBE — replay everything above the mark.
	missed, err := RemoteReplayEvents(ctx, db, "peer-beta", mark, 0)
	if err != nil {
		t.Fatalf("replay select: %v", err)
	}
	if len(missed) != 3 {
		t.Fatalf("replay recovered %d events, want exactly the 3 missed (3,4,5)", len(missed))
	}
	for i, ev := range missed {
		want := id(3 + i)
		if ev.EventID != want {
			t.Errorf("replay[%d] = %s, want %s (deterministic order)", i, ev.EventID, want)
		}
	}

	// Re-ingesting the replay marks them replay-source rows; the mark
	// advances to the newest replayed id.
	for _, ev := range missed {
		res, err := IngestRemoteEvent(ctx, db, &RemoteEvent{
			SchedulerID: ev.SchedulerID, EventID: ev.EventID, Kind: ev.Kind,
			Project: ev.Project, TickID: ev.TickID, Status: ev.Status,
			TS: ev.TS, Topic: ev.Topic, Pattern: ev.Pattern,
			IngestedAt: ev.IngestedAt, Source: RemoteSourceReplay,
		})
		if err != nil {
			t.Fatalf("replay ingest %s: %v", ev.EventID, err)
		}
		if !res.DedupeDrop {
			t.Errorf("replay ingest of stored %s = %+v, want DedupeDrop (idempotent replay)", ev.EventID, res)
		}
	}
	if err := SetRemoteLastEventID(ctx, db, "peer-beta", missed[len(missed)-1].EventID); err != nil {
		t.Fatal(err)
	}

	// Phase 4: REPLAY AGAIN — a no-op. The mark covers everything; the
	// replay selects nothing and the row count is unchanged.
	var rowsBefore, rowsAfter int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM remote_events WHERE scheduler_id = 'peer-beta'`).Scan(&rowsBefore); err != nil {
		t.Fatal(err)
	}
	mark2, _ := RemoteLastEventID(ctx, db, "peer-beta")
	again, err := RemoteReplayEvents(ctx, db, "peer-beta", mark2, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != 0 {
		t.Errorf("second replay recovered %d events, want 0 (replay is a no-op after the first)", len(again))
	}
	// Even a FULL re-replay (mark ignored) adds no rows — keyed inserts drop.
	full, err := RemoteReplayEvents(ctx, db, "peer-beta", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range full {
		if _, err := IngestRemoteEvent(ctx, db, &RemoteEvent{
			SchedulerID: ev.SchedulerID, EventID: ev.EventID, Kind: ev.Kind,
			Project: ev.Project, TickID: ev.TickID, Status: ev.Status,
			IngestedAt: ev.IngestedAt, Source: RemoteSourceReplay,
		}); err != nil {
			t.Fatalf("full replay ingest %s: %v", ev.EventID, err)
		}
	}
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM remote_events WHERE scheduler_id = 'peer-beta'`).Scan(&rowsAfter); err != nil {
		t.Fatal(err)
	}
	if rowsAfter != rowsBefore {
		t.Errorf("rows before/after full re-replay = %d/%d, want equal (no duplicates)", rowsBefore, rowsAfter)
	}
}

func TestRemoteLastEventIDMonotone(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	hi := "peer-gamma-00000000000000000010"
	lo := "peer-gamma-00000000000000000004"
	if err := SetRemoteLastEventID(ctx, db, "peer-gamma", hi); err != nil {
		t.Fatal(err)
	}
	// A lagging write (stale replay racing live) must never pull it back.
	if err := SetRemoteLastEventID(ctx, db, "peer-gamma", lo); err != nil {
		t.Fatal(err)
	}
	mark, err := RemoteLastEventID(ctx, db, "peer-gamma")
	if err != nil {
		t.Fatal(err)
	}
	if mark != hi {
		t.Errorf("mark = %q after lagging write %q, want %q (monotone)", mark, lo, hi)
	}
	// A foreign (non-counter) id never moves a counter-form mark.
	if err := SetRemoteLastEventID(ctx, db, "peer-gamma", "weird-id"); err != nil {
		t.Fatal(err)
	}
	if mark, _ := RemoteLastEventID(ctx, db, "peer-gamma"); mark != hi {
		t.Errorf("mark = %q after non-counter write, want %q", mark, hi)
	}
}

func TestRemotePeersWithLastEvent(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	now := time.Now()
	// Peer with traffic, peer registered-never-seen.
	if err := UpsertPeer(ctx, db, &Peer{ID: "peer-delta", URL: "http://d:1"}); err != nil {
		t.Fatal(err)
	}
	if err := UpsertPeer(ctx, db, &Peer{ID: "peer-echo"}); err != nil {
		t.Fatal(err)
	}
	if err := PeerHeartbeat(ctx, db, "peer-delta"); err != nil {
		t.Fatal(err)
	}
	if _, err := IngestRemoteEvent(ctx, db, &RemoteEvent{
		SchedulerID: "peer-delta",
		EventID:     "peer-delta-00000000000000000001",
		Kind:        "tick.terminal",
		Project:     "proj-d",
		TickID:      "t-d",
		Status:      "completed",
		IngestedAt:  now.UTC().Format(time.RFC3339Nano),
	}); err != nil {
		t.Fatal(err)
	}
	rows, err := RemotePeersWithLastEvent(ctx, db, PeerFreshnessWindowDefault, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(rows))
	}
	byID := map[string]RemotePeerLastEvent{}
	for _, r := range rows {
		byID[r.Peer.ID] = r
	}
	d := byID["peer-delta"]
	if d.LastEvent == nil || d.LastEvent.EventID != "peer-delta-00000000000000000001" {
		t.Errorf("peer-delta last event = %+v, want the ingested event", d.LastEvent)
	}
	if d.Stale {
		t.Errorf("peer-delta heartbeated now — must be fresh, got stale=true")
	}
	e := byID["peer-echo"]
	if e.LastEvent != nil {
		t.Errorf("peer-echo has no events; last = %+v, want nil", e.LastEvent)
	}
	if !e.Stale {
		t.Errorf("peer-echo never heartbeated — must render stale (never down)")
	}
	if e.Peer.LastContact != "" {
		t.Errorf("peer-echo last_contact = %q, want \"\" (never fabricated)", e.Peer.LastContact)
	}
}

func TestCountRemoteEventDedupeDrops(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	if n, err := CountRemoteEventDedupeDrops(ctx, db, ""); err != nil || n != 0 {
		t.Fatalf("fresh store drops = %d (%v), want 0", n, err)
	}
	if err := BumpRemoteDedupeDrops(ctx, db, "peer-zeta", 2); err != nil {
		t.Fatal(err)
	}
	if err := BumpRemoteDedupeDrops(ctx, db, "peer-zeta", 1); err != nil {
		t.Fatal(err)
	}
	if err := BumpRemoteDedupeDrops(ctx, db, "peer-other", 5); err != nil {
		t.Fatal(err)
	}
	if n, _ := CountRemoteEventDedupeDrops(ctx, db, "peer-zeta"); n != 3 {
		t.Errorf("peer-zeta drops = %d, want 3", n)
	}
	if n, _ := CountRemoteEventDedupeDrops(ctx, db, ""); n != 8 {
		t.Errorf("fleet drops = %d, want 8", n)
	}
}
