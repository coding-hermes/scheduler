# SCHED-GAP-1726 — Composable Usage Pools

**Status:** Spec-first design contract (no runtime change in this commit)  
**Scope:** Add dispatch/admission accounting pools orthogonal to the existing namespace weight allocator and namespace concurrency cap.  
**Related authority:** `docs/design-decisions.md` (namespace cap/G7), `specs/S04-weight-packer.md`, `specs/S07-multi-namespace-extension.md`, `docs/dispatch-spec.md` (SCHED-GAP-1710), `docs/remote-scope.md` (REMOTE-001), `internal/scheduler/slot_pool.go`, `internal/scheduler/dispatch_leg.go`.

## 1. Problem statement

The scheduler currently has a process-local global `SlotPool`, namespace-level `max_concurrent` admission, and a remote dispatch path, but no reusable capacity identity independent of namespace membership. Thus a remote lane can occupy a control-box slot despite executing elsewhere, and multiple foreman/satellite namespaces on one bunker cannot share one host capacity ceiling. Add named usage pools so each tick is charged atomically to every applicable resource pool, independent of its namespace and execution location.

## 2. Terms and invariants

- **Usage pool:** A named, typed concurrency resource with a configured positive `active_limit`, an accounting scope, and a stable identity. It counts admitted active ticks/leases; it does not measure weight, USD, worker-wave children, or queued work.
- **Local pool:** Capacity physically consumed on the scheduler/control host. A remote-dispatched tick does not consume this pool.
- **Remote host pool:** Capacity consumed on the execution host. The canonical identity is `host:<stable-host-id>` (also called the bunker pool); it MUST NOT contain namespace, foreman, satellite, lane, or agent identity. All work dispatched to that host, across namespaces and scheduler processes, resolves to the same identity.
- **Project pool:** Optional per-project capacity ceiling, identity `project:<project-name>`; applies in addition to the host and/or local pool.
- **Membership:** A lane may belong to zero or more explicit named pools. It may also receive implicit pools from its execution target (local or `host:<stable-host-id>`) and optional project cap. Duplicate resolved pool IDs are deduplicated before reservation.
- Namespace allocation and `max_concurrent` remain their existing independent scheduling/admission controls. They do not define, alias, or own usage-pool identity. A tick is admitted only if it passes every existing gate and every applicable usage pool.
- A dispatched work item continues to contain lane + board/workdir reference + correlation ID and **never a task ID**. Pool metadata is scheduler-side admission/accounting metadata; it does not alter the work-dispatch payload contract.

## 3. Policy and resolution

1. Each pool has a globally unique `pool_id`, `kind` (`local`, `host`, or `project`), integer `active_limit`, and `enabled` flag. Limits are positive integers. `enabled=false` means not considered for admission; it is not an implicit unlimited cap.
2. The fleet may configure explicit lane memberships in addition to implicit execution-resource membership. Effective membership is the sorted, deduplicated union of explicit memberships and implicit memberships.
3. Local execution receives `local:<authority-id>` (the local control-box pool). Remote execution receives `host:<stable-host-id>` and does **not** receive the control-box local pool. A remote lane may additionally receive `project:<name>` and explicit named pools. There is no cross-pool borrowing.
4. The active limit of the mandatory remote host pool is configurable in the range **8–12 inclusive**, with default **12**. The deployment must choose the host pool's value; each host has one value shared by every lane assigned to it. Values outside the range are rejected at configuration load/admission-authority startup. This bound is specific to the SCHED-GAP-1726 bunker pool, not all arbitrary named pools.
5. Missing or unreadable configuration for a required remote host pool is a loud configuration/admission error. Do not reinterpret it as unlimited, local execution, a namespace pool, or a different host. Remote dispatch must fail closed before handout; emit an observable `usage_pool_missing` reason/event naming lane, target host, and expected pool ID. Preserve dispatch-spec no-fallback behavior.
6. A dispatch target must resolve to a stable host ID before admission. The host ID is provisioned/configured identity (not transient agent alias, hostname lookup result, namespace, or inferred from a workdir). Renaming an agent or moving a lane between namespaces does not change the identity of the same host pool.
7. Namespace `max_concurrent` remains in force for local AND remote lanes as the existing namespace policy unless explicitly disabled by its existing contract; it is an additional gate and does not count as local physical occupancy. (If later evidence establishes that namespace cap is meant to be location-local, that requires a separate ruling.)

## 4. Atomic admission and lifecycle

**Admission point:** `SlotPool.spawn` is the authoritative admission site (existing G7 ruling). Packer/evaluate selection may estimate availability but cannot reserve or substitute for the authoritative gate. Every entry point (normal selection, manual/API spawn, orphan resume, queue replay/continuation) reaches the same admission operation.

