# S07 — Multi-Namespace Weight-Budget Extension

**Status:** Draft  
**Depends on:** S01, S02, S03, S04, S05, S06  
**Pages target:** 6-8

---

## 1. Overview

The current scheduler uses a single flat pool: 33 projects compete for one global weight budget (B=100). This works for a homogeneous workload (coding foremen), but the scheduler should schedule **any Hermes cron workload** — not just coding. Different workloads have fundamentally different shapes:

| Workload Type | Job Weight | Concurrency | Example |
|---|---|---|---|
| Coding foremen | Heavy (w=60+) | 3-5 per tick | `scheduler-go`, `speclang` |
| Data cleanup | Light (w=1-8) | 20+ per tick | `purge-old-tickets`, `compact-duckbrain` |
| Health checks | Ultra-light (w=1-2) | Every tick | `gateway-health`, `disk-watchdog` |
| Infrastructure | Moderate (w=3-10) | Periodic | `cert-renewal`, `duckbrain-sync` |

A flat pool can't express these relationships. A coding foreman at w=60 and a disk cleanup job at w=60 mean totally different things — the foreman is 60 out of a coding budget, the cleanup is 60 out of… what?

### Solution: Namespaces as Weight Pools

A **namespace** is a pool of related cron jobs with its own weight budget. Two levels of weighting create the "airline cargo" metaphor:

1. **Global level**: Namespaces compete for shares of the global budget (B=100)
2. **Intra-namespace level**: Jobs within a namespace compete for their namespace's allocation

A job's effective global consumption is the **product** of two independent weights:

```
effective_consumption = namespace_allocation × (w_job / Σw_all_jobs_in_namespace)
```

This means a data-cleanup job with w_job=60 is NOT the same as a coding-hermes job with w_job=60. The data-cleanup namespace gets ~10% of global budget, so that w_job=60 job consumes `60/Σw_data-cleanup × 10%` of global — maybe 2-3 units. The coding-hermes job with w_job=60 consumes `60/Σw_coding × 70%` of global — maybe 12-15 units. Same intra-namespace weight, different global impact.

---

## 2. Dependencies

| Dependency | Purpose | Failure Mode |
|---|---|---|
| SQLite (S02) | Namespace and namespace_ticks tables | Namespace operations fail; scheduler falls back to flat mode |
| Urgency Calculator (S03) | Unchanged — per-job urgency still used | Per-job urgency unaffected |
| Weight Packer (S04) | Two-level packing algorithm replaces flat greedy | Namespace allocation skipped; all jobs treated as one flat pool |
| REST API (S06) | New `/namespaces` endpoints | Namespace management unavailable via API |
| Config (S01) | New `NamespaceMode` toggle; global budget unchanged | Feature gated behind config flag |

---

## 3. Interface

### 3.1 Namespace Allocator

```go
package scheduler

// NamespaceAllocator distributes the global budget across namespaces.
type NamespaceAllocator struct {
    budget int  // global budget (default 100)
}

func NewNamespaceAllocator(budget int) *NamespaceAllocator

// Allocate runs the two-phase distribution: reserved floor + proportional remainder.
// Returns per-namespace allocation in budget units.
// Namespaces that get zero allocation still get their reserved floor.
func (a *NamespaceAllocator) Allocate(
    namespaces []Namespace,
) map[string]int // namespace_id → allocated budget

// SetBudget updates the global budget at runtime.
func (a *NamespaceAllocator) SetBudget(budget int)
```

### 3.2 Multi-Pool Packer (extends S04)

```go
// MultiPoolPacker replaces WeightPacker when namespace mode is enabled.
type MultiPoolPacker struct {
    allocator   *NamespaceAllocator
    maxConcurrent int
}

func NewMultiPoolPacker(budget, maxConcurrent int) *MultiPoolPacker

// Pack runs the full four-phase algorithm and returns selected projects.
func (m *MultiPoolPacker) Pack(
    projects []ProjectWithUrgency,
    namespaces []Namespace,
    lastCompleted map[string]time.Time,
    running []string,
    now time.Time,
) PackResult
```

