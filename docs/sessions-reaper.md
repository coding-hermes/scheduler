# Session reaper (SCHED-GAP-089) — real state store, explicit opt-in

Status: seam landed, automated wiring deliberately NOT landed (blocker below).

## History: the fake, and what replaced it

The first SCHED-GAP-089 implementation was fake. Migration v23 created a
scheduler-local `sessions` table inside the scheduler's own database
(`~/.hermes/coding-hermes/scheduler.db`) shaped `id, platform TEXT,
created_at TEXT, updated_at, ended_at TEXT`, and `ReapZombieSessions` closed
rows only in that table. No real Hermes session ever lived there: the fake
could never reap anything real, and anything it did write was its own
fiction. Daemon startup additionally ran a pass automatically on every boot.

Round 2 replaces it with an honest reaper shaped by the REAL Hermes agent
state database (`~/.hermes/state.db`):

```
sessions(id TEXT PK, source TEXT NOT NULL,
         started_at REAL NOT NULL, ended_at REAL,
         end_reason TEXT, last_activity_at REAL, ...)
```

— epoch-seconds REALs (not text timestamps), `source` (not `platform`), no
`created_at`/`updated_at` columns. `internal/agentlog` already reads this
database (read-only) for the dashboard; this reaper mirrors its DSN
discipline with write capability.

What happened to the fake pieces:

| fake piece | disposition |
|---|---|
| migration v23 (fake table + backfill) | Tombstoned: now `DROP TABLE IF EXISTS sessions` with a rewritten ledger desc |
| live DBs that already ran v23 | Healed by migration v44: drops any leftover fake table, rewrites the v23 ledger row |
| `internal/database/sessions.go` reaper | Replaced by `internal/database/hermesstate.go` (real schema) |
| `internal/scheduler/session_reaper.go` | Thin logging seam over the real reaper |
| automatic pass at daemon startup | REMOVED — see blocker; the pass is an explicit operator command |

## The seam

- `database.ReapStaleHermesSessions(ctx, db, cfg)` — one pass over an OPEN
  state.db handle. `cfg.Apply=false` (the zero value) is a DRY-RUN: it
  reports candidates and stalest-idle and provably writes nothing (no UPDATE
  on that path). `cfg.Apply=true` closes exactly the selected rows with
  `ended_at = COALESCE(last_activity_at, started_at)` (the session's own
  last activity, never the reap time) and `end_reason = 'reaped'`.
- Selection: `source='api_server' AND ended_at IS NULL AND
  COALESCE(last_activity_at, started_at) < cutoff` — one predicate shared by
  the dry-run count and the apply update (tested for drift).
- Clock: `clock.WithClock` context injection; threshold via
  `cfg.StaleAfter` (<= 0 → `DefaultHermesReapStaleAfter`, 24h). Idempotent:
  closed rows stop matching `ended_at IS NULL`.
- Fail-closed: any database whose `sessions` table lacks the real columns —
  including the old fake v23 shape and a database with no sessions table at
  all (e.g. the scheduler's own `--db`) — fails with
  `database.ErrHermesSessionsShape`. It never "succeeds" against a wrong
  database and never creates a missing file (`mode=rw`).
- `scheduler.ReapStaleHermesSessions` — the same pass with one log line; the
  reusable programmatic seam.

## Operator usage

```sh
# dry-run: what would be closed? (writes nothing)
schedulerd --reap-sessions

# apply: close them (ended_at + end_reason='reaped')
schedulerd --reap-sessions --reap-sessions-apply

# narrower threshold / alternate path
schedulerd --reap-sessions --session-reap-threshold 72h --session-db /path/to/state.db
```

The one-shot runs before the scheduler database is opened; the reaper never
touches the scheduler's own `--db`.

## Safety model

1. **Dry-run default.** No flag combination writes without an explicit
   `--reap-sessions-apply`; `--reap-sessions-apply` alone refuses to run.
2. **mode=rw, never creates.** A missing state.db is an error, never a fresh
   database. A busy_timeout (5s) queues the pass behind the live agent's
   locks instead of fighting them.
3. **Fail-closed on shape.** `ErrHermesSessionsShape` on anything that is
   not the real schema — the fake v23 shape is rejected, not "reaped".
4. **Conservative closure values.** `ended_at` is the session's own last
   activity (or start), so a reaped row's reported duration stays truthful;
   `end_reason='reaped'` is distinct from the agent's own reasons so reaped
   rows are always auditable and reversible by eye.
5. **Idempotent.** Second pass selects zero rows.

## Blocker: why this is not wired into the daemon

- **No liveness handshake.** The scheduler has no way to distinguish "the
  api_server client is gone" from "the client is idle". Staleness is an
  inactivity heuristic over `last_activity_at`; on the live store 140k+
  api_server rows are currently open with `end_reason` NULL — long-lived
  gateway sessions are NORMAL on this host, so an automated write pass would
  close rows the agent still considers live.
- **The database is the agent's, not the scheduler's.** state.db is the live
  Hermes process's own WAL database (the same one `internal/agentlog`
  refuses to write). Automated cross-process writes from a second daemon are
  a policy decision the scheduler is not positioned to make, especially
  while billing/usage surfaces read those rows.
- Therefore: **the reaper ships as an explicit, operator-invoked one-shot**
  (`--reap-sessions [--reap-sessions-apply]`). No startup hook, no ticker,
  no bespoke cron. If a liveness handshake with the gateway ever exists
  (e.g. the gateway itself reporting last-seen, or the agent exposing a
  close API), the seam is ready: wire `scheduler.ReapStaleHermesSessions`
  behind it and keep the dry-run default.

## Tests

`internal/database/hermesstate_test.go` covers, against temp SQLite files
shaped like the real schema (clock injected, no sleeps): dry-run no-mutation,
apply closes stale api_server only (recent / non-api / already-ended
preserved, started_at fallback), idempotency, threshold + clock injection,
fail-closed on the fake v23 shape and on a sessions-less scheduler DB,
missing-file-never-created, and the v23 tombstone/v44 heal.
