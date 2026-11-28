# SCHED-GAP-17084 — independent group destination re-audit + E2E proof

Row: **SCHED-GAP-17084** ("Re-audit group Telegram destination alignment end to end"),
depends on SCHED-GAP-17083 (complete, product commit `4f74d276`).
Worktree: `/home/kara/worktrees/coding-hermes-scheduler-SCHED-GAP-17084`, branch `wt/SCHED-GAP-17084`,
base `origin/main @ 79b488cf`.
Author of this evidence: coding-hermes-worker (this tick).

**Sanitization.** No destination value appears in this file. Every distinct
`projects.deliver` value is reduced to `T<n>/<sha256:8>`; the reference and the
fingerprint are derived by `cmd/destaudit` and are stable across runs, so a
reader can compare two audits without either one publishing a chat id, thread id
or token. The live stores were only ever read (`mode=ro`); nothing was written to
`~/.hermes`, no daemon endpoint was called, no Telegram message was sent, and the
large DB snapshots used for the drift rehearsal were deleted afterwards
(scratch left at 472 KB). The only telegram-shaped strings quoted below are the
synthetic test fixtures (`-100777`), which share no value with the fleet.

---

## 1. What this row changed (and what it deliberately did not)

The audit's central finding is that SCHED-GAP-17083 landed the *policy store and
preview* but nothing consumed it: `internal/scheduler` delivered to whatever
`projects.deliver` said, so a lane whose group carried an approved mapping could
post into a different thread forever. `docs/api.md` said so literally ("This
policy does not change project destinations or delivery"). An "alignment verdict"
against an unenforced mapping would have been a report, not a proof, and the
row's own reasoning names the missing piece: *"SCHED-GAP-088 addressed a
historical operational mapping, not ongoing enforcement."*

Arms this audit added (minimum needed to make the required E2E assertions
meaningful; nothing else):

| arm | file | why it had to exist |
| --- | --- | --- |
| delivery-time decision | `internal/blocks/destination_effective.go` (`DecideDelivery`) | single-project sibling of `InventoryDestinations`, same status vocabulary, plus the effective target delivery must use (out of band, never in JSON) |
| delivery guard + inheritance | `internal/scheduler/deliver_destination_guard.go`, wired in `deliver.go` (`deliverOutputWithMode`, `deliverAlertWith`) | the one choke point every tick report and timeout alert already passes |
| daemon wiring | `cmd/schedulerd/main.go` | the guard reads the same `groups.jsonl` the deploy-block API writes |
| overwrite alert | `internal/api/destination_overwrite.go`, `internal/api/block_handlers.go` | rewriting an approval re-routes every lane it owns; the PUT stays permitted, the change is named |
| read-only fleet audit | `cmd/destaudit/` | reproducible, sanitized inventory for the whole fleet |
| docs | `docs/api.md` | the previous sentence was now false; the enforcement table replaces it |

**Not implemented here** (recorded as findings, per the brief — not silently
built): persisted per-project approval/change records; an apply verb; an API-level
rollback/clear verb; write protection; a sync-path writer for `groups.jsonl`.

---

## 2. Independent inventory (live fleet, read-only)

Command (exact):

```
go run ./cmd/destaudit --db ~/.hermes/coding-hermes/scheduler.db \
                        --groups ~/.hermes/coding-hermes/groups.jsonl
```

Result: `exit 0`.

| fact | value |
| --- | --- |
| enabled lanes audited | **321** |
| namespaces represented | **12** |
| deploy groups in the store | **1** (`pilot`, 1 member) |
| groups with an APPROVED destination policy | **0** |
| groups with an unapproved policy | **0** |
| distinct destination values in use | **23** |
| lanes with NO destination at all | **6** |

Status counts from the delivery decision (the same function the guard runs):

| status | lanes | meaning |
| --- | --- | --- |
| aligned | 0 | no approved mapping exists to be aligned with |
| inherited | 0 | same reason |
| drift | 0 | detectable only against an approval |
| conflict | 0 | no lane is claimed by two groups |
| unapproved | 0 | no exception exists anywhere |
| invalid | 0 | no approved value exists to be invalid |
| **unknown** | **321** | no approved policy owns the lane — delivery unchanged |

Destinations are per-lane topics, not per-namespace: the same namespace spans
up to 20 distinct values (e.g. `coding-hermes` 28 lanes / 20 refs, `docs` 66
lanes / 8 refs). Namespace row (namespace → distinct refs):

| namespace | distinct refs |
| --- | --- |
| coding-hermes | 20 |
| dogfood | 19 |
| ops | 19 |
| pm | 19 |
| qa | 19 |
| duckbrain-sync | 18 |
| review | 18 |
| releases | 10 |
| docs | 8 |
| perf | 5 |
| code-factory | 1 |
| data-cleanup | 1 |

Family view (prefix before the first `-`; a heuristic, stated as such). "off
modal" = lanes whose value differs from the family's most common value:

| family | lanes | modal ref | on modal | off modal |
| --- | --- | --- | --- | --- |
| guard | 11 | T14/e3f0a770 | 11 | 0 |
| hermes | 14 | T16/385463ff | 11 | 3 |
| mischief | 11 | T14/e3f0a770 | 11 | 0 |
| monitoring | 11 | T18/d074fe61 | 11 | 0 |
| task | 11 | T20/ad7e1e76 | 11 | 0 |
| voice | 11 | T22/c1c1074b | 11 | 0 |
| code | 10 | T7/2e97ce7d | 10 | 0 |
| h3 | 11 | T11/84a67fc9 | 10 | 1 |
| dagger | 11 | T10/be632e8a | 9 | 2 |
| digest | 11 | T11/84a67fc9 | 9 | 2 |
| logsey | 11 | T11/84a67fc9 | 9 | 2 |
| lore | 11 | T11/84a67fc9 | 9 | 2 |
| pulse | 11 | T11/84a67fc9 | 9 | 2 |
| crier | 11 | T9/c65956cb | 8 | 3 |
| terminal | 11 | T21/7f43d939 | 8 | 3 |
| boardctl | 11 | T3/85716bf2 | 7 | 4 |
| bunker | 11 | T5/282c61ba | 7 | 4 |
| chimera | 11 | T6/0a89da53 | 7 | 4 |
| coding | 12 | T3/85716bf2 | 7 | 5 |
| heading | 11 | T15/9c7619e7 | 7 | 4 |
| off | 11 | T2/f6a9f996 | 7 | 4 |
| trouble | 11 | T14/e3f0a770 | 7 | 4 |
| warpfs | 11 | T23/e2a80c4c | 7 | 4 |
| consensus | 11 | T8/fc71be4a | 6 | 5 |
| duckbrain | 11 | T12/e74b214c | 6 | 5 |
| gitreins | 11 | T13/8c608ae3 | 6 | 5 |
| muster | 10 | T19/d85b5188 | 6 | 4 |
| operator | 6 | T3/85716bf2 | 5 | 1 |
| auger | 10 | T1/17422c4a | 4 | 6 |
| doc | 2 | T4/78d053f0 | 2 | 0 |
| lane | 1 | T17/3fbdb6a1 | 1 | 0 |
| project | 1 | none | 1 | 0 |
| release | 1 | T4/78d053f0 | 1 | 0 |
| reports | 1 | none | 1 | 0 |

Aggregate: **34** families, **11** entirely on one ref, **23** split across refs,
**79** lanes off their family's modal ref. Lanes with no destination at all:
`dagger-v2-sync`, `duckbrain-qa`, `gitreins-sync`, `hermes-dagger-sync`,
`project-sitrep-print`, `reports-sync`.

**Verdict.** Aligned **0** / drift **0** / conflict **0** / unknown **321**. The
mapping is *not* misaligned today — it is *unowned*: no approved authoritative
mapping exists for any lane, so nothing can be declared aligned or drifted, and
the enforcement added here is armed with no authority to apply. The 23 split
families and 79 off-modal lanes are the candidate set an owner-approved mapping
must classify; this audit classifies and **never changes** ownership (no group,
policy, project row or live setting was written outside test fixtures).

---

## 3. Executable evidence

### 3.1 Required gate (each command must exit 0)

Verbatim from one green run of the third command (2026-11-28 02:43, this tree):

```
$ env -u SCHEDULER_OPERATOR_RESTART_APPROVED go build ./...
build_rc=0
$ env -u SCHEDULER_OPERATOR_RESTART_APPROVED go vet ./...
vet_rc=0
$ env -u SCHEDULER_OPERATOR_RESTART_APPROVED go test ./internal/blocks ./internal/api ./internal/scheduler -count=1
ok  	github.com/coding-hermes/scheduler/internal/blocks	0.082s
ok  	github.com/coding-hermes/scheduler/internal/api	99.812s
ok  	github.com/coding-hermes/scheduler/internal/scheduler	173.201s
test_rc=0
```

`env -u SCHEDULER_OPERATOR_RESTART_APPROVED` is mandatory on this host: an
ambient `=1` makes the repo's own scheduler tests fail closed for unrelated
reasons.

Also run green on this tree: `golangci-lint run ./cmd/destaudit/...
./internal/blocks/... ./internal/scheduler/... ./internal/api/...` → `0 issues`
(the CI-pinned v2.15.0), and `go test ./cmd/schedulerd` (the README package-count
and flag-parity guards, after the README count moved 29 → 30 for the new
`cmd/destaudit` package).

### 3.6 One pre-existing flaky test — every attempt reported, nothing hidden

The third gate command is intermittent because of `TestSCHEDGAP1666_SevenDaySimulation`
(`internal/scheduler/schedgap1666_test.go:737`, "tasks lane flip:board_empty
admissions = 0 ..., want exactly 1"), a board-wake simulation that asserts on a
5 s/10 s settle wait. It is not this row's code and the test is untouched here.
Full attempt table (same commands, same host, sequential):

| tree | command | outcomes |
| --- | --- | --- |
| this branch | 3-package gate command | **green ×3, red ×2** (both reds: only `TestSCHEDGAP1666`, same line/assertion) |
| **unmodified base 79b488cf** (scratch worktree) | 3-package gate command | green ×6, red ×0 |
| this branch | `make test` (`./... -short`) | green ×1, red ×2 (same test) |
| **unmodified base 79b488cf** | `make test` | **red ×1 — identical assertion, same line** |
| this branch | `go test -short ./internal/scheduler` (alone) | green ×5 |
| unmodified base | `go test -short ./internal/scheduler` (alone) | green ×3 |

**What this table does and does not prove.** The base-tree `make test` red is
direct evidence that the test is nondeterministic without this row's changes
(same command, same test, same line 737, same "0 vs 1" values). The failing
assertion counts ADMISSION REASONS — it is stamped from the park registry and the
board-wake nudge source (`slot_pool.go:690-703`, `schedgap1666.go:234`), both
untouched here, and delivery only happens after that stamp, so no code path this
row changed can produce the value. What the table does NOT prove is full
independence of the flake rate: on this branch it reddened 2 of 5 gate runs while
base stayed green in 6, and this package now runs ~14 more tests (each a
sub-second fake-transport delivery), which shifts when the simulation executes
inside a package-parallel run. That is a test-environment effect, not a product
one, but it is stated rather than hidden. Not fixed (out of this row's scope).

### 3.2 Restart persistence

* `internal/blocks/destination_persistence_test.go::TestDestinationPolicyPersistsAcrossStoreInstances`
  — write a policy through the store, then read it through a **second** `Store`
  over the same paths (the daemon-restart shape; reads re-parse the file) and
  assert `DecideDelivery` classifies the lane `aligned` from the reloaded data.
* The live-shape rehearsal below ran the audit as a **separate process twice**;
  the two reports were byte-identical (`cmp` clean).

### 3.3 Sync persistence — not applicable, proven absent

There is no sync-path writer for the group store. The only writer of
`groups.jsonl` is `internal/blocks.Store` (reached through the deploy-block API);
`grep -rn "groups.jsonl|GroupsPath|blocks.NewStore" --include=*.go .` shows
`internal/blocks/store.go` (the store), `internal/api/*` (handlers),
`internal/mcp/server.go` + `cmd/schedulerd/main.go` (read/construct only). So the
policy's durability is the file itself plus the daemon re-read armed at boot
(`scheduler.SetDestinationPolicySource(blocks.Store.ListGroups)` in
`cmd/schedulerd/main.go`), which the E2E test
`TestDestinationGuardReadsTheGroupsJSONLStore` exercises from file to transport.

### 3.4 Non-destructive preview + rollback

* `TestDestinationPreviewLeavesStoreBytesUntouched` — digest `groups.jsonl`,
  run `InventoryDestinations` + `PlanDestinationMigration` + `DecideDelivery`,
  digest again: identical bytes (the preview cannot mutate).
* `TestDestinationPolicyRollbackRestoresBytes` — capture the file bytes, migrate
  the policy to a new target (file digest changes), restore the captured bytes:
  digest returns to the original **and** the old policy reads back. This is the
  only rollback path that exists (see F4).
* Live-shape rehearsal (below): run A and run C (after restore) produce identical
  audit reports.

### 3.5 Live-shape drift rehearsal (against a scratch COPY of the live DB)

Setup: the live `scheduler.db` was copied to a scratch path; a scratch
`groups.jsonl` was written with one group (`audit-family`) whose approved
destination equals the live target of `auger-docs` (value read from the copy,
never printed), containing `auger-docs`, `auger-ops`, `auger-readme`.

| run | action | status counts | exit |
| --- | --- | --- | --- |
| A | audit the copy | aligned 3, unknown 318 | 0 |
| A2 | audit the copy again (separate process) | identical to A (`cmp` clean) | 0 |
| B | `UPDATE projects SET deliver=<a different live target> WHERE name='auger-ops'` on the copy, then audit | aligned 2, **drift 1** (`auger-ops`, origin `audit-family`), unknown 318 | **1** |
| C | restore the pre-mutation copy, audit | identical to A (`cmp` clean) | 0 |

That is the whole drift class end to end on real fleet data: the same lane is
`aligned` when it matches its approval, `drift` (exit 1, named) when one thread
away, and `aligned` again after rollback.

---

## 4. E2E delivery through the TEST transport, with negative controls

The transport is the repo's existing fake `hermes` PATH stub (`setupFakeHermes`):
`deliverOutputWithMode`/`deliverAlertWith` really exec `hermes send`, the stub
records the argv, no network is touched. Tests:
`internal/scheduler/deliver_destination_guard_test.go`.

| control | assertion | result |
| --- | --- | --- |
| happy path | delivered `--to <approved target>` | PASS |
| lane inheritance (new lane, no target) | delivered `--to <group's approved target>`, no "no delivery target configured" | PASS |
| **wrong thread** | nothing sent, no retry attempts, `DEST-DRIFT` line, no destination in the log | PASS |
| approved exception | delivered to the exception target | PASS |
| unapproved exception | nothing sent, `DEST-UNAPPROVED` | PASS |
| conflicting ownership | nothing sent, `DEST-CONFLICT` | PASS |
| invalid approval | nothing sent, `DEST-INVALID` | PASS |
| policy-less / no group | delivered unchanged (pre-17084 behaviour) | PASS |
| guard disabled (no source) | delivered unchanged | PASS |
| unreadable store | **fail-open**: delivered + `DEST-UNAVAILABLE` (a config fault must not silence the fleet) | PASS |
| alert path | wrong-thread alert refused, approved alert delivered | PASS |
| file-mode refusal | nothing sent, no report artifact left | PASS |
| real store wiring | `blocks.Store.ListGroups` over a real `groups.jsonl` → transport; drift refused | PASS |

### 4.1 Red proofs (each violation deliberately reintroduced, then restored)

| violation reintroduced | test that went red | evidence |
| --- | --- | --- |
| removed the guard calls from `deliver.go` | 9 scheduler guard tests | `TestDestinationGuardRefusesWrongThread`: `drifted target was delivered; captured "send --to telegram:-100777:12 ..."`; `TestDestinationGuardInheritsGroupDestination`: `inherited delivery missing; captured ""` |
| `DecideDelivery` drift branch allowed | `TestDecideDeliveryRules/drift` + 5 scheduler tests | `--- FAIL: TestDecideDeliveryRules/drift` |
| inheritance branch returned the empty current value | `TestDecideDeliveryRules/inherited`, `TestDestinationGuardInheritsGroupDestination` | `inherited delivery missing; captured ""` |
| conflict refusal removed | `TestDecideDeliveryRules/conflicting_*` (2), `TestDestinationGuardRefusesConflictingOwnership` | `conflicting ownership delivered; captured "send --to telegram:-100777:11 ..."` |
| unapproved exception allowed to authorize | `TestDecideDeliveryRules/unapproved_exception`, `TestDestinationGuardRefusesUnapprovedException` | `unapproved exception was delivered; captured "send --to telegram:-100777:22 ..."` |
| `DEST-OVERWRITE` alert disabled | `TestGroupUpdateAlertsOnDestinationPolicyOverwrite` | `approved destination replacement was not alerted` |

Every mutation was reverted; the restored files' digests match the pre-mutation
copies (`sha256sum` compared, all OK), and the full gate was re-run green after
the last restore.

---

## 5. Remaining unknowns and unresolved drift

* **F1 — no approved mapping exists (blocking for alignment).** 321/321 lanes are
  `unknown`: one group (`pilot`), zero policies. Enforcement is armed but has no
  authority to apply. Until an owner approves a mapping, "aligned"/"drift" is not
  answerable for any live lane.
* **F2 — 6 lanes have no destination at all** (`dagger-v2-sync`, `duckbrain-qa`,
  `gitreins-sync`, `hermes-dagger-sync`, `project-sitrep-print`, `reports-sync`):
  their tick reports are dropped with "no delivery target configured". Lane
  inheritance fixes this **only** once their group has an approved policy; a
  project row with `deliver` set would also do it.
* **F3 — approvals are request-local, never persisted**: there is no
  approval/change record, so no audit trail of who approved what, when, or with
  which evidence.
* **F4 — no apply, no clear, no rollback verb.** `GroupUpdate.DestinationPolicy`
  is a pointer: `nil` means "not supplied", so no PUT can *clear* a policy (pinned
  by `TestGroupUpdateCannotClearPolicy`). Rollback today is a file-level restore
  of `groups.jsonl` (proven byte-identical), which is operational, not an API.
* **F5 — no sync persistence path**: `groups.jsonl` has exactly one writer (the
  deploy-block API). If another node's daemon is started with a different
  `--groups-file`, the policy silently does not apply there.
* **F6 — 23 of 34 families split across 2–6 destination refs, 79 lanes off their
  family's modal ref.** Cannot be classified as drift or as intentional
  per-role topics without an approved mapping; classifies as `unknown`.
* **F7 — live enforcement is a no-op today** (0 approved policies), demonstrated
  by the live audit's `exit 0` and by the pre-existing delivery tests still
  passing unchanged.

Nothing unresolved was silently repaired: no live settings, group memberships,
policies or project rows were modified; classification only.

---

## 6. Files added / changed by this row

```
CHANGELOG.md                                       [Unreleased] entry for the enforcement + audit
README.md                                          package count 29→30, cmd/ list, destaudit usage
cmd/destaudit/main.go                              read-only fleet audit
cmd/destaudit/main_test.go                         sanitization + refs + exit code
cmd/schedulerd/main.go                             arm the guard from groups.jsonl
docs/api.md                                        enforcement table (replaces "does not change delivery")
internal/api/block_handlers.go                     DEST-OVERWRITE alert on PUT
internal/api/destination_overwrite.go              sanitized overwrite summary
internal/api/destination_overwrite_pure_test.go    summary rules
internal/api/destination_overwrite_test.go         alert fires / does not leak
internal/blocks/destination_effective.go           DecideDelivery (delivery-time decision)
internal/blocks/destination_effective_test.go      rules + inventory parity + no-mutation
internal/blocks/destination_persistence_test.go    restart, non-destructive preview, rollback
internal/scheduler/deliver.go                      guard call sites (report + alert)
internal/scheduler/deliver_destination_guard.go    guard, inheritance, once/day alerts
internal/scheduler/deliver_destination_guard_test.go  E2E matrix + negative controls
```

Hygiene note: an audit rehearsal accidentally left one untracked scratch file at
the repository root (it held a real destination value from the DB copy). It was
deleted before staging; the staged set below contains no destination value —
`grep` over every added/changed file shows only the synthetic `-100777` /
`-100999` fixtures and the `telegram:<chat>:<thread>` grammar in docs.

---

## 7. Rework (apply/rollback + sync)

The first delivery was graded **INCOMPLETE** on two criteria. Verbatim from run
`14aa71ac`:

> "No approved migration apply exists. `internal/blocks/destination_plan.go:21-26`
> only previews and never persists; `internal/api/destination_preview.go:47-49`
> always returns `dry_run:true`; `docs/api.md:232-236` explicitly says no apply or
> rollback verb exists. The rollback test at
> `internal/blocks/destination_persistence_test.go:108-148` manually restores
> `groups.jsonl` bytes. Restart persistence is covered at lines 45-70, but no
> sync-path writer or sync persistence test exists, as recorded in audit.md
> sections 3.3 and F5."

…with the two uncovered failure modes:

0. "An operator can obtain a ready destination migration preview but cannot apply
   it through a reversible, idempotent migration path; rollback requires manually
   replacing `groups.jsonl`."
1. "Approved destination policy is not demonstrated to survive configuration
   synchronization, and nodes using different groups files can silently enforce
   different mappings."

Sections 1–6 are unchanged and still stand (the inventory, the guard matrix, the
sanitized evidence). Section 7 adds the missing half and re-checks the one
property of failure mode 1 that this product can actually execute.

### 7.1 What was built (and where the line is drawn)

Two verbs on the **existing** identity-gated deploy endpoint, as body keys — the
same shape `destination_preview` already uses, so no route was added and the
documented route table (`internal/api/server_test.go` documentedPaths) is
unchanged:

```
POST /api/v1/groups/{name}/deploy {"destination_apply":{"approvals":[{project,from,to}…]}}
POST /api/v1/groups/{name}/deploy {"destination_rollback":{"plan_digest":"sha256:…"}}
```

The writes are the ones that already exist. **No second writer, no second store,
no second source of truth:**

| what moves | writer |
| --- | --- |
| the lane's delivery target (`projects.deliver`) | `database.UpdateProject` — the exact writer `PUT /api/v1/projects/{name}` uses |
| the change ledger | `blocks.Store` → `writeJSONL` into `groups.jsonl`, the store's only writer |

The ledger lives in the group row (`destination_history`, newest 20 kept) for
three reasons: it is written by the store that already owns the file, a fresh
process reads it back with the policy (so a rollback capture cannot be lost by a
restart), and it keeps the rollback capture next to the approval that authorized
it. That record holds `kind`/`actor`/`at`/`plan_digest` plus the moved
`project` and its `prior`/`next` pair — the pair **is** the rollback capture, and
it is the same confidentiality class as the policy destination already stored in
that file. Every rendered surface (API response, `DEST-APPLY` / `DEST-ROLLBACK`
log line, event-log details, MCP group read, audit CLI) shows only
`<sha256:8>` refs; group read/create/update responses omit both the policy and
the ledger, and a create body (REST or MCP) cannot inject a ledger record.

Contract, all enforced against the **current** store at apply time:

* **all-or-nothing** — one refused approval refuses the batch; a stale plan is
  refused (`code:"not_ready"`, per-item status) instead of applied;
* **idempotent** — when every approved lane already carries its target there is
  nothing to move, and the call performs *zero writes*, so `groups.jsonl` stays
  byte-identical (proved by digest, not by inspection);
* **non-destructive** — the prior value of every moved lane is captured before
  the write, a mid-batch write failure is compensated back, and every other group
  row / lane / column is asserted byte-identical;
* **reversible** — rollback restores the captured prior values (including an
  unset lane), refuses when nothing is captured, when the digest names another
  migration, or when a lane changed since the apply (it never clobbers a newer
  decision);
* **auditable** — each apply and each rollback appends a record with who (the
  authenticated operator identity), when (RFC3339 UTC), the plan digest, and the
  prior→new refs.

One honest semantic consequence, stated rather than buried: **rollback reverses
the migration, not the approval.** The policy still names the migrated target, so
a rolled-back lane reads as `drift` (refused) until the operator re-approves or
applies a fresh plan. That is the visible direction — the alternative (silently
delivering to a target the approval does not name) is the bug this row exists to
remove.

### 7.2 Executed tests (names + result)

`internal/blocks` (`go test ./internal/blocks -run 'TestDestination(Apply|Rollback|Ledger|PolicyFingerprint)' -v`, exit 0, 13 pass):

| test | proves |
| --- | --- |
| `TestDestinationApplyMovesLanesAndRecordsTheChange` | drift + unset lane both move; prior captured per lane; ledger record has kind/actor/at/plan digest/moves; policy untouched |
| `TestDestinationApplyIsIdempotent` | second apply: `applied=0 already=2`, **zero writer calls**, `groups.jsonl` digest identical |
| `TestDestinationApplyLeavesTheRestOfTheStoreUntouched` | every group row (minus the ledger field) byte-identical; `templates.jsonl` digest identical; unrelated lane never written |
| `TestDestinationApplyRefusesAStalePlan` (3 sub-tests) | `from` no longer matches → `stale_approval`; `to` is not the approved target → `stale_approval`; non-member → `not_a_member`; zero writes, store digest unchanged in all three |
| `TestDestinationApplyRefusesWithoutAnApprovedPolicy` | an unapproved policy authorizes nothing |
| `TestDestinationRollbackRestoresPriorTargets` | prior targets restored (incl. the empty one); mapping digest returns to its pre-apply value; ledger gains a rollback record naming the same plan digest; policy untouched |
| `TestDestinationRollbackRefusesWhenNothingCaptured` | never-applied → `nothing_captured`; apply→rollback→rollback → `nothing_captured` again, zero writes, store digest unchanged |
| `TestDestinationRollbackRefusesDivergedLane` | lane changed after the apply → `state_diverged` with the lane named; zero writes |
| `TestDestinationRollbackPlanDigestMustMatch` | a digest naming another migration → `digest_mismatch`; zero writes |
| `TestDestinationApplyCompensatesAFailedLaneWrite` | second lane write fails → first lane restored to its prior value, no ledger record, reported as an IO fault (not a refusal) |
| `TestDestinationApplySurvivesAFreshStoreInstance` | **B**: after the apply, a *second* Store over the same path (restarted daemon shape) classifies the lane `aligned`, inherits for the empty lane, agrees on the policy fingerprint, and a different approved mapping produces a different fingerprint |
| `TestDestinationLedgerNeverRendersRawDestinations` | the sanitized view carries refs only; the store still holds the capture (asserted explicitly, not implied) |
| `TestDestinationPolicyFingerprintIsStableAndSanitized` | order-independent, mapping-sensitive, no destination inside |

`internal/api` (`go test ./internal/api -run 'TestDestination(Apply|Rollback|GroupCreateCan)' -v`, exit 0, 8 pass) — the same properties through the real handlers, a real SQLite DB and a real `groups.jsonl` on disk:

| test | proves |
| --- | --- |
| `TestDestinationApplyThroughTheAPI` | 200 with `applied:2`; `projects.deliver` really changed (read back via `database.GetProject`); weight/cooldown/model/provider/enabled/decay untouched ("without overwriting unrelated settings"); ledger present in the store, absent from every group body |
| `TestDestinationApplyOverHTTPNeverLeaksDestinations` | response bytes carry no chat id and no `telegram:` string; only `next_ref` refs |
| `TestDestinationApplyIsIdempotentOverHTTP` | repeat POST → `applied:0 already_applied:2`, `groups.jsonl` digest identical, still one ledger record |
| `TestDestinationRollbackThroughTheAPI` | both lanes restored (incl. the cleared one); ledger gains the rollback; policy untouched; a fresh store sees the restored state and classifies it `drift` against the still-approved policy |
| `TestDestinationApplyRefusesAStalePlanOverHTTP` | 409 `code:"not_ready"` + `refused:[{api-a,stale_approval}]`; lane unmoved; store digest unchanged; no destination in the refusal |
| `TestDestinationRollbackRefusesWhenNothingCapturedOverHTTP` | 409 `code:"nothing_captured"`; lane unmoved; store digest unchanged |
| `TestDestinationApplyRequiresAnApprovedPolicy` | unapproved policy → 409, lane unmoved |
| `TestDestinationGroupCreateCannotInjectALedgerRecord` | a create body carrying `destination_history` is dropped before the store sees it |

`internal/scheduler` (`go test ./internal/scheduler -run TestDestinationGuardEnforcesAnAppliedMigration -v`, exit 0):

* `TestDestinationGuardEnforcesAnAppliedMigration` — drives the real delivery
  path (`deliverOutput` → fake `hermes` transport) across an apply: **before**
  the lane is refused (drift against the approval, nothing captured by the
  transport), **after** a *fresh* Store over the same file delivers to the
  migrated target, and the fingerprint prefix is the `sha256:` form the daemon
  logs.

`cmd/destaudit` (`go test ./cmd/destaudit -v`, exit 0, 7 pass) — two new:
`TestReportPrintsThePolicyFingerprint`, `TestFingerprintIgnoresUnapprovedPolicies`.

### 7.3 Red proofs (each violation reintroduced, then restored)

| mutation | tests that went RED (verbatim) | restored |
| --- | --- | --- |
| stale-plan refusal disabled (`if false && len(refused) > 0`) | `TestDestinationApplyRefusesAStalePlan`: `apply err = <nil>, want a refusal` (all 3 sub-tests) · `TestDestinationApplyRefusesAStalePlanOverHTTP`: `stale apply = 200 map[applied:0 …], want 409` | `sha256 64ac6f46…` (identical to pre-mutation copy) |
| nothing-captured refusal disabled (`return DestinationRollbackResult{}, nil`) | `TestDestinationRollbackRefusesWhenNothingCaptured`: `rollback without a capture: err = <nil>, want code "nothing_captured"` · `…OverHTTP`: `rollback without a capture = 200 map[… rolled_back:0], want 409` | same digest |
| idempotent early return disabled (ledger written with zero moves) | `TestDestinationApplyIsIdempotent`: `repeat apply changed the store: 727e5f8a… → ddeac995…` · `…OverHTTP`: `repeat apply changed groups.jsonl: f5b80ab9 → e46228d0` | same digest |

Each mutation was applied to one file, the two affected test arms run, the file
restored from the pre-mutation copy, and the digest compared (`sha256sum`,
identical); the full gate was re-run green afterwards (7.5).

### 7.4 Failure mode 1 — sync persistence, executed vs. out of scope

**Finding (unchanged, now with the grep):** there is **no sync path** for
`groups.jsonl` in this repo. Its only writer is `internal/blocks.Store`
(`writeJSONL`); everything else reads or constructs. No script, cron, deploy or
`fleet-sync` path copies it — `grep -rn "groups.jsonl|GroupsPath|blocks.NewStore"`
returns `internal/blocks/store.go` (the store), `internal/api/*` (handlers),
`internal/mcp/server.go` + `cmd/schedulerd/main.go` (read/construct), docs and
the audit CLI. The file **is** the source of truth for the node that owns it.

**Executed instead — the property that matters here:** the applied state must be
what a process that did not perform the apply enforces, and the two states must
be distinguishable when they differ. Both are tests, not claims:

* `TestDestinationApplySurvivesAFreshStoreInstance` — a second `Store` over the
  same path reads the applied policy + ledger and classifies the lane `aligned`
  (and the empty lane as inheriting the approved destination);
* `TestDestinationGuardEnforcesAnAppliedMigration` — the same fact at the
  delivery layer: refusal before the migration, delivery to the migrated target
  after it, through the transport the fleet actually uses;
* `TestDestinationRollbackThroughTheAPI` — the restored state is what a fresh
  store enforces afterwards.

**The "silently different mappings" half is now observable, not solved:**
`blocks.DestinationPolicyFingerprint` digests the approved mapping set
(`sha256:16`, no destination recoverable), and it is printed by **both** surfaces
an operator runs — `schedulerd` at boot (`DEST-STORE: groups=<path>
approved_policies=<n> policy_fingerprint=<digest>`) and `cmd/destaudit` (report
line + `policy_fingerprint` in `--json`). Equal fingerprints = equal
enforcement; a node started with a different `--groups-file` (or an older copy)
is detectable instead of silent.

**Not claimed:** no cross-node convergence test exists, because the product has
no cross-node propagation to test. A node still has to be given the file by
whatever mechanism its operator chooses; this row makes the divergence visible
and leaves the transport out of scope, stating the reason rather than inventing
a sync path inside a delivery-guard row.

### 7.5 Gate (green, verbatim)

```
$ env -u SCHEDULER_OPERATOR_RESTART_APPROVED go build ./...
build_rc=0
$ env -u SCHEDULER_OPERATOR_RESTART_APPROVED go vet ./...
vet_rc=0
$ env -u SCHEDULER_OPERATOR_RESTART_APPROVED go test ./internal/blocks ./internal/api ./internal/scheduler -count=1
ok  github.com/coding-hermes/scheduler/internal/blocks    0.151s
ok  github.com/coding-hermes/scheduler/internal/api       92.018s
ok  github.com/coding-hermes/scheduler/internal/scheduler 168.970s
test_rc=0
```

Also green on this tree: `go test ./internal/mcp -count=1` (returns 0 after the
MCP ledger-strip guard), `go test ./cmd/destaudit -count=1`, and
`golangci-lint run ./...` → `0 issues`. The `cmd/queueprobe` guard file-scope
warning named in the brief is pre-existing and untouched.

### 7.6 Live-shape re-check (read-only)

Same command as §2, re-run after the rework, on the live stores opened `mode=ro`:

```
$ destaudit --db ~/.hermes/coding-hermes/scheduler.db --groups ~/.hermes/coding-hermes/groups.jsonl
  groups:             ~/.hermes/coding-hermes/groups.jsonl (1 groups, 0 approved policies, 0 unapproved)
  scheduler db:       ~/.hermes/coding-hermes/scheduler.db (mode=ro)
  lanes audited:      321 (enabled only)
  policy fingerprint: sha256:e3b0c44298fc1c14 (compare across nodes; no sync path exists)
  STATUS COUNTS: unknown 321 · UNRESOLVED DRIFT (0) · audit_rc=0
```

`sha256:e3b0c442…` is the digest of an **empty** approved-mapping set: today the
fleet has no approval at all, so an apply against live state refuses with
`not_ready` ("no APPROVED destination policy") rather than migrating anything —
F1/F7 remain true and untouched by this rework.

### 7.7 Files added / changed by the rework

```
internal/blocks/destination_migrate.go             apply/rollback engine, ledger, refs, fingerprint
internal/blocks/destination_migrate_test.go        13 tests (idempotent/non-destructive/reversible/refusals)
internal/blocks/blocks.go                          Group.DestinationHistory (ledger field)
internal/api/destination_migrate.go                the two verbs, sanitized responses, refusal mapping
internal/api/destination_migrate_test.go           8 HTTP tests incl. both refusal arms
internal/api/block_handlers.go                     body keys, ledger/policy hidden from group bodies, create-ingest guard
internal/mcp/handlers.go                           ledger stripped from MCP group reads + create ingest
internal/scheduler/deliver_destination_migrate_test.go  delivery-layer proof across an apply (fresh store)
cmd/destaudit/main.go                              policy_fingerprint in report + --json; shared fingerprint primitive
cmd/destaudit/policy_fingerprint_test.go           2 tests
cmd/schedulerd/main.go                             DEST-STORE boot line (path + approved count + fingerprint)
docs/api.md                                        apply/rollback contract, refusal table, sync scope note
CHANGELOG.md                                       [Unreleased] entry for the apply/rollback verbs
```

### 7.8 What was still not executed (named, not papered over)

* **No live-daemon HTTP rehearsal of the verbs.** The apply path is exercised
  through the real handlers against a real SQLite DB and a real `groups.jsonl`,
  but not against a running `schedulerd` pointed at a copy of the live DB: that
  needs a second daemon on a scratch port and a scratch store, and the live fleet
  has zero approved policies (so the only honest live result would be the
  `not_ready` refusal already covered by tests). Left out deliberately; nothing
  in this section claims otherwise.
* **No cross-node sync test** — no sync surface exists (§7.4).
* **Approvals themselves are still request-local.** The *migration* now has an
  audit trail (who/when/plan digest/prior→new), but the approval that authorized
  it is still just the `destination_policy` write; there is no separate
  approval record with its own evidence. F3 is narrowed, not closed.
* **A policy still cannot be cleared** via any verb (F4's second half:
  `GroupUpdate.DestinationPolicy` is a pointer, `nil` means "not supplied"). Apply
  and rollback do not change that, and `TestGroupUpdateCannotClearPolicy` still
  pins it.
* **Write protection on the policy does not exist**: an operator PUT can still
  replace an approval (it now logs `DEST-OVERWRITE`). Out of this rework's scope.
