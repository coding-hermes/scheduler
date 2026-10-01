package bus

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coding-hermes/scheduler/internal/database"
)

// REMOTE-006 (§5) bus-side acceptance: the durable ingest wrapper stores the
// SAME envelope once (duplicate = counted drop), replay after a simulated
// gap recovers the missed events through ReplaySinceGap/ReplayEvent, and a
// second replay is a no-op. The database is the migrated test template the
// database package ships (initTestDB in client_test.go).

// newRemote006DB opens a fresh, fully-migrated SQLite database (the same
// database.InitDB the daemon boots with — migrations, WAL, foreign keys).
func newRemote006DB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := database.InitDB(filepath.Join(t.TempDir(), "remote006.db"))
	if err != nil {
		t.Fatalf("init db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestIngestStoreDeduplicatesSameEnvelope(t *testing.T) {
	db := newRemote006DB(t)
	s := NewIngestStore(db)
	ctx := context.Background()
	ing := IngestedEvent{
		Envelope: Envelope{
			SchedulerID: "peer-alpha",
			EventID:     "peer-alpha-00000000000000000042",
			Kind:        KindTickTerminal,
			Project:     "proj-a",
			TickID:      "t-42",
			Status:      "completed",
			TS:          time.Now().UTC().Format(time.RFC3339Nano),
		},
		Topic:      TickTopic("peer-alpha"),
		Pattern:    "sched.tick.>",
		IngestedAt: time.Now().UTC(),
	}
	res1, err := s.IngestEvent(ctx, ing, database.RemoteSourceLive)
	if err != nil {
		t.Fatalf("first ingest: %v", err)
	}
	if !res1.Stored {
		t.Errorf("first ingest = %+v, want Stored", res1)
	}
	res2, err := s.IngestEvent(ctx, ing, database.RemoteSourceLive)
	if err != nil {
		t.Fatalf("duplicate ingest: %v (a dedupe drop is not an error)", err)
	}
	if !res2.DedupeDrop {
		t.Errorf("duplicate ingest = %+v, want DedupeDrop", res2)
	}
	// Exactly one durable row, and the high-water mark followed the event.
	var count int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM remote_events WHERE scheduler_id = 'peer-alpha'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Errorf("stored rows = %d, want exactly 1", count)
	}
	mark, err := database.RemoteLastEventID(ctx, db, "peer-alpha")
	if err != nil || mark != ing.EventID {
		t.Errorf("mark = %q (%v), want %q", mark, err, ing.EventID)
	}
}

