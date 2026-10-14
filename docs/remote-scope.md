# Remote management + federation — scope decision

**Status:** DECIDED — 2026-09-30. **Authority:** owner invoked _bankai_ on `REMOTE-001`; the two
decisions are recorded per `scheduler-remote-federation-proposal.html` §6 (the proposal's
recommendations are accepted).

This is the input the SPEC/BUILD chain (`REMOTE-002`…`REMOTE-014`) is built on. It changes no code.

## 1 · The two decisions

1. **Does the primary ever write a peer's state directly? → No. Never.**
   The primary **commands**; the owning peer writes its own records. It costs one extra network hop per
   change and buys a **single-writer guarantee** across the whole federation — which is what removes the
   need for distributed locks.
2. **Is "primary" a config role or an election? → Config role, not an election.**
   Promotion to primary is a **deliberate one-line config change**. No consensus protocol, no split-brain
   to reason about. Self-election is explicitly out of scope (a different, much larger project).

## 2 · Placement + provisioning path

- A remote scheduler is **provisioned like a dev box, not a task** — the **Bunker** path (spawn with a
  deliberate TTL, provision the toolchain, keep it alive), the same loop already proven against `dedi-2`.
- **Placement, not code:** a second box is a second scheduler pointed at **its own local gateway**. The
  load gate reads *that* box's `/proc/loadavg`, so "remote lanes are not gated by the local box's load"
  is satisfied by placement alone. The per-namespace `load_gate='off'` opt-out (migration v31,
  `internal/scheduler/load_gate.go`) already ships — remote lanes extend it, they do not invent it.
- **Nothing changes for local lanes.** A one-box fleet behaves exactly as today.

## 3 · Design laws (locked)

- **Autonomy is the base state.** Every scheduler is correct and complete while every peer is
  unreachable. Federation adds visibility and command; it must **never** become a dependency of
  scheduling. (This is what turns "what if it can't reach the backend?" from an outage into a non-event.)
- **Single writer per lane.** A lane is owned by exactly one scheduler; the owner is the only writer,
  ever. Ownership — not a broker — keeps writers honest.
- **A silent peer is STALE with a last-contact time — never "down".**
- **No central Redis, no shared database.** Transport = each peer's own HTTP API + the Crier mesh
  (use only what `~/crier/docs/mesh-protocol.md` documents as shipped: envelope, correlation id,
  opt-in auth).
- **Cross-box ordering is per-scheduler + correlation ids** — there is no global log, by design.

## 4 · Out of scope (explicitly)

Global event ordering · shared database / central broker · distributed locks · self-election of primary.

## 5 · Time cost (proposal §3)

Identity + registration + heartbeat ≈ **1 day**; standing up the first remote scheduler behind it ≈
**another day**, mostly provisioning.

## 6 · Next

`REMOTE-002` (SPEC) — scheduler identity, peer registry, the three federation flows, partition
contract. Then the short-term slice `REMOTE-003` (identity + peer registry + heartbeat).
