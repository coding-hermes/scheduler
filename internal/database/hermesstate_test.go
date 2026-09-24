package database

// SCHED-GAP-089 tests for the REAL-schema reaper (hermesstate.go).
//
// Every fixture is a temp SQLite file shaped like the real Hermes state.db
// sessions table (representative column subset: the five the reaper touches).
// Nothing here opens the operator's live ~/.hermes/state.db. The clock is
// always injected via clock.WithClock — no test sleeps.

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coding-hermes/scheduler/internal/clock"
)

// reapFixtureClock is the fixed "now" every test builds rows around.
var reapFixtureClock = time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)

// newHermesStateFixture creates a temp SQLite database whose sessions table
// carries the real Hermes columns (a representative subset — the reaper
// fails closed on anything missing these five) and returns the open handle.
func newHermesStateFixture(t *testing.T) (*sql.DB, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("open fixture %s: %v", path, err)
	}
	t.Cleanup(func() { db.Close() })
	_, err = db.Exec(`CREATE TABLE sessions (
	    id              TEXT PRIMARY KEY,
	    source          TEXT NOT NULL,
	    started_at      REAL NOT NULL,
	    ended_at        REAL,
	    end_reason      TEXT,
	    last_activity_at REAL
	)`)
	if err != nil {
		t.Fatalf("create sessions fixture: %v", err)
	}
	return db, path
}

// reapCtx pins the fixture clock so every row age is deterministic.
func reapCtx() context.Context {
	return clock.WithClock(context.Background(), clock.NewFixed(reapFixtureClock))
}

// epoch converts a time into the state.db epoch-seconds REAL representation.
func epoch(t time.Time) float64 { return float64(t.UnixNano()) / 1e9 }

// reapRow is the full observed state of one fixture row.
type reapRow struct {
	id             string
	source         string
	startedAt      float64
	endedAt        sql.NullFloat64
	endReason      sql.NullString
	lastActivityAt sql.NullFloat64
}

func getReapRow(t *testing.T, db *sql.DB, id string) reapRow {
	t.Helper()
	var r reapRow
	err := db.QueryRow(`SELECT id, source, started_at, ended_at, end_reason, last_activity_at
	                    FROM sessions WHERE id = ?`, id).
		Scan(&r.id, &r.source, &r.startedAt, &r.endedAt, &r.endReason, &r.lastActivityAt)
	if err != nil {
		t.Fatalf("query session %q: %v", id, err)
	}
	return r
}

func seedReapRows(t *testing.T, db *sql.DB, rows []reapRow) {
	t.Helper()
	for _, r := range rows {
		var ended, endReason any
		if r.endedAt.Valid {
			ended = r.endedAt.Float64
		}
		if r.endReason.Valid {
			endReason = r.endReason.String
		}
		var last any
		if r.lastActivityAt.Valid {
			last = r.lastActivityAt.Float64
		}
		if _, err := db.Exec(`INSERT INTO sessions (id, source, started_at, ended_at, end_reason, last_activity_at)
		                      VALUES (?, ?, ?, ?, ?, ?)`,
			r.id, r.source, r.startedAt, ended, endReason, last); err != nil {
			t.Fatalf("insert session %q: %v", r.id, err)
		}
	}
}

// standardFixture is the shared row set (clock fixed at reapFixtureClock,
// so a 24h cutoff is reapFixtureClock-24h):
//
//	stale-api               REAP   (idle 30h, ended_at <- last_activity_at)
//	stale-api-null-activity REAP   (idle 40h, ended_at <- started_at fallback)
//	fresh-api               keep   (idle 0.5h)
//	stale-cron              keep   (stale but source != api_server)
//	ended-api               keep   (already closed by the agent itself)
func standardFixture(t *testing.T) (*sql.DB, string) {
	t.Helper()
	db, path := newHermesStateFixture(t)
	seedReapRows(t, db, []reapRow{
		{id: "stale-api", source: "api_server",
			startedAt: epoch(reapFixtureClock.Add(-50 * time.Hour)), lastActivityAt: sql.NullFloat64{Float64: epoch(reapFixtureClock.Add(-30 * time.Hour)), Valid: true}},
		{id: "stale-api-null-activity", source: "api_server",
			startedAt: epoch(reapFixtureClock.Add(-40 * time.Hour))},
		{id: "fresh-api", source: "api_server",
			startedAt: epoch(reapFixtureClock.Add(-1 * time.Hour)), lastActivityAt: sql.NullFloat64{Float64: epoch(reapFixtureClock.Add(-30 * time.Minute)), Valid: true}},
		{id: "stale-cron", source: "cron",
			startedAt: epoch(reapFixtureClock.Add(-40 * time.Hour)), lastActivityAt: sql.NullFloat64{Float64: epoch(reapFixtureClock.Add(-39 * time.Hour)), Valid: true}},
		{id: "ended-api", source: "api_server",
			startedAt: epoch(reapFixtureClock.Add(-48 * time.Hour)), endedAt: sql.NullFloat64{Float64: epoch(reapFixtureClock.Add(-47 * time.Hour)), Valid: true}, endReason: sql.NullString{String: "agent_close", Valid: true}},
	})
	return db, path
}

