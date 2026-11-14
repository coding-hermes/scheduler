// Command retention-deferrals makes a safe, copy-only compacted scheduler DB.
// The live scheduler database is opened read-only; deletes and VACUUM run only
// on the newly-created copy.
package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"io"
	"os"

	_ "modernc.org/sqlite"

	"github.com/coding-hermes/scheduler/internal/clock"
	"github.com/coding-hermes/scheduler/internal/database"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("retention-deferrals", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dbPath := fs.String("db", os.ExpandEnv("$HOME/.hermes/coding-hermes/scheduler.db"), "live scheduler database (read-only source)")
	output := fs.String("output", "", "new compacted database path (must not already exist)")
	retention := fs.Duration("retention", database.DefaultDeferralRetention, "deferral retention window; 0 = keep forever")
	batchSize := fs.Int("batch-size", database.DefaultDeferralBatch(), "maximum rows deleted per short transaction")
	pause := fs.Duration("pause", 0, "pause between delete batches; keeps the daemon responsive")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 || *output == "" {
		_, _ = fmt.Fprintln(stderr, "usage: retention-deferrals --db PATH --output NEW_PATH [--retention 336h | 0]")
		return 2
	}
	if *pause < 0 {
		_, _ = fmt.Fprintln(stderr, "retention-deferrals: --pause must be >= 0")
		return 2
	}
	if err := database.ValidateDeferralRetention(*retention, *batchSize); err != nil {
		_, _ = fmt.Fprintf(stderr, "retention-deferrals: %v\n", err)
		return 2
	}

	ctx := context.Background()
	if err := database.CopyDatabase(ctx, *dbPath, *output); err != nil {
		_, _ = fmt.Fprintf(stderr, "retention-deferrals: copy source: %v\n", err)
		return 1
	}
	copyDB, err := sql.Open("sqlite", *output)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "retention-deferrals: open copy: %v\n", err)
		return 1
	}
	defer copyDB.Close()
	copyDB.SetMaxOpenConns(1)
	if _, err := copyDB.Exec("PRAGMA busy_timeout=5000"); err != nil {
		_, _ = fmt.Fprintf(stderr, "retention-deferrals: configure copy: %v\n", err)
		return 1
	}

	now := clock.Real().Now()
	before, err := database.DeferralRetentionSnapshot(ctx, copyDB, now, *retention)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "retention-deferrals: snapshot before: %v\n", err)
		return 1
	}
	deleted := int64(0)
	if *retention > 0 {
		cutoff := now.Add(-*retention)
		for {
			n, pruneErr := database.PruneDeferrals(ctx, copyDB, cutoff, *batchSize)
			if pruneErr != nil {
				_, _ = fmt.Fprintf(stderr, "retention-deferrals: delete batch: %v\n", pruneErr)
				return 1
			}
			deleted += n
			if n == 0 {
				break
			}
			if *pause > 0 {
				clock.Real().Sleep(*pause)
			}
		}
	}
	afterDelete, err := database.DeferralRetentionSnapshot(ctx, copyDB, now, *retention)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "retention-deferrals: snapshot after delete: %v\n", err)
		return 1
	}
	if _, err := copyDB.ExecContext(ctx, "VACUUM"); err != nil {
		_, _ = fmt.Fprintf(stderr, "retention-deferrals: vacuum copy: %v\n", err)
		return 1
	}
	afterVacuum, err := database.DeferralRetentionSnapshot(ctx, copyDB, now, *retention)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "retention-deferrals: snapshot after vacuum: %v\n", err)
		return 1
	}
	sourceInfo, sourceErr := os.Stat(*dbPath)
	copyInfo, copyErr := os.Stat(*output)
	if sourceErr != nil || copyErr != nil {
		_, _ = fmt.Fprintf(stderr, "retention-deferrals: stat source/copy: %v / %v\n", sourceErr, copyErr)
		return 1
	}
	mode := retention.String()
	if *retention == 0 {
		mode = "keep forever"
	}
	_, _ = fmt.Fprintf(stdout, "source=%s source_bytes=%d\n", *dbPath, sourceInfo.Size())
	_, _ = fmt.Fprintf(stdout, "copy=%s copy_bytes_after_vacuum=%d\n", *output, copyInfo.Size())
	_, _ = fmt.Fprintf(stdout, "retention=%s batch_size=%d deleted_rows=%d\n", mode, *batchSize, deleted)
	_, _ = fmt.Fprintf(stdout, "deferrals rows_before=%d rows_after_delete=%d rows_after_vacuum=%d payload_before=%d payload_after_delete=%d payload_after_vacuum=%d\n",
		before.Rows, afterDelete.Rows, afterVacuum.Rows, before.PayloadBytes, afterDelete.PayloadBytes, afterVacuum.PayloadBytes)
	_, _ = fmt.Fprintf(stdout, "silent_regrowth overdue_before=%d overdue_after=%d threshold=2x-window\n", before.OverdueRows, afterVacuum.OverdueRows)
	var latestProject, latestReason, latestAt string
	if err := copyDB.QueryRowContext(ctx, `SELECT project_name, reason, created_at FROM deferrals ORDER BY created_at DESC, id DESC LIMIT 1`).Scan(&latestProject, &latestReason, &latestAt); err == nil {
		_, _ = fmt.Fprintf(stdout, "dashboard_last_deferral project=%s reason=%s created_at=%s\n", latestProject, latestReason, latestAt)
	} else {
		_, _ = fmt.Fprintf(stdout, "dashboard_last_deferral unavailable=%v\n", err)
	}
	_, _ = fmt.Fprintln(stdout, "metrics_shapes=unchanged; admission_counters=daemon-memory-only")
	return 0
}
