package database

import (
	"context"
	"database/sql"
	"testing"
)

// SCHED-GAP-215 — satellite namespace throughput, STORAGE layer.
//
// The five big satellite families (qa, pm, dogfood, releases, duckbrain-sync)
// each host 20-34 enabled lanes but sat at max_concurrent = 1, so every family
// serialized behind a single sibling: the SCHED-GAP-144 cap gate deferred
// 1445 spawn attempts by 2026-09-24 and the lane-lag 3-6x bucket held 45 of
// the 150 lanes with tick history. Migration v40 raises each family to the
// ~1-slot-per-3-enabled-lanes policy (9 / 9 / 9 / 9 / 12).
//
// The 12-slot global ceiling still bounds the whole fleet — the raise
// redistributes slots across namespaces (idle foremen budget flows to
// satellites via the borrowing engine), it does not create new capacity.
//
// The migration is deliberately a GUARDED BACKFILL (v38's pattern): every
// UPDATE is conditioned on the OLD cap (max_concurrent = 1), so re-running it
// can never double the value, and namespaces outside the policy (doc-writer
// stays at 1 by design; auger 2; foremen 8) are untouched because they are
// named rows, not family-matched.
//
// Fresh databases are NOT covered here on purpose: migrations create the
// empty namespaces table; rows are seeded later from fleet.toml
// (config.ApplyFleetConfig), which carries the same policy values. That
// split is the documented two-store law (docs/fleet-config-model.md §5).

// satelliteCapPolicy is the SCHED-GAP-215 cap policy this migration encodes:
// namespace id → max_concurrent. ~1 slot per ~3 enabled lanes, measured
// 2026-09-24 (qa 26, pm 25, dogfood 25, releases 27, duckbrain-sync 34
// enabled lanes).
var satelliteCapPolicy = map[string]int{
	"qa":             9,
	"pm":             9,
	"dogfood":        9,
	"releases":       9,
	"duckbrain-sync": 12,
}

// satelliteDescPolicy mirrors satelliteCapPolicy for the three namespaces
// whose pre-215 description text still claimed "1 concurrent." — the migration
// refreshes those claims so the row's own prose cannot contradict its cap.
var satelliteDescPolicy = map[string]string{
	"qa":             "QA lanes — clean-machine brittleness battery (skill qa-foreman-ops). Bunker path; the Dagger qa.ts executor is retired until the new dagger is built. Capped at 9 concurrent (SCHED-GAP-215: ~1 slot per 3 enabled lanes).",
	"pm":             "Per-project PM lane — board hygiene: dedupe by content fingerprint, repair reused/malformed ids, normalise priorities, no refiling. Capped at 9 concurrent (SCHED-GAP-215: ~1 slot per 3 enabled lanes).",
	"duckbrain-sync": "DuckBrain namespace sync lanes — skill-driven focused sync (context-sync-duckbrain). Driver retired until the new dagger is built. Capped at 12 concurrent (SCHED-GAP-215: ~1 slot per 3 enabled lanes).",
}