// assertReapRowEqual fails when any field of two snapshots differs.
func assertReapRowEqual(t *testing.T, phase, id string, want, got reapRow) {
	t.Helper()
	if want.source != got.source || want.startedAt != got.startedAt ||
		want.endedAt != got.endedAt || want.endReason != got.endReason ||
		want.lastActivityAt != got.lastActivityAt {
		t.Fatalf("%s: %s mutated: before %+v after %+v", phase, id, want, got)
	}
}

// expectEpochNear asserts a REAL column value within 1ms (float64 epoch
// representation is exact to ~256ns at current epochs; 1ms leaves headroom).
func expectEpochNear(t *testing.T, label string, got, want float64) {
	t.Helper()
	if math.Abs(got-want) > 1e-3 {
		t.Fatalf("%s = %v, want %v (±1ms)", label, got, want)
	}
}

// TestReapStaleHermesSessions_DryRunNoMutation: the zero-value config
// (Apply unset) is a dry-run — it reports exactly what it would close and
// provably mutates nothing.
func TestReapStaleHermesSessions_DryRunNoMutation(t *testing.T) {
	db, _ := standardFixture(t)
	ctx := reapCtx()

	ids := []string{"stale-api", "stale-api-null-activity", "fresh-api", "stale-cron", "ended-api"}
	before := make(map[string]reapRow, len(ids))
	for _, id := range ids {
		before[id] = getReapRow(t, db, id)
	}

	res, err := ReapStaleHermesSessions(ctx, db, HermesReaperConfig{StaleAfter: 24 * time.Hour})
	if err != nil {
		t.Fatalf("ReapStaleHermesSessions (dry-run): %v", err)
	}
	if res.Apply {
		t.Fatal("dry-run result reports Apply=true")
	}
	if res.Candidates != 2 {
		t.Fatalf("dry-run Candidates = %d, want 2", res.Candidates)
	}
	if res.Reaped != 0 {
		t.Fatalf("dry-run Reaped = %d, want 0 (no mutation)", res.Reaped)
	}
	// The stalest candidate is stale-api-null-activity (idle 40h).
	if want := 40 * time.Hour; math.Abs((res.OldestIdle - want).Hours()) > 0.01 {
		t.Fatalf("dry-run OldestIdle = %v, want ~%v", res.OldestIdle, want)
	}

	for _, id := range ids {
		assertReapRowEqual(t, "dry-run", id, before[id], getReapRow(t, db, id))
	}
}

