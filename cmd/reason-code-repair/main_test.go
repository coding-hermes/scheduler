package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/coding-hermes/scheduler/internal/database"
)

// SCHED-GAP-16947 acceptance at the CLI layer: the repair subcommand must run
// against a real migrated database, reclassify the Oct 10-12 block, refuse an
// unmigrated database, and be safe to re-run.

// newRepairTestDB builds a fully migrated database with two lanes in the
// measured blackout shape: one lane dark all window, one lane that committed.
func newRepairTestDB(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "scheduler.db")
	db, err := database.InitDB(path)
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	defer db.Close()
	ctx := context.Background()
	for _, p := range []string{"dark-lane", "busy-lane"} {
		if _, err := db.ExecContext(ctx, `INSERT INTO projects (name, repo_url, workdir, enabled, created_at, updated_at)
			VALUES (?, 'https://example.com/x', '/tmp/x', 1, '2026-10-10T00:00:00Z', '2026-10-10T00:00:00Z')`, p); err != nil {
			t.Fatalf("seed project %s: %v", p, err)
		}
	}
	rows := []struct {
		id, project, outcome string
		commits              int
	}{
		{"busy-1", "busy-lane", "committed", 2},
		{"dark-1", "dark-lane", "dry_run", 0},
		{"dark-2", "dark-lane", "dry_run", 0},
	}
	for i, r := range rows {
		at := "2026-10-1" + string(rune('0'+i)) + "T03:00:00Z"
		if _, err := db.ExecContext(ctx, `INSERT INTO ticks
			(id, project_name, status, outcome, spawned_at, completed_at, tokens_in, tokens_out, commits, files_changed, created_at)
			VALUES (?, ?, 'completed', ?, ?, ?, 0, 0, ?, 0, ?)`,
			r.id, r.project, r.outcome, at, at, r.commits, at); err != nil {
			t.Fatalf("seed tick %s: %v", r.id, err)
		}
	}
	return path
}

// TestReasonCodeRepair_CLIAppliesAndIsIdempotent runs the real subcommand.
func TestReasonCodeRepair_CLIAppliesAndIsIdempotent(t *testing.T) {
	path := newRepairTestDB(t)

	var out, errOut bytes.Buffer
	code := run([]string{"--db", path, "--from", "2026-10-10", "--to", "2026-10-13", "--apply", "--json"}, &out, &errOut)
	if code != 0 {
		t.Fatalf("run(apply) = %d, stderr=%s", code, errOut.String())
	}
	var rep database.ReasonCodeRepairReport
	if err := json.Unmarshal(out.Bytes(), &rep); err != nil {
		t.Fatalf("decode JSON report: %v\n%s", err, out.String())
	}
	if !rep.Apply || rep.Applied != 2 || rep.Candidates != 2 {
		t.Fatalf("report apply=%v applied=%d candidates=%d, want true/2/2 (report=%+v)", rep.Apply, rep.Applied, rep.Candidates, rep)
	}
	if rep.Classes[database.TickReasonCodeLaneUnavailable] != 2 {
		t.Errorf("classes = %v, want 2 lane_unavailable (the dark lane's zero-token dry ticks)", rep.Classes)
	}

	// The codes are in the COLUMN, with the derivation annotation.
	db, err := database.InitDB(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer db.Close()
	var code2, derivation string
	if err := db.QueryRow(`SELECT reason_code, reason_code_derivation FROM ticks WHERE id='dark-1'`).
		Scan(&code2, &derivation); err != nil {
		t.Fatalf("read repaired row: %v", err)
	}
	if code2 != database.TickReasonCodeLaneUnavailable {
		t.Errorf("dark-1 reason_code = %q, want %q", code2, database.TickReasonCodeLaneUnavailable)
	}
	if !strings.Contains(derivation, "three_day_lane_blackout") {
		t.Errorf("dark-1 derivation = %q, want the blackout class annotation", derivation)
	}

	// The productive lane keeps its empty code: nothing is owed.
	var busyCode string
	if err := db.QueryRow(`SELECT reason_code FROM ticks WHERE id='busy-1'`).Scan(&busyCode); err != nil {
		t.Fatalf("read busy row: %v", err)
	}
	if busyCode != "" {
		t.Errorf("productive row reason_code = %q, want \"\"", busyCode)
	}

	// Idempotent: the second run reports the work as already stamped.
	out.Reset()
	errOut.Reset()
	if code := run([]string{"--db", path, "--from", "2026-10-10", "--to", "2026-10-13", "--apply", "--json"}, &out, &errOut); code != 0 {
		t.Fatalf("second run = %d, stderr=%s", code, errOut.String())
	}
	var again database.ReasonCodeRepairReport
	if err := json.Unmarshal(out.Bytes(), &again); err != nil {
		t.Fatalf("decode second report: %v", err)
	}
	if again.Applied != 0 || again.AlreadyStamped != 2 {
		t.Errorf("second run applied=%d already_stamped=%d, want 0/2", again.Applied, again.AlreadyStamped)
	}
}

// TestReasonCodeRepair_CLIDryRunWritesNothing pins the default posture.
func TestReasonCodeRepair_CLIDryRunWritesNothing(t *testing.T) {
	path := newRepairTestDB(t)
	var out, errOut bytes.Buffer
	if code := run([]string{"--db", path, "--from", "2026-10-10", "--to", "2026-10-13"}, &out, &errOut); code != 0 {
		t.Fatalf("dry run = %d, stderr=%s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "DRY RUN") {
		t.Errorf("dry-run output does not say DRY RUN:\n%s", out.String())
	}
	db, err := database.InitDB(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer db.Close()
	var stamped int
	if err := db.QueryRow(`SELECT count(*) FROM ticks WHERE reason_code <> ''`).Scan(&stamped); err != nil {
		t.Fatalf("count stamped: %v", err)
	}
	if stamped != 0 {
		t.Errorf("dry run wrote %d codes, want 0", stamped)
	}
}

// TestReasonCodeRepair_CLIRejectsUnmigratedDB proves the schema guard: a
// database whose ticks table predates v91 is refused with a migrate-first
// instruction instead of failing every UPDATE mid-pass.
func TestReasonCodeRepair_CLIRejectsUnmigratedDB(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if _, err := db.Exec(`CREATE TABLE ticks (id TEXT PRIMARY KEY)`); err != nil {
		t.Fatalf("create pre-v91 ticks table: %v", err)
	}
	db.Close()

	var out, errOut bytes.Buffer
	code := run([]string{"--db", path, "--apply"}, &out, &errOut)
	if code != 1 {
		t.Fatalf("run against a database without the reason columns = %d, want 1 (stderr=%s)", code, errOut.String())
	}
	if !strings.Contains(errOut.String(), "migrate it first") {
		t.Errorf("guard message = %q, want a migrate-first instruction", errOut.String())
	}
}
