package database

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestDeferralRetentionCopyDeletesOnlyOldRowsAndVacuumPreservesShape(t *testing.T) {
	source := filepath.Join(t.TempDir(), "source.db")
	db, err := InitDB(source)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	for _, row := range []struct {
		project string
		at      string
		detail  string
	}{
		{"old", now.Add(-30 * 24 * time.Hour).Format(time.RFC3339), "old-detail"},
		{"recent", now.Add(-2 * 24 * time.Hour).Format(time.RFC3339), "recent-detail"},
	} {
		if _, err := db.Exec(`INSERT INTO deferrals(project_name,reason,pass_id,detail,created_at) VALUES(?,?,?,?,?)`, row.project, "cap", 1, row.detail, row.at); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	copyPath := filepath.Join(t.TempDir(), "copy.db")
	if err := CopyDatabase(context.Background(), source, copyPath); err != nil {
		t.Fatalf("CopyDatabase: %v", err)
	}
	copy, err := InitDB(copyPath)
	if err != nil {
		t.Fatal(err)
	}
	before, err := DeferralRetentionSnapshot(context.Background(), copy, now, DefaultDeferralRetention)
	if err != nil {
		t.Fatal(err)
	}
	if before.Rows != 2 || before.OverdueRows != 1 {
		t.Fatalf("before snapshot = %+v, want two rows and one >2x-window row", before)
	}
	deleted, err := PruneDeferrals(context.Background(), copy, now.Add(-DefaultDeferralRetention), 1)
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 1 {
		t.Fatalf("deleted = %d, want one bounded batch", deleted)
	}
	afterDelete, err := DeferralRetentionSnapshot(context.Background(), copy, now, DefaultDeferralRetention)
	if err != nil {
		t.Fatal(err)
	}
	if afterDelete.Rows != 1 || afterDelete.PayloadBytes >= before.PayloadBytes {
		t.Fatalf("after delete = %+v, want one row and less payload", afterDelete)
	}
	if _, err := copy.Exec("VACUUM"); err != nil {
		t.Fatal(err)
	}
	afterVacuum, err := DeferralRetentionSnapshot(context.Background(), copy, now, DefaultDeferralRetention)
	if err != nil {
		t.Fatal(err)
	}
	if afterVacuum.Rows != afterDelete.Rows || afterVacuum.PayloadBytes != afterDelete.PayloadBytes {
		t.Fatalf("vacuum changed logical rows: delete=%+v vacuum=%+v", afterDelete, afterVacuum)
	}
	var kept string
	if err := copy.QueryRow("SELECT project_name FROM deferrals").Scan(&kept); err != nil {
		t.Fatal(err)
	}
	if kept != "recent" {
		t.Fatalf("kept project = %q, want recent", kept)
	}
	_ = copy.Close()
}

func TestDeferralRetentionZeroKeepsForeverAndNegativeRejects(t *testing.T) {
	if err := ValidateDeferralRetention(0, 1); err != nil {
		t.Fatalf("zero retention: %v", err)
	}
	if err := ValidateDeferralRetention(-time.Second, 1); err == nil {
		t.Fatal("negative retention accepted")
	}
	if err := ValidateDeferralRetention(time.Hour, 0); err == nil {
		t.Fatal("zero batch accepted")
	}
}

func TestDeferralRetentionSnapshotRegrowthGuard(t *testing.T) {
	db := newTestDB(t)
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	_, err := db.Exec(`INSERT INTO deferrals(project_name,reason,pass_id,detail,created_at) VALUES('old','cap',1,'detail',?)`, now.Add(-29*24*time.Hour).Format(time.RFC3339))
	if err != nil {
		t.Fatal(err)
	}
	stats, err := DeferralRetentionSnapshot(context.Background(), db, now, 14*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if stats.OverdueRows != 1 {
		t.Fatalf("overdue rows = %d, want 1 for silent-regrowth guard", stats.OverdueRows)
	}
	if stats.Rows != 1 {
		t.Fatalf("rows = %d, want one", stats.Rows)
	}
}