// TestReapStaleHermesSessions_ApplyClosesStaleAPIServerOnly: an Apply pass
// closes exactly the stale open api_server rows — ended_at from the
// session's own last activity (started_at fallback when never recorded),
// end_reason 'reaped' — and preserves recent, non-api and already-ended rows.
func TestReapStaleHermesSessions_ApplyClosesStaleAPIServerOnly(t *testing.T) {
	db, _ := standardFixture(t)
	ctx := reapCtx()

	res, err := ReapStaleHermesSessions(ctx, db, HermesReaperConfig{StaleAfter: 24 * time.Hour, Apply: true})
	if err != nil {
		t.Fatalf("ReapStaleHermesSessions (apply): %v", err)
	}
	if !res.Apply {
		t.Fatal("apply pass reported Apply=false")
	}
	if res.Candidates != 2 || res.Reaped != 2 {
		t.Fatalf("apply pass Candidates=%d Reaped=%d, want 2/2", res.Candidates, res.Reaped)
	}

	// stale-api: closed at its own last_activity_at, never at reap time.
	got := getReapRow(t, db, "stale-api")
	if !got.endedAt.Valid {
		t.Fatal("stale-api still open after apply pass")
	}
	expectEpochNear(t, "stale-api.ended_at", got.endedAt.Float64, epoch(reapFixtureClock.Add(-30*time.Hour)))
	if got.endReason.String != EndReasonReaped {
		t.Fatalf("stale-api.end_reason = %q, want %q", got.endReason.String, EndReasonReaped)
	}

	// stale-api-null-activity: ended_at falls back to started_at.
	got = getReapRow(t, db, "stale-api-null-activity")
	if !got.endedAt.Valid {
		t.Fatal("stale-api-null-activity still open after apply pass")
	}
	expectEpochNear(t, "stale-api-null-activity.ended_at", got.endedAt.Float64, epoch(reapFixtureClock.Add(-40*time.Hour)))
	if got.endReason.String != EndReasonReaped {
		t.Fatalf("stale-api-null-activity.end_reason = %q, want %q", got.endReason.String, EndReasonReaped)
	}

	// fresh-api: untouched, still open, no reason.
	fresh := getReapRow(t, db, "fresh-api")
	if fresh.endedAt.Valid || fresh.endReason.Valid {
		t.Fatalf("fresh api_server session closed: %+v", fresh)
	}
	// stale-cron: stale but not api_server — the reaper never broadens scope.
	cron := getReapRow(t, db, "stale-cron")
	if cron.endedAt.Valid || cron.endReason.Valid {
		t.Fatalf("non-api_server session closed: %+v", cron)
	}
	// ended-api: the agent's own closure is never overwritten.
	ended := getReapRow(t, db, "ended-api")
	if !ended.endedAt.Valid || ended.endReason.String != "agent_close" {
		t.Fatalf("already-ended session mutated: %+v", ended)
	}
	expectEpochNear(t, "ended-api.ended_at", ended.endedAt.Float64, epoch(reapFixtureClock.Add(-47*time.Hour)))
}

// TestReapStaleHermesSessions_Idempotent: a closed row stops matching
// (ended_at IS NULL), so the second identical pass selects and reaps zero —
// and the dry-run candidate count of pass 1 equals its Apply reaped count,
// pinning the select and update predicates to the same row set.
func TestReapStaleHermesSessions_Idempotent(t *testing.T) {
	db, _ := standardFixture(t)
	ctx := reapCtx()
	cfg := HermesReaperConfig{StaleAfter: 24 * time.Hour, Apply: true}

	first, err := ReapStaleHermesSessions(ctx, db, cfg)
	if err != nil {
		t.Fatalf("first apply pass: %v", err)
	}
	if first.Reaped != first.Candidates {
		t.Fatalf("first pass Candidates=%d Reaped=%d — select/update predicate drift", first.Candidates, first.Reaped)
	}

	second, err := ReapStaleHermesSessions(ctx, db, cfg)
	if err != nil {
		t.Fatalf("second apply pass: %v", err)
	}
	if second.Reaped != 0 || second.Candidates != 0 {
		t.Fatalf("second pass Candidates=%d Reaped=%d, want 0/0 (idempotent)", second.Candidates, second.Reaped)
	}

	third, err := ReapStaleHermesSessions(ctx, db, HermesReaperConfig{Apply: true}) // defaults too
	if err != nil {
		t.Fatalf("third apply pass: %v", err)
	}
	if third.Reaped != 0 || third.Candidates != 0 {
		t.Fatalf("third pass Candidates=%d Reaped=%d, want 0/0", third.Candidates, third.Reaped)
	}
}

