# S03 — Urgency Calculator

**Status:** Draft  
**Depends on:** S01, S02  

---

## 1. Overview

The urgency calculator determines which projects the scheduler should run this tick. It has two functions:

1. **ComputeInterval** — maps a project's priority (1-10 integer, stored in the database) to a tick interval using a geometric/exponential curve. Accepts `float64` for forward compatibility with fractional priorities, but currently only integer values 1-10 are used (cast from `int` at the call site in `packer_select.go`).
2. **ComputeUrgency** — returns a numeric urgency score combining priority with how long the project has been waiting

Urgency is computed every evaluation cycle for every enabled project. The weight-budget packer sorts by urgency to decide who runs.

---

## 2. Dependencies

| Dependency | Purpose |
|-----------|---------|
| `time` (stdlib) | Duration calculations |
| `math` (stdlib) | `math.Pow` for exponential curve |
| `Config` (from S01) | `MinInterval`, `MaxInterval`, `NumLevels` |

No I/O. Pure computation. This is the most heavily unit-tested component.

---

## 3. Interface

```go
package scheduler

import "time"

// UrgencyCalculator computes tick intervals and urgency scores.
type UrgencyCalculator struct {
    minInterval time.Duration
    maxInterval time.Duration
    numLevels   int
    ratio       float64  // precomputed: maxInterval / minInterval (as seconds)
}

// NewUrgencyCalculator creates a calculator with the given range.
// minI must be less than maxI. numLevels must be >= 2.
func NewUrgencyCalculator(minI, maxI time.Duration, numLevels int) *UrgencyCalculator

// ComputeInterval returns the geometric tick interval for a priority value.
// priority: 1 = fastest (minInterval), numLevels = slowest (maxInterval).
// Accepts `float64` for forward compatibility with fractional priorities;
// currently cast from the database `int` (1-10) at the call site.
// Formula: interval = minInterval * ratio ^ ((priority - 1) / (numLevels - 1))
func (u *UrgencyCalculator) ComputeInterval(priority float64) time.Duration

// ComputeUrgency returns the urgency score for a project.
// Formula: urgency = priority * (1 + elapsed / interval) ^ decayRate
// elapsed = now - lastCompletedAt (or now - createdAt if never completed)
// interval = ComputeInterval(priority)
func (u *UrgencyCalculator) ComputeUrgency(
    priority float64,
    decayRate float64,
    now time.Time,
    lastCompletedAt *time.Time,
    createdAt time.Time,
) float64

// SetRange updates the min/max interval range at runtime.
// Recalculates the internal ratio. Does not change numLevels.
func (u *UrgencyCalculator) SetRange(minI, maxI time.Duration)
```

---

## 4. Behavior

### 4.1 Geometric Interval Formula

```
Given:
  ratio = maxInterval_seconds / minInterval_seconds
  position = (priority - 1) / (numLevels - 1)     // 0.0 to 1.0

  interval_seconds = minInterval_seconds × ratio^position
```

**Step-by-step:**

```
1. Validate: minInterval < maxInterval (else return minInterval, log error)
2. Compute ratio = maxInterval_seconds / minInterval_seconds
3. Clamp priority to [1, numLevels]
4. Compute position = (priority - 1) / (numLevels - 1)
5. Compute multiplier = ratio ^ position
6. Return minInterval * multiplier
```

### 4.2 Example: Default Range (20 min → 24 hours, 10 levels)

```
ratio = 86400 / 1200 = 72.0

P=1:  1200 × 72^(0/9)   = 1200 × 1.000  = 1,200s  (20 min)
P=2:  1200 × 72^(1/9)   = 1200 × 1.609  = 1,931s  (32 min)
P=3:  1200 × 72^(2/9)   = 1200 × 2.588  = 3,106s  (52 min)
P=4:  1200 × 72^(3/9)   = 1200 × 4.163  = 4,996s  (83 min)
P=5:  1200 × 72^(4/9)   = 1200 × 6.698  = 8,038s  (134 min)
P=6:  1200 × 72^(5/9)   = 1200 × 10.776 = 12,931s (215 min)
P=7:  1200 × 72^(6/9)   = 1200 × 17.340 = 20,808s (347 min)
P=8:  1200 × 72^(7/9)   = 1200 × 27.903 = 33,484s (558 min)
P=9:  1200 × 72^(8/9)   = 1200 × 44.900 = 53,880s (898 min)
P=10: 1200 × 72^(9/9)   = 1200 × 72.000 = 86,400s (1440 min = 24h)
```

