package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// SCHED-GAP-1755 migration v72+v73 pin: the turn_deadline column exists, defaults
// to ” (inherit the global 30m per-turn knob), and the duckbrain-sync
// namespace is migrated to '0s' (the per-turn EXEMPT that stops the 30m
// deadline from killing productive sync lanes' long DuckBrain HTTP writes).
func TestMigrationV72_TurnDeadline(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	// The column must exist and default to ''.
	if _, err := db.ExecContext(ctx, `INSERT INTO namespaces (id) VALUES ('ns-plain')`); err != nil {
		t.Fatalf("insert plain namespace: %v", err)
	}
	var raw string
	if err := db.QueryRowContext(ctx, `SELECT COALESCE(turn_deadline, '') FROM namespaces WHERE id = 'ns-plain'`).Scan(&raw); err != nil {
		// "no such column" would mean migration v72 did not arm the column —
		// fail with the exact reason, never a derived zero.
		t.Fatalf("read turn_deadline for a fresh namespace: %v", err)
	}
	if raw != "" {
		t.Errorf("fresh namespace turn_deadline = %q, want \"\" (inherit the global knob)", raw)
	}

	// duckbrain-sync exemption: re-run the v73 backfill against a database
	// where the namespace row and the column already exist — the LIVE
	// upgrade path (the row predates the migration). The ALTER's duplicate
	// column is tolerated by the migration runner, so deleting the version
	// row and re-migrating replays exactly the arm that matters here.
	if _, err := db.ExecContext(ctx, `INSERT INTO namespaces (id) VALUES ('duckbrain-sync')`); err != nil {
		if !strings.Contains(err.Error(), "UNIQUE") {
			t.Fatalf("insert duckbrain-sync: %v", err)
		}
	}
	var seeded string
	if err := db.QueryRowContext(ctx, `SELECT COALESCE(turn_deadline, '') FROM namespaces WHERE id = 'duckbrain-sync'`).Scan(&seeded); err != nil {
		t.Fatalf("read fresh duckbrain-sync turn_deadline: %v", err)
	}
	if seeded != "" {
		t.Fatalf("fresh duckbrain-sync turn_deadline = %q, want \"\" before the v73 restatement", seeded)
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM migrations WHERE version = 73`); err != nil {
		t.Fatalf("unapply v73: %v", err)
	}
	if err := Migrate(ctx, db); err != nil {
		t.Fatalf("re-run migration ladder: %v", err)
	}
	if err := db.QueryRowContext(ctx, `SELECT COALESCE(turn_deadline, '') FROM namespaces WHERE id = 'duckbrain-sync'`).Scan(&raw); err != nil {
		t.Fatalf("read duckbrain-sync turn_deadline: %v", err)
	}
	if raw != "0s" {
		t.Errorf("duckbrain-sync turn_deadline = %q, want \"0s\" (per-turn exempt, SCHED-GAP-1755)", raw)
	}

	// The QA half (v74): a builder family gets a LONG deadline, not an
	// exemption — measured on the live fleet, qa dies to the same 30m class
	// mid-battery but a full exemption would leave a genuine hang on the 3h
	// slot. Same replay shape as the duckbrain-sync arm.
	if _, err := db.ExecContext(ctx, `INSERT OR IGNORE INTO namespaces (id) VALUES ('qa')`); err != nil {
		t.Fatalf("insert qa: %v", err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE namespaces SET turn_deadline = '' WHERE id = 'qa'`); err != nil {
		t.Fatalf("reset qa turn_deadline: %v", err)
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM migrations WHERE version = 74`); err != nil {
		t.Fatalf("unapply v74: %v", err)
	}
	if err := Migrate(ctx, db); err != nil {
		t.Fatalf("re-run migration ladder for v74: %v", err)
	}
	var qaDL string
	if err := db.QueryRowContext(ctx, `SELECT COALESCE(turn_deadline, '') FROM namespaces WHERE id = 'qa'`).Scan(&qaDL); err != nil {
		t.Fatalf("read qa turn_deadline: %v", err)
	}
	if qaDL != "2h" {
		t.Errorf("qa turn_deadline = %q, want \"2h\" (long-battery override, SCHED-GAP-1755)", qaDL)
	}
	// The v74 predicate is idempotent: an operator-set value must survive a
	// replay (only a row still at the '' default is written).
	if _, err := db.ExecContext(ctx, `UPDATE namespaces SET turn_deadline = '90m' WHERE id = 'qa'`); err != nil {
		t.Fatalf("operator override: %v", err)
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM migrations WHERE version = 74`); err != nil {
		t.Fatalf("unapply v74 (second time): %v", err)
	}
	if err := Migrate(ctx, db); err != nil {
		t.Fatalf("re-run migration ladder (second time): %v", err)
	}
	if err := db.QueryRowContext(ctx, `SELECT COALESCE(turn_deadline, '') FROM namespaces WHERE id = 'qa'`).Scan(&qaDL); err != nil {
		t.Fatalf("read qa turn_deadline after replay: %v", err)
	}
	if qaDL != "90m" {
		t.Errorf("qa turn_deadline after replay = %q, want the operator's \"90m\" preserved (the backfill never overrides a set value)", qaDL)
	}

	// MigrationVersion must be at the head of the ladder on a migrated DB.
	v, err := MigrationVersion(ctx, db)
	if err != nil {
		t.Fatalf("MigrationVersion: %v", err)
	}
	if v < 74 {
		t.Errorf("MigrationVersion = %d, want >= 74", v)
	}
}

// UpdateNamespace must accept the per-turn vocabulary (” / '0s' / positive
// durations) verbatim and reject garbage + negative durations with a 400-class
// error — never silently warn-and-default (unlike the hand-edited row path).
func TestUpdateNamespace_TurnDeadlineVocabulary(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, `INSERT INTO namespaces (id) VALUES ('ns-td')`); err != nil {
		t.Fatalf("insert namespace: %v", err)
	}

	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		{in: "0s", want: "0s", ok: true},
		{in: "90m", want: "90m", ok: true},
		{in: "2h", want: "2h", ok: true},
		{in: "", want: "", ok: true},
		{in: "garbage", want: "", ok: false},
		{in: "-5m", want: "", ok: false},
	}
	for _, c := range cases {
		val := c.in
		err := UpdateNamespace(ctx, db, "ns-td", NamespacePatch{TurnDeadline: &val})
		if c.ok && err != nil {
			t.Errorf("turn_deadline %q: UpdateNamespace failed: %v", c.in, err)
			continue
		}
		if !c.ok {
			if err == nil {
				t.Errorf("turn_deadline %q: UpdateNamespace accepted a garbage value", c.in)
			}
			continue
		}
		var got string
		if err := db.QueryRowContext(ctx, `SELECT COALESCE(turn_deadline, '') FROM namespaces WHERE id = 'ns-td'`).Scan(&got); err != nil {
			t.Fatalf("read back turn_deadline: %v", err)
		}
		if got != c.want {
			t.Errorf("turn_deadline %q: read back %q, want %q", c.in, got, c.want)
		}
	}
	_ = errors.Is // keep the errors import pin explicit for the future
	_ = fmt.Sprintf
	_ = sql.ErrNoRows
}