// TestReapStaleHermesSessions_ThresholdAndClockInjection: both the staleness
// threshold and the clock are injection points — the same row flips between
// candidate and preserved as either moves, with no sleeping.
func TestReapStaleHermesSessions_ThresholdAndClockInjection(t *testing.T) {
	db, _ := newHermesStateFixture(t)
	// One open api_server session idle exactly 5h at T0.
	lastActivity := reapFixtureClock.Add(-5 * time.Hour)
	seedReapRows(t, db, []reapRow{
		{id: "idle5h", source: "api_server",
			startedAt: epoch(reapFixtureClock.Add(-6 * time.Hour)), lastActivityAt: sql.NullFloat64{Float64: epoch(lastActivity), Valid: true}},
	})

	// T0 with a 6h threshold: not stale yet.
	res, err := ReapStaleHermesSessions(reapCtx(), db, HermesReaperConfig{StaleAfter: 6 * time.Hour})
	if err != nil {
		t.Fatalf("T0/6h dry-run: %v", err)
	}
	if res.Candidates != 0 {
		t.Fatalf("T0/6h Candidates = %d, want 0", res.Candidates)
	}

	// Same threshold, clock advanced 2h: the row crosses the cutoff.
	advanced := clock.WithClock(context.Background(), clock.NewFixed(reapFixtureClock.Add(2*time.Hour)))
	res, err = ReapStaleHermesSessions(advanced, db, HermesReaperConfig{StaleAfter: 6 * time.Hour})
	if err != nil {
		t.Fatalf("T0+2h/6h dry-run: %v", err)
	}
	if res.Candidates != 1 {
		t.Fatalf("T0+2h/6h Candidates = %d, want 1", res.Candidates)
	}

	// Same clock, threshold tightened to 4h: candidate again.
	res, err = ReapStaleHermesSessions(reapCtx(), db, HermesReaperConfig{StaleAfter: 4 * time.Hour})
	if err != nil {
		t.Fatalf("T0/4h dry-run: %v", err)
	}
	if res.Candidates != 1 {
		t.Fatalf("T0/4h Candidates = %d, want 1", res.Candidates)
	}

	// Zero/negative threshold normalizes to DefaultHermesReapStaleAfter:
	// 26h after T0 the 5h-idle row is 31h idle — well past the 24h default.
	late := clock.WithClock(context.Background(), clock.NewFixed(reapFixtureClock.Add(26*time.Hour)))
	res, err = ReapStaleHermesSessions(late, db, HermesReaperConfig{})
	if err != nil {
		t.Fatalf("default-threshold dry-run: %v", err)
	}
	if res.Candidates != 1 {
		t.Fatalf("default-threshold Candidates = %d, want 1", res.Candidates)
	}

	// And nothing above mutated anything (all passes were dry-runs).
	got := getReapRow(t, db, "idle5h")
	if got.endedAt.Valid || got.endReason.Valid {
		t.Fatalf("threshold/clock probes mutated the row: %+v", got)
	}
}

// TestReapStaleHermesSessions_FailsClosedOnWrongShape: the old fake v23
// shape (platform/created_at/updated_at TEXT) must be REJECTED, not reaped;
// so must a database with no sessions table at all (e.g. the scheduler's
// own --db).
func TestReapStaleHermesSessions_FailsClosedOnWrongShape(t *testing.T) {
	t.Run("old fake v23 schema rejected", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "state.db")
		db, err := sql.Open("sqlite", "file:"+path)
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		defer db.Close()
		if _, err := db.Exec(`CREATE TABLE sessions (
		    id TEXT PRIMARY KEY, platform TEXT NOT NULL DEFAULT '',
		    created_at TEXT NOT NULL, updated_at TEXT, ended_at TEXT)`); err != nil {
			t.Fatalf("create fake-shape sessions: %v", err)
		}
		if _, err := db.Exec(`INSERT INTO sessions (id, platform, created_at)
		                      VALUES ('fake-1', 'api_server', '2026-07-01T00:00:00Z')`); err != nil {
			t.Fatalf("seed fake row: %v", err)
		}

		res, err := ReapStaleHermesSessions(reapCtx(), db, HermesReaperConfig{StaleAfter: time.Hour, Apply: true})
		if !errors.Is(err, ErrHermesSessionsShape) {
			t.Fatalf("want ErrHermesSessionsShape, got %v", err)
		}
		if res.Reaped != 0 {
			t.Fatalf("shape failure reported Reaped=%d, want 0", res.Reaped)
		}
		var platform, created, ended sql.NullString
		if err := db.QueryRow(`SELECT platform, created_at, ended_at FROM sessions WHERE id='fake-1'`).
			Scan(&platform, &created, &ended); err != nil {
			t.Fatalf("query fake row: %v", err)
		}
		if ended.Valid {
			t.Fatalf("fake-shape row was mutated: ended_at=%v", ended.String)
		}
	})

	t.Run("missing sessions table rejected", func(t *testing.T) {
		db, _ := newHermesStateFixture(t)
		if _, err := db.Exec(`DROP TABLE sessions`); err != nil {
			t.Fatalf("drop sessions: %v", err)
		}
		if _, err := ReapStaleHermesSessions(reapCtx(), db, HermesReaperConfig{}); !errors.Is(err, ErrHermesSessionsShape) {
			t.Fatalf("want ErrHermesSessionsShape on table-less db, got %v", err)
		}
	})
}

