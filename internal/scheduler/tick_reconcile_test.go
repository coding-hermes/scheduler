package scheduler_test

// RELEASE-048: late-commit reconciliation for failed/timeout ticks.
//
// The arms these tests pin:
//
//   - a failed tick whose window landed commits AFTER booking gets a
//     non-zero commit count, the SCHED-GAP-104 split, an advanced
//     completed_at and a following project row — while its status stays
//     failed (the failure is real; only the artifact accounting was
//     missing) — the reconciler half of the fix;
//   - a failed tick whose window already held commits at booking time gets
//     them measured by lifecycle.Complete itself (the terminal-write half);
//   - the pass is idempotent: a second pass annotates nothing and moves
//     nothing;
//   - a genuinely zero window is stamped (reconciled_at) and never
//     fabricated into work; within the recheck interval it is not
//     re-scanned;
//   - a project with no measurable git repo is stamped unmeasurable with
//     commits left 0 — an honest figure, never a sentinel borrowed from the
//     terminal-write path;
//   - the dry-run mode writes nothing.

import (
	"database/sql"

	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coding-hermes/scheduler/internal/database"
	"github.com/coding-hermes/scheduler/internal/scheduler"
)

// relengFixtures bundles what every reconciliation test needs: a real temp
// git repo, a project row that resolves to it, and a failed tick row.
type relengFixtures struct {
	db        *sql.DB
	lt        *scheduler.LifecycleTracker
	workdir   string
	project   string
	tickID    string
	spawned   time.Time
	completed time.Time
}

// relengBase is the pinned clock every fixture hangs off — all instants are
// derived from it, never from the wall clock.
var relengBase = time.Date(2026, 11, 7, 10, 25, 21, 0, time.UTC)

// makeTempGitRepoRel builds a throwaway git repo (external-test copy of the
// internal test package's helper — the two test packages do not share
// symbols, and this package needs the same fixture).
func makeTempGitRepoRel(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %s in %s: %v\n%s", strings.Join(args, " "), dir, err, out)
		}
	}
	run("init")
	run("config", "user.email", "test@test.com")
	run("config", "user.name", "Test")
	return dir
}

// gitCommitAtRel commits filename into dir with an EXPLICIT committer/author
// date so the test, not the wall clock, chooses the commit ordinals the
// window measurements read (same convention as the internal package's
// gitCommitAt — pinned dates, no sleeps).
func gitCommitAtRel(t *testing.T, dir, filename, content string, when time.Time) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, filename), []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", filename, err)
	}
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_DATE="+when.UTC().Format(time.RFC3339),
			"GIT_COMMITTER_DATE="+when.UTC().Format(time.RFC3339))
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	run("add", filename)
	run("commit", "-m", "add "+filename)
}

func newRelengFixtures(t *testing.T, withGit bool) *relengFixtures {
	t.Helper()
	if withGit {
		if _, err := exec.LookPath("git"); err != nil {
			t.Skip("git not available")
		}
	}
	db := newTestDB(t)
	f := &relengFixtures{
		db:      db,
		lt:      scheduler.NewLifecycleTracker(db),
		project: "releng-reconcile-lane",
		tickID:  "releng-reconcile-lane-2026-11-07-10-25-21",
		spawned: relengBase,
		// The tick's booked end: 3h wall, the releng shape.
		completed: relengBase.Add(3 * time.Hour),
	}
	if withGit {
		f.workdir = makeTempGitRepoRel(t)
		// The repo's history starts BEFORE the tick (a real repo always has
		// prior commits). Without this root commit gitFilesChanged's
		// diff-tree on a parentless commit reads 0 — the fixture must mirror
		// the repos the reconciler actually measures.
		gitCommitAtRel(t, f.workdir, "init.go", "package init", relengBase.Add(-time.Hour))
	}
	if err := database.CreateProject(t.Context(), db, &database.Project{
		Name:    f.project,
		RepoURL: "https://example.com/" + f.project,
		Workdir: f.workdir,
		Weight:  10, Priority: 5, CooldownS: 0, DecayRate: 1.0,
		Model: "m", Provider: "p", Enabled: true,
	}); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	// The tick row exactly as the drop path wrote it: status=failed,
	// outcome=failed, commits=0, no measurement of any kind.
	if _, err := db.Exec(`
		INSERT INTO ticks (id, project_name, status, outcome, spawned_at, completed_at, exit_code, commits, files_changed, created_at)
		VALUES (?, ?, 'failed', 'failed', ?, ?, -1, 0, 0, ?)
	`, f.tickID, f.project, f.spawned.Format(time.RFC3339), f.completed.Format(time.RFC3339), f.spawned.Format(time.RFC3339)); err != nil {
		t.Fatalf("insert failed tick row: %v", err)
	}
	// The project row as lifecycle.Complete left it: last_tick_completed is
	// this tick's completed_at (it is the project's newest tick),
	// last_tick_status failed.
	if _, err := db.Exec(`
		UPDATE projects SET last_tick_completed = ?, last_tick_status = 'failed' WHERE name = ?
	`, f.completed.Format(time.RFC3339), f.project); err != nil {
		t.Fatalf("seed project row: %v", err)
	}
	return f
}

