# S04 — Weight-Budget Packer

**Status:** Draft  
**Depends on:** S01, S02, S03  

---

## 1. Overview

The weight-budget packer decides which projects run this tick given a fixed weight budget and concurrency cap. It sorts projects by urgency (from S03), then greedily packs them into the budget — highest urgency first. Lightweight projects squeeze into remaining gaps that heavyweight projects can't fill.

This is a **fractional knapsack lite** — projects are indivisible (you can't run half a foreman), but weight is the resource consumed.

---

## 2. Dependencies

| Dependency | Purpose |
|-----------|---------|
| `sort` (stdlib) | Sort projects by urgency |
| `Store` (S02) | Query last completed tick for cooldown; count running ticks |
| `time` (stdlib) | Cooldown comparison |

---

## 3. Interface

```go
package scheduler

import "time"

// WeightPacker sorts projects by urgency and greedily packs into a weight budget.
type WeightPacker struct {
    budget        int
    maxConcurrent int
}

// NewWeightPacker creates a packer with the given budget and concurrency cap.
func NewWeightPacker(budget, maxConcurrent int) *WeightPacker

// Pack selects which projects should run this tick.
// projects: all enabled projects with computed urgency
// running: tick IDs of currently-running ticks (for concurrency check)
// now: current time (for cooldown check)
// Returns the subset of projects that should spawn this tick.
func (w *WeightPacker) Pack(
    projects []ProjectWithUrgency,
    lastCompleted map[string]time.Time,
    running []string,
    now time.Time,
) []ProjectWithUrgency

// SetBudget updates the weight budget at runtime.
func (w *WeightPacker) SetBudget(budget int)
```

### Input/Output Types

```go
// ProjectWithUrgency combines a project with its computed urgency.
type ProjectWithUrgency struct {
    Name      string
    Weight    int
    Priority  int
    CooldownS int
    Urgency   float64
}

// SkippedReason explains why a project was not selected.
type SkippedReason string

const (
    SkipCooldown      SkippedReason = "cooldown_not_elapsed"
    SkipBudgetExhausted              = "budget_exhausted"
    SkipMaxConcurrent                = "max_concurrent_reached"
    SkipDisabled                     = "disabled"
)

// PackResult provides full visibility into the packing decision.
type PackResult struct {
    Selected []ProjectWithUrgency
    Skipped  []struct {
        Project ProjectWithUrgency
        Reason  SkippedReason
    }
}
```

---

## 4. Behavior

### 4.1 Packing Algorithm

```
1. Filter: remove disabled projects
2. Sort by urgency descending
3. Initialize: budget_remaining = budget, selected = []
4. For each project in sorted order:
   a. Cooldown check:
      - If project.CooldownS > 0 AND lastCompleted exists:
        - elapsed = now.Sub(lastCompleted).Seconds()
        - If elapsed < project.CooldownS: SKIP (reason: cooldown_not_elapsed)
        - Else: proceed
      - If never completed: proceed (no cooldown applies)
   b. Budget check:
      - If project.Weight > budget_remaining: SKIP (reason: budget_exhausted)
      - Else: proceed
   c. Concurrency check:
      - If len(running) + len(selected) >= maxConcurrent: SKIP (reason: max_concurrent_reached)
      - Else: proceed
   d. SELECT: add to selected, budget_remaining -= project.Weight
5. Return PackResult{selected, skipped}
```

### 4.2 Decision Tree

```
For each project (sorted by urgency descending):
│
├── Cooldown not elapsed?
│   └── YES → skip (cooldown_not_elapsed)
│
├── weight > budget_remaining?
│   └── YES → skip (budget_exhausted)
│
├── would exceed maxConcurrent?
│   └── YES → skip (max_concurrent_reached)
│
└── ALL CHECKS PASS → SELECT
    └── budget_remaining -= weight
    └── selected.append(project)
```

### 4.3 Packing Example

```
Budget: 100, Max Concurrent: 8, Already Running: 2

Sorted by urgency:
  muster      w=25, P=8, U=42.1  → SELECT (budget: 75)
  consensus   w=20, P=7, U=38.3  → SELECT (budget: 55)
  chronicle   w=18, P=6, U=32.1  → SELECT (budget: 37)
  ai-poke     w=8,  P=5, U=28.4  → SELECT (budget: 29)
  helix       w=30, P=9, U=27.1  → SKIP (30 > 29) — budget exhausted, but...
  h4f         w=5,  P=4, U=24.3  → SELECT (budget: 24) — lightweight squeezes in!
  dexdat      w=3,  P=3, U=22.1  → SELECT (budget: 21)
  crier       w=4,  P=2, U=18.7  → SELECT (budget: 17)
  bunker      w=12, P=6, U=15.2  → SKIP (12 > 17... wait, 12 < 17)
                                   → SELECT (budget: 5)
  mythos      w=20, P=5, U=12.8  → SKIP (20 > 5)
  speclang    w=3,  P=1, U=8.2   → SELECT (budget: 2)
  --- maxConcurrent reached (8 selected + 2 running = 10, cap is 8) ---
  --- actually cap is 8 total... hmm ---

Let me redo: running=2, so max to select=6 (8-2)

  Selected: muster, consensus, chronicle, ai-poke, h4f, dexdat (6 projects)
  Budget used: 25+20+18+8+5+3 = 79
  Budget remaining: 21
  Skipped: helix (30 > 21), crier, bunker, mythos, speclang (concurrency cap)
```

**Key insight:** `h4f` (w=5) and `dexdat` (w=3) squeezed in AFTER `helix` (w=30) was skipped for budget. Lightweight high-urgency projects always find a slot.

### 4.4 Urgency Tie-Breaking

When two projects have equal urgency (within floating-point epsilon of 0.001):

1. Higher priority wins
2. If still tied: lower weight wins (lighter projects cost less to run)
3. If still tied: alphabetical by name (deterministic)

```go
func compareUrgency(a, b ProjectWithUrgency) int {
    diff := b.Urgency - a.Urgency
    if math.Abs(diff) < 0.001 {
        // Tie-break: higher priority first
        if a.Priority != b.Priority {
            diff = float64(b.Priority - a.Priority)
        } else {
            // Tie-break: lighter first
            diff = float64(a.Weight - b.Weight)
            if math.Abs(diff) < 0.001 {
                // Ultimate tie-break: alphabetical
                if a.Name < b.Name {
                    return -1
                }
                return 1
            }
        }
    }
    if diff > 0 {
        return 1   // b has higher urgency
    }
    return -1      // a has higher urgency
}
```

---

## 5. Edge Cases

| Scenario | Expected Behavior |
|----------|-------------------|
| Empty project list | Return empty PackResult |
| All projects overweight (weight > budget) | All skipped with `budget_exhausted` |
| Budget = 0 | All skipped with `budget_exhausted` |
| Max concurrent = 0 | All skipped with `max_concurrent_reached` |
| All projects in cooldown | All skipped with `cooldown_not_elapsed` |
| Running = maxConcurrent already | All skipped with `max_concurrent_reached` |
| Single project fits perfectly (weight = budget) | Selected, budget = 0, all others skipped |
| No lastCompleted for any project | No cooldown checks apply |
| Negative cooldown (shouldn't happen — CHECK constraint) | Treated as 0 (no cooldown) |
| Clock skew (lastCompleted > now) | Elapsed = 0 → cooldown applies |

---

## 6. States

The packer is stateless between evaluation cycles. `budget` and `maxConcurrent` are set at construction and mutable via `SetBudget`.

---

## 7. Errors

| Condition | Behavior |
|-----------|----------|
| `budget <= 0` on `NewWeightPacker` | `log.Printf("WARNING: budget <= 0, no projects will be scheduled")`; set budget = 0 |
| `maxConcurrent <= 0` on `NewWeightPacker` | `log.Printf("WARNING: maxConcurrent <= 0, no projects will be scheduled")`; set maxConcurrent = 0 |
| `SetBudget(negative)` | `log.Printf("WARNING: negative budget, setting to 0")`; set budget = 0 |

The packer **never panics**. Invalid configs result in zero scheduling (safe failure mode — nothing runs, nothing breaks).

---

## 8. Testing

```go
func TestPack_EmptyProjects(t *testing.T) {
    // nil or empty project list → empty result
}

func TestPack_SingleProjectFits(t *testing.T) {
    // Budget=100, project weight=25, no cooldown → selected
}

func TestPack_SingleProjectOverweight(t *testing.T) {
    // Budget=20, project weight=25 → skipped (budget_exhausted)
}

func TestPack_GreedyFillsBudget(t *testing.T) {
    // 5 projects: weights 10, 20, 30, 40, 50, budget=100
    // Expected: 10, 20, 30, 40 selected (budget used: 100), 50 skipped
}

func TestPack_LightProjectsSqueezeIn(t *testing.T) {
    // After selecting heavy projects, budget=19
    // Project with weight=18 fits, project with weight=20 doesn't
    // Then project with weight=3 STILL fits → selected
    // Demonstrates the squeeze-in property
}

func TestPack_CooldownBlocks(t *testing.T) {
    // Project ran 1 minute ago, cooldown=300s → skipped
}

func TestPack_CooldownElapsed(t *testing.T) {
    // Project ran 6 minutes ago, cooldown=300s → selected
}

func TestPack_NeverRunNoCooldown(t *testing.T) {
    // No lastCompleted → cooldown does not apply → selected
}

func TestPack_MaxConcurrentCap(t *testing.T) {
    // maxConcurrent=3, already running 2 → only 1 can be selected
}

func TestPack_BudgetZero(t *testing.T) {
    // Budget=0 → all skipped
}

func TestPack_TieBreaking(t *testing.T) {
    // Two projects with equal urgency → higher priority wins
    // Two projects with equal urgency + priority → lower weight wins
    // Two projects with equal urgency + priority + weight → alphabetical
}

func TestPack_SetBudget(t *testing.T) {
    // Budget=50, call SetBudget(200), next Pack uses 200
}
```

---

## 9. Security

No security concerns — pure in-memory computation with no external I/O during `Pack()`. The database queries (lastCompleted, running count) happen in the scheduler loop, not in the packer.

---

## 10. Performance

|| Metric | Target |
||--------|--------|
|| Sort 33 projects | < 5 µs (Go's pdqsort, O(n log n)) |
|| Pack 33 projects | < 3 µs (single pass, O(n)) |
|| Total | < 10 µs |

Zero allocations beyond the result slice. The `PackResult` is stack-friendly for the common case (< 33 projects).

---

## 11. Multi-Namespace Extension

**See S07 for the full specification.** When `NamespaceMode=true`, the flat `WeightPacker` is replaced by a `MultiPoolPacker` with a four-phase algorithm:

### 11.1 MultiPoolPacker Interface

```go
// MultiPoolPacker replaces WeightPacker when namespace mode is enabled.
type MultiPoolPacker struct {
    allocator     *NamespaceAllocator
    maxConcurrent int
}

func NewMultiPoolPacker(budget, maxConcurrent int) *MultiPoolPacker

// Pack runs the full four-phase algorithm.
func (m *MultiPoolPacker) Pack(
    projects []ProjectWithUrgency,
    namespaces []Namespace,
    lastCompleted map[string]time.Time,
    running []string,
    now time.Time,
) PackResult
```

### 11.2 Four-Phase Algorithm

```
PHASE 1 — NAMESPACE ALLOCATION
  reserved_sum = Σ(ns.reserved)
  remainder = budget - reserved_sum
  For each namespace:
    allocation = ns.reserved + (ns.weight / Σweights) × remainder
    allocation = min(allocation, ns.hard_cap)

PHASE 2 — INTRA-NAMESPACE PACKING
  For each namespace with allocation > 0:
    effective_weight = allocation × (project.weight / Σweights_in_ns)
    Sort by urgency, greedy pack into allocation

PHASE 3 — BORROWING PASS
  Collect unused budget from satisfied namespaces
  Distribute to hungry namespaces (up to their hard_caps)
  Re-pack borrowers with new allocation (one level of re-borrowing)

PHASE 4 — SPAWN
  Collect all selected projects → run_queue → SpawnEngine (S05)
  Record namespace_ticks rows
```

### 11.3 Backward Compatibility

`NamespaceMode=false` (default) uses the flat `WeightPacker` exactly as specified in sections 1–10. No behavior change. Existing deployments continue unchanged.

### 11.4 Two-Axis Weighting

A project's **effective global consumption** is the product of two independent weights:

```
effective_consumption = namespace_allocation × (w_job / Σw_all_jobs_in_namespace)
```

This means a job with `weight=60` in the `data-cleanup` namespace (gets ~9 units global) has an effective weight of `9 × (60/200) = 2.7 → 2` units. The same `weight=60` in `coding-hermes` (gets ~61 units global) has an effective weight of `61 × (60/230) = 15.9 → 15` units. Same intra-namespace weight, fundamentally different global impact — exactly the "airline cargo" two-axis model.
