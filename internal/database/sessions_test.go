package database

import (
	"context"
	"database/sql"
	"testing"
	"time"
)

// tsLayout is the seconds-only RFC3339 UTC layout used for session
// timestamps in these tests — matches the text format the reaper compares.
const tsLayout = "2006-01-02T15:04:05Z07:00"

// sessionRow mirrors a row of the sessions table for assertions.
type sessionRow struct {
	id        string
	platform  string
	createdAt string
	updatedAt sql.NullString
	endedAt   sql.NullString
}

func getSessionRow(t *testing.T, db *sql.DB, id string) sessionRow {
	t.Helper()
	var r sessionRow
	err := db.QueryRow(`SELECT id, platform, created_at, updated_at, ended_at
	                    FROM sessions WHERE id = ?`, id).
		Scan(&r.id, &r.platform, &r.createdAt, &r.updatedAt, &r.endedAt)
	if err != nil {
		t.Fatalf("query session %q: %v", id, err)
	}
	return r
}

func seedTestSessions(t *testing.T, db *sql.DB, rows []sessionRow) {
	t.Helper()
	for _, r := range rows {
		var upd, end any
		if r.updatedAt.Valid {
			upd = r.updatedAt.String
		}
		if r.endedAt.Valid {
			end = r.endedAt.String
		}
		if _, err := db.Exec(`INSERT INTO sessions (id, platform, created_at, updated_at, ended_at)
		                      VALUES (?, ?, ?, ?, ?)`,
			r.id, r.platform, r.createdAt, upd, end); err != nil {
			t.Fatalf("insert session %q: %v", r.id, err)
		}
	}
}

// TestMigrationV23BackfillsZombieSessions verifies migration v23 creates the
// sessions table (idempotently) and backfills ended_at = COALESCE(updated_at,
// created_at) for existing api_server zombie rows while leaving non-api_server
// and already-ended rows untouched.
func TestMigrationV23BackfillsZombieSessions(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open :memory:: %v", err)
	}
	defer db.Close()
	ctx := context.Background()

	if _, err := db.Exec(`CREATE TABLE sessions (
	    id         TEXT PRIMARY KEY,
	    platform   TEXT NOT NULL DEFAULT '',
	    created_at TEXT NOT NULL,
	    updated_at TEXT,
	    ended_at   TEXT
	);`); err != nil {
		t.Fatalf("pre-create sessions table: %v", err)
	}

	const ts = "2026-07-01T00:00:00Z"
	const later = "2026-08-01T00:00:00Z"
	seedTestSessions(t, db, []sessionRow{
		{id: "a", platform: "api_server", createdAt: ts, updatedAt: sql.NullString{String: later, Valid: true}},                                                                       // zombie, both timestamps
		{id: "b", platform: "api_server", createdAt: ts, updatedAt: sql.NullString{}},                                                                                                 // zombie, updated_at NULL → created_at fallback
		{id: "c", platform: "telegram", createdAt: ts, updatedAt: sql.NullString{String: later, Valid: true}},                                                                         // non-api_server zombie → untouched
		{id: "d", platform: "api_server", createdAt: ts, updatedAt: sql.NullString{String: later, Valid: true}, endedAt: sql.NullString{String: "2026-06-01T00:00:00Z", Valid: true}}, // already ended → unchanged
	})

	if err := Migrate(ctx, db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	v, err := MigrationVersion(ctx, db)
	if err != nil {
		t.Fatalf("MigrationVersion: %v", err)
	}
	if v != latestMigration {
		t.Fatalf("migration version = %d, want %d", v, latestMigration)
	}

	// (a) api_server zombie backfilled from updated_at.
	a := getSessionRow(t, db, "a")
	if !a.endedAt.Valid || a.endedAt.String != later {
		t.Fatalf("row a ended_at = %v, want %s", a.endedAt, later)
	}
	// (b) updated_at NULL → fall back to created_at.
	b := getSessionRow(t, db, "b")
	if !b.endedAt.Valid || b.endedAt.String != ts {
		t.Fatalf("row b ended_at = %v, want created_at fallback %s", b.endedAt, ts)
	}
	// (c) telegram zombie must stay open.
	c := getSessionRow(t, db, "c")
	if c.endedAt.Valid {
		t.Fatalf("row c (telegram) ended_at = %v, want NULL (untouched)", c.endedAt)
	}
	// (d) already-ended row unchanged.
	d := getSessionRow(t, db, "d")
	if !d.endedAt.Valid || d.endedAt.String != "2026-06-01T00:00:00Z" {
		t.Fatalf("row d ended_at = %v, want unchanged 2026-06-01T00:00:00Z", d.endedAt)
	}
}

