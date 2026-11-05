package scheduler

// lane_hooks_test.go — S13 wired-but-never-run, proven at the call site.
//
// The lane-hook machinery (internal/lanehook, slot_pool's hookPlan/runPreHooks/
// runPostHooks) shipped with unit tests for its parts and NO test of the wiring
// that matters:
//
//   * a PRE step that refuses must return an error the spawn path turns into
//     TickFailed with `hook_failed:<id>` — and it must do so BEFORE a gateway
//     spawn, so a vetoed tick costs a receipt and nothing else;
//   * the POST list must still run when PRE vetoed, because a lease taken in
//     PRE has to be released;
//   * the receipt must record the RESOLVED argv, not just the command string —
//     with placeholders the same definition sends different argv per lane.
//
// These tests drive the real ExecRunner against a real SQLite (the migrated
// test schema) and real scripts, so a receipt row is evidence rather than a
// mock's opinion.

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coding-hermes/scheduler/internal/lanehook"
)

// writeLaneHookScript writes an executable shell script with the given body.
func writeLaneHookScript(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	script := "#!/bin/sh\nset -eu\n" + body + "\n"
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatalf("write %s: %v", p, err)
	}
	return p
}

// writeHooksToml writes a hooks file with one PRE and one POST step for lane.
func writeHooksToml(t *testing.T, dir, lane, preScript, postScript string) string {
	t.Helper()
	doc := `[scheduler]
hook_order = "level_first"

[[hook]]
id = "pre-step"
phase = "pre"
level = "lane"
target = "` + lane + `"
priority = 20
command = "` + preScript + `"
args = ["--worktree", "{workdir}", "--lane", "{lane}", "--project", "{project}"]
timeout_s = 30
on_failure = "closed"

[[hook]]
id = "post-step"
phase = "post"
level = "lane"
target = "` + lane + `"
priority = 80
command = "` + postScript + `"
args = ["--worktree", "{workdir}", "--lane", "{lane}"]
timeout_s = 30
on_failure = "closed"
`
	p := filepath.Join(dir, "hooks.toml")
	if err := os.WriteFile(p, []byte(doc), 0o600); err != nil {
		t.Fatalf("write hooks.toml: %v", err)
	}
	return p
}

// laneHookFixture builds a pool armed with a hooks file and a project whose
// workdir exists, returning the pool, the project and the report directory the
// scripts write into (so "did it actually run" is checkable independently of
// the receipts).
func laneHookFixture(t *testing.T, db *sql.DB, lane, preBody, postBody string) (*SlotPool, PackedProject, string) {
	t.Helper()
	dir := t.TempDir()
	reportDir := filepath.Join(dir, "report")
	if err := os.MkdirAll(reportDir, 0o755); err != nil {
		t.Fatalf("mkdir report: %v", err)
	}
	work := filepath.Join(dir, "worktree")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatalf("mkdir worktree: %v", err)
	}
	pre := writeLaneHookScript(t, dir, "pre.sh", preBody)
	post := writeLaneHookScript(t, dir, "post.sh", postBody)
	hooksPath := writeHooksToml(t, dir, lane, pre, post)

	p := NewSlotPool(1, nil, nil)
	set, err := lanehook.Open(hooksPath)
	if err != nil {
		t.Fatalf("arm hooks: %v", err)
	}
	if got := len(set.Steps()); got != 2 {
		t.Fatalf("armed %d steps, want 2", got)
	}
	p.SetHooks(set, lanehook.NewExecRunner(db, nil))

	return p, PackedProject{
		Name:        lane,
		Workdir:     work,
		RepoURL:     "local:" + filepath.Join(dir, "repo"),
		NamespaceID: "ns-lanehooks",
	}, reportDir
}

func hookReceipts(t *testing.T, db *sql.DB, tickID string) []struct {
	Phase, HookID, State, Argv string
} {
	t.Helper()
	// run_id, not seq: PRE and POST each number their own steps from 1, so
	// ordering by seq interleaves the phases. run_id is insertion order, which
	// is the order the runner actually executed.
	rows, err := db.Query(`SELECT phase, hook_id, state, argv FROM hook_runs WHERE tick_id = ? ORDER BY run_id`, tickID)
	if err != nil {
		t.Fatalf("query hook_runs: %v", err)
	}
	defer rows.Close()
	var out []struct{ Phase, HookID, State, Argv string }
	for rows.Next() {
		var r struct{ Phase, HookID, State, Argv string }
		if err := rows.Scan(&r.Phase, &r.HookID, &r.State, &r.Argv); err != nil {
			t.Fatalf("scan hook_runs: %v", err)
		}
		out = append(out, r)
	}
	return out
}

