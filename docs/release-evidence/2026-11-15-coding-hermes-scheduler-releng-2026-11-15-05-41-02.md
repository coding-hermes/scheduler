# Release readiness — coding-hermes/scheduler — 2026-11-15

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
