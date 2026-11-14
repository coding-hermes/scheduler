# Scheduler release readiness — 2026-11-11

Certified candidate (code-certified): 9a790e97073e54de9ea609e203790f03b32864ed (`origin/main`, GitHub repository `coding-hermes/scheduler`); carrier tip: evidence and board carriers are being prepared from a clean detached worktree. This header supersedes the prior edd679a4 (Pass 5) certification; historical pass bodies remain preserved below. Canonical remote: `https://github.com/coding-hermes/scheduler.git`; default branch: `main`; fetched branch, local HEAD, and `git ls-remote` agree at this SHA.

Baseline: `v1.16.0`, annotated tag object `9049119de442ea15043dc1b46fa891a2cbd49393`, peeled commit `28a676686e1f93db901c53b3e1408729574ea7e1`. Remote tag and Release object agree; the v1.16.0 Release is published, non-draft, non-prerelease, with 8 binaries plus `SHA256SUMS`.

Decision: v1.17.0 minor is due but NOT CUT. The range `v1.16.0..9b1fdd227da01122d3e162ca80c3c53aead9229a` contains 58 commits, 56 non-merge and 2 merges. Conventional census: 2 `feat`, 3 `fix`, 0 `perf`, 53 other; no `BREAKING CHANGE` footer or type-`!` declaration was found. The material features are `0197ef25` and `b8fd3c42`; material fixes include `8d3be7f6`, `80e8361a`, and `384fafec`, with additional operational fixes represented in the prepared changelog. Semver therefore remains MINOR: v1.17.0.

Version surface: the repository-owned prep has already landed at `b37a1f690c0995a2d4f1e2f94f35536233c691a9` (`chore(release): prepare v1.17.0 changelog for RELEASE-056`). `CHANGELOG.md` names v1.17.0 and contains exactly one `[Unreleased]` heading. `scripts/check-version-authority.sh v1.17.0` passes on the certified tree. Re-running `scripts/release-prep.sh --dry-run v1.17.0` correctly refuses with `VERSION_HEADING_EXISTS`; no version-prep mutation was made.

CI evidence: the prep SHA `b37a1f690c0995a2d4f1e2f94f35536233c691a9` has exact-SHA terminal green runs `43014634750` (`CI`) and `43014633887` (`CI Pipeline`). The recorded jobs include lint, build, vet, full tests, race detectors, integration, simulation, end-to-end scheduler verify, and platform builds; tag-only Container and Release jobs were skipped on this non-tag push. The certified current SHA `9b1fdd227da01122d3e162ca80c3c53aead9229a` has runs `43097719178` (`CI`) and `43097718378` (`CI Pipeline`) still queued with zero jobs, and the commit check-runs API returns `total_count: 0`. These runs are more than a minutes-old queue stall and are UNPROVEN, not green. The exact-head CI gate blocks the cut. The current delta from the prep SHA is 8 commits touching only board files, `.gitreins/tasks.yaml`, and `docs/lane-authority.md`; this does not convert the current exact-SHA gate into green.

Repository release tooling: `Makefile` provides `release-prep` and `tag`; `scripts/release-prep.sh` promotes the changelog without tagging; `scripts/release-claim.sh` is the single-writer claim; `docs/releases.md` documents the mechanism. `.github/workflows/release.yaml` owns publication on `push` tags `v*`, builds 8 static cross-platform Go assets and `SHA256SUMS`, and must not be raced by a manual Release. No Dockerfile or GoReleaser configuration exists.

Build evidence at the certified tree: a clean local `go test -short -count=1 ./...` passed across all packages. A local `make BINDIR=<scratch> build` passed, but the Makefile's build path produced dynamically linked Linux binaries because it is a development build; the release workflow's `CGO_ENABLED=0` cross-build was separately reproduced for 8 binaries. The reproduced assets comprise 8 binaries plus `SHA256SUMS` (9 files), all checksum-verifying; Linux assets are statically linked; `schedulerd-linux-amd64 --version` and `migrate-linux-amd64 --version` report `v1.17.0` with the certified short commit. These are strategy evidence only, not published assets.