// TestReapStaleHermesSessions_OnSchedulerDBFailsClosed pins the honest
// boundary: after the v23 tombstone the scheduler's own DB has no sessions
// table, so pointing the reaper at it fails closed instead of "succeeding"
// against a fake.
func TestReapStaleHermesSessions_OnSchedulerDBFailsClosed(t *testing.T) {
	db := newTestDB(t) // InitDB(":memory:") — fully migrated scheduler schema
	if _, err := ReapStaleHermesSessions(reapCtx(), db, HermesReaperConfig{Apply: true}); !errors.Is(err, ErrHermesSessionsShape) {
		t.Fatalf("reaper against scheduler DB must fail closed with ErrHermesSessionsShape, got %v", err)
	}
}

// TestReapOpenHermesStateDB_MissingFileNeverCreated: mode=rw must not
// create the file — a missing state.db is an error, never a fresh database.
func TestReapOpenHermesStateDB_MissingFileNeverCreated(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nonexistent", "state.db")
	db, err := OpenHermesStateDB(path)
	if err == nil {
		db.Close()
		t.Fatal("OpenHermesStateDB succeeded on a missing file")
	}
	if db != nil {
		t.Fatal("OpenHermesStateDB returned a handle with a nil error contract violation")
	}
	if _, serr := os.Stat(path); serr == nil {
		t.Fatal("OpenHermesStateDB created the missing file")
	}
	if !strings.Contains(err.Error(), "not accessible") {
		t.Fatalf("error should name the inaccessible path, got: %v", err)
	}
}

// TestReapOpenHermesStateDB_OpensExistingFile: an existing file opens with
// a working (pass-gated) handle; DefaultHermesSessionDBPath resolves under
// the user's home.
func TestReapOpenHermesStateDB_OpensExistingFile(t *testing.T) {
	db, path := newHermesStateFixture(t)
	_ = db // fixture handle open; open a second handle through the real opener
	opened, err := OpenHermesStateDB(path)
	if err != nil {
		t.Fatalf("OpenHermesStateDB(%s): %v", path, err)
	}
	defer opened.Close()

	var one int
	if err := opened.QueryRow(`SELECT 1`).Scan(&one); err != nil || one != 1 {
		t.Fatalf("opened handle not usable: one=%d err=%v", one, err)
	}

	def := DefaultHermesSessionDBPath()
	if !strings.HasSuffix(def, filepath.Join(".hermes", "state.db")) {
		t.Fatalf("DefaultHermesSessionDBPath = %q, want a path ending in .hermes/state.db", def)
	}
}

// TestMigrate_V23Tombstoned: the fake v23 migration is gone — a fully
// migrated scheduler database must NOT contain a sessions table, and the
// migration ledger must record the tombstone instead of v23.
func TestMigrate_V23Tombstoned(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	v, err := MigrationVersion(ctx, db)
	if err != nil {
		t.Fatalf("MigrationVersion: %v", err)
	}
	if v != latestMigration {
		t.Fatalf("migration version = %d, want %d", v, latestMigration)
	}

	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='sessions'`).Scan(&n); err != nil {
		t.Fatalf("check sessions table: %v", err)
	}
	if n != 0 {
		t.Fatal("scheduler DB has a sessions table — the fake v23 path is back")
	}

	var ledger int
	if err := db.QueryRow(`SELECT COUNT(*) FROM migrations WHERE version=23 AND desc LIKE '%tombstone%'`).Scan(&ledger); err != nil {
		t.Fatalf("check migration ledger: %v", err)
	}
	if ledger != 1 {
		t.Fatal("v23 tombstone not recorded in the migrations ledger")
	}
}
