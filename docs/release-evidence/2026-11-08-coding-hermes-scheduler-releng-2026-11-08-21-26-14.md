# Release readiness evidence — coding-hermes/scheduler

Tick: coding-hermes-scheduler-releng-2026-11-08-21-26-14
Measured: 2026-11-08 (UTC-05)
Repository: https://github.com/coding-hermes/scheduler.git
Canonical root: /home/kara/coding-hermes-scheduler/coding-herms-scheduler
Default branch: main

## Candidate identity

- HEAD: e7a9cc53f72d221b4b1addd680db8b203568493e
- origin/main: e7a9cc53f72d221b4b1addd680db8b203568493e
- git ls-remote origin refs/heads/main: e7a9cc53f72d221b4b1addd680db8b203568493e
- Working tree: clean
- Prior release: v1.15.2 -> commit 40373714bcc142ecd4989c3ffbe9aaf8882b0af4
- Range: v1.15.2..e7a9cc53f72d221b4b1addd680db8b203568493e
- Non-merge commits in range: 37
- Path delta includes shipped source and tests; this is not a bookkeeping-only delta.

## Semver census

The 37 non-merge commits reconcile as:

- feat: 3 -> minor signal
- fix: 7 -> patch signal
- perf: 0
- chore: 1
- docs: 0
- refactor: 0
- test: 2
- board: 17
- qa: 4
- pm: 2
- foreman: 1
- BREAKING CHANGE or conventional type!: 0

Material source commits include feat(ops) consume hop-liveness alerts, feat(board-stale) log BOARD-STALE at pick, feat(wakes) persisted wake tables, and seven fix commits across ops, lanehook, and policy-guard. Proposed bump: minor, v1.16.0. This is not a major candidate.

## Release tooling and version hygiene

Repository-owned surfaces found:

- scripts/release-prep.sh and Makefile target release-prep
- Makefile target tag, protected by release-claim.sh
- .github/workflows/release.yaml, tag trigger v*
- docs/releases.md
- CHANGELOG.md

The repository has no Dockerfile, Goreleaser configuration, or package manifest. The release workflow builds eight static Go binaries (schedulerd and migrate for linux/darwin × amd64/arm64), creates SHA256SUMS, and publishes the GitHub Release.

Current version state:

- CHANGELOG has exactly one empty ## [Unreleased] heading and the newest release heading is ## [1.15.2] — 2026-11-07.
- bash scripts/release-prep.sh --check v1.16.0: REASON=VERSION_HEADING_EXISTS; exit 2, with the driver explaining that CHANGELOG.md does not yet name 1.16.0.
- bash scripts/check-version-authority.sh --structure-only: PASS; the tree structure is valid, but it does not prepare v1.16.0.

Readiness consequence: the candidate requires repo-owned version preparation (populate [Unreleased], then promote it with make release-prep VERSION=v1.16.0), a new exact-SHA CI cycle, and only then tag/release operations. This readiness sweep did not edit the changelog or tag.

## Hosted CI

Exact candidate SHA e7a9cc53f72d221b4b1addd680db8b203568493e:

- CI run 42514986121: completed/success. Required jobs completed/success: build, lint, race-scheduler, race-blocks, multiarch Verify amd64, multiarch Verify arm64 native, and six platform builds. Tag-only Container and Release jobs were skipped by their conditions.
- CI Pipeline run 42514985433: completed/success. All seven jobs completed/success: Test (Go 1.26), Race Detector (internal/api), Lint, Build Binaries, Integration Test, Simulation Smoke Test, and CI All Clear.

URLs:
- https://github.com/coding-hermes/scheduler/actions/runs/42514986121
- https://github.com/coding-hermes/scheduler/actions/runs/42514985433

These runs certify the current source candidate, not a future changelog-prep commit. The prep commit must have its own exact-SHA green runs before a cut.

## GitReins and dogfood

- Recent repository verdicts include PASS verdicts for c475e951, a98c03b9, ead9d113, 14e5a9bf, fb65a9a3, ed05e7c7, and dc7388c5. The latest PASS artifact a76dcf81 is bound to c475e951. A recent FAIL/INCOMPLETE verdict 95350568 is bound to 88447e2d and concerns a historical guard-provenance criterion; it is not the current candidate. No fresh full release verdict is bound to e7a9cc53.
- Dogfood evidence exists in .coding-hermes/dogfood-log.md and records prior operator runs, including a 2026-11-06 lane-hook run with a red act leg and a historical 2026-10-27 install run with ci-pass FAIL. No dogfood result in the repository is bound to the current candidate. Current dogfood status: UNVERIFIED for this release candidate, not PASS.

## Deployed surface

GET http://127.0.0.1:9090/api/v1/health returned HTTP 200 JSON with:

- build_sha: d0dad6d9
- version: v1.14.0-40-gd0dad6d9-dirty
- build_time: 2026-11-07T05:42:56Z
- db: connected

The listener is schedulerd PID 3347652 on 127.0.0.1:9090. The loaded build is stale against the candidate and carries a dirty build marker. This is an operational deployment finding, owned by existing SCHED-GAP-162 / RELEASE-051 work; it is not duplicated here and does not alter source-CI readiness.

## Previous release and public posture

- v1.15.2 exists remotely as an annotated tag on 40373714 and a non-draft, non-prerelease GitHub Release with 9 assets (8 binaries plus SHA256SUMS). Prior release evidence records checksum and build provenance verification.
- Repository is public and MIT licensed.
- security_and_analysis: secret_scanning enabled, secret_scanning_push_protection enabled, dependabot_security_updates enabled; secret_scanning_non_provider_patterns and secret_scanning_validity_checks disabled. Existing RELEASE-052 owns the latter two.
- main protection: required_pull_request_reviews requires one approval and code-owner review; allow_force_pushes=false; enforce_admins=false; required_status_checks=null.

## Decision

MINOR RELEASE DUE under the release policy because at least one feat commit landed since v1.15.2 and exact current-head hosted CI is green. No major/breaking confirmation is needed. No tag or Release object was created by this readiness sweep because the repository-owned version-prep/changelog step is not yet satisfied and the future prep commit has not been CI-certified.

Foreman unblock sequence: populate and promote CHANGELOG v1.16.0 using the repository driver; push the prep commit; obtain terminal green CI on that exact SHA; rerun release-prep/version authority and release-surface checks; then use the repository tag-triggered release workflow. Do not manually create a competing Release object while the tag workflow is available.
