# SCHED-GAP-107 verification — task bump (fresh-run suite + RED-check proofs)

Task row: `SCHED-GAP-107-VER` · Feature: SCHED-GAP-107 (task bump), landed at
`19b8ed7` (migration v26, scheduler/DB/API) + `fd430ef` (config pin guard).
This suite ADDS tests only — no production code was changed.

New test files (all `-short`-safe, deterministic, no wall-clock sleeps, no network):

- `internal/database/schedgap107_verify_test.go` — migration v26 row/columns/DEFAULTs,
  snapshot exactness (adaptive-OFF shape), full ClearBump zeroing, error identities.
- `internal/api/schedgap107_verify_test.go` — boundary acceptance (ticks 1/8, cooldown
  =7200), rejection sweep, invalid-JSON 400, unbump 404 + response restore, status
  `bumps[].started_at`.
- `internal/scheduler/schedgap107_verify_test.go` — bumpTickCompleted return-value
  contract, Phase A completeness pre-Phase B, idle streak resumption, hard-cap
  force-revert on UNFLAGGED ticks, adaptive-still-runs-during-bump, packFlat
  override, countEligibleProjects mirror.

## Acceptance criterion → test → RED-check mutation

| # | SCHED-GAP-107 acceptance criterion | Proving test(s) | RED mutation that turns it FAIL |
|---|---|---|---|
| 1 | Migration v26: 9 `projects.bump_*` columns + `ticks.bump` exist after Migrate | `TestVerify_Migrate26_BumpColumnsAndDefaults` (+ existing `TestMigrate_BumpColumns`) | n/a — assertion is structural (schema presence) |
| 2 | `BumpProject` snapshots pre-bump state exactly; cooldown takes effect immediately | `TestVerify_BumpProject_SnapshotExactAdaptiveOff` (+ existing `TestBumpProject_SnapshotAndImmediateEffect`, `TestBump_ProjectWide`) | n/a — assertion is structural (snapshot equality) |
| 3 | `ClearBump` restores cooldown/floor/ceiling/streak verbatim and zeroes all bump fields | `TestVerify_BumpProject_SnapshotExactAdaptiveOff` (+ existing `TestBumpProject_SnapshotAndImmediateEffect` restore half, `TestBump_ManualClear`) | n/a — assertion is structural |
| 4 | `BumpProject`/`ClearBump` on missing project errors (identity) | `TestVerify_BumpProject_MissingMapsToErrProjectNotFound` (+ existing `TestBumpProject_NotFound`) | n/a — assertion is structural |
| 5 | API bounds: `ticks` 0 and 9 → 400; 1..8 accepted | `TestVerify_BumpAPI_TicksRangeSweep`, `TestVerify_BumpAPI_BoundariesAccepted` (+ existing `TestAPI_BumpProject_TicksOutOfRange`) | n/a — assertion is structural (validation matrix) |
| 6 | API: `cooldown` < 7200 → 400; = 7200 accepted | existing `TestAPI_BumpProject_CooldownBelowFloor` + `TestVerify_BumpAPI_BoundariesAccepted` | n/a — assertion is structural |
| 7 | API: missing/whitespace `reason` → 400; invalid JSON → 400 | existing `TestAPI_BumpProject_MissingReason` + `TestVerify_BumpAPI_InvalidJSON400` | n/a — assertion is structural |
| 8 | API: unknown project 404; disabled 409; double-bump 409 | existing `TestAPI_BumpProject_NotFound`/`_Disabled`/`_AlreadyBumped` + `TestVerify_UnbumpAPI_NotFound404` | n/a — assertion is structural |
| 9 | `POST /unbump` clears state (Phase A only), response carries restored values | existing `TestAPI_UnbumpProject` + `TestVerify_UnbumpAPI_ResponseRestoresState` | n/a — assertion is structural |
| 10 | `GET /api/v1/status` `bumps[]` carries active fields incl. `started_at`; `[]` (not null) when none | existing `TestAPI_StatusShowsBumps`/`_BumpsEmptyArrayWhenNone` + `TestVerify_StatusBumpsCarriesStartedAt` | n/a — assertion is structural |
| 11 | Two-phase auto-revert: last bump tick returns true, Phase A restore observable BEFORE Phase B; Phase B re-evaluates restored baseline (real work → floor; idle → streak resumes, one eval) | `TestVerify_TwoPhaseRevert_FinalTickReturnsTrueAndRestores`, `TestVerify_TwoPhaseRevert_IdleResumesRestoredStreak` (+ existing `TestBump_AutoRevert`, `TestBump_IdleNoStreakChange`, `TestBump_RealWorkProgress`) | **M2** below |
| 12 | 12h hard cap force-reverts (flagged AND unflagged ticks) | existing `TestBump_12hHardCap` + `TestVerify_HardCap_UnflaggedTickForceReverts` | n/a — covered by the countdown family; hard-cap mutation equivalent to M2's never-revert shape |
| 13 | Only `bump=1` ticks consume the countdown | existing `TestBump_OnlyFlaggedTicksConsume` + `TestVerify_HardCap_UnflaggedTickForceReverts` (fresh half) | n/a — assertion is structural (state equality) |
| 14 | Packer bump-cooldown override in EVERY selection path (flat Pick, namespace Pack, packFlat) + bump urgency tier; adaptive still runs mid-bump (override is load-bearing) | `TestVerify_PackerOverride_PackFlatPath`, `TestVerify_AdaptiveStillRunsDuringBump` (+ existing `TestBump_PackerCooldownOverride`, `TestBump_MultiPoolPackOverride`) | **M1** below |
| 15 | `countEligibleProjects` mirror uses the bump cooldown (GAP-050 parity) | `TestVerify_EligibleMirror_UsesBumpCooldown` | **M4** below |
| 16 | `ApplyFleetConfig` skips cooldown re-pin while `bump_active=1`; re-pins normally when inactive | existing `TestApplyFleetConfig_BumpActivePreservesCooldown` / `_BumpInactiveRepinsNormally` (config package — negative/positive pair already present, not duplicated) | **M3** below |

