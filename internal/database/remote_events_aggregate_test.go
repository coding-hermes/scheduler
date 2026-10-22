package database

// SCHED-GAP-1750 — the aggregate arms of the REMOTE-006 reads. Before this
// fix RemoteLastEventID hard-rejected schedulerID "" with
// "remote last event id: scheduler_id required", which broke the MCP tool's
// advertised no-id form AND masked a 500 behind the REST fleet-wide route
// (itself 404ing on the dispatch bug). The convention elsewhere in this
// file (CountRemoteEventDedupeDrops) is that "" = aggregate; these cells
// pin the same shape on RemoteLastEventID and RemoteLastEvent, plus the
// preserved per-scheduler reads and SetRemoteLastEventID's unchanged
// no-empty-id guard (a mark write must never be attributed to nobody).

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"
)

func seedTwoSchedulers(t *testing.T, db *sql.DB) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	alpha := &RemoteEvent{
		SchedulerID: "sched-alpha", EventID: "sched-alpha-00000000000000000009",
		Kind: "tick.terminal", Project: "proj-a", TickID: "t-9", Status: "completed",
		TS: now, IngestedAt: now, Source: "live",
	}
	beta := &RemoteEvent{
		SchedulerID: "sched-beta", EventID: "sched-beta-00000000000000000012",
		Kind: "tick.terminal", Project: "proj-b", TickID: "t-12", Status: "completed",
		TS: now, IngestedAt: now, Source: "live",
	}
	for _, e := range []*RemoteEvent{alpha, beta} {
		if _, err := IngestRemoteEvent(ctx, db, e); err != nil {
			t.Fatalf("ingest %s: %v", e.SchedulerID, err)
		}
		if err := SetRemoteLastEventID(ctx, db, e.SchedulerID, e.EventID); err != nil {
			t.Fatalf("set mark %s: %v", e.SchedulerID, err)
		}
	}
	if err := BumpRemoteDedupeDrops(ctx, db, "sched-alpha", 3); err != nil {
		t.Fatalf("bump alpha: %v", err)
	}
}

// The aggregate mark is the MAX across schedulers; the per-scheduler reads
// are unchanged.
func TestRemoteLastEventIDAggregate(t *testing.T) {
	db := newTestDB(t)
	seedTwoSchedulers(t, db)
	ctx := context.Background()

	mark, err := RemoteLastEventID(ctx, db, "")
	if err != nil {
		t.Fatalf("aggregate mark: %v (the pre-fix daemon answered \"scheduler_id required\" here)", err)
	}
	if mark != "sched-beta-00000000000000000012" {
		t.Errorf("aggregate mark = %q, want beta's (greatest across schedulers)", mark)
	}
	if mark, _ := RemoteLastEventID(ctx, db, "sched-alpha"); mark != "sched-alpha-00000000000000000009" {
		t.Errorf("per-scheduler alpha mark = %q, want alpha's (arm preserved)", mark)
	}

	// Empty store: the aggregate is "" (the replay-from-scratch condition),
	// not an error.
	empty := newTestDB(t)
	if mark, err := RemoteLastEventID(context.Background(), empty, ""); err != nil || mark != "" {
		t.Errorf("empty-store aggregate mark = %q, %v; want \"\", nil", mark, err)
	}
}

// The aggregate last-event read is the newest ingested frame across all
// schedulers; the per-scheduler read is unchanged; the empty state is
// sql.ErrNoRows for the caller to render.
func TestRemoteLastEventAggregate(t *testing.T) {
	db := newTestDB(t)
	seedTwoSchedulers(t, db)
	ctx := context.Background()

	ev, err := RemoteLastEvent(ctx, db, "")
	if err != nil {
		t.Fatalf("aggregate last event: %v", err)
	}
	if ev.EventID != "sched-beta-00000000000000000012" {
		t.Errorf("aggregate last event = %s, want beta's (newest ingested)", ev.EventID)
	}
	if ev.SchedulerID != "sched-beta" {
		t.Errorf("aggregate last event scheduler = %s, want sched-beta", ev.SchedulerID)
	}
	if pev, err := RemoteLastEvent(ctx, db, "sched-alpha"); err != nil || pev.EventID != "sched-alpha-00000000000000000009" {
		t.Errorf("per-scheduler alpha read = %v, %v; want alpha's (arm preserved)", pev, err)
	}
	if _, err := RemoteLastEvent(context.Background(), newTestDB(t), ""); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("empty-store aggregate read = %v, want sql.ErrNoRows (caller renders)", err)
	}
}

// SetRemoteLastEventID still refuses an empty scheduler id: a mark write
// must never be attributed to nobody.
func TestSetRemoteLastEventIDStillRejectsEmptyID(t *testing.T) {
	db := newTestDB(t)
	err := SetRemoteLastEventID(context.Background(), db, "", "whatever-00000000000000000001")
	if err == nil {
		t.Fatalf("empty-id mark write accepted — the no-attribution guard is gone")
	}
	if want := "scheduler_id required"; !contains(err.Error(), want) {
		t.Errorf("empty-id write error = %q, want it to name %q", err.Error(), want)
	}
}