### 3.3 Borrowing Engine

```go
// BorrowingEngine redistributes idle namespace capacity.
type BorrowingEngine struct{}

func NewBorrowingEngine() *BorrowingEngine

// Borrow collects unused budget from under-utilized namespaces and
// distributes it to namespaces with queued jobs that hit their allocation ceiling.
// Returns updated allocations (allocated + borrowed - lent).
func (b *BorrowingEngine) Borrow(
    allocations map[string]int,
    namespaces []Namespace,
    queuedJobs map[string][]ProjectWithUrgency, // namespace_id → jobs still queued
) map[string]int
```

### 3.4 Config Extension

```go
// Added to Config (S01):
type Config struct {
    // ... existing fields ...
    NamespaceMode  bool `env:"SCHEDULER_NAMESPACE_MODE" default:"false"`
    // When false (default): flat single-pool mode (backward compatible)
    // When true: multi-namespace two-axis mode
}
```

---

## 4. Behavior

### 4.1 Four-Phase Scheduling Algorithm

EVERY TICK (60s), when `NamespaceMode=true`:

```
═══════════════════════════════════════════
PHASE 1 — NAMESPACE ALLOCATION
═══════════════════════════════════════════
1. Load all enabled namespaces from SQLite.
   If zero namespaces exist → fall back to flat mode for this tick.

2. Sum reserved across all enabled namespaces → R_total.
   If R_total > B → log ERROR, proportionally scale reserved values.

3. remainder = B - R_total.
   If remainder < 0 → set remainder = 0, all namespaces get exactly reserved.

4. For each namespace:
   a. proportional_share = (namespace.weight / Σall_weights) × remainder
   b. allocation = reserved + proportional_share
   c. allocation = min(allocation, hard_cap)
   d. If hard_cap == 0 → no cap (interpret as B)
   e. Store allocation in map[namespace_id]int

5. Log the allocation table at DEBUG level.
```

```
═══════════════════════════════════════════
PHASE 2 — INTRA-NAMESPACE PACKING (per namespace)
═══════════════════════════════════════════
For each namespace (skip if allocation == 0):
1. Filter projects belonging to this namespace.
   Projects with namespace_id=NULL are unscheduled (skip them).

2. For each project in this namespace:
   a. Compute urgency using S03 ComputeUrgency (unchanged)
   b. effective_weight = allocation × (project.weight / Σall_project_weights_in_ns)
   c. effective_weight = max(effective_weight, 1)  // floor at 1 unit

3. Sort projects by urgency descending.

4. Greedy pack into namespace's allocation:
   budget_remaining = allocation
   for project in sorted_projects:
       if cooldown not elapsed: SKIP
       if effective_weight > budget_remaining: SKIP
       if running_count + selected_count >= maxConcurrent: SKIP
       SELECT, budget_remaining -= effective_weight

5. Track:
   - selected[namespace_id] = []Project  (projects to run)
   - queued[namespace_id] = []Project     (projects that didn't fit)
   - unused[namespace_id] = budget_remaining  (idle capacity)
```

```
═══════════════════════════════════════════
PHASE 3 — BORROWING PASS
═══════════════════════════════════════════
1. Collect unused budget from namespaces where:
   unused > 0 AND queued is empty (namespace fully satisfied)
   → lent_pool += unused

2. Identify borrowing candidates:
   namespaces where queued is non-empty (hit allocation ceiling)

3. If lent_pool == 0 OR no borrowers → skip to Phase 4.

4. Distribute lent_pool to borrowers:
   a. Sort borrowers by namespace weight descending (heavier namespaces get priority)
   b. For each borrower:
      - max_borrow = min(hard_cap - original_allocation, lent_pool)
      - If max_borrow <= 0: skip (already at cap)
      - need = sum of effective_weights of queued jobs
      - borrow = min(need, max_borrow)
      - allocation += borrow, lent_pool -= borrow

5. Re-pack borrowing namespaces with new allocation:
   For each borrower that received budget:
   a. budget_remaining = new_allocation - already_selected_budget
   b. Continue packing queued jobs into budget_remaining
   c. If any budget still unused after re-pack → flows back to lent_pool
   d. Recurse once more (only one level of re-borrowing)

6. Log borrowing activity: who lent, who borrowed, how much.
```

