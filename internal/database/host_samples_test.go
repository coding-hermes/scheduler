package database

// ADV-R13 — regression tests for the host_samples helpers.
//
// The contracts pinned: the insert round-trips every field; the latest
// read returns the highest id with no other row; an empty table is an
// honest sql.ErrNoRows (never a synthesized zero row); the migration is
// appended, not renumbered.

import (
	"context"
	"database/sql"
	"testing"
)

func TestADVR13_HostSampleInsertAndLatestRoundTrip(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)

	if _, err := LatestHostSample(ctx, db); err != sql.ErrNoRows {
		t.Fatalf("LatestHostSample on empty table = %v, want sql.ErrNoRows", err)
	}

	s := &HostSample{
		SampledAt:         "2031-05-04T03:02:01Z",
		Load1:             1.25,
		Load5:             0.75,
		Load15:            0.5,
		MemTotalBytes:     16 << 30,
		MemAvailableBytes: 8 << 30,
		Source:            "proc",
	}
	if err := InsertHostSample(ctx, db, s); err != nil {
		t.Fatalf("InsertHostSample: %v", err)
	}
	if s.ID == 0 {
		t.Fatal("InsertHostSample left ID unset — want the AUTOINCREMENT id back")
	}

	got, err := LatestHostSample(ctx, db)
	if err != nil {
		t.Fatalf("LatestHostSample: %v", err)
	}
	if got.ID != s.ID || got.SampledAt != s.SampledAt || got.Load1 != s.Load1 ||
		got.Load5 != s.Load5 || got.Load15 != s.Load15 ||
		got.MemTotalBytes != s.MemTotalBytes || got.MemAvailableBytes != s.MemAvailableBytes ||
		got.Source != s.Source {
		t.Errorf("round trip mismatch:\n got %+v\nwant %+v", got, s)
	}
}

func TestADVR13_LatestHostSampleReturnsNewestRow(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)

	rows := []*HostSample{
		{SampledAt: "2031-05-04T03:02:01Z", Load1: 0.1, Load5: 0.2, Load15: 0.3,
			MemTotalBytes: 1, MemAvailableBytes: 1, Source: "proc"},
		{SampledAt: "2031-05-04T03:03:01Z", Load1: 1.1, Load5: 1.2, Load15: 1.3,
			MemTotalBytes: 2, MemAvailableBytes: 2, Source: "proc"},
	}
	for _, s := range rows {
		if err := InsertHostSample(ctx, db, s); err != nil {
			t.Fatalf("InsertHostSample: %v", err)
		}
	}

	got, err := LatestHostSample(ctx, db)
	if err != nil {
		t.Fatalf("LatestHostSample: %v", err)
	}
	if got.ID != rows[1].ID || got.Load1 != 1.1 {
		t.Errorf("latest = id %d load1 %v, want the NEWEST row (id %d, load1 1.1)", got.ID, got.Load1, rows[1].ID)
	}
}

func TestADVR13_Migration34AppendsHostSamples(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)

	v, err := MigrationVersion(ctx, db)
	if err != nil {
		t.Fatalf("MigrationVersion: %v", err)
	}
	if v != latestMigration {
		t.Fatalf("migration version = %d, want %d", v, latestMigration)
	}
	if latestMigration < 34 {
		t.Fatalf("latestMigration = %d, want >= 34 (ADV-R13 host_samples)", latestMigration)
	}
	var name string
	if err := db.QueryRow(`SELECT name FROM sqlite_master WHERE type='table' AND name='host_samples'`).Scan(&name); err != nil {
		t.Fatalf("host_samples table missing after migrations: %v", err)
	}
	// The source column's DEFAULT '' contract (migration verbatim shape).
	if _, err := db.Exec(`INSERT INTO host_samples
		(sampled_at, load1, load5, load15, mem_total_bytes, mem_available_bytes)
		VALUES ('2031-05-04T03:02:01Z', 0.1, 0.2, 0.3, 1, 1)`); err != nil {
		t.Fatalf("insert without source (want DEFAULT '' accepted): %v", err)
	}
	got, err := LatestHostSample(ctx, db)
	if err != nil {
		t.Fatalf("LatestHostSample: %v", err)
	}
	if got.Source != "" {
		t.Errorf("source = %q, want '' (DEFAULT)", got.Source)
	}
}