GitReins: no verdict file is bound to the certified SHA. The latest relevant historical record is the RELEASE-055 repair, which is now complete on the GitReins repository; no candidate-bound non-degraded scheduler verdict was found. This leg is UNVERIFIED for the current candidate and is not substituted with the prior candidate's evidence.

Dogfood/QA: no current exact-SHA dogfood record was found for `9b1fdd227da01122d3e162ca80c3c53aead9229a`. The latest repository dogfood material is historical and predates current scheduler/dispatch changes; it is not a waiver. QA rows `QA-CHS-190`, `QA-CHS-196`, and `QA-CHS-197` remain open, including unavailable lint/setup and bunker launch/watchdog infrastructure. Dogfood is UNVERIFIED for this candidate.

Dependencies/security: `go.mod` uses Go 1.26.10. `govulncheck ./...` exits 3 and reports reachable standard-library vulnerability `GO-2026-6859` in `net/http@go1.26.10`, fixed in Go 1.26.11; it reports no other vulnerabilities. This is a release blocker until the toolchain is updated and re-gated, or an explicit security disposition is recorded. `go list -u -m` also reports available updates for pprof, x/exp, x/mod, x/sync, x/sys, x/text, and modernc components; no dependency mutation was made.

Deployed staleness: `GET http://127.0.0.1:9090/api/v1/health` is HTTP 200 and reports build SHA `9b1fdd22`, but the running `bin/schedulerd` is a dirty development build (`v1.16.0-58-g9b1fdd22-dirty`) rather than a release artifact. The host also contains `/usr/local/bin/schedulerd`, a stale root-owned binary from the prior RELEASE-051 finding. The live deployment gap remains owned by `SCHED-GAP-162`; the root-owned shadow binary remains `RELEASE-051`. No daemon restart or root-owned mutation was attempted.

Open blockers and unblock path:

1. Exact-head CI runs `43097719178` and `43097718378` are queued with zero jobs; obtain terminal green conclusions on the certified SHA, or re-run after the queue defect is resolved.
2. `govulncheck` finds `GO-2026-6859`, fixed in Go 1.26.11; update the toolchain and re-run the full hosted gates, or record an owner-approved security disposition.
3. No candidate-bound GitReins verdict or dogfood result is available; run and bind both to the exact release candidate.
4. Deployment remains stale under `SCHED-GAP-162`; this is carried forward and does not authorize a release.

No tag, GitHub Release, deployment, product-code change, or board mutation was made by this sweep.

## Pass 2 — delta re-delivery at the current tip (decision unchanged: NO-CUT)

The releng lane fired again for the same tick after pass 1 was pushed. `origin/main` moved two commits (`b7a5e202` docs + `7ee83d11` board, both authored by the pass-1 sweep itself), so this pass re-anchors every count and receipt to the new tip rather than re-appending the pass-1 numbers.

Certified candidate (revised): `7ee83d1106146540a2e752f70f2052f94554ba4e`. Certified by `git rev-parse origin/main` after `git fetch origin main`, and the commit was materialised in a clean detached carrier worktree outside the shared checkout.

