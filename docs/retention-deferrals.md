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

## PERF-007 live copy evidence (2026-11-16)

This measurement used the live daemon's open database descriptor `/proc/1669553/fd/7` as the read-only source, because the daemon-owned path was transiently being replaced during the run. The output copy was `/home/kara/.hermes/cache/scratch/perf007-copy.db`; it was deleted after verification. The source was never modified.

Command:

```sh
./bin/retention-deferrals \\
  --db /proc/1669553/fd/7 \\
  --output /home/kara/.hermes/cache/scratch/perf007-copy.db \\
  --retention 336h --batch-size 10000000 --pause 0
```

Exact report:

```text
source=/proc/1669553/fd/7 source_bytes=17175048192
copy=/home/kara/.hermes/cache/scratch/perf007-copy.db copy_bytes_after_vacuum=3642011648
retention=336h0m0s batch_size=10000000 deleted_rows=9425290
deferrals rows_before=11860687 rows_after_delete=2435397 rows_after_vacuum=2435397 payload_before=13312890095 payload_after_delete=1614031516 payload_after_vacuum=1614031516
silent_regrowth overdue_before=347766 overdue_after=0 threshold=2x-window
dashboard_last_deferral project=boardctl-foreman reason=budget created_at=2026-11-16T12:14:10Z
metrics_shapes=unchanged; admission_counters=daemon-memory-only
```

The dashboard latest-deferral query plan on the pruned copy was:

```text
QUERY PLAN
`--SEARCH d USING INDEX idx_deferrals_project_created (project_name=?)
```

Five timed executions each returned `budget|2026-11-16T12:14:10Z` with `elapsed=0.00 sec`. During the copy/prune run, live probes stayed HTTP 200. Representative probes were `health=200 0.344763s` / `metrics=200 0.135233s`, `health=200 2.331725s` / `metrics=200 0.219221s`, and `health=200 0.376671s` / `metrics=200 0.143155s`.

The live `/api/v1/metrics` shape after the run was:

```text
deferrals_keys=["admitted_by_namespace","available","by_reason","passes"]
by_reason_keys=["board_unchanged","board_unowned","budget","cap","cooldown","dependency_blocked","failed_cooldown","load_gate","no_work","ok","sibling_held","stale_premise","tasks_deferred","tasks_no_actionable","tasks_no_work"]
admitted_keys=["code-factory","coding-hermes","data-cleanup","docs","dogfood","duckbrain-sync","ops","perf","pm","qa","releases","review"]
passes_type="number"
```

The command itself does not contact the daemon and therefore does not verify that metrics shape. Its output now says `metrics_shapes=unchecked`; the live metrics shape above is a separately captured operator measurement.

ch:trace row=PERF-007 spec=none:board-acceptance test=internal/database/deferral_retention_test.go::TestDeferralRetentionCopyDeletesOnlyOldRowsAndVacuumPreservesShape wave=none:serial-worker evidence=docs/retention-deferrals.md::PERF-007-live-copy-2026-11-16 witness=/proc/1669553/fd/7 verdict=none:foreman-runs-judge commit=none:worker-commit memory=none:foreman-closeout
STATUS: partial
TIER: T2
EVIDENCE: committed live copy report, dashboard plan/timing, daemon probes, and metrics shape; full suite has pre-existing scheduler failures
