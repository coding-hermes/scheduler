# Verdict: SCHED-GAP-135-2026-09-17-04

**Task:** SCHED-GAP-135 — phantom running ticks + eval-stall blind
**Evaluated:** 2026-09-17T05:05:25.016599
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
  ✓ When the gateway transport dial is refused, the spawn retry-exhaust path must mark the tick failed (write to DB) and call SlotPool.Release, so that the row does not stay status='running' with pid=0 forever. The eval-stall detector at loop.go:784 must NOT return early on phantom running>0; the new test must prove the row gets marked failed and the slot gets released, and prove checkEvalStall is no longer silenced by a stuck-running row. Verify with go test -count=1 -short -p 1 ./internal/scheduler/ and a RED mutation: force the failure write to be skipped, watch the new test fail, then restore.: All elements verified with live command output. (1) Failure write + Release: internal/scheduler/slot_pool.go:382-397 — on Spawn error the path calls p.lifecycle.Complete(TickOutcome{Status: TickFailed, Error: err.Error()}) then returns; the spawn goroutine's deferred clearReserve/Release frees the slot. (2) loop.go:827 — `if running > 0 && !l.allRunningRowsArePhantoms() { return }` replaces the blanket early return; helper allRunningRowsArePhantoms() at loop.go:747 uses gatewayZombieMaxAge (15m, tick_process.go:259) with julianday() comparison, fails safe on zero rows/query error. (3) RED MUTATION PROVEN: the working tree contained `if true { return } // RED MUTATION: skip failure write` at slot_pool.go:382; with it applied, `go test -count=1 -short -p 1 -run SCHEDGAP135 -v ./internal/scheduler/` exited 1 with `--- FAIL: TestSCHEDGAP135_RetryExhaustMarksFailedAndReleasesSlot (3.58s)`, 4x `status="running", want "failed"` and `running tick rows = 4, want 0`. (4) RESTORED: `git checkout -- internal/scheduler/slot_pool.go`; `grep -rn 'RED MUTATION' internal/` => CLEAN; committed HEAD version retains the failure write. (5) GREEN after restore: same command => `--- PASS: TestSCHEDGAP135_RetryExhaustMarksFailedAndReleasesSlot (3.59s)`, `--- PASS: TestSCHEDGAP135_EvalStallNotBlindedByPhantomRunning (0.06s)` with log `EVAL-STALL: last eval 10m0s ago with 4 running ticks — forced re-evaluation`, `--- PASS: TestSCHEDGAP135_EvalStallLiveRunningStillSuppresses (0.08s)`, `ok github.com/coding-hermes/scheduler/internal/scheduler 3.750s`. (6) Full suite: `go test -count=1 -short -p 1 ./internal/scheduler/` => exit 0, `ok github.com/coding-hermes/scheduler/internal/scheduler 65.154s`, zero FAIL lines. Test file schedgap135_phantom_test.go asserts tickStatusOf=="failed" for all 4 ticks, RunningCount()==0, pool.Running()==0, final GATEWAY RETRY 3/3 line per tick, plus assertForcedEval/assertStallEventCount(db,1) for the phantom-stall case.
SCHED-GAP-135 is fully implemented and verified: the retry-exhaust path marks ticks failed and releases slots, checkEvalStall no longer returns early on phantom running>0, the new tests are RED-proven against a skipped failure write and GREEN after restore, and the full scheduler package suite passes (ok 65.154s).

## Summary

Judge Result: SCHED-GAP-135-2026-09-17-04

Stage tier1: PASS
    ✓ guard: Tier 1 Guards: PASS  (test mode: full)
  ✓ secrets — clean
  ✓ go_build — ok
  ✓ go_lint — ok
  ✓ go

Stage tier2: PASS
  COMPLETE
  ✓ When the gateway transport dial is refused, the spawn retry-exhaust path must mark the tick failed (write to DB) and call SlotPool.Release, so that the row does not stay status='running' with pid=0 forever. The eval-stall detector at loop.go:784 must NOT return early on phantom running>0; the new test must prove the row gets marked failed and the slot gets released, and prove checkEvalStall is no longer silenced by a stuck-running row. Verify with go test -count=1 -short -p 1 ./internal/scheduler/ and a RED mutation: force the failure write to be skipped, watch the new test fail, then restore.: All elements verified with live command output. (1) Failure write + Release: internal/scheduler/slot_pool.go:382-397 — on Spawn error the path calls p.lifecycle.Complete(TickOutcome{Status: TickFailed, Error: err.Error()}) then returns; the spawn goroutine's deferred clearReserve/Release frees the slot. (2) loop.go:827 — `if running > 0 && !l.allRunningRowsArePhantoms() { return }` replaces the blanket early return; helper allRunningRowsArePhantoms() at loop.go:747 uses gatewayZombieMaxAge (15m, tick_process.go:259) with julianday() comparison, fails safe on zero rows/query error. (3) RED MUTATION PROVEN: the working tree contained `if true { return } // RED MUTATION: skip failure write` at slot_pool.go:382; with it applied, `go test -count=1 -short -p 1 -run SCHEDGAP135 -v ./internal/scheduler/` exited 1 with `--- FAIL: TestSCHEDGAP135_RetryExhaustMarksFailedAndReleasesSlot (3.58s)`, 4x `status="running", want "failed"` and `running tick rows = 4, want 0`. (4) RESTORED: `git checkout -- internal/scheduler/slot_pool.go`; `grep -rn 'RED MUTATION' internal/` => CLEAN; committed HEAD version retains the failure write. (5) GREEN after restore: same command => `--- PASS: TestSCHEDGAP135_RetryExhaustMarksFailedAndReleasesSlot (3.59s)`, `--- PASS: TestSCHEDGAP135_EvalStallNotBlindedByPhantomRunning (0.06s)` with log `EVAL-STALL: last eval 10m0s ago with 4 running ticks — forced re-evaluation`, `--- PASS: TestSCHEDGAP135_EvalStallLiveRunningStillSuppresses (0.08s)`, `ok github.com/coding-hermes/scheduler/internal/scheduler 3.750s`. (6) Full suite: `go test -count=1 -short -p 1 ./internal/scheduler/` => exit 0, `ok github.com/coding-hermes/scheduler/internal/scheduler 65.154s`, zero FAIL lines. Test file schedgap135_phantom_test.go asserts tickStatusOf=="failed" for all 4 ticks, RunningCount()==0, pool.Running()==0, final GATEWAY RETRY 3/3 line per tick, plus assertForcedEval/assertStallEventCount(db,1) for the phantom-stall case.
SCHED-GAP-135 is fully implemented and verified: the retry-exhaust path marks ticks failed and releases slots, checkEvalStall no longer returns early on phantom running>0, the new tests are RED-proven against a skipped failure write and GREEN after restore, and the full scheduler package suite passes (ok 65.154s).

Overall: PASS ✓
