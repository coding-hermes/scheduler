# Release readiness evidence — coding-hermes/scheduler

Tick: `coding-hermes-scheduler-releng-2026-11-14-04-57-03`

## Candidate

- Repository: `coding-hermes/scheduler`
- Canonical remote: `https://github.com/coding-hermes/scheduler.git`
- Default branch: `main`
- Code-certified candidate: `ecbd780e3bafb088e903735a3403a77865d2a610`
- Baseline: `v1.16.0` → `28a676686e1f93db901c53b3e1408729574ea7e1`
- `v1.17.0` tag and GitHub Release: absent
- The stand-in checkout was two commits behind before fetch; all measurements use the fetched remote candidate.

## Conventional-commit census

Range `v1.16.0..ecbd780e3bafb088e903735a3403a77865d2a610`:

- 141 total commits; 135 non-merge commits; class sum 135.
- `feat`: 2; `fix`: 8; `perf`: 7; `docs`: 20; `chore`: 28; `build`: 1; `test`: 0; `ci`: 0; `style`: 0; `refactor`: 0; `revert`: 0; other: 69.
- Breaking subjects/footers: 0.
- Proposal: MINOR `v1.17.0`. No major confirmation is required because no breaking marker exists.
- Seventeen `feat`/`fix`/`perf` subjects are present in the full range. The twelve post-prep material changes remain absent from the prepared `[1.17.0]` section; `RELEASE-060` remains open.

## Version and release tooling

- `scripts/release-prep.sh`, `scripts/check-version-authority.sh`, and the `make release-prep`/`make tag` targets are present.
- `scripts/check-version-authority.sh --repo <candidate-tree> v1.17.0` returned PASS; `[Unreleased]` occurs exactly once.
- The repository-owned path is `make release-prep` followed by `make tag`. `make tag` requires a clean tree and `HEAD == origin/main`, then uses `scripts/release-claim.sh` before creating and pushing an annotated tag.
- `.github/workflows/release.yaml` is tag-triggered and builds eight `CGO_ENABLED=0` static binaries plus `SHA256SUMS`.
- No Dockerfile or GoReleaser configuration exists.

## CI exact-head evidence

- CI run `43564804779` (`CI`) is in progress overall, but its build job `132356178180` is terminal FAILURE at `Invariant fixture battery (SCHED-GAP-147)`. The log reports exactly two violations: `PERF-007 status=failed` and `PERF-001-R3-R4 status=failed`; the fixture battery itself reports `245 passed, 4 skipped`.
- CI Pipeline run `43564804021` is in progress; `Lint` is successful while `Test (Go 1.26)` and `Race Detector (internal/api)` are still running.
- Exact candidate check-runs total: 8. Check-suites total: 2. This is not green and cannot clear the release gate.
- The previous carrier's runs (`43562525981` failure and `43562525419` success at `68789d6353e8d438237842b259c0fd6fe8bf23dc`) do not transfer to this candidate.

## GitReins and dogfood

- No `.gitreins/history` verdict is bound to the candidate or current carrier. Latest relevant historical verdicts are `a3369593` (PERF-007, `passed=false`) and `f7ab1f0b` (PERF-009, degraded/failed). Candidate-bound GitReins evidence is UNVERIFIED.
- No candidate-exact dogfood/QA receipt names the candidate SHA. Historical dogfood includes INFO/UNVERIFIED/harness-class cells and is not a waiver. Candidate-bound dogfood/QA evidence is UNVERIFIED.

## Security and deployment

- `go.mod` pins `toolchain go1.26.11`.
- `govulncheck ./...` returned exit 0: `No vulnerabilities found.`
- Live `http://127.0.0.1:9090/api/v1/health` returned `status=ok`, `db=connected`, `build_sha=9b1fdd22`, and version `v1.16.0-58-g9b1fdd22-dirty` for PID `276029`. `go version -m` reports Go `1.26.10`, `CGO_ENABLED=1`, and `vcs.modified=true`. The deployed daemon is stale versus the candidate and remains owned by `SCHED-GAP-162`/`RELEASE-051`. No deployment mutation was made.

## Decision

**NO CUT.** Do not tag, publish, deploy, or mutate the changelog. Exact-head CI is not green, `RELEASE-059`'s board-vocabulary blocker remains in the candidate, `RELEASE-060`'s changelog reconciliation remains open, and candidate-bound GitReins/dogfood evidence is absent. Existing tags and Releases were not modified.
