// Command reconcile-ticks reconciles failed/timeout scheduler ticks whose
// windows landed commits AFTER the row was booked terminal, and stamps the
// work back onto the row (RELEASE-048).
//
// THE CASE IT EXISTS FOR. A gateway turn torn down without a terminal event
// books the tick failed with commits=0 (the drop path has no SpawnedTick and
// never measured), while the gateway-side session — a server-side process the
// scheduler never signals on any terminal path — keeps working and commits
// land hours later. Measured on coding-hermes-scheduler-releng 2026-11-07:
// tick coding-hermes-scheduler-releng-2026-11-07-15-25-21 stamped failed
// 13:25:22, its session's release commits landed 15:38:11-15:42:38, model
// usage continuing to 16:29:23. The daemon now runs the same pass
// periodically (internal/scheduler/tick_process.go); this CLI is the
// operator surface for a deliberate one-shot run against a named DB — the
// same code path, never an ad-hoc UPDATE.
//
// WHAT IT WRITES (with --apply only; the default run is a dry-run):
//
//	ticks.commits / files_changed   — the extended window's measured figures
//	ticks.code_commits / board_commits — the SCHED-GAP-104 split
//	ticks.completed_at              — advanced to the newest commit in the
//	                                  window (never backward; status NEVER
//	                                  touched)
//	projects.last_tick_completed    — follows while the project row still
//	                                  points at the reconciled tick
//	ticks.reconciled_at             — the pass watermark (idempotence)
//
// An unmeasurable project (no git workdir reachable from workdir/repo_url)
// is stamped reconciled_at once with commits left 0 — the honest figure,
// never a fabricated count and never the -1 sentinel (which is reserved for
// the terminal-write path's unmeasured marker).
//
// Usage:
//
//	# dry-run (default): measures and reports, writes nothing
//	reconcile-ticks --project coding-hermes-scheduler-releng
//
//	# apply: stamp the annotations
//	reconcile-ticks --project coding-hermes-scheduler-releng --apply
package main

import (
	"database/sql"
	"flag"
	"fmt"
	"io"
	"os"

	_ "modernc.org/sqlite" // pure-Go SQLite driver — the scheduler's driver

	"github.com/coding-hermes/scheduler/internal/clock"
	"github.com/coding-hermes/scheduler/internal/scheduler"
)

// defaultDBPath mirrors cmd/schedulerd/main.go's --db default.
func defaultDBPath() string {
	return os.ExpandEnv("$HOME/.hermes/coding-hermes/scheduler.db")
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run is the whole CLI: flag parsing, one pass, report. It returns a process
// exit code and never calls os.Exit itself so tests can drive it.
func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("reconcile-ticks", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dbPath := fs.String("db", defaultDBPath(), "SQLite scheduler database path")
	project := fs.String("project", "", "Restrict the pass to one project's ticks (empty = every project)")
	lookback := fs.Duration("lookback", scheduler.ReconcileLookbackDefault, "How far back to scan failed/timeout rows")
	recheck := fs.Duration("recheck-after", scheduler.ReconcileRecheckDefault, "Skip rows whose reconciled_at watermark is newer than this")
	applyFlag := fs.Bool("apply", false, "Write the annotations (explicit opt-in; without it the run is a dry-run)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		_, _ = fmt.Fprintf(stderr, "REASON=BAD_USAGE: unexpected argument %q\n", fs.Arg(0))
		return 2
	}

	db, err := sql.Open("sqlite", *dbPath)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "REASON=DB_OPEN_FAILED: %v\n", err)
		return 1
	}
	defer db.Close()
	// The daemon owns this file and holds it in WAL mode; a second writer is
	// legal but must not fail on a transient lock — wait, then report.
	if _, err := db.Exec(`PRAGMA busy_timeout = 10000`); err != nil {
		_, _ = fmt.Fprintf(stderr, "REASON=DB_PRAGMA_FAILED: %v\n", err)
		return 1
	}

	lt := scheduler.NewLifecycleTracker(db)
	rep, ran := lt.ReconcileLateTickCommits(scheduler.ReconcileOpts{
		// clock.Real() is the seam, not a direct time.Now: the repo-wide
		// clock guard (internal/clock/stdlib_guard_test.go) forbids stdlib
		// clock reads outside the clock package, cmd/ included.
		Now:          clock.Real().Now(),
		Lookback:     *lookback,
		RecheckAfter: *recheck,
		Project:      *project,
		Apply:        *applyFlag,
	})
	if !ran {
		_, _ = fmt.Fprintln(stdout, "pass skipped: cadence gate (not reachable from the CLI — MinCadence is unset here)")
		return 0
	}

	mode := "dry-run"
	if *applyFlag {
		mode = "apply"
	}
	scope := "all projects"
	if *project != "" {
		scope = "project " + *project
	}
	_, _ = fmt.Fprintf(stdout, "reconcile-ticks %s (%s, lookback %s):\n", mode, scope, *lookback)
	_, _ = fmt.Fprintf(stdout, "  examined=%d annotated=%d advanced=%d still_zero=%d unmeasurable=%d skipped=%d\n",
		rep.Examined, rep.Annotated, rep.Advanced, rep.StillZero, rep.Unmeasurable, len(rep.Skipped))
	for _, s := range rep.Skipped {
		_, _ = fmt.Fprintf(stdout, "  - %s\n", s)
	}
	if !*applyFlag && rep.Annotated > 0 {
		_, _ = fmt.Fprintf(stdout, "\nre-run with --apply to write these annotations.\n")
	}
	return 0
}