// tickRow is the reconciler's read-back shape for one tick row.
type tickRow struct {
	status       string
	commits      int
	filesChanged int
	codeCommits  int
	boardCommits int
	completedAt  string
	reconciledAt sql.NullString
}

func readTickRow(t *testing.T, db *sql.DB, id string) tickRow {
	t.Helper()
	var r tickRow
	err := db.QueryRow(`
		SELECT status, commits, files_changed, code_commits, board_commits, completed_at, reconciled_at
		FROM ticks WHERE id = ?
	`, id).Scan(&r.status, &r.commits, &r.filesChanged, &r.codeCommits, &r.boardCommits, &r.completedAt, &r.reconciledAt)
	if err != nil {
		t.Fatalf("read tick row %s: %v", id, err)
	}
	return r
}

// TestRELEASE048_ReconcileAnnotatesLateCommits is arm 1 and arm 4 in one
// row: a failed tick whose session landed a commit AFTER the row was booked
// terminal gets the commit counted, the completion instant advanced to the
// commit's own timestamp, and the project row advanced with it — while the
// row's status stays failed.
func TestRELEASE048_ReconcileAnnotatesLateCommits(t *testing.T) {
	f := newRelengFixtures(t, true)

	// One commit INSIDE the booked window would be the terminal-write half's
	// job (its absence here is what makes this the LATE-commit case); the
	// late commit lands 2h13m after completed_at — the measured releng
	// shape (failed 13:25:22, release commits 15:38:11).
	late := f.completed.Add(2*time.Hour + 13*time.Minute)
	gitCommitAtRel(t, f.workdir, "release.go", "package release", late)

	now := late.Add(time.Minute)
	rep, ran := f.lt.ReconcileLateTickCommits(scheduler.ReconcileOpts{
		Now: now, Apply: true,
	})
	if !ran {
		t.Fatal("pass did not run")
	}
	if rep.Annotated != 1 {
		t.Fatalf("annotated = %d, want 1 (skipped: %v)", rep.Annotated, rep.Skipped)
	}
	if rep.Advanced != 1 {
		t.Errorf("advanced = %d, want 1", rep.Advanced)
	}
	row := readTickRow(t, f.db, f.tickID)
	if row.commits != 1 {
		t.Errorf("commits = %d, want 1 (the late commit must land on the row)", row.commits)
	}
	if row.filesChanged != 1 {
		t.Errorf("files_changed = %d, want 1", row.filesChanged)
	}
	// SCHED-GAP-104 split: the commit touches a code path, so it must
	// classify as code (board_commits 0) — never the unmeasured -1.
	if row.codeCommits != 1 || row.boardCommits != 0 {
		t.Errorf("split = (%d code, %d board), want (1, 0)", row.codeCommits, row.boardCommits)
	}
	// Arm 4: completed_at advanced to the commit's committer instant (never
	// backward), and the status is UNTOUCHED — reconciliation is accounting,
	// not a verdict rewrite.
	if row.completedAt != late.Format(time.RFC3339) {
		t.Errorf("completed_at = %s, want %s (advanced to the newest commit)", row.completedAt, late.Format(time.RFC3339))
	}
	if row.status != "failed" {
		t.Errorf("status = %s, want failed (must never be rewritten)", row.status)
	}
	if !row.reconciledAt.Valid || row.reconciledAt.String == "" {
		t.Errorf("reconciled_at not stamped — the idempotence watermark is missing")
	}
	// Arm 4's project-row half: last_tick_completed follows the tick while
	// the project row still points at it.
	var projectCompleted string
	if err := f.db.QueryRow(`SELECT last_tick_completed FROM projects WHERE name = ?`, f.project).Scan(&projectCompleted); err != nil {
		t.Fatalf("read project row: %v", err)
	}
	if projectCompleted != late.Format(time.RFC3339) {
		t.Errorf("project last_tick_completed = %s, want %s (must follow the reconciled tick)", projectCompleted, late.Format(time.RFC3339))
	}
	var projectStatus string
	if err := f.db.QueryRow(`SELECT last_tick_status FROM projects WHERE name = ?`, f.project).Scan(&projectStatus); err != nil {
		t.Fatalf("read project status: %v", err)
	}
	if projectStatus != "failed" {
		t.Errorf("project last_tick_status = %s, want failed (the API must keep telling the truth about the verdict)", projectStatus)
	}
}

