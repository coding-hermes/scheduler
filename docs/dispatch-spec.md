# Work dispatch — the third bus verb (SCHED-GAP-1665, the dispatch leg)

**Status:** SPEC + IMPLEMENTED — 2026-10-02. The verb and its operator surface ship in
`internal/bus/dispatch.go` and `cmd/scheduler-dispatch`; the lane→target resolution and the
spawn-path wiring are the REMAINING half of `SCHED-GAP-1665` and are explicitly out of scope here
(§7). Cross-references: `docs/remote-spec.md` (federation flows), `docs/remote-scope.md` (the
ownership + autonomy laws), `docs/federation-query-spec.md` §2.5 (correlation ids).

---

## 1 · The gap

The scheduler's bus client speaks two of the three verbs a cluster needs:

| Verb | Meaning | Status |
|---|---|---|
| **Publish** | announce (tick-terminal, lane-state) | REMOTE-004 — best-effort, log-and-drop |
| **Query** | ask one peer a question, wait for the correlated reply | REMOTE-009 |
| **Dispatch** | **hand ONE agent a unit of work** | **this spec** |

Without a dispatch verb the fleet has visibility and introspection but no way to say *"this lane's
work executes on agent X"* — which is the whole point of a fleet whose design is one agent per
project. `SCHED-GAP-1665` measured it: every enabled lane rides one shared gateway on the control
box (`command` and `gateway_key` are empty for all of them).

## 2 · The law this verb inverts (deliberately, and only here)

The autonomy law (`docs/remote-scope.md` §3) says a Crier failure NEVER blocks or fails a tick:
Publish drops an event with a log line and returns nil. **A dispatched JOB is the opposite.** If a
hand-out evaporates, the work simply never happens, and the fleet has already paid three times for
that silent-fallback shape. So:

- a disabled client is an **error** (`ErrDisabled`), not a no-op;
- an unknown target (`404`), a refusal (`4xx`), an unreachable relay or a remote delivery failure
  (`5xx`) each return a **named** error carrying the agent id, the correlation id and the relay's
  own reason;
- **one call is one attempt.** No retry, no alternate transport, and never a silent fall into the
  shared gateway.

The autonomy law protects *scheduling* from a bus outage. A dispatch is what the scheduler decided
to schedule, so its failure must be loud enough to be recorded and retried by the caller.

## 3 · Wire contract

Transport is the relay's **durable per-agent inbox** (`~/crier/docs/openapi.yaml` → `inboxDeliver`),
because an agent that is offline when work is handed out must still receive it when it comes back.

```
POST {crier.url}/agents/{agent_id}/inbox
Authorization: Bearer <crier token>          # opt-in; never in argv (GAP-038)
X-Agent-ID:    scheduler-<scheduler_id>
Content-Type:  application/json

{
  "payload": {
    "kind":         "work.dispatch",
    "lane":         "<lane the scheduler picked>",
    "board":        "<reference the agent resolves>",   // optional if workdir present
    "workdir":      "<reference the agent resolves>",   // optional if board present
    "corr_id":      "<correlation id>",
    "scheduler_id": "<sender identity>",
    "issued_at":    "<RFC3339Nano>"
  },
  "sender":          "scheduler-<scheduler_id>",
  "request_id":      "<correlation id>",
  "idempotency_key": "<correlation id>"
}
```

Accept shapes: `201 {"id","transport":"inbox"}` (durable), `202` webhook/`held`, `200` blocking
webhook. All are receipts; the client returns a `DispatchReceipt` only on `2xx`.

### THE PAYLOAD LAW

The payload carries the **lane**, a **board/workdir reference**, and the **correlation id**. It
never carries a **task id**: *the scheduler picks the LANE, the foreman picks the TASK.* A payload
that named a task would move task selection into the scheduler — the exact boundary this verb
exists to keep.

The correlation id rides three ways in one request — `payload.corr_id` (the job's identity, read by
the agent), `request_id` (the relay's correlation passthrough) and `idempotency_key` (the relay's
dedupe key). A retry of the SAME correlation id after an ambiguous timeout is therefore answered
from the recorded accept: one key, one message, no duplicate job.

## 4 · Failure vocabulary

