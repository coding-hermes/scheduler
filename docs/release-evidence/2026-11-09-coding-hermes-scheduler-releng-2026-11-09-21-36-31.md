# Release readiness evidence — coding-hermes/scheduler

Tick: coding-hermes-scheduler-releng-2026-11-09-21-36-31
Measured: 2026-11-09 21:41 UTC
Repository: https://github.com/coding-hermes/scheduler.git
Default branch: main

## Candidate identity

- Candidate: 5af19558da7f7f14e9df8ee81f21452aa855ccf0
- origin/main and GitHub API main agree on the candidate SHA.
- `git rev-list --left-right --count origin/main...HEAD`: 0 0 in the clean detached candidate worktree.
- Baseline: v1.15.2 -> 40373714bcc142ecd4989c3ffbe9aaf8882b0af4.
- Range: v1.15.2..5af19558da7f7f14e9df8ee81f21452aa855ccf0.
- Existing v1.16.0 tag: absent locally and remotely.

## Commit census and decision

The range contains 68 commits / 62 non-merge commits. The non-merge conventional census reconciles to 62:

- feat: 3
- fix: 8
- perf: 0
- chore/docs/refactor/test/ci/build/style/revert: 6
- other or fleet bookkeeping subjects: 45
- BREAKING CHANGE footer or conventional type!: 0

Shipped-source path inventory across the range: 34 source paths, 6 documentation paths, 6 test paths, and 9 other paths outside `.coding-hermes` and `.gitreins`. Material examples include `feat(ops)`, `feat(board-stale)`, `feat(wakes)`, eight fixes, and the current routing-policy fix. The delta is not bookkeeping-only.

Decision: MINOR RELEASE DUE under the release policy because feat commits landed after v1.15.2. No major confirmation is required. The release is NOT CUT because the repository's required version-preparation invariant is not satisfied.

## Repository release contract

Repository-owned tooling is present and is authoritative:

- `scripts/release-prep.sh` and `make release-prep`
- `make tag`, protected by `scripts/release-claim.sh`
- `scripts/check-version-authority.sh`
- `docs/releases.md`
- `.github/workflows/release.yaml`

The repository has no Dockerfile, GoReleaser configuration, npm manifest, or Python package manifest. The tag-triggered workflow builds eight static Go binaries (schedulerd and migrate for linux/darwin x amd64/arm64), generates `SHA256SUMS`, and publishes the GitHub Release. No generic `gh release create` fallback was used.

Version checks on the exact candidate:

- `scripts/check-version-authority.sh --structure-only`: PASS; exactly one `[Unreleased]`, newest release heading `## [1.15.2] — 2026-11-07`.
- `scripts/release-prep.sh --check v1.16.0`: exit 2, `REASON=VERSION_HEADING_EXISTS`; the driver says CHANGELOG does not yet name 1.16.0 and instructs `make release-prep VERSION=v1.16.0`.

The existing `[Unreleased]` body is empty. The foreman must populate it from the range, promote it with the repository driver, push the prep commit, and obtain exact-SHA green CI before tagging.

## Hosted CI

Exact candidate SHA 5af19558da7f7f14e9df8ee81f21452aa855ccf0 has two terminal green runs:

- CI: run 42687256767, completed/success. Build, lint, race-scheduler, race-blocks, both native verification jobs, and six platform builds completed/success. Tag-only Container and Release jobs were completed/skipped by their conditions.
- CI Pipeline: run 42687255968, completed/success. Test (Go 1.26), API race detector, lint, simulation smoke, integration, build binaries, and CI All Clear completed/success.

The green runs certify the current source candidate. They do not certify a future CHANGELOG version-prep commit.

## GitReins and dogfood

- A fresh `gitreins guard --full --scope working-tree --json` on the clean candidate returned top-level `outcome=pass` but `metadata.degraded=true`, `changedFileCount=0`, and skipped `go_build`, `go_lint`, and `go_tests` because no files were in scope. This is not fresh non-degraded release evidence.
- The repository's committed verdicts contain historical PASS results, but no fresh full non-degraded verdict bound to 5af19558da7f7f14e9df8ee81f21452aa855ccf0.
- Dogfood is UNVERIFIED for this candidate. The latest dogfood run is bound to 2ca34321; the candidate has subsequent shipped-source changes in `internal/scheduler/multipool_packer.go` and `internal/scheduler/session_resume.go`.

## Previous release and deployed surface

The v1.15.2 GitHub Release is published, non-draft, non-prerelease, and has 9 assets: 8 binaries plus `SHA256SUMS`. The prior evidence path records checksum and provenance verification.

The live scheduler listener at 127.0.0.1:9090 answered HTTP 200, but reports build_sha `d0dad6d9`, version `v1.14.0-40-gd0dad6d9-dirty`, and build_time `2026-11-07T05:42:56Z`, while the service is active under PID 3347652. The deployed surface is stale and remains owned by SCHED-GAP-162 / RELEASE-051; this sweep does not restart it.

## Public posture

The repository is public and MIT licensed. GitHub reports secret scanning, push protection, and Dependabot security updates enabled; non-provider secret patterns and validity checks remain disabled under RELEASE-052. Main protection reports one required approval and code-owner review, force pushes disabled, admin enforcement disabled, and no required status checks. These posture findings do not block this source release decision.

## Release state and unblock sequence

No tag, GitHub Release, or release artifact was created by this readiness sweep. The candidate is a minor release candidate, but release is blocked by version preparation and the absence of fresh non-degraded release verdict/dogfood evidence.

Foreman sequence: populate and promote CHANGELOG v1.16.0 with the repository-owned driver; push the prep commit; verify terminal green CI on that exact full SHA; run a fresh non-degraded release guard on the prep tree; then tag v1.16.0 with the repository's claim-protected path. The tag-triggered workflow is the sole publisher. Verify the remote annotated tag, non-draft Release, all 9 assets, checksums, `vcs.revision`/`vcs.modified=false` on all 8 binaries, and both tag-triggered workflows before closing the readiness row.
