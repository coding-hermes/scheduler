# Tick-to-Hermes-session attribution

`ticks.session_id` is a legacy scheduler field, not a Hermes session key. It may contain a gateway response ID (`resp_…`), a tick placeholder, or no value. Do not join it to `sessions.id`.

Each scheduler gateway request sends the tick ID as `X-Hermes-Session-Key`. Hermes persists that value as `sessions.session_key`, while `sessions.id` is its own UUID. Therefore the durable linkage is:

`scheduler.db.ticks.id = state.db.sessions.session_key`

The databases are separate SQLite files. The direct join below is the canonical relationship; when querying separate databases, attach `state.db` and qualify `sessions` as `hermes_state.sessions`:

```sql
SELECT t.*, s.*
FROM ticks t
LEFT JOIN sessions s ON s.session_key = t.id;
```

To run it with SQLite across the separate files:

```sql
ATTACH DATABASE '/path/to/state.db' AS hermes_state;

SELECT t.*, s.*
FROM main.ticks AS t
LEFT JOIN hermes_state.sessions AS s ON s.session_key = t.id;
```

For attribution scoped to a project/lane (the scheduler's project name is the lane name), filter the same join:

```sql
ATTACH DATABASE '/path/to/state.db' AS hermes_state;

SELECT t.id AS tick_id, t.project_name, t.spawned_at, t.completed_at,
       s.id AS hermes_session_id, s.source, s.cwd, s.session_key
FROM main.ticks AS t
LEFT JOIN hermes_state.sessions AS s ON s.session_key = t.id
WHERE t.project_name = 'warpfs'
ORDER BY t.spawned_at DESC;
```

Measured against the live scheduler database on 2026-11-05 (~150k ticks), `ticks.session_id` contained 61,115 NULLs, 5,821 literal `gateway` values, 215 UUIDs, 145 empty strings, and otherwise mostly `resp_…` gateway response IDs plus a handful of tick-ID placeholders. Joining `ticks.session_id = sessions.id` resolved only 9 of 998 warpfs ticks. The canonical key join resolved 682 of 4,019 warpfs ticks all-time and 360 of 360 in the last 30 days; all matched sessions had `source='api_server'` and `cwd IS NULL`.

This is an exact-key join, not content/time-window inference. `NULL` session columns mean no matching Hermes session row exists in the attached state database; do not fall back to `ticks.session_id`. Ticks that did not reach the gateway (for example, queued or refused spawns) naturally have no corresponding session. Use the Hermes state database for the same account/gateway that received the request.

This is the linkage key sent by the scheduler in `internal/scheduler/gateway_stream.go` and the non-streaming gateway client. The response/envelope IDs captured in `ticks.session_id` and `gateway_trace.session_id` are separate identifiers and remain available for their legacy uses.