// TestRELEASE048_ReconcileIsIdempotent — the second pass annotates nothing
// and moves nothing: an annotated row (commits > 0) drops out of the
// candidate set by the commits=0 filter alone.
func TestRELEASE048_ReconcileIsIdempotent(t *testing.T) {
	f := newRelengFixtures(t, true)
	late := f.completed.Add(time.Hour)
	gitCommitAtRel(t, f.workdir, "late.go", "package late", late)
	now := late.Add(time.Minute)
	if rep, _ := f.lt.ReconcileLateTickCommits(scheduler.ReconcileOpts{Now: now, Apply: true}); rep.Annotated != 1 {
		t.Fatalf("premise: first pass annotated %d rows, want 1", rep.Annotated)
	}
	before := readTickRow(t, f.db, f.tickID)

	// A second commit lands before the second pass — it must NOT be folded
	// into the already-annotated row: the row was annotated once, the pass
	// never re-touches it.
	gitCommitAtRel(t, f.workdir, "later.go", "package later", now.Add(-time.Second))
	rep, _ := f.lt.ReconcileLateTickCommits(scheduler.ReconcileOpts{Now: now.Add(time.Minute), Apply: true})
	if rep.Annotated != 0 {
		t.Fatalf("second pass annotated %d rows, want 0 (idempotence)", rep.Annotated)
	}
	after := readTickRow(t, f.db, f.tickID)
	if after != before {
		t.Errorf("row moved on the second pass: before=%+v after=%+v", before, after)
	}
}

// TestRELEASE048_ReconcileZeroWindowStamped — a genuinely empty window is
// stamped (reconciled_at) with commits left 0 — a measured zero is honest —
// and the stamp suppresses re-scanning within the recheck interval.
func TestRELEASE048_ReconcileZeroWindowStamped(t *testing.T) {
	f := newRelengFixtures(t, true)
	// A commit BEFORE the tick spawned (repo history) — outside every window.
	gitCommitAtRel(t, f.workdir, "old.go", "package old", relengBase.Add(-time.Hour))
	now := f.completed.Add(time.Minute)

	rep, _ := f.lt.ReconcileLateTickCommits(scheduler.ReconcileOpts{Now: now, Apply: true, RecheckAfter: 15 * time.Minute})
	if rep.StillZero != 1 || rep.Annotated != 0 {
		t.Fatalf("first pass: still_zero=%d annotated=%d, want 1/0", rep.StillZero, rep.Annotated)
	}
	row := readTickRow(t, f.db, f.tickID)
	if row.commits != 0 {
		t.Errorf("commits = %d, want 0 (an empty window must never be fabricated into work)", row.commits)
	}
	if row.completedAt != f.completed.Format(time.RFC3339) {
		t.Errorf("completed_at = %s, want unchanged %s", row.completedAt, f.completed.Format(time.RFC3339))
	}
	if !row.reconciledAt.Valid {
		t.Error("reconciled_at not stamped on the zero-find pass")
	}

	// Within the recheck interval the watermark suppresses the sweep — the
	// pass must not git-sweep the same zero row on every eval. Examined
	// counts SQL-prefilter survivors, so the assertion is that nothing was
	// re-measured (StillZero) and the watermark is unchanged.
	rep2, _ := f.lt.ReconcileLateTickCommits(scheduler.ReconcileOpts{Now: now.Add(5 * time.Minute), Apply: true, RecheckAfter: 15 * time.Minute})
	if rep2.StillZero != 0 || rep2.Annotated != 0 {
		t.Errorf("second pass re-measured the row: still_zero=%d annotated=%d, want 0/0 (watermark suppression)", rep2.StillZero, rep2.Annotated)
	}
	var stampAfter string
	if err := f.db.QueryRow(`SELECT reconciled_at FROM ticks WHERE id=?`, f.tickID).Scan(&stampAfter); err != nil {
		t.Fatal(err)
	}
	row1 := readTickRow(t, f.db, f.tickID)
	if stampAfter != row1.reconciledAt.String {
		t.Errorf("reconciled_at moved from %s to %s — the suppressed pass must not re-stamp", row1.reconciledAt.String, stampAfter)
	}
	// Past the interval it is re-checked — and still honest zero.
	rep3, _ := f.lt.ReconcileLateTickCommits(scheduler.ReconcileOpts{Now: now.Add(16 * time.Minute), Apply: true, RecheckAfter: 15 * time.Minute})
	if rep3.StillZero != 1 {
		t.Errorf("re-check pass still_zero=%d, want 1 (the window is still empty)", rep3.StillZero)
	}
}

