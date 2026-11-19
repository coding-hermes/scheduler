# Release readiness — coding-hermes/scheduler — 2026-11-19 (pass 5)

**Decision: NO-CUT.** The next release remains **v1.17.0 (minor)**, but the tip is **RED in CI** for a real
product-code reason. No tag was created, no GitHub Release was published, no artifact was built for
publication, nothing was deployed or restarted, and no version surface was modified. The only mutations are this
evidence document and the board rows named at the end.

- Tick: `coding-hermes-scheduler-releng-2026-11-19-00-58-11` (pass 5 of the rolling readiness chain)
- Ready-at: 2026-11-19T01:06Z
- Previous pass: `coding-hermes-scheduler-releng-2026-11-15-05-41-02` (pass 4, NO-CUT)

## Identity

| Item | Value |
|---|---|
| Repository | `coding-hermes/scheduler` (GitHub; client is `gh`) |
| Canonical checkout | `/home/kara/coding-hermes-scheduler/coding-herms-scheduler` |
| Remote | `https://github.com/coding-hermes/scheduler.git` |
| Baseline tag | `v1.16.0` → peeled commit `28a676686e1f93db901c53b3e1408729574ea7e1` |
| Candidate | `origin/main` `d2d8eef017e0970b88531966bf870f042777ddc9` (tree `d7610de5a4b8c3cb18fdac02e3a77e13e26ebf2a`) |
| Checkout position | local `HEAD` == `origin/main` (0 ahead / 0 behind) |
| Existing `v1.17.*` tag | absent from the remote |
| Latest Release | `v1.16.0`, published 2026-11-09, 8 binaries + `SHA256SUMS` |

The canonical checkout carries two foreign uncommitted writes that this pass did **not** author and did **not**
commit: a CoS annotation on `SCHED-GAP-17027` inside the board file, and a `.gitreins/tasks.yaml` status flip.
Gates were therefore run in a **clean detached worktree** at the exact candidate SHA
(`/home/kara/.hermes/cache/scratch/releng-d2d8eef`), not in the shared checkout.

## Commit census and semver

Command (strict subject classifier, lowercase Conventional Commits, breaking footers counted only at line start):

```
python3 scripts/conventional-commit-census.py <repo> 28a6766 d2d8eef
```

| Bucket | Count |
|---|---|
| non-merge commits in range | **233** |
| `feat` | 3 |
| `fix` | 15 |
| `perf` | 8 |
| `docs` / `chore` / `test` / `build` / other conventional | 204 |
| non-conventional | 2 |
| breaking subjects | 0 |
| breaking footers | 0 |
| unrecognized types (fleet bookkeeping) | `board` 85, `ops` 15, `foreman` 13, `qa` 9, `dogfood` 7, `pm` 2, `settle` 1, `releng` 1 |

Sum check 233 = 233. Rule outcome: **MINOR → v1.17.0**. No breaking change, so no major bump and no owner
confirmation was required this pass.

## Gates at the exact candidate

All commands run in the clean detached worktree at `d2d8eef0`.

| Gate | Evidence | Result |
|---|---|---|
| Version authority | `bash scripts/check-version-authority.sh v1.17.0` | **PASS** rc0 — CHANGELOG names v1.17.0, `[Unreleased]` present exactly once |
| Release prep | `bash scripts/release-prep.sh --check v1.17.0` | **PASS** rc0 — "already names 1.17.0 — nothing to do" |
| Build | `GOTOOLCHAIN=local go build ./...` | **PASS** rc0 |
| Vet | `GOTOOLCHAIN=local go vet ./...` | **PASS** rc0 |
| Short tests | `GOTOOLCHAIN=auto go test -short -count=1 ./...` | **PASS** — 27 packages `ok`, 0 `FAIL` |
| Lint (CI gate) | `golangci-lint run ./...` (v2.15.0) | **FAIL** — 1 issue: `cmd/scheduler-query/main.go:257:6: func usageExit is unused (unused)` |
| Board invariant | `python3 ops/check-fleet-invariants.py --board-only` | **PASS** — 0 violations (942 rows scanned) |
| Vulnerability scan | `GOTOOLCHAIN=auto govulncheck ./...` | **PASS** — "No vulnerabilities found" |
| Exact-candidate hosted CI | Actions API `head_sha=d2d8eef…` | **RED** — CI `44317585852` failure, CI Pipeline `44317583144` failure |
| GitReins | `.gitreins/history` scan for the candidate | **ABSENT** — no verdict names `d2d8eef0`, `60a30051` or `3a527f30`; newest verdict binds `12f4a02e` |
| Dogfood / QA | `docs/dogfood/` recency | **ABSENT** — newest entry 2026-10-20, no candidate SHA |
| Deployment | live `GET /api/v1/health` | **CURRENT** — `build_sha d2d8eef0`, built 2026-11-18T23:33:55Z; no staleness |

### The red is a product regression, not infrastructure

Full-repo lint returns **exactly one** finding — nothing is hidden behind CI's annotation cap. The finding's
provenance is unambiguous:

| Commit | `usageExit` call sites | State |
|---|---|---|
| `3a527f30` (last green, CI `44178681584` / `44178680431` success) | 6 — lines 106, 111, 116, 121, 124, 127 | referenced |
| `60a30051` (fix: flags interspersed with positionals) | 4 references remained | referenced |
| `d2d8eef0` (fix: missing flag value fails with message alone) | **0** — definition retained at line 257 | **dead code → lint FAIL** |