```
═══════════════════════════════════════════
PHASE 4 — SPAWN (delegates to S05)
═══════════════════════════════════════════
1. Collect all selected projects across all namespaces → run_queue
2. For each project in run_queue:
   a. Generate tick ID: <project>-<YYYY>-<MM>-<DD>-<HH>-<mm>-<ss>
   b. Write tick to SQLite (status=queued)
   c. Call SpawnEngine.Spawn() (S05)
3. Record namespace_ticks row per namespace:
   - allocated: budget given
   - used: budget actually consumed (sum of effective_weights of spawned jobs)
   - borrowed: extra budget received in Phase 3
   - lent: budget given away in Phase 3
   - job_count: number of jobs spawned
4. Write evaluation events (decision level) per namespace.
```

### 4.2 Allocation Math (Detailed)

```
Given:
  B = 100 (global budget)
  Namespaces with (weight, reserved, hard_cap):
    coding-hermes:    (60, 25, 85)
    monitoring:       (15,  8, 30)
    data-cleanup:     (10,  3, 35)
    duckbrain-infra:  (10,  2, 20)
    backup:           ( 5,  2, 15)

Step 1: R_total = 25 + 8 + 3 + 2 + 2 = 40

Step 2: remainder = 100 - 40 = 60
        Σweights = 60 + 15 + 10 + 10 + 5 = 100

Step 3: Proportional distribution:
  coding-hermes:    25 + (60/100)×60 = 25 + 36.0 = 61.0 → 61 (under 85 cap)
  monitoring:        8 + (15/100)×60 =  8 +  9.0 = 17.0 → 17 (under 30 cap)
  data-cleanup:      3 + (10/100)×60 =  3 +  6.0 =  9.0 →  9 (under 35 cap)
  duckbrain-infra:   2 + (10/100)×60 =  2 +  6.0 =  8.0 →  8 (under 20 cap)
  backup:            2 +  (5/100)×60 =  2 +  3.0 =  5.0 →  5 (under 15 cap)
  ─────────────────────────────────────────────────────────────────
  TOTAL: 100 ✓
```

### 4.3 Effective Weight Calculation

```
For namespace data-cleanup with allocation=9 and jobs:
  purge-old-tickets:    w=5
  compact-duckbrain:    w=8
  rotate-logs:          w=3
  vacuum-sqlite:        w=4
  Σw = 20

Effective weights:
  purge-old-tickets:    9 × (5/20) = 2.25 → 2
  compact-duckbrain:    9 × (8/20) = 3.60 → 3
  rotate-logs:          9 × (3/20) = 1.35 → 1
  vacuum-sqlite:        9 × (4/20) = 1.80 → 1

Total effective: 2 + 3 + 1 + 1 = 7 ≤ 9 ✓
All 4 jobs fit in this namespace's budget.
Compare: in flat mode, these jobs would compete against coding foremen at w=60.
```

### 4.4 Borrowing Examples

**Scenario A — Heavy coding, starved monitoring:**
```
coding-hermes: allocated=61, used=58, unused=3
monitoring:    allocated=17, used=17, queued=5 jobs (need 8 more)
data-cleanup:  allocated=9,  used=3,  unused=6
backup:        allocated=5,  used=5,  unused=0

Lent pool: 3 (coding) + 6 (cleanup) = 9
Borrowers: monitoring (needs 8, cap allows up to 30-17=13 more)
Result: monitoring gets +8 → runs all queued jobs
        Remaining 1 → flows back or is absorbed by next borrower
```

