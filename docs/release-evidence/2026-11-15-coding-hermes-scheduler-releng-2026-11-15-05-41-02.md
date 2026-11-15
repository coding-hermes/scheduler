# Release readiness — coding-hermes/scheduler — 2026-11-15

**Current certified candidate: `origin/main` `4eebeeb70ca86ac53cfdab6de2ce8e2a7a9d84e1` (tree `3040b437`), exact-SHA hosted CI terminal green. Decision unchanged: NO-CUT — see "Refire pass (same tick, pass 3)" below. The first-pass text that follows is preserved unedited as history.**

## Decision

NO-CUT. The next proposed release remains v1.17.0 (minor), but the exact candidate has no hosted CI runs, no candidate-bound GitReins verdict, and no candidate-exact dogfood/QA receipt. The required release gates are therefore unverified. No tag, GitHub Release, artifact publication, deployment, or changelog mutation was performed.

## Identity and candidate

- Repository: coding-hermes/scheduler
- Canonical checkout: /home/kara/coding-hermes-scheduler/coding-herms-scheduler
- Remote: https://github.com/coding-hermes/scheduler.git
- Baseline: v1.16.0 peeled commit 28a676686e1f93db901c53b3e1408729574ea7e1
- Candidate: origin/main 9135fadca82d5bf1dfd7079e64fadac22f16103d
- Candidate tree: isolated clean detached worktree at the full candidate SHA
- Candidate status: product tree is current, but the canonical checkout is a shared dirty/stale carrier (ahead 1, behind 38, modified board files); no release mutation was attempted there
- Existing v1.17.* tag: absent from the remote
- Existing latest Release: v1.16.0, published 2026-11-09T22:57:35Z, 8 binaries plus SHA256SUMS

## Commit census and semver

Command: git rev-list --no-merges --count v1.16.0..origin/main

Result: 173 non-merge commits.

Strict subject classifier: lowercase Conventional Commit type, optional scope, optional `!`, then `:`; breaking footers counted only at line start (`BREAKING CHANGE:` or `BREAKING-CHANGE:`). Counts: feat 2, fix 8, perf 7, other conventional 155, non-conventional 1. Breaking subjects/footers: 0. The resulting bump is MINOR, proposed v1.17.0. The material subjects are the two feat commits, eight fix commits, and seven perf commits; duplicate recovery subjects are preserved in the census and must be represented once per user-visible change in release notes.

The current [1.17.0] section was prepared at b37a1f690c0995a2d4f1e2f94f35536233c691a9. Twelve post-prep material commits are listed by SHA in the section, but b01954ad612b4a740bf8db184691fbac1109880e is not covered. Changelog coverage is incomplete; RELEASE-060 remains pending.

## Gates

| Gate | Evidence | Result |
|---|---|---|
| Version authority | `bash scripts/check-version-authority.sh v1.17.0` on clean candidate | PASS |
| Release prep | `bash scripts/release-prep.sh --check v1.17.0` | PASS; nothing to do |
| Build | `GOTOOLCHAIN=local go build ./...` on candidate | PASS, rc 0 |
| Vet | `GOTOOLCHAIN=local go vet ./...` on candidate | PASS, rc 0 |
| Short tests | `GOTOOLCHAIN=auto go test -short -count=1 ./...` on candidate | PASS, rc 0 |
| Board invariant | `python3 ops/check-fleet-invariants.py --board-only` | PASS, 0 violations |
| Vulnerability scan | `GOTOOLCHAIN=auto govulncheck ./...` | PASS, rc 0, no vulnerabilities found after toolchain auto-selected go1.26.11; the host-local go1.26.5 scan is not release evidence and was superseded by this run |
| Exact candidate hosted CI | GitHub Actions API query for head_sha=9135fadca82d5bf1dfd7079e64fadac22f16103d | UNVERIFIED; 0 runs and 0 check-runs |
| Nearby CI | Parent 7402ad07: CI 43684151172 and CI Pipeline 43684150666 success; parent is not the candidate and is not substituted for exact-head evidence | INFORMATIONAL only |
| GitReins | No verdict artifact in `.gitreins/history` bound to 9135fadca82d5bf1dfd7079e64fadac22f16103d; latest relevant stored verdicts are historical or bound to other commits | UNVERIFIED |
| Dogfood/QA | Latest dogfood log is historical through 2026-11-12 and current pending QA rows do not name the candidate SHA | UNVERIFIED |
| Deployment | `/api/v1/health` HTTP 200, build_sha 9b1fdd22, version v1.16.0-58-g9b1fdd22-dirty; live binary is stale relative to candidate | STALE; owned by SCHED-GAP-162 / RELEASE-051 |