- Range: `v1.16.0..7ee83d11` = **62 commits** (`git rev-list --count`; was 58 at pass 1 — the two added commits are this lane's own evidence and board records).
- Delta `9b1fdd22..7ee83d11` = 3 files: `.coding-hermes/board/events.jsonl`, `.coding-hermes/board/tasks.jsonl`, and this evidence file. **Zero product-source files changed**, so pass 1's build and test evidence describes the same product tree.
- Conventional census unchanged: 2 `feat`, 3 `fix`, 0 `perf`, no `BREAKING CHANGE` footer and no type-`!` declaration → **v1.17.0 minor is still the correct bump**.
- Changelog coverage: material commits after prep `b37a1f69` = **0**, and all five material range commits (`0197ef25`, `b8fd3c42`, `8d3be7f6`, `80e8361a`, `384fafec`) are cited by full SHA in the `[1.17.0]` section. The coverage question is discharged and must not be carried forward.
- Version authority: `scripts/check-version-authority.sh v1.17.0` → PASS at the certified tree; `[Unreleased]` appears exactly once.

### Blocker 1 from pass 1 (exact-head CI) — DISCHARGED

The certified candidate has **terminal green exact-SHA CI**, read per job, not per run:

- Run `43118765966` (`CI`): `lint`, `build`, `race-blocks`, `race-scheduler`, `multiarch / Verify (amd64)`, `multiarch / Verify (arm64, native)`, and builds for `linux/amd64`, `linux/arm64`, `darwin/amd64`, `darwin/arm64`, `windows/amd64`, `windows/arm64` — all `success`. `Container (multi-arch)` and `Release` are `skipped`, which is the tag-only path on a non-tag push.
- Run `43118764312` (`CI Pipeline`): `Lint`, `Test (Go 1.26)`, `Race Detector (internal/api)`, `Build Binaries`, `Integration Test`, `Simulation Smoke Test`, `CI All Clear` — all `success`.

Pass 1 named runs `43097719178` / `43097718378` (SHA `9b1fdd22`) as queued with zero jobs. Those two are still queued and are now permanent queue orphans, but the stall was transient: every push after them received a terminal conclusion (`60ed9c65`, `452c3876`, `5dcf51d4`, `ca2d918e`, `9d6b332b` variants; `4d460e08` → `43117295124` success, `1c80f17c` → `43118094163` success, `b7a5e202` → `43118753590` success, `7ee83d11` → both runs success). The orphan pair is a runner-queue hygiene observation, not a release gate, because the certified tree is green at its own SHA.

### Blocker 2 from pass 1 (GO-2026-6859) — STILL LIVE, and now the only hard blocker

`govulncheck ./...` re-run at the certified tree exits **3** with exactly one reachable vulnerability:

- `GO-2026-6859` — `net/http@go1.26.10`, **Fixed in:** `net/http@go1.26.11`. `go.mod` still declares `toolchain go1.26.10` and the toolchain has not moved since pass 1. No other vulnerabilities are reported in imported or required packages.

A reachable standard-library vulnerability in the HTTP stack of a scheduler whose control plane is HTTP is a hard release blocker. Unblock path filed as **RELEASE-058** (bump the Go toolchain to 1.26.11, re-run the full hosted gates, re-probe `govulncheck`), so this does not need an owner disposition unless the owner prefers to waive it explicitly.

### Blocker 3 from pass 1 (GitReins verdict, dogfood) — UNCHANGED / UNVERIFIED

No verdict file is bound to `9b1fdd22` or `7ee83d11`. The only recent verdict in the repository is the `SCHED-429-001` PASS recorded against worktree `bc2b5ebe`, which is a task verdict, not a candidate-bound release verdict. No exact-SHA dogfood record exists; `docs/dogfood/*` stops at 2026-10-27 and predates the 1.17.0 material changes. Both legs remain UNVERIFIED for this candidate.

### Blocker 4 from pass 1 (deployed staleness) — UNCHANGED

`GET http://127.0.0.1:9090/api/v1/health` → HTTP 200 with `build_sha` `9b1fdd22`, i.e. the running binary is a development build, not a release artifact. The live deployment gap remains `SCHED-GAP-162`; the root-owned shadow binary remains `RELEASE-051`. Deployment is not authorized by this pass.

### Tag / Release surface

`git ls-remote --tags origin 'refs/tags/v1.17*'` returns empty — **v1.17.0 is absent**. The newest Release object remains `v1.16.0` (`2026-11-09T22:57:35Z`). Neither a tag nor a Release was created, moved, or deleted.

### Decision

v1.17.0 minor remains **DUE and NOT CUT**. One hard blocker (`GO-2026-6859` on toolchain go1.26.10) plus two unverified evidence legs (candidate-bound GitReins verdict, exact-SHA dogfood). Once RELEASE-058 lands and the resulting SHA is green with both evidence legs bound to it, the repository-owned path owns publication: `make release-prep VERSION=v1.17.0` then an annotated tag, with `.github/workflows/release.yaml` publishing on `push` of tags matching `v*` — a manual `gh release create` must not race it.

This pass mutated exactly two durable things: this evidence file and one `audit` event on `RELEASE-057`. No second readiness row was filed, no tag, Release, deployment, or product-code change was made.

## Pass 3 — verification-only delta at carrier tip `4324a580` (decision unchanged: NO-CUT)

The releng lane fired again on the same tick id after pass 2 was pushed. `origin/main` moved three
commits since pass 2's certified `7ee83d11`: `9375950f` (this lane's pass-2 evidence doc), `a7af6b56`
(board: `RELEASE-058` + `RELEASE-057` audit event) and `4324a580` (board: `SCHED-GAP-17012`, filed by a
sibling lane). The path set `7ee83d11..4324a580` is three files — `.coding-hermes/board/events.jsonl`,
`.coding-hermes/board/tasks.jsonl`, `docs/release-evidence/<this file>` — and the **material
(non-bookkeeping) path count is 0**. The product tree is byte-identical to the pass-2 certified tree, so
the pass-1/pass-2 census and build evidence CARRY rather than being re-derived; only the gates a new tip
invalidates were re-measured.

