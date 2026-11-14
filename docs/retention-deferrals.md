# Deferral audit retention and safe inspection

`deferrals` is an audit table used by the queue dashboard and metrics surfaces. It is intentionally not part of the scheduler's admission decision. The supported cleanup path is the `retention-deferrals` command; it never deletes from the live database.

## Default and override

The command uses a conservative 14-day retention window:

```sh
./bin/retention-deferrals \
  --db "$HOME/.hermes/coding-hermes/scheduler.db" \
  --output "$HOME/.hermes/coding-hermes/scheduler-retained.db"
```

`--retention 0` means keep forever. A negative value is rejected. `--batch-size` defaults to 1000 rows and each batch is its own short transaction. `--pause` can add a delay between batches when the host is busy. The source database is copied first with `VACUUM INTO`; the delete and final `VACUUM` operate only on the new output file.

The output includes source and copy byte sizes, rows before/after DELETE and VACUUM, payload bytes, deleted rows, and a silent-regrowth check. The regrowth check reports rows older than twice the configured window. A non-zero `overdue_after` is an operator signal that retention is not being run often enough or that the configured window is not the intended policy.

## Dashboard and metrics compatibility

The command does not alter the `deferrals` columns or query shape. The dashboard's latest-deferral evidence remains available (`reason`, `detail`, and `created_at`), and the `/api/v1/metrics` `deferrals` block remains the in-memory admission-counter shape. Those counters are process-local and reset on restart; the table is durable audit history. `admission_counters` and the dashboard's latest-deferral plan/timing must therefore be read as separate sources, not combined into a fabricated total.

## Large database workaround

Do not run `DELETE`, `VACUUM`, or `.backup` against the live scheduler DB while the daemon owns it. For a large or slow database:

1. Check free space for both the source and the destination. `VACUUM INTO` needs room for a complete destination before any reclaim is visible.
2. Run `retention-deferrals` to a different filesystem or a dedicated scratch path, using a modest batch size and a pause.
3. Query the output copy with `sqlite3 -readonly` or the repository's read-only connection helpers.
4. Validate row counts and `PRAGMA quick_check` on the copy before replacing any operator artifact. The command's source is never modified, so an interrupted run can be discarded and retried to a new destination.

A copy-only run against the full production database remains a separate live measurement. Unit tests use a populated temporary SQLite copy and prove that old rows are deleted, recent rows survive, logical rows are unchanged by VACUUM, and the >2x-window regrowth guard is visible.

ch:trace row=PERF-007 spec=none:board-acceptance test=internal/database/deferral_retention_test.go::TestDeferralRetentionCopyDeletesOnlyOldRowsAndVacuumPreservesShape wave=none:serial-worker evidence=none:live-17gb-copy-not-run witness=none:full-live-copy-not-run verdict=none:foreman-runs-judge commit=none:worker-commit memory=none:foreman-closeout
STATUS: partial
TIER: T2
EVIDENCE: internal/database/deferral_retention_test.go; deterministic temporary SQLite copy test; full live 17GB copy and daemon latency arm remain unverified
