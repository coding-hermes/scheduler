# Durable coding execution store

SCHED-HARNESS-002B adds dark SQLite tables for architecture revision 2.1's
coding-run control plane. They are migrated on ordinary database creation, but
no scheduler loop, API, Crier sender, connector, or board writer uses them yet.

## Authority boundaries

`execution_runs` is the scheduler-owned logical run. Submission idempotency is
scoped by `(submitting_authority, submission_idempotency_key)`; an identical
brief digest replays the row and a different digest conflicts. Its closed state
vocabulary includes `connector_completed`, `verifying`, and `completed` as
separate states. `verification_state` is independent. A connector event cannot
write `completed`, verification state, projection success, or board state.

`execution_attempts` contains generated `(run_id, generation)` values, a unique
attempt idempotency key, requested/selected/served route identities, connector
identity, exact lease epoch, and native process incarnation (`pid`, `pgid`,
container/cgroup ID, and process start time). Attempt admission and dispatch
outbox insertion are one SQLite write transaction.

`execution_events` is append-only by `(attempt_id, sequence)`. The row records
whether current-generation fencing accepted it. Late and wrong-generation
events remain durable with a rejection reason, but cannot mutate attempt or
parent state. A Crier acknowledgement is transport evidence only and always
has no execution-completion authority.

`workspace_leases` identifies the repository, base revision, branch, host, and
path exactly. Epoch acquisition is compare-and-set and the partial unique index
permits only one active writer for an identity. Renewal and revocation require
the active epoch; a replacement writer requires a later epoch after revocation
or release.

`verification_receipts` is keyed by run, attempt, criterion, and commit. It
stores deterministic verifier inputs and results, trusted environment,
artifact digest, and verifier identity. Connector claims are not receipts.

`execution_projections` separately journals task-router outcome, DuckBrain,
board, and notification work. Idempotency-key and payload-digest equality make
replay safe. A failed projection is retried without creating a coding attempt.

`execution_outbox` holds only `execution.dispatch` commands. Admission persists
the command before network I/O. Claim, acknowledgement, and retry are durable;
the idempotency key remains constant across ambiguous delivery retries. An
outbox acknowledgement cannot change execution or verification state.

## Recovery

The repository recovery query returns all unsettled parent runs, every active
workspace lease, and pending or claimed outbox rows. Claimed rows are returned
for reconciliation rather than silently replaced. Recovery does not create a
new session or generation.

## Activation status

These tables are intentionally dark. This slice does not dispatch a command,
activate a connector, expose an API, close a board row, or operate on a live
scheduler database.
