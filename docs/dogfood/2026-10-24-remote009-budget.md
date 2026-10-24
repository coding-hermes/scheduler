# REMOTE009 fresh-system round-trip budget

Task: SCHED-GAP-16927

Diagnosis

The requester timeout is controlled by `QueryEnvelope.BudgetMS` in
`internal/bus/query.go:265-266`; the documented default is
`queryDefaultBudgetMS = 10000` at `internal/bus/query.go:59-64`. The socket
read deadline is armed from the same budget at `internal/bus/query.go:277-280`.

Before this change, `TestREMOTE009_ResponderFullRoundTrip` overrode that
production default with `BudgetMS: 2000` at
`internal/bus/query_responder_test.go:56-57`. The fresh-system evidence failed
at 2.01s with `bus: query reply timeout`, which is consistent with that
shorter test-only deadline expiring while a loaded two-vCPU system schedules
the responder and relay goroutines. No responder-specific timeout constant was
found: the expired budget was the requester's explicit 2,000ms budget.

The successful round-trip test now pins the documented 10,000ms default. This
is a test-harness correction, not a product timeout change. The requester still
accepts explicit budgets unchanged, and `TestREMOTE009_MissingReplyFailsFast`
keeps a 100ms explicit budget and asserts the named timeout returns within 1s.

Evidence

- Fresh-system capture: `/tmp/df-ops-1023/bunker-logs/native.log:458-459`
  reports `TestREMOTE009_ResponderFullRoundTrip` failing after `2.01s` with
  `bus: query reply timeout`.
- Baseline focused run before the change:
  `go test ./internal/bus -run TestREMOTE009_ResponderFullRoundTrip -count=5 -v`
  passed all 5 runs in `0.021s`.
- Constrained idle run:
  `GOMAXPROCS=2 taskset -c 0,1 go test ./internal/bus -run TestREMOTE009_ResponderFullRoundTrip -count=5 -v`
  passed all 5 runs in `0.020s`.
- Loaded constrained run: two pinned `yes` CPU co-tenants, load average
  `13.42 11.39 8.56`, `GOMAXPROCS=2`, `taskset -c 0,1`, and 20 repetitions all
  passed in `0.079s` (`go test` wall; command elapsed `1.00s`). The host had 16
  CPUs and 61,080 MiB total memory; no cgroup `cpu.max` or `memory.max` value
  was exposed in this session.

Conclusion

The reported failure was not reproducible on this host, even with two-CPU
pinning and saturated co-tenant CPU load. The captured fresh-system failure is
best explained by a test-only 2s budget that was shorter than the production
default, not by a responder reply-path defect. The missing-reply fast-failure
regression remains explicit and deterministic.