// TestLaneHooks_PreVetoFailsTickAndPostStillRuns is the contract the spawn path
// depends on: a refusing PRE returns ErrHookFailed (which the caller writes as
// `hook_failed:<id>` and TickFailed), and POST runs anyway so a PRE-acquired
// lease is released.
func TestLaneHooks_PreVetoFailsTickAndPostStillRuns(t *testing.T) {
	db := newTestDB(t)
	const (
		lane   = "lh-proj-qa"
		tickID = "lh-proj-qa-2026-11-05-00-00-00"
	)
	preBody := `echo "PRE_RAN" >> "$REPORT_DIR/pre.marker"; echo "refusing: disk floor" >&2; exit 65`
	postBody := `echo "POST_RAN" >> "$REPORT_DIR/post.marker"; exit 0`

	dir := t.TempDir()
	_ = dir
	pool, proj, reportDir := laneHookFixture(t, db, lane, preBody, postBody)
	t.Setenv("REPORT_DIR", reportDir)

	plan, have := pool.hookPlan(proj)
	if !have {
		t.Fatal("hookPlan returned no plan for an armed lane — the feature-off path was taken")
	}
	if len(plan.Pre) != 1 || len(plan.Post) != 1 {
		t.Fatalf("plan = %d pre / %d post, want 1/1", len(plan.Pre), len(plan.Post))
	}

	err := pool.runPreHooks(proj, tickID, time.Now(), plan)
	if err == nil {
		t.Fatal("runPreHooks returned nil for a hook that exited 65 — a veto must veto")
	}
	var hf *lanehook.ErrHookFailed
	if !errors.As(err, &hf) {
		t.Fatalf("error type = %T (%v), want *lanehook.ErrHookFailed so the caller can name hook_failed:<id>", err, err)
	}
	if hf.HookID != "pre-step" {
		t.Errorf("ErrHookFailed.HookID = %q, want pre-step", hf.HookID)
	}
	if hf.Code != 65 {
		t.Errorf("ErrHookFailed.Code = %d, want 65", hf.Code)
	}

	// POST runs even though PRE vetoed — the spawn path calls it before
	// returning (slot_pool.go, "POST still runs: a lease taken in PRE must be
	// released").
	pool.runPostHooks(proj, tickID, plan)

	if _, statErr := os.Stat(filepath.Join(reportDir, "pre.marker")); statErr != nil {
		t.Errorf("the PRE script did not run: %v", statErr)
	}
	if _, statErr := os.Stat(filepath.Join(reportDir, "post.marker")); statErr != nil {
		t.Errorf("the POST script did not run after a PRE veto: %v", statErr)
	}

	rec := hookReceipts(t, db, tickID)
	if len(rec) != 2 {
		t.Fatalf("receipts = %d, want 2 (pre + post): %+v", len(rec), rec)
	}
	if rec[0].Phase != "pre" || rec[0].State != "failed" {
		t.Errorf("first receipt = %+v, want pre/failed", rec[0])
	}
	if rec[1].Phase != "post" || rec[1].State != "ok" {
		t.Errorf("second receipt = %+v, want post/ok — POST must run after a PRE veto", rec[1])
	}

	// The receipt must carry the RESOLVED argv: {workdir} and {project} are the
	// whole point of a placeholder, and "what did we actually send" is the only
	// question a failed hook's reader has.
	if !strings.Contains(rec[0].Argv, proj.Workdir) {
		t.Errorf("pre receipt argv = %q, want it to contain the expanded workdir %q", rec[0].Argv, proj.Workdir)
	}
	if strings.Contains(rec[0].Argv, "{workdir}") || strings.Contains(rec[0].Argv, "{project}") {
		t.Errorf("pre receipt argv = %q still carries unexpanded placeholders", rec[0].Argv)
	}
	// Project, not lane: `lh-proj-qa` is project `lh-proj`, role `qa`.
	if !strings.Contains(rec[0].Argv, "--project lh-proj") {
		t.Errorf("pre receipt argv = %q, want --project lh-proj (the PROJECT, not the lane name)", rec[0].Argv)
	}
	if strings.Contains(rec[0].Argv, "--project lh-proj-qa") {
		t.Errorf("pre receipt argv = %q expanded {project} to the LANE name", rec[0].Argv)
	}
}