So `d2d8eef0` removed the last caller of its own helper and left the helper defined. `3a527f30` is an ancestor
of the tip, which confines the red to the final two commits. The CI workflow pins
`golangci/golangci-lint-action@v7` with `version: latest`, but an unused-symbol fact is not version-sensitive;
it reproduced locally at v2.15.0.

Process note: the gitreins task covering the `scheduler-query` work was marked **complete** at
2026-11-18T13:49:45Z, 39 seconds after the red push. A gitreins completion is a working-tree judgement and is
**not** candidate-CI evidence — it did not catch this red.

## Changelog coverage

The `[1.17.0]` section was authored at `b37a1f69` on 2026-11-11. In range `v1.16.0..d2d8eef0` there are **30**
material (`feat`/`fix`/`perf`) commits; **18** are not cited by SHA anywhere in `CHANGELOG.md`, including
genuinely shipped behaviour added after the section was written:

- `0197ef25` feat(scheduler): admission-time cheap skip for tasks-mode lanes with no actionable work
- `b8fd3c42` feat(dispatch): scheduler-dispatch emits `reply_to` and `text` from config
- `90465a9a` feat(lanehook): `{session}` placeholder for the tick's Hermes session id
- `308829d5` fix(scheduler): closed `failure_reason` vocabulary for dry ticks
- `062be060` fix(scheduler): surface aborted resume failures
- `384fafec` fix(scheduler): repair harness failure accounting
- `8d3be7f6` fix(scheduler): defer gateway 429 self-saturation
- `80e8361a` fix(scheduler): conservative admission skip classification
- `837005ef` / `a3ff734c` fix(scheduler): shutdown/abort semantics
- `60a30051` / `d2d8eef0` fix(scheduler-query): flag parsing and missing-value message
- `4923aa96` perf: recalibrate status latency bar
- `75a68040` fix: complete PERF-007 retention acceptance
- `b6b6606c` build: Go toolchain 1.26.11 for GO-2026-6859

The remainder are a QA tick note and a gitreins lifecycle commit. Coverage is therefore materially incomplete,
and the release would ship three undocumented features. Owner row: **RELEASE-060** (still pending, persists).

## Release tooling and artifact plan

Repository-owned tooling exists and takes precedence over generic `gh release create`:

- `make release-prep VERSION=v1.17.0` → `scripts/release-prep.sh` (refuses unsafe/incomplete preparation; does not tag)
- `scripts/release-claim.sh` → serialises concurrent release writers
- `scripts/check-version-authority.sh` → asserts the CHANGELOG names the tag (runs in CI before the build)
- `.github/workflows/release.yaml` → triggered by pushing a `v*` tag; builds eight CGO-disabled
  Linux/Darwin amd64/arm64 binaries **outside** the checkout (`$RUNNER_TEMP/dist`, per RELEASE-037), verifies
  version and VCS stamps, writes `SHA256SUMS`, and publishes the GitHub Release
- No `Dockerfile`, no GoReleaser config → no container artifact applies

Because the workflow triggers on the tag, the publication action for this repo is **`git push origin v1.17.0`**,
not `gh release create`. Cutting before the lint red is fixed would publish a release whose own tag-CI fails.

## Open blockers

| Row | Status | Premise at this pass |
|---|---|---|
| **RELEASE-062 (new)** | pending P1 | **BINDING.** Red CI at the tip from dead `usageExit`. Only unmet machine gate. |
| RELEASE-060 | pending P2 | Persists — 18 of 30 material commits uncited in `[1.17.0]`. |
| RELEASE-061 | pending P2 | Persists — CI still runs no `govulncheck`; the gate remains manual. |
| RELEASE-058 | pending P1 | **Resolved in tree** — `go.mod` pins `toolchain go1.26.11`, scan clean. Needs foreman close-out. |
| RELEASE-059 | pending P1 | **Resolved in tree** — 0 `status=failed` rows; invariant PASS. Needs foreman close-out. |
| RELEASE-051 | blocked P1 | **Resolved on the host** — `/usr/local/bin/schedulerd` no longer exists; live daemon runs `d2d8eef0`. Needs foreman close-out. |
| REL-GUARD-001 | blocked P2 | Forge tag-protection policy; does not gate this cut. |
| REL-GUARD-003 | — | Phantom, retired in pass 4; no such id exists anywhere. |

## Mutation record

- Created **RELEASE-062** (P1) — the lint/CI blocker, scoped to one file plus at most one test.
- Folded pass 5 into the rolling row **RELEASE-057**: candidate `d2d8eef0`, all gates re-measured, decision
  `NO_CUT`, `candidate_ci.status = red`, and blocker premises re-stated.
- Board write is append + one in-place row rewrite; **0 ids dropped**, 943 rows, invariant PASS.
- Created this evidence document.
- **Not** done: no tag, no release, no `git push` of any ref, no force-move, no version bump, no deploy or
  restart, no CHANGELOG edit.

## What unblocks the cut

Fix RELEASE-062 (one dead function), get both workflows green on that exact commit, then either close out
RELEASE-060's changelog reconciliation or accept it explicitly. At that point `make release-prep VERSION=v1.17.0`
plus the tag push is a mechanical cut.