**Key properties:**
- Priority 1→2: +12 min (big meaningful jump at the fast end)
- Priority 9→10: +9 hours (but both are "roughly once a day")
- The curve expands when maxInterval grows, contracts when it shrinks

### 4.3 Urgency Formula

```
Given:
  interval = ComputeInterval(priority)
  elapsed = now - lastCompletedAt  (in seconds, as float64)
  If never completed: elapsed = now - createdAt

  urgency = priority × (1 + elapsed / interval) ^ decayRate
```

**Step-by-step:**

```
1. Compute interval = ComputeInterval(priority)
2. Determine elapsed:
   a. If lastCompletedAt is not nil: elapsed = now.Sub(*lastCompletedAt).Seconds()
   b. Else: elapsed = now.Sub(createdAt).Seconds()
3. Compute base = 1 + (elapsed / interval)
4. Compute urgency = priority * math.Pow(base, decayRate)
5. Return urgency
```

### 4.4 Urgency Behavior

| Scenario | Priority | Last Completed | Now | Urgency |
|----------|----------|---------------|-----|---------|
| Just ran, should wait | 10 | 2 min ago | now | ~10.1 (barely above priority) |
| Overdue to run | 10 | 1 hour ago | now | ~40 (4× priority) |
| Very overdue | 10 | 8 hours ago | now | ~180 (18× priority) |
| Starved low priority | 1 | 8 hours ago | now | ~8 (8× priority) |
| Never run | 5 | — | now | ~5.1 (slightly above priority) |
| Just created | 5 | — (created 1 min ago) | now | ~5.0 |

**Decay guarantees:** Even a priority-1 project that hasn't run in days will eventually accumulate enough urgency to beat a priority-10 project that just ran. Starvation is mathematically impossible.

---

## 5. Edge Cases

### 5.1 ComputeInterval

| Input | Expected |
|-------|----------|
| `priority = 1` | `minInterval` exactly |
| `priority = numLevels` | `maxInterval` exactly |
| `priority = 0.5` | Clamped to 1 → `minInterval` |
| `priority = numLevels + 5` | Clamped to numLevels → `maxInterval` |
| `priority = 3.7` | Smooth intermediate value between P=3 and P=4 |
| `minInterval = maxInterval` | Returns minInterval (ratio=1, all priorities give same interval) |
| `minInterval > maxInterval` | Logs error, returns minInterval |
| `numLevels = 1` | Position = 0/0 = NaN → handle: return minInterval |

### 5.2 ComputeUrgency

| Input | Expected |
|-------|----------|
| `elapsed = 0` (just completed) | `urgency = priority` (base = 1, 1^decay = 1) |
| `elapsed < 0` (clock skew) | Clamp elapsed to 0 |
| `decayRate = 0` | `urgency = priority` (no decay, constant urgency) |
| `decayRate < 0` | Treat as 0 (negative decay makes no sense) |
| `lastCompletedAt = nil` | Use `createdAt` |
| `createdAt > now` (clock skew) | `elapsed = 0` |
| `interval = 0` (division by zero) | Can't happen: minInterval > 0 enforced by Config.Validate() |

### 5.3 SetRange at Runtime

| Scenario | Behavior |
|----------|----------|
| Range expands (20m→24h to 20m→48h) | All intervals recalculate; low priorities spread apart |
| Range contracts (20m→24h to 20m→12h) | All intervals compress; low priorities cluster tighter |
| New range invalid (min >= max) | Log error, keep old range |
| Called mid-evaluation | Safe — next evaluation cycle uses new range |

---

## 6. States

The urgency calculator is stateless except for `minInterval`, `maxInterval`, `numLevels`, and the precomputed `ratio`. `SetRange` is the only state mutation, and it's called infrequently (only when Bane changes `/fleet range`).

No concurrent access concerns — `SetRange` is only called from the API handler, and the evaluation loop only reads.

---

## 7. Errors

| Condition | Behavior |
|-----------|----------|
| `minInterval >= maxInterval` on `NewUrgencyCalculator` | `log.Printf("WARNING: minInterval >= maxInterval, using minInterval for all priorities")`; set `ratio = 1.0` |
| `numLevels < 2` on `NewUrgencyCalculator` | `log.Printf("WARNING: numLevels < 2, setting to 2")`; set `numLevels = 2` |
| `priority < 0` | Clamp to 1 |
| `elapsed < 0` (clock skew) | Clamp to 0 |
| `decayRate < 0` | Treat as 0 |
| `interval = 0` | Cannot occur — `minInterval` must be positive per config validation |

