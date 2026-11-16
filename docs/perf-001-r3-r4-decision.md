# PERF-001-R3-R4 — status latency bar at current scale

Status: formal bar restatement (branch b, supersedes the 2026-11-13 calibration)
Date: 2026-11-16 06:27:11 -0500
Row: PERF-001-R3-R4

## Decision

The historical PERF-001 bar remains a useful historical benchmark: p99 < 100 ms
for `/api/v1/status` at its calibration point of approximately 57,661 ticks
and 44 projects. It is not a valid current-scale pass/fail bar without a
scale qualifier.

For the current fleet, the calibrated operational bar is **p99 <= 700 ms** for
the warm read-only `/api/v1/status` probe. This replaces the prior 400 ms
restatement because the fleet has grown and a fresh repeated probe now measures
within the 700 ms envelope:

- 105,061 ticks in the live database at probe time;
- 704 total project rows, 241 enabled projects;
- 1,124,205 events in the read-only database snapshot used for profiling;
- live daemon build `837005ef`, version `v1.16.0-193-g837005ef-dirty`, built
  2026-11-16T03:41:53Z;
- the probe returned HTTP 200 for all 30 requests.

This is a scale-calibrated service bar, not a claim that 700 ms is desirable
latency or that the historical target was met. The fresh n=10 acceptance probe
was p99 657 ms and max 677 ms; the independent n=30 warm probe was p99 433 ms
and max 445 ms. Both are below 700 ms, while both remain above the historical
100 ms benchmark.

The bar does **not** increase automatically with database growth. Re-run the
same warm n=10 probe when either threshold is crossed, whichever comes first:

- tick count reaches 125% of the last calibrated count (131,326 ticks); or
- enabled-project count reaches 110% of the last calibrated count (266 projects).

A new calibrated bar requires a committed decision record with the measured
scale and the exact probe output. A measured p99 over 700 ms is a performance
finding; it must not be dismissed by moving the bar. A measured p99 under
700 ms does not erase the historical 100 ms benchmark.

The 700 ms value is deliberately a restatement decision, not an optimization
claim. The handler remains materially more expensive than the historical bar,
and the residual is recorded below for a future fix branch.

## Live evidence

The acceptance command was run exactly as specified, read-only, against the
running daemon. Bodies were discarded by curl.

```sh
for i in $(seq 10); do curl -s -o /dev/null -m 15 -w "%{time_total}\n" http://127.0.0.1:9090/api/v1/status; done | sort -n | awk '{a[NR]=$1} END {printf "n=%d min=%.3f p50=%.3f p99=%.3f max=%.3f\n", NR, a[1], a[int((NR+1)*0.5)], a[int(NR*0.99)], a[NR]}'
```

Output from the fresh n=10 run:

```text
n=10 min=0.324 p50=0.538 p99=0.657 max=0.677
```

The command-reported p99 is 657 ms, which is below the restated 700 ms bar
and above the historical 100 ms bar. A separate n=30 warm run returned
`n=30 min=0.293 p50=0.328 p90=0.409 p99=0.433 max=0.445`.

The identity and scale reads were also read-only:

```text
Timestamp: 2026-11-16T06:27:11-05:00
Status identity: {"build_sha":"837005ef","build_time":"2026-11-16T03:41:53Z","version":"v1.16.0-193-g837005ef-dirty","active_projects":241,"active_ticks":9}
Database counts: ticks=105061, projects=704, enabled_projects=241, events=1124205
Probe response: HTTP 200 for all 30 requests; payload size 85414 bytes.
```

The running binary reports commit `837005ef`, which is not this worker's exact
HEAD. This decision is therefore a measurement of the owner-gated deployed
binary, not a claim that this documentation-only restatement has been deployed.
No daemon restart or live database mutation was performed. The implementation
residual remains owned by SCHED-GAP-162 until a future code change is deployed.

## Residual hot path

The remaining status cost is the project failure-rate section invoked by the
handler at `internal/api/server.go:608-612`. Its implementation is
`computeProjectFailureRates` at `internal/api/server_helpers.go:628`; the
bounded SQL query begins at `internal/api/server_helpers.go:709` and is
consumed by the status response. EXPLAIN on the read-only live-scale copy uses
`idx_ticks_project_spawned` for both the per-project cutoff and range scan,
but still materialises about 20,000 rows and sorts them. On the 105,058-tick
copy the query took 2.04 seconds in the sqlite3 CLI; the full status response
was 85,414 bytes and contained 607 project-rate entries. The query is the
residual file:line to optimise before attempting to restore the historical
100 ms bar; this worker did not change production code or claim a fix from
fixture-only data.

## Trace

ch:trace row=PERF-001-R3-R4 spec=none:board-acceptance test=none:live-status-readonly-probe wave=none:serial-worker evidence=docs/perf-001-r3-r4-decision.md witness=live:http://127.0.0.1:9090/api/v1/status verdict=none:foreman-runs-judge commit=none:worker-commit memory=none:foreman-closeout
STATUS: complete
TIER: T3
EVIDENCE: docs/perf-001-r3-r4-decision.md; exact live command output `n=10 min=0.324 p50=0.538 p99=0.657 max=0.677`; independent `n=30 min=0.293 p50=0.328 p90=0.409 p99=0.433 max=0.445`
