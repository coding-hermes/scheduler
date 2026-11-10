SCHED-FAILCNT-001 evidence

Status: implementation evidence only; live scheduler state was not accessed.

Source write-path census (base 1b6209fa83a9c29313d840cf8011219e9dd9739b)

- spawn.go: noteSpawnFailure writes `consecutive_failures + 1`.
- spawn.go: noteSpawnFailureClassed is the classified wrapper and was the intended transport gate.
- spawn.go: gatewayKeyRejected calls noteSpawnFailure directly.
- spawn.go: the gateway response-failure arm calls noteSpawnFailure directly.
- spawn.go: the gateway-turn-stalled arm calls noteSpawnFailure directly.
- spawn.go: exec pipe, stderr pipe, and process-start errors call noteSpawnFailure directly.
- spawn.go: successful spawn writes `consecutive_failures = 0`.
- spawn.go: successful gateway response writes `consecutive_failures = 0` unless it is an instant turn.
- lifecycle.go: TickCompleted writes `consecutive_failures = 0`.
- lifecycle.go: non-transport TickTimeout writes `consecutive_failures + 1`.

Measured defect and fix

The raw HTTP 429 error text `gateway POST: HTTP 429: {"error":"Too many concurrent runs (max 44)"}` did not match the shared harness marker list. Consequently, the drop path's call to noteSpawnFailureClassed reached noteSpawnFailure and incremented the project counter. The fix adds the exact `too many concurrent runs` marker to the shared classifier. The successful completion reset already existed in lifecycle.Complete and is covered by the new three-transport-ticks-then-committed-tick test.

Test evidence

- RED pre-fix: none:pre-change execution was not performed in this worktree; no pre-fix red is claimed.
- GREEN: `go test ./internal/scheduler -run 'TestSCHEDFAILCNT001|TestSCHEDGAP137A|TestSCHEDGAP16940' -count=1 -v` — PASS; focused package result .
- Repair tests: `python3 -m unittest ops/test_repair_consecutive_failures.py` — PASS; focused package result .
- Live per-lane before/after table: none:live-scheduler-state-unavailable.
- Instrumented live run: none:live-scheduler-state-unavailable.

Repair utility

`ops/repair-consecutive-failures.py` is read-only by default. `--apply` lowers a counter only to the count of failed/timeout ticks in the bounded window whose `failure_reason` is not `gateway_transport` or `gateway_drain`; it never raises counters. The utility was not run against live scheduler state.

ch:trace row=SCHED-FAILCNT-001 spec=specs/S05-spawn-engine-lifecycle.md#4.4-completion-and-outcome-query wave=none:serial-worker test=internal/scheduler/sched_failcnt_001_test.go::TestSCHEDFAILCNT001_Gateway429DoesNotMoveCounter doc=docs/evidence/SCHED-FAILCNT-001.md evidence=docs/evidence/SCHED-FAILCNT-001.md witness=none:live-scheduler-state-unavailable verdict=none:open-finding commit=none:open memory=none:unverified
