# Wake tables — run a lane at a future time or on an external event

**Owner directive (2026-11-07):** *"we are going to need support for wake tables — if i say to have a set time or event in the future for waking up and running a lane they can be programmed via api etc."*

A wake is a persisted promise in the scheduler DB (`wakes` table, migration 88): **run this lane at that time, daily at that time, or when an external system says so.** Rows are never deleted — `fired` / `cancelled` / `skipped` are states, and the table is the audit of who asked for what and what came of it.

## The API

```bash
# One-shot: run the release lane tonight at 2am (local time in the body)
curl -X POST http://localhost:9090/api/v1/wakes -d '{
  "project": "myproj-releng",
  "at": "2026-11-09T02:00:00-05:00",
  "note": "cut the release after the freeze"
}'

# Recurring: every day at 02:00 local (first occurrence is auto-computed)
curl -X POST http://localhost:9090/api/v1/wakes -d '{
  "project": "myproj-sync", "daily": "02:00"
}'

# External: fired by ANOTHER system when its own event happens
curl -X POST http://localhost:9090/api/v1/wakes -d '{
  "project": "myproj-qa", "external": true, "payload": "deploy finished"
}'

# ...and that other system fires it when ready:
curl -X POST http://localhost:9090/api/v1/wakes/<id>/fire

# Lists (filter by state), inspect, cancel
curl 'http://localhost:9090/api/v1/wakes?state=pending'
curl http://localhost:9090/api/v1/wakes/<id>
curl -X DELETE http://localhost:9090/api/v1/wakes/<id>   # state -> cancelled, never deleted
```

Exactly **one** scheduling clause per wake (`at` | `in_minutes` | `daily` | `external`) — a body with two is a 400, not a guess. A past `at` is a 400: a wake that fires immediately is a spawn request, use `/api/v1/projects/<name>/spawn`. A wake for a disabled lane is a 409 — a promise the runner would have to skip.

## What a fired wake actually does

1. **Claim** — the runner claims the row (`pending → firing`) with a conditional UPDATE, so the runner and an API `/fire` can never both fire the same wake. Once-only is structural, not hopeful.
2. **Spawn** — through the **operator path**: the manual bypass (e), the one always-granted, always-logged bypass. The wake is a pre-authorized operator instruction, so cooldowns don't block it — but it never *wears* the manual name: the tick row records `nudge_source = wake_table`, and the log names the wake id. "Who spawned this at 2am?" answers with the wake, not a generic manual.
3. **Observe** — a spawn that returned a tick id is not yet a kept promise: the tick row's terminal status lands moments later, and under `--no-exec-fallback` every gateway outage stamps it `deferred` almost immediately. The runner reads the tick row (bounded: one read + up to three re-reads ~300 ms apart) before settling.
4. **Settle** — `fired` with the produced `fired_tick_id` (dailies re-arm to their next occurrence instead) **only when the observed tick verdict is completed — or still queued/running after the window**. A tick that came back `deferred` / `failed` / `timeout` / `aborted` does NOT settle the wake: it stays pending on the SAME occurrence with a 5-minute retry backoff, `fired_at`/`fired_tick_id` stay NULL, and a MEDIUM `wake_spawn_not_fired` event names the tick's reason (throttled to one per 30 min). Without this, a gateway outage produced an audit trail that said "fired" while a daily silently lost its day. Every outcome is on the row.

## The refusals that matter

| Condition | Behaviour |
|---|---|
| **Blackout window** | The wake **stays pending** and retries (pushed 5 minutes out per attempt, events throttled to one per 30 min). Operator time-of-day intent defers — it does not override the fleet's do-not-spawn windows. |
| **Lane busy / spawn refused** (`ErrProjectRunning` dedup) | The wake **stays pending** and retries, pushed 5 minutes out per attempt (same deferral as the blackout path). The lane is mid-tick, so hammering the gate every poll buys nothing; the skip/fired event pair is throttled to one per 30 min. |
| **Spawned tick comes back not-fired** (`deferred` / `failed` / `timeout` / `aborted`) | The wake **stays pending on the same occurrence** and retries 5 minutes out (REV-004). The spawned tick is observed before settlement; a not-fired verdict keeps `fired_at`/`fired_tick_id` NULL, emits a MEDIUM `wake_spawn_not_fired` event naming the tick's reason, and never re-arms a daily to its next occurrence — the occurrence is retried, not consumed. |

A wake whose lane is disabled or unreadable also settles `skipped` with an event: a wake that dies silently is a lie the operator eventually pays for.

## What a wake is NOT

- **Not a cron replacement.** Lanes already tick on their configured cadence; a wake is for "this lane, at this time, once (or daily)" — release cuts, post-deploy verification, follow-ups.
- **Not a cooldown override for the general case.** It uses the operator path because that is what it is — the operator's own pre-authorized spawn. It cannot park-flip a lane, cannot bypass a blackout, and cannot double-spawn a mid-tick lane (the SCHED-GAP-030 dedup applies).
- **Not config.** Wake rows live in `scheduler.db` (runtime scheduling state, the `ticks` class), not in git-backed JSONL.

## Verification

- `internal/scheduler/wake_table_test.go` — due fires exactly once; cancelled never fires; daily re-arms forward (never into the past); external wakes never fire from the scheduled pass; a refused external fire leaves the schedule standing; busy → pending retry +5m (throttled); blackout → re-pend with a pushed retry; spawn-hook errors settle skipped; a spawned tick that reads back deferred/failed keeps the occurrence pending with a 5-minute retry, NULL fire stamps and no `spawned=true` event (MEDIUM event names the reason); a still-running tick settles as fired today; validation table.
- `internal/api/wakes_api_test.go` — real-handler round-trips: create/list/cancel/fire over a real SQLite, the exactly-one-clause law, past-`at` 400, unknown-lane 404, unwired-runner 503, wired-fire 202 then 409.
- Run: `go test ./internal/scheduler/ -run TestWake -count=1` and `go test ./internal/api/ -run TestWake -count=1`