**Scenario B — Idle coding, cleanup surge:**
```
coding-hermes: allocated=61, used=8,  unused=53
data-cleanup:  allocated=9,  used=9,  queued=22 jobs (need 26 more)
backup:        allocated=5,  used=2,  unused=3

Lent pool: 53 (coding) + 3 (backup) = 56
Borrowers: data-cleanup (needs 26, cap allows up to 35-9=26 more)
Result: data-cleanup gets +26 → runs 22+ jobs at effective weights ~1-2
        Uses 9+26=35 (hits hard cap), packs 40+ lightweight jobs
        Remaining 30 lent units go unused this tick
```

### 4.5 Fallback Behavior

When `NamespaceMode=false` (default) or no namespaces exist:
- Scheduler operates exactly as the current flat single-pool mode
- All projects compete in one pool with B=100
- No namespace_ticks rows are written
- API namespace endpoints return empty results with a note

This is **not** a breaking change. Existing deployments continue unchanged.

---

## 5. Data

### 5.1 Namespaces Table (new)

```sql
CREATE TABLE IF NOT EXISTS namespaces (
    id          TEXT PRIMARY KEY NOT NULL,
    weight      INTEGER NOT NULL DEFAULT 10 CHECK(weight >= 1 AND weight <= 100),
    reserved    INTEGER NOT NULL DEFAULT 1 CHECK(reserved >= 0),
    hard_cap    INTEGER NOT NULL DEFAULT 100 CHECK(hard_cap >= 0),
    enabled     INTEGER NOT NULL DEFAULT 1 CHECK(enabled IN (0, 1)),
    description TEXT,
    created_at  TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at  TEXT NOT NULL DEFAULT (datetime('now'))
);
```

### 5.2 Projects Table Migration

```sql
-- Migration version 2:
ALTER TABLE projects ADD COLUMN namespace_id TEXT 
    REFERENCES namespaces(id) ON DELETE SET NULL;

CREATE INDEX IF NOT EXISTS idx_projects_namespace ON projects(namespace_id);
```

Projects with `namespace_id=NULL` are **unscheduled** in namespace mode. They appear in the dashboard as "unassigned" and must be moved to a namespace to run.

### 5.3 Namespace Ticks Table (new)

```sql
CREATE TABLE IF NOT EXISTS namespace_ticks (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    tick_group   TEXT NOT NULL,           -- group identifier: <YYYY>-<MM>-<DD>-<HH>-<mm>-<ss>
    namespace_id TEXT NOT NULL REFERENCES namespaces(id),
    allocated    INTEGER NOT NULL,        -- budget given this tick
    used         INTEGER NOT NULL,        -- budget actually consumed (sum of effective weights)
    borrowed     INTEGER NOT NULL DEFAULT 0, -- extra budget from other namespaces
    lent         INTEGER NOT NULL DEFAULT 0, -- budget given to other namespaces
    job_count    INTEGER NOT NULL,        -- how many jobs ran
    created_at   TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE INDEX IF NOT EXISTS idx_namespace_ticks_group ON namespace_ticks(tick_group);
CREATE INDEX IF NOT EXISTS idx_namespace_ticks_ns ON namespace_ticks(namespace_id, created_at DESC);
```

### 5.4 Go Model Structs (additions to S02)