Override-site inventory (criterion 14): `packer.go` (flat Pick, 1 site),
`packer_select.go` (namespace Pack, scoring + gate, 2 sites),
`multipool_packer.go` (packFlat, scoring + gate, 2 sites),
`loop.go` `countEligibleProjects` (mirror). **No selection path lacks the
override — no finding.**

## RED-check proofs (performed in `git worktree` scratch copies at /tmp/redcheck-*, since removed; committed tree untouched)

### M1 — packer bump cooldown override reversed

- Mutation: `if s.bumpActive && s.bumpCooldownS > 0` → `if false && …` at all three
  selection-path sites (`packer.go:181`, `packer_select.go:168`, `multipool_packer.go:184`).
- Result (3 tests FAIL — one per selection path):

```
--- FAIL: TestBump_PackerCooldownOverride (0.01s)
    bump_test.go:384: bumped project not selected despite bump cooldown elapsed (3h > 7200s)
--- FAIL: TestBump_MultiPoolPackOverride (0.01s)
    bump_test.go:444: ns-bumped urgency = 20.834220211513845, want >= bump tier
--- FAIL: TestVerify_PackerOverride_PackFlatPath (0.01s)
    schedgap107_verify_test.go:207: bumped project not selected by packFlat despite bump cooldown elapsed (3h > 7200s) — override missing from the flat selection path
```

### M2 — Phase A auto-revert skipped on the final tick

- Mutation: `bump.go` `if remaining > 1` → `if remaining > 0` (countdown decrements
  to zero but never performs the final-tick revert; `bumpTickCompleted` returns false forever).
- Result (3 tests FAIL):

```
--- FAIL: TestBump_AutoRevert (0.01s)
    bump_test.go:132: bump still active after final tick
--- FAIL: TestVerify_TwoPhaseRevert_FinalTickReturnsTrueAndRestores (0.01s)
    schedgap107_verify_test.go:53: final bump tick: bumpTickCompleted = false, want true (Phase A performed)
--- FAIL: TestVerify_TwoPhaseRevert_IdleResumesRestoredStreak (0.01s)
    schedgap107_verify_test.go:92: bump not reverted after 4 idle bump ticks
```

### M3 — ApplyFleetConfig bump-active pin skip removed

- Mutation: `loader.go` `if existing != nil && existing.BumpActive` → `if existing != nil && false`
  (regen re-pins cooldown from fleet.toml over the active bump — the original judge-found bug).
- Result:

```
--- FAIL: TestApplyFleetConfig_BumpActivePreservesCooldown (0.01s)
    schedgap107_test.go:73: CooldownS after regen = 43200, want 7200 (bump owns cooldown while active; regen must not clobber)
```

### M4 — countEligibleProjects mirror override disabled

- Mutation: `loop.go` `if bumpActive == 1 && bumpCD > 0` → `if false && …` (the
  eligibility mirror ignores the bump cooldown).
- Result:

```
--- FAIL: TestVerify_EligibleMirror_UsesBumpCooldown (0.01s)
    schedgap107_verify_test.go:244: countEligibleProjects = 0, want 1 (bump cooldown 7200s elapsed overrides stored 86400s)
```

- Note: this mutation is what forced the test to be honest. The first version
  of the mirror test PASSED with the override removed (BumpProject writes the
  bump value into `cooldown_s`, so a fresh bump masks the missing override);
  only the mid-bump-escalated state (stored 86400 ≠ bump 7200) exposes it —
  the state the override exists for.

## Reproduce

```
cd ~/coding-hermes-scheduler/coding-herms-scheduler
go test -count=1 -short -run 'Bump|Verify_' ./internal/...   # the SCHED-GAP-107 surface
go test -count=1 -short -p 1 ./...                            # full sequential gate
```

Observed (fresh run, `-short -p 1`, full gate `./...`): all 11 packages ok —
cmd/migrate, cmd/schedulerd, internal/{api, blocks, config, dashboard,
database, mcp, scheduler, sync, version} — 0 failures. New tests: 3 (DB) + 6
(API, incl. 4 boundary subtests) + 7 (scheduler). `go build ./...` ok,
`go vet ./internal/...` clean, `golangci-lint run ./internal/...` → 0 issues,
`gofmt -l` clean for all touched files.
RED re-runs: apply the one-line mutation above in a scratch worktree, run the
named test, observe the FAIL line quoted above, discard the worktree. Four
mutations were performed (M1–M4); all four produced the expected RED, and no
mutation remains in the committed tree.

## Bugs found in SCHED-GAP-107 itself

None. Every guard under test held; the four mutations above each produced the
expected RED. Two latent observations (not bugs): (1) an empty-body POST /bump
returns 400 via the JSON decode error ("invalid JSON: EOF") rather than the
missing-reason message — correct status, adequate diagnostics; (2) a FRESH
bump writes its cooldown into `cooldown_s`, which masks a missing override in
readers that use `cooldown_s` — tests for the override must simulate the
mid-bump adaptive-escalated state (stored ≠ bump) to be mutation-sensitive
(discovered via M4; all override tests in this suite do).