**Required contract:** `ReserveAll(ctx, tickID, lane, poolIDs) (lease, decision, error)` atomically checks each pool's active count against its configured limit and either reserves all pools or reserves none. Reservation rows have a unique lease/tick identity and unique `(lease_id,pool_id)` membership. No partial acquisition may be visible after a refusal, cancellation, or error. The operation is serialized at the shared capacity authority, not merely by a process-local mutex. Separate foreman/satellite processes sharing a host pool must not oversubscribe it.

- Validate that every pool exists, is enabled, and has a valid limit before changing any count. Missing required pools fail loudly; invalid pools fail closed.
- Check all pools and create the lease in one atomic authority transaction. On capacity denial, return the blocking pool ID and its `active`, `limit`, and retry/defer state. Do not wait while holding a DB transaction or process mutex.
- After reservation, preserve the existing project dedup reservation. Only then wait for/acquire the local process semaphore if the effective execution is local. A remote tick must not acquire or consume the control-box local semaphore. The bunker/host pool reservation itself is the remote concurrency gate.
- On local semaphore timeout, enqueue/start failure, remote dispatch refusal, cancelled attempt, normal completion, timeout, panic recovery, process shutdown, and any other terminal path, release the complete lease exactly once. Release is idempotent by lease ID; one tick cannot release another tick's reservation.
- Transfer a reservation from `reserved` to `active` without decrement/increment gap when the tick enters running state. Keep it active until lifecycle completion is durably terminal. Remote dispatched work remains active until the dispatch reply completes, expires, or is explicitly cancelled and terminalized under the existing dispatch contract.
- Startup recovery reconciles leases against durable tick state. Terminal ticks release leaked leases; a nonterminal tick with a valid owner retains/reconstructs its lease without double-counting; an ownerless/ambiguous lease is recovered according to one deterministic policy and emits a recovery event. Recovery MUST NOT temporarily permit over-admission. Persist enough lease ownership/expiry data to distinguish a live attempt from a crashed owner.
- If the authority is unavailable, admission fails closed with `usage_pool_authority_unavailable`; it must not run unaccounted. Rollback partial state before returning.

**Concurrency protocol:** all pool checks and lease mutations for one execution host are linearizable at one authority. Every pool in a lane's effective membership (mandatory host/local pool, optional project pool, and explicit pools) MUST be owned by that same authority so `ReserveAll` can be one atomic transaction; cross-authority membership is rejected at configuration validation rather than implemented as a best-effort saga. For host pools shared across processes, every participating scheduler/foreman must use the same host-pool authority. This may be a shared host-local transactional store or an authenticated host-local authority API; it must not require a central cross-federation database, Redis, or distributed lock service. The concrete authority transport is an implementation prerequisite (see §11).

## 5. Defer and error observability

A capacity denial is a **defer**, not a failed tick: keep queued/selected work eligible, consume no cooldown, add no failure count, and release any other provisional resource. Record one structured reason with stable vocabulary:

| Reason | Meaning | Required fields |
|---|---|---|
| `usage_pool_capacity` | A configured pool is full | `tick_id`, `lane`, `pool_id`, `pool_kind`, `active`, `limit`, `waited_ms`, `retryable=true` |
| `usage_pool_missing` | Required host/explicit pool absent | `lane`, `host_id` if known, `pool_id`, `retryable=false`, configuration source |
| `usage_pool_invalid` | Bad ID, disabled required pool, or invalid limit | `lane`, `pool_id`, validation detail; no secret/config contents |
| `usage_pool_authority_unavailable` | Authority cannot atomically admit/release | `lane`, `pool_ids`, error class, retryable flag |
| `usage_pool_recovery` | Startup reconciled or quarantined a lease | `lease_id`, `tick_id`, pool IDs, prior state, action |

Expose per-pool `active`, `limit`, `available`, and deferred count in the existing status/dashboard/operator surface; expose tick-level defer reason through the existing events/tick observability path. Logs must name pool and reason without credentials. A missing pool is not reported as capacity-full.

## 6. Data/config contract (logical model)

Exact migration and Go type choices belong to implementation, but implementation MUST preserve this logical model and constraints:

- `usage_pools(pool_id PRIMARY KEY, kind, active_limit, enabled, authority_id, updated_at)`.
- `usage_pool_memberships(lane PRIMARY KEY component, pool_id REFERENCES usage_pools)` or an equivalent normalized many-to-many relation; duplicate lane/pool pairs are forbidden.
- `usage_pool_leases(lease_id PRIMARY KEY, tick_id, lane, state, owner_id, created_at, updated_at, expires_at)` plus `usage_pool_lease_members(lease_id, pool_id)` with unique pair and foreign keys. Lease ID/tick ID idempotency must prevent duplicate reservations on retry.
- Define and enforce that active usage equals count of nonterminal leases for a pool (or an equivalent transactional counter with reconciliation proof); never maintain an unverified independent counter.
- Stable `host_id` is explicit immutable deployment configuration, and host pool IDs are derived only from its canonical configured value. Configuration reload cannot silently move existing active leases to a new pool.
- Add settings in the existing fleet/root config authority with source precedence consistent with scheduler conventions. Do not assume root TOML `max_concurrent` is live: `docs/design-decisions.md` records that the root `[scheduler] max_concurrent` is not applied today. The new usage-pool keys need explicit wiring and config-parity tests rather than inheriting that stale assumption.