```go
// Namespace represents a weight pool for related cron jobs.
type Namespace struct {
    ID          string    `json:"id"`
    Weight      int       `json:"weight"`
    Reserved    int       `json:"reserved"`
    HardCap     int       `json:"hard_cap"`
    Enabled     bool      `json:"enabled"`
    Description string    `json:"description"`
    CreatedAt   time.Time `json:"created_at"`
    UpdatedAt   time.Time `json:"updated_at"`
}

// NamespacePatch is used for partial updates.
type NamespacePatch struct {
    Weight      *int    `json:"weight,omitempty"`
    Reserved    *int    `json:"reserved,omitempty"`
    HardCap     *int    `json:"hard_cap,omitempty"`
    Enabled     *bool   `json:"enabled,omitempty"`
    Description *string `json:"description,omitempty"`
}

// NamespaceTick records per-namespace utilization per evaluation cycle.
type NamespaceTick struct {
    ID          int64     `json:"id"`
    TickGroup   string    `json:"tick_group"`
    NamespaceID string    `json:"namespace_id"`
    Allocated   int       `json:"allocated"`
    Used        int       `json:"used"`
    Borrowed    int       `json:"borrowed"`
    Lent        int       `json:"lent"`
    JobCount    int       `json:"job_count"`
    CreatedAt   time.Time `json:"created_at"`
}

// Project gets an additional field:
type Project struct {
    // ... existing fields ...
    NamespaceID *string `json:"namespace_id"` // NULL = unscheduled in namespace mode
}
```

### 5.5 DuckBrain Key Schema (additions)

#### `/fleet/namespaces` — Namespace Registry

```json
{
    "updated_at": "2026-07-12T14:05:00Z",
    "namespaces": [
        {
            "id": "coding-hermes",
            "weight": 60,
            "reserved": 25,
            "hard_cap": 85,
            "enabled": true,
            "project_count": 31,
            "last_allocation": 61,
            "last_used": 58,
            "last_jobs": 6
        }
    ]
}
```

#### `/fleet/namespaces/<id>/status` — Per-Namespace Detail

```json
{
    "id": "coding-hermes",
    "weight": 60,
    "reserved": 25,
    "hard_cap": 85,
    "enabled": true,
    "description": "Coding Hermes foreman fleet",
    "project_count": 31,
    "current_allocation": 61,
    "current_usage": 58,
    "borrowing_history": {
        "last_10_ticks": [
            {"tick_group": "2026-07-12-14-05-00", "allocated": 61, "used": 58, "borrowed": 0, "lent": 3, "jobs": 6}
        ]
    },
    "updated_at": "2026-07-12T14:05:00Z"
}
```

---

## 6. States

### 6.1 Namespace State

```
Namespace: ENABLED ── enabled=false ──▶ DISABLED
           DISABLED ── enabled=true ──▶ ENABLED

When disabled:
  - Gets zero allocation (no reserved floor)
  - Weight excluded from Σweights
  - All projects in namespace become unscheduled
  - Existing namespace_ticks rows preserved
```

### 6.2 Per-Tick Allocation State

```
For each namespace, every tick:
  ALLOCATED → packed → USED (portion consumed)
                      → UNUSED (available for lending)
  
  If USED < ALLOCATED: LENT = ALLOCATED - USED (offered to borrowers)
  If USED == ALLOCATED and jobs queued: BORROW (receive from lenders)
```

### 6.3 Project Namespace Assignment

```
Project: UNSCHEDULED (namespace_id=NULL) ── assign ──▶ SCHEDULED (namespace_id=<ns>)
         SCHEDULED ── unassign ──▶ UNSCHEDULED
         SCHEDULED in ns_A ── move ──▶ SCHEDULED in ns_B
```

---

## 7. Errors

| Condition | Behavior |
|---|---|
| No namespaces exist | Fall back to flat mode; log WARNING |
| Σreserved > B | Log ERROR; proportionally scale all reserved values |
| Namespace not found (API) | 404 `"namespace not found"` |
| Duplicate namespace ID (API) | 409 `"namespace already exists"` |
| Invalid weight/reserved/cap (API) | 400 with field name |
| Project moved to non-existent namespace | 400 `"namespace not found"` |
| Project moved but namespace disabled | Accepted; project won't run until namespace enabled |
| All namespaces disabled | Fall back to flat mode for that tick |
| Borrowing calculation overflow | Clamp to hard_cap; log WARNING |
| Zero-weight namespace | Treated as weight=1; log WARNING |