## Release tooling and artifact plan

Repository-owned tooling is present and must be used: `make release-prep VERSION=v1.17.0`, then the explicitly separate `make tag VERSION=v1.17.0` path. `scripts/release-prep.sh` refuses unsafe or incomplete version preparation and does not tag. `scripts/release-claim.sh` prevents concurrent release writers. `.github/workflows/release.yaml` triggers on pushed `v*` tags, builds eight CGO-disabled Linux/Darwin amd64/arm64 binaries, runs version and VCS-stamp checks, creates SHA256SUMS, and publishes the GitHub Release. There is no Dockerfile and no GoReleaser configuration; no container artifact applies.

## Open blockers

- Exact candidate hosted CI is absent, not green; do not tag.
- No non-degraded GitReins verdict is bound to the candidate.
- No candidate-exact dogfood/QA evidence is bound to the candidate.
- CHANGELOG coverage is incomplete for the post-prep material range; RELEASE-060 owns the reconciliation.
- The deployed daemon is stale and dirty; ownership remains with SCHED-GAP-162 / RELEASE-051.
- RELEASE-059 and RELEASE-060 remain pending even though their implementation work is present in the published history; their exact current-head evidence must be re-read before closure.

## Mutation record

This pass created no tag and no GitHub Release, did not force-move any ref, did not change the product version, and did not deploy or restart the service. The readiness artifact and rolling RELEASE-057 board evidence are the only intended release-engineering mutations.

---

# Refire pass (same tick, pass 3) — 2026-11-15T07:15Z

Tick `coding-hermes-scheduler-releng-2026-11-15-05-41-02` fired a third time. This is a RE-FIRE, not a new readiness pass, and everything below was re-measured at the current remote tip rather than inherited from the text above.

## Certified candidate and material identity

- Certified candidate: `origin/main` `4eebeeb70ca86ac53cfdab6de2ce8e2a7a9d84e1`, tree `3040b4379c84b29633709e1886bb6ef0264185a2`.
- Prior candidate named above: `9135fadca82d5bf1dfd7079e64fadac22f16103d`, tree `ba70c0ae6b508dab88d9f2d95a80b11e866ae6b2`.
- Tree entry proof (`git ls-tree` on both): **38 entries each, identical top-level entry names**, and the only differing objects are the `.coding-hermes` and `docs` subtrees. Path diff `9135fadc..4eebeeb7` = 3 files, all bookkeeping: `.coding-hermes/board/events.jsonl`, `.coding-hermes/board/tasks.jsonl`, `docs/release-evidence/2026-11-15-…md`. Product paths changed: **0**.
- The move is therefore bookkeeping-only, which licenses carrying the first pass's census, build, and security receipts — but every structured field below was re-read at the new candidate, and the CI receipt is new.

## Why the CI story changed

The prior pass recorded the candidate as UNVERIFIED because `9135fadc` had zero workflow runs and zero check-runs. Re-queried now: still `actions/runs?head_sha=9135fadc…` = **0**, and `commits/9135fadc…/check-runs` = **0** — that SHA was pushed inside a multi-commit push whose head received the run, so it never had a run of its own.

Its bookkeeping-only descendant `4eebeeb7` **was** graded, and both required workflows are now terminal:

| Workflow | Run | head_sha | Conclusion | Required jobs |
|---|---|---|---|---|
| CI | 43686761270 | 4eebeeb7 | success | race-scheduler, race-blocks, build, lint, multiarch Verify (amd64 + arm64 native), 6 cross-builds — all success; `multiarch / Container (multi-arch)` and `multiarch / Release` skipped (tag-gated release jobs, expected) |
| CI Pipeline | 43686760678 | 4eebeeb7 | success | Lint, Test (Go 1.26), Race Detector (internal/api), Integration Test, Build Binaries, Simulation Smoke Test, CI All Clear — all success, none skipped |