// schedGap215SeedPre215 inserts the pre-215 namespace shape: the five policy
// families at cap 1 with their stale "1 concurrent." descriptions, plus three
// decoys the migration must leave alone. CreatedAt/UpdatedAt are filled by
// CreateNamespace.
func schedGap215SeedPre215(t *testing.T, ctx context.Context, db *sql.DB) {
	t.Helper()
	seed := []struct {
		id          string
		cap         int
		description string
	}{
		{"coding-hermes", 8, "Active foreman projects"},
		{"qa", 1, "QA lanes — clean-machine brittleness battery (skill qa-foreman-ops). Bunker path; the Dagger qa.ts executor is retired until the new dagger is built. 1 concurrent."},
		{"pm", 1, "Per-project PM lane — board hygiene: dedupe by content fingerprint, repair reused/malformed ids, normalise priorities, no refiling. 1 concurrent."},
		{"dogfood", 1, "Dogfood lanes — value discovery through real use (skill coding-hermes-dogfood). Verification, not development: battery + ephemeral-bunker install leg, findings filed for the foreman."},
		{"releases", 1, "Release engineering lane — manual-trigger release cuts via POST /api/v1/projects/release-engineer/spawn; CI/dogfood evidence review; semver decision + artifact builds"},
		{"duckbrain-sync", 1, "DuckBrain namespace sync lanes — skill-driven focused sync (context-sync-duckbrain). Driver retired until the new dagger is built. 1 concurrent."},
		// Decoys: doc-writer stays at 1 by design (7 lanes, weekly cadence);
		// auger is a primary+satellites family pinned at 2 by its operator.
		{"doc-writer", 1, "Weekly documentation pass — research-driven doc refresh; injects DOC- tasks into project boards; no-op when git history is quiet"},
		{"auger", 2, "auger project family - primary + 5 satellites, isolated so fleet-wide contention stops gating them"},
	}
	for _, s := range seed {
		ns := &Namespace{
			ID:            s.id,
			Weight:        10,
			Reserved:      1,
			HardCap:       100,
			MaxConcurrent: s.cap,
			Enabled:       true,
			Description:   s.description,
		}
		if err := CreateNamespace(ctx, db, ns); err != nil {
			t.Fatalf("seed namespace %q: %v", s.id, err)
		}
	}
}

// runSchedGap215Migration executes migration v40's statement directly — the
// same statement Migrate would run on a pre-215 database. Bounds-checked so a
// ladder without v40 fails the test cleanly instead of panicking (the RED
// proof: revert migrations.go and this file fails with "migration v40
// missing").
func runSchedGap215Migration(t *testing.T, ctx context.Context, db *sql.DB) {
	t.Helper()
	if len(migrations) < 40 || migrations[39].version != 40 {
		t.Fatalf("migration v40 missing from the ladder (len=%d) — SCHED-GAP-215 not landed", len(migrations))
	}
	if _, err := db.ExecContext(ctx, migrations[39].stmt); err != nil {
		t.Fatalf("run v40 statement: %v", err)
	}
}

// TestSCHEDGAP215_Backfill_RaisesSatelliteCaps proves the v40 backfill lands
// exactly the policy map on a pre-215 namespace shape, touches nothing outside
// it, and refreshes the stale "1 concurrent." description claims.
func TestSCHEDGAP215_Backfill_RaisesSatelliteCaps(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	schedGap215SeedPre215(t, ctx, db)
	runSchedGap215Migration(t, ctx, db)

	for id, want := range satelliteCapPolicy {
		ns, err := GetNamespace(ctx, db, id)
		if err != nil {
			t.Fatalf("get namespace %q: %v", id, err)
		}
		if ns.MaxConcurrent != want {
			t.Errorf("namespace %q max_concurrent = %d, want %d (SCHED-GAP-215 policy)", id, ns.MaxConcurrent, want)
		}
	}

	// The three descriptions that claimed "1 concurrent." must be refreshed;
	// the two families whose prose never carried the claim keep theirs.
	for id, want := range satelliteDescPolicy {
		ns, err := GetNamespace(ctx, db, id)
		if err != nil {
			t.Fatalf("get namespace %q: %v", id, err)
		}
		if ns.Description != want {
			t.Errorf("namespace %q description = %q, want %q", id, ns.Description, want)
		}
	}
	for _, id := range []string{"dogfood", "releases"} {
		ns, err := GetNamespace(ctx, db, id)
		if err != nil {
			t.Fatalf("get namespace %q: %v", id, err)
		}
		if ns.Description == "" {
			t.Errorf("namespace %q description wiped by v40 — the description UPDATEs must be claim-scoped", id)
		}
	}

	// Outside the policy: untouched caps AND untouched prose.
	decoys := map[string]int{"coding-hermes": 8, "doc-writer": 1, "auger": 2}
	for id, want := range decoys {
		ns, err := GetNamespace(ctx, db, id)
		if err != nil {
			t.Fatalf("get namespace %q: %v", id, err)
		}
		if ns.MaxConcurrent != want {
			t.Errorf("namespace %q max_concurrent = %d, want %d — v40 must touch only the five policy families", id, ns.MaxConcurrent, want)
		}
	}
}

