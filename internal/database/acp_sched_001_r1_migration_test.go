package database

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coding-hermes/scheduler/internal/clock"
	"github.com/coding-hermes/scheduler/internal/execution"
	"github.com/coding-hermes/scheduler/internal/foreman"
)

// migrationsUpTo returns the ordered migrations with version <= v.
func migrationsUpTo(v int) []migration {
	var out []migration
	for _, m := range migrations {
		if m.version <= v {
			out = append(out, m)
		}
	}
	return out
}

// v69Fixture opens a raw database migrated exactly to schema v69 — the state
// a populated pre-seam daemon upgrades from.
func v69Fixture(t *testing.T) (*sql.DB, context.Context) {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "v69.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	db.SetMaxOpenConns(1)
	ctx := clock.WithClock(context.Background(), clock.NewFixed(executionTestNow))
	if err := applyMigrations(ctx, db, migrationsUpTo(69)); err != nil {
		t.Fatal(err)
	}
	return db, ctx
}

func insertV69Run(t *testing.T, ctx context.Context, db *sql.DB, id, state string) {
	t.Helper()
	now := executionTestNow.Format(time.RFC3339Nano)
	if _, err := db.ExecContext(ctx, `INSERT INTO execution_runs
		(run_id,submitting_authority,submission_idempotency_key,brief_id,brief_revision,brief_digest,tick_id,lane,task_id,state,created_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`,
		id, "foreman:test", "submit-"+id, "brief-"+id, "r1", "digest-"+id, "tick-1", "test", "TASK-1", state, now, now); err != nil {
		t.Fatal(err)
	}
}

func parentStateOf(t *testing.T, ctx context.Context, db *sql.DB, id string) string {
	t.Helper()
	var state string
	if err := db.QueryRowContext(ctx, `SELECT parent_state FROM execution_runs WHERE run_id=?`, id).Scan(&state); err != nil {
		t.Fatal(err)
	}
	return state
}

// TestRegressionV69RowsUpgradeByDurableTruth proves the v70 backfill derives
// parent state from durable v69 connector/run truth: terminal rows stay
// terminal (never queued or re-invokable), in-flight rows keep running.
func TestRegressionV69RowsUpgradeByDurableTruth(t *testing.T) {
	db, ctx := v69Fixture(t)
	insertV69Run(t, ctx, db, "run-active", "working")
	insertV69Run(t, ctx, db, "run-completed", "completed")
	insertV69Run(t, ctx, db, "run-failed", "failed")
	insertV69Run(t, ctx, db, "run-cancelled", "canceled")

	if err := Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	for id, want := range map[string]string{
		"run-active":    "executing",
		"run-completed": "settled",
		"run-failed":    "settled",
		"run-cancelled": "settled",
	} {
		if got := parentStateOf(t, ctx, db, id); got != want {
			t.Fatalf("%s parent_state=%q, want %q", id, got, want)
		}
	}

	// v69 terminal rows remain terminal through v70: they are never queued
	// or re-invokable as fresh parents, and active rows are not adoptable
	// either — a legacy row is not a seam parent.
	store := NewParentRunStore(db)
	for _, id := range []string{"run-active", "run-completed", "run-failed", "run-cancelled"} {
		if _, _, err := store.BeginForemanInvocation(ctx, id); err == nil {
			t.Fatalf("%s became re-invokable after upgrade", id)
		}
		raw := legacyParentBrief(t, id)
		if _, _, err := store.AdmitParentRun(ctx, foreman.ParentRunSpec{
			ParentRunID: id, SubmittingAuthority: "foreman:test", SubmissionIdempotencyKey: "submit-" + id,
			ParentBriefJSON: raw,
		}); err == nil {
			t.Fatalf("%s was adopted into a fresh parent after upgrade", id)
		}
	}
	// Terminal truth is preserved on the connector plane too.
	for _, id := range []string{"run-completed", "run-failed", "run-cancelled"} {
		var state string
		if err := db.QueryRowContext(ctx, `SELECT state FROM execution_runs WHERE run_id=?`, id).Scan(&state); err != nil {
			t.Fatal(err)
		}
		if !execution.IsTerminalAttemptState(state) {
			t.Fatalf("%s connector state regressed to %q", id, state)
		}
	}
}