## 7. Compatibility and rollout

1. Add schema/config support in disabled-by-default observe-only mode. With no usage-pool config, existing local and remote behavior remains byte-for-byte unchanged; no capacity is silently inferred beyond the configured target-to-host mapping.
2. Populate explicit pool definitions/memberships and stable host IDs from reviewed config. Verify all remote target rows map to exactly one host identity and the required host pool exists; do not edit live namespace assignments or enablement as part of this feature.
3. Observe-only mode computes and reports would-admit/would-defer decisions but does not gate; compare pool accounting against process/local and namespace occupancy. No dispatch behavior or routing changes in this phase.
4. Enable enforcement for one canary host pool, verify cross-namespace sharing and atomic release/recovery, then expand host-by-host. Keep a documented rollback switch to disable enforcement without deleting definitions or lease history.
5. Rollback stops new pool enforcement, drains/reconciles leases, and leaves existing namespaces, targets, and routing untouched. Never change dispatch target resolution or fall back to the control-box gateway as a pool workaround.

## 8. Acceptance criteria (each has proof)

- **AC-1 — Orthogonal membership:** Given lane A belongs to namespace X and pools P and Q, when another lane in namespace Y requests P, then both are charged to P and namespace membership does not alter pool identity — proof: unit test `TestUsagePoolMembershipAcrossNamespaces` (T).
- **AC-2 — Multi-pool atomicity:** Given one free pool and one full pool, when a lane requiring both is admitted, then neither pool retains a reservation and the defer identifies the full pool — proof: injected-failure and capacity tests `TestReserveAllAtomicNoPartialLease` (T).
- **AC-3 — Remote locality:** Given a remote lane targeting host H, when admitted, then it consumes `host:H` and optional project/explicit pools but does not consume local control-box occupancy — proof: integration test `TestRemoteTickUsesHostPoolNotLocalPool` (T).
- **AC-4 — Shared stable host identity:** Given foreman F and satellite S on the same host H but in different namespaces and separate scheduler processes, when their combined active work reaches the host limit, then the next request defers on the same `host:H` pool — proof: multi-process integration test `TestHostPoolSharedAcrossNamespacesAndProcesses` (T).
- **AC-5 — Configurable host cap:** Given a configured host limit of 8 or 12, then exactly that number can be active; values below 8 or above 12 fail validation — proof: boundary table `TestHostPoolLimitRange` (T).
- **AC-6 — Release on all exits:** Given a reservation, when each terminal path (local acquire timeout, enqueue/start failure, dispatch refusal, cancel, completion, timeout, panic, reaper, restart recovery) occurs, then all lease memberships are released exactly once and capacity becomes available — proof: table test `TestUsagePoolLeaseReleaseAllExits` plus restart test (T).
- **AC-7 — Recovery without over-admission:** Given process crash at each reservation/activation/completion boundary, when authority recovery runs, then active usage matches nonterminal tick ownership and no capacity gap allows limit+1 admission — proof: crash/restart integration matrix (T).
- **AC-8 — Missing remote pool fails loudly:** Given a remote target host with no required host pool, when dispatch admission is attempted, then it creates no dispatch handout, consumes no pool/slot, does not run locally, and emits `usage_pool_missing` — proof: regression `TestRemoteMissingHostPoolFailsClosedNoFallback` (T).
- **AC-9 — Observable deferral:** Given each deny/failure class, when observed via event/status/tick surfaces, then stable reason, pool identity, and active/limit (when applicable) are present — proof: API/dashboard contract tests `TestUsagePoolDeferralSurfaces` (T).
- **AC-10 — Dispatch contract preserved:** Given a pool-admitted remote lane, when handed out, then the payload contains lane/reference/correlation only and no task ID; no usage-pool field changes this — proof: dispatch wire regression `TestDispatchPayloadNeverContainsTaskID` and usage metadata exclusion test (T).
- **AC-11 — Compatibility / rollback:** Given no usage pools or enforcement disabled, when local/remote lanes run, then pre-feature behavior and target routing remain unchanged; observe-only makes no admission mutation — proof: golden compatibility tests `TestUsagePoolsDisabledCompatibility` and `TestUsagePoolsObserveOnlyNoMutation` (T).
- **AC-12 — No live operational mutation:** Given implementation and rollout, when config/schema ships, then no code path automatically edits namespace assignments, enablement, dispatch targets, or routing — proof: migration/config review and byte-diff test on fixture config (T).