Exact-SHA hosted CI at the certified candidate is therefore **GREEN**, not UNVERIFIED. This is the one gate that improved since the first pass.

## Gates re-run at `4eebeeb7`

All commands from the clean detached worktree at the full candidate SHA; exit codes captured by redirection, not by pipeline.

| Gate | Command | Result |
|---|---|---|
| Version authority | `bash scripts/check-version-authority.sh v1.17.0` | rc 0 — `PASS — CHANGELOG.md names v1.17.0; [Unreleased] present exactly once` |
| Release prep | `bash scripts/release-prep.sh --check v1.17.0` | rc 0 — `already names 1.17.0 — nothing to do` |
| Build | `go build ./...` | rc 0 |
| Vet | `go vet ./...` | rc 0 |
| Short tests | `env -u SCHEDULER_OPERATOR_RESTART_APPROVED GOTOOLCHAIN=auto go test -short -count=1 ./...` | rc 0 — 26 packages `ok`, 0 `FAIL` |
| Board invariant (board-only) | `python3 ops/check-fleet-invariants.py --board-only` | rc 0 — `PASS — 0 violation(s)`, 151 open rows / 151 unique |
| Vulnerability scan | `GOTOOLCHAIN=auto govulncheck ./...` with `go version go1.26.11` | rc 0 — `No vulnerabilities found.` (`GO-2026-6859` is closed by the now-selected toolchain) |
| Tag absence | `git tag -l 'v1.17*'`, `git ls-remote --tags origin 'refs/tags/v1.17*'` | empty — v1.17.0 has never been tagged |
| Release absence | `gh release list` | newest is v1.16.0 (2026-11-09T22:57:35Z) |
| GitReins | `.gitreins/history/` newest directory is `2026-09-19`; no verdict names `4eebeeb7` or `9135fadc` | **UNVERIFIED** |
| Dogfood/QA | `.coding-hermes/dogfood-log.md` is historical (2026-11-12); no battery receipt names the candidate SHA | **UNVERIFIED** |
| Deployment | `/api/v1/health` 200 — `build_sha 9b1fdd22`, `version v1.16.0-58-g9b1fdd22-dirty`, uptime 80h29m | **STALE** — 126 commits behind the candidate; owned by SCHED-GAP-162 / RELEASE-051 |

## Census at the certified candidate

`git rev-list --no-merges --count v1.16.0..4eebeeb7` = **175** non-merge commits (184 including merges). Strict subject classifier (`^([a-z][a-z0-9-]*)(\([^)]*\))?(!)?: `) plus line-start breaking footers:

feat 2 · fix 8 · perf 7 · board 50 · chore 38 · docs 25 · ops 15 · foreman 13 · qa 8 · dogfood 4 · pm 2 · build 1 · gitreins 1 · non-conventional 1 (`QA tick coding-hermes-scheduler-qa-2026-11-13-10-07-07: annotate 8 rows, no new defects`). Buckets sum to 175. Breaking `!` subjects: 0. Line-start `BREAKING CHANGE:` footers: 0.

**v1.17.0 MINOR remains the correct bump.** +2 commits since the first pass's 173, both bookkeeping (`docs(releng)` evidence + `board(releng)` carrier).

## Correction: the CHANGELOG gap is wider than previously recorded

The first pass named only `b01954ad` as uncovered. Re-deriving coverage by SHA against `CHANGELOG.md` at the certified candidate, `b01954ad` **is** now cited (the PERF-008 observatory-entry), but **five material commits are cited nowhere in the file** — not by SHA, and their addresses/behaviour appear under no description either (`grep -c` = 0 for `SCHED-429-001`, `SCHED-FAILCNT-001`, `SCHED-GAP-17007`, `SCHED-GAP-16983`, `reply_to`, `harness failure`):

