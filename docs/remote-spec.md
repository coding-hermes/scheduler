# Remote management + federation — implementation SPEC (REMOTE-002)

**Status:** SPEC — 2026-09-30. Input: the DECIDED scope in `docs/remote-scope.md` (REMOTE-001). This is a
build contract for `REMOTE-003`…`REMOTE-014`; it changes no code.

The whole design rests on one law: **autonomy is the base state.** Every scheduler is correct and
complete while every peer is unreachable. Federation adds *visibility* and *command*; it is never a
dependency of scheduling.

---

## 1 · Scheduler identity

- New config: `[scheduler] id` (string). Default = host-derived (short hostname), overridable in
  `schedulerd.toml` and by env (`SCHEDULER_ID`). Resolved once at boot and logged (`SCHEDULER: id=<id>`).
- A `scheduler_id TEXT NOT NULL DEFAULT ''` column is added to **`projects`, `ticks`, `events`**
  (migration style mirrors `internal/database/migrations.go`; one migration, backfilled with the local
  id at first boot). Identity must be **stable across restarts** — it is not derived per-tick.
- Every row a scheduler writes carries its `scheduler_id`, so a merged/federated view can always say
  **who wrote this**. This is the join key for the whole federation.

## 2 · Peer registry

| Route | Purpose |
|---|---|
| `POST /api/v1/peers` | register/refresh a peer: `{id, url, version, capabilities}` → 200 (upsert) |
| `GET /api/v1/peers` | list peers with `{id, url, last_contact, stale, version}` |
| `POST /api/v1/peers/{id}/heartbeat` | liveness ping; stamps `last_contact` |

- **Rendering law:** a peer with no heartbeat within the freshness window is **STALE** with a
  `last_contact` timestamp — **never "down"**. `stale` is a boolean; `last_contact` is always present
  (possibly `""` if never seen). There is no "down" state anywhere in the surface.
- Registry is **local, per scheduler** (no shared store). Auth: operator token, fail-closed
  (SCHED-GAP-1602).

## 3 · The three flows

| Flow | Direction | Transport | Why | Failure behaviour |
|---|---|---|---|---|
| **Visibility** — tick started/finished/failed, lane state | peer → primary | **Crier topic** (one publisher per scheduler) | fan-out + durable inboxes already built; correlation ids | publish is best-effort; a failed publish never blocks the tick (autonomy law) |
| **Control** — pause/resume/spawn/update a lane | primary → peer | the peer's **existing HTTP API** (operator token): `PUT /api/v1/projects/{name}`, `POST /api/v1/projects/{name}/spawn\|pause\|resume` | the capability already exists and is authenticated (SCHED-GAP-1602); no new protocol | command fails-closed; the primary logs the correlation id and retries on the next poll |
| **Registration** | peer → primary | HTTP (`POST /api/v1/peers`) | one-time + heartbeat; simple, debuggable | peer retries with backoff; stays autonomous meanwhile |

Crier mesh facts (only what `~/crier/docs/mesh-protocol.md` documents as shipped): envelope,
correlation id, opt-in auth. No feature it marks "not implemented" is used.

## 4 · Ownership law (single writer)

**A lane is owned by exactly one scheduler. The owner is the only writer for that lane's projects,
ticks, and events — ever.** The primary **commands and never writes** a peer's state.

- Enforced at the write path: a scheduler writes only rows whose `scheduler_id` is its own; ingest of a
  foreign `scheduler_id` is read-only (displayed, never mutated).
- This is what removes the distributed-lock machinery a shared store would otherwise force on us.
  Ownership — not a broker — keeps writers honest.

## 5 · Partition contract

- A peer **keeps scheduling** with the primary unreachable. No shared DB, no broker, no coordination.
- **Events spool locally** while the primary is unreachable (reuse the `internal/sync` spooled-outbox
  pattern: append to a local spool, drain on reconnect).
- **Replay is idempotent on `(scheduler_id, event_id)`** — the ingest side drops a duplicate key, so a
  replayed spool can never double-apply.
- Cross-box ordering is **per-scheduler + correlation id**; there is no global order, by design.

## 6 · Explicitly out of scope

Global event ordering · shared database / central broker · distributed locks · self-election of primary
(primary is a **config role**).

## 7 · Task chain

| Row | Implements |
|---|---|
| `REMOTE-003` BUILD | §1 identity + §2 peer registry + heartbeat (the short-term slice) |
| `REMOTE-004` BUILD | §3 visibility flow (Crier publisher/subscriber) |
| `REMOTE-005` BUILD | §3 control flow (primary→peer control client + correlation logging) |
| `REMOTE-006` BUILD | §5 idempotent ingest + spooled replay + the Remote dashboard section |
| `REMOTE-007` SPEC | the READ contract (query surfaces) — the read half |
| `REMOTE-008` BUILD | §7 HTTP `/api/v1/federation/*` read surface |
| `REMOTE-009` BUILD | §7 Crier bus query transport (correlated reply) |
| `REMOTE-010` BUILD | §7 MCP query surface |
| `REMOTE-011` BUILD | §7 CLI `query` verb |
| `REMOTE-012` BUILD | §7 aggregating query (fan-out, merge, degrade) |
| `REMOTE-013` BUILD | §7 read-access policy + cross-box read audit |
| `REMOTE-014` BUILD | §7 surface conformance battery (every surface returns the same envelope/errors) |

## 8 · Open questions / risks

- **Clock skew** on `last_contact` / staleness — decide the freshness window and whether to trust the
  peer's clock or the primary's receive time (recommend: primary's receive time; store both).
- **`event_id` generation** — must be unique per scheduler without coordination; recommend a monotonic
  counter + scheduler_id, not a random uuid, so replay ordering within a peer is stable.
- **Pagination across peers** — the aggregating query (REMOTE-012) must define a per-peer page and a
  merge rule; borrow the contract from REMOTE-007 rather than inventing one.
- **Partial availability** — when some peers are unreachable, the aggregate must return the reachable
  set + explicit STALE entries, never a partial list presented as complete.