func TestReplaySinceGapRecoversAndRepeatsAreNoOps(t *testing.T) {
	db := newRemote006DB(t)
	s := NewIngestStore(db)
	ctx := context.Background()

	mk := func(n int) IngestedEvent {
		return IngestedEvent{
			Envelope: Envelope{
				SchedulerID: "peer-beta",
				EventID:     mintEventID("peer-beta", n),
				Kind:        KindTickTerminal,
				Project:     "proj-b",
				TickID:      "t-b",
				Status:      "completed",
				TS:          time.Now().UTC().Format(time.RFC3339Nano),
			},
			Topic:      TickTopic("peer-beta"),
			Pattern:    "sched.tick.>",
			IngestedAt: time.Now().UTC().Add(time.Duration(n) * time.Second),
		}
	}
	ingest := func(ing IngestedEvent, source string) RemoteIngestResult {
		t.Helper()
		res, err := s.IngestEvent(ctx, ing, source)
		if err != nil {
			t.Fatalf("ingest %s: %v", ing.EventID, err)
		}
		return res
	}

	// Connected: events 1-2 live; the mark rides the live ingests.
	if !ingest(mk(1), database.RemoteSourceLive).Stored {
		t.Fatal("event 1 must store")
	}
	if !ingest(mk(2), database.RemoteSourceLive).Stored {
		t.Fatal("event 2 must store")
	}
	mark, err := database.RemoteLastEventID(ctx, db, "peer-beta")
	if err != nil {
		t.Fatal(err)
	}
	// The mark auto-advanced to the newest live event (42-width ids: #2).
	if want := mintEventID("peer-beta", 2); mark != want {
		t.Fatalf("mark = %q, want %q (auto-advance)", mark, want)
	}

	// DISCONNECTED: events 3-5 are stored (spooled at the primary) but the
	// caller's mark-advance duty is the same wrapper — to simulate a true
	// gap we hold the mark at #2 by advancing through ReplayEvent semantics:
	// store the events WITHOUT the auto-advance by writing rows directly.
	for n := 3; n <= 5; n++ {
		ev := mk(n)
		if _, err := database.IngestRemoteEvent(ctx, db, &database.RemoteEvent{
			SchedulerID: ev.SchedulerID, EventID: ev.EventID, Kind: ev.Kind,
			Project: ev.Project, TickID: ev.TickID, Status: ev.Status,
			TS: ev.TS, Topic: ev.Topic, Pattern: ev.Pattern,
			IngestedAt: ev.IngestedAt.Format(time.RFC3339Nano),
			Source:     database.RemoteSourceLive,
		}); err != nil {
			t.Fatalf("spool store %d: %v", n, err)
		}
	}
	// The gap is real: the mark still names #2 while 5 rows exist.
	if n, _ := database.RemoteEventCount(ctx, db, "peer-beta"); n != 5 {
		t.Fatalf("stored rows = %d, want 5", n)
	}

	// RESUBSCRIBE: replay recovers exactly the missed interval (3,4,5).
	missed, since, err := s.ReplaySinceGap(ctx, "peer-beta", 0)
	if err != nil {
		t.Fatalf("replay since gap: %v", err)
	}
	if since != mark {
		t.Errorf("replay since = %q, want the stored mark %q", since, mark)
	}
	if len(missed) != 3 {
		t.Fatalf("replay recovered %d events, want exactly 3 (the gap)", len(missed))
	}
	var rowsBefore int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM remote_events WHERE scheduler_id = 'peer-beta'`).Scan(&rowsBefore); err != nil {
		t.Fatal(err)
	}
	for _, ev := range missed {
		res, err := s.ReplayEvent(ctx, ev)
		if err != nil {
			t.Fatalf("replay %s: %v", ev.EventID, err)
		}
		if !res.DedupeDrop {
			t.Errorf("replay %s = %+v, want DedupeDrop (rows are keyed)", ev.EventID, res)
		}
	}
	// Drops during replay are COUNTED, not silent.
	if drops, _ := database.CountRemoteEventDedupeDrops(ctx, db, "peer-beta"); drops != 3 {
		t.Errorf("counted replay drops = %d, want 3", drops)
	}
	// The mark advanced to the newest replayed id (#5).
	want5 := mintEventID("peer-beta", 5)
	if m, _ := database.RemoteLastEventID(ctx, db, "peer-beta"); m != want5 {
		t.Errorf("mark after replay = %q, want %q", m, want5)
	}

	// SECOND REPLAY: a no-op — nothing above the mark, no new rows.
	again, _, err := s.ReplaySinceGap(ctx, "peer-beta", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != 0 {
		t.Errorf("second replay recovered %d events, want 0", len(again))
	}
	var rowsAfter int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM remote_events WHERE scheduler_id = 'peer-beta'`).Scan(&rowsAfter); err != nil {
		t.Fatal(err)
	}
	if rowsAfter != rowsBefore {
		t.Errorf("rows before/after second replay = %d/%d, want equal", rowsBefore, rowsAfter)
	}
	// Replaying the FULL history re-drops everything and adds nothing.
	full, err := database.RemoteReplayEvents(ctx, db, "peer-beta", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range full {
		if _, err := s.ReplayEvent(ctx, ev); err != nil {
			t.Fatalf("full re-replay %s: %v", ev.EventID, err)
		}
	}
	var rowsFinal int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM remote_events WHERE scheduler_id = 'peer-beta'`).Scan(&rowsFinal); err != nil {
		t.Fatal(err)
	}
	if rowsFinal != rowsBefore {
		t.Errorf("rows after full re-replay = %d, want %d (idempotent)", rowsFinal, rowsBefore)
	}
}

// mintEventID mirrors the client's NextEventID shape ("%s-%020d") without a
// Client instance — the store keys on the pair, but the replay policy orders
// by the counter tail, so the tests must mint the real shape.
func mintEventID(schedulerID string, n int) string {
	return schedulerID + "-" + padCounter(n)
}

func padCounter(n int) string {
	const digits = 20
	s := ""
	for n > 0 {
		s = string(rune('0'+n%10)) + s
		n /= 10
	}
	for len(s) < digits {
		s = "0" + s
	}
	if s == "" {
		s = strings.Repeat("0", digits)
	}
	return s
}
