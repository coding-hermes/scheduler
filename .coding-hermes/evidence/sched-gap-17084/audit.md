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
