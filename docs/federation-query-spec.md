# Federation query + access contract — SPEC (REMOTE-007)

**Status:** SPEC — 2026-10-01. Builds on `docs/remote-scope.md` (REMOTE-001) and `docs/remote-spec.md`
(REMOTE-002..006). This is the design authority for REMOTE-008..014: the *query* half of federation —
how one scheduler reads state that another scheduler owns — expressed ONCE and carried by several
transports. Control (writing/commanding a peer) is the other half and is already specified in
`remote-spec.md` §3; this document does not change it.

## 1. Purpose

The primary must be able to ASK a peer a question ("what is your queue", "what fired in the last
hour", "which lanes are starved") and get an answer **identical in shape no matter how the question
travelled** — HTTP, the Crier bus, MCP, or the CLI. Transports are conveniences; the contract is the
product. Get the contract wrong once and every surface drifts; get it right and each transport is a
thin adapter (REMOTE-008/009/010/011), the aggregate is a fan-out (REMOTE-012), and the battery is a
loop (REMOTE-014).

## 2. The one contract (transport-agnostic)

### 2.1 Query envelope

A query is a single JSON object. Every transport carries EXACTLY this object, verbatim:

```
{
  "op":        "<catalogue op id>",      // §2.3
  "args":      { ... },                  // op-specific, all optional unless the op says otherwise
  "corr_id":   "<string>",               // caller-assigned; echoed in the reply (audit + dedupe)
  "budget_ms": <int>,                    // per-peer wall budget; the peer clamps to its own cap
  "want":      "answer" | "partial"      // "answer" = all-or-error; "partial" = best-effort with gaps
}
```

`op` is REQUIRED. `corr_id` is REQUIRED and MUST be unique per (caller, minute); it is the idempotency
key (a replayed query with the same `corr_id` returns the first answer, §2.5). Absent `budget_ms`
means "peer default"; absent `want` means `"answer"`.

### 2.2 Response envelope

```
{
  "corr_id":   "<echoed>",
  "op":        "<echoed>",
  "peer":      "<answering scheduler_id>",
  "status":    "ok" | "partial" | "error" | "stale",
  "as_of":     "<RFC3339 UTC>",          // when the ANSWERING peer read its state
  "age_ms":    <int>,                    // now - as_of at answer time (the freshness the caller sees)
  "data":      { ... },                  // op-specific; present for ok/partial
  "gaps":      [ { "what": "...", "why": "..." } ],  // present for partial; empty array otherwise
  "error":     { "code": "<stable>", "message": "<human>" },  // present only for status=error
  "contract":  "<contract-version>"      // §2.6
}
```

Invariants every transport MUST preserve:
- `peer` names the scheduler that OWNS the data (spec §4 single-writer — the answer always comes from
  the owner, never a cache of a peer).
- `as_of`/`age_ms` describe the DATA, not the transport. A cached read carries its own `as_of`.
- `status="stale"` is a first-class answer (the peer answered, but its own last self-observation is
  older than its freshness window) — never conflated with `error`.
- `data` is `[]`/`{}` (never `null`) when the answer is empty — the fleet-wide null-with-a-reason rule.

### 2.3 Read catalogue (the ops)

The catalogue is deliberately SMALL at v1; new ops are additive and each names its owner:

| op | args | data | owner row |
|---|---|---|---|
| `peer.status` | — | identity, version, clock, load, uptime | REMOTE-008 |
| `fleet.status` | — | peers, projects, active ticks, budget | REMOTE-008 |
| `projects.list` | `filter` | project rows (name, enabled, weight, priority, cooldown) | REMOTE-008 |
| `queue.get` | — | the peer's ordered scheduling queue | REMOTE-008 |
| `ticks.list` | `since`, `limit` | tick rows | REMOTE-008 |
| `events.list` | `since`, `limit`, `severity` | event rows | REMOTE-008 |

Each transports an answer the peer ALREADY computes for its own dashboard/REST — a federation query
MUST NOT invent a new aggregation the peer cannot answer from its own read path.

### 2.4 Ordering + pagination

Where an op returns a list, the order is the op's (documented, stable) and pagination is a cursor
(`next` token), never an offset — offsets double-count across concurrent writes.

### 2.5 Idempotency + replay

`(caller_scheduler_id, corr_id, op)` is the identity of a query. A peer keeps a short replay window
(default 5 min) mapping that key → the reply it already produced, so a retried query (a reconnect, a
bus redelivery) returns the identical answer instead of re-reading or double-counting. This mirrors the
ingest idempotency of `remote-spec.md` §5; the same "duplicate is a drop, not an error" law applies.

### 2.6 Contract version

`contract` is a semver-ish string. A transport that does not understand a peer's contract version MUST
degrade to `status="partial"` with a named gap — never guess. Breaking a response shape is a contract
major bump; adding an op is a minor bump.

## 3. The four transports (thin adapters, ONE contract)

| transport | shape | row |
|---|---|---|
| HTTP | `POST /api/v1/federation/query` (body = the envelope), plus `GET /api/v1/federation/catalogue` | REMOTE-008 |
| Crier bus | request on `fed.query.<peer>` with a correlated reply on `fed.reply.<corr_id>` | REMOTE-009 |
| MCP | one tool per catalogue op (e.g. `fed_queue_get`) + `fed_query` as the generic escape hatch | REMOTE-010 |
| CLI | `scheduler query <peer> <op> [--args k=v] [--want partial]` | REMOTE-011 |

Every adapter: parses its surface → builds the envelope → calls the SAME internal query entry point →
returns the SAME response. An adapter that computes an answer itself is a bug (REMOTE-014 catches it).

## 4. Access model (REMOTE-013)

- **Read is scoped, not open.** A peer answers only ops its operator has published (`[federation]
  allow = ["fleet.status","queue.get", ...]`); anything else is `status="error" code="op_not_allowed"`
  — a named refusal, never an empty answer (an empty answer lies).
- **Per-peer scoping.** The primary may ask peer A more than peer B; the allow-list is per peer, keyed
  by the peer's identity from the registry (REMOTE-003).
- **Auth.** Every transport fails closed when no operator credential is configured (the REMOTE-003
  posture). The bus carries the same opt-in bearer; MCP inherits the server's operator token.
- **Cross-box read audit.** Every answered query writes one local audit row: `{corr_id, caller, op,
  status, age_ms, ts}`. A query the peer REFUSED is audited too (the refusal is the interesting event).
  Reads are observable, not silently free.

## 5. Aggregation (REMOTE-012)

The primary's aggregate query fans out the SAME envelope to N peers with a per-peer `budget_ms`, then
merges: `ok` blocks merge; any `partial`/`error`/timeout becomes a named `gap` for that peer and the
overall `status` drops to `partial` (the caller is never told a federated answer is complete when a
peer went silent). Per-peer degradation is explicit: the aggregate lists every peer with its own
status — a peer that timed out is `status="error" code="timeout"`, not an omitted row.

## 6. Conformance battery (REMOTE-014)

For a fixed fixture board + clock, EVERY transport must return a byte-identical response for the same
op/args (modulo the transport-specific `peer`/timing fields, normalized). The battery runs each op over
each transport and diffs against the internal entry point's answer — the internal call is the oracle.
A transport that drifts fails CI. This is the CTL-003 doctrine (every surface mirrors one contract)
applied to the read path.

## 7. Non-goals

- No cross-peer WRITES (control is `remote-spec.md` §3; queries are read-only by construction).
- No peer-to-peer query fan-out (only the primary aggregates; a peer answers about itself).
- No distributed cache of a peer's state (single-writer, `remote-scope.md` — the primary may cache an
  ANSWER, never the peer's state as if it owned it).
- No new aggregation the answering peer cannot compute from its own read path.

## 8. Open questions (resolve in the build rows, not here)

1. Does `fleet.status` on a peer include the peer's OWN peers (transitive), or only itself? (default:
   only itself + its registry, one hop)
2. Replay window default (5 min) and whether it is configurable per peer.
3. Whether the aggregate `partial` status should carry a `complete=false` boolean for cheap consumers.