// legacyParentBrief builds a complete canonical parent brief for a legacy run
// id — admission must still refuse it.
func legacyParentBrief(t *testing.T, runID string) []byte {
	t.Helper()
	brief := execution.ParentBrief{
		SchemaVersion: "parent-brief/v1", BriefID: "brief-legacy", RevisionID: "r1",
		IssuedAt: "2026-10-15T11:00:00Z", IssuedBy: "scheduler:test", ParentRunID: runID,
		Project: "coding-hermes-scheduler", Lane: "test", TaskID: "TASK-1", TickID: "tick-1",
		Mission:     "legacy",
		ContextRefs: []execution.ContextRef{{Namespace: "goals", KeyOrID: "goal-1", RetrievedAt: "2026-10-15T11:00:00Z", Authority: "goal"}},
		Scope:       execution.Scope{AllowedPaths: []string{"internal/**"}},
		Constraints: []execution.Constraint{{ID: "c1", Level: "hard", Statement: "stay dark"}},
		NonGoals:    []string{"no live wiring"},
		AcceptanceCriteria: []execution.AcceptanceCriterion{{
			ID: "ac1", Statement: "tests pass", Required: true,
			ProofRequirements: []execution.ProofRequirement{{ID: "p1", Kind: "test", Argv: []string{"go", "test"}, TimeoutS: 60, ExpectExit: 0}},
		}},
		DeadlineAt:         "2026-10-15T18:00:00Z",
		Budget:             execution.ParentBudget{MaxCostUnits: 1, MaxWallTimeoutS: 3600},
		CancellationPolicy: "cooperative",
		Provenance:         execution.Provenance{ForemanRunID: "foreman-run-1", BoardRevision: "board-1"},
	}
	raw, err := json.Marshal(brief)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := execution.BriefDigest(raw)
	if err != nil {
		t.Fatal(err)
	}
	brief.BriefDigest = digest
	out, err := json.Marshal(brief)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func columnExists(t *testing.T, ctx context.Context, db *sql.DB, table, column string) bool {
	t.Helper()
	var n int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM pragma_table_info('`+table+`') WHERE name = ?`, column).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n == 1
}

func indexExists(t *testing.T, ctx context.Context, db *sql.DB, name string) bool {
	t.Helper()
	var n int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type='index' AND name=?`, name).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n > 0
}

func assertParentRunSchemaComplete(t *testing.T, ctx context.Context, db *sql.DB) {
	t.Helper()
	for _, col := range []struct{ table, name string }{
		{"execution_runs", "parent_state"},
		{"execution_runs", "parent_brief_json"},
		{"execution_runs", "foreman_invocation_id"},
		{"execution_runs", "max_active_attempts"},
		{"execution_runs", "quiescence_receipt_ref"},
		{"execution_attempts", "step_id"},
		{"execution_attempts", "step_generation"},
		{"execution_attempts", "brief_digest"},
		{"execution_attempts", "quiescence_receipt_ref"},
		{"execution_attempts", "start_deadline_at"},
		{"execution_attempts", "reconciled_no_start"},
	} {
		if !columnExists(t, ctx, db, col.table, col.name) {
			t.Fatalf("column %s.%s missing after migration", col.table, col.name)
		}
	}
	for _, idx := range []string{"execution_attempts_step_gen_idx", "execution_runs_parent_state_idx"} {
		if !indexExists(t, ctx, db, idx) {
			t.Fatalf("index %s missing after migration", idx)
		}
	}
}

