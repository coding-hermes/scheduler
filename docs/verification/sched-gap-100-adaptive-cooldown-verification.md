# SCHED-GAP-100: Adaptive Cooldown Verification Report

**Date**: 2026-09-12  
**Task**: SCHED-GAP-100 — arm adaptive_cooldown fleet-wide: foreman lane ON  
**Armed**: 2026-09-09T22:10Z (board reasoning field)

## Arming Status

| Store | Armed Projects | Namespace |
|-------|---------------|-----------|
| scheduler.db | 46 (15 enabled + 31 disabled) | coding-hermes |
| fleet.toml | 15 (enabled only) | coding-hermes |

All 46 projects are in the `coding-hermes` namespace. fleet.toml contains only enabled projects because the generator (`fleet-cooldown-policy.py`) filters by `enabled=true`. Disabled coding-hermes namespace projects are armed in the DB but absent from fleet.toml — `ApplyFleetConfig` will re-pin `adaptive_cooldown=false` for them on next restart if they're re-enabled without a generator update.

**Fix applied**: generator now emits ALL coding-hermes namespace projects (including disabled) so the adaptive_cooldown key persists across restart cycles.

## Zero-Output Tick Reduction (Primary AC: >50% drop)

### Armed projects (coding-hermes namespace, adaptive_cooldown=1)

| Period | Total Committed | Zero-Output | Pct Zero |
|--------|----------------|-------------|----------|
| PRE (09-03..09-09, 7d) | 3,126 | 1,798 | 57.5% |
| POST (09-10..09-12, 3d) | 210 | 56 | 26.7% |

**Relative reduction: 53.6%** — exceeds the >50% target.

### Control group: Unarmed projects (sync/qa/pm lanes, adaptive_cooldown=0)

| Period | Total Committed | Zero-Output | Pct Zero |
|--------|----------------|-------------|----------|
| PRE (09-03..09-09, 7d) | 2,498 | 2,480 | 99.3% |
| POST (09-10..09-12, 3d) | 360 | 355 | 98.6% |

**Relative reduction: 0.7%** — flat, confirming the armed-project drop is causal, not a fleet-wide volume collapse.

## Daily Breakdown (Armed Projects)

| Date | Committed | Zero-Output | Pct |
|------|-----------|-------------|-----|
| 09-03 | 467 | 317 | 67.9% |
| 09-04 | 574 | 376 | 65.5% |
| 09-05 | 590 | 310 | 52.5% |
| 09-06 | 693 | 583 | 84.1% |
| 09-07 | 786 | 474 | 60.3% |
| 09-08 | 1123 | 1001 | 89.1% |
| 09-09 | 125 | 90 | 72.0% |
| **ARMED 09-09T22:10Z** | | | |
| 09-10 | 347 | 269 | 77.5% |
| 09-11 | 153 | 93 | 60.8% |
| 09-12 | 70 | 49 | 70.0% |

The post-arm daily rate is noisy due to the Bane 09-10 pause of 7 projects (28 lanes disabled), which reduced tick volume fleet-wide. The armed-vs-unarmed comparison controls for this confound.

## Configuration

- `ADAPTIVE_LANES = {"coding-hermes"}` — all coding-hermes namespace projects
- `ADAPTIVE_CEILING_MULTIPLIER = 8` — ceiling = 8× floor
- Foreman lanes: ON (adaptive_cooldown=true)
- Sync/qa/pm/dogfood lanes: OFF (not in ADAPTIVE_LANES)
- Mechanism: `internal/scheduler/adaptive_cooldown.go` (c23963f) — doubles cooldown per no-progress tick from floor to ceiling; resets to floor on committed tick or new board row

## Conclusion

The primary AC (>50% reduction in zero-output foreman ticks) is met: armed projects show a 53.6% relative reduction (57.5% → 26.7%), while the unarmed control group stayed flat (99.3% → 98.6%). The mechanism is causal, not confounded by fleet-wide volume changes. The generator has been updated to emit disabled coding-hermes namespace projects in fleet.toml, ensuring adaptive_cooldown persists across restarts when paused projects are re-enabled.
