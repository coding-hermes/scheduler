Release readiness evidence — coding-hermes/scheduler
Tick: coding-hermes-scheduler-releng-2026-11-06-14-41-12
Measured: 2026-11-06

Identity
- Canonical checkout: /home/kara/coding-hermes-scheduler/coding-herms-scheduler
- Remote: https://github.com/coding-hermes/scheduler.git
- Default branch: main
- origin/main and local HEAD: fad18a9aace857bd192290e91df4661868bf756d
- Prior release: v1.14.0 -> a95f695b9d2add441fd2d01b88ac10423fa254a5
- Existing v1.14.0 tag and Release were not modified.

Range and decision
- Range v1.14.0..fad18a9a: 8 commits, paths are .coding-hermes/board/events.jsonl, .coding-hermes/board/tasks.jsonl, and .qa/chaos-probes/errorpath.sh.
- The only non-board commit is 8f38f82e, test(qa): add real schedulerd errorpath probe. It is test/QA coverage, not shipped product behavior.
- Conventional census: 0 feat, 0 fix, 0 perf, 1 test, 0 breaking; all remaining commits are board/harness bookkeeping. The census reconciles to the range's 8 commits when merge/bookkeeping subjects are included.
- Decision: NOT DUE / NO CUT. Decision-1 zero material shipped-source commits applies. No new version is proposed, no tag is created, and no Release object is created.

CI
- The latest build-input commit 8f38f82e has exact-SHA green CI: CI Pipeline 42257856311 and CI 42257857696.
- CI Pipeline jobs all completed successfully: Lint, Test (Go 1.26), Race Detector (internal/api), Simulation Smoke Test, Integration Test, Build Binaries, CI All Clear.
- CI jobs all completed successfully: race-scheduler, multiarch Verify amd64/arm64, race-blocks, lint, build, and six cross-build jobs. Tag-gated Container/Release jobs were skipped by design.
- Current HEAD fad18a9a is a board-only [ci skip] carrier over the green 8f38f82e code tree; no exact-SHA run exists for fad18a9a. This does not change the not-due decision, but it would block a release cut unless a new candidate receives required CI or is proven code-equivalent at cut time.

GitReins and dogfood
- Latest 2026-11-06 verdicts are passed=true for QA-CHS-187 at 8f38f82e (bf5c5ade) and 992fdcb9 (5a7e765c), PERF-006 at 72bd5758 (e8d34435), and PERF-006-R1 at a7b6c7cf (618e6a44).
- No fresh full-tree non-degraded verdict is bound to fad18a9a; this is classified as unavailable, not PASS.
- Current dogfood evidence exists for the 2026-11-06 lane-hook run, but it is measured at older HEAD 8c4745d1 and is not exact-candidate evidence. It reports SCHED-GAP-16993 and PERF-006 as live findings; those rows already exist.

Tooling and artifact posture
- Repository-owned release procedure exists: docs/releases.md, Makefile release-prep/tag targets, scripts/release-prep.sh, and .github/workflows/release.yaml.
- Go release workflow builds 8 static binaries plus SHA256SUMS and publishes a GitHub Release. No release artifact was built for this not-due sweep.
- v1.14.0 remains published, non-draft/non-prerelease, with 9 assets and green tag runs 42255965516 and 42255965924.
- Open release findings remain RELEASE-037 (7/8 v1.14.0 binaries report vcs.modified=true) and RELEASE-038 (local toolchain floor go1.26.6 with reachable stdlib advisories). RELEASE-041 remains open for empty-[Unreleased] sequencing.

Deployment
- coding-hermes-scheduler.service is inactive, while a schedulerd process is running from the checkout at commit 8f38f82e and reports dev-8f38f82e. This is not a published release binary and is stale relative to v1.14.0; deployment freshness remains an operational finding already tracked by SCHED-GAP-16994.

Board validation
- boardctl validate --fail-on dangling-dep: RESULT: OK (914 tolerated legacy warnings).
- Evidence file is repository-local and is intended to be committed with the RELEASE-* row.