| Sentinel | Trigger | Caller's move |
|---|---|---|
| `ErrDispatchTargetUnknown` | relay `404` (no such agent) | fix the target; retrying unchanged is pointless |
| `ErrDispatchRefused` | other `4xx` (bad request, guard block, namespace mismatch, over-quota) | the job is NOT queued — surface it |
| `ErrDispatchUnreachable` | dial/timeout, or remote delivery failure (`5xx`) | retryable; the idempotency key makes it safe |
| `ErrDispatchFailed` | any other non-accept | unknown shape, surfaced rather than guessed at |
| `ErrDisabled` | the bus client is disabled | no attempt was made — degrade deliberately |

A failure returns the **zero** receipt plus the error, so a caller can never mistake a failed
hand-out for a queued job.

## 5 · Operator surface

```
scheduler-dispatch <agent-id> --lane <lane> (--board <ref> | --workdir <ref>) [--corr-id <id>]
                   [--url <relay>] [--scheduler-id <id>] [--dry-run] [--json]
```

Exit codes: `0` accepted · `1` not accepted (refused / unknown / unreachable) · `2` usage.
`--dry-run` prints the endpoint and the exact request body (byte-identical to the wire body)
without opening a connection — evidence before action. The credential is read from
`CRIER_AUTH_TOKEN`; there is deliberately no `--token` flag.

## 6 · Proof

Live, against the running fleet relay (registers a throwaway agent, dispatches, re-dispatches the
same correlation id, reads the bytes back out of that agent's own inbox, then unregisters it):

```sh
CRIER_LIVE_URL=http://127.0.0.1:8767 CRIER_LIVE_TOKEN=<relay bearer> \
  go test ./internal/bus/ -run TestLiveDispatch -v -count=1
```

Hermetic unit proof (`go test ./internal/bus/`) pins the wire shape, the no-task-member law, the
refusal paths, the loud-failure paths and the retry idempotency without a socket.

## 7 · The remaining half of SCHED-GAP-1665 — IMPLEMENTED (SCHED-GAP-1710)

The three items below were out of scope when the verb shipped. They are now
implemented from the TICK PATH (SCHED-GAP-1710), and this section states where
each lives:

- **Lane → target resolution.** A lane declares its execution target in
  `dispatch-targets.jsonl` (`$SCHEDULER_DISPATCH_TARGETS`, default
  `<home>/.hermes/coding-hermes/dispatch-targets.jsonl`):
  `{"lane":"helix","agent":"helix","board":"…","workdir":"…"}`. It is a LANE
  property, not a router decision — per-project pins outrank the router, so a
  target chosen by the router could contradict them. A lane with no row (and a
  missing file) keeps today's behaviour exactly: the change is additive. A row
  with `enabled=false` parks a target; a row with no agent, or a second row that
  re-targets a lane, is REFUSED on read and reported (never silently dropped).
- **Spawn-path wiring.** `internal/scheduler/dispatch_leg.go` runs the hand-out
  where the tick is spawned (`SlotPool.spawn`): the receipt is written to
  `tick_dispatch` (migration 62) with the agent the relay ACCEPTED (never the
  configured one), the correlation id, the relay message id and the transport.
  An unreachable, unknown or refusing target **fails the tick** with the named
  error and the attempt recorded — there is deliberately no fallback to the
  shared gateway anywhere in the path.
- **Reply ingestion.** The scheduler holds its own bus identity
  (`CRIER_AGENT_ID` + `CRIER_AGENT_KEY_FILE`, PKCS#8 PEM) and polls its durable
  relay inbox (`internal/bus/inbox.go`: signed retrieve/ack). A message is an
  answer **only** if it names a hand-out this scheduler made — the relay message
  id it replies to (`in_reply_to`) or the correlation id it echoes (including
  inside the original payload the fleet dispatcher hands back as `task`).
  Anything else is left strictly alone: not acked, never allowed to close a
  tick. A remote lane with no reply identity REFUSES to dispatch at all, because
  a hand-out whose answer can never be correlated is the silent-failure shape
  this verb exists to remove.

Hermetic proof: `internal/scheduler/dispatch_leg_test.go` (ready → hand-out →
agent answers → tick completes with no request ever reaching the gateway;
unknown target → tick fails loudly; no identity → nothing is sent) and
`internal/scheduler/dispatch_targets_test.go`. Live proof:

```sh
CRIER_LIVE_URL=http://127.0.0.1:8767 CRIER_LIVE_TOKEN=<relay bearer> \
  go test ./internal/scheduler/ -run TestLiveDispatchLeg -v -count=1
```

which registers two throwaway agents on a running relay, hands one a unit of
work, reads it back out of that agent's own inbox, has it answer into the
scheduler's inbox and asserts the SHIPPED receive leg correlates the answer back
to the hand-out and releases the lease by ack.
