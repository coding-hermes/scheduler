# Verdict: PERF-001

**Task:** S10 violation: /api/v1/status p99 < 100ms (N+1 misattribution corrected — real costs were events-table scan + windowed CTE)
**Evaluated:** 2026-08-17T15:28:12.305818
**Result:** ✓ PASS

## Pipeline Stages

- ✓ **tier1**
  -   ✓ guard: Tier 1 Guards: PASS  (test mode: full)
  ✓ secrets — clean
  ✓ go_build — ok
  ✓ go_lint — ok
  ✓ go
- ✓ **tier2**
  - COMPLETE
  ✓ /api/v1/status p99 < 100ms over 10 live samples at production row count (57k+ ticks, 254k events): Measured p99 = 53,921us (54ms) over 10 live samples with a loop attached (production path) at 44 projects / 57,661 ticks / 254,000 events (temp perf test in internal/api, since removed); max sample 53.9ms, all samples < 100ms. internal/api/server.go status handler.
  ✓ computeProjectFailureRates has NO window-function CTE (indexed per-project loop, ~5ms total measured through modernc): internal/api/server_helpers.go computeProjectFailureRates uses DISTINCT join + per-project `SELECT status FROM ticks WHERE project_name=? AND completed_at IS NOT NULL ORDER BY spawned_at DESC LIMIT ?`; grep for ROW_NUMBER|OVER(|PARTITION BY = 0 matches repo-wide. EXPLAIN QUERY PLAN confirms SEARCH ticks USING INDEX idx_ticks_project_spawned; measured 44 per-project queries = 956us total.
  ✓ last_evaluation served from in-memory loop state when loop attached (no events-table scan in status handler): internal/api/server.go:147-152: `if s.loop != nil { if t := s.loop.LastEvalTime(); !t.IsZero() { lastEval = t.UTC().Format(time.RFC3339) } } else { lastEval = getLastEvalTime(ctx, s.db) }` — Loop.LastEvalTime (internal/scheduler/loop.go:360) reads in-memory l.lastEval (set in evaluate() at tick_process.go:22); events-table scan only in the no-loop fallback.
  ✓ all internal/api tests pass including WindowTruncation and ExcludesHardDeletedProjects (DOGFOOD-009): go test ./internal/api/ -count=1 -v → ok 0.327s; TestComputeProjectFailureRates_WindowTruncation PASS and TestComputeProjectFailureRates_ExcludesHardDeletedProjects PASS (DOGFOOD-009 ghost filtering preserved via projects JOIN).
  ✓ go build and go vet clean: go build ./... exit 0; go vet ./... exit 0 (both run against HEAD 28f745e).
All five PERF-001 criteria verified: status p99 measured 54ms at production row count (57,661 ticks / 254,000 events), failure-rate aggregation uses an indexed per-project loop with no window-function CTE, last_evaluation served from in-memory loop state, all internal/api tests (incl. WindowTruncation and ExcludesHardDeletedProjects) pass, and go build/go vet are clean.

## Summary

Judge Result: PERF-001

Stage tier1: PASS
    ✓ guard: Tier 1 Guards: PASS  (test mode: full)
  ✓ secrets — clean
  ✓ go_build — ok
  ✓ go_lint — ok
  ✓ go

Stage tier2: PASS
  COMPLETE
  ✓ /api/v1/status p99 < 100ms over 10 live samples at production row count (57k+ ticks, 254k events): Measured p99 = 53,921us (54ms) over 10 live samples with a loop attached (production path) at 44 projects / 57,661 ticks / 254,000 events (temp perf test in internal/api, since removed); max sample 53.9ms, all samples < 100ms. internal/api/server.go status handler.
  ✓ computeProjectFailureRates has NO window-function CTE (indexed per-project loop, ~5ms total measured through modernc): internal/api/server_helpers.go computeProjectFailureRates uses DISTINCT join + per-project `SELECT status FROM ticks WHERE project_name=? AND completed_at IS NOT NULL ORDER BY spawned_at DESC LIMIT ?`; grep for ROW_NUMBER|OVER(|PARTITION BY = 0 matches repo-wide. EXPLAIN QUERY PLAN confirms SEARCH ticks USING INDEX idx_ticks_project_spawned; measured 44 per-project queries = 956us total.
  ✓ last_evaluation served from in-memory loop state when loop attached (no events-table scan in status handler): internal/api/server.go:147-152: `if s.loop != nil { if t := s.loop.LastEvalTime(); !t.IsZero() { lastEval = t.UTC().Format(time.RFC3339) } } else { lastEval = getLastEvalTime(ctx, s.db) }` — Loop.LastEvalTime (internal/scheduler/loop.go:360) reads in-memory l.lastEval (set in evaluate() at tick_process.go:22); events-table scan only in the no-loop fallback.
  ✓ all internal/api tests pass including WindowTruncation and ExcludesHardDeletedProjects (DOGFOOD-009): go test ./internal/api/ -count=1 -v → ok 0.327s; TestComputeProjectFailureRates_WindowTruncation PASS and TestComputeProjectFailureRates_ExcludesHardDeletedProjects PASS (DOGFOOD-009 ghost filtering preserved via projects JOIN).
  ✓ go build and go vet clean: go build ./... exit 0; go vet ./... exit 0 (both run against HEAD 28f745e).
All five PERF-001 criteria verified: status p99 measured 54ms at production row count (57,661 ticks / 254,000 events), failure-rate aggregation uses an indexed per-project loop with no window-function CTE, last_evaluation served from in-memory loop state, all internal/api tests (incl. WindowTruncation and ExcludesHardDeletedProjects) pass, and go build/go vet are clean.

Overall: PASS ✓
