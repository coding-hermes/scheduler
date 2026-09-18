package database

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

// CTL-002 — event fan-out. LogEvent must publish every committed row to the
// subscribers of its database, must never block a writer on a slow subscriber,
// and must keep subscribers of one database isolated from another's.

func hubTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := InitDB(filepath.Join(t.TempDir(), "scheduler.db"))
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func logHubEvent(t *testing.T, db *sql.DB, msg string) Event {
	t.Helper()
	ev := Event{Severity: SeverityInfo, Component: "hubtest", Message: msg, Details: "{}"}
	if err := LogEvent(context.Background(), db, &ev); err != nil {
		t.Fatalf("LogEvent(%q): %v", msg, err)
	}
	return ev
}

func recvHubEvent(t *testing.T, ch <-chan Event, timeout time.Duration) (Event, bool) {
	t.Helper()
	select {
	case ev, ok := <-ch:
		return ev, ok
	case <-time.After(timeout):
		return Event{}, false
	}
}

// TestEventsHub_PublishesCommittedRow pins the publish-after-commit contract:
// a subscriber receives the row with the database-assigned ID.
func TestEventsHub_PublishesCommittedRow(t *testing.T) {
	db := hubTestDB(t)
	ch, cancel := SubscribeEvents(db, 4)
	defer cancel()

	if got := SubscriberCount(db); got != 1 {
		t.Fatalf("SubscriberCount = %d, want 1", got)
	}

	written := logHubEvent(t, db, "published")
	got, ok := recvHubEvent(t, ch, time.Second)
	if !ok {
		t.Fatal("no event published to the subscriber")
	}
	if got.ID != written.ID || got.Message != "published" || got.Component != "hubtest" {
		t.Errorf("published event = %+v, want id %d message published", got, written.ID)
	}
}

// TestEventsHub_FailedInsertDoesNotPublish: the CHECK constraint rejects a bad
// severity, and a failed write must not reach subscribers.
func TestEventsHub_FailedInsertDoesNotPublish(t *testing.T) {
	db := hubTestDB(t)
	ch, cancel := SubscribeEvents(db, 4)
	defer cancel()

	bad := Event{Severity: EventSeverity("BOGUS"), Component: "hubtest", Message: "nope", Details: "{}"}
	if err := LogEvent(context.Background(), db, &bad); err == nil {
		t.Fatal("LogEvent with an invalid severity succeeded, want a CHECK-constraint error")
	}
	if ev, ok := recvHubEvent(t, ch, 100*time.Millisecond); ok {
		t.Errorf("failed insert published %+v, want nothing", ev)
	}
}

// TestEventsHub_FullBufferDropsInsteadOfBlocking is the back-pressure
// guarantee: a subscriber that never drains its (small) buffer makes LogEvent
// drop events, never block the writer.
func TestEventsHub_FullBufferDropsInsteadOfBlocking(t *testing.T) {
	db := hubTestDB(t)
	ch, cancel := SubscribeEvents(db, 1)
	defer cancel()

	var slowest time.Duration
	for i := 0; i < 50; i++ {
		start := time.Now()
		logHubEvent(t, db, "burst")
		if d := time.Since(start); d > slowest {
			slowest = d
		}
	}
	if slowest > time.Second {
		t.Errorf("slowest LogEvent with a full subscriber buffer = %s, want < 1s (a blocking publish would hang)", slowest)
	}
	// The one-deep buffer holds exactly the first event; the other 49 were
	// dropped instead of queueing.
	if got := len(ch); got != 1 {
		t.Errorf("pending events = %d, want 1 (bounded buffer)", got)
	}
	if ev, ok := recvHubEvent(t, ch, time.Second); !ok || ev.Message != "burst" {
		t.Errorf("buffered event = %+v (ok=%v), want the first burst event", ev, ok)
	}
}

// TestEventsHub_SlowSubscriberDoesNotStarveFastOne: the drop is per
// subscriber — a healthy subscriber still receives the whole burst.
func TestEventsHub_SlowSubscriberDoesNotStarveFastOne(t *testing.T) {
	db := hubTestDB(t)
	slow, slowCancel := SubscribeEvents(db, 1)
	defer slowCancel()
	fast, fastCancel := SubscribeEvents(db, 128)
	defer fastCancel()

	if got := SubscriberCount(db); got != 2 {
		t.Fatalf("SubscriberCount = %d, want 2", got)
	}
	const burst = 64
	for i := 0; i < burst; i++ {
		logHubEvent(t, db, "burst")
	}

	for i := 0; i < burst; i++ {
		if _, ok := recvHubEvent(t, fast, time.Second); !ok {
			t.Fatalf("fast subscriber received %d of %d events", i, burst)
		}
	}
	if got := len(slow); got != 1 {
		t.Errorf("slow subscriber pending = %d, want 1", got)
	}
}

// TestEventsHub_UnsubscribeReleasesHubEntry: cancel removes the subscription
// (and the per-database hub once it is empty), is idempotent, and publishing
// after unsubscribe is a safe no-op.
func TestEventsHub_UnsubscribeReleasesHubEntry(t *testing.T) {
	db := hubTestDB(t)
	_, cancel := SubscribeEvents(db, 4)
	if got := SubscriberCount(db); got != 1 {
		t.Fatalf("SubscriberCount = %d, want 1", got)
	}
	cancel()
	cancel() // idempotent
	if got := SubscriberCount(db); got != 0 {
		t.Fatalf("SubscriberCount after cancel = %d, want 0", got)
	}

	// No subscribers: the write path must not panic and must still commit.
	logHubEvent(t, db, "after-unsubscribe")
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM events`).Scan(&n); err != nil {
		t.Fatalf("count events: %v", err)
	}
	if n != 1 {
		t.Errorf("events row count = %d, want 1", n)
	}
}

// TestEventsHub_NoCrossDatabaseLeakage: a subscriber only sees events written
// to the database it subscribed to.
func TestEventsHub_NoCrossDatabaseLeakage(t *testing.T) {
	dbA := hubTestDB(t)
	dbB := hubTestDB(t)
	fromA, cancelA := SubscribeEvents(dbA, 4)
	defer cancelA()
	fromB, cancelB := SubscribeEvents(dbB, 4)
	defer cancelB()

	logHubEvent(t, dbB, "for-b")

	if _, ok := recvHubEvent(t, fromA, 100*time.Millisecond); ok {
		t.Error("subscriber of db A received an event written to db B")
	}
	ev, ok := recvHubEvent(t, fromB, time.Second)
	if !ok || ev.Message != "for-b" {
		t.Errorf("db B subscriber got %+v (ok=%v), want the for-b event", ev, ok)
	}
	if got := SubscriberCount(dbA); got != 1 {
		t.Errorf("SubscriberCount(dbA) = %d, want 1", got)
	}
}