// TestReapZombieSessions verifies the reaper closes only stale api_server
// zombies (older than threshold) and is idempotent across runs.
func TestReapZombieSessions(t *testing.T) {
	db := newTestDB(t) // InitDB(":memory:") — fully migrated, sessions exists
	ctx := context.Background()

	now := time.Now().UTC()
	stale := now.Add(-48 * time.Hour).Format(tsLayout)
	recent := now.Add(-time.Minute).Format(tsLayout)
	seedTestSessions(t, db, []sessionRow{
		{id: "z-1", platform: "api_server", createdAt: stale, updatedAt: sql.NullString{String: stale, Valid: true}},                                                                         // stale, both timestamps
		{id: "z-2", platform: "api_server", createdAt: stale, updatedAt: sql.NullString{}},                                                                                                   // stale, updated_at NULL → created_at
		{id: "live", platform: "api_server", createdAt: recent, updatedAt: sql.NullString{String: recent, Valid: true}},                                                                      // fresh → stays open
		{id: "tg", platform: "telegram", createdAt: stale, updatedAt: sql.NullString{String: stale, Valid: true}},                                                                            // stale but non-api_server → stays open
		{id: "ended", platform: "api_server", createdAt: stale, updatedAt: sql.NullString{String: stale, Valid: true}, endedAt: sql.NullString{String: "2020-01-01T00:00:00Z", Valid: true}}, // already ended → unchanged
	})

	before := map[string]sessionRow{
		"live":  getSessionRow(t, db, "live"),
		"tg":    getSessionRow(t, db, "tg"),
		"ended": getSessionRow(t, db, "ended"),
	}

	reaped, err := ReapZombieSessions(ctx, db, 24*time.Hour)
	if err != nil {
		t.Fatalf("ReapZombieSessions: %v", err)
	}
	if reaped != 2 {
		t.Fatalf("reaped = %d, want 2", reaped)
	}

	z1 := getSessionRow(t, db, "z-1")
	if !z1.endedAt.Valid || z1.endedAt.String != z1.updatedAt.String {
		t.Fatalf("z-1 ended_at = %v, want updated_at %s", z1.endedAt, z1.updatedAt.String)
	}
	z2 := getSessionRow(t, db, "z-2")
	if !z2.endedAt.Valid || z2.endedAt.String != z2.createdAt {
		t.Fatalf("z-2 ended_at = %v, want created_at fallback %s", z2.endedAt, z2.createdAt)
	}

	// Live / telegram / already-ended rows must be byte-identical.
	for _, id := range []string{"live", "tg", "ended"} {
		got := getSessionRow(t, db, id)
		want := before[id]
		if got.createdAt != want.createdAt || got.updatedAt != want.updatedAt || got.endedAt != want.endedAt {
			t.Fatalf("%s mutated by reaper: before %+v after %+v", id, want, got)
		}
		if id == "live" && got.endedAt.Valid {
			t.Fatalf("live api_server session was reaped: %+v", got)
		}
	}

	// Idempotency: a second run reaps nothing.
	reaped2, err := ReapZombieSessions(ctx, db, 24*time.Hour)
	if err != nil {
		t.Fatalf("ReapZombieSessions (2nd run): %v", err)
	}
	if reaped2 != 0 {
		t.Fatalf("second run reaped = %d, want 0 (idempotent)", reaped2)
	}
}

// TestReapZombieSessions_MissingTableNoop verifies the reaper is a safe no-op
// when the sessions table does not exist (e.g. a DB that predates migration
// v23), so daemon startup is never blocked.
func TestReapZombieSessions_MissingTableNoop(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open :memory:: %v", err)
	}
	defer db.Close()

	reaped, err := ReapZombieSessions(context.Background(), db, 24*time.Hour)
	if err != nil {
		t.Fatalf("ReapZombieSessions on table-less DB: %v", err)
	}
	if reaped != 0 {
		t.Fatalf("reaped = %d, want 0 on table-less DB", reaped)
	}
}
