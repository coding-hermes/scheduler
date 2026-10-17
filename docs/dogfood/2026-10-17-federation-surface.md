# Dogfood 2026-10-17 — federation read surface (REMOTE-007..014), live fleet

Target: coding-hermes-scheduler @ v1.12.0-410-g33ceee01 (HEAD eb7f0c94, running
daemon built from this tree, systemd user unit, uptime 42h).

## Why this surface

All previous dogfood runs (08-25 → 09-25) covered the REST control surface,
the dashboard at 389–496 lanes, and the simulator. The REMOTE-00x federation
query family — one contract, four transports (HTTP / bus / MCP / CLI),
deny-all read policy REMOTE-013, aggregate REMOTE-012 — ships 14 complete
board rows and zero dogfood coverage. This run drives it the way a real
fleet operator would.

## The promise

"federation-query-spec.md §1: one scheduler ASKS a peer a question and gets
an answer identical in shape no matter how it travelled (HTTP / bus / MCP /
CLI); reads are scoped, not open — absence of an allow is a REFUSAL, never a
default-yes; every answered query is audited."

## What a user would do, in order

1. Boot the fleet, wonder "can I ask this scheduler for its queue?"
2. Read docs — find the spec at `docs/federation-query-spec.md`, the API
   surface at `docs/api.md`, MCP tools at README — then set credentials and
    call the surface.
3. Publish allow-list entries into the fleet config before any read works.

## What I actually did (evidence: corr_ids in the audit rows)

| Step | Result |
|---|---|
| HTTP `POST /api/v1/federation/query`, valid token, all 6 ops probed | status=error code=op_not_allowed, HTTP 403, envelope 100% §2.2-shape (corr_id/op/peer/status/as_of/age_ms/data/gaps/error/contract) — exact refusal rendered below |
| `strace`/probe of auth ladder | no credential → 401; wrong token → 401; correct token but unpublished op → 403; missing corr_id → 400 with missing_corr_id; unknown op → 400 with unknown_op |
| MCP `fed_query` | status=error code=op_not_allowed, caller identity mcp:auth-token, HTTP 200 (JSON-RPC error envelope — correct per docs/api.md 74-97) |
| MCP `fed_peer_status` | same posture; missing federation_corr_id caught gracefully |
| CLI `scheduler-query karaHermes-mde-7840hs peer.status` | transport_error 401 from crier subscribe when CRIER_AUTH_TOKEN unset (correct: credentials never in argv); op_not_allowed after the env var is sourced |
| Audit | Every read, refused or not, leaves one `api.federation` events row naming outcome/corr_id/caller/op (verified via `GET /api/v1/events?limit=60` — query was rejected as op_not_allowed but the refusal itself appears — good: refusal IS the audited interesting event, per the policy docstring) |
| Replay idempotency | Fixed corr_id re-posted returns the identical envelope — verified on the unknown-op path (dg-rep-1 × 2, byte-identical) |
| Aggregate | `fleet.aggregate` on a real fleet with an EMPTY peer registry → same deny-all 403 posture, envelope-shape identical |
| Config snapshot | GET /api/v1/config renders a 949-byte snapshot with zero mention of federation/scheduler identity/allow entries |

## Verdict

SHIPPABLE on the honesty honesty envelope: every read, refused or answered,
cites a stable error code (op_not_allowed / unknown_op / missing_corr_id) in
the exact §2.2 envelope shape. DOES-NOT-SERVE on operator affordance: the
deny-all posture is invisible in the config surface, the docs never mention
publishing allows until you go grep internal/api/federation_policy.go
yourself, and the caller identity key differs per transport
(operator:token / mcp:auth-token / bus:transport) with no README line
explaining the split. Rows SCHED-GAP-1738..1741 filed.

## What a new user needs that nothing documents

1. Which allow entries to publish for which caller key (`*` vs exact caller
   vs `mcp:auth-token` vs `bus:transport` vs `operator:token`).
2. The "publish before probe" ladder: tokens → allow → read.
3. Nothing in README's Quickstart/MCP discussion mentions any of it.

## What I'd need to finish the operator path honestly

Not mine to fix in this tick (read-only lane): the config surface does not
render the federation layer. This should be surfaced in the resp object of
GET /api/v1/config or the /health payload — a null with a reason beats a
silent omission (Bane's rule).

## Install leg

SKIPPED per the running-daemon rationale documented in DOGFOOD-025: this is
a live production daemon (89217 total ticks, 42h uptime, 10 active),
build-from-source verified 2026-09-25 (63 s) and the install premise has
not changed (build + boot + dashboard all re-verified within the last
30 days). Nothing new would have been learned from an ephemeral re-run.

## Perf (Step 2b)

Overview warm 12–13 ms across 5 sequential runs (no row — nobody feels 12 ms),
one cold-after-60s read at 1.36 s (a floor, not a defect — the 60s snapshot
cache from commit 60d13025 lives as designed). MCP fed tool 4 ms, HTTP fed
19–96 ms. Nothing here slow enough for a user to notice; no PERF row filed.

## Not done (honest gaps)

- Did not file a row for e.g. `want=partial` semantics (contract §2.2)
  because the deny-all posture means no op can be driven to a real
  ok/partial/stale answer in this posture; the conformance battery
  (REMOTE-014 test cells in cmd/scheduler-query) covers that heater locally
  with fixture allow-lists.
- Did not touch the Crier bus transport end-to-end (would require a peer to
  be REGISTERED in the local registry: `GET /api/v1/peers` returned
  count=0 — the probing surface never fires because the peer table is
  empty on this deployment).
