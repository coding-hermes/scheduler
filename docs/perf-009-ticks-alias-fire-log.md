# PERF-009 — fire log (`/api/v1/ticks` ORDER BY binds the COALESCE alias)

Row: `PERF-009` (pending, P2). Hot path: `internal/api/server_helpers.go:880` (`listTicks`),
statement `:881`, order clause `:891`.

This file is the fire log for that row's tick id `coding-hermes-scheduler-perf-2026-11-13-04-14-58`.
Lines are APPENDED, never rewritten.

2026-11-13a — fire 1 (recorded in the PERF-009 row itself; this log file did not exist yet).
  Filed the row. Measured HEAD `ee05ad2c`, loadavg 3.64/5.28/6.90, 16 cores.
  Live `GET /api/v1/ticks?limit=50` median 209.4 ms (n=12, 108,855 B). Paired fixture A/B
  (103,980 ticks / 904 projects / 13 indexes, ANALYZEd, 45 interleaved rounds):
  shipped 204.51 ms -> 0.44 ms with `idx_ticks_spawned` + order term on the table column = 472.9x;
  index inert under the shipped statement (204.51 -> 206.21 ms, ratio 0.99).

2026-11-13b — fire 2 (this fire; duplicate dispatch of the same tick id). Records-only plus the
  sanctioned high-load remeasurement; no product code changed, nothing deployed.
  Landing at the hash this fire saw: HEAD `45a1b4cf` == `origin/main` (fresh fetch), PERF-009 once
  in the committed tree with one `task_created` event, 0 duplicate ids, and
  `git diff --stat ee05ad2c HEAD -- internal cmd plugin scripts ops Makefile go.mod go.sum` empty.
  Same binary `./fixprobe`, same 3 fixtures, 45 interleaved rounds, loadavg 11.77/14.97/12.84
  (fire 1 window: 3.64/5.28/6.90 — this host is 3.2-3.9x more loaded):
    SHIPPED_STMT__no_index          233.91 ms   (fire 1: 204.51)
    SHIPPED_STMT__idx(spawned_at)   233.60 ms   (fire 1: 206.21, inert ratio 1.00)
    FIXED_ORDERBY__no_index         252.25 ms   (fire 1: 223.01)
    FIXED_ORDERBY__idx(spawned_at)    0.50 ms   (fire 1: 0.44)
    SHIPPED_STMT__idx(COALESCE)       0.42 ms   (fire 1: 0.38)
  PAIRED RATIO 470.0x (fire 1: 472.9x) — the win is not a quiet-host artifact.
  Live, same daemon, loadavg 12.09, n=12: median 234.0 ms, min 214.4, max 373.6, 50 rows / 99,936 B.
  Absolute seconds are NOT comparable across windows (control moved 204.51 -> 233.91 ms with load);
  quote the ratio. Live-after-fix stays UNVERIFIED (no migration deployed).
  Artifacts: `/home/kara/.hermes/cache/scratch/perf-chs-20261113-041458-f1/` (fixprobe + fixtures,
  fire2-fixprobe.out, fire2-live.out, load-before.txt, load-after.txt),
  note guard trace in `perf-chs-20261113-041458-f1b/note-guard.out`.