// TestRELEASE048_ReconcileUnmeasurableStamped — a project whose workdir has
// no git repo (the fleet's stand-in shape) is stamped once with commits left
// 0, and the repo_url local: fallback is what makes the MEASURABLE case
// work: a stand-in workdir plus a local: repo_url resolves to the real
// checkout and the late commit lands on the row.
func TestRELEASE048_ReconcileUnmeasurableStampedAndLocalFallback(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	db := newTestDB(t)
	lt := scheduler.NewLifecycleTracker(db)

	// Shape 1: empty workdir, non-local repo_url → unmeasurable, stamped once.
	if err := database.CreateProject(t.Context(), db, &database.Project{
		Name: "unmeasurable-lane", RepoURL: "https://example.com/u", Workdir: "",
		Weight: 10, Priority: 5, CooldownS: 0, DecayRate: 1.0,
		Model: "m", Provider: "p", Enabled: true,
	}); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	_, err := db.Exec(`
		INSERT INTO ticks (id, project_name, status, outcome, spawned_at, completed_at, exit_code, commits, files_changed, created_at)
		VALUES ('unmeasurable-tick', 'unmeasurable-lane', 'failed', 'failed', ?, ?, -1, 0, 0, ?)
	`, relengBase.Format(time.RFC3339), relengBase.Add(time.Hour).Format(time.RFC3339), relengBase.Format(time.RFC3339))
	if err != nil {
		t.Fatalf("insert tick: %v", err)
	}
	rep, _ := lt.ReconcileLateTickCommits(scheduler.ReconcileOpts{Now: relengBase.Add(2 * time.Hour), Apply: true})
	if rep.Unmeasurable != 1 {
		t.Fatalf("unmeasurable = %d, want 1 (skipped: %v)", rep.Unmeasurable, rep.Skipped)
	}
	var commits int
	if err := db.QueryRow(`SELECT commits FROM ticks WHERE id='unmeasurable-tick'`).Scan(&commits); err != nil {
		t.Fatal(err)
	}
	if commits != 0 {
		t.Errorf("commits = %d, want 0 (honest figure for an unmeasurable window — the -1 sentinel belongs to the terminal-write path)", commits)
	}

	// Shape 2: stand-in workdir (not a git root) + repo_url local:<git repo>
	// — the releng lane's real shape. The reconciler must resolve through
	// repo_url and count the late commit.
	repo := makeTempGitRepoRel(t)
	gitCommitAtRel(t, repo, "init.go", "package init", relengBase.Add(-2*time.Hour))
	lane := "standin-releng-lane"
	if err := database.CreateProject(t.Context(), db, &database.Project{
		Name: lane, RepoURL: "local:" + repo, Workdir: "/tmp/not-a-git-repo-standin",
		Weight: 10, Priority: 5, CooldownS: 0, DecayRate: 1.0,
		Model: "m", Provider: "p", Enabled: true,
	}); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	tickID := "standin-releng-tick"
	late := relengBase.Add(time.Hour)
	gitCommitAtRel(t, repo, "cut.go", "package cut", late)
	if _, err := db.Exec(`
		INSERT INTO ticks (id, project_name, status, outcome, spawned_at, completed_at, exit_code, commits, files_changed, created_at)
		VALUES (?, ?, 'failed', 'failed', ?, ?, -1, 0, 0, ?)
	`, tickID, lane, relengBase.Format(time.RFC3339), relengBase.Add(30*time.Minute).Format(time.RFC3339), relengBase.Format(time.RFC3339)); err != nil {
		t.Fatalf("insert tick: %v", err)
	}
	rep2, _ := lt.ReconcileLateTickCommits(scheduler.ReconcileOpts{Now: late.Add(time.Minute), Apply: true})
	if rep2.Annotated != 1 {
		t.Fatalf("local-fallback annotated = %d, want 1 (skipped: %v)", rep2.Annotated, rep2.Skipped)
	}
	row := readTickRow(t, db, tickID)
	if row.commits != 1 {
		t.Errorf("commits = %d, want 1 (the local: repo_url fallback must reach the real checkout)", row.commits)
	}
}