| Commit | Type | Subject | Product paths |
|---|---|---|---|
| `8d3be7f6` | fix | fix(scheduler): defer gateway 429 self-saturation (SCHED-429-001) | internal/api/server.go, internal/api/server_helpers.go, internal/scheduler/gateway_client.go |
| `80e8361a` | fix | fix(scheduler): conservative admission skip classification; perpetual fixtures excluded (SCHED-GAP-17007-M1) | internal/scheduler/admission_preflight.go, internal/scheduler/nowork.go |
| `0197ef25` | feat | feat(scheduler): admission-time cheap skip for tasks-mode lanes with no actionable rows (SCHED-GAP-17007-M1) | internal/api/server.go, internal/scheduler/{admission_preflight,loop,nowork}.go |
| `b8fd3c42` | feat | feat(dispatch): scheduler-dispatch emits reply_to and text from config; correlation keys named (SCHED-GAP-16983-A) | cmd/scheduler-dispatch/main.go, internal/bus/query.go |
| `384fafec` | fix | fix(scheduler): repair harness failure accounting (SCHED-FAILCNT-001) | internal/scheduler/failureclass.go, ops/repair-consecutive-failures.py |

All five landed 2026-11-10, are **not** ancestors of `v1.16.0`, and touch shipped product code. The `[1.17.0]` "Not in this release" note currently asserts that everything outside the twelve prep-range commits is "board, harness, QA, releng, documentation, or operational bookkeeping" — that sentence is false for these five and must be corrected as part of the reconciliation. **RELEASE-060's premise survives and is wider than its title states**; it is the single hard product-content blocker left.

Covered material commits (verified present): `05253993`, `a86b16bf`, `00430d6b`, `c8fac45c`, `ffff7175`, `233fcf28`, `97d0b822`, `405ba9c4`, `4018dbd2`, `4854241e`, `45a1b4cf`, `b01954ad` = 12 of 17.

## Blocker premise re-check (fetched board, not prior prose)

- **RELEASE-058** (bump Go toolchain to 1.26.11) — **premise resolved, row still pending.** `git show 4eebeeb7:go.mod` carries `toolchain go1.26.11`, bumped by `b6b6606c` ("build: update Go toolchain to 1.26.11 for GO-2026-6859"), and `govulncheck ./...` at the certified candidate reports no vulnerabilities. Flagged for foreman close, not closed here.
- **RELEASE-059** (normalise PERF-007 / PERF-001-R3-R4 `status=failed`) — **premise resolved, row still pending.** `grep -c '"status":"failed"'` on the tip `tasks.jsonl` = **0**; both rows are `pending`; the board-only invariant gate passes with 0 violations. Its own `foreman_note` already cites worker commit `ec79d638` / merged tree `d3257e86`.
- **RELEASE-060** (reconcile `[1.17.0]`) — **premise persists, scope widened** to the five commits above; its `foreman_note` cites `0b346791`/`10ed8367`/`d3257e86`, which reconciled the twelve prep-range commits only.
- **RELEASE-051 / SCHED-GAP-162** — deployment staleness unchanged (126 commits behind, dirty build).

## Decision

**NO-CUT for v1.17.0.** The hosted-CI arm is now green and the local gate set is fully green, but RELEASE-057's acceptance requires three things that are still not met: complete changelog coverage for every material commit (five uncovered, above), a non-degraded GitReins verdict bound to the same SHA (none exists anywhere in `.gitreins/history`), and candidate-exact dogfood/QA evidence (the newest battery is 2026-11-12 and names no candidate SHA). A green badge does not substitute for those, so the cut stays closed.

Unblock path, in order: (1) foreman reconciles `[1.17.0]` for the five commits and corrects the "Not in this release" sentence (RELEASE-060); (2) a dogfood/QA battery or GitReins judge run is bound to the then-current candidate; (3) releng re-runs this sweep and, if every arm is green, uses the repository-owned `make release-prep VERSION=v1.17.0` → `make tag VERSION=v1.17.0` path — never a bare `gh release create`.

## Mutation record (this pass)

No tag created, no GitHub Release, no ref force-moved, no version surface or changelog edited, no deploy or restart, no product code touched. Workers dispatched: 0 of a wave budget of 2 — the findings this pass are verification and reconciliation, not fan-out work. The only intended mutations are this artifact and the rolling RELEASE-057 carrier.
