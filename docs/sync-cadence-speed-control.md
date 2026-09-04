# Sync-cadence speed control — design note (Bane 2026-09-04, data from scheduler.db 7d)

## Current state (all numbers re-derived 2026-09-04)

- Sync fleet = 46 enabled projects, ALL at cooldown 21600s (6h) — uniform,
  zero differentiation. 181 sync ticks/day actual.
- Cost per sync tick now ~$0.032 (model fix 09-01 landed: P8_SYNC profile).
  Pre-09-01 they ran $0.18-$0.88/tick (wrong lane) — that class is DEAD.
- Sync lane 7d: $303.50 total, but $257 of it was 08-30/08-31/09-01
  (pre-fix lanes). Post-fix run-rate: ~$5/day. Still 1264 ticks/7d ≈ 656M
  tokens even at the cheap lane.
- Foreman lane 7d: $646.73 — muster alone $310.68 (66 ticks, ~every 69min,
  3.5M tok/tick). muster+temple+eduos = $553 = 57% of fleet spend.
- Priority distribution: 97 projects at p5, 28 at p10, 42 at p4 —
  nearly flat, so urgency barely differentiates; fast projects are fast
  because ComputeInterval(priority=10) is tiny.
- decay_rate=1.0 on every project, never tuned. No auto-tune in kara
  scheduler (that was the eduos cube fork).
- Cadence does NOT adapt to content: heading-sync ticks every ~6h
  regardless of whether the upstream repo moved. Every sync tick pays
  pipeline+bookkeeping even when the repo had zero new commits.
- Idle foremen: 546 foreman-lane ticks/7d changed ZERO files (33M tokens);
  SCHED-GAP-089 was re-attempted 8+ times with no dispatch.

## The gap: no signal-driven slowdown

Urgency only grows with ELAPSED TIME (decay 1.0, never tuned per project).
Nothing tells the scheduler "this project produced nothing for 14 ticks"
or "upstream hasn't moved". Speed is static per project; cost shows up only
in weight/budget gating, never cadence.

## Design: outcome-driven cadence (idle speed control)

Per-project multiplier `pace` recomputed after each tick, multiplying the
effective cooldown:

    effective_cooldown = cooldown_s * pace
    pace = clamp(0.5 .. 8.0)

Signal rules (start conservative, all in scheduler loop post-tick):

1. **Idle stretch**: N consecutive ticks with outcome=dry_run or
   files_changed=0 → pace *= 1.5^N (cap 8). First committed tick resets
   pace=1. SCHED-GAP-089 class: after 4 failed-dispatch ticks the project
   cools to 16x instead of re-burning every 30min.
2. **Upstream quiet** (sync lane): the sync pipeline already fetches;
   have it report upstream_refs_moved=true/false in the tick result. False →
   pace *= 2 (6h→12h→24h→48h, cap 8x). True → pace=1 immediately.
   Cuts ~60-70% of sync ticks: most upstreams don't move 4x/day.
3. **Token ceiling per value**: if avg tokens/tick > 2M and commits/tick
   < 0.5 over a 10-tick window → pace *= 2 (muster/temple class: they'd
   need EITHER cheaper ticks or fewer of them; the data shows 3.5M tok/tick
   is the norm there).
4. **Manual override**: existing cooldown API/MCP stays authoritative;
   pace is a multiplier displayed next to it (dashboard + /api/v1/status),
   reset via POST /api/v1/projects/{name}/pace.

Doctrines honored:
- Work selection stays ORDERING-driven (urgency sort untouched) — pace only
  stretches cooldown (a scheduling METRIC), never reorders work inside the
  project. Cost appears only as a signal INPUT (rule 3), not the ordering key.
- Fail-open: pace computation wrapped, default 1.0 on any error.
- Visible: every pace change → scheduler event + DuckBrain key (auditable).

## Expected impact (from 7d data)

- Sync lane post-fix ~$5/day → ~$1.80/day (65% of ticks find quiet upstreams).
- Idle foreman stretch (rule 1): ~$8-12/7d saved + stops the 8-attempt
  dispatch loops.
- Muster-class rule 3: halving cadence on 3.5M-tok projects saves
  ~$150/7d at current work volumes.
- Total realistic: ~$40-55/week (~35-40% of fleet spend) with zero impact
  on active projects (committed ticks reset pace instantly).

## Sizing

- pace column + loop integration + rules 1&4: M (1-2 days with tests).
- Rule 2 (sync reports upstream_refs_moved): S-M (pipeline already has the
  fetch result; one field through the tick-result contract).
- Dashboard/status exposure: S.