---

## 8. Testing

### Unit Tests

```go
func TestNamespaceAllocator_RespectedFloors(t *testing.T) {
    // Verify every namespace gets at least its reserved budget
    // Even when one namespace has weight=100 and others have weight=1
}

func TestNamespaceAllocator_HardCapEnforced(t *testing.T) {
    // Namespace with hard_cap=30 never gets more than 30
    // Even when it's the only namespace and B=100
}

func TestNamespaceAllocator_SumEqualsBudget(t *testing.T) {
    // Sum of all allocations == B (within rounding tolerance ±1)
    // Test with various namespace count and weight distributions
}

func TestNamespaceAllocator_ZeroReservedSum(t *testing.T) {
    // All namespaces have reserved=0 → purely proportional
}

func TestEffectiveWeight_ScalesCorrectly(t *testing.T) {
    // Job w=60 in ns with allocation=10, Σw=200 → effective = 10*(60/200) = 3
    // Job w=5 in ns with allocation=10, Σw=20 → effective = 10*(5/20) = 2.5 → 2
}

func TestEffectiveWeight_FloorAtOne(t *testing.T) {
    // Tiny job in tiny namespace: allocation=1, w=1, Σw=100 → effective = 0.01 → 1
}

func TestBorrowing_LenderHasUnused(t *testing.T) {
    // Namespace A used 5 of 20 → 15 lent
    // Namespace B needs 10, has cap room → borrows 10
    // Remaining 5 stays in lent pool for next borrower
}

func TestBorrowing_HardCapBlocks(t *testing.T) {
    // Namespace B has hard_cap=30, already allocated 28, needs 10
    // Can only borrow 2 (30-28)
}

func TestBorrowing_NoLenders(t *testing.T) {
    // All namespaces fully utilized → no borrowing
}

func TestBorrowing_NoBorrowers(t *testing.T) {
    // Large unused pool but all namespaces satisfied → nothing happens
}

func TestMultiPoolPacker_FallbackFlatMode(t *testing.T) {
    // NamespaceMode=false → uses flat single-pool packing (unchanged behavior)
}

func TestMultiPoolPacker_EmptyNamespaces(t *testing.T) {
    // No namespaces in DB → fall back to flat mode
}

func TestMultiPoolPacker_UnassignedProjects(t *testing.T) {
    // Projects with namespace_id=NULL are never selected in namespace mode
}

func TestMultiPoolPacker_DisabledNamespace(t *testing.T) {
    // Disabled namespace gets zero allocation, its projects don't run
}
```

### Integration Tests

```text
1. Create 3 namespaces with projects, run evaluation, verify per-namespace allocation
2. Verify namespace_ticks rows are written with correct allocated/used/borrowed/lent
3. Verify borrowing: starve one namespace, give surplus to another, verify redistribution
4. Toggle NamespaceMode at runtime, verify smooth transition
5. Verify migration: add namespace_id column, existing projects get NULL (unscheduled)
6. Verify fallback: disable all namespaces, flat mode still works
```

---

## 9. Security

| Vector | Mitigation |
|---|---|
| Namespace ID injection | PK constraint limits length; parameterized queries |
| Budget overflow | All allocations clamped to [0, B]; sum validated each tick |
| Reserved > budget | Detected at allocation time; proportionally scaled |
| Borrowing recursion | Max 1 level of re-borrowing (Phase 3 step 6) |
| Starvation via namespace | Reserved floor guarantees every enabled namespace runs something |
| Starvation via hard_cap | Hard cap prevents any namespace from consuming the entire budget |

---

## 10. Performance

