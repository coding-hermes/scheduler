# PERF-001-R3-R4 — status latency bar at current scale

Status: formal bar restatement (branch b)
Date: 2026-11-13 20:26:53 -0500
Row: PERF-001-R3-R4

## Decision

The historical PERF-001 bar remains a useful historical benchmark: p99 < 100 ms
for `/api/v1/status` at its calibration point of approximately 57,661 ticks
and 44 projects. It is not a valid current-scale pass/fail bar without a
scale qualifier.

For the current fleet, the operational bar is **p99 <= 400 ms** for the warm
read-only `/api/v1/status` probe, calibrated at the measurement below:

- 104,233 ticks in the live database at probe time;
- 904 total project rows, 204 enabled projects;
- 1,086,697 events;
- daemon build `9b1fdd22`, `schedulerd v1.16.0-58-g9b1fdd22-dirty`, built
  2026-11-11T22:20:06Z;
- the probe returned HTTP 200 for all ten requests.

This is a bar-integrity decision, not a claim that 400 ms is user-visible
slowness. The current result is below the restated bar. The 100 ms historical
bar is retained rather than silently re-labelled as a current-scale pass.

The bar does **not** increase automatically with database growth. Re-run the
same warm n=10 probe when either threshold is crossed, whichever comes first:

- tick count reaches 125% of the last calibrated count; or
- enabled-project count reaches 110% of the last calibrated count.

A new calibrated bar requires a committed decision record with the measured
scale and the exact probe output. A measured p99 over 400 ms is a performance
finding; it must not be dismissed by moving the bar. A measured p99 under
400 ms does not erase the historical 100 ms benchmark.

## Live evidence

The acceptance command was run exactly as specified, read-only, against the
running daemon. Bodies were discarded by curl.

```sh
for i in $(seq 10); do curl -s -o /dev/null -m 15 -w "%{time_total}\n" http://127.0.0.1:9090/api/v1/status; done | sort -n | awk '{a[NR]=$1} END {printf "n=%d min=%.3f p50=%.3f p99=%.3f max=%.3f\n", NR, a[1], a[int((NR+1)*0.5)], a[int(NR*0.99)], a[NR]}'
```

Output:

```text
n=10 min=0.244 p50=0.261 p99=0.268 max=0.270
```

The command-reported p99 is 268 ms, which is below the restated 400 ms bar
and above the historical 100 ms bar.

The identity and scale reads were also read-only:

```text
Timestamp: 2026-11-13T20:26:53-05:00
Status identity: {"build_sha":"9b1fdd22","build_time":"2026-11-11T22:20:06Z","version":"v1.16.0-58-g9b1fdd22-dirty","active_projects":204,"active_ticks":4}
Process: pid 276029, started Wed Nov 11 17:20:09 2026, RSS 551872 KiB
Executable: /home/kara/coding-hermes-scheduler/coding-herms-scheduler/bin/schedulerd
Database counts: ticks=104233, projects=904, enabled_projects=204, events=1086697
Load at probe: 3.17 3.96 5.77 3/5783 4018987
```

The running binary reports commit `9b1fdd22`. The worker checkout is at
`d6c9f4530f02261aa7b4e1d0651a0ad1bdf55071`; `git rev-list -n 1 HEAD --
internal/scheduler` is `233fcf28bd6de80fa2178d0e8a5bd42ab3649175`. Therefore the
live daemon is an ancestor of this checkout, not the exact worker HEAD. The
freshness check against the daemon's own commit returned `FRESH` (exit 0); a
check against worker `HEAD` correctly returned `STALE` (exit 1). The result is
reported against the live daemon identity, not claimed as an exact-HEAD
measurement.

## Residual hot path

The remaining status cost is the project failure-rate section invoked by the
handler at `internal/api/server.go:608-612`. Its implementation is
`computeProjectFailureRates` at `internal/api/server_helpers.go:628`; the
bounded SQL query begins at `internal/api/server_helpers.go:709` and is
consumed by the status response. This is the residual file:line to profile or
optimize before changing the 400 ms bar. The prior profile identified this
function as the largest status CPU component; this decision record does not
claim a new query improvement from fixture-only data.

## Trace

ch:trace row=PERF-001-R3-R4 spec=none:board-acceptance test=none:live-status-readonly-probe wave=none:serial-worker evidence=docs/perf-001-r3-r4-decision.md witness=live:http://127.0.0.1:9090/api/v1/status verdict=none:foreman-runs-judge commit=none:worker-commit memory=none:foreman-closeout
STATUS: complete
TIER: T3
EVIDENCE: docs/perf-001-r3-r4-decision.md; exact live command output `n=10 min=0.244 p50=0.261 p99=0.268 max=0.270`