// TestRegressionPartialV70SchemaRepairedNotMarkedCompleteEarly proves a
// partially present v70 schema is completed statement by statement, a hard
// failure never records the version, and a false-complete ledger is repaired
// by v71.
func TestRegressionPartialV70SchemaRepairedNotMarkedCompleteEarly(t *testing.T) {
	t.Run("partial columns are completed before the version is recorded", func(t *testing.T) {
		db, ctx := v69Fixture(t)
		// A previous partial application left two columns behind.
		if _, err := db.ExecContext(ctx, `ALTER TABLE execution_runs ADD COLUMN parent_state TEXT NOT NULL DEFAULT 'queued'`); err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(ctx, `ALTER TABLE execution_attempts ADD COLUMN step_id TEXT NOT NULL DEFAULT ''`); err != nil {
			t.Fatal(err)
		}
		if err := Migrate(ctx, db); err != nil {
			t.Fatal(err)
		}
		assertParentRunSchemaComplete(t, ctx, db)
		v, err := MigrationVersion(ctx, db)
		if err != nil || v < 71 {
			t.Fatalf("migration version=%d err=%v", v, err)
		}
	})

	t.Run("hard failure records no version", func(t *testing.T) {
		db, ctx := v69Fixture(t)
		// Break the schema mid-migration: the run table statements land, the
		// attempt table statements cannot.
		if _, err := db.ExecContext(ctx, `ALTER TABLE execution_attempts RENAME TO execution_attempts_hidden`); err != nil {
			t.Fatal(err)
		}
		if err := Migrate(ctx, db); err == nil {
			t.Fatal("migration over a broken schema unexpectedly succeeded")
		}
		var recorded int
		if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM migrations WHERE version=70`).Scan(&recorded); err != nil {
			t.Fatal(err)
		}
		if recorded != 0 {
			t.Fatal("migration 70 was marked complete early")
		}
		// Restoring the table lets the next boot repair and complete.
		if _, err := db.ExecContext(ctx, `ALTER TABLE execution_attempts_hidden RENAME TO execution_attempts`); err != nil {
			t.Fatal(err)
		}
		if err := Migrate(ctx, db); err != nil {
			t.Fatal(err)
		}
		assertParentRunSchemaComplete(t, ctx, db)
	})

	t.Run("false-complete ledger is repaired by v71", func(t *testing.T) {
		db, ctx := v69Fixture(t)
		// A previous runner recorded version 70 with a partial schema.
		if _, err := db.ExecContext(ctx, `ALTER TABLE execution_runs ADD COLUMN parent_state TEXT NOT NULL DEFAULT 'queued'`); err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(ctx, `INSERT INTO migrations (version, desc) VALUES (70, 'partial: recorded early')`); err != nil {
			t.Fatal(err)
		}
		if err := Migrate(ctx, db); err != nil {
			t.Fatal(err)
		}
		assertParentRunSchemaComplete(t, ctx, db)
	})
}

// TestRegressionLegacyAdmissionCannotBypassParentGates proves the older
// raw admission path enforces the same parent authority gates: a queued
// parent without a foreman invocation produces zero attempt and zero
// dispatch-outbox rows.
func TestRegressionLegacyAdmissionCannotBypassParentGates(t *testing.T) {
	db, ctx := executionTestDB(t)
	if _, _, err := CreateExecutionRun(ctx, db, ExecutionRun{
		RunID: "run-1", SubmittingAuthority: "foreman:test", SubmissionIdempotencyKey: "submit-run-1",
		BriefID: "brief", BriefRevision: "r1", BriefDigest: "digest-run-1", Lane: "test", TaskID: "TASK-1",
	}); err != nil {
		t.Fatal(err)
	}
	if a, _, err := AdmitExecutionAttempt(ctx, db, admission("run-1", "attempt-1", "akey-1", "lease-1", 1, 0)); err == nil {
		t.Fatalf("queued parent bypassed parent gates: %+v", a)
	}
	var attempts, outbox int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM execution_attempts WHERE run_id='run-1'`).Scan(&attempts); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM execution_outbox WHERE run_id='run-1'`).Scan(&outbox); err != nil {
		t.Fatal(err)
	}
	if attempts != 0 || outbox != 0 {
		t.Fatalf("legacy admission left attempts=%d outbox=%d", attempts, outbox)
	}

	// A lease owned by another parent cannot authorize the legacy path.
	makeParentExecutable(t, ctx, db, "run-1")
	foreign := admission("run-1", "attempt-1", "akey-1", "lease-1", 1, 0)
	foreign.Lease.OwnerRunID = "run-foreign"
	if _, _, err := AdmitExecutionAttempt(ctx, db, foreign); !errors.Is(err, execution.ErrLeaseOwnership) {
		t.Fatalf("foreign lease owner error=%v", err)
	}
	// The same path admits only under the default one-active-attempt policy
	// bound to this parent/attempt.
	if _, _, err := AdmitExecutionAttempt(ctx, db, admission("run-1", "attempt-1", "akey-1", "lease-1", 1, 0)); err != nil {
		t.Fatalf("legitimate legacy admission refused: %v", err)
	}
	second := admission("run-1", "attempt-2", "akey-2", "lease-2", 2, 1)
	if _, _, err := AdmitExecutionAttempt(ctx, db, second); !errors.Is(err, execution.ErrTooManyActiveAttempts) {
		t.Fatalf("default concurrency bypassed: error=%v", err)
	}
}

// TestRegressionEvidenceSequenceIsAllocatedFromTheDurableStream proves the
// caller cannot go backward, collide or skip the append-only sequence.
func TestRegressionEvidenceSequenceIsAllocatedFromTheDurableStream(t *testing.T) {
	db, ctx := executionTestDB(t)
	createTestRun(t, ctx, db, "run-1")
	a, _, err := AdmitExecutionAttempt(ctx, db, admission("run-1", "attempt-1", "akey-1", "lease-1", 1, 0))
	if err != nil {
		t.Fatal(err)
	}
	// Sequence 0 allocates the next durable sequence.
	first, _, err := AppendExecutionEvent(ctx, db, ExecutionEvent{
		RunID: "run-1", AttemptID: a.AttemptID, Generation: 1, Sequence: 0,
		Kind: "progress", PayloadDigest: "digest-1",
	})
	if err != nil || first.Sequence != 1 {
		t.Fatalf("allocated sequence=%+v err=%v", first, err)
	}
	// Explicit sequences must continue the stream exactly.
	if _, _, err := AppendExecutionEvent(ctx, db, ExecutionEvent{
		RunID: "run-1", AttemptID: a.AttemptID, Generation: 1, Sequence: 7,
		Kind: "progress", PayloadDigest: "digest-2",
	}); !errors.Is(err, execution.ErrEvidenceSequence) {
		t.Fatalf("skipping sequence error=%v", err)
	}
	if _, _, err := AppendExecutionEvent(ctx, db, ExecutionEvent{
		RunID: "run-1", AttemptID: a.AttemptID, Generation: 1, Sequence: 1,
		Kind: "progress", PayloadDigest: "digest-3",
	}); !errors.Is(err, ErrExecutionConflict) {
		t.Fatalf("colliding sequence error=%v", err)
	}
	second, _, err := AppendExecutionEvent(ctx, db, ExecutionEvent{
		RunID: "run-1", AttemptID: a.AttemptID, Generation: 1, Sequence: 2,
		Kind: "progress", PayloadDigest: "digest-4",
	})
	if err != nil || second.Sequence != 2 {
		t.Fatalf("continuing sequence=%+v err=%v", second, err)
	}
	var count int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM execution_events`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("stream has %d rows, want 2", count)
	}
	if strings.TrimSpace(first.PayloadDigest) == "" {
		t.Fatal("evidence digest missing")
	}
}