## 9. Test matrix

| Layer | Scenario | Expected result |
|---|---|---|
| Unit | Lane in 0, 1, and multiple pools; duplicate membership | Zero is allowed only where no mandatory implicit pool applies; multiple deduplicated; duplicate config rejected or normalized deterministically |
| Unit | Pool full; several pools full | No partial reservation; stable blocking-pool selection (lexicographically smallest full pool ID) |
| Unit | Capacity exactly at limit / one over | Exactly limit admitted, next denied |
| Unit | Host ID whitespace/empty/change | Invalid identity refused; active leases cannot be re-keyed silently |
| Unit | Local vs remote target | Local gets local pool; remote excludes local and gets host pool |
| Unit | Missing/disabled/invalid host pool | Fail closed before handout; stable reason; no local fallback |
| Unit | Repeated reserve/release and duplicate retry | Idempotent by lease/tick key; counts remain exact |
| Unit | Cancellation/error between acquiring different pools | All-or-none rollback |
| Integration | Two goroutines, independent processes, same host pool | No oversubscription under simultaneous admission |
| Integration | Different namespaces, same bunker host | Shared active count and same observable pool ID |
| Integration | Dispatch accepted, reply, timeout, refusal | Lease held until terminal outcome, then fully released |
| Integration | Crash after reserve / after running transition / during release | Restart recovery is idempotent and capacity-safe |
| Integration | Observe-only and enforcement-off rollout | Reporting only; no reservation/gate changes |
| Regression | Namespace cap, process-local local cap, dispatch no-task/no-fallback | Existing independent controls remain intact |
| Surface | Status, event, logs, dashboard | Same stable reason and pool identity; no secrets |

Run Go tests sequentially per repository convention: `go test -short -p 1 ./...`. Live dispatch probes are not acceptance prerequisites for this spec commit and must not be run against the fleet during implementation without separate authorization.

## 10. Edge cases

- Empty explicit membership: still apply mandatory execution-resource pool (local or remote host) when enforcement is enabled.
- Lane assigned to two host IDs: reject before dispatch; never reserve both to conceal target ambiguity.
- Host identity missing for remote dispatch: fail before dispatch with `usage_pool_missing`/invalid-host detail.
- Pool deleted/disabled while leases are active: prohibit destructive removal or limit lowering below current active count; drain or return a validation error. Do not orphan lease rows.
- Limit lowered below current occupancy: retain current leases, deny new admissions until below limit, and report `active > limit`; do not revoke running work.
- Capacity release authority unavailable: retain lease and alert/reconcile; do not pretend it was released. Release retries are idempotent.
- Multiple blocking pools: report the deterministic first blocker and optionally include all blockers in structured details.
- Work item retry with same tick/lease: no duplicate occupancy; retry with a different tick is distinct.
- Host ID rename or host migration: explicit operator migration maps old to new only after drain/reconcile; no automatic aliasing.
- Local semaphore capacity and local usage pool may have the same nominal limit but are distinct enforcement layers during rollout; avoid double-counting them as separate active usage pools.

## 11. Dependencies, non-goals, and unresolved decision

**Dependencies:** existing namespace-cap admission, SlotPool project reservation and namespace pending accounting, SQLite migrations/config loading, dispatch-target resolution and dispatch tick lifecycle, structured events/status/dashboard, and a host-local linearizable authority shared by all participants.

**Non-goals:** changing namespace assignments/caps or weight packing; changing route/dispatch target selection; adding task IDs to dispatch; distributed global ordering, cross-federation shared database, Redis, or distributed lock service; counting worker processes/wave children as ticks; changing USD budgets or cooldown policy; live DB/config/enablement mutation in this spec work.

**Unresolved implementation choice (must be settled before code):** how the bunker host-pool authority is reached by independent scheduler processes. The selected mechanism must be linearizable, durable across restart, local to the host/pool, and compatible with REMOTE-001's no-central-store/autonomy law. Candidate implementations are a shared host-local SQLite authority or an authenticated host-local reservation API. Do not treat a process-local mutex or each scheduler's private SQLite file as sufficient. The spec fixes observable semantics and safety properties; implementation work must record the chosen authority and its failure/recovery protocol in an ADR before enabling enforcement.

## 12. Supersession notes

This contract supersedes only S07/S04 statements that make a namespace the sole kind of resource pool or imply namespace mode is the complete multi-pool admission model. It does not supersede existing namespace weight allocation, borrowing, or `max_concurrent` behavior. The older S07 draft's fallback prose (“all namespaces disabled” or “no namespaces” falls back to flat mode) describes its historical weight-packer proposal and is not permission to bypass a missing required usage pool for a remote lane. `docs/design-decisions.md` remains authoritative for current runtime behavior until implementation lands.
