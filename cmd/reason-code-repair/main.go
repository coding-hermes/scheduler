// Command reason-code-repair reclassifies the dry ticks of one historical
// window from the evidence still present in their rows, annotating the
// derivation instead of rewriting history (SCHED-GAP-16947, deliverable e).
//
// WHY. 2026-10-10..12 carried 2,701 zero-token dry ticks (1,043 / 1,034 / 624
// per day) while other lanes committed in the same window — a three-day lane
// blackout the schema booked as idle. Those rows were written before
// ticks.reason_code existed, so the way to stop 'dry' hiding the outage is a
// repair pass that reads each row's own evidence (outcome, token legs, artifact
// columns) plus the lane's productivity across the window, writes the closed
// vocabulary code, and records the derivation it used in
// ticks.reason_code_derivation.
//
// WHAT IT NEVER DOES. It does not delete rows, does not touch status, outcome,
// error, tokens, commits or any other historical column, and does not overwrite
// a code that is already set: the UPDATE's WHERE clause requires an EMPTY
// reason_code (the honest-empty value), so a second run over the same window
// writes nothing (idempotent by construction). Without --apply it is a dry run
// that prints the same report.
//
// USAGE
//
//	reason-code-repair --from 2026-10-10 --to 2026-10-13            # dry run
//	reason-code-repair --from 2026-10-10 --to 2026-10-13 --apply    # write
//	reason-code-repair --db /path/scheduler.db --apply --json
//
// --from is inclusive and --to is exclusive; both are interpreted as UTC days
// (YYYY-MM-DD) or RFC3339 instants. The defaults are exactly the measured
// blackout block.
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	_ "modernc.org/sqlite"

	"github.com/coding-hermes/scheduler/internal/database"
)

// default window: the 2026-10-10..12 lane blackout this row measured.
const (
	defaultFrom = "2026-10-10"
	defaultTo   = "2026-10-13"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("reason-code-repair", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dbPath := fs.String("db", os.ExpandEnv("$HOME/.hermes/coding-hermes/scheduler.db"), "scheduler database to repair")
	from := fs.String("from", defaultFrom, "window start, inclusive (YYYY-MM-DD or RFC3339, UTC)")
	to := fs.String("to", defaultTo, "window end, exclusive (YYYY-MM-DD or RFC3339, UTC)")
	apply := fs.Bool("apply", false, "write the codes (omit for a dry run that writes nothing)")
	asJSON := fs.Bool("json", false, "print the report as JSON")
	limit := fs.Int("verbosity-limit", 20, "maximum per-row lines to print in text mode (0 = all)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		_, _ = fmt.Fprintln(stderr, "usage: reason-code-repair [--db PATH] [--from YYYY-MM-DD] [--to YYYY-MM-DD] [--apply] [--json]")
		return 2
	}
	fromT, err := parseWindowBound(*from)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "reason-code-repair: --from: %v\n", err)
		return 2
	}
	toT, err := parseWindowBound(*to)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "reason-code-repair: --to: %v\n", err)
		return 2
	}
	if !toT.After(fromT) {
		_, _ = fmt.Fprintln(stderr, "reason-code-repair: --to must be after --from")
		return 2
	}

	ctx := context.Background()
	db, err := sql.Open("sqlite", *dbPath)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "reason-code-repair: open %s: %v\n", *dbPath, err)
		return 1
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if _, err := db.ExecContext(ctx, "PRAGMA busy_timeout=5000"); err != nil {
		_, _ = fmt.Fprintf(stderr, "reason-code-repair: configure %s: %v\n", *dbPath, err)
		return 1
	}
	// Refuse to work on a database that predates the v91 columns: the repair's
	// whole point is the annotation column, and silently writing on an older
	// schema would fail every UPDATE mid-pass.
	if ok, err := hasReasonColumns(ctx, db); err != nil {
		_, _ = fmt.Fprintf(stderr, "reason-code-repair: probe schema: %v\n", err)
		return 1
	} else if !ok {
		_, _ = fmt.Fprintf(stderr, "reason-code-repair: %s lacks ticks.reason_code / ticks.reason_code_derivation — migrate it first (cmd/migrate)\n", *dbPath)
		return 1
	}

	rep, err := database.RepairTickReasonCodes(ctx, db, database.ReasonCodeRepairOpts{
		From:  fromT,
		To:    toT,
		Apply: *apply,
	})
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "reason-code-repair: %v\n", err)
		return 1
	}

	if *asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(rep); err != nil {
			_, _ = fmt.Fprintf(stderr, "reason-code-repair: encode report: %v\n", err)
			return 1
		}
		return 0
	}
	printTextReport(stdout, rep, *limit)
	return 0
}

// parseWindowBound accepts a UTC day (YYYY-MM-DD) or a full RFC3339 instant.
func parseWindowBound(s string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.UTC(), nil
	}
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		return time.Time{}, fmt.Errorf("%q is neither YYYY-MM-DD nor RFC3339", s)
	}
	return t.UTC(), nil
}

// hasReasonColumns reports whether the v91 columns exist on this database.
func hasReasonColumns(ctx context.Context, db *sql.DB) (bool, error) {
	var n int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM pragma_table_info('ticks')
		 WHERE name IN ('reason_code', 'reason_code_derivation')`).Scan(&n); err != nil {
		return false, err
	}
	return n == 2, nil
}

// printTextReport renders the human report: the window, the skip accounting (a
// skip is never silent), the class split, then the per-row derivations.
func printTextReport(w io.Writer, rep database.ReasonCodeRepairReport, limit int) {
	mode := "DRY RUN (nothing written)"
	if rep.Apply {
		mode = "APPLIED"
	}
	_, _ = fmt.Fprintf(w, "reason-code repair %s\n", mode)
	_, _ = fmt.Fprintf(w, "  window          [%s, %s)\n", rep.WindowFrom, rep.WindowTo)
	_, _ = fmt.Fprintf(w, "  concluded ticks  %d\n", rep.Concluded)
	_, _ = fmt.Fprintf(w, "  candidates       %d (dry/no_work, no artifact, no code yet)\n", rep.Candidates)
	_, _ = fmt.Fprintf(w, "  already stamped  %d\n", rep.AlreadyStamped)
	_, _ = fmt.Fprintf(w, "  unmeasured       %d (artifact delta -1: cannot be repaired into \"none\")\n", rep.Unmeasured)
	_, _ = fmt.Fprintf(w, "  artifact present %d (nothing owed)\n", rep.Artifact)
	_, _ = fmt.Fprintf(w, "  applied          %d\n", rep.Applied)
	if len(rep.Classes) > 0 {
		_, _ = fmt.Fprintln(w, "  classes:")
		for _, code := range database.TickReasonCodes {
			if n, ok := rep.Classes[code]; ok && n > 0 {
				_, _ = fmt.Fprintf(w, "    %-22s %d\n", code, n)
			}
		}
		for code, n := range rep.Classes {
			if n > 0 && !containsString(database.TickReasonCodes, code) {
				_, _ = fmt.Fprintf(w, "    %-22s %d\n", code, n)
			}
		}
	}
	shown := 0
	for _, row := range rep.Rows {
		if limit > 0 && shown >= limit {
			_, _ = fmt.Fprintf(w, "  ... %d more row(s); use --verbosity-limit 0 for all\n", len(rep.Rows)-shown)
			break
		}
		_, _ = fmt.Fprintf(w, "  %s %-24s %-20s %s\n", row.TickID, row.Project, row.Code, row.Derivation)
		shown++
	}
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
