# Harness connector boundary

`internal/connector` is a dark, harness-neutral execution boundary. It is not
wired into the scheduler daemon. The scheduler remains responsible for durable
run, attempt, lease, and outbox records and for selecting the first compatible
connector while preserving Task Router candidate order. Connectors do not call
Task Router, dispatch work through Crier, mutate the board, or decide that a run
has passed independent verification.

Every request, result, and event carries the scheduler's run ID, generation,
attempt key and ID, route digest, workspace lease ID and epoch, connector profile
ID and digest, requested candidate, and (after start) native session and turn
IDs. Requested model/provider identity is immutable routing intent. Actual
model/provider identity is separate native evidence: the Codex adapter first
obtains it from `thread/start` and updates it on a correlated `model/rerouted`
notification.

The lifecycle is `Describe`, `Preflight`, `Start`, `Events`, `Get`,
`WaitSettled`, `Steer`, `Cancel`, `Collect`, and `Dispose`. Optional operations
fail closed unless explicitly advertised. Events are bounded and ordered;
artifacts are out-of-band references with sizes and SHA-256 digests. Disposal
only closes native resources and grants no authority to remove a dirty workspace
or evidence.

Terminal native evidence is deliberately not settlement. Settlement requires a
matching `turn/completed`, followed by `thread/read` reporting runtime thread
status `idle`. Cancellation additionally requires `turn/interrupt`, terminal
`interrupted` evidence, and an injected supervisor's zero-descendant proof.
Neither receipt is durable completion or verification authority.

## Codex app-server adapter

`internal/connector/codexapp` implements the boundary against Codex app-server
JSON-RPC V2 over stdio, one JSON object per line. It is pinned to Codex
`0.152.0`. The wire shapes were derived from the generated schemas supplied at:

```text
/tmp/codex-app-server-schema-0.152.0/
```

In particular, the adapter uses `v1/InitializeParams.json` and
`v1/InitializeResponse.json`, plus the V2 account, model list, thread start/read,
turn start/steer/interrupt, turn completion, thread status, and model reroute
schemas. That directory is a read-only build input/reference and is not copied
into the repository.

The adapter accepts an already-created transport plus supervisor, collector,
clock seams, and the exact assigned connector profile ID and digest. It never
launches Codex itself. Preflight performs a bounded refreshing `account/read`
and bounded paginated `model/list`, then retains the matching ready receipt.
Start consumes that receipt exactly once before wire I/O, validates the Slice 1
execution-brief digest, renders the complete authored brief, and uses the exact
leased workspace as `cwd`, explicit model/provider, `workspace-write`, approval
policy `never`, and disabled/restricted network policy. It validates the native
applied policy before starting a turn and deletes a known partial thread on any
failure. An uncertain start poisons and closes the transport. Raw auth
responses, credentials, environment dumps, and JSON-RPC payloads are never
placed in events or errors.

The JSON-RPC client bounds line length, pending calls, request IDs, and event
buffering. It demultiplexes responses and notifications, rejects unknown or
duplicate response IDs, propagates EOF, respects context cancellation, and
fails the connection on malformed or miscorrelated native lifecycle evidence.
Initialization is serialized to one handshake, and the bounded event channel
has one explicit consumer; a second subscriber is rejected.
