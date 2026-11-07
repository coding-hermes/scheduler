# Dry-run throttle — five dry runs warn, ten suspend

**Owner directive (2026-11-07):** *"if there are five dry runs then do something ... if it has ten dry runs as in does not come in actual work then we suspend the format for running and we hit it with the cooldown essentially."*

A lane that keeps being spawned and keeps producing nothing is the most expensive shape the fleet has: it burns a session, a model seat and a slot, and it reports success. Before this feature the fleet had a **pacing** answer to that (NOOP-PACING spaces a tasks-admitted lane's re-admission by its cooldown floor, doubling to 8×) and a **re-entry** answer for foremen (the noop_guard tells the session "do work or explain"). Neither one *stops* the lane, and neither applied at all to satellite lanes, whose no-ops are explicitly allowed by class and therefore never counted.

## The measurement

`projects.no_progress_ticks` — consecutive ticks that produced **no artifact of the lane's own work**.

- Progress is a **code commit** or a **DuckBrain memory key** (SCHED-GAP-104). Board bookkeeping is not work: a tick that only rewrote its own `tasks.jsonl` is dry.
- **Except for this throttle, a board commit also counts.** That is deliberately one leg wider than the pacing streak, and the reason is asymmetric risk: a pacing decision only spaces a lane, but a suspension must be liftable by the lane's own output or it is permanent. A docs/PM lane's product *is* the board — judging it dry on code commits alone would suspend the one lane class that never writes code and then never let it back, because the pin could only be cleared by work it is not supposed to do. A formatter/lint lane with nothing to format commits nothing at all, so it is still exactly what this feature targets.
- The commit window is classified from the workdir; when it cannot be measured (no git, exec failure, claimed commits with none in the window) the classification **fails open** — any commit, file delta or memory key counts. A measurement gap must never charge a lane a suspension.
- Since SCHED-GAP-17000 the streak is recorded for **every** non-deferred tick, not only `admission_mode = "tasks"` lanes. A deferred tick (a gateway blip that never ran a turn) never charges the lane.

## The two lines

| Streak | What happens | Event |
|---|---|---|
| **5** (`DryRunWarnStreak`) | Warning, once per crossing. The lane is told, the operator sees it, nothing is stopped yet. | `dry_run_warn` |
| **`dry_run_throttle`** (default **10**) | **Suspended**: `cooldown_pin_s` is set to the lane's `cooldown_ceiling_s` (8× `cooldown_floor_s` when there is no ceiling), `cooldown_pin_by = 'dry_run_throttle'`, `cooldown_pin_at` stamped. The pin outlasts the lane's spawn interval, so it stops being admitted without being disabled. | `dry_run_suspended` |

All events are written under component `noop_guard` — the guard already owns "what did this tick produce", and a second component for the same question would split the operator's query.

## The setting

```toml
[[projects]]
name = "some-docs-lane"
dry_run_throttle = 10    # 0/absent = default 10; anything else must be >= 5
```

- Column: `projects.dry_run_throttle` (migration 86), **NULL/0 = default**.
- Wire: `PUT /api/v1/projects/<name>` accepts `{"dry_run_throttle": N}`; a value below 5 is refused **400** with a message saying why (a suspension that lands before its own warning line teaches an operator that the scheduler is erratic).
- `fleet.toml` pins it only when the key is present, so an API-set value survives a keyless restart (the `noop_allowed` / GatewayKey convention).

## Recovery — the part that makes this safe

The suspension is **self-clearing**. The first tick that produces real work resets the streak and clears the pin, but **only** when `cooldown_pin_by = 'dry_run_throttle'`:

- An **operator pin is never touched** by this feature — not to raise it, not to clear it. If one is in force, a due suspension is *deferred* (logged, no event), because the lane is already slower than the throttle would make it and the operator set that pin for a reason the scheduler cannot see.
- A lane with **no cooldown policy at all** (no floor, no ceiling) is not given an invented one: the suspension is recorded as `dry_run_suspend_unavailable` rather than silently making up a number.

## Why not just disable the lane

Disabling is the same decision with a worse reversibility story: `enabled = 0` outlives the condition that produced it, hides the lane from the surfaces that count work, and needs a human (or a reconcile pass) to undo. A cooldown pin is a *rate*, and the lane's own work is the thing that lifts it — which is exactly what "automated code factory" requires.

## Verification

- `internal/scheduler/dry_run_throttle_test.go` — pure-function tests for the thresholds and the pin arithmetic; DB tests proving that the streak suspends at the limit, that a custom `dry_run_throttle` is honoured, that a warning does **not** pin, that recovery clears **only** this feature's pin, and that an operator's pin survives both the suspension attempt and the recovery.
- Run: `go test ./internal/scheduler/ -run TestDryRun -count=1`