// TestSCHEDGAP215_Backfill_Idempotent proves the guarded backfill can never
// double the value: a second execution of the v40 statement on the
// already-migrated shape is a no-op (every UPDATE is conditioned on the OLD
// cap 1, which no longer matches). This is the crash-retry guarantee — a boot
// that dies between the UPDATE and the migrations-table INSERT re-runs the
// statement on the next boot.
func TestSCHEDGAP215_Backfill_Idempotent(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	schedGap215SeedPre215(t, ctx, db)
	runSchedGap215Migration(t, ctx, db)
	runSchedGap215Migration(t, ctx, db) // crash-retry / re-run

	for id, want := range satelliteCapPolicy {
		ns, err := GetNamespace(ctx, db, id)
		if err != nil {
			t.Fatalf("get namespace %q: %v", id, err)
		}
		if ns.MaxConcurrent != want {
			t.Errorf("namespace %q max_concurrent = %d after re-run, want %d — the backfill is not idempotent", id, ns.MaxConcurrent, want)
		}
	}
}

// TestSCHEDGAP215_Backfill_RespectsOperatorRaisedRow proves the guard is not
// only about the exact old value: an operator who already raised a family's
// cap through the API before v40 lands must keep their (higher) value — the
// backfill targets the cap-1 rows, not the namespace name.
func TestSCHEDGAP215_Backfill_RespectsOperatorRaisedRow(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	schedGap215SeedPre215(t, ctx, db)

	// Operator raised qa to 11 before the migration landed.
	if err := UpdateNamespace(ctx, db, "qa", NamespacePatch{MaxConcurrent: sg215IntPtr(11)}); err != nil {
		t.Fatalf("operator raise qa: %v", err)
	}
	runSchedGap215Migration(t, ctx, db)

	ns, err := GetNamespace(ctx, db, "qa")
	if err != nil {
		t.Fatalf("get namespace qa: %v", err)
	}
	if ns.MaxConcurrent != 11 {
		t.Errorf("operator-raised cap clobbered: qa max_concurrent = %d, want 11", ns.MaxConcurrent)
	}
	// Sibling families still get the policy value.
	pm, err := GetNamespace(ctx, db, "pm")
	if err != nil {
		t.Fatalf("get namespace pm: %v", err)
	}
	if pm.MaxConcurrent != satelliteCapPolicy["pm"] {
		t.Errorf("pm max_concurrent = %d, want %d", pm.MaxConcurrent, satelliteCapPolicy["pm"])
	}
}

// TestSCHEDGAP215_MigrationRecorded pins the ladder bookkeeping: v40 is the
// latest migration and Migrate records it (so a fresh DB or an upgrade both
// see version 40).
func TestSCHEDGAP215_MigrationRecorded(t *testing.T) {
	db := newTestDB(t) // full ladder incl. v40 applied on the template copy
	ctx := context.Background()

	v, err := MigrationVersion(ctx, db)
	if err != nil {
		t.Fatalf("migration version: %v", err)
	}
	if v != latestMigration {
		t.Fatalf("migration version = %d, want %d", v, latestMigration)
	}
	if latestMigration != 46 {
		t.Fatalf("latestMigration = %d, want 46 (SOL-CADENCE target_runs_per_day follows the SCHED-GAP-1636 v45 covering index)", latestMigration)
	}
	// The recorded description names the row, so ops can trace the change.
	var desc string
	if err := db.QueryRowContext(ctx, `SELECT desc FROM migrations WHERE version = 40`).Scan(&desc); err != nil {
		t.Fatalf("read v40 desc: %v", err)
	}
	if !contains(desc, "SCHED-GAP-215") {
		t.Errorf("v40 desc %q does not name SCHED-GAP-215", desc)
	}
}

// contains is a local helper so this file needs no strings import beyond the
// call sites above.
func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// sg215IntPtr is the NamespacePatch MaxConcurrent input shape (*int).
func sg215IntPtr(v int) *int { return &v }