| Metric | Target |
|---|---|
| Namespace allocation (Phase 1) | < 5 µs (O(n) for n namespaces, n ≤ 20) |
| Intra-ns packing (Phase 2) | < 50 µs (O(p log p) per namespace, total p ≤ 100) |
| Borrowing (Phase 3) | < 10 µs (O(n log n) for n namespaces) |
| Total multi-pool overhead | < 100 µs above flat mode |
| Namespace CRUD (API) | < 20ms (single-row SQLite ops) |
| Namespace_ticks write | < 10ms (batch insert with one transaction per tick_group) |

### Backward Compatibility

- `NamespaceMode=false` (default) → zero performance impact, zero behavioral change
- `NamespaceMode=true` with no namespaces → falls back to flat mode with one log warning
- Existing SQLite databases migrate automatically (new column with NULL default)
- Existing projects continue running in flat mode until assigned to namespaces

### Migration Path

1. Deploy scheduler binary with namespace support (NamespaceMode=false)
2. Schema migration adds `namespaces` and `namespace_ticks` tables, `projects.namespace_id` column
3. Bane creates namespaces and assigns projects via API or MCP
4. Bane sets `NamespaceMode=true` via API or config
5. Scheduler transitions to two-axis mode on next tick
6. Old flat mode remains available by setting `NamespaceMode=false`

---

## 11. Fleet Commands (MCP + CLI)

```
/ns list                           # All namespaces with weights, reserved, hard_caps, project counts
/ns create NAME WEIGHT RESERVED HARD_CAP [DESCRIPTION]  # Create namespace
/ns get NAME                       # Namespace detail with utilization history
/ns weight NAME N                  # Set namespace weight (1-100)
/ns reserved NAME N                # Set reserved capacity
/ns cap NAME N                     # Set hard cap (0 = unlimited)
/ns move PROJECT_ID NAMESPACE      # Move a project to a different namespace
/ns unassign PROJECT_ID            # Remove project from namespace (unscheduled)
/ns enable NAME                    # Enable namespace
/ns disable NAME                   # Disable namespace (projects unscheduled)
/ns status                         # Per-namespace utilization, borrowing activity, current allocations
/ns delete NAME                    # Delete namespace (projects become unassigned)
```

---

## 12. Real Fleet Configuration (Example)

```json
{
    "namespaces": [
        {
            "id": "coding-hermes",
            "weight": 60,
            "reserved": 25,
            "hard_cap": 85,
            "description": "Coding Hermes autonomous foreman fleet — 31 projects"
        },
        {
            "id": "monitoring",
            "weight": 15,
            "reserved": 8,
            "hard_cap": 30,
            "description": "Health checks, watchdogs, cost tracking"
        },
        {
            "id": "data-cleanup",
            "weight": 10,
            "reserved": 3,
            "hard_cap": 35,
            "description": "Log rotation, DuckBrain compaction, old ticket purge"
        },
        {
            "id": "duckbrain-infra",
            "weight": 10,
            "reserved": 2,
            "hard_cap": 20,
            "description": "DuckBrain sync, memory compaction, index rebuilds"
        },
        {
            "id": "backup",
            "weight": 5,
            "reserved": 2,
            "hard_cap": 15,
            "description": "Cert renewal, config backup, database dumps"
        }
    ]
}
```

This configuration guarantees:
- **coding-hermes**: Always gets ≥25 units (runs at least 1-2 foremen), up to 85 when others are idle
- **monitoring**: Always gets ≥8 units (every health check runs), can surge to 30
- **data-cleanup**: Always gets ≥3 units (at least one cleanup job), can pack 20+ lightweight jobs when coding is quiet
- **duckbrain-infra**: Always gets ≥2 units, never starves
- **backup**: Always gets ≥2 units, never starves
- **No namespace can consume the entire budget** (hard caps prevent monopoly)
- **Idle capacity flows to hungry namespaces** (borrowing)