The calculator **never panics**. All invalid inputs produce a log warning and a safe default.

---

## 8. Testing

### Unit Test Scenarios

```go
func TestComputeInterval_Defaults(t *testing.T) {
    // Setup: min=20m, max=24h, levels=10
    // Priority 1 = 20m ± 1s
    // Priority 10 = 24h ± 1s
    // Priority 5 = ~134 min ± 1s
    // Priority 5 is closer to 2h than to 24h (logarithmic, not linear)
}

func TestComputeInterval_Decimal(t *testing.T) {
    // Priority 3.5 is halfway between 3 (52m) and 4 (83m) on log scale
    // Not halfway in minutes — verify it's the geometric midpoint
}

func TestComputeInterval_CustomRange(t *testing.T) {
    // min=10m, max=12h, levels=20
    // Priority 1 = 10m, Priority 20 = 12h
    // Verify ratio and clamping
}

func TestComputeInterval_EdgeCases(t *testing.T) {
    // Priority 0 → clamped to 1
    // Priority 100 → clamped to numLevels
    // Priority -1 → clamped to 1
    // minInterval = maxInterval → all intervals equal
    // numLevels = 2 → position is either 0 or 1
}

func TestComputeUrgency_JustCompleted(t *testing.T) {
    // Elapsed=0 → urgency = priority
}

func TestComputeUrgency_Overdue(t *testing.T) {
    // Elapsed = 2 × interval → urgency = priority × 2^decayRate
    // Verify with decayRate=1.0, decayRate=1.5, decayRate=0
}

func TestComputeUrgency_NeverRun(t *testing.T) {
    // lastCompletedAt=nil, elapsed from createdAt
    // 1 hour since creation with priority=5, interval=134m
    // urgency = 5 × (1 + 3600/8040)^1.0 = 5 × 1.448 = 7.24
}

func TestComputeUrgency_EdgeCases(t *testing.T) {
    // elapsed < 0 → clamped to 0
    // decayRate < 0 → treated as 0
    // createdAt > now → elapsed = 0
}

func TestSetRange_Expands(t *testing.T) {
    // Change maxInterval from 24h to 48h
    // Priority 10 interval doubles, priority 1 stays same
}

func TestSetRange_Invalid(t *testing.T) {
    // min >= max → log warning, keep old range
}
```

---

## 9. Security

No security concerns — pure computation with no I/O or external input beyond numeric config values. All inputs are validated and clamped.

---

## 10. Performance

| Metric | Target |
|--------|--------|
| `ComputeInterval` | < 1 µs (single math.Pow call) |
| `ComputeUrgency` | < 1 µs (one math.Pow + arithmetic) |
| Batch 33 projects | < 50 µs (33 × ~1.5 µs) |

No allocations on the heap — all return types are primitive (`float64`, `time.Duration`).

### Go Implementation Notes

```go
func (u *UrgencyCalculator) ComputeInterval(priority float64) time.Duration {
    // Clamp priority to [1, numLevels]
    p := priority
    if p < 1 {
        p = 1
    } else if p > float64(u.numLevels) {
        p = float64(u.numLevels)
    }

    // position = (p - 1) / (numLevels - 1)
    position := (p - 1) / float64(u.numLevels-1)

    // multiplier = ratio ^ position
    multiplier := math.Pow(u.ratio, position)

    // interval_seconds = minInterval_seconds * multiplier
    seconds := u.minInterval.Seconds() * multiplier

    return time.Duration(seconds * float64(time.Second))
}

func (u *UrgencyCalculator) ComputeUrgency(
    priority, decayRate float64,
    now time.Time,
    lastCompletedAt *time.Time,
    createdAt time.Time,
) float64 {
    // Clamp decay rate
    d := decayRate
    if d < 0 {
        d = 0
    }

    // Determine elapsed
    var elapsed float64
    if lastCompletedAt != nil {
        elapsed = now.Sub(*lastCompletedAt).Seconds()
    } else {
        elapsed = now.Sub(createdAt).Seconds()
    }
    if elapsed < 0 {
        elapsed = 0
    }

    interval := u.ComputeInterval(priority)
    intervalSeconds := interval.Seconds()

    base := 1.0 + (elapsed / intervalSeconds)

    return priority * math.Pow(base, d)
}
```