// TestLaneHooks_AllGreenRunsBothPhases is the positive control: the same wiring
// with a passing PRE must return nil and leave two ok receipts.
func TestLaneHooks_AllGreenRunsBothPhases(t *testing.T) {
	db := newTestDB(t)
	const (
		lane   = "lh-green-qa"
		tickID = "lh-green-qa-2026-11-05-00-10-00"
	)
	dir := t.TempDir()
	pool, proj, reportDir := laneHookFixture(t, db, lane,
		`echo "PRE_RAN" >> "$REPORT_DIR/pre.marker"; exit 0`,
		`echo "POST_RAN" >> "$REPORT_DIR/post.marker"; exit 0`)
	_ = dir
	t.Setenv("REPORT_DIR", reportDir)

	plan, have := pool.hookPlan(proj)
	if !have {
		t.Fatal("no plan for an armed lane")
	}
	if err := pool.runPreHooks(proj, tickID, time.Now(), plan); err != nil {
		t.Fatalf("runPreHooks = %v, want nil for a passing PRE", err)
	}
	pool.runPostHooks(proj, tickID, plan)

	rec := hookReceipts(t, db, tickID)
	if len(rec) != 2 {
		t.Fatalf("receipts = %d, want 2: %+v", len(rec), rec)
	}
	for _, r := range rec {
		if r.State != "ok" {
			t.Errorf("receipt %s/%s state = %q, want ok", r.Phase, r.HookID, r.State)
		}
		if r.Argv == "" {
			t.Errorf("receipt %s/%s has an empty argv", r.Phase, r.HookID)
		}
	}
}

// TestLaneHooks_NoHooksArmedIsTheOldPath proves the feature-off path stays free:
// with no set armed, hookPlan reports no plan and nothing is spawned or recorded.
func TestLaneHooks_NoHooksArmedIsTheOldPath(t *testing.T) {
	db := newTestDB(t)
	pool := NewSlotPool(1, nil, nil)
	proj := PackedProject{Name: "lh-none-qa", Workdir: t.TempDir(), RepoURL: "local:/tmp/x"}
	if _, have := pool.hookPlan(proj); have {
		t.Fatal("hookPlan reported a plan with no hooks armed — the fleet's default path changed")
	}
	if rec := hookReceipts(t, db, "anything"); len(rec) != 0 {
		t.Fatalf("receipts written with no hooks armed: %+v", rec)
	}
}

// TestLaneHooks_PreBudgetIsBounded keeps the promise that a hung PRE cannot hold
// a slot forever: a step whose script outlives its timeout returns an error and
// records a timeout receipt with the group killed.
func TestLaneHooks_PreBudgetIsBounded(t *testing.T) {
	if testing.Short() {
		t.Skip("timeout case sleeps past a hook budget")
	}
	db := newTestDB(t)
	const (
		lane   = "lh-slow-qa"
		tickID = "lh-slow-qa-2026-11-05-00-20-00"
	)
	pool, proj, reportDir := laneHookFixture(t, db, lane,
		`sleep 5; exit 0`, `exit 0`)
	t.Setenv("REPORT_DIR", reportDir)

	plan, have := pool.hookPlan(proj)
	if !have {
		t.Fatal("no plan")
	}
	// Force a 1s budget by shrinking the step timeout in the plan.
	for i := range plan.Pre {
		plan.Pre[i].TimeoutS = 1
	}
	start := time.Now()
	err := pool.runPreHooks(proj, tickID, time.Now(), plan)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("runPreHooks returned nil for a hook that overran its budget")
	}
	if elapsed > 4*time.Second {
		t.Errorf("runPreHooks took %s with a 1s step budget — the timeout did not bound it", elapsed)
	}
	rec := hookReceipts(t, db, tickID)
	if len(rec) == 0 || rec[0].State != "timeout" {
		t.Errorf("receipts = %+v, want a timeout state on the first", rec)
	}
}

var _ = context.Background
