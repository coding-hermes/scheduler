# Scheduling-Algorithm Integration — design sketch (Bane 2026-09-02)

## Current scheduler algorithm (verified in code 2026-09-02)

- `urgency.go` — exponential urgency: `ComputeUrgency(priority, decay, now,
  lastCompleted, createdAt)` → interval mapping (`ComputeInterval`).
- `packer_select.go` — sort: **urgency DESC → priority → last-tick ASC**
  (older last-tick wins ties).
- `fairness.go` — starvation guard: `isStarving()` + `starvationBoostUrgency`
  (age-based boost inside a starvation window derived from cooldown).
- `multipool_packer.go` — per-namespace budget packing (multi-pool weight
  packing), blackout windows, budget gate, flat fallback.
- `slowdown.go` — failure backoff (`FailureBackoff`), slowdown on consecutive
  failures.

It is a **priority-decay + starvation-boost greedy packer**: good at "nothing
starves", silent about opportunity cost.

## Proposed pluggable strategy interface

The sort happens in ONE place (`packer_select.go` sort + `urgency.go`
score). Extract a `SelectStrategy` interface:

```go
type SelectStrategy interface {
    Name() string
    Score(ctx ScoreContext) float64   // per-project ordering score
    Explain(ctx ScoreContext) string  // one-line WHY for the dashboard/report
}
type ScoreContext struct {
    Project        *database.Project
    NamespaceState NamespaceState   // running, budget, blackout
    History        TickHistory      // completions, failures, durations, cost
    Board          BoardState       // open rows, oldest open age, priorities
    Now            time.Time
}
```

Ship 3 strategies behind it (v1):

1. **current** (default) — priority-decay + starvation boost, as today.
   `Explain()` = "urgency 0.72 (idle 4h2m, priority 3)".
2. **EDF** (earliest-deadline-first) — per-project SLA from fleet.toml
   (`target_interval`); score = minutes overdue. Best when cadence promises
   matter more than priority labels.
3. **lottery** — weighted random by urgency². Preserves urgency ordering in
   expectation but breaks the deterministic head-of-line pattern the PM
   digest keeps hitting; useful as an A/B baseline.

> **DOCTRINE (Bane 2026-09-02):** work is picked by CORRECT ORDERING within
> the project (urgency/priority). Cost NEVER drives work selection — price
> only ranks eligible *models* at spawn time (task-router). A cost-aware
> selection strategy was considered and REJECTED on these grounds.

## Integration points (all exist today)

- `fleet.toml`: per-project `strategy = "edf"` override +
  `[scheduler] default_strategy = "current"`.
- `/api/v1/status`: per-project `strategy`, `score`, `explain` fields.
- Dashboard: strategy badge + the Explain() line (UX: makes the scheduler
  auditable the way the task-router is).
- Watchdog suite: golden-file test — same fixture board, each strategy's
  pick order snapshot; any change to an algorithm shows as a diff.

## Non-goals (v1)

- No ML/learned scheduler (need ≥90 days of clean post-corruption tick data
  first — the zombie sessions and corrupted state.db would poison it).
- No preemption of running ticks (drain-safe restarts first — SCHED-GAP-090).

## Sizing

- Interface extraction + current-strategy parity: **M** (1–2 days, tests).
- EDF strategy + fleet.toml wiring + dashboard explain: **M**.
- Cost-aware + lottery: **S each** once the interface lands.
- Watchdog golden tests: **S**.
