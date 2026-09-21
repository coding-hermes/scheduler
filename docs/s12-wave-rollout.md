# S12 wave rollout plan — steps 1–5 (ADV-R14, G9 residue)

Status and sequence for the S12 concurrent-wave rollout. Steps 1–2 are
landed (code); steps 3–5 were unstarted when this doc was written
(2026-09-21) and get their owner / sequence / trigger conditions here.
Config semantics live in [docs/wave-config.md](wave-config.md).

## Rollout steps

| # | Step | Status | Owner | Sequence / trigger condition |
|---|------|--------|-------|------------------------------|
| 1 | Scheduler-side wave mechanics: namespace wave columns (v27), manifest ingest, `wave_workers_cap` + `WAVE_BUDGET` injection, tick-boundary shed | DONE — SCHED-GAP-109/110/113 (819e0476) | — (landed) | S12 spec §6.2/§15; no operator action |
| 2 | Wave ops hardening: reaper with recover-before-dispatch preamble, per-worker cost attribution | DONE — SCHED-GAP-114 (e5d227a), SCHED-GAP-115 (e1ecdd2) | — (landed) | Depended on step 1 |
| 3 | Durable wave config for the primary lane: wave keys into the `[[namespaces]]` block, live row set via API, two-store parity verified | OPEN | Recommendation: `coding-hermes-scheduler-foreman-ops` lane (foreman tick; config edits are lane-owned). OWNER=unassigned; needs Bane | FIRST of the open steps. Trigger: the drift measured 2026-09-21 — the live DB has `coding-hermes` waving (`wave_enabled=1`, `wave_workers_cap=12`, waves ran through 2026-09-20) while `fleet.toml` carries NO wave keys; wave keys are create-only at import, so a recreated row lands default-off. Sequence: add wave keys to the TOML block → set the live row via `PUT /api/v1/namespaces/{id}` → verify both stores with the read-only SQL probe in docs/wave-config.md |
| 4 | Foreman-side manifest contract adoption: EVERY wave writes `.coding-hermes/waves/<scheduler-tick-id>.json` BEFORE dispatching workers | OPEN | Recommendation: `coding-hermes-foreman` wave section (prompt text) + verification by the `coding-hermes-scheduler-foreman-ops` lane. OWNER=unassigned; needs Bane | After step 3: requires a live namespace with `wave_enabled=true`. Load-bearing: SCHED-GAP-114's recover-before-dispatch preamble reads this manifest to recover interrupted waves — a wave without a manifest is unrecoverable by the reaper |
| 5 | Operational ratification: run the capped namespace live through the 90-day data window; review status surfaces and `tick_workers` telemetry quality | OPEN | Recommendation: `coding-hermes-scheduler-foreman-ops` lane (periodic audit). OWNER=unassigned; needs Bane | After step 4; starts the 90-day window in earnest (durable cap > 0 on a live namespace, not the session-local state of 2026-09-21) |

Sequence is strictly 3 → 4 → 5: config durability before the manifest
contract (step 4 needs a durable `wave_enabled` namespace to act on), and
the manifest contract before ratification (step 5's telemetry assumes
reaper-recoverable waves).

## Deferred decision: per-namespace slot-equivalent ("3-worker wave = 1 slot as policy")

Status: **DEFERRED** (deferred-behind-rows).

Ruling (Option C sub-ruling, G9 / ADV-R14): `wave_workers_cap` is advisory
at the composition layer FOREVER; enforcement of worker concurrency stays
at the tick-boundary shed (`internal/scheduler/wave_budget.go:108` + the
`waveShedSet` path in `internal/scheduler/packer_select.go`). The
slot-equivalent — counting a 3-worker wave as 1 slot against namespace
`max_concurrent` — is capacity-ACCOUNTING policy, not composition advice:
it changes admission semantics, so it needs data, not a config flip.

Trigger conditions (BOTH must fire before this decision is revisited):

1. **90-day data window elapsed** AFTER both (a) this documentation change
   (ADV-R14, landed 2026-09-21) and (b) rollout steps 1–2 complete —
   SCHED-GAP-114 (e5d227a) and SCHED-GAP-115 (e1ecdd2) are both landed, so
   the window is open; earliest revisit ≈ 2026-12-20. Rows 114/115 matter
   because the window's data quality depends on them: 114 keeps
   `worker_count` honest across interrupted-wave recovery, 115 provides
   the per-worker cost attribution any slot-equivalent economics must be
   computed from.
2. **A deployment with `wave_workers_cap > 0` on a live namespace**
   (`wave_enabled=true`). Measured 2026-09-21: condition is already met
   session-locally (`coding-hermes`: enabled=1, cap=12) but NOT durably —
   rollout step 3 is what makes it a deployment. Until step 3 lands, the
   window's "live deployment" evidence is provisional.

Until both conditions fire, any proposal to enforce a slot-equivalent is a
design violation of the Option C ruling (same class as adding an admission
gate outside `SlotPool.spawn` — the G7 ruling). When the triggers fire,
file a board row on this repo; do not implement from this doc alone.

## Measured state at authoring time (2026-09-21)

```sh
sqlite3 "file:$HOME/.hermes/coding-hermes/scheduler.db?mode=ro" \
  "SELECT id, wave_enabled, wave_tick_timeout, wave_workers_cap FROM namespaces ORDER BY id;"
# coding-hermes|1|3h|12   (all other namespaces: 0||0)
grep -c 'wave_' ~/.hermes/fleet.toml || true
# 0 wave config keys in fleet.toml — the DB wave state is session-local
```

The ADV-R14 row premise "no wave_enabled namespaces" was true of the
CONFIG surface (no durable deployment) but not of the live DB row; this
doc records both honestly.