// TestRELEASE048_CompleteMeasuresFailedWindow — the terminal-write half:
// lifecycle.Complete measures the git window for a failed outcome whose
// window already held a commit at booking time (previously the struct-default
// commits=0 zero-accounted it), respects the unmeasured sentinel for an
// unmeasurable repo, and leaves a deferred outcome untouched.
func TestRELEASE048_CompleteMeasuresFailedWindow(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	db := newTestDB(t)
	lt := scheduler.NewLifecycleTracker(db)
	repo := makeTempGitRepoRel(t)
	lane := "complete-measure-lane"
	gitCommitAtRel(t, repo, "init.go", "package init", relengBase.Add(-time.Hour))
	if err := database.CreateProject(t.Context(), db, &database.Project{
		Name: lane, RepoURL: "https://example.com/" + lane, Workdir: repo,
		Weight: 10, Priority: 5, CooldownS: 0, DecayRate: 1.0,
		Model: "m", Provider: "p", Enabled: true,
	}); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	if err := lt.Enqueue(lane, "cm-failed-tick"); err != nil {
		t.Fatal(err)
	}
	if err := lt.StartRunning("cm-failed-tick"); err != nil {
		t.Fatal(err)
	}
	start := relengBase
	gitCommitAtRel(t, repo, "midwindow.go", "package mid", start.Add(time.Minute))
	end := start.Add(3 * time.Hour)
	if err := lt.Complete(scheduler.TickOutcome{
		TickID: "cm-failed-tick", Project: lane,
		Started: start, Finished: end,
		Status: scheduler.TickFailed, ExitCode: -1,
		Error: "gateway unreachable and exec fallback disabled: gateway POST: context deadline exceeded",
	}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	row := readTickRow(t, db, "cm-failed-tick")
	if row.commits != 1 {
		t.Errorf("commits = %d, want 1 (the window held a commit at booking — a fabricated 0 zero-accounts it)", row.commits)
	}
	if row.filesChanged != 1 {
		t.Errorf("files_changed = %d, want 1", row.filesChanged)
	}
	if row.status != "failed" {
		t.Errorf("status = %s, want failed", row.status)
	}

	// The unmeasured sentinel: the same outcome against a project with no
	// git repo writes -1/-1, never a measured-looking 0.
	noRepo := "complete-measure-norepo"
	if err := database.CreateProject(t.Context(), db, &database.Project{
		Name: noRepo, RepoURL: "https://example.com/x", Workdir: t.TempDir(),
		Weight: 10, Priority: 5, CooldownS: 0, DecayRate: 1.0,
		Model: "m", Provider: "p", Enabled: true,
	}); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	if err := lt.Enqueue(noRepo, "cm-unmeasured-tick"); err != nil {
		t.Fatal(err)
	}
	if err := lt.StartRunning("cm-unmeasured-tick"); err != nil {
		t.Fatal(err)
	}
	if err := lt.Complete(scheduler.TickOutcome{
		TickID: "cm-unmeasured-tick", Project: noRepo,
		Started: start, Finished: end,
		Status: scheduler.TickTimeout, ExitCode: -1,
	}); err != nil {
		t.Fatalf("Complete (norepo): %v", err)
	}
	row = readTickRow(t, db, "cm-unmeasured-tick")
	if row.commits != -1 || row.filesChanged != -1 {
		t.Errorf("unmeasurable window wrote (%d, %d), want the -1/-1 unmeasured sentinel", row.commits, row.filesChanged)
	}

	// Deferred is NOT a failure: its pre-counted figures are respected and
	// no window measurement runs on the deferral path.
	if err := lt.Enqueue(lane, "cm-deferred-tick"); err != nil {
		t.Fatal(err)
	}
	if err := lt.StartRunning("cm-deferred-tick"); err != nil {
		t.Fatal(err)
	}
	if err := lt.Complete(scheduler.TickOutcome{
		TickID: "cm-deferred-tick", Project: lane,
		Started: start, Finished: end,
		Status: scheduler.TickDeferred, ExitCode: -1,
		Error: "gateway unreachable and exec fallback disabled: gateway transient error: sse stream ended without a terminal event",
	}); err != nil {
		t.Fatalf("Complete (deferred): %v", err)
	}
	row = readTickRow(t, db, "cm-deferred-tick")
	if row.commits != 0 {
		t.Errorf("deferred row commits = %d, want 0 — a deferral is not a failure and must not gain a window measurement here", row.commits)
	}
	if row.status != "deferred" {
		t.Errorf("status = %s, want deferred", row.status)
	}
}

// TestRELEASE048_WindowGraceCap — the extended window is capped at one
// extra tick-wall past the booking instant: commits landing BEYOND the cap
// (sibling traffic in the same repo) must NOT be attributed to the row.
func TestRELEASE048_WindowGraceCap(t *testing.T) {
	f := newRelengFixtures(t, true)
	// Inside the cap: completed_at + 13min (wall = 3h, cap = 16:25:21).
	withinCap := f.completed.Add(13 * time.Minute)
	gitCommitAtRel(t, f.workdir, "within.go", "package within", withinCap)
	// Beyond the cap: completed_at + 3h1min — outside [spawned, 2×wall].
	beyondCap := f.completed.Add(3*time.Hour + time.Minute)
	gitCommitAtRel(t, f.workdir, "beyond.go", "package beyond", beyondCap)

	now := beyondCap.Add(time.Minute)
	rep, _ := f.lt.ReconcileLateTickCommits(scheduler.ReconcileOpts{Now: now, Apply: true})
	if rep.Annotated != 1 {
		t.Fatalf("annotated = %d, want 1 (skipped: %v)", rep.Annotated, rep.Skipped)
	}
	row := readTickRow(t, f.db, f.tickID)
	if row.commits != 1 {
		t.Errorf("commits = %d, want 1 — the commit beyond the 2×wall cap is sibling traffic, not this tick's work", row.commits)
	}
	if row.completedAt != withinCap.Format(time.RFC3339) {
		t.Errorf("completed_at = %s, want %s (the newest commit INSIDE the cap)", row.completedAt, withinCap.Format(time.RFC3339))
	}
}

// TestRELEASE048_ReconcileDryRunWritesNothing — the CLI's default mode
// measures and reports, then writes nothing at all.
func TestRELEASE048_ReconcileDryRunWritesNothing(t *testing.T) {
	f := newRelengFixtures(t, true)
	late := f.completed.Add(time.Hour)
	gitCommitAtRel(t, f.workdir, "dry.go", "package dry", late)
	now := late.Add(time.Minute)
	rep, _ := f.lt.ReconcileLateTickCommits(scheduler.ReconcileOpts{Now: now, Apply: false})
	if rep.Annotated != 1 {
		t.Fatalf("dry-run reported annotated=%d, want 1 (it must SEE the work)", rep.Annotated)
	}
	row := readTickRow(t, f.db, f.tickID)
	if row.commits != 0 || row.completedAt != f.completed.Format(time.RFC3339) || row.reconciledAt.Valid {
		t.Errorf("dry-run wrote: %+v — a dry-run must not touch the row", row)
	}
}

// TestRELEASE048_ProjectRowGuard — the project row follows the reconciled
// tick ONLY while it still points at it: a newer tick's completion instant
// on the project row is never rolled back by an older tick's reconciliation.
func TestRELEASE048_ProjectRowGuard(t *testing.T) {
	f := newRelengFixtures(t, true)
	late := f.completed.Add(time.Hour)
	gitCommitAtRel(t, f.workdir, "guarded.go", "package guarded", late)
	// A NEWER tick has since completed and stamped the project row with a
	// later instant — the reconciled tick is no longer the project's newest.
	newerInstant := late.Add(2 * time.Hour)
	if _, err := f.db.Exec(`UPDATE projects SET last_tick_completed = ? WHERE name = ?`,
		newerInstant.Format(time.RFC3339), f.project); err != nil {
		t.Fatal(err)
	}
	now := late.Add(time.Minute)
	rep, _ := f.lt.ReconcileLateTickCommits(scheduler.ReconcileOpts{Now: now, Apply: true})
	if rep.Annotated != 1 || rep.Advanced != 1 {
		t.Fatalf("tick annotation = %d/%d, want 1/1 (skipped: %v)", rep.Annotated, rep.Advanced, rep.Skipped)
	}
	var projectCompleted string
	if err := f.db.QueryRow(`SELECT last_tick_completed FROM projects WHERE name = ?`, f.project).Scan(&projectCompleted); err != nil {
		t.Fatal(err)
	}
	if projectCompleted != newerInstant.Format(time.RFC3339) {
		t.Errorf("project last_tick_completed = %s, want %s — an older tick's reconciliation must never roll the project row back", projectCompleted, newerInstant.Format(time.RFC3339))
	}
}

// TestRELEASE048_ProjectFilter — the CLI's --project surfaces exactly one
// project's rows and leaves other projects' rows unexamined.
func TestRELEASE048_ProjectFilter(t *testing.T) {
	f := newRelengFixtures(t, true)
	other := "other-lane"
	if err := database.CreateProject(t.Context(), f.db, &database.Project{
		Name: other, RepoURL: "https://example.com/o", Workdir: "",
		Weight: 10, Priority: 5, CooldownS: 0, DecayRate: 1.0,
		Model: "m", Provider: "p", Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Exec(`
		INSERT INTO ticks (id, project_name, status, outcome, spawned_at, completed_at, exit_code, commits, files_changed, created_at)
		VALUES ('other-tick', 'other-lane', 'timeout', 'timeout', ?, ?, -1, 0, 0, ?)
	`, relengBase.Format(time.RFC3339), relengBase.Add(time.Hour).Format(time.RFC3339), relengBase.Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}
	now := relengBase.Add(2 * time.Hour)
	rep, _ := f.lt.ReconcileLateTickCommits(scheduler.ReconcileOpts{Now: now, Apply: true, Project: f.project})
	if rep.Examined != 1 {
		t.Errorf("examined = %d, want 1 (only %s's rows; skipped: %v)", rep.Examined, f.project, rep.Skipped)
	}
	var reconciled sql.NullString
	if err := f.db.QueryRow(`SELECT reconciled_at FROM ticks WHERE id='other-tick'`).Scan(&reconciled); err != nil {
		t.Fatal(err)
	}
	if reconciled.Valid {
		t.Error("a filtered pass must not stamp rows of other projects")
	}
}

// TestRELEASE048_CommitAnatomySplit pins the SCHED-GAP-104 board/code split
// on a reconciled row: a commit touching ONLY board bookkeeping paths
// (.coding-hermes/) classifies as a board commit, a code commit as code —
// the releng case's duplicate-close would have shown up as board_commits.
func TestRELEASE048_CommitAnatomySplit(t *testing.T) {
	f := newRelengFixtures(t, true)
	late := f.completed.Add(time.Hour)
	// A board-only commit: every touched path is under .coding-hermes/.
	if err := os.MkdirAll(filepath.Join(f.workdir, ".coding-hermes", "board"), 0o755); err != nil {
		t.Fatalf("mkdir board: %v", err)
	}
	path := filepath.Join(f.workdir, ".coding-hermes", "board", "rows.jsonl")
	if err := os.WriteFile(path, []byte("{}\n"), 0o644); err != nil {
		t.Fatalf("write board row: %v", err)
	}
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", f.workdir}, args...)...)
		cmd.Env = append(cmd.Env,
			"GIT_AUTHOR_DATE="+late.UTC().Format(time.RFC3339),
			"GIT_COMMITTER_DATE="+late.UTC().Format(time.RFC3339))
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	run("add", ".coding-hermes/board/rows.jsonl")
	run("commit", "-m", "board: close a row")

	now := late.Add(time.Minute)
	rep, _ := f.lt.ReconcileLateTickCommits(scheduler.ReconcileOpts{Now: now, Apply: true})
	if rep.Annotated != 1 {
		t.Fatalf("annotated = %d, want 1 (skipped: %v)", rep.Annotated, rep.Skipped)
	}
	row := readTickRow(t, f.db, f.tickID)
	if row.commits != 1 {
		t.Fatalf("commits = %d, want 1", row.commits)
	}
	if row.boardCommits != 1 || row.codeCommits != 0 {
		t.Errorf("split = (%d code, %d board), want (0, 1) — a board-only commit is bookkeeping, not code", row.codeCommits, row.boardCommits)
	}
}