- Code-certified candidate: `7ee83d1106146540a2e752f70f2052f94554ba4e` (unchanged).
- Carrier tip: `4324a58004bf7b4d28a0c25c6be1d8c6cb47252e` (`git rev-parse origin/main` after `git fetch`).
- Range `v1.16.0..4324a580` = **65 commits** (`git rev-list --count`; 62 at pass 2 — the delta is this
  lane's own bookkeeping plus one sibling board row).
- Conventional census unchanged: 2 `feat`, 3 `fix`, 0 `perf`, no `BREAKING CHANGE` footer, no type-`!`
  declaration → **v1.17.0 MINOR is still the correct bump**.
- Tag/Release surface: `git ls-remote --tags origin 'refs/tags/v1.17*'` is empty; the newest Release
  object is still `v1.16.0` (`2026-11-09T22:57:35Z`). No pass created, moved or deleted a tag or Release.

### Blocker 1 — `GO-2026-6859` STILL LIVE (re-measured at the carrier)

`govulncheck ./...` at `4324a580` exits **3**:

```
Vulnerability #1: GO-2026-6859
  Found in: net/http@go1.26.10
  Fixed in: net/http@go1.26.11
```

`go.mod` still declares `toolchain go1.26.10` and the lane's host toolchain is `go1.26.10`. The unblock
work item **`RELEASE-058`** (bump to Go 1.26.11, re-run the hosted gates, re-probe `govulncheck`) is
still `pending`; nothing has landed for it. A reachable stdlib vulnerability in the HTTP stack of an
HTTP control plane is a hard release blocker, so this pass does not authorize a cut.

### Blocker 2 — exact-SHA CI at the carrier: IN PROGRESS / UNPROVEN

Runs `43123331819` (`CI`) and `43123330353` (`CI Pipeline`) for carrier `4324a580` are `in_progress`
(created `2026-11-11T23:54Z`) — neither terminal, therefore neither green nor red, and not evidence.
The code-certified candidate keeps its own terminal green receipt from pass 2 — run `43118765966`
(`CI`) and run `43118764312` (`CI Pipeline`) at `7ee83d11`, with all required jobs started and
successful (`Container`/`Release` skipped = tag-only path). That receipt is **not** transferred to the
carrier; candidate CI and carrier CI are separate receipts.

### Blocker 3 — candidate-bound GitReins verdict and dogfood: UNVERIFIED (unchanged)

No verdict file is bound to `7ee83d11` or to `4324a580`; the only recent verdict in the repository is
the task verdict `SCHED-429-001` against worktree `bc2b5ebe`. No exact-SHA dogfood record exists
(`docs/dogfood/*` stops at 2026-10-27 and predates the 1.17.0 material changes). Both legs stay
UNVERIFIED for this candidate and are not substituted with the prior candidate's evidence.

### Blocker 4 — deployed-binary staleness: UNCHANGED

`GET http://127.0.0.1:9090/api/v1/health` → HTTP 200, `build_sha` `9b1fdd22`, i.e. a development build
roughly two code commits behind the code-certified candidate and behind the un-cut release. The live
deployment gap remains `SCHED-GAP-162`; the root-owned shadow binary remains `RELEASE-051`. No daemon
restart or root-owned mutation was attempted.

### Decision

v1.17.0 remains **DUE and NOT CUT**, for the same single hard blocker as pass 2 plus the same two
unverified evidence legs. The cut becomes possible only when `RELEASE-058` lands (`go.mod` toolchain
≥ `go1.26.11`), the resulting SHA carries terminal green exact-SHA CI, and candidate-bound GitReins and
dogfood evidence exist. Publication is owned by the repository's own path — `make release-prep
VERSION=v1.17.0`, then `make tag VERSION=v1.17.0` — with `.github/workflows/release.yaml` publishing on
`refs/tags/v1.*` push; a manual `gh release create` must not race it.

Mutations this pass: this evidence file, a note/evidence refresh on `RELEASE-057`, and one `audit` event
on `RELEASE-057`. No new readiness row, no tag, no Release, no deployment, no product-code change.

## Pass 4 — daily readiness at candidate b01954ad (NO-CUT)

The remote default branch advanced materially from the prior code-certified tree. The pass therefore re-derived the decision rather than carrying the prior census. The candidate is `b01954ad612b4a740bf8db184691fbac1109880e`; the previous pass-3 carrier was `4324a58004bf7b4d28a0c25c6be1d8c6cb47252e`.

- Range `v1.16.0..b01954ad` contains 88 commits; 2 `feat`, 3 `fix`, 1 `perf`, and 82 other non-merge commits. No `BREAKING CHANGE:` footer or type-`!` declaration was found. The material delta is the existing five feature/fix commits plus `b01954ad` (`perf: file PERF-008 …`), so the proposed bump remains `v1.17.0` MINOR.
- The six material product commits are cited in `CHANGELOG.md` by full SHA except the new `b01954ad` performance finding. Release-note coverage is therefore incomplete for the new material commit; no prep mutation was made in this verification-only lane.
- Version authority remains ready: `scripts/check-version-authority.sh v1.17.0` PASS; `[Unreleased]` is present exactly once; `scripts/release-prep.sh --check v1.17.0` PASS.
- Candidate CI is mixed at the time of measurement. `CI` run `43366181153` is terminal `success` at the exact SHA; all code/test/race/platform jobs are green, with tag-only Container and Release jobs skipped. `CI Pipeline` run `43366180350` is still `in_progress` at the exact SHA; Test and Race Detector jobs are active. Exact-head CI is therefore UNPROVEN and blocks the cut until that workflow is terminal green.
- Repository-owned release tooling remains authoritative: `make release-prep`, `make tag`, `scripts/release-prep.sh`, `scripts/release-claim.sh`, and tag-triggered `.github/workflows/release.yaml`. The workflow owns publication and produces eight static Go binaries plus `SHA256SUMS`; no Dockerfile or GoReleaser configuration exists. No tag or Release was created.
- Dependency/security re-check at the candidate: `go.mod` declares `toolchain go1.26.11`; `go version` is `go1.26.11 linux/amd64`; `govulncheck ./...` exits 0 with `No vulnerabilities found.` The prior GO-2026-6859 blocker is discharged by the landed toolchain update, but the candidate still requires exact-head CI and release-evidence legs.
- GitReins: no `.gitreins/history/**/verdict.json` names the candidate SHA. Candidate-bound GitReins evidence is UNVERIFIED.
- Dogfood/QA: no current dogfood or release-evidence record names the candidate SHA. The latest dogfood record is the 2026-11-12 battery: fresh install and tests passed, lint was INFO because `golangci-lint` was unavailable, several late cells were UNVERIFIED on the first agent, and the second run completed with harness-class CI red while hosted CI for its older SHA was green. This is historical evidence, not a candidate waiver.
- Deployed staleness: `GET http://127.0.0.1:9090/api/v1/health` returned HTTP 200 with `build_sha=9b1fdd22`, `version=v1.16.0-58-g9b1fdd22-dirty`; the running daemon is not the candidate or a release artifact. No restart or root-owned mutation was attempted.

Decision: v1.17.0 remains DUE and NOT CUT. The exact-head `CI Pipeline` run is still in progress, and candidate-bound GitReins and dogfood evidence are absent. The required next gates are terminal success for run `43366180350`, a GitReins verdict bound to `b01954ad612b4a740bf8db184691fbac1109880e`, and a candidate-exact dogfood/QA receipt. Nothing was tagged, published, deployed, or developed by this sweep.

## Pass 5 — daily readiness at candidate edd679a4 (NO-CUT)

Identity and baseline: `origin/main`, local HEAD, and `git ls-remote origin refs/heads/main` all resolve to `edd679a4ef87f26ebc6e8549671828e70376059c`. Baseline is `v1.16.0` (peeled commit `28a676686e1f93db901c53b3e1408729574ea7e1`). Remote `refs/tags/v1.17*` is empty and `gh release view v1.17.0` reports `release not found`.

- Range `v1.16.0..edd679a4` contains 132 commits / 125 non-merge commits. Census reconciles: `feat=2`, `fix=8`, `perf=7`, `nonmaterial=106`, `other=2`, total `125`; no breaking marker. Proposed bump remains `v1.17.0` MINOR.
- Changelog coverage is incomplete for newly landed material changes: the v1.17.0 section cites the earlier feature/fix SHAs but not post-prep material work including `c8fac45`, `ffff7175`, `233fcf28`, `97d0b822`, `4018dbd2`, and the `05253993` retention tool path. No changelog mutation was made.
- Version authority passes for v1.17.0; the repository-owned path remains `make release-prep`, then `make tag`, with tag publication owned by `.github/workflows/release.yaml`. It builds eight static Go binaries plus `SHA256SUMS`; no Dockerfile or GoReleaser configuration exists.
- Exact candidate CI is unproven: `gh run list --repo coding-hermes/scheduler --commit edd679a4...` returns no runs; check-runs and check-suites both return `total_count=0`. The two head commits carry `[ci skip]`. Product paths are byte-identical to the last fully executed green SHA `8b21658d099f8f8aa99aa39657855c328ded8d6d`, whose CI runs `43546150329` and `43546149704` were terminal success with required jobs green. This conditional identity evidence does not become current-SHA green CI.
- Security is clear at the candidate: Go `1.26.11`, `go.mod` toolchain `1.26.11`, and `govulncheck ./...` exits 0 with `No vulnerabilities found.` GO-2026-6859 is discharged.
- GitReins is not release-grade: no verdict is bound to `edd679a4`; latest relevant verdicts are `PERF-007` at `a3369593` passed=false and `PERF-009` at `f7ab1f0b` degraded/failed. Both remain unresolved.
- Dogfood/QA is unverified: no receipt names `edd679a4`; latest QA includes harness disk-starvation findings QA-CHS-201/202. Historical evidence is not substituted.
- Deployment is stale: live health reports `build_sha=9b1fdd22`, `version=v1.16.0-58-g9b1fdd22-dirty`; no restart or deployment mutation was attempted. Ownership remains SCHED-GAP-162 / RELEASE-051.

### Known issues

PERF-007 remains GitReins FAIL with live-copy/dashboard/metrics/latency acceptance unverified. PERF-009 remains degraded/failed with no candidate-bound verdict. QA-CHS-201/202 remain pending harness disk-starvation findings. SCHED-GAP-162 and RELEASE-051 remain stale deployment findings.

Decision: v1.17.0 remains DUE and NOT CUT. Current-SHA CI is unproven, changelog coverage is incomplete, candidate-bound GitReins is not passing, and candidate-exact dogfood/QA is absent. No tag, Release, deployment, changelog, or product-code mutation was made by this sweep.

## Pass 6 — re-delivery at tip 9a790e97: exact-SHA CI is RED (decision unchanged: NO-CUT)

Identity and baseline: `origin/main`, the detached carrier worktree HEAD, and `git ls-remote origin refs/heads/main` all resolve to `9a790e97073e54de9ea609e203790f03b32864ed` (commit date `2026-11-14 00:12:36 -0500`, subject `board(releng): refresh RELEASE-057 for 2026-11-14 readiness pass`; no `[ci skip]` in the message). Baseline remains `v1.16.0` (annotated tag `9049119de442ea15043dc1b46fa891a2cbd49393`, peeled commit `28a676686e1f93db901c53b3e1408729574ea7e1`). Remote `refs/tags/v1.17*` is empty and `gh release view v1.17.0` reports `release not found`.

This pass re-derives the release layers at the new tip instead of quoting Pass 5. Two things changed since Pass 5 certified `edd679a4`: the tip advanced by two releng bookkeeping commits (`6bb24e8d`, `9a790e97`), and the exact-SHA CI question that Pass 5 had to record as unproven is now answered — with a FAILURE.

- Range `v1.16.0..9a790e97` contains 134 commits / 127 non-merge commits. Census reconciles: `feat=2`, `fix=8`, `perf=7`, `chore=28`, `docs=18`, `build=1`, `other=63`, total `127`; no `BREAKING CHANGE` footer and no type-`!` declaration. Material-path commits (excluding `.coding-hermes/`, `.gitreins/`, `docs/`, `CHANGELOG.md`, `README.md`) number 14. Proposed bump remains `v1.17.0` MINOR and is not in question this pass.
- **B1 — exact-SHA CI is RED.** Run `43560452603` (workflow `CI`) at `9a790e97` concluded `failure` in job `build` at step `Invariant fixture battery (SCHED-GAP-147)`. The gate output is two board-vocabulary violations:
  - `VIOLATION board-vocab PERF-007: status=failed`
  - `VIOLATION board-vocab PERF-001-R3-R4: status=failed`

  Reproduced locally against the tip's own board blob: `python3 ops/check-fleet-invariants.py --board-only --board <tip>/.coding-hermes/board/tasks.jsonl` reports exactly those two violations over `901 row(s) scanned`, `0 legacy closed spelling(s)`, `FAIL — 2 violation(s)`. The red is therefore deterministic board data, not flaky infrastructure.
- Attribution of B1: `failed` is a TICK status (`internal/database/models.go`, `StatusFailed TickStatus = "failed"`), not a board status. The closed writer vocabulary is `pending`, `in_progress`, `blocked`, `complete`, `duplicate` (`internal/scheduler/board_vocab.go`, `BoardAllowedStatuses`, mirrored by check 8 of `ops/check-fleet-invariants.py` and pinned in lockstep by `TestBoardVocabValidator_AllowedSetMatchesPythonGate`). Ownership evidence: `git show 8b21658d:.coding-hermes/board/tasks.jsonl` carries zero occurrences of the compact `"status":"failed"` spelling, while `c1a71bbf` (`chore: close recovered scheduler wave [ci skip]`, `2026-11-13 21:50:40 -0500`) is the commit that introduced both rows — and it carried `[ci skip]`, so no CI run ever graded it. The Pass 5 releng carrier inherited the rows unchanged and was the first non-skipped commit to expose them.
- Companion pipeline: run `43560451658` (workflow `CI Pipeline`) at the same SHA was `in_progress` at read time — `Lint` `success`, `Test (Go 1.26)` and `Race Detector (internal/api)` still running. Its terminal conclusion is not yet evidence; the `CI` workflow failure above already decides the gate.
- Conditional identity, not a substitute: the last fully executed green SHA is `8b21658d099f8f8aa99aa39657855c328ded8d6d` (runs `43546150329`, `43546149704`, both terminal `success`). Product paths between `8b21658d` and the tip are identical to that SHA's product paths (the intervening commits are board/docs carriers), but that equivalence does not become current-SHA green CI, and it cannot launder B1 because the failing gate reads the board file those commits changed.
- **B2 — changelog coverage is incomplete.** The `[1.17.0]` section was authored at `b37a1f69` (`chore(release): prepare v1.17.0 changelog for RELEASE-056`, `2026-11-11 06:44:54 -0500`) covering `v1.16.0..04ec8f88`. Twelve material commits have landed since, and none of them is cited in the section: `05253993`, `a86b16bf`, `00430d6b`, `c8fac45c`, `ffff7175`, `233fcf28`, `97d0b822`, `405ba9c4`, `4018dbd2`, `4854241e`, `45a1b4cf`, `b01954ad`. Note the duplicate-subject clusters (`05253993`/`a86b16bf` retention tool; `97d0b822`/`405ba9c4`/`4018dbd2` waiver-state restart; `4018dbd2` and companions), so the reconcile row must itemise unique changes, not commit count. No changelog mutation was made by this pass.
- Version authority passes for the proposed tag: `bash scripts/check-version-authority.sh v1.17.0` → `PASS — CHANGELOG.md names v1.17.0; [Unreleased] present exactly once`.
- Repo-owned release path is present and is what a cut must use: `Makefile` targets `release-prep` (line 42) and `tag` (line 46); `.github/workflows/release.yaml` triggers on `tags: ['v*']`, runs `scripts/check-version-authority.sh "${GITHUB_REF#refs/tags/}"`, then builds eight static Go binaries plus `SHA256SUMS`. No `Dockerfile` and no GoReleaser configuration exist, so no image build applies.
- **B3 — no candidate-bound verification evidence.** No `.gitreins/history/**/verdict.json` names `9a790e97`; the latest relevant verdicts remain `PERF-007` at `a3369593` `passed=false` and `PERF-009` at `f7ab1f0b` degraded. No dogfood or QA record names the tip either; the newest battery is the 2026-11-12 run with harness-class red. Historical evidence is not substituted for candidate-bound evidence.
- **B4 — deployed binary is stale.** `go version -m ./bin/schedulerd` reports module `v1.16.1-0.20261111204314-9b1fdd227da0+dirty`; `GET /api/v1/health` returns `build_sha=9b1fdd22`, `build_time=2026-11-11T22:20:06Z`. That build is 58 commits after `v1.16.0` and 76 commits behind the tip. No restart, rebuild, or deployment mutation was attempted; ownership remains with the deployment findings (SCHED-GAP-162 / RELEASE-051).

### Known issues

B1 is the load-bearing blocker: every future commit that does not carry `[ci skip]` will fail the same gate until the two rows are normalised, so the tip cannot produce green CI and no tag may be cut on it. B2 would publish stale notes if a cut were forced. B3 leaves the change-set uncertified by an independent grader. B4 means no user-visible improvement ships on a tag alone. PERF-007 remains a GitReins FAIL and PERF-009 remains degraded; QA-CHS-201/202 remain pending harness disk-starvation findings.

Decision: v1.17.0 remains DUE and NOT CUT. The exact-SHA `CI` gate is RED for a reproducible board-data defect, changelog coverage is incomplete for twelve post-prep material commits, candidate-bound GitReins and dogfood evidence are absent, and the deployed binary is 76 commits stale. No tag was created, no Release was published, no binary was deployed, and no product, changelog, or board-status mutation was made by this sweep.

Push-time identity, recorded for the next pass: between this sweep's read and its push, `origin/main` advanced from `9a790e97` to `68789d63` (`board: audit fleet watcher health 2026-11-14`, a board-only carrier touching `.coding-hermes/board/events.jsonl` by one audit event). The delta contains no product path, so every finding above stands unchanged at the new tip; the first push attempt was rejected as non-fast-forward and the carriers were rebuilt on `68789d63` instead of forced, so the two board rows this pass filed carry the higher event ids.

Next gates, in order: (1) normalise `PERF-007` and `PERF-001-R3-R4` to an in-vocabulary status that preserves their failed outcome in evidence, then confirm `board-only` invariant check exits 0 and a fresh non-skipped commit's `CI` run is terminal `success` (RELEASE-059); (2) reconcile the `[1.17.0]` section against the twelve post-prep material commits (RELEASE-060); (3) bind a GitReins verdict and a dogfood receipt to the post-fix SHA; (4) only then run `make release-prep`, `make tag`, and let `release.yaml` publish.

<!-- ch:trace row=RELEASE-057 spec=board:RELEASE-057 wave=coding-hermes-scheduler-releng-2026-11-14-04-57-03#pass-6 test=python3 ops/check-fleet-invariants.py --board-only --board <tip board>; bash scripts/check-version-authority.sh v1.17.0 doc=docs/release-evidence/2026-11-11-coding-hermes-scheduler-releng-22-49-24.md evidence=gh run view 43560452603 --log-failed witness=internal/scheduler/board_vocab.go verdict=none:releng-verify-only commit=carrier-sha-in-board-row memory=none:duckbrain-releng-key STATUS: complete TIER: T1 EVIDENCE: CI run 43560452603 failure log plus local invariant reproduction -->
