# Verdict: PERF-001

**Task:** Single-pass failure-rate aggregation in computeProjectFailureRates (N+1 fix)
**Evaluated:** 2026-08-17T15:16:49.390739
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
  ✓ computeProjectFailureRates issues at most 2 queries total (no per-project loop); all existing internal/api tests pass; window truncation semantics preserved; DOGFOOD-009 ghost filtering preserved; go build and go vet clean: internal/api/server_helpers.go (HEAD commit 5c40117): computeProjectFailureRates now issues exactly ONE query — a single CTE (`WITH ranked AS (SELECT ... ROW_NUMBER() OVER (PARTITION BY t.project_name ORDER BY t.spawned_at DESC, t.id DESC) ... JOIN projects p ON p.name = t.project_name WHERE t.completed_at IS NOT NULL) ... WHERE rn <= ? GROUP BY project_name`) replacing the per-project N+1 loop; no per-project query loop remains. go test ./internal/api/ -count=1 → ok (0.469s), including TestComputeProjectFailureRates_WindowTruncation (PASS: window=5 → total=5 failed=1; window=100 → total=10 failed=6, verifying truncation) and TestComputeProjectFailureRates_ExcludesHardDeletedProjects (PASS, DOGFOOD-009 ghost filtering preserved via the JOIN inside the CTE). go build ./... exit 0; go vet ./... exit 0.
PERF-001 single-pass failure-rate aggregation is correctly implemented: one CTE query (≤2 bound), all internal/api tests pass, window truncation and DOGFOOD-009 ghost filtering preserved, and go build/go vet are clean.

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
  ✓ computeProjectFailureRates issues at most 2 queries total (no per-project loop); all existing internal/api tests pass; window truncation semantics preserved; DOGFOOD-009 ghost filtering preserved; go build and go vet clean: internal/api/server_helpers.go (HEAD commit 5c40117): computeProjectFailureRates now issues exactly ONE query — a single CTE (`WITH ranked AS (SELECT ... ROW_NUMBER() OVER (PARTITION BY t.project_name ORDER BY t.spawned_at DESC, t.id DESC) ... JOIN projects p ON p.name = t.project_name WHERE t.completed_at IS NOT NULL) ... WHERE rn <= ? GROUP BY project_name`) replacing the per-project N+1 loop; no per-project query loop remains. go test ./internal/api/ -count=1 → ok (0.469s), including TestComputeProjectFailureRates_WindowTruncation (PASS: window=5 → total=5 failed=1; window=100 → total=10 failed=6, verifying truncation) and TestComputeProjectFailureRates_ExcludesHardDeletedProjects (PASS, DOGFOOD-009 ghost filtering preserved via the JOIN inside the CTE). go build ./... exit 0; go vet ./... exit 0.
PERF-001 single-pass failure-rate aggregation is correctly implemented: one CTE query (≤2 bound), all internal/api tests pass, window truncation and DOGFOOD-009 ghost filtering preserved, and go build/go vet are clean.

Overall: PASS ✓
